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

	"github.com/flint-fhir/flint/automq"
	"github.com/flint-fhir/flint/pkg/auth"
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

	existingRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
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
	var patchedJSON []byte
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

	if s.validator != nil {
		outcome := s.validator.ValidateJSON(resType, patchedJSON)
		if !outcome.IsValid() {
			outcome.WriteHTTP(w, http.StatusBadRequest)
			return
		}
	}

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

	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to encode patched resource")
		return
	}

	var idx *postgres.SearchIndexes
	if extractor, ok := s.extractors[resType]; ok {
		idx, _ = extractor(tenant, resID, protoBytes)
	}
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

	rec, err := s.store.WriteResourceWithMeta(r.Context(), postgres.ResourceInput{
		TenantID:        tenant,
		ResType:         resType,
		ResID:           resID,
		ResourceProto:   protoBytes,
		SearchIndexes:   idx,
		ExpectedVersion: existingRec.ResVersion,
	})
	if errors.Is(err, postgres.ErrVersionConflict) {
		writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", err.Error())
		return
	}
	if err != nil {
		s.logger.Error("write patched resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to store patched resource")
		return
	}

	s.publishCDC(r.Context(), automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     resID,
		ProtoData: protoBytes,
		Op:        "U",
	})

	outJSON, err := s.protoToJSONWithMeta(resType, protoBytes, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		outJSON = patchedJSON
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	w.Write(outJSON)
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

	if s.validator != nil {
		outcome := s.validator.ValidateJSON(resType, body)
		if !outcome.IsValid() {
			outcome.WriteHTTP(w, http.StatusBadRequest)
			return
		}
	}

	normalizedBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, body)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("invalid FHIR JSON: %v", err))
		return
	}

	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(normalizedBody, msg); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("invalid FHIR JSON: %v", err))
		return
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

			if extractor, ok := s.extractors[resType]; ok {
				finalIdx, _ = extractor(tenant, finalResID, finalProtoBytes)
			}

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

	setAuditPatientID(r.Context(), extractPatientIDFromIndexes(resType, rec.ResID, finalIdx))

	s.publishCDC(r.Context(), automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     rec.ResID,
		ProtoData: finalProtoBytes,
		Op:        "U",
	})

	jsonBytes, err := s.protoToJSONWithMeta(resType, finalProtoBytes, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		jsonBytes = body
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, rec.ResID))
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	if rec.Created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	w.Write(jsonBytes)
}

// handleConditionalDelete handles DELETE /fhir/r4/{tenant}/{resourceType}?{searchParams}.
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

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		if err := validateAndScopeIfNoneExistForPatient(&condParams, resType, cleanID(secCtx.GetPatientId()), secCtx.GetPatientId()); err != nil {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
			return
		}
	}

	rec, err := s.store.ConditionalDeleteResource(
		r.Context(),
		tenant,
		resType,
		condParams,
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
	if errors.Is(err, postgres.ErrPreconditionFailed) {
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

	if rec != nil && rec.ResVersion > 0 {
		w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
		w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, rec.ResID))
		s.publishCDC(r.Context(), automq.Message{
			TenantID: tenant,
			ResType:  resType,
			ResID:    rec.ResID,
			Op:       "D",
		})
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
