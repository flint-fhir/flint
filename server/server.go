// Package server implements the Flint FHIR R4 REST API.
//
// This serves FHIR resources from the Postgres operational store.
// Resources are stored as proto blobs and converted to JSON on read.
package server

import (
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

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
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
	jsonBytes, err := s.protoToJSON(resType, rec.ResourceProto)
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

	jsonBytes, err := s.protoToJSON(resType, rec.ResourceProto)
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
	if v := r.URL.Query().Get("_count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			count = n
		}
	}
	if v := r.URL.Query().Get("_offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
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
			if jsonBytes, err := s.protoToJSON(resType, rec.ResourceProto); err == nil {
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
// Accepts FHIR JSON, converts to proto, stores in Postgres.
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

	// Unmarshal JSON → proto
	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(body, msg); err != nil {
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

	// Store in Postgres
	rec, err := s.store.WriteResourceWithMeta(r.Context(), postgres.ResourceInput{
		TenantID:      tenant,
		ResType:       resType,
		ResID:         resID,
		ResourceProto: protoBytes,
		SearchIndexes: idx,
	})
	if err != nil {
		s.logger.Error("create resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to store resource")
		return
	}

	// Return the resource as JSON with 201 Created, ETag, and Last-Modified
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, resID))
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusCreated)
	w.Write(body) // echo back the input JSON
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

	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(body, msg); err != nil {
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

	jsonBytes, err := s.protoToJSON(resType, protoBytes)
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
	if entityRef := q.Get("entity"); entityRef != "" {
		if parts := strings.SplitN(entityRef, "/", 2); len(parts) == 2 {
			params.EntityType = parts[0]
			params.EntityID = parts[1]
		} else {
			params.EntityID = entityRef
		}
	}
	if v := q.Get("_count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Count = n
		}
	}
	if v := q.Get("_offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Offset = n
		}
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

// handleSearch handles GET /{resourceType}?{params} — search resources.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")

	params := postgres.SearchParams{
		TenantID:   tenant,
		ResType:    resType,
		Strings:    make(map[string]string),
		Tokens:     make(map[string]string),
		Dates:      make(map[string]postgres.DateOp),
		Quantities: make(map[string]postgres.QuantityOp),
		References: make(map[string]string),
	}

	// Parse _count and _offset
	if v := r.URL.Query().Get("_count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Count = n
		}
	}
	if v := r.URL.Query().Get("_offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Offset = n
		}
	}

	// Parse _include and _revinclude
	for _, incVal := range r.URL.Query()["_include"] {
		if inc, err := postgres.ParseInclude(incVal); err == nil {
			params.Includes = append(params.Includes, inc)
		}
	}
	for _, revVal := range r.URL.Query()["_revinclude"] {
		if rev, err := postgres.ParseInclude(revVal); err == nil {
			params.RevIncludes = append(params.RevIncludes, rev)
		}
	}

	// Classify search parameters by type
	stringParams := map[string]bool{
		"family": true, "given": true, "name": true,
		"address": true, "address-city": true, "address-state": true,
		"address-postalcode": true, "address-country": true,
	}
	tokenParams := map[string]bool{
		"identifier": true, "gender": true, "active": true,
		"email": true, "phone": true, "language": true,
		"deceased": true, "status": true, "code": true,
		"category": true, "type": true, "class": true,
		"clinical-status": true, "verification-status": true,
	}
	refParams := map[string]bool{
		"organization": true, "general-practitioner": true,
		"subject": true, "patient": true, "encounter": true,
		"performer": true, "asserter": true, "recorder": true,
		"participant": true,
	}
	dateParams := map[string]bool{
		"birthdate": true, "date": true, "onset-date": true,
	}
	quantityParams := map[string]bool{
		"value-quantity": true,
	}

	for key, values := range r.URL.Query() {
		if strings.HasPrefix(key, "_") {
			continue // skip special params handled above
		}
		value := values[0]

		// Check for chained parameters (e.g., patient.name=Smith)
		if chained, ok := postgres.ParseChainedParam(key, value); ok {
			params.Chained = append(params.Chained, *chained)
			continue
		}

		switch {
		case stringParams[key]:
			params.Strings[key] = value
		case tokenParams[key]:
			params.Tokens[key] = value
		case refParams[key]:
			params.References[key] = value
		case dateParams[key]:
			if dOp, err := postgres.ParseDateOp(value); err == nil {
				params.Dates[key] = dOp
			}
		case quantityParams[key]:
			if qOp, err := postgres.ParseQuantityOp(value); err == nil {
				params.Quantities[key] = qOp
			}
		default:
			// Default fallback for unknown params
			params.Tokens[key] = value
		}
	}

	sr, err := s.store.Search(r.Context(), params)
	if err != nil {
		s.logger.Error("search", "error", err, "type", resType)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "search error")
		return
	}

	// Build FHIR Bundle response
	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
		"total":        sr.Total,
		"entry":        []any{},
	}

	entries := make([]any, 0, len(sr.Matches)+len(sr.Includes))

	// 1. Primary search matches
	for _, res := range sr.Matches {
		jsonBytes, err := s.protoToJSON(res.ResType, res.ResourceProto)
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

	// 2. Included resources (_include and _revinclude)
	for _, inc := range sr.Includes {
		jsonBytes, err := s.protoToJSON(inc.ResType, inc.ResourceProto)
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
	bundle["entry"] = entries

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
						"type":         "Patient",
						"versioning":   "versioned-update",
						"readHistory":  true,
						"updateCreate": true,
						"interaction":  interactions,
						"searchParam": []map[string]string{
							{"name": "family", "type": "string"},
							{"name": "given", "type": "string"},
							{"name": "identifier", "type": "token"},
							{"name": "gender", "type": "token"},
							{"name": "birthdate", "type": "date"},
							{"name": "organization", "type": "reference"},
						},
					},
					{
						"type":         "Condition",
						"versioning":   "versioned-update",
						"readHistory":  true,
						"updateCreate": true,
						"interaction":  interactions,
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
						"type":         "Encounter",
						"versioning":   "versioned-update",
						"readHistory":  true,
						"updateCreate": true,
						"interaction":  interactions,
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
						"type":         "Observation",
						"versioning":   "versioned-update",
						"readHistory":  true,
						"updateCreate": true,
						"interaction":  interactions,
						"searchParam": []map[string]string{
							{"name": "code", "type": "token"},
							{"name": "status", "type": "token"},
							{"name": "category", "type": "token"},
							{"name": "patient", "type": "reference"},
							{"name": "subject", "type": "reference"},
							{"name": "date", "type": "date"},
							{"name": "performer", "type": "reference"},
						},
					},
					{
						"type":         "Practitioner",
						"versioning":   "versioned-update",
						"readHistory":  true,
						"updateCreate": true,
						"interaction":  interactions,
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
				},
			},
		},
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(cap)
}

// protoToJSON converts proto bytes to FHIR JSON using the registered message type.
func (s *Server) protoToJSON(resType string, protoBytes []byte) ([]byte, error) {
	md, ok := s.protoRegistry[resType]
	if !ok {
		// Fallback: try to use Any wrapper
		var anyMsg anypb.Any
		if err := proto.Unmarshal(protoBytes, &anyMsg); err != nil {
			return nil, fmt.Errorf("no registered type for %s and proto unmarshal failed: %w", resType, err)
		}
		return protojson.Marshal(&anyMsg)
	}

	// Use the registered message descriptor to create a dynamic message
	_ = protodesc.ToFileDescriptorProto // ensure import is used
	msg := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(protoBytes, msg); err != nil {
		return nil, fmt.Errorf("unmarshal %s proto: %w", resType, err)
	}

	marshaler := protojson.MarshalOptions{
		UseProtoNames:   false, // use camelCase JSON names
		EmitUnpopulated: false, // skip empty fields
	}

	return marshaler.Marshal(msg)
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
