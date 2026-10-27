package postgres_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	c3pb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/codes_go_proto"
	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	allpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/allergy_intolerance_go_proto"
	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	diagpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/diagnostic_report_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	immpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/immunization_go_proto"
	locpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/location_go_proto"
	medreqpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/medication_request_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	orgpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/organization_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"
	procpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/procedure_go_proto"
	specpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/specimen_go_proto"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestDefaultIndexExtractors(t *testing.T) {
	extractors := postgres.DefaultIndexExtractors()
	require.NotNil(t, extractors)
	for _, rt := range fhirutil.USCoreResourceTypes() {
		assert.Contains(t, extractors, rt, "expected US Core resource type %s in DefaultIndexExtractors", rt)
	}
	assert.Contains(t, extractors, "Task")
	assert.Contains(t, extractors, "Appointment")
	assert.Contains(t, extractors, "Claim")
	assert.Contains(t, extractors, "RiskAssessment")
	assert.Contains(t, extractors, "VisionPrescription")
	assert.Len(t, extractors, len(fhirutil.AllR4ResourceTypes()))
	assert.GreaterOrEqual(t, len(extractors), 145)
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
		Status: &obspb.Observation_StatusCode{
			Value: 3, // FINAL
		},
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
		Value: &obspb.Observation_ValueX{
			Choice: &obspb.Observation_ValueX_Quantity{
				Quantity: &dtpb.Quantity{
					Value:  &dtpb.Decimal{Value: "88.5"},
					Unit:   &dtpb.String{Value: "beats/minute"},
					System: &dtpb.Uri{Value: "http://unitsofmeasure.org"},
					Code:   &dtpb.Code{Value: "/min"},
				},
			},
		},
		Component: []*obspb.Observation_Component{
			{
				Code: &dtpb.CodeableConcept{
					Coding: []*dtpb.Coding{
						{
							System: &dtpb.Uri{Value: "http://loinc.org"},
							Code:   &dtpb.Code{Value: "8480-6"},
						},
					},
				},
				Value: &obspb.Observation_Component_ValueX{
					Choice: &obspb.Observation_Component_ValueX_Quantity{
						Quantity: &dtpb.Quantity{
							Value:  &dtpb.Decimal{Value: "120"},
							Unit:   &dtpb.String{Value: "mmHg"},
							System: &dtpb.Uri{Value: "http://unitsofmeasure.org"},
							Code:   &dtpb.Code{Value: "mm[Hg]"},
						},
					},
				},
			},
		},
	}
	bytes, err := proto.Marshal(obs)
	require.NoError(t, err)

	idx, err := postgres.ExtractObservation("tenant-1", "obs-100", bytes)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundCode, foundStatusFinal, foundComponentCode, foundComboCode bool
	for _, tok := range idx.Tokens {
		if tok.SpName == "code" && tok.SpValue == "8867-4" {
			foundCode = true
		}
		if tok.SpName == "status" && tok.SpValue == "final" {
			foundStatusFinal = true
		}
		if tok.SpName == "component-code" && tok.SpValue == "8480-6" {
			foundComponentCode = true
		}
		if tok.SpName == "combo-code" && tok.SpValue == "8480-6" {
			foundComboCode = true
		}
	}
	assert.True(t, foundCode, "expected observation code 8867-4")
	assert.True(t, foundStatusFinal, "expected normalized lowercase observation status final")
	assert.True(t, foundComponentCode, "expected component-code 8480-6")
	assert.True(t, foundComboCode, "expected combo-code 8480-6")

	var foundValQty, foundCompQty, foundComboQty, foundCompCVQSys, foundCompCVQCode bool
	for _, q := range idx.Quantities {
		if q.SpName == "value-quantity" && q.SpValue == 88.5 && q.SpCode == "/min" {
			foundValQty = true
		}
		if q.SpName == "component-value-quantity" && q.SpValue == 120 && q.SpCode == "mm[Hg]" {
			foundCompQty = true
		}
		if q.SpName == "combo-value-quantity" && q.SpValue == 88.5 {
			foundComboQty = true
		}
		if q.SpName == "comp-cvq:http://loinc.org|8480-6" && q.SpValue == 120 && q.SpCode == "mm[Hg]" {
			foundCompCVQSys = true
		}
		if q.SpName == "comp-cvq:8480-6" && q.SpValue == 120 && q.SpCode == "mm[Hg]" {
			foundCompCVQCode = true
		}
	}
	assert.True(t, foundValQty, "expected value-quantity index 88.5 /min")
	assert.True(t, foundCompQty, "expected component-value-quantity index 120 mm[Hg]")
	assert.True(t, foundComboQty, "expected combo-value-quantity index")
	assert.True(t, foundCompCVQSys, "expected paired composite quantity comp-cvq:http://loinc.org|8480-6")
	assert.True(t, foundCompCVQCode, "expected paired composite quantity comp-cvq:8480-6")
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

func TestExtractMedicationRequest(t *testing.T) {
	mr := &medreqpb.MedicationRequest{
		Id: &dtpb.Id{Value: "mr-100"},
		Status: &medreqpb.MedicationRequest_StatusCode{
			Value: c3pb.MedicationrequestStatusCode_ACTIVE,
		},
		Intent: &medreqpb.MedicationRequest_IntentCode{
			Value: c3pb.MedicationRequestIntentCode_ORDER,
		},
		Medication: &medreqpb.MedicationRequest_MedicationX{
			Choice: &medreqpb.MedicationRequest_MedicationX_CodeableConcept{
				CodeableConcept: &dtpb.CodeableConcept{
					Coding: []*dtpb.Coding{
						{
							System: &dtpb.Uri{Value: "http://www.nlm.nih.gov/research/umls/rxnorm"},
							Code:   &dtpb.Code{Value: "1049502"},
						},
					},
				},
			},
		},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-100"},
			},
		},
		AuthoredOn: &dtpb.DateTime{ValueUs: 1700000000000000},
	}
	b, err := proto.Marshal(mr)
	require.NoError(t, err)

	idx, err := postgres.ExtractMedicationRequest("tenant-1", "mr-100", b)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundCode, foundStatus, foundIntent bool
	for _, tok := range idx.Tokens {
		if tok.SpName == "code" && tok.SpValue == "1049502" {
			foundCode = true
		}
		if tok.SpName == "status" && tok.SpValue == "active" {
			foundStatus = true
		}
		if tok.SpName == "intent" && tok.SpValue == "order" {
			foundIntent = true
		}
	}
	assert.True(t, foundCode, "expected RxNorm code 1049502")
	assert.True(t, foundStatus, "expected active status")
	assert.True(t, foundIntent, "expected order intent")
	require.NotEmpty(t, idx.Dates)
	assert.Equal(t, "authoredon", idx.Dates[0].SpName)
}

func TestExtractAllergyIntolerance(t *testing.T) {
	ai := &allpb.AllergyIntolerance{
		Id: &dtpb.Id{Value: "allergy-100"},
		ClinicalStatus: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{
					System: &dtpb.Uri{Value: "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical"},
					Code:   &dtpb.Code{Value: "active"},
				},
			},
		},
		Criticality: &allpb.AllergyIntolerance_CriticalityCode{
			Value: c3pb.AllergyIntoleranceCriticalityCode_HIGH,
		},
		Code: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{
					System: &dtpb.Uri{Value: "http://snomed.info/sct"},
					Code:   &dtpb.Code{Value: "91936005"},
				},
			},
		},
		Patient: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-100"},
			},
		},
	}
	b, err := proto.Marshal(ai)
	require.NoError(t, err)

	idx, err := postgres.ExtractAllergyIntolerance("tenant-1", "allergy-100", b)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundCode, foundCrit, foundPatient, foundSubject bool
	for _, tok := range idx.Tokens {
		if tok.SpName == "code" && tok.SpValue == "91936005" {
			foundCode = true
		}
		if tok.SpName == "criticality" && tok.SpValue == "high" {
			foundCrit = true
		}
	}
	for _, ref := range idx.References {
		if ref.SpName == "patient" && ref.TargetID == "pat-100" {
			foundPatient = true
		}
		if ref.SpName == "subject" && ref.TargetID == "pat-100" {
			foundSubject = true
		}
	}
	assert.True(t, foundCode)
	assert.True(t, foundCrit)
	assert.True(t, foundPatient)
	assert.True(t, foundSubject, "expected automatic subject alias for patient reference")
}

func TestExtractProcedure(t *testing.T) {
	proc := &procpb.Procedure{
		Id: &dtpb.Id{Value: "proc-100"},
		Status: &procpb.Procedure_StatusCode{
			Value: c3pb.EventStatusCode_COMPLETED,
		},
		Code: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{
					System: &dtpb.Uri{Value: "http://snomed.info/sct"},
					Code:   &dtpb.Code{Value: "80146002"},
				},
			},
		},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-100"},
			},
		},
		Performed: &procpb.Procedure_PerformedX{
			Choice: &procpb.Procedure_PerformedX_DateTime{
				DateTime: &dtpb.DateTime{ValueUs: 1700000000000000},
			},
		},
	}
	b, err := proto.Marshal(proc)
	require.NoError(t, err)

	idx, err := postgres.ExtractProcedure("tenant-1", "proc-100", b)
	require.NoError(t, err)
	require.NotNil(t, idx)
	assert.NotEmpty(t, idx.Tokens)
	assert.NotEmpty(t, idx.Dates)
}

func TestExtractDiagnosticReport(t *testing.T) {
	dr := &diagpb.DiagnosticReport{
		Id: &dtpb.Id{Value: "dr-100"},
		Status: &diagpb.DiagnosticReport_StatusCode{
			Value: c3pb.DiagnosticReportStatusCode_FINAL,
		},
		Code: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{
					System: &dtpb.Uri{Value: "http://loinc.org"},
					Code:   &dtpb.Code{Value: "58410-2"},
				},
			},
		},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-100"},
			},
		},
		Result: []*dtpb.Reference{
			{
				Reference: &dtpb.Reference_ObservationId{
					ObservationId: &dtpb.ReferenceId{Value: "obs-100"},
				},
			},
		},
	}
	b, err := proto.Marshal(dr)
	require.NoError(t, err)

	idx, err := postgres.ExtractDiagnosticReport("tenant-1", "dr-100", b)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundResult bool
	for _, ref := range idx.References {
		if ref.SpName == "result" && ref.TargetType == "Observation" && ref.TargetID == "obs-100" {
			foundResult = true
		}
	}
	assert.True(t, foundResult, "expected result reference to Observation/obs-100")
}

func TestExtractImmunization(t *testing.T) {
	imm := &immpb.Immunization{
		Id: &dtpb.Id{Value: "imm-100"},
		VaccineCode: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{
					System: &dtpb.Uri{Value: "http://hl7.org/fhir/sid/cvx"},
					Code:   &dtpb.Code{Value: "208"},
				},
			},
		},
		Patient: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-100"},
			},
		},
		Occurrence: &immpb.Immunization_OccurrenceX{
			Choice: &immpb.Immunization_OccurrenceX_DateTime{
				DateTime: &dtpb.DateTime{ValueUs: 1700000000000000},
			},
		},
	}
	b, err := proto.Marshal(imm)
	require.NoError(t, err)

	idx, err := postgres.ExtractImmunization("tenant-1", "imm-100", b)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundVaccine bool
	for _, tok := range idx.Tokens {
		if tok.SpName == "vaccine-code" && tok.SpValue == "208" {
			foundVaccine = true
		}
	}
	assert.True(t, foundVaccine, "expected CVX vaccine-code 208")
}

func TestExtractOrganization(t *testing.T) {
	org := &orgpb.Organization{
		Id:     &dtpb.Id{Value: "org-100"},
		Active: &dtpb.Boolean{Value: true},
		Name:   &dtpb.String{Value: "Flint Health System"},
		Alias:  []*dtpb.String{{Value: "FHS"}},
	}
	b, err := proto.Marshal(org)
	require.NoError(t, err)

	idx, err := postgres.ExtractOrganization("tenant-1", "org-100", b)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundName, foundAlias bool
	for _, s := range idx.Strings {
		if s.SpName == "name" && s.SpValue == "flint health system" {
			foundName = true
		}
		if s.SpName == "name" && s.SpValue == "fhs" {
			foundAlias = true
		}
	}
	assert.True(t, foundName)
	assert.True(t, foundAlias)
}

func TestExtractSpecimen(t *testing.T) {
	spec := &specpb.Specimen{
		Id: &dtpb.Id{Value: "spec-100"},
		Status: &specpb.Specimen_StatusCode{
			Value: c3pb.SpecimenStatusCode_AVAILABLE,
		},
		AccessionIdentifier: &dtpb.Identifier{
			System: &dtpb.Uri{Value: "http://hospital.example.org/specimens"},
			Value:  &dtpb.String{Value: "ACC-100"},
		},
		Type: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{
					System: &dtpb.Uri{Value: "http://snomed.info/sct"},
					Code:   &dtpb.Code{Value: "119297000"},
				},
			},
		},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "pat-1"},
			},
		},
	}
	b, err := proto.Marshal(spec)
	require.NoError(t, err)

	idx, err := postgres.ExtractSpecimen("tenant-1", "spec-100", b)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundStatus, foundAccession, foundType, foundPatient bool
	for _, tok := range idx.Tokens {
		if tok.SpName == "status" && tok.SpValue == "available" {
			foundStatus = true
		}
		if tok.SpName == "accession" && tok.SpValue == "ACC-100" {
			foundAccession = true
		}
		if tok.SpName == "type" && tok.SpValue == "119297000" {
			foundType = true
		}
	}
	for _, ref := range idx.References {
		if ref.SpName == "patient" && ref.TargetType == "Patient" && ref.TargetID == "pat-1" {
			foundPatient = true
		}
	}
	assert.True(t, foundStatus, "expected status available")
	assert.True(t, foundAccession, "expected accession ACC-100")
	assert.True(t, foundType, "expected SNOMED type 119297000")
	assert.True(t, foundPatient, "expected patient reference pat-1")
}

func TestExtractLocation(t *testing.T) {
	loc := &locpb.Location{
		Id: &dtpb.Id{Value: "loc-100"},
		Status: &locpb.Location_StatusCode{
			Value: c3pb.LocationStatusCode_ACTIVE,
		},
		Name: &dtpb.String{Value: "Flint Cardiology Clinic"},
		Address: &dtpb.Address{
			City:       &dtpb.String{Value: "Boston"},
			State:      &dtpb.String{Value: "MA"},
			PostalCode: &dtpb.String{Value: "02115"},
		},
		ManagingOrganization: &dtpb.Reference{
			Reference: &dtpb.Reference_OrganizationId{
				OrganizationId: &dtpb.ReferenceId{Value: "org-100"},
			},
		},
	}
	b, err := proto.Marshal(loc)
	require.NoError(t, err)

	idx, err := postgres.ExtractLocation("tenant-1", "loc-100", b)
	require.NoError(t, err)
	require.NotNil(t, idx)

	var foundName, foundCity bool
	for _, s := range idx.Strings {
		if s.SpName == "name" && s.SpValue == "flint cardiology clinic" {
			foundName = true
		}
		if s.SpName == "address-city" && s.SpValue == "boston" {
			foundCity = true
		}
	}
	var foundStatus, foundOrg bool
	for _, tok := range idx.Tokens {
		if tok.SpName == "status" && tok.SpValue == "active" {
			foundStatus = true
		}
	}
	for _, ref := range idx.References {
		if ref.SpName == "organization" && ref.TargetType == "Organization" && ref.TargetID == "org-100" {
			foundOrg = true
		}
	}
	assert.True(t, foundName, "expected name")
	assert.True(t, foundCity, "expected address-city boston")
	assert.True(t, foundStatus, "expected status active")
	assert.True(t, foundOrg, "expected organization reference org-100")
}

func jsonToProtoBytes(t *testing.T, md protoreflect.MessageDescriptor, rawJSON []byte) []byte {
	t.Helper()
	norm, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawJSON)
	require.NoError(t, err)
	msg := dynamicpb.NewMessage(md)
	require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(norm, msg))
	b, err := proto.Marshal(msg)
	require.NoError(t, err)
	return b
}

func TestDynamicResourceExtractor_USCoreAndR4Resources(t *testing.T) {
	extractors := postgres.DefaultIndexExtractors()
	descriptors := fhirutil.AllR4ResourceDescriptors()

	t.Run("DocumentReference dynamic extraction", func(t *testing.T) {
		rawJSON := []byte(`{
			"resourceType": "DocumentReference",
			"id": "docref-test-1",
			"status": "current",
			"type": {"coding": [{"system": "http://loinc.org", "code": "34133-9"}]},
			"category": [{"coding": [{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-documentreference-category", "code": "clinical-note"}]}],
			"subject": {"reference": "Patient/pat-100"},
			"date": "2025-03-10T10:30:00Z",
			"content": [{"attachment": {"contentType": "text/plain", "url": "Binary/bin-1"}}]
		}`)
		protoBytes := jsonToProtoBytes(t, descriptors["DocumentReference"], rawJSON)
		idx, err := extractors["DocumentReference"]("tenant-1", "docref-test-1", protoBytes)
		require.NoError(t, err)
		require.NotNil(t, idx)

		var foundStatus, foundType, foundCategory, foundPatient bool
		for _, tok := range idx.Tokens {
			if tok.SpName == "status" && tok.SpValue == "current" {
				foundStatus = true
			}
			if tok.SpName == "type" && tok.SpValue == "34133-9" {
				foundType = true
			}
			if tok.SpName == "category" && tok.SpValue == "clinical-note" {
				foundCategory = true
			}
		}
		for _, ref := range idx.References {
			if ref.SpName == "patient" && ref.TargetType == "Patient" && ref.TargetID == "pat-100" {
				foundPatient = true
			}
		}
		assert.True(t, foundStatus, "expected status=current")
		assert.True(t, foundType, "expected type=34133-9")
		assert.True(t, foundCategory, "expected category=clinical-note")
		assert.True(t, foundPatient, "expected patient=Patient/pat-100")
		assert.NotEmpty(t, idx.Dates, "expected date index")
	})

	t.Run("CarePlan, Goal, Coverage, Provenance, and Task dynamic extraction", func(t *testing.T) {
		// CarePlan
		cpJSON := []byte(`{
			"resourceType": "CarePlan",
			"id": "cp-test-1",
			"status": "active",
			"intent": "plan",
			"category": [{"coding": [{"system": "http://hl7.org/fhir/us/core/CodeSystem/careplan-category", "code": "assess-plan"}]}],
			"subject": {"reference": "Patient/pat-100"}
		}`)
		cpBytes := jsonToProtoBytes(t, descriptors["CarePlan"], cpJSON)
		cpIdx, err := extractors["CarePlan"]("tenant-1", "cp-test-1", cpBytes)
		require.NoError(t, err)
		require.NotEmpty(t, cpIdx.Tokens)
		require.NotEmpty(t, cpIdx.References)

		// Goal with lifecycleStatus -> lifecycle-status
		goalJSON := []byte(`{
			"resourceType": "Goal",
			"id": "goal-test-1",
			"lifecycleStatus": "active",
			"description": {"text": "Walk 30 minutes daily"},
			"subject": {"reference": "Patient/pat-100"},
			"target": [{"dueDate": "2025-06-01"}]
		}`)
		goalBytes := jsonToProtoBytes(t, descriptors["Goal"], goalJSON)
		goalIdx, err := extractors["Goal"]("tenant-1", "goal-test-1", goalBytes)
		require.NoError(t, err)
		var foundLifecycle bool
		for _, tok := range goalIdx.Tokens {
			if tok.SpName == "lifecycle-status" && tok.SpValue == "active" {
				foundLifecycle = true
			}
		}
		assert.True(t, foundLifecycle, "expected Goal lifecycle-status=active")
		assert.NotEmpty(t, goalIdx.Dates, "expected Goal target-date index")

		// Coverage with beneficiary -> patient + beneficiary
		covJSON := []byte(`{
			"resourceType": "Coverage",
			"id": "cov-test-1",
			"status": "active",
			"beneficiary": {"reference": "Patient/pat-100"},
			"payor": [{"reference": "Organization/org-100"}]
		}`)
		covBytes := jsonToProtoBytes(t, descriptors["Coverage"], covJSON)
		covIdx, err := extractors["Coverage"]("tenant-1", "cov-test-1", covBytes)
		require.NoError(t, err)
		var foundBeneficiary, foundPatient bool
		for _, ref := range covIdx.References {
			if ref.SpName == "beneficiary" && ref.TargetID == "pat-100" {
				foundBeneficiary = true
			}
			if ref.SpName == "patient" && ref.TargetID == "pat-100" {
				foundPatient = true
			}
		}
		assert.True(t, foundBeneficiary, "expected beneficiary reference")
		assert.True(t, foundPatient, "expected patient reference derived from beneficiary")

		// Provenance with target -> Reference
		provJSON := []byte(`{
			"resourceType": "Provenance",
			"id": "prov-test-1",
			"target": [{"reference": "Observation/obs-100"}],
			"recorded": "2025-03-10T10:35:00Z",
			"agent": [{"who": {"reference": "Practitioner/prac-100"}}]
		}`)
		provBytes := jsonToProtoBytes(t, descriptors["Provenance"], provJSON)
		provIdx, err := extractors["Provenance"]("tenant-1", "prov-test-1", provBytes)
		require.NoError(t, err)
		var foundTarget, foundAgent bool
		for _, ref := range provIdx.References {
			if ref.SpName == "target" && ref.TargetType == "Observation" && ref.TargetID == "obs-100" {
				foundTarget = true
			}
			if ref.SpName == "agent" && ref.TargetType == "Practitioner" && ref.TargetID == "prac-100" {
				foundAgent = true
			}
		}
		assert.True(t, foundTarget, "expected Provenance target reference")
		assert.True(t, foundAgent, "expected Provenance agent reference")

		// Non-US-Core R4 resource: Task with for -> patient
		taskJSON := []byte(`{
			"resourceType": "Task",
			"id": "task-test-1",
			"status": "requested",
			"intent": "order",
			"code": {"coding": [{"system": "http://hl7.org/fhir/CodeSystem/task-code", "code": "fulfill"}]},
			"for": {"reference": "Patient/pat-100"}
		}`)
		taskBytes := jsonToProtoBytes(t, descriptors["Task"], taskJSON)
		taskIdx, err := extractors["Task"]("tenant-1", "task-test-1", taskBytes)
		require.NoError(t, err)
		var foundTaskStatus, foundTaskPatient bool
		for _, tok := range taskIdx.Tokens {
			if tok.SpName == "status" && tok.SpValue == "requested" {
				foundTaskStatus = true
			}
		}
		for _, ref := range taskIdx.References {
			if ref.SpName == "patient" && ref.TargetID == "pat-100" {
				foundTaskPatient = true
			}
		}
		assert.True(t, foundTaskStatus, "expected Task status=requested")
		assert.True(t, foundTaskPatient, "expected Task patient reference from Task.for")

		// Questionnaire with canonical url -> SpidxURI
		qJSON := []byte(`{
			"resourceType": "Questionnaire",
			"id": "q-test-1",
			"status": "active",
			"url": "http://hl7.org/fhir/Questionnaire/phq-9"
		}`)
		qBytes := jsonToProtoBytes(t, descriptors["Questionnaire"], qJSON)
		qIdx, err := extractors["Questionnaire"]("tenant-1", "q-test-1", qBytes)
		require.NoError(t, err)
		var foundURI bool
		for _, u := range qIdx.URIs {
			if u.SpName == "url" && u.SpValue == "http://hl7.org/fhir/Questionnaire/phq-9" {
				foundURI = true
			}
		}
		assert.True(t, foundURI, "expected Questionnaire.url to be indexed in SpidxURI")

		// QuestionnaireResponse with canonical questionnaire -> SpidxURI + SpidxReference
		qrJSON := []byte(`{
			"resourceType": "QuestionnaireResponse",
			"id": "qr-test-1",
			"status": "completed",
			"questionnaire": "http://hl7.org/fhir/Questionnaire/phq-9"
		}`)
		qrBytes := jsonToProtoBytes(t, descriptors["QuestionnaireResponse"], qrJSON)
		qrIdx, err := extractors["QuestionnaireResponse"]("tenant-1", "qr-test-1", qrBytes)
		require.NoError(t, err)
		var foundQRURI, foundQRRef bool
		for _, u := range qrIdx.URIs {
			if u.SpName == "questionnaire" && u.SpValue == "http://hl7.org/fhir/Questionnaire/phq-9" {
				foundQRURI = true
			}
		}
		for _, ref := range qrIdx.References {
			if ref.SpName == "questionnaire" && ref.TargetType == "Questionnaire" && ref.TargetID == "phq-9" {
				foundQRRef = true
			}
		}
		assert.True(t, foundQRURI, "expected QuestionnaireResponse.questionnaire URI index")
		assert.True(t, foundQRRef, "expected QuestionnaireResponse.questionnaire Reference index")
	})

	t.Run("Token :text modifier strings from CodeableConcept.text, Coding.display, and Identifier.type", func(t *testing.T) {
		condJSON := []byte(`{
			"resourceType": "Condition",
			"id": "cond-text-1",
			"code": {
				"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}],
				"text": "Essential Hypertension"
			},
			"category": [
				{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "encounter-diagnosis", "display": "Encounter Diagnosis"}]}
			],
			"subject": {"reference": "Patient/pat-100"}
		}`)
		condBytes := jsonToProtoBytes(t, descriptors["Condition"], condJSON)
		condIdx, err := extractors["Condition"]("tenant-1", "cond-text-1", condBytes)
		require.NoError(t, err)

		var foundCodeDisplay, foundCodeText, foundCatDisplay bool
		for _, s := range condIdx.Strings {
			if s.SpName == "code:text" && s.SpValue == "hypertensive disorder" {
				foundCodeDisplay = true
			}
			if s.SpName == "code:text" && s.SpValue == "essential hypertension" {
				foundCodeText = true
			}
			if s.SpName == "category:text" && s.SpValue == "encounter diagnosis" {
				foundCatDisplay = true
			}
		}
		assert.True(t, foundCodeDisplay, "expected Condition code:text from Coding.display")
		assert.True(t, foundCodeText, "expected Condition code:text from CodeableConcept.text")
		assert.True(t, foundCatDisplay, "expected Condition category:text from Coding.display")

		patJSON := []byte(`{
			"resourceType": "Patient",
			"id": "pat-text-1",
			"identifier": [
				{
					"type": {
						"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MR", "display": "Medical Record Number"}],
						"text": "Hospital MRN"
					},
					"system": "http://hospital.example.org/mrn",
					"value": "MRN-999"
				}
			]
		}`)
		patBytes := jsonToProtoBytes(t, descriptors["Patient"], patJSON)
		patIdx, err := extractors["Patient"]("tenant-1", "pat-text-1", patBytes)
		require.NoError(t, err)
		var foundIDTypeText bool
		for _, s := range patIdx.Strings {
			if s.SpName == "identifier:text" && (s.SpValue == "medical record number" || s.SpValue == "hospital mrn") {
				foundIDTypeText = true
			}
		}
		assert.True(t, foundIDTypeText, "expected Patient identifier:text from Identifier.type")
	})
}
