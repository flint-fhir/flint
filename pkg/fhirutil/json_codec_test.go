package fhirutil_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"hegel.dev/go/hegel"

	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestNormalizeFHIRJSONToProtoJSON_StandardFlatFHIR(t *testing.T) {
	extractors := postgres.DefaultIndexExtractors()

	t.Run("Standard flat FHIR Patient JSON unmarshals and extracts search indexes", func(t *testing.T) {
		rawPatient := []byte(`{
			"resourceType": "Patient",
			"id": "flat-pat-1",
			"active": true,
			"gender": "female",
			"birthDate": "1990-05-15",
			"name": [
				{
					"family": "Wonderland",
					"given": ["Alice", "Marie"]
				}
			],
			"identifier": [
				{
					"system": "http://hospital.example.org/mrn",
					"value": "MRN-999"
				}
			],
			"address": [
				{
					"postalCode": "94105"
				}
			]
		}`)

		pat := &patpb.Patient{}
		normBytes, err := fhirutil.NormalizeFHIRJSONToProtoJSON(pat.ProtoReflect().Descriptor(), rawPatient)
		require.NoError(t, err)

		unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
		require.NoError(t, unmarshaler.Unmarshal(normBytes, pat))

		assert.Equal(t, "flat-pat-1", pat.GetId().GetValue())
		assert.True(t, pat.GetActive().GetValue())
		require.Len(t, pat.GetName(), 1)
		assert.Equal(t, "Wonderland", pat.GetName()[0].GetFamily().GetValue())
		require.Len(t, pat.GetName()[0].GetGiven(), 2)
		assert.Equal(t, "Alice", pat.GetName()[0].GetGiven()[0].GetValue())

		protoBytes, err := proto.Marshal(pat)
		require.NoError(t, err)

		idx, err := extractors["Patient"]("tenant-1", "flat-pat-1", protoBytes)
		require.NoError(t, err)
		assert.NotEmpty(t, idx.Strings)
		assert.NotEmpty(t, idx.Tokens)
		assert.NotEmpty(t, idx.Dates)
	})

	t.Run("Standard flat FHIR Observation with reference and valueQuantity", func(t *testing.T) {
		rawObs := []byte(`{
			"resourceType": "Observation",
			"id": "flat-obs-1",
			"status": "final",
			"subject": {
				"reference": "Patient/flat-pat-1"
			},
			"effectiveDateTime": "2025-03-01T10:00:00Z",
			"code": {
				"coding": [
					{
						"system": "http://loinc.org",
						"code": "29463-7"
					}
				]
			},
			"valueQuantity": {
				"value": 72.5,
				"unit": "kg",
				"system": "http://unitsofmeasure.org",
				"code": "kg"
			}
		}`)

		obs := &obspb.Observation{}
		normBytes, err := fhirutil.NormalizeFHIRJSONToProtoJSON(obs.ProtoReflect().Descriptor(), rawObs)
		require.NoError(t, err)

		unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
		require.NoError(t, unmarshaler.Unmarshal(normBytes, obs))

		assert.Equal(t, "flat-obs-1", obs.GetId().GetValue())
		assert.Equal(t, "flat-pat-1", obs.GetSubject().GetPatientId().GetValue())
		assert.Equal(t, "72.5", obs.GetValue().GetQuantity().GetValue().GetValue())

		protoBytes, err := proto.Marshal(obs)
		require.NoError(t, err)

		idx, err := extractors["Observation"]("tenant-1", "flat-obs-1", protoBytes)
		require.NoError(t, err)
		require.NotEmpty(t, idx.References)
		assert.Equal(t, "flat-pat-1", idx.References[0].TargetID)
		require.NotEmpty(t, idx.Tokens)
		require.NotEmpty(t, idx.Dates)
		require.NotEmpty(t, idx.Quantities)
	})

	t.Run("Standard flat FHIR Encounter with hyphenated status in-progress and Condition reference", func(t *testing.T) {
		rawEnc := []byte(`{
			"resourceType": "Encounter",
			"id": "flat-enc-1",
			"status": "in-progress",
			"class": {
				"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode",
				"code": "IMP"
			},
			"subject": {
				"reference": "Patient/flat-pat-1"
			}
		}`)

		enc := &encpb.Encounter{}
		normBytes, err := fhirutil.NormalizeFHIRJSONToProtoJSON(enc.ProtoReflect().Descriptor(), rawEnc)
		require.NoError(t, err)

		unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
		require.NoError(t, unmarshaler.Unmarshal(normBytes, enc))
		assert.Equal(t, "flat-pat-1", enc.GetSubject().GetPatientId().GetValue())
		assert.Equal(t, "IMP", enc.GetClassValue().GetCode().GetValue())

		rawCond := []byte(`{
			"resourceType": "Condition",
			"id": "flat-cond-1",
			"subject": {
				"reference": "Patient/flat-pat-1"
			},
			"code": {
				"coding": [{"system": "http://snomed.info/sct", "code": "44054006"}]
			},
			"onsetDateTime": "2025-02-10T08:00:00Z"
		}`)

		cond := &condpb.Condition{}
		normCond, err := fhirutil.NormalizeFHIRJSONToProtoJSON(cond.ProtoReflect().Descriptor(), rawCond)
		require.NoError(t, err)
		require.NoError(t, unmarshaler.Unmarshal(normCond, cond))
		assert.Equal(t, "flat-pat-1", cond.GetSubject().GetPatientId().GetValue())
	})
}

func TestDenormalizeProtoJSONToFHIRJSON_RoundTripAndValidation(t *testing.T) {
	valEngine := validation.NewEngine(validation.DefaultOptions())
	lastUpdated := time.Date(2025, 4, 1, 12, 30, 0, 0, time.UTC)

	t.Run("Observation with valueQuantity, component, effectiveDateTime, and Reference round-trips to valid flat FHIR JSON", func(t *testing.T) {
		rawObs := []byte(`{
			"resourceType": "Observation",
			"id": "rt-obs-1",
			"status": "final",
			"code": {
				"coding": [{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure"}]
			},
			"subject": {
				"reference": "Patient/rt-pat-1"
			},
			"effectiveDateTime": "2025-03-01T10:00:00Z",
			"valueQuantity": {
				"value": 88.5,
				"unit": "beats/minute",
				"system": "http://unitsofmeasure.org",
				"code": "/min"
			},
			"component": [
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
					"valueQuantity": {"value": 120, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				}
			]
		}`)

		md := (&obspb.Observation{}).ProtoReflect().Descriptor()
		norm, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawObs)
		require.NoError(t, err)

		obs := &obspb.Observation{}
		require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(norm, obs))
		protoBytes, err := proto.Marshal(obs)
		require.NoError(t, err)

		outJSON, err := fhirutil.DenormalizeProtoJSONToFHIRJSON(md, "Observation", protoBytes, 3, lastUpdated)
		require.NoError(t, err)

		var outMap map[string]any
		require.NoError(t, json.Unmarshal(outJSON, &outMap))
		assert.Equal(t, "Observation", outMap["resourceType"])
		assert.Equal(t, "rt-obs-1", outMap["id"])
		assert.Equal(t, "final", outMap["status"])
		assert.Equal(t, "Patient/rt-pat-1", outMap["subject"].(map[string]any)["reference"])
		assert.Equal(t, 88.5, outMap["valueQuantity"].(map[string]any)["value"])
		assert.Equal(t, "3", outMap["meta"].(map[string]any)["versionId"])
		assert.Equal(t, "2025-04-01T12:30:00Z", outMap["meta"].(map[string]any)["lastUpdated"])

		outcome := valEngine.ValidateJSON("Observation", outJSON)
		assert.True(t, outcome.IsValid(), "expected round-tripped Observation JSON to pass FHIR R4 validation: %v", outcome.Issues)
	})

	t.Run("Encounter with class and hyphenated status in-progress round-trips to valid flat FHIR JSON", func(t *testing.T) {
		rawEnc := []byte(`{
			"resourceType": "Encounter",
			"id": "rt-enc-1",
			"status": "in-progress",
			"class": {
				"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode",
				"code": "AMB"
			},
			"subject": {
				"reference": "Patient/rt-pat-1"
			}
		}`)

		md := (&encpb.Encounter{}).ProtoReflect().Descriptor()
		norm, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawEnc)
		require.NoError(t, err)

		enc := &encpb.Encounter{}
		require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(norm, enc))
		protoBytes, err := proto.Marshal(enc)
		require.NoError(t, err)

		outJSON, err := fhirutil.DenormalizeProtoJSONToFHIRJSON(md, "Encounter", protoBytes, 1, lastUpdated)
		require.NoError(t, err)

		var outMap map[string]any
		require.NoError(t, json.Unmarshal(outJSON, &outMap))
		assert.Equal(t, "in-progress", outMap["status"])
		assert.Equal(t, "AMB", outMap["class"].(map[string]any)["code"])
		assert.Nil(t, outMap["classValue"])

		outcome := valEngine.ValidateJSON("Encounter", outJSON)
		assert.True(t, outcome.IsValid(), "expected round-tripped Encounter JSON to pass FHIR R4 validation: %v", outcome.Issues)
	})

	t.Run("Condition and Patient with birthDate, telecom, address, and identifier round-trip cleanly", func(t *testing.T) {
		rawPat := []byte(`{
			"resourceType": "Patient",
			"id": "rt-pat-full",
			"active": true,
			"gender": "female",
			"birthDate": "1985-11-23",
			"identifier": [{"system": "http://hospital.example/mrn", "value": "MRN-900"}],
			"name": [{"use": "official", "family": "Connor", "given": ["Sarah"]}],
			"telecom": [{"system": "phone", "value": "555-0199", "use": "mobile"}],
			"address": [{"line": ["100 Cyberdyne Way"], "city": "Los Angeles", "state": "CA", "postalCode": "90001"}]
		}`)
		patMD := (&patpb.Patient{}).ProtoReflect().Descriptor()
		normPat, err := fhirutil.NormalizeFHIRJSONToProtoJSON(patMD, rawPat)
		require.NoError(t, err)
		pat := &patpb.Patient{}
		require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normPat, pat))
		patBytes, err := proto.Marshal(pat)
		require.NoError(t, err)

		outPatJSON, err := fhirutil.DenormalizeProtoJSONToFHIRJSON(patMD, "Patient", patBytes, 2, lastUpdated)
		require.NoError(t, err)
		var outPat map[string]any
		require.NoError(t, json.Unmarshal(outPatJSON, &outPat))
		assert.Equal(t, "1985-11-23", outPat["birthDate"])
		assert.Equal(t, "female", outPat["gender"])
		assert.True(t, valEngine.ValidateJSON("Patient", outPatJSON).IsValid())
	})
}

func TestRewriteURNReferences(t *testing.T) {
	raw := []byte(`{
		"resourceType": "Observation",
		"id": "urn:uuid:obs-self-0001",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
		"subject": {"reference": "urn:uuid:1111-2222"},
		"performer": [{"reference": "urn:uuid:3333-4444"}, {"reference": "Practitioner/existing-1"}]
	}`)
	rewritten, err := fhirutil.RewriteURNReferences(raw, map[string]string{
		"urn:uuid:obs-self-0001": "Observation/obs-assigned-0",
		"urn:uuid:1111-2222":     "Patient/pat-assigned-1",
		"urn:uuid:3333-4444":     "Practitioner/prac-assigned-2",
	})
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &m))
	// Resource "id" field must strip the "Observation/" prefix so it remains a valid FHIR id
	assert.Equal(t, "obs-assigned-0", m["id"])
	assert.Equal(t, "Patient/pat-assigned-1", m["subject"].(map[string]any)["reference"])
	perfs := m["performer"].([]any)
	assert.Equal(t, "Practitioner/prac-assigned-2", perfs[0].(map[string]any)["reference"])
	assert.Equal(t, "Practitioner/existing-1", perfs[1].(map[string]any)["reference"])

	valEngine := validation.NewEngine(validation.DefaultOptions())
	outcome := valEngine.ValidateJSON("Observation", rewritten)
	assert.True(t, outcome.IsValid(), "expected rewritten Observation with id to pass FHIR validation: %v", outcome.Issues)
}

func TestProperty_NormalizeAndDenormalizeRoundTripValidates(t *testing.T) {
	md := (&patpb.Patient{}).ProtoReflect().Descriptor()
	valEngine := validation.NewEngine(validation.DefaultOptions())
	ts := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.FromRegex(`[a-z0-9]{2,16}`, true))
		family := hegel.Draw(ht, hegel.FromRegex(`[A-Za-z]{2,20}`, true))
		gender := hegel.Draw(ht, hegel.FromRegex(`(male|female|other|unknown)`, true))

		raw := []byte(`{"resourceType":"Patient","id":"` + id + `","active":true,"gender":"` + gender + `","birthDate":"1990-05-15","name":[{"family":"` + family + `"}]}`)
		norm, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, raw)
		if err != nil {
			ht.Fatalf("NormalizeFHIRJSONToProtoJSON failed: %v", err)
		}
		pat := &patpb.Patient{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(norm, pat); err != nil {
			ht.Fatalf("protojson.Unmarshal failed on normalized JSON %s: %v", string(norm), err)
		}
		protoBytes, err := proto.Marshal(pat)
		if err != nil {
			ht.Fatalf("proto.Marshal failed: %v", err)
		}
		outJSON, err := fhirutil.DenormalizeProtoJSONToFHIRJSON(md, "Patient", protoBytes, 1, ts)
		if err != nil {
			ht.Fatalf("DenormalizeProtoJSONToFHIRJSON failed: %v", err)
		}
		outcome := valEngine.ValidateJSON("Patient", outJSON)
		if !outcome.IsValid() {
			ht.Fatalf("round-tripped Patient JSON failed validation: %s", string(outJSON))
		}
	})
}
