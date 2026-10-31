package server

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/store/postgres"
)

const fuzzCanonicalPatientJSON = `{
	"resourceType": "Patient",
	"id": "fuzz-pat-1",
	"active": true,
	"gender": "female",
	"birthDate": "1985-04-12",
	"name": [
		{"use": "official", "family": "Chalmers", "given": ["Peter", "James"]},
		{"use": "usual", "given": ["Jim"]}
	],
	"telecom": [
		{"system": "phone", "value": "(03) 5555 6473", "use": "work", "rank": 1}
	],
	"address": [
		{"use": "home", "line": ["534 Erewhon St"], "city": "PleasantVille", "state": "VIC", "postalCode": "3999"}
	],
	"identifier": [
		{"system": "http://hospital.example.org/mrn", "value": "MRN-FUZZ-001"}
	]
}`

// FuzzFHIRPathR4Engine fuzzes the HL7 FHIRPath R4 evaluator (evalFHIRPath) with
// arbitrary expressions to verify zero panics on malformed or adversarial syntax.
func FuzzFHIRPathR4Engine(f *testing.F) {
	seeds := []string{
		"Patient.name.where(use = 'official').given.first()",
		"name.given.count() > 1 and active = true",
		"(1 + 2 * 3) = 7 and (10 div 3) = 3 and (10 mod 3) = 1",
		"birthDate < @2000-01-01 and (@1985-04-12 + 10 years) = @1995-04-12",
		"name.select(family + ', ' + given.first()).distinct()",
		"telecom.where(system = 'phone').value.matches('^\\(03\\).*$')",
		"name.given.aggregate($this.length() + $total, 0)",
		"true implies false",
		"{}.empty() and true.not() = false",
		"address.line.replaceMatches('[0-9]+', '#')",
		"name.repeat(given)",
		"10.5 'kg' > 5.0 'kg'",
		"((((unclosed.paren.where(x = '1')",
		"name.where(use = 'unterminated string)",
		"name[-999999999999999999999]",
		"1 / 0",
		"1 div 0",
		"1 mod 0",
		"substring(-50, 999999)",
		"...",
		"|",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	var root map[string]any
	if err := json.Unmarshal([]byte(fuzzCanonicalPatientJSON), &root); err != nil {
		f.Fatalf("unmarshal canonical patient: %v", err)
	}

	f.Fuzz(func(t *testing.T, expr string) {
		_ = evalFHIRPath(root, expr)
	})
}

// FuzzPatchEngines fuzzes RFC 6902 JSON Patch, HL7 FHIRPath Parameters Patch,
// and RFC 7396 JSON Merge Patch against a canonical FHIR R4 Patient JSON payload.
func FuzzPatchEngines(f *testing.F) {
	seeds := []string{
		`[{"op":"replace","path":"/gender","value":"other"},{"op":"add","path":"/name/0/given/-","value":"Marie"}]`,
		`[{"op":"test","path":"/active","value":true},{"op":"remove","path":"/telecom/0"}]`,
		`[{"op":"move","from":"/name/1","path":"/name/0"},{"op":"copy","from":"/gender","path":"/birthDate"}]`,
		`[{"op":"replace","path":"/name/999999/family","value":"OutOfBounds"}]`,
		`[{"op":"add","path":"invalid-no-leading-slash","value":123}]`,
		`[{"op":"move","from":"/name","path":"/name/0/given"}]`,
		`{"resourceType":"Parameters","parameter":[{"name":"operation","part":[{"name":"type","valueCode":"replace"},{"name":"path","valueString":"Patient.gender"},{"name":"value","valueCode":"male"}]}]}`,
		`{"resourceType":"Parameters","parameter":[{"name":"operation","part":[{"name":"type","valueCode":"insert"},{"name":"path","valueString":"Patient.name"},{"name":"index","valueInteger":0},{"name":"value","valueHumanName":{"family":"Added"}}]}]}`,
		`{"resourceType":"Parameters","parameter":[{"name":"operation","part":[{"name":"type","valueCode":"delete"},{"name":"path","valueString":"Patient.telecom[0]"}}]}`,
		`{"gender":"unknown","telecom":null,"address":[{"city":"UpdatedCity"}]}`,
		`[`,
		`{"malformed":`,
		`null`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	baseDoc := []byte(fuzzCanonicalPatientJSON)

	f.Fuzz(func(t *testing.T, patchPayload string) {
		raw := []byte(patchPayload)
		_, _, _ = ApplyJSONPatch(baseDoc, raw)
		_, _, _ = ApplyFHIRPatch(baseDoc, raw)
		_, _ = ApplyMergePatch(baseDoc, raw)
	})
}

// FuzzValidationAndDynamicExtraction fuzzes US Core / StructureDefinition JSON validation,
// ProtoJSON normalization, dynamicpb unmarshaling, and search index extraction.
func FuzzValidationAndDynamicExtraction(f *testing.F) {
	seeds := []string{
		fuzzCanonicalPatientJSON,
		`{"resourceType":"Observation","id":"obs-1","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/fuzz-pat-1"},"effectiveDateTime":"2026-01-15T10:30:00Z","valueQuantity":{"value":72,"unit":"/min","system":"http://unitsofmeasure.org","code":"/min"}}`,
		`{"resourceType":"Patient","id":"invalid id with spaces","gender":"not-a-valid-gender","birthDate":"9999-99-99","unknownElement":123}`,
		`{"resourceType":"Patient","extension":[{"url":"http://hl7.org/fhir/us/core/StructureDefinition/us-core-race","extension":[{"url":"ombCategory","valueCoding":{"system":"urn:oid:2.16.840.1.113883.6.238","code":"2106-3"}}]}],"contained":[{"resourceType":"Organization","id":"org1","name":"Contained Org"}],"managingOrganization":{"reference":"#org1"}}`,
		`{"resourceType":123,"id":null,"name":"not-an-array"}`,
		`[]`,
		`{`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	valEngine := validation.NewEngine(validation.DefaultOptions())
	descriptors := fhirutil.AllR4ResourceDescriptors()
	patMD := descriptors["Patient"]
	obsMD := descriptors["Observation"]
	patExtractor := postgres.NewDynamicResourceExtractor("Patient", patMD)
	obsExtractor := postgres.NewDynamicResourceExtractor("Observation", obsMD)

	f.Fuzz(func(t *testing.T, rawJSON string) {
		payload := []byte(rawJSON)
		for _, spec := range []struct {
			resType   string
			md        func()
			extractor postgres.IndexExtractorFunc
		}{
			{resType: "Patient", extractor: patExtractor},
			{resType: "Observation", extractor: obsExtractor},
		} {
			_ = valEngine.ValidateJSON(spec.resType, payload)
			md := patMD
			if spec.resType == "Observation" {
				md = obsMD
			}
			norm, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, payload)
			if err != nil {
				continue
			}
			msg := dynamicpb.NewMessage(md)
			unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
			if err := unmarshaler.Unmarshal(norm, msg); err != nil {
				continue
			}
			pb, err := proto.Marshal(msg)
			if err != nil {
				continue
			}
			_, _ = spec.extractor("fuzz-tenant", "fuzz-id", pb)
		}
	})
}
