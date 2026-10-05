package spike1

import (
	"encoding/json"
	"os"
	"testing"
)

func TestWrapUnwrapPrimitive(t *testing.T) {
	// Simple value
	wrapped := WrapPrimitive("Smith")
	val, ext := UnwrapPrimitive(wrapped)
	if val != "Smith" {
		t.Errorf("got %v, want Smith", val)
	}
	if ext != nil {
		t.Errorf("got extensions %v, want nil", ext)
	}

	// Value with extensions (like birthDate with birthTime extension)
	wrappedWithExt := map[string]interface{}{
		"value": "1990-01-15",
		"extension": []interface{}{
			map[string]interface{}{
				"url":           "http://hl7.org/fhir/StructureDefinition/patient-birthTime",
				"valueDateTime": "1990-01-15T04:30:00-05:00",
			},
		},
	}
	val, ext = UnwrapPrimitive(wrappedWithExt)
	if val != "1990-01-15" {
		t.Errorf("got %v, want 1990-01-15", val)
	}
	if ext == nil {
		t.Error("expected extensions, got nil")
	}
	if _, ok := ext["extension"]; !ok {
		t.Error("expected extension key in extensions map")
	}
}

func TestGenderEnumMapping(t *testing.T) {
	fhir := map[string]interface{}{"gender": "male"}
	proto := FHIRToProtoJSON(fhir)

	genderWrapped := proto["gender"].(map[string]interface{})
	if genderWrapped["value"] != "MALE" {
		t.Errorf("FHIR→Proto: got %v, want MALE", genderWrapped["value"])
	}

	backToFHIR := ProtoJSONToFHIR(proto)
	if backToFHIR["gender"] != "male" {
		t.Errorf("Proto→FHIR: got %v, want male", backToFHIR["gender"])
	}
}

func TestBirthDateWithExtension(t *testing.T) {
	fhir := map[string]interface{}{
		"birthDate": "1990-01-15",
		"_birthDate": map[string]interface{}{
			"extension": []interface{}{
				map[string]interface{}{
					"url":           "http://hl7.org/fhir/StructureDefinition/patient-birthTime",
					"valueDateTime": "1990-01-15T04:30:00-05:00",
				},
			},
		},
	}

	proto := FHIRToProtoJSON(fhir)

	// Proto representation should merge value and extensions
	bd := proto["birthDate"].(map[string]interface{})
	if bd["value"] != "1990-01-15" {
		t.Errorf("Proto birthDate value: got %v, want 1990-01-15", bd["value"])
	}
	if bd["extension"] == nil {
		t.Error("Proto birthDate should have extension merged in")
	}

	// Round-trip back
	backToFHIR := ProtoJSONToFHIR(proto)
	if backToFHIR["birthDate"] != "1990-01-15" {
		t.Errorf("FHIR birthDate: got %v, want 1990-01-15", backToFHIR["birthDate"])
	}
	if backToFHIR["_birthDate"] == nil {
		t.Error("FHIR _birthDate should be preserved")
	}
}

func TestFullPatientRoundTrip(t *testing.T) {
	// Load the Synthea patient
	data, err := os.ReadFile("../../testdata/synthea/patient_example.json")
	if err != nil {
		t.Fatalf("failed to read patient: %v", err)
	}

	roundTripped, lostFields, err := RoundTrip(data)
	if err != nil {
		t.Fatalf("round trip failed: %v", err)
	}

	if len(lostFields) > 0 {
		t.Errorf("LOST FIELDS in round trip: %v", lostFields)
	}

	// Parse both for structural comparison
	var original map[string]interface{}
	json.Unmarshal(data, &original)

	var result map[string]interface{}
	json.Unmarshal(roundTripped, &result)

	// Key field comparisons
	checks := []struct {
		field string
		want  interface{}
	}{
		{"resourceType", "Patient"},
		{"id", "synthea-abc123"},
		{"gender", "male"},
		{"birthDate", "1990-01-15"},
		{"active", true},
	}

	for _, c := range checks {
		got := result[c.field]
		if got != c.want {
			t.Errorf("field %q: got %v (%T), want %v (%T)", c.field, got, got, c.want, c.want)
		}
	}

	// Check _birthDate extension survived
	if result["_birthDate"] == nil {
		t.Error("_birthDate extension lost in round trip")
	}

	// Check complex types survived
	for _, field := range []string{"name", "identifier", "telecom", "address", "maritalStatus"} {
		if result[field] == nil {
			t.Errorf("complex field %q lost in round trip", field)
		}
	}

	t.Logf("Round-trip output:\n%s", string(roundTripped))
}
