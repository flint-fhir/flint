package postgres_test

import (
	"testing"
	"time"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/gen/go/store/postgres"
)

func TestExtractPatientIndexes(t *testing.T) {
	patient := &patpb.Patient{
		Id:     &dtpb.Id{Value: "pat-123"},
		Active: &dtpb.Boolean{Value: true},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "Smith"},
				Given: []*dtpb.String{
					{Value: "John"},
					{Value: "Michael"},
				},
			},
		},
		BirthDate: &dtpb.Date{
			ValueUs: 631152000000000,
		},
		Address: []*dtpb.Address{
			{
				City:  &dtpb.String{Value: "Nashville"},
				State: &dtpb.String{Value: "TN"},
			},
		},
		ManagingOrganization: &dtpb.Reference{
			Reference: &dtpb.Reference_OrganizationId{
				OrganizationId: &dtpb.ReferenceId{Value: "org-789"},
			},
		},
	}

	idx := postgres.ExtractPatientIndexes("tenant-hca", patient)
	require.NotNil(t, idx)

	// Verify family / given
	var families, givens, cities []string
	for _, s := range idx.Strings {
		switch s.SpName {
		case "family":
			families = append(families, s.SpValue)
		case "given":
			givens = append(givens, s.SpValue)
		case "address-city":
			cities = append(cities, s.SpValue)
		}
	}
	assert.Contains(t, families, "smith")
	assert.Contains(t, givens, "john")
	assert.Contains(t, givens, "michael")
	assert.Contains(t, cities, "nashville")

	// Verify active token
	var activeTokens []string
	for _, tok := range idx.Tokens {
		if tok.SpName == "active" {
			activeTokens = append(activeTokens, tok.SpValue)
		}
	}
	assert.Contains(t, activeTokens, "true")

	// Verify date
	require.NotEmpty(t, idx.Dates)
	assert.Equal(t, "birthdate", idx.Dates[0].SpName)
	assert.Equal(t, time.UnixMicro(631152000000000).UTC(), idx.Dates[0].SpLow)

	// Verify reference
	require.NotEmpty(t, idx.References)
	assert.Equal(t, "organization", idx.References[0].SpName)
	assert.Equal(t, "Organization", idx.References[0].TargetType)
	assert.Equal(t, "org-789", idx.References[0].TargetID)
}

func TestExtractConditionIndexes(t *testing.T) {
	cond := &condpb.Condition{
		Id: &dtpb.Id{Value: "cond-123"},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-123"},
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

	idx := postgres.ExtractConditionIndexes("tenant-hca", cond)
	require.NotNil(t, idx)

	// Verify code token
	var codes []string
	for _, tok := range idx.Tokens {
		if tok.SpName == "code" {
			codes = append(codes, tok.SpValue)
		}
	}
	assert.Contains(t, codes, "44054006")

	// Verify onset date
	var onsetDates []postgres.SpidxDate
	for _, d := range idx.Dates {
		if d.SpName == "onset-date" {
			onsetDates = append(onsetDates, d)
		}
	}
	require.NotEmpty(t, onsetDates)
	assert.Equal(t, time.UnixMicro(1609459200000000).UTC(), onsetDates[0].SpLow)

	// Verify patient / subject reference
	var subjectRefs []postgres.SpidxReference
	for _, ref := range idx.References {
		if ref.SpName == "subject" || ref.SpName == "patient" {
			subjectRefs = append(subjectRefs, ref)
		}
	}
	require.NotEmpty(t, subjectRefs)
	assert.Equal(t, "pat-123", subjectRefs[0].TargetID)
}

func TestExtractEncounterIndexes(t *testing.T) {
	enc := &encpb.Encounter{
		Id: &dtpb.Id{Value: "enc-123"},
		ClassValue: &dtpb.Coding{
			System: &dtpb.Uri{Value: "http://terminology.hl7.org/CodeSystem/v3-ActCode"},
			Code:   &dtpb.Code{Value: "AMB"},
		},
		Period: &dtpb.Period{
			Start: &dtpb.DateTime{ValueUs: 1609459200000000},
			End:   &dtpb.DateTime{ValueUs: 1609462800000000},
		},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-123"},
			},
		},
	}

	idx := postgres.ExtractEncounterIndexes("tenant-hca", enc)
	require.NotNil(t, idx)

	// Verify class token
	var classValues []string
	for _, tok := range idx.Tokens {
		if tok.SpName == "class" {
			classValues = append(classValues, tok.SpValue)
		}
	}
	assert.Contains(t, classValues, "AMB")

	// Verify period date
	var periodDates []postgres.SpidxDate
	for _, d := range idx.Dates {
		if d.SpName == "date" {
			periodDates = append(periodDates, d)
		}
	}
	require.NotEmpty(t, periodDates)
	assert.Equal(t, time.UnixMicro(1609459200000000).UTC(), periodDates[0].SpLow)
	assert.Equal(t, time.UnixMicro(1609462800000000).UTC(), periodDates[0].SpHigh)
}

func TestExtractObservationIndexes(t *testing.T) {
	obs := &obspb.Observation{
		Id: &dtpb.Id{Value: "obs-123"},
		Code: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{
					System: &dtpb.Uri{Value: "http://loinc.org"},
					Code:   &dtpb.Code{Value: "8867-4"},
				},
			},
		},
		Effective: &obspb.Observation_EffectiveX{
			Choice: &obspb.Observation_EffectiveX_DateTime{
				DateTime: &dtpb.DateTime{ValueUs: 1609459200000000},
			},
		},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-123"},
			},
		},
	}

	idx := postgres.ExtractObservationIndexes("tenant-hca", obs)
	require.NotNil(t, idx)

	// Verify code token
	var codes []string
	for _, tok := range idx.Tokens {
		if tok.SpName == "code" {
			codes = append(codes, tok.SpValue)
		}
	}
	assert.Contains(t, codes, "8867-4")

	// Verify effective date
	var dates []postgres.SpidxDate
	for _, d := range idx.Dates {
		if d.SpName == "date" {
			dates = append(dates, d)
		}
	}
	require.NotEmpty(t, dates)
	assert.Equal(t, time.UnixMicro(1609459200000000).UTC(), dates[0].SpLow)
}

func TestExtractPractitionerIndexes(t *testing.T) {
	prac := &pracpb.Practitioner{
		Id:     &dtpb.Id{Value: "prac-123"},
		Active: &dtpb.Boolean{Value: true},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "House"},
				Given:  []*dtpb.String{{Value: "Gregory"}},
			},
		},
	}

	idx := postgres.ExtractPractitionerIndexes("tenant-hca", prac)
	require.NotNil(t, idx)

	var families, givens []string
	for _, s := range idx.Strings {
		switch s.SpName {
		case "family":
			families = append(families, s.SpValue)
		case "given":
			givens = append(givens, s.SpValue)
		}
	}
	assert.Contains(t, families, "house")
	assert.Contains(t, givens, "gregory")
}
