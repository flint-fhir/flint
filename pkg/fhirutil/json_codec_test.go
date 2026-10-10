package fhirutil_test

import (
	"testing"

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

func TestProperty_NormalizeFHIRJSONNeverPanics(t *testing.T) {
	md := (&patpb.Patient{}).ProtoReflect().Descriptor()
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.FromRegex(`[a-z0-9\-]{1,16}`, true))
		family := hegel.Draw(ht, hegel.FromRegex(`[A-Za-z]{1,20}`, true))
		gender := hegel.Draw(ht, hegel.FromRegex(`(male|female|other|unknown)`, true))

		raw := []byte(`{"resourceType":"Patient","id":"` + id + `","gender":"` + gender + `","name":[{"family":"` + family + `"}]}`)
		norm, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, raw)
		if err != nil {
			ht.Fatalf("NormalizeFHIRJSONToProtoJSON failed: %v", err)
		}
		pat := &patpb.Patient{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(norm, pat); err != nil {
			ht.Fatalf("protojson.Unmarshal failed on normalized JSON %s: %v", string(norm), err)
		}
		if pat.GetId().GetValue() != id {
			ht.Fatalf("expected id %q, got %q", id, pat.GetId().GetValue())
		}
	})
}
