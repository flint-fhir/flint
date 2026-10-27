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
	"strings"
	"time"

	"github.com/flint-fhir/flint/pkg/auth"
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
