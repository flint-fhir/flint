package postgres_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"

	"github.com/flint-fhir/flint/store/postgres"
)

func TestDefaultIndexExtractors(t *testing.T) {
	extractors := postgres.DefaultIndexExtractors()
	require.NotNil(t, extractors)
	assert.Contains(t, extractors, "Patient")
	assert.Contains(t, extractors, "Condition")
	assert.Contains(t, extractors, "Encounter")
	assert.Contains(t, extractors, "Observation")
	assert.Contains(t, extractors, "Practitioner")
	assert.Len(t, extractors, 5)
}

func TestExtractPatient(t *testing.T) {
	pat := &patpb.Patient{
		Id:     &dtpb.Id{Value: "pat-100"},
		Active: &dtpb.Boolean{Value: true},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "Doe"},
				Given:  []*dtpb.String{{Value: "Jane"}},
			},
		},
		BirthDate: &dtpb.Date{ValueUs: 631152000000000},
	}
	bytes, err := proto.Marshal(pat)
	require.NoError(t, err)

	idx, err := postgres.ExtractPatient("tenant-1", "pat-100", bytes)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundFamily, foundGiven, foundActive bool
	for _, s := range idx.Strings {
		if s.SpName == "family" && s.SpValue == "doe" {
			foundFamily = true
		}
		if s.SpName == "given" && s.SpValue == "jane" {
			foundGiven = true
		}
	}
	for _, tok := range idx.Tokens {
		if tok.SpName == "active" && tok.SpValue == "true" {
			foundActive = true
		}
	}
	assert.True(t, foundFamily, "expected family name doe")
	assert.True(t, foundGiven, "expected given name jane")
	assert.True(t, foundActive, "expected active token true")
	require.NotEmpty(t, idx.Dates)
	assert.Equal(t, "birthdate", idx.Dates[0].SpName)
}

func TestExtractPatient_FallbackResID(t *testing.T) {
	// Patient without Id field in proto — should fall back to passed resID
	pat := &patpb.Patient{
		Active: &dtpb.Boolean{Value: true},
	}
	bytes, err := proto.Marshal(pat)
	require.NoError(t, err)

	idx, err := postgres.ExtractPatient("tenant-1", "fallback-id", bytes)
	require.NoError(t, err)
	require.NotNil(t, idx)
	require.NotEmpty(t, idx.Tokens)
	assert.Equal(t, "fallback-id", idx.Tokens[0].ResID)
}

func TestExtractPatient_InvalidBytes(t *testing.T) {
	_, err := postgres.ExtractPatient("tenant-1", "p1", []byte("invalid-proto"))
	require.Error(t, err)
}

func TestExtractCondition(t *testing.T) {
	cond := &condpb.Condition{
		Id: &dtpb.Id{Value: "cond-100"},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-100"},
			},
		},
		Code: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{
					System: &dtpb.Uri{Value: "http://snomed.info/sct"},
					Code:   &dtpb.Code{Value: "44054006"},
				},
			},
		},
		Onset: &condpb.Condition_OnsetX{
			Choice: &condpb.Condition_OnsetX_DateTime{
				DateTime: &dtpb.DateTime{
					ValueUs: 1609459200000000,
				},
			},
		},
	}
	bytes, err := proto.Marshal(cond)
	require.NoError(t, err)

	idx, err := postgres.ExtractCondition("tenant-1", "cond-100", bytes)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundCode bool
	for _, tok := range idx.Tokens {
		if tok.SpName == "code" && tok.SpValue == "44054006" {
			foundCode = true
		}
	}
	assert.True(t, foundCode, "expected condition code")
	require.NotEmpty(t, idx.Dates)
	assert.Equal(t, "onset-date", idx.Dates[0].SpName)
}

func TestExtractEncounter(t *testing.T) {
	enc := &encpb.Encounter{
		Id: &dtpb.Id{Value: "enc-100"},
		ClassValue: &dtpb.Coding{
			System: &dtpb.Uri{Value: "http://terminology.hl7.org/CodeSystem/v3-ActCode"},
			Code:   &dtpb.Code{Value: "AMB"},
		},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-100"},
			},
		},
		Period: &dtpb.Period{
			Start: &dtpb.DateTime{ValueUs: 1609459200000000},
			End:   &dtpb.DateTime{ValueUs: 1609462800000000},
		},
	}
	bytes, err := proto.Marshal(enc)
	require.NoError(t, err)

	idx, err := postgres.ExtractEncounter("tenant-1", "enc-100", bytes)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundClass bool
	for _, tok := range idx.Tokens {
		if tok.SpName == "class" && tok.SpValue == "AMB" {
			foundClass = true
		}
	}
	assert.True(t, foundClass, "expected class token AMB")
	require.NotEmpty(t, idx.Dates)
	assert.Equal(t, time.UnixMicro(1609459200000000).UTC(), idx.Dates[0].SpLow)
}

func TestExtractObservation(t *testing.T) {
	obs := &obspb.Observation{
		Id: &dtpb.Id{Value: "obs-100"},
		Code: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{
					System: &dtpb.Uri{Value: "http://loinc.org"},
					Code:   &dtpb.Code{Value: "8867-4"},
				},
			},
		},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-100"},
			},
		},
		Effective: &obspb.Observation_EffectiveX{
			Choice: &obspb.Observation_EffectiveX_DateTime{
				DateTime: &dtpb.DateTime{ValueUs: 1609459200000000},
			},
		},
	}
	bytes, err := proto.Marshal(obs)
	require.NoError(t, err)

	idx, err := postgres.ExtractObservation("tenant-1", "obs-100", bytes)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundCode bool
	for _, tok := range idx.Tokens {
		if tok.SpName == "code" && tok.SpValue == "8867-4" {
			foundCode = true
		}
	}
	assert.True(t, foundCode, "expected observation code 8867-4")
}

func TestExtractPractitioner(t *testing.T) {
	prac := &pracpb.Practitioner{
		Id:     &dtpb.Id{Value: "prac-100"},
		Active: &dtpb.Boolean{Value: true},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "Watson"},
				Given:  []*dtpb.String{{Value: "John"}},
			},
		},
	}
	bytes, err := proto.Marshal(prac)
	require.NoError(t, err)

	idx, err := postgres.ExtractPractitioner("tenant-1", "prac-100", bytes)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundFamily bool
	for _, s := range idx.Strings {
		if s.SpName == "family" && s.SpValue == "watson" {
			foundFamily = true
		}
	}
	assert.True(t, foundFamily, "expected family name watson")
}
