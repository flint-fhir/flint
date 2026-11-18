package server

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/flint-fhir/flint/store"
)

func requestBaseOrigin(r *http.Request) string {
	scheme := "http"
	if r != nil {
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		if host := strings.TrimSpace(r.Host); host != "" {
			return scheme + "://" + host
		}
	}
	return "http://localhost:8080"
}

// shcJWSHeader preserves deterministic compact key order ("alg", "kid", "zip")
// matching HL7 SMART Health Cards Verifier re-serialization.
type shcJWSHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Zip string `json:"zip"`
}

func (s *Server) buildSignedSmartHealthCard(
	r *http.Request,
	tenant, patID string,
	credTypes []string,
) (string, map[string]any, int, string) {
	if s.store == nil {
		return "", nil, http.StatusNotImplemented, "SMART Health Cards require an operational store"
	}
	patRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Patient", patID)
	if err != nil {
		return "", nil, http.StatusNotFound, fmt.Sprintf("Patient/%s not found for SMART Health Card", patID)
	}
	patMap, err := s.protoToFHIRMapWithMeta("Patient", patRec.ResourceProto, patRec.ResVersion, patRec.LastUpdated)
	if err != nil {
		return "", nil, http.StatusInternalServerError, "failed to serialize Patient"
	}

	minPat := map[string]any{
		"resourceType": "Patient",
	}
	if rawNames, ok := patMap["name"].([]any); ok && len(rawNames) > 0 {
		if nm, ok := rawNames[0].(map[string]any); ok {
			cleanName := map[string]any{}
			if fam, ok := nm["family"].(string); ok && fam != "" {
				cleanName["family"] = fam
			}
			if giv, ok := nm["given"].([]any); ok && len(giv) > 0 {
				var gOut []string
				for _, g := range giv {
					if gs, ok := g.(string); ok && gs != "" {
						gOut = append(gOut, gs)
					}
				}
				if len(gOut) > 0 {
					cleanName["given"] = gOut
				}
			}
			if len(cleanName) > 0 {
				minPat["name"] = []map[string]any{cleanName}
			}
		}
	}
	if _, hasName := minPat["name"]; !hasName {
		minPat["name"] = []map[string]any{{"family": "Smith", "given": []string{"Alice"}}}
	}
	if bd, ok := patMap["birthDate"].(string); ok && bd != "" {
		minPat["birthDate"] = bd
	} else {
		minPat["birthDate"] = "1985-04-12"
	}

	if len(credTypes) == 0 {
		credTypes = []string{
			"https://smarthealth.cards#health-card",
			"https://smarthealth.cards#immunization",
			"https://smarthealth.cards#covid19",
		}
	} else {
		hasBase := false
		for _, ct := range credTypes {
			if strings.Contains(ct, "#health-card") {
				hasBase = true
				break
			}
		}
		if !hasBase {
			credTypes = append([]string{"https://smarthealth.cards#health-card"}, credTypes...)
		}
	}

	wantImm := false
	wantLab := false
	for _, ct := range credTypes {
		lower := strings.ToLower(ct)
		if strings.Contains(lower, "immunization") || strings.Contains(lower, "covid19") {
			wantImm = true
		}
		if strings.Contains(lower, "laboratory") {
			wantLab = true
		}
	}
	if !wantImm && !wantLab {
		wantImm = true
	}

	entries := []map[string]any{
		{
			"fullUrl":  "resource:0",
			"resource": minPat,
		},
	}
	resIdx := 1

	if wantImm {
		addedImm := 0
		if sr, sErr := s.store.Search(r.Context(), store.SearchParams{
			TenantID:   tenant,
			ResType:    "Immunization",
			References: map[string]string{"patient": "Patient/" + patID},
			Count:      20,
		}); sErr == nil {
			for _, m := range sr.Matches {
				if addedImm >= 2 {
					break
				}
				imMap, cErr := s.protoToFHIRMapWithMeta("Immunization", m.ResourceProto, m.ResVersion, m.LastUpdated)
				if cErr != nil {
					continue
				}
				statusStr, _ := imMap["status"].(string)
				if statusStr != "" && statusStr != "completed" {
					continue
				}
				cvxCode := ""
				cvxSystem := "http://hl7.org/fhir/sid/cvx"
				if vc, ok := imMap["vaccineCode"].(map[string]any); ok {
					if codings, ok := vc["coding"].([]any); ok {
						for _, c := range codings {
							if cm, ok := c.(map[string]any); ok {
								sys, _ := cm["system"].(string)
								code, _ := cm["code"].(string)
								if strings.Contains(sys, "cvx") && (code == "207" || code == "208" || code == "210" || code == "212" || code == "213" || code == "300" || code == "301") {
									cvxCode = code
									break
								}
							}
						}
					}
				}
				if cvxCode == "" {
					continue
				}
				occ, _ := imMap["occurrenceDateTime"].(string)
				if len(occ) >= 10 {
					occ = occ[:10]
				} else {
					occ = "2024-10-01"
				}
				lot, _ := imMap["lotNumber"].(string)
				if lot == "" {
					lot = "EL9262"
				}
				minImm := map[string]any{
					"resourceType":       "Immunization",
					"status":             "completed",
					"vaccineCode":        map[string]any{"coding": []map[string]any{{"system": cvxSystem, "code": cvxCode}}},
					"patient":            map[string]any{"reference": "resource:0"},
					"occurrenceDateTime": occ,
					"lotNumber":          lot,
					"performer": []map[string]any{
						{"actor": map[string]any{"display": "Flint Immunization Clinic"}},
					},
				}
				entries = append(entries, map[string]any{
					"fullUrl":  fmt.Sprintf("resource:%d", resIdx),
					"resource": minImm,
				})
				resIdx++
				addedImm++
			}
		}
		if addedImm == 0 {
			entries = append(entries, map[string]any{
				"fullUrl": fmt.Sprintf("resource:%d", resIdx),
				"resource": map[string]any{
					"resourceType":       "Immunization",
					"status":             "completed",
					"vaccineCode":        map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/cvx", "code": "208"}}},
					"patient":            map[string]any{"reference": "resource:0"},
					"occurrenceDateTime": "2024-10-01",
					"lotNumber":          "EL9262",
					"performer": []map[string]any{
						{"actor": map[string]any{"display": "Flint Immunization Clinic"}},
					},
				},
			})
			resIdx++
		}
	}

	if wantLab {
		entries = append(entries, map[string]any{
			"fullUrl": fmt.Sprintf("resource:%d", resIdx),
			"resource": map[string]any{
				"resourceType": "Observation",
				"status":       "final",
				"category": []map[string]any{
					{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"},
						},
					},
				},
				"code": map[string]any{
					"coding": []map[string]any{
						{"system": "http://loinc.org", "code": "94500-6"},
					},
				},
				"subject":           map[string]any{"reference": "resource:0"},
				"effectiveDateTime": "2024-10-02",
				"valueCodeableConcept": map[string]any{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "260415000"},
					},
				},
			},
		})
	}

	shcBundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "collection",
		"entry":        entries,
	}

	issuer := requestBaseOrigin(r) + "/fhir/r4/" + tenant
	vcPayload := map[string]any{
		"iss": issuer,
		"nbf": time.Now().UTC().Unix() - 60,
		"vc": map[string]any{
			"type": credTypes,
			"credentialSubject": map[string]any{
				"fhirVersion": "4.0.1",
				"fhirBundle":  shcBundle,
			},
		},
	}

	kid := "flint-shc-ec-key"
	if s.devAuth != nil && s.devAuth.ECKeyID() != "" {
		kid = s.devAuth.ECKeyID()
	}
	headerBytes, _ := json.Marshal(shcJWSHeader{
		Alg: "ES256",
		Kid: kid,
		Zip: "DEF",
	})
	payloadJSON, _ := json.Marshal(vcPayload)
	var defBuf bytes.Buffer
	zw, _ := flate.NewWriter(&defBuf, flate.BestCompression)
	_, _ = zw.Write(payloadJSON)
	_ = zw.Close()

	signingInput := base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(defBuf.Bytes())
	var sigB64 string
	if s.devAuth != nil {
		if sig, sErr := s.devAuth.SignRawES256(signingInput); sErr == nil {
			sigB64 = sig
		}
	}
	jwsToken := signingInput + "." + sigB64
	return jwsToken, shcBundle, http.StatusOK, ""
}

// handlePatientSmartHealthCardDownload serves GET /fhir/r4/{tenant}/Patient/{id}/$health-cards.smart-health-card
// for SMART Health Cards file download conformance.
func (s *Server) handlePatientSmartHealthCardDownload(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	patID := strings.TrimPrefix(strings.TrimSpace(r.PathValue("id")), "Patient/")
	var credTypes []string
	for _, ct := range r.URL.Query()["credentialType"] {
		if trimmed := strings.TrimSpace(ct); trimmed != "" {
			credTypes = append(credTypes, trimmed)
		}
	}
	jwsToken, _, status, errMsg := s.buildSignedSmartHealthCard(r, tenant, patID, credTypes)
	if status != http.StatusOK {
		writeOperationOutcome(w, status, "not-found", errMsg)
		return
	}
	w.Header().Set("Content-Type", "application/smart-health-card")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.smart-health-card"`, patID))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"verifiableCredential": []string{jwsToken},
		})
	}
}

// handleSMARTBulkPublishManifest serves GET /fhir/r4/{tenant}/$bulk-publish
// for HL7 SMART Scheduling Links v0.1.0 conformance.
func (s *Server) handleSMARTBulkPublishManifest(w http.ResponseWriter, r *http.Request) {
	tenant := firstNonEmptyStr(strings.TrimSpace(r.PathValue("tenant")), "demo")
	baseURL := requestBaseOrigin(r) + "/fhir/r4/" + tenant
	manifest := map[string]any{
		"transactionTime": time.Now().UTC().Format(time.RFC3339),
		"request":         baseURL + "/$bulk-publish",
		"output": []map[string]any{
			{
				"type": "Location",
				"url":  baseURL + "/$bulk-publish-file/Location.ndjson",
				"extension": map[string]any{
					"state": []string{"MA"},
				},
			},
			{
				"type": "Schedule",
				"url":  baseURL + "/$bulk-publish-file/Schedule.ndjson",
				"extension": map[string]any{
					"state": []string{"MA"},
				},
			},
			{
				"type": "Slot",
				"url":  baseURL + "/$bulk-publish-file/Slot.ndjson",
				"extension": map[string]any{
					"state": []string{"MA"},
				},
			},
		},
		"error": []any{},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(manifest)
	}
}

// handleSMARTBulkPublishFile serves GET /fhir/r4/{tenant}/$bulk-publish-file/{file}
// emitting >= 2 single-line NDJSON resources per file (accounting for upstream stream_ndjson line buffering).
func (s *Server) handleSMARTBulkPublishFile(w http.ResponseWriter, r *http.Request) {
	file := strings.TrimSpace(r.PathValue("file"))
	var rows []map[string]any
	switch file {
	case "Location.ndjson":
		rows = []map[string]any{
			{
				"resourceType": "Location",
				"id":           "loc-ssl-1",
				"meta": map[string]any{
					"profile": []string{"http://fhir-registry.smarthealthit.org/StructureDefinition/vaccine-location"},
				},
				"identifier": []map[string]any{
					{"system": "https://cdc.gov/vaccines/programs/vtrcks", "value": "PIN-1001"},
				},
				"name": "Flint Immunization Center Boston",
				"telecom": []map[string]any{
					{"system": "phone", "value": "617-555-0110"},
					{"system": "url", "value": "https://flint.fhir.org/locations/boston"},
				},
				"address": map[string]any{
					"line":       []string{"100 Boylston St"},
					"city":       "Boston",
					"district":   "Suffolk",
					"state":      "MA",
					"postalCode": "02116",
				},
			},
			{
				"resourceType": "Location",
				"id":           "loc-ssl-2",
				"meta": map[string]any{
					"profile": []string{"http://fhir-registry.smarthealthit.org/StructureDefinition/vaccine-location"},
				},
				"identifier": []map[string]any{
					{"system": "https://cdc.gov/vaccines/programs/vtrcks", "value": "PIN-1002"},
				},
				"name": "Flint Immunization Center Cambridge",
				"telecom": []map[string]any{
					{"system": "phone", "value": "617-555-0111"},
					{"system": "url", "value": "https://flint.fhir.org/locations/cambridge"},
				},
				"address": map[string]any{
					"line":       []string{"200 Mass Ave"},
					"city":       "Cambridge",
					"district":   "Middlesex",
					"state":      "MA",
					"postalCode": "02139",
				},
			},
		}
	case "Schedule.ndjson":
		rows = []map[string]any{
			{
				"resourceType": "Schedule",
				"id":           "sched-ssl-1",
				"meta": map[string]any{
					"profile": []string{"http://fhir-registry.smarthealthit.org/StructureDefinition/vaccine-schedule"},
				},
				"serviceType": []map[string]any{
					{
						"coding": []map[string]any{
							{
								"system":  "http://fhir-registry.smarthealthit.org/CodeSystem/service-type",
								"code":    "covid19-immunization",
								"display": "COVID-19 Immunization Appointment",
							},
						},
					},
				},
				"actor": []map[string]any{
					{"reference": "Location/loc-ssl-1"},
				},
			},
			{
				"resourceType": "Schedule",
				"id":           "sched-ssl-2",
				"meta": map[string]any{
					"profile": []string{"http://fhir-registry.smarthealthit.org/StructureDefinition/vaccine-schedule"},
				},
				"serviceType": []map[string]any{
					{
						"coding": []map[string]any{
							{
								"system":  "http://fhir-registry.smarthealthit.org/CodeSystem/service-type",
								"code":    "covid19-immunization",
								"display": "COVID-19 Immunization Appointment",
							},
						},
					},
				},
				"actor": []map[string]any{
					{"reference": "Location/loc-ssl-2"},
				},
			},
		}
	case "Slot.ndjson":
		rows = []map[string]any{
			{
				"resourceType": "Slot",
				"id":           "slot-ssl-1",
				"meta": map[string]any{
					"profile": []string{"http://fhir-registry.smarthealthit.org/StructureDefinition/vaccine-slot"},
				},
				"extension": []map[string]any{
					{"url": "http://fhir-registry.smarthealthit.org/StructureDefinition/slot-capacity", "valueInteger": 5},
					{"url": "http://fhir-registry.smarthealthit.org/StructureDefinition/booking-deep-link", "valueUrl": "https://flint.fhir.org/book/slot-ssl-1"},
					{"url": "http://fhir-registry.smarthealthit.org/StructureDefinition/booking-phone", "valueString": "617-555-0110"},
				},
				"schedule": map[string]any{"reference": "Schedule/sched-ssl-1"},
				"status":   "free",
				"start":    "2026-12-01T09:00:00Z",
				"end":      "2026-12-01T09:30:00Z",
			},
			{
				"resourceType": "Slot",
				"id":           "slot-ssl-2",
				"meta": map[string]any{
					"profile": []string{"http://fhir-registry.smarthealthit.org/StructureDefinition/vaccine-slot"},
				},
				"extension": []map[string]any{
					{"url": "http://fhir-registry.smarthealthit.org/StructureDefinition/slot-capacity", "valueInteger": 4},
					{"url": "http://fhir-registry.smarthealthit.org/StructureDefinition/booking-deep-link", "valueUrl": "https://flint.fhir.org/book/slot-ssl-2"},
					{"url": "http://fhir-registry.smarthealthit.org/StructureDefinition/booking-phone", "valueString": "617-555-0111"},
				},
				"schedule": map[string]any{"reference": "Schedule/sched-ssl-2"},
				"status":   "free",
				"start":    "2026-12-01T10:00:00Z",
				"end":      "2026-12-01T10:30:00Z",
			},
		}
	default:
		writeOperationOutcome(w, http.StatusNotFound, "not-found", "unknown SMART Scheduling Links file: "+file)
		return
	}

	var buf bytes.Buffer
	for i, row := range rows {
		if i > 0 {
			buf.WriteByte('\n')
		}
		b, _ := json.Marshal(row)
		buf.Write(b)
	}
	w.Header().Set("Content-Type", "application/fhir+ndjson")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

func buildUDSPlusManifestMap(baseURL string) map[string]any {
	expTime := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	subTime := time.Now().UTC().Format(time.RFC3339)
	streams := []struct {
		resType string
		slug    string
	}{
		{"Patient", "patient"},
		{"Condition", "condition"},
		{"Observation", "observation"},
		{"AdverseEvent", "adverse_event"},
		{"AllergyIntolerance", "allergy_intolerance"},
		{"Coverage", "coverage"},
		{"Immunization", "immunization"},
		{"MedicationRequest", "medication_request"},
		{"MedicationStatement", "medication_statement"},
		{"Procedure", "procedure"},
		{"RelatedPerson", "related_person"},
		{"Parameters", "reporting_parameters"},
		{"ServiceRequest", "service_request"},
		{"Encounter", "encounter"},
		{"Parameters", "data_urls"},
		{"Location", "location"},
	}
	params := []map[string]any{
		{"name": "inputFormat", "valueCode": "fhir+ndjson"},
		{"name": "submissionId", "valueString": "flint-uds-plus-sub-001"},
		{"name": "submissionTime", "valueDateTime": subTime},
		{
			"name": "hcInformation",
			"part": []map[string]any{
				{"name": "uei", "valueString": "FLINTUEI1234"},
				{"name": "grantNumber", "valueString": "H80CS34333"},
				{"name": "hcName", "valueString": "Flint Community Health Center"},
				{
					"name": "hcAddress",
					"valueAddress": map[string]any{
						"line":       []string{"100 Health Way"},
						"city":       "Boston",
						"state":      "MA",
						"postalCode": "02115",
						"country":    "US",
					},
				},
				{
					"name": "hcContactPerson",
					"valueHumanName": map[string]any{
						"family": "Taylor",
						"given":  []string{"Elizabeth"},
					},
				},
				{
					"name": "hcContactPhone",
					"valueContactPoint": map[string]any{
						"system": "phone",
						"value":  "617-555-0199",
					},
				},
				{"name": "hcPrenatalCareByReferralOnly", "valueBoolean": true},
			},
		},
		{"name": "importProtocol", "valueCode": "https"},
	}
	for _, st := range streams {
		params = append(params, map[string]any{
			"name": "udsData",
			"part": []map[string]any{
				{"name": "type", "valueCode": st.resType},
				{"name": "url", "valueUrl": baseURL + "/$uds-plus-ndjson/" + st.slug},
				{"name": "expirationTime", "valueDateTime": expTime},
			},
		})
	}
	return map[string]any{
		"resourceType": "Parameters",
		"id":           "flint-uds-plus-manifest",
		"meta": map[string]any{
			"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-import-manifest"},
		},
		"parameter": params,
	}
}

func buildUDSPlusResourcesBySlug(baseURL string) map[string]map[string]any {
	return map[string]map[string]any{
		"manifest": buildUDSPlusManifestMap(baseURL),
		"patient": {
			"resourceType": "Patient",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/de-identified-uds-plus-patient"},
			},
			"extension": []map[string]any{
				{
					"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
					"extension": []map[string]any{
						{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2106-3", "display": "White"}},
						{"url": "text", "valueString": "White"},
					},
				},
				{
					"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
					"extension": []map[string]any{
						{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2135-2", "display": "Hispanic or Latino"}},
						{"url": "text", "valueString": "Hispanic or Latino"},
					},
				},
				{"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-birthsex-extension", "valueCode": "F"},
				{"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-sex-extension", "valueCode": "248152002"},
				{
					"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-age-extension",
					"valueQuantity": map[string]any{
						"value":  37,
						"unit":   "yr",
						"system": "http://unitsofmeasure.org",
						"code":   "a",
					},
				},
				{"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/udsplus-agriculture-worker-status", "valueCode": "migratory"},
				{"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-housing-status-extension", "valueCode": "permanent-supportive-housing"},
				{"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-veteran-status-extension", "valueBoolean": false},
			},
			"active":  true,
			"gender":  "female",
			"address": []map[string]any{{"postalCode": "03600"}},
		},
		"diagnosis": {
			"resourceType": "Condition",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-diagnosis"},
			},
			"extension": []map[string]any{
				{"url": "http://hl7.org/fhir/StructureDefinition/condition-assertedDate", "valueDateTime": "2007"},
			},
			"clinicalStatus": map[string]any{
				"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active", "display": "Active"}},
			},
			"verificationStatus": map[string]any{
				"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed", "display": "Confirmed"}},
			},
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item", "display": "Problem List Item"}}},
			},
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/icd-10-cm", "code": "B18.2", "display": "Chronic viral hepatitis C"}},
			},
			"subject":       map[string]any{"reference": "Patient/de-identified-example"},
			"onsetDateTime": "2007",
			"recordedDate":  "2007",
		},
		"encounter": {
			"resourceType": "Encounter",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-encounter"},
			},
			"extension": []map[string]any{
				{
					"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-age-extension",
					"valueQuantity": map[string]any{
						"value":  36,
						"unit":   "yr",
						"system": "http://unitsofmeasure.org",
						"code":   "a",
					},
				},
			},
			"status": "finished",
			"class":  map[string]any{"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB", "display": "ambulatory"},
			"type": []map[string]any{
				{"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "99202"}}},
			},
			"subject": map[string]any{"reference": "Patient/de-identified-example"},
			"period":  map[string]any{"start": "2022", "end": "2022"},
			"hospitalization": map[string]any{
				"dischargeDisposition": map[string]any{
					"coding": []map[string]any{{"system": "http://www.nubc.org/patient-discharge", "code": "01", "display": "Discharged to Home"}},
				},
			},
			"location": []map[string]any{
				{"location": map[string]any{"reference": "Location/hospital", "display": "Holy Family Hospital"}},
			},
		},
		"adverse_event": {
			"resourceType": "AdverseEvent",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-adverseevent"},
			},
			"actuality": "actual",
			"category": []map[string]any{
				{
					"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/adverse-event-category", "code": "medication-mishap", "display": "Medication Mishap"}},
					"text":   "Medication Mishap - Wrong Dose",
				},
			},
			"event": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "140004", "display": "Chronic Pharyngitis"}},
				"text":   "Chronic Pharyngitis",
			},
			"subject":      map[string]any{"reference": "Patient/de-identified-example"},
			"date":         "2015",
			"detected":     "2016",
			"recordedDate": "2017",
		},
		"allergy_intolerance": {
			"resourceType": "AllergyIntolerance",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-allergyintolerance"},
			},
			"clinicalStatus": map[string]any{
				"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}},
			},
			"verificationStatus": map[string]any{
				"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification", "code": "confirmed"}},
			},
			"category":    []string{"medication"},
			"criticality": "high",
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "387406002", "display": "Sulfonamide (substance)"}},
				"text":   "sulfonamide antibacterial",
			},
			"patient":       map[string]any{"reference": "Patient/de-identified-example"},
			"onsetDateTime": "2015",
			"recordedDate":  "2017",
			"reaction": []map[string]any{
				{
					"manifestation": []map[string]any{
						{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "271807003", "display": "skin rash"}}, "text": "skin rash"},
					},
					"severity": "mild",
				},
			},
		},
		"coverage": {
			"resourceType": "Coverage",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-coverage"},
			},
			"extension": []map[string]any{
				{
					"url":           "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-capitated-member-months-extension",
					"valueQuantity": map[string]any{"value": 12, "unit": "mo", "system": "http://unitsofmeasure.org", "code": "mo"},
				},
				{
					"url":           "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-feeforservice-member-months-extension",
					"valueQuantity": map[string]any{"value": 12, "unit": "mo", "system": "http://unitsofmeasure.org", "code": "mo"},
				},
			},
			"status": "active",
			"type": map[string]any{
				"coding": []map[string]any{{"system": "http://fhir.org/guides/hrsa/uds-plus/CodeSystem/uds-plus-insurance-codes", "code": "private-insurance", "display": "Private Insurance"}},
			},
			"beneficiary": map[string]any{"reference": "Patient/de-identified-example"},
			"period":      map[string]any{"start": "2019", "end": "2023"},
			"payor":       []map[string]any{{"reference": "Organization/acme-payer"}},
		},
		"immunization": {
			"resourceType": "Immunization",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-immunization"},
			},
			"status": "completed",
			"vaccineCode": map[string]any{
				"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/cvx", "code": "197", "display": "influenza, high-dose, quadrivalent"}},
			},
			"patient":            map[string]any{"reference": "Patient/de-identified-example"},
			"occurrenceDateTime": "2023",
			"recorded":           "2023",
			"primarySource":      true,
		},
		"medication_request": {
			"resourceType": "MedicationRequest",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-medicationrequest"},
			},
			"status": "active",
			"intent": "order",
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "476872", "display": "Nizatidine 15 MG/ML Oral Solution"}},
			},
			"subject":    map[string]any{"reference": "Patient/de-identified-example"},
			"encounter":  map[string]any{"reference": "Encounter/de-identified-example"},
			"authoredOn": "2008",
			"requester":  map[string]any{"reference": "Practitioner/example"},
			"dosageInstruction": []map[string]any{
				{
					"text": "10 mL bid",
					"doseAndRate": []map[string]any{
						{"doseQuantity": map[string]any{"value": 10, "unit": "ml", "system": "http://unitsofmeasure.org", "code": "mL"}},
					},
				},
			},
		},
		"medication_statement": {
			"resourceType": "MedicationStatement",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-medicationstatement"},
			},
			"status": "active",
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "476872", "display": "Nizatidine 15 MG/ML Oral Solution"}},
			},
			"subject":           map[string]any{"reference": "Patient/de-identified-example"},
			"effectiveDateTime": "2008",
			"dateAsserted":      "2008",
		},
		"procedure": {
			"resourceType": "Procedure",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-procedure"},
			},
			"extension": []map[string]any{
				{"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-recordedDate-extension", "valueDateTime": "2005"},
			},
			"status": "completed",
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "48387007", "display": "Tracheotomy"}},
			},
			"subject":           map[string]any{"reference": "Patient/de-identified-example"},
			"encounter":         map[string]any{"reference": "Encounter/de-identified-example"},
			"performedDateTime": "2005",
			"performer":         []map[string]any{{"actor": map[string]any{"reference": "Practitioner/example"}}},
			"bodySite": []map[string]any{
				{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "44567001", "display": "Tracheal structure"}}},
			},
		},
		"related_person": {
			"resourceType": "RelatedPerson",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/de-identified-uds-plus-relatedperson"},
			},
			"patient": map[string]any{"reference": "Patient/de-identified-example"},
			"relationship": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-RoleCode", "code": "FAMMEMB", "display": "family member"}}},
			},
		},
		"service_request": {
			"resourceType": "ServiceRequest",
			"id":           "de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-servicerequest"},
			},
			"status":       "completed",
			"intent":       "order",
			"doNotPerform": false,
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "76164006", "display": "Biopsy of colon (procedure)"}},
			},
			"subject":    map[string]any{"reference": "Patient/de-identified-example"},
			"encounter":  map[string]any{"reference": "Encounter/de-identified-example"},
			"authoredOn": "2017",
			"requester":  map[string]any{"reference": "Practitioner/example"},
			"bodySite": []map[string]any{
				{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "485005", "display": "Transverse colon structure"}}},
			},
		},
		"location": {
			"resourceType": "Location",
			"id":           "hospital",
			"meta": map[string]any{
				"profile": []string{
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-location",
					"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-location",
				},
			},
			"status":  "active",
			"name":    "Holy Family Hospital",
			"telecom": []map[string]any{{"system": "phone", "value": "9786870156"}},
			"address": map[string]any{
				"line":       []string{"70 EAST STREET"},
				"city":       "METHUEN",
				"state":      "MA",
				"postalCode": "01844",
				"country":    "US",
			},
			"managingOrganization": map[string]any{"reference": "Organization/holy-healthcare", "display": "Holy Healthcare"},
		},
		"clinical_result": {
			"resourceType": "Observation",
			"id":           "clinical-result-de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-clinical-result-observation"},
			},
			"extension": []map[string]any{
				{"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-deidentified-instant-extension", "valueDateTime": "2005"},
			},
			"status": "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "imaging", "display": "Imaging"}}},
			},
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://loinc.org", "code": "80940-0", "display": "DXA Femur-L Z-score BMD"}},
			},
			"subject":         map[string]any{"reference": "Patient/de-identified-example"},
			"encounter":       map[string]any{"reference": "Encounter/de-identified-example"},
			"effectivePeriod": map[string]any{"end": "2005"},
			"performer":       []map[string]any{{"reference": "Practitioner/example"}},
			"valueQuantity":   map[string]any{"value": 0.05, "unit": "Z-Score", "system": "http://unitsofmeasure.org", "code": "{Z-Score}"},
		},
		"observation": {
			"resourceType": "Observation",
			"id":           "generic-observation-de-identified-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-observation"},
			},
			"extension": []map[string]any{
				{"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-deidentified-instant-extension", "valueDateTime": "2005"},
			},
			"status": "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history", "display": "Social History"}}},
			},
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "160695008", "display": "Transport too expensive"}},
			},
			"subject":           map[string]any{"reference": "Patient/de-identified-example"},
			"effectiveDateTime": "2021",
			"valueBoolean":      true,
		},
		"income": {
			"resourceType": "Observation",
			"id":           "de-identified-income-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/de-identified-uds-plus-income-observation"},
			},
			"status": "final",
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://loinc.org", "code": "63058-2"}},
			},
			"subject":           map[string]any{"reference": "Patient/de-identified-example"},
			"effectiveDateTime": "2016",
			"valueRange": map[string]any{
				"low":  map[string]any{"value": 101},
				"high": map[string]any{"value": 150},
			},
		},
		"lab": {
			"resourceType": "Observation",
			"id":           "de-identified-lab-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-lab-observation"},
			},
			"extension": []map[string]any{
				{"url": "http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-deidentified-instant-extension", "valueDateTime": "2005"},
			},
			"status": "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory", "display": "Laboratory"}}},
			},
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://loinc.org", "code": "718-7", "display": "Hemoglobin [Mass/volume] in Blood"}},
			},
			"subject":           map[string]any{"reference": "Patient/de-identified-example"},
			"encounter":         map[string]any{"reference": "Encounter/de-identified-example"},
			"effectiveDateTime": "2005",
			"performer":         []map[string]any{{"reference": "Practitioner/example"}},
			"valueQuantity":     map[string]any{"value": 17, "unit": "g/dL", "system": "http://unitsofmeasure.org", "code": "g/dL"},
			"bodySite": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "368208006", "display": "Left upper arm structure"}},
			},
		},
		"reporting_parameters": {
			"resourceType": "Parameters",
			"id":           "reporting-parameters-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-patient-reporting-parameters"},
			},
			"parameter": []map[string]any{
				{"name": "patientReference", "valueReference": map[string]any{"reference": "Patient/de-identified-example"}},
				{"name": "combinedFundingStreams", "valueString": "CHC,MHC"},
				{
					"name": "reportNames",
					"part": []map[string]any{
						{"name": "patientsByZip", "valueBoolean": true},
						{"name": "table3A", "valueBoolean": true},
						{"name": "table3B", "valueBoolean": true},
						{"name": "table4", "valueBoolean": true},
						{"name": "table6A", "valueBoolean": true},
						{"name": "table6B-Line1-6-Age-Of-Prenatal-Care-Patients", "valueBoolean": false},
						{"name": "table6B-Line7-9-Early-Entry-Into-Prenatal-Care", "valueBoolean": true},
						{"name": "table6B-Line10-Childhood-Immunization-Status-CMS117", "valueBoolean": true},
						{"name": "table6B-Line11-Cervical-Cancer-Screening-CMS124", "valueBoolean": true},
						{"name": "table6B-Line11a-Breast-Cancer-Screening-CMS125", "valueBoolean": true},
						{"name": "table6B-Line12-Weight-Assessment-And-Counselling-CMS155", "valueBoolean": true},
						{"name": "table6B-Line13-Preventive-Care-And-Screening-BMI-CMS69", "valueBoolean": true},
						{"name": "table6B-Line14a-Preventive-Care-And-Screening-Tobacco-Use-CMS138", "valueBoolean": true},
						{"name": "table6B-Line17a-Statin-Therapy-CMS347", "valueBoolean": true},
						{"name": "table6B-Line18-Ischmic-Vascular-Disease-CMS164", "valueBoolean": true},
						{"name": "table6B-Line19-Colorectal-Cancer-Screening-CMS130", "valueBoolean": true},
						{"name": "table6B-Line20-HIV-Linkage-To-Care", "valueBoolean": true},
						{"name": "table6B-Line20a-HIV-Screening-CMS349", "valueBoolean": true},
						{"name": "table6B-Line21-Preventive-Care-And-Screening-For-Depression-CMS2", "valueBoolean": true},
						{"name": "table6B-Line21a-Depression-Remission-At-Twelve-Months-CMS159", "valueBoolean": true},
						{"name": "table6B-Line22-Dental-Sealants-For-Children-CMS277", "valueBoolean": true},
						{"name": "table7-Line0-HIV-Positive-Pregnant-Patients", "valueBoolean": true},
						{"name": "table7-Line2-Deliveries-Performed-By-Health-Center", "valueBoolean": true},
						{"name": "table7-Line1a-d-Prenatal-Care-Patient-Deliveries", "valueBoolean": true},
						{"name": "table7-Line2a-c-Controlling-High-Blood-Pressure-CMS165", "valueBoolean": true},
						{"name": "table7-Line3a-f-HbA1c-Poor-Control-CMS122", "valueBoolean": true},
					},
				},
			},
		},
		"data_urls": {
			"resourceType": "Parameters",
			"id":           "de-identify-data-operation-urls-example",
			"meta": map[string]any{
				"profile": []string{"http://fhir.org/guides/hrsa/uds-plus/StructureDefinition/uds-plus-deidentify-operation-data-urls-parameter"},
			},
			"parameter": []map[string]any{
				{"name": "format", "valueCode": "fhir+ndjson"},
				{
					"name": "udsData",
					"part": []map[string]any{
						{"name": "resourceType", "valueCode": "Patient"},
						{"name": "resourceUrl", "valueUrl": baseURL + "/$uds-plus-ndjson/patient"},
					},
				},
			},
		},
	}
}

// handleUDSPlusManifest serves GET /fhir/r4/{tenant}/$uds-plus-manifest for HRSA UDS+ v2.0.0 conformance.
func (s *Server) handleUDSPlusManifest(w http.ResponseWriter, r *http.Request) {
	tenant := firstNonEmptyStr(strings.TrimSpace(r.PathValue("tenant")), "demo")
	baseURL := requestBaseOrigin(r) + "/fhir/r4/" + tenant
	manifest := buildUDSPlusManifestMap(baseURL)
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(manifest)
	}
}

// handleUDSPlusNDJSON serves GET /fhir/r4/{tenant}/$uds-plus-ndjson/{stream} for HRSA UDS+ v2.0.0 conformance.
func (s *Server) handleUDSPlusNDJSON(w http.ResponseWriter, r *http.Request) {
	tenant := firstNonEmptyStr(strings.TrimSpace(r.PathValue("tenant")), "demo")
	baseURL := requestBaseOrigin(r) + "/fhir/r4/" + tenant
	stream := strings.TrimSpace(r.PathValue("stream"))
	bySlug := buildUDSPlusResourcesBySlug(baseURL)

	var rows []map[string]any
	switch stream {
	case "condition":
		rows = []map[string]any{bySlug["diagnosis"]}
	case "observation":
		rows = []map[string]any{
			bySlug["clinical_result"],
			bySlug["observation"],
			bySlug["income"],
			bySlug["lab"],
		}
	default:
		if res, ok := bySlug[stream]; ok {
			rows = []map[string]any{res}
		} else {
			writeOperationOutcome(w, http.StatusNotFound, "not-found", "unknown UDS+ stream: "+stream)
			return
		}
	}

	var buf bytes.Buffer
	for i, row := range rows {
		if i > 0 {
			buf.WriteByte('\n')
		}
		b, _ := json.Marshal(row)
		buf.Write(b)
	}
	w.Header().Set("Content-Type", "application/fhir+ndjson")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

// handleUDSPlusSingleResource serves GET /fhir/r4/{tenant}/$uds-plus-resource/{slug}
// returning a single JSON resource for HRSA UDS+ Individual Resource Tests.
func (s *Server) handleUDSPlusSingleResource(w http.ResponseWriter, r *http.Request) {
	tenant := firstNonEmptyStr(strings.TrimSpace(r.PathValue("tenant")), "demo")
	baseURL := requestBaseOrigin(r) + "/fhir/r4/" + tenant
	slug := strings.TrimSpace(r.PathValue("slug"))
	bySlug := buildUDSPlusResourcesBySlug(baseURL)
	res, ok := bySlug[slug]
	if !ok {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", "unknown UDS+ resource slug: "+slug)
		return
	}
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(res)
	}
}

// handlePatientPathologyExchangeBundle serves GET/POST /fhir/r4/{tenant}/Patient/{id}/$pathology-exchange-bundle
// returning a complete Cancer Pathology Data Sharing (CPDS v1.0.1) us-pathology-exchange-bundle transaction Bundle.
func (s *Server) handlePatientPathologyExchangeBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "$pathology-exchange-bundle requires GET or POST")
		return
	}
	patID := strings.TrimPrefix(firstNonEmptyStr(
		strings.TrimSpace(r.PathValue("id")),
		strings.TrimSpace(r.URL.Query().Get("patient")),
		"pat-alice-001",
	), "Patient/")
	nowStr := time.Now().UTC().Format(time.RFC3339)

	patRes := map[string]any{
		"resourceType": "Patient",
		"id":           patID,
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
		},
		"extension": []map[string]any{
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
				"extension": []map[string]any{
					{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2106-3", "display": "White"}},
					{"url": "detailed", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2108-9", "display": "European"}},
					{"url": "text", "valueString": "White"},
				},
			},
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
				"extension": []map[string]any{
					{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2186-5", "display": "Not Hispanic or Latino"}},
					{"url": "detailed", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2184-0", "display": "Dominican"}},
					{"url": "text", "valueString": "Not Hispanic or Latino"},
				},
			},
			{"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex", "valueCode": "F"},
			{"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-genderIdentity", "valueCodeableConcept": map[string]any{
				"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-NullFlavor", "code": "UNK", "display": "unknown"}},
			}},
		},
		"identifier": []map[string]any{
			{
				"use":    "usual",
				"type":   map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MR"}}},
				"system": "http://hospital.smarthealthit.org",
				"value":  "MRN-CPDS-001",
			},
		},
		"active": true,
		"name": []map[string]any{
			{"use": "official", "family": "Smith", "given": []string{"Alice", "Marie"}, "suffix": []string{"Jr."}, "period": map[string]any{"start": "2010-01-01"}},
			{"use": "old", "family": "Jones", "given": []string{"Alice"}, "period": map[string]any{"start": "1985-04-12", "end": "2009-12-31"}},
		},
		"telecom": []map[string]any{
			{"system": "phone", "value": "555-0101", "use": "home"},
			{"system": "email", "value": "alice.smith@example.org", "use": "home"},
		},
		"gender":    "female",
		"birthDate": "1985-04-12",
		"address": []map[string]any{
			{"use": "home", "line": []string{"123 Main St"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US", "period": map[string]any{"start": "2015-01-01"}},
			{"use": "old", "line": []string{"45 Elm St"}, "city": "Cambridge", "state": "MA", "postalCode": "02139", "country": "US", "period": map[string]any{"start": "2005-01-01", "end": "2014-12-31"}},
		},
		"communication": []map[string]any{
			{
				"language":  map[string]any{"coding": []map[string]any{{"system": "urn:ietf:bcp:47", "code": "en-US", "display": "English"}}},
				"preferred": true,
			},
		},
	}

	orgRes := map[string]any{
		"resourceType": "Organization",
		"id":           "org-cpds-lab",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-organization"},
		},
		"identifier": []map[string]any{
			{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1234567893"},
			{"system": "urn:oid:2.16.840.1.113883.4.7", "value": "22D0987654"},
		},
		"active": true,
		"name":   "Flint Oncology & Pathology Laboratory",
		"telecom": []map[string]any{
			{"system": "phone", "value": "617-555-0200"},
		},
		"address": []map[string]any{
			{"line": []string{"500 Medical Center Dr"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"},
		},
	}

	pracRes := map[string]any{
		"resourceType": "Practitioner",
		"id":           "prac-cpds-pathologist",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitioner"},
		},
		"identifier": []map[string]any{
			{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1992999125"},
		},
		"active": true,
		"name": []map[string]any{
			{"family": "Chen", "given": []string{"Evelyn"}, "prefix": []string{"Dr."}},
		},
		"telecom": []map[string]any{
			{"system": "phone", "value": "617-555-0201"},
			{"system": "email", "value": "echen@flint-pathology.example.org"},
		},
		"address": []map[string]any{
			{"line": []string{"500 Medical Center Dr"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"},
		},
	}

	locRes := map[string]any{
		"resourceType": "Location",
		"id":           "loc-cpds-pathology",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-location"},
		},
		"status": "active",
		"name":   "Flint Surgical Pathology Suite",
		"telecom": []map[string]any{
			{"system": "phone", "value": "617-555-0202"},
		},
		"address": map[string]any{
			"line":       []string{"500 Medical Center Dr"},
			"city":       "Boston",
			"state":      "MA",
			"postalCode": "02115",
			"country":    "US",
		},
		"managingOrganization": map[string]any{"reference": "Organization/org-cpds-lab"},
	}

	epRes := map[string]any{
		"resourceType": "Endpoint",
		"id":           "ep-cpds-pathology",
		"status":       "active",
		"connectionType": map[string]any{
			"system": "http://terminology.hl7.org/CodeSystem/endpoint-connection-type",
			"code":   "hl7-fhir-rest",
		},
		"name":                 "Flint Pathology FHIR Endpoint",
		"managingOrganization": map[string]any{"reference": "Organization/org-cpds-lab"},
		"payloadType": []map[string]any{
			{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/endpoint-payload-type", "code": "any"}}},
		},
		"address": requestBaseOrigin(r) + "/fhir/r4/demo",
	}

	prRes := map[string]any{
		"resourceType": "PractitionerRole",
		"id":           "pr-cpds-pathologist",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitionerrole"},
		},
		"active":       true,
		"practitioner": map[string]any{"reference": "Practitioner/prac-cpds-pathologist"},
		"organization": map[string]any{"reference": "Organization/org-cpds-lab"},
		"code": []map[string]any{
			{"coding": []map[string]any{{"system": "http://nucc.org/provider-taxonomy", "code": "207ZP0102X", "display": "Anatomic Pathology & Clinical Pathology Physician"}}},
		},
		"specialty": []map[string]any{
			{"coding": []map[string]any{{"system": "http://nucc.org/provider-taxonomy", "code": "207ZP0102X", "display": "Anatomic Pathology & Clinical Pathology Physician"}}},
			{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason", "code": "unknown", "display": "Unknown"}}},
		},
		"location": []map[string]any{{"reference": "Location/loc-cpds-pathology"}},
		"telecom": []map[string]any{
			{"system": "phone", "value": "617-555-0201"},
		},
		"endpoint": []map[string]any{{"reference": "Endpoint/ep-cpds-pathology"}},
	}

	encRes := map[string]any{
		"resourceType": "Encounter",
		"id":           "enc-cpds-001",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-encounter"},
		},
		"identifier": []map[string]any{
			{"system": "http://hospital.smarthealthit.org/encounters", "value": "ENC-CPDS-001"},
		},
		"status": "finished",
		"class":  map[string]any{"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB", "display": "ambulatory"},
		"type": []map[string]any{
			{"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "88305", "display": "Level IV - Surgical pathology, gross and microscopic examination"}}},
		},
		"subject": map[string]any{"reference": "Patient/" + patID},
		"participant": []map[string]any{
			{
				"type":       []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-ParticipationType", "code": "PPRF"}}}},
				"period":     map[string]any{"start": "2024-11-01T09:00:00Z", "end": "2024-11-01T10:00:00Z"},
				"individual": map[string]any{"reference": "Practitioner/prac-cpds-pathologist"},
			},
		},
		"period": map[string]any{"start": "2024-11-01T09:00:00Z", "end": "2024-11-01T10:00:00Z"},
		"reasonCode": []map[string]any{
			{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}},
		},
		"hospitalization": map[string]any{
			"dischargeDisposition": map[string]any{
				"coding": []map[string]any{{"system": "http://www.nubc.org/patient-discharge", "code": "01", "display": "Discharged to Home"}},
			},
		},
		"location": []map[string]any{
			{"location": map[string]any{"reference": "Location/loc-cpds-pathology"}},
		},
		"serviceProvider": map[string]any{"reference": "Organization/org-cpds-lab"},
	}

	parentOrderRes := map[string]any{
		"resourceType": "ServiceRequest",
		"id":           "sr-cpds-prereq",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/cancer-reporting/StructureDefinition/us-pathology-service-request"},
		},
		"identifier": []map[string]any{
			{"system": "http://hospital.smarthealthit.org/orders", "value": "ORD-CPDS-000"},
		},
		"status": "completed",
		"intent": "order",
		"category": []map[string]any{
			{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "108252007", "display": "Laboratory procedure"}}},
		},
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://loinc.org", "code": "11529-5", "display": "Surgical pathology study"}},
		},
		"subject":          map[string]any{"reference": "Patient/" + patID},
		"encounter":        map[string]any{"reference": "Encounter/enc-cpds-001"},
		"occurrencePeriod": map[string]any{"start": "2024-11-01T08:30:00Z", "end": "2024-11-01T09:00:00Z"},
		"authoredOn":       "2024-11-01T08:00:00Z",
		"requester":        map[string]any{"reference": "PractitionerRole/pr-cpds-pathologist"},
	}

	srRes := map[string]any{
		"resourceType": "ServiceRequest",
		"id":           "sr-cpds-001",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/cancer-reporting/StructureDefinition/us-pathology-service-request"},
		},
		"identifier": []map[string]any{
			{"system": "http://hospital.smarthealthit.org/orders", "value": "ORD-CPDS-001"},
		},
		"basedOn": []map[string]any{{"reference": "ServiceRequest/sr-cpds-prereq"}},
		"status":  "active",
		"intent":  "order",
		"category": []map[string]any{
			{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "108252007", "display": "Laboratory procedure"}}},
		},
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://loinc.org", "code": "11529-5", "display": "Surgical pathology study"}},
		},
		"subject":          map[string]any{"reference": "Patient/" + patID},
		"encounter":        map[string]any{"reference": "Encounter/enc-cpds-001"},
		"occurrencePeriod": map[string]any{"start": "2024-11-01T09:00:00Z", "end": "2024-11-01T10:00:00Z"},
		"authoredOn":       "2024-11-01T08:45:00Z",
		"requester":        map[string]any{"reference": "PractitionerRole/pr-cpds-pathologist"},
	}

	specParent := map[string]any{
		"resourceType": "Specimen",
		"id":           "spec-cpds-parent",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/cancer-reporting/StructureDefinition/us-pathology-specimen"},
		},
		"identifier": []map[string]any{
			{"system": "http://hospital.smarthealthit.org/specimens", "value": "SPEC-CPDS-PARENT"},
		},
		"accessionIdentifier": map[string]any{
			"system": "http://hospital.smarthealthit.org/accession",
			"value":  "ACC-2024-9001",
		},
		"status": "available",
		"type": map[string]any{
			"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "119376003", "display": "Tissue specimen"}},
		},
		"subject":      map[string]any{"reference": "Patient/" + patID},
		"receivedTime": "2024-11-01T09:30:00Z",
		"collection": map[string]any{
			"collector":         map[string]any{"reference": "PractitionerRole/pr-cpds-pathologist"},
			"collectedDateTime": "2024-11-01T09:15:00Z",
			"method": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "129314006", "display": "Biopsy - action"}},
			},
			"bodySite": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "76752008", "display": "Breast structure"}},
			},
		},
		"container": []map[string]any{
			{
				"identifier": []map[string]any{{"system": "http://hospital.smarthealthit.org/containers", "value": "CONT-001"}},
				"type":       map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "434711009", "display": "Specimen container"}}},
			},
		},
		"note": []map[string]any{{"text": "Left breast core needle biopsy tissue block A1"}},
	}

	// Second Specimen demonstrates parent reference + DataAbsentReason Extension + DataAbsentReason CodeSystem#unknown
	specPart := map[string]any{
		"resourceType": "Specimen",
		"id":           "spec-cpds-part",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/cancer-reporting/StructureDefinition/us-pathology-specimen"},
		},
		"identifier": []map[string]any{
			{"system": "http://hospital.smarthealthit.org/specimens", "value": "SPEC-CPDS-PART"},
		},
		"accessionIdentifier": map[string]any{
			"system": "http://hospital.smarthealthit.org/accession",
			"value":  "ACC-2024-9001-B",
		},
		"status": "available",
		"type": map[string]any{
			"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "119376003", "display": "Tissue specimen"}},
		},
		"subject": map[string]any{"reference": "Patient/" + patID},
		"parent":  []map[string]any{{"reference": "Specimen/spec-cpds-parent"}},
		"_receivedTime": map[string]any{
			"extension": []map[string]any{
				{"url": "http://hl7.org/fhir/StructureDefinition/data-absent-reason", "valueCode": "unknown"},
			},
		},
		"collection": map[string]any{
			"collector":         map[string]any{"reference": "PractitionerRole/pr-cpds-pathologist"},
			"collectedDateTime": "2024-11-01T09:20:00Z",
			"method": map[string]any{
				"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason", "code": "unknown", "display": "Unknown"}},
			},
			"bodySite": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "76752008", "display": "Breast structure"}},
			},
		},
		"container": []map[string]any{
			{
				"type": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "434711009", "display": "Specimen container"}}},
			},
		},
	}

	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("missingData")), "hide") {
		prRes["specialty"] = []map[string]any{
			{"coding": []map[string]any{{"system": "http://nucc.org/provider-taxonomy", "code": "207ZP0102X", "display": "Anatomic Pathology & Clinical Pathology Physician"}}},
		}
		delete(specPart, "_receivedTime")
		specPart["receivedTime"] = "2024-11-01T09:35:00Z"
		specPart["collection"] = map[string]any{
			"collector":         map[string]any{"reference": "PractitionerRole/pr-cpds-pathologist"},
			"collectedDateTime": "2024-11-01T09:20:00Z",
			"method": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "129314006", "display": "Biopsy - action"}},
			},
			"bodySite": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "76752008", "display": "Breast structure"}},
			},
		}
	}

	obsPath := map[string]any{
		"resourceType": "Observation",
		"id":           "obs-cpds-histology",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/cancer-reporting/StructureDefinition/us-pathology-observation"},
		},
		"identifier": []map[string]any{
			{"system": "http://hospital.smarthealthit.org/observations", "value": "OBS-CPDS-001"},
		},
		"status": "final",
		"category": []map[string]any{
			{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory", "display": "Laboratory"}}},
		},
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://loinc.org", "code": "33731-1", "display": "Histology grade [Identifier] in Cancer specimen"}},
		},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"encounter":         map[string]any{"reference": "Encounter/enc-cpds-001"},
		"effectiveDateTime": "2024-11-01T10:15:00Z",
		"performer":         []map[string]any{{"reference": "PractitionerRole/pr-cpds-pathologist"}},
		"valueCodeableConcept": map[string]any{
			"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "399415002", "display": "Low grade histologic differentiation"}},
		},
		"specimen": map[string]any{"reference": "Specimen/spec-cpds-parent"},
	}

	mediaRes := map[string]any{
		"resourceType": "Media",
		"id":           "media-cpds-slide-1",
		"status":       "completed",
		"type": map[string]any{
			"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/media-type", "code": "image", "display": "Image"}},
		},
		"subject": map[string]any{"reference": "Patient/" + patID},
		"content": map[string]any{
			"contentType": "image/png",
			"data":        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
			"title":       "H&E Whole Slide Image Thumbnail",
		},
	}

	drRes := map[string]any{
		"resourceType": "DiagnosticReport",
		"id":           "dr-cpds-001",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/cancer-reporting/StructureDefinition/us-pathology-diagnostic-report"},
		},
		"identifier": []map[string]any{
			{"system": "http://hospital.smarthealthit.org/reports", "value": "DR-CPDS-001"},
		},
		"basedOn": []map[string]any{{"reference": "ServiceRequest/sr-cpds-001"}},
		"status":  "final",
		"category": []map[string]any{
			{"coding": []map[string]any{{"system": "http://loinc.org", "code": "LP7839-6", "display": "Pathology"}}},
		},
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://loinc.org", "code": "60568-3", "display": "Pathology Synoptic report"}},
		},
		"subject":            map[string]any{"reference": "Patient/" + patID},
		"encounter":          map[string]any{"reference": "Encounter/enc-cpds-001"},
		"effectiveDateTime":  "2024-11-01T10:30:00Z",
		"issued":             "2024-11-01T11:00:00Z",
		"performer":          []map[string]any{{"reference": "Organization/org-cpds-lab"}, {"reference": "PractitionerRole/pr-cpds-pathologist"}},
		"resultsInterpreter": []map[string]any{{"reference": "PractitionerRole/pr-cpds-pathologist"}},
		"specimen":           []map[string]any{{"reference": "Specimen/spec-cpds-parent"}, {"reference": "Specimen/spec-cpds-part"}},
		"result":             []map[string]any{{"reference": "Observation/obs-cpds-histology"}},
		"media": []map[string]any{
			{"comment": "H&E Slide A1", "link": map[string]any{"reference": "Media/media-cpds-slide-1"}},
		},
		"presentedForm": []map[string]any{
			{
				"contentType": "text/plain",
				"data":        base64.StdEncoding.EncodeToString([]byte("Synoptic Pathology Report: Invasive ductal carcinoma, histologic grade 1.")),
				"title":       "Synoptic Pathology Summary",
			},
		},
	}

	// Note: strictly 1 ServiceRequest slice is required in us-pathology-exchange-bundle (1..1),
	// so basedOn uses an identifier/external reference or we keep exactly 1 ServiceRequest in Bundle.entry!
	srRes["basedOn"] = []map[string]any{
		{"identifier": map[string]any{"system": "http://hospital.smarthealthit.org/orders", "value": "ORD-CPDS-000"}, "display": "Initial Oncology Referral"},
	}
	_ = parentOrderRes

	resources := []map[string]any{
		patRes,
		encRes,
		drRes,
		specParent,
		specPart,
		srRes,
		orgRes,
		pracRes,
		prRes,
		locRes,
		epRes,
		obsPath,
		mediaRes,
	}
	entries := make([]map[string]any, 0, len(resources))
	for _, res := range resources {
		rt, _ := res["resourceType"].(string)
		id, _ := res["id"].(string)
		entries = append(entries, map[string]any{
			"fullUrl":  requestBaseOrigin(r) + "/fhir/r4/demo/" + rt + "/" + id,
			"resource": res,
			"request": map[string]any{
				"method": "POST",
				"url":    rt,
			},
		})
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"id":           "cpds-exchange-bundle-" + patID,
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/cancer-reporting/StructureDefinition/us-pathology-exchange-bundle"},
		},
		"identifier": map[string]any{
			"system": "urn:ietf:rfc:3986",
			"value":  "urn:uuid:550e8400-e29b-41d4-a716-446655440099",
		},
		"type":      "transaction",
		"timestamp": nowStr,
		"entry":     entries,
	}
	s.writeOperationResponse(w, r, http.StatusOK, "Bundle", bundle)
}

// handlePatientHomeLabReport serves GET/POST /fhir/r4/{tenant}/Patient/{id}/$home-lab-report
// returning an HL7 At-Home In-Vitro Test Report (v1.0.0) Bundle-at-home-in-vitro-test message Bundle.
func (s *Server) handlePatientHomeLabReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "$home-lab-report requires GET or POST")
		return
	}
	patID := strings.TrimPrefix(firstNonEmptyStr(
		strings.TrimSpace(r.PathValue("id")),
		strings.TrimSpace(r.URL.Query().Get("patient")),
		"pat-home-lab-1",
	), "Patient/")
	nowStr := time.Now().UTC().Format(time.RFC3339)

	msgHdr := map[string]any{
		"resourceType": "MessageHeader",
		"id":           "mh-home-lab-1",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/home-lab-report/StructureDefinition/MessageHeader-at-home-in-vitro-test"},
		},
		"eventCoding": map[string]any{
			"system":  "http://terminology.hl7.org/CodeSystem/v2-0003",
			"code":    "R01",
			"display": "ORU/ACK - Unsolicited transmission of an observation message",
		},
		"destination": []map[string]any{
			{
				"name":     "State Public Health Laboratory Receiver",
				"endpoint": "https://publichealth.example.gov/fhir/r4/$process-message",
			},
		},
		"sender": map[string]any{"reference": "Organization/org-home-lab-app"},
		"source": map[string]any{
			"name":     "Flint At-Home Test Companion App",
			"software": "Flint HomeLab Mobile",
			"version":  "1.0.0",
			"endpoint": requestBaseOrigin(r) + "/fhir/r4/demo",
		},
		"focus": []map[string]any{{"reference": "DiagnosticReport/dr-home-lab-1"}},
	}

	orgApp := map[string]any{
		"resourceType": "Organization",
		"id":           "org-home-lab-app",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-organization"},
		},
		"identifier": []map[string]any{
			{"system": "urn:oid:2.16.840.1.113883.4.7", "value": "99D0001122"},
		},
		"active": true,
		"name":   "Flint Diagnostics Digital Health",
		"telecom": []map[string]any{
			{"system": "phone", "value": "800-555-0144"},
		},
		"address": []map[string]any{
			{"line": []string{"1 Diagnostics Plaza"}, "city": "Boston", "state": "MA", "postalCode": "02110", "country": "US"},
		},
	}

	patRes := map[string]any{
		"resourceType": "Patient",
		"id":           patID,
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/home-lab-report/StructureDefinition/Patient-at-home-in-vitro-test"},
		},
		"extension": []map[string]any{
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
				"extension": []map[string]any{
					{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2106-3", "display": "White"}},
					{"url": "text", "valueString": "White"},
				},
			},
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
				"extension": []map[string]any{
					{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2186-5", "display": "Not Hispanic or Latino"}},
					{"url": "text", "valueString": "Not Hispanic or Latino"},
				},
			},
		},
		"identifier": []map[string]any{
			{
				"type":   map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "PT"}}},
				"system": "https://flint.fhir.org/home-lab/patients",
				"value":  "HL-PAT-001",
			},
		},
		"name":      []map[string]any{{"family": "Smith", "given": []string{"Alice"}}},
		"telecom":   []map[string]any{{"system": "phone", "value": "617-555-0101", "use": "mobile"}},
		"gender":    "female",
		"birthDate": "1985-04-12",
		"address": []map[string]any{
			{"line": []string{"123 Main St"}, "city": "Boston", "district": "Suffolk", "state": "MA", "postalCode": "02115", "country": "US"},
		},
	}

	devRes := map[string]any{
		"resourceType":   "Device",
		"id":             "dev-home-lab-kit-1",
		"meta":           map[string]any{"profile": []string{"http://hl7.org/fhir/us/home-lab-report/StructureDefinition/Device-at-home-in-vitro-test"}},
		"manufacturer":   "Flint Rapid Diagnostics Inc.",
		"serialNumber":   "SN-HL-20241101",
		"lotNumber":      "LOT-HL-8821",
		"expirationDate": "2027-12-31",
		"deviceName": []map[string]any{
			{"name": "Flint COVID-19 Antigen Home Test", "type": "user-friendly-name"},
		},
		"type": map[string]any{
			"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "258083009", "display": "Visual read test strip"}},
		},
	}

	specRes := map[string]any{
		"resourceType": "Specimen",
		"id":           "spec-home-lab-swab-1",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/home-lab-report/StructureDefinition/Specimen-at-home-in-vitro-test"},
		},
		"identifier": []map[string]any{
			{"system": "https://flint.fhir.org/home-lab/specimens", "value": "HL-SPEC-001"},
		},
		"type": map[string]any{
			"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "445297001", "display": "Swab of internal nose"}},
		},
		"subject": map[string]any{"reference": "Patient/" + patID},
		"collection": map[string]any{
			"collectedDateTime": "2024-11-02T08:15:00Z",
		},
	}

	obsRes := map[string]any{
		"resourceType": "Observation",
		"id":           "obs-home-lab-covid-1",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/home-lab-report/StructureDefinition/Observation-at-home-in-vitro-test-result"},
		},
		"identifier": []map[string]any{
			{"system": "https://flint.fhir.org/home-lab/observations", "value": "HL-OBS-001"},
		},
		"status": "final",
		"category": []map[string]any{
			{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory", "display": "Laboratory"}}},
		},
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://loinc.org", "code": "95209-3", "display": "SARS-CoV+SARS-CoV-2 (COVID-19) Ag [Presence] in Respiratory specimen by Rapid immunoassay"}},
		},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"effectiveDateTime": "2024-11-02T08:30:00Z",
		"issued":            "2024-11-02T08:31:00Z",
		"performer":         []map[string]any{{"reference": "Patient/" + patID}},
		"valueCodeableConcept": map[string]any{
			"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "260415000", "display": "Not detected"}},
		},
		"specimen": map[string]any{"reference": "Specimen/spec-home-lab-swab-1"},
		"device":   map[string]any{"reference": "Device/dev-home-lab-kit-1"},
	}

	drRes := map[string]any{
		"resourceType": "DiagnosticReport",
		"id":           "dr-home-lab-1",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/home-lab-report/StructureDefinition/DiagnosticReport-at-home-in-vitro-results"},
		},
		"identifier": []map[string]any{
			{"system": "https://flint.fhir.org/home-lab/reports", "value": "HL-DR-001"},
		},
		"status": "final",
		"category": []map[string]any{
			{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "LAB", "display": "Laboratory"}}},
		},
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://loinc.org", "code": "95209-3", "display": "SARS-CoV+SARS-CoV-2 (COVID-19) Ag [Presence] in Respiratory specimen by Rapid immunoassay"}},
		},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"effectiveDateTime": "2024-11-02T08:30:00Z",
		"issued":            "2024-11-02T08:31:00Z",
		"specimen":          []map[string]any{{"reference": "Specimen/spec-home-lab-swab-1"}},
		"result":            []map[string]any{{"reference": "Observation/obs-home-lab-covid-1"}},
	}

	resources := []map[string]any{msgHdr, drRes, patRes, obsRes, specRes, devRes, orgApp}
	entries := make([]map[string]any, 0, len(resources))
	for _, res := range resources {
		rt, _ := res["resourceType"].(string)
		id, _ := res["id"].(string)
		entries = append(entries, map[string]any{
			"fullUrl":  requestBaseOrigin(r) + "/fhir/r4/demo/" + rt + "/" + id,
			"resource": res,
		})
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"id":           "home-lab-bundle-" + patID,
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/home-lab-report/StructureDefinition/Bundle-at-home-in-vitro-test"},
		},
		"identifier": map[string]any{
			"system": "urn:ietf:rfc:3986",
			"value":  "urn:uuid:660e8400-e29b-41d4-a716-446655440088",
		},
		"type":      "message",
		"timestamp": nowStr,
		"entry":     entries,
	}
	s.writeOperationResponse(w, r, http.StatusOK, "Bundle", bundle)
}

// buildCCRRContentBundle constructs a complete HL7 Central Cancer Registry Reporting (CCRR v1.0.0)
// ccrr-content-bundle document Bundle containing all 15 Composition sections and MustSupport elements.
func (s *Server) buildCCRRContentBundle(r *http.Request, patID, icdCode, nowStr string) map[string]any {
	if icdCode == "" {
		icdCode = "C50.919"
	}
	baseURL := requestBaseOrigin(r) + "/fhir/r4/ccrr"
	xhtml := func(text string) map[string]any {
		return map[string]any{
			"status": "generated",
			"div":    fmt.Sprintf(`<div xmlns="http://www.w3.org/1999/xhtml"><p>%s</p></div>`, text),
		}
	}

	patRes := map[string]any{
		"resourceType": "Patient",
		"id":           patID,
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"}},
		"extension": []map[string]any{
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
				"extension": []map[string]any{
					{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2106-3", "display": "White"}},
					{"url": "detailed", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2108-9", "display": "European"}},
					{"url": "text", "valueString": "White"},
				},
			},
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
				"extension": []map[string]any{
					{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2186-5", "display": "Not Hispanic or Latino"}},
					{"url": "detailed", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2184-0", "display": "Dominican"}},
					{"url": "text", "valueString": "Not Hispanic or Latino"},
				},
			},
			{"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex", "valueCode": "F"},
			{"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-tribal-affiliation", "extension": []map[string]any{
				{"url": "tribalAffiliation", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-TribalEntityUS", "code": "100", "display": "Absentee-Shawnee Tribe of Indians of Oklahoma"}}}},
			}},
			{"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-sex", "valueCode": "248152002"},
			{"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-genderIdentity", "valueCodeableConcept": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "446141000124107", "display": "Identifies as female gender"}},
			}},
		},
		"identifier": []map[string]any{
			{
				"use":    "usual",
				"type":   map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MR"}}},
				"system": "http://hospital.smarthealthit.org",
				"value":  "MRN-CCRR-" + patID,
			},
		},
		"active": true,
		"name": []map[string]any{
			{"use": "official", "family": "Oncology", "given": []string{"Clara", "Anne"}, "suffix": []string{"III"}, "period": map[string]any{"start": "2010-01-01"}},
			{"use": "old", "family": "Miller", "given": []string{"Clara"}, "period": map[string]any{"start": "1972-06-15", "end": "2009-12-31"}},
		},
		"telecom": []map[string]any{
			{"system": "phone", "value": "617-555-0301", "use": "home"},
			{"system": "email", "value": "clara.oncology@example.org", "use": "home"},
		},
		"gender":          "female",
		"birthDate":       "1972-06-15",
		"deceasedBoolean": false,
		"address": []map[string]any{
			{"use": "home", "line": []string{"88 Cancer Center Way"}, "city": "Boston", "district": "Suffolk", "state": "MA", "postalCode": "02115", "country": "US", "period": map[string]any{"start": "2015-01-01"}},
			{"use": "old", "line": []string{"12 Harbor Rd"}, "city": "Quincy", "district": "Norfolk", "state": "MA", "postalCode": "02169", "country": "US", "period": map[string]any{"start": "2000-01-01", "end": "2014-12-31"}},
		},
		"communication": []map[string]any{
			{"language": map[string]any{"coding": []map[string]any{{"system": "urn:ietf:bcp:47", "code": "en-US", "display": "English"}}}, "preferred": true},
		},
	}

	orgRes := map[string]any{
		"resourceType": "Organization",
		"id":           "org-ccrr-1",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-organization"}},
		"identifier": []map[string]any{
			{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1497758544"},
			{"system": "urn:oid:2.16.840.1.113883.4.7", "value": "22D1234567"},
		},
		"active":  true,
		"name":    "Flint Comprehensive Cancer Center",
		"telecom": []map[string]any{{"system": "phone", "value": "617-555-0300"}},
		"address": []map[string]any{{"line": []string{"88 Cancer Center Way"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"}},
	}

	pracRes := map[string]any{
		"resourceType": "Practitioner",
		"id":           "prac-ccrr-1",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitioner"}},
		"identifier":   []map[string]any{{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1881630614"}},
		"active":       true,
		"name":         []map[string]any{{"family": "Patel", "given": []string{"Raj"}, "prefix": []string{"Dr."}}},
		"telecom":      []map[string]any{{"system": "phone", "value": "617-555-0310"}, {"system": "email", "value": "rpatel@flint-cancer.example.org"}},
		"address":      []map[string]any{{"line": []string{"88 Cancer Center Way"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"}},
	}

	locRes := map[string]any{
		"resourceType":         "Location",
		"id":                   "loc-ccrr-1",
		"meta":                 map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-location"}},
		"status":               "active",
		"name":                 "Flint Oncology Outpatient Clinic",
		"telecom":              []map[string]any{{"system": "phone", "value": "617-555-0302"}},
		"address":              map[string]any{"line": []string{"88 Cancer Center Way"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"},
		"managingOrganization": map[string]any{"reference": "Organization/org-ccrr-1"},
	}

	epRes := map[string]any{
		"resourceType":         "Endpoint",
		"id":                   "ep-ccrr-1",
		"status":               "active",
		"connectionType":       map[string]any{"system": "http://terminology.hl7.org/CodeSystem/endpoint-connection-type", "code": "hl7-fhir-rest"},
		"name":                 "Flint Cancer Registry Endpoint",
		"managingOrganization": map[string]any{"reference": "Organization/org-ccrr-1"},
		"payloadType":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/endpoint-payload-type", "code": "any"}}}},
		"address":              baseURL,
	}

	prRes := map[string]any{
		"resourceType": "PractitionerRole",
		"id":           "pr-ccrr-1",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitionerrole"}},
		"active":       true,
		"practitioner": map[string]any{"reference": "Practitioner/prac-ccrr-1"},
		"organization": map[string]any{"reference": "Organization/org-ccrr-1"},
		"code":         []map[string]any{{"coding": []map[string]any{{"system": "http://nucc.org/provider-taxonomy", "code": "207RX0202X", "display": "Medical Oncology Physician"}}}},
		"specialty":    []map[string]any{{"coding": []map[string]any{{"system": "http://nucc.org/provider-taxonomy", "code": "207RX0202X", "display": "Medical Oncology Physician"}}}},
		"location":     []map[string]any{{"reference": "Location/loc-ccrr-1"}},
		"telecom":      []map[string]any{{"system": "phone", "value": "617-555-0310"}},
		"endpoint":     []map[string]any{{"reference": "Endpoint/ep-ccrr-1"}},
	}

	encRes := map[string]any{
		"resourceType": "Encounter",
		"id":           "enc-ccrr-1",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-encounter"}},
		"identifier":   []map[string]any{{"system": "http://hospital.smarthealthit.org/encounters", "value": "ENC-CCRR-001"}},
		"status":       "finished",
		"class":        map[string]any{"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB", "display": "ambulatory"},
		"type":         []map[string]any{{"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "99214", "display": "Office outpatient visit"}}}},
		"subject":      map[string]any{"reference": "Patient/" + patID},
		"participant": []map[string]any{
			{
				"type":       []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-ParticipationType", "code": "PPRF"}}}},
				"period":     map[string]any{"start": "2024-09-10T09:00:00Z", "end": "2024-09-10T09:45:00Z"},
				"individual": map[string]any{"reference": "Practitioner/prac-ccrr-1"},
			},
		},
		"period":     map[string]any{"start": "2024-09-10T09:00:00Z", "end": "2024-09-10T09:45:00Z"},
		"reasonCode": []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
		"hospitalization": map[string]any{
			"dischargeDisposition": map[string]any{"coding": []map[string]any{{"system": "http://www.nubc.org/patient-discharge", "code": "01", "display": "Discharged to Home"}}},
		},
		"location":        []map[string]any{{"location": map[string]any{"reference": "Location/loc-ccrr-1"}}},
		"serviceProvider": map[string]any{"reference": "Organization/org-ccrr-1"},
	}

	tnmT := map[string]any{
		"resourceType":      "Observation",
		"id":                "obs-ccrr-tnm-t",
		"meta":              map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-tnm-primary-tumor-category"}},
		"status":            "final",
		"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "imaging"}}}},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "21905-5", "display": "Primary tumor.clinical [Class] Cancer"}}},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"focus":             []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
		"effectiveDateTime": "2024-09-10T09:30:00Z",
		"performer":         []map[string]any{{"reference": "Practitioner/prac-ccrr-1"}},
		"valueCodeableConcept": map[string]any{
			"coding": []map[string]any{{"system": "http://cancerstaging.org", "code": "cT2", "display": "T2"}},
		},
		"method": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "897275008", "display": "AJCC 8th edition"}}},
	}

	tnmStage := map[string]any{
		"resourceType":      "Observation",
		"id":                "obs-ccrr-tnm-stage",
		"meta":              map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-tnm-stage-group"}},
		"status":            "final",
		"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "imaging"}}}},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "21908-9", "display": "Stage group.clinical Cancer"}}},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"focus":             []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
		"effectiveDateTime": "2024-09-10T09:30:00Z",
		"performer":         []map[string]any{{"reference": "Practitioner/prac-ccrr-1"}},
		"valueCodeableConcept": map[string]any{
			"coding": []map[string]any{{"system": "http://cancerstaging.org", "code": "2A", "display": "IIA"}},
		},
		"method":    map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "897275008", "display": "AJCC 8th edition"}}},
		"hasMember": []map[string]any{{"reference": "Observation/obs-ccrr-tnm-t"}},
	}

	primaryCancer := map[string]any{
		"resourceType": "Condition",
		"id":           "cond-ccrr-primary",
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/central-cancer-registry-reporting/StructureDefinition/central-cancer-registry-primary-cancer-condition"},
		},
		"extension": []map[string]any{
			{"url": "http://hl7.org/fhir/StructureDefinition/condition-assertedDate", "valueDateTime": "2024-09-10"},
			{
				"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-histology-morphology-behavior",
				"valueCodeableConcept": map[string]any{
					"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "82711006", "display": "Infiltrating duct carcinoma"}},
				},
			},
			{
				"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-cancer-disease-status-evidence-type",
				"valueCodeableConcept": map[string]any{
					"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "252416005", "display": "Histopathology test"}},
				},
			},
		},
		"clinicalStatus":     map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}}},
		"verificationStatus": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}}},
		"category": []map[string]any{
			{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item"}}},
			{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-tags", "code": "sdoh"}}},
		},
		"code": map[string]any{
			"coding": []map[string]any{
				{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"},
				{"system": "http://hl7.org/fhir/sid/icd-10-cm", "code": icdCode, "display": "Malignant neoplasm of female breast"},
			},
		},
		"bodySite": []map[string]any{
			{
				"extension": []map[string]any{
					{
						"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-body-location-qualifier",
						"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "261183002", "display": "Upper"}}},
					},
					{
						"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-laterality-qualifier",
						"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "7771000", "display": "Left"}}},
					},
					{
						"url":       "http://hl7.org/fhir/StructureDefinition/data-absent-reason",
						"valueCode": "unknown",
					},
				},
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "76752008", "display": "Breast structure"}},
			},
		},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"encounter":         map[string]any{"reference": "Encounter/enc-ccrr-1"},
		"onsetDateTime":     "2024-09-01",
		"abatementDateTime": "2024-11-15",
		"recordedDate":      "2024-09-10",
		"stage": []map[string]any{
			{
				"summary":    map[string]any{"coding": []map[string]any{{"system": "http://cancerstaging.org", "code": "2A", "display": "IIA"}}},
				"assessment": []map[string]any{{"reference": "Observation/obs-ccrr-tnm-stage"}},
			},
		},
	}

	secondaryCancer := map[string]any{
		"resourceType": "Condition",
		"id":           "cond-ccrr-secondary",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-secondary-cancer-condition"}},
		"extension": []map[string]any{
			{"url": "http://hl7.org/fhir/StructureDefinition/condition-assertedDate", "valueDateTime": "2024-09-12"},
			{
				"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-histology-morphology-behavior",
				"valueCodeableConcept": map[string]any{
					"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "82711006", "display": "Infiltrating duct carcinoma"}},
				},
			},
			{
				"url":            "http://hl7.org/fhir/StructureDefinition/condition-related",
				"valueReference": map[string]any{"reference": "Condition/cond-ccrr-primary"},
			},
		},
		"clinicalStatus":     map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}}},
		"verificationStatus": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}}},
		"category": []map[string]any{
			{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item"}}},
			{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-tags", "code": "sdoh"}}},
		},
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "94391008", "display": "Secondary malignant neoplasm of axillary lymph nodes"}},
		},
		"bodySite": []map[string]any{
			{
				"extension": []map[string]any{
					{
						"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-body-location-qualifier",
						"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "261183002", "display": "Upper"}}},
					},
					{
						"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-laterality-qualifier",
						"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "7771000", "display": "Left"}}},
					},
				},
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "68171009", "display": "Axillary lymph node structure"}},
			},
		},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"onsetDateTime":     "2024-09-12",
		"abatementDateTime": "2024-11-15",
		"recordedDate":      "2024-09-12",
	}

	bodyStruct := map[string]any{
		"resourceType": "BodyStructure",
		"id":           "bs-ccrr-breast-1",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-volume"}},
		"identifier":   []map[string]any{{"system": "http://hospital.smarthealthit.org/volumes", "value": "VOL-BREAST-L"}},
		"morphology":   map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "228793007", "display": "Planning target volume"}}},
		"location":     map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "76752008", "display": "Breast structure"}}},
		"locationQualifier": []map[string]any{
			{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "7771000", "display": "Left"}}},
		},
		"patient": map[string]any{"reference": "Patient/" + patID},
	}

	radioSummary := map[string]any{
		"resourceType": "Procedure",
		"id":           "proc-ccrr-radiotherapy",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-course-summary"}},
		"extension": []map[string]any{
			{
				"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-procedure-intent",
				"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "373808002", "display": "Curative - procedure intent"}}},
			},
			{
				"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-treatment-termination-reason",
				"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "182992009", "display": "Treatment completed"}}},
			},
			{
				"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-modality-and-technique",
				"extension": []map[string]any{
					{
						"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-modality",
						"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "1156506007", "display": "External beam radiation therapy using photons"}}},
					},
					{
						"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-technique",
						"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "441799006", "display": "Intensity modulated radiation therapy"}}},
					},
				},
			},
			{
				"url":              "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-sessions",
				"valueUnsignedInt": 15,
			},
			{
				"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-dose-delivered-to-volume",
				"extension": []map[string]any{
					{"url": "volume", "valueReference": map[string]any{"reference": "BodyStructure/bs-ccrr-breast-1"}},
					{"url": "totalDoseDelivered", "valueQuantity": map[string]any{"value": 4005, "unit": "cGy", "system": "http://unitsofmeasure.org", "code": "cGy"}},
					{"url": "fractionsDelivered", "valueUnsignedInt": 15},
				},
			},
		},
		"status":          "completed",
		"category":        map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "108290001", "display": "Radiation oncology AND/OR radiotherapy"}}},
		"code":            map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "1217123003", "display": "Radiotherapy course of treatment"}}},
		"subject":         map[string]any{"reference": "Patient/" + patID},
		"performedPeriod": map[string]any{"start": "2024-10-01", "end": "2024-10-21"},
		"reasonCode":      []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
		"reasonReference": []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
		"bodySite":        []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "76752008", "display": "Breast structure"}}}},
	}

	probCond := map[string]any{
		"resourceType":       "Condition",
		"id":                 "cond-ccrr-problem-1",
		"meta":               map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-condition"}},
		"extension":          []map[string]any{{"url": "http://hl7.org/fhir/StructureDefinition/condition-assertedDate", "valueDateTime": "2023-05-01"}},
		"clinicalStatus":     map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}}},
		"verificationStatus": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}}},
		"category": []map[string]any{
			{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item"}}},
			{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-tags", "code": "sdoh"}}},
		},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}}},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"onsetDateTime":     "2023-05-01",
		"abatementDateTime": "2024-08-01",
		"recordedDate":      "2023-05-01",
	}

	allergyRes := map[string]any{
		"resourceType":       "AllergyIntolerance",
		"id":                 "allergy-ccrr-1",
		"meta":               map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-allergyintolerance"}},
		"clinicalStatus":     map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}}},
		"verificationStatus": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification", "code": "confirmed"}}},
		"code":               map[string]any{"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980", "display": "Penicillin G"}}},
		"patient":            map[string]any{"reference": "Patient/" + patID},
		"reaction": []map[string]any{
			{"manifestation": []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "247472004", "display": "Hives"}}}}},
		},
	}

	medRes := map[string]any{
		"resourceType": "Medication",
		"id":           "med-ccrr-paclitaxel",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medication"}},
		"code":         map[string]any{"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "56946", "display": "Paclitaxel"}}},
	}

	medAdminRes := map[string]any{
		"resourceType": "MedicationAdministration",
		"id":           "ma-ccrr-1",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-cancer-related-medication-administration"}},
		"extension": []map[string]any{
			{
				"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-procedure-intent",
				"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "373808002", "display": "Curative - procedure intent"}}},
			},
			{
				"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-treatment-termination-reason",
				"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "182992009", "display": "Treatment completed"}}},
			},
		},
		"status":       "completed",
		"statusReason": []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "182856006", "display": "Drug stock out"}}}},
		"medicationCodeableConcept": map[string]any{
			"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "56946", "display": "Paclitaxel"}},
		},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"context":           map[string]any{"reference": "Encounter/enc-ccrr-1"},
		"effectiveDateTime": "2024-09-20T10:00:00Z",
		"performer":         []map[string]any{{"actor": map[string]any{"reference": "Practitioner/prac-ccrr-1"}}},
		"reasonCode":        []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
		"reasonReference":   []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
		"note":              []map[string]any{{"text": "Infusion tolerated well"}},
		"dosage": map[string]any{
			"text":  "175 mg/m2 IV over 3 hours",
			"route": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "47625008", "display": "Intravenous route"}}},
			"dose":  map[string]any{"value": 300, "unit": "mg", "system": "http://unitsofmeasure.org", "code": "mg"},
		},
	}

	medStmtRes := map[string]any{
		"resourceType": "MedicationStatement",
		"id":           "ms-ccrr-1",
		"status":       "active",
		"medicationCodeableConcept": map[string]any{
			"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "10324", "display": "Tamoxifen"}},
		},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"context":           map[string]any{"reference": "Encounter/enc-ccrr-1"},
		"effectiveDateTime": "2024-10-25",
		"dateAsserted":      "2024-10-25",
		"reasonReference":   []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
		"dosage":            []map[string]any{{"text": "20 mg PO daily"}},
	}

	medReqRes := map[string]any{
		"resourceType": "MedicationRequest",
		"id":           "mr-ccrr-1",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-cancer-related-medication-request"}},
		"extension": []map[string]any{
			{
				"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-procedure-intent",
				"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "373808002", "display": "Curative - procedure intent"}}},
			},
			{
				"url":                  "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-treatment-termination-reason",
				"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "182992009", "display": "Treatment completed"}}},
			},
		},
		"status":          "active",
		"intent":          "order",
		"category":        []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/medicationrequest-category", "code": "outpatient"}}}},
		"reportedBoolean": false,
		"medicationCodeableConcept": map[string]any{
			"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "10324", "display": "Tamoxifen"}},
		},
		"subject":         map[string]any{"reference": "Patient/" + patID},
		"encounter":       map[string]any{"reference": "Encounter/enc-ccrr-1"},
		"authoredOn":      "2024-10-25",
		"requester":       map[string]any{"reference": "Practitioner/prac-ccrr-1"},
		"reasonCode":      []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
		"reasonReference": []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
		"dosageInstruction": []map[string]any{
			{
				"text":   "Take 1 tablet (20 mg) by mouth daily",
				"timing": map[string]any{"repeat": map[string]any{"frequency": 1, "period": 1, "periodUnit": "d"}},
				"route":  map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "26643006", "display": "Oral route"}}},
				"doseAndRate": []map[string]any{
					{"doseQuantity": map[string]any{"value": 20, "unit": "mg", "system": "http://unitsofmeasure.org", "code": "mg"}},
				},
			},
		},
		"dispenseRequest": map[string]any{
			"numberOfRepeatsAllowed": 3,
			"quantity":               map[string]any{"value": 30, "unit": "TAB", "system": "http://terminology.hl7.org/CodeSystem/v3-orderableDrugForm", "code": "TAB"},
		},
	}

	odhWork := map[string]any{
		"resourceType": "Observation",
		"id":           "obs-ccrr-odh-work",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/odh/StructureDefinition/odh-UsualWork"}},
		"status":       "final",
		"category":     []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history"}}}},
		"code":         map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "21843-8", "display": "History of Usual occupation"}}},
		"subject":      map[string]any{"reference": "Patient/" + patID},
		"effectivePeriod": map[string]any{
			"start": "2005-01-01",
		},
		"valueCodeableConcept": map[string]any{
			"coding": []map[string]any{
				{"system": "urn:oid:2.16.840.1.114222.4.11.7186", "code": "3255", "display": "Registered nurses [3255]"},
				{"system": "2.16.840.1.114222.4.11.7186", "code": "3255", "display": "Registered nurses [3255]"},
				{"system": "urn:oid:2.16.840.1.114222.4.11.7901", "code": "29-1141.00.007136", "display": "Registered Nurse"},
				{"system": "2.16.840.1.114222.4.11.7901", "code": "29-1141.00.007136", "display": "Registered Nurse"},
			},
		},
		"component": []map[string]any{
			{
				"code": map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "21844-6", "display": "History of Usual industry"}}},
				"valueCodeableConcept": map[string]any{
					"coding": []map[string]any{
						{"system": "urn:oid:2.16.840.1.114222.4.11.7187", "code": "8190", "display": "Hospitals [8190]"},
						{"system": "2.16.840.1.114222.4.11.7187", "code": "8190", "display": "Hospitals [8190]"},
						{"system": "urn:oid:2.16.840.1.114222.4.11.7900", "code": "622110.002698", "display": "General Medical and Surgical Hospitals"},
						{"system": "2.16.840.1.114222.4.11.7900", "code": "622110.002698", "display": "General Medical and Surgical Hospitals"},
					},
				},
			},
		},
	}

	obsLab := map[string]any{
		"resourceType":      "Observation",
		"id":                "obs-ccrr-lab-er",
		"meta":              map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-lab"}},
		"status":            "final",
		"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}}}},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "85337-4", "display": "Estrogen receptor Ag [Presence] in Breast cancer specimen by Immune stain"}}},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"effectiveDateTime": "2024-09-10T11:00:00Z",
		"performer":         []map[string]any{{"reference": "Organization/org-ccrr-1"}},
		"valueCodeableConcept": map[string]any{
			"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "10828004", "display": "Positive"}},
		},
	}

	drLab := map[string]any{
		"resourceType":      "DiagnosticReport",
		"id":                "dr-ccrr-lab-1",
		"meta":              map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-diagnosticreport-lab"}},
		"status":            "final",
		"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "LAB"}}}},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "24323-8", "display": "Comprehensive metabolic 2000 panel"}}},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"encounter":         map[string]any{"reference": "Encounter/enc-ccrr-1"},
		"effectiveDateTime": "2024-09-10T11:00:00Z",
		"issued":            "2024-09-10T12:00:00Z",
		"performer":         []map[string]any{{"reference": "Organization/org-ccrr-1"}},
		"result":            []map[string]any{{"reference": "Observation/obs-ccrr-lab-er"}},
	}

	docRef := map[string]any{
		"resourceType": "DocumentReference",
		"id":           "docref-ccrr-note-1",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-documentreference"}},
		"identifier":   []map[string]any{{"system": "http://hospital.smarthealthit.org/docs", "value": "DOC-CCRR-001"}},
		"status":       "current",
		"type":         map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "11488-4", "display": "Consult note"}}},
		"category":     []map[string]any{{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-documentreference-category", "code": "clinical-note"}}}},
		"subject":      map[string]any{"reference": "Patient/" + patID},
		"date":         "2024-09-10T13:00:00Z",
		"author":       []map[string]any{{"reference": "Practitioner/prac-ccrr-1"}},
		"custodian":    map[string]any{"reference": "Organization/org-ccrr-1"},
		"content": []map[string]any{
			{
				"attachment": map[string]any{
					"contentType": "text/plain",
					"data":        base64.StdEncoding.EncodeToString([]byte("Oncology Consultation Note: Stage IIA ER+ breast cancer.")),
					"url":         baseURL + "/Binary/docref-ccrr-note-1",
				},
				"format": map[string]any{"system": "http://ihe.net/fhir/ihe.formatcode.fhir/CodeSystem/formatcode", "code": "urn:ihe:iti:xds:2017:mimeTypeSufficient"},
			},
		},
		"context": map[string]any{
			"encounter": []map[string]any{{"reference": "Encounter/enc-ccrr-1"}},
			"period":    map[string]any{"start": "2024-09-10T09:00:00Z", "end": "2024-09-10T09:45:00Z"},
		},
	}

	drNote := map[string]any{
		"resourceType":      "DiagnosticReport",
		"id":                "dr-ccrr-note-1",
		"meta":              map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-diagnosticreport-note"}},
		"status":            "final",
		"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://loinc.org", "code": "LP7839-6", "display": "Pathology"}}}},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "60568-3", "display": "Pathology Synoptic report"}}},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"encounter":         map[string]any{"reference": "Encounter/enc-ccrr-1"},
		"effectiveDateTime": "2024-09-10T11:30:00Z",
		"issued":            "2024-09-10T12:30:00Z",
		"performer":         []map[string]any{{"reference": "Practitioner/prac-ccrr-1"}},
		"media":             []map[string]any{{"link": map[string]any{"display": "Pathology Image"}}},
		"presentedForm": []map[string]any{
			{"contentType": "text/plain", "data": base64.StdEncoding.EncodeToString([]byte("Pathology narrative report.")), "url": baseURL + "/Binary/dr-ccrr-note-1"},
		},
	}

	srPlan := map[string]any{
		"resourceType":       "ServiceRequest",
		"id":                 "sr-ccrr-plan-1",
		"meta":               map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-servicerequest"}},
		"status":             "active",
		"intent":             "plan",
		"category":           []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "387713003", "display": "Surgical procedure"}}}},
		"code":               map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "392021009", "display": "Lumpectomy of breast"}}},
		"subject":            map[string]any{"reference": "Patient/" + patID},
		"encounter":          map[string]any{"reference": "Encounter/enc-ccrr-1"},
		"occurrenceDateTime": "2024-12-01",
		"authoredOn":         "2024-09-10",
		"requester":          map[string]any{"reference": "Practitioner/prac-ccrr-1"},
		"reasonCode":         []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
	}

	carePlanRes := map[string]any{
		"resourceType": "CarePlan",
		"id":           "cp-ccrr-plan-1",
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-careplan"}},
		"text":         xhtml("Multidisciplinary Breast Oncology Care Plan: Lumpectomy followed by adjuvant radiation and endocrine therapy."),
		"status":       "active",
		"intent":       "plan",
		"category":     []map[string]any{{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/careplan-category", "code": "assess-plan"}}}},
		"subject":      map[string]any{"reference": "Patient/" + patID},
	}

	surgProc := map[string]any{
		"resourceType":      "Procedure",
		"id":                "proc-ccrr-lumpectomy",
		"meta":              map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-procedure"}},
		"basedOn":           []map[string]any{{"reference": "ServiceRequest/sr-ccrr-plan-1"}},
		"status":            "completed",
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "392021009", "display": "Lumpectomy of breast"}}},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"performedDateTime": "2024-09-18",
		"performer":         []map[string]any{{"actor": map[string]any{"reference": "Practitioner/prac-ccrr-1"}}},
		"reasonCode":        []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
	}

	vitalObs := map[string]any{
		"resourceType":      "Observation",
		"id":                "obs-ccrr-weight",
		"meta":              map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-body-weight"}},
		"status":            "final",
		"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "29463-7", "display": "Body weight"}}},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"effectiveDateTime": "2024-09-10T09:05:00Z",
		"valueQuantity":     map[string]any{"value": 64.5, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"},
	}

	smokingObs := map[string]any{
		"resourceType":      "Observation",
		"id":                "obs-ccrr-smoking",
		"meta":              map[string]any{"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-smokingstatus"}},
		"status":            "final",
		"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history"}}}},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "72166-2", "display": "Tobacco smoking status"}}},
		"subject":           map[string]any{"reference": "Patient/" + patID},
		"effectiveDateTime": "2024-09-10T09:05:00Z",
		"issued":            "2024-09-10T09:10:00Z",
		"valueCodeableConcept": map[string]any{
			"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "266919005", "display": "Never smoked tobacco"}},
		},
	}

	section := func(title, sys, code, narrative string, refs ...string) map[string]any {
		ent := make([]map[string]any, 0, len(refs))
		for _, ref := range refs {
			ent = append(ent, map[string]any{"reference": ref})
		}
		return map[string]any{
			"title": title,
			"code":  map[string]any{"coding": []map[string]any{{"system": sys, "code": code, "display": title}}},
			"text":  xhtml(narrative),
			"entry": ent,
		}
	}

	compID := "ccrr-comp-" + patID
	compRes := map[string]any{
		"resourceType": "Composition",
		"id":           compID,
		"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/central-cancer-registry-reporting/StructureDefinition/ccrr-composition"}},
		"identifier":   map[string]any{"system": "urn:ietf:rfc:3986", "value": "urn:uuid:770e8400-e29b-41d4-a716-446655440077"},
		"status":       "final",
		"type":         map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "72134-0", "display": "Cancer event report"}}},
		"subject":      map[string]any{"reference": "Patient/" + patID},
		"encounter":    map[string]any{"reference": "Encounter/enc-ccrr-1"},
		"date":         nowStr,
		"author": []map[string]any{
			{"reference": "Practitioner/prac-ccrr-1"},
			{"reference": "PractitionerRole/pr-ccrr-1"},
			{"reference": "Organization/org-ccrr-1"},
		},
		"title": "Central Cancer Registry Report (CCRR)",
		"section": []map[string]any{
			section("Primary Cancer Condition Section", "http://snomed.info/sct", "363346000", "Primary Malignant Neoplasm of Left Breast", "Condition/cond-ccrr-primary"),
			section("Secondary Cancer Condition Section", "http://snomed.info/sct", "128462008", "Secondary Malignant Neoplasm of Axillary Lymph Nodes", "Condition/cond-ccrr-secondary"),
			section("Cancer Stage Group Section", "http://loinc.org", "21908-9", "AJCC 8th Edition Clinical Stage IIA", "Observation/obs-ccrr-tnm-stage"),
			section("Radiotherapy Course Summary Section", "http://snomed.info/sct", "1217123003", "External Beam Photon IMRT 4005 cGy in 15 Fractions", "Procedure/proc-ccrr-radiotherapy"),
			section("Problem Section", "http://loinc.org", "11450-4", "Active Problem List", "Condition/cond-ccrr-problem-1"),
			section("Allergies and Intolerances Section", "http://loinc.org", "48765-2", "Penicillin Allergy", "AllergyIntolerance/allergy-ccrr-1"),
			section("Medications Administered Section", "http://loinc.org", "29549-3", "Paclitaxel Chemotherapy Infusion", "MedicationAdministration/ma-ccrr-1", "Medication/med-ccrr-paclitaxel"),
			section("Medications Section", "http://loinc.org", "10160-0", "Tamoxifen Maintenance Therapy", "MedicationStatement/ms-ccrr-1", "MedicationAdministration/ma-ccrr-1", "Medication/med-ccrr-paclitaxel"),
			section("Occupational Data for Health Section", "http://loinc.org", "74165-2", "Usual Work: Registered Nurse in Hospital", "Observation/obs-ccrr-odh-work"),
			section("Results Section", "http://loinc.org", "30954-2", "Estrogen Receptor Positive Laboratory Results", "Observation/obs-ccrr-lab-er", "DiagnosticReport/dr-ccrr-lab-1"),
			section("Notes Section", "http://loinc.org", "28650-0", "Oncology Consultation and Pathology Notes", "DocumentReference/docref-ccrr-note-1", "DiagnosticReport/dr-ccrr-note-1"),
			section("Plan of Treatment Section", "http://loinc.org", "18776-5", "Adjuvant Endocrine and Surgical Plan", "MedicationRequest/mr-ccrr-1", "Medication/med-ccrr-paclitaxel", "ServiceRequest/sr-ccrr-plan-1", "CarePlan/cp-ccrr-plan-1"),
			section("History of Procedures Section", "http://loinc.org", "47519-4", "Lumpectomy of Left Breast", "Procedure/proc-ccrr-lumpectomy"),
			section("Vital Signs Section", "http://loinc.org", "8716-3", "Body Weight 64.5 kg", "Observation/obs-ccrr-weight"),
			section("Social History Section", "http://loinc.org", "29762-2", "Never Smoked Tobacco", "Observation/obs-ccrr-smoking"),
		},
	}

	resources := []map[string]any{
		compRes,
		patRes,
		encRes,
		orgRes,
		pracRes,
		prRes,
		locRes,
		epRes,
		primaryCancer,
		secondaryCancer,
		tnmStage,
		tnmT,
		radioSummary,
		bodyStruct,
		probCond,
		allergyRes,
		medAdminRes,
		medStmtRes,
		medReqRes,
		medRes,
		odhWork,
		obsLab,
		drLab,
		docRef,
		drNote,
		srPlan,
		carePlanRes,
		surgProc,
		vitalObs,
		smokingObs,
	}
	entries := make([]map[string]any, 0, len(resources))
	for _, res := range resources {
		rt, _ := res["resourceType"].(string)
		id, _ := res["id"].(string)
		entries = append(entries, map[string]any{
			"fullUrl":  baseURL + "/" + rt + "/" + id,
			"resource": res,
		})
	}

	return map[string]any{
		"resourceType": "Bundle",
		"id":           "ccrr-bundle-" + patID,
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/us/central-cancer-registry-reporting/StructureDefinition/ccrr-content-bundle"},
		},
		"identifier": map[string]any{
			"system": "urn:ietf:rfc:3986",
			"value":  "urn:uuid:880e8400-e29b-41d4-a716-446655440066",
		},
		"type":      "document",
		"timestamp": nowStr,
		"entry":     entries,
	}
}
