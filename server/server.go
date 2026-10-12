// Package server implements the Flint FHIR R4 REST API.
//
// This serves FHIR resources from the Postgres operational store.
// Resources are stored as proto blobs and converted to JSON on read.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/flint-fhir/flint/automq"
	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/store/postgres"
)

var idSeq atomic.Uint64

// Server is the Flint FHIR REST API server.
type Server struct {
	store  *postgres.Store
	logger *slog.Logger
	// protoRegistry maps FHIR resource type names to proto message descriptors.
	// Used to unmarshal proto bytes back to proto messages for JSON conversion.
	protoRegistry  map[string]protoreflect.MessageDescriptor
	extractors     map[string]postgres.IndexExtractorFunc
	smartConfig    *SMARTConfig
	tokenValidator auth.TokenValidator
	validator      validation.Validator
	auditRecorder  audit.Recorder
	cdcPublisher   automq.Publisher
}

// New creates a new FHIR server.
func New(store *postgres.Store, logger *slog.Logger) *Server {
	var auditRec audit.Recorder
	if store != nil {
		auditRec = audit.NewStoreRecorder(store)
	} else {
		auditRec = audit.NewMemoryRecorder()
	}
	return &Server{
		store:         store,
		logger:        logger,
		protoRegistry: make(map[string]protoreflect.MessageDescriptor),
		extractors:    postgres.DefaultIndexExtractors(),
		validator:     validation.NewEngine(validation.DefaultOptions()),
		auditRecorder: auditRec,
	}
}

// SetCDCPublisher configures the optional AutoMQ / Kafka CDC publisher for lakehouse streaming.
func (s *Server) SetCDCPublisher(p automq.Publisher) {
	s.cdcPublisher = p
}

// CDCPublisher returns the configured AutoMQ / Kafka CDC publisher, or nil if disabled.
func (s *Server) CDCPublisher() automq.Publisher {
	return s.cdcPublisher
}

func (s *Server) publishCDC(ctx context.Context, msgs ...automq.Message) {
	if s.cdcPublisher == nil || len(msgs) == 0 {
		return
	}
	if err := s.cdcPublisher.Publish(ctx, msgs); err != nil && s.logger != nil {
		s.logger.Warn("failed to publish CDC event to AutoMQ", "error", err, "count", len(msgs))
	}
}

// SetAuditRecorder configures the HIPAA / ONC security audit event recorder.
func (s *Server) SetAuditRecorder(r audit.Recorder) {
	s.auditRecorder = r
}

// AuditRecorder returns the active security audit event recorder.
func (s *Server) AuditRecorder() audit.Recorder {
	return s.auditRecorder
}

// SetValidator configures the FHIR StructureDefinition and ValueSet validator.
// Passing nil disables validation.
func (s *Server) SetValidator(v validation.Validator) {
	s.validator = v
}

// SetTokenValidator configures the SMART on FHIR bearer token validator.
// When set, all non-public FHIR endpoints require valid tokens and matching scopes.
func (s *Server) SetTokenValidator(v auth.TokenValidator) {
	s.tokenValidator = v
}

// RegisterResourceType registers a proto message type for a FHIR resource type.
// This allows the server to unmarshal proto bytes and convert to JSON.
func (s *Server) RegisterResourceType(resType string, msg proto.Message) {
	s.protoRegistry[resType] = msg.ProtoReflect().Descriptor()
}

// RegisterIndexExtractor registers a search index extractor function for a resource type.
func (s *Server) RegisterIndexExtractor(resType string, fn postgres.IndexExtractorFunc) {
	s.extractors[resType] = fn
}

// Handler returns the HTTP handler for the FHIR API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// FHIR REST endpoints
	mux.HandleFunc("GET /fhir/r4/{tenant}/AuditEvent/{id}", s.handleReadAuditEvent)
	mux.HandleFunc("GET /fhir/r4/{tenant}/AuditEvent", s.handleSearchAuditEvents)
	mux.HandleFunc("GET /fhir/r4/{tenant}/Patient/{id}/$everything", s.handlePatientEverything)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}/_history/{vid}", s.handleVRead)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}/_history", s.handleInstanceHistory)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}", s.handleRead)
	mux.HandleFunc("PUT /fhir/r4/{tenant}/{resourceType}/{id}", s.handleUpdate)
	mux.HandleFunc("DELETE /fhir/r4/{tenant}/{resourceType}/{id}", s.handleDelete)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}", s.handleSearch)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/$validate", s.handleValidate)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$validate", s.handleValidate)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}", s.handleCreate)
	mux.HandleFunc("POST /fhir/r4/{tenant}", s.handleBundle)
	mux.HandleFunc("GET /fhir/r4/{tenant}/metadata", s.handleMetadata)
	mux.HandleFunc("GET /fhir/r4/{tenant}/.well-known/smart-configuration", s.handleSMARTConfig)

	return s.auditMiddleware(s.authMiddleware(mux))
}

// handleRead handles GET /{resourceType}/{id} — read a single resource.
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("%s/%s not found", resType, resID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		if rec != nil && rec.ResVersion > 0 {
			w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
		}
		writeOperationOutcome(w, http.StatusGone, "deleted",
			fmt.Sprintf("%s/%s has been deleted", resType, resID))
		return
	}
	if err != nil {
		s.logger.Error("read resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal error")
		return
	}

	// Convert proto bytes to JSON
	jsonBytes, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		s.logger.Error("proto to json", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "serialization error")
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	w.Write(jsonBytes)
}

// handleVRead handles GET /{resourceType}/{id}/_history/{vid} — read a specific historical version.
func (s *Server) handleVRead(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")
	vidStr := r.PathValue("vid")

	version, err := strconv.Atoi(vidStr)
	if err != nil || version <= 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid",
			fmt.Sprintf("invalid version id %q", vidStr))
		return
	}

	rec, err := s.store.ReadResourceVersion(r.Context(), tenant, resType, resID, version)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("%s/%s/_history/%d not found", resType, resID, version))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		if rec != nil && rec.ResVersion > 0 {
			w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
		}
		writeOperationOutcome(w, http.StatusGone, "deleted",
			fmt.Sprintf("%s/%s/_history/%d was a deletion", resType, resID, version))
		return
	}
	if err != nil {
		s.logger.Error("vread resource", "error", err, "type", resType, "id", resID, "version", version)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal error")
		return
	}

	jsonBytes, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		s.logger.Error("proto to json in vread", "error", err, "type", resType, "id", resID, "version", version)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "serialization error")
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	w.Write(jsonBytes)
}

// handleInstanceHistory handles GET /{resourceType}/{id}/_history — list resource versions as a history Bundle.
func (s *Server) handleInstanceHistory(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	count := 100
	offset := 0
	if v := strings.TrimSpace(r.URL.Query().Get("_count")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _count parameter %q: must be a positive integer", v))
			return
		}
		count = n
	}
	if v := strings.TrimSpace(r.URL.Query().Get("_offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _offset parameter %q: must be a non-negative integer", v))
			return
		}
		offset = n
	}

	records, total, err := s.store.ListResourceHistory(r.Context(), tenant, resType, resID, count, offset)
	if err != nil {
		s.logger.Error("list resource history", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to query resource history")
		return
	}

	entries := make([]any, 0, len(records))
	for _, rec := range records {
		method := "PUT"
		status := "200 OK"
		if rec.IsDeleted {
			method = "DELETE"
			status = "204 No Content"
		} else if rec.ResVersion == 1 {
			method = "POST"
			status = "201 Created"
		}

		entry := map[string]any{
			"fullUrl": fmt.Sprintf("%s/%s", resType, resID),
			"request": map[string]any{
				"method": method,
				"url":    fmt.Sprintf("%s/%s", resType, resID),
			},
			"response": map[string]any{
				"status":       status,
				"etag":         postgres.FormatETag(rec.ResVersion),
				"lastModified": rec.LastUpdated.UTC().Format(time.RFC3339),
			},
		}

		if !rec.IsDeleted && len(rec.ResourceProto) > 0 {
			if jsonBytes, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
				var resource any
				if err := json.Unmarshal(jsonBytes, &resource); err == nil {
					entry["resource"] = resource
				}
			}
		}

		entries = append(entries, entry)
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "history",
		"total":        total,
		"entry":        entries,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(bundle)
}

// handleCreate handles POST /{resourceType} — create a FHIR resource.
// Accepts FHIR JSON (standard flat R4 JSON or google/fhir protojson), converts to proto, stores in Postgres.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")

	md, ok := s.protoRegistry[resType]
	if !ok {
		writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
			fmt.Sprintf("resource type %s not registered", resType))
		return
	}

	// Read request body
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB max
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read body")
		return
	}

	// Validate against FHIR R4 StructureDefinitions and ValueSets if validator configured
	if s.validator != nil {
		outcome := s.validator.ValidateJSON(resType, body)
		if !outcome.IsValid() {
			outcome.WriteHTTP(w, http.StatusBadRequest)
			return
		}
	}

	// Normalize standard flat HL7 FHIR R4 JSON to google/fhir protojson shape
	normalizedBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, body)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("invalid FHIR JSON: %v", err))
		return
	}

	// Unmarshal JSON → proto
	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(normalizedBody, msg); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("invalid FHIR JSON: %v", err))
		return
	}

	// Extract resource ID from the proto
	resID := extractProtoResourceID(msg)
	if resID == "" {
		// Generate a unique ID if none provided
		resID = generateID()
		setProtoResourceID(msg, resID)
	}

	// Marshal proto → bytes for storage
	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception",
			"failed to encode resource")
		return
	}

	// Extract search indexes if available
	var idx *postgres.SearchIndexes
	if extractor, ok := s.extractors[resType]; ok {
		var extractErr error
		idx, extractErr = extractor(tenant, resID, protoBytes)
		if extractErr != nil {
			s.logger.Warn("index extraction failed, storing without indexes",
				"type", resType, "id", resID, "error", extractErr)
		}
	}
	setAuditPatientID(r.Context(), extractPatientIDFromIndexes(resType, resID, idx))

	// Enforce patient compartment isolation on incoming resource payload
	if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
		if strings.EqualFold(resType, "Patient") || postgres.IsPatientCompartmentResourceType(resType) {
			if !postgres.IndexesBelongToPatientCompartment(resType, resID, idx, cleanID(secCtx.GetPatientId())) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot create %s outside patient compartment",
						secCtx.GetPatientId(), resType))
				return
			}
		}
	}

	input := postgres.ResourceInput{
		TenantID:      tenant,
		ResType:       resType,
		ResID:         resID,
		ResourceProto: protoBytes,
		SearchIndexes: idx,
		CreateOnly:    true,
	}

	// Evaluate Conditional Create (If-None-Exist header) atomically per FHIR R4 3.1.0.5.1
	if ifNoneExist := strings.TrimSpace(r.Header.Get("If-None-Exist")); ifNoneExist != "" && s.store != nil {
		condParams, err := postgres.ParseIfNoneExist(tenant, resType, ifNoneExist)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
			if err := validateAndScopeIfNoneExistForPatient(&condParams, resType, cleanID(secCtx.GetPatientId()), secCtx.GetPatientId()); err != nil {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
				return
			}
		}

		rec, created, err := s.store.ConditionalCreateResource(r.Context(), input, condParams)
		if errors.Is(err, postgres.ErrPreconditionFailed) {
			writeOperationOutcome(w, http.StatusPreconditionFailed, "duplicate", err.Error())
			return
		}
		if errors.Is(err, postgres.ErrResourceAlreadyExists) {
			writeOperationOutcome(w, http.StatusConflict, "duplicate", err.Error())
			return
		}
		if err != nil {
			s.logger.Error("conditional create", "error", err, "type", resType, "ifNoneExist", ifNoneExist)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "conditional create failed")
			return
		}
		if created {
			s.publishCDC(r.Context(), automq.Message{
				TenantID:  tenant,
				ResType:   resType,
				ResID:     rec.ResID,
				ProtoData: rec.ResourceProto,
				Op:        "U",
			})
		}
		jsonBytes, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
		if err != nil {
			s.logger.Error("conditional create proto to json", "error", err, "type", resType, "id", rec.ResID)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "serialization error")
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, rec.ResID))
		w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
		w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
		if created {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		w.Write(jsonBytes)
		return
	}

	// Store in Postgres (CreateOnly = true ensures POST does not overwrite existing active resources)
	rec, err := s.store.WriteResourceWithMeta(r.Context(), input)
	if errors.Is(err, postgres.ErrResourceAlreadyExists) {
		writeOperationOutcome(w, http.StatusConflict, "duplicate", err.Error())
		return
	}
	if err != nil {
		s.logger.Error("create resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to store resource")
		return
	}

	s.publishCDC(r.Context(), automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     resID,
		ProtoData: rec.ResourceProto,
		Op:        "U",
	})

	jsonBytes, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		jsonBytes = body
	}

	// Return the serialized resource as JSON with 201 Created, Location, ETag, and Last-Modified
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, resID))
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusCreated)
	w.Write(jsonBytes)
}

func validateAndScopeIfNoneExistForPatient(condParams *postgres.SearchParams, resType, allowedPatientID, rawPatientID string) error {
	if allowedPatientID == "" {
		return fmt.Errorf("patient-scoped token is missing required patient_id claim")
	}
	if strings.EqualFold(resType, "Patient") {
		if condParams.ID != "" {
			for part := range strings.SplitSeq(condParams.ID, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
					return fmt.Errorf("patient-scoped token (%s) cannot evaluate If-None-Exist for patient %s", rawPatientID, trimmed)
				}
			}
		}
		condParams.ID = allowedPatientID
		return nil
	}
	if postgres.IsPatientCompartmentResourceType(resType) {
		if condParams.References == nil {
			condParams.References = make(map[string]string)
		}
		if pRef := condParams.References["patient"]; pRef != "" {
			for part := range strings.SplitSeq(pRef, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
					return fmt.Errorf("patient-scoped token (%s) cannot evaluate If-None-Exist for patient %s", rawPatientID, trimmed)
				}
			}
		}
		if sRef := condParams.References["subject"]; sRef != "" {
			for part := range strings.SplitSeq(sRef, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
					return fmt.Errorf("patient-scoped token (%s) cannot evaluate If-None-Exist for subject %s", rawPatientID, trimmed)
				}
			}
		}
		for _, ch := range condParams.Chained {
			if (ch.RefParam == "patient" || ch.RefParam == "subject") && ch.TargetParam == "_id" {
				for part := range strings.SplitSeq(ch.Value, ",") {
					if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
						return fmt.Errorf("patient-scoped token (%s) cannot evaluate If-None-Exist for %s._id=%s", rawPatientID, ch.RefParam, trimmed)
					}
				}
			}
		}
		condParams.References["patient"] = "Patient/" + allowedPatientID
		if condParams.References["subject"] != "" {
			condParams.References["subject"] = "Patient/" + allowedPatientID
		}
		return nil
	}
	return fmt.Errorf("patient-scoped token (%s) cannot evaluate If-None-Exist on non-compartment resource %s", rawPatientID, resType)
}

// handleUpdate handles PUT /{resourceType}/{id} — update or create-on-update a FHIR resource.
// Supports optimistic concurrency control via the If-Match header (returning 412 Precondition Failed on conflict).
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
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

	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB max
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
	if bodyID != "" && bodyID != resID {
		writeOperationOutcome(w, http.StatusBadRequest, "invariant",
			fmt.Sprintf("resource id %q in body does not match URL id %q", bodyID, resID))
		return
	}
	if bodyID == "" {
		setProtoResourceID(msg, resID)
	}

	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to encode resource")
		return
	}

	var idx *postgres.SearchIndexes
	if extractor, ok := s.extractors[resType]; ok {
		var extractErr error
		idx, extractErr = extractor(tenant, resID, protoBytes)
		if extractErr != nil {
			s.logger.Warn("index extraction failed on update, storing without indexes",
				"type", resType, "id", resID, "error", extractErr)
		}
	}
	setAuditPatientID(r.Context(), extractPatientIDFromIndexes(resType, resID, idx))

	// Enforce patient compartment isolation on incoming resource payload
	if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
		if strings.EqualFold(resType, "Patient") || postgres.IsPatientCompartmentResourceType(resType) {
			if !postgres.IndexesBelongToPatientCompartment(resType, resID, idx, cleanID(secCtx.GetPatientId())) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot update %s/%s outside patient compartment",
						secCtx.GetPatientId(), resType, resID))
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
		ExpectedVersion: expectedVersion,
	})
	if errors.Is(err, postgres.ErrVersionConflict) {
		writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", err.Error())
		return
	}
	if err != nil {
		s.logger.Error("update resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to update resource")
		return
	}

	s.publishCDC(r.Context(), automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     resID,
		ProtoData: protoBytes,
		Op:        "U",
	})

	jsonBytes, err := s.protoToJSONWithMeta(resType, protoBytes, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		jsonBytes = body
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	if rec.Created {
		w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, resID))
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	w.Write(jsonBytes)
}

// handleDelete handles DELETE /{resourceType}/{id} — soft-delete a FHIR resource.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	rec, err := s.store.DeleteResourceWithMeta(r.Context(), tenant, resType, resID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.logger.Error("delete resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to delete resource")
		return
	}
	if rec != nil && rec.ResVersion > 0 {
		w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
		s.publishCDC(r.Context(), automq.Message{
			TenantID: tenant,
			ResType:  resType,
			ResID:    resID,
			Op:       "D",
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

func extractProtoResourceID(msg *dynamicpb.Message) string {
	idField := msg.Descriptor().Fields().ByName("id")
	if idField == nil {
		return ""
	}
	idMsg := msg.Get(idField).Message()
	if !idMsg.IsValid() {
		return ""
	}
	valField := idMsg.Descriptor().Fields().ByName("value")
	if valField == nil {
		return ""
	}
	return idMsg.Get(valField).String()
}

func setProtoResourceID(msg *dynamicpb.Message, resID string) {
	idField := msg.Descriptor().Fields().ByName("id")
	if idField == nil {
		return
	}
	idMsg := msg.Mutable(idField).Message()
	if valField := idMsg.Descriptor().Fields().ByName("value"); valField != nil {
		idMsg.Set(valField, protoreflect.ValueOfString(resID))
	}
}

// handleValidate handles POST /{tenant}/{resourceType}/$validate and POST /{tenant}/$validate.
// Validates the incoming resource against FHIR R4 StructureDefinitions and ValueSets
// without persisting to the database, returning an OperationOutcome.
func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	_ = tenant
	resType := r.PathValue("resourceType")

	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB max
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read body")
		return
	}

	if len(body) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "empty request body for $validate")
		return
	}

	targetJSON := body
	var rootMap map[string]any
	if err := json.Unmarshal(body, &rootMap); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("malformed JSON: %v", err))
		return
	}

	// Support FHIR Parameters resource wrapper (e.g. { resourceType: "Parameters", parameter: [{ name: "resource", resource: ... }] })
	if rootType, _ := rootMap["resourceType"].(string); rootType == "Parameters" {
		if params, ok := rootMap["parameter"].([]any); ok {
			for _, p := range params {
				if pMap, ok := p.(map[string]any); ok {
					if pMap["name"] == "resource" {
						if resObj, ok := pMap["resource"].(map[string]any); ok {
							if marshaled, err := json.Marshal(resObj); err == nil {
								targetJSON = marshaled
							}
						}
					}
				}
			}
		}
	}

	if s.validator == nil {
		outcome := validation.NewOutcome()
		outcome.WriteHTTP(w, http.StatusOK)
		return
	}

	outcome := s.validator.ValidateJSON(resType, targetJSON)
	outcome.WriteHTTP(w, http.StatusOK)
}

// generateID creates a collision-free unique ID for resources and audit events.
func generateID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), idSeq.Add(1))
}

// handleReadAuditEvent handles GET /{tenant}/AuditEvent/{id}.
func (s *Server) handleReadAuditEvent(w http.ResponseWriter, r *http.Request) {
	if s.auditRecorder == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", "AuditEvent recorder not configured")
		return
	}
	tenant := r.PathValue("tenant")
	auditID := r.PathValue("id")

	rec, err := s.auditRecorder.Read(r.Context(), tenant, auditID)
	if err != nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("AuditEvent/%s not found", auditID))
		return
	}

	if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
		if cleanID(rec.AgentPatient) != cleanID(secCtx.GetPatientId()) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot read AuditEvent/%s outside patient compartment",
					secCtx.GetPatientId(), auditID))
			return
		}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(audit.ToFHIRResource(*rec))
}

// handleSearchAuditEvents handles GET /{tenant}/AuditEvent?{params}.
func (s *Server) handleSearchAuditEvents(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	q := r.URL.Query()

	params := postgres.AuditQueryParams{
		TenantID:     tenant,
		Action:       q.Get("action"),
		SubtypeCode:  q.Get("subtype"),
		Outcome:      q.Get("outcome"),
		AgentSubject: q.Get("agent"),
		AgentPatient: q.Get("patient"),
		EntityType:   q.Get("entity-type"),
		EntityID:     q.Get("entity-id"),
	}
	if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
		allowedPatientID := cleanID(secCtx.GetPatientId())
		if params.AgentPatient != "" && cleanID(params.AgentPatient) != allowedPatientID {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot query AuditEvents for patient %s",
					secCtx.GetPatientId(), params.AgentPatient))
			return
		}
		params.AgentPatient = allowedPatientID
	}
	if entityRef := q.Get("entity"); entityRef != "" {
		if parts := strings.SplitN(entityRef, "/", 2); len(parts) == 2 {
			params.EntityType = parts[0]
			params.EntityID = parts[1]
		} else {
			params.EntityID = entityRef
		}
	}
	if v := strings.TrimSpace(q.Get("_count")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _count parameter %q: must be a positive integer", v))
			return
		}
		params.Count = n
	}
	if v := strings.TrimSpace(q.Get("_offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _offset parameter %q: must be a non-negative integer", v))
			return
		}
		params.Offset = n
	}

	var records []postgres.AuditRecord
	var total int
	if s.auditRecorder != nil {
		var err error
		records, total, err = s.auditRecorder.Query(r.Context(), params)
		if err != nil {
			s.logger.Error("query audit events", "error", err, "tenant", tenant)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to query AuditEvents")
			return
		}
	}

	entries := make([]any, 0, len(records))
	for _, rec := range records {
		entries = append(entries, map[string]any{
			"fullUrl":  fmt.Sprintf("AuditEvent/%s", rec.AuditID),
			"resource": audit.ToFHIRResource(rec),
			"search": map[string]any{
				"mode": "match",
			},
		})
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
		"total":        total,
		"entry":        entries,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(bundle)
}

// handlePatientEverything handles GET /{tenant}/Patient/{id}/$everything — assemble full patient compartment Bundle.
func (s *Server) handlePatientEverything(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	patientID := r.PathValue("id")
	q := r.URL.Query()

	params := postgres.PatientEverythingParams{
		TenantID:  tenant,
		PatientID: patientID,
		Count:     100,
	}

	if typeRaw := strings.TrimSpace(q.Get("_type")); typeRaw != "" {
		for part := range strings.SplitSeq(typeRaw, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				params.ResourceTypes = append(params.ResourceTypes, trimmed)
			}
		}
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx {
		for _, rt := range params.ResourceTypes {
			if !auth.Allows(secCtx, rt, auth.ActionRead) && !auth.Allows(secCtx, rt, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for $everything _type %s", rt))
				return
			}
		}
	}

	if sinceRaw := strings.TrimSpace(q.Get("_since")); sinceRaw != "" {
		dOp, err := postgres.ParseDateOp(sinceRaw)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _since parameter %q: %v", sinceRaw, err))
			return
		}
		t := dOp.Value
		params.Since = &t
	}

	if v := strings.TrimSpace(q.Get("_count")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _count parameter %q: must be a positive integer", v))
			return
		}
		params.Count = n
	}
	if v := strings.TrimSpace(q.Get("_offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _offset parameter %q: must be a non-negative integer", v))
			return
		}
		params.Offset = n
	}

	sr, err := s.store.PatientEverything(r.Context(), params)
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
		s.logger.Error("patient $everything", "error", err, "patient", patientID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to assemble Patient/$everything")
		return
	}

	if hasSecCtx && len(sr.Includes) > 0 {
		beforeInc := len(sr.Includes)
		sr.Includes = s.filterAuthorizedIncludes(r, tenant, secCtx, sr.Includes)
		if removed := beforeInc - len(sr.Includes); removed > 0 {
			sr.Total = max(len(sr.Matches)+len(sr.Includes), sr.Total-removed)
		}
	}

	s.writeSearchSetBundle(w, sr)
}

// handleSearch handles GET /{resourceType}?{params} — search resources.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")

	if err := postgres.ValidateSearchQuery(r.URL.Query()); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	params := postgres.BuildSearchParamsFromQuery(tenant, resType, r.URL.Query())

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx {
		// Enforce SMART scopes on explicit _include target types and _revinclude source types
		for _, inc := range params.Includes {
			if inc.TargetType != "" && !auth.Allows(secCtx, inc.TargetType, auth.ActionRead) && !auth.Allows(secCtx, inc.TargetType, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for _include target %s", inc.TargetType))
				return
			}
		}
		for _, rev := range params.RevIncludes {
			if rev.SourceType != "" && !auth.Allows(secCtx, rev.SourceType, auth.ActionRead) && !auth.Allows(secCtx, rev.SourceType, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for _revinclude source %s", rev.SourceType))
				return
			}
		}

		// Enforce patient compartment scoping on searches when using a patient-restricted SMART token
		if auth.IsPatientRestricted(secCtx) {
			allowedPatientID := cleanID(secCtx.GetPatientId())
			if strings.EqualFold(resType, "Patient") {
				if params.ID != "" && cleanID(params.ID) != allowedPatientID {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("patient-scoped token (%s) cannot search patient %s",
							secCtx.GetPatientId(), params.ID))
					return
				}
				params.ID = allowedPatientID
			} else if postgres.IsPatientCompartmentResourceType(resType) {
				if params.References == nil {
					params.References = make(map[string]string)
				}
				params.References["patient"] = "Patient/" + allowedPatientID
				if params.References["subject"] != "" {
					params.References["subject"] = "Patient/" + allowedPatientID
				}
			}
		}
	}

	sr, err := s.store.Search(r.Context(), params)
	if err != nil {
		s.logger.Error("search", "error", err, "type", resType)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "search error")
		return
	}

	if hasSecCtx && len(sr.Includes) > 0 {
		sr.Includes = s.filterAuthorizedIncludes(r, tenant, secCtx, sr.Includes)
	}

	s.writeSearchSetBundle(w, sr)
}

func (s *Server) filterAuthorizedIncludes(r *http.Request, tenant string, secCtx *auth.SecurityContext, includes []postgres.SearchResult) []postgres.SearchResult {
	if secCtx == nil || len(includes) == 0 {
		return includes
	}
	patientRestricted := auth.IsPatientRestricted(secCtx)
	allowedPatientID := cleanID(secCtx.GetPatientId())
	filtered := make([]postgres.SearchResult, 0, len(includes))

	for _, inc := range includes {
		if !auth.Allows(secCtx, inc.ResType, auth.ActionRead) && !auth.Allows(secCtx, inc.ResType, auth.ActionSearch) {
			continue
		}
		if patientRestricted {
			if strings.EqualFold(inc.ResType, "Patient") {
				if cleanID(inc.ResID) != allowedPatientID {
					continue
				}
			} else if postgres.IsPatientCompartmentResourceType(inc.ResType) {
				if extractor, ok := s.extractors[inc.ResType]; ok && len(inc.ResourceProto) > 0 {
					idx, err := extractor(tenant, inc.ResID, inc.ResourceProto)
					if err != nil || !postgres.IndexesBelongToPatientCompartment(inc.ResType, inc.ResID, idx, allowedPatientID) {
						continue
					}
				} else if s.store != nil {
					belongs, err := s.store.BelongsToPatientCompartment(r.Context(), tenant, inc.ResType, inc.ResID, allowedPatientID)
					if err != nil || !belongs {
						continue
					}
				}
			}
		}
		filtered = append(filtered, inc)
	}
	return filtered
}

func (s *Server) writeSearchSetBundle(w http.ResponseWriter, sr *postgres.SearchResults) {
	entries := make([]any, 0, len(sr.Matches)+len(sr.Includes))

	// 1. Primary search matches
	for _, res := range sr.Matches {
		jsonBytes, err := s.protoToJSONWithMeta(res.ResType, res.ResourceProto, res.ResVersion, res.LastUpdated)
		if err != nil {
			s.logger.Error("proto to json in search", "error", err, "type", res.ResType, "id", res.ResID)
			continue
		}

		var resource any
		json.Unmarshal(jsonBytes, &resource)

		entries = append(entries, map[string]any{
			"fullUrl":  fmt.Sprintf("%s/%s", res.ResType, res.ResID),
			"resource": resource,
			"search": map[string]any{
				"mode": "match",
			},
		})
	}

	// 2. Included resources (_include, _revinclude, and $everything compartment resources)
	for _, inc := range sr.Includes {
		jsonBytes, err := s.protoToJSONWithMeta(inc.ResType, inc.ResourceProto, inc.ResVersion, inc.LastUpdated)
		if err != nil {
			s.logger.Error("proto to json for included resource", "error", err, "type", inc.ResType, "id", inc.ResID)
			continue
		}

		var resource any
		json.Unmarshal(jsonBytes, &resource)

		entries = append(entries, map[string]any{
			"fullUrl":  fmt.Sprintf("%s/%s", inc.ResType, inc.ResID),
			"resource": resource,
			"search": map[string]any{
				"mode": "include",
			},
		})
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
		"total":        sr.Total,
		"entry":        entries,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(bundle)
}

// handleMetadata returns the FHIR CapabilityStatement.
func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	interactions := []map[string]string{
		{"code": "read"},
		{"code": "vread"},
		{"code": "update"},
		{"code": "delete"},
		{"code": "history-instance"},
		{"code": "create"},
		{"code": "search-type"},
	}

	cap := map[string]any{
		"resourceType": "CapabilityStatement",
		"status":       "active",
		"kind":         "instance",
		"fhirVersion":  "4.0.1",
		"format":       []string{"json"},
		"software": map[string]any{
			"name":    "Flint FHIR Server",
			"version": "0.1.0",
		},
		"rest": []map[string]any{
			{
				"mode": "server",
				"resource": []map[string]any{
					{
						"type":              "Patient",
						"versioning":        "versioned-update",
						"readHistory":       true,
						"updateCreate":      true,
						"conditionalCreate": true,
						"interaction":       interactions,
						"searchParam": []map[string]string{
							{"name": "family", "type": "string"},
							{"name": "given", "type": "string"},
							{"name": "identifier", "type": "token"},
							{"name": "gender", "type": "token"},
							{"name": "birthdate", "type": "date"},
							{"name": "organization", "type": "reference"},
						},
						"operation": []map[string]string{
							{
								"name":       "everything",
								"definition": "http://hl7.org/fhir/OperationDefinition/Patient-everything",
							},
						},
					},
					{
						"type":              "Condition",
						"versioning":        "versioned-update",
						"readHistory":       true,
						"updateCreate":      true,
						"conditionalCreate": true,
						"interaction":       interactions,
						"searchParam": []map[string]string{
							{"name": "code", "type": "token"},
							{"name": "clinical-status", "type": "token"},
							{"name": "verification-status", "type": "token"},
							{"name": "category", "type": "token"},
							{"name": "patient", "type": "reference"},
							{"name": "subject", "type": "reference"},
							{"name": "encounter", "type": "reference"},
							{"name": "onset-date", "type": "date"},
						},
					},
					{
						"type":              "Encounter",
						"versioning":        "versioned-update",
						"readHistory":       true,
						"updateCreate":      true,
						"conditionalCreate": true,
						"interaction":       interactions,
						"searchParam": []map[string]string{
							{"name": "class", "type": "token"},
							{"name": "status", "type": "token"},
							{"name": "type", "type": "token"},
							{"name": "patient", "type": "reference"},
							{"name": "subject", "type": "reference"},
							{"name": "date", "type": "date"},
							{"name": "participant", "type": "reference"},
						},
					},
					{
						"type":              "Observation",
						"versioning":        "versioned-update",
						"readHistory":       true,
						"updateCreate":      true,
						"conditionalCreate": true,
						"interaction":       interactions,
						"searchParam": []map[string]string{
							{"name": "code", "type": "token"},
							{"name": "status", "type": "token"},
							{"name": "category", "type": "token"},
							{"name": "patient", "type": "reference"},
							{"name": "subject", "type": "reference"},
							{"name": "date", "type": "date"},
							{"name": "performer", "type": "reference"},
							{"name": "value-quantity", "type": "quantity"},
							{"name": "component-value-quantity", "type": "quantity"},
							{"name": "combo-value-quantity", "type": "quantity"},
						},
					},
					{
						"type":              "Practitioner",
						"versioning":        "versioned-update",
						"readHistory":       true,
						"updateCreate":      true,
						"conditionalCreate": true,
						"interaction":       interactions,
						"searchParam": []map[string]string{
							{"name": "family", "type": "string"},
							{"name": "given", "type": "string"},
							{"name": "name", "type": "string"},
							{"name": "identifier", "type": "token"},
							{"name": "active", "type": "token"},
							{"name": "gender", "type": "token"},
							{"name": "email", "type": "token"},
							{"name": "phone", "type": "token"},
							{"name": "address", "type": "string"},
						},
					},
					{
						"type":        "AuditEvent",
						"interaction": []map[string]string{{"code": "read"}, {"code": "search-type"}},
						"searchParam": []map[string]string{
							{"name": "action", "type": "token"},
							{"name": "subtype", "type": "token"},
							{"name": "outcome", "type": "token"},
							{"name": "agent", "type": "token"},
							{"name": "patient", "type": "reference"},
							{"name": "entity", "type": "reference"},
							{"name": "entity-type", "type": "token"},
							{"name": "entity-id", "type": "token"},
						},
					},
				},
				"operation": []map[string]any{
					{
						"name":       "validate",
						"definition": "http://hl7.org/fhir/OperationDefinition/Resource-validate",
					},
					{
						"name":       "everything",
						"definition": "http://hl7.org/fhir/OperationDefinition/Patient-everything",
					},
				},
			},
		},
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(cap)
}

// protoToJSONWithMeta converts proto bytes to standard flat HL7 FHIR R4 JSON using the registered message type.
func (s *Server) protoToJSONWithMeta(resType string, protoBytes []byte, version int, lastUpdated time.Time) ([]byte, error) {
	md, ok := s.protoRegistry[resType]
	if !ok {
		// Fallback: try to use Any wrapper
		var anyMsg anypb.Any
		if err := proto.Unmarshal(protoBytes, &anyMsg); err != nil {
			return nil, fmt.Errorf("no registered type for %s and proto unmarshal failed: %w", resType, err)
		}
		return protojson.Marshal(&anyMsg)
	}

	_ = protodesc.ToFileDescriptorProto // ensure import is used
	return fhirutil.DenormalizeProtoJSONToFHIRJSON(md, resType, protoBytes, version, lastUpdated)
}

// writeOperationOutcome writes a FHIR OperationOutcome error response.
func writeOperationOutcome(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "OperationOutcome",
		"issue": []map[string]any{
			{
				"severity":    "error",
				"code":        code,
				"diagnostics": message,
			},
		},
	})
}
