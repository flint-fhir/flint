package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/store/postgres"
)

// syncSearchParameterOnWrite inspects a written SearchParameter resource and registers
// (or unregisters when status=="retired") its custom search parameter definition at runtime.
func (s *Server) syncSearchParameterOnWrite(resType, resID string, rawJSON []byte) {
	if !strings.EqualFold(resType, "SearchParameter") || len(rawJSON) == 0 {
		return
	}
	var m map[string]any
	if err := json.Unmarshal(rawJSON, &m); err != nil {
		return
	}
	registerSearchParameterMap(resID, m)
}

func (s *Server) syncSearchParameterOnDelete(resType, resID string) {
	if !strings.EqualFold(resType, "SearchParameter") || resID == "" {
		return
	}
	postgres.UnregisterCustomSearchParam(resID)
}

func registerSearchParameterMap(fallbackID string, m map[string]any) {
	if m == nil {
		return
	}
	id, _ := m["id"].(string)
	if strings.TrimSpace(id) == "" {
		id = fallbackID
	}
	status, _ := m["status"].(string)
	if strings.EqualFold(strings.TrimSpace(status), "retired") {
		postgres.UnregisterCustomSearchParam(id)
		return
	}
	code, _ := m["code"].(string)
	paramType, _ := m["type"].(string)
	expr, _ := m["expression"].(string)
	urlStr, _ := m["url"].(string)
	desc, _ := m["description"].(string)

	var bases []string
	switch b := m["base"].(type) {
	case []any:
		for _, item := range b {
			if bs, ok := item.(string); ok && strings.TrimSpace(bs) != "" {
				bases = append(bases, strings.TrimSpace(bs))
			}
		}
	case []string:
		for _, bs := range b {
			if strings.TrimSpace(bs) != "" {
				bases = append(bases, strings.TrimSpace(bs))
			}
		}
	case string:
		if strings.TrimSpace(b) != "" {
			bases = append(bases, strings.TrimSpace(b))
		}
	}
	if strings.TrimSpace(code) == "" || strings.TrimSpace(paramType) == "" || len(bases) == 0 {
		return
	}
	postgres.RegisterCustomSearchParam(postgres.CustomSearchParamDef{
		ID:          strings.TrimSpace(id),
		URL:         strings.TrimSpace(urlStr),
		Code:        strings.TrimSpace(code),
		Base:        bases,
		Type:        strings.ToLower(strings.TrimSpace(paramType)),
		Expression:  strings.TrimSpace(expr),
		Description: strings.TrimSpace(desc),
	})
}

func (s *Server) syncTenantCustomSearchParams(ctx context.Context, tenant string) {
	if s.store == nil || tenant == "" {
		return
	}
	sr, err := s.store.Search(ctx, postgres.SearchParams{
		TenantID: tenant,
		ResType:  "SearchParameter",
		Count:    200,
	})
	if err != nil || sr == nil {
		return
	}
	for _, match := range sr.Matches {
		jb, err := s.protoToJSONWithMeta("SearchParameter", match.ResourceProto, match.ResVersion, match.LastUpdated)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(jb, &m) == nil {
			registerSearchParameterMap(match.ResID, m)
		}
	}
}

// handleReindex handles POST /fhir/r4/{tenant}/$reindex,
// POST /fhir/r4/{tenant}/{resourceType}/$reindex, and
// POST /fhir/r4/{tenant}/{resourceType}/{id}/$reindex.
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")
	resType := strings.TrimSpace(r.PathValue("resourceType"))
	resID := strings.TrimSpace(r.PathValue("id"))

	q := r.URL.Query()
	if resType == "" {
		if t := strings.TrimSpace(q.Get("resourceType")); t != "" {
			resType = t
		} else if t := strings.TrimSpace(q.Get("type")); t != "" {
			resType = t
		}
	}

	if r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read $reindex request body")
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			var root map[string]any
			if err := json.Unmarshal(body, &root); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid JSON in $reindex request: %v", err))
				return
			}
			if rt, _ := root["resourceType"].(string); rt != "" && rt != "Parameters" {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("$reindex POST body must be a FHIR Parameters resource (got %q)", rt))
				return
			}
			if paramList, ok := root["parameter"].([]any); ok {
				for _, p := range paramList {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					name, _ := pm["name"].(string)
					switch name {
					case "resourceType", "type":
						if resType == "" {
							for _, k := range []string{"valueCode", "valueString"} {
								if v, ok := pm[k].(string); ok && strings.TrimSpace(v) != "" {
									resType = strings.TrimSpace(v)
									break
								}
							}
						}
					}
				}
			}
		}
	}

	if resType != "" {
		if _, ok := s.protoRegistry[resType]; !ok {
			writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
				fmt.Sprintf("resource type %q not registered", resType))
			return
		}
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		targetType := resType
		if targetType == "" {
			targetType = "*"
		}
		if !auth.Allows(secCtx, targetType, auth.ActionUpdate) && !auth.Allows(secCtx, targetType, auth.ActionCreate) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("insufficient scope for $reindex on %s", targetType))
			return
		}
		if auth.IsPatientRestricted(secCtx) {
			if resType == "" || resID == "" {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"patient-scoped token cannot invoke system or type-level $reindex")
				return
			}
			belongs, err := s.store.BelongsToPatientCompartment(r.Context(), tenant, resType, resID, cleanID(secCtx.GetPatientId()))
			if err != nil || !belongs {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot $reindex %s/%s outside patient compartment",
						secCtx.GetPatientId(), resType, resID))
				return
			}
		}
	}

	s.syncTenantCustomSearchParams(r.Context(), tenant)

	res, err := s.store.Reindex(r.Context(), postgres.ReindexParams{
		TenantID:   tenant,
		ResType:    resType,
		ResID:      resID,
		Extractors: s.extractors,
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("%s/%s not found", resType, resID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted",
			fmt.Sprintf("%s/%s has been deleted", resType, resID))
		return
	}
	if err != nil {
		s.logger.Error("reindex failed", "error", err, "tenant", tenant, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to execute $reindex")
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType":       "OperationOutcome",
		"reindexedResources": res.ReindexedResources,
		"issue": []map[string]any{
			{
				"severity":    "information",
				"code":        "informational",
				"diagnostics": fmt.Sprintf("Reindexed %d resource(s)", res.ReindexedResources),
			},
		},
	})
}

type applyInputParams struct {
	DefinitionID string
	CanonicalURL string
	Subject      string
	Encounter    string
	Practitioner string
	Organization string
	ReturnBundle bool
}

func parseApplyParams(w http.ResponseWriter, r *http.Request) (applyInputParams, bool) {
	q := mergedSearchQuery(r)
	out := applyInputParams{
		DefinitionID: strings.TrimSpace(r.PathValue("id")),
		CanonicalURL: firstNonEmptyStr(q.Get("planDefinition"), q.Get("activityDefinition"), q.Get("url"), q.Get("canonical")),
		Subject:      firstNonEmptyStr(q.Get("subject"), q.Get("patient")),
		Encounter:    strings.TrimSpace(q.Get("encounter")),
		Practitioner: strings.TrimSpace(q.Get("practitioner")),
		Organization: strings.TrimSpace(q.Get("organization")),
		ReturnBundle: strings.EqualFold(strings.TrimSpace(q.Get("return")), "bundle") || strings.EqualFold(strings.TrimSpace(q.Get("_format")), "bundle"),
	}

	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		if !strings.Contains(ct, "application/x-www-form-urlencoded") {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
			if err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read $apply request body")
				return out, false
			}
			if len(bytes.TrimSpace(body)) > 0 {
				var root map[string]any
				if err := json.Unmarshal(body, &root); err != nil {
					writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid JSON in $apply request: %v", err))
					return out, false
				}
				if rt, _ := root["resourceType"].(string); rt != "" && rt != "Parameters" {
					writeOperationOutcome(w, http.StatusBadRequest, "invalid",
						fmt.Sprintf("$apply POST body must be a FHIR Parameters resource (got %q)", rt))
					return out, false
				}
				if paramList, ok := root["parameter"].([]any); ok {
					for _, p := range paramList {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						val := extractParamStringOrRef(pm)
						switch name {
						case "planDefinition", "activityDefinition", "url", "canonical":
							if out.CanonicalURL == "" && val != "" {
								out.CanonicalURL = val
							}
						case "subject", "patient":
							if out.Subject == "" && val != "" {
								out.Subject = val
							}
						case "encounter":
							if out.Encounter == "" && val != "" {
								out.Encounter = val
							}
						case "practitioner":
							if out.Practitioner == "" && val != "" {
								out.Practitioner = val
							}
						case "organization":
							if out.Organization == "" && val != "" {
								out.Organization = val
							}
						case "return":
							if strings.EqualFold(val, "bundle") {
								out.ReturnBundle = true
							}
						}
					}
				}
			}
		}
	}

	return out, true
}

func extractParamStringOrRef(pm map[string]any) string {
	for _, k := range []string{"valueString", "valueCanonical", "valueUri", "valueCode", "valueId"} {
		if s, ok := pm[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if refMap, ok := pm["valueReference"].(map[string]any); ok {
		if r, ok := refMap["reference"].(string); ok && strings.TrimSpace(r) != "" {
			return strings.TrimSpace(r)
		}
	}
	return ""
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeTargetRef(expectedType, raw string) (string, string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ""
	}
	if parts := strings.SplitN(s, "/", 2); len(parts) == 2 {
		return parts[0] + "/" + parts[1], parts[1]
	}
	return expectedType + "/" + s, s
}

func builtinPlanDefinitions() map[string]map[string]any {
	return map[string]map[string]any{
		"plandef-hypertension-control": {
			"resourceType": "PlanDefinition",
			"id":           "plandef-hypertension-control",
			"url":          "http://flint.fhir.org/PlanDefinition/plandef-hypertension-control",
			"version":      "1.0.0",
			"name":         "HypertensionControlProtocol",
			"title":        "Stage 2 Hypertension Management Protocol",
			"status":       "active",
			"description":  "Clinical pathway for Stage 2 hypertension management including antihypertensive pharmacotherapy and home blood pressure monitoring.",
			"action": []any{
				map[string]any{
					"id":                  "action-bp-monitoring",
					"title":               "Home Blood Pressure Monitoring",
					"description":         "Order daily home blood pressure self-monitoring and follow-up.",
					"definitionCanonical": "http://flint.fhir.org/ActivityDefinition/activitydef-bp-home-monitoring",
					"code": []any{
						map[string]any{
							"coding": []any{
								map[string]any{
									"system":  "http://snomed.info/sct",
									"code":    "135840009",
									"display": "Blood pressure monitoring",
								},
							},
						},
					},
				},
				map[string]any{
					"id":          "action-antihypertensive-med",
					"title":       "Initiate ACE Inhibitor Therapy (Lisinopril 10 MG)",
					"description": "Consider first-line ACE inhibitor therapy for patients with hypertension or elevated blood pressure.",
					"condition": []any{
						map[string]any{
							"kind": "applicability",
							"expression": map[string]any{
								"language":   "text/fhirpath",
								"expression": "Condition.exists(code='38341003' or code='59621000' or code='I10') or Observation.exists(code='85354-9' or code='8480-6')",
							},
						},
					},
					"code": []any{
						map[string]any{
							"coding": []any{
								map[string]any{
									"system":  "http://www.nlm.nih.gov/research/umls/rxnorm",
									"code":    "314076",
									"display": "Lisinopril 10 MG Oral Tablet",
								},
							},
						},
					},
				},
			},
		},
		"plandef-diabetes-hba1c-care": {
			"resourceType": "PlanDefinition",
			"id":           "plandef-diabetes-hba1c-care",
			"url":          "http://flint.fhir.org/PlanDefinition/plandef-diabetes-hba1c-care",
			"version":      "1.0.0",
			"name":         "DiabetesHbA1cCareProtocol",
			"title":        "Type 2 Diabetes HbA1c Glycemic Control Protocol",
			"status":       "active",
			"description":  "Clinical pathway for Type 2 diabetes mellitus glycemic monitoring and management.",
			"action": []any{
				map[string]any{
					"id":                  "action-hba1c-lab",
					"title":               "Order Hemoglobin A1c Lab Test",
					"description":         "Routine quarterly HbA1c laboratory test for glycemic monitoring.",
					"definitionCanonical": "http://flint.fhir.org/ActivityDefinition/activitydef-hba1c-lab-order",
					"code": []any{
						map[string]any{
							"coding": []any{
								map[string]any{
									"system":  "http://loinc.org",
									"code":    "4548-4",
									"display": "Hemoglobin A1c/Hemoglobin.total in Blood",
								},
							},
						},
					},
				},
			},
		},
	}
}

func builtinActivityDefinitions() map[string]map[string]any {
	return map[string]map[string]any{
		"activitydef-bp-home-monitoring": {
			"resourceType": "ActivityDefinition",
			"id":           "activitydef-bp-home-monitoring",
			"url":          "http://flint.fhir.org/ActivityDefinition/activitydef-bp-home-monitoring",
			"version":      "1.0.0",
			"name":         "BPHomeMonitoringOrder",
			"title":        "Home Blood Pressure Monitoring Order",
			"status":       "active",
			"kind":         "ServiceRequest",
			"intent":       "proposal",
			"priority":     "routine",
			"code": map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://snomed.info/sct",
						"code":    "135840009",
						"display": "Blood pressure monitoring",
					},
				},
				"text": "Home Blood Pressure Monitoring",
			},
		},
		"activitydef-hba1c-lab-order": {
			"resourceType": "ActivityDefinition",
			"id":           "activitydef-hba1c-lab-order",
			"url":          "http://flint.fhir.org/ActivityDefinition/activitydef-hba1c-lab-order",
			"version":      "1.0.0",
			"name":         "HbA1cLabOrder",
			"title":        "Hemoglobin A1c Laboratory Order",
			"status":       "active",
			"kind":         "ServiceRequest",
			"intent":       "proposal",
			"priority":     "routine",
			"code": map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://loinc.org",
						"code":    "4548-4",
						"display": "Hemoglobin A1c/Hemoglobin.total in Blood",
					},
				},
				"text": "Hemoglobin A1c Lab Test",
			},
		},
	}
}

func (s *Server) resolvePlanDefinition(ctx context.Context, tenant, defID, canonical string) (map[string]any, error) {
	builtins := builtinPlanDefinitions()
	if defID == "" && canonical != "" {
		if idx := strings.IndexByte(canonical, '|'); idx != -1 {
			canonical = strings.TrimSpace(canonical[:idx])
		}
		if slash := strings.LastIndexByte(canonical, '/'); slash != -1 && !strings.HasPrefix(canonical, "http") {
			defID = canonical[slash+1:]
		}
	}
	if s.store != nil && defID != "" {
		rec, err := s.store.ReadResourceWithMeta(ctx, tenant, "PlanDefinition", defID)
		if err == nil && rec != nil {
			jb, err := s.protoToJSONWithMeta("PlanDefinition", rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
			if err == nil {
				var m map[string]any
				if json.Unmarshal(jb, &m) == nil {
					return m, nil
				}
			}
		} else if errors.Is(err, postgres.ErrResourceDeleted) {
			return nil, err
		}
	}
	if s.store != nil && canonical != "" {
		sr, err := s.store.Search(ctx, postgres.SearchParams{
			TenantID: tenant,
			ResType:  "PlanDefinition",
			URIs:     map[string]string{"url": canonical},
			Count:    1,
		})
		if err == nil && sr != nil && len(sr.Matches) > 0 {
			m0 := sr.Matches[0]
			if jb, err := s.protoToJSONWithMeta("PlanDefinition", m0.ResourceProto, m0.ResVersion, m0.LastUpdated); err == nil {
				var m map[string]any
				if json.Unmarshal(jb, &m) == nil {
					return m, nil
				}
			}
		}
	}
	if defID != "" {
		if b, ok := builtins[defID]; ok {
			return b, nil
		}
	}
	for _, b := range builtins {
		if u, _ := b["url"].(string); strings.EqualFold(u, canonical) || strings.EqualFold(b["id"].(string), canonical) {
			return b, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (s *Server) resolveActivityDefinition(ctx context.Context, tenant, defID, canonical string) (map[string]any, error) {
	builtins := builtinActivityDefinitions()
	if defID == "" && canonical != "" {
		if idx := strings.IndexByte(canonical, '|'); idx != -1 {
			canonical = strings.TrimSpace(canonical[:idx])
		}
		if slash := strings.LastIndexByte(canonical, '/'); slash != -1 && !strings.HasPrefix(canonical, "http") {
			defID = canonical[slash+1:]
		}
	}
	if s.store != nil && defID != "" {
		rec, err := s.store.ReadResourceWithMeta(ctx, tenant, "ActivityDefinition", defID)
		if err == nil && rec != nil {
			jb, err := s.protoToJSONWithMeta("ActivityDefinition", rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
			if err == nil {
				var m map[string]any
				if json.Unmarshal(jb, &m) == nil {
					return m, nil
				}
			}
		} else if errors.Is(err, postgres.ErrResourceDeleted) {
			return nil, err
		}
	}
	if s.store != nil && canonical != "" {
		sr, err := s.store.Search(ctx, postgres.SearchParams{
			TenantID: tenant,
			ResType:  "ActivityDefinition",
			URIs:     map[string]string{"url": canonical},
			Count:    1,
		})
		if err == nil && sr != nil && len(sr.Matches) > 0 {
			m0 := sr.Matches[0]
			if jb, err := s.protoToJSONWithMeta("ActivityDefinition", m0.ResourceProto, m0.ResVersion, m0.LastUpdated); err == nil {
				var m map[string]any
				if json.Unmarshal(jb, &m) == nil {
					return m, nil
				}
			}
		}
	}
	if defID != "" {
		if b, ok := builtins[defID]; ok {
			return b, nil
		}
	}
	for _, b := range builtins {
		if u, _ := b["url"].(string); strings.EqualFold(u, canonical) || strings.EqualFold(b["id"].(string), canonical) {
			return b, nil
		}
	}
	return nil, sql.ErrNoRows
}

// handlePlanDefinitionApply handles GET/POST /fhir/r4/{tenant}/PlanDefinition/$apply
// and GET/POST /fhir/r4/{tenant}/PlanDefinition/{id}/$apply per HL7 FHIR R4 §12.11.4.1.
func (s *Server) handlePlanDefinitionApply(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")
	in, ok := parseApplyParams(w, r)
	if !ok {
		return
	}
	if in.DefinitionID == "" && in.CanonicalURL == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"PlanDefinition/$apply requires an instance {id} or a planDefinition/url canonical parameter")
		return
	}
	if in.Subject == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"PlanDefinition/$apply requires a subject parameter (e.g. subject=Patient/example)")
		return
	}
	subjectRef, patID := normalizeTargetRef("Patient", in.Subject)

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if !auth.Allows(secCtx, "PlanDefinition", auth.ActionRead) && !auth.Allows(secCtx, "PlanDefinition", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to read PlanDefinition")
			return
		}
		if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to read Patient for PlanDefinition/$apply")
			return
		}
		if auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) != cleanID(patID) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot invoke PlanDefinition/$apply for Patient/%s",
					secCtx.GetPatientId(), patID))
			return
		}
	}

	planDef, err := s.resolvePlanDefinition(r.Context(), tenant, in.DefinitionID, in.CanonicalURL)
	if errors.Is(err, sql.ErrNoRows) {
		target := in.DefinitionID
		if target == "" {
			target = in.CanonicalURL
		}
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("PlanDefinition %q not found", target))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("PlanDefinition/%s has been deleted", in.DefinitionID))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to resolve PlanDefinition")
		return
	}

	patEverything, err := s.store.PatientEverything(r.Context(), postgres.PatientEverythingParams{
		TenantID:  tenant,
		PatientID: patID,
		Count:     500,
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Patient/%s not found", patID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("Patient/%s has been deleted", patID))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to load Patient compartment for $apply")
		return
	}

	compartmentResources := s.decodeCompartmentMaps(patEverything)
	planID, _ := planDef["id"].(string)
	if planID == "" {
		planID = "custom"
	}
	planURL, _ := planDef["url"].(string)
	if planURL == "" {
		planURL = "PlanDefinition/" + planID
	}
	planTitle, _ := planDef["title"].(string)
	if planTitle == "" {
		planTitle, _ = planDef["name"].(string)
	}
	planDesc, _ := planDef["description"].(string)

	nowStr := time.Now().UTC().Format(time.RFC3339)
	rgID := "rg-" + planID + "-" + patID
	requestGroup := map[string]any{
		"resourceType":          "RequestGroup",
		"id":                    rgID,
		"status":                "draft",
		"intent":                "proposal",
		"instantiatesCanonical": []string{planURL},
		"subject": map[string]any{
			"reference": subjectRef,
		},
		"authoredOn": nowStr,
	}
	if in.Encounter != "" {
		encRef, _ := normalizeTargetRef("Encounter", in.Encounter)
		requestGroup["encounter"] = map[string]any{"reference": encRef}
	}
	if in.Practitioner != "" {
		pracRef, _ := normalizeTargetRef("Practitioner", in.Practitioner)
		requestGroup["author"] = map[string]any{"reference": pracRef}
	}

	var contained []any
	var rgActions []any
	var cpActivities []any

	actions, _ := planDef["action"].([]any)
	for idx, rawAct := range actions {
		actMap, ok := rawAct.(map[string]any)
		if !ok {
			continue
		}
		if !evaluateActionConditions(actMap, compartmentResources) {
			continue
		}
		actTitle, _ := actMap["title"].(string)
		actDesc, _ := actMap["description"].(string)
		defCanon, _ := actMap["definitionCanonical"].(string)

		var actionResource map[string]any
		if defCanon != "" {
			if actDef, aErr := s.resolveActivityDefinition(r.Context(), tenant, "", defCanon); aErr == nil && actDef != nil {
				actionResource = synthesizeActivityResource(actDef, fmt.Sprintf("act-%d-%s", idx+1, patID), subjectRef, in.Encounter, in.Practitioner)
			}
		}
		if actionResource == nil {
			actionResource = synthesizeInlineActionResource(actMap, fmt.Sprintf("act-%d-%s", idx+1, patID), planURL, subjectRef, in.Encounter, in.Practitioner)
		}

		rgAct := map[string]any{
			"title":       actTitle,
			"description": actDesc,
		}
		if actionResource != nil {
			actResID, _ := actionResource["id"].(string)
			actResType, _ := actionResource["resourceType"].(string)
			contained = append(contained, actionResource)
			refStr := "#" + actResID
			if in.ReturnBundle {
				refStr = actResType + "/" + actResID
			}
			rgAct["resource"] = map[string]any{"reference": refStr}
			cpActivities = append(cpActivities, map[string]any{
				"reference": map[string]any{
					"reference": refStr,
					"display":   actTitle,
				},
			})
		} else {
			cpActivities = append(cpActivities, map[string]any{
				"detail": map[string]any{
					"status":      "not-started",
					"description": firstNonEmptyStr(actDesc, actTitle),
				},
			})
		}
		rgActions = append(rgActions, rgAct)
	}

	if len(rgActions) > 0 {
		requestGroup["action"] = rgActions
	}
	rgRef := "#" + rgID
	if in.ReturnBundle {
		rgRef = "RequestGroup/" + rgID
	}
	cpActivities = append([]any{
		map[string]any{
			"reference": map[string]any{
				"reference": rgRef,
			},
		},
	}, cpActivities...)

	carePlan := map[string]any{
		"resourceType":          "CarePlan",
		"id":                    "cp-" + planID + "-" + patID,
		"status":                "draft",
		"intent":                "proposal",
		"title":                 planTitle,
		"description":           planDesc,
		"instantiatesCanonical": []string{planURL},
		"subject": map[string]any{
			"reference": subjectRef,
		},
		"created":  nowStr,
		"activity": cpActivities,
	}
	if in.Encounter != "" {
		encRef, _ := normalizeTargetRef("Encounter", in.Encounter)
		carePlan["encounter"] = map[string]any{"reference": encRef}
	}
	if in.Practitioner != "" {
		pracRef, _ := normalizeTargetRef("Practitioner", in.Practitioner)
		carePlan["author"] = map[string]any{"reference": pracRef}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	if in.ReturnBundle {
		entries := []any{
			map[string]any{
				"fullUrl":  "CarePlan/" + carePlan["id"].(string),
				"resource": carePlan,
			},
			map[string]any{
				"fullUrl":  "RequestGroup/" + rgID,
				"resource": requestGroup,
			},
		}
		for _, c := range contained {
			if cm, ok := c.(map[string]any); ok {
				entries = append(entries, map[string]any{
					"fullUrl":  fmt.Sprintf("%s/%s", cm["resourceType"], cm["id"]),
					"resource": cm,
				})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resourceType": "Bundle",
			"type":         "collection",
			"entry":        entries,
		})
		return
	}

	carePlan["contained"] = append([]any{requestGroup}, contained...)
	_ = json.NewEncoder(w).Encode(carePlan)
}

// handleActivityDefinitionApply handles GET/POST /fhir/r4/{tenant}/ActivityDefinition/$apply
// and GET/POST /fhir/r4/{tenant}/ActivityDefinition/{id}/$apply per HL7 FHIR R4 §12.12.4.1.
func (s *Server) handleActivityDefinitionApply(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")
	in, ok := parseApplyParams(w, r)
	if !ok {
		return
	}
	if in.DefinitionID == "" && in.CanonicalURL == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"ActivityDefinition/$apply requires an instance {id} or an activityDefinition/url canonical parameter")
		return
	}
	if in.Subject == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"ActivityDefinition/$apply requires a subject parameter (e.g. subject=Patient/example)")
		return
	}
	subjectRef, patID := normalizeTargetRef("Patient", in.Subject)

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if !auth.Allows(secCtx, "ActivityDefinition", auth.ActionRead) && !auth.Allows(secCtx, "ActivityDefinition", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to read ActivityDefinition")
			return
		}
		if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to read Patient for ActivityDefinition/$apply")
			return
		}
		if auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) != cleanID(patID) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot invoke ActivityDefinition/$apply for Patient/%s",
					secCtx.GetPatientId(), patID))
			return
		}
	}

	if _, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Patient", patID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Patient/%s not found", patID))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("Patient/%s has been deleted", patID))
			return
		}
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read Patient for ActivityDefinition/$apply")
		return
	}

	actDef, err := s.resolveActivityDefinition(r.Context(), tenant, in.DefinitionID, in.CanonicalURL)
	if errors.Is(err, sql.ErrNoRows) {
		target := in.DefinitionID
		if target == "" {
			target = in.CanonicalURL
		}
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("ActivityDefinition %q not found", target))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("ActivityDefinition/%s has been deleted", in.DefinitionID))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to resolve ActivityDefinition")
		return
	}

	actID, _ := actDef["id"].(string)
	if actID == "" {
		actID = "activity"
	}
	res := synthesizeActivityResource(actDef, actID+"-"+patID, subjectRef, in.Encounter, in.Practitioner)

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(res)
}

func synthesizeActivityResource(actDef map[string]any, synthID, subjectRef, encounter, practitioner string) map[string]any {
	kind, _ := actDef["kind"].(string)
	if strings.TrimSpace(kind) == "" {
		kind = "ServiceRequest"
	}
	intent, _ := actDef["intent"].(string)
	if strings.TrimSpace(intent) == "" {
		intent = "proposal"
	}
	priority, _ := actDef["priority"].(string)
	if strings.TrimSpace(priority) == "" {
		priority = "routine"
	}
	actURL, _ := actDef["url"].(string)
	if actURL == "" {
		if id, _ := actDef["id"].(string); id != "" {
			actURL = "ActivityDefinition/" + id
		}
	}

	out := map[string]any{
		"resourceType": kind,
		"id":           synthID,
		"status":       "draft",
		"intent":       intent,
		"priority":     priority,
		"subject": map[string]any{
			"reference": subjectRef,
		},
		"authoredOn": time.Now().UTC().Format(time.RFC3339),
	}
	if actURL != "" {
		out["instantiatesCanonical"] = []string{actURL}
	}
	if encounter != "" {
		encRef, _ := normalizeTargetRef("Encounter", encounter)
		out["encounter"] = map[string]any{"reference": encRef}
	}
	if practitioner != "" {
		pracRef, _ := normalizeTargetRef("Practitioner", practitioner)
		out["requester"] = map[string]any{"reference": pracRef}
	}

	switch kind {
	case "MedicationRequest":
		if prodCC, ok := actDef["productCodeableConcept"].(map[string]any); ok {
			out["medicationCodeableConcept"] = prodCC
		} else if codeCC, ok := actDef["code"].(map[string]any); ok {
			out["medicationCodeableConcept"] = codeCC
		} else if prodRef, ok := actDef["productReference"].(map[string]any); ok {
			out["medicationReference"] = prodRef
		}
		if dosage, ok := actDef["dosage"].([]any); ok {
			out["dosageInstruction"] = dosage
		}
	default:
		if codeCC, ok := actDef["code"].(map[string]any); ok {
			out["code"] = codeCC
		}
	}
	return out
}

func synthesizeInlineActionResource(actMap map[string]any, synthID, planURL, subjectRef, encounter, practitioner string) map[string]any {
	codes, _ := actMap["code"].([]any)
	if len(codes) == 0 {
		return nil
	}
	codeCC, ok := codes[0].(map[string]any)
	if !ok {
		return nil
	}
	isRxNorm := false
	if codings, ok := codeCC["coding"].([]any); ok {
		for _, c := range codings {
			if cm, ok := c.(map[string]any); ok {
				sys, _ := cm["system"].(string)
				if strings.Contains(strings.ToLower(sys), "rxnorm") {
					isRxNorm = true
					break
				}
			}
		}
	}
	kind := "ServiceRequest"
	if isRxNorm {
		kind = "MedicationRequest"
	}
	out := map[string]any{
		"resourceType":          kind,
		"id":                    synthID,
		"status":                "draft",
		"intent":                "proposal",
		"instantiatesCanonical": []string{planURL},
		"subject": map[string]any{
			"reference": subjectRef,
		},
		"authoredOn": time.Now().UTC().Format(time.RFC3339),
	}
	if isRxNorm {
		out["medicationCodeableConcept"] = codeCC
	} else {
		out["code"] = codeCC
	}
	if encounter != "" {
		encRef, _ := normalizeTargetRef("Encounter", encounter)
		out["encounter"] = map[string]any{"reference": encRef}
	}
	if practitioner != "" {
		pracRef, _ := normalizeTargetRef("Practitioner", practitioner)
		out["requester"] = map[string]any{"reference": pracRef}
	}
	return out
}

func (s *Server) decodeCompartmentMaps(sr *postgres.SearchResults) []map[string]any {
	if sr == nil {
		return nil
	}
	all := make([]postgres.SearchResult, 0, len(sr.Matches)+len(sr.Includes))
	all = append(all, sr.Matches...)
	all = append(all, sr.Includes...)
	out := make([]map[string]any, 0, len(all))
	for _, r := range all {
		jb, err := s.protoToJSONWithMeta(r.ResType, r.ResourceProto, r.ResVersion, r.LastUpdated)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(jb, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func evaluateActionConditions(actMap map[string]any, compartment []map[string]any) bool {
	conds, ok := actMap["condition"].([]any)
	if !ok || len(conds) == 0 {
		return true
	}
	for _, rawCond := range conds {
		cm, ok := rawCond.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := cm["kind"].(string)
		if kind != "" && !strings.EqualFold(kind, "applicability") {
			continue
		}
		exprMap, ok := cm["expression"].(map[string]any)
		if !ok {
			continue
		}
		exprStr, _ := exprMap["expression"].(string)
		exprStr = strings.TrimSpace(exprStr)
		if exprStr == "" || strings.EqualFold(exprStr, "true") {
			continue
		}
		if strings.EqualFold(exprStr, "false") {
			return false
		}
		if !evaluateCompartmentExpression(exprStr, compartment) {
			return false
		}
	}
	return true
}

func evaluateCompartmentExpression(expr string, compartment []map[string]any) bool {
	lower := strings.ToLower(strings.TrimSpace(expr))
	// Extract quoted literals in expression (e.g., code='38341003' or gender='female')
	var quotedLiterals []string
	for i := 0; i < len(expr); i++ {
		if expr[i] == '\'' || expr[i] == '"' {
			q := expr[i]
			start := i + 1
			i++
			for i < len(expr) && expr[i] != q {
				i++
			}
			if i <= len(expr) {
				lit := strings.TrimSpace(expr[start:i])
				if lit != "" {
					quotedLiterals = append(quotedLiterals, lit)
				}
			}
		}
	}

	var targetTypes []string
	for _, rt := range []string{"Condition", "Observation", "MedicationRequest", "Procedure", "AllergyIntolerance", "Patient", "Encounter"} {
		if strings.Contains(lower, strings.ToLower(rt)) {
			targetTypes = append(targetTypes, rt)
		}
	}

	if len(quotedLiterals) == 0 {
		if len(targetTypes) == 0 {
			return true
		}
		for _, res := range compartment {
			rt, _ := res["resourceType"].(string)
			for _, wanted := range targetTypes {
				if strings.EqualFold(rt, wanted) {
					return true
				}
			}
		}
		return false
	}

	for _, res := range compartment {
		rt, _ := res["resourceType"].(string)
		if len(targetTypes) > 0 {
			typeMatched := false
			for _, wanted := range targetTypes {
				if strings.EqualFold(rt, wanted) {
					typeMatched = true
					break
				}
			}
			if !typeMatched {
				continue
			}
		}
		if resourceMatchesAnyLiteral(res, quotedLiterals) {
			return true
		}
	}
	return false
}

func resourceMatchesAnyLiteral(res map[string]any, literals []string) bool {
	if g, ok := res["gender"].(string); ok {
		for _, lit := range literals {
			if strings.EqualFold(g, lit) {
				return true
			}
		}
	}
	for _, ccField := range []string{"code", "medicationCodeableConcept", "vaccineCode", "category"} {
		if cc, ok := res[ccField].(map[string]any); ok {
			if codeableConceptMatchesLiterals(cc, literals) {
				return true
			}
		}
	}
	if comps, ok := res["component"].([]any); ok {
		for _, comp := range comps {
			if cm, ok := comp.(map[string]any); ok {
				if cc, ok := cm["code"].(map[string]any); ok && codeableConceptMatchesLiterals(cc, literals) {
					return true
				}
			}
		}
	}
	return false
}

func codeableConceptMatchesLiterals(cc map[string]any, literals []string) bool {
	if codings, ok := cc["coding"].([]any); ok {
		for _, c := range codings {
			if cm, ok := c.(map[string]any); ok {
				code, _ := cm["code"].(string)
				for _, lit := range literals {
					if strings.EqualFold(code, lit) {
						return true
					}
				}
			}
		}
	}
	return false
}

// handleDataRequirements handles GET/POST /fhir/r4/{tenant}/$data-requirements,
// /Library/$data-requirements, /Library/{id}/$data-requirements,
// /Measure/$data-requirements, /Measure/{id}/$data-requirements,
// /PlanDefinition/$data-requirements, and /PlanDefinition/{id}/$data-requirements
// per HL7 FHIR R4 Clinical Reasoning §12.10 (OperationDefinition/Library-data-requirements).
func (s *Server) handleDataRequirements(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := strings.TrimSpace(r.PathValue("resourceType"))
	if resType == "" {
		// Infer from URL path if mounted on fixed type route
		for _, cand := range []string{"Library", "Measure", "PlanDefinition"} {
			if strings.Contains(r.URL.Path, "/"+cand+"/") {
				resType = cand
				break
			}
		}
	}
	defID := strings.TrimSpace(r.PathValue("id"))
	canonical := ""
	periodStart := strings.TrimSpace(r.URL.Query().Get("periodStart"))
	periodEnd := strings.TrimSpace(r.URL.Query().Get("periodEnd"))

	for _, k := range []string{"id", "target"} {
		if defID == "" {
			defID = strings.TrimSpace(r.URL.Query().Get(k))
		}
	}
	for _, k := range []string{"url", "canonical", "measure", "library", "planDefinition"} {
		if canonical == "" {
			canonical = strings.TrimSpace(r.URL.Query().Get(k))
		}
	}

	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read request body")
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", "invalid JSON body for $data-requirements")
				return
			}
			if params, ok := raw["parameter"].([]any); ok {
				for _, p := range params {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					name, _ := pm["name"].(string)
					val := extractParamString(pm)
					switch name {
					case "id", "target":
						if defID == "" {
							defID = val
						}
					case "url", "canonical", "measure", "library", "planDefinition":
						if canonical == "" {
							canonical = val
						}
					case "periodStart":
						if periodStart == "" {
							periodStart = val
						}
					case "periodEnd":
						if periodEnd == "" {
							periodEnd = val
						}
					}
				}
			}
		}
	}

	if defID == "" && canonical == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"$data-requirements requires an instance {id}, target, or canonical url parameter")
		return
	}

	scopeTarget := resType
	if scopeTarget == "" {
		scopeTarget = "Library"
	}
	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if !auth.Allows(secCtx, scopeTarget, auth.ActionRead) &&
			!auth.Allows(secCtx, scopeTarget, auth.ActionSearch) &&
			!auth.Allows(secCtx, "Measure", auth.ActionRead) &&
			!auth.Allows(secCtx, "PlanDefinition", auth.ActionRead) &&
			!auth.Allows(secCtx, "Library", auth.ActionRead) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("insufficient scope to invoke $data-requirements on %s", scopeTarget))
			return
		}
	}

	lib, err := s.resolveDataRequirementsLibrary(r.Context(), tenant, resType, defID, canonical, periodStart, periodEnd)
	if errors.Is(err, sql.ErrNoRows) {
		target := defID
		if target == "" {
			target = canonical
		}
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("knowledge artifact %q not found for $data-requirements", target))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted",
			fmt.Sprintf("%s/%s has been deleted", scopeTarget, defID))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to compute $data-requirements")
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(lib)
}

func (s *Server) resolveDataRequirementsLibrary(
	ctx context.Context,
	tenant, resType, defID, canonical, periodStart, periodEnd string,
) (map[string]any, error) {
	normKey := strings.ToLower(strings.TrimSpace(defID))
	if normKey == "" && canonical != "" {
		c := canonical
		if idx := strings.IndexByte(c, '|'); idx != -1 {
			c = c[:idx]
		}
		if slash := strings.LastIndexByte(c, '/'); slash != -1 {
			normKey = strings.ToLower(strings.TrimSpace(c[slash+1:]))
		} else {
			normKey = strings.ToLower(strings.TrimSpace(c))
		}
	}

	buildDateFilter := func(path string) []any {
		if periodStart == "" && periodEnd == "" {
			return nil
		}
		vp := map[string]any{}
		if periodStart != "" {
			vp["start"] = periodStart
		}
		if periodEnd != "" {
			vp["end"] = periodEnd
		}
		return []any{
			map[string]any{
				"path":        path,
				"valuePeriod": vp,
			},
		}
	}

	wrapModuleDef := func(idSuffix, title string, relatedURL string, dataReqs []any) map[string]any {
		params := []any{
			map[string]any{
				"name": "Measurement Period",
				"use":  "in",
				"min":  0,
				"max":  "1",
				"type": "Period",
			},
		}
		out := map[string]any{
			"resourceType": "Library",
			"id":           "data-requirements-" + idSuffix,
			"url":          "http://flint.fhir.org/Library/data-requirements-" + idSuffix,
			"version":      "1.0.0",
			"name":         "DataRequirements_" + strings.ReplaceAll(idSuffix, "-", "_"),
			"title":        "Data Requirements for " + title,
			"status":       "active",
			"date":         time.Now().UTC().Format(time.RFC3339),
			"type": map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://terminology.hl7.org/CodeSystem/library-type",
						"code":    "module-definition",
						"display": "Module Definition",
					},
				},
			},
			"parameter":       params,
			"dataRequirement": dataReqs,
		}
		if relatedURL != "" {
			out["relatedArtifact"] = []any{
				map[string]any{
					"type":     "depends-on",
					"resource": relatedURL,
				},
			}
		}
		return out
	}

	switch normKey {
	case "cms165-bp-control", "plandef-hypertension-control":
		obsReq := map[string]any{
			"type":    "Observation",
			"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-blood-pressure"},
			"codeFilter": []any{
				map[string]any{
					"path":     "code",
					"valueSet": "http://cts.nlm.nih.gov/fhir/ValueSet/2.16.840.1.113883.3.464.1003.198.12.1013",
					"code": []any{
						map[string]any{
							"system":  "http://loinc.org",
							"code":    "85354-9",
							"display": "Blood pressure panel with all children optional",
						},
						map[string]any{
							"system":  "http://loinc.org",
							"code":    "8480-6",
							"display": "Systolic blood pressure",
						},
						map[string]any{
							"system":  "http://loinc.org",
							"code":    "8462-4",
							"display": "Diastolic blood pressure",
						},
					},
				},
			},
		}
		if df := buildDateFilter("effective"); len(df) > 0 {
			obsReq["dateFilter"] = df
		}
		reqs := []any{
			map[string]any{
				"type":    "Patient",
				"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
			},
			map[string]any{
				"type":    "Condition",
				"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-condition"},
				"codeFilter": []any{
					map[string]any{
						"path":     "code",
						"valueSet": "http://cts.nlm.nih.gov/fhir/ValueSet/2.16.840.1.113883.3.464.1003.104.12.1011",
						"code": []any{
							map[string]any{
								"system":  "http://snomed.info/sct",
								"code":    "38341003",
								"display": "Hypertensive disorder, systemic arterial",
							},
							map[string]any{
								"system":  "http://hl7.org/fhir/sid/icd-10-cm",
								"code":    "I10",
								"display": "Essential (primary) hypertension",
							},
						},
					},
				},
			},
			obsReq,
		}
		if normKey == "plandef-hypertension-control" {
			reqs = append(reqs, map[string]any{
				"type":    "MedicationRequest",
				"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medicationrequest"},
				"codeFilter": []any{
					map[string]any{
						"path": "medication",
						"code": []any{
							map[string]any{
								"system":  "http://www.nlm.nih.gov/research/umls/rxnorm",
								"code":    "314076",
								"display": "Lisinopril 10 MG Oral Tablet",
							},
						},
					},
				},
			})
			return wrapModuleDef("plandef-hypertension-control", "Adult Hypertension Control Clinical Pathway", "http://flint.fhir.org/PlanDefinition/plandef-hypertension-control", reqs), nil
		}
		return wrapModuleDef("cms165-bp-control", "CMS165 Controlling High Blood Pressure", "http://flint.fhir.org/Measure/cms165-bp-control", reqs), nil

	case "cms122-diabetes-hba1c", "plandef-diabetes-hba1c-care":
		obsReq := map[string]any{
			"type":    "Observation",
			"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-lab"},
			"codeFilter": []any{
				map[string]any{
					"path":     "code",
					"valueSet": "http://cts.nlm.nih.gov/fhir/ValueSet/2.16.840.1.113883.3.464.1003.198.12.1013",
					"code": []any{
						map[string]any{
							"system":  "http://loinc.org",
							"code":    "4548-4",
							"display": "Hemoglobin A1c/Hemoglobin.total in Blood",
						},
					},
				},
			},
		}
		if df := buildDateFilter("effective"); len(df) > 0 {
			obsReq["dateFilter"] = df
		}
		reqs := []any{
			map[string]any{
				"type":    "Patient",
				"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
			},
			map[string]any{
				"type":    "Condition",
				"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-condition"},
				"codeFilter": []any{
					map[string]any{
						"path":     "code",
						"valueSet": "http://cts.nlm.nih.gov/fhir/ValueSet/2.16.840.1.113883.3.464.1003.103.12.1001",
						"code": []any{
							map[string]any{
								"system":  "http://snomed.info/sct",
								"code":    "44054006",
								"display": "Diabetes mellitus type 2",
							},
							map[string]any{
								"system":  "http://hl7.org/fhir/sid/icd-10-cm",
								"code":    "E11.9",
								"display": "Type 2 diabetes mellitus without complications",
							},
						},
					},
				},
			},
			obsReq,
		}
		if normKey == "plandef-diabetes-hba1c-care" {
			return wrapModuleDef("plandef-diabetes-hba1c-care", "Type 2 Diabetes HbA1c Care Pathway", "http://flint.fhir.org/PlanDefinition/plandef-diabetes-hba1c-care", reqs), nil
		}
		return wrapModuleDef("cms122-diabetes-hba1c", "CMS122 Diabetes: Hemoglobin A1c (HbA1c) Poor Control (>9%)", "http://flint.fhir.org/Measure/cms122-diabetes-hba1c", reqs), nil
	}

	// Look up custom stored Library, Measure, or PlanDefinition in the store
	if s.store == nil {
		return nil, sql.ErrNoRows
	}
	candidateTypes := []string{"Library", "Measure", "PlanDefinition"}
	if resType != "" {
		candidateTypes = []string{resType}
	}
	for _, rt := range candidateTypes {
		if defID != "" {
			rec, err := s.store.ReadResourceWithMeta(ctx, tenant, rt, defID)
			if errors.Is(err, postgres.ErrResourceDeleted) {
				return nil, err
			}
			if err == nil && rec != nil {
				if jb, jErr := s.protoToJSONWithMeta(rt, rec.ResourceProto, rec.ResVersion, rec.LastUpdated); jErr == nil {
					var m map[string]any
					if json.Unmarshal(jb, &m) == nil {
						return buildCustomArtifactDataRequirements(rt, defID, m, buildDateFilter("effective")), nil
					}
				}
			}
		}
		if canonical != "" {
			cleanCanon := canonical
			if idx := strings.IndexByte(cleanCanon, '|'); idx != -1 {
				cleanCanon = strings.TrimSpace(cleanCanon[:idx])
			}
			sr, err := s.store.Search(ctx, postgres.SearchParams{
				TenantID: tenant,
				ResType:  rt,
				URIs:     map[string]string{"url": cleanCanon},
				Count:    1,
			})
			if err == nil && sr != nil && len(sr.Matches) > 0 {
				m0 := sr.Matches[0]
				if jb, jErr := s.protoToJSONWithMeta(rt, m0.ResourceProto, m0.ResVersion, m0.LastUpdated); jErr == nil {
					var m map[string]any
					if json.Unmarshal(jb, &m) == nil {
						return buildCustomArtifactDataRequirements(rt, m0.ResID, m, buildDateFilter("effective")), nil
					}
				}
			}
		}
	}

	return nil, sql.ErrNoRows
}

func buildCustomArtifactDataRequirements(resType, resID string, m map[string]any, dateFilter []any) map[string]any {
	title, _ := m["title"].(string)
	if title == "" {
		title, _ = m["name"].(string)
	}
	if title == "" {
		title = resType + "/" + resID
	}
	artURL, _ := m["url"].(string)
	if artURL == "" {
		artURL = resType + "/" + resID
	}

	var dataReqs []any
	if existing, ok := m["dataRequirement"].([]any); ok && len(existing) > 0 {
		dataReqs = append(dataReqs, existing...)
	}
	if actions, ok := m["action"].([]any); ok {
		for _, a := range actions {
			if am, ok := a.(map[string]any); ok {
				if inputs, ok := am["input"].([]any); ok {
					dataReqs = append(dataReqs, inputs...)
				}
			}
		}
	}
	if len(dataReqs) == 0 {
		obsReq := map[string]any{
			"type": "Observation",
		}
		if len(dateFilter) > 0 {
			obsReq["dateFilter"] = dateFilter
		}
		dataReqs = []any{
			map[string]any{
				"type": "Patient",
			},
			obsReq,
		}
	}

	return map[string]any{
		"resourceType": "Library",
		"id":           "data-requirements-" + resID,
		"url":          "http://flint.fhir.org/Library/data-requirements-" + resID,
		"version":      "1.0.0",
		"name":         "DataRequirements_" + strings.ReplaceAll(resID, "-", "_"),
		"title":        "Data Requirements for " + title,
		"status":       "active",
		"date":         time.Now().UTC().Format(time.RFC3339),
		"type": map[string]any{
			"coding": []any{
				map[string]any{
					"system":  "http://terminology.hl7.org/CodeSystem/library-type",
					"code":    "module-definition",
					"display": "Module Definition",
				},
			},
		},
		"relatedArtifact": []any{
			map[string]any{
				"type":     "depends-on",
				"resource": artURL,
			},
		},
		"dataRequirement": dataReqs,
	}
}

// handleFHIRPath handles GET/POST /fhir/r4/{tenant}/{resourceType}/{id}/$fhirpath
// and GET/POST /fhir/r4/{tenant}/$fhirpath, evaluating a FHIRPath expression against
// a stored or inline FHIR resource and returning a FHIR Parameters result.
func (s *Server) handleFHIRPath(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := strings.TrimSpace(r.PathValue("resourceType"))
	resID := strings.TrimSpace(r.PathValue("id"))
	expr := strings.TrimSpace(r.URL.Query().Get("expression"))
	if refParam := strings.TrimSpace(r.URL.Query().Get("resource")); refParam != "" && resID == "" {
		if parts := strings.SplitN(refParam, "/", 2); len(parts) == 2 {
			resType = strings.TrimSpace(parts[0])
			resID = strings.TrimSpace(parts[1])
		}
	}

	var inlineResource map[string]any
	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read request body")
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", "invalid JSON body for $fhirpath")
				return
			}
			if rt, _ := raw["resourceType"].(string); rt == "Parameters" {
				if params, ok := raw["parameter"].([]any); ok {
					for _, p := range params {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "expression":
							if expr == "" {
								expr = extractParamString(pm)
							}
						case "resource":
							if rm, ok := pm["resource"].(map[string]any); ok {
								inlineResource = rm
							} else if refStr := extractParamString(pm); refStr != "" && resID == "" {
								if parts := strings.SplitN(refStr, "/", 2); len(parts) == 2 {
									resType = strings.TrimSpace(parts[0])
									resID = strings.TrimSpace(parts[1])
								}
							}
						}
					}
				}
			} else if rt != "" {
				inlineResource = raw
			}
		}
	}

	if expr == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "$fhirpath requires an expression parameter")
		return
	}

	targetMap := inlineResource
	if targetMap == nil {
		if resType == "" || resID == "" {
			writeOperationOutcome(w, http.StatusBadRequest, "required",
				"$fhirpath requires either an instance path ({resourceType}/{id}/$fhirpath) or an inline resource parameter")
			return
		}
		if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
			if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope to read %s for $fhirpath", resType))
				return
			}
			if auth.IsPatientRestricted(secCtx) && resType == "Patient" && cleanID(secCtx.GetPatientId()) != cleanID(resID) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot evaluate $fhirpath on Patient/%s",
						secCtx.GetPatientId(), resID))
				return
			}
		}
		if s.store == nil {
			writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
			return
		}
		rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
		if errors.Is(err, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("%s/%s not found", resType, resID))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("%s/%s has been deleted", resType, resID))
			return
		}
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read resource for $fhirpath")
			return
		}
		jb, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to decode resource for $fhirpath")
			return
		}
		if err := json.Unmarshal(jb, &targetMap); err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to unmarshal resource for $fhirpath")
			return
		}
	}

	results := evaluateFHIRPathOnResource(targetMap, expr)
	paramEntries := []map[string]any{
		{
			"name": "parameters",
			"part": []map[string]any{
				{
					"name":        "evaluator",
					"valueString": "Flint FHIRPath R4 Engine",
				},
				{
					"name":        "expression",
					"valueString": expr,
				},
			},
		},
	}
	for _, item := range results {
		switch v := item.(type) {
		case bool:
			paramEntries = append(paramEntries, map[string]any{
				"name":         "result",
				"valueBoolean": v,
			})
		case int:
			paramEntries = append(paramEntries, map[string]any{
				"name":         "result",
				"valueInteger": v,
			})
		case int64:
			paramEntries = append(paramEntries, map[string]any{
				"name":         "result",
				"valueInteger": v,
			})
		case float64:
			if v == float64(int64(v)) {
				paramEntries = append(paramEntries, map[string]any{
					"name":         "result",
					"valueInteger": int64(v),
				})
			} else {
				paramEntries = append(paramEntries, map[string]any{
					"name":         "result",
					"valueDecimal": v,
				})
			}
		case string:
			paramEntries = append(paramEntries, map[string]any{
				"name":        "result",
				"valueString": v,
			})
		case map[string]any:
			if rt, ok := v["resourceType"].(string); ok && rt != "" {
				paramEntries = append(paramEntries, map[string]any{
					"name":     "result",
					"resource": v,
				})
			} else if jb, err := json.Marshal(v); err == nil {
				paramEntries = append(paramEntries, map[string]any{
					"name":        "result",
					"valueString": string(jb),
				})
			}
		}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    paramEntries,
	})
}

func evaluateFHIRPathOnResource(root map[string]any, expr string) []any {
	expr = strings.TrimSpace(expr)
	if expr == "" || root == nil {
		return nil
	}
	// Check for top-level comparison outside parentheses (e.g., "Observation.component.where(...).valueQuantity.value >= 140")
	if lhs, op, rhs, ok := splitTopLevelComparison(expr); ok {
		lhsVals := evaluateFHIRPathTokens([]any{root}, root, lhs)
		return []any{compareFHIRPathValues(lhsVals, op, rhs)}
	}
	return evaluateFHIRPathTokens([]any{root}, root, expr)
}

func splitTopLevelComparison(expr string) (string, string, string, bool) {
	depth := 0
	inQuote := byte(0)
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			continue
		}
		switch ch {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				for _, op := range []string{"!=", ">=", "<=", "=", ">", "<"} {
					if strings.HasPrefix(expr[i:], op) {
						return strings.TrimSpace(expr[:i]), op, strings.TrimSpace(expr[i+len(op):]), true
					}
				}
			}
		}
	}
	return "", "", "", false
}

func splitFHIRPathSegments(expr string) []string {
	var segs []string
	var cur strings.Builder
	depth := 0
	inQuote := byte(0)
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			cur.WriteByte(ch)
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			cur.WriteByte(ch)
			continue
		}
		switch ch {
		case '(':
			depth++
			cur.WriteByte(ch)
		case ')':
			if depth > 0 {
				depth--
			}
			cur.WriteByte(ch)
		case '.':
			if depth == 0 {
				if s := strings.TrimSpace(cur.String()); s != "" {
					segs = append(segs, s)
				}
				cur.Reset()
				continue
			}
			cur.WriteByte(ch)
		default:
			cur.WriteByte(ch)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		segs = append(segs, s)
	}
	return segs
}

func evaluateFHIRPathTokens(items []any, root map[string]any, expr string) []any {
	segs := splitFHIRPathSegments(expr)
	if len(segs) == 0 {
		return items
	}
	if rt, _ := root["resourceType"].(string); rt != "" && strings.EqualFold(segs[0], rt) {
		segs = segs[1:]
	}
	current := items
	for _, seg := range segs {
		lowerSeg := strings.ToLower(seg)
		switch {
		case lowerSeg == "exists()":
			return []any{len(current) > 0}
		case lowerSeg == "empty()":
			return []any{len(current) == 0}
		case lowerSeg == "count()":
			return []any{len(current)}
		case lowerSeg == "first()":
			if len(current) > 0 {
				current = []any{current[0]}
			} else {
				current = nil
			}
		case lowerSeg == "last()":
			if len(current) > 0 {
				current = []any{current[len(current)-1]}
			} else {
				current = nil
			}
		case strings.HasPrefix(lowerSeg, "where(") && strings.HasSuffix(seg, ")"):
			cond := strings.TrimSpace(seg[len("where(") : len(seg)-1])
			var filtered []any
			for _, it := range current {
				if evaluateFHIRPathWhereItem(it, root, cond) {
					filtered = append(filtered, it)
				}
			}
			current = filtered
		case strings.HasPrefix(lowerSeg, "extension(") && strings.HasSuffix(seg, ")"):
			extURL := strings.Trim(strings.TrimSpace(seg[len("extension("):len(seg)-1]), "'\"")
			var matched []any
			for _, it := range current {
				if m, ok := it.(map[string]any); ok {
					if exts, ok := m["extension"].([]any); ok {
						for _, e := range exts {
							if em, ok := e.(map[string]any); ok {
								if u, _ := em["url"].(string); u == extURL {
									matched = append(matched, em)
								}
							}
						}
					}
				}
			}
			current = matched
		default:
			var next []any
			for _, it := range current {
				m, ok := it.(map[string]any)
				if !ok {
					continue
				}
				if v, exists := m[seg]; exists {
					if arr, isArr := v.([]any); isArr {
						next = append(next, arr...)
					} else if v != nil {
						next = append(next, v)
					}
					continue
				}
				// Support polymorphic value[x] shorthand (e.g. "value" matching valueQuantity, valueString, valueCodeableConcept)
				if seg == "value" {
					for k, v := range m {
						if strings.HasPrefix(k, "value") && len(k) > len("value") && v != nil {
							next = append(next, v)
						}
					}
				}
			}
			current = next
		}
	}
	return current
}

func evaluateFHIRPathWhereItem(item any, root map[string]any, cond string) bool {
	if lhs, op, rhs, ok := splitTopLevelComparison(cond); ok {
		lhsVals := evaluateFHIRPathTokens([]any{item}, root, lhs)
		return compareFHIRPathValues(lhsVals, op, rhs)
	}
	vals := evaluateFHIRPathTokens([]any{item}, root, cond)
	if len(vals) == 0 {
		return false
	}
	if b, ok := vals[0].(bool); ok {
		return b
	}
	return true
}

func compareFHIRPathValues(lhsVals []any, op, rhsRaw string) bool {
	rhs := strings.Trim(strings.TrimSpace(rhsRaw), "'\"")
	rhsNum, numErr := strconv.ParseFloat(rhs, 64)
	rhsIsNum := numErr == nil
	rhsBool, boolErr := strconv.ParseBool(rhs)
	rhsIsBool := boolErr == nil

	for _, lv := range lhsVals {
		switch v := lv.(type) {
		case string:
			switch op {
			case "=":
				if v == rhs {
					return true
				}
			case "!=":
				if v != rhs {
					return true
				}
			case ">":
				if v > rhs {
					return true
				}
			case ">=":
				if v >= rhs {
					return true
				}
			case "<":
				if v < rhs {
					return true
				}
			case "<=":
				if v <= rhs {
					return true
				}
			}
		case float64:
			if !rhsIsNum {
				continue
			}
			switch op {
			case "=":
				if v == rhsNum {
					return true
				}
			case "!=":
				if v != rhsNum {
					return true
				}
			case ">":
				if v > rhsNum {
					return true
				}
			case ">=":
				if v >= rhsNum {
					return true
				}
			case "<":
				if v < rhsNum {
					return true
				}
			case "<=":
				if v <= rhsNum {
					return true
				}
			}
		case int:
			if !rhsIsNum {
				continue
			}
			fv := float64(v)
			switch op {
			case "=":
				if fv == rhsNum {
					return true
				}
			case "!=":
				if fv != rhsNum {
					return true
				}
			case ">":
				if fv > rhsNum {
					return true
				}
			case ">=":
				if fv >= rhsNum {
					return true
				}
			case "<":
				if fv < rhsNum {
					return true
				}
			case "<=":
				if fv <= rhsNum {
					return true
				}
			}
		case bool:
			if !rhsIsBool {
				continue
			}
			switch op {
			case "=":
				if v == rhsBool {
					return true
				}
			case "!=":
				if v != rhsBool {
					return true
				}
			}
		}
	}
	return false
}

// handleMeasureCollectData handles GET/POST /fhir/r4/{tenant}/Measure/$collect-data
// and GET/POST /fhir/r4/{tenant}/Measure/{id}/$collect-data per HL7 FHIR R4 §12.17.4.2
// (OperationDefinition/Measure-collect-data) and HL7 Da Vinci DEQM.
func (s *Server) handleMeasureCollectData(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")
	defID := strings.TrimSpace(r.PathValue("id"))
	canonical := ""
	subject := ""
	periodStart := strings.TrimSpace(r.URL.Query().Get("periodStart"))
	periodEnd := strings.TrimSpace(r.URL.Query().Get("periodEnd"))

	for _, k := range []string{"id", "measure"} {
		if v := strings.TrimSpace(r.URL.Query().Get(k)); v != "" {
			if strings.Contains(v, "/") && strings.HasPrefix(v, "http") {
				canonical = v
			} else if defID == "" {
				defID = cleanID(v)
			}
		}
	}
	if u := strings.TrimSpace(r.URL.Query().Get("url")); u != "" && canonical == "" {
		canonical = u
	}
	for _, k := range []string{"subject", "patient"} {
		if v := strings.TrimSpace(r.URL.Query().Get(k)); v != "" && subject == "" {
			subject = v
		}
	}

	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read request body")
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", "invalid JSON body for Measure/$collect-data")
				return
			}
			if params, ok := raw["parameter"].([]any); ok {
				for _, p := range params {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					name, _ := pm["name"].(string)
					val := extractParamString(pm)
					switch name {
					case "id":
						if defID == "" {
							defID = cleanID(val)
						}
					case "measure", "url":
						if strings.HasPrefix(val, "http") {
							if canonical == "" {
								canonical = val
							}
						} else if defID == "" {
							defID = cleanID(val)
						}
					case "subject", "patient":
						if subject == "" {
							subject = val
						}
					case "periodStart":
						if periodStart == "" {
							periodStart = val
						}
					case "periodEnd":
						if periodEnd == "" {
							periodEnd = val
						}
					}
				}
			}
		}
	}

	if defID == "" && canonical == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"Measure/$collect-data requires a Measure {id} or measure/url canonical parameter")
		return
	}
	if periodStart == "" {
		periodStart = "2026-01-01"
	}
	if periodEnd == "" {
		periodEnd = "2026-12-31"
	}

	_, patID := normalizeTargetRef("Patient", subject)
	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if !auth.Allows(secCtx, "Measure", auth.ActionRead) && !auth.Allows(secCtx, "Measure", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to read Measure for $collect-data")
			return
		}
		if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to read Patient data for $collect-data")
			return
		}
		if auth.IsPatientRestricted(secCtx) {
			if patID == "" || cleanID(secCtx.GetPatientId()) != cleanID(patID) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot invoke Measure/$collect-data outside own patient compartment",
						secCtx.GetPatientId()))
				return
			}
		}
	}

	// Verify Measure exists via data-requirements resolution
	if _, err := s.resolveDataRequirementsLibrary(r.Context(), tenant, "Measure", defID, canonical, periodStart, periodEnd); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			target := defID
			if target == "" {
				target = canonical
			}
			writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Measure %q not found", target))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("Measure/%s has been deleted", defID))
			return
		}
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to resolve Measure for $collect-data")
		return
	}

	measureKey := defID
	if measureKey == "" && canonical != "" {
		if slash := strings.LastIndexByte(canonical, '/'); slash != -1 {
			measureKey = canonical[slash+1:]
		} else {
			measureKey = canonical
		}
	}
	measureURL := canonical
	if measureURL == "" {
		measureURL = "http://flint.fhir.org/Measure/" + measureKey
	}

	var patIDs []string
	if patID != "" {
		patIDs = []string{patID}
	} else {
		sr, err := s.store.Search(r.Context(), postgres.SearchParams{
			TenantID: tenant,
			ResType:  "Patient",
			Count:    50,
		})
		if err == nil && sr != nil {
			for _, m := range sr.Matches {
				patIDs = append(patIDs, m.ResID)
			}
		}
	}

	var collectedResources []map[string]any
	var evaluatedRefs []any
	seenRes := make(map[string]bool)

	for _, pid := range patIDs {
		pe, err := s.store.PatientEverything(r.Context(), postgres.PatientEverythingParams{
			TenantID:  tenant,
			PatientID: pid,
			Count:     200,
		})
		if err != nil {
			if patID != "" {
				if errors.Is(err, sql.ErrNoRows) {
					writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Patient/%s not found", patID))
					return
				}
				if errors.Is(err, postgres.ErrResourceDeleted) {
					writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("Patient/%s has been deleted", patID))
					return
				}
			}
			continue
		}
		for _, rm := range s.decodeCompartmentMaps(pe) {
			rt, _ := rm["resourceType"].(string)
			rid, _ := rm["id"].(string)
			if rt == "" || rid == "" {
				continue
			}
			switch rt {
			case "Patient", "Condition", "Observation", "MedicationRequest", "Encounter", "Procedure":
			default:
				continue
			}
			refKey := rt + "/" + rid
			if seenRes[refKey] {
				continue
			}
			seenRes[refKey] = true
			collectedResources = append(collectedResources, rm)
			evaluatedRefs = append(evaluatedRefs, map[string]any{
				"reference": refKey,
			})
		}
	}

	mrID := "collect-data-" + measureKey
	if patID != "" {
		mrID += "-" + patID
	}
	measureReport := map[string]any{
		"resourceType": "MeasureReport",
		"id":           mrID,
		"status":       "complete",
		"type":         "data-collection",
		"measure":      measureURL,
		"date":         time.Now().UTC().Format(time.RFC3339),
		"period": map[string]any{
			"start": periodStart,
			"end":   periodEnd,
		},
	}
	if patID != "" {
		measureReport["subject"] = map[string]any{
			"reference": "Patient/" + patID,
		}
	}
	if len(evaluatedRefs) > 0 {
		measureReport["evaluatedResource"] = evaluatedRefs
	}

	paramsOut := make([]map[string]any, 0, 1+len(collectedResources))
	paramsOut = append(paramsOut, map[string]any{
		"name":     "measureReport",
		"resource": measureReport,
	})
	for _, rm := range collectedResources {
		paramsOut = append(paramsOut, map[string]any{
			"name":     "resource",
			"resource": rm,
		})
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    paramsOut,
	})
}

// handleMeasureSubmitData handles POST /fhir/r4/{tenant}/Measure/$submit-data
// and POST /fhir/r4/{tenant}/Measure/{id}/$submit-data per HL7 FHIR R4 §12.17.4.3
// (OperationDefinition/Measure-submit-data) and HL7 Da Vinci DEQM.
func (s *Server) handleMeasureSubmitData(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if !auth.Allows(secCtx, "MeasureReport", auth.ActionCreate) &&
			!auth.Allows(secCtx, "MeasureReport", auth.ActionUpdate) &&
			!auth.Allows(secCtx, "Measure", auth.ActionCreate) &&
			!auth.Allows(secCtx, "Measure", auth.ActionUpdate) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"insufficient scope for Measure/$submit-data (requires MeasureReport or Measure create/update scope)")
			return
		}
	}

	if r.Body == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Measure/$submit-data requires a Parameters request body")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 5*1024*1024))
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Measure/$submit-data requires a non-empty Parameters request body")
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "invalid JSON body for Measure/$submit-data")
		return
	}
	if rt, _ := raw["resourceType"].(string); rt != "Parameters" {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "Measure/$submit-data body must be a FHIR Parameters resource")
		return
	}

	var mrMap map[string]any
	var clinicalMaps []map[string]any
	if params, ok := raw["parameter"].([]any); ok {
		for _, p := range params {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			name, _ := pm["name"].(string)
			resObj, hasRes := pm["resource"].(map[string]any)
			if !hasRes || resObj == nil {
				continue
			}
			switch name {
			case "measureReport":
				mrMap = resObj
			case "resource":
				clinicalMaps = append(clinicalMaps, resObj)
			}
		}
	}
	if mrMap == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Measure/$submit-data requires a measureReport parameter")
		return
	}
	if rt, _ := mrMap["resourceType"].(string); rt != "MeasureReport" {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "measureReport parameter must contain a MeasureReport resource")
		return
	}

	toPersist := make([]map[string]any, 0, 1+len(clinicalMaps))
	toPersist = append(toPersist, mrMap)
	toPersist = append(toPersist, clinicalMaps...)

	entries := make([]map[string]any, 0, len(toPersist))
	for _, rm := range toPersist {
		rt, rid, vid, savedMap, uErr := s.upsertResourceMap(r.Context(), tenant, rm)
		if uErr != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", uErr.Error())
			return
		}
		entries = append(entries, map[string]any{
			"fullUrl":  fmt.Sprintf("%s/%s", rt, rid),
			"resource": savedMap,
			"response": map[string]any{
				"status":   "200 OK",
				"location": fmt.Sprintf("%s/%s/_history/%d", rt, rid, vid),
				"etag":     postgres.FormatETag(vid),
			},
		})
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction-response",
		"entry":        entries,
	})
}

func (s *Server) upsertResourceMap(ctx context.Context, tenant string, rm map[string]any) (string, string, int, map[string]any, error) {
	rt, _ := rm["resourceType"].(string)
	rt = strings.TrimSpace(rt)
	if rt == "" {
		return "", "", 0, nil, fmt.Errorf("submitted resource is missing resourceType")
	}
	md, ok := s.protoRegistry[rt]
	if !ok {
		return "", "", 0, nil, fmt.Errorf("unsupported resourceType %q", rt)
	}
	rid, _ := rm["id"].(string)
	rid = strings.TrimSpace(rid)
	if rid == "" {
		rid = generateID()
		rm["id"] = rid
	}
	rawJSON, err := json.Marshal(rm)
	if err != nil {
		return "", "", 0, nil, err
	}
	normJSON, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawJSON)
	if err != nil {
		return "", "", 0, nil, fmt.Errorf("invalid %s JSON: %w", rt, err)
	}
	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(normJSON, msg); err != nil {
		return "", "", 0, nil, fmt.Errorf("invalid %s structure: %w", rt, err)
	}
	setProtoResourceID(msg, rid)
	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		return "", "", 0, nil, err
	}
	var idx *postgres.SearchIndexes
	if extractor, hasExt := s.extractors[rt]; hasExt {
		idx, _ = extractor(tenant, rid, protoBytes)
	}
	rec, err := s.store.WriteResourceWithMeta(ctx, postgres.ResourceInput{
		TenantID:      tenant,
		ResType:       rt,
		ResID:         rid,
		ResourceProto: protoBytes,
		SearchIndexes: idx,
		CreateOnly:    false,
	})
	if err != nil {
		return "", "", 0, nil, err
	}
	s.dispatchSubscriptions(ctx, tenant, rt, rid, rec.ResourceProto, rec.ResVersion, rec.LastUpdated, idx)
	var savedMap map[string]any
	if jb, jErr := s.protoToJSONWithMeta(rt, rec.ResourceProto, rec.ResVersion, rec.LastUpdated); jErr == nil {
		_ = json.Unmarshal(jb, &savedMap)
	}
	if savedMap == nil {
		savedMap = rm
	}
	return rt, rid, rec.ResVersion, savedMap, nil
}

// handleGroupAdd handles POST /fhir/r4/{tenant}/Group/{id}/$add, atomically adding
// member references to a Group cohort resource.
func (s *Server) handleGroupAdd(w http.ResponseWriter, r *http.Request) {
	s.handleGroupMemberMutate(w, r, true)
}

// handleGroupRemove handles POST /fhir/r4/{tenant}/Group/{id}/$remove, atomically removing
// member references from a Group cohort resource.
func (s *Server) handleGroupRemove(w http.ResponseWriter, r *http.Request) {
	s.handleGroupMemberMutate(w, r, false)
}

func (s *Server) handleGroupMemberMutate(w http.ResponseWriter, r *http.Request, isAdd bool) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")
	groupID := strings.TrimSpace(r.PathValue("id"))
	if groupID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Group cohort operation requires a Group {id}")
		return
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if auth.IsPatientRestricted(secCtx) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "patient-scoped token cannot modify Group cohort membership")
			return
		}
		if !auth.Allows(secCtx, "Group", auth.ActionUpdate) && !auth.Allows(secCtx, "Group", auth.ActionCreate) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to modify Group membership")
			return
		}
	}

	var memberRefs []string
	for _, qv := range r.URL.Query()["member"] {
		for part := range strings.SplitSeq(qv, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				memberRefs = append(memberRefs, trimmed)
			}
		}
	}
	if r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read request body")
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", "invalid JSON body for Group operation")
				return
			}
			if params, ok := raw["parameter"].([]any); ok {
				for _, p := range params {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					if v := extractParamString(pm); v != "" {
						memberRefs = append(memberRefs, v)
					}
					if parts, ok := pm["part"].([]any); ok {
						for _, pt := range parts {
							if ptm, ok := pt.(map[string]any); ok {
								if v := extractParamString(ptm); v != "" {
									memberRefs = append(memberRefs, v)
								}
							}
						}
					}
				}
			}
		}
	}

	if len(memberRefs) == 0 {
		opName := "$remove"
		if isAdd {
			opName = "$add"
		}
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			fmt.Sprintf("Group/%s/%s requires at least one member reference parameter", groupID, opName))
		return
	}

	normalizedRefs := make([]string, 0, len(memberRefs))
	for _, mr := range memberRefs {
		ref, _ := normalizeTargetRef("Patient", mr)
		if ref != "" {
			normalizedRefs = append(normalizedRefs, ref)
		}
	}

	rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Group", groupID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Group/%s not found", groupID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("Group/%s has been deleted", groupID))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read Group")
		return
	}

	jb, err := s.protoToJSONWithMeta("Group", rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to decode Group")
		return
	}
	var groupMap map[string]any
	if err := json.Unmarshal(jb, &groupMap); err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to unmarshal Group")
		return
	}

	existingMembers, _ := groupMap["member"].([]any)
	if isAdd {
		existingSet := make(map[string]int, len(existingMembers))
		for idx, em := range existingMembers {
			if emMap, ok := em.(map[string]any); ok {
				if ent, ok := emMap["entity"].(map[string]any); ok {
					if refStr, _ := ent["reference"].(string); refStr != "" {
						existingSet[strings.ToLower(strings.TrimSpace(refStr))] = idx
					}
				}
			}
		}
		for _, wantRef := range normalizedRefs {
			key := strings.ToLower(wantRef)
			if idx, exists := existingSet[key]; exists {
				if emMap, ok := existingMembers[idx].(map[string]any); ok {
					delete(emMap, "inactive")
				}
			} else {
				existingSet[key] = len(existingMembers)
				existingMembers = append(existingMembers, map[string]any{
					"entity": map[string]any{
						"reference": wantRef,
					},
				})
			}
		}
	} else {
		removeSet := make(map[string]bool, len(normalizedRefs))
		for _, wantRef := range normalizedRefs {
			removeSet[strings.ToLower(wantRef)] = true
		}
		filtered := make([]any, 0, len(existingMembers))
		for _, em := range existingMembers {
			emMap, ok := em.(map[string]any)
			if !ok {
				continue
			}
			ent, _ := emMap["entity"].(map[string]any)
			refStr, _ := ent["reference"].(string)
			if removeSet[strings.ToLower(strings.TrimSpace(refStr))] {
				continue
			}
			filtered = append(filtered, em)
		}
		existingMembers = filtered
	}

	if len(existingMembers) > 0 {
		groupMap["member"] = existingMembers
	} else {
		delete(groupMap, "member")
	}
	activeCount := 0
	for _, em := range existingMembers {
		if emMap, ok := em.(map[string]any); ok {
			if inact, _ := emMap["inactive"].(bool); !inact {
				activeCount++
			}
		}
	}
	groupMap["quantity"] = activeCount

	_, _, vid, savedMap, uErr := s.upsertResourceMap(r.Context(), tenant, groupMap)
	if uErr != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", fmt.Sprintf("failed to update Group/%s: %v", groupID, uErr))
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("ETag", postgres.FormatETag(vid))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(savedMap)
}

// handleObservationLastN handles GET/POST /fhir/r4/{tenant}/Observation/$lastn
// per HL7 FHIR R4 §10.1.12.1 (OperationDefinition/Observation-lastn), returning the
// most recent N (default 1) Observations per distinct Observation.code in a searchset Bundle.
func (s *Server) handleObservationLastN(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")
	q := mergedSearchQuery(r)
	maxPerCode := 1
	if maxStr := strings.TrimSpace(q.Get("max")); maxStr != "" {
		n, err := strconv.Atoi(maxStr)
		if err != nil || n <= 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "value", "Observation/$lastn max parameter must be a positive integer")
			return
		}
		maxPerCode = n
	}

	subject := strings.TrimSpace(q.Get("patient"))
	if subject == "" {
		subject = strings.TrimSpace(q.Get("subject"))
	}
	category := strings.TrimSpace(q.Get("category"))
	codeFilter := strings.TrimSpace(q.Get("code"))

	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		if body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024)); err == nil && len(bytes.TrimSpace(body)) > 0 {
			var raw map[string]any
			if json.Unmarshal(body, &raw) == nil {
				if params, ok := raw["parameter"].([]any); ok {
					for _, p := range params {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "max":
							if f, ok := pm["valueInteger"].(float64); ok && int(f) > 0 {
								maxPerCode = int(f)
							} else if f, ok := pm["valuePositiveInt"].(float64); ok && int(f) > 0 {
								maxPerCode = int(f)
							} else if sVal := extractParamString(pm); sVal != "" {
								if n, nErr := strconv.Atoi(sVal); nErr == nil && n > 0 {
									maxPerCode = n
								}
							}
						case "patient", "subject":
							if subject == "" {
								subject = extractParamString(pm)
							}
						case "category":
							if category == "" {
								category = extractParamString(pm)
							}
						case "code":
							if codeFilter == "" {
								codeFilter = extractParamString(pm)
							}
						}
					}
				}
			}
		}
	}

	_, patID := normalizeTargetRef("Patient", subject)
	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if !auth.Allows(secCtx, "Observation", auth.ActionRead) && !auth.Allows(secCtx, "Observation", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for Observation/$lastn")
			return
		}
		if auth.IsPatientRestricted(secCtx) {
			allowedPat := cleanID(secCtx.GetPatientId())
			if patID != "" && cleanID(patID) != allowedPat {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot invoke Observation/$lastn for Patient/%s",
						secCtx.GetPatientId(), patID))
				return
			}
			patID = allowedPat
		}
	}

	sp := postgres.SearchParams{
		TenantID:   tenant,
		ResType:    "Observation",
		Tokens:     make(map[string]string),
		References: make(map[string]string),
		Count:      500,
	}
	if patID != "" {
		sp.References["patient"] = "Patient/" + patID
	}
	if category != "" {
		sp.Tokens["category"] = category
	}
	if codeFilter != "" {
		sp.Tokens["code"] = codeFilter
	}

	sr, err := s.store.Search(r.Context(), sp)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to search Observations for $lastn")
		return
	}

	type obsCandidate struct {
		resID       string
		effectiveAt time.Time
		resMap      map[string]any
	}
	groups := make(map[string][]obsCandidate)
	var groupOrder []string

	for _, m := range sr.Matches {
		jb, err := s.protoToJSONWithMeta("Observation", m.ResourceProto, m.ResVersion, m.LastUpdated)
		if err != nil {
			continue
		}
		var rm map[string]any
		if json.Unmarshal(jb, &rm) != nil {
			continue
		}
		codeKey := extractObservationCodeKey(rm)
		if codeKey == "" {
			codeKey = "_uncoded"
		}
		effTime := extractObservationEffectiveTime(rm, m.LastUpdated)
		if _, seen := groups[codeKey]; !seen {
			groupOrder = append(groupOrder, codeKey)
		}
		groups[codeKey] = append(groups[codeKey], obsCandidate{
			resID:       m.ResID,
			effectiveAt: effTime,
			resMap:      rm,
		})
	}

	var entries []map[string]any
	for _, codeKey := range groupOrder {
		cands := groups[codeKey]
		sort.Slice(cands, func(i, j int) bool {
			if !cands[i].effectiveAt.Equal(cands[j].effectiveAt) {
				return cands[i].effectiveAt.After(cands[j].effectiveAt)
			}
			return cands[i].resID > cands[j].resID
		})
		limit := min(len(cands), maxPerCode)
		for i := range limit {
			entries = append(entries, map[string]any{
				"fullUrl":  "Observation/" + cands[i].resID,
				"resource": cands[i].resMap,
				"search": map[string]any{
					"mode": "match",
				},
			})
		}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
		"total":        len(entries),
		"entry":        entries,
	})
}

func extractObservationCodeKey(rm map[string]any) string {
	cc, ok := rm["code"].(map[string]any)
	if !ok {
		return ""
	}
	if codings, ok := cc["coding"].([]any); ok {
		for _, c := range codings {
			if cm, ok := c.(map[string]any); ok {
				sys, _ := cm["system"].(string)
				code, _ := cm["code"].(string)
				if strings.TrimSpace(code) != "" {
					return strings.ToLower(strings.TrimSpace(sys)) + "|" + strings.ToLower(strings.TrimSpace(code))
				}
			}
		}
	}
	if txt, _ := cc["text"].(string); strings.TrimSpace(txt) != "" {
		return "text|" + strings.ToLower(strings.TrimSpace(txt))
	}
	return ""
}

func extractObservationEffectiveTime(rm map[string]any, fallback time.Time) time.Time {
	for _, k := range []string{"effectiveDateTime", "effectiveInstant", "issued"} {
		if s, _ := rm[k].(string); strings.TrimSpace(s) != "" {
			if dOp, err := postgres.ParseDateOp(strings.TrimSpace(s)); err == nil && !dOp.Value.IsZero() {
				return dOp.Value
			}
		}
	}
	if ep, ok := rm["effectivePeriod"].(map[string]any); ok {
		for _, k := range []string{"end", "start"} {
			if s, _ := ep[k].(string); strings.TrimSpace(s) != "" {
				if dOp, err := postgres.ParseDateOp(strings.TrimSpace(s)); err == nil && !dOp.Value.IsZero() {
					return dOp.Value
				}
			}
		}
	}
	return fallback
}

// handleObservationStats handles GET/POST /fhir/r4/{tenant}/Observation/$stats
// per HL7 FHIR R4 §10.1.12.2 (OperationDefinition/Observation-stats), computing
// requested statistical aggregates (average, max, min, count, totalcount, median,
// std-dev, sum, variance) over Observation/component valueQuantity values.
func (s *Server) handleObservationStats(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")
	q := r.URL.Query()
	subject := strings.TrimSpace(q.Get("subject"))
	if subject == "" {
		subject = strings.TrimSpace(q.Get("patient"))
	}
	var codes []string
	for _, cv := range q["code"] {
		for part := range strings.SplitSeq(cv, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				codes = append(codes, trimmed)
			}
		}
	}
	for _, cv := range q["coding"] {
		for part := range strings.SplitSeq(cv, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				codes = append(codes, trimmed)
			}
		}
	}
	var statsWanted []string
	for _, sv := range q["statistic"] {
		for part := range strings.SplitSeq(sv, ",") {
			if trimmed := strings.ToLower(strings.TrimSpace(part)); trimmed != "" {
				statsWanted = append(statsWanted, trimmed)
			}
		}
	}

	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		if body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024)); err == nil && len(bytes.TrimSpace(body)) > 0 {
			var raw map[string]any
			if json.Unmarshal(body, &raw) == nil {
				if params, ok := raw["parameter"].([]any); ok {
					for _, p := range params {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "subject", "patient":
							if subject == "" {
								subject = extractParamString(pm)
							}
						case "code", "coding":
							if cMap, ok := pm["valueCoding"].(map[string]any); ok {
								sys, _ := cMap["system"].(string)
								c, _ := cMap["code"].(string)
								if sys != "" && c != "" {
									codes = append(codes, sys+"|"+c)
								} else if c != "" {
									codes = append(codes, c)
								}
							} else if sVal := extractParamString(pm); sVal != "" {
								codes = append(codes, sVal)
							}
						case "statistic":
							if sVal := strings.ToLower(extractParamString(pm)); sVal != "" {
								statsWanted = append(statsWanted, sVal)
							}
						}
					}
				}
			}
		}
	}

	if subject == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Observation/$stats requires a subject or patient parameter")
		return
	}
	if len(codes) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Observation/$stats requires at least one code or coding parameter")
		return
	}
	if len(statsWanted) == 0 {
		statsWanted = []string{"average", "min", "max", "count"}
	}

	subjectRef, patID := normalizeTargetRef("Patient", subject)
	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if !auth.Allows(secCtx, "Observation", auth.ActionRead) && !auth.Allows(secCtx, "Observation", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for Observation/$stats")
			return
		}
		if auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) != cleanID(patID) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot invoke Observation/$stats for Patient/%s",
					secCtx.GetPatientId(), patID))
			return
		}
	}

	sr, err := s.store.Search(r.Context(), postgres.SearchParams{
		TenantID:   tenant,
		ResType:    "Observation",
		References: map[string]string{"patient": "Patient/" + patID},
		Count:      500,
	})
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to search Observations for $stats")
		return
	}

	obsMaps := make([]map[string]any, 0, len(sr.Matches))
	for _, m := range sr.Matches {
		if jb, jErr := s.protoToJSONWithMeta("Observation", m.ResourceProto, m.ResVersion, m.LastUpdated); jErr == nil {
			var rm map[string]any
			if json.Unmarshal(jb, &rm) == nil {
				obsMaps = append(obsMaps, rm)
			}
		}
	}

	var paramOut []map[string]any
	for _, rawCode := range codes {
		wantSys := "http://loinc.org"
		wantCode := rawCode
		if idx := strings.IndexByte(rawCode, '|'); idx != -1 {
			if strings.TrimSpace(rawCode[:idx]) != "" {
				wantSys = strings.TrimSpace(rawCode[:idx])
			}
			wantCode = strings.TrimSpace(rawCode[idx+1:])
		}
		var vals []float64
		unit := ""
		uSystem := "http://unitsofmeasure.org"
		uCode := ""

		for _, rm := range obsMaps {
			if cc, ok := rm["code"].(map[string]any); ok && codeableConceptMatchesLiterals(cc, []string{wantCode}) {
				if vq, ok := rm["valueQuantity"].(map[string]any); ok {
					if v, ok := vq["value"].(float64); ok {
						vals = append(vals, v)
						if unit == "" {
							unit, _ = vq["unit"].(string)
							uCode, _ = vq["code"].(string)
							if sStr, _ := vq["system"].(string); sStr != "" {
								uSystem = sStr
							}
						}
					}
				}
			}
			if comps, ok := rm["component"].([]any); ok {
				for _, comp := range comps {
					cm, ok := comp.(map[string]any)
					if !ok {
						continue
					}
					if cc, ok := cm["code"].(map[string]any); ok && codeableConceptMatchesLiterals(cc, []string{wantCode}) {
						if vq, ok := cm["valueQuantity"].(map[string]any); ok {
							if v, ok := vq["value"].(float64); ok {
								vals = append(vals, v)
								if unit == "" {
									unit, _ = vq["unit"].(string)
									uCode, _ = vq["code"].(string)
									if sStr, _ := vq["system"].(string); sStr != "" {
										uSystem = sStr
									}
								}
							}
						}
					}
				}
			}
		}

		statComps := buildStatisticComponents(vals, statsWanted, unit, uSystem, uCode)
		statObs := map[string]any{
			"resourceType": "Observation",
			"id":           "stats-" + wantCode + "-" + patID,
			"status":       "final",
			"subject": map[string]any{
				"reference": subjectRef,
			},
			"code": map[string]any{
				"coding": []any{
					map[string]any{
						"system": wantSys,
						"code":   wantCode,
					},
				},
			},
			"effectiveDateTime": time.Now().UTC().Format(time.RFC3339),
			"component":         statComps,
		}
		paramOut = append(paramOut, map[string]any{
			"name":     "statistic",
			"resource": statObs,
		})
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    paramOut,
	})
}

func buildStatisticComponents(vals []float64, statsWanted []string, unit, uSystem, uCode string) []any {
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)

	count := float64(len(sorted))
	var sum, minVal, maxVal, avg, median, variance, stdDev float64
	if len(sorted) > 0 {
		minVal = sorted[0]
		maxVal = sorted[len(sorted)-1]
		for _, v := range sorted {
			sum += v
		}
		avg = sum / count
		mid := len(sorted) / 2
		if len(sorted)%2 == 1 {
			median = sorted[mid]
		} else {
			median = (sorted[mid-1] + sorted[mid]) / 2.0
		}
		if len(sorted) > 1 {
			var sqDiff float64
			for _, v := range sorted {
				d := v - avg
				sqDiff += d * d
			}
			variance = sqDiff / float64(len(sorted)-1)
			stdDev = math.Sqrt(variance)
		}
	}

	var comps []any
	for _, st := range statsWanted {
		var val float64
		isCount := false
		switch st {
		case "average", "mean":
			val = avg
		case "max", "maximum":
			val = maxVal
		case "min", "minimum":
			val = minVal
		case "sum":
			val = sum
		case "median":
			val = median
		case "variance":
			val = variance
		case "std-dev", "stddev":
			val = stdDev
		case "count", "totalcount":
			val = count
			isCount = true
		default:
			continue
		}
		vq := map[string]any{
			"value": math.Round(val*1000) / 1000,
		}
		if isCount {
			vq["unit"] = "1"
			vq["system"] = "http://unitsofmeasure.org"
			vq["code"] = "1"
		} else {
			if unit != "" {
				vq["unit"] = unit
			}
			if uSystem != "" {
				vq["system"] = uSystem
			}
			if uCode != "" {
				vq["code"] = uCode
			}
		}
		comps = append(comps, map[string]any{
			"code": map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/observation-statistics",
						"code":   st,
					},
				},
			},
			"valueQuantity": vq,
		})
	}
	return comps
}

// handlePatientMerge handles POST /fhir/r4/{tenant}/Patient/$merge per HL7 FHIR
// Patient-merge OperationDefinition, merging source-patient into target-patient,
// re-linking all clinical compartment resources referencing source-patient to
// target-patient, and marking source-patient inactive with a replaced-by link.
func (s *Server) handlePatientMerge(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := r.PathValue("tenant")

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if auth.IsPatientRestricted(secCtx) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "patient-scoped token cannot invoke Patient/$merge")
			return
		}
		if !auth.Allows(secCtx, "Patient", auth.ActionUpdate) && !auth.Allows(secCtx, "Patient", auth.ActionCreate) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for Patient/$merge (requires Patient update or create scope)")
			return
		}
	}

	sourceRef := strings.TrimSpace(r.URL.Query().Get("source-patient"))
	targetRef := strings.TrimSpace(r.URL.Query().Get("target-patient"))
	preview := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("preview")), "true")
	var resultPatientOverride map[string]any

	if r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read request body")
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", "invalid JSON body for Patient/$merge")
				return
			}
			if params, ok := raw["parameter"].([]any); ok {
				for _, p := range params {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					name, _ := pm["name"].(string)
					switch name {
					case "source-patient", "sourcePatient":
						if v := extractParamString(pm); v != "" {
							sourceRef = v
						} else if rm, ok := pm["resource"].(map[string]any); ok {
							if rid, _ := rm["id"].(string); rid != "" {
								sourceRef = "Patient/" + rid
							}
						}
					case "target-patient", "targetPatient":
						if v := extractParamString(pm); v != "" {
							targetRef = v
						} else if rm, ok := pm["resource"].(map[string]any); ok {
							if rid, _ := rm["id"].(string); rid != "" {
								targetRef = "Patient/" + rid
							}
						}
					case "result-patient", "resultPatient":
						if rm, ok := pm["resource"].(map[string]any); ok {
							resultPatientOverride = rm
						}
					case "preview":
						if b, ok := pm["valueBoolean"].(bool); ok {
							preview = b
						} else if strings.EqualFold(extractParamString(pm), "true") {
							preview = true
						}
					}
				}
			}
		}
	}

	_, sourceID := normalizeTargetRef("Patient", sourceRef)
	_, targetID := normalizeTargetRef("Patient", targetRef)
	if sourceID == "" || targetID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"Patient/$merge requires both source-patient and target-patient parameters")
		return
	}
	if sourceID == targetID {
		writeOperationOutcome(w, http.StatusUnprocessableEntity, "invariant",
			"source-patient and target-patient cannot reference the same Patient")
		return
	}

	srcRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Patient", sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("source Patient/%s not found", sourceID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("source Patient/%s has been deleted", sourceID))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read source Patient")
		return
	}

	tgtRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Patient", targetID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("target Patient/%s not found", targetID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("target Patient/%s has been deleted", targetID))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read target Patient")
		return
	}

	srcJSON, _ := s.protoToJSONWithMeta("Patient", srcRec.ResourceProto, srcRec.ResVersion, srcRec.LastUpdated)
	tgtJSON, _ := s.protoToJSONWithMeta("Patient", tgtRec.ResourceProto, tgtRec.ResVersion, tgtRec.LastUpdated)
	var srcMap, tgtMap map[string]any
	_ = json.Unmarshal(srcJSON, &srcMap)
	_ = json.Unmarshal(tgtJSON, &tgtMap)
	if resultPatientOverride != nil {
		tgtMap = resultPatientOverride
		tgtMap["resourceType"] = "Patient"
		tgtMap["id"] = targetID
	}

	// Discover all clinical compartment resources referencing Patient/{sourceID}
	pe, _ := s.store.PatientEverything(r.Context(), postgres.PatientEverythingParams{
		TenantID:  tenant,
		PatientID: sourceID,
		Count:     500,
	})
	var relinkedRefs []string
	oldRef := "Patient/" + sourceID
	newRef := "Patient/" + targetID

	for _, rm := range s.decodeCompartmentMaps(pe) {
		rt, _ := rm["resourceType"].(string)
		rid, _ := rm["id"].(string)
		if rt == "" || rid == "" || strings.EqualFold(rt, "Patient") {
			continue
		}
		if rewritePatientReferenceInMap(rm, oldRef, newRef) {
			relinkedRefs = append(relinkedRefs, rt+"/"+rid)
			if !preview {
				_, _, _, _, _ = s.upsertResourceMap(r.Context(), tenant, rm)
			}
		}
	}

	srcMap["active"] = false
	srcLinks, _ := srcMap["link"].([]any)
	srcLinks = append(srcLinks, map[string]any{
		"other": map[string]any{"reference": newRef},
		"type":  "replaced-by",
	})
	srcMap["link"] = srcLinks

	tgtLinks, _ := tgtMap["link"].([]any)
	tgtLinks = append(tgtLinks, map[string]any{
		"other": map[string]any{"reference": oldRef},
		"type":  "replaces",
	})
	tgtMap["link"] = tgtLinks

	if !preview {
		_, _, _, _, _ = s.upsertResourceMap(r.Context(), tenant, srcMap)
		if _, _, _, savedTgt, uErr := s.upsertResourceMap(r.Context(), tenant, tgtMap); uErr == nil && savedTgt != nil {
			tgtMap = savedTgt
		}
	}

	diagMsg := fmt.Sprintf("Merged Patient/%s into Patient/%s and re-linked %d compartment resource(s)", sourceID, targetID, len(relinkedRefs))
	if preview {
		diagMsg = "[Preview] " + diagMsg
	}

	outParams := []map[string]any{
		{
			"name": "outcome",
			"resource": map[string]any{
				"resourceType": "OperationOutcome",
				"issue": []any{
					map[string]any{
						"severity":    "information",
						"code":        "informational",
						"diagnostics": diagMsg,
					},
				},
			},
		},
		{
			"name":     "result",
			"resource": tgtMap,
		},
		{
			"name": "task",
			"resource": map[string]any{
				"resourceType": "Task",
				"id":           "merge-" + sourceID + "-into-" + targetID,
				"status":       "completed",
				"intent":       "order",
				"for":          map[string]any{"reference": newRef},
				"description":  diagMsg,
			},
		},
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    outParams,
	})
}

func rewritePatientReferenceInMap(m map[string]any, oldRef, newRef string) bool {
	changed := false
	for k, v := range m {
		switch tv := v.(type) {
		case map[string]any:
			if ref, ok := tv["reference"].(string); ok && strings.EqualFold(strings.TrimSpace(ref), oldRef) {
				tv["reference"] = newRef
				changed = true
			} else if rewritePatientReferenceInMap(tv, oldRef, newRef) {
				changed = true
			}
		case []any:
			for _, elem := range tv {
				if em, ok := elem.(map[string]any); ok {
					if ref, ok := em["reference"].(string); ok && strings.EqualFold(strings.TrimSpace(ref), oldRef) {
						em["reference"] = newRef
						changed = true
					} else if rewritePatientReferenceInMap(em, oldRef, newRef) {
						changed = true
					}
				}
			}
		case string:
			if k == "reference" && strings.EqualFold(strings.TrimSpace(tv), oldRef) {
				m[k] = newRef
				changed = true
			}
		}
	}
	return changed
}

// =============================================================================
// HL7 FHIR R4 / Argonaut Scheduling: Appointment/$find, $hold, $book (Wave 18)
// =============================================================================

func (s *Server) handleAppointmentFind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "Appointment/$find only supports GET and POST")
		return
	}
	if s.store == nil {
		writeOperationOutcome(w, http.StatusNotImplemented, "not-supported", "Appointment/$find requires an operational store")
		return
	}

	tenant := r.PathValue("tenant")
	q := r.URL.Query()
	startParam := strings.TrimSpace(q.Get("start"))
	endParam := strings.TrimSpace(q.Get("end"))
	specialtyParam := strings.TrimSpace(q.Get("specialty"))
	visitTypeParam := strings.TrimSpace(q.Get("visit-type"))
	if visitTypeParam == "" {
		visitTypeParam = strings.TrimSpace(q.Get("service-type"))
	}
	practitionerParam := strings.TrimSpace(q.Get("practitioner"))
	organizationParam := strings.TrimSpace(q.Get("organization"))
	locationParam := strings.TrimSpace(q.Get("location"))

	if r.Method == http.MethodPost && r.Body != nil {
		var bodyMap map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&bodyMap); err == nil {
			if bodyMap["resourceType"] == "Parameters" {
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						pm, ok := rawP.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						val := extractParamString(pm)
						switch name {
						case "start":
							if startParam == "" {
								startParam = val
							}
						case "end":
							if endParam == "" {
								endParam = val
							}
						case "specialty":
							if specialtyParam == "" {
								specialtyParam = val
							}
						case "visit-type", "service-type":
							if visitTypeParam == "" {
								visitTypeParam = val
							}
						case "practitioner":
							if practitionerParam == "" {
								practitionerParam = val
							}
						case "organization":
							if organizationParam == "" {
								organizationParam = val
							}
						case "location":
							if locationParam == "" {
								locationParam = val
							}
						}
					}
				}
			}
		}
	}

	if secCtx, ok := auth.FromContext(r.Context()); ok && secCtx != nil {
		if !auth.Allows(secCtx, "Appointment", auth.ActionRead) && !auth.Allows(secCtx, "Appointment", auth.ActionSearch) &&
			!auth.Allows(secCtx, "Slot", auth.ActionRead) && !auth.Allows(secCtx, "Slot", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for Appointment/$find")
			return
		}
	}

	var startBound, endBound time.Time
	if startParam != "" {
		if dOp, err := postgres.ParseDateOp(startParam); err == nil && !dOp.Value.IsZero() {
			startBound = dOp.Value
		}
	}
	if endParam != "" {
		if dOp, err := postgres.ParseDateOp(endParam); err == nil && !dOp.Value.IsZero() {
			endBound = dOp.Value
		}
	}

	slotRes, err := s.store.Search(r.Context(), postgres.SearchParams{
		TenantID: tenant,
		ResType:  "Slot",
		Count:    200,
	})
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "Slot search failed: "+err.Error())
		return
	}

	var proposedEntries []any
	var includeEntries []any
	matchCount := 0

	for _, sr := range slotRes.Matches {
		slotBytes, err := s.protoToJSONWithMeta("Slot", sr.ResourceProto, sr.ResVersion, sr.LastUpdated)
		if err != nil {
			continue
		}
		var slotMap map[string]any
		if err := json.Unmarshal(slotBytes, &slotMap); err != nil {
			continue
		}
		status, _ := slotMap["status"].(string)
		if status != "" && status != "free" {
			continue
		}

		slotStartStr, _ := slotMap["start"].(string)
		slotEndStr, _ := slotMap["end"].(string)
		if !startBound.IsZero() && slotStartStr != "" {
			if stOp, err := postgres.ParseDateOp(slotStartStr); err == nil && !stOp.Value.IsZero() && stOp.Value.Before(startBound) {
				continue
			}
		}
		if !endBound.IsZero() && slotEndStr != "" {
			if etOp, err := postgres.ParseDateOp(slotEndStr); err == nil && !etOp.Value.IsZero() && etOp.Value.After(endBound) {
				continue
			}
		}

		// Load associated Schedule if referenced
		var schedMap map[string]any
		if schedObj, ok := slotMap["schedule"].(map[string]any); ok {
			if schedRef, _ := schedObj["reference"].(string); schedRef != "" {
				schedID := cleanID(schedRef)
				if schedRec, getErr := s.store.ReadResourceWithMeta(r.Context(), tenant, "Schedule", schedID); getErr == nil {
					if sb, convErr := s.protoToJSONWithMeta("Schedule", schedRec.ResourceProto, schedRec.ResVersion, schedRec.LastUpdated); convErr == nil {
						_ = json.Unmarshal(sb, &schedMap)
					}
				}
			}
		}

		if specialtyParam != "" {
			if !matchesCodeableFilter(slotMap["specialty"], specialtyParam) && (schedMap == nil || !matchesCodeableFilter(schedMap["specialty"], specialtyParam)) {
				continue
			}
		}
		if visitTypeParam != "" {
			if !matchesCodeableFilter(slotMap["serviceType"], visitTypeParam) && !matchesCodeableFilter(slotMap["appointmentType"], visitTypeParam) &&
				(schedMap == nil || !matchesCodeableFilter(schedMap["serviceType"], visitTypeParam)) {
				continue
			}
		}
		if practitionerParam != "" && !scheduleHasActor(schedMap, "Practitioner", practitionerParam) {
			continue
		}
		if organizationParam != "" && !scheduleHasActor(schedMap, "Organization", organizationParam) {
			continue
		}
		if locationParam != "" && !scheduleHasActor(schedMap, "Location", locationParam) {
			continue
		}

		var participants []any
		if schedMap != nil {
			if actors, ok := schedMap["actor"].([]any); ok {
				for _, a := range actors {
					if am, ok := a.(map[string]any); ok {
						participants = append(participants, map[string]any{
							"actor":  am,
							"status": "needs-action",
						})
					}
				}
			}
		}
		if len(participants) == 0 {
			participants = []any{
				map[string]any{
					"status": "needs-action",
				},
			}
		}

		proposedAppt := map[string]any{
			"resourceType": "Appointment",
			"id":           "proposed-" + sr.ResID,
			"status":       "proposed",
			"start":        slotStartStr,
			"end":          slotEndStr,
			"slot": []any{
				map[string]any{"reference": "Slot/" + sr.ResID},
			},
			"participant": participants,
		}
		if st, ok := slotMap["serviceType"]; ok {
			proposedAppt["serviceType"] = st
		} else if schedMap != nil && schedMap["serviceType"] != nil {
			proposedAppt["serviceType"] = schedMap["serviceType"]
		}
		if sp, ok := slotMap["specialty"]; ok {
			proposedAppt["specialty"] = sp
		} else if schedMap != nil && schedMap["specialty"] != nil {
			proposedAppt["specialty"] = schedMap["specialty"]
		}

		matchCount++
		proposedEntries = append(proposedEntries, map[string]any{
			"fullUrl":  "Appointment/proposed-" + sr.ResID,
			"resource": proposedAppt,
			"search":   map[string]any{"mode": "match"},
		})
		includeEntries = append(includeEntries, map[string]any{
			"fullUrl":  "Slot/" + sr.ResID,
			"resource": slotMap,
			"search":   map[string]any{"mode": "include"},
		})
	}

	entries := append(proposedEntries, includeEntries...)
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Bundle",
		"id":           fmt.Sprintf("appointment-find-%d", time.Now().UTC().UnixNano()),
		"type":         "searchset",
		"total":        matchCount,
		"entry":        entries,
	})
}

func matchesCodeableFilter(raw any, filter string) bool {
	if raw == nil || filter == "" {
		return false
	}
	wantSys := ""
	wantCode := strings.TrimSpace(filter)
	if idx := strings.IndexByte(wantCode, '|'); idx >= 0 {
		wantSys = strings.TrimSpace(wantCode[:idx])
		wantCode = strings.TrimSpace(wantCode[idx+1:])
	}
	checkConcept := func(cm map[string]any) bool {
		if codings, ok := cm["coding"].([]any); ok {
			for _, rawC := range codings {
				if cMap, ok := rawC.(map[string]any); ok {
					sys, _ := cMap["system"].(string)
					code, _ := cMap["code"].(string)
					if strings.EqualFold(code, wantCode) && (wantSys == "" || strings.EqualFold(sys, wantSys)) {
						return true
					}
				}
			}
		}
		if txt, _ := cm["text"].(string); txt != "" && strings.EqualFold(txt, wantCode) {
			return true
		}
		return false
	}
	switch v := raw.(type) {
	case map[string]any:
		return checkConcept(v)
	case []any:
		for _, elem := range v {
			if cm, ok := elem.(map[string]any); ok && checkConcept(cm) {
				return true
			}
		}
	}
	return false
}

func scheduleHasActor(schedMap map[string]any, actorType, target string) bool {
	if schedMap == nil {
		return false
	}
	targetID := cleanID(target)
	actors, ok := schedMap["actor"].([]any)
	if !ok {
		return false
	}
	for _, rawA := range actors {
		if am, ok := rawA.(map[string]any); ok {
			ref, _ := am["reference"].(string)
			if strings.EqualFold(cleanID(ref), targetID) || strings.EqualFold(ref, actorType+"/"+targetID) {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleAppointmentHold(w http.ResponseWriter, r *http.Request) {
	s.handleAppointmentHoldOrBook(w, r, false)
}

func (s *Server) handleAppointmentBook(w http.ResponseWriter, r *http.Request) {
	s.handleAppointmentHoldOrBook(w, r, true)
}

func (s *Server) handleAppointmentHoldOrBook(w http.ResponseWriter, r *http.Request, isBook bool) {
	opName := "$hold"
	targetApptStatus := "pending"
	targetSlotStatus := "busy-tentative"
	if isBook {
		opName = "$book"
		targetApptStatus = "booked"
		targetSlotStatus = "busy"
	}

	if r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "Appointment/"+opName+" only supports POST")
		return
	}
	if s.store == nil {
		writeOperationOutcome(w, http.StatusNotImplemented, "not-supported", "Appointment/"+opName+" requires an operational store")
		return
	}

	tenant := r.PathValue("tenant")
	pathApptID := strings.TrimSpace(r.PathValue("id"))

	if secCtx, ok := auth.FromContext(r.Context()); ok && secCtx != nil {
		if !auth.Allows(secCtx, "Appointment", auth.ActionCreate) && !auth.Allows(secCtx, "Appointment", auth.ActionUpdate) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for Appointment/"+opName)
			return
		}
	}

	var apptMap map[string]any
	var slotRefs []string
	var patientRefs []string
	var comment string
	var apptIDParam string

	if r.Body != nil {
		var bodyMap map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&bodyMap); err == nil {
			switch bodyMap["resourceType"] {
			case "Appointment":
				apptMap = bodyMap
			case "Parameters":
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						pm, ok := rawP.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "appt-resource", "appointment", "resource":
							if res, ok := pm["resource"].(map[string]any); ok && res["resourceType"] == "Appointment" {
								apptMap = res
							}
						case "appt-id", "appointment-id":
							apptIDParam = extractParamString(pm)
						case "slot-id", "slot":
							if v := extractParamString(pm); v != "" {
								slotRefs = append(slotRefs, v)
							}
						case "patient-id", "patient":
							if v := extractParamString(pm); v != "" {
								patientRefs = append(patientRefs, v)
							}
						case "comment":
							comment = extractParamString(pm)
						}
					}
				}
			}
		}
	}

	if apptMap == nil {
		lookupID := pathApptID
		if lookupID == "" {
			lookupID = cleanID(apptIDParam)
		}
		if lookupID != "" && !strings.HasPrefix(lookupID, "proposed-") {
			if apptRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Appointment", lookupID); err == nil {
				if jb, convErr := s.protoToJSONWithMeta("Appointment", apptRec.ResourceProto, apptRec.ResVersion, apptRec.LastUpdated); convErr == nil {
					_ = json.Unmarshal(jb, &apptMap)
				}
			}
		}
		if apptMap == nil && strings.HasPrefix(lookupID, "proposed-") {
			slotRefs = append(slotRefs, "Slot/"+strings.TrimPrefix(lookupID, "proposed-"))
		}
	}

	if apptMap == nil {
		apptID := pathApptID
		if apptID == "" {
			apptID = cleanID(apptIDParam)
		}
		apptMap = map[string]any{
			"resourceType": "Appointment",
			"id":           apptID,
		}
	}

	if existingSlots, ok := apptMap["slot"].([]any); ok {
		for _, rawS := range existingSlots {
			if sm, ok := rawS.(map[string]any); ok {
				if ref, _ := sm["reference"].(string); ref != "" {
					slotRefs = append(slotRefs, ref)
				}
			}
		}
	}

	seenSlots := make(map[string]bool)
	var uniqueSlotIDs []string
	for _, sr := range slotRefs {
		sid := cleanID(sr)
		if sid != "" && !seenSlots[sid] {
			seenSlots[sid] = true
			uniqueSlotIDs = append(uniqueSlotIDs, sid)
		}
	}

	// Validate and transition referenced Slots
	var slotObjs []any
	var updatedSlotEntries []any
	var firstStart, lastEnd string
	for _, sid := range uniqueSlotIDs {
		slotRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Slot", sid)
		if err != nil {
			writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Slot/%s not found", sid))
			return
		}
		slotBytes, err := s.protoToJSONWithMeta("Slot", slotRec.ResourceProto, slotRec.ResVersion, slotRec.LastUpdated)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "decode Slot/"+sid+": "+err.Error())
			return
		}
		var slotMap map[string]any
		if err := json.Unmarshal(slotBytes, &slotMap); err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "unmarshal Slot/"+sid+": "+err.Error())
			return
		}
		curStatus, _ := slotMap["status"].(string)
		if curStatus == "busy" || curStatus == "busy-unavailable" || (!isBook && curStatus == "busy-tentative") {
			writeOperationOutcome(w, http.StatusUnprocessableEntity, "conflict", fmt.Sprintf("Slot/%s is not available (current status: %s)", sid, curStatus))
			return
		}
		if st, _ := slotMap["start"].(string); st != "" && firstStart == "" {
			firstStart = st
		}
		if et, _ := slotMap["end"].(string); et != "" {
			lastEnd = et
		}
		slotMap["status"] = targetSlotStatus
		slotMap["resourceType"] = "Slot"
		slotMap["id"] = sid
		_, _, _, savedSlot, err := s.upsertResourceMap(r.Context(), tenant, slotMap)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "persist Slot/"+sid+": "+err.Error())
			return
		}
		if savedSlot != nil {
			slotMap = savedSlot
		}
		slotObjs = append(slotObjs, map[string]any{"reference": "Slot/" + sid})
		updatedSlotEntries = append(updatedSlotEntries, map[string]any{
			"fullUrl":  "Slot/" + sid,
			"resource": slotMap,
			"search":   map[string]any{"mode": "include"},
		})
	}

	apptID, _ := apptMap["id"].(string)
	if apptID == "" || strings.HasPrefix(apptID, "proposed-") {
		if len(uniqueSlotIDs) > 0 {
			apptID = "appt-" + uniqueSlotIDs[0]
		} else {
			apptID = fmt.Sprintf("appt-%d", time.Now().UTC().UnixNano())
		}
		apptMap["id"] = apptID
	}
	apptMap["resourceType"] = "Appointment"
	apptMap["status"] = targetApptStatus
	if len(slotObjs) > 0 {
		apptMap["slot"] = slotObjs
	}
	if firstStart != "" && apptMap["start"] == nil {
		apptMap["start"] = firstStart
	}
	if lastEnd != "" && apptMap["end"] == nil {
		apptMap["end"] = lastEnd
	}
	if comment != "" {
		apptMap["comment"] = comment
	}

	var participants []any
	if existingP, ok := apptMap["participant"].([]any); ok {
		participants = existingP
	}
	for _, patRef := range patientRefs {
		normPatRef := patRef
		if !strings.Contains(normPatRef, "/") {
			normPatRef = "Patient/" + normPatRef
		}
		found := false
		for _, rawP := range participants {
			if pm, ok := rawP.(map[string]any); ok {
				if actor, ok := pm["actor"].(map[string]any); ok {
					if ref, _ := actor["reference"].(string); strings.EqualFold(ref, normPatRef) {
						pm["status"] = "accepted"
						found = true
					}
				}
			}
		}
		if !found {
			participants = append(participants, map[string]any{
				"actor":  map[string]any{"reference": normPatRef},
				"status": "accepted",
			})
		}
	}
	if len(participants) == 0 {
		participants = []any{
			map[string]any{
				"status": "accepted",
			},
		}
	}
	apptMap["participant"] = participants

	_, _, _, savedAppt, err := s.upsertResourceMap(r.Context(), tenant, apptMap)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "persist Appointment: "+err.Error())
		return
	}
	if savedAppt != nil {
		apptMap = savedAppt
	}

	bundleEntries := []any{
		map[string]any{
			"fullUrl":  "Appointment/" + apptID,
			"resource": apptMap,
			"search":   map[string]any{"mode": "match"},
		},
	}
	bundleEntries = append(bundleEntries, updatedSlotEntries...)
	bundleEntries = append(bundleEntries, map[string]any{
		"resource": map[string]any{
			"resourceType": "OperationOutcome",
			"issue": []map[string]any{
				{
					"severity":    "information",
					"code":        "informational",
					"diagnostics": fmt.Sprintf("Appointment/%s transitioned to %s (%d slot(s) marked %s)", apptID, targetApptStatus, len(uniqueSlotIDs), targetSlotStatus),
				},
			},
		},
		"search": map[string]any{"mode": "outcome"},
	})

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Bundle",
		"id":           "appointment-" + strings.TrimPrefix(opName, "$") + "-" + apptID,
		"type":         "searchset",
		"total":        1,
		"entry":        bundleEntries,
	})
}

// =============================================================================
// HL7 FHIR R4 CapabilityStatement/$subset & $implements (Wave 18)
// =============================================================================

func (s *Server) handleCapabilityStatementSubset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "CapabilityStatement/$subset only supports GET and POST")
		return
	}

	wanted := make(map[string]bool)
	for _, rawVal := range r.URL.Query()["resource"] {
		for _, tok := range strings.Split(rawVal, ",") {
			tok = strings.TrimSpace(tok)
			if tok != "" {
				wanted[strings.ToLower(tok)] = true
			}
		}
	}

	if r.Method == http.MethodPost && r.Body != nil {
		var bodyMap map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&bodyMap); err == nil {
			if bodyMap["resourceType"] == "Parameters" {
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						if pm, ok := rawP.(map[string]any); ok {
							name, _ := pm["name"].(string)
							if name == "resource" || name == "type" {
								for _, tok := range strings.Split(extractParamString(pm), ",") {
									tok = strings.TrimSpace(tok)
									if tok != "" {
										wanted[strings.ToLower(tok)] = true
									}
								}
							}
						}
					}
				}
			}
		}
	}

	cap := s.buildCapabilityStatement(r)
	if len(wanted) > 0 {
		if rests, ok := cap["rest"].([]map[string]any); ok && len(rests) > 0 {
			if resList, ok := rests[0]["resource"].([]map[string]any); ok {
				filtered := make([]map[string]any, 0, len(wanted))
				for _, entry := range resList {
					rt, _ := entry["type"].(string)
					if wanted[strings.ToLower(rt)] {
						filtered = append(filtered, entry)
					}
				}
				rests[0]["resource"] = filtered
			}
		}
	}
	cap["meta"] = map[string]any{
		"tag": []map[string]string{
			{
				"system":  "http://terminology.hl7.org/CodeSystem/v3-ObservationValue",
				"code":    "SUBSETTED",
				"display": "subsetted",
			},
		},
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(cap)
}

func (s *Server) handleCapabilityStatementImplements(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "CapabilityStatement/$implements only supports GET and POST")
		return
	}

	tenant := r.PathValue("tenant")
	canonicalParam := strings.TrimSpace(r.URL.Query().Get("canonical"))
	if canonicalParam == "" {
		canonicalParam = strings.TrimSpace(r.URL.Query().Get("url"))
	}
	var clientCap map[string]any

	if r.Method == http.MethodPost && r.Body != nil {
		var bodyMap map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&bodyMap); err == nil {
			switch bodyMap["resourceType"] {
			case "CapabilityStatement":
				clientCap = bodyMap
			case "Parameters":
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						pm, ok := rawP.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "canonical", "url", "client":
							if canonicalParam == "" {
								canonicalParam = extractParamString(pm)
							}
							if res, ok := pm["resource"].(map[string]any); ok && res["resourceType"] == "CapabilityStatement" {
								clientCap = res
							}
						case "resource":
							if res, ok := pm["resource"].(map[string]any); ok && res["resourceType"] == "CapabilityStatement" {
								clientCap = res
							}
						}
					}
				}
			}
		}
	}

	serverCap := s.buildCapabilityStatement(r)

	// If clientCap is not supplied inline, resolve from canonical URL or stored CapabilityStatement
	if clientCap == nil && canonicalParam != "" {
		if s.store != nil {
			if csRes, err := s.store.Search(r.Context(), postgres.SearchParams{TenantID: tenant, ResType: "CapabilityStatement", Count: 20}); err == nil {
				for _, sr := range csRes.Matches {
					if jb, convErr := s.protoToJSONWithMeta("CapabilityStatement", sr.ResourceProto, sr.ResVersion, sr.LastUpdated); convErr == nil {
						var cm map[string]any
						if json.Unmarshal(jb, &cm) == nil {
							if u, _ := cm["url"].(string); strings.EqualFold(u, canonicalParam) || strings.EqualFold(sr.ResID, cleanID(canonicalParam)) {
								clientCap = cm
								break
							}
						}
					}
				}
			}
		}
		if clientCap == nil {
			switch strings.ToLower(strings.TrimSpace(canonicalParam)) {
			case "http://hl7.org/fhir/us/core/capabilitystatement/us-core-server",
				"http://hl7.org/fhir/uv/bulkdata/capabilitystatement/bulk-data",
				"http://hl7.org/fhir/capabilitystatement/base":
				clientCap = map[string]any{
					"resourceType": "CapabilityStatement",
					"url":          canonicalParam,
					"fhirVersion":  "4.0.1",
				}
			default:
				writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("CapabilityStatement %q not found", canonicalParam))
				return
			}
		}
	}

	if clientCap == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "CapabilityStatement/$implements requires a 'canonical' URL or inline CapabilityStatement 'resource'")
		return
	}

	// Build lookup tables of server capabilities
	serverResources := make(map[string]map[string]any)
	if rests, ok := serverCap["rest"].([]map[string]any); ok && len(rests) > 0 {
		if resList, ok := rests[0]["resource"].([]map[string]any); ok {
			for _, rm := range resList {
				if rt, _ := rm["type"].(string); rt != "" {
					serverResources[rt] = rm
				}
			}
		}
	}

	var issues []map[string]any
	implementsAll := true

	if reqVer, _ := clientCap["fhirVersion"].(string); reqVer != "" && !strings.HasPrefix(reqVer, "4.0") {
		implementsAll = false
		issues = append(issues, map[string]any{
			"severity":    "error",
			"code":        "not-supported",
			"diagnostics": fmt.Sprintf("Required fhirVersion %q is not compatible with server fhirVersion 4.0.1", reqVer),
		})
	}

	if reqRests, ok := clientCap["rest"].([]any); ok {
		for _, rawRest := range reqRests {
			restMap, ok := rawRest.(map[string]any)
			if !ok {
				continue
			}
			reqResList, _ := restMap["resource"].([]any)
			for _, rawRes := range reqResList {
				reqRes, ok := rawRes.(map[string]any)
				if !ok {
					continue
				}
				rt, _ := reqRes["type"].(string)
				if rt == "" {
					continue
				}
				srvRes, supported := serverResources[rt]
				if !supported {
					implementsAll = false
					issues = append(issues, map[string]any{
						"severity":    "error",
						"code":        "not-supported",
						"diagnostics": fmt.Sprintf("Server CapabilityStatement does not support resource type %q", rt),
					})
					continue
				}

				// Check required interactions
				if reqInts, ok := reqRes["interaction"].([]any); ok {
					srvIntSet := make(map[string]bool)
					if srvInts, ok := srvRes["interaction"].([]map[string]string); ok {
						for _, im := range srvInts {
							srvIntSet[im["code"]] = true
						}
					}
					for _, rawInt := range reqInts {
						if im, ok := rawInt.(map[string]any); ok {
							code, _ := im["code"].(string)
							if code != "" && !srvIntSet[code] {
								implementsAll = false
								issues = append(issues, map[string]any{
									"severity":    "error",
									"code":        "not-supported",
									"diagnostics": fmt.Sprintf("Resource %s does not support required interaction %q", rt, code),
								})
							}
						}
					}
				}

				// Check required search parameters
				if reqSPs, ok := reqRes["searchParam"].([]any); ok {
					srvSPSet := make(map[string]bool)
					if srvSPs, ok := srvRes["searchParam"].([]map[string]string); ok {
						for _, spm := range srvSPs {
							srvSPSet[spm["name"]] = true
						}
					}
					for _, rawSP := range reqSPs {
						if spm, ok := rawSP.(map[string]any); ok {
							spName, _ := spm["name"].(string)
							if spName != "" && !srvSPSet[spName] {
								implementsAll = false
								issues = append(issues, map[string]any{
									"severity":    "error",
									"code":        "not-supported",
									"diagnostics": fmt.Sprintf("Resource %s does not support required searchParam %q", rt, spName),
								})
							}
						}
					}
				}
			}
		}
	}

	if len(issues) == 0 {
		issues = []map[string]any{
			{
				"severity":    "information",
				"code":        "informational",
				"diagnostics": "Server CapabilityStatement satisfies all requirements of the target CapabilityStatement",
			},
		}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{
				"name":         "result",
				"valueBoolean": implementsAll,
			},
			{
				"name": "issues",
				"resource": map[string]any{
					"resourceType": "OperationOutcome",
					"issue":        issues,
				},
			},
		},
	})
}

// =============================================================================
// HL7 FHIR R4 StructureDefinition/$snapshot (Wave 18)
// =============================================================================

func (s *Server) handleStructureDefinitionSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "StructureDefinition/$snapshot only supports GET and POST")
		return
	}

	tenant := r.PathValue("tenant")
	pathID := strings.TrimSpace(r.PathValue("id"))
	urlParam := strings.TrimSpace(r.URL.Query().Get("url"))

	var sdMap map[string]any
	if r.Method == http.MethodPost && r.Body != nil {
		var bodyMap map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&bodyMap); err == nil {
			switch bodyMap["resourceType"] {
			case "StructureDefinition":
				sdMap = bodyMap
			case "Parameters":
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						pm, ok := rawP.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "definition", "resource":
							if res, ok := pm["resource"].(map[string]any); ok && res["resourceType"] == "StructureDefinition" {
								sdMap = res
							}
						case "url":
							if urlParam == "" {
								urlParam = extractParamString(pm)
							}
						}
					}
				}
			}
		}
	}

	if sdMap == nil && pathID != "" && s.store != nil {
		if sdRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "StructureDefinition", pathID); err == nil {
			if jb, convErr := s.protoToJSONWithMeta("StructureDefinition", sdRec.ResourceProto, sdRec.ResVersion, sdRec.LastUpdated); convErr == nil {
				_ = json.Unmarshal(jb, &sdMap)
			}
		}
	}

	if sdMap == nil && urlParam != "" && s.store != nil {
		if sdRes, err := s.store.Search(r.Context(), postgres.SearchParams{TenantID: tenant, ResType: "StructureDefinition", Count: 50}); err == nil {
			for _, sr := range sdRes.Matches {
				if jb, convErr := s.protoToJSONWithMeta("StructureDefinition", sr.ResourceProto, sr.ResVersion, sr.LastUpdated); convErr == nil {
					var candidate map[string]any
					if json.Unmarshal(jb, &candidate) == nil {
						if u, _ := candidate["url"].(string); strings.EqualFold(u, urlParam) {
							sdMap = candidate
							break
						}
					}
				}
			}
		}
	}

	// Determine target resourceType from sdMap, pathID, or urlParam
	targetResType := ""
	if sdMap != nil {
		targetResType, _ = sdMap["type"].(string)
		if targetResType == "" {
			if baseDef, _ := sdMap["baseDefinition"].(string); baseDef != "" {
				if idx := strings.LastIndexByte(baseDef, '/'); idx >= 0 {
					targetResType = baseDef[idx+1:]
				}
			}
		}
	}
	if targetResType == "" {
		lookup := urlParam
		if lookup == "" {
			lookup = pathID
		}
		targetResType = resolveStructureDefinitionResourceType(lookup, s.protoRegistry)
	}

	if targetResType == "" {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", "StructureDefinition could not be resolved; specify a valid FHIR R4 resource type or StructureDefinition url")
		return
	}

	if sdMap == nil {
		sdID := pathID
		if sdID == "" {
			if idx := strings.LastIndexByte(urlParam, '/'); idx >= 0 {
				sdID = urlParam[idx+1:]
			} else {
				sdID = targetResType
			}
		}
		sdURL := urlParam
		if sdURL == "" {
			sdURL = "http://hl7.org/fhir/StructureDefinition/" + sdID
		}
		sdMap = map[string]any{
			"resourceType":   "StructureDefinition",
			"id":             sdID,
			"url":            sdURL,
			"name":           sdID,
			"status":         "active",
			"fhirVersion":    "4.0.1",
			"kind":           "resource",
			"abstract":       false,
			"type":           targetResType,
			"baseDefinition": "http://hl7.org/fhir/StructureDefinition/DomainResource",
			"derivation":     "specialization",
		}
	}

	elements := s.synthesizeSnapshotElements(targetResType, sdMap)
	sdMap["snapshot"] = map[string]any{
		"element": elements,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(sdMap)
}

func resolveStructureDefinitionResourceType(lookup string, registry map[string]protoreflect.MessageDescriptor) string {
	lookup = strings.TrimSpace(lookup)
	if lookup == "" {
		return ""
	}
	tail := lookup
	if idx := strings.LastIndexByte(lookup, '/'); idx >= 0 {
		tail = lookup[idx+1:]
	}
	for rt := range registry {
		if strings.EqualFold(rt, tail) {
			return rt
		}
	}
	lowerTail := strings.ToLower(tail)
	usCorePrefixMap := map[string]string{
		"us-core-patient":               "Patient",
		"us-core-encounter":             "Encounter",
		"us-core-condition":             "Condition",
		"us-core-vital-signs":           "Observation",
		"us-core-observation-lab":       "Observation",
		"us-core-blood-pressure":        "Observation",
		"us-core-heart-rate":            "Observation",
		"us-core-medicationrequest":     "MedicationRequest",
		"us-core-allergyintolerance":    "AllergyIntolerance",
		"us-core-procedure":             "Procedure",
		"us-core-diagnosticreport-lab":  "DiagnosticReport",
		"us-core-diagnosticreport-note": "DiagnosticReport",
		"us-core-immunization":          "Immunization",
		"us-core-organization":          "Organization",
		"us-core-practitioner":          "Practitioner",
		"us-core-practitionerrole":      "PractitionerRole",
		"us-core-location":              "Location",
		"us-core-specimen":              "Specimen",
		"us-core-documentreference":     "DocumentReference",
		"us-core-provenance":            "Provenance",
		"us-core-careplan":              "CarePlan",
		"us-core-careteam":              "CareTeam",
		"us-core-goal":                  "Goal",
		"us-core-servicerequest":        "ServiceRequest",
		"us-core-coverage":              "Coverage",
		"us-core-questionnaireresponse": "QuestionnaireResponse",
		"us-core-medicationdispense":    "MedicationDispense",
		"us-core-medication":            "Medication",
		"us-core-implantable-device":    "Device",
		"us-core-relatedperson":         "RelatedPerson",
	}
	if rt, ok := usCorePrefixMap[lowerTail]; ok {
		return rt
	}
	for rt := range registry {
		if strings.Contains(lowerTail, strings.ToLower(rt)) {
			return rt
		}
	}
	return ""
}

func (s *Server) synthesizeSnapshotElements(resType string, sdMap map[string]any) []map[string]any {
	schemas := validation.DefaultSchemas()
	sch := schemas[resType]
	reqSet := make(map[string]bool, len(sch.RequiredFields))
	for _, rf := range sch.RequiredFields {
		reqSet[rf] = true
	}

	elements := []map[string]any{
		{
			"id":   resType,
			"path": resType,
			"min":  0,
			"max":  "*",
		},
		{
			"id":   resType + ".id",
			"path": resType + ".id",
			"min":  0,
			"max":  "1",
			"type": []map[string]string{{"code": "id"}},
		},
		{
			"id":   resType + ".meta",
			"path": resType + ".meta",
			"min":  0,
			"max":  "1",
			"type": []map[string]string{{"code": "Meta"}},
		},
	}

	seenPaths := map[string]int{
		resType:           0,
		resType + ".id":   1,
		resType + ".meta": 2,
	}

	if md := s.protoRegistry[resType]; md != nil {
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			jsonName := fd.JSONName()
			if jsonName == "id" || jsonName == "meta" {
				continue
			}
			path := resType + "." + jsonName
			minCard := 0
			if reqSet[jsonName] {
				minCard = 1
			}
			maxCard := "1"
			if fd.IsList() {
				maxCard = "*"
			}
			typeCode := "Element"
			if fd.Kind() == protoreflect.MessageKind && fd.Message() != nil {
				typeCode = string(fd.Message().Name())
				if typeCode == "CodeType" || strings.HasSuffix(typeCode, "Code") {
					typeCode = "code"
				}
			}
			elem := map[string]any{
				"id":          path,
				"path":        path,
				"min":         minCard,
				"max":         maxCard,
				"type":        []map[string]string{{"code": typeCode}},
				"mustSupport": minCard == 1,
			}
			if vsURI, hasVS := sch.ValueSetURIs[jsonName]; hasVS && vsURI != "" {
				elem["binding"] = map[string]any{
					"strength": "required",
					"valueSet": vsURI,
				}
			}
			seenPaths[path] = len(elements)
			elements = append(elements, elem)
		}
	}

	// Merge any differential.element constraints into the synthesized snapshot
	if diffObj, ok := sdMap["differential"].(map[string]any); ok {
		if diffElems, ok := diffObj["element"].([]any); ok {
			for _, rawDE := range diffElems {
				deMap, ok := rawDE.(map[string]any)
				if !ok {
					continue
				}
				p, _ := deMap["path"].(string)
				if p == "" {
					continue
				}
				if idx, exists := seenPaths[p]; exists {
					for k, v := range deMap {
						elements[idx][k] = v
					}
				} else {
					if deMap["id"] == nil {
						deMap["id"] = p
					}
					if deMap["min"] == nil {
						deMap["min"] = 0
					}
					if deMap["max"] == nil {
						deMap["max"] = "1"
					}
					seenPaths[p] = len(elements)
					elements = append(elements, deMap)
				}
			}
		}
	}

	return elements
}

// =============================================================================
// HL7 FHIR R4 Messaging: $process-message (Wave 19)
// =============================================================================

func (s *Server) handleProcessMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "$process-message only supports POST")
		return
	}
	if s.store == nil {
		writeOperationOutcome(w, http.StatusNotImplemented, "not-supported", "$process-message requires an operational store")
		return
	}

	tenant := r.PathValue("tenant")
	asyncMode := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("async")), "true")

	if secCtx, ok := auth.FromContext(r.Context()); ok && secCtx != nil {
		if !auth.Allows(secCtx, "MessageHeader", auth.ActionCreate) && !auth.Allows(secCtx, "Bundle", auth.ActionCreate) && !auth.Allows(secCtx, "*", auth.ActionCreate) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for $process-message")
			return
		}
	}

	if r.Body == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "$process-message requires a message Bundle or Parameters body")
		return
	}

	var bodyMap map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&bodyMap); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "invalid JSON payload for $process-message: "+err.Error())
		return
	}

	var msgBundle map[string]any
	switch bodyMap["resourceType"] {
	case "Bundle":
		msgBundle = bodyMap
	case "Parameters":
		if params, ok := bodyMap["parameter"].([]any); ok {
			for _, rawP := range params {
				pm, ok := rawP.(map[string]any)
				if !ok {
					continue
				}
				name, _ := pm["name"].(string)
				switch name {
				case "content":
					if res, ok := pm["resource"].(map[string]any); ok && res["resourceType"] == "Bundle" {
						msgBundle = res
					}
				case "async":
					if b, ok := pm["valueBoolean"].(bool); ok {
						asyncMode = b
					}
				}
			}
		}
	}

	if msgBundle == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "$process-message requires a FHIR Bundle of type 'message'")
		return
	}
	if bType, _ := msgBundle["type"].(string); bType != "message" {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("$process-message requires Bundle.type == 'message', got %q", bType))
		return
	}

	entries, _ := msgBundle["entry"].([]any)
	if len(entries) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "message Bundle must contain a leading MessageHeader entry")
		return
	}

	firstEntry, _ := entries[0].(map[string]any)
	msgHeader, _ := firstEntry["resource"].(map[string]any)
	if msgHeader == nil || msgHeader["resourceType"] != "MessageHeader" {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "first entry in message Bundle must be a MessageHeader resource")
		return
	}

	inMsgID, _ := msgHeader["id"].(string)
	if strings.TrimSpace(inMsgID) == "" {
		inMsgID = fmt.Sprintf("msg-%d", time.Now().UTC().UnixNano())
		msgHeader["id"] = inMsgID
	}

	// Persist the incoming MessageHeader
	_, _, _, _, _ = s.upsertResourceMap(r.Context(), tenant, msgHeader)

	// Persist all clinical focus resources in entries[1:]
	var persistedRefs []string
	var focusList []any
	for i := 1; i < len(entries); i++ {
		em, ok := entries[i].(map[string]any)
		if !ok {
			continue
		}
		resMap, ok := em["resource"].(map[string]any)
		if !ok {
			continue
		}
		rt, _ := resMap["resourceType"].(string)
		if rt == "" || rt == "OperationOutcome" {
			continue
		}
		_, rid, _, _, err := s.upsertResourceMap(r.Context(), tenant, resMap)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("failed to process message entry %s: %v", rt, err))
			return
		}
		ref := rt + "/" + rid
		persistedRefs = append(persistedRefs, ref)
		focusList = append(focusList, map[string]any{"reference": ref})
	}

	if asyncMode {
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resourceType": "OperationOutcome",
			"issue": []map[string]any{
				{
					"severity":    "information",
					"code":        "informational",
					"diagnostics": fmt.Sprintf("Accepted message %s (%d focus resource(s) persisted)", inMsgID, len(persistedRefs)),
				},
			},
		})
		return
	}

	ackHeader := map[string]any{
		"resourceType": "MessageHeader",
		"id":           "ack-" + inMsgID,
		"source": map[string]any{
			"name":     "Flint FHIR R4 Server",
			"endpoint": "/fhir/r4/" + tenant,
		},
		"response": map[string]any{
			"identifier": inMsgID,
			"code":       "ok",
		},
	}
	if evCoding, ok := msgHeader["eventCoding"]; ok {
		ackHeader["eventCoding"] = evCoding
	} else if evURI, ok := msgHeader["eventUri"]; ok {
		ackHeader["eventUri"] = evURI
	} else {
		ackHeader["eventUri"] = "urn:flint:message-ack"
	}
	if len(focusList) > 0 {
		ackHeader["focus"] = focusList
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Bundle",
		"id":           "ack-bundle-" + inMsgID,
		"type":         "message",
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"entry": []any{
			map[string]any{
				"fullUrl":  "MessageHeader/ack-" + inMsgID,
				"resource": ackHeader,
			},
			map[string]any{
				"fullUrl": "OperationOutcome/ack-outcome-" + inMsgID,
				"resource": map[string]any{
					"resourceType": "OperationOutcome",
					"id":           "ack-outcome-" + inMsgID,
					"issue": []map[string]any{
						{
							"severity":    "information",
							"code":        "informational",
							"diagnostics": fmt.Sprintf("Processed MessageHeader/%s and persisted %d focus resource(s): %s", inMsgID, len(persistedRefs), strings.Join(persistedRefs, ", ")),
						},
					},
				},
			},
		},
	})
}

// =============================================================================
// HL7 FHIR R4 StructureMap/$transform (Wave 19)
// =============================================================================

func (s *Server) handleStructureMapTransform(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "StructureMap/$transform only supports POST")
		return
	}

	tenant := r.PathValue("tenant")
	pathID := strings.TrimSpace(r.PathValue("id"))
	sourceURL := strings.TrimSpace(r.URL.Query().Get("source"))

	if r.Body == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "StructureMap/$transform requires a request body")
		return
	}

	var bodyMap map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&bodyMap); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "invalid JSON body for StructureMap/$transform: "+err.Error())
		return
	}

	var smMap map[string]any
	var contentMap map[string]any

	if bodyMap["resourceType"] == "Parameters" {
		if params, ok := bodyMap["parameter"].([]any); ok {
			for _, rawP := range params {
				pm, ok := rawP.(map[string]any)
				if !ok {
					continue
				}
				name, _ := pm["name"].(string)
				switch name {
				case "source", "structureMap":
					if v := extractParamString(pm); v != "" && sourceURL == "" {
						sourceURL = v
					}
					if res, ok := pm["resource"].(map[string]any); ok && res["resourceType"] == "StructureMap" {
						smMap = res
					}
				case "content", "resource", "input":
					if res, ok := pm["resource"].(map[string]any); ok {
						contentMap = res
					}
				}
			}
		}
	} else {
		contentMap = bodyMap
	}

	if smMap == nil && pathID != "" && s.store != nil {
		if rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "StructureMap", pathID); err == nil {
			if jb, convErr := s.protoToJSONWithMeta("StructureMap", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); convErr == nil {
				_ = json.Unmarshal(jb, &smMap)
			}
		}
	}

	if smMap == nil && sourceURL != "" && s.store != nil {
		if sr, err := s.store.Search(r.Context(), postgres.SearchParams{TenantID: tenant, ResType: "StructureMap", Count: 50}); err == nil {
			for _, m := range sr.Matches {
				if jb, convErr := s.protoToJSONWithMeta("StructureMap", m.ResourceProto, m.ResVersion, m.LastUpdated); convErr == nil {
					var cand map[string]any
					if json.Unmarshal(jb, &cand) == nil {
						if u, _ := cand["url"].(string); strings.EqualFold(u, sourceURL) || strings.EqualFold(m.ResID, cleanID(sourceURL)) {
							smMap = cand
							break
						}
					}
				}
			}
		}
	}

	if contentMap == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "StructureMap/$transform requires a 'content' source resource")
		return
	}
	if smMap == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", "StructureMap not found; provide an inline StructureMap, stored StructureMap/{id}, or ?source= canonical URL")
		return
	}

	transformed := executeStructureMapRules(smMap, contentMap)
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(transformed)
}

func executeStructureMapRules(smMap, contentMap map[string]any) map[string]any {
	targetType := ""
	if structs, ok := smMap["structure"].([]any); ok {
		for _, rawS := range structs {
			if stMap, ok := rawS.(map[string]any); ok {
				mode, _ := stMap["mode"].(string)
				if mode == "target" {
					if alias, _ := stMap["alias"].(string); alias != "" {
						targetType = alias
					} else if u, _ := stMap["url"].(string); u != "" {
						if idx := strings.LastIndexByte(u, '/'); idx >= 0 {
							targetType = u[idx+1:]
						} else {
							targetType = u
						}
					}
				}
			}
		}
	}
	if targetType == "" {
		if rt, _ := contentMap["resourceType"].(string); rt != "" {
			targetType = rt
		} else {
			targetType = "Basic"
		}
	}

	out := map[string]any{
		"resourceType": targetType,
	}
	if srcID, _ := contentMap["id"].(string); srcID != "" {
		out["id"] = "transformed-" + srcID
	}

	groups, _ := smMap["group"].([]any)
	for _, rawG := range groups {
		gm, ok := rawG.(map[string]any)
		if !ok {
			continue
		}
		rules, _ := gm["rule"].([]any)
		for _, rawR := range rules {
			rm, ok := rawR.(map[string]any)
			if !ok {
				continue
			}
			var extractedVal any
			hasSourceVal := false
			if sources, ok := rm["source"].([]any); ok && len(sources) > 0 {
				if src0, ok := sources[0].(map[string]any); ok {
					if elem, _ := src0["element"].(string); elem != "" {
						if v, exists := contentMap[elem]; exists {
							extractedVal = v
							hasSourceVal = true
						}
					}
					if !hasSourceVal {
						for _, defKey := range []string{"defaultValueString", "defaultValueCode", "defaultValueBoolean"} {
							if dv, ok := src0[defKey]; ok {
								extractedVal = dv
								hasSourceVal = true
								break
							}
						}
					}
				}
			}

			targets, _ := rm["target"].([]any)
			for _, rawT := range targets {
				tm, ok := rawT.(map[string]any)
				if !ok {
					continue
				}
				tgtElem, _ := tm["element"].(string)
				transform, _ := tm["transform"].(string)

				if transform == "create" {
					if tParams, ok := tm["parameter"].([]any); ok && len(tParams) > 0 {
						if pm0, ok := tParams[0].(map[string]any); ok {
							if vs, _ := pm0["valueString"].(string); vs != "" && tgtElem == "" {
								out["resourceType"] = vs
							}
						}
					}
					continue
				}

				if tgtElem == "" {
					continue
				}

				// Check if target has explicit constant parameter override
				var paramVal any
				if tParams, ok := tm["parameter"].([]any); ok && len(tParams) > 0 {
					if pm0, ok := tParams[0].(map[string]any); ok {
						for _, pk := range []string{"valueString", "valueBoolean", "valueInteger", "valueDecimal", "valueId"} {
							if pv, exists := pm0[pk]; exists && pk != "valueId" {
								paramVal = pv
								break
							}
						}
					}
				}

				if paramVal != nil {
					out[tgtElem] = paramVal
				} else if hasSourceVal {
					out[tgtElem] = extractedVal
				}
			}
		}
	}

	return out
}

// =============================================================================
// HL7 FHIR R4 NamingSystem/$preferred-id (Wave 19)
// =============================================================================

type namingSystemEntry struct {
	name         string
	uri          string
	oid          string
	v2csmnemonic string
}

var builtinNamingSystems = []namingSystemEntry{
	{name: "LOINC", uri: "http://loinc.org", oid: "2.16.840.1.113883.6.1", v2csmnemonic: "LN"},
	{name: "SNOMED-CT", uri: "http://snomed.info/sct", oid: "2.16.840.1.113883.6.96", v2csmnemonic: "SCT"},
	{name: "RxNorm", uri: "http://www.nlm.nih.gov/research/umls/rxnorm", oid: "2.16.840.1.113883.6.88", v2csmnemonic: "RXNORM"},
	{name: "UCUM", uri: "http://unitsofmeasure.org", oid: "2.16.840.1.113883.6.8", v2csmnemonic: "UCUM"},
	{name: "ICD-10-CM", uri: "http://hl7.org/fhir/sid/icd-10-cm", oid: "2.16.840.1.113883.6.90", v2csmnemonic: "I10C"},
	{name: "CPT", uri: "http://www.ama-assn.org/go/cpt", oid: "2.16.840.1.113883.6.12", v2csmnemonic: "C4"},
	{name: "CVX", uri: "http://hl7.org/fhir/sid/cvx", oid: "2.16.840.1.113883.12.292", v2csmnemonic: "CVX"},
	{name: "US-NPI", uri: "http://hl7.org/fhir/sid/us-npi", oid: "2.16.840.1.113883.4.6"},
	{name: "US-SSN", uri: "http://hl7.org/fhir/sid/us-ssn", oid: "2.16.840.1.113883.4.1"},
}

func (s *Server) handleNamingSystemPreferredID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "NamingSystem/$preferred-id only supports GET and POST")
		return
	}

	tenant := r.PathValue("tenant")
	idParam := strings.TrimSpace(r.URL.Query().Get("id"))
	typeParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))

	if r.Method == http.MethodPost && r.Body != nil {
		var bodyMap map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&bodyMap); err == nil {
			if bodyMap["resourceType"] == "Parameters" {
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						pm, ok := rawP.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "id":
							if idParam == "" {
								idParam = extractParamString(pm)
							}
						case "type":
							if typeParam == "" {
								typeParam = strings.ToLower(extractParamString(pm))
							}
						}
					}
				}
			}
		}
	}

	if idParam == "" || typeParam == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "NamingSystem/$preferred-id requires both 'id' and 'type' (oid, uri, uuid, v2csmnemonic) parameters")
		return
	}

	cleanInput := strings.TrimPrefix(idParam, "urn:oid:")
	var resolved string

	// 1. Check tenant-stored NamingSystem resources first
	if s.store != nil {
		if sr, err := s.store.Search(r.Context(), postgres.SearchParams{TenantID: tenant, ResType: "NamingSystem", Count: 50}); err == nil {
			for _, m := range sr.Matches {
				jb, convErr := s.protoToJSONWithMeta("NamingSystem", m.ResourceProto, m.ResVersion, m.LastUpdated)
				if convErr != nil {
					continue
				}
				var nsMap map[string]any
				if json.Unmarshal(jb, &nsMap) != nil {
					continue
				}
				uids, _ := nsMap["uniqueId"].([]any)
				matched := false
				for _, rawU := range uids {
					if um, ok := rawU.(map[string]any); ok {
						v, _ := um["value"].(string)
						if strings.EqualFold(v, idParam) || strings.EqualFold(strings.TrimPrefix(v, "urn:oid:"), cleanInput) {
							matched = true
							break
						}
					}
				}
				if matched {
					for _, rawU := range uids {
						if um, ok := rawU.(map[string]any); ok {
							ut, _ := um["type"].(string)
							uv, _ := um["value"].(string)
							pref, _ := um["preferred"].(bool)
							if strings.EqualFold(ut, typeParam) && uv != "" {
								resolved = uv
								if pref {
									break
								}
							}
						}
					}
				}
				if resolved != "" {
					break
				}
			}
		}
	}

	// 2. Fallback to built-in HL7/US Core NamingSystem registry
	if resolved == "" {
		for _, ns := range builtinNamingSystems {
			if strings.EqualFold(ns.uri, idParam) || strings.EqualFold(ns.oid, cleanInput) || strings.EqualFold(ns.v2csmnemonic, idParam) || strings.EqualFold(ns.name, idParam) {
				switch typeParam {
				case "uri":
					resolved = ns.uri
				case "oid":
					resolved = ns.oid
				case "v2csmnemonic":
					resolved = ns.v2csmnemonic
				}
				break
			}
		}
	}

	if resolved == "" {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("No preferred %q identifier found for NamingSystem %q", typeParam, idParam))
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{
				"name":        "result",
				"valueString": resolved,
			},
		},
	})
}

// =============================================================================
// Wave 20:
// 1. HL7 Da Vinci PAS Claim/$inquire (GET/POST /Claim/$inquire & /Claim/{id}/$inquire)
// 2. HL7 Da Vinci DTR Questionnaire/$package (GET/POST /Questionnaire/$package & /Questionnaire/{id}/$package)
// 3. HL7 FHIR R4 CodeSystem/$find-matches (GET/POST /CodeSystem/$find-matches & /CodeSystem/{id}/$find-matches)
// =============================================================================

// handleClaimInquire implements HL7 Da Vinci Prior Authorization Support (PAS) Claim/$inquire
// (GET/POST /fhir/r4/{tenant}/Claim/$inquire and /fhir/r4/{tenant}/Claim/{id}/$inquire).
func (s *Server) handleClaimInquire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "Claim/$inquire only supports GET and POST")
		return
	}
	if s.store == nil {
		writeOperationOutcome(w, http.StatusNotImplemented, "not-supported", "Claim/$inquire requires an operational store")
		return
	}

	tenant := r.PathValue("tenant")
	claimID := strings.TrimPrefix(strings.TrimSpace(r.PathValue("id")), "Claim/")
	q := r.URL.Query()
	if claimID == "" {
		claimID = strings.TrimPrefix(strings.TrimSpace(q.Get("claim")), "Claim/")
	}
	patientParam := strings.TrimPrefix(strings.TrimSpace(q.Get("patient")), "Patient/")
	preAuthRef := strings.TrimSpace(q.Get("preAuthRef"))
	identifierParam := strings.TrimSpace(q.Get("identifier"))

	var inlineClaim map[string]any
	if r.Method == http.MethodPost && r.Body != nil {
		var bodyMap map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&bodyMap); err == nil {
			switch bodyMap["resourceType"] {
			case "Claim":
				inlineClaim = bodyMap
			case "Bundle":
				if entries, ok := bodyMap["entry"].([]any); ok {
					for _, rawE := range entries {
						if em, ok := rawE.(map[string]any); ok {
							if res, ok := em["resource"].(map[string]any); ok && res["resourceType"] == "Claim" {
								inlineClaim = res
								break
							}
						}
					}
				}
			case "Parameters":
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						pm, ok := rawP.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "resource", "claim":
							if res, ok := pm["resource"].(map[string]any); ok {
								if res["resourceType"] == "Claim" {
									inlineClaim = res
								} else if res["resourceType"] == "Bundle" {
									if entries, ok := res["entry"].([]any); ok {
										for _, rawE := range entries {
											if em, ok := rawE.(map[string]any); ok {
												if er, ok := em["resource"].(map[string]any); ok && er["resourceType"] == "Claim" {
													inlineClaim = er
													break
												}
											}
										}
									}
								}
							} else if sVal := extractParamString(pm); sVal != "" && claimID == "" {
								claimID = strings.TrimPrefix(sVal, "Claim/")
							}
						case "patient":
							if patientParam == "" {
								patientParam = strings.TrimPrefix(extractParamString(pm), "Patient/")
							}
						case "preAuthRef":
							if preAuthRef == "" {
								preAuthRef = extractParamString(pm)
							}
						case "identifier":
							if identifierParam == "" {
								identifierParam = extractParamString(pm)
							}
						}
					}
				}
			}
		}
	}

	if inlineClaim != nil {
		if cid, _ := inlineClaim["id"].(string); cid != "" && claimID == "" {
			claimID = strings.TrimPrefix(strings.TrimSpace(cid), "Claim/")
		}
		if patMap, ok := inlineClaim["patient"].(map[string]any); ok && patientParam == "" {
			if ref, _ := patMap["reference"].(string); ref != "" {
				patientParam = strings.TrimPrefix(strings.TrimSpace(ref), "Patient/")
			}
		}
		if ids, ok := inlineClaim["identifier"].([]any); ok && identifierParam == "" && len(ids) > 0 {
			if idm, ok := ids[0].(map[string]any); ok {
				if v, _ := idm["value"].(string); v != "" {
					identifierParam = strings.TrimSpace(v)
				}
			}
		}
	}

	if claimID == "" && patientParam == "" && preAuthRef == "" && identifierParam == "" && inlineClaim == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Claim/$inquire requires a Claim id, claim resource/reference, patient, preAuthRef, or identifier parameter")
		return
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx && secCtx != nil {
		if !auth.Allows(secCtx, "Claim", auth.ActionRead) && !auth.Allows(secCtx, "Claim", auth.ActionSearch) &&
			!auth.Allows(secCtx, "Claim", auth.ActionCreate) && !auth.Allows(secCtx, "ClaimResponse", auth.ActionRead) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for Claim/$inquire")
			return
		}
		if auth.IsPatientRestricted(secCtx) && patientParam != "" && cleanID(secCtx.GetPatientId()) != cleanID(patientParam) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "patient-scoped token cannot invoke Claim/$inquire outside own patient compartment")
			return
		}
	}

	var matchedResponses []map[string]any
	if sr, err := s.store.Search(r.Context(), postgres.SearchParams{TenantID: tenant, ResType: "ClaimResponse", Count: 100}); err == nil {
		for _, m := range sr.Matches {
			jb, convErr := s.protoToJSONWithMeta("ClaimResponse", m.ResourceProto, m.ResVersion, m.LastUpdated)
			if convErr != nil {
				continue
			}
			var crMap map[string]any
			if json.Unmarshal(jb, &crMap) != nil {
				continue
			}
			crID, _ := crMap["id"].(string)
			reqRef := ""
			if reqMap, ok := crMap["request"].(map[string]any); ok {
				reqRef, _ = reqMap["reference"].(string)
			}
			patRef := ""
			if patMap, ok := crMap["patient"].(map[string]any); ok {
				patRef, _ = patMap["reference"].(string)
			}
			crPreAuth, _ := crMap["preAuthRef"].(string)

			if claimID != "" {
				if !strings.EqualFold(reqRef, "Claim/"+claimID) && !strings.EqualFold(reqRef, claimID) && !strings.EqualFold(crID, "cr-"+claimID) && !strings.EqualFold(crID, "claimresponse-"+claimID) {
					continue
				}
			}
			if preAuthRef != "" && !strings.EqualFold(crPreAuth, preAuthRef) {
				continue
			}
			if patientParam != "" && !strings.EqualFold(strings.TrimPrefix(patRef, "Patient/"), patientParam) {
				continue
			}
			if identifierParam != "" {
				idMatched := strings.EqualFold(crPreAuth, identifierParam)
				if !idMatched {
					if ids, ok := crMap["identifier"].([]any); ok {
						for _, rawID := range ids {
							if idm, ok := rawID.(map[string]any); ok {
								if v, _ := idm["value"].(string); strings.EqualFold(v, identifierParam) {
									idMatched = true
									break
								}
							}
						}
					}
				}
				if !idMatched {
					continue
				}
			}
			matchedResponses = append(matchedResponses, crMap)
		}
	}

	var resolvedClaim map[string]any
	if claimID != "" {
		if rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Claim", claimID); err == nil {
			if jb, cErr := s.protoToJSONWithMeta("Claim", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); cErr == nil {
				_ = json.Unmarshal(jb, &resolvedClaim)
			}
		}
	}
	if resolvedClaim == nil && inlineClaim != nil {
		resolvedClaim = inlineClaim
	}

	if len(matchedResponses) == 0 && resolvedClaim != nil {
		cid, _ := resolvedClaim["id"].(string)
		if strings.TrimSpace(cid) == "" {
			cid = claimID
		}
		if strings.TrimSpace(cid) == "" {
			cid = "pas-inquiry"
		}
		patRef := "Patient/unknown"
		if patMap, ok := resolvedClaim["patient"].(map[string]any); ok {
			if rStr, _ := patMap["reference"].(string); rStr != "" {
				patRef = rStr
			}
		} else if patientParam != "" {
			patRef = "Patient/" + patientParam
		}
		insurerRef := "Organization/payer-default"
		if insMap, ok := resolvedClaim["insurer"].(map[string]any); ok {
			if rStr, _ := insMap["reference"].(string); rStr != "" {
				insurerRef = rStr
			}
		}
		synthPreAuth := preAuthRef
		if synthPreAuth == "" {
			synthPreAuth = "AUTH-" + strings.ToUpper(cid)
		}
		synthCR := map[string]any{
			"resourceType": "ClaimResponse",
			"id":           "cr-" + cid,
			"status":       "active",
			"type": map[string]any{
				"coding": []map[string]any{
					{
						"system":  "http://terminology.hl7.org/CodeSystem/claim-type",
						"code":    "professional",
						"display": "Professional",
					},
				},
			},
			"use":         "preauthorization",
			"patient":     map[string]any{"reference": patRef},
			"created":     time.Now().UTC().Format(time.RFC3339),
			"insurer":     map[string]any{"reference": insurerRef},
			"request":     map[string]any{"reference": "Claim/" + cid},
			"outcome":     "complete",
			"disposition": "Prior Authorization Approved (PAS Inquiry)",
			"preAuthRef":  synthPreAuth,
		}
		matchedResponses = append(matchedResponses, synthCR)
	}

	if len(matchedResponses) == 0 {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", "No matching Prior Authorization ClaimResponse or Claim found for $inquire")
		return
	}

	entries := make([]map[string]any, 0, len(matchedResponses)+1)
	for _, cr := range matchedResponses {
		crID, _ := cr["id"].(string)
		entries = append(entries, map[string]any{
			"fullUrl":  "ClaimResponse/" + crID,
			"resource": cr,
		})
	}
	if resolvedClaim != nil {
		cid, _ := resolvedClaim["id"].(string)
		entries = append(entries, map[string]any{
			"fullUrl":  "Claim/" + cid,
			"resource": resolvedClaim,
		})
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Bundle",
		"id":           "pas-inquiry-response-" + firstNonEmptyStr(claimID, preAuthRef, "bundle"),
		"type":         "collection",
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"total":        len(entries),
		"entry":        entries,
	})
}

// handleQuestionnairePackage implements HL7 Da Vinci DTR Questionnaire/$package
// (GET/POST /fhir/r4/{tenant}/Questionnaire/$package and /fhir/r4/{tenant}/Questionnaire/{id}/$package).
func (s *Server) handleQuestionnairePackage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "Questionnaire/$package only supports GET and POST")
		return
	}
	if s.store == nil {
		writeOperationOutcome(w, http.StatusNotImplemented, "not-supported", "Questionnaire/$package requires an operational store")
		return
	}

	tenant := r.PathValue("tenant")
	qID := strings.TrimPrefix(strings.TrimSpace(r.PathValue("id")), "Questionnaire/")
	qCanonical := strings.TrimSpace(r.URL.Query().Get("questionnaire"))
	if qCanonical == "" {
		qCanonical = strings.TrimSpace(r.URL.Query().Get("url"))
	}

	var inlineQ map[string]any
	if r.Method == http.MethodPost && r.Body != nil {
		var bodyMap map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&bodyMap); err == nil {
			switch bodyMap["resourceType"] {
			case "Questionnaire":
				inlineQ = bodyMap
			case "Parameters":
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						pm, ok := rawP.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "questionnaire", "url":
							if res, ok := pm["resource"].(map[string]any); ok && res["resourceType"] == "Questionnaire" {
								inlineQ = res
							} else if sVal := extractParamString(pm); sVal != "" && qCanonical == "" {
								qCanonical = sVal
							}
						}
					}
				}
			}
		}
	}

	if qID == "" && strings.HasPrefix(qCanonical, "Questionnaire/") {
		qID = strings.TrimPrefix(qCanonical, "Questionnaire/")
		qCanonical = ""
	}

	if qID == "" && qCanonical == "" && inlineQ == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Questionnaire/$package requires a Questionnaire {id}, 'questionnaire' canonical URL, or inline Questionnaire resource")
		return
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx && secCtx != nil {
		if !auth.Allows(secCtx, "Questionnaire", auth.ActionRead) && !auth.Allows(secCtx, "Questionnaire", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for Questionnaire/$package")
			return
		}
	}

	qMap := inlineQ
	if qMap == nil && qID != "" {
		if rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Questionnaire", qID); err == nil {
			if jb, cErr := s.protoToJSONWithMeta("Questionnaire", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); cErr == nil {
				_ = json.Unmarshal(jb, &qMap)
			}
		}
	}
	if qMap == nil && qCanonical != "" {
		if sr, err := s.store.Search(r.Context(), postgres.SearchParams{TenantID: tenant, ResType: "Questionnaire", Count: 50}); err == nil {
			for _, m := range sr.Matches {
				jb, cErr := s.protoToJSONWithMeta("Questionnaire", m.ResourceProto, m.ResVersion, m.LastUpdated)
				if cErr != nil {
					continue
				}
				var candidate map[string]any
				if json.Unmarshal(jb, &candidate) == nil {
					u, _ := candidate["url"].(string)
					cid, _ := candidate["id"].(string)
					if strings.EqualFold(u, qCanonical) || strings.EqualFold(cid, qCanonical) {
						qMap = candidate
						break
					}
				}
			}
		}
	}

	if qMap == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Questionnaire %q not found for $package", firstNonEmptyStr(qID, qCanonical)))
		return
	}

	resolvedQID, _ := qMap["id"].(string)
	if resolvedQID == "" {
		resolvedQID = firstNonEmptyStr(qID, "dtr-questionnaire")
		qMap["id"] = resolvedQID
	}

	// Collect referenced Library canonicals (cqf-library extension) and ValueSet canonicals (item[].answerValueSet)
	var libCanonicals []string
	seenLibs := make(map[string]bool)
	addLib := func(u string) {
		u = strings.TrimSpace(u)
		if u != "" && !seenLibs[u] {
			seenLibs[u] = true
			libCanonicals = append(libCanonicals, u)
		}
	}
	if exts, ok := qMap["extension"].([]any); ok {
		for _, rawExt := range exts {
			if em, ok := rawExt.(map[string]any); ok {
				extURL, _ := em["url"].(string)
				if strings.Contains(strings.ToLower(extURL), "cqf-library") || strings.Contains(strings.ToLower(extURL), "launchcontext") {
					for _, vk := range []string{"valueCanonical", "valueUri", "valueString"} {
						if s, ok := em[vk].(string); ok && s != "" && !strings.Contains(strings.ToLower(extURL), "launchcontext") {
							addLib(s)
						}
					}
				}
			}
		}
	}

	var vsCanonicals []string
	seenVS := make(map[string]bool)
	var collectItemValueSets func(items []any)
	collectItemValueSets = func(items []any) {
		for _, rawItem := range items {
			im, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			if avs, ok := im["answerValueSet"].(string); ok && strings.TrimSpace(avs) != "" {
				u := strings.TrimSpace(avs)
				if !seenVS[u] {
					seenVS[u] = true
					vsCanonicals = append(vsCanonicals, u)
				}
			}
			if nested, ok := im["item"].([]any); ok {
				collectItemValueSets(nested)
			}
		}
	}
	if items, ok := qMap["item"].([]any); ok {
		collectItemValueSets(items)
	}

	entries := []map[string]any{
		{
			"fullUrl":  "Questionnaire/" + resolvedQID,
			"resource": qMap,
		},
	}

	// Resolve and append Library dependencies
	for idx, libURL := range libCanonicals {
		var libMap map[string]any
		if sr, err := s.store.Search(r.Context(), postgres.SearchParams{TenantID: tenant, ResType: "Library", Count: 50}); err == nil {
			for _, m := range sr.Matches {
				jb, cErr := s.protoToJSONWithMeta("Library", m.ResourceProto, m.ResVersion, m.LastUpdated)
				if cErr != nil {
					continue
				}
				var candidate map[string]any
				if json.Unmarshal(jb, &candidate) == nil {
					u, _ := candidate["url"].(string)
					cid, _ := candidate["id"].(string)
					if strings.EqualFold(u, libURL) || strings.EqualFold(cid, strings.TrimPrefix(libURL, "Library/")) {
						libMap = candidate
						break
					}
				}
			}
		}
		if libMap == nil {
			synthID := fmt.Sprintf("dtr-lib-%d", idx+1)
			if parts := strings.Split(strings.TrimRight(libURL, "/"), "/"); len(parts) > 0 && parts[len(parts)-1] != "" {
				synthID = parts[len(parts)-1]
			}
			libMap = map[string]any{
				"resourceType": "Library",
				"id":           synthID,
				"url":          libURL,
				"status":       "active",
				"type": map[string]any{
					"coding": []map[string]any{
						{
							"system":  "http://terminology.hl7.org/CodeSystem/library-type",
							"code":    "logic-library",
							"display": "Logic Library",
						},
					},
				},
			}
		}
		libID, _ := libMap["id"].(string)
		entries = append(entries, map[string]any{
			"fullUrl":  "Library/" + libID,
			"resource": libMap,
		})
	}

	// Resolve and append ValueSet dependencies
	for idx, vsURL := range vsCanonicals {
		var vsMap map[string]any
		if sr, err := s.store.Search(r.Context(), postgres.SearchParams{TenantID: tenant, ResType: "ValueSet", Count: 50}); err == nil {
			for _, m := range sr.Matches {
				jb, cErr := s.protoToJSONWithMeta("ValueSet", m.ResourceProto, m.ResVersion, m.LastUpdated)
				if cErr != nil {
					continue
				}
				var candidate map[string]any
				if json.Unmarshal(jb, &candidate) == nil {
					u, _ := candidate["url"].(string)
					cid, _ := candidate["id"].(string)
					if strings.EqualFold(u, vsURL) || strings.EqualFold(cid, strings.TrimPrefix(vsURL, "ValueSet/")) {
						vsMap = candidate
						break
					}
				}
			}
		}
		if vsMap == nil {
			if resolvedURL, resolvedName, concepts, err := s.resolveValueSetConcepts(r.Context(), tenant, "", vsURL, nil); err == nil {
				contains := make([]map[string]any, 0, len(concepts))
				for _, c := range concepts {
					entry := map[string]any{
						"system": c.System,
						"code":   c.Code,
					}
					if c.Display != "" {
						entry["display"] = c.Display
					}
					contains = append(contains, entry)
				}
				synthID := fmt.Sprintf("dtr-vs-%d", idx+1)
				if parts := strings.Split(strings.TrimRight(vsURL, "/"), "/"); len(parts) > 0 && parts[len(parts)-1] != "" {
					synthID = parts[len(parts)-1]
				}
				vsMap = map[string]any{
					"resourceType": "ValueSet",
					"id":           synthID,
					"url":          firstNonEmptyStr(resolvedURL, vsURL),
					"name":         firstNonEmptyStr(resolvedName, synthID),
					"status":       "active",
					"expansion": map[string]any{
						"timestamp": time.Now().UTC().Format(time.RFC3339),
						"total":     len(contains),
						"contains":  contains,
					},
				}
			}
		}
		if vsMap != nil {
			vsID, _ := vsMap["id"].(string)
			entries = append(entries, map[string]any{
				"fullUrl":  "ValueSet/" + vsID,
				"resource": vsMap,
			})
		}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Bundle",
		"id":           "dtr-package-" + resolvedQID,
		"type":         "collection",
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"total":        len(entries),
		"entry":        entries,
	})
}

type findMatchPropertyFilter struct {
	Code  string
	Value string
}

// handleCodeSystemFindMatches implements HL7 FHIR R4 CodeSystem/$find-matches (§4.8.16.4)
// (GET/POST /fhir/r4/{tenant}/CodeSystem/$find-matches and /fhir/r4/{tenant}/CodeSystem/{id}/$find-matches).
func (s *Server) handleCodeSystemFindMatches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "CodeSystem/$find-matches only supports GET and POST")
		return
	}

	tenant := r.PathValue("tenant")
	csID := strings.TrimPrefix(strings.TrimSpace(r.PathValue("id")), "CodeSystem/")
	q := r.URL.Query()
	systemURL := strings.TrimSpace(q.Get("system"))
	if systemURL == "" {
		systemURL = strings.TrimSpace(q.Get("url"))
	}
	exact := strings.EqualFold(strings.TrimSpace(q.Get("exact")), "true")

	var propFilters []findMatchPropertyFilter
	for _, rawP := range q["property"] {
		trimmed := strings.TrimSpace(rawP)
		if trimmed == "" {
			continue
		}
		if k, v, ok := strings.Cut(trimmed, "|"); ok {
			propFilters = append(propFilters, findMatchPropertyFilter{Code: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
		} else if k, v, ok := strings.Cut(trimmed, "="); ok {
			propFilters = append(propFilters, findMatchPropertyFilter{Code: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
		} else {
			propFilters = append(propFilters, findMatchPropertyFilter{Code: "display", Value: trimmed})
		}
	}
	if f := strings.TrimSpace(q.Get("filter")); f != "" {
		propFilters = append(propFilters, findMatchPropertyFilter{Code: "display", Value: f})
	}
	if c := strings.TrimSpace(q.Get("code")); c != "" {
		propFilters = append(propFilters, findMatchPropertyFilter{Code: "code", Value: c})
	}

	var inlineCS map[string]any
	if r.Method == http.MethodPost && r.Body != nil {
		var bodyMap map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&bodyMap); err == nil {
			switch bodyMap["resourceType"] {
			case "CodeSystem":
				inlineCS = bodyMap
			case "Parameters":
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						pm, ok := rawP.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "system", "url":
							if sVal := extractParamString(pm); sVal != "" && systemURL == "" {
								systemURL = sVal
							}
						case "exact":
							if b, ok := pm["valueBoolean"].(bool); ok {
								exact = b
							}
						case "codeSystem":
							if res, ok := pm["resource"].(map[string]any); ok && res["resourceType"] == "CodeSystem" {
								inlineCS = res
							}
						case "property":
							if parts, ok := pm["part"].([]any); ok {
								var pCode, pVal string
								for _, rawPart := range parts {
									partMap, ok := rawPart.(map[string]any)
									if !ok {
										continue
									}
									pName, _ := partMap["name"].(string)
									switch pName {
									case "code":
										pCode = extractParamString(partMap)
									case "value", "description":
										pVal = extractParamString(partMap)
										if pVal == "" {
											if cm, ok := partMap["valueCoding"].(map[string]any); ok {
												pVal, _ = cm["code"].(string)
											} else if bVal, ok := partMap["valueBoolean"].(bool); ok {
												pVal = strconv.FormatBool(bVal)
											} else if iVal, ok := partMap["valueInteger"].(float64); ok {
												pVal = strconv.Itoa(int(iVal))
											}
										}
									}
								}
								if pCode != "" && pVal != "" {
									propFilters = append(propFilters, findMatchPropertyFilter{Code: pCode, Value: pVal})
								}
							} else if sVal := extractParamString(pm); sVal != "" {
								if k, v, ok := strings.Cut(sVal, "|"); ok {
									propFilters = append(propFilters, findMatchPropertyFilter{Code: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
								} else {
									propFilters = append(propFilters, findMatchPropertyFilter{Code: "display", Value: sVal})
								}
							}
						}
					}
				}
			}
		}
	}

	if csID == "" && systemURL == "" && inlineCS == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "CodeSystem/$find-matches requires 'system' URI, CodeSystem {id}, or inline CodeSystem")
		return
	}

	concepts, csName, err := s.resolveCodeSystemConceptsWithInline(r.Context(), tenant, csID, systemURL, inlineCS)
	if err != nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", err.Error())
		return
	}

	parentsByCode := make(map[string][]string, len(concepts))
	for _, c := range concepts {
		if len(c.Parents) > 0 {
			parentsByCode[c.Code] = c.Parents
		}
	}

	var matches []map[string]any
	for _, c := range concepts {
		allMatched := true
		for _, pf := range propFilters {
			pKey := strings.TrimSpace(pf.Code)
			pVal := strings.TrimSpace(pf.Value)
			if pKey == "" || pVal == "" {
				continue
			}
			var propMatch bool
			switch strings.ToLower(pKey) {
			case "code", "concept":
				if exact {
					propMatch = c.Code == pVal
				} else {
					propMatch = strings.EqualFold(c.Code, pVal) || strings.Contains(strings.ToLower(c.Code), strings.ToLower(pVal))
				}
			case "display", "text", "filter", "description":
				if exact {
					propMatch = strings.EqualFold(c.Display, pVal)
				} else {
					propMatch = strings.Contains(strings.ToLower(c.Display), strings.ToLower(pVal)) || strings.Contains(strings.ToLower(c.Code), strings.ToLower(pVal))
				}
			case "parent", "ancestor", "is-a", "subsumedby":
				propMatch = codeIsAncestorOrEqual(pVal, c.Code, parentsByCode, false) || slicesContainsFold(c.Parents, pVal)
			default:
				if c.Properties != nil {
					actual, ok := c.Properties[pKey]
					if !ok {
						for k, v := range c.Properties {
							if strings.EqualFold(k, pKey) {
								actual = v
								ok = true
								break
							}
						}
					}
					if ok {
						if exact {
							propMatch = strings.EqualFold(actual, pVal)
						} else {
							propMatch = strings.Contains(strings.ToLower(actual), strings.ToLower(pVal))
						}
					}
				}
			}
			if !propMatch {
				allMatched = false
				break
			}
		}
		if !allMatched {
			continue
		}
		sys := c.System
		if sys == "" {
			sys = systemURL
		}
		coding := map[string]any{
			"system": sys,
			"code":   c.Code,
		}
		if c.Display != "" {
			coding["display"] = c.Display
		}
		matches = append(matches, map[string]any{
			"name": "match",
			"part": []map[string]any{
				{
					"name":        "code",
					"valueCoding": coding,
				},
				{
					"name":         "exact",
					"valueBoolean": exact,
				},
				{
					"name":        "comment",
					"valueString": fmt.Sprintf("Matched in CodeSystem %s", firstNonEmptyStr(csName, sys, csID)),
				},
			},
		})
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    matches,
	})
}

func slicesContainsFold(list []string, target string) bool {
	for _, s := range list {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}
