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
	"github.com/flint-fhir/flint/pkg/fhirutil"
)

func isEnumTokenParam(spName string) bool {
	switch spName {
	case "status", "lifecycle-status", "clinical-status", "verification-status", "gender", "address-use", "telecom-use", "intent", "criticality", "category", "type", "priority", "severity", "mode":
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

func appendMetaIndexes(idx *SearchIndexes, tenantID, resType, resID string, meta *dtpb.Meta) {
	if idx == nil || meta == nil {
		return
	}
	if src := strings.TrimSpace(meta.GetSource().GetValue()); src != "" {
		idx.URIs = append(idx.URIs, SpidxURI{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   "_source",
			SpValue:  src,
		})
		idx.Tokens = append(idx.Tokens, SpidxToken{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   "_source",
			SpValue:  src,
		})
	}
	for _, p := range meta.GetProfile() {
		prof := strings.TrimSpace(p.GetValue())
		if prof == "" {
			continue
		}
		idx.URIs = append(idx.URIs, SpidxURI{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   "_profile",
			SpValue:  prof,
		})
		idx.Tokens = append(idx.Tokens, SpidxToken{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   "_profile",
			SpValue:  prof,
		})
	}
	for _, sec := range meta.GetSecurity() {
		if sec == nil {
			continue
		}
		sys := strings.TrimSpace(sec.GetSystem().GetValue())
		code := strings.TrimSpace(sec.GetCode().GetValue())
		disp := fhirutil.NormalizeString(sec.GetDisplay().GetValue())
		if code != "" || sys != "" {
			idx.Tokens = append(idx.Tokens, SpidxToken{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   "_security",
				SpSystem: sys,
				SpValue:  code,
			})
		}
		if disp != "" {
			idx.Strings = append(idx.Strings, SpidxString{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   "_security:text",
				SpValue:  disp,
			})
		}
	}
	for _, tag := range meta.GetTag() {
		if tag == nil {
			continue
		}
		sys := strings.TrimSpace(tag.GetSystem().GetValue())
		code := strings.TrimSpace(tag.GetCode().GetValue())
		disp := fhirutil.NormalizeString(tag.GetDisplay().GetValue())
		if code != "" || sys != "" {
			idx.Tokens = append(idx.Tokens, SpidxToken{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   "_tag",
				SpSystem: sys,
				SpValue:  code,
			})
		}
		if disp != "" {
			idx.Strings = append(idx.Strings, SpidxString{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   "_tag:text",
				SpValue:  disp,
			})
		}
	}
}

func appendRefLogicalIdentifierTokens(idx *SearchIndexes, tenantID, resType, resID string, ref *dtpb.Reference, spNames ...string) {
	if idx == nil || ref == nil || ref.GetIdentifier() == nil {
		return
	}
	id := ref.GetIdentifier()
	val := strings.TrimSpace(id.GetValue().GetValue())
	if val == "" {
		return
	}
	sys := strings.TrimSpace(id.GetSystem().GetValue())
	for _, spName := range spNames {
		idx.Tokens = append(idx.Tokens, SpidxToken{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   spName + ":identifier",
			SpSystem: sys,
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractPatientIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "Patient", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "Patient", effID, r.ProtoReflect(), "", 0)
	appendRefLogicalIdentifierTokens(idx, tenantID, "Patient", effID, r.GetManagingOrganization(), "organization")
	for _, gp := range r.GetGeneralPractitioner() {
		appendRefLogicalIdentifierTokens(idx, tenantID, "Patient", effID, gp, "general-practitioner")
	}
	return idx, nil
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractConditionIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "Condition", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "Condition", effID, r.ProtoReflect(), "", 0)
	appendRefLogicalIdentifierTokens(idx, tenantID, "Condition", effID, r.GetSubject(), "patient", "subject")
	appendRefLogicalIdentifierTokens(idx, tenantID, "Condition", effID, r.GetEncounter(), "encounter")
	return idx, nil
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractEncounterIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "Encounter", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "Encounter", effID, r.ProtoReflect(), "", 0)
	appendRefLogicalIdentifierTokens(idx, tenantID, "Encounter", effID, r.GetSubject(), "patient", "subject")
	appendRefLogicalIdentifierTokens(idx, tenantID, "Encounter", effID, r.GetServiceProvider(), "service-provider")
	return idx, nil
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
	appendMetaIndexes(idx, tenantID, "Observation", effectiveID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "Observation", effectiveID, r.ProtoReflect(), "", 0)
	appendRefLogicalIdentifierTokens(idx, tenantID, "Observation", effectiveID, r.GetSubject(), "patient", "subject")
	appendRefLogicalIdentifierTokens(idx, tenantID, "Observation", effectiveID, r.GetEncounter(), "encounter")
	if q := r.GetValue().GetQuantity(); q != nil {
		appendQuantityIndexes(idx, tenantID, "Observation", effectiveID, "value-quantity", q)
		appendQuantityIndexes(idx, tenantID, "Observation", effectiveID, "combo-value-quantity", q)
	}
	if cc := r.GetValue().GetCodeableConcept(); cc != nil {
		extractCodeableConceptTokens(idx, tenantID, "Observation", effectiveID, cc.ProtoReflect(), []string{"value-concept", "combo-value-concept"})
	}
	if sv := r.GetValue().GetStringValue(); sv != nil && strings.TrimSpace(sv.GetValue()) != "" {
		idx.Strings = append(idx.Strings, SpidxString{
			TenantID: tenantID,
			ResType:  "Observation",
			ResID:    effectiveID,
			SpName:   "value-string",
			SpValue:  fhirutil.NormalizeString(sv.GetValue()),
		})
	}
	if dt := r.GetValue().GetDateTime(); dt != nil {
		if t, ok := extractTemporalTime(dt.ProtoReflect()); ok {
			idx.Dates = append(idx.Dates, SpidxDate{
				TenantID: tenantID,
				ResType:  "Observation",
				ResID:    effectiveID,
				SpName:   "value-date",
				SpLow:    t,
				SpHigh:   t,
			})
		}
	}
	if per := r.GetValue().GetPeriod(); per != nil {
		if startT, endT, ok := extractPeriodTimes(per.ProtoReflect()); ok {
			idx.Dates = append(idx.Dates, SpidxDate{
				TenantID: tenantID,
				ResType:  "Observation",
				ResID:    effectiveID,
				SpName:   "value-date",
				SpLow:    startT,
				SpHigh:   endT,
			})
		}
	}
	for _, comp := range r.GetComponent() {
		var compCodes []string
		for _, coding := range comp.GetCode().GetCoding() {
			sys := ""
			if coding.GetSystem() != nil {
				sys = coding.GetSystem().GetValue()
			}
			code := ""
			if coding.GetCode() != nil {
				code = coding.GetCode().GetValue()
			}
			if code != "" {
				idx.Tokens = append(idx.Tokens,
					SpidxToken{TenantID: tenantID, ResType: "Observation", ResID: effectiveID, SpName: "component-code", SpSystem: sys, SpValue: code},
					SpidxToken{TenantID: tenantID, ResType: "Observation", ResID: effectiveID, SpName: "combo-code", SpSystem: sys, SpValue: code},
				)
				compCodes = append(compCodes, code)
				if sys != "" {
					compCodes = append(compCodes, sys+"|"+code)
				}
			}
		}
		if q := comp.GetValue().GetQuantity(); q != nil {
			appendQuantityIndexes(idx, tenantID, "Observation", effectiveID, "component-value-quantity", q)
			appendQuantityIndexes(idx, tenantID, "Observation", effectiveID, "combo-value-quantity", q)
			for _, ck := range compCodes {
				appendQuantityIndexes(idx, tenantID, "Observation", effectiveID, "comp-cvq:"+ck, q)
			}
		}
		if cc := comp.GetValue().GetCodeableConcept(); cc != nil {
			extractCodeableConceptTokens(idx, tenantID, "Observation", effectiveID, cc.ProtoReflect(), []string{"component-value-concept", "combo-value-concept"})
			for _, coding := range cc.GetCoding() {
				vSys := ""
				if coding.GetSystem() != nil {
					vSys = coding.GetSystem().GetValue()
				}
				vCode := ""
				if coding.GetCode() != nil {
					vCode = coding.GetCode().GetValue()
				}
				if vCode != "" {
					for _, ck := range compCodes {
						idx.Tokens = append(idx.Tokens, SpidxToken{
							TenantID: tenantID,
							ResType:  "Observation",
							ResID:    effectiveID,
							SpName:   "comp-cvc:" + ck,
							SpSystem: vSys,
							SpValue:  vCode,
						})
					}
				}
			}
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractPractitionerIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "Practitioner", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "Practitioner", effID, r.ProtoReflect(), "", 0)
	return idx, nil
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractMedicationRequestIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "MedicationRequest", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "MedicationRequest", effID, r.ProtoReflect(), "", 0)
	return idx, nil
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractAllergyIntoleranceIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "AllergyIntolerance", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "AllergyIntolerance", effID, r.ProtoReflect(), "", 0)
	return idx, nil
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractProcedureIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "Procedure", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "Procedure", effID, r.ProtoReflect(), "", 0)
	return idx, nil
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractDiagnosticReportIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "DiagnosticReport", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "DiagnosticReport", effID, r.ProtoReflect(), "", 0)
	return idx, nil
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractImmunizationIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "Immunization", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "Immunization", effID, r.ProtoReflect(), "", 0)
	return idx, nil
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
	appendMetaIndexes(idx, tenantID, "Organization", effectiveID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "Organization", effectiveID, r.ProtoReflect(), "", 0)
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractSpecimenIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "Specimen", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "Specimen", effID, r.ProtoReflect(), "", 0)
	return idx, nil
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
	effID := r.GetId().GetValue()
	idx := normalizeSearchIndexes(genpostgres.ExtractLocationIndexes(tenantID, &r))
	appendMetaIndexes(idx, tenantID, "Location", effID, r.GetMeta())
	appendTokenTextFromMessage(idx, tenantID, "Location", effID, r.ProtoReflect(), "", 0)
	return idx, nil
}

// DefaultIndexExtractors returns the default map of resource type to IndexExtractorFunc
// for all ~146 official HL7 FHIR R4 resource types (combining universal protoreflect
// extractors with static generated extractors).
func DefaultIndexExtractors() map[string]IndexExtractorFunc {
	allDescs := fhirutil.AllR4ResourceDescriptors()
	extractors := make(map[string]IndexExtractorFunc, len(allDescs))
	for resType, md := range allDescs {
		extractors[resType] = NewDynamicResourceExtractor(resType, md)
	}
	extractors["Patient"] = ExtractPatient
	extractors["Condition"] = ExtractCondition
	extractors["Encounter"] = ExtractEncounter
	extractors["Observation"] = ExtractObservation
	extractors["Practitioner"] = ExtractPractitioner
	extractors["MedicationRequest"] = ExtractMedicationRequest
	extractors["AllergyIntolerance"] = ExtractAllergyIntolerance
	extractors["Procedure"] = ExtractProcedure
	extractors["DiagnosticReport"] = ExtractDiagnosticReport
	extractors["Immunization"] = ExtractImmunization
	extractors["Organization"] = ExtractOrganization
	extractors["Specimen"] = ExtractSpecimen
	extractors["Location"] = ExtractLocation
	return extractors
}
