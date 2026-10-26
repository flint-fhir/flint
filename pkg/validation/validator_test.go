package validation_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/flint-fhir/flint/pkg/validation"
)

func TestValidationEngine_ValidResources(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	t.Run("Valid Patient", func(t *testing.T) {
		patJSON := `{
			"resourceType": "Patient",
			"id": "pat-001",
			"active": true,
			"gender": "female",
			"birthDate": "1992-05-18",
			"name": [{"family": "Curie", "given": ["Marie"]}]
		}`
		outcome := engine.ValidateJSON("Patient", []byte(patJSON))
		if !outcome.IsValid() {
			t.Fatalf("expected valid Patient, got issues: %+v", outcome.Issues)
		}
	})

	t.Run("Valid Observation", func(t *testing.T) {
		obsJSON := `{
			"resourceType": "Observation",
			"id": "obs-100",
			"status": "final",
			"code": {
				"coding": [{
					"system": "http://loinc.org",
					"code": "8867-4"
				}]
			},
			"valueQuantity": {
				"value": 72.5,
				"unit": "kg",
				"system": "http://unitsofmeasure.org",
				"code": "kg"
			}
		}`
		outcome := engine.ValidateJSON("Observation", []byte(obsJSON))
		if !outcome.IsValid() {
			t.Fatalf("expected valid Observation, got issues: %+v", outcome.Issues)
		}
	})
}

func TestValidationEngine_StructureAndUnknownFields(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	t.Run("Reject unknown field in strict mode", func(t *testing.T) {
		patJSON := `{
			"resourceType": "Patient",
			"id": "pat-002",
			"nonExistentField": "invalid_value"
		}`
		outcome := engine.ValidateJSON("Patient", []byte(patJSON))
		if outcome.IsValid() {
			t.Fatal("expected invalid due to unknown field")
		}

		found := false
		for _, iss := range outcome.Issues {
			if iss.Code == validation.CodeStructure && iss.Expression[0] == "Patient.nonExistentField" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected issue on Patient.nonExistentField, got: %+v", outcome.Issues)
		}
	})

	t.Run("Reject mismatched resourceType", func(t *testing.T) {
		jsonPayload := `{
			"resourceType": "Condition",
			"id": "c-1"
		}`
		outcome := engine.ValidateJSON("Patient", []byte(jsonPayload))
		if outcome.IsValid() {
			t.Fatal("expected error for mismatched resourceType")
		}
		if outcome.Issues[0].Code != validation.CodeInvariant {
			t.Errorf("expected CodeInvariant, got %v", outcome.Issues[0].Code)
		}
	})
}

func TestValidationEngine_RequiredElements(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	t.Run("Observation missing status and code", func(t *testing.T) {
		obsJSON := `{
			"resourceType": "Observation",
			"id": "obs-missing"
		}`
		outcome := engine.ValidateJSON("Observation", []byte(obsJSON))
		if outcome.IsValid() {
			t.Fatal("expected error for missing required elements")
		}

		missingStatus := false
		missingCode := false
		for _, iss := range outcome.Issues {
			if iss.Code == validation.CodeRequired {
				if len(iss.Expression) > 0 && iss.Expression[0] == "Observation.status" {
					missingStatus = true
				}
				if len(iss.Expression) > 0 && iss.Expression[0] == "Observation.code" {
					missingCode = true
				}
			}
		}
		if !missingStatus || !missingCode {
			t.Errorf("expected both status and code to be flagged, got: %+v", outcome.Issues)
		}
	})

	t.Run("Condition missing subject", func(t *testing.T) {
		condJSON := `{
			"resourceType": "Condition",
			"id": "cond-001"
		}`
		outcome := engine.ValidateJSON("Condition", []byte(condJSON))
		if outcome.IsValid() {
			t.Fatal("expected error for missing subject on Condition")
		}
	})
}

func TestValidationEngine_ValueSetBindings(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	t.Run("Patient invalid gender code", func(t *testing.T) {
		patJSON := `{
			"resourceType": "Patient",
			"gender": "alien"
		}`
		outcome := engine.ValidateJSON("Patient", []byte(patJSON))
		if outcome.IsValid() {
			t.Fatal("expected invalid due to unknown gender")
		}
		if outcome.Issues[0].Code != validation.CodeInvalid {
			t.Errorf("expected CodeInvalid, got %v", outcome.Issues[0].Code)
		}
	})

	t.Run("Observation invalid status code", func(t *testing.T) {
		obsJSON := `{
			"resourceType": "Observation",
			"status": "complete",
			"code": {"coding": [{"code": "1234"}]}
		}`
		outcome := engine.ValidateJSON("Observation", []byte(obsJSON))
		if outcome.IsValid() {
			t.Fatal("expected invalid status code")
		}
		if outcome.Issues[0].Code != validation.CodeInvalid {
			t.Errorf("expected CodeInvalid, got %v", outcome.Issues[0].Code)
		}
	})
}

func TestValidationEngine_PrimitiveFormats(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	t.Run("Patient invalid birthDate format", func(t *testing.T) {
		patJSON := `{
			"resourceType": "Patient",
			"birthDate": "01/15/1990"
		}`
		outcome := engine.ValidateJSON("Patient", []byte(patJSON))
		if outcome.IsValid() {
			t.Fatal("expected error for invalid birthDate format")
		}
		if outcome.Issues[0].Code != validation.CodeValue {
			t.Errorf("expected CodeValue, got %v", outcome.Issues[0].Code)
		}
	})

	t.Run("Patient invalid ID format", func(t *testing.T) {
		patJSON := `{
			"resourceType": "Patient",
			"id": "invalid ID with spaces!"
		}`
		outcome := engine.ValidateJSON("Patient", []byte(patJSON))
		if outcome.IsValid() {
			t.Fatal("expected error for invalid ID characters")
		}
		if outcome.Issues[0].Code != validation.CodeValue {
			t.Errorf("expected CodeValue, got %v", outcome.Issues[0].Code)
		}
	})
}

func TestOutcome_ToOperationOutcomeMap(t *testing.T) {
	outcome := validation.NewOutcome()
	outcome.AddWarning(validation.CodeInformational, "Advisory warning", "Observation.note")
	outcome.AddIssue(validation.SeverityInformation, validation.CodeInformational, "All checks nominal")
	if !outcome.IsValid() {
		t.Fatal("expected outcome with only warning/information to remain valid")
	}

	outcome.AddError(validation.CodeRequired, "Missing element", "Observation.status")
	if outcome.IsValid() {
		t.Fatal("expected outcome with error to be invalid")
	}

	oo := outcome.ToOperationOutcomeMap()
	if oo["resourceType"] != "OperationOutcome" {
		t.Errorf("expected resourceType OperationOutcome, got %v", oo["resourceType"])
	}

	issues := oo["issue"].([]map[string]any)
	if len(issues) != 3 {
		t.Fatalf("expected 3 issues, got %d", len(issues))
	}
	if issues[2]["severity"] != "error" || issues[2]["code"] != "required" {
		t.Errorf("issue fields mismatch: %+v", issues[2])
	}

	rec := httptest.NewRecorder()
	outcome.WriteHTTP(rec, http.StatusBadRequest)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestValidationEngine_StrictUnknownAndTypes(t *testing.T) {
	strictEngine := validation.NewEngine(validation.DefaultOptions())

	t.Run("Unknown top-level element rejected in strict mode", func(t *testing.T) {
		outcome := strictEngine.ValidateJSON("Patient", []byte(`{
			"resourceType": "Patient",
			"id": "pat-strict-1",
			"unknownCustomField": "foo"
		}`))
		if outcome.IsValid() {
			t.Fatal("expected strict mode to reject unknownCustomField")
		}
	})

	t.Run("Empty payload, malformed JSON, mismatched resourceType, and unregistered resourceType warning", func(t *testing.T) {
		if strictEngine.ValidateJSON("Patient", nil).IsValid() {
			t.Fatal("expected empty payload to fail")
		}
		if strictEngine.ValidateJSON("Patient", []byte(`{malformed`)).IsValid() {
			t.Fatal("expected malformed JSON to fail")
		}
		if strictEngine.ValidateJSON("Patient", []byte(`{"id":"1"}`)).IsValid() {
			t.Fatal("expected missing resourceType to fail")
		}
		if strictEngine.ValidateJSON("Patient", []byte(`{"resourceType":123}`)).IsValid() {
			t.Fatal("expected non-string resourceType to fail")
		}
		if strictEngine.ValidateJSON("Patient", []byte(`{"resourceType":"Observation"}`)).IsValid() {
			t.Fatal("expected mismatched resourceType to fail")
		}
		riskAssess := strictEngine.ValidateJSON("RiskAssessment", []byte(`{"resourceType":"RiskAssessment","id":"ra-1"}`))
		if !riskAssess.IsValid() {
			t.Fatalf("expected standard R4 resourceType RiskAssessment to be registered and valid, got %+v", riskAssess.Issues)
		}
		unreg := strictEngine.ValidateJSON("CustomUnregisteredResource", []byte(`{"resourceType":"CustomUnregisteredResource"}`))
		if !unreg.IsValid() || len(unreg.Issues) == 0 {
			t.Fatal("expected unregistered resourceType to return warning outcome")
		}
	})

	t.Run("Invalid effectiveDateTime, issued instant, wrapped code, and RegisterSchema", func(t *testing.T) {
		obsBadDates := strictEngine.ValidateJSON("Observation", []byte(`{
			"resourceType": "Observation",
			"status": {"value": "bad-wrapped-status"},
			"code": {"coding": [{"code": "8867-4"}]},
			"effectiveDateTime": "not-a-datetime",
			"issued": "2025-01-01"
		}`))
		if obsBadDates.IsValid() || len(obsBadDates.Issues) < 3 {
			t.Fatalf("expected invalid status, effectiveDateTime, and issued to produce >=3 errors, got %+v", obsBadDates.Issues)
		}

		condBadCoding := strictEngine.ValidateJSON("Condition", []byte(`{
			"resourceType": "Condition",
			"subject": {"reference": "Patient/1"},
			"clinicalStatus": {"coding": [{"code": "not-a-valid-clinical-status"}]}
		}`))
		if condBadCoding.IsValid() {
			t.Fatal("expected invalid Condition.clinicalStatus coding to fail")
		}

		strictEngine.RegisterSchema(validation.ResourceSchema{
			ResourceType:   "CustomResource",
			RequiredFields: []string{"mandatoryField"},
			AllowedFields:  map[string]bool{"resourceType": true, "mandatoryField": true},
		})
		if strictEngine.ValidateJSON("CustomResource", []byte(`{"resourceType":"CustomResource"}`)).IsValid() {
			t.Fatal("expected missing mandatoryField on CustomResource to fail")
		}
	})

	t.Run("Proto-derived CodeBindings and primitive date/dateTime/instant checks on non-US-Core R4 types", func(t *testing.T) {
		// Valid RiskAssessment with proto-derived status enum and occurrenceDateTime
		validRA := strictEngine.ValidateJSON("RiskAssessment", []byte(`{
			"resourceType": "RiskAssessment",
			"id": "ra-valid",
			"status": "final",
			"occurrenceDateTime": "2026-01-15T10:00:00Z"
		}`))
		if !validRA.IsValid() {
			t.Fatalf("expected valid RiskAssessment, got %+v", validRA.Issues)
		}

		// Invalid RiskAssessment status enum and occurrenceDateTime
		invalidRA := strictEngine.ValidateJSON("RiskAssessment", []byte(`{
			"resourceType": "RiskAssessment",
			"id": "ra-invalid",
			"status": "not-a-valid-status",
			"occurrenceDateTime": "not-a-datetime"
		}`))
		if invalidRA.IsValid() || len(invalidRA.Issues) < 2 {
			t.Fatalf("expected invalid RiskAssessment status and occurrenceDateTime to fail with >=2 errors, got %+v", invalidRA.Issues)
		}

		// Invalid Subscription status and end instant
		invalidSub := strictEngine.ValidateJSON("Subscription", []byte(`{
			"resourceType": "Subscription",
			"status": "invalid-sub-status",
			"end": "2026-01-15"
		}`))
		if invalidSub.IsValid() || len(invalidSub.Issues) < 2 {
			t.Fatalf("expected invalid Subscription status and end instant to fail with >=2 errors, got %+v", invalidSub.Issues)
		}
	})

	t.Run("Recursive BackboneElement, Period dateTime, and Bundle.entry.resource validation", func(t *testing.T) {
		// Unknown nested field in Patient.name and invalid Period.start dateTime
		badNested := strictEngine.ValidateJSON("Observation", []byte(`{
			"resourceType": "Observation",
			"status": "final",
			"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
			"effectivePeriod": {
				"start": "not-a-valid-datetime",
				"unknownPeriodField": "foo"
			},
			"component": [
				{
					"code": {"coding": [{"code": "8480-6"}]},
					"undeclaredComponentField": 123
				}
			]
		}`))
		if badNested.IsValid() || len(badNested.Issues) < 3 {
			t.Fatalf("expected recursive validation to catch invalid effectivePeriod.start, unknownPeriodField, and undeclaredComponentField, got %+v", badNested.Issues)
		}

		// Bundle with an invalid embedded Observation resource inside entry[0].resource
		badBundle := strictEngine.ValidateJSON("Bundle", []byte(`{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {
						"resourceType": "Observation",
						"status": "invalid-obs-status"
					}
				}
			]
		}`))
		if badBundle.IsValid() {
			t.Fatal("expected Bundle with invalid embedded Observation to fail validation")
		}
	})
}
