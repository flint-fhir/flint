package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/auth"
	automq "github.com/flint-fhir/flint/pkg/cdc"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/store/postgres"
)

type importJob struct {
	ID              string
	Tenant          string
	RequestURL      string
	TransactionTime time.Time
	Status          string // "in-progress", "completed", "error"
	ImportedCount   int
	CountsByType    map[string]int
	Errors          []string
}

type importSourceBlock struct {
	ResourceType string
	NDJSON       string
	SourceURL    string
}

// handleBulkImport handles POST /fhir/r4/{tenant}/$import — HL7 Bulk Data Access NDJSON import.
// Accepts either a FHIR Parameters resource (with inputFormat and repeated input blocks containing
// type + url / data: URI / content) or a raw application/fhir+ndjson stream.
// Supports both synchronous 200 OK OperationOutcome and Prefer: respond-async 202 Accepted job polling.
func (s *Server) handleBulkImport(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		writeOperationOutcome(w, http.StatusForbidden, "forbidden",
			"patient-scoped tokens cannot invoke system-level $import")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 50*1024*1024)) // 50MB max
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read $import request body")
		return
	}
	trimmedBody := body
	if !isProtoContentType(r) {
		trimmedBody = bytes.TrimSpace(body)
	}
	if len(trimmedBody) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "empty request body for $import")
		return
	}

	blocks, err := s.parseBulkImportBlocks(r.Context(), r.Header.Get("Content-Type"), trimmedBody)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if len(blocks) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "no NDJSON input blocks found in $import request")
		return
	}

	importedCount := 0
	countsByType := make(map[string]int)
	var importErrs []string

	for blockIdx, blk := range blocks {
		scanner := bufio.NewScanner(strings.NewReader(blk.NDJSON))
		scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			resType, _, err := s.importSingleNDJSONResource(r.Context(), tenant, blk.ResourceType, []byte(line), secCtx)
			if err != nil {
				importErrs = append(importErrs, fmt.Sprintf("input[%d] line %d: %v", blockIdx, lineNum, err))
				continue
			}
			importedCount++
			countsByType[resType]++
		}
		if err := scanner.Err(); err != nil {
			importErrs = append(importErrs, fmt.Sprintf("input[%d] read error: %v", blockIdx, err))
		}
	}

	if isAsyncPrefer(r) {
		jobID := fmt.Sprintf("imp-%s", generateID())
		baseURL := requestBaseURL(r)
		reqURL := baseURL + r.URL.RequestURI()
		status := "completed"
		if importedCount == 0 && len(importErrs) > 0 {
			status = "error"
		}
		job := &importJob{
			ID:              jobID,
			Tenant:          tenant,
			RequestURL:      reqURL,
			TransactionTime: time.Now().UTC(),
			Status:          status,
			ImportedCount:   importedCount,
			CountsByType:    countsByType,
			Errors:          importErrs,
		}
		s.importMu.Lock()
		if s.importJobs == nil {
			s.importJobs = make(map[string]*importJob)
		}
		s.importJobs[jobID] = job
		s.importMu.Unlock()

		pollURL := fmt.Sprintf("%s/fhir/r4/%s/$import-poll/%s", baseURL, tenant, jobID)
		w.Header().Set("Content-Location", pollURL)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if importedCount == 0 && len(importErrs) > 0 {
		outcome := validation.NewOutcome()
		for _, msg := range importErrs {
			outcome.AddError(validation.CodeInvalid, msg, "$import")
		}
		outcome.WriteHTTP(w, http.StatusBadRequest)
		return
	}

	issues := []map[string]any{
		{
			"severity":    "information",
			"code":        "informational",
			"diagnostics": fmt.Sprintf("Successfully imported %d FHIR resource(s) via $import", importedCount),
		},
	}
	for _, msg := range importErrs {
		issues = append(issues, map[string]any{
			"severity":    "warning",
			"code":        "processing",
			"diagnostics": msg,
		})
	}
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "OperationOutcome",
		"issue":        issues,
	})
}

func (s *Server) parseBulkImportBlocks(ctx context.Context, contentType string, body []byte) ([]importSourceBlock, error) {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if strings.Contains(ct, "protobuf") || strings.Contains(ct, "fhir+proto") {
		bundleMap, err := fhirutil.ProtoBytesToFHIRMap(fhirutil.BundleDescriptor(), "Bundle", body, 0, time.Time{})
		if err != nil {
			return nil, fmt.Errorf("invalid Bundle protobuf in $import request: %v", err)
		}
		entries, _ := bundleMap["entry"].([]any)
		var blocks []importSourceBlock
		for _, rawE := range entries {
			em, ok := rawE.(map[string]any)
			if !ok {
				continue
			}
			resMap, ok := em["resource"].(map[string]any)
			if !ok || resMap == nil {
				continue
			}
			rt, _ := resMap["resourceType"].(string)
			lineBytes, err := json.Marshal(resMap)
			if err != nil {
				continue
			}
			blocks = append(blocks, importSourceBlock{
				ResourceType: rt,
				NDJSON:       string(lineBytes),
			})
		}
		return blocks, nil
	}
	if strings.Contains(ct, "ndjson") {
		return []importSourceBlock{{NDJSON: string(body)}}, nil
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		// Fallback: treat multi-line JSON objects as raw NDJSON stream
		if bytes.Contains(body, []byte("\n")) {
			return []importSourceBlock{{NDJSON: string(body)}}, nil
		}
		return nil, fmt.Errorf("invalid JSON in $import request: %v", err)
	}

	rt, _ := root["resourceType"].(string)
	if rt != "Parameters" {
		// Single FHIR resource or NDJSON with 1 line
		if rt != "" {
			return []importSourceBlock{{ResourceType: rt, NDJSON: string(body)}}, nil
		}
		return nil, fmt.Errorf("$import JSON body must be a FHIR Parameters resource or NDJSON stream")
	}

	params, _ := root["parameter"].([]any)
	var blocks []importSourceBlock
	for _, p := range params {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _ := pm["name"].(string)
		switch name {
		case "inputFormat":
			fmtVal := strings.TrimSpace(extractParameterString(pm))
			if fmtVal != "" && !isSupportedNDJSONFormat(fmtVal) {
				return nil, fmt.Errorf("unsupported $import inputFormat %q (must be application/fhir+ndjson)", fmtVal)
			}
		case "input":
			var expectedType, rawURL, rawContent string
			if parts, ok := pm["part"].([]any); ok {
				for _, rawPart := range parts {
					partMap, ok := rawPart.(map[string]any)
					if !ok {
						continue
					}
					pName, _ := partMap["name"].(string)
					switch pName {
					case "type":
						expectedType = strings.TrimSpace(extractParameterString(partMap))
					case "url":
						rawURL = strings.TrimSpace(extractParameterString(partMap))
					case "content", "data":
						rawContent = extractParameterString(partMap)
					}
				}
			}
			if rawContent == "" {
				rawContent = extractParameterString(pm)
			}
			if rawContent != "" && rawURL == "" {
				blocks = append(blocks, importSourceBlock{
					ResourceType: expectedType,
					NDJSON:       rawContent,
				})
				continue
			}
			if rawURL != "" {
				ndjson, err := s.resolveImportInputURL(ctx, rawURL)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, importSourceBlock{
					ResourceType: expectedType,
					NDJSON:       ndjson,
					SourceURL:    rawURL,
				})
			}
		}
	}
	return blocks, nil
}

func (s *Server) resolveImportInputURL(ctx context.Context, rawURL string) (string, error) {
	if strings.HasPrefix(rawURL, "data:") {
		commaIdx := strings.IndexByte(rawURL, ',')
		if commaIdx == -1 {
			return "", fmt.Errorf("invalid data: URI in $import input.url")
		}
		meta := rawURL[5:commaIdx]
		payload := rawURL[commaIdx+1:]
		if strings.Contains(strings.ToLower(meta), ";base64") {
			decoded, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				return "", fmt.Errorf("invalid base64 data: URI in $import input.url: %v", err)
			}
			return string(decoded), nil
		}
		unescaped, err := url.PathUnescape(payload)
		if err != nil {
			return payload, nil
		}
		return unescaped, nil
	}

	if strings.HasPrefix(rawURL, "{") {
		return rawURL, nil
	}

	if err := ValidateSubscriptionEndpoint(rawURL, s.disallowPrivateWebhooks); err != nil {
		return "", fmt.Errorf("invalid $import input.url %q: %w", rawURL, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request for $import input.url %q: %w", rawURL, err)
	}
	req.Header.Set("Accept", "application/fhir+ndjson, application/x-ndjson, application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch $import input.url %q: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch $import input.url %q returned HTTP %d", rawURL, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 50*1024*1024))
	if err != nil {
		return "", fmt.Errorf("read $import input.url %q: %w", rawURL, err)
	}
	return string(b), nil
}

func (s *Server) importSingleNDJSONResource(ctx context.Context, tenant, expectedType string, rawJSON []byte, secCtx *auth.SecurityContext) (string, string, error) {
	var root map[string]any
	if err := json.Unmarshal(rawJSON, &root); err != nil {
		return "", "", fmt.Errorf("malformed JSON: %v", err)
	}
	resType, _ := root["resourceType"].(string)
	resType = strings.TrimSpace(resType)
	if resType == "" {
		return "", "", fmt.Errorf("missing resourceType")
	}
	if expectedType != "" && !strings.EqualFold(expectedType, resType) {
		return "", "", fmt.Errorf("resourceType %q does not match input block type %q", resType, expectedType)
	}

	md, ok := s.protoRegistry[resType]
	if !ok {
		return "", "", fmt.Errorf("unsupported resourceType %q", resType)
	}

	if secCtx != nil {
		if !auth.Allows(secCtx, resType, auth.ActionCreate) && !auth.Allows(secCtx, resType, auth.ActionUpdate) {
			return "", "", fmt.Errorf("insufficient SMART scope to import %s", resType)
		}
	}

	if s.validator != nil {
		outcome := s.validator.ValidateJSON(resType, rawJSON)
		if !outcome.IsValid() {
			if len(outcome.Issues) > 0 {
				return "", "", fmt.Errorf("validation failed for %s: %s", resType, outcome.Issues[0].Diagnostics)
			}
			return "", "", fmt.Errorf("validation failed for %s", resType)
		}
	}

	if err := s.validateSubscriptionResourceJSON(resType, rawJSON); err != nil {
		return "", "", err
	}
	rawJSON = normalizeSubscriptionOnWrite(resType, rawJSON)

	normalizedBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawJSON)
	if err != nil {
		return "", "", fmt.Errorf("normalize %s JSON: %v", resType, err)
	}

	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(normalizedBody, msg); err != nil {
		return "", "", fmt.Errorf("unmarshal %s proto: %v", resType, err)
	}

	resID := extractProtoResourceID(msg)
	if resID == "" {
		resID = generateID()
		setProtoResourceID(msg, resID)
	}

	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		return "", "", fmt.Errorf("marshal %s proto: %v", resType, err)
	}

	var idx *postgres.SearchIndexes
	if extractor, ok := s.extractors[resType]; ok {
		idx, _ = extractor(tenant, resID, protoBytes)
	}
	if strings.EqualFold(resType, "Provenance") {
		s.enrichProvenancePatientIndex(ctx, tenant, resID, idx)
	}

	rec, err := s.store.WriteResourceWithMeta(ctx, postgres.ResourceInput{
		TenantID:      tenant,
		ResType:       resType,
		ResID:         resID,
		ResourceProto: protoBytes,
		SearchIndexes: idx,
	})
	if err != nil {
		return "", "", fmt.Errorf("store %s/%s: %w", resType, resID, err)
	}

	s.publishCDC(ctx, automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     resID,
		ProtoData: protoBytes,
		Op:        "U",
	})
	s.dispatchSubscriptions(ctx, tenant, resType, resID, protoBytes, rec.ResVersion, rec.LastUpdated, idx)
	return resType, resID, nil
}

// handleImportPoll handles GET /fhir/r4/{tenant}/$import-poll/{jobId}.
func (s *Server) handleImportPoll(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	jobID := r.PathValue("jobId")

	if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
		writeOperationOutcome(w, http.StatusForbidden, "forbidden", "patient-scoped token cannot poll system $import job")
		return
	}

	s.importMu.RLock()
	var job *importJob
	if s.importJobs != nil {
		job = s.importJobs[jobID]
	}
	s.importMu.RUnlock()

	if job == nil || job.Tenant != tenant {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("import job %q not found", jobID))
		return
	}

	types := make([]string, 0, len(job.CountsByType))
	for rt := range job.CountsByType {
		types = append(types, rt)
	}
	sort.Strings(types)

	outputs := make([]map[string]any, 0, len(types))
	for _, rt := range types {
		outputs = append(outputs, map[string]any{
			"type":  rt,
			"count": job.CountsByType[rt],
		})
	}
	errs := make([]map[string]any, 0, len(job.Errors))
	for _, msg := range job.Errors {
		errs = append(errs, map[string]any{
			"type":        "OperationOutcome",
			"diagnostics": msg,
		})
	}

	manifest := map[string]any{
		"transactionTime": job.TransactionTime.UTC().Format(time.RFC3339),
		"request":         job.RequestURL,
		"status":          job.Status,
		"importedCount":   job.ImportedCount,
		"output":          outputs,
		"error":           errs,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(manifest)
}

// handleImportCancel handles DELETE /fhir/r4/{tenant}/$import-poll/{jobId}.
func (s *Server) handleImportCancel(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	jobID := r.PathValue("jobId")

	s.importMu.Lock()
	var found bool
	if s.importJobs != nil {
		if job, ok := s.importJobs[jobID]; ok && job.Tenant == tenant {
			delete(s.importJobs, jobID)
			found = true
		}
	}
	s.importMu.Unlock()

	if !found {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("import job %q not found", jobID))
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "OperationOutcome",
		"issue": []map[string]any{
			{
				"severity":    "information",
				"code":        "informational",
				"diagnostics": fmt.Sprintf("Import job %s cancelled and removed", jobID),
			},
		},
	})
}

// handlePatientPurge handles POST /fhir/r4/{tenant}/Patient/{id}/$purge.
// Atomically deletes (or physically expunges when expunge=true) the target Patient and all clinical
// resources in their Patient compartment, leaving shared non-compartment resources untouched.
func (s *Server) handlePatientPurge(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")
	patientID := cleanID(r.PathValue("id"))
	if patientID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "missing Patient ID for $purge")
		return
	}

	hardExpunge := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("expunge")), "true")
	if r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024))
		if err == nil && len(bytes.TrimSpace(body)) > 0 {
			var root map[string]any
			if json.Unmarshal(body, &root) == nil {
				if params, ok := root["parameter"].([]any); ok {
					for _, p := range params {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						if name, _ := pm["name"].(string); name == "expunge" {
							if bVal, ok := pm["valueBoolean"].(bool); ok {
								hardExpunge = bVal
							}
						}
					}
				}
			}
		}
	}

	setAuditPatientID(r.Context(), patientID)
	purged, err := s.store.PurgePatientCompartment(r.Context(), tenant, patientID, hardExpunge)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Patient/%s not found", patientID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("Patient/%s has already been deleted", patientID))
		return
	}
	if err != nil {
		s.logger.Error("patient purge failed", "error", err, "tenant", tenant, "patientId", patientID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to purge patient compartment")
		return
	}

	cdcMsgs := make([]automq.Message, 0, len(purged))
	for _, rec := range purged {
		cdcMsgs = append(cdcMsgs, automq.Message{
			TenantID: tenant,
			ResType:  rec.ResType,
			ResID:    rec.ResID,
			Op:       "D",
		})
	}
	s.publishCDC(r.Context(), cdcMsgs...)

	modeLabel := "soft-deleted"
	if hardExpunge {
		modeLabel = "physically expunged"
	}
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "OperationOutcome",
		"issue": []map[string]any{
			{
				"severity":    "information",
				"code":        "informational",
				"diagnostics": fmt.Sprintf("Purged %d resource(s) from Patient/%s compartment (%s)", len(purged), patientID, modeLabel),
			},
		},
	})
}

// handleExpunge handles POST /fhir/r4/{tenant}/$expunge, POST /fhir/r4/{tenant}/{resourceType}/$expunge,
// and POST /fhir/r4/{tenant}/{resourceType}/{id}/$expunge.
func (s *Server) handleExpunge(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")
	resType := strings.TrimSpace(r.PathValue("resourceType"))
	resID := strings.TrimSpace(r.PathValue("id"))

	if resType != "" {
		if _, ok := s.protoRegistry[resType]; !ok {
			writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
				fmt.Sprintf("resource type %q not registered", resType))
			return
		}
	}

	q := r.URL.Query()
	params := postgres.ExpungeParams{
		TenantID:                tenant,
		ResType:                 resType,
		ResID:                   resID,
		ExpungeDeletedResources: strings.EqualFold(strings.TrimSpace(q.Get("expungeDeletedResources")), "true"),
		ExpungePreviousVersions: strings.EqualFold(strings.TrimSpace(q.Get("expungePreviousVersions")), "true"),
		ExpungeEverything:       strings.EqualFold(strings.TrimSpace(q.Get("expungeEverything")), "true"),
	}
	if limStr := strings.TrimSpace(q.Get("limit")); limStr != "" {
		if n, err := strconv.Atoi(limStr); err == nil && n > 0 {
			params.Limit = n
		}
	}

	if r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read $expunge request body")
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			var root map[string]any
			if err := json.Unmarshal(body, &root); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid JSON in $expunge request: %v", err))
				return
			}
			if rt, _ := root["resourceType"].(string); rt != "" && rt != "Parameters" {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("$expunge POST body must be a FHIR Parameters resource (got %q)", rt))
				return
			}
			if paramList, ok := root["parameter"].([]any); ok {
				for _, p := range paramList {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					name, _ := pm["name"].(string)
					switch name {
					case "expungeDeletedResources":
						if bVal, ok := pm["valueBoolean"].(bool); ok {
							params.ExpungeDeletedResources = bVal
						}
					case "expungePreviousVersions":
						if bVal, ok := pm["valueBoolean"].(bool); ok {
							params.ExpungePreviousVersions = bVal
						}
					case "expungeEverything":
						if bVal, ok := pm["valueBoolean"].(bool); ok {
							params.ExpungeEverything = bVal
						}
					case "limit":
						if fVal, ok := pm["valueInteger"].(float64); ok && int(fVal) > 0 {
							params.Limit = int(fVal)
						}
					}
				}
			}
		}
	}

	expungedCount, err := s.store.ExpungeResources(r.Context(), params)
	if err != nil {
		s.logger.Error("expunge failed", "error", err, "tenant", tenant, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to execute $expunge")
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "OperationOutcome",
		"issue": []map[string]any{
			{
				"severity":    "information",
				"code":        "informational",
				"diagnostics": fmt.Sprintf("Expunged %d record(s)", expungedCount),
			},
		},
	})
}

// handleDiff handles GET/POST /fhir/r4/{tenant}/{resourceType}/{id}/$diff,
// POST /fhir/r4/{tenant}/{resourceType}/$diff, and POST /fhir/r4/{tenant}/$diff.
// Compares two versions of a FHIR resource (or two inline resources passed via a Parameters body)
// and returns a FHIR Parameters patch resource detailing add, delete, and replace operations.
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := strings.TrimSpace(r.PathValue("resourceType"))
	resID := strings.TrimSpace(r.PathValue("id"))
	q := r.URL.Query()

	fromVerStr := strings.TrimSpace(q.Get("fromVersion"))
	toVerStr := strings.TrimSpace(q.Get("version"))
	if toVerStr == "" {
		toVerStr = strings.TrimSpace(q.Get("toVersion"))
	}
	includeMeta := strings.EqualFold(strings.TrimSpace(q.Get("includeMeta")), "true")

	var fromMap, toMap map[string]any
	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read $diff request body")
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			var root map[string]any
			if err := json.Unmarshal(body, &root); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid JSON in $diff request: %v", err))
				return
			}
			if rt, _ := root["resourceType"].(string); rt != "" && rt != "Parameters" {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("$diff POST body must be a FHIR Parameters resource (got %q)", rt))
				return
			}
			if paramList, ok := root["parameter"].([]any); ok {
				for _, p := range paramList {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					name, _ := pm["name"].(string)
					switch name {
					case "fromVersion":
						if v := strings.TrimSpace(extractParameterString(pm)); v != "" && fromVerStr == "" {
							fromVerStr = v
						}
					case "version", "toVersion":
						if v := strings.TrimSpace(extractParameterString(pm)); v != "" && toVerStr == "" {
							toVerStr = v
						}
					case "includeMeta":
						if bVal, ok := pm["valueBoolean"].(bool); ok {
							includeMeta = bVal
						}
					case "from", "fromResource", "left":
						if res, ok := pm["resource"].(map[string]any); ok {
							fromMap = res
						}
					case "to", "toResource", "right":
						if res, ok := pm["resource"].(map[string]any); ok {
							toMap = res
						}
					}
				}
			}
		}
	}

	if fromMap != nil && toMap != nil {
		fromType, _ := fromMap["resourceType"].(string)
		toType, _ := toMap["resourceType"].(string)
		if fromType == "" || toType == "" {
			writeOperationOutcome(w, http.StatusBadRequest, "required", "both 'from' and 'to' resources in $diff must have resourceType")
			return
		}
		if fromType != toType {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("cannot $diff resources of different types (%s vs %s)", fromType, toType))
			return
		}
		if resType != "" && !strings.EqualFold(resType, fromType) {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("resourceType %q in body does not match endpoint type %q", fromType, resType))
			return
		}
		diffParams := computeFHIRParametersDiff(fromType, fromMap, toMap, includeMeta)
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(diffParams)
		return
	}

	if resType == "" || resID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"system or type-level $diff requires 'from' and 'to' resources in a FHIR Parameters body")
		return
	}
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}

	curRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("%s/%s not found", resType, resID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) && toVerStr == "" {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("%s/%s has been deleted", resType, resID))
		return
	}
	if err != nil && !errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read resource for $diff")
		return
	}

	toVer := curRec.ResVersion
	if toVerStr != "" {
		v, err := strconv.Atoi(toVerStr)
		if err != nil || v <= 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid target version %q", toVerStr))
			return
		}
		toVer = v
	}
	fromVer := max(1, toVer-1)
	if fromVerStr != "" {
		v, err := strconv.Atoi(fromVerStr)
		if err != nil || v <= 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid fromVersion %q", fromVerStr))
			return
		}
		fromVer = v
	}

	fromRec, err := s.store.ReadResourceVersion(r.Context(), tenant, resType, resID, fromVer)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("%s/%s/_history/%d not found", resType, resID, fromVer))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted",
			fmt.Sprintf("%s/%s/_history/%d was a deletion", resType, resID, fromVer))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read fromVersion for $diff")
		return
	}

	toRec, err := s.store.ReadResourceVersion(r.Context(), tenant, resType, resID, toVer)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("%s/%s/_history/%d not found", resType, resID, toVer))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted",
			fmt.Sprintf("%s/%s/_history/%d was a deletion", resType, resID, toVer))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read target version for $diff")
		return
	}

	fromMap, err = s.protoToFHIRMapWithMeta(resType, fromRec.ResourceProto, fromRec.ResVersion, fromRec.LastUpdated)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to serialize fromVersion")
		return
	}
	toMap, err = s.protoToFHIRMapWithMeta(resType, toRec.ResourceProto, toRec.ResVersion, toRec.LastUpdated)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to serialize target version")
		return
	}

	diffParams := computeFHIRParametersDiff(resType, fromMap, toMap, includeMeta)
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(diffParams)
}

func computeFHIRParametersDiff(resType string, fromMap, toMap map[string]any, includeMeta bool) map[string]any {
	cleanFrom := cloneMapForDiff(fromMap, includeMeta)
	cleanTo := cloneMapForDiff(toMap, includeMeta)

	var ops []any
	diffMapsRecursive(resType, cleanFrom, cleanTo, &ops)

	return map[string]any{
		"resourceType": "Parameters",
		"parameter":    ops,
	}
}

func cloneMapForDiff(m map[string]any, includeMeta bool) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "resourceType" {
			continue
		}
		if k == "meta" && !includeMeta {
			if metaMap, ok := v.(map[string]any); ok {
				filteredMeta := make(map[string]any)
				for mk, mv := range metaMap {
					if mk == "versionId" || mk == "lastUpdated" {
						continue
					}
					filteredMeta[mk] = mv
				}
				if len(filteredMeta) > 0 {
					out["meta"] = filteredMeta
				}
				continue
			}
		}
		out[k] = v
	}
	return out
}

func diffMapsRecursive(parentPath string, fromMap, toMap map[string]any, ops *[]any) {
	keysMap := make(map[string]bool, len(fromMap)+len(toMap))
	for k := range fromMap {
		keysMap[k] = true
	}
	for k := range toMap {
		keysMap[k] = true
	}
	keys := make([]string, 0, len(keysMap))
	for k := range keysMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		oldVal, hasOld := fromMap[k]
		newVal, hasNew := toMap[k]
		elemPath := parentPath + "." + k

		if !hasOld && hasNew {
			*ops = append(*ops, buildDiffOperation("add", parentPath, k, nil, newVal))
			continue
		}
		if hasOld && !hasNew {
			*ops = append(*ops, buildDiffOperation("delete", elemPath, k, oldVal, nil))
			continue
		}
		if reflect.DeepEqual(oldVal, newVal) {
			continue
		}

		oldSub, oldIsMap := oldVal.(map[string]any)
		newSub, newIsMap := newVal.(map[string]any)
		if oldIsMap && newIsMap {
			diffMapsRecursive(elemPath, oldSub, newSub, ops)
			continue
		}

		*ops = append(*ops, buildDiffOperation("replace", elemPath, k, oldVal, newVal))
	}
}

func buildDiffOperation(opType, path, name string, oldVal, newVal any) map[string]any {
	parts := []map[string]any{
		{"name": "type", "valueCode": opType},
		{"name": "path", "valueString": path},
	}
	if opType == "add" {
		parts = append(parts, map[string]any{"name": "name", "valueString": name})
	}
	if oldVal != nil {
		parts = append(parts, buildDiffValuePart("previousValue", oldVal))
	}
	if newVal != nil {
		parts = append(parts, buildDiffValuePart("value", newVal))
	}
	return map[string]any{
		"name": "operation",
		"part": parts,
	}
}

func buildDiffValuePart(partName string, val any) map[string]any {
	switch v := val.(type) {
	case string:
		return map[string]any{"name": partName, "valueString": v}
	case bool:
		return map[string]any{"name": partName, "valueBoolean": v}
	case float64:
		if float64(int64(v)) == v {
			return map[string]any{"name": partName, "valueInteger": int64(v)}
		}
		return map[string]any{"name": partName, "valueDecimal": v}
	default:
		b, _ := json.Marshal(v)
		return map[string]any{"name": partName, "valueString": string(b)}
	}
}

func isAsyncPrefer(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, h := range r.Header.Values("Prefer") {
		if strings.Contains(strings.ToLower(h), "respond-async") {
			return true
		}
	}
	return false
}

func extractParameterString(pm map[string]any) string {
	for _, k := range []string{"valueString", "valueCode", "valueUri", "valueUrl", "valueId", "valueCanonical", "valueMarkdown"} {
		if sVal, ok := pm[k].(string); ok && sVal != "" {
			return sVal
		}
	}
	if fVal, ok := pm["valueInteger"].(float64); ok {
		return strconv.Itoa(int(fVal))
	}
	return ""
}

func isSupportedNDJSONFormat(f string) bool {
	lower := strings.ToLower(strings.TrimSpace(f))
	switch lower {
	case "application/fhir+ndjson", "application/ndjson", "ndjson", "application/x-ndjson":
		return true
	default:
		return false
	}
}
