package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/store/postgres"
)

const maxStoredExportJobs = 256

type exportJob struct {
	ID         string
	TenantID   string
	PatientID  string
	CreatedAt  time.Time
	RequestURL string
	TypesOrder []string
	Files      map[string][]byte // resourceType -> NDJSON bytes
	Counts     map[string]int    // resourceType -> line count
	Combined   []byte            // optional combined NDJSON
	Status     string            // "in-progress", "completed", "failed"
	Err        string
	done       chan struct{}
}

func (s *Server) storeExportJob(job *exportJob) {
	if s.store != nil && job != nil && job.Status != "in-progress" {
		_ = s.store.SaveExportJob(context.Background(), postgres.ExportJobRecord{
			ID:         job.ID,
			TenantID:   job.TenantID,
			PatientID:  job.PatientID,
			CreatedAt:  job.CreatedAt,
			RequestURL: job.RequestURL,
			TypesOrder: job.TypesOrder,
			Files:      job.Files,
			Counts:     job.Counts,
			Combined:   nil, // Per-type files are already stored in Files; avoid 2x storage bloat in Postgres
		})
	}
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	if s.exportJobs == nil {
		s.exportJobs = make(map[string]*exportJob)
	}
	if _, exists := s.exportJobs[job.ID]; !exists && len(s.exportJobs) >= maxStoredExportJobs {
		var oldestID string
		var oldestTime time.Time
		for id, j := range s.exportJobs {
			if oldestID == "" || j.CreatedAt.Before(oldestTime) {
				oldestID = id
				oldestTime = j.CreatedAt
			}
		}
		if oldestID != "" {
			delete(s.exportJobs, oldestID)
		}
	}
	s.exportJobs[job.ID] = job
}

func (s *Server) getExportJob(tenant, jobID string) *exportJob {
	s.exportMu.RLock()
	if s.exportJobs != nil {
		if job, ok := s.exportJobs[jobID]; ok && job.TenantID == tenant {
			s.exportMu.RUnlock()
			return job
		}
	}
	s.exportMu.RUnlock()

	if s.store != nil {
		if rec, err := s.store.GetExportJob(context.Background(), tenant, jobID); err == nil && rec != nil {
			return &exportJob{
				ID:         rec.ID,
				TenantID:   rec.TenantID,
				PatientID:  rec.PatientID,
				CreatedAt:  rec.CreatedAt,
				RequestURL: rec.RequestURL,
				TypesOrder: rec.TypesOrder,
				Files:      rec.Files,
				Counts:     rec.Counts,
				Combined:   rec.Combined,
			}
		}
	}
	return nil
}

func (s *Server) deleteExportJob(tenant, jobID string) bool {
	deletedDB := false
	if s.store != nil {
		deletedDB, _ = s.store.DeleteExportJob(context.Background(), tenant, jobID)
	}
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	if s.exportJobs == nil {
		return deletedDB
	}
	job, ok := s.exportJobs[jobID]
	if !ok || job.TenantID != tenant {
		return deletedDB
	}
	delete(s.exportJobs, jobID)
	return true
}

func isValidNDJSONFormat(raw string) bool {
	v := strings.ToLower(strings.TrimSpace(raw))
	if idx := strings.Index(v, ";"); idx != -1 {
		v = strings.TrimSpace(v[:idx])
	}
	switch v {
	case "", "application/fhir+ndjson", "application/ndjson", "ndjson":
		return true
	default:
		return false
	}
}

// handleSystemExport handles GET/POST /fhir/r4/{tenant}/$export (System-level FHIR Bulk Data Access).
func (s *Server) handleSystemExport(w http.ResponseWriter, r *http.Request) {
	s.executeBulkExport(w, r, false, "", "")
}

// handlePatientGroupExport handles GET/POST /fhir/r4/{tenant}/Patient/$export (Patient-level FHIR Bulk Data Access).
func (s *Server) handlePatientGroupExport(w http.ResponseWriter, r *http.Request) {
	s.executeBulkExport(w, r, true, "", "")
}

// handleSinglePatientExport handles GET/POST /fhir/r4/{tenant}/{resourceType}/{id}/$export
// Supports Patient/{id}/$export (Single-Patient Compartment Bulk Export) and
// Group/{id}/$export (ONC (g)(10) Group Cohort Bulk Export).
func (s *Server) handleSinglePatientExport(w http.ResponseWriter, r *http.Request) {
	resType := r.PathValue("resourceType")
	if resType == "Group" {
		groupID := strings.TrimSpace(r.PathValue("id"))
		s.executeBulkExport(w, r, true, "", groupID)
		return
	}
	if resType != "" && resType != "Patient" {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("$export is not supported on %s instances", resType))
		return
	}
	patientID := strings.TrimSpace(r.PathValue("id"))
	s.executeBulkExport(w, r, true, patientID, "")
}

type exportTypeFilter struct {
	resType string
	params  postgres.SearchParams
}

type parsedExportRequest struct {
	outputFormat string
	typeRaw      string
	sinceRaw     string
	untilRaw     string
	elements     []string
	typeFilters  []string
	patientRefs  []string
	asyncParam   string
	formatParam  string
}

func parseExportRequest(r *http.Request) (parsedExportRequest, error) {
	q := mergedSearchQuery(r)
	req := parsedExportRequest{
		outputFormat: q.Get("_outputFormat"),
		typeRaw:      strings.Join(q["_type"], ","),
		sinceRaw:     q.Get("_since"),
		untilRaw:     q.Get("_until"),
		asyncParam:   q.Get("_async"),
		formatParam:  q.Get("_format"),
	}
	for _, el := range q["_elements"] {
		for part := range strings.SplitSeq(el, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				req.elements = append(req.elements, trimmed)
			}
		}
	}
	for _, tf := range q["_typeFilter"] {
		for part := range strings.SplitSeq(tf, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				req.typeFilters = append(req.typeFilters, trimmed)
			}
		}
	}
	for _, pat := range q["patient"] {
		for part := range strings.SplitSeq(pat, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				req.patientRefs = append(req.patientRefs, trimmed)
			}
		}
	}

	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		if !strings.Contains(ct, "application/x-www-form-urlencoded") {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
			if err != nil {
				return req, fmt.Errorf("failed to read $export request body")
			}
			if strings.TrimSpace(string(body)) != "" {
				var raw map[string]any
				if err := json.Unmarshal(body, &raw); err != nil {
					return req, fmt.Errorf("invalid JSON in $export request: %v", err)
				}
				if rt, _ := raw["resourceType"].(string); rt != "Parameters" {
					return req, fmt.Errorf("$export POST body must be a Parameters resource (got %q)", rt)
				}
				var bodyTypes []string
				if paramsArr, ok := raw["parameter"].([]any); ok {
					for _, p := range paramsArr {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						val := ""
						for _, k := range []string{"valueString", "valueCode", "valueUri", "valueCanonical", "valueInstant", "valueDateTime", "valueDate"} {
							if sVal, ok := pm[k].(string); ok && strings.TrimSpace(sVal) != "" {
								val = strings.TrimSpace(sVal)
								break
							}
						}
						if val == "" {
							if refMap, ok := pm["valueReference"].(map[string]any); ok {
								if refStr, ok := refMap["reference"].(string); ok && strings.TrimSpace(refStr) != "" {
									val = strings.TrimSpace(refStr)
								}
							}
						}
						if val == "" {
							if bVal, ok := pm["valueBoolean"].(bool); ok {
								if bVal {
									val = "true"
								} else {
									val = "false"
								}
							}
						}
						if val == "" {
							continue
						}
						switch name {
						case "_outputFormat":
							if req.outputFormat == "" {
								req.outputFormat = val
							}
						case "_type":
							bodyTypes = append(bodyTypes, val)
						case "_since":
							if req.sinceRaw == "" {
								req.sinceRaw = val
							}
						case "_until":
							if req.untilRaw == "" {
								req.untilRaw = val
							}
						case "_elements":
							for part := range strings.SplitSeq(val, ",") {
								if trimmed := strings.TrimSpace(part); trimmed != "" {
									req.elements = append(req.elements, trimmed)
								}
							}
						case "_typeFilter":
							for part := range strings.SplitSeq(val, ",") {
								if trimmed := strings.TrimSpace(part); trimmed != "" {
									req.typeFilters = append(req.typeFilters, trimmed)
								}
							}
						case "patient":
							for part := range strings.SplitSeq(val, ",") {
								if trimmed := strings.TrimSpace(part); trimmed != "" {
									req.patientRefs = append(req.patientRefs, trimmed)
								}
							}
						case "_async":
							if req.asyncParam == "" {
								req.asyncParam = val
							}
						case "_format":
							if req.formatParam == "" {
								req.formatParam = val
							}
						}
					}
				}
				if len(bodyTypes) > 0 && req.typeRaw == "" {
					req.typeRaw = strings.Join(bodyTypes, ",")
				}
			}
		}
	}
	return req, nil
}

func (s *Server) executeBulkExport(w http.ResponseWriter, r *http.Request, isPatientExport bool, patientID string, groupID string) {
	tenant := r.PathValue("tenant")
	reqParams, err := parseExportRequest(r)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	if !isValidNDJSONFormat(reqParams.outputFormat) {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid",
			fmt.Sprintf("unsupported _outputFormat %q: only application/fhir+ndjson is supported", reqParams.outputFormat))
		return
	}

	var requestedTypes []string
	if typeRaw := strings.TrimSpace(reqParams.typeRaw); typeRaw != "" {
		seen := make(map[string]bool)
		for part := range strings.SplitSeq(typeRaw, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			if _, ok := s.protoRegistry[trimmed]; !ok {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("unsupported resource type %q in _type parameter", trimmed))
				return
			}
			if !seen[trimmed] {
				seen[trimmed] = true
				requestedTypes = append(requestedTypes, trimmed)
			}
		}
	}

	var since *time.Time
	if sinceRaw := strings.TrimSpace(reqParams.sinceRaw); sinceRaw != "" {
		dOp, err := postgres.ParseDateOp(sinceRaw)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _since parameter %q: %v", sinceRaw, err))
			return
		}
		t := dOp.Value
		since = &t
	}

	var until *time.Time
	if untilRaw := strings.TrimSpace(reqParams.untilRaw); untilRaw != "" {
		dOp, err := postgres.ParseDateOp(untilRaw)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _until parameter %q: %v", untilRaw, err))
			return
		}
		t := dOp.Value
		if dOp.High.After(dOp.Value) {
			t = dOp.High.Add(-time.Microsecond)
		}
		until = &t
	}
	if since != nil && until != nil && until.Before(*since) {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid",
			fmt.Sprintf("_until (%s) must not be earlier than _since (%s)", reqParams.untilRaw, reqParams.sinceRaw))
		return
	}

	for _, el := range reqParams.elements {
		if idx := strings.IndexByte(el, '.'); idx != -1 {
			rt := strings.TrimSpace(el[:idx])
			field := strings.TrimSpace(el[idx+1:])
			if rt == "" || field == "" || strings.Contains(field, ".") {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("invalid _elements token %q", el))
				return
			}
			if _, ok := s.protoRegistry[rt]; !ok {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("unsupported resource type %q in _elements token %q", rt, el))
				return
			}
		}
	}

	var parsedTypeFilters []exportTypeFilter
	for _, tf := range reqParams.typeFilters {
		parts := strings.SplitN(tf, "?", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _typeFilter %q: must be formatted as ResourceType?param=value", tf))
			return
		}
		rt := strings.TrimSpace(parts[0])
		if _, ok := s.protoRegistry[rt]; !ok {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("unsupported resource type %q in _typeFilter %q", rt, tf))
			return
		}
		qVals, err := url.ParseQuery(strings.TrimSpace(parts[1]))
		if err != nil || len(qVals) == 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid query in _typeFilter %q", tf))
			return
		}
		if err := postgres.ValidateSearchQuery(qVals); err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _typeFilter %q: %v", tf, err))
			return
		}
		sp := postgres.BuildSearchParamsFromQuery(tenant, rt, qVals)
		sp.Count = 10000
		parsedTypeFilters = append(parsedTypeFilters, exportTypeFilter{
			resType: rt,
			params:  sp,
		})
	}

	if len(reqParams.patientRefs) > 0 && (!isPatientExport || patientID != "") {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid",
			"the patient parameter is only supported on Patient/$export and Group/{id}/$export")
		return
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx {
		if auth.IsPatientRestricted(secCtx) {
			allowedPat := cleanID(secCtx.GetPatientId())
			if allowedPat == "" {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"patient-scoped token is missing required patient_id claim")
				return
			}
			if !isPatientExport || patientID == "" || groupID != "" {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"patient-scoped token cannot invoke system-level or multi-patient $export; use /Patient/{id}/$export")
				return
			}
			if cleanID(patientID) != allowedPat {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot export patient %s", secCtx.GetPatientId(), patientID))
				return
			}
			patientID = allowedPat
		}

		if len(requestedTypes) > 0 {
			for _, rt := range requestedTypes {
				if !auth.Allows(secCtx, rt, auth.ActionRead) && !auth.Allows(secCtx, rt, auth.ActionSearch) {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("insufficient scope for $export _type %s", rt))
					return
				}
			}
		} else {
			authorized := s.authorizedExportResourceTypes(secCtx)
			if len(authorized) == 0 {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"insufficient scope for $export")
				return
			}
			requestedTypes = authorized
		}
	}

	if patientID != "" {
		setAuditPatientID(r.Context(), patientID)
	}

	if s.store == nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
		return
	}

	// Verify target Patient exists upfront for single-patient compartment export
	if patientID != "" {
		_, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Patient", patientID)
		if errors.Is(err, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found",
				fmt.Sprintf("Patient/%s not found", patientID))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted",
				fmt.Sprintf("Patient/%s has been deleted", patientID))
			return
		}
		if err != nil {
			s.logger.Error("bulk export patient lookup failed", "error", err, "tenant", tenant, "patient", patientID)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to execute bulk export")
			return
		}
	}

	var cohortPatientIDs []string
	var emptyCohort bool

	if groupID != "" {
		groupPatIDs, isBuiltinAll, err := s.resolveExportGroupCohort(r.Context(), tenant, groupID, 0, make(map[string]bool))
		if errors.Is(err, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found",
				fmt.Sprintf("Group/%s not found", groupID))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted",
				fmt.Sprintf("Group/%s has been deleted", groupID))
			return
		}
		if err != nil {
			s.logger.Error("bulk export group lookup failed", "error", err, "tenant", tenant, "group", groupID)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to execute bulk export")
			return
		}
		if !isBuiltinAll {
			cohortPatientIDs = groupPatIDs
			if len(cohortPatientIDs) == 0 {
				emptyCohort = true
			}
		}
	}

	if len(reqParams.patientRefs) > 0 {
		var requestedPatIDs []string
		seenReq := make(map[string]bool)
		for _, ref := range reqParams.patientRefs {
			cid := cleanExportPatientRef(ref)
			if cid != "" && !seenReq[cid] {
				seenReq[cid] = true
				requestedPatIDs = append(requestedPatIDs, cid)
			}
		}
		if emptyCohort {
			// Group already has 0 active patients
		} else if len(cohortPatientIDs) > 0 {
			groupSet := make(map[string]bool, len(cohortPatientIDs))
			for _, pid := range cohortPatientIDs {
				groupSet[pid] = true
			}
			var intersected []string
			for _, pid := range requestedPatIDs {
				if groupSet[pid] {
					intersected = append(intersected, pid)
				}
			}
			cohortPatientIDs = intersected
			if len(cohortPatientIDs) == 0 {
				emptyCohort = true
			}
		} else {
			cohortPatientIDs = requestedPatIDs
			if len(cohortPatientIDs) == 0 {
				emptyCohort = true
			}
		}
	}

	typeFilterAllowed, err := s.resolveExportTypeFilterAllowedIDs(r.Context(), parsedTypeFilters)
	if err != nil {
		s.logger.Error("bulk export _typeFilter evaluation failed", "error", err, "tenant", tenant)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to evaluate _typeFilter criteria")
		return
	}

	exportParams := postgres.ExportParams{
		TenantID:           tenant,
		PatientID:          patientID,
		PatientIDs:         cohortPatientIDs,
		EmptyCohort:        emptyCohort,
		PatientCompartment: isPatientExport && patientID == "" && len(cohortPatientIDs) == 0 && !emptyCohort,
		ResourceTypes:      requestedTypes,
		Since:              since,
		Until:              until,
	}

	prefer := strings.ToLower(r.Header.Get("Prefer"))
	isAsync := strings.Contains(prefer, "respond-async") || strings.EqualFold(reqParams.asyncParam, "true")
	accept := strings.ToLower(r.Header.Get("Accept"))
	wantsManifest := (strings.Contains(accept, "application/json") || strings.Contains(accept, "application/fhir+json")) &&
		!strings.Contains(accept, "ndjson") || strings.EqualFold(reqParams.formatParam, "manifest")

	// Fast path for direct NDJSON streaming (neither async nor manifest requested):
	// stream each resource line directly to the response writer in paginated DB batches
	// without buffering the entire export in memory or writing a job row to Postgres.
	if !isAsync && !wantsManifest {
		w.Header().Set("Content-Type", "application/fhir+ndjson; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		patientRestricted := hasSecCtx && auth.IsPatientRestricted(secCtx)
		allowedPat := ""
		if patientRestricted {
			allowedPat = cleanID(secCtx.GetPatientId())
		}
		_ = s.store.StreamExportResources(r.Context(), exportParams, func(res postgres.SearchResult) error {
			if !matchesExportTypeFilter(typeFilterAllowed, res.ResType, res.ResID) {
				return nil
			}
			if hasSecCtx {
				if patientRestricted && patientID != "" && strings.EqualFold(res.ResType, "Patient") && cleanID(res.ResID) != allowedPat {
					return nil
				}
				if !s.resourceAllowedByGranularScope(secCtx, tenant, res.ResType, res.ResID, res.ResourceProto) {
					return nil
				}
			}
			line, ok := s.formatExportNDJSONLine(res, reqParams.elements)
			if ok {
				_, _ = w.Write(line)
			}
			return nil
		})
		return
	}

	jobID := uuid.NewString()
	jobPatientID := ""
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		jobPatientID = cleanID(secCtx.GetPatientId())
	} else if patientID != "" {
		jobPatientID = cleanID(patientID)
	}

	base := requestBaseURL(r)
	pollLocation := fmt.Sprintf("%s/fhir/r4/%s/$export-poll/%s", base, tenant, jobID)
	w.Header().Set("Content-Location", pollLocation)

	if isAsync {
		job := &exportJob{
			ID:         jobID,
			TenantID:   tenant,
			PatientID:  jobPatientID,
			CreatedAt:  time.Now().UTC(),
			RequestURL: base + r.URL.RequestURI(),
			Status:     "in-progress",
			done:       make(chan struct{}),
		}
		s.storeExportJob(job)

		asyncCtx := context.WithoutCancel(r.Context())
		go s.populateExportJob(asyncCtx, job, exportParams, typeFilterAllowed, reqParams.elements, hasSecCtx, secCtx, patientID)

		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resourceType": "OperationOutcome",
			"issue": []map[string]any{
				{
					"severity":    "information",
					"code":        "informational",
					"diagnostics": fmt.Sprintf("Bulk export job %s accepted; poll %s", jobID, pollLocation),
				},
			},
		})
		return
	}

	// Synchronous manifest mode
	job := &exportJob{
		ID:         jobID,
		TenantID:   tenant,
		PatientID:  jobPatientID,
		CreatedAt:  time.Now().UTC(),
		RequestURL: base + r.URL.RequestURI(),
		Status:     "in-progress",
		done:       make(chan struct{}),
	}
	s.populateExportJob(r.Context(), job, exportParams, typeFilterAllowed, reqParams.elements, hasSecCtx, secCtx, patientID)
	if job.Err != "" {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to execute bulk export")
		return
	}
	s.writeExportManifest(w, r, job)
}

func cleanExportPatientRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if idx := strings.LastIndex(ref, "Patient/"); idx != -1 {
		return strings.TrimSpace(ref[idx+len("Patient/"):])
	}
	return cleanID(ref)
}

func (s *Server) resolveExportGroupCohort(
	ctx context.Context,
	tenant, groupID string,
	depth int,
	visited map[string]bool,
) ([]string, bool, error) {
	if depth > 4 || visited[groupID] {
		return nil, false, nil
	}
	visited[groupID] = true

	res, err := s.store.ReadResourceWithMeta(ctx, tenant, "Group", groupID)
	if errors.Is(err, sql.ErrNoRows) {
		if depth == 0 && (strings.EqualFold(groupID, "all") || strings.EqualFold(groupID, "all-patients")) {
			return nil, true, nil
		}
		return nil, false, err
	}
	if err != nil {
		return nil, false, err
	}

	jsonBytes, err := s.protoToJSONWithMeta("Group", res.ResourceProto, res.ResVersion, res.LastUpdated)
	if err != nil {
		return nil, false, err
	}

	var groupDoc struct {
		Member []struct {
			Entity struct {
				Reference string `json:"reference"`
				Type      string `json:"type"`
			} `json:"entity"`
			Period struct {
				End string `json:"end"`
			} `json:"period"`
			Inactive bool `json:"inactive"`
		} `json:"member"`
	}
	if err := json.Unmarshal(jsonBytes, &groupDoc); err != nil {
		return nil, false, err
	}

	now := time.Now().UTC()
	var patientIDs []string
	seen := make(map[string]bool)

	for _, m := range groupDoc.Member {
		if m.Inactive {
			continue
		}
		if endStr := strings.TrimSpace(m.Period.End); endStr != "" {
			if dOp, err := postgres.ParseDateOp(endStr); err == nil {
				if dOp.Value.Before(now) {
					continue
				}
			}
		}
		ref := strings.TrimSpace(m.Entity.Reference)
		if ref == "" {
			continue
		}
		if idx := strings.LastIndex(ref, "Patient/"); idx != -1 {
			pid := strings.TrimSpace(ref[idx+len("Patient/"):])
			if pid != "" && !seen[pid] {
				seen[pid] = true
				patientIDs = append(patientIDs, pid)
			}
			continue
		}
		if idx := strings.LastIndex(ref, "Group/"); idx != -1 {
			subGroupID := strings.TrimSpace(ref[idx+len("Group/"):])
			if subGroupID != "" {
				subPats, _, subErr := s.resolveExportGroupCohort(ctx, tenant, subGroupID, depth+1, visited)
				if subErr == nil {
					for _, pid := range subPats {
						if !seen[pid] {
							seen[pid] = true
							patientIDs = append(patientIDs, pid)
						}
					}
				}
			}
			continue
		}
		if strings.EqualFold(m.Entity.Type, "Patient") && !strings.Contains(ref, "/") {
			if !seen[ref] {
				seen[ref] = true
				patientIDs = append(patientIDs, ref)
			}
		}
	}

	return patientIDs, false, nil
}

func (s *Server) resolveExportTypeFilterAllowedIDs(
	ctx context.Context,
	filters []exportTypeFilter,
) (map[string]map[string]bool, error) {
	if len(filters) == 0 {
		return nil, nil
	}
	allowed := make(map[string]map[string]bool)
	for _, f := range filters {
		if _, exists := allowed[f.resType]; !exists {
			allowed[f.resType] = make(map[string]bool)
		}
		params := f.params
		if err := s.expandTerminologySearchParams(ctx, params.TenantID, &params); err != nil {
			return nil, err
		}
		sr, err := s.store.Search(ctx, params)
		if err != nil {
			return nil, err
		}
		for _, match := range sr.Matches {
			allowed[f.resType][match.ResID] = true
		}
	}
	return allowed, nil
}

func matchesExportTypeFilter(allowed map[string]map[string]bool, resType, resID string) bool {
	if len(allowed) == 0 {
		return true
	}
	idSet, hasFilterForType := allowed[resType]
	if !hasFilterForType {
		return true
	}
	return idSet[resID]
}

func (s *Server) formatExportNDJSONLine(res postgres.SearchResult, elements []string) ([]byte, bool) {
	jsonBytes, err := s.protoToJSONWithMeta(res.ResType, res.ResourceProto, res.ResVersion, res.LastUpdated)
	if err != nil {
		s.logger.Error("proto to json in export", "error", err, "type", res.ResType, "id", res.ResID)
		return nil, false
	}
	if len(elements) > 0 {
		var resource map[string]any
		if err := json.Unmarshal(jsonBytes, &resource); err == nil {
			resource = applySummaryOrElements(resource, "", elements)
			if projected, mErr := json.Marshal(resource); mErr == nil {
				jsonBytes = projected
			}
		}
	}
	var compactLine bytes.Buffer
	if err := json.Compact(&compactLine, jsonBytes); err != nil {
		compactLine.Reset()
		compactLine.Write(bytes.ReplaceAll(jsonBytes, []byte("\n"), []byte(" ")))
	}
	compactLine.WriteByte('\n')
	return compactLine.Bytes(), true
}

func (s *Server) populateExportJob(
	ctx context.Context,
	job *exportJob,
	exportParams postgres.ExportParams,
	typeFilterAllowed map[string]map[string]bool,
	elements []string,
	hasSecCtx bool,
	secCtx *auth.SecurityContext,
	patientID string,
) {
	defer func() {
		if job.done != nil {
			close(job.done)
		}
	}()

	files := make(map[string]*bytes.Buffer)
	counts := make(map[string]int)
	var typesOrder []string

	patientRestricted := hasSecCtx && auth.IsPatientRestricted(secCtx)
	allowedPat := ""
	if patientRestricted {
		allowedPat = cleanID(secCtx.GetPatientId())
	}

	err := s.store.StreamExportResources(ctx, exportParams, func(res postgres.SearchResult) error {
		if !matchesExportTypeFilter(typeFilterAllowed, res.ResType, res.ResID) {
			return nil
		}
		if hasSecCtx {
			if patientRestricted && patientID != "" && strings.EqualFold(res.ResType, "Patient") && cleanID(res.ResID) != allowedPat {
				return nil
			}
			if !s.resourceAllowedByGranularScope(secCtx, exportParams.TenantID, res.ResType, res.ResID, res.ResourceProto) {
				return nil
			}
		}
		line, ok := s.formatExportNDJSONLine(res, elements)
		if !ok {
			return nil
		}
		buf, exists := files[res.ResType]
		if !exists {
			buf = &bytes.Buffer{}
			files[res.ResType] = buf
			typesOrder = append(typesOrder, res.ResType)
		}
		buf.Write(line)
		counts[res.ResType]++
		return nil
	})

	fileBytes := make(map[string][]byte, len(files))
	for rt, buf := range files {
		fileBytes[rt] = buf.Bytes()
	}

	s.exportMu.Lock()
	job.TypesOrder = typesOrder
	job.Files = fileBytes
	job.Counts = counts
	if err != nil {
		job.Status = "failed"
		job.Err = err.Error()
	} else {
		job.Status = "completed"
	}
	s.exportMu.Unlock()

	if err == nil {
		s.storeExportJob(job)
	}
}

func requestBaseURL(r *http.Request) string {
	if r == nil || r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) authorizedExportResourceTypes(secCtx *auth.SecurityContext) []string {
	types := make([]string, 0, len(s.protoRegistry))
	for rt := range s.protoRegistry {
		if auth.Allows(secCtx, rt, auth.ActionRead) || auth.Allows(secCtx, rt, auth.ActionSearch) {
			types = append(types, rt)
		}
	}
	sort.Strings(types)
	return types
}

// handleExportPoll handles GET /fhir/r4/{tenant}/$export-poll/{jobId}.
func (s *Server) handleExportPoll(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	jobID := r.PathValue("jobId")

	job := s.getExportJob(tenant, jobID)
	if job == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("export job %s not found", jobID))
		return
	}

	if job.done != nil {
		if strings.EqualFold(r.URL.Query().Get("_nowait"), "true") {
			select {
			case <-job.done:
			default:
				w.Header().Set("X-Progress", "in-progress")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusAccepted)
				return
			}
		} else {
			select {
			case <-job.done:
			case <-time.After(2 * time.Second):
				w.Header().Set("X-Progress", "in-progress")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusAccepted)
				return
			}
		}
	}

	if !s.authorizeExportJobAccess(w, r, job, "") {
		return
	}

	if job.PatientID != "" {
		setAuditPatientID(r.Context(), job.PatientID)
	}

	if job.Status == "failed" {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception",
			fmt.Sprintf("bulk export job %s failed: %s", jobID, job.Err))
		return
	}

	s.writeExportManifest(w, r, job)
}

// handleExportCancel handles DELETE /fhir/r4/{tenant}/$export-poll/{jobId}.
func (s *Server) handleExportCancel(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	jobID := r.PathValue("jobId")

	job := s.getExportJob(tenant, jobID)
	if job == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("export job %s not found", jobID))
		return
	}
	if job.done != nil {
		select {
		case <-job.done:
		case <-time.After(500 * time.Millisecond):
		}
	}

	if !s.authorizeExportJobAccess(w, r, job, "") {
		return
	}

	if job.PatientID != "" {
		setAuditPatientID(r.Context(), job.PatientID)
	}

	s.deleteExportJob(tenant, jobID)
	w.WriteHeader(http.StatusAccepted)
}

// handleExportFile handles GET /fhir/r4/{tenant}/$export-file/{jobId}/{fileName}.
func (s *Server) handleExportFile(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	jobID := r.PathValue("jobId")
	fileName := r.PathValue("fileName")

	job := s.getExportJob(tenant, jobID)
	if job == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("export job %s not found", jobID))
		return
	}
	if job.done != nil {
		select {
		case <-job.done:
		case <-time.After(2 * time.Second):
			w.Header().Set("X-Progress", "in-progress")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}

	resType := strings.TrimSuffix(fileName, ".ndjson")
	if !s.authorizeExportJobAccess(w, r, job, resType) {
		return
	}

	if job.PatientID != "" {
		setAuditPatientID(r.Context(), job.PatientID)
	}

	ndjsonBytes, ok := job.Files[resType]
	if !ok {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("export file %s not found in job %s", fileName, jobID))
		return
	}

	w.Header().Set("Content-Type", "application/fhir+ndjson; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ndjsonBytes)
}

func (s *Server) authorizeExportJobAccess(w http.ResponseWriter, r *http.Request, job *exportJob, resType string) bool {
	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if !hasSecCtx {
		return true
	}

	if auth.IsPatientRestricted(secCtx) {
		allowedPat := cleanID(secCtx.GetPatientId())
		if allowedPat == "" {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"patient-scoped token is missing required patient_id claim")
			return false
		}
		if job.PatientID == "" || cleanID(job.PatientID) != allowedPat {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot access export job %s", secCtx.GetPatientId(), job.ID))
			return false
		}
	}

	if resType != "" {
		if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("insufficient scope to download %s export file", resType))
			return false
		}
	} else {
		// Polling or cancelling a job requires read/search permission on at least one exported type (or Patient)
		allowed := auth.Allows(secCtx, "Patient", auth.ActionRead) || auth.Allows(secCtx, "Patient", auth.ActionSearch)
		if !allowed {
			for _, rt := range job.TypesOrder {
				if auth.Allows(secCtx, rt, auth.ActionRead) || auth.Allows(secCtx, rt, auth.ActionSearch) {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"insufficient scope to access export job")
			return false
		}
	}

	return true
}

func (s *Server) writeExportManifest(w http.ResponseWriter, r *http.Request, job *exportJob) {
	base := requestBaseURL(r)
	outputs := make([]map[string]any, 0, len(job.TypesOrder))
	for _, rt := range job.TypesOrder {
		outputs = append(outputs, map[string]any{
			"type":  rt,
			"url":   fmt.Sprintf("%s/fhir/r4/%s/$export-file/%s/%s.ndjson", base, job.TenantID, job.ID, rt),
			"count": job.Counts[rt],
		})
	}

	manifest := map[string]any{
		"transactionTime":     job.CreatedAt.Format(time.RFC3339),
		"request":             job.RequestURL,
		"requiresAccessToken": s.tokenValidator != nil,
		"output":              outputs,
		"error":               []any{},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(manifest)
}
