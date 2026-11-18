package validation

import (
	"testing"
)

func TestTrack8CRDLogicalModelsAndProfiles(t *testing.T) {
	eng := NewEngine(DefaultOptions())

	reqPayload := map[string]any{
		"hook":         "order-sign",
		"hookInstance": "123e4567-e89b-12d3-a456-426614174000",
		"context": map[string]any{
			"userId":    "Practitioner/prac-1",
			"patientId": "pat-1",
		},
	}
	out := eng.ValidateResourceWithProfiles("", reqPayload, []string{
		"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/CRDHooksRequest|2.2.1",
	}, "")
	if !out.IsValid() {
		t.Fatalf("expected valid CRDHooksRequest, got issues: %+v", out.Issues)
	}

	badReq := map[string]any{"hook": "order-sign"}
	badOut := eng.ValidateResourceWithProfiles("", badReq, []string{
		"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/CRDHooksRequest",
	}, "")
	if badOut.IsValid() {
		t.Fatalf("expected invalid CRDHooksRequest when hookInstance and context are missing")
	}

	covResp := map[string]any{
		"cards": []any{},
		"systemActions": []any{
			map[string]any{"type": "update"},
		},
	}
	covOut := eng.ValidateResourceWithProfiles("", covResp, []string{
		"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/CRDHooksResponseBase|2.2.1",
		"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/CRDHooksResponse-coverageInformation|2.2.1",
	}, "")
	if !covOut.IsValid() {
		t.Fatalf("expected valid CRDHooksResponse-coverageInformation, got: %+v", covOut.Issues)
	}

	// Inferno DaVinciCRDTestKit validates individual systemActions wrapped as {"systemActions": [action]} without "cards".
	sysActionOnly := map[string]any{
		"systemActions": []any{
			map[string]any{"type": "update"},
		},
	}
	sysActionOut := eng.ValidateResourceWithProfiles("", sysActionOnly, []string{
		"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/CRDHooksResponseBase|2.2.1",
		"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/CRDHooksResponse-coverageInformation|2.2.1",
	}, "")
	if !sysActionOut.IsValid() {
		t.Fatalf("expected valid systemActions-only CRDHooksResponse-coverageInformation, got: %+v", sysActionOut.Issues)
	}

	missingSysActions := map[string]any{"cards": []any{}}
	missingOut := eng.ValidateResourceWithProfiles("", missingSysActions, []string{
		"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/CRDHooksResponse-coverageInformation",
	}, "")
	if missingOut.IsValid() {
		t.Fatalf("expected CRDHooksResponse-coverageInformation without systemActions to fail")
	}

	dr := map[string]any{
		"resourceType": "DeviceRequest",
		"id":           "dr-1",
		"status":       "draft",
		"intent":       "original-order",
		"codeCodeableConcept": map[string]any{
			"coding": []any{
				map[string]any{"system": "https://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets", "code": "E0424"},
			},
		},
		"subject": map[string]any{"reference": "Patient/pat-1"},
	}
	drOut := eng.ValidateResourceWithProfiles("DeviceRequest", dr, []string{
		"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/profile-devicerequest|2.2.1",
	}, "")
	if !drOut.IsValid() {
		t.Fatalf("expected valid CRD DeviceRequest, got: %+v", drOut.Issues)
	}
	for _, iss := range drOut.Issues {
		if iss.Code == CodeNotSupported {
			t.Fatalf("unexpected unsupported profile warning: %+v", iss)
		}
	}

	dtrQR := map[string]any{
		"resourceType":  "QuestionnaireResponse",
		"id":            "qr-1",
		"questionnaire": "http://flint.health/fhir/Questionnaire/dtr-home-oxygen|2.2.0",
		"status":        "in-progress",
		"subject":       map[string]any{"reference": "Patient/pat-1"},
	}
	qrOut := eng.ValidateResourceWithProfiles("QuestionnaireResponse", dtrQR, []string{
		"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaireresponse|2.2.0",
		"http://hl7.org/fhir/uv/sdc/StructureDefinition/sdc-questionnaireresponse",
	}, "")
	if !qrOut.IsValid() {
		t.Fatalf("expected valid DTR QuestionnaireResponse, got: %+v", qrOut.Issues)
	}
}
