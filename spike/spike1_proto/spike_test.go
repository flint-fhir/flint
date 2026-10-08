package spike1_proto

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
)

// TestProtojsonMarshalOutput proves that protojson.Marshal() wraps primitives
// in objects, producing INVALID FHIR JSON.
func TestProtojsonMarshalOutput(t *testing.T) {
	// Build a Patient proto the same way google/fhir would
	patient := &MiniPatient{
		Id: &FHIRString{Value: "synthea-abc123"},
		Identifier: []*Identifier{
			{
				System: &FHIRString{Value: "https://github.com/synthetichealth/synthea"},
				Value:  &FHIRString{Value: "abc123-def456"},
			},
		},
		Active: &FHIRBoolean{Value: true},
		Name: []*HumanName{
			{
				Family: &FHIRString{Value: "Smith"},
				Given: []*FHIRString{
					{Value: "John"},
					{Value: "Michael"},
				},
				Prefix: []*FHIRString{
					{Value: "Mr."},
				},
			},
		},
		Gender:          GenderCode_MALE,
		BirthDate:       &FHIRDate{Value: "1990-01-15"},
		DeceasedBoolean: &FHIRBoolean{Value: false},
	}

	// Marshal with protojson (what google/fhir would produce)
	marshaler := protojson.MarshalOptions{
		Indent:        "  ",
		UseProtoNames: true,
	}
	protoJSON, err := marshaler.Marshal(patient)
	if err != nil {
		t.Fatalf("protojson.Marshal failed: %v", err)
	}

	t.Logf("=== protojson.Marshal() output (INVALID FHIR JSON) ===\n%s", string(protoJSON))

	// Parse and check the structural problems
	var parsed map[string]interface{}
	json.Unmarshal(protoJSON, &parsed)

	// PROBLEM 1: id is wrapped
	// FHIR expects: "id": "synthea-abc123"
	// Proto gives:  "id": {"value": "synthea-abc123"}
	idVal := parsed["id"]
	idMap, isWrapped := idVal.(map[string]interface{})
	if !isWrapped {
		t.Fatal("Expected id to be wrapped in object, but it wasn't")
	}
	t.Logf("PROBLEM 1: id is wrapped: %v (FHIR expects flat string)", idMap)

	// PROBLEM 2: family inside HumanName is wrapped
	names := parsed["name"].([]interface{})
	name0 := names[0].(map[string]interface{})
	familyVal := name0["family"]
	familyMap, isWrapped := familyVal.(map[string]interface{})
	if !isWrapped {
		t.Fatal("Expected family to be wrapped in object")
	}
	t.Logf("PROBLEM 2: family is wrapped: %v (FHIR expects flat string)", familyMap)

	// PROBLEM 3: given is array of objects instead of array of strings
	givenVal := name0["given"].([]interface{})
	given0 := givenVal[0]
	_, isWrapped = given0.(map[string]interface{})
	if !isWrapped {
		t.Fatal("Expected given[0] to be wrapped in object")
	}
	t.Logf("PROBLEM 3: given is array of objects: %v (FHIR expects array of strings)", givenVal)

	// PROBLEM 4: gender is enum name instead of lowercase string
	genderVal := parsed["gender"]
	genderStr, _ := genderVal.(string)
	if genderStr != "male" {
		t.Logf("PROBLEM 4: gender is %q (FHIR expects \"male\")", genderStr)
	}

	// Now demonstrate the FIX: unwrap to valid FHIR JSON
	fhirJSON := ProtoToFHIRJSON(parsed)
	fhirBytes, _ := json.MarshalIndent(fhirJSON, "", "  ")
	t.Logf("\n=== After ProtoToFHIRJSON() (VALID FHIR JSON) ===\n%s", string(fhirBytes))

	// Validate the fix
	if fhirJSON["id"] != "synthea-abc123" {
		t.Errorf("id should be flat string, got %v", fhirJSON["id"])
	}

	fhirNames := fhirJSON["name"].([]interface{})
	fhirName0 := fhirNames[0].(map[string]interface{})
	if fhirName0["family"] != "Smith" {
		t.Errorf("family should be flat string, got %v", fhirName0["family"])
	}

	fhirGiven := fhirName0["given"].([]interface{})
	if fhirGiven[0] != "John" {
		t.Errorf("given[0] should be flat string, got %v", fhirGiven[0])
	}

	if fhirJSON["gender"] != "male" {
		t.Errorf("gender should be lowercase, got %v", fhirJSON["gender"])
	}

	if fhirJSON["birthDate"] != "1990-01-15" {
		t.Errorf("birthDate should be flat string, got %v", fhirJSON["birthDate"])
	}
}

// ProtoToFHIRJSON transforms protojson output into valid FHIR JSON.
// This is the actual algorithm proto2type needs to generate.
func ProtoToFHIRJSON(protoJSON map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	for key, val := range protoJSON {
		switch key {
		// Top-level primitives: unwrap {"value": X} → X
		case "id", "active", "birth_date", "deceased_boolean", "multiple_birth_boolean":
			fhirKey := protoNameToFHIR(key)
			value, extensions := unwrap(val)
			if value != nil {
				result[fhirKey] = value
			}
			if extensions != nil {
				result["_"+fhirKey] = extensions
			}

		// Enum: MALE → male
		case "gender":
			if s, ok := val.(string); ok {
				result["gender"] = strings.ToLower(s)
			}

		// Complex types with nested primitives: recurse
		case "name":
			result["name"] = unwrapHumanNames(val)
		case "identifier":
			result["identifier"] = unwrapIdentifiers(val)

		default:
			result[key] = val
		}
	}

	return result
}

func unwrap(val interface{}) (value interface{}, extensions map[string]interface{}) {
	m, ok := val.(map[string]interface{})
	if !ok {
		return val, nil
	}
	value = m["value"]
	ext := make(map[string]interface{})
	for k, v := range m {
		if k != "value" {
			ext[k] = v
		}
	}
	if len(ext) > 0 {
		extensions = ext
	}
	return
}

func unwrapHumanNames(val interface{}) []interface{} {
	names, ok := val.([]interface{})
	if !ok {
		return nil
	}
	var out []interface{}
	for _, n := range names {
		nm := n.(map[string]interface{})
		fhirName := make(map[string]interface{})

		if f, ok := nm["family"]; ok {
			v, _ := unwrap(f)
			fhirName["family"] = v
		}
		if g, ok := nm["given"]; ok {
			fhirName["given"] = unwrapStringArray(g)
		}
		if p, ok := nm["prefix"]; ok {
			fhirName["prefix"] = unwrapStringArray(p)
		}
		// use is already a string in the proto (it's an enum, but in HumanName it maps directly)
		if u, ok := nm["use"]; ok {
			fhirName["use"] = u
		}

		out = append(out, fhirName)
	}
	return out
}

func unwrapIdentifiers(val interface{}) []interface{} {
	ids, ok := val.([]interface{})
	if !ok {
		return nil
	}
	var out []interface{}
	for _, id := range ids {
		im := id.(map[string]interface{})
		fhirId := make(map[string]interface{})
		if s, ok := im["system"]; ok {
			v, _ := unwrap(s)
			fhirId["system"] = v
		}
		if v, ok := im["value"]; ok {
			val, _ := unwrap(v)
			fhirId["value"] = val
		}
		out = append(out, fhirId)
	}
	return out
}

func unwrapStringArray(val interface{}) []interface{} {
	arr, ok := val.([]interface{})
	if !ok {
		return nil
	}
	var out []interface{}
	for _, item := range arr {
		v, _ := unwrap(item)
		out = append(out, v)
	}
	return out
}

func protoNameToFHIR(name string) string {
	// Proto uses snake_case, FHIR uses camelCase
	mapping := map[string]string{
		"birth_date":             "birthDate",
		"deceased_boolean":       "deceasedBoolean",
		"multiple_birth_boolean": "multipleBirthBoolean",
	}
	if fhir, ok := mapping[name]; ok {
		return fhir
	}
	return name
}

// TestRoundTripFidelity does the full cycle: build proto → marshal → fix → compare
func TestRoundTripFidelity(t *testing.T) {
	patient := &MiniPatient{
		Id:     &FHIRString{Value: "test-123"},
		Active: &FHIRBoolean{Value: true},
		Name: []*HumanName{
			{
				Family: &FHIRString{Value: "O'Brien"},
				Given:  []*FHIRString{{Value: "Miles"}, {Value: "Edward"}},
			},
		},
		Gender: GenderCode_MALE,
		BirthDate: &FHIRDate{
			Value: "1962-06-15",
			Extension: []*Extension{
				{
					Url:   "http://hl7.org/fhir/StructureDefinition/patient-birthTime",
					Value: &Extension_ValueDateTime{ValueDateTime: "1962-06-15T14:30:00Z"},
				},
			},
		},
	}

	// Proto → JSON
	marshaler := protojson.MarshalOptions{UseProtoNames: true}
	protoBytes, err := marshaler.Marshal(patient)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Parse the proto JSON
	var protoJSON map[string]interface{}
	json.Unmarshal(protoBytes, &protoJSON)

	// Fix to FHIR JSON
	fhirJSON := ProtoToFHIRJSON(protoJSON)

	// Validate
	assert := func(field string, got, want interface{}) {
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Errorf("%s: got %v, want %v", field, got, want)
		}
	}

	assert("id", fhirJSON["id"], "test-123")
	assert("active", fhirJSON["active"], true)
	assert("gender", fhirJSON["gender"], "male")
	assert("birthDate", fhirJSON["birthDate"], "1962-06-15")

	// birthDate should have _birthDate with extension
	bdExt := fhirJSON["_birthDate"]
	if bdExt == nil {
		t.Error("_birthDate extension lost")
	} else {
		t.Logf("_birthDate preserved: %v", bdExt)
	}

	// Names
	names := fhirJSON["name"].([]interface{})
	name0 := names[0].(map[string]interface{})
	assert("family", name0["family"], "O'Brien")

	given := name0["given"].([]interface{})
	assert("given[0]", given[0], "Miles")
	assert("given[1]", given[1], "Edward")

	out, _ := json.MarshalIndent(fhirJSON, "", "  ")
	t.Logf("Final FHIR JSON:\n%s", string(out))
}
