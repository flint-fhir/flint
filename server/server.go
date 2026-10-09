// Package server implements the Flint FHIR R4 REST API.
//
// This serves FHIR resources from the Postgres operational store.
// Resources are stored as proto blobs and converted to JSON on read.
package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/store/postgres"
)

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
}

// New creates a new FHIR server.
func New(store *postgres.Store, logger *slog.Logger) *Server {
	return &Server{
		store:         store,
		logger:        logger,
		protoRegistry: make(map[string]protoreflect.MessageDescriptor),
		extractors:    postgres.DefaultIndexExtractors(),
	}
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
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}", s.handleRead)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}", s.handleSearch)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}", s.handleCreate)
	mux.HandleFunc("POST /fhir/r4/{tenant}", s.handleBundle)
	mux.HandleFunc("GET /fhir/r4/{tenant}/metadata", s.handleMetadata)
	mux.HandleFunc("GET /fhir/r4/{tenant}/.well-known/smart-configuration", s.handleSMARTConfig)

	return s.authMiddleware(mux)
}

// handleRead handles GET /{resourceType}/{id} — read a single resource.
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	protoBytes, err := s.store.ReadResource(r.Context(), tenant, resType, resID)
	if err == sql.ErrNoRows {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("%s/%s not found", resType, resID))
		return
	}
	if err != nil {
		s.logger.Error("read resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal error")
		return
	}

	// Convert proto bytes to JSON
	jsonBytes, err := s.protoToJSON(resType, protoBytes)
	if err != nil {
		s.logger.Error("proto to json", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "serialization error")
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonBytes)
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

	// Unmarshal JSON → proto
	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(body, msg); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("invalid FHIR JSON: %v", err))
		return
	}

	// Extract resource ID from the proto
	idField := msg.Descriptor().Fields().ByName("id")
	resID := ""
	if idField != nil {
		idMsg := msg.Get(idField).Message()
		if idMsg.IsValid() {
			valField := idMsg.Descriptor().Fields().ByName("value")
			if valField != nil {
				resID = idMsg.Get(valField).String()
			}
		}
	}
	if resID == "" {
		// Generate a UUID if no ID provided
		resID = generateID()
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
	err = s.store.WriteResource(r.Context(), postgres.ResourceInput{
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

	// Return the resource as JSON with 201 Created
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, resID))
	w.WriteHeader(http.StatusCreated)
	w.Write(body) // echo back the input JSON
}

// generateID creates a simple unique ID for resources without one.
func generateID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
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
						"type":        "Patient",
						"interaction": []map[string]string{{"code": "read"}, {"code": "search-type"}},
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
						"type":        "Condition",
						"interaction": []map[string]string{{"code": "read"}, {"code": "search-type"}},
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
						"type":        "Encounter",
						"interaction": []map[string]string{{"code": "read"}, {"code": "search-type"}},
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
						"type":        "Observation",
						"interaction": []map[string]string{{"code": "read"}, {"code": "search-type"}},
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
						"type":        "Practitioner",
						"interaction": []map[string]string{{"code": "read"}, {"code": "search-type"}},
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
