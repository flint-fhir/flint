package postgres

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"

	genpostgres "github.com/flint-fhir/flint/gen/go/store/postgres"
)

// ExtractPatient extracts search indexes from Patient proto bytes.
func ExtractPatient(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r patpb.Patient
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Patient: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return genpostgres.ExtractPatientIndexes(tenantID, &r), nil
}

// ExtractCondition extracts search indexes from Condition proto bytes.
func ExtractCondition(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r condpb.Condition
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Condition: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return genpostgres.ExtractConditionIndexes(tenantID, &r), nil
}

// ExtractEncounter extracts search indexes from Encounter proto bytes.
func ExtractEncounter(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r encpb.Encounter
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Encounter: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return genpostgres.ExtractEncounterIndexes(tenantID, &r), nil
}

// ExtractObservation extracts search indexes from Observation proto bytes.
func ExtractObservation(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r obspb.Observation
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Observation: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return genpostgres.ExtractObservationIndexes(tenantID, &r), nil
}

// ExtractPractitioner extracts search indexes from Practitioner proto bytes.
func ExtractPractitioner(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r pracpb.Practitioner
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Practitioner: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return genpostgres.ExtractPractitionerIndexes(tenantID, &r), nil
}

// DefaultIndexExtractors returns the default map of resource type to IndexExtractorFunc
// for all supported Big 5 FHIR resources.
func DefaultIndexExtractors() map[string]IndexExtractorFunc {
	return map[string]IndexExtractorFunc{
		"Patient":      ExtractPatient,
		"Condition":    ExtractCondition,
		"Encounter":    ExtractEncounter,
		"Observation":  ExtractObservation,
		"Practitioner": ExtractPractitioner,
	}
}
