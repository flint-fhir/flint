package validation_test

import (
	"testing"

	"github.com/flint-fhir/flint/pkg/validation"
)

func TestTrack7Profiles_C4BBPDexHRexAndPAS(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	t.Run("US Core v3.1.1 us-core-condition", func(t *testing.T) {
		prof := []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-condition"}
		valid := `{"resourceType":"Condition","id":"c-1","clinicalStatus":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/condition-clinical","code":"active"}]},"code":{"coding":[{"system":"http://snomed.info/sct","code":"59621000"}]},"subject":{"reference":"Patient/1"}}`
		if out := engine.ValidateJSONWithProfiles("Condition", []byte(valid), prof, ""); !out.IsValid() {
			t.Fatalf("expected valid us-core-condition, got: %+v", out.Issues)
		}
		invalid := `{"resourceType":"Condition","id":"c-2","code":{"coding":[{"system":"http://snomed.info/sct","code":"59621000"}]},"subject":{"reference":"Patient/1"}}`
		if out := engine.ValidateJSONWithProfiles("Condition", []byte(invalid), prof, ""); out.IsValid() {
			t.Fatal("expected missing clinicalStatus to fail us-core-condition")
		}
	})

	t.Run("C4BB v2.0.0 ExplanationOfBenefit subtypes", func(t *testing.T) {
		inpProf := []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-ExplanationOfBenefit-Inpatient-Institutional"}
		validInp := `{
			"resourceType":"ExplanationOfBenefit",
			"id":"eob-inp-1",
			"identifier":[{"system":"http://example.org/claim","value":"CLM-1"}],
			"status":"active",
			"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"institutional"}]},
			"subType":{"coding":[{"system":"http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBInstitutionalClaimSubType","code":"inpatient"}]},
			"use":"claim",
			"patient":{"reference":"Patient/1"},
			"created":"2026-01-15T10:00:00Z",
			"insurer":{"reference":"Organization/1"},
			"provider":{"reference":"Organization/2"},
			"outcome":"complete",
			"diagnosis":[{"sequence":1,"diagnosisCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/sid/icd-10-cm","code":"I10"}]}}],
			"insurance":[{"focal":true,"coverage":{"reference":"Coverage/1"}}]
		}`
		if out := engine.ValidateJSONWithProfiles("ExplanationOfBenefit", []byte(validInp), inpProf, ""); !out.IsValid() {
			t.Fatalf("expected valid C4BB Inpatient EOB, got: %+v", out.Issues)
		}

		// Missing diagnosis on institutional inpatient EOB must fail
		noDiagInp := `{
			"resourceType":"ExplanationOfBenefit",
			"id":"eob-inp-2",
			"identifier":[{"system":"http://example.org/claim","value":"CLM-2"}],
			"status":"active",
			"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"institutional"}]},
			"subType":{"coding":[{"system":"http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBInstitutionalClaimSubType","code":"inpatient"}]},
			"use":"claim",
			"patient":{"reference":"Patient/1"},
			"created":"2026-01-15T10:00:00Z",
			"insurer":{"reference":"Organization/1"},
			"provider":{"reference":"Organization/2"},
			"outcome":"complete",
			"insurance":[{"focal":true,"coverage":{"reference":"Coverage/1"}}]
		}`
		if out := engine.ValidateJSONWithProfiles("ExplanationOfBenefit", []byte(noDiagInp), inpProf, ""); out.IsValid() {
			t.Fatal("expected missing diagnosis to fail C4BB-ExplanationOfBenefit-Inpatient-Institutional")
		}

		// Wrong use / outcome must fail C4BB EOB
		badUseOutcome := `{
			"resourceType":"ExplanationOfBenefit",
			"id":"eob-bad",
			"identifier":[{"system":"http://example.org/claim","value":"CLM-3"}],
			"status":"active",
			"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"pharmacy"}]},
			"use":"preauthorization",
			"patient":{"reference":"Patient/1"},
			"created":"2026-01-15T10:00:00Z",
			"insurer":{"reference":"Organization/1"},
			"provider":{"reference":"Organization/2"},
			"outcome":"queued",
			"insurance":[{"focal":true,"coverage":{"reference":"Coverage/1"}}]
		}`
		baseProf := []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-ExplanationOfBenefit"}
		if out := engine.ValidateJSONWithProfiles("ExplanationOfBenefit", []byte(badUseOutcome), baseProf, ""); out.IsValid() {
			t.Fatal("expected preauthorization/queued EOB to fail C4BB-ExplanationOfBenefit")
		}

		// Pharmacy EOB passed to Inpatient profile skips institutional subtype diagnosis check cleanly
		validPharm := `{
			"resourceType":"ExplanationOfBenefit",
			"id":"eob-pharm",
			"identifier":[{"system":"http://example.org/claim","value":"CLM-4"}],
			"status":"active",
			"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"pharmacy"}]},
			"use":"claim",
			"patient":{"reference":"Patient/1"},
			"created":"2026-01-15T10:00:00Z",
			"insurer":{"reference":"Organization/1"},
			"provider":{"reference":"Organization/2"},
			"outcome":"complete",
			"insurance":[{"focal":true,"coverage":{"reference":"Coverage/1"}}]
		}`
		if out := engine.ValidateJSONWithProfiles("ExplanationOfBenefit", []byte(validPharm), inpProf, ""); !out.IsValid() {
			t.Fatalf("expected non-matching pharmacy EOB to pass base C4BB checks, got: %+v", out.Issues)
		}

		// Outpatient EOB passed to Inpatient profile skips inpatient subtype branch cleanly
		outpatOnInpat := `{
			"resourceType":"ExplanationOfBenefit",
			"id":"eob-outpat",
			"identifier":[{"system":"http://example.org/claim","value":"CLM-5"}],
			"status":"active",
			"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"institutional"}]},
			"subType":{"coding":[{"system":"http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBInstitutionalClaimSubType","code":"outpatient"}]},
			"use":"claim",
			"patient":{"reference":"Patient/1"},
			"created":"2026-01-15T10:00:00Z",
			"insurer":{"reference":"Organization/1"},
			"provider":{"reference":"Organization/2"},
			"outcome":"complete",
			"insurance":[{"focal":true,"coverage":{"reference":"Coverage/1"}}]
		}`
		if out := engine.ValidateJSONWithProfiles("ExplanationOfBenefit", []byte(outpatOnInpat), inpProf, ""); !out.IsValid() {
			t.Fatalf("expected outpatient subType to skip inpatient check, got: %+v", out.Issues)
		}
	})

	t.Run("HRex $member-match in/out & PDex PriorAuthorization", func(t *testing.T) {
		inProf := []string{"http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-parameters-member-match-in"}
		validIn := `{"resourceType":"Parameters","parameter":[{"name":"MemberPatient","resource":{"resourceType":"Patient","id":"1"}},{"name":"CoverageToMatch","resource":{"resourceType":"Coverage","id":"c1","status":"draft","beneficiary":{"reference":"Patient/1"},"payor":[{"display":"Payer"}]}}]}`
		if out := engine.ValidateJSONWithProfiles("Parameters", []byte(validIn), inProf, ""); !out.IsValid() {
			t.Fatalf("expected valid hrex-parameters-member-match-in, got: %+v", out.Issues)
		}
		invalidIn := `{"resourceType":"Parameters","parameter":[{"name":"MemberPatient","resource":{"resourceType":"Patient","id":"1"}}]}`
		if out := engine.ValidateJSONWithProfiles("Parameters", []byte(invalidIn), inProf, ""); out.IsValid() {
			t.Fatal("expected missing CoverageToMatch to fail hrex-parameters-member-match-in")
		}

		outProf := []string{"http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-parameters-member-match-out"}
		validOut := `{"resourceType":"Parameters","parameter":[{"name":"MemberIdentifier","valueIdentifier":{"system":"http://example.org/member","value":"MEM-1"}}]}`
		if out := engine.ValidateJSONWithProfiles("Parameters", []byte(validOut), outProf, ""); !out.IsValid() {
			t.Fatalf("expected valid hrex-parameters-member-match-out, got: %+v", out.Issues)
		}
		twoParamsOut := `{"resourceType":"Parameters","parameter":[{"name":"MemberIdentifier","valueIdentifier":{"value":"MEM-1"}},{"name":"Patient","resource":{"resourceType":"Patient","id":"1"}}]}`
		if out := engine.ValidateJSONWithProfiles("Parameters", []byte(twoParamsOut), outProf, ""); out.IsValid() {
			t.Fatal("expected 2-parameter output to fail strict hrex-parameters-member-match-out")
		}
		wrongNameOut := `{"resourceType":"Parameters","parameter":[{"name":"WrongName","valueString":"MEM-1"}]}`
		if out := engine.ValidateJSONWithProfiles("Parameters", []byte(wrongNameOut), outProf, ""); out.IsValid() {
			t.Fatal("expected non-MemberIdentifier output to fail hrex-parameters-member-match-out")
		}

		paProf := []string{"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-priorauthorization"}
		badPA := `{
			"resourceType":"ExplanationOfBenefit",
			"id":"pa-bad",
			"status":"active",
			"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"professional"}]},
			"use":"claim",
			"patient":{"reference":"Patient/1"},
			"created":"2026-01-15T10:00:00Z",
			"insurer":{"reference":"Organization/1"},
			"provider":{"reference":"Organization/2"},
			"outcome":"complete",
			"insurance":[{"focal":true,"coverage":{"reference":"Coverage/1"}}],
			"item":[{"sequence":1,"productOrService":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"99213"}]}}]
		}`
		if out := engine.ValidateJSONWithProfiles("ExplanationOfBenefit", []byte(badPA), paProf, ""); out.IsValid() {
			t.Fatal("expected use=claim to fail pdex-priorauthorization")
		}
	})

	t.Run("Da Vinci PAS v2.2.1 Request and Response Bundles", func(t *testing.T) {
		reqProf := []string{"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"}
		validReq := `{"resourceType":"Bundle","identifier":{"system":"http://example.org","value":"B-1"},"type":"collection","timestamp":"2026-02-01T10:00:00Z","entry":[{"fullUrl":"urn:uuid:11111111-1111-4111-8111-000000000001","resource":{"resourceType":"Patient","id":"11111111-1111-4111-8111-000000000001"}}]}`
		if out := engine.ValidateJSONWithProfiles("Bundle", []byte(validReq), reqProf, ""); !out.IsValid() {
			t.Fatalf("expected valid PAS Request Bundle, got: %+v", out.Issues)
		}
		wrongType := `{"resourceType":"Bundle","identifier":{"system":"http://example.org","value":"B-1"},"type":"searchset","timestamp":"2026-02-01T10:00:00Z","entry":[{"fullUrl":"urn:uuid:11111111-1111-4111-8111-000000000001","resource":{"resourceType":"Patient","id":"11111111-1111-4111-8111-000000000001"}}]}`
		if out := engine.ValidateJSONWithProfiles("Bundle", []byte(wrongType), reqProf, ""); out.IsValid() {
			t.Fatal("expected Bundle.type=searchset to fail profile-pas-request-bundle")
		}

		respProf := []string{"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-response-bundle"}
		validResp := `{"resourceType":"Bundle","type":"collection","timestamp":"2026-02-01T10:00:00Z","entry":[{"fullUrl":"urn:uuid:11111111-1111-4111-8111-000000000002","resource":{"resourceType":"Patient","id":"11111111-1111-4111-8111-000000000002"}}]}`
		if out := engine.ValidateJSONWithProfiles("Bundle", []byte(validResp), respProf, ""); !out.IsValid() {
			t.Fatalf("expected valid PAS Response Bundle, got: %+v", out.Issues)
		}
	})
}
