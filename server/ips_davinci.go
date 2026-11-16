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
	"github.com/flint-fhir/flint/store"
)

// handlePatientSummary implements GET|POST /fhir/r4/{tenant}/Patient/{id}/$summary
// and GET|POST /fhir/r4/{tenant}/Patient/$summary
// per the HL7 FHIR R4 International Patient Summary (IPS) Implementation Guide.
// It assembles a FHIR document Bundle whose first entry is a Composition resource
// (LOINC 60591-5 "Patient summary Document") followed by the Patient and all
// clinical compartment resources organized into standard IPS sections.
func (s *Server) handlePatientSummary(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	patientID := cleanID(r.PathValue("id"))
	if patientID == "" {
		q := r.URL.Query()
		for _, k := range []string{"patient", "id", "subject"} {
			if v := strings.TrimSpace(q.Get(k)); v != "" {
				patientID = cleanID(strings.TrimPrefix(v, "Patient/"))
				break
			}
		}
	}
	if r.Method == http.MethodPost && r.Body != nil {
		if rawBody, err := io.ReadAll(io.LimitReader(r.Body, 2<<20)); err == nil && len(rawBody) > 0 {
			var bodyMap map[string]any
			if isProtoContentType(r) {
				bodyMap, _ = s.protoToFHIRMap("Parameters", rawBody)
			} else {
				_ = json.Unmarshal(rawBody, &bodyMap)
			}
			if bodyMap != nil && patientID == "" {
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						pm, ok := rawP.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "patient", "id", "subject":
							if v := extractParamString(pm); v != "" {
								patientID = cleanID(strings.TrimPrefix(v, "Patient/"))
							}
						}
					}
				}
			}
		}
	}
	if patientID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Patient ID is required for $summary")
		return
	}
	bundle, status, code, msg := s.assembleIPSSummaryBundle(r, tenant, patientID)
	if status != http.StatusOK {
		writeOperationOutcome(w, status, code, msg)
		return
	}

	s.writeTerminologyResourceResponse(w, r, "Bundle", bundle)
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

	sr, err := s.store.PatientEverything(r.Context(), store.PatientEverythingParams{
		TenantID:  tenant,
		PatientID: patientID,
		Count:     500,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, http.StatusNotFound, "not-found", fmt.Sprintf("Patient/%s not found", patientID)
	}
	if errors.Is(err, store.ErrResourceDeleted) {
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
		if m, err := s.protoToFHIRMapWithMeta("Patient", sr.Matches[0].ResourceProto, sr.Matches[0].ResVersion, sr.Matches[0].LastUpdated); err == nil {
			patientRes = m
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
		res, err := s.protoToFHIRMapWithMeta(inc.ResType, inc.ResourceProto, inc.ResVersion, inc.LastUpdated)
		if err != nil {
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

type hrexCoverageMatch struct {
	memberID        string
	memberSys       string
	assignerRef     string
	assignerDisplay string
}

func (s *Server) writeNegotiatedOutcome(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	s.writeOperationResponse(w, r, status, "OperationOutcome", map[string]any{
		"resourceType": "OperationOutcome",
		"issue": []any{
			map[string]any{
				"severity":    "error",
				"code":        code,
				"diagnostics": message,
			},
		},
	})
}

func parseHRexBoundTime(raw string, endOfDay bool) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		if endOfDay {
			return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, time.UTC), true
		}
		return t.UTC(), true
	}
	return time.Time{}, false
}

func validateHRexMemberMatchConsent(consent map[string]any, now time.Time) (string, bool) {
	status, _ := consent["status"].(string)
	if strings.TrimSpace(status) != "active" {
		return fmt.Sprintf("HRex Consent status must be 'active' for Patient/$member-match (got %q)", status), false
	}
	if prov, ok := consent["provision"].(map[string]any); ok {
		if pType, _ := prov["type"].(string); strings.EqualFold(strings.TrimSpace(pType), "deny") {
			return "HRex Consent provision.type 'deny' prohibits Patient/$member-match disclosure", false
		}
		if period, ok := prov["period"].(map[string]any); ok {
			if startStr, _ := period["start"].(string); startStr != "" {
				if startT, ok := parseHRexBoundTime(startStr, false); ok && now.Before(startT) {
					return fmt.Sprintf("HRex Consent provision.period.start (%s) is in the future", startStr), false
				}
			}
			if endStr, _ := period["end"].(string); endStr != "" {
				if endT, ok := parseHRexBoundTime(endStr, true); ok && now.After(endT) {
					return fmt.Sprintf("HRex Consent provision.period.end (%s) has expired", endStr), false
				}
			}
		}
	}
	return "", true
}

func cloneShallowFHIRMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src)+4)
	for k, v := range src {
		out[k] = v
	}
	return out
}

func extractPayorAssigner(cov map[string]any) (ref, display string) {
	payors, ok := cov["payor"].([]any)
	if !ok {
		return "", ""
	}
	for _, rawP := range payors {
		pm, ok := rawP.(map[string]any)
		if !ok {
			continue
		}
		r, _ := pm["reference"].(string)
		d, _ := pm["display"].(string)
		r = strings.TrimSpace(r)
		d = strings.TrimSpace(d)
		if r != "" || d != "" {
			return r, d
		}
	}
	return "", ""
}

// handlePatientMemberMatch implements POST /fhir/r4/{tenant}/Patient/$member-match
// per the HL7 Da Vinci Health Record Exchange (HRex) STU 1.0 / 1.1 Member Match specification.
func (s *Server) handlePatientMemberMatch(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	if s.store == nil {
		s.writeNegotiatedOutcome(w, r, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil || len(body) == 0 {
		s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "required", "Parameters request body is required for Patient/$member-match")
		return
	}

	payload, err := s.decodeOperationBodyMap(r, body, "Parameters")
	if err != nil {
		s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "structure", fmt.Sprintf("malformed payload in Patient/$member-match: %v", err))
		return
	}
	if rt, _ := payload["resourceType"].(string); rt != "Parameters" {
		s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "invalid", "Patient/$member-match requires a Parameters resource body")
		return
	}

	var (
		memberPatient   map[string]any
		coverageToMatch map[string]any
		coverageToLink  map[string]any
		consentRes      map[string]any
		mpCount         int
		ctmCount        int
		ctlCount        int
		consentCount    int
	)
	if paramsList, ok := payload["parameter"].([]any); ok {
		for _, rawP := range paramsList {
			pm, ok := rawP.(map[string]any)
			if !ok {
				continue
			}
			name, _ := pm["name"].(string)
			res, _ := pm["resource"].(map[string]any)
			switch name {
			case "MemberPatient":
				mpCount++
				if res == nil || res["resourceType"] != "Patient" {
					s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "invalid", "MemberPatient parameter must contain a Patient resource")
					return
				}
				memberPatient = res
			case "CoverageToMatch":
				ctmCount++
				if res == nil || res["resourceType"] != "Coverage" {
					s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "invalid", "CoverageToMatch parameter must contain a Coverage resource")
					return
				}
				coverageToMatch = res
			case "CoverageToLink":
				ctlCount++
				if res == nil || res["resourceType"] != "Coverage" {
					s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "invalid", "CoverageToLink parameter must contain a Coverage resource")
					return
				}
				coverageToLink = res
			case "Consent":
				consentCount++
				if res == nil || res["resourceType"] != "Consent" {
					s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "invalid", "Consent parameter must contain a Consent resource")
					return
				}
				consentRes = res
			}
		}
	}

	if mpCount > 1 || ctmCount > 1 || ctlCount > 1 || consentCount > 1 {
		s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "structure",
			"Patient/$member-match parameters MemberPatient, CoverageToMatch, CoverageToLink, and Consent have max cardinality 1")
		return
	}
	if memberPatient == nil || coverageToMatch == nil {
		s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "required",
			"Patient/$member-match requires both MemberPatient and CoverageToMatch parameters")
		return
	}

	if consentRes != nil {
		if reason, ok := validateHRexMemberMatchConsent(consentRes, time.Now().UTC()); !ok {
			s.writeNegotiatedOutcome(w, r, http.StatusUnprocessableEntity, "business-rule", reason)
			return
		}
	}

	inDemo := extractPatientDemographics(memberPatient)
	if inDemo.isEmpty() {
		s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "required",
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
	reqPayorRef, reqPayorDisp := extractPayorAssigner(coverageToMatch)

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	patientRestricted := hasSecCtx && auth.IsPatientRestricted(secCtx)
	allowedPatientID := ""
	if patientRestricted {
		allowedPatientID = cleanID(secCtx.GetPatientId())
	}

	if hasSecCtx {
		if coverageToLink != nil &&
			!auth.Allows(secCtx, "Coverage", auth.ActionCreate) &&
			!auth.Allows(secCtx, "Coverage", auth.ActionUpdate) &&
			!auth.Allows(secCtx, "Patient", auth.ActionCreate) &&
			!auth.Allows(secCtx, "Patient", auth.ActionUpdate) {
			s.writeNegotiatedOutcome(w, r, http.StatusForbidden, "forbidden",
				"insufficient scope to persist CoverageToLink in Patient/$member-match")
			return
		}
		if consentRes != nil &&
			!auth.Allows(secCtx, "Consent", auth.ActionCreate) &&
			!auth.Allows(secCtx, "Consent", auth.ActionUpdate) &&
			!auth.Allows(secCtx, "Patient", auth.ActionCreate) &&
			!auth.Allows(secCtx, "Patient", auth.ActionUpdate) {
			s.writeNegotiatedOutcome(w, r, http.StatusForbidden, "forbidden",
				"insufficient scope to persist Consent in Patient/$member-match")
			return
		}
	}

	// Fetch valid Coverage resources in the tenant to check subscriberId / identifier links.
	covSearchParams := store.SearchParams{
		TenantID: tenant,
		ResType:  "Coverage",
		Count:    200,
	}
	if patientRestricted {
		covSearchParams.References = map[string]string{"patient": "Patient/" + allowedPatientID}
	}
	allCovSR, _ := s.store.Search(r.Context(), covSearchParams)
	coveragePatients := make(map[string]hrexCoverageMatch)
	patientHasAnyCoverage := make(map[string]bool)
	if allCovSR != nil {
		for _, m := range allCovSR.Matches {
			cov, err := s.protoToFHIRMapWithMeta("Coverage", m.ResourceProto, m.ResVersion, m.LastUpdated)
			if err != nil {
				continue
			}
			if covStatus, _ := cov["status"].(string); strings.EqualFold(strings.TrimSpace(covStatus), "entered-in-error") {
				continue
			}
			patID := extractCoverageBeneficiaryPatientID(cov)
			if patID == "" || (patientRestricted && cleanID(patID) != allowedPatientID) {
				continue
			}
			patientHasAnyCoverage[patID] = true

			subID, _ := cov["subscriberId"].(string)
			subID = strings.TrimSpace(subID)
			matchedCov := false
			memberVal := subID
			memberSys := ""
			if covSubscriberID != "" && strings.EqualFold(subID, covSubscriberID) {
				matchedCov = true
			}
			if cids, ok := cov["identifier"].([]any); ok {
				for _, rawCID := range cids {
					if cim, ok := rawCID.(map[string]any); ok {
						v, _ := cim["value"].(string)
						sys, _ := cim["system"].(string)
						vTrim := strings.TrimSpace(v)
						if matchedCov && memberSys == "" && sys != "" {
							memberSys = strings.TrimSpace(sys)
						}
						if !matchedCov {
							if covSubscriberID != "" && strings.EqualFold(vTrim, covSubscriberID) {
								matchedCov = true
								if memberVal == "" {
									memberVal = vTrim
								}
								memberSys = strings.TrimSpace(sys)
							}
							for _, want := range covIdentifiers {
								if strings.EqualFold(vTrim, want) {
									matchedCov = true
									if memberVal == "" {
										memberVal = vTrim
									}
									memberSys = strings.TrimSpace(sys)
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
				assRef, assDisp := extractPayorAssigner(cov)
				coveragePatients[patID] = hrexCoverageMatch{
					memberID:        memberVal,
					memberSys:       memberSys,
					assignerRef:     assRef,
					assignerDisplay: assDisp,
				}
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
	patCandParams := store.PatientMatchCandidateParams{
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
		s.writeNegotiatedOutcome(w, r, http.StatusInternalServerError, "exception", "failed to search Patients for $member-match")
		return
	}

	type matchedMember struct {
		patientID       string
		memberID        string
		memberSys       string
		assignerRef     string
		assignerDisplay string
		confidence      float64
	}
	var matches []matchedMember

	for _, m := range patCandidates {
		if patientRestricted && cleanID(m.ResID) != allowedPatientID {
			continue
		}
		if hasSecCtx && !s.resourceAllowedByGranularScope(secCtx, tenant, "Patient", m.ResID, m.ResourceProto) {
			continue
		}
		candMap, err := s.protoToFHIRMapWithMeta("Patient", m.ResourceProto, m.ResVersion, m.LastUpdated)
		if err != nil {
			continue
		}
		candDemo := extractPatientDemographics(candMap)
		// Deterministic HRex birthDate guard: explicit birthDate mismatch disqualifies candidate.
		if inDemo.birthDate != "" && candDemo.birthDate != "" && inDemo.birthDate != candDemo.birthDate {
			continue
		}
		demoScore := scorePatientCandidate(inDemo, candDemo)
		covMatch, hasCovMatch := coveragePatients[m.ResID]

		qualifies := false
		covMemberID := covMatch.memberID
		covMemberSys := covMatch.memberSys
		if hasCovMatch && demoScore >= 0.50 {
			qualifies = true
		} else if demoScore >= 0.70 {
			// Check if CoverageToMatch.subscriberId or identifier matches a Patient.identifier directly
			matchedPatID := false
			for _, pid := range candDemo.identifiers {
				if covSubscriberID != "" && pid.code == strings.ToLower(covSubscriberID) {
					matchedPatID = true
					covMemberID = covSubscriberID
					break
				}
				for _, want := range covIdentifiers {
					if pid.code == want {
						matchedPatID = true
						covMemberID = want
						break
					}
				}
				if matchedPatID {
					break
				}
			}
			if matchedPatID {
				qualifies = true
			} else if !patientHasAnyCoverage[m.ResID] && len(coveragePatients) == 0 {
				if covSubscriberID == "" && len(covIdentifiers) == 0 {
					qualifies = true
				} else if demoScore >= 0.80 {
					qualifies = true
				}
			}
		}

		if qualifies {
			memVal := covMemberID
			memSys := covMemberSys
			if memSys == "" {
				memSys = "http://hl7.org/fhir/sid/us-mb"
			}
			if ids, ok := candMap["identifier"].([]any); ok {
				for _, rawID := range ids {
					if im, ok := rawID.(map[string]any); ok {
						v, _ := im["value"].(string)
						sys, _ := im["system"].(string)
						if v != "" {
							if memVal == "" {
								memVal = v
							}
							if covMemberSys == "" && sys != "" {
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
			assRef := covMatch.assignerRef
			assDisp := covMatch.assignerDisplay
			if assRef == "" && assDisp == "" {
				assRef = reqPayorRef
				assDisp = reqPayorDisp
			}
			matches = append(matches, matchedMember{
				patientID:       m.ResID,
				memberID:        memVal,
				memberSys:       memSys,
				assignerRef:     assRef,
				assignerDisplay: assDisp,
				confidence:      demoScore,
			})
		}
	}

	if len(matches) == 0 {
		s.writeNegotiatedOutcome(w, r, http.StatusUnprocessableEntity, "not-found",
			"No matching member found for Patient/$member-match criteria")
		return
	}
	if len(matches) > 1 {
		s.writeNegotiatedOutcome(w, r, http.StatusUnprocessableEntity, "multiple-matches",
			fmt.Sprintf("Multiple (%d) ambiguous member matches found for Patient/$member-match criteria", len(matches)))
		return
	}

	winner := matches[0]
	setAuditPatientID(r.Context(), winner.patientID)

	var linkedCoverageID string
	if coverageToLink != nil {
		covCopy := cloneShallowFHIRMap(coverageToLink)
		covCopy["resourceType"] = "Coverage"
		covCopy["beneficiary"] = map[string]any{
			"reference": "Patient/" + winner.patientID,
		}
		if _, hasStatus := covCopy["status"]; !hasStatus {
			covCopy["status"] = "active"
		}
		if _, hasPayor := covCopy["payor"]; !hasPayor {
			if p, ok := coverageToMatch["payor"]; ok {
				covCopy["payor"] = p
			} else {
				covCopy["payor"] = []any{map[string]any{"display": "Linked Health Plan"}}
			}
		}
		persistedCovID, pErr := s.persistFHIRMapResource(r, tenant, "Coverage", covCopy)
		if pErr != nil {
			if errors.Is(pErr, errForbiddenCompartment) {
				s.writeNegotiatedOutcome(w, r, http.StatusForbidden, "forbidden", pErr.Error())
				return
			}
			s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid CoverageToLink resource: %v", pErr))
			return
		}
		linkedCoverageID = persistedCovID
	}

	var linkedConsentID string
	if consentRes != nil {
		consentCopy := cloneShallowFHIRMap(consentRes)
		consentCopy["resourceType"] = "Consent"
		consentCopy["patient"] = map[string]any{
			"reference": "Patient/" + winner.patientID,
		}
		if _, hasScope := consentCopy["scope"]; !hasScope {
			consentCopy["scope"] = map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://terminology.hl7.org/CodeSystem/consentscope",
						"code":    "patient-privacy",
						"display": "Privacy Consent",
					},
				},
			}
		}
		if _, hasCat := consentCopy["category"]; !hasCat {
			consentCopy["category"] = []any{
				map[string]any{
					"coding": []any{
						map[string]any{
							"system":  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
							"code":    "IDSCL",
							"display": "Information Disclosure",
						},
					},
				},
			}
		}
		if _, hasPolicy := consentCopy["policy"]; !hasPolicy {
			if _, hasPolicyRule := consentCopy["policyRule"]; !hasPolicyRule {
				consentCopy["policyRule"] = map[string]any{
					"coding": []any{
						map[string]any{
							"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode",
							"code":   "OPTIN",
						},
					},
				}
			}
		}
		persistedConsentID, cErr := s.persistFHIRMapResource(r, tenant, "Consent", consentCopy)
		if cErr != nil {
			if errors.Is(cErr, errForbiddenCompartment) {
				s.writeNegotiatedOutcome(w, r, http.StatusForbidden, "forbidden", cErr.Error())
				return
			}
			s.writeNegotiatedOutcome(w, r, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid Consent resource: %v", cErr))
			return
		}
		linkedConsentID = persistedConsentID
	}

	memIdent := map[string]any{
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
	}
	if winner.assignerRef != "" || winner.assignerDisplay != "" {
		assigner := map[string]any{}
		if winner.assignerRef != "" {
			assigner["reference"] = winner.assignerRef
		}
		if winner.assignerDisplay != "" {
			assigner["display"] = winner.assignerDisplay
		}
		memIdent["assigner"] = assigner
	}

	outParams := []any{
		map[string]any{
			"name":            "MemberIdentifier",
			"valueIdentifier": memIdent,
		},
		map[string]any{
			"name": "Patient",
			"valueReference": map[string]any{
				"reference": "Patient/" + winner.patientID,
			},
		},
	}
	if linkedCoverageID != "" {
		outParams = append(outParams, map[string]any{
			"name": "CoverageToLink",
			"valueReference": map[string]any{
				"reference": "Coverage/" + linkedCoverageID,
			},
		})
	}
	if linkedConsentID != "" {
		outParams = append(outParams, map[string]any{
			"name": "Consent",
			"valueReference": map[string]any{
				"reference": "Consent/" + linkedConsentID,
			},
		})
	}

	respParams := map[string]any{
		"resourceType": "Parameters",
		"parameter":    outParams,
	}

	s.writeOperationResponse(w, r, http.StatusOK, "Parameters", respParams)
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

	payload, err := s.decodeOperationBodyMap(r, body, "Claim")
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("malformed payload in Claim/$submit: %v", err))
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
			if auth.IsPatientRestricted(secCtx) && !strings.EqualFold(supType, "Patient") && !store.IsPatientCompartmentResourceType(supType) {
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

	s.writeOperationResponse(w, r, http.StatusOK, "ClaimResponse", claimResp)
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
	var idx *store.SearchIndexes
	if extractor, ok := s.extractors[resType]; ok {
		idx, _ = extractor(tenant, resID, protoBytes)
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if auth.IsPatientRestricted(secCtx) {
			allowedPatientID := cleanID(secCtx.GetPatientId())
			if strings.EqualFold(resType, "Patient") || store.IsPatientCompartmentResourceType(resType) {
				if !store.IndexesBelongToPatientCompartment(resType, resID, idx, allowedPatientID) {
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

	rec, err := s.store.WriteResourceWithMeta(r.Context(), store.ResourceInput{
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
