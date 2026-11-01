package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/flint-fhir/flint/store/postgres"
)

type terminologyConcept struct {
	System     string            `json:"system"`
	Code       string            `json:"code"`
	Display    string            `json:"display,omitempty"`
	Parents    []string          `json:"-"`
	Properties map[string]string `json:"-"`
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
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "recurrence", Display: "Recurrence", Parents: []string{"active"}},
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "relapse", Display: "Relapse", Parents: []string{"active"}},
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "inactive", Display: "Inactive"},
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "remission", Display: "Remission", Parents: []string{"inactive"}},
			{System: "http://terminology.hl7.org/CodeSystem/condition-clinical", Code: "resolved", Display: "Resolved", Parents: []string{"inactive"}},
		},
	},
	"http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical": {
		Name: "AllergyIntoleranceClinicalStatusCodes",
		URL:  "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical",
		Concepts: []terminologyConcept{
			{System: "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", Code: "active", Display: "Active"},
			{System: "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", Code: "inactive", Display: "Inactive"},
			{System: "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", Code: "resolved", Display: "Resolved", Parents: []string{"inactive"}},
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
			{System: "http://loinc.org", Code: "8480-6", Display: "Systolic blood pressure", Parents: []string{"85354-9"}},
			{System: "http://loinc.org", Code: "8462-4", Display: "Diastolic blood pressure", Parents: []string{"85354-9"}},
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
			{System: "http://snomed.info/sct", Code: "73211009", Display: "Diabetes mellitus"},
			{System: "http://snomed.info/sct", Code: "44054006", Display: "Diabetes mellitus type 2", Parents: []string{"73211009"}},
			{System: "http://snomed.info/sct", Code: "38341003", Display: "Hypertensive disorder, systemic arterial"},
			{System: "http://snomed.info/sct", Code: "59621000", Display: "Essential hypertension", Parents: []string{"38341003"}},
			{System: "http://snomed.info/sct", Code: "71388002", Display: "Procedure"},
			{System: "http://snomed.info/sct", Code: "29303009", Display: "Electrocardiographic procedure", Parents: []string{"71388002"}},
			{System: "http://snomed.info/sct", Code: "119297000", Display: "Blood specimen"},
			{System: "http://snomed.info/sct", Code: "108252007", Display: "Laboratory procedure", Parents: []string{"71388002"}},
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
	"http://hl7.org/fhir/ValueSet/administrative-gender":            "http://hl7.org/fhir/administrative-gender",
	"http://hl7.org/fhir/ValueSet/observation-status":               "http://hl7.org/fhir/observation-status",
	"http://hl7.org/fhir/ValueSet/condition-clinical":               "http://terminology.hl7.org/CodeSystem/condition-clinical",
	"http://hl7.org/fhir/ValueSet/allergyintolerance-clinical":      "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical",
	"http://hl7.org/fhir/ValueSet/observation-category":             "http://terminology.hl7.org/CodeSystem/observation-category",
	"http://hl7.org/fhir/ValueSet/observation-vitalsignresult":      "http://loinc.org",
	"http://hl7.org/fhir/us/core/ValueSet/us-core-vital-signs":      "http://loinc.org",
	"http://hl7.org/fhir/ValueSet/condition-code":                   "http://snomed.info/sct",
	"http://hl7.org/fhir/us/core/ValueSet/us-core-condition-code":   "http://snomed.info/sct",
	"http://hl7.org/fhir/us/core/ValueSet/us-core-procedure-code":   "http://snomed.info/sct",
	"http://hl7.org/fhir/us/core/ValueSet/us-core-medication-codes": "http://www.nlm.nih.gov/research/umls/rxnorm",
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
							if name == "valueSet" || name == "conceptMap" || name == "codeSystem" {
								if resMap, ok := pm["resource"].(map[string]any); ok {
									inlineResource = resMap
								}
							}
							if bVal, ok := pm["valueBoolean"].(bool); ok {
								if bVal {
									out[name] = "true"
								} else {
									out[name] = "false"
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
							if name == "codeableConcept" {
								if ccMap, ok := pm["valueCodeableConcept"].(map[string]any); ok {
									if codings, ok := ccMap["coding"].([]any); ok && len(codings) > 0 {
										if codingMap, ok := codings[0].(map[string]any); ok {
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
							if name == "codingA" || name == "codingB" {
								if codingMap, ok := pm["valueCoding"].(map[string]any); ok {
									if sys, _ := codingMap["system"].(string); sys != "" && out["system"] == "" {
										out["system"] = sys
									}
									if code, _ := codingMap["code"].(string); code != "" {
										if name == "codingA" && out["codeA"] == "" {
											out["codeA"] = code
										} else if name == "codingB" && out["codeB"] == "" {
											out["codeB"] = code
										}
									}
								}
							}
						}
					}
				case "ValueSet", "ConceptMap", "CodeSystem":
					inlineResource = root
				}
			}
		}
	}
	return out, inlineResource
}

func (s *Server) resolveValueSetConcepts(ctx context.Context, tenant, vsID, vsURL string, inlineVS map[string]any) (string, string, []terminologyConcept, error) {
	return s.resolveValueSetConceptsWithDepth(ctx, tenant, vsID, vsURL, inlineVS, 0)
}

func (s *Server) resolveValueSetConceptsWithDepth(ctx context.Context, tenant, vsID, vsURL string, inlineVS map[string]any, depth int) (string, string, []terminologyConcept, error) {
	if depth > 5 {
		return "", "", nil, fmt.Errorf("maximum ValueSet nesting depth exceeded")
	}
	var vsMap map[string]any
	if inlineVS != nil {
		vsMap = inlineVS
	} else if vsID != "" && s.store != nil {
		rec, err := s.store.ReadResourceWithMeta(ctx, tenant, "ValueSet", vsID)
		if err != nil {
			return "", "", nil, err
		}
		if m, err := s.protoToFHIRMapWithMeta("ValueSet", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
			vsMap = m
		}
	} else if vsURL != "" && s.store != nil {
		sr, err := s.store.Search(ctx, postgres.SearchParams{
			TenantID: tenant,
			ResType:  "ValueSet",
			URIs:     map[string]string{"url": vsURL},
			Count:    1,
		})
		if err != nil || len(sr.Matches) == 0 {
			sr, err = s.store.Search(ctx, postgres.SearchParams{
				TenantID: tenant,
				ResType:  "ValueSet",
				Tokens:   map[string]string{"url": vsURL},
				Count:    1,
			})
		}
		if err == nil && len(sr.Matches) > 0 {
			res := sr.Matches[0]
			if m, err := s.protoToFHIRMapWithMeta("ValueSet", res.ResourceProto, res.ResVersion, res.LastUpdated); err == nil {
				vsMap = m
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
						concepts = append(concepts, s.evaluateComposeConceptSet(ctx, tenant, incMap, depth)...)
					}
				}
				if excludes, ok := compose["exclude"].([]any); ok && len(excludes) > 0 {
					var excluded []terminologyConcept
					for _, exc := range excludes {
						excMap, ok := exc.(map[string]any)
						if !ok {
							continue
						}
						excluded = append(excluded, s.evaluateComposeConceptSet(ctx, tenant, excMap, depth)...)
					}
					if len(excluded) > 0 {
						kept := make([]terminologyConcept, 0, len(concepts))
						for _, c := range concepts {
							isExcl := false
							for _, ex := range excluded {
								if ex.Code == c.Code && (ex.System == "" || c.System == "" || strings.EqualFold(ex.System, c.System)) {
									isExcl = true
									break
								}
							}
							if !isExcl {
								kept = append(kept, c)
							}
						}
						concepts = kept
					}
				}
			}
		}
		return resolvedURL, resolvedName, deduplicateTerminologyConcepts(concepts), nil
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

func deduplicateTerminologyConcepts(in []terminologyConcept) []terminologyConcept {
	seen := make(map[string]bool, len(in))
	out := make([]terminologyConcept, 0, len(in))
	for _, c := range in {
		if c.Code == "" {
			continue
		}
		key := strings.ToLower(c.System) + "|" + c.Code
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) evaluateComposeConceptSet(ctx context.Context, tenant string, setMap map[string]any, depth int) []terminologyConcept {
	sys, _ := setMap["system"].(string)
	var sysConcepts []terminologyConcept
	hasSysCriteria := false

	if rawConcepts, ok := setMap["concept"].([]any); ok && len(rawConcepts) > 0 {
		hasSysCriteria = true
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
					sysConcepts = append(sysConcepts, terminologyConcept{System: sys, Code: code, Display: disp})
				}
			}
		}
	} else if rawFilters, ok := setMap["filter"].([]any); ok && len(rawFilters) > 0 && sys != "" {
		hasSysCriteria = true
		csConcepts, _, _ := s.resolveCodeSystemConcepts(ctx, tenant, "", sys)
		parentsByCode := buildCodeSystemParentsMap(csConcepts)
		for _, c := range csConcepts {
			matchedAll := true
			for _, rf := range rawFilters {
				fm, ok := rf.(map[string]any)
				if !ok {
					continue
				}
				if !matchesCodeSystemFilter(c, fm, parentsByCode) {
					matchedAll = false
					break
				}
			}
			if matchedAll {
				sysConcepts = append(sysConcepts, c)
			}
		}
	} else if sys != "" {
		hasSysCriteria = true
		csConcepts, _, _ := s.resolveCodeSystemConcepts(ctx, tenant, "", sys)
		sysConcepts = append(sysConcepts, csConcepts...)
	}

	var vsConcepts []terminologyConcept
	hasVSCriteria := false
	if rawVSList, ok := setMap["valueSet"].([]any); ok && len(rawVSList) > 0 {
		for idx, rawVS := range rawVSList {
			vsCanon, _ := rawVS.(string)
			vsCanon = strings.TrimSpace(vsCanon)
			if vsCanon == "" {
				continue
			}
			_, _, nested, err := s.resolveValueSetConceptsWithDepth(ctx, tenant, "", vsCanon, nil, depth+1)
			if err != nil {
				continue
			}
			if !hasVSCriteria || idx == 0 {
				vsConcepts = nested
				hasVSCriteria = true
			} else {
				vsConcepts = intersectTerminologyConcepts(vsConcepts, nested)
			}
		}
	}

	if hasSysCriteria && hasVSCriteria {
		return intersectTerminologyConcepts(sysConcepts, vsConcepts)
	}
	if hasSysCriteria {
		return sysConcepts
	}
	return vsConcepts
}

func intersectTerminologyConcepts(a, b []terminologyConcept) []terminologyConcept {
	var out []terminologyConcept
	for _, ca := range a {
		for _, cb := range b {
			if ca.Code == cb.Code && (ca.System == "" || cb.System == "" || strings.EqualFold(ca.System, cb.System)) {
				merged := ca
				if merged.Display == "" {
					merged.Display = cb.Display
				}
				if merged.System == "" {
					merged.System = cb.System
				}
				out = append(out, merged)
				break
			}
		}
	}
	return out
}

func buildCodeSystemParentsMap(concepts []terminologyConcept) map[string][]string {
	parents := make(map[string][]string, len(concepts))
	for _, c := range concepts {
		if len(c.Parents) > 0 {
			parents[c.Code] = append(parents[c.Code], c.Parents...)
		}
	}
	return parents
}

// codeIsAncestorOrEqual returns true if ancestorCode == code (when allowEqual is true)
// or if ancestorCode is a direct or transitive parent of code in parentsByCode.
func codeIsAncestorOrEqual(ancestorCode, code string, parentsByCode map[string][]string, allowEqual bool) bool {
	if ancestorCode == "" || code == "" {
		return false
	}
	if ancestorCode == code {
		return allowEqual
	}
	visited := map[string]bool{code: true}
	queue := append([]string(nil), parentsByCode[code]...)
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if curr == ancestorCode {
			return true
		}
		if visited[curr] {
			continue
		}
		visited[curr] = true
		queue = append(queue, parentsByCode[curr]...)
	}
	return false
}

func matchesCodeSystemFilter(c terminologyConcept, fm map[string]any, parentsByCode map[string][]string) bool {
	prop, _ := fm["property"].(string)
	op, _ := fm["op"].(string)
	val, _ := fm["value"].(string)
	prop = strings.TrimSpace(prop)
	op = strings.ToLower(strings.TrimSpace(op))

	targetVal := c.Code
	if prop != "" && !strings.EqualFold(prop, "code") && !strings.EqualFold(prop, "concept") {
		if strings.EqualFold(prop, "display") {
			targetVal = c.Display
		} else if c.Properties != nil {
			targetVal = c.Properties[prop]
		} else {
			targetVal = ""
		}
	}

	switch op {
	case "=":
		return strings.EqualFold(targetVal, val) || c.Code == val
	case "in":
		for part := range strings.SplitSeq(val, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" && (strings.EqualFold(targetVal, trimmed) || c.Code == trimmed) {
				return true
			}
		}
		return false
	case "not-in":
		for part := range strings.SplitSeq(val, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" && (strings.EqualFold(targetVal, trimmed) || c.Code == trimmed) {
				return false
			}
		}
		return true
	case "is-a":
		return codeIsAncestorOrEqual(val, c.Code, parentsByCode, true)
	case "descendent-of", "descendant-of":
		return codeIsAncestorOrEqual(val, c.Code, parentsByCode, false)
	case "is-not-a":
		return !codeIsAncestorOrEqual(val, c.Code, parentsByCode, true)
	case "regex":
		re, err := regexp.Compile(val)
		if err != nil {
			return false
		}
		return re.MatchString(targetVal)
	case "exists":
		wantExists := strings.EqualFold(strings.TrimSpace(val), "true")
		hasVal := strings.TrimSpace(targetVal) != ""
		return wantExists == hasVal
	default:
		return false
	}
}

func (s *Server) resolveCodeSystemConcepts(ctx context.Context, tenant, csID, csURL string) ([]terminologyConcept, string, error) {
	return s.resolveCodeSystemConceptsWithInline(ctx, tenant, csID, csURL, nil)
}

func (s *Server) resolveCodeSystemConceptsWithInline(ctx context.Context, tenant, csID, csURL string, inlineCS map[string]any) ([]terminologyConcept, string, error) {
	var csMap map[string]any
	if inlineCS != nil && inlineCS["resourceType"] == "CodeSystem" {
		csMap = inlineCS
	} else if csID != "" && s.store != nil {
		rec, err := s.store.ReadResourceWithMeta(ctx, tenant, "CodeSystem", csID)
		if err != nil {
			return nil, "", err
		}
		if m, err := s.protoToFHIRMapWithMeta("CodeSystem", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
			csMap = m
		}
	} else if csURL != "" && s.store != nil {
		sr, err := s.store.Search(ctx, postgres.SearchParams{
			TenantID: tenant,
			ResType:  "CodeSystem",
			URIs:     map[string]string{"url": csURL},
			Count:    1,
		})
		if err != nil || len(sr.Matches) == 0 {
			sr, err = s.store.Search(ctx, postgres.SearchParams{
				TenantID: tenant,
				ResType:  "CodeSystem",
				Tokens:   map[string]string{"url": csURL},
				Count:    1,
			})
		}
		if err == nil && len(sr.Matches) > 0 {
			res := sr.Matches[0]
			if m, err := s.protoToFHIRMapWithMeta("CodeSystem", res.ResourceProto, res.ResVersion, res.LastUpdated); err == nil {
				csMap = m
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
		childToParents := make(map[string][]string)
		var walkConcepts func(list []any, parentCode string)
		walkConcepts = func(list []any, parentCode string) {
			for _, item := range list {
				cm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				code, _ := cm["code"].(string)
				disp, _ := cm["display"].(string)
				var parents []string
				if parentCode != "" {
					parents = append(parents, parentCode)
				}
				var props map[string]string
				if rawProps, ok := cm["property"].([]any); ok {
					for _, rp := range rawProps {
						pm, ok := rp.(map[string]any)
						if !ok {
							continue
						}
						pCode, _ := pm["code"].(string)
						pVal := ""
						for _, vk := range []string{"valueCode", "valueString", "valueUri"} {
							if sVal, ok := pm[vk].(string); ok && sVal != "" {
								pVal = sVal
								break
							}
						}
						if pVal == "" {
							if bVal, ok := pm["valueBoolean"].(bool); ok {
								pVal = strconv.FormatBool(bVal)
							} else if iVal, ok := pm["valueInteger"].(float64); ok {
								pVal = strconv.Itoa(int(iVal))
							}
						}
						if pCode != "" && pVal != "" {
							if props == nil {
								props = make(map[string]string)
							}
							props[pCode] = pVal
							switch pCode {
							case "parent", "subsumedBy":
								parents = append(parents, pVal)
							case "child":
								if code != "" {
									childToParents[pVal] = append(childToParents[pVal], code)
								}
							}
						}
					}
				}
				if code != "" {
					concepts = append(concepts, terminologyConcept{
						System:     sysURL,
						Code:       code,
						Display:    disp,
						Parents:    parents,
						Properties: props,
					})
				}
				if children, ok := cm["concept"].([]any); ok {
					walkConcepts(children, code)
				}
			}
		}
		if rootConcepts, ok := csMap["concept"].([]any); ok {
			walkConcepts(rootConcepts, "")
		}
		if len(childToParents) > 0 {
			for i := range concepts {
				if extra, ok := childToParents[concepts[i].Code]; ok {
					concepts[i].Parents = append(concepts[i].Parents, extra...)
				}
			}
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

// handleCodeSystemValidateCode handles GET/POST /fhir/r4/{tenant}/CodeSystem/$validate-code and /CodeSystem/{id}/$validate-code
// per HL7 FHIR R4 OperationDefinition/CodeSystem-validate-code.
func (s *Server) handleCodeSystemValidateCode(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	csID := r.PathValue("id")
	params, inlineRes := extractTerminologyParams(r)
	systemURL := firstNonEmpty(params["url"], params["system"])
	code := params["code"]
	reqDisplay := params["display"]

	if code == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "CodeSystem/$validate-code requires 'code' (or 'coding'/'codeableConcept') parameter")
		return
	}
	if csID == "" && systemURL == "" && inlineRes == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "CodeSystem/$validate-code requires 'url' (or 'system') parameter, instance id, or inline CodeSystem")
		return
	}

	csConcepts, _, err := s.resolveCodeSystemConceptsWithInline(r.Context(), tenant, csID, systemURL, inlineRes)
	if err != nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("CodeSystem %q not found", firstNonEmpty(systemURL, csID)))
		return
	}

	var matched *terminologyConcept
	for i := range csConcepts {
		c := &csConcepts[i]
		if c.Code == code {
			matched = c
			break
		}
	}

	result := matched != nil
	msg := ""
	if matched != nil && reqDisplay != "" && matched.Display != "" && !strings.EqualFold(reqDisplay, matched.Display) {
		result = false
		msg = fmt.Sprintf("code %q is valid in CodeSystem %q, but display %q does not match expected %q", code, firstNonEmpty(matched.System, systemURL, csID), reqDisplay, matched.Display)
	} else if matched == nil {
		msg = fmt.Sprintf("code %q is not in CodeSystem %q", code, firstNonEmpty(systemURL, csID))
	}

	outParams := []map[string]any{
		{"name": "result", "valueBoolean": result},
		{"name": "code", "valueCode": code},
	}
	if matched != nil {
		if sys := firstNonEmpty(matched.System, systemURL); sys != "" {
			outParams = append(outParams, map[string]any{"name": "system", "valueUri": sys})
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

// handleCodeSystemSubsumes handles GET/POST /fhir/r4/{tenant}/CodeSystem/$subsumes and /CodeSystem/{id}/$subsumes
// per HL7 FHIR R4 OperationDefinition/CodeSystem-subsumes.
func (s *Server) handleCodeSystemSubsumes(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	csID := r.PathValue("id")
	params, inlineRes := extractTerminologyParams(r)
	systemURL := firstNonEmpty(params["system"], params["url"])
	codeA := params["codeA"]
	codeB := params["codeB"]

	if codeA == "" || codeB == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "CodeSystem/$subsumes requires 'codeA' and 'codeB' (or 'codingA' and 'codingB') parameters")
		return
	}
	if csID == "" && systemURL == "" && inlineRes == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "CodeSystem/$subsumes requires 'system' parameter or CodeSystem instance id")
		return
	}

	csConcepts, _, err := s.resolveCodeSystemConceptsWithInline(r.Context(), tenant, csID, systemURL, inlineRes)
	if err != nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("CodeSystem %q not found", firstNonEmpty(systemURL, csID)))
		return
	}

	var foundA, foundB bool
	for _, c := range csConcepts {
		if c.Code == codeA {
			foundA = true
		}
		if c.Code == codeB {
			foundB = true
		}
	}
	if !foundA || !foundB {
		missing := codeA
		if foundA {
			missing = codeB
		}
		writeOperationOutcome(w, http.StatusBadRequest, "code-invalid",
			fmt.Sprintf("code %q is not valid in CodeSystem %q", missing, firstNonEmpty(systemURL, csID)))
		return
	}

	parentsMap := buildCodeSystemParentsMap(csConcepts)
	outcome := "not-subsumed"
	if strings.EqualFold(codeA, codeB) {
		outcome = "equivalent"
	} else if codeIsAncestorOrEqual(codeA, codeB, parentsMap, false) {
		outcome = "subsumes"
	} else if codeIsAncestorOrEqual(codeB, codeA, parentsMap, false) {
		outcome = "subsumed-by"
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "outcome", "valueCode": outcome},
		},
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

type conceptMapEntry struct {
	SourceSystem string
	SourceCode   string
	SourceDisp   string
	TargetSystem string
	TargetCode   string
	TargetDisp   string
	Equivalence  string
	MapURL       string
}

var builtInConceptMaps = []conceptMapEntry{
	{
		SourceSystem: "http://snomed.info/sct",
		SourceCode:   "38341003",
		SourceDisp:   "Hypertensive disorder, systemic arterial",
		TargetSystem: "http://hl7.org/fhir/sid/icd-10-cm",
		TargetCode:   "I10",
		TargetDisp:   "Essential (primary) hypertension",
		Equivalence:  "equivalent",
		MapURL:       "http://hl7.org/fhir/ConceptMap/cm-snomed-to-icd10",
	},
	{
		SourceSystem: "http://snomed.info/sct",
		SourceCode:   "59621000",
		SourceDisp:   "Essential hypertension",
		TargetSystem: "http://hl7.org/fhir/sid/icd-10-cm",
		TargetCode:   "I10",
		TargetDisp:   "Essential (primary) hypertension",
		Equivalence:  "equivalent",
		MapURL:       "http://hl7.org/fhir/ConceptMap/cm-snomed-to-icd10",
	},
	{
		SourceSystem: "http://snomed.info/sct",
		SourceCode:   "44054006",
		SourceDisp:   "Diabetes mellitus type 2",
		TargetSystem: "http://hl7.org/fhir/sid/icd-10-cm",
		TargetCode:   "E11.9",
		TargetDisp:   "Type 2 diabetes mellitus without complications",
		Equivalence:  "equivalent",
		MapURL:       "http://hl7.org/fhir/ConceptMap/cm-snomed-to-icd10",
	},
	{
		SourceSystem: "http://snomed.info/sct",
		SourceCode:   "73211009",
		SourceDisp:   "Diabetes mellitus",
		TargetSystem: "http://hl7.org/fhir/sid/icd-10-cm",
		TargetCode:   "E11.9",
		TargetDisp:   "Type 2 diabetes mellitus without complications",
		Equivalence:  "wider",
		MapURL:       "http://hl7.org/fhir/ConceptMap/cm-snomed-to-icd10",
	},
	{
		SourceSystem: "http://snomed.info/sct",
		SourceCode:   "195967001",
		SourceDisp:   "Asthma",
		TargetSystem: "http://hl7.org/fhir/sid/icd-10-cm",
		TargetCode:   "J45.909",
		TargetDisp:   "Unspecified asthma, uncomplicated",
		Equivalence:  "equivalent",
		MapURL:       "http://hl7.org/fhir/ConceptMap/cm-snomed-to-icd10",
	},
	{
		SourceSystem: "http://loinc.org",
		SourceCode:   "8867-4",
		SourceDisp:   "Heart rate",
		TargetSystem: "http://snomed.info/sct",
		TargetCode:   "364075005",
		TargetDisp:   "Heart rate",
		Equivalence:  "equivalent",
		MapURL:       "http://hl7.org/fhir/ConceptMap/cm-loinc-to-snomed",
	},
	{
		SourceSystem: "http://loinc.org",
		SourceCode:   "85354-9",
		SourceDisp:   "Blood pressure panel with all children optional",
		TargetSystem: "http://snomed.info/sct",
		TargetCode:   "75367002",
		TargetDisp:   "Blood pressure",
		Equivalence:  "equivalent",
		MapURL:       "http://hl7.org/fhir/ConceptMap/cm-loinc-to-snomed",
	},
	{
		SourceSystem: "http://loinc.org",
		SourceCode:   "4548-4",
		SourceDisp:   "Hemoglobin A1c/Hemoglobin.total in Blood",
		TargetSystem: "http://snomed.info/sct",
		TargetCode:   "43396009",
		TargetDisp:   "Hemoglobin A1c measurement",
		Equivalence:  "equivalent",
		MapURL:       "http://hl7.org/fhir/ConceptMap/cm-loinc-to-snomed",
	},
}

func extractEntriesFromConceptMapJSON(cmMap map[string]any) []conceptMapEntry {
	var out []conceptMapEntry
	mapURL, _ := cmMap["url"].(string)
	groups, _ := cmMap["group"].([]any)
	for _, rawG := range groups {
		gm, ok := rawG.(map[string]any)
		if !ok {
			continue
		}
		srcSys, _ := gm["source"].(string)
		tgtSys, _ := gm["target"].(string)
		elements, _ := gm["element"].([]any)
		for _, rawEl := range elements {
			elm, ok := rawEl.(map[string]any)
			if !ok {
				continue
			}
			srcCode, _ := elm["code"].(string)
			srcDisp, _ := elm["display"].(string)
			targets, _ := elm["target"].([]any)
			for _, rawTgt := range targets {
				tm, ok := rawTgt.(map[string]any)
				if !ok {
					continue
				}
				tgtCode, _ := tm["code"].(string)
				tgtDisp, _ := tm["display"].(string)
				eq, _ := tm["equivalence"].(string)
				if eq == "" {
					eq = "equivalent"
				}
				if srcCode != "" && tgtCode != "" {
					out = append(out, conceptMapEntry{
						SourceSystem: srcSys,
						SourceCode:   srcCode,
						SourceDisp:   srcDisp,
						TargetSystem: tgtSys,
						TargetCode:   tgtCode,
						TargetDisp:   tgtDisp,
						Equivalence:  eq,
						MapURL:       mapURL,
					})
				}
			}
		}
	}
	return out
}

// handleConceptMapTranslate handles GET/POST /fhir/r4/{tenant}/ConceptMap/$translate and /ConceptMap/{id}/$translate
// per HL7 FHIR R4 OperationDefinition/ConceptMap-translate.
func (s *Server) handleConceptMapTranslate(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	cmID := r.PathValue("id")
	params, inlineCM := extractTerminologyParams(r)

	cmURL := params["url"]
	code := params["code"]
	system := params["system"]
	targetSystem := firstNonEmpty(params["targetsystem"], params["targetSystem"])
	reverse := strings.EqualFold(params["reverse"], "true")

	if code == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "ConceptMap/$translate requires 'code' (or 'coding') parameter")
		return
	}

	var candidateEntries []conceptMapEntry
	foundExplicitMap := false

	if inlineCM != nil {
		candidateEntries = append(candidateEntries, extractEntriesFromConceptMapJSON(inlineCM)...)
		foundExplicitMap = true
	} else if cmID != "" && s.store != nil {
		rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "ConceptMap", cmID)
		if err != nil {
			writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("ConceptMap/%s not found", cmID))
			return
		}
		if cmMap, err := s.protoToFHIRMapWithMeta("ConceptMap", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
			candidateEntries = append(candidateEntries, extractEntriesFromConceptMapJSON(cmMap)...)
			foundExplicitMap = true
		}
	} else if cmURL != "" && s.store != nil {
		sr, err := s.store.Search(r.Context(), postgres.SearchParams{
			TenantID: tenant,
			ResType:  "ConceptMap",
			URIs:     map[string]string{"url": cmURL},
			Count:    10,
		})
		if err != nil || len(sr.Matches) == 0 {
			sr, err = s.store.Search(r.Context(), postgres.SearchParams{
				TenantID: tenant,
				ResType:  "ConceptMap",
				Tokens:   map[string]string{"url": cmURL},
				Count:    10,
			})
		}
		if err == nil && len(sr.Matches) > 0 {
			for _, m := range sr.Matches {
				if cmMap, err := s.protoToFHIRMapWithMeta("ConceptMap", m.ResourceProto, m.ResVersion, m.LastUpdated); err == nil {
					candidateEntries = append(candidateEntries, extractEntriesFromConceptMapJSON(cmMap)...)
					foundExplicitMap = true
				}
			}
		}
	}

	if !foundExplicitMap {
		// Also include any stored ConceptMaps in the tenant when translating by system/targetsystem,
		// pre-filtering via spidx_uri (source-uri / target-uri) when available.
		if cmURL == "" && s.store != nil {
			uriFilters := map[string]string{}
			if system != "" {
				if reverse {
					uriFilters["target-uri"] = system
				} else {
					uriFilters["source-uri"] = system
				}
			}
			if targetSystem != "" {
				if reverse {
					uriFilters["source-uri"] = targetSystem
				} else {
					uriFilters["target-uri"] = targetSystem
				}
			}
			var sr *postgres.SearchResults
			var err error
			if len(uriFilters) > 0 {
				sr, err = s.store.Search(r.Context(), postgres.SearchParams{
					TenantID: tenant,
					ResType:  "ConceptMap",
					URIs:     uriFilters,
					Count:    100,
				})
			}
			if sr == nil || err != nil || len(sr.Matches) == 0 {
				sr, err = s.store.Search(r.Context(), postgres.SearchParams{
					TenantID: tenant,
					ResType:  "ConceptMap",
					Count:    100,
				})
			}
			if err == nil && sr != nil {
				for _, m := range sr.Matches {
					if cmMap, err := s.protoToFHIRMapWithMeta("ConceptMap", m.ResourceProto, m.ResVersion, m.LastUpdated); err == nil {
						candidateEntries = append(candidateEntries, extractEntriesFromConceptMapJSON(cmMap)...)
					}
				}
			}
		}
		for _, b := range builtInConceptMaps {
			if cmURL == "" || strings.EqualFold(b.MapURL, cmURL) {
				candidateEntries = append(candidateEntries, b)
			}
		}
	}

	var matchParts []map[string]any
	for _, entry := range candidateEntries {
		srcSys, srcCode := entry.SourceSystem, entry.SourceCode
		tgtSys, tgtCode, tgtDisp := entry.TargetSystem, entry.TargetCode, entry.TargetDisp
		if reverse {
			srcSys, srcCode = entry.TargetSystem, entry.TargetCode
			tgtSys, tgtCode, tgtDisp = entry.SourceSystem, entry.SourceCode, entry.SourceDisp
		}
		if !strings.EqualFold(srcCode, code) {
			continue
		}
		if system != "" && srcSys != "" && !strings.EqualFold(srcSys, system) {
			continue
		}
		if targetSystem != "" && tgtSys != "" && !strings.EqualFold(tgtSys, targetSystem) {
			continue
		}
		conceptCoding := map[string]any{
			"code": tgtCode,
		}
		if tgtSys != "" {
			conceptCoding["system"] = tgtSys
		}
		if tgtDisp != "" {
			conceptCoding["display"] = tgtDisp
		}
		parts := []map[string]any{
			{"name": "equivalence", "valueCode": entry.Equivalence},
			{"name": "concept", "valueCoding": conceptCoding},
		}
		if entry.MapURL != "" {
			parts = append(parts, map[string]any{"name": "source", "valueUri": entry.MapURL})
		}
		matchParts = append(matchParts, map[string]any{
			"name": "match",
			"part": parts,
		})
	}

	hasResult := len(matchParts) > 0
	msg := fmt.Sprintf("No translation found for code %q", code)
	if hasResult {
		msg = fmt.Sprintf("%d match(es) found", len(matchParts))
	}
	outParams := []map[string]any{
		{"name": "result", "valueBoolean": hasResult},
		{"name": "message", "valueString": msg},
	}
	outParams = append(outParams, matchParts...)

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    outParams,
	})
}

// ExpandTerminologySearchParams resolves terminology-aware token search modifiers
// (:in, :not-in, :below, :above) across direct and chained token search parameters,
// rewriting them into spidx_token-compatible filters.
func (s *Server) ExpandTerminologySearchParams(ctx context.Context, tenant string, params *postgres.SearchParams) error {
	return s.expandTerminologySearchParams(ctx, tenant, params)
}

// expandTerminologySearchParams resolves terminology-aware token search modifiers
// (:in, :not-in, :below, :above) across direct and chained token search parameters,
// rewriting them into spidx_token-compatible filters.
func (s *Server) expandTerminologySearchParams(ctx context.Context, tenant string, params *postgres.SearchParams) error {
	if params == nil {
		return nil
	}

	if len(params.TokenFilters) > 0 {
		newFilters := make(map[string][]string, len(params.TokenFilters))
		newTokens := make(map[string]string, len(params.Tokens))
		for spKey, filterVals := range params.TokenFilters {
			baseName := spKey
			mod := ""
			if colonIdx := strings.IndexByte(spKey, ':'); colonIdx != -1 {
				baseName = spKey[:colonIdx]
				mod = strings.ToLower(strings.TrimSpace(spKey[colonIdx+1:]))
			}
			switch mod {
			case "in", "not-in", "below", "above":
				for _, rawVal := range filterVals {
					rewrittenKey, rewrittenVal, err := s.expandSingleTerminologyTokenValue(ctx, tenant, baseName, mod, rawVal)
					if err != nil {
						return err
					}
					if rewrittenKey != "" && rewrittenVal != "" {
						newFilters[rewrittenKey] = append(newFilters[rewrittenKey], rewrittenVal)
						if _, exists := newTokens[rewrittenKey]; !exists {
							newTokens[rewrittenKey] = rewrittenVal
						}
					}
				}
			default:
				newFilters[spKey] = append(newFilters[spKey], filterVals...)
				if v, ok := params.Tokens[spKey]; ok {
					newTokens[spKey] = v
				} else if len(filterVals) > 0 {
					newTokens[spKey] = filterVals[0]
				}
			}
		}
		params.TokenFilters = newFilters
		params.Tokens = newTokens
	}

	for i := range params.Chained {
		ch := &params.Chained[i]
		mod := strings.ToLower(strings.TrimSpace(ch.TargetModifier))
		if ch.ParamType == "token" && (mod == "in" || mod == "not-in" || mod == "below" || mod == "above") {
			rewrittenKey, rewrittenVal, err := s.expandSingleTerminologyTokenValue(ctx, tenant, ch.TargetParam, mod, ch.Value)
			if err != nil {
				return err
			}
			if strings.HasSuffix(rewrittenKey, ":not") {
				ch.TargetModifier = "not"
			} else {
				ch.TargetModifier = ""
			}
			ch.Value = rewrittenVal
		}
	}

	for i := range params.Filters {
		if err := s.expandFilterExprTerminology(ctx, tenant, &params.Filters[i]); err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) expandFilterExprTerminology(ctx context.Context, tenant string, expr *postgres.FilterExpr) error {
	if expr == nil {
		return nil
	}
	switch expr.Kind {
	case "and", "or", "not":
		for i := range expr.Children {
			if err := s.expandFilterExprTerminology(ctx, tenant, &expr.Children[i]); err != nil {
				return err
			}
		}
	case "param":
		op := strings.ToLower(strings.TrimSpace(expr.Op))
		var mod string
		switch op {
		case "in":
			mod = "in"
		case "ni":
			mod = "not-in"
		case "ss":
			mod = "above"
		case "sb":
			mod = "below"
		default:
			return nil
		}
		_, rewrittenVal, err := s.expandSingleTerminologyTokenValue(ctx, tenant, expr.Param, mod, expr.Value)
		if err != nil {
			return err
		}
		if op == "ni" {
			expr.Op = "ne"
		} else {
			expr.Op = "eq"
		}
		expr.Value = rewrittenVal
	}
	return nil
}

func (s *Server) expandSingleTerminologyTokenValue(ctx context.Context, tenant, baseName, modifier, rawVal string) (string, string, error) {
	switch modifier {
	case "in", "not-in":
		var tokens []string
		seen := make(map[string]bool)
		addTok := func(t string) {
			if t != "" && !seen[t] {
				seen[t] = true
				tokens = append(tokens, t)
			}
		}
		for part := range strings.SplitSeq(rawVal, ",") {
			vsRef := strings.TrimSpace(part)
			if vsRef == "" {
				continue
			}
			vsID := ""
			vsURL := ""
			if strings.HasPrefix(vsRef, "http://") || strings.HasPrefix(vsRef, "https://") || strings.HasPrefix(vsRef, "urn:") {
				vsURL = vsRef
			} else if strings.HasPrefix(vsRef, "ValueSet/") {
				vsID = strings.TrimPrefix(vsRef, "ValueSet/")
			} else {
				vsID = vsRef
				vsURL = vsRef
			}
			_, _, concepts, err := s.resolveValueSetConcepts(ctx, tenant, vsID, vsURL, nil)
			if err != nil && vsID != "" && vsURL != "" {
				_, _, concepts, err = s.resolveValueSetConcepts(ctx, tenant, "", vsURL, nil)
			}
			if err != nil {
				return "", "", fmt.Errorf("invalid :%s ValueSet %q: %w", modifier, vsRef, err)
			}
			for _, c := range concepts {
				if c.Code == "" {
					continue
				}
				if c.System != "" {
					addTok(c.System + "|" + c.Code)
				}
				if c.System == "" || baseName == "gender" || baseName == "status" || baseName == "clinical-status" || baseName == "verification-status" || baseName == "category" {
					addTok(c.Code)
				}
			}
		}
		if modifier == "in" {
			if len(tokens) == 0 {
				return baseName, "__empty_valueset__|__none__", nil
			}
			return baseName, strings.Join(tokens, ","), nil
		}
		if len(tokens) == 0 {
			return "", "", nil
		}
		return baseName + ":not", strings.Join(tokens, ","), nil

	case "below", "above":
		var tokens []string
		seen := make(map[string]bool)
		addTok := func(t string) {
			if t != "" && !seen[t] {
				seen[t] = true
				tokens = append(tokens, t)
			}
		}
		for part := range strings.SplitSeq(rawVal, ",") {
			item := strings.TrimSpace(part)
			if item == "" {
				continue
			}
			addTok(item)
			var sys, code string
			if p := strings.SplitN(item, "|", 2); len(p) == 2 {
				sys = strings.TrimSpace(p[0])
				code = strings.TrimSpace(p[1])
			} else {
				code = item
			}
			if sys == "" {
				switch baseName {
				case "clinical-status":
					sys = "http://terminology.hl7.org/CodeSystem/condition-clinical"
				case "gender":
					sys = "http://hl7.org/fhir/administrative-gender"
				case "status":
					sys = "http://hl7.org/fhir/observation-status"
				}
			}
			if sys != "" && code != "" {
				if concepts, _, err := s.resolveCodeSystemConcepts(ctx, tenant, "", sys); err == nil && len(concepts) > 0 {
					parentsByCode := make(map[string][]string, len(concepts))
					for _, c := range concepts {
						if len(c.Parents) > 0 {
							parentsByCode[c.Code] = c.Parents
						}
					}
					for _, c := range concepts {
						matched := false
						if modifier == "below" {
							matched = codeIsAncestorOrEqual(code, c.Code, parentsByCode, true)
						} else {
							matched = codeIsAncestorOrEqual(c.Code, code, parentsByCode, true)
						}
						if matched {
							if c.System != "" {
								addTok(c.System + "|" + c.Code)
							}
							if !strings.Contains(item, "|") || baseName == "clinical-status" || baseName == "status" || baseName == "gender" {
								addTok(c.Code)
							}
						}
					}
				}
			}
		}
		return baseName, strings.Join(tokens, ","), nil
	}
	return baseName, rawVal, nil
}

type closureConceptEntry struct {
	System         string
	Code           string
	Display        string
	AddedAtVersion int
}

type closureTableState struct {
	Version  int
	Concepts []closureConceptEntry
}

var (
	closureTablesMu sync.Mutex
	closureTables   = map[string]*closureTableState{}
)

// handleClosure handles POST /fhir/r4/{tenant}/$closure per HL7 FHIR R4 OperationDefinition/ConceptMap-closure (§4.8.15.6).
// Maintains a stateful transitive concept subsumption closure table identified by 'name',
// accepting new 'concept' Codings and/or a 'version' resynchronization parameter and returning a ConceptMap.
func (s *Server) handleClosure(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = "default"
	}
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	reqVersion := strings.TrimSpace(q.Get("version"))
	var inputConcepts []terminologyConcept

	for _, rawC := range q["concept"] {
		for part := range strings.SplitSeq(rawC, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			if p := strings.SplitN(trimmed, "|", 2); len(p) == 2 {
				inputConcepts = append(inputConcepts, terminologyConcept{
					System: strings.TrimSpace(p[0]),
					Code:   strings.TrimSpace(p[1]),
				})
			} else {
				inputConcepts = append(inputConcepts, terminologyConcept{
					System: "http://snomed.info/sct",
					Code:   trimmed,
				})
			}
		}
	}

	if r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read $closure request body")
			return
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid JSON in $closure request: %v", err))
				return
			}
			if rt, _ := raw["resourceType"].(string); rt != "" && rt != "Parameters" {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("$closure body must be a Parameters resource (got %q)", rt))
				return
			}
			if paramsArr, ok := raw["parameter"].([]any); ok {
				for _, rawP := range paramsArr {
					pm, ok := rawP.(map[string]any)
					if !ok {
						continue
					}
					pName, _ := pm["name"].(string)
					switch pName {
					case "name":
						if name == "" {
							for _, k := range []string{"valueString", "valueCode", "valueId"} {
								if v, ok := pm[k].(string); ok && strings.TrimSpace(v) != "" {
									name = strings.TrimSpace(v)
									break
								}
							}
						}
					case "version":
						if reqVersion == "" {
							for _, k := range []string{"valueId", "valueString", "valueCode"} {
								if v, ok := pm[k].(string); ok && strings.TrimSpace(v) != "" {
									reqVersion = strings.TrimSpace(v)
									break
								}
							}
						}
					case "concept":
						if cm, ok := pm["valueCoding"].(map[string]any); ok {
							sys, _ := cm["system"].(string)
							code, _ := cm["code"].(string)
							disp, _ := cm["display"].(string)
							if strings.TrimSpace(code) != "" {
								if strings.TrimSpace(sys) == "" {
									sys = "http://snomed.info/sct"
								}
								inputConcepts = append(inputConcepts, terminologyConcept{
									System:  strings.TrimSpace(sys),
									Code:    strings.TrimSpace(code),
									Display: strings.TrimSpace(disp),
								})
							}
						} else if sVal, ok := pm["valueString"].(string); ok && strings.TrimSpace(sVal) != "" {
							if p := strings.SplitN(strings.TrimSpace(sVal), "|", 2); len(p) == 2 {
								inputConcepts = append(inputConcepts, terminologyConcept{
									System: strings.TrimSpace(p[0]),
									Code:   strings.TrimSpace(p[1]),
								})
							}
						}
					}
				}
			}
		}
	}

	if name == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "$closure requires 'name' parameter")
		return
	}

	tableKey := tenant + "/" + name
	closureTablesMu.Lock()
	state, exists := closureTables[tableKey]
	if !exists {
		state = &closureTableState{Version: 1}
		closureTables[tableKey] = state
	}

	minVersionFilter := 0
	if reqVersion != "" {
		vNum, err := strconv.Atoi(reqVersion)
		if err != nil || vNum < 0 {
			closureTablesMu.Unlock()
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid $closure version %q", reqVersion))
			return
		}
		if vNum == 0 {
			// Re-initialize / full resync from version 1
			minVersionFilter = 1
		} else {
			minVersionFilter = vNum + 1
		}
	} else if len(inputConcepts) > 0 {
		state.Version++
		minVersionFilter = state.Version
	}

	curVersion := state.Version
	for _, ic := range inputConcepts {
		alreadyPresent := false
		for i := range state.Concepts {
			if strings.EqualFold(state.Concepts[i].System, ic.System) && strings.EqualFold(state.Concepts[i].Code, ic.Code) {
				alreadyPresent = true
				if reqVersion == "" {
					// Include pairs involving re-submitted concepts in this delta response
					state.Concepts[i].AddedAtVersion = curVersion
				}
				break
			}
		}
		if !alreadyPresent {
			state.Concepts = append(state.Concepts, closureConceptEntry{
				System:         ic.System,
				Code:           ic.Code,
				Display:        ic.Display,
				AddedAtVersion: curVersion,
			})
		}
	}

	snapshot := make([]closureConceptEntry, len(state.Concepts))
	copy(snapshot, state.Concepts)
	closureTablesMu.Unlock()

	// Group snapshot concepts by system and compute subsumption pairs
	bySystem := make(map[string][]closureConceptEntry)
	var systemOrder []string
	for _, c := range snapshot {
		if _, seen := bySystem[c.System]; !seen {
			systemOrder = append(systemOrder, c.System)
		}
		bySystem[c.System] = append(bySystem[c.System], c)
	}

	groups := make([]map[string]any, 0, len(systemOrder))
	for _, sys := range systemOrder {
		entries := bySystem[sys]
		if len(entries) == 0 || minVersionFilter == 0 {
			continue
		}
		csConcepts, _, err := s.resolveCodeSystemConcepts(r.Context(), tenant, "", sys)
		if err != nil || len(csConcepts) == 0 {
			continue
		}
		parentsMap := buildCodeSystemParentsMap(csConcepts)

		var elements []map[string]any
		for i := range entries {
			src := entries[i]
			var targets []map[string]any
			for j := range entries {
				if i == j {
					continue
				}
				tgt := entries[j]
				if src.AddedAtVersion < minVersionFilter && tgt.AddedAtVersion < minVersionFilter {
					continue
				}
				if strings.EqualFold(src.Code, tgt.Code) {
					targets = append(targets, map[string]any{
						"code":        tgt.Code,
						"equivalence": "equal",
					})
				} else if codeIsAncestorOrEqual(tgt.Code, src.Code, parentsMap, false) {
					// tgt is an ancestor of src -> tgt subsumes src
					targets = append(targets, map[string]any{
						"code":        tgt.Code,
						"equivalence": "subsumes",
					})
				} else if codeIsAncestorOrEqual(src.Code, tgt.Code, parentsMap, false) {
					// src is an ancestor of tgt -> src specializes / subsumes tgt
					targets = append(targets, map[string]any{
						"code":        tgt.Code,
						"equivalence": "specializes",
					})
				}
			}
			if len(targets) > 0 {
				el := map[string]any{
					"code":   src.Code,
					"target": targets,
				}
				if src.Display != "" {
					el["display"] = src.Display
				}
				elements = append(elements, el)
			}
		}
		if len(elements) > 0 {
			groups = append(groups, map[string]any{
				"source":  sys,
				"target":  sys,
				"element": elements,
			})
		}
	}

	out := map[string]any{
		"resourceType": "ConceptMap",
		"id":           name,
		"url":          fmt.Sprintf("http://flint.fhir.org/ConceptMap/%s", name),
		"version":      strconv.Itoa(curVersion),
		"name":         name,
		"status":       "active",
		"experimental": true,
		"date":         time.Now().UTC().Format(time.RFC3339),
		"group":        groups,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}
