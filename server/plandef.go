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
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/fhirutil"
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
