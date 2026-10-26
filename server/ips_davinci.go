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

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/auth"
	automq "github.com/flint-fhir/flint/pkg/cdc"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/store/postgres"
)

// handlePatientSummary implements GET|POST /fhir/r4/{tenant}/Patient/{id}/$summary
// per the HL7 FHIR R4 International Patient Summary (IPS) Implementation Guide.
// It assembles a FHIR document Bundle whose first entry is a Composition resource
// (LOINC 60591-5 "Patient summary Document") followed by the Patient and all
// clinical compartment resources organized into standard IPS sections.
func (s *Server) handlePatientSummary(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	patientID := cleanID(r.PathValue("id"))
	if patientID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Patient ID is required for $summary")
		return
	}
	bundle, status, code, msg := s.assembleIPSSummaryBundle(r, tenant, patientID)
	if status != http.StatusOK {
		writeOperationOutcome(w, status, code, msg)
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(bundle)
}

func (s *Server) assembleIPSSummaryBundle(r *http.Request, tenant, patientID string) (map[string]any, int, string, string) {
	if s.store == nil {
		return nil, http.StatusServiceUnavailable, "exception", "store not configured"
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		if cleanID(secCtx.GetPatientId()) != patientID {
			return nil, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot access $summary for Patient/%s",
					secCtx.GetPatientId(), patientID)
		}
	}

	sr, err := s.store.PatientEverything(r.Context(), postgres.PatientEverythingParams{
		TenantID:  tenant,
		PatientID: patientID,
		Count:     500,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, http.StatusNotFound, "not-found", fmt.Sprintf("Patient/%s not found", patientID)
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		return nil, http.StatusGone, "deleted", fmt.Sprintf("Patient/%s has been deleted", patientID)
	}
	if err != nil {
		s.logger.Error("patient $summary everything query", "error", err, "patient", patientID)
		return nil, http.StatusInternalServerError, "exception", "failed to assemble Patient/$summary"
	}

	if hasSecCtx && len(sr.Includes) > 0 {
		sr.Includes = s.filterAuthorizedIncludes(r, tenant, secCtx, sr.Includes)
	}

	var patientRes map[string]any
	if len(sr.Matches) > 0 {
		if jb, err := s.protoToJSONWithMeta("Patient", sr.Matches[0].ResourceProto, sr.Matches[0].ResVersion, sr.Matches[0].LastUpdated); err == nil {
			_ = json.Unmarshal(jb, &patientRes)
		}
	}
	if patientRes == nil {
		patientRes = map[string]any{
			"resourceType": "Patient",
			"id":           patientID,
		}
	}

	var (
		medRefs        []map[string]any
		allergyRefs    []map[string]any
		problemRefs    []map[string]any
		procedureRefs  []map[string]any
		immunRefs      []map[string]any
		vitalRefs      []map[string]any
		resultRefs     []map[string]any
		planRefs       []map[string]any
		authorRef      string
		clinicalBundle []map[string]any
	)

	for _, inc := range sr.Includes {
		jb, err := s.protoToJSONWithMeta(inc.ResType, inc.ResourceProto, inc.ResVersion, inc.LastUpdated)
		if err != nil {
			continue
		}
		var res map[string]any
		if err := json.Unmarshal(jb, &res); err != nil {
			continue
		}
		ref := map[string]any{
			"reference": fmt.Sprintf("%s/%s", inc.ResType, inc.ResID),
		}
		clinicalBundle = append(clinicalBundle, map[string]any{
			"fullUrl":  fmt.Sprintf("%s/%s", inc.ResType, inc.ResID),
			"resource": res,
		})

		switch inc.ResType {
		case "MedicationRequest", "MedicationStatement", "MedicationAdministration", "MedicationDispense":
			medRefs = append(medRefs, ref)
		case "AllergyIntolerance":
			allergyRefs = append(allergyRefs, ref)
		case "Condition":
			problemRefs = append(problemRefs, ref)
		case "Procedure":
			procedureRefs = append(procedureRefs, ref)
		case "Immunization":
			immunRefs = append(immunRefs, ref)
		case "Observation":
			if isVitalSignObservation(res) {
				vitalRefs = append(vitalRefs, ref)
			} else {
				resultRefs = append(resultRefs, ref)
			}
		case "DiagnosticReport":
			resultRefs = append(resultRefs, ref)
		case "CarePlan", "Goal", "ServiceRequest":
			planRefs = append(planRefs, ref)
		case "Practitioner", "Organization":
			if authorRef == "" {
				authorRef = fmt.Sprintf("%s/%s", inc.ResType, inc.ResID)
			}
		}
	}

	if authorRef == "" {
		authorRef = fmt.Sprintf("Patient/%s", patientID)
	}

	makeSection := func(title, loincCode, loincDisplay string, entries []map[string]any, required bool) (map[string]any, bool) {
		if len(entries) == 0 && !required {
			return nil, false
		}
		sec := map[string]any{
			"title": title,
			"code": map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://loinc.org",
						"code":    loincCode,
						"display": loincDisplay,
					},
				},
			},
			"text": map[string]any{
				"status": "generated",
				"div":    fmt.Sprintf(`<div xmlns="http://www.w3.org/1999/xhtml">%s (%d entries)</div>`, title, len(entries)),
			},
		}
		if len(entries) > 0 {
			sec["entry"] = entries
		} else {
			sec["emptyReason"] = map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://terminology.hl7.org/CodeSystem/list-empty-reason",
						"code":    "nilknown",
						"display": "Nil Known",
					},
				},
			}
		}
		return sec, true
	}

	var sections []any
	if sec, ok := makeSection("Medication Summary", "10160-0", "History of Medication use Narrative", medRefs, true); ok {
		sections = append(sections, sec)
	}
	if sec, ok := makeSection("Allergies and Intolerances", "48765-2", "Allergies and adverse reactions Document", allergyRefs, true); ok {
		sections = append(sections, sec)
	}
	if sec, ok := makeSection("Active Problems", "11450-4", "Problem list - Reported", problemRefs, true); ok {
		sections = append(sections, sec)
	}
	if sec, ok := makeSection("History of Procedures", "47519-4", "History of Procedures Document", procedureRefs, false); ok {
		sections = append(sections, sec)
	}
	if sec, ok := makeSection("Immunizations", "11369-6", "History of Immunization Narrative", immunRefs, false); ok {
		sections = append(sections, sec)
	}
	if sec, ok := makeSection("Vital Signs", "8716-3", "Vital signs", vitalRefs, false); ok {
		sections = append(sections, sec)
	}
	if sec, ok := makeSection("Diagnostic Results", "30954-2", "Relevant diagnostic tests/laboratory data Narrative", resultRefs, false); ok {
		sections = append(sections, sec)
	}
	if sec, ok := makeSection("Plan of Treatment", "18776-5", "Plan of care note", planRefs, false); ok {
		sections = append(sections, sec)
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	compID := "ips-" + patientID
	composition := map[string]any{
		"resourceType": "Composition",
		"id":           compID,
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/uv/ips/StructureDefinition/Composition-uv-ips"},
		},
		"status": "final",
		"type": map[string]any{
			"coding": []any{
				map[string]any{
					"system":  "http://loinc.org",
					"code":    "60591-5",
					"display": "Patient summary Document",
				},
			},
		},
		"subject": map[string]any{
			"reference": "Patient/" + patientID,
		},
		"date":  nowStr,
		"title": "International Patient Summary",
		"author": []any{
			map[string]any{
				"reference": authorRef,
			},
		},
		"section": sections,
	}

	entries := make([]any, 0, 2+len(clinicalBundle))
	entries = append(entries,
		map[string]any{
			"fullUrl":  "Composition/" + compID,
			"resource": composition,
		},
		map[string]any{
			"fullUrl":  "Patient/" + patientID,
			"resource": patientRes,
		},
	)
	for _, cb := range clinicalBundle {
		entries = append(entries, cb)
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"id":           "ips-bundle-" + patientID,
		"meta": map[string]any{
			"profile": []string{"http://hl7.org/fhir/uv/ips/StructureDefinition/Bundle-uv-ips"},
		},
		"identifier": map[string]any{
			"system": "urn:ietf:rfc:3986",
			"value":  "urn:uuid:ips-" + patientID,
		},
		"type":      "document",
		"timestamp": nowStr,
		"entry":     entries,
	}
	return bundle, http.StatusOK, "", ""
}

func isVitalSignObservation(obs map[string]any) bool {
	if cats, ok := obs["category"].([]any); ok {
		for _, c := range cats {
			if cm, ok := c.(map[string]any); ok {
				if codings, ok := cm["coding"].([]any); ok {
					for _, cd := range codings {
						if cdm, ok := cd.(map[string]any); ok {
							if code, _ := cdm["code"].(string); strings.EqualFold(code, "vital-signs") {
								return true
							}
						}
					}
				}
			}
		}
	}
	if codeObj, ok := obs["code"].(map[string]any); ok {
		if codings, ok := codeObj["coding"].([]any); ok {
			for _, cd := range codings {
				if cdm, ok := cd.(map[string]any); ok {
					switch cdm["code"] {
					case "85354-9", "8480-6", "8462-4", "96607-7", "96608-5", "96609-3", "8867-4", "8310-5", "9279-1", "2708-6", "59408-5", "29463-7", "8302-2", "39156-5":
						return true
					}
				}
			}
		}
	}
	return false
}

// handlePatientMemberMatch implements POST /fhir/r4/{tenant}/Patient/$member-match
// per the HL7 Da Vinci Payer Data Exchange (PDex) / HRex Member Match specification.
func (s *Server) handlePatientMemberMatch(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil || len(body) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Parameters request body is required for Patient/$member-match")
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("malformed JSON in Patient/$member-match: %v", err))
		return
	}
	if rt, _ := payload["resourceType"].(string); rt != "Parameters" {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "Patient/$member-match requires a Parameters resource body")
		return
	}

	var memberPatient map[string]any
	var coverageToMatch map[string]any
	if paramsList, ok := payload["parameter"].([]any); ok {
		for _, rawP := range paramsList {
			pm, ok := rawP.(map[string]any)
			if !ok {
				continue
			}
			name, _ := pm["name"].(string)
			res, _ := pm["resource"].(map[string]any)
			if res == nil {
				continue
			}
			switch name {
			case "MemberPatient":
				memberPatient = res
			case "CoverageToMatch":
				coverageToMatch = res
			}
		}
	}

	if memberPatient == nil || coverageToMatch == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"Patient/$member-match requires both MemberPatient and CoverageToMatch parameters")
		return
	}

	inDemo := extractPatientDemographics(memberPatient)
	if inDemo.isEmpty() {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"MemberPatient must include demographic attributes (name, birthDate, or identifier)")
		return
	}

	covSubscriberID, _ := coverageToMatch["subscriberId"].(string)
	covSubscriberID = strings.TrimSpace(covSubscriberID)
	var covIdentifiers []string
	if ids, ok := coverageToMatch["identifier"].([]any); ok {
		for _, rawID := range ids {
			if im, ok := rawID.(map[string]any); ok {
				val, _ := im["value"].(string)
				if val != "" {
					covIdentifiers = append(covIdentifiers, strings.ToLower(strings.TrimSpace(val)))
				}
			}
		}
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	patientRestricted := hasSecCtx && auth.IsPatientRestricted(secCtx)
	allowedPatientID := ""
	if patientRestricted {
		allowedPatientID = cleanID(secCtx.GetPatientId())
	}

	// Fetch active Coverage resources in the tenant to check subscriberId / identifier links.
	// Pre-filter via spidx_token (identifier / subscriber-id) when CoverageToMatch specifies
	// a subscriberId or identifier, with fallback to a bounded scan for legacy/differently-cased rows.
	covSearchParams := postgres.SearchParams{
		TenantID: tenant,
		ResType:  "Coverage",
		Count:    200,
	}
	if patientRestricted {
		covSearchParams.References = map[string]string{"patient": "Patient/" + allowedPatientID}
	}
	var covTokenParts []string
	seenCovTok := make(map[string]bool)
	addCovTok := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		for _, cand := range []string{v, strings.ToLower(v), strings.ToUpper(v)} {
			if !seenCovTok[cand] {
				seenCovTok[cand] = true
				covTokenParts = append(covTokenParts, cand)
			}
		}
	}
	addCovTok(covSubscriberID)
	for _, cid := range covIdentifiers {
		addCovTok(cid)
	}
	var covSR *postgres.SearchResults
	if len(covTokenParts) > 0 {
		filteredCovParams := covSearchParams
		filteredCovParams.Tokens = map[string]string{"identifier": strings.Join(covTokenParts, ",")}
		covSR, _ = s.store.Search(r.Context(), filteredCovParams)
	}
	if covSR == nil || len(covSR.Matches) == 0 {
		covSR, _ = s.store.Search(r.Context(), covSearchParams)
	}
	coveragePatients := make(map[string]string) // patientID -> memberIdentifierValue
	if covSR != nil {
		for _, m := range covSR.Matches {
			jb, err := s.protoToJSONWithMeta("Coverage", m.ResourceProto, m.ResVersion, m.LastUpdated)
			if err != nil {
				continue
			}
			var cov map[string]any
			if err := json.Unmarshal(jb, &cov); err != nil {
				continue
			}
			patID := extractCoverageBeneficiaryPatientID(cov)
			if patID == "" || (patientRestricted && cleanID(patID) != allowedPatientID) {
				continue
			}
			subID, _ := cov["subscriberId"].(string)
			subID = strings.TrimSpace(subID)
			matchedCov := false
			memberVal := subID
			if covSubscriberID != "" && strings.EqualFold(subID, covSubscriberID) {
				matchedCov = true
			}
			if !matchedCov && len(covIdentifiers) > 0 {
				if cids, ok := cov["identifier"].([]any); ok {
					for _, rawCID := range cids {
						if cim, ok := rawCID.(map[string]any); ok {
							v, _ := cim["value"].(string)
							vTrim := strings.TrimSpace(v)
							for _, want := range covIdentifiers {
								if strings.EqualFold(vTrim, want) || (covSubscriberID != "" && strings.EqualFold(vTrim, covSubscriberID)) {
									matchedCov = true
									if memberVal == "" {
										memberVal = vTrim
									}
									break
								}
							}
						}
					}
				}
			}
			if matchedCov {
				if memberVal == "" {
					memberVal = patID
				}
				coveragePatients[patID] = memberVal
			}
		}
	}

	// Fetch candidate Patients in the tenant pre-filtered via spidx_* indexes
	var candIdentifiers []string
	for _, idKey := range inDemo.identifiers {
		if idKey.code != "" {
			candIdentifiers = append(candIdentifiers, idKey.code)
		}
	}
	if covSubscriberID != "" {
		candIdentifiers = append(candIdentifiers, covSubscriberID)
	}
	var extraPatIDs []string
	for patID := range coveragePatients {
		extraPatIDs = append(extraPatIDs, patID)
	}
	patCandParams := postgres.PatientMatchCandidateParams{
		TenantID:          tenant,
		ExtraCandidateIDs: extraPatIDs,
		Identifiers:       candIdentifiers,
		Families:          inDemo.families,
		Givens:            inDemo.givens,
		BirthDate:         inDemo.birthDate,
		Limit:             1000,
	}
	if patientRestricted {
		patCandParams.RestrictedPatientID = allowedPatientID
	}
	patCandidates, err := s.store.SearchPatientMatchCandidates(r.Context(), patCandParams)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to search Patients for $member-match")
		return
	}

	type matchedMember struct {
		patientID  string
		memberID   string
		memberSys  string
		confidence float64
	}
	var matches []matchedMember

	for _, m := range patCandidates {
		if patientRestricted && cleanID(m.ResID) != allowedPatientID {
			continue
		}
		if hasSecCtx && !s.resourceAllowedByGranularScope(secCtx, tenant, "Patient", m.ResID, m.ResourceProto) {
			continue
		}
		jb, err := s.protoToJSONWithMeta("Patient", m.ResourceProto, m.ResVersion, m.LastUpdated)
		if err != nil {
			continue
		}
		var candMap map[string]any
		if err := json.Unmarshal(jb, &candMap); err != nil {
			continue
		}
		candDemo := extractPatientDemographics(candMap)
		demoScore := scorePatientCandidate(inDemo, candDemo)
		covMemberID, hasCovMatch := coveragePatients[m.ResID]

		// Require either:
		// 1. Coverage match + demographic plausibility (score >= 0.50), OR
		// 2. High-confidence demographic/identifier match (score >= 0.70) when no Coverage resources exist or subscriberId matches Patient identifier
		qualifies := false
		if hasCovMatch && demoScore >= 0.50 {
			qualifies = true
		} else if demoScore >= 0.70 {
			if len(coveragePatients) == 0 || covSubscriberID == "" {
				qualifies = true
			} else {
				// Also check if CoverageToMatch.subscriberId matches one of the Patient's identifiers directly
				for _, pid := range candDemo.identifiers {
					if pid.code == strings.ToLower(covSubscriberID) {
						qualifies = true
						covMemberID = covSubscriberID
						break
					}
				}
			}
		}

		if qualifies {
			memVal := covMemberID
			memSys := "http://hl7.org/fhir/sid/us-mb"
			if ids, ok := candMap["identifier"].([]any); ok {
				for _, rawID := range ids {
					if im, ok := rawID.(map[string]any); ok {
						v, _ := im["value"].(string)
						sys, _ := im["system"].(string)
						if v != "" {
							if memVal == "" {
								memVal = v
							}
							if sys != "" {
								memSys = sys
							}
							break
						}
					}
				}
			}
			if memVal == "" {
				if covSubscriberID != "" {
					memVal = covSubscriberID
				} else {
					memVal = m.ResID
				}
			}
			matches = append(matches, matchedMember{
				patientID:  m.ResID,
				memberID:   memVal,
				memberSys:  memSys,
				confidence: demoScore,
			})
		}
	}

	if len(matches) == 0 {
		writeOperationOutcome(w, http.StatusUnprocessableEntity, "not-found",
			"No matching member found for Patient/$member-match criteria")
		return
	}
	if len(matches) > 1 {
		writeOperationOutcome(w, http.StatusUnprocessableEntity, "multiple-matches",
			fmt.Sprintf("Multiple (%d) ambiguous member matches found for Patient/$member-match criteria", len(matches)))
		return
	}

	winner := matches[0]
	respParams := map[string]any{
		"resourceType": "Parameters",
		"parameter": []any{
			map[string]any{
				"name": "MemberIdentifier",
				"valueIdentifier": map[string]any{
					"type": map[string]any{
						"coding": []any{
							map[string]any{
								"system":  "http://terminology.hl7.org/CodeSystem/v2-0203",
								"code":    "MB",
								"display": "Member Number",
							},
						},
					},
					"system": winner.memberSys,
					"value":  winner.memberID,
				},
			},
			map[string]any{
				"name": "Patient",
				"valueReference": map[string]any{
					"reference": "Patient/" + winner.patientID,
				},
			},
		},
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(respParams)
}

func extractCoverageBeneficiaryPatientID(cov map[string]any) string {
	if ben, ok := cov["beneficiary"].(map[string]any); ok {
		if ref, _ := ben["reference"].(string); ref != "" {
			return cleanID(ref)
		}
	}
	return ""
}

// handleClaimSubmit implements POST /fhir/r4/{tenant}/Claim/$submit
// per HL7 FHIR R4 and the Da Vinci Prior Authorization Support (PAS) IG.
// It persists the submitted Claim (and any supporting resources when wrapped in a Bundle)
// and generates, persists, and returns a ClaimResponse.
func (s *Server) handleClaimSubmit(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")

	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil || len(body) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "request body is required for Claim/$submit")
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("malformed JSON in Claim/$submit: %v", err))
		return
	}

	var claimMap map[string]any
	var supportingResources []map[string]any

	switch payload["resourceType"] {
	case "Claim":
		claimMap = payload
	case "Bundle":
		claimMap, supportingResources = extractClaimFromBundle(payload)
	case "Parameters":
		if paramsList, ok := payload["parameter"].([]any); ok {
			for _, rawP := range paramsList {
				pm, ok := rawP.(map[string]any)
				if !ok {
					continue
				}
				res, _ := pm["resource"].(map[string]any)
				if res == nil {
					continue
				}
				switch res["resourceType"] {
				case "Claim":
					claimMap = res
				case "Bundle":
					claimMap, supportingResources = extractClaimFromBundle(res)
				}
			}
		}
	}

	if claimMap == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"Claim/$submit requires a Claim resource (directly, inside a Bundle, or in a Parameters.resource parameter)")
		return
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		claimPatientID := ""
		if patRef, ok := claimMap["patient"].(map[string]any); ok {
			if refStr, _ := patRef["reference"].(string); refStr != "" {
				claimPatientID = cleanID(refStr)
			}
		}
		if claimPatientID == "" || claimPatientID != cleanID(secCtx.GetPatientId()) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"access denied: Claim patient does not match restricted patient compartment")
			return
		}
	}

	// Verify scopes and compartment rules on any supporting resources from a PAS Bundle before writing
	for _, supRes := range supportingResources {
		supType, _ := supRes["resourceType"].(string)
		if supType == "" || supType == "Claim" {
			continue
		}
		if hasSecCtx {
			if !auth.Allows(secCtx, supType, auth.ActionCreate) && !auth.Allows(secCtx, supType, auth.ActionUpdate) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope to persist supporting %s resource in Claim/$submit Bundle", supType))
				return
			}
			if auth.IsPatientRestricted(secCtx) && !strings.EqualFold(supType, "Patient") && !postgres.IsPatientCompartmentResourceType(supType) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot create non-compartment supporting resource %s in Claim/$submit",
						secCtx.GetPatientId(), supType))
				return
			}
		}
	}

	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}

	// Persist any supporting resources from a PAS Bundle first
	for _, supRes := range supportingResources {
		supType, _ := supRes["resourceType"].(string)
		if supType == "" || supType == "Claim" {
			continue
		}
		if _, err := s.persistFHIRMapResource(r, tenant, supType, supRes); err != nil {
			if errors.Is(err, errForbiddenCompartment) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
				return
			}
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid supporting %s resource: %v", supType, err))
			return
		}
	}

	// Persist the submitted Claim
	claimID, err := s.persistFHIRMapResource(r, tenant, "Claim", claimMap)
	if err != nil {
		if errors.Is(err, errForbiddenCompartment) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
			return
		}
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid Claim resource: %v", err))
		return
	}

	useStr, _ := claimMap["use"].(string)
	if useStr == "" {
		useStr = "preauthorization"
	}
	claimType := claimMap["type"]
	if claimType == nil {
		claimType = map[string]any{
			"coding": []any{
				map[string]any{
					"system": "http://terminology.hl7.org/CodeSystem/claim-type",
					"code":   "professional",
				},
			},
		}
	}
	patientRef := claimMap["patient"]
	if patientRef == nil {
		patientRef = map[string]any{"reference": "Patient/unknown"}
	}
	insurerRef := claimMap["insurer"]
	if insurerRef == nil {
		insurerRef = map[string]any{"display": "Flint Health Plan"}
	}

	var responseItems []any
	if items, ok := claimMap["item"].([]any); ok {
		for idx, rawItem := range items {
			im, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			seq := idx + 1
			if sNum, ok := im["sequence"].(float64); ok && sNum > 0 {
				seq = int(sNum)
			}
			adjAmount := 100.0
			currency := "USD"
			if net, ok := im["net"].(map[string]any); ok {
				if val, ok := net["value"].(float64); ok {
					adjAmount = val
				}
				if cur, ok := net["currency"].(string); ok && cur != "" {
					currency = cur
				}
			} else if up, ok := im["unitPrice"].(map[string]any); ok {
				if val, ok := up["value"].(float64); ok {
					adjAmount = val
				}
				if cur, ok := up["currency"].(string); ok && cur != "" {
					currency = cur
				}
			}
			responseItems = append(responseItems, map[string]any{
				"itemSequence": seq,
				"adjudication": []any{
					map[string]any{
						"category": map[string]any{
							"coding": []any{
								map[string]any{
									"system":  "http://terminology.hl7.org/CodeSystem/adjudication",
									"code":    "submitted",
									"display": "Submitted Amount",
								},
							},
						},
						"amount": map[string]any{
							"value":    adjAmount,
							"currency": currency,
						},
					},
					map[string]any{
						"category": map[string]any{
							"coding": []any{
								map[string]any{
									"system":  "http://terminology.hl7.org/CodeSystem/adjudication",
									"code":    "benefit",
									"display": "Benefit Amount",
								},
							},
						},
						"amount": map[string]any{
							"value":    adjAmount,
							"currency": currency,
						},
					},
				},
			})
		}
	}

	claimRespID := "claimresp-" + claimID
	claimResp := map[string]any{
		"resourceType": "ClaimResponse",
		"id":           claimRespID,
		"status":       "active",
		"type":         claimType,
		"use":          useStr,
		"patient":      patientRef,
		"created":      time.Now().UTC().Format(time.RFC3339),
		"insurer":      insurerRef,
		"request": map[string]any{
			"reference": "Claim/" + claimID,
		},
		"outcome":     "complete",
		"disposition": "Approved",
		"preAuthRef":  "PA-" + strings.ToUpper(claimID),
	}
	if len(responseItems) > 0 {
		claimResp["item"] = responseItems
	}

	_, _ = s.persistFHIRMapResource(r, tenant, "ClaimResponse", claimResp)

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(claimResp)
}

func extractClaimFromBundle(bundle map[string]any) (map[string]any, []map[string]any) {
	var claimMap map[string]any
	var supporting []map[string]any
	if entries, ok := bundle["entry"].([]any); ok {
		for _, rawE := range entries {
			em, ok := rawE.(map[string]any)
			if !ok {
				continue
			}
			res, ok := em["resource"].(map[string]any)
			if !ok {
				continue
			}
			if res["resourceType"] == "Claim" && claimMap == nil {
				claimMap = res
			} else {
				supporting = append(supporting, res)
			}
		}
	}
	return claimMap, supporting
}

func (s *Server) persistFHIRMapResource(r *http.Request, tenant, resType string, resMap map[string]any) (string, error) {
	md, ok := s.protoRegistry[resType]
	if !ok {
		return "", fmt.Errorf("resource type %s not registered", resType)
	}
	resID, _ := resMap["id"].(string)
	if resID == "" {
		resID = generateID()
		resMap["id"] = resID
	}
	rawJSON, err := json.Marshal(resMap)
	if err != nil {
		return "", err
	}
	if s.validator != nil {
		outcome := s.validator.ValidateJSON(resType, rawJSON)
		if !outcome.IsValid() {
			return "", fmt.Errorf("validation failed for %s/%s", resType, resID)
		}
	}
	normalizedBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawJSON)
	if err != nil {
		return "", err
	}
	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(normalizedBody, msg); err != nil {
		return "", err
	}
	setProtoResourceID(msg, resID)
	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		return "", err
	}
	var idx *postgres.SearchIndexes
	if extractor, ok := s.extractors[resType]; ok {
		idx, _ = extractor(tenant, resID, protoBytes)
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if auth.IsPatientRestricted(secCtx) {
			allowedPatientID := cleanID(secCtx.GetPatientId())
			if strings.EqualFold(resType, "Patient") || postgres.IsPatientCompartmentResourceType(resType) {
				if !postgres.IndexesBelongToPatientCompartment(resType, resID, idx, allowedPatientID) {
					return "", fmt.Errorf("%w: %s/%s does not belong to restricted patient compartment %s",
						errForbiddenCompartment, resType, resID, allowedPatientID)
				}
				if s.store != nil {
					exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), tenant, resType, resID, allowedPatientID)
					if err != nil {
						return "", err
					}
					if exists && !allowed {
						return "", fmt.Errorf("%w: cannot overwrite existing %s/%s belonging to another patient",
							errForbiddenCompartment, resType, resID)
					}
				}
			} else if resType != "Bundle" {
				return "", fmt.Errorf("%w: patient-scoped token (%s) cannot persist non-compartment resource %s/%s",
					errForbiddenCompartment, secCtx.GetPatientId(), resType, resID)
			}
		}
		if resType != "ClaimResponse" && !auth.Allows(secCtx, resType, auth.ActionUpdate) && s.store != nil {
			if rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID); err == nil && rec != nil && !rec.IsDeleted {
				return "", fmt.Errorf("%w: cannot overwrite existing %s/%s without update scope",
					errForbiddenCompartment, resType, resID)
			}
		}
	}

	rec, err := s.store.WriteResourceWithMeta(r.Context(), postgres.ResourceInput{
		TenantID:      tenant,
		ResType:       resType,
		ResID:         resID,
		ResourceProto: protoBytes,
		SearchIndexes: idx,
	})
	if err != nil {
		return "", err
	}
	s.publishCDC(r.Context(), automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     resID,
		ProtoData: rec.ResourceProto,
		Op:        "U",
	})
	return resID, nil
}
