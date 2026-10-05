// Package server implements the Flint FHIR R4 REST API.
//
// This serves FHIR resources from the Postgres operational store.
// Resources are stored as proto blobs and converted to JSON on read.
package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/flint-fhir/flint/store/postgres"
)

// Server is the Flint FHIR REST API server.
type Server struct {
	store  *postgres.Store
	logger *slog.Logger
	// protoRegistry maps FHIR resource type names to proto message descriptors.
	// Used to unmarshal proto bytes back to proto messages for JSON conversion.
	protoRegistry map[string]protoreflect.MessageDescriptor
}

// New creates a new FHIR server.
func New(store *postgres.Store, logger *slog.Logger) *Server {
	return &Server{
		store:         store,
		logger:        logger,
		protoRegistry: make(map[string]protoreflect.MessageDescriptor),
	}
}

// RegisterResourceType registers a proto message type for a FHIR resource type.
// This allows the server to unmarshal proto bytes and convert to JSON.
func (s *Server) RegisterResourceType(resType string, msg proto.Message) {
	s.protoRegistry[resType] = msg.ProtoReflect().Descriptor()
}

// Handler returns the HTTP handler for the FHIR API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// FHIR REST endpoints
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}", s.handleRead)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}", s.handleSearch)
	mux.HandleFunc("GET /fhir/r4/{tenant}/metadata", s.handleMetadata)

	return mux
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

// handleSearch handles GET /{resourceType}?{params} — search resources.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")

	params := postgres.SearchParams{
		TenantID:   tenant,
		ResType:    resType,
		Strings:    make(map[string]string),
		Tokens:     make(map[string]string),
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

	// Classify search parameters by type
	// Known string params
	stringParams := map[string]bool{
		"family": true, "given": true, "name": true,
		"address": true, "address-city": true, "address-state": true,
		"address-postalcode": true, "address-country": true,
	}
	// Known token params
	tokenParams := map[string]bool{
		"identifier": true, "gender": true, "active": true,
		"email": true, "phone": true, "language": true,
		"deceased": true, "status": true, "code": true,
		"category": true, "type": true, "class": true,
	}
	// Known reference params
	refParams := map[string]bool{
		"organization": true, "general-practitioner": true,
		"subject": true, "patient": true, "encounter": true,
		"performer": true, "asserter": true, "recorder": true,
	}

	for key, values := range r.URL.Query() {
		if strings.HasPrefix(key, "_") {
			continue // skip special params
		}
		value := values[0]
		switch {
		case stringParams[key]:
			params.Strings[key] = value
		case tokenParams[key]:
			params.Tokens[key] = value
		case refParams[key]:
			params.References[key] = value
		default:
			// Default to token for unknown params
			params.Tokens[key] = value
		}
	}

	results, total, err := s.store.Search(r.Context(), params)
	if err != nil {
		s.logger.Error("search", "error", err, "type", resType)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "search error")
		return
	}

	// Build FHIR Bundle response
	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
		"total":        total,
		"entry":        []any{},
	}

	entries := make([]any, 0, len(results))
	for _, res := range results {
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
