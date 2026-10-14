package postgres

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/fhirutil"
)

// NewDynamicResourceExtractor returns an IndexExtractorFunc that uses protobuf reflection
// to extract standard FHIR R4 search parameters (tokens, strings, dates, quantities, and
// references) from any of the ~146 official HL7 FHIR R4 resource types.
func NewDynamicResourceExtractor(resType string, md protoreflect.MessageDescriptor) IndexExtractorFunc {
	return func(tenantID string, resID string, protoBytes []byte) (*SearchIndexes, error) {
		msg := dynamicpb.NewMessage(md)
		if err := proto.Unmarshal(protoBytes, msg); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", resType, err)
		}
		if resID == "" {
			if idFD := md.Fields().ByName("id"); idFD != nil && msg.Has(idFD) {
				resID = extractWrappedString(msg.Get(idFD).Message())
			}
		}

		idx := &SearchIndexes{}
		extractFromMessage(idx, tenantID, resType, resID, msg, "", 0)
		return normalizeSearchIndexes(idx), nil
	}
}

func extractFromMessage(idx *SearchIndexes, tenantID, resType, resID string, msg protoreflect.Message, parentField string, depth int) {
	if !msg.IsValid() || depth > 2 {
		return
	}

	msg.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		fieldName := string(fd.Name())
		switch fieldName {
		case "id", "meta", "implicit_rules", "language", "text", "contained", "extension", "modifier_extension":
			return true
		}

		if fd.IsList() {
			list := val.List()
			for i := 0; i < list.Len(); i++ {
				extractSingleFieldValue(idx, tenantID, resType, resID, fd, list.Get(i), fieldName, parentField, depth)
			}
			return true
		}

		extractSingleFieldValue(idx, tenantID, resType, resID, fd, val, fieldName, parentField, depth)
		return true
	})
}

func extractSingleFieldValue(
	idx *SearchIndexes,
	tenantID, resType, resID string,
	fd protoreflect.FieldDescriptor,
	val protoreflect.Value,
	fieldName, parentField string,
	depth int,
) {
	if fd.Kind() != protoreflect.MessageKind {
		return
	}
	subMsg := val.Message()
	if !subMsg.IsValid() {
		return
	}
	fullName := string(subMsg.Descriptor().FullName())

	switch fullName {
	case "google.fhir.r4.core.Identifier":
		spName := "identifier"
		if fieldName == "accession_identifier" {
			spName = "accession"
		}
		sys, v := extractIdentifier(subMsg)
		if v != "" {
			idx.Tokens = append(idx.Tokens, SpidxToken{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   spName,
				SpSystem: sys,
				SpValue:  v,
			})
		}

	case "google.fhir.r4.core.CodeableConcept":
		spNames := mapCodeableConceptSpNames(resType, fieldName, parentField)
		extractCodeableConceptTokens(idx, tenantID, resType, resID, subMsg, spNames)

	case "google.fhir.r4.core.Coding":
		spNames := mapCodeableConceptSpNames(resType, fieldName, parentField)
		extractCodingToken(idx, tenantID, resType, resID, subMsg, spNames)

	case "google.fhir.r4.core.Reference":
		spNames := mapReferenceSpNames(resType, fieldName, parentField)
		extractReferenceIndex(idx, tenantID, resType, resID, subMsg, spNames)

	case "google.fhir.r4.core.HumanName":
		extractHumanNameStrings(idx, tenantID, resType, resID, subMsg)

	case "google.fhir.r4.core.Address":
		extractAddressStrings(idx, tenantID, resType, resID, subMsg)

	case "google.fhir.r4.core.Date", "google.fhir.r4.core.DateTime", "google.fhir.r4.core.Instant":
		spNames := mapDateSpNames(resType, fieldName, parentField)
		if t, ok := extractTemporalTime(subMsg); ok {
			for _, spName := range spNames {
				idx.Dates = append(idx.Dates, SpidxDate{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   spName,
					SpLow:    t,
					SpHigh:   t,
				})
			}
		}

	case "google.fhir.r4.core.Period":
		spNames := mapDateSpNames(resType, fieldName, parentField)
		startT, endT, ok := extractPeriodTimes(subMsg)
		if ok {
			for _, spName := range spNames {
				idx.Dates = append(idx.Dates, SpidxDate{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   spName,
					SpLow:    startT,
					SpHigh:   endT,
				})
			}
		}

	case "google.fhir.r4.core.String", "google.fhir.r4.core.Markdown":
		s := strings.TrimSpace(extractWrappedString(subMsg))
		if s != "" {
			spNames := mapStringSpNames(fieldName, parentField)
			for _, spName := range spNames {
				idx.Strings = append(idx.Strings, SpidxString{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   spName,
					SpValue:  fhirutil.NormalizeString(s),
				})
			}
		}

	case "google.fhir.r4.core.Canonical", "google.fhir.r4.core.Uri":
		s := strings.TrimSpace(extractWrappedString(subMsg))
		if s != "" {
			spName := strings.ReplaceAll(fieldName, "_", "-")
			idx.Tokens = append(idx.Tokens, SpidxToken{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   spName,
				SpValue:  s,
			})
		}

	case "google.fhir.r4.core.Boolean":
		if fieldName == "active" {
			if valFD := subMsg.Descriptor().Fields().ByName("value"); valFD != nil && subMsg.Has(valFD) && valFD.Kind() == protoreflect.BoolKind {
				bVal := "false"
				if subMsg.Get(valFD).Bool() {
					bVal = "true"
				}
				idx.Tokens = append(idx.Tokens, SpidxToken{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   "active",
					SpValue:  bVal,
				})
			}
		}

	default:
		// Check if this is an enum wrapper message (has a single enum "value" field)
		if enumStr, ok := extractEnumWrapper(subMsg); ok {
			spNames := mapEnumSpNames(fieldName, parentField)
			for _, spName := range spNames {
				idx.Tokens = append(idx.Tokens, SpidxToken{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   spName,
					SpValue:  enumStr,
				})
			}
			return
		}
		// Recurse into choice [x] wrappers (ValueX, EffectiveX, PerformedX, OccurrenceX, MedicationX, TargetX) or backbone elements
		nextParent := fieldName
		if parentField != "" {
			nextParent = parentField
		}
		extractFromMessage(idx, tenantID, resType, resID, subMsg, nextParent, depth+1)
	}
}

func extractWrappedString(msg protoreflect.Message) string {
	if !msg.IsValid() {
		return ""
	}
	valFD := msg.Descriptor().Fields().ByName("value")
	if valFD == nil || !msg.Has(valFD) || valFD.Kind() != protoreflect.StringKind {
		return ""
	}
	return msg.Get(valFD).String()
}

func extractEnumWrapper(msg protoreflect.Message) (string, bool) {
	if !msg.IsValid() {
		return "", false
	}
	valFD := msg.Descriptor().Fields().ByName("value")
	if valFD == nil || valFD.Kind() != protoreflect.EnumKind || !msg.Has(valFD) {
		return "", false
	}
	enumNum := msg.Get(valFD).Enum()
	if enumNum == 0 {
		return "", false
	}
	ev := valFD.Enum().Values().ByNumber(enumNum)
	if ev == nil {
		return "", false
	}
	return strings.ToLower(strings.ReplaceAll(string(ev.Name()), "_", "-")), true
}

func extractIdentifier(msg protoreflect.Message) (string, string) {
	var sys, val string
	if sysFD := msg.Descriptor().Fields().ByName("system"); sysFD != nil && msg.Has(sysFD) {
		sys = extractWrappedString(msg.Get(sysFD).Message())
	}
	if valFD := msg.Descriptor().Fields().ByName("value"); valFD != nil && msg.Has(valFD) {
		val = extractWrappedString(msg.Get(valFD).Message())
	}
	return sys, val
}

func extractCodingToken(idx *SearchIndexes, tenantID, resType, resID string, codingMsg protoreflect.Message, spNames []string) {
	var sys, code string
	if sysFD := codingMsg.Descriptor().Fields().ByName("system"); sysFD != nil && codingMsg.Has(sysFD) {
		sys = extractWrappedString(codingMsg.Get(sysFD).Message())
	}
	if codeFD := codingMsg.Descriptor().Fields().ByName("code"); codeFD != nil && codingMsg.Has(codeFD) {
		code = extractWrappedString(codingMsg.Get(codeFD).Message())
	}
	if code == "" {
		return
	}
	for _, spName := range spNames {
		idx.Tokens = append(idx.Tokens, SpidxToken{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   spName,
			SpSystem: sys,
			SpValue:  code,
		})
	}
}

func extractCodeableConceptTokens(idx *SearchIndexes, tenantID, resType, resID string, ccMsg protoreflect.Message, spNames []string) {
	codingFD := ccMsg.Descriptor().Fields().ByName("coding")
	if codingFD != nil && ccMsg.Has(codingFD) {
		list := ccMsg.Get(codingFD).List()
		for i := 0; i < list.Len(); i++ {
			extractCodingToken(idx, tenantID, resType, resID, list.Get(i).Message(), spNames)
		}
	}
}

func extractReferenceIndex(idx *SearchIndexes, tenantID, resType, resID string, refMsg protoreflect.Message, spNames []string) {
	var targetType, targetID string
	refMsg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		name := string(fd.Name())
		if name == "uri" && fd.Kind() == protoreflect.MessageKind {
			raw := extractWrappedString(v.Message())
			if parts := strings.SplitN(raw, "/", 2); len(parts) == 2 {
				targetType = parts[0]
				targetID = parts[1]
				return false
			}
		}
		if strings.HasSuffix(name, "_id") && fd.Kind() == protoreflect.MessageKind {
			idVal := extractWrappedString(v.Message())
			if idVal != "" {
				targetType = snakeToPascal(strings.TrimSuffix(name, "_id"))
				targetID = idVal
				return false
			}
		}
		return true
	})

	if targetID == "" {
		return
	}
	for _, spName := range spNames {
		idx.References = append(idx.References, SpidxReference{
			TenantID:   tenantID,
			ResType:    resType,
			ResID:      resID,
			SpName:     spName,
			TargetType: targetType,
			TargetID:   targetID,
		})
	}
	// If any reference directly targets a Patient (e.g., Beneficiary on Coverage, Patient on RelatedPerson/Goal/CarePlan/DocumentReference),
	// also index under "patient" so Patient compartment queries and ?patient= work uniformly.
	if targetType == "Patient" {
		hasPatient := false
		for _, spName := range spNames {
			if spName == "patient" || spName == "subject" {
				hasPatient = true
				break
			}
		}
		if !hasPatient {
			idx.References = append(idx.References, SpidxReference{
				TenantID:   tenantID,
				ResType:    resType,
				ResID:      resID,
				SpName:     "patient",
				TargetType: "Patient",
				TargetID:   targetID,
			})
		}
	}
}

func extractHumanNameStrings(idx *SearchIndexes, tenantID, resType, resID string, nameMsg protoreflect.Message) {
	if famFD := nameMsg.Descriptor().Fields().ByName("family"); famFD != nil && nameMsg.Has(famFD) {
		if fam := fhirutil.NormalizeString(extractWrappedString(nameMsg.Get(famFD).Message())); fam != "" {
			idx.Strings = append(idx.Strings,
				SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: "family", SpValue: fam},
				SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: "name", SpValue: fam},
			)
		}
	}
	if givFD := nameMsg.Descriptor().Fields().ByName("given"); givFD != nil && nameMsg.Has(givFD) {
		list := nameMsg.Get(givFD).List()
		for i := 0; i < list.Len(); i++ {
			if giv := fhirutil.NormalizeString(extractWrappedString(list.Get(i).Message())); giv != "" {
				idx.Strings = append(idx.Strings,
					SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: "given", SpValue: giv},
					SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: "name", SpValue: giv},
				)
			}
		}
	}
	if txtFD := nameMsg.Descriptor().Fields().ByName("text"); txtFD != nil && nameMsg.Has(txtFD) {
		if txt := fhirutil.NormalizeString(extractWrappedString(nameMsg.Get(txtFD).Message())); txt != "" {
			idx.Strings = append(idx.Strings,
				SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: "name", SpValue: txt},
			)
		}
	}
}

func extractAddressStrings(idx *SearchIndexes, tenantID, resType, resID string, addrMsg protoreflect.Message) {
	addStr := func(spName, raw string) {
		norm := fhirutil.NormalizeString(raw)
		if norm == "" {
			return
		}
		idx.Strings = append(idx.Strings,
			SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: norm},
			SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: "address", SpValue: norm},
		)
	}
	if fd := addrMsg.Descriptor().Fields().ByName("city"); fd != nil && addrMsg.Has(fd) {
		addStr("address-city", extractWrappedString(addrMsg.Get(fd).Message()))
	}
	if fd := addrMsg.Descriptor().Fields().ByName("state"); fd != nil && addrMsg.Has(fd) {
		addStr("address-state", extractWrappedString(addrMsg.Get(fd).Message()))
	}
	if fd := addrMsg.Descriptor().Fields().ByName("postal_code"); fd != nil && addrMsg.Has(fd) {
		addStr("address-postalcode", extractWrappedString(addrMsg.Get(fd).Message()))
	}
	if fd := addrMsg.Descriptor().Fields().ByName("country"); fd != nil && addrMsg.Has(fd) {
		addStr("address-country", extractWrappedString(addrMsg.Get(fd).Message()))
	}
}

func extractTemporalTime(msg protoreflect.Message) (time.Time, bool) {
	valFD := msg.Descriptor().Fields().ByName("value_us")
	if valFD == nil || !msg.Has(valFD) || valFD.Kind() != protoreflect.Int64Kind {
		return time.Time{}, false
	}
	return time.UnixMicro(msg.Get(valFD).Int()).UTC(), true
}

func extractPeriodTimes(msg protoreflect.Message) (time.Time, time.Time, bool) {
	var startT, endT time.Time
	var hasStart, hasEnd bool
	if sFD := msg.Descriptor().Fields().ByName("start"); sFD != nil && msg.Has(sFD) {
		startT, hasStart = extractTemporalTime(msg.Get(sFD).Message())
	}
	if eFD := msg.Descriptor().Fields().ByName("end"); eFD != nil && msg.Has(eFD) {
		endT, hasEnd = extractTemporalTime(msg.Get(eFD).Message())
	}
	if !hasStart && !hasEnd {
		return time.Time{}, time.Time{}, false
	}
	if !hasStart {
		startT = endT
	}
	if !hasEnd {
		endT = startT
	}
	return startT, endT, true
}

func mapEnumSpNames(fieldName, parentField string) []string {
	base := fieldName
	if parentField != "" && (fieldName == "status" || fieldName == "code") {
		base = parentField + "_" + fieldName
	}
	switch base {
	case "status", "doc_status":
		return []string{"status"}
	case "lifecycle_status":
		return []string{"lifecycle-status", "status"}
	case "clinical_status":
		return []string{"clinical-status"}
	case "verification_status":
		return []string{"verification-status"}
	case "intent":
		return []string{"intent"}
	case "priority":
		return []string{"priority"}
	case "gender":
		return []string{"gender"}
	case "mode":
		return []string{"mode"}
	case "criticality":
		return []string{"criticality"}
	default:
		return []string{strings.ReplaceAll(fieldName, "_", "-")}
	}
}

func mapCodeableConceptSpNames(resType, fieldName, parentField string) []string {
	effective := fieldName
	if fieldName == "codeable_concept" && parentField != "" {
		effective = parentField
	}
	switch effective {
	case "code", "medication", "product":
		return []string{"code"}
	case "vaccine_code":
		return []string{"vaccine-code", "code"}
	case "type":
		return []string{"type"}
	case "category":
		return []string{"category"}
	case "clinical_status":
		return []string{"clinical-status"}
	case "verification_status":
		return []string{"verification-status"}
	case "specialty":
		return []string{"specialty"}
	case "role":
		return []string{"role"}
	case "relationship":
		return []string{"relationship"}
	case "body_site":
		return []string{"bodysite"}
	case "description":
		if resType == "Goal" {
			return []string{"description", "code"}
		}
		return []string{"code"}
	default:
		return []string{strings.ReplaceAll(effective, "_", "-")}
	}
}

func mapReferenceSpNames(resType, fieldName, parentField string) []string {
	effective := fieldName
	if fieldName == "reference" && parentField != "" {
		effective = parentField
	}
	if (fieldName == "who" || fieldName == "actor" || fieldName == "member" || fieldName == "individual") && parentField != "" {
		effective = parentField + "_" + fieldName
	}
	switch effective {
	case "patient":
		return []string{"patient"}
	case "subject":
		return []string{"subject"}
	case "beneficiary":
		return []string{"beneficiary", "patient"}
	case "encounter", "context":
		return []string{"encounter"}
	case "practitioner", "Actor", "participant_individual":
		return []string{"practitioner"}
	case "organization", "managing_organization", "service_provider", "custodian":
		return []string{"organization"}
	case "location":
		return []string{"location"}
	case "performer", "performer_actor":
		return []string{"performer"}
	case "participant_member", "participant_actor":
		return []string{"participant"}
	case "requester":
		return []string{"requester"}
	case "author":
		return []string{"author"}
	case "target":
		return []string{"target"}
	case "agent_who":
		return []string{"agent"}
	case "payor":
		return []string{"payor"}
	case "medication":
		return []string{"medication"}
	case "device":
		return []string{"device"}
	case "questionnaire":
		return []string{"questionnaire"}
	case "part_of":
		return []string{"partof"}
	case "based_on":
		return []string{"based-on"}
	case "item":
		return []string{"item"}
	case "entity":
		if resType == "Group" {
			return []string{"member", "entity"}
		}
		return []string{"entity"}
	default:
		return []string{strings.ReplaceAll(effective, "_", "-")}
	}
}

func mapDateSpNames(resType, fieldName, parentField string) []string {
	effective := fieldName
	if (fieldName == "date" || fieldName == "date_time" || fieldName == "period" || fieldName == "instant") && parentField != "" {
		effective = parentField
	}
	switch effective {
	case "birth_date":
		return []string{"birthdate"}
	case "authored", "authored_on":
		return []string{"authored", "authoredon", "date"}
	case "recorded":
		return []string{"recorded", "date"}
	case "issued", "created", "indexed":
		return []string{"date", effective}
	case "period", "effective", "performed", "occurrence", "onset":
		return []string{"date", "period"}
	case "due":
		if resType == "Goal" {
			return []string{"target-date", "date"}
		}
		return []string{"date"}
	case "when_handed_over":
		return []string{"whenhandedover", "date"}
	case "when_prepared":
		return []string{"whenprepared", "date"}
	default:
		return []string{"date", strings.ReplaceAll(effective, "_", "-")}
	}
}

func mapStringSpNames(fieldName, parentField string) []string {
	switch fieldName {
	case "name", "alias", "title", "description":
		return []string{"name", fieldName}
	case "udi_carrier", "device_identifier":
		return []string{"udi-carrier"}
	default:
		return nil
	}
}

func snakeToPascal(s string) string {
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + strings.ToLower(p[1:]))
	}
	return b.String()
}
