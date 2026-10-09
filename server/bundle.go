package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

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
	Method string `json:"method"` // PUT, POST, DELETE
	URL    string `json:"url"`    // e.g. "Patient/123"
}

// handleBundle handles POST /{tenant} — process a FHIR transaction/batch Bundle.
func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")

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

	// Process each entry
	responseEntries := make([]map[string]any, 0, len(bundle.Entry))
	inputs := make([]postgres.ResourceInput, 0, len(bundle.Entry))

	for i, entry := range bundle.Entry {
		// Extract resourceType from the resource JSON
		var resourceMeta struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(entry.Resource, &resourceMeta); err != nil {
			if bundle.Type == "transaction" {
				writeOperationOutcome(w, http.StatusBadRequest, "structure",
					fmt.Sprintf("entry[%d]: invalid resource JSON", i))
				return
			}
			responseEntries = append(responseEntries, bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: invalid resource JSON", i)))
			continue
		}

		resType := resourceMeta.ResourceType
		if resType == "" {
			if bundle.Type == "transaction" {
				writeOperationOutcome(w, http.StatusBadRequest, "required",
					fmt.Sprintf("entry[%d]: missing resourceType", i))
				return
			}
			responseEntries = append(responseEntries, bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: missing resourceType", i)))
			continue
		}

		// Determine method and URL
		method := "PUT"
		resID := ""
		if entry.Request != nil {
			method = entry.Request.Method
			// Parse URL like "Patient/123"
			parts := strings.SplitN(entry.Request.URL, "/", 2)
			if len(parts) == 2 {
				resID = parts[1]
			}
		}

		// Handle DELETE
		if method == "DELETE" {
			if resID == "" {
				if bundle.Type == "transaction" {
					writeOperationOutcome(w, http.StatusBadRequest, "required",
						fmt.Sprintf("entry[%d]: DELETE requires resource ID", i))
					return
				}
				responseEntries = append(responseEntries, bundleErrorResponse(
					http.StatusBadRequest, fmt.Sprintf("entry[%d]: DELETE requires resource ID", i)))
				continue
			}
			if err := s.store.DeleteResource(r.Context(), tenant, resType, resID); err != nil {
				if bundle.Type == "transaction" {
					writeOperationOutcome(w, http.StatusInternalServerError, "exception",
						fmt.Sprintf("entry[%d]: delete failed: %v", i, err))
					return
				}
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
				if bundle.Type == "transaction" {
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
			if bundle.Type == "transaction" {
				writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
					fmt.Sprintf("entry[%d]: resource type %s not registered", i, resType))
				return
			}
			// Resource type not registered — store raw JSON as proto bytes (passthrough)
			responseEntries = append(responseEntries, bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: resource type %s not registered", i, resType)))
			continue
		}

		msg := dynamicpb.NewMessage(md)
		unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
		if err := unmarshaler.Unmarshal(entry.Resource, msg); err != nil {
			if bundle.Type == "transaction" {
				writeOperationOutcome(w, http.StatusBadRequest, "structure",
					fmt.Sprintf("entry[%d]: invalid %s JSON: %v", i, resType, err))
				return
			}
			responseEntries = append(responseEntries, bundleErrorResponse(
				http.StatusBadRequest, fmt.Sprintf("entry[%d]: invalid %s JSON: %v", i, resType, err)))
			continue
		}

		// Extract ID from proto if not in request URL
		if resID == "" {
			idField := msg.Descriptor().Fields().ByName("id")
			if idField != nil {
				idMsg := msg.Get(idField).Message()
				if idMsg.IsValid() {
					valField := idMsg.Descriptor().Fields().ByName("value")
					if valField != nil {
						resID = idMsg.Get(valField).String()
					}
				}
			}
		}
		if resID == "" {
			resID = generateID()
		}

		protoBytes, err := proto.Marshal(msg)
		if err != nil {
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

		inputs = append(inputs, postgres.ResourceInput{
			TenantID:      tenant,
			ResType:       resType,
			ResID:         resID,
			ResourceProto: protoBytes,
			SearchIndexes: idx,
		})

		status := "200 OK"
		if method == "POST" {
			status = "201 Created"
		}
		responseEntries = append(responseEntries, map[string]any{
			"fullUrl": fmt.Sprintf("%s/%s", resType, resID),
			"response": map[string]any{
				"status":   status,
				"location": fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, resID),
			},
		})
	}

	// Batch write all resources in a single transaction
	if len(inputs) > 0 {
		if err := s.store.WriteBatch(r.Context(), inputs); err != nil {
			s.logger.Error("bundle write", "error", err, "entries", len(inputs))
			writeOperationOutcome(w, http.StatusInternalServerError, "exception",
				fmt.Sprintf("batch write failed: %v", err))
			return
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
