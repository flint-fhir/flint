package postgres

import (
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

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

	genpostgres "github.com/flint-fhir/flint/gen/go/store/postgres"
)

func isEnumTokenParam(spName string) bool {
	switch spName {
	case "status", "gender", "address-use", "telecom-use", "intent", "criticality", "category", "type", "priority", "severity":
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
	// Ensure clinical resources with "patient" reference also have "subject" (and vice versa)
	// so both ?patient= and ?subject= work uniformly across all US Core resources.
	hasPatient := false
	hasSubject := false
	for _, ref := range idx.References {
		switch ref.SpName {
		case "patient":
			hasPatient = true
		case "subject":
			hasSubject = true
		}
	}
	if hasPatient && !hasSubject {
		for _, ref := range idx.References {
			if ref.SpName == "patient" {
				cp := ref
				cp.SpName = "subject"
				idx.References = append(idx.References, cp)
			}
		}
	} else if hasSubject && !hasPatient {
		for _, ref := range idx.References {
			if ref.SpName == "subject" && (ref.TargetType == "Patient" || ref.TargetType == "") {
				cp := ref
				cp.SpName = "patient"
				idx.References = append(idx.References, cp)
			}
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

// ExtractMedicationRequest extracts search indexes from MedicationRequest proto bytes.
func ExtractMedicationRequest(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r medreqpb.MedicationRequest
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal MedicationRequest: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return normalizeSearchIndexes(genpostgres.ExtractMedicationRequestIndexes(tenantID, &r)), nil
}

// ExtractAllergyIntolerance extracts search indexes from AllergyIntolerance proto bytes.
func ExtractAllergyIntolerance(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r allpb.AllergyIntolerance
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal AllergyIntolerance: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return normalizeSearchIndexes(genpostgres.ExtractAllergyIntoleranceIndexes(tenantID, &r)), nil
}

// ExtractProcedure extracts search indexes from Procedure proto bytes.
func ExtractProcedure(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r procpb.Procedure
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Procedure: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return normalizeSearchIndexes(genpostgres.ExtractProcedureIndexes(tenantID, &r)), nil
}

// ExtractDiagnosticReport extracts search indexes from DiagnosticReport proto bytes.
func ExtractDiagnosticReport(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r diagpb.DiagnosticReport
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal DiagnosticReport: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return normalizeSearchIndexes(genpostgres.ExtractDiagnosticReportIndexes(tenantID, &r)), nil
}

// ExtractImmunization extracts search indexes from Immunization proto bytes.
func ExtractImmunization(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r immpb.Immunization
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Immunization: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return normalizeSearchIndexes(genpostgres.ExtractImmunizationIndexes(tenantID, &r)), nil
}

// ExtractOrganization extracts search indexes from Organization proto bytes.
func ExtractOrganization(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r orgpb.Organization
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Organization: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	effectiveID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractOrganizationIndexes(tenantID, &r))
	for _, a := range r.GetAlias() {
		if a != nil && a.GetValue() != "" {
			idx.Strings = append(idx.Strings, SpidxString{
				TenantID: tenantID,
				ResType:  "Organization",
				ResID:    effectiveID,
				SpName:   "name",
				SpValue:  strings.ToLower(a.GetValue()),
			})
		}
	}
	return idx, nil
}

// ExtractSpecimen extracts search indexes from Specimen proto bytes.
func ExtractSpecimen(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r specpb.Specimen
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Specimen: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return normalizeSearchIndexes(genpostgres.ExtractSpecimenIndexes(tenantID, &r)), nil
}

// ExtractLocation extracts search indexes from Location proto bytes.
func ExtractLocation(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
	var r locpb.Location
	if err := proto.Unmarshal(protoBytes, &r); err != nil {
		return nil, fmt.Errorf("unmarshal Location: %w", err)
	}
	if (r.GetId() == nil || r.GetId().GetValue() == "") && resID != "" {
		r.Id = &dtpb.Id{Value: resID}
	}
	return normalizeSearchIndexes(genpostgres.ExtractLocationIndexes(tenantID, &r)), nil
}

// DefaultIndexExtractors returns the default map of resource type to IndexExtractorFunc
// for all supported US Core v6.1.0 & v7.0.0 / USCDI v3 & v4 FHIR R4 resources.
func DefaultIndexExtractors() map[string]IndexExtractorFunc {
	return map[string]IndexExtractorFunc{
		"Patient":            ExtractPatient,
		"Condition":          ExtractCondition,
		"Encounter":          ExtractEncounter,
		"Observation":        ExtractObservation,
		"Practitioner":       ExtractPractitioner,
		"MedicationRequest":  ExtractMedicationRequest,
		"AllergyIntolerance": ExtractAllergyIntolerance,
		"Procedure":          ExtractProcedure,
		"DiagnosticReport":   ExtractDiagnosticReport,
		"Immunization":       ExtractImmunization,
		"Organization":       ExtractOrganization,
		"Specimen":           ExtractSpecimen,
		"Location":           ExtractLocation,
	}
}
