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
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/auth"
	automq "github.com/flint-fhir/flint/pkg/cdc"
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
	Method          string `json:"method"`                    // PUT, POST, PATCH, DELETE, GET, HEAD
	URL             string `json:"url"`                       // e.g. "Patient/123"
	IfMatch         string `json:"ifMatch,omitempty"`         // Optional ETag for optimistic concurrency (e.g. W/"1")
	IfNoneMatch     string `json:"ifNoneMatch,omitempty"`     // Optional ETag for conditional read (e.g. W/"1" or *)
	IfModifiedSince string `json:"ifModifiedSince,omitempty"` // Optional timestamp for conditional read
	IfNoneExist     string `json:"ifNoneExist,omitempty"`     // Optional search query for conditional create (e.g. "identifier=http://mrn|123")
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
	preferReturn := parsePreferReturn(r)
	ops := make([]postgres.BundleOperation, 0, len(bundle.Entry))
	opEntryIdx := make([]int, 0, len(bundle.Entry))
	txGetIndices := make([]int, 0)
	responseEntries := make([]map[string]any, len(bundle.Entry))
	inBundleIndexed := make([]inBundleIndexedResource, 0, len(bundle.Entry))

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
				pathPart, _, _ := strings.Cut(cleanURL, "?")
				parts := strings.SplitN(strings.Trim(pathPart, "/"), "/", 2)
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

	// Per FHIR R4 §3.1.0.10.1, transaction Bundles execute in order:
	// 1. DELETE -> 2. POST -> 3. PUT/PATCH -> 4. GET/HEAD,
	// while preserving the original entry order in the returned Bundle.entry array.
	order := make([]int, len(bundle.Entry))
	for i := range bundle.Entry {
		order[i] = i
	}
	if isTransaction {
		slices.SortStableFunc(order, func(a, b int) int {
			return bundleEntryMethodPriority(bundle.Entry[a]) - bundleEntryMethodPriority(bundle.Entry[b])
		})
	}

	for _, i := range order {
		entry := bundle.Entry[i]
		method := "PUT"
		resTypeFromURL := ""
		resID := ""
		urlQuery := ""
		expectedVersion := 0

		if entry.Request != nil {
			if entry.Request.Method != "" {
				method = strings.ToUpper(strings.TrimSpace(entry.Request.Method))
			}
			cleanURL := strings.Trim(entry.Request.URL, "/")
			if strings.HasPrefix(cleanURL, "fhir/r4/"+tenant+"/") {
				cleanURL = strings.TrimPrefix(cleanURL, "fhir/r4/"+tenant+"/")
			}
			if cleanURL != "" {
				pathPart, qPart, hasQuery := strings.Cut(cleanURL, "?")
				if hasQuery {
					urlQuery = qPart
				}
				parts := strings.SplitN(strings.Trim(pathPart, "/"), "/", 2)
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
					responseEntries[i] = bundleErrorResponse(
						http.StatusBadRequest, fmt.Sprintf("entry[%d]: invalid ifMatch %q", i, entry.Request.IfMatch))
					continue
				}
				expectedVersion = v
			}
		}

		if method != "POST" && method != "PUT" && method != "PATCH" && method != "DELETE" && method != "GET" && method != "HEAD" {
			msg := fmt.Sprintf("entry[%d]: unsupported bundle request method %q (expected POST, PUT, PATCH, DELETE, GET, or HEAD)", i, method)
			if isTransaction {
				writeOperationOutcome(w, http.StatusBadRequest, "not-supported", msg)
				return
			}
			responseEntries[i] = bundleErrorResponse(http.StatusBadRequest, msg)
			continue
		}

		if method == "GET" || method == "HEAD" {
			if method == "HEAD" && len(bytes.TrimSpace(entry.Resource)) > 0 && string(bytes.TrimSpace(entry.Resource)) != "null" {
				msg := fmt.Sprintf("entry[%d]: HEAD request in Bundle must not include a resource payload", i)
				if isTransaction {
					writeOperationOutcome(w, http.StatusBadRequest, "invalid", msg)
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusBadRequest, msg)
				continue
			}
			if entry.Request == nil || strings.TrimSpace(entry.Request.URL) == "" {
				msg := fmt.Sprintf("entry[%d]: %s requires non-empty request.url", i, method)
				if isTransaction {
					writeOperationOutcome(w, http.StatusBadRequest, "required", msg)
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusBadRequest, msg)
				continue
			}
			if hasSecCtx {
				if auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) == "" {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden", "patient-scoped token is missing required patient_id claim")
					return
				}
				if resTypeFromURL != "" && !strings.HasPrefix(resTypeFromURL, "$") && resTypeFromURL != "_history" && resTypeFromURL != "metadata" {
					allowed := false
					if resID != "" {
						allowed = auth.Allows(secCtx, resTypeFromURL, auth.ActionRead)
					} else {
						allowed = auth.Allows(secCtx, resTypeFromURL, auth.ActionSearch) || auth.Allows(secCtx, resTypeFromURL, auth.ActionRead)
					}
					if !allowed {
						msg := fmt.Sprintf("entry[%d]: insufficient scope for %s on %s", i, method, resTypeFromURL)
						if isTransaction {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
							return
						}
						responseEntries[i] = bundleErrorResponse(http.StatusForbidden, msg)
						continue
					}
				}
			}
			if isTransaction {
				txGetIndices = append(txGetIndices, i)
				continue
			}
			getResp, _ := s.executeBundleGetEntry(r, tenant, entry.Request, urnMap)
			responseEntries[i] = getResp
			continue
		}

		resType := resTypeFromURL
		if method != "PATCH" && len(entry.Resource) > 0 && string(entry.Resource) != "null" {
			if len(urnMap) > 0 {
				if rewritten, err := fhirutil.RewriteURNReferences(entry.Resource, urnMap); err == nil {
					entry.Resource = rewritten
				}
			}
			resolvedRes, refStatus, refErr := s.resolveConditionalReferencesInJSON(r.Context(), tenant, entry.Resource, inBundleIndexed)
			if refErr != nil {
				msg := fmt.Sprintf("entry[%d]: %v", i, refErr)
				if isTransaction {
					writeOperationOutcome(w, refStatus, conditionalRefOutcomeCode(refStatus), msg)
					return
				}
				responseEntries[i] = bundleErrorResponse(refStatus, msg)
				continue
			}
			entry.Resource = resolvedRes
			var resourceMeta struct {
				ResourceType string `json:"resourceType"`
			}
			if err := json.Unmarshal(entry.Resource, &resourceMeta); err != nil {
				if isTransaction {
					writeOperationOutcome(w, http.StatusBadRequest, "structure",
						fmt.Sprintf("entry[%d]: invalid resource JSON", i))
					return
				}
				responseEntries[i] = bundleErrorResponse(
					http.StatusBadRequest, fmt.Sprintf("entry[%d]: invalid resource JSON", i))
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
			responseEntries[i] = bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: missing resourceType", i))
			continue
		}

		// Per-entry SMART scope verification
		if hasSecCtx {
			if auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) == "" {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden", "patient-scoped token is missing required patient_id claim")
				return
			}
			var requiredAction auth.Action
			switch method {
			case "POST":
				requiredAction = auth.ActionCreate
			case "PUT", "PATCH":
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
				}
				responseEntries[i] = bundleErrorResponse(http.StatusForbidden, msg)
				if isTransaction {
					return
				}
				continue
			}
			if auth.IsPatientRestricted(secCtx) && !strings.EqualFold(resType, "Patient") && !postgres.IsPatientCompartmentResourceType(resType) {
				msg := fmt.Sprintf("entry[%d]: patient-scoped token (%s) cannot %s non-compartment resource %s", i, secCtx.GetPatientId(), method, resType)
				if isTransaction {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusForbidden, msg)
				continue
			}
		}

		// Parse Conditional PUT / Conditional PATCH / Conditional DELETE when urlQuery is present
		var entryCondParams *postgres.SearchParams
		if urlQuery != "" && (method == "PUT" || method == "PATCH" || method == "DELETE") && s.store != nil {
			condParams, err := postgres.ParseIfNoneExist(tenant, resType, urlQuery)
			if err != nil {
				if isTransaction {
					writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("entry[%d]: %v", i, err))
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusBadRequest, fmt.Sprintf("entry[%d]: %v", i, err))
				continue
			}
			if hasSecCtx && auth.IsPatientRestricted(secCtx) {
				if err := validateAndScopeIfNoneExistForPatient(&condParams, resType, cleanID(secCtx.GetPatientId()), secCtx.GetPatientId()); err != nil {
					if isTransaction {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden", fmt.Sprintf("entry[%d]: %v", i, err))
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusForbidden, fmt.Sprintf("entry[%d]: %v", i, err))
					continue
				}
			}
			if isTransaction && method != "PATCH" {
				entryCondParams = &condParams
			} else {
				condParams.Count = 2
				sr, err := s.store.Search(r.Context(), condParams)
				if err != nil {
					if isTransaction {
						writeOperationOutcome(w, http.StatusInternalServerError, "exception", fmt.Sprintf("entry[%d]: conditional search failed", i))
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusInternalServerError, fmt.Sprintf("entry[%d]: conditional search failed", i))
					continue
				}
				if sr.Total > 1 {
					msg := fmt.Sprintf("entry[%d]: conditional %s matched %d resources (expected 0 or 1)", i, method, sr.Total)
					if isTransaction {
						writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", msg)
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusPreconditionFailed, msg)
					continue
				}
				if sr.Total == 1 {
					resID = sr.Matches[0].ResID
				} else if method == "PATCH" {
					msg := fmt.Sprintf("entry[%d]: conditional PATCH matched 0 %s resources", i, resType)
					if isTransaction {
						writeOperationOutcome(w, http.StatusNotFound, "not-found", msg)
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusNotFound, msg)
					continue
				} else if method == "DELETE" {
					if expectedVersion > 0 {
						msg := fmt.Sprintf("entry[%d]: conditional DELETE matched 0 %s resources (expected version %d)", i, resType, expectedVersion)
						if isTransaction {
							writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", msg)
							return
						}
						responseEntries[i] = bundleErrorResponse(http.StatusPreconditionFailed, msg)
						continue
					}
					// 0 matches on conditional DELETE is an idempotent 204 No Content
					responseEntries[i] = map[string]any{
						"response": map[string]any{"status": "204 No Content"},
					}
					continue
				}
			}
		}

		// Handle PATCH entry by reading current resource, applying RFC 6902 / RFC 7396 patch, and converting to PUT
		if method == "PATCH" {
			if resID == "" || s.store == nil {
				msg := fmt.Sprintf("entry[%d]: PATCH requires resource ID and active store", i)
				if isTransaction {
					writeOperationOutcome(w, http.StatusBadRequest, "required", msg)
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusBadRequest, msg)
				continue
			}
			if _, ok := s.protoRegistry[resType]; !ok {
				msg := fmt.Sprintf("entry[%d]: resource type %s not registered", i, resType)
				if isTransaction {
					writeOperationOutcome(w, http.StatusBadRequest, "not-supported", msg)
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusBadRequest, msg)
				continue
			}
			if hasSecCtx && auth.IsPatientRestricted(secCtx) {
				exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), tenant, resType, resID, secCtx.GetPatientId())
				if err != nil {
					if isTransaction {
						writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusInternalServerError, "internal security check error")
					continue
				}
				if exists && !allowed {
					msg := fmt.Sprintf("entry[%d]: patient-scoped token (%s) cannot patch %s/%s outside patient compartment",
						i, secCtx.GetPatientId(), resType, resID)
					if isTransaction {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusForbidden, msg)
					continue
				}
			}
			currentRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
			if errors.Is(err, sql.ErrNoRows) {
				msg := fmt.Sprintf("entry[%d]: %s/%s not found", i, resType, resID)
				if isTransaction {
					writeOperationOutcome(w, http.StatusNotFound, "not-found", msg)
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusNotFound, msg)
				continue
			}
			if errors.Is(err, postgres.ErrResourceDeleted) {
				msg := fmt.Sprintf("entry[%d]: %s/%s has been deleted", i, resType, resID)
				if isTransaction {
					writeOperationOutcome(w, http.StatusGone, "deleted", msg)
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusGone, msg)
				continue
			}
			if err != nil {
				if isTransaction {
					writeOperationOutcome(w, http.StatusInternalServerError, "exception", err.Error())
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusInternalServerError, err.Error())
				continue
			}
			if hasSecCtx {
				if constraints, constrained := auth.ScopeQueryConstraints(secCtx, resType, auth.ActionUpdate); constrained {
					if !s.resourceSatisfiesScopeConstraints(tenant, resType, resID, currentRec.ResourceProto, constraints) {
						msg := fmt.Sprintf("entry[%d]: insufficient granular SMART v2 scope to patch %s/%s", i, resType, resID)
						if isTransaction {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
							return
						}
						responseEntries[i] = bundleErrorResponse(http.StatusForbidden, msg)
						continue
					}
				}
			}
			if expectedVersion > 0 && currentRec.ResVersion != expectedVersion {
				msg := fmt.Sprintf("entry[%d]: version conflict on %s/%s", i, resType, resID)
				if isTransaction {
					writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", msg)
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusPreconditionFailed, msg)
				continue
			}
			currentJSON, err := s.protoToJSONWithMeta(resType, currentRec.ResourceProto, 0, time.Time{})
			if err != nil {
				if isTransaction {
					writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to decode existing resource")
					return
				}
				responseEntries[i] = bundleErrorResponse(http.StatusInternalServerError, "failed to decode existing resource")
				continue
			}
			trimmedPatch := bytes.TrimSpace(entry.Resource)
			var patchedJSON []byte
			if len(trimmedPatch) > 0 && trimmedPatch[0] == '[' {
				var status int
				patchedJSON, status, err = ApplyJSONPatch(currentJSON, trimmedPatch)
				if err != nil {
					if isTransaction {
						writeOperationOutcome(w, status, "invalid", fmt.Sprintf("entry[%d]: %v", i, err))
						return
					}
					responseEntries[i] = bundleErrorResponse(status, fmt.Sprintf("entry[%d]: %v", i, err))
					continue
				}
			} else if isFHIRParametersPatch(trimmedPatch) {
				var status int
				patchedJSON, status, err = ApplyFHIRPatch(currentJSON, trimmedPatch)
				if err != nil {
					if isTransaction {
						writeOperationOutcome(w, status, "invalid", fmt.Sprintf("entry[%d]: %v", i, err))
						return
					}
					responseEntries[i] = bundleErrorResponse(status, fmt.Sprintf("entry[%d]: %v", i, err))
					continue
				}
			} else {
				patchedJSON, err = ApplyMergePatch(currentJSON, trimmedPatch)
				if err != nil {
					if isTransaction {
						writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("entry[%d]: %v", i, err))
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusBadRequest, fmt.Sprintf("entry[%d]: %v", i, err))
					continue
				}
			}
			var patchedMap map[string]any
			if err := json.Unmarshal(patchedJSON, &patchedMap); err == nil {
				if rt, ok := patchedMap["resourceType"].(string); !ok || rt != resType {
					msg := fmt.Sprintf("entry[%d]: PATCH cannot change resourceType from %q to %v", i, resType, patchedMap["resourceType"])
					if isTransaction {
						writeOperationOutcome(w, http.StatusBadRequest, "invariant", msg)
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusBadRequest, msg)
					continue
				}
				if rawID, hasID := patchedMap["id"]; hasID && rawID != nil {
					idStr, ok := rawID.(string)
					if !ok || (idStr != "" && idStr != resID) {
						msg := fmt.Sprintf("entry[%d]: PATCH cannot change resource id from %q to %v", i, resID, rawID)
						if isTransaction {
							writeOperationOutcome(w, http.StatusBadRequest, "invariant", msg)
							return
						}
						responseEntries[i] = bundleErrorResponse(http.StatusBadRequest, msg)
						continue
					}
				}
				patchedMap["id"] = resID
				if remars, err := json.Marshal(patchedMap); err == nil {
					patchedJSON = remars
				}
			}
			resolvedPatch, refStatus, refErr := s.resolveConditionalReferencesInJSON(r.Context(), tenant, patchedJSON, inBundleIndexed)
			if refErr != nil {
				msg := fmt.Sprintf("entry[%d]: %v", i, refErr)
				if isTransaction {
					writeOperationOutcome(w, refStatus, conditionalRefOutcomeCode(refStatus), msg)
					return
				}
				responseEntries[i] = bundleErrorResponse(refStatus, msg)
				continue
			}
			entry.Resource = resolvedPatch
			method = "PUT"
		}

		// Handle DELETE
		if method == "DELETE" {
			if isTransaction && entryCondParams != nil {
				ops = append(ops, postgres.BundleOperation{
					Method: "DELETE",
					Input: postgres.ResourceInput{
						TenantID:        tenant,
						ResType:         resType,
						ExpectedVersion: expectedVersion,
					},
					ConditionalParams: entryCondParams,
					VerifyDeleteMatch: func(matchedID string) error {
						if hasSecCtx && auth.IsPatientRestricted(secCtx) {
							exists, allowed, cErr := s.checkExistingResourceCompartment(r.Context(), tenant, resType, matchedID, secCtx.GetPatientId())
							if cErr != nil {
								return cErr
							}
							if !exists || !allowed {
								return fmt.Errorf("%w: %s/%s", errForbiddenCompartment, resType, matchedID)
							}
						}
						return nil
					},
				})
				opEntryIdx = append(opEntryIdx, i)
				continue
			}

			if resID == "" {
				if isTransaction {
					writeOperationOutcome(w, http.StatusBadRequest, "required",
						fmt.Sprintf("entry[%d]: DELETE requires resource ID", i))
					return
				}
				responseEntries[i] = bundleErrorResponse(
					http.StatusBadRequest, fmt.Sprintf("entry[%d]: DELETE requires resource ID", i))
				continue
			}

			if hasSecCtx && auth.IsPatientRestricted(secCtx) {
				exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), tenant, resType, resID, secCtx.GetPatientId())
				if err != nil {
					if isTransaction {
						writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusInternalServerError, "internal security check error")
					continue
				}
				if !exists || !allowed {
					msg := fmt.Sprintf("entry[%d]: patient-scoped token (%s) cannot delete %s/%s outside patient compartment",
						i, secCtx.GetPatientId(), resType, resID)
					if isTransaction {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusForbidden, msg)
					continue
				}
			}

			if isTransaction {
				ops = append(ops, postgres.BundleOperation{
					Method: "DELETE",
					Input: postgres.ResourceInput{
						TenantID:        tenant,
						ResType:         resType,
						ResID:           resID,
						ExpectedVersion: expectedVersion,
					},
				})
				opEntryIdx = append(opEntryIdx, i)
				continue
			}

			// Batch mode: execute immediately
			delRec, err := s.store.DeleteResourceWithExpectedVersion(r.Context(), tenant, resType, resID, expectedVersion)
			if errors.Is(err, postgres.ErrVersionConflict) {
				responseEntries[i] = bundleErrorResponse(http.StatusPreconditionFailed, err.Error())
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				responseEntries[i] = bundleErrorResponse(
					http.StatusInternalServerError, fmt.Sprintf("entry[%d]: delete failed: %v", i, err))
				continue
			}
			respMap := map[string]any{
				"status": "204 No Content",
			}
			if delRec != nil && delRec.ResVersion > 0 {
				respMap["etag"] = postgres.FormatETag(delRec.ResVersion)
				s.publishCDC(r.Context(), automq.Message{
					TenantID: tenant,
					ResType:  resType,
					ResID:    resID,
					Op:       "D",
				})
			}
			responseEntries[i] = map[string]any{
				"response": respMap,
			}
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
				responseEntries[i] = map[string]any{
					"response": map[string]any{
						"status":  "400 Bad Request",
						"outcome": outcome.ToOperationOutcomeMap(),
					},
				}
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
			responseEntries[i] = bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: resource type %s not registered", i, resType))
			continue
		}

		entry.Resource = normalizeSubscriptionOnWrite(resType, entry.Resource)

		normalizedEntry, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, entry.Resource)
		if err != nil {
			if isTransaction {
				writeOperationOutcome(w, http.StatusBadRequest, "structure",
					fmt.Sprintf("entry[%d]: invalid %s JSON: %v", i, resType, err))
				return
			}
			responseEntries[i] = bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: invalid %s JSON: %v", i, resType, err))
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
			responseEntries[i] = bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: invalid %s JSON: %v", i, resType, err))
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
			responseEntries[i] = bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: resource id %q in body does not match URL id %q", i, bodyID, resID))
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
			responseEntries[i] = bundleErrorResponse(
				http.StatusInternalServerError, fmt.Sprintf("entry[%d]: proto encode failed", i))
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
		inBundleIndexed = append(inBundleIndexed, inBundleIndexedResource{
			resType: resType,
			resID:   resID,
			idx:     idx,
		})
		setAuditPatientID(r.Context(), extractPatientIDFromIndexes(resType, resID, idx))

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
					responseEntries[i] = bundleErrorResponse(http.StatusInternalServerError, "internal security check error")
					continue
				}
				if exists && !allowed {
					msg := fmt.Sprintf("entry[%d]: patient-scoped token (%s) cannot update %s/%s belonging to another patient",
						i, secCtx.GetPatientId(), resType, resID)
					if isTransaction {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusForbidden, msg)
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
					responseEntries[i] = bundleErrorResponse(http.StatusForbidden, msg)
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
				responseEntries[i] = bundleErrorResponse(
					http.StatusBadRequest, fmt.Sprintf("entry[%d]: %v", i, err))
				continue
			}
			if hasSecCtx && auth.IsPatientRestricted(secCtx) {
				allowedPatientID := cleanID(secCtx.GetPatientId())
				if err := validateAndScopeIfNoneExistForPatient(&parsed, resType, allowedPatientID, secCtx.GetPatientId()); err != nil {
					msg := fmt.Sprintf("entry[%d]: %v", i, err)
					if isTransaction {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden", msg)
						return
					}
					responseEntries[i] = bundleErrorResponse(http.StatusForbidden, msg)
					continue
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
			bundleOp := postgres.BundleOperation{
				Method:            method,
				Input:             input,
				IfNoneExist:       condParams,
				ConditionalParams: entryCondParams,
			}
			if method == "PUT" && entryCondParams != nil {
				capturedBodyID := bodyID
				capturedResID := resID
				capturedResType := resType
				capturedExpectedVersion := expectedVersion
				capturedMsg := proto.Clone(msg).(*dynamicpb.Message)
				bundleOp.BuildWriteInput = func(matchedID string, isExisting bool) (postgres.ResourceInput, error) {
					targetID := capturedResID
					if isExisting {
						if capturedBodyID != "" && capturedBodyID != matchedID {
							return postgres.ResourceInput{}, fmt.Errorf("%w: body id %q != matched id %q", errConditionalIDMismatch, capturedBodyID, matchedID)
						}
						targetID = matchedID
					}
					setProtoResourceID(capturedMsg, targetID)
					pBytes, mErr := proto.Marshal(capturedMsg)
					if mErr != nil {
						return postgres.ResourceInput{}, mErr
					}
					var pIdx *postgres.SearchIndexes
					if extractor, ok := s.extractors[capturedResType]; ok {
						pIdx, _ = extractor(tenant, targetID, pBytes)
					}
					if hasSecCtx && auth.IsPatientRestricted(secCtx) {
						allowedPat := cleanID(secCtx.GetPatientId())
						if strings.EqualFold(capturedResType, "Patient") || postgres.IsPatientCompartmentResourceType(capturedResType) {
							if !postgres.IndexesBelongToPatientCompartment(capturedResType, targetID, pIdx, allowedPat) {
								return postgres.ResourceInput{}, fmt.Errorf("%w: %s/%s", errForbiddenCompartment, capturedResType, targetID)
							}
						}
					}
					return postgres.ResourceInput{
						TenantID:        tenant,
						ResType:         capturedResType,
						ResID:           targetID,
						ResourceProto:   pBytes,
						SearchIndexes:   pIdx,
						ExpectedVersion: capturedExpectedVersion,
					}, nil
				}
			}
			ops = append(ops, bundleOp)
			opEntryIdx = append(opEntryIdx, i)
			continue
		}

		// Batch mode: execute entry independently
		if condParams != nil && s.store != nil {
			rec, created, err := s.store.ConditionalCreateResource(r.Context(), input, *condParams)
			if errors.Is(err, postgres.ErrPreconditionFailed) {
				responseEntries[i] = bundleErrorResponse(http.StatusPreconditionFailed, err.Error())
				continue
			}
			if errors.Is(err, postgres.ErrResourceAlreadyExists) {
				responseEntries[i] = bundleErrorResponse(http.StatusConflict, err.Error())
				continue
			}
			if err != nil {
				responseEntries[i] = bundleErrorResponse(http.StatusInternalServerError, err.Error())
				continue
			}
			if created {
				s.publishCDC(r.Context(), automq.Message{
					TenantID:  tenant,
					ResType:   resType,
					ResID:     rec.ResID,
					ProtoData: rec.ResourceProto,
					Op:        "U",
				})
				s.dispatchSubscriptions(r.Context(), tenant, resType, rec.ResID, rec.ResourceProto, rec.ResVersion, rec.LastUpdated, idx)
			}
			status := "200 OK"
			if created {
				status = "201 Created"
			}
			responseEntries[i] = s.buildBundleWriteEntry(preferReturn, tenant, resType, rec.ResID, rec.ResVersion, rec.LastUpdated, rec.ResourceProto, status)
			continue
		}

		if s.store != nil {
			rec, err := s.store.WriteResourceWithMeta(r.Context(), input)
			if errors.Is(err, postgres.ErrVersionConflict) {
				responseEntries[i] = bundleErrorResponse(http.StatusPreconditionFailed, err.Error())
				continue
			}
			if errors.Is(err, postgres.ErrResourceAlreadyExists) {
				responseEntries[i] = bundleErrorResponse(http.StatusConflict, err.Error())
				continue
			}
			if err != nil {
				responseEntries[i] = bundleErrorResponse(http.StatusInternalServerError, err.Error())
				continue
			}
			s.publishCDC(r.Context(), automq.Message{
				TenantID:  tenant,
				ResType:   resType,
				ResID:     rec.ResID,
				ProtoData: rec.ResourceProto,
				Op:        "U",
			})
			s.dispatchSubscriptions(r.Context(), tenant, resType, rec.ResID, rec.ResourceProto, rec.ResVersion, rec.LastUpdated, idx)
			status := "200 OK"
			if method == "POST" || rec.Created {
				status = "201 Created"
			}
			responseEntries[i] = s.buildBundleWriteEntry(preferReturn, tenant, resType, rec.ResID, rec.ResVersion, rec.LastUpdated, rec.ResourceProto, status)
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
		if errors.Is(err, errConditionalIDMismatch) {
			writeOperationOutcome(w, http.StatusBadRequest, "invariant", err.Error())
			return
		}
		if errors.Is(err, errForbiddenCompartment) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
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
			origIdx := opEntryIdx[idxOp]
			if op.Method == "DELETE" {
				respMap := map[string]any{
					"status": "204 No Content",
				}
				if res.Record != nil && res.Record.ResVersion > 0 {
					respMap["etag"] = postgres.FormatETag(res.Record.ResVersion)
					delID := res.ResID
					if delID == "" {
						delID = op.Input.ResID
					}
					s.publishCDC(r.Context(), automq.Message{
						TenantID: tenant,
						ResType:  op.Input.ResType,
						ResID:    delID,
						Op:       "D",
					})
				}
				responseEntries[origIdx] = map[string]any{
					"response": respMap,
				}
				continue
			}

			rec := res.Record
			if origEntry := bundle.Entry[origIdx]; strings.HasPrefix(origEntry.FullURL, "urn:uuid:") && rec != nil {
				urnMap[origEntry.FullURL] = rec.ResType + "/" + rec.ResID
			}
			if res.Created || op.IfNoneExist == nil {
				s.publishCDC(r.Context(), automq.Message{
					TenantID:  tenant,
					ResType:   rec.ResType,
					ResID:     rec.ResID,
					ProtoData: rec.ResourceProto,
					Op:        "U",
				})
				s.dispatchSubscriptions(r.Context(), tenant, rec.ResType, rec.ResID, rec.ResourceProto, rec.ResVersion, rec.LastUpdated, op.Input.SearchIndexes)
			}
			status := res.Status
			if status == "" {
				status = "200 OK"
				if op.Method == "POST" && res.Created {
					status = "201 Created"
				}
			}
			responseEntries[origIdx] = s.buildBundleWriteEntry(preferReturn, tenant, rec.ResType, rec.ResID, rec.ResVersion, rec.LastUpdated, rec.ResourceProto, status)
		}
	}

	// Execute GET/HEAD entries in transaction Bundles last (FHIR R4 DELETE -> POST -> PUT/PATCH -> GET/HEAD)
	for _, i := range txGetIndices {
		getResp, _ := s.executeBundleGetEntry(r, tenant, bundle.Entry[i].Request, urnMap)
		responseEntries[i] = getResp
	}

	// Filter out any nil entries if s.store == nil in unit tests
	finalEntries := make([]map[string]any, 0, len(responseEntries))
	for _, re := range responseEntries {
		if re != nil {
			finalEntries = append(finalEntries, re)
		}
	}

	// Build response Bundle
	responseBundle := map[string]any{
		"resourceType": "Bundle",
		"type":         bundle.Type + "-response",
		"entry":        finalEntries,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(responseBundle)
}

func conditionalRefOutcomeCode(status int) string {
	switch status {
	case http.StatusPreconditionFailed:
		return "duplicate"
	case http.StatusForbidden:
		return "forbidden"
	default:
		return "invalid"
	}
}

func bundleEntryMethodPriority(entry bundleEntry) int {
	method := "PUT"
	if entry.Request != nil && entry.Request.Method != "" {
		method = strings.ToUpper(strings.TrimSpace(entry.Request.Method))
	}
	switch method {
	case "DELETE":
		return 1
	case "POST":
		return 2
	case "PUT", "PATCH":
		return 3
	case "GET", "HEAD":
		return 4
	default:
		return 0
	}
}

func (s *Server) buildBundleWriteEntry(preferReturn, tenant, resType, resID string, version int, lastUpdated time.Time, protoBytes []byte, status string) map[string]any {
	respMap := map[string]any{
		"status":   status,
		"location": fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, resID),
	}
	if version > 0 {
		respMap["etag"] = postgres.FormatETag(version)
	}
	if !lastUpdated.IsZero() {
		respMap["lastModified"] = lastUpdated.UTC().Format(time.RFC3339)
	}
	entryMap := map[string]any{
		"fullUrl":  fmt.Sprintf("%s/%s", resType, resID),
		"response": respMap,
	}
	switch preferReturn {
	case "representation":
		if len(protoBytes) > 0 {
			if jsonBytes, err := s.protoToJSONWithMeta(resType, protoBytes, version, lastUpdated); err == nil {
				var resObj map[string]any
				if err := json.Unmarshal(jsonBytes, &resObj); err == nil {
					entryMap["resource"] = resObj
				}
			}
		}
	case "OperationOutcome":
		respMap["outcome"] = map[string]any{
			"resourceType": "OperationOutcome",
			"issue": []map[string]any{
				{
					"severity":    "information",
					"code":        "informational",
					"diagnostics": fmt.Sprintf("%s/%s version %d stored", resType, resID, version),
				},
			},
		}
	}
	return entryMap
}

type bundleSubResponseRecorder struct {
	header     http.Header
	body       bytes.Buffer
	statusCode int
}

func (r *bundleSubResponseRecorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}

func (r *bundleSubResponseRecorder) WriteHeader(statusCode int) {
	if r.statusCode == 0 {
		r.statusCode = statusCode
	}
}

func (r *bundleSubResponseRecorder) Write(b []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.body.Write(b)
}

func (s *Server) executeBundleGetEntry(r *http.Request, tenant string, entryReq *bundleEntryRequest, urnMap map[string]string) (map[string]any, int) {
	rawURL := ""
	method := http.MethodGet
	if entryReq != nil {
		rawURL = entryReq.URL
		if strings.EqualFold(strings.TrimSpace(entryReq.Method), http.MethodHead) {
			method = http.MethodHead
		}
	}
	resolvedURL := strings.TrimSpace(rawURL)
	for urn, ref := range urnMap {
		if strings.Contains(resolvedURL, urn) {
			resolvedURL = strings.ReplaceAll(resolvedURL, urn, ref)
		}
	}
	cleanRel := strings.TrimLeft(resolvedURL, "/")
	if !strings.HasPrefix(cleanRel, "fhir/r4/") {
		cleanRel = "fhir/r4/" + tenant + "/" + cleanRel
	}
	targetURL := "/" + cleanRel
	subReq, err := http.NewRequestWithContext(r.Context(), method, targetURL, nil)
	if err != nil {
		return bundleErrorResponse(http.StatusBadRequest, fmt.Sprintf("invalid %s url %q: %v", method, rawURL, err)), http.StatusBadRequest
	}
	if r.Host != "" {
		subReq.Host = r.Host
	}
	if authHdr := r.Header.Get("Authorization"); authHdr != "" {
		subReq.Header.Set("Authorization", authHdr)
	}
	if entryReq != nil {
		if inm := strings.TrimSpace(entryReq.IfNoneMatch); inm != "" {
			subReq.Header.Set("If-None-Match", inm)
		}
		if ims := strings.TrimSpace(entryReq.IfModifiedSince); ims != "" {
			subReq.Header.Set("If-Modified-Since", ims)
		}
	}

	rec := &bundleSubResponseRecorder{header: make(http.Header)}
	s.authMiddleware(s.fhirRouter()).ServeHTTP(rec, subReq)
	statusCode := rec.statusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	respMap := map[string]any{
		"status": fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
	}
	if etag := rec.header.Get("ETag"); etag != "" {
		respMap["etag"] = etag
	}
	if lm := rec.header.Get("Last-Modified"); lm != "" {
		respMap["lastModified"] = lm
	}

	entryMap := map[string]any{
		"response": respMap,
	}
	if rec.body.Len() > 0 {
		var bodyObj map[string]any
		if err := json.Unmarshal(rec.body.Bytes(), &bodyObj); err == nil {
			if statusCode >= 400 && bodyObj["resourceType"] == "OperationOutcome" {
				respMap["outcome"] = bodyObj
			} else {
				entryMap["resource"] = bodyObj
				if rt, _ := bodyObj["resourceType"].(string); rt != "" && rt != "Bundle" {
					if id, _ := bodyObj["id"].(string); id != "" {
						entryMap["fullUrl"] = fmt.Sprintf("%s/%s", rt, id)
					}
				}
			}
		}
	}
	return entryMap, statusCode
}

type inBundleIndexedResource struct {
	resType string
	resID   string
	idx     *postgres.SearchIndexes
}

// resolveConditionalReferencesInJSON resolves FHIR R4 conditional references (§3.1.0.7.2, e.g.
// "reference": "Patient?identifier=http://mrn|123") inside a resource JSON payload to concrete
// "ResourceType/id" references before validation and storage.
func (s *Server) resolveConditionalReferencesInJSON(
	ctx context.Context,
	tenant string,
	rawJSON []byte,
	inBundle []inBundleIndexedResource,
) ([]byte, int, error) {
	if (s.store == nil && len(inBundle) == 0) || !bytes.Contains(rawJSON, []byte("?")) {
		return rawJSON, http.StatusOK, nil
	}
	var root any
	if err := json.Unmarshal(rawJSON, &root); err != nil {
		return rawJSON, http.StatusOK, nil
	}
	changed := false
	secCtx, hasSecCtx := auth.FromContext(ctx)

	var walk func(node any) (int, error)
	walk = func(node any) (int, error) {
		switch v := node.(type) {
		case map[string]any:
			if refRaw, ok := v["reference"].(string); ok && strings.Contains(refRaw, "?") {
				trimmed := strings.TrimSpace(refRaw)
				if !strings.HasPrefix(trimmed, "http://") &&
					!strings.HasPrefix(trimmed, "https://") &&
					!strings.HasPrefix(trimmed, "urn:") &&
					!strings.HasPrefix(trimmed, "#") &&
					!strings.HasPrefix(trimmed, "/") {
					targetType, queryStr, hasQ := strings.Cut(trimmed, "?")
					targetType = strings.Trim(targetType, "/")
					if hasQ && targetType != "" && queryStr != "" {
						if _, registered := s.protoRegistry[targetType]; !registered {
							return http.StatusBadRequest, fmt.Errorf("conditional reference target resource type %q is not registered", targetType)
						}
						condParams, err := postgres.ParseIfNoneExist(tenant, targetType, queryStr)
						if err != nil {
							return http.StatusBadRequest, fmt.Errorf("invalid conditional reference %q: %w", trimmed, err)
						}
						if err := s.expandTerminologySearchParams(ctx, tenant, &condParams); err != nil {
							return http.StatusBadRequest, fmt.Errorf("invalid conditional reference %q: %w", trimmed, err)
						}
						if hasSecCtx && auth.IsPatientRestricted(secCtx) {
							if err := validateAndScopeIfNoneExistForPatient(&condParams, targetType, cleanID(secCtx.GetPatientId()), secCtx.GetPatientId()); err != nil {
								return http.StatusForbidden, err
							}
						}
						var matchedIDs []string
						seenIDs := make(map[string]bool)
						for _, ib := range inBundle {
							if matchesInBundleSearchIndexes(ib, condParams) && !seenIDs[ib.resID] {
								seenIDs[ib.resID] = true
								matchedIDs = append(matchedIDs, ib.resID)
							}
						}
						if s.store != nil {
							condParams.Count = 2
							sr, err := s.store.Search(ctx, condParams)
							if err != nil {
								return http.StatusInternalServerError, fmt.Errorf("conditional reference search failed for %q: %w", trimmed, err)
							}
							for _, m := range sr.Matches {
								if !seenIDs[m.ResID] {
									seenIDs[m.ResID] = true
									matchedIDs = append(matchedIDs, m.ResID)
								}
							}
						}
						if len(matchedIDs) == 0 {
							return http.StatusBadRequest, fmt.Errorf("conditional reference %q matched 0 resources", trimmed)
						}
						if len(matchedIDs) > 1 {
							return http.StatusPreconditionFailed, fmt.Errorf("conditional reference %q matched %d resources (expected 1)", trimmed, len(matchedIDs))
						}
						v["reference"] = targetType + "/" + matchedIDs[0]
						changed = true
					}
				}
			}
			for _, child := range v {
				if st, err := walk(child); err != nil {
					return st, err
				}
			}
		case []any:
			for _, item := range v {
				if st, err := walk(item); err != nil {
					return st, err
				}
			}
		}
		return http.StatusOK, nil
	}

	if st, err := walk(root); err != nil {
		return nil, st, err
	}
	if !changed {
		return rawJSON, http.StatusOK, nil
	}
	marshaled, err := json.Marshal(root)
	if err != nil {
		return rawJSON, http.StatusOK, nil
	}
	return marshaled, http.StatusOK, nil
}

func matchesInBundleSearchIndexes(ib inBundleIndexedResource, condParams postgres.SearchParams) bool {
	if !strings.EqualFold(ib.resType, condParams.ResType) {
		return false
	}
	if condParams.ID != "" && ib.resID != condParams.ID {
		return false
	}
	if ib.idx == nil {
		return len(condParams.Tokens) == 0 && len(condParams.Strings) == 0 && len(condParams.URIs) == 0 && len(condParams.References) == 0
	}
	for k, rawVal := range condParams.Tokens {
		matched := false
		for _, tok := range ib.idx.Tokens {
			if !strings.EqualFold(tok.SpName, k) {
				continue
			}
			cand := tok.SpValue
			if tok.SpSystem != "" {
				cand = tok.SpSystem + "|" + tok.SpValue
			}
			if auth.TokenValueMatchesConstraint(cand, rawVal) || strings.EqualFold(tok.SpValue, rawVal) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for k, rawVal := range condParams.Strings {
		matched := false
		for _, str := range ib.idx.Strings {
			if strings.EqualFold(str.SpName, k) && strings.HasPrefix(strings.ToLower(str.SpValue), strings.ToLower(rawVal)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for k, rawVal := range condParams.URIs {
		matched := false
		for _, u := range ib.idx.URIs {
			if strings.EqualFold(u.SpName, k) && u.SpValue == rawVal {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for k, rawVal := range condParams.References {
		matched := false
		for _, ref := range ib.idx.References {
			if strings.EqualFold(ref.SpName, k) && (ref.TargetType+"/"+ref.TargetID == rawVal || ref.TargetID == rawVal) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
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
