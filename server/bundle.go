package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/store/postgres"
)

// bundleRequest is the JSON structure of a FHIR Bundle.
type bundleRequest struct {
	ResourceType string        `json:"resourceType"`
	Type         string        `json:"type"` // "transaction" or "batch"
	Entry        []bundleEntry `json:"entry"`
}

// bundleEntry is a single entry in a FHIR Bundle.
type bundleEntry struct {
	FullURL  string              `json:"fullUrl,omitempty"`
	Resource json.RawMessage     `json:"resource"`
	Request  *bundleEntryRequest `json:"request,omitempty"`
}

// bundleEntryRequest is the request part of a Bundle entry.
type bundleEntryRequest struct {
	Method      string `json:"method"`                // PUT, POST, DELETE
	URL         string `json:"url"`                   // e.g. "Patient/123"
	IfMatch     string `json:"ifMatch,omitempty"`     // Optional ETag for optimistic concurrency (e.g. W/"1")
	IfNoneExist string `json:"ifNoneExist,omitempty"` // Optional search query for conditional create (e.g. "identifier=http://mrn|123")
}

// handleBundle handles POST /{tenant} — process a FHIR transaction/batch Bundle.
func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	secCtx, hasSecCtx := auth.FromContext(r.Context())

	body, err := io.ReadAll(io.LimitReader(r.Body, 50*1024*1024)) // 50MB max
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read body")
		return
	}

	var bundle bundleRequest
	if err := json.Unmarshal(body, &bundle); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("invalid Bundle JSON: %v", err))
		return
	}

	if bundle.ResourceType != "Bundle" {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("expected resourceType Bundle, got %s", bundle.ResourceType))
		return
	}

	if bundle.Type != "transaction" && bundle.Type != "batch" {
		writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
			fmt.Sprintf("unsupported bundle type: %s (expected transaction or batch)", bundle.Type))
		return
	}

	isTransaction := bundle.Type == "transaction"
	ops := make([]postgres.BundleOperation, 0, len(bundle.Entry))
	responseEntries := make([]map[string]any, 0, len(bundle.Entry))

	// Pre-pass: assign server IDs for urn:uuid: fullUrl entries so intra-bundle
	// (including forward) urn:uuid: references can be rewritten before validation and indexing.
	urnMap := make(map[string]string)
	preAssignedIDs := make([]string, len(bundle.Entry))
	for i, entry := range bundle.Entry {
		if !strings.HasPrefix(entry.FullURL, "urn:uuid:") {
			continue
		}
		method := "PUT"
		resType := ""
		resID := ""
		if entry.Request != nil {
			if entry.Request.Method != "" {
				method = strings.ToUpper(strings.TrimSpace(entry.Request.Method))
			}
			cleanURL := strings.Trim(entry.Request.URL, "/")
			if cleanURL != "" {
				parts := strings.SplitN(cleanURL, "/", 2)
				resType = parts[0]
				if len(parts) == 2 {
					resID = parts[1]
				}
			}
		}
		if method != "POST" && method != "PUT" {
			continue
		}
		bodyID := ""
		if len(entry.Resource) > 0 && string(entry.Resource) != "null" {
			var rawMap map[string]any
			if err := json.Unmarshal(entry.Resource, &rawMap); err == nil {
				if rt, ok := rawMap["resourceType"].(string); ok && rt != "" {
					resType = rt
				}
				switch idVal := rawMap["id"].(type) {
				case string:
					bodyID = idVal
				case map[string]any:
					if v, ok := idVal["value"].(string); ok {
						bodyID = v
					}
				}
			}
		}
		if resType == "" {
			continue
		}
		if strings.HasPrefix(resID, "urn:uuid:") {
			resID = ""
		}
		if strings.HasPrefix(bodyID, "urn:uuid:") {
			bodyID = ""
		}
		if resID == "" {
			resID = bodyID
		}
		if resID == "" {
			resID = generateID()
		}
		preAssignedIDs[i] = resID
		urnMap[entry.FullURL] = resType + "/" + resID
	}

	for i, entry := range bundle.Entry {
		method := "PUT"
		resTypeFromURL := ""
		resID := ""
		expectedVersion := 0

		if entry.Request != nil {
			if entry.Request.Method != "" {
				method = strings.ToUpper(strings.TrimSpace(entry.Request.Method))
			}
			cleanURL := strings.Trim(entry.Request.URL, "/")
			if cleanURL != "" {
				parts := strings.SplitN(cleanURL, "/", 2)
				resTypeFromURL = parts[0]
				if len(parts) == 2 {
					resID = parts[1]
				}
			}
			if strings.TrimSpace(entry.Request.IfMatch) != "" {
				v, err := postgres.ParseETagVersion(entry.Request.IfMatch)
				if err != nil {
					if isTransaction {
						writeOperationOutcome(w, http.StatusBadRequest, "invalid",
							fmt.Sprintf("entry[%d]: invalid ifMatch %q", i, entry.Request.IfMatch))
						return
					}
					responseEntries = append(responseEntries, bundleErrorResponse(
						http.StatusBadRequest, fmt.Sprintf("entry[%d]: invalid ifMatch %q", i, entry.Request.IfMatch)))
					continue
				}
				expectedVersion = v
			}
		}

		if method != "POST" && method != "PUT" && method != "DELETE" {
			msg := fmt.Sprintf("entry[%d]: unsupported bundle request method %q (expected POST, PUT, or DELETE)", i, method)
			if isTransaction {
				writeOperationOutcome(w, http.StatusBadRequest, "not-supported", msg)
				return
			}
			responseEntries = append(responseEntries, bundleErrorResponse(http.StatusBadRequest, msg))
			continue
		}

		resType := resTypeFromURL
		if len(entry.Resource) > 0 && string(entry.Resource) != "null" {
			if len(urnMap) > 0 {
				if rewritten, err := fhirutil.RewriteURNReferences(entry.Resource, urnMap); err == nil {
					entry.Resource = rewritten
				}
			}
			var resourceMeta struct {
				ResourceType string `json:"resourceType"`
			}
			if err := json.Unmarshal(entry.Resource, &resourceMeta); err != nil {
				if isTransaction {
					writeOperationOutcome(w, http.StatusBadRequest, "structure",
						fmt.Sprintf("entry[%d]: invalid resource JSON", i))
					return
				}
				responseEntries = append(responseEntries, bundleErrorResponse(
					http.StatusBadRequest, fmt.Sprintf("entry[%d]: invalid resource JSON", i)))
				continue
			}
			if resourceMeta.ResourceType != "" {
				resType = resourceMeta.ResourceType
			}
		}

		if resType == "" {
			if isTransaction {
				writeOperationOutcome(w, http.StatusBadRequest, "required",
					fmt.Sprintf("entry[%d]: missing resourceType", i))
				return
			}
			responseEntries = append(responseEntries, bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: missing resourceType", i)))
			continue
		}

		// Per-entry SMART scope verification
		if hasSecCtx {
			var requiredAction auth.Action
			switch method {
			case "POST":
				requiredAction = auth.ActionCreate
			case "PUT":
				requiredAction = auth.ActionUpdate
			case "DELETE":
				requiredAction = auth.ActionDelete
			default:
				requiredAction = auth.ActionUpdate
			}
			if !auth.Allows(secCtx, resType, requiredAction) {
				msg := fmt.Sprintf("entry[%d]: insufficient scope for %s on %s", i, method, resType)
				if isTransaction {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
					return
				}
				responseEntries = append(responseEntries, bundleErrorResponse(http.StatusForbidden, msg))
				continue
			}
		}

		// Handle DELETE
		if method == "DELETE" {
			if resID == "" {
				if isTransaction {
					writeOperationOutcome(w, http.StatusBadRequest, "required",
						fmt.Sprintf("entry[%d]: DELETE requires resource ID", i))
					return
				}
				responseEntries = append(responseEntries, bundleErrorResponse(
					http.StatusBadRequest, fmt.Sprintf("entry[%d]: DELETE requires resource ID", i)))
				continue
			}

			if hasSecCtx && auth.IsPatientRestricted(secCtx) {
				exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), tenant, resType, resID, secCtx.GetPatientId())
				if err != nil {
					if isTransaction {
						writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
						return
					}
					responseEntries = append(responseEntries, bundleErrorResponse(http.StatusInternalServerError, "internal security check error"))
					continue
				}
				if !exists || !allowed {
					msg := fmt.Sprintf("entry[%d]: patient-scoped token (%s) cannot delete %s/%s outside patient compartment",
						i, secCtx.GetPatientId(), resType, resID)
					if isTransaction {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
						return
					}
					responseEntries = append(responseEntries, bundleErrorResponse(http.StatusForbidden, msg))
					continue
				}
			}

			if isTransaction {
				ops = append(ops, postgres.BundleOperation{
					Method: "DELETE",
					Input: postgres.ResourceInput{
						TenantID: tenant,
						ResType:  resType,
						ResID:    resID,
					},
				})
				continue
			}

			// Batch mode: execute immediately
			if err := s.store.DeleteResource(r.Context(), tenant, resType, resID); err != nil {
				responseEntries = append(responseEntries, bundleErrorResponse(
					http.StatusInternalServerError, fmt.Sprintf("entry[%d]: delete failed: %v", i, err)))
				continue
			}
			responseEntries = append(responseEntries, map[string]any{
				"response": map[string]any{
					"status": "204 No Content",
				},
			})
			continue
		}

		// Validate resource entry against StructureDefinitions and ValueSets if validator configured
		if s.validator != nil && (method == "POST" || method == "PUT") {
			outcome := s.validator.ValidateJSON(resType, entry.Resource)
			if !outcome.IsValid() {
				if isTransaction {
					outcome.WriteHTTP(w, http.StatusBadRequest)
					return
				}
				responseEntries = append(responseEntries, map[string]any{
					"response": map[string]any{
						"status":  "400 Bad Request",
						"outcome": outcome.ToOperationOutcomeMap(),
					},
				})
				continue
			}
		}

		// For PUT/POST: convert JSON → proto → bytes
		md, ok := s.protoRegistry[resType]
		if !ok {
			if isTransaction {
				writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
					fmt.Sprintf("entry[%d]: resource type %s not registered", i, resType))
				return
			}
			responseEntries = append(responseEntries, bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: resource type %s not registered", i, resType)))
			continue
		}

		normalizedEntry, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, entry.Resource)
		if err != nil {
			if isTransaction {
				writeOperationOutcome(w, http.StatusBadRequest, "structure",
					fmt.Sprintf("entry[%d]: invalid %s JSON: %v", i, resType, err))
				return
			}
			responseEntries = append(responseEntries, bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: invalid %s JSON: %v", i, resType, err)))
			continue
		}

		msg := dynamicpb.NewMessage(md)
		unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
		if err := unmarshaler.Unmarshal(normalizedEntry, msg); err != nil {
			if isTransaction {
				writeOperationOutcome(w, http.StatusBadRequest, "structure",
					fmt.Sprintf("entry[%d]: invalid %s JSON: %v", i, resType, err))
				return
			}
			responseEntries = append(responseEntries, bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: invalid %s JSON: %v", i, resType, err)))
			continue
		}

		// Extract ID from proto if not in request URL
		bodyID := extractProtoResourceID(msg)
		if strings.HasPrefix(bodyID, "urn:uuid:") {
			bodyID = ""
		}
		if method == "PUT" && resID != "" && bodyID != "" && bodyID != resID {
			if isTransaction {
				writeOperationOutcome(w, http.StatusBadRequest, "invariant",
					fmt.Sprintf("entry[%d]: resource id %q in body does not match URL id %q", i, bodyID, resID))
				return
			}
			responseEntries = append(responseEntries, bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: resource id %q in body does not match URL id %q", i, bodyID, resID)))
			continue
		}
		if resID == "" {
			resID = preAssignedIDs[i]
		}
		if resID == "" {
			resID = bodyID
		}
		if resID == "" {
			resID = generateID()
		}
		setProtoResourceID(msg, resID)

		protoBytes, err := proto.Marshal(msg)
		if err != nil {
			if isTransaction {
				writeOperationOutcome(w, http.StatusInternalServerError, "exception",
					fmt.Sprintf("entry[%d]: proto encode failed", i))
				return
			}
			responseEntries = append(responseEntries, bundleErrorResponse(
				http.StatusInternalServerError, fmt.Sprintf("entry[%d]: proto encode failed", i)))
			continue
		}

		var idx *postgres.SearchIndexes
		if extractor, ok := s.extractors[resType]; ok {
			var extractErr error
			idx, extractErr = extractor(tenant, resID, protoBytes)
			if extractErr != nil {
				s.logger.Warn("index extraction failed in bundle, storing without indexes",
					"type", resType, "id", resID, "error", extractErr)
			}
		}

		// Enforce patient compartment isolation on existing target (PUT) and incoming payload (POST/PUT)
		if hasSecCtx && auth.IsPatientRestricted(secCtx) {
			allowedPatientID := cleanID(secCtx.GetPatientId())
			if method == "PUT" && (strings.EqualFold(resType, "Patient") || postgres.IsPatientCompartmentResourceType(resType)) && s.store != nil {
				exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), tenant, resType, resID, allowedPatientID)
				if err != nil {
					if isTransaction {
						writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
						return
					}
					responseEntries = append(responseEntries, bundleErrorResponse(http.StatusInternalServerError, "internal security check error"))
					continue
				}
				if exists && !allowed {
					msg := fmt.Sprintf("entry[%d]: patient-scoped token (%s) cannot update %s/%s belonging to another patient",
						i, secCtx.GetPatientId(), resType, resID)
					if isTransaction {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
						return
					}
					responseEntries = append(responseEntries, bundleErrorResponse(http.StatusForbidden, msg))
					continue
				}
			}
			if strings.EqualFold(resType, "Patient") || postgres.IsPatientCompartmentResourceType(resType) {
				if !postgres.IndexesBelongToPatientCompartment(resType, resID, idx, allowedPatientID) {
					msg := fmt.Sprintf("entry[%d]: patient-scoped token (%s) cannot write %s/%s outside patient compartment",
						i, secCtx.GetPatientId(), resType, resID)
					if isTransaction {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
						return
					}
					responseEntries = append(responseEntries, bundleErrorResponse(http.StatusForbidden, msg))
					continue
				}
			}
		}

		var condParams *postgres.SearchParams
		if method == "POST" && entry.Request != nil && strings.TrimSpace(entry.Request.IfNoneExist) != "" {
			parsed, err := postgres.ParseIfNoneExist(tenant, resType, entry.Request.IfNoneExist)
			if err != nil {
				if isTransaction {
					writeOperationOutcome(w, http.StatusBadRequest, "invalid",
						fmt.Sprintf("entry[%d]: %v", i, err))
					return
				}
				responseEntries = append(responseEntries, bundleErrorResponse(
					http.StatusBadRequest, fmt.Sprintf("entry[%d]: %v", i, err)))
				continue
			}
			if hasSecCtx && auth.IsPatientRestricted(secCtx) {
				allowedPatientID := cleanID(secCtx.GetPatientId())
				if strings.EqualFold(resType, "Patient") {
					parsed.ID = allowedPatientID
				} else if postgres.IsPatientCompartmentResourceType(resType) {
					if parsed.References == nil {
						parsed.References = make(map[string]string)
					}
					if parsed.References["subject"] == "" {
						parsed.References["patient"] = "Patient/" + allowedPatientID
					}
				}
			}
			condParams = &parsed
		}

		input := postgres.ResourceInput{
			TenantID:        tenant,
			ResType:         resType,
			ResID:           resID,
			ResourceProto:   protoBytes,
			SearchIndexes:   idx,
			ExpectedVersion: expectedVersion,
			CreateOnly:      method == "POST",
		}

		if isTransaction {
			ops = append(ops, postgres.BundleOperation{
				Method:      method,
				Input:       input,
				IfNoneExist: condParams,
			})
			continue
		}

		// Batch mode: execute entry independently
		if condParams != nil && s.store != nil {
			rec, created, err := s.store.ConditionalCreateResource(r.Context(), input, *condParams)
			if errors.Is(err, postgres.ErrPreconditionFailed) {
				responseEntries = append(responseEntries, bundleErrorResponse(http.StatusPreconditionFailed, err.Error()))
				continue
			}
			if errors.Is(err, postgres.ErrResourceAlreadyExists) {
				responseEntries = append(responseEntries, bundleErrorResponse(http.StatusConflict, err.Error()))
				continue
			}
			if err != nil {
				responseEntries = append(responseEntries, bundleErrorResponse(http.StatusInternalServerError, err.Error()))
				continue
			}
			status := "200 OK"
			if created {
				status = "201 Created"
			}
			responseEntries = append(responseEntries, map[string]any{
				"fullUrl": fmt.Sprintf("%s/%s", resType, rec.ResID),
				"response": map[string]any{
					"status":   status,
					"location": fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, rec.ResID),
					"etag":     postgres.FormatETag(rec.ResVersion),
				},
			})
			continue
		}

		if s.store != nil {
			rec, err := s.store.WriteResourceWithMeta(r.Context(), input)
			if errors.Is(err, postgres.ErrVersionConflict) {
				responseEntries = append(responseEntries, bundleErrorResponse(http.StatusPreconditionFailed, err.Error()))
				continue
			}
			if errors.Is(err, postgres.ErrResourceAlreadyExists) {
				responseEntries = append(responseEntries, bundleErrorResponse(http.StatusConflict, err.Error()))
				continue
			}
			if err != nil {
				responseEntries = append(responseEntries, bundleErrorResponse(http.StatusInternalServerError, err.Error()))
				continue
			}
			status := "200 OK"
			if method == "POST" || rec.Created {
				status = "201 Created"
			}
			responseEntries = append(responseEntries, map[string]any{
				"fullUrl": fmt.Sprintf("%s/%s", resType, rec.ResID),
				"response": map[string]any{
					"status":   status,
					"location": fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, rec.ResID),
					"etag":     postgres.FormatETag(rec.ResVersion),
				},
			})
		}
	}

	// Execute all operations atomically in a single SQL transaction for transaction Bundles
	if isTransaction && len(ops) > 0 && s.store != nil {
		results, err := s.store.ExecuteTransactionBundle(r.Context(), ops)
		if errors.Is(err, postgres.ErrVersionConflict) || errors.Is(err, postgres.ErrPreconditionFailed) {
			writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", err.Error())
			return
		}
		if errors.Is(err, postgres.ErrResourceAlreadyExists) {
			writeOperationOutcome(w, http.StatusConflict, "duplicate", err.Error())
			return
		}
		if err != nil {
			s.logger.Error("bundle transaction failed", "error", err, "ops", len(ops))
			writeOperationOutcome(w, http.StatusInternalServerError, "exception",
				fmt.Sprintf("transaction bundle failed: %v", err))
			return
		}

		for idxOp, res := range results {
			op := ops[idxOp]
			if op.Method == "DELETE" {
				respMap := map[string]any{
					"status": "204 No Content",
				}
				if res.Record != nil && res.Record.ResVersion > 0 {
					respMap["etag"] = postgres.FormatETag(res.Record.ResVersion)
				}
				responseEntries = append(responseEntries, map[string]any{
					"response": respMap,
				})
				continue
			}

			rec := res.Record
			status := res.Status
			if status == "" {
				status = "200 OK"
				if op.Method == "POST" && res.Created {
					status = "201 Created"
				}
			}
			respMap := map[string]any{
				"status":   status,
				"location": fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, rec.ResType, rec.ResID),
			}
			if rec.ResVersion > 0 {
				respMap["etag"] = postgres.FormatETag(rec.ResVersion)
			}
			responseEntries = append(responseEntries, map[string]any{
				"fullUrl":  fmt.Sprintf("%s/%s", rec.ResType, rec.ResID),
				"response": respMap,
			})
		}
	}

	// Build response Bundle
	responseBundle := map[string]any{
		"resourceType": "Bundle",
		"type":         bundle.Type + "-response",
		"entry":        responseEntries,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(responseBundle)
}

func bundleErrorResponse(status int, message string) map[string]any {
	return map[string]any{
		"response": map[string]any{
			"status": fmt.Sprintf("%d %s", status, http.StatusText(status)),
			"outcome": map[string]any{
				"resourceType": "OperationOutcome",
				"issue": []map[string]any{
					{"severity": "error", "code": "exception", "diagnostics": message},
				},
			},
		},
	}
}
