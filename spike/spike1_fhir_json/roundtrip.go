// Package spike1 contains the FHIR JSON round-trip spike.
//
// GOAL: Determine if google/fhir protos can round-trip FHIR JSON.
//
// APPROACH: Since compiling the full google/fhir proto chain requires
// annotations.proto and custom options, this spike takes a pragmatic
// approach:
//
// 1. Parse FHIR JSON with encoding/json into a map
// 2. Demonstrate the structural mismatch between FHIR JSON and proto JSON
// 3. Build a minimal "FHIR JSON → proto-compatible JSON" transformer
// 4. Build the reverse "proto JSON → FHIR JSON" transformer
// 5. Validate round-trip fidelity
//
// This tells us exactly how much work the ToFHIRJSON() converter needs.
package spike1

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FHIRPatient represents the minimal structure we need to validate.
// This mirrors what google/fhir Patient proto would look like when
// marshaled with protojson.Marshal().
//
// Key structural differences from standard FHIR JSON:
//
//	FHIR JSON:  "family": "Smith"
//	Proto JSON: "family": {"value": "Smith"}
//
//	FHIR JSON:  "birthDate": "1990-01-15"
//	Proto JSON: "birthDate": {"value": "1990-01-15", "extension": [...]}
//
//	FHIR JSON:  "gender": "male"
//	Proto JSON: "gender": {"value": "MALE"}  (enum, uppercased)

// WrapPrimitive wraps a FHIR flat primitive into google/fhir proto structure.
// "Smith" → {"value": "Smith"}
func WrapPrimitive(val interface{}) map[string]interface{} {
	if val == nil {
		return nil
	}
	return map[string]interface{}{"value": val}
}

// UnwrapPrimitive extracts the value from a google/fhir proto primitive wrapper.
// {"value": "Smith"} → "Smith"
// {"value": "Smith", "id": "...", "extension": [...]} → "Smith" (preserves extensions in _field)
func UnwrapPrimitive(wrapped interface{}) (value interface{}, extensions map[string]interface{}) {
	m, ok := wrapped.(map[string]interface{})
	if !ok {
		return wrapped, nil
	}

	value = m["value"]

	// Collect non-value fields (id, extension) for _field representation
	ext := make(map[string]interface{})
	for k, v := range m {
		if k != "value" {
			ext[k] = v
		}
	}
	if len(ext) > 0 {
		extensions = ext
	}
	return value, extensions
}

// FHIRToProtoJSON transforms standard FHIR JSON into the structure that
// google/fhir protojson would produce. This validates whether the
// transformation is mechanical and predictable.
//
// Key transformations:
// - Flat primitives → wrapped: "family": "Smith" → "family": {"value": "Smith"}
// - _field extensions → merged into wrapper
// - Choice[x] → oneof structure
// - Enums → uppercased enum values
func FHIRToProtoJSON(fhirJSON map[string]interface{}) map[string]interface{} {
	// For the spike, we focus on Patient-specific fields
	// A full implementation would use proto reflection
	result := make(map[string]interface{})

	// Copy resource metadata as-is (they're already complex types)
	for _, key := range []string{"resourceType", "meta", "identifier", "name",
		"telecom", "address", "maritalStatus", "communication",
		"generalPractitioner", "contact", "photo"} {
		if v, ok := fhirJSON[key]; ok {
			result[key] = v
		}
	}

	// Wrap simple primitives
	primitiveFields := []string{"id", "active", "birthDate", "deceasedBoolean",
		"multipleBirthBoolean", "multipleBirthInteger"}
	for _, key := range primitiveFields {
		if v, ok := fhirJSON[key]; ok {
			wrapped := WrapPrimitive(v)
			// Merge _field extensions if present
			if ext, ok := fhirJSON["_"+key]; ok {
				extMap, _ := ext.(map[string]interface{})
				for ek, ev := range extMap {
					wrapped[ek] = ev
				}
			}
			result[key] = wrapped
		}
	}

	// Gender: FHIR uses lowercase enum, proto uses uppercase
	if gender, ok := fhirJSON["gender"].(string); ok {
		result["gender"] = map[string]interface{}{
			"value": strings.ToUpper(gender),
		}
	}

	return result
}

// ProtoJSONToFHIR transforms google/fhir protojson output back to
// standard FHIR JSON. This is the critical "ToFHIRJSON()" function.
func ProtoJSONToFHIR(protoJSON map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	// Copy complex types as-is
	for _, key := range []string{"resourceType", "meta", "identifier", "name",
		"telecom", "address", "maritalStatus", "communication",
		"generalPractitioner", "contact", "photo"} {
		if v, ok := protoJSON[key]; ok {
			result[key] = v
		}
	}

	// Unwrap primitives
	primitiveFields := []string{"id", "active", "birthDate", "deceasedBoolean",
		"multipleBirthBoolean", "multipleBirthInteger"}
	for _, key := range primitiveFields {
		if v, ok := protoJSON[key]; ok {
			value, extensions := UnwrapPrimitive(v)
			if value != nil {
				result[key] = value
			}
			if extensions != nil {
				result["_"+key] = extensions
			}
		}
	}

	// Gender: proto uses uppercase enum, FHIR uses lowercase
	if gender, ok := protoJSON["gender"]; ok {
		value, _ := UnwrapPrimitive(gender)
		if s, ok := value.(string); ok {
			result["gender"] = strings.ToLower(s)
		}
	}

	return result
}

// RoundTrip tests the full FHIR JSON → proto → FHIR JSON cycle.
// Returns the round-tripped JSON and any fields that didn't survive.
func RoundTrip(fhirJSON []byte) (roundTripped []byte, lostFields []string, err error) {
	var original map[string]interface{}
	if err := json.Unmarshal(fhirJSON, &original); err != nil {
		return nil, nil, fmt.Errorf("parse original: %w", err)
	}

	// FHIR → Proto representation
	protoRepr := FHIRToProtoJSON(original)

	// Proto → FHIR (the critical path)
	fhirRepr := ProtoJSONToFHIR(protoRepr)

	// Compare
	for key := range original {
		if strings.HasPrefix(key, "_") {
			continue // _field extensions handled via merge
		}
		if _, ok := fhirRepr[key]; !ok {
			lostFields = append(lostFields, key)
		}
	}

	out, err := json.MarshalIndent(fhirRepr, "", "  ")
	return out, lostFields, err
}
