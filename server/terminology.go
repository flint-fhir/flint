package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flint-fhir/flint/store/postgres"
)

type terminologyConcept struct {
	System  string `json:"system"`
	Code    string `json:"code"`
	Display string `json:"display,omitempty"`
}

type builtInCodeSystemDef struct {
	Name     string
	URL      string
	Concepts []terminologyConcept
}

var builtInCodeSystems = map[string]builtInCodeSystemDef{
	"http://hl7.org/fhir/administrative-gender": {
		Name: "AdministrativeGender",
		URL:  "http://hl7.org/fhir/administrative-gender",
		Concepts: []terminologyConcept{
			{System: "http://hl7.org/fhir/administrative-gender", Code: "male", Display: "Male"},
			{System: "http://hl7.org/fhir/administrative-gender", Code: "female", Display: "Female"},
			{System: "http://hl7.org/fhir/administrative-gender", Code: "other", Display: "Other"},
			{System: "http://hl7.org/fhir/administrative-gender", Code: "unknown", Display: "Unknown"},
		},
	},
	"http://hl7.org/fhir/observation-status": {
		Name: "ObservationStatus",
		URL:  "http://hl7.org/fhir/observation-status",
		Concepts: []terminologyConcept{
			{System: "http://hl7.org/fhir/observation-status", Code: "registered", Display: "Registered"},
			{System: "http://hl7.org/fhir/observation-status", Code: "preliminary", Display: "Preliminary"},
			{System: "http://hl7.org/fhir/observation-status", Code: "final", Display: "Final"},
			{System: "http://hl7.org/fhir/observation-status", Code: "amended", Display: "Amended"},
			{System: "http://hl7.org/fhir/observation-status", Code: "corrected", Display: "Corrected"},
			{System: "http://hl7.org/fhir/observation-status", Code: "cancelled", Display: "Cancelled"},
			{System: "http://hl7.org/fhir/observation-status", Code: "entered-in-error", Display: "Entered in Error"},
			{System: "http://hl7.org/fhir/observation-status", Code: "unknown", Display: "Unknown"},
		},
	},
	"http://terminology.hl7.org/CodeSystem/condition-clinical": {
		Name: "ConditionClinicalStatusCodes",
		URL:  "http://terminology.hl7.org/CodeSystem/condition-clinical",
		Concepts: []terminologyConcept{
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "active", Display: "Active"},
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "recurrence", Display: "Recurrence"},
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "relapse", Display: "Relapse"},
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "inactive", Display: "Inactive"},
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "remission", Display: "Remission"},
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "resolved", Display: "Resolved"},
		},
	},
	"http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical": {
		Name: "AllergyIntoleranceClinicalStatusCodes",
		URL:  "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical",
		Concepts: []terminologyConcept{
			{System: "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", Code: "active", Display: "Active"},
			{System: "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", Code: "inactive", Display: "Inactive"},
			{System: "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", Code: "resolved", Display: "Resolved"},
		},
	},
	"http://terminology.hl7.org/CodeSystem/observation-category": {
		Name: "ObservationCategoryCodes",
		URL:  "http://terminology.hl7.org/CodeSystem/observation-category",
		Concepts: []terminologyConcept{
			{System: "http://terminology.hl7.org/CodeSystem/observation-category", Code: "vital-signs", Display: "Vital Signs"},
			{System: "http://terminology.hl7.org/CodeSystem/observation-category", Code: "laboratory", Display: "Laboratory"},
			{System: "http://terminology.hl7.org/CodeSystem/observation-category", Code: "social-history", Display: "Social History"},
			{System: "http://terminology.hl7.org/CodeSystem/observation-category", Code: "imaging", Display: "Imaging"},
			{System: "http://terminology.hl7.org/CodeSystem/observation-category", Code: "survey", Display: "Survey"},
		},
	},
	"http://loinc.org": {
		Name: "LOINC",
		URL:  "http://loinc.org",
		Concepts: []terminologyConcept{
			{System: "http://loinc.org", Code: "8867-4", Display: "Heart rate"},
			{System: "http://loinc.org", Code: "85354-9", Display: "Blood pressure panel with all children optional"},
			{System: "http://loinc.org", Code: "8480-6", Display: "Systolic blood pressure"},
			{System: "http://loinc.org", Code: "8462-4", Display: "Diastolic blood pressure"},
			{System: "http://loinc.org", Code: "9279-1", Display: "Respiratory rate"},
			{System: "http://loinc.org", Code: "8310-5", Display: "Body temperature"},
			{System: "http://loinc.org", Code: "29463-7", Display: "Body weight"},
			{System: "http://loinc.org", Code: "8302-2", Display: "Body height"},
			{System: "http://loinc.org", Code: "39156-5", Display: "Body mass index (BMI) [Ratio]"},
			{System: "http://loinc.org", Code: "2708-6", Display: "Oxygen saturation in Arterial blood"},
			{System: "http://loinc.org", Code: "59408-5", Display: "Oxygen saturation in Arterial blood by Pulse oximetry"},
			{System: "http://loinc.org", Code: "72166-2", Display: "Tobacco smoking status"},
			{System: "http://loinc.org", Code: "96607-7", Display: "Blood pressure panel mean systolic and mean diastolic"},
			{System: "http://loinc.org", Code: "24323-8", Display: "Comprehensive metabolic 2000 panel - Serum or Plasma"},
			{System: "http://loinc.org", Code: "4548-4", Display: "Hemoglobin A1c/Hemoglobin.total in Blood"},
			{System: "http://loinc.org", Code: "2339-0", Display: "Glucose [Mass/volume] in Blood"},
		},
	},
	"http://snomed.info/sct": {
		Name: "SNOMED_CT",
		URL:  "http://snomed.info/sct",
		Concepts: []terminologyConcept{
			{System: "http://snomed.info/sct", Code: "44054006", Display: "Diabetes mellitus type 2"},
			{System: "http://snomed.info/sct", Code: "38341003", Display: "Hypertensive disorder, systemic arterial"},
			{System: "http://snomed.info/sct", Code: "29303009", Display: "Electrocardiographic procedure"},
			{System: "http://snomed.info/sct", Code: "119297000", Display: "Blood specimen"},
			{System: "http://snomed.info/sct", Code: "108252007", Display: "Laboratory procedure"},
		},
	},
	"http://www.nlm.nih.gov/research/umls/rxnorm": {
		Name: "RxNorm",
		URL:  "http://www.nlm.nih.gov/research/umls/rxnorm",
		Concepts: []terminologyConcept{
			{System: "http://www.nlm.nih.gov/research/umls/rxnorm", Code: "860975", Display: "24 HR Metformin hydrochloride 500 MG Extended Release Oral Tablet"},
			{System: "http://www.nlm.nih.gov/research/umls/rxnorm", Code: "197361", Display: "Amlodipine 5 MG Oral Tablet"},
			{System: "http://www.nlm.nih.gov/research/umls/rxnorm", Code: "314076", Display: "Lisinopril 10 MG Oral Tablet"},
		},
	},
}

var builtInValueSetToCodeSystem = map[string]string{
	"http://hl7.org/fhir/ValueSet/administrative-gender":       "http://hl7.org/fhir/administrative-gender",
	"http://hl7.org/fhir/ValueSet/observation-status":          "http://hl7.org/fhir/observation-status",
	"http://hl7.org/fhir/ValueSet/condition-clinical":          "http://terminology.hl7.org/CodeSystem/condition-clinical",
	"http://hl7.org/fhir/ValueSet/allergyintolerance-clinical": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical",
	"http://hl7.org/fhir/ValueSet/observation-category":        "http://terminology.hl7.org/CodeSystem/observation-category",
	"http://hl7.org/fhir/ValueSet/observation-vitalsignresult": "http://loinc.org",
	"http://hl7.org/fhir/us/core/ValueSet/us-core-vital-signs": "http://loinc.org",
}

func extractTerminologyParams(r *http.Request) (map[string]string, map[string]any) {
	out := make(map[string]string)
	for k, vals := range r.URL.Query() {
		if len(vals) > 0 {
			out[k] = strings.TrimSpace(vals[0])
		}
	}
	var inlineResource map[string]any
	if r.Method == http.MethodPost && r.Body != nil {
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if err == nil && len(bodyBytes) > 0 {
			var root map[string]any
			if json.Unmarshal(bodyBytes, &root) == nil {
				rt, _ := root["resourceType"].(string)
				switch rt {
				case "Parameters":
					if params, ok := root["parameter"].([]any); ok {
						for _, p := range params {
							pm, ok := p.(map[string]any)
							if !ok {
								continue
							}
							name, _ := pm["name"].(string)
							if name == "" {
								continue
							}
							for _, vk := range []string{"valueUri", "valueCanonical", "valueCode", "valueString", "valueId"} {
								if s, ok := pm[vk].(string); ok && s != "" {
									out[name] = strings.TrimSpace(s)
								}
							}
							if iVal, ok := pm["valueInteger"].(float64); ok {
								out[name] = strconv.Itoa(int(iVal))
							}
							if name == "valueSet" {
								if resMap, ok := pm["resource"].(map[string]any); ok {
									inlineResource = resMap
								}
							}
							if name == "coding" {
								if codingMap, ok := pm["valueCoding"].(map[string]any); ok {
									if sys, _ := codingMap["system"].(string); sys != "" && out["system"] == "" {
										out["system"] = sys
									}
									if code, _ := codingMap["code"].(string); code != "" && out["code"] == "" {
										out["code"] = code
									}
									if disp, _ := codingMap["display"].(string); disp != "" && out["display"] == "" {
										out["display"] = disp
									}
								}
							}
						}
					}
				case "ValueSet":
					inlineResource = root
				}
			}
		}
	}
	return out, inlineResource
}

func (s *Server) resolveValueSetConcepts(ctx context.Context, tenant, vsID, vsURL string, inlineVS map[string]any) (string, string, []terminologyConcept, error) {
	var vsMap map[string]any
	if inlineVS != nil {
		vsMap = inlineVS
	} else if vsID != "" && s.store != nil {
		rec, err := s.store.ReadResourceWithMeta(ctx, tenant, "ValueSet", vsID)
		if err != nil {
			return "", "", nil, err
		}
		if jsonBytes, err := s.protoToJSONWithMeta("ValueSet", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
			_ = json.Unmarshal(jsonBytes, &vsMap)
		}
	} else if vsURL != "" && s.store != nil {
		sr, err := s.store.Search(ctx, postgres.SearchParams{
			TenantID: tenant,
			ResType:  "ValueSet",
			Tokens:   map[string]string{"url": vsURL},
			Count:    1,
		})
		if err == nil && len(sr.Matches) > 0 {
			res := sr.Matches[0]
			if jsonBytes, err := s.protoToJSONWithMeta("ValueSet", res.ResourceProto, res.ResVersion, res.LastUpdated); err == nil {
				_ = json.Unmarshal(jsonBytes, &vsMap)
			}
		}
	}

	if vsMap != nil {
		resolvedURL, _ := vsMap["url"].(string)
		if resolvedURL == "" {
			resolvedURL = vsURL
		}
		resolvedName, _ := vsMap["name"].(string)
		var concepts []terminologyConcept

		if exp, ok := vsMap["expansion"].(map[string]any); ok {
			if contains, ok := exp["contains"].([]any); ok {
				for _, c := range contains {
					if cm, ok := c.(map[string]any); ok {
						sys, _ := cm["system"].(string)
						code, _ := cm["code"].(string)
						disp, _ := cm["display"].(string)
						if code != "" {
							concepts = append(concepts, terminologyConcept{System: sys, Code: code, Display: disp})
						}
					}
				}
			}
		}
		if len(concepts) == 0 {
			if compose, ok := vsMap["compose"].(map[string]any); ok {
				if includes, ok := compose["include"].([]any); ok {
					for _, inc := range includes {
						incMap, ok := inc.(map[string]any)
						if !ok {
							continue
						}
						sys, _ := incMap["system"].(string)
						if rawConcepts, ok := incMap["concept"].([]any); ok && len(rawConcepts) > 0 {
							for _, rc := range rawConcepts {
								if cm, ok := rc.(map[string]any); ok {
									code, _ := cm["code"].(string)
									disp, _ := cm["display"].(string)
									if disp == "" && sys != "" {
										if lookedUp, _, ok := s.lookupCodeInSystem(ctx, tenant, "", sys, code); ok {
											disp = lookedUp.Display
										}
									}
									if code != "" {
										concepts = append(concepts, terminologyConcept{System: sys, Code: code, Display: disp})
									}
								}
							}
						} else if sys != "" {
							csConcepts, _, _ := s.resolveCodeSystemConcepts(ctx, tenant, "", sys)
							concepts = append(concepts, csConcepts...)
						}
					}
				}
			}
		}
		return resolvedURL, resolvedName, concepts, nil
	}

	// Fallback to built-in ValueSet or CodeSystem URL
	targetCS := builtInValueSetToCodeSystem[vsURL]
	if targetCS == "" {
		targetCS = vsURL
	}
	if csDef, ok := builtInCodeSystems[targetCS]; ok {
		return vsURL, csDef.Name, csDef.Concepts, nil
	}

	return "", "", nil, fmt.Errorf("ValueSet %q not found", firstNonEmpty(vsURL, vsID))
}

func (s *Server) resolveCodeSystemConcepts(ctx context.Context, tenant, csID, csURL string) ([]terminologyConcept, string, error) {
	var csMap map[string]any
	if csID != "" && s.store != nil {
		rec, err := s.store.ReadResourceWithMeta(ctx, tenant, "CodeSystem", csID)
		if err != nil {
			return nil, "", err
		}
		if jsonBytes, err := s.protoToJSONWithMeta("CodeSystem", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
			_ = json.Unmarshal(jsonBytes, &csMap)
		}
	} else if csURL != "" && s.store != nil {
		sr, err := s.store.Search(ctx, postgres.SearchParams{
			TenantID: tenant,
			ResType:  "CodeSystem",
			Tokens:   map[string]string{"url": csURL},
			Count:    1,
		})
		if err == nil && len(sr.Matches) > 0 {
			res := sr.Matches[0]
			if jsonBytes, err := s.protoToJSONWithMeta("CodeSystem", res.ResourceProto, res.ResVersion, res.LastUpdated); err == nil {
				_ = json.Unmarshal(jsonBytes, &csMap)
			}
		}
	}

	if csMap != nil {
		sysURL, _ := csMap["url"].(string)
		if sysURL == "" {
			sysURL = csURL
		}
		csName, _ := csMap["name"].(string)
		if csName == "" {
			csName, _ = csMap["title"].(string)
		}
		var concepts []terminologyConcept
		var walkConcepts func(list []any)
		walkConcepts = func(list []any) {
			for _, item := range list {
				cm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				code, _ := cm["code"].(string)
				disp, _ := cm["display"].(string)
				if code != "" {
					concepts = append(concepts, terminologyConcept{
						System:  sysURL,
						Code:    code,
						Display: disp,
					})
				}
				if children, ok := cm["concept"].([]any); ok {
					walkConcepts(children)
				}
			}
		}
		if rootConcepts, ok := csMap["concept"].([]any); ok {
			walkConcepts(rootConcepts)
		}
		return concepts, csName, nil
	}

	if csDef, ok := builtInCodeSystems[csURL]; ok {
		return csDef.Concepts, csDef.Name, nil
	}

	return nil, "", fmt.Errorf("CodeSystem %q not found", firstNonEmpty(csURL, csID))
}

func (s *Server) lookupCodeInSystem(ctx context.Context, tenant, csID, system, code string) (terminologyConcept, string, bool) {
	concepts, csName, err := s.resolveCodeSystemConcepts(ctx, tenant, csID, system)
	if err != nil {
		return terminologyConcept{}, "", false
	}
	for _, c := range concepts {
		if c.Code == code {
			return c, csName, true
		}
	}
	return terminologyConcept{}, csName, false
}

// handleValueSetExpand handles GET/POST /fhir/r4/{tenant}/ValueSet/$expand and /ValueSet/{id}/$expand.
func (s *Server) handleValueSetExpand(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	vsID := r.PathValue("id")
	params, inlineVS := extractTerminologyParams(r)
	vsURL := params["url"]

	if vsID == "" && vsURL == "" && inlineVS == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "ValueSet/$expand requires 'url' parameter, instance id, or inline ValueSet")
		return
	}

	resolvedURL, resolvedName, concepts, err := s.resolveValueSetConcepts(r.Context(), tenant, vsID, vsURL, inlineVS)
	if err != nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", err.Error())
		return
	}

	filter := strings.ToLower(strings.TrimSpace(params["filter"]))
	var filtered []terminologyConcept
	for _, c := range concepts {
		if filter != "" {
			if !strings.Contains(strings.ToLower(c.Code), filter) && !strings.Contains(strings.ToLower(c.Display), filter) {
				continue
			}
		}
		filtered = append(filtered, c)
	}

	total := len(filtered)
	offset := 0
	if v := params["offset"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	count := 100
	if v := params["count"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			count = n
		}
	}

	start := min(offset, total)
	end := min(start+count, total)
	page := filtered[start:end]

	contains := make([]map[string]any, 0, len(page))
	for _, c := range page {
		entry := map[string]any{
			"system": c.System,
			"code":   c.Code,
		}
		if c.Display != "" {
			entry["display"] = c.Display
		}
		contains = append(contains, entry)
	}

	out := map[string]any{
		"resourceType": "ValueSet",
		"status":       "active",
		"expansion": map[string]any{
			"identifier": "urn:uuid:" + uuid.NewString(),
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
			"total":      total,
			"offset":     offset,
			"contains":   contains,
		},
	}
	if vsID != "" {
		out["id"] = vsID
	}
	if resolvedURL != "" {
		out["url"] = resolvedURL
	}
	if resolvedName != "" {
		out["name"] = resolvedName
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// handleValueSetValidateCode handles GET/POST /fhir/r4/{tenant}/ValueSet/$validate-code and /ValueSet/{id}/$validate-code.
func (s *Server) handleValueSetValidateCode(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	vsID := r.PathValue("id")
	params, inlineVS := extractTerminologyParams(r)
	vsURL := params["url"]
	code := params["code"]
	system := params["system"]
	reqDisplay := params["display"]

	if code == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "ValueSet/$validate-code requires 'code' parameter")
		return
	}
	if vsID == "" && vsURL == "" && inlineVS == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "ValueSet/$validate-code requires 'url' parameter, instance id, or inline ValueSet")
		return
	}

	_, _, concepts, err := s.resolveValueSetConcepts(r.Context(), tenant, vsID, vsURL, inlineVS)
	if err != nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", err.Error())
		return
	}

	var matched *terminologyConcept
	for i := range concepts {
		c := &concepts[i]
		if c.Code == code && (system == "" || c.System == "" || c.System == system) {
			matched = c
			break
		}
	}

	result := matched != nil
	msg := ""
	if matched != nil && reqDisplay != "" && matched.Display != "" && !strings.EqualFold(reqDisplay, matched.Display) {
		result = false
		msg = fmt.Sprintf("code %q is valid in system %q, but display %q does not match expected %q", code, matched.System, reqDisplay, matched.Display)
	} else if matched == nil {
		msg = fmt.Sprintf("code %q (system %q) is not in ValueSet %q", code, system, firstNonEmpty(vsURL, vsID))
	}

	outParams := []map[string]any{
		{"name": "result", "valueBoolean": result},
		{"name": "code", "valueCode": code},
	}
	if matched != nil {
		if matched.System != "" {
			outParams = append(outParams, map[string]any{"name": "system", "valueUri": matched.System})
		}
		if matched.Display != "" {
			outParams = append(outParams, map[string]any{"name": "display", "valueString": matched.Display})
		}
	}
	if msg != "" {
		outParams = append(outParams, map[string]any{"name": "message", "valueString": msg})
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    outParams,
	})
}

// handleCodeSystemLookup handles GET/POST /fhir/r4/{tenant}/CodeSystem/$lookup and /CodeSystem/{id}/$lookup.
func (s *Server) handleCodeSystemLookup(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	csID := r.PathValue("id")
	params, _ := extractTerminologyParams(r)
	system := params["system"]
	code := params["code"]

	if code == "" || (system == "" && csID == "") {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "CodeSystem/$lookup requires 'code' and 'system' (or CodeSystem instance id)")
		return
	}

	concept, csName, ok := s.lookupCodeInSystem(r.Context(), tenant, csID, system, code)
	if !ok {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("code %q not found in CodeSystem %q", code, firstNonEmpty(system, csID)))
		return
	}

	if csName == "" {
		csName = firstNonEmpty(concept.System, system)
	}

	outParams := []map[string]any{
		{"name": "name", "valueString": csName},
		{"name": "code", "valueCode": concept.Code},
		{"name": "display", "valueString": concept.Display},
	}
	if concept.System != "" {
		outParams = append(outParams, map[string]any{"name": "system", "valueUri": concept.System})
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    outParams,
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
