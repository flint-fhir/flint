package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/store/postgres"
)

// handleQuestionnairePopulate implements POST/GET /fhir/r4/{tenant}/Questionnaire/$populate
// and /fhir/r4/{tenant}/Questionnaire/{id}/$populate per HL7 FHIR SDC $populate.
func (s *Server) handleQuestionnairePopulate(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	qID := r.PathValue("id")
	if s.store == nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
		return
	}

	subjectRef := strings.TrimSpace(r.URL.Query().Get("subject"))
	if subjectRef == "" {
		subjectRef = strings.TrimSpace(r.URL.Query().Get("patient"))
	}
	questionnaireURL := strings.TrimSpace(r.URL.Query().Get("questionnaire"))
	var questionnaireMap map[string]any

	if r.Method == http.MethodPost && r.Body != nil {
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if err == nil && len(bodyBytes) > 0 {
			var root map[string]any
			if json.Unmarshal(bodyBytes, &root) == nil {
				switch root["resourceType"] {
				case "Questionnaire":
					questionnaireMap = root
				case "Parameters":
					if params, ok := root["parameter"].([]any); ok {
						for _, rawP := range params {
							pm, ok := rawP.(map[string]any)
							if !ok {
								continue
							}
							name, _ := pm["name"].(string)
							switch name {
							case "subject", "patient":
								if refMap, ok := pm["valueReference"].(map[string]any); ok {
									if refStr, _ := refMap["reference"].(string); refStr != "" {
										subjectRef = strings.TrimSpace(refStr)
									}
								} else if sVal, _ := pm["valueString"].(string); sVal != "" {
									subjectRef = strings.TrimSpace(sVal)
								}
							case "questionnaire":
								if resMap, ok := pm["resource"].(map[string]any); ok && resMap["resourceType"] == "Questionnaire" {
									questionnaireMap = resMap
								} else if cVal, _ := pm["valueCanonical"].(string); cVal != "" {
									questionnaireURL = strings.TrimSpace(cVal)
								} else if uVal, _ := pm["valueUri"].(string); uVal != "" {
									questionnaireURL = strings.TrimSpace(uVal)
								} else if refMap, ok := pm["valueReference"].(map[string]any); ok {
									if refStr, _ := refMap["reference"].(string); refStr != "" {
										qID = strings.TrimPrefix(refStr, "Questionnaire/")
									}
								}
							case "questionnaireRef":
								if refMap, ok := pm["valueReference"].(map[string]any); ok {
									if refStr, _ := refMap["reference"].(string); refStr != "" {
										qID = strings.TrimPrefix(refStr, "Questionnaire/")
									}
								}
							}
						}
					}
				}
			}
		}
	}

	patientID := strings.TrimPrefix(subjectRef, "Patient/")
	if idx := strings.LastIndex(patientID, "/"); idx != -1 {
		patientID = patientID[idx+1:]
	}
	if patientID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Questionnaire/$populate requires a 'subject' (Patient/{id}) parameter")
		return
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx && auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) != cleanID(patientID) {
		writeOperationOutcome(w, http.StatusForbidden, "forbidden", "access denied: token is restricted to a different patient compartment")
		return
	}

	// Load Patient resource
	patRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Patient", patientID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Patient/%s not found", patientID))
			return
		}
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read Patient for $populate")
		return
	}
	if hasSecCtx && !s.resourceAllowedByGranularScope(secCtx, tenant, "Patient", patientID, patRec.ResourceProto) {
		writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to read Patient for Questionnaire/$populate")
		return
	}
	patJSON, err := s.protoToJSONWithMeta("Patient", patRec.ResourceProto, patRec.ResVersion, patRec.LastUpdated)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to serialize Patient for $populate")
		return
	}
	var patMap map[string]any
	_ = json.Unmarshal(patJSON, &patMap)

	// Load Questionnaire if not provided inline
	if questionnaireMap == nil && qID != "" && qID != "phq-9" && qID != "clinical-intake" {
		qRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Questionnaire", qID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, postgres.ErrResourceDeleted) {
				writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Questionnaire/%s not found", qID))
				return
			}
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read Questionnaire")
			return
		}
		if qBytes, err := s.protoToJSONWithMeta("Questionnaire", qRec.ResourceProto, qRec.ResVersion, qRec.LastUpdated); err == nil {
			_ = json.Unmarshal(qBytes, &questionnaireMap)
		}
	} else if questionnaireMap == nil && (qID == "phq-9" || qID == "clinical-intake") {
		if qRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Questionnaire", qID); err == nil {
			if qBytes, err := s.protoToJSONWithMeta("Questionnaire", qRec.ResourceProto, qRec.ResVersion, qRec.LastUpdated); err == nil {
				_ = json.Unmarshal(qBytes, &questionnaireMap)
			}
		}
	}
	if questionnaireMap == nil && questionnaireURL != "" {
		if sr, err := s.store.Search(r.Context(), postgres.SearchParams{
			TenantID: tenant,
			ResType:  "Questionnaire",
			Tokens:   map[string]string{"url": questionnaireURL},
			Count:    1,
		}); err == nil && len(sr.Matches) > 0 {
			m := sr.Matches[0]
			if qBytes, err := s.protoToJSONWithMeta("Questionnaire", m.ResourceProto, m.ResVersion, m.LastUpdated); err == nil {
				_ = json.Unmarshal(qBytes, &questionnaireMap)
			}
		}
	}
	if questionnaireMap == nil {
		questionnaireMap = defaultClinicalIntakeQuestionnaire(questionnaireURL)
	}

	// Fetch patient clinical context for pre-population
	evResults, _ := s.store.PatientEverything(r.Context(), postgres.PatientEverythingParams{
		TenantID:  tenant,
		PatientID: patientID,
		Count:     500,
	})
	var conditions, allergies, meds, observations []map[string]any
	if evResults != nil {
		includes := evResults.Includes
		if hasSecCtx && len(includes) > 0 {
			includes = s.filterAuthorizedIncludes(r, tenant, secCtx, includes)
		}
		for _, m := range includes {
			jb, err := s.protoToJSONWithMeta(m.ResType, m.ResourceProto, m.ResVersion, m.LastUpdated)
			if err != nil {
				continue
			}
			var rm map[string]any
			if json.Unmarshal(jb, &rm) != nil {
				continue
			}
			switch m.ResType {
			case "Condition":
				conditions = append(conditions, rm)
			case "AllergyIntolerance":
				allergies = append(allergies, rm)
			case "MedicationRequest":
				meds = append(meds, rm)
			case "Observation":
				observations = append(observations, rm)
			}
		}
	}

	canonicalURL, _ := questionnaireMap["url"].(string)
	if canonicalURL == "" {
		canonicalURL = questionnaireURL
	}
	if canonicalURL == "" && qID != "" {
		canonicalURL = "Questionnaire/" + qID
	}

	var qrItems []any
	if rawItems, ok := questionnaireMap["item"].([]any); ok {
		qrItems = populateQuestionnaireItems(rawItems, patMap, conditions, allergies, meds, observations)
	}

	qr := map[string]any{
		"resourceType": "QuestionnaireResponse",
		"id":           "qr-pop-" + patientID,
		"status":       "in-progress",
		"subject": map[string]any{
			"reference": "Patient/" + patientID,
		},
		"authored": time.Now().UTC().Format(time.RFC3339),
	}
	if canonicalURL != "" {
		qr["questionnaire"] = canonicalURL
	}
	if len(qrItems) > 0 {
		qr["item"] = qrItems
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(qr)
}

func defaultClinicalIntakeQuestionnaire(urlStr string) map[string]any {
	if urlStr == "" {
		urlStr = "http://flint.health/fhir/Questionnaire/clinical-intake"
	}
	return map[string]any{
		"resourceType": "Questionnaire",
		"id":           "clinical-intake",
		"url":          urlStr,
		"status":       "active",
		"title":        "Flint Standard Clinical Intake & Vital Signs Questionnaire",
		"item": []any{
			map[string]any{"linkId": "patient-name", "text": "Patient Full Name", "type": "string"},
			map[string]any{"linkId": "birthDate", "text": "Date of Birth", "type": "date"},
			map[string]any{"linkId": "gender", "text": "Administrative Gender", "type": "choice"},
			map[string]any{"linkId": "mrn", "text": "Medical Record Number", "type": "string"},
			map[string]any{"linkId": "active-conditions", "text": "Active Problem List", "type": "open-choice"},
			map[string]any{"linkId": "active-allergies", "text": "Known Allergies", "type": "open-choice"},
			map[string]any{"linkId": "active-medications", "text": "Current Medications", "type": "open-choice"},
			map[string]any{"linkId": "vital-heart-rate", "text": "Latest Heart Rate (/min)", "type": "quantity"},
		},
	}
}

func formatPatientDisplayName(pat map[string]any) string {
	names, ok := pat["name"].([]any)
	if !ok || len(names) == 0 {
		return ""
	}
	nm, ok := names[0].(map[string]any)
	if !ok {
		return ""
	}
	if txt, _ := nm["text"].(string); txt != "" {
		return txt
	}
	var parts []string
	if givens, ok := nm["given"].([]any); ok {
		for _, g := range givens {
			if gs, ok := g.(string); ok && gs != "" {
				parts = append(parts, gs)
			}
		}
	}
	if fam, _ := nm["family"].(string); fam != "" {
		parts = append(parts, fam)
	}
	return strings.Join(parts, " ")
}

func populateQuestionnaireItems(
	items []any,
	pat map[string]any,
	conditions, allergies, meds, observations []map[string]any,
) []any {
	var out []any
	for _, rawItem := range items {
		im, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		linkID, _ := im["linkId"].(string)
		text, _ := im["text"].(string)
		itemType, _ := im["type"].(string)
		lowerKey := strings.ToLower(linkID + " " + text)

		respItem := map[string]any{
			"linkId": linkID,
		}
		if text != "" {
			respItem["text"] = text
		}

		// Recurse for nested groups/items
		if childItems, ok := im["item"].([]any); ok && len(childItems) > 0 {
			populatedChildren := populateQuestionnaireItems(childItems, pat, conditions, allergies, meds, observations)
			if len(populatedChildren) > 0 {
				respItem["item"] = populatedChildren
			}
		}

		if itemType != "group" && itemType != "display" {
			answers := resolveItemAnswers(lowerKey, im, pat, conditions, allergies, meds, observations)
			if len(answers) > 0 {
				respItem["answer"] = answers
			}
		}

		if _, hasAns := respItem["answer"]; hasAns || respItem["item"] != nil {
			out = append(out, respItem)
		}
	}
	return out
}

func resolveItemAnswers(
	lowerKey string,
	itemDef map[string]any,
	pat map[string]any,
	conditions, allergies, meds, observations []map[string]any,
) []any {
	switch {
	case strings.Contains(lowerKey, "name") && !strings.Contains(lowerKey, "medication"):
		if nameStr := formatPatientDisplayName(pat); nameStr != "" {
			return []any{map[string]any{"valueString": nameStr}}
		}
	case strings.Contains(lowerKey, "birthdate") || strings.Contains(lowerKey, "birth-date") || strings.Contains(lowerKey, "dob") || strings.Contains(lowerKey, "date of birth"):
		if bd, _ := pat["birthDate"].(string); bd != "" {
			return []any{map[string]any{"valueDate": bd}}
		}
	case strings.Contains(lowerKey, "gender") || strings.Contains(lowerKey, "sex"):
		if g, _ := pat["gender"].(string); g != "" {
			if itemDef["type"] == "choice" || itemDef["type"] == "coding" {
				return []any{map[string]any{"valueCoding": map[string]any{"system": "http://hl7.org/fhir/administrative-gender", "code": g}}}
			}
			return []any{map[string]any{"valueString": g}}
		}
	case strings.Contains(lowerKey, "mrn") || strings.Contains(lowerKey, "identifier") || strings.Contains(lowerKey, "medical record"):
		if ids, ok := pat["identifier"].([]any); ok {
			for _, rawID := range ids {
				if idm, ok := rawID.(map[string]any); ok {
					if v, _ := idm["value"].(string); v != "" {
						return []any{map[string]any{"valueString": v}}
					}
				}
			}
		}
	case strings.Contains(lowerKey, "condition") || strings.Contains(lowerKey, "problem") || strings.Contains(lowerKey, "diagnosis"):
		var ans []any
		for _, c := range conditions {
			if coding := extractFirstCodingMap(c, "code"); coding != nil {
				ans = append(ans, map[string]any{"valueCoding": coding})
			}
		}
		if len(ans) > 0 {
			return ans
		}
	case strings.Contains(lowerKey, "allerg"):
		var ans []any
		for _, a := range allergies {
			if coding := extractFirstCodingMap(a, "code"); coding != nil {
				ans = append(ans, map[string]any{"valueCoding": coding})
			}
		}
		if len(ans) > 0 {
			return ans
		}
	case strings.Contains(lowerKey, "medication") || strings.Contains(lowerKey, "meds"):
		var ans []any
		for _, m := range meds {
			if coding := extractFirstCodingMap(m, "medicationCodeableConcept"); coding != nil {
				ans = append(ans, map[string]any{"valueCoding": coding})
			}
		}
		if len(ans) > 0 {
			return ans
		}
	case strings.Contains(lowerKey, "heart-rate") || strings.Contains(lowerKey, "heart rate") || strings.Contains(lowerKey, "8867-4"):
		for _, obs := range observations {
			if hasCodingCode(obs, "code", "8867-4") {
				if vq, ok := obs["valueQuantity"].(map[string]any); ok {
					return []any{map[string]any{"valueQuantity": vq}}
				}
			}
		}
	case strings.Contains(lowerKey, "hba1c") || strings.Contains(lowerKey, "4548-4"):
		for _, obs := range observations {
			if hasCodingCode(obs, "code", "4548-4") {
				if vq, ok := obs["valueQuantity"].(map[string]any); ok {
					return []any{map[string]any{"valueQuantity": vq}}
				}
			}
		}
	}

	// Fallback to Questionnaire.item.initial if present
	if initials, ok := itemDef["initial"].([]any); ok && len(initials) > 0 {
		return initials
	}
	return nil
}

func extractFirstCodingMap(res map[string]any, field string) map[string]any {
	cc, ok := res[field].(map[string]any)
	if !ok {
		return nil
	}
	codings, ok := cc["coding"].([]any)
	if !ok || len(codings) == 0 {
		return nil
	}
	if cm, ok := codings[0].(map[string]any); ok {
		return cm
	}
	return nil
}

func hasCodingCode(res map[string]any, field, wantCode string) bool {
	cc, ok := res[field].(map[string]any)
	if !ok {
		return false
	}
	codings, _ := cc["coding"].([]any)
	for _, rawC := range codings {
		if cm, ok := rawC.(map[string]any); ok {
			if c, _ := cm["code"].(string); strings.EqualFold(c, wantCode) {
				return true
			}
		}
	}
	return false
}

// handleQuestionnaireResponseExtract implements POST /fhir/r4/{tenant}/QuestionnaireResponse/$extract
// and /fhir/r4/{tenant}/QuestionnaireResponse/{id}/$extract per HL7 FHIR SDC $extract.
func (s *Server) handleQuestionnaireResponseExtract(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	qrID := r.PathValue("id")
	persist := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("persist")), "true")

	var qrMap map[string]any
	if r.Body != nil {
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if err == nil && len(bodyBytes) > 0 {
			var root map[string]any
			if json.Unmarshal(bodyBytes, &root) == nil {
				switch root["resourceType"] {
				case "QuestionnaireResponse":
					qrMap = root
				case "Parameters":
					if params, ok := root["parameter"].([]any); ok {
						for _, rawP := range params {
							pm, ok := rawP.(map[string]any)
							if !ok {
								continue
							}
							name, _ := pm["name"].(string)
							switch name {
							case "questionnaire-response", "questionnaireResponse", "resource":
								if resMap, ok := pm["resource"].(map[string]any); ok && resMap["resourceType"] == "QuestionnaireResponse" {
									qrMap = resMap
								}
							case "persist":
								if bVal, ok := pm["valueBoolean"].(bool); ok {
									persist = bVal
								} else if sVal, ok := pm["valueString"].(string); ok {
									persist = strings.EqualFold(sVal, "true")
								}
							}
						}
					}
				}
			}
		}
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())

	if qrMap == nil && qrID != "" && s.store != nil {
		rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "QuestionnaireResponse", qrID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, postgres.ErrResourceDeleted) {
				writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("QuestionnaireResponse/%s not found", qrID))
				return
			}
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read QuestionnaireResponse")
			return
		}
		if hasSecCtx && !s.resourceAllowedByGranularScope(secCtx, tenant, "QuestionnaireResponse", qrID, rec.ResourceProto) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to read QuestionnaireResponse for $extract")
			return
		}
		if jb, err := s.protoToJSONWithMeta("QuestionnaireResponse", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
			_ = json.Unmarshal(jb, &qrMap)
		}
	}

	if qrMap == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "QuestionnaireResponse/$extract requires a QuestionnaireResponse body, Parameters, or instance ID")
		return
	}

	if idStr, _ := qrMap["id"].(string); idStr != "" && qrID == "" {
		qrID = idStr
	}
	if qrID == "" {
		qrID = "qr-ext-1"
	}

	subjectRef, _ := qrMap["subject"].(map[string]any)
	if subjectRef == nil {
		subjectRef = map[string]any{"reference": "Patient/unknown"}
	}
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		refStr, _ := subjectRef["reference"].(string)
		subjPatientID := ""
		if strings.HasPrefix(refStr, "Patient/") || (!strings.Contains(refStr, "/") && refStr != "" && refStr != "unknown") {
			subjPatientID = cleanID(refStr)
		}
		if subjPatientID == "" || subjPatientID != cleanID(secCtx.GetPatientId()) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"access denied: QuestionnaireResponse subject does not match restricted patient compartment")
			return
		}
	}

	authored, _ := qrMap["authored"].(string)
	if authored == "" {
		authored = time.Now().UTC().Format(time.RFC3339)
	}

	var entries []any
	var extractedResources []map[string]any
	seq := 0
	var walkExtract func(items []any)
	walkExtract = func(items []any) {
		for _, rawItem := range items {
			im, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			linkID, _ := im["linkId"].(string)
			text, _ := im["text"].(string)
			defURI, _ := im["definition"].(string)
			lowerKey := strings.ToLower(linkID + " " + text + " " + defURI)

			if childItems, ok := im["item"].([]any); ok && len(childItems) > 0 {
				walkExtract(childItems)
			}

			answers, _ := im["answer"].([]any)
			if len(answers) == 0 {
				continue
			}
			// Skip pure patient demographic fields that already live on Patient
			if strings.Contains(lowerKey, "patient-name") || linkID == "birthDate" || linkID == "gender" || linkID == "mrn" {
				continue
			}

			for _, rawAns := range answers {
				am, ok := rawAns.(map[string]any)
				if !ok {
					continue
				}
				seq++
				switch {
				case strings.Contains(lowerKey, "condition") || strings.Contains(lowerKey, "problem") || strings.Contains(lowerKey, "diagnosis"):
					if vc, ok := am["valueCoding"].(map[string]any); ok {
						condRes := map[string]any{
							"resourceType": "Condition",
							"id":           fmt.Sprintf("ext-cond-%s-%d", qrID, seq),
							"clinicalStatus": map[string]any{
								"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}},
							},
							"verificationStatus": map[string]any{
								"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}},
							},
							"code":    map[string]any{"coding": []any{vc}},
							"subject": subjectRef,
						}
						extractedResources = append(extractedResources, condRes)
						entries = append(entries, map[string]any{
							"fullUrl":  "urn:uuid:" + uuid.NewString(),
							"resource": condRes,
							"request":  map[string]any{"method": "POST", "url": "Condition"},
						})
					}
				case strings.Contains(lowerKey, "allerg"):
					if vc, ok := am["valueCoding"].(map[string]any); ok {
						allRes := map[string]any{
							"resourceType": "AllergyIntolerance",
							"id":           fmt.Sprintf("ext-allergy-%s-%d", qrID, seq),
							"clinicalStatus": map[string]any{
								"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}},
							},
							"verificationStatus": map[string]any{
								"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification", "code": "confirmed"}},
							},
							"code":    map[string]any{"coding": []any{vc}},
							"patient": subjectRef,
						}
						extractedResources = append(extractedResources, allRes)
						entries = append(entries, map[string]any{
							"fullUrl":  "urn:uuid:" + uuid.NewString(),
							"resource": allRes,
							"request":  map[string]any{"method": "POST", "url": "AllergyIntolerance"},
						})
					}
				default:
					obsCode := map[string]any{
						"coding": []any{
							map[string]any{
								"system":  "http://loinc.org",
								"code":    inferObservationLOINCCode(lowerKey, linkID),
								"display": firstNonEmpty(text, linkID),
							},
						},
					}
					obsRes := map[string]any{
						"resourceType":      "Observation",
						"id":                fmt.Sprintf("ext-obs-%s-%d", qrID, seq),
						"status":            "final",
						"code":              obsCode,
						"subject":           subjectRef,
						"effectiveDateTime": authored,
						"derivedFrom": []any{
							map[string]any{"reference": "QuestionnaireResponse/" + qrID},
						},
					}
					if vq, ok := am["valueQuantity"].(map[string]any); ok {
						obsRes["valueQuantity"] = vq
						obsRes["category"] = []any{
							map[string]any{
								"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}},
							},
						}
					} else if vc, ok := am["valueCoding"].(map[string]any); ok {
						obsRes["valueCodeableConcept"] = map[string]any{"coding": []any{vc}}
						obsRes["category"] = []any{
							map[string]any{
								"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "survey"}},
							},
						}
					} else if vs, ok := am["valueString"].(string); ok {
						obsRes["valueString"] = vs
					} else if vb, ok := am["valueBoolean"].(bool); ok {
						obsRes["valueBoolean"] = vb
					} else if vi, ok := am["valueInteger"].(float64); ok {
						obsRes["valueInteger"] = int(vi)
					} else {
						continue
					}
					extractedResources = append(extractedResources, obsRes)
					entries = append(entries, map[string]any{
						"fullUrl":  "urn:uuid:" + uuid.NewString(),
						"resource": obsRes,
						"request":  map[string]any{"method": "POST", "url": "Observation"},
					})
				}
			}
		}
	}

	if items, ok := qrMap["item"].([]any); ok {
		walkExtract(items)
	}

	if persist {
		for _, resMap := range extractedResources {
			targetType, _ := resMap["resourceType"].(string)
			if hasSecCtx && !auth.Allows(secCtx, targetType, auth.ActionCreate) && !auth.Allows(secCtx, targetType, auth.ActionUpdate) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope to persist extracted %s resource", targetType))
				return
			}
		}
		if s.store == nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
			return
		}
		for _, resMap := range extractedResources {
			targetType, _ := resMap["resourceType"].(string)
			if _, err := s.persistFHIRMapResource(r, tenant, targetType, resMap); err != nil {
				if errors.Is(err, errForbiddenCompartment) {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
					return
				}
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("failed to persist extracted %s: %v", targetType, err))
				return
			}
		}
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"id":           "extract-" + qrID,
		"type":         "transaction",
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"entry":        entries,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(bundle)
}

func inferObservationLOINCCode(lowerKey, linkID string) string {
	switch {
	case strings.Contains(lowerKey, "heart-rate") || strings.Contains(lowerKey, "heart rate") || strings.Contains(lowerKey, "8867-4"):
		return "8867-4"
	case strings.Contains(lowerKey, "systolic") || strings.Contains(lowerKey, "8480-6"):
		return "8480-6"
	case strings.Contains(lowerKey, "diastolic") || strings.Contains(lowerKey, "8462-4"):
		return "8462-4"
	case strings.Contains(lowerKey, "hba1c") || strings.Contains(lowerKey, "4548-4"):
		return "4548-4"
	case strings.Contains(lowerKey, "bmi") || strings.Contains(lowerKey, "39156-5"):
		return "39156-5"
	case strings.Contains(lowerKey, "weight") || strings.Contains(lowerKey, "29463-7"):
		return "29463-7"
	default:
		if linkID != "" {
			return linkID
		}
		return "8867-4"
	}
}

// handleCompositionDocument implements GET/POST /fhir/r4/{tenant}/Composition/{id}/$document
// per HL7 FHIR R4 OperationDefinition/Composition-document.
func (s *Server) handleCompositionDocument(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	compID := r.PathValue("id")
	if compID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Composition ID is required for $document")
		return
	}
	if s.store == nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
		return
	}

	persist := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("persist")), "true")
	if r.Method == http.MethodPost && r.Body != nil {
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
		if err == nil && len(bodyBytes) > 0 {
			var root map[string]any
			if json.Unmarshal(bodyBytes, &root) == nil && root["resourceType"] == "Parameters" {
				if params, ok := root["parameter"].([]any); ok {
					for _, rawP := range params {
						if pm, ok := rawP.(map[string]any); ok && pm["name"] == "persist" {
							if bVal, ok := pm["valueBoolean"].(bool); ok {
								persist = bVal
							} else if sVal, ok := pm["valueString"].(string); ok {
								persist = strings.EqualFold(sVal, "true")
							}
						}
					}
				}
			}
		}
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx && persist && !auth.Allows(secCtx, "Bundle", auth.ActionCreate) && !auth.Allows(secCtx, "Bundle", auth.ActionUpdate) {
		writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to persist Composition/$document Bundle")
		return
	}

	compRec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Composition", compID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Composition/%s not found", compID))
			return
		}
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read Composition")
		return
	}
	if hasSecCtx && !s.resourceAllowedByGranularScope(secCtx, tenant, "Composition", compID, compRec.ResourceProto) {
		writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to read Composition for $document")
		return
	}
	compJSON, err := s.protoToJSONWithMeta("Composition", compRec.ResourceProto, compRec.ResVersion, compRec.LastUpdated)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to serialize Composition")
		return
	}
	var compMap map[string]any
	if err := json.Unmarshal(compJSON, &compMap); err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to decode Composition JSON")
		return
	}

	// Enforce patient compartment isolation if Composition.subject is a Patient
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		subjPatientID := ""
		if subj, ok := compMap["subject"].(map[string]any); ok {
			if ref, _ := subj["reference"].(string); strings.HasPrefix(ref, "Patient/") {
				subjPatientID = strings.TrimPrefix(ref, "Patient/")
			}
		}
		if cleanID(subjPatientID) != cleanID(secCtx.GetPatientId()) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "access denied: token is restricted to a different patient compartment")
			return
		}
	}

	var orderedRefs []string
	seenRefs := map[string]bool{
		"Composition/" + compID: true,
	}
	addRef := func(refObj any) {
		rm, ok := refObj.(map[string]any)
		if !ok {
			return
		}
		refStr, _ := rm["reference"].(string)
		refStr = strings.TrimSpace(refStr)
		if refStr == "" || strings.HasPrefix(refStr, "#") || strings.HasPrefix(refStr, "http") || strings.HasPrefix(refStr, "urn:") {
			return
		}
		if !seenRefs[refStr] {
			seenRefs[refStr] = true
			orderedRefs = append(orderedRefs, refStr)
		}
	}

	addRef(compMap["subject"])
	addRef(compMap["encounter"])
	if authors, ok := compMap["author"].([]any); ok {
		for _, a := range authors {
			addRef(a)
		}
	}
	addRef(compMap["custodian"])
	if attesters, ok := compMap["attester"].([]any); ok {
		for _, att := range attesters {
			if am, ok := att.(map[string]any); ok {
				addRef(am["party"])
			}
		}
	}

	var walkSections func(sections []any)
	walkSections = func(sections []any) {
		for _, rawSec := range sections {
			sm, ok := rawSec.(map[string]any)
			if !ok {
				continue
			}
			addRef(sm["focus"])
			if secAuthors, ok := sm["author"].([]any); ok {
				for _, a := range secAuthors {
					addRef(a)
				}
			}
			if secEntries, ok := sm["entry"].([]any); ok {
				for _, e := range secEntries {
					addRef(e)
				}
			}
			if childSecs, ok := sm["section"].([]any); ok {
				walkSections(childSecs)
			}
		}
	}
	if sections, ok := compMap["section"].([]any); ok {
		walkSections(sections)
	}

	entries := []any{
		map[string]any{
			"fullUrl":  "Composition/" + compID,
			"resource": compMap,
		},
	}

	for _, ref := range orderedRefs {
		parts := strings.SplitN(ref, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		resType, resID := parts[0], parts[1]
		if hasSecCtx && auth.IsPatientRestricted(secCtx) {
			allowedPatientID := cleanID(secCtx.GetPatientId())
			if strings.EqualFold(resType, "Patient") && cleanID(resID) != allowedPatientID {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot assemble document referencing Patient/%s outside patient compartment",
						secCtx.GetPatientId(), resID))
				return
			}
			if !strings.EqualFold(resType, "Patient") && postgres.IsPatientCompartmentResourceType(resType) {
				exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), tenant, resType, resID, allowedPatientID)
				if err != nil {
					writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
					return
				}
				if exists && !allowed {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("patient-scoped token (%s) cannot assemble document referencing %s/%s outside patient compartment",
							secCtx.GetPatientId(), resType, resID))
					return
				}
			}
		}
		rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
		if err != nil {
			continue
		}
		if hasSecCtx && !s.resourceAllowedByGranularScope(secCtx, tenant, resType, resID, rec.ResourceProto) {
			continue
		}
		jb, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
		if err != nil {
			continue
		}
		var rm map[string]any
		if json.Unmarshal(jb, &rm) != nil {
			continue
		}
		entries = append(entries, map[string]any{
			"fullUrl":  resType + "/" + resID,
			"resource": rm,
		})
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	docBundleID := "doc-" + compID
	docBundle := map[string]any{
		"resourceType": "Bundle",
		"id":           docBundleID,
		"identifier": map[string]any{
			"system": "urn:ietf:rfc:3986",
			"value":  "urn:uuid:" + uuid.NewString(),
		},
		"type":      "document",
		"timestamp": nowStr,
		"entry":     entries,
	}

	if persist {
		if _, err := s.persistFHIRMapResource(r, tenant, "Bundle", docBundle); err != nil {
			if errors.Is(err, errForbiddenCompartment) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
				return
			}
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("failed to persist document Bundle: %v", err))
			return
		}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(docBundle)
}
