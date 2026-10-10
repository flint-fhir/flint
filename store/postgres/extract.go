package postgres

import (
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"

	genpostgres "github.com/flint-fhir/flint/gen/go/store/postgres"
)

func isEnumTokenParam(spName string) bool {
	switch spName {
	case "status", "gender", "address-use", "telecom-use":
		return true
	default:
		return false
	}
}

func normalizeSearchIndexes(idx *SearchIndexes) *SearchIndexes {
	if idx == nil {
		return nil
	}
	for i := range idx.Tokens {
		if idx.Tokens[i].SpSystem == "" && isEnumTokenParam(idx.Tokens[i].SpName) {
			idx.Tokens[i].SpValue = strings.ToLower(strings.ReplaceAll(idx.Tokens[i].SpValue, "_", "-"))
		}
	}
	return idx
}

func appendQuantityIndexes(idx *SearchIndexes, tenantID, resType, resID, spName string, q *dtpb.Quantity) {
	if idx == nil || q == nil || q.GetValue() == nil || q.GetValue().GetValue() == "" {
		return
	}
	val, err := strconv.ParseFloat(q.GetValue().GetValue(), 64)
	if err != nil {
		return
	}
	sys := ""
	if q.GetSystem() != nil {
		sys = q.GetSystem().GetValue()
	}
	code := ""
	if q.GetCode() != nil {
		code = q.GetCode().GetValue()
	}
	unit := ""
	if q.GetUnit() != nil {
		unit = q.GetUnit().GetValue()
	}
	primaryCode := code
	if primaryCode == "" {
		primaryCode = unit
	}
	idx.Quantities = append(idx.Quantities, SpidxQuantity{
		TenantID: tenantID,
		ResType:  resType,
		ResID:    resID,
		SpName:   spName,
		SpSystem: sys,
		SpCode:   primaryCode,
		SpValue:  val,
	})
	if unit != "" && unit != primaryCode {
		idx.Quantities = append(idx.Quantities, SpidxQuantity{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   spName,
			SpSystem: sys,
			SpCode:   unit,
			SpValue:  val,
		})
	}
}

// ExtractPatient extracts search indexes from Patient proto bytes.
func ExtractPatient(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r patpb.Patient
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Patient: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return normalizeSearchIndexes(genpostgres.ExtractPatientIndexes(tenantID, &r)), nil
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
	return normalizeSearchIndexes(genpostgres.ExtractConditionIndexes(tenantID, &r)), nil
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
	return normalizeSearchIndexes(genpostgres.ExtractEncounterIndexes(tenantID, &r)), nil
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
	effectiveID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractObservationIndexes(tenantID, &r))
	if q := r.GetValue().GetQuantity(); q != nil {
		appendQuantityIndexes(idx, tenantID, "Observation", effectiveID, "value-quantity", q)
		appendQuantityIndexes(idx, tenantID, "Observation", effectiveID, "combo-value-quantity", q)
	}
	for _, comp := range r.GetComponent() {
		if q := comp.GetValue().GetQuantity(); q != nil {
			appendQuantityIndexes(idx, tenantID, "Observation", effectiveID, "component-value-quantity", q)
			appendQuantityIndexes(idx, tenantID, "Observation", effectiveID, "combo-value-quantity", q)
		}
	}
	return idx, nil
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
	return normalizeSearchIndexes(genpostgres.ExtractPractitionerIndexes(tenantID, &r)), nil
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
