package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
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

var (
	errConditionalIDMismatch = errors.New("resource id in body does not match conditional update match id")
	errForbiddenCompartment  = errors.New("patient-scoped token cannot access resource outside patient compartment")
)

// jsonPatchOp represents a single RFC 6902 JSON Patch operation.
type jsonPatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	From  string          `json:"from,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// handlePatch handles PATCH /fhir/r4/{tenant}/{resourceType}/{id}.
// Supports RFC 6902 JSON Patch (application/json-patch+json) and
// RFC 7396 JSON Merge Patch (application/merge-patch+json or application/fhir+json).
func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	md, ok := s.protoRegistry[resType]
	if !ok {
		writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
			fmt.Sprintf("resource type %s not registered", resType))
		return
	}

	xProvTemplate, err := parseXProvenanceHeader(r)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	expectedVersion := 0
	if ifMatch := strings.TrimSpace(r.Header.Get("If-Match")); ifMatch != "" {
		v, err := postgres.ParseETagVersion(ifMatch)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid If-Match header %q", ifMatch))
			return
		}
		expectedVersion = v
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read patch body")
		return
	}
	trimmedBody := bytes.TrimSpace(body)
	if len(trimmedBody) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "empty PATCH request body")
		return
	}

	var (
		existingRec *postgres.ResourceRecord
		rec         *postgres.ResourceRecord
		patchedJSON []byte
		protoBytes  []byte
		idx         *postgres.SearchIndexes
	)
	for attempt := 0; ; attempt++ {
		existingRec, err = s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
		if errors.Is(err, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found",
				fmt.Sprintf("%s/%s not found", resType, resID))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			if existingRec != nil && existingRec.ResVersion > 0 {
				w.Header().Set("ETag", postgres.FormatETag(existingRec.ResVersion))
			}
			writeOperationOutcome(w, http.StatusGone, "deleted",
				fmt.Sprintf("%s/%s has been deleted", resType, resID))
			return
		}
		if err != nil {
			s.logger.Error("read resource for patch", "error", err, "type", resType, "id", resID)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read resource for patch")
			return
		}

		if expectedVersion > 0 && existingRec.ResVersion != expectedVersion {
			writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict",
				fmt.Sprintf("%v: expected version %d, current version %d", postgres.ErrVersionConflict, expectedVersion, existingRec.ResVersion))
			return
		}

		currentJSON, err := s.protoToJSONWithMeta(resType, existingRec.ResourceProto, 0, time.Time{})
		if err != nil {
			s.logger.Error("denormalize resource for patch", "error", err, "type", resType, "id", resID)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to serialize resource for patch")
			return
		}

		contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if strings.Contains(contentType, "json-patch+json") || trimmedBody[0] == '[' {
			var status int
			patchedJSON, status, err = ApplyJSONPatch(currentJSON, trimmedBody)
			if err != nil {
				code := "invalid"
				if status == http.StatusUnprocessableEntity {
					code = "invariant"
				}
				writeOperationOutcome(w, status, code, err.Error())
				return
			}
		} else if isFHIRParametersPatch(trimmedBody) {
			var status int
			patchedJSON, status, err = ApplyFHIRPatch(currentJSON, trimmedBody)
			if err != nil {
				code := "invalid"
				if status == http.StatusUnprocessableEntity {
					code = "invariant"
				}
				writeOperationOutcome(w, status, code, err.Error())
				return
			}
		} else {
			patchedJSON, err = ApplyMergePatch(currentJSON, trimmedBody)
			if err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "structure", err.Error())
				return
			}
		}

		var patchedMap map[string]any
		if err := json.Unmarshal(patchedJSON, &patchedMap); err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("invalid patched JSON: %v", err))
			return
		}
		if rt, ok := patchedMap["resourceType"].(string); !ok || rt != resType {
			writeOperationOutcome(w, http.StatusBadRequest, "invariant",
				fmt.Sprintf("PATCH cannot change resourceType from %q to %v", resType, patchedMap["resourceType"]))
			return
		}
		if rawID, hasID := patchedMap["id"]; hasID && rawID != nil {
			idStr, ok := rawID.(string)
			if !ok || (idStr != "" && idStr != resID) {
				writeOperationOutcome(w, http.StatusBadRequest, "invariant",
					fmt.Sprintf("PATCH cannot change resource id from %q to %v", resID, rawID))
				return
			}
		}
		patchedMap["id"] = resID
		patchedJSON, err = json.Marshal(patchedMap)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to marshal patched resource")
			return
		}

		resolvedPatch, refStatus, refErr := s.resolveConditionalReferencesInJSON(r.Context(), tenant, patchedJSON, nil)
		if refErr != nil {
			writeOperationOutcome(w, refStatus, conditionalRefOutcomeCode(refStatus), refErr.Error())
			return
		}
		patchedJSON = resolvedPatch

		if s.validator != nil {
			outcome := s.validator.ValidateJSON(resType, patchedJSON)
			if !outcome.IsValid() {
				outcome.WriteHTTP(w, http.StatusBadRequest)
				return
			}
		}

		patchedJSON = normalizeSubscriptionOnWrite(resType, patchedJSON)

		normalizedBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, patchedJSON)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "structure",
				fmt.Sprintf("invalid patched FHIR JSON: %v", err))
			return
		}

		msg := dynamicpb.NewMessage(md)
		unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
		if err := unmarshaler.Unmarshal(normalizedBody, msg); err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "structure",
				fmt.Sprintf("invalid patched FHIR JSON: %v", err))
			return
		}
		setProtoResourceID(msg, resID)

		protoBytes, err = proto.Marshal(msg)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to encode patched resource")
			return
		}

		s.syncSearchParameterOnWrite(resType, resID, patchedJSON)

		idx = nil
		if extractor, ok := s.extractors[resType]; ok {
			idx, _ = extractor(tenant, resID, protoBytes)
		}
		s.enrichProvenancePatientIndex(r.Context(), tenant, resType, idx)
		setAuditPatientID(r.Context(), extractPatientIDFromIndexes(resType, resID, idx))

		if secCtx, ok := auth.FromContext(r.Context()); ok {
			if auth.IsPatientRestricted(secCtx) {
				if strings.EqualFold(resType, "Patient") || postgres.IsPatientCompartmentResourceType(resType) {
					if !postgres.IndexesBelongToPatientCompartment(resType, resID, idx, cleanID(secCtx.GetPatientId())) {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("patient-scoped token (%s) cannot patch %s/%s outside patient compartment",
								secCtx.GetPatientId(), resType, resID))
						return
					}
				}
			}
			if constraints, constrained := auth.ScopeQueryConstraints(secCtx, resType, auth.ActionUpdate); constrained {
				if !s.resourceSatisfiesScopeConstraints(tenant, resType, resID, existingRec.ResourceProto, constraints) ||
					!s.resourceSatisfiesScopeConstraints(tenant, resType, resID, protoBytes, constraints) {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("insufficient granular SMART v2 scope to patch %s/%s", resType, resID))
					return
				}
			}
		}

		rec, err = s.store.WriteResourceWithMeta(r.Context(), postgres.ResourceInput{
			TenantID:        tenant,
			ResType:         resType,
			ResID:           resID,
			ResourceProto:   protoBytes,
			SearchIndexes:   idx,
			ExpectedVersion: existingRec.ResVersion,
		})
		if errors.Is(err, postgres.ErrVersionConflict) {
			if expectedVersion == 0 && attempt < 16 {
				continue
			}
			writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", err.Error())
			return
		}
		if err != nil {
			s.logger.Error("write patched resource", "error", err, "type", resType, "id", resID)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to store patched resource")
			return
		}
		break
	}

	s.publishCDC(r.Context(), automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     resID,
		ProtoData: protoBytes,
		Op:        "U",
	})
	s.dispatchSubscriptions(r.Context(), tenant, resType, resID, protoBytes, rec.ResVersion, rec.LastUpdated, idx)
	s.recordAutoProvenance(r, w, tenant, resType, resID, rec.ResVersion, idx, xProvTemplate)

	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	s.writeResourceWriteResponse(w, r, http.StatusOK, md, resType, resID, rec.ResVersion, rec.LastUpdated, protoBytes, patchedJSON)
}

// handleConditionalUpdate handles PUT /fhir/r4/{tenant}/{resourceType}?{searchParams}.
func (s *Server) handleConditionalUpdate(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")

	md, ok := s.protoRegistry[resType]
	if !ok {
		writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
			fmt.Sprintf("resource type %s not registered", resType))
		return
	}

	xProvTemplate, err := parseXProvenanceHeader(r)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	q := r.URL.Query()
	if err := postgres.ValidateSearchQuery(q); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	condParams := postgres.BuildSearchParamsFromQuery(tenant, resType, q)
	if !postgres.HasSearchCriteria(condParams) {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"conditional update requires at least one search parameter in the query string")
		return
	}
	if err := s.expandTerminologySearchParams(r.Context(), tenant, &condParams); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		if err := validateAndScopeIfNoneExistForPatient(&condParams, resType, cleanID(secCtx.GetPatientId()), secCtx.GetPatientId()); err != nil {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
			return
		}
	}

	expectedVersion := 0
	if ifMatch := strings.TrimSpace(r.Header.Get("If-Match")); ifMatch != "" {
		v, err := postgres.ParseETagVersion(ifMatch)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid If-Match header %q", ifMatch))
			return
		}
		expectedVersion = v
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read body")
		return
	}

	msg := dynamicpb.NewMessage(md)
	if isProtoContentType(r) {
		if len(body) == 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "empty protobuf request body")
			return
		}
		if err := proto.Unmarshal(body, msg); err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "structure",
				fmt.Sprintf("invalid FHIR Protobuf wire payload: %v", err))
			return
		}
	} else {
		resolvedBody, refStatus, refErr := s.resolveConditionalReferencesInJSON(r.Context(), tenant, body, nil)
		if refErr != nil {
			writeOperationOutcome(w, refStatus, conditionalRefOutcomeCode(refStatus), refErr.Error())
			return
		}
		body = resolvedBody

		if s.validator != nil {
			outcome := s.validator.ValidateJSON(resType, body)
			if !outcome.IsValid() {
				outcome.WriteHTTP(w, http.StatusBadRequest)
				return
			}
		}

		body = normalizeSubscriptionOnWrite(resType, body)

		normalizedBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, body)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "structure",
				fmt.Sprintf("invalid FHIR JSON: %v", err))
			return
		}

		unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
		if err := unmarshaler.Unmarshal(normalizedBody, msg); err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "structure",
				fmt.Sprintf("invalid FHIR JSON: %v", err))
			return
		}
	}
	bodyID := extractProtoResourceID(msg)

	var finalProtoBytes []byte
	var finalResID string
	var finalIdx *postgres.SearchIndexes

	rec, err := s.store.ConditionalUpdateResource(
		r.Context(),
		tenant,
		resType,
		condParams,
		func(matchedID string, isExisting bool) (postgres.ResourceInput, error) {
			if isExisting {
				if bodyID != "" && bodyID != matchedID {
					return postgres.ResourceInput{}, fmt.Errorf("%w: body id %q != matched id %q", errConditionalIDMismatch, bodyID, matchedID)
				}
				finalResID = matchedID
			} else {
				if bodyID != "" {
					finalResID = bodyID
				} else {
					finalResID = generateID()
				}
			}

			setProtoResourceID(msg, finalResID)
			pBytes, mErr := proto.Marshal(msg)
			if mErr != nil {
				return postgres.ResourceInput{}, mErr
			}
			finalProtoBytes = pBytes

			s.syncSearchParameterOnWrite(resType, finalResID, body)

			if extractor, ok := s.extractors[resType]; ok {
				finalIdx, _ = extractor(tenant, finalResID, finalProtoBytes)
			}
			s.enrichProvenancePatientIndex(r.Context(), tenant, resType, finalIdx)

			if hasSecCtx && auth.IsPatientRestricted(secCtx) {
				if strings.EqualFold(resType, "Patient") || postgres.IsPatientCompartmentResourceType(resType) {
					if !postgres.IndexesBelongToPatientCompartment(resType, finalResID, finalIdx, cleanID(secCtx.GetPatientId())) {
						return postgres.ResourceInput{}, fmt.Errorf("%w: %s/%s", errForbiddenCompartment, resType, finalResID)
					}
				}
			}

			return postgres.ResourceInput{
				TenantID:        tenant,
				ResType:         resType,
				ResID:           finalResID,
				ResourceProto:   finalProtoBytes,
				SearchIndexes:   finalIdx,
				ExpectedVersion: expectedVersion,
			}, nil
		},
	)
	if errors.Is(err, postgres.ErrPreconditionFailed) || errors.Is(err, postgres.ErrVersionConflict) {
		writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", err.Error())
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
		s.logger.Error("conditional update resource", "error", err, "type", resType)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to execute conditional update")
		return
	}

	patID := extractPatientIDFromIndexes(resType, rec.ResID, finalIdx)
	setAuditPatientID(r.Context(), patID)

	s.publishCDC(r.Context(), automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     rec.ResID,
		ProtoData: finalProtoBytes,
		Op:        "U",
	})
	s.dispatchSubscriptions(r.Context(), tenant, resType, rec.ResID, finalProtoBytes, rec.ResVersion, rec.LastUpdated, finalIdx)
	s.recordAutoProvenance(r, w, tenant, resType, rec.ResID, rec.ResVersion, finalIdx, xProvTemplate)

	w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, rec.ResID))
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	status := http.StatusOK
	if rec.Created {
		status = http.StatusCreated
	}
	s.writeResourceWriteResponse(w, r, status, md, resType, rec.ResID, rec.ResVersion, rec.LastUpdated, finalProtoBytes, body)
}

// handleConditionalPatch handles PATCH /fhir/r4/{tenant}/{resourceType}?{searchParams}
// per HL7 FHIR R4 Conditional Patch (§3.1.0.7.1).
func (s *Server) handleConditionalPatch(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")

	if _, ok := s.protoRegistry[resType]; !ok {
		writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
			fmt.Sprintf("resource type %s not registered", resType))
		return
	}
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}

	q := r.URL.Query()
	if err := postgres.ValidateSearchQuery(q); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	condParams := postgres.BuildSearchParamsFromQuery(tenant, resType, q)
	if !postgres.HasSearchCriteria(condParams) {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"conditional patch requires at least one search parameter in the query string")
		return
	}
	if err := s.expandTerminologySearchParams(r.Context(), tenant, &condParams); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		if err := validateAndScopeIfNoneExistForPatient(&condParams, resType, cleanID(secCtx.GetPatientId()), secCtx.GetPatientId()); err != nil {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
			return
		}
	}

	condParams.Count = 2
	condParams.Offset = 0
	sr, err := s.store.Search(r.Context(), condParams)
	if err != nil {
		s.logger.Error("conditional patch search", "error", err, "type", resType)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to evaluate conditional patch search")
		return
	}
	if sr.Total == 0 || len(sr.Matches) == 0 {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("conditional patch matched 0 %s resources", resType))
		return
	}
	if sr.Total > 1 {
		writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict",
			fmt.Sprintf("%v: conditional patch matched %d existing %s resources", postgres.ErrPreconditionFailed, sr.Total, resType))
		return
	}

	matchedID := sr.Matches[0].ResID
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		if strings.EqualFold(resType, "Patient") && cleanID(matchedID) != cleanID(secCtx.GetPatientId()) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot patch patient %s", secCtx.GetPatientId(), matchedID))
			return
		}
		if !strings.EqualFold(resType, "Patient") && postgres.IsPatientCompartmentResourceType(resType) {
			exists, allowed, cErr := s.checkExistingResourceCompartment(r.Context(), tenant, resType, matchedID, secCtx.GetPatientId())
			if cErr != nil {
				writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
				return
			}
			if exists && !allowed {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot patch %s/%s outside patient compartment", secCtx.GetPatientId(), resType, matchedID))
				return
			}
		}
	}

	w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, matchedID))
	r.SetPathValue("id", matchedID)
	s.handlePatch(w, r)
}

// handleConditionalDelete handles DELETE /fhir/r4/{tenant}/{resourceType}?{searchParams}.
// Enforces single conditional delete (412 Precondition Failed on >1 matches) by default,
// and supports multiple conditional delete when _count (>1) is explicitly specified.
func (s *Server) handleConditionalDelete(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")

	q := r.URL.Query()
	if err := postgres.ValidateSearchQuery(q); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	condParams := postgres.BuildSearchParamsFromQuery(tenant, resType, q)
	if !postgres.HasSearchCriteria(condParams) {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"conditional delete requires at least one search parameter in the query string")
		return
	}
	if err := s.expandTerminologySearchParams(r.Context(), tenant, &condParams); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	if ifMatch := strings.TrimSpace(r.Header.Get("If-Match")); ifMatch != "" {
		v, err := postgres.ParseETagVersion(ifMatch)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid If-Match header %q", ifMatch))
			return
		}
		condParams.ExpectedVersion = v
	}

	maxCount := 1
	if rawCount := strings.TrimSpace(q.Get("_count")); rawCount != "" {
		if n, cErr := strconv.Atoi(rawCount); cErr == nil && n > 1 {
			maxCount = n
		}
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		if err := validateAndScopeIfNoneExistForPatient(&condParams, resType, cleanID(secCtx.GetPatientId()), secCtx.GetPatientId()); err != nil {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
			return
		}
	}

	recs, err := s.store.ConditionalDeleteResources(
		r.Context(),
		tenant,
		resType,
		condParams,
		maxCount,
		func(matchedID string) error {
			if hasSecCtx && auth.IsPatientRestricted(secCtx) {
				if strings.EqualFold(resType, "Patient") && cleanID(matchedID) != cleanID(secCtx.GetPatientId()) {
					return errForbiddenCompartment
				}
				if !strings.EqualFold(resType, "Patient") && postgres.IsPatientCompartmentResourceType(resType) {
					_, allowed, cErr := s.checkExistingResourceCompartment(r.Context(), tenant, resType, matchedID, secCtx.GetPatientId())
					if cErr != nil {
						return cErr
					}
					if !allowed {
						return errForbiddenCompartment
					}
				}
			}
			return nil
		},
	)
	if errors.Is(err, postgres.ErrPreconditionFailed) || errors.Is(err, postgres.ErrVersionConflict) {
		writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", err.Error())
		return
	}
	if errors.Is(err, errForbiddenCompartment) {
		writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	if err != nil {
		s.logger.Error("conditional delete resource", "error", err, "type", resType)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to execute conditional delete")
		return
	}

	if len(recs) > 0 {
		first := recs[0]
		if first.ResVersion > 0 {
			w.Header().Set("ETag", postgres.FormatETag(first.ResVersion))
			w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, first.ResID))
		}
		for _, rec := range recs {
			if rec.ResVersion > 0 {
				s.syncSearchParameterOnDelete(resType, rec.ResID)
				s.publishCDC(r.Context(), automq.Message{
					TenantID: tenant,
					ResType:  resType,
					ResID:    rec.ResID,
					Op:       "D",
				})
			}
		}
	}
	if parsePreferReturn(r) == "OperationOutcome" {
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resourceType": "OperationOutcome",
			"issue": []map[string]any{
				{
					"severity":    "information",
					"code":        "informational",
					"diagnostics": fmt.Sprintf("Conditional delete soft-deleted %d %s resource(s)", len(recs), resType),
				},
			},
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ApplyJSONPatch applies an RFC 6902 JSON Patch document to docJSON.
// Returns (patchedJSON, httpStatusOnError, error).
func ApplyJSONPatch(docJSON []byte, patchJSON []byte) ([]byte, int, error) {
	var ops []jsonPatchOp
	if err := json.Unmarshal(patchJSON, &ops); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("malformed JSON Patch array: %v", err)
	}
	if len(ops) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("JSON Patch array must contain at least one operation")
	}

	var doc any
	if err := json.Unmarshal(docJSON, &doc); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("unmarshal target document: %v", err)
	}

	for i, op := range ops {
		opName := strings.ToLower(strings.TrimSpace(op.Op))
		if op.Path == "" && opName != "" {
			return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d] (%s): missing required 'path'", i, opName)
		}
		if op.Path != "" && !strings.HasPrefix(op.Path, "/") {
			return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d] (%s): path %q must start with '/'", i, opName, op.Path)
		}

		switch opName {
		case "add":
			if len(op.Value) == 0 {
				return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d] (add): missing required 'value'", i)
			}
			var val any
			if err := json.Unmarshal(op.Value, &val); err != nil {
				return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d] (add): invalid value JSON: %v", i, err)
			}
			var err error
			doc, err = jsonPointerAdd(doc, parseJSONPointer(op.Path), val)
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("patch op[%d] (add %s): %v", i, op.Path, err)
			}

		case "remove":
			var err error
			doc, _, err = jsonPointerRemove(doc, parseJSONPointer(op.Path))
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("patch op[%d] (remove %s): %v", i, op.Path, err)
			}

		case "replace":
			if len(op.Value) == 0 {
				return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d] (replace): missing required 'value'", i)
			}
			var val any
			if err := json.Unmarshal(op.Value, &val); err != nil {
				return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d] (replace): invalid value JSON: %v", i, err)
			}
			var err error
			doc, err = jsonPointerReplace(doc, parseJSONPointer(op.Path), val)
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("patch op[%d] (replace %s): %v", i, op.Path, err)
			}

		case "move":
			if op.From == "" || !strings.HasPrefix(op.From, "/") {
				return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d] (move): invalid or missing 'from' pointer", i)
			}
			var movedVal any
			var err error
			doc, movedVal, err = jsonPointerRemove(doc, parseJSONPointer(op.From))
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("patch op[%d] (move from %s): %v", i, op.From, err)
			}
			doc, err = jsonPointerAdd(doc, parseJSONPointer(op.Path), movedVal)
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("patch op[%d] (move to %s): %v", i, op.Path, err)
			}

		case "copy":
			if op.From == "" || !strings.HasPrefix(op.From, "/") {
				return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d] (copy): invalid or missing 'from' pointer", i)
			}
			copiedVal, err := jsonPointerGet(doc, parseJSONPointer(op.From))
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("patch op[%d] (copy from %s): %v", i, op.From, err)
			}
			cloned := deepCopyJSONValue(copiedVal)
			doc, err = jsonPointerAdd(doc, parseJSONPointer(op.Path), cloned)
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("patch op[%d] (copy to %s): %v", i, op.Path, err)
			}

		case "test":
			if len(op.Value) == 0 {
				return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d] (test): missing required 'value'", i)
			}
			var expectedVal any
			if err := json.Unmarshal(op.Value, &expectedVal); err != nil {
				return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d] (test): invalid value JSON: %v", i, err)
			}
			actualVal, err := jsonPointerGet(doc, parseJSONPointer(op.Path))
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("patch op[%d] (test %s): %v", i, op.Path, err)
			}
			if !reflect.DeepEqual(actualVal, expectedVal) {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("patch op[%d] (test %s) failed: expected %v, got %v", i, op.Path, expectedVal, actualVal)
			}

		default:
			return nil, http.StatusBadRequest, fmt.Errorf("patch op[%d]: unsupported operation %q", i, op.Op)
		}
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return out, http.StatusOK, nil
}

func parseJSONPointer(path string) []string {
	if path == "" || path == "/" {
		return nil
	}
	rawTokens := strings.Split(path[1:], "/")
	tokens := make([]string, len(rawTokens))
	for i, t := range rawTokens {
		t = strings.ReplaceAll(t, "~1", "/")
		t = strings.ReplaceAll(t, "~0", "~")
		tokens[i] = t
	}
	return tokens
}

func jsonPointerGet(node any, tokens []string) (any, error) {
	curr := node
	for _, tok := range tokens {
		switch c := curr.(type) {
		case map[string]any:
			val, exists := c[tok]
			if !exists {
				return nil, fmt.Errorf("key %q not found", tok)
			}
			curr = val
		case []any:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(c) {
				return nil, fmt.Errorf("array index %q out of bounds (len %d)", tok, len(c))
			}
			curr = c[idx]
		default:
			return nil, fmt.Errorf("cannot traverse %T at token %q", curr, tok)
		}
	}
	return curr, nil
}

func jsonPointerAdd(node any, tokens []string, val any) (any, error) {
	if len(tokens) == 0 {
		return val, nil
	}
	tok := tokens[0]
	if len(tokens) == 1 {
		switch c := node.(type) {
		case map[string]any:
			c[tok] = val
			return c, nil
		case []any:
			if tok == "-" {
				return append(c, val), nil
			}
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx > len(c) {
				return nil, fmt.Errorf("array index %q out of bounds for add (len %d)", tok, len(c))
			}
			c = append(c, nil)
			copy(c[idx+1:], c[idx:])
			c[idx] = val
			return c, nil
		default:
			return nil, fmt.Errorf("cannot add property %q to %T", tok, node)
		}
	}

	switch c := node.(type) {
	case map[string]any:
		child, exists := c[tok]
		if !exists {
			return nil, fmt.Errorf("parent key %q does not exist", tok)
		}
		updated, err := jsonPointerAdd(child, tokens[1:], val)
		if err != nil {
			return nil, err
		}
		c[tok] = updated
		return c, nil
	case []any:
		idx, err := strconv.Atoi(tok)
		if err != nil || idx < 0 || idx >= len(c) {
			return nil, fmt.Errorf("array index %q out of bounds (len %d)", tok, len(c))
		}
		updated, err := jsonPointerAdd(c[idx], tokens[1:], val)
		if err != nil {
			return nil, err
		}
		c[idx] = updated
		return c, nil
	default:
		return nil, fmt.Errorf("cannot traverse %T at token %q", node, tok)
	}
}

func jsonPointerRemove(node any, tokens []string) (any, any, error) {
	if len(tokens) == 0 {
		return nil, nil, fmt.Errorf("cannot remove root document")
	}
	tok := tokens[0]
	if len(tokens) == 1 {
		switch c := node.(type) {
		case map[string]any:
			removed, exists := c[tok]
			if !exists {
				return nil, nil, fmt.Errorf("key %q does not exist", tok)
			}
			delete(c, tok)
			return c, removed, nil
		case []any:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(c) {
				return nil, nil, fmt.Errorf("array index %q out of bounds for remove (len %d)", tok, len(c))
			}
			removed := c[idx]
			c = append(c[:idx], c[idx+1:]...)
			return c, removed, nil
		default:
			return nil, nil, fmt.Errorf("cannot remove property %q from %T", tok, node)
		}
	}

	switch c := node.(type) {
	case map[string]any:
		child, exists := c[tok]
		if !exists {
			return nil, nil, fmt.Errorf("parent key %q does not exist", tok)
		}
		updated, removed, err := jsonPointerRemove(child, tokens[1:])
		if err != nil {
			return nil, nil, err
		}
		c[tok] = updated
		return c, removed, nil
	case []any:
		idx, err := strconv.Atoi(tok)
		if err != nil || idx < 0 || idx >= len(c) {
			return nil, nil, fmt.Errorf("array index %q out of bounds (len %d)", tok, len(c))
		}
		updated, removed, err := jsonPointerRemove(c[idx], tokens[1:])
		if err != nil {
			return nil, nil, err
		}
		c[idx] = updated
		return c, removed, nil
	default:
		return nil, nil, fmt.Errorf("cannot traverse %T at token %q", node, tok)
	}
}

func jsonPointerReplace(node any, tokens []string, val any) (any, error) {
	if len(tokens) == 0 {
		return val, nil
	}
	tok := tokens[0]
	if len(tokens) == 1 {
		switch c := node.(type) {
		case map[string]any:
			if _, exists := c[tok]; !exists {
				return nil, fmt.Errorf("key %q does not exist for replace", tok)
			}
			c[tok] = val
			return c, nil
		case []any:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(c) {
				return nil, fmt.Errorf("array index %q out of bounds for replace (len %d)", tok, len(c))
			}
			c[idx] = val
			return c, nil
		default:
			return nil, fmt.Errorf("cannot replace property %q on %T", tok, node)
		}
	}

	switch c := node.(type) {
	case map[string]any:
		child, exists := c[tok]
		if !exists {
			return nil, fmt.Errorf("parent key %q does not exist", tok)
		}
		updated, err := jsonPointerReplace(child, tokens[1:], val)
		if err != nil {
			return nil, err
		}
		c[tok] = updated
		return c, nil
	case []any:
		idx, err := strconv.Atoi(tok)
		if err != nil || idx < 0 || idx >= len(c) {
			return nil, fmt.Errorf("array index %q out of bounds (len %d)", tok, len(c))
		}
		updated, err := jsonPointerReplace(c[idx], tokens[1:], val)
		if err != nil {
			return nil, err
		}
		c[idx] = updated
		return c, nil
	default:
		return nil, fmt.Errorf("cannot traverse %T at token %q", node, tok)
	}
}

func deepCopyJSONValue(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}

// ApplyMergePatch applies an RFC 7396 JSON Merge Patch object to docJSON.
func ApplyMergePatch(docJSON []byte, mergePatchJSON []byte) ([]byte, error) {
	var target map[string]any
	if err := json.Unmarshal(docJSON, &target); err != nil {
		return nil, fmt.Errorf("unmarshal target JSON: %v", err)
	}
	var patch map[string]any
	if err := json.Unmarshal(mergePatchJSON, &patch); err != nil {
		return nil, fmt.Errorf("malformed JSON Merge Patch object: %v", err)
	}
	merged := applyMergePatchMap(target, patch)
	return json.Marshal(merged)
}

func applyMergePatchMap(target map[string]any, patch map[string]any) map[string]any {
	if target == nil {
		target = make(map[string]any)
	}
	for k, v := range patch {
		if v == nil {
			delete(target, k)
			continue
		}
		if patchSub, isMap := v.(map[string]any); isMap {
			targetSub, _ := target[k].(map[string]any)
			target[k] = applyMergePatchMap(targetSub, patchSub)
		} else {
			target[k] = v
		}
	}
	return target
}

func isFHIRParametersPatch(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var peek struct {
		ResourceType string `json:"resourceType"`
	}
	if err := json.Unmarshal(trimmed, &peek); err != nil {
		return false
	}
	return peek.ResourceType == "Parameters"
}

var knownRepeatingFHIRFields = map[string]bool{
	"identifier":          true,
	"name":                true,
	"telecom":             true,
	"address":             true,
	"given":               true,
	"prefix":              true,
	"suffix":              true,
	"line":                true,
	"coding":              true,
	"category":            true,
	"performer":           true,
	"participant":         true,
	"reasonCode":          true,
	"reasonReference":     true,
	"note":                true,
	"extension":           true,
	"modifierExtension":   true,
	"contained":           true,
	"link":                true,
	"communication":       true,
	"generalPractitioner": true,
	"contact":             true,
	"entry":               true,
	"section":             true,
	"author":              true,
	"target":              true,
	"agent":               true,
	"entity":              true,
	"insurance":           true,
	"item":                true,
	"diagnosis":           true,
	"careTeam":            true,
	"supportingInfo":      true,
	"basedOn":             true,
	"partOf":              true,
	"endpoint":            true,
	"photo":               true,
	"qualification":       true,
	"profile":             true,
	"security":            true,
	"tag":                 true,
}

// ApplyFHIRPatch applies an HL7 FHIR R4 FHIRPath Patch (Parameters resource) to docJSON.
// Supports operation types: add, insert, delete, replace, and move, including round-trip
// compatibility with $diff Parameters documents.
func ApplyFHIRPatch(docJSON []byte, patchJSON []byte) ([]byte, int, error) {
	var root map[string]any
	if err := json.Unmarshal(patchJSON, &root); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("malformed FHIRPath Patch JSON: %v", err)
	}
	if rt, _ := root["resourceType"].(string); rt != "Parameters" {
		return nil, http.StatusBadRequest, fmt.Errorf("FHIRPath Patch must be a Parameters resource, got %q", rt)
	}
	rawParams, _ := root["parameter"].([]any)
	var opMaps []map[string]any
	for _, rp := range rawParams {
		if pm, ok := rp.(map[string]any); ok && pm["name"] == "operation" {
			opMaps = append(opMaps, pm)
		}
	}
	if len(opMaps) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("FHIRPath Patch Parameters must contain at least one 'operation' parameter")
	}

	var doc any
	if err := json.Unmarshal(docJSON, &doc); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("unmarshal target document: %v", err)
	}
	docMap, ok := doc.(map[string]any)
	if !ok {
		return nil, http.StatusInternalServerError, fmt.Errorf("target document is not a JSON object")
	}
	rootResType, _ := docMap["resourceType"].(string)
	origID, _ := docMap["id"].(string)

	for i, opMap := range opMaps {
		rawParts, _ := opMap["part"].([]any)
		var opType, path, propName string
		var index, source, destination int
		var hasIndex, hasSource, hasDest, hasValue bool
		var val any

		for _, rp := range rawParts {
			pm, ok := rp.(map[string]any)
			if !ok {
				continue
			}
			pName, _ := pm["name"].(string)
			switch pName {
			case "type":
				opType = strings.ToLower(strings.TrimSpace(extractFHIRPatchString(pm)))
			case "path":
				path = strings.TrimSpace(extractFHIRPatchString(pm))
			case "name":
				propName = strings.TrimSpace(extractFHIRPatchString(pm))
			case "index":
				index, hasIndex = extractFHIRPatchInt(pm)
			case "source":
				source, hasSource = extractFHIRPatchInt(pm)
			case "destination":
				destination, hasDest = extractFHIRPatchInt(pm)
			case "value":
				val, hasValue = extractFHIRPatchValue(pm)
			}
		}

		switch opType {
		case "add":
			if !hasValue {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (add): missing required 'value' part", i)
			}
			tokens, err := parseFHIRPathTokens(path, rootResType)
			if err != nil {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (add): %v", i, err)
			}
			if propName == "" && len(tokens) > 0 {
				propName = tokens[len(tokens)-1]
				tokens = tokens[:len(tokens)-1]
			}
			if propName == "" {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (add): missing required 'name' part", i)
			}
			containerRaw, err := jsonPointerGet(doc, tokens)
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (add %s): %v", i, path, err)
			}
			container, ok := containerRaw.(map[string]any)
			if !ok {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (add %s): target is not an object", i, path)
			}
			if existing, exists := container[propName]; exists {
				if existingSlice, isSlice := existing.([]any); isSlice {
					if valSlice, valIsSlice := val.([]any); valIsSlice {
						container[propName] = append(existingSlice, valSlice...)
					} else {
						container[propName] = append(existingSlice, val)
					}
				} else {
					container[propName] = val
				}
			} else {
				if _, valIsSlice := val.([]any); !valIsSlice && knownRepeatingFHIRFields[propName] {
					container[propName] = []any{val}
				} else {
					container[propName] = val
				}
			}

		case "insert":
			if path == "" || !hasIndex || !hasValue {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (insert): requires 'path', 'index', and 'value' parts", i)
			}
			tokens, err := parseFHIRPathTokens(path, rootResType)
			if err != nil || len(tokens) == 0 {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (insert): invalid path %q", i, path)
			}
			arrRaw, err := jsonPointerGet(doc, tokens)
			if err != nil {
				if index == 0 {
					doc, err = jsonPointerAdd(doc, tokens, []any{val})
					if err != nil {
						return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (insert %s): %v", i, path, err)
					}
					continue
				}
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (insert %s): %v", i, path, err)
			}
			arr, ok := arrRaw.([]any)
			if !ok {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (insert %s): target is not an array", i, path)
			}
			if index < 0 || index > len(arr) {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (insert %s): index %d out of bounds (len %d)", i, path, index, len(arr))
			}
			arr = append(arr, nil)
			copy(arr[index+1:], arr[index:])
			arr[index] = val
			doc, err = jsonPointerReplace(doc, tokens, arr)
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (insert %s): %v", i, path, err)
			}

		case "delete":
			if path == "" {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (delete): missing required 'path' part", i)
			}
			tokens, err := parseFHIRPathTokens(path, rootResType)
			if err != nil || len(tokens) == 0 {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (delete): invalid path %q", i, path)
			}
			doc, _, err = jsonPointerRemove(doc, tokens)
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (delete %s): %v", i, path, err)
			}
			// If removing an array element left the parent slice empty, remove the empty slice key from its parent map
			if len(tokens) >= 2 {
				if _, idxErr := strconv.Atoi(tokens[len(tokens)-1]); idxErr == nil {
					parentTokens := tokens[:len(tokens)-1]
					if remArr, gErr := jsonPointerGet(doc, parentTokens); gErr == nil {
						if s, isSlice := remArr.([]any); isSlice && len(s) == 0 {
							doc, _, _ = jsonPointerRemove(doc, parentTokens)
						}
					}
				}
			}

		case "replace":
			if path == "" || !hasValue {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (replace): requires 'path' and 'value' parts", i)
			}
			tokens, err := parseFHIRPathTokens(path, rootResType)
			if err != nil || len(tokens) == 0 {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (replace): invalid path %q", i, path)
			}
			curVal, err := jsonPointerGet(doc, tokens)
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (replace %s): %v", i, path, err)
			}
			if curSlice, isSlice := curVal.([]any); isSlice {
				if _, valIsSlice := val.([]any); !valIsSlice && len(curSlice) == 1 {
					val = []any{val}
				}
			}
			doc, err = jsonPointerReplace(doc, tokens, val)
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (replace %s): %v", i, path, err)
			}

		case "move":
			if path == "" || !hasSource || !hasDest {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (move): requires 'path', 'source', and 'destination' parts", i)
			}
			tokens, err := parseFHIRPathTokens(path, rootResType)
			if err != nil || len(tokens) == 0 {
				return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d] (move): invalid path %q", i, path)
			}
			arrRaw, err := jsonPointerGet(doc, tokens)
			if err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (move %s): %v", i, path, err)
			}
			arr, ok := arrRaw.([]any)
			if !ok {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (move %s): target is not an array", i, path)
			}
			if source < 0 || source >= len(arr) || destination < 0 || destination >= len(arr) {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (move %s): source %d or destination %d out of bounds (len %d)", i, path, source, destination, len(arr))
			}
			if source != destination {
				elem := arr[source]
				arr = append(arr[:source], arr[source+1:]...)
				arr = append(arr, nil)
				copy(arr[destination+1:], arr[destination:])
				arr[destination] = elem
				doc, err = jsonPointerReplace(doc, tokens, arr)
				if err != nil {
					return nil, http.StatusUnprocessableEntity, fmt.Errorf("fhirpatch op[%d] (move %s): %v", i, path, err)
				}
			}

		default:
			return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch op[%d]: unsupported operation type %q", i, opType)
		}
	}

	if finalMap, ok := doc.(map[string]any); ok {
		if newRT, _ := finalMap["resourceType"].(string); rootResType != "" && newRT != rootResType {
			return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch: cannot mutate immutable resourceType from %q to %q", rootResType, newRT)
		}
		if newID, _ := finalMap["id"].(string); origID != "" && newID != origID {
			return nil, http.StatusBadRequest, fmt.Errorf("fhirpatch: cannot mutate immutable resource id from %q to %q", origID, newID)
		}
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return out, http.StatusOK, nil
}

func parseFHIRPathTokens(path, rootResType string) ([]string, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == rootResType {
		return nil, nil
	}
	segments := strings.Split(path, ".")
	var tokens []string
	for segIdx, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return nil, fmt.Errorf("empty segment in FHIRPath %q", path)
		}
		if segIdx == 0 && seg == rootResType {
			continue
		}
		for {
			openIdx := strings.IndexByte(seg, '[')
			if openIdx == -1 {
				break
			}
			closeIdx := strings.IndexByte(seg[openIdx:], ']')
			if closeIdx == -1 {
				return nil, fmt.Errorf("unclosed bracket in FHIRPath %q", path)
			}
			closeIdx += openIdx
			fieldPart := strings.TrimSpace(seg[:openIdx])
			idxStr := strings.TrimSpace(seg[openIdx+1 : closeIdx])
			n, err := strconv.Atoi(idxStr)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("invalid array index %q in FHIRPath %q", idxStr, path)
			}
			if fieldPart != "" {
				tokens = append(tokens, fieldPart)
			}
			tokens = append(tokens, strconv.Itoa(n))
			seg = strings.TrimSpace(seg[closeIdx+1:])
		}
		if seg != "" {
			tokens = append(tokens, seg)
		}
	}
	return tokens, nil
}

func extractFHIRPatchString(pm map[string]any) string {
	for _, k := range []string{"valueCode", "valueString", "valueUri", "valueId", "valueCanonical", "valueDate", "valueDateTime", "valueInstant"} {
		if s, ok := pm[k].(string); ok {
			return s
		}
	}
	return ""
}

func extractFHIRPatchInt(pm map[string]any) (int, bool) {
	for _, k := range []string{"valueInteger", "valueUnsignedInt", "valuePositiveInt"} {
		if f, ok := pm[k].(float64); ok {
			return int(f), true
		}
	}
	if s, ok := pm["valueString"].(string); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return n, true
		}
	}
	return 0, false
}

func extractFHIRPatchValue(pm map[string]any) (any, bool) {
	if subParts, ok := pm["part"].([]any); ok && len(subParts) > 0 {
		return fhirPatchPartsToMap(subParts), true
	}
	if resObj, ok := pm["resource"].(map[string]any); ok {
		return resObj, true
	}
	for k, v := range pm {
		if !strings.HasPrefix(k, "value") || len(k) <= len("value") || v == nil {
			continue
		}
		if k == "valueString" {
			if s, ok := v.(string); ok {
				ts := strings.TrimSpace(s)
				if (strings.HasPrefix(ts, "{") && strings.HasSuffix(ts, "}")) ||
					(strings.HasPrefix(ts, "[") && strings.HasSuffix(ts, "]")) {
					var decoded any
					if json.Unmarshal([]byte(ts), &decoded) == nil {
						return decoded, true
					}
				}
				return s, true
			}
		}
		return v, true
	}
	return nil, false
}

func fhirPatchPartsToMap(parts []any) map[string]any {
	out := make(map[string]any)
	for _, rp := range parts {
		pm, ok := rp.(map[string]any)
		if !ok {
			continue
		}
		name, _ := pm["name"].(string)
		if name == "" {
			continue
		}
		v, ok := extractFHIRPatchValue(pm)
		if !ok {
			continue
		}
		if existing, exists := out[name]; exists {
			if s, isSlice := existing.([]any); isSlice {
				out[name] = append(s, v)
			} else {
				out[name] = []any{existing, v}
			}
		} else if knownRepeatingFHIRFields[name] {
			if _, isSlice := v.([]any); isSlice {
				out[name] = v
			} else {
				out[name] = []any{v}
			}
		} else {
			out[name] = v
		}
	}
	return out
}
