package validation_test

import (
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
	outcome.AddError(validation.CodeRequired, "Missing element", "Observation.status")

	oo := outcome.ToOperationOutcomeMap()
	if oo["resourceType"] != "OperationOutcome" {
		t.Errorf("expected resourceType OperationOutcome, got %v", oo["resourceType"])
	}

	issues := oo["issue"].([]map[string]any)
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(issues))
	}
	if issues[0]["severity"] != "error" || issues[0]["code"] != "required" {
		t.Errorf("issue fields mismatch: %+v", issues[0])
	}
}
