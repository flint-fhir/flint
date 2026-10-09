package validation_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"hegel.dev/go/hegel"

	"github.com/flint-fhir/flint/pkg/validation"
)

func TestProperty_ValidPatientInvariance(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.FromRegex(`[a-zA-Z0-9\-]{3,15}`, true))
		gender := hegel.Draw(ht, hegel.FromRegex(`(male|female|other|unknown)`, true))
		birthDate := hegel.Draw(ht, hegel.FromRegex(`(19[7-9][0-9]|20[0-2][0-9])-(0[1-9]|1[0-2])-(0[1-9]|1[0-9]|2[0-8])`, true))
		family := hegel.Draw(ht, hegel.FromRegex(`[A-Za-z]{2,10}`, true))

		patMap := map[string]any{
			"resourceType": "Patient",
			"id":           id,
			"gender":       gender,
			"birthDate":    birthDate,
			"name": []map[string]any{
				{"family": family},
			},
		}

		rawBytes, err := json.Marshal(patMap)
		if err != nil {
			ht.Fatalf("marshal failed: %v", err)
		}

		outcome := engine.ValidateJSON("Patient", rawBytes)
		if !outcome.IsValid() {
			ht.Fatalf("valid Patient payload rejected unexpectedly: %+v", outcome.Issues)
		}
	})
}

func TestProperty_ObservationRequiredFields(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	hegel.Test(t, func(ht *hegel.T) {
		status := hegel.Draw(ht, hegel.FromRegex(`(registered|preliminary|final|amended|corrected|cancelled|entered-in-error|unknown)`, true))
		code := hegel.Draw(ht, hegel.FromRegex(`[0-9]{4,6}`, true))
		dropStatus := hegel.Draw(ht, hegel.FromRegex(`(true|false)`, true)) == "true"

		obsMap := map[string]any{
			"resourceType": "Observation",
			"id":           "obs-prop-1",
		}

		if !dropStatus {
			obsMap["status"] = status
		}
		// Always drop code when not dropping status to guarantee a missing required field
		if dropStatus {
			obsMap["code"] = map[string]any{
				"coding": []map[string]any{{"code": code}},
			}
		}

		rawBytes, _ := json.Marshal(obsMap)
		outcome := engine.ValidateJSON("Observation", rawBytes)
		if outcome.IsValid() {
			ht.Fatalf("expected missing required field error, but payload was accepted: %s", string(rawBytes))
		}

		foundRequired := false
		for _, iss := range outcome.Issues {
			if iss.Code == validation.CodeRequired {
				foundRequired = true
				break
			}
		}
		if !foundRequired {
			ht.Fatalf("expected CodeRequired issue, got: %+v", outcome.Issues)
		}
	})
}

func TestProperty_InvalidGenderRejected(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	hegel.Test(t, func(ht *hegel.T) {
		// Draw arbitrary non-standard gender codes
		badGender := hegel.Draw(ht, hegel.FromRegex(`[a-z]{6,12}`, true))
		if badGender == "female" {
			badGender = "invalid-gender"
		}

		patJSON := fmt.Sprintf(`{"resourceType":"Patient","gender":%q}`, badGender)
		outcome := engine.ValidateJSON("Patient", []byte(patJSON))

		if outcome.IsValid() {
			ht.Fatalf("invalid gender %q was accepted", badGender)
		}

		foundInvalidCode := false
		for _, iss := range outcome.Issues {
			if iss.Code == validation.CodeInvalid {
				foundInvalidCode = true
				break
			}
		}
		if !foundInvalidCode {
			ht.Fatalf("expected CodeInvalid issue for gender %q, got: %+v", badGender, outcome.Issues)
		}
	})
}

func TestProperty_ValidatorNeverPanics(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	hegel.Test(t, func(ht *hegel.T) {
		fuzzInput := hegel.Draw(ht, hegel.FromRegex(`.*`, true))
		expectedType := hegel.Draw(ht, hegel.FromRegex(`(Patient|Observation|Condition|Encounter|Practitioner|RandomType)`, true))

		// Must never panic or crash
		outcome := engine.ValidateJSON(expectedType, []byte(fuzzInput))
		_ = outcome.IsValid()
		_ = outcome.ToOperationOutcomeMap()
	})
}
