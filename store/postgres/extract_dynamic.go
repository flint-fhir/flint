package postgres

import (
	"fmt"
	"strconv"
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
	if depth == 0 && !strings.Contains(resID, "#") {
		extractExtensionsContainedAndCustomFromRoot(idx, tenantID, resType, resID, msg)
	}

	msg.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		fieldName := string(fd.Name())
		switch fieldName {
		case "meta":
			if depth == 0 && fd.Kind() == protoreflect.MessageKind && !fd.IsList() {
				extractMetaFromMessage(idx, tenantID, resType, resID, val.Message())
			}
			return true
		case "text":
			if depth == 0 && fd.Kind() == protoreflect.MessageKind && !fd.IsList() {
				extractNarrativeFromMessage(idx, tenantID, resType, resID, val.Message())
			}
			return true
		case "id", "implicit_rules", "language", "contained", "extension", "modifier_extension":
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

func extractMetaFromMessage(idx *SearchIndexes, tenantID, resType, resID string, metaMsg protoreflect.Message) {
	if idx == nil || !metaMsg.IsValid() {
		return
	}
	md := metaMsg.Descriptor()
	if srcFD := md.Fields().ByName("source"); srcFD != nil && metaMsg.Has(srcFD) && srcFD.Kind() == protoreflect.MessageKind {
		if src := strings.TrimSpace(extractWrappedString(metaMsg.Get(srcFD).Message())); src != "" {
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
	}
	if profFD := md.Fields().ByName("profile"); profFD != nil && metaMsg.Has(profFD) && profFD.IsList() {
		list := metaMsg.Get(profFD).List()
		for i := 0; i < list.Len(); i++ {
			if prof := strings.TrimSpace(extractWrappedString(list.Get(i).Message())); prof != "" {
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
		}
	}
	for _, fieldSpec := range []struct {
		field  string
		spName string
	}{
		{field: "security", spName: "_security"},
		{field: "tag", spName: "_tag"},
	} {
		fd := md.Fields().ByName(protoreflect.Name(fieldSpec.field))
		if fd == nil || !metaMsg.Has(fd) || !fd.IsList() {
			continue
		}
		list := metaMsg.Get(fd).List()
		for i := 0; i < list.Len(); i++ {
			cm := list.Get(i).Message()
			if !cm.IsValid() {
				continue
			}
			var sys, code, disp string
			if sysFD := cm.Descriptor().Fields().ByName("system"); sysFD != nil && cm.Has(sysFD) {
				sys = strings.TrimSpace(extractWrappedString(cm.Get(sysFD).Message()))
			}
			if codeFD := cm.Descriptor().Fields().ByName("code"); codeFD != nil && cm.Has(codeFD) {
				code = strings.TrimSpace(extractWrappedString(cm.Get(codeFD).Message()))
			}
			if dispFD := cm.Descriptor().Fields().ByName("display"); dispFD != nil && cm.Has(dispFD) {
				disp = fhirutil.NormalizeString(extractWrappedString(cm.Get(dispFD).Message()))
			}
			if code != "" || sys != "" {
				idx.Tokens = append(idx.Tokens, SpidxToken{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   fieldSpec.spName,
					SpSystem: sys,
					SpValue:  code,
				})
			}
			if disp != "" {
				idx.Strings = append(idx.Strings, SpidxString{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   fieldSpec.spName + ":text",
					SpValue:  disp,
				})
			}
		}
	}
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
		if typeFD := subMsg.Descriptor().Fields().ByName("type"); typeFD != nil && subMsg.Has(typeFD) && typeFD.Kind() == protoreflect.MessageKind {
			extractCodeableConceptTextStrings(idx, tenantID, resType, resID, subMsg.Get(typeFD).Message(), []string{spName})
		}
		extractIdentifierOfTypeTokens(idx, tenantID, resType, resID, subMsg, spName)

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

	case "google.fhir.r4.core.ContactPoint":
		extractContactPointTokens(idx, tenantID, resType, resID, subMsg)

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
			if resType == "Coverage" && fieldName == "subscriber_id" {
				idx.Tokens = append(idx.Tokens,
					SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: "subscriber-id", SpValue: s},
					SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: "identifier", SpValue: s},
				)
			}
		}

	case "google.fhir.r4.core.Canonical", "google.fhir.r4.core.Uri", "google.fhir.r4.core.Url", "google.fhir.r4.core.Oid", "google.fhir.r4.core.Uuid":
		s := strings.TrimSpace(extractWrappedString(subMsg))
		if s != "" {
			spName := strings.ReplaceAll(fieldName, "_", "-")
			if (fieldName == "uri" || fieldName == "canonical" || fieldName == "url") && parentField != "" {
				spName = strings.ReplaceAll(parentField, "_", "-")
			}
			idx.URIs = append(idx.URIs, SpidxURI{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   spName,
				SpValue:  s,
			})
			idx.Tokens = append(idx.Tokens, SpidxToken{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   spName,
				SpValue:  s,
			})
			if resType == "ConceptMap" && (spName == "source" || spName == "target") {
				aliasName := spName + "-uri"
				idx.URIs = append(idx.URIs, SpidxURI{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   aliasName,
					SpValue:  s,
				})
				idx.Tokens = append(idx.Tokens, SpidxToken{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   aliasName,
					SpValue:  s,
				})
			}
			if spName == "questionnaire" || spName == "instantiates-canonical" {
				targetType := ""
				targetID := s
				if spName == "questionnaire" {
					targetType = "Questionnaire"
				}
				if slashIdx := strings.LastIndex(s, "/"); slashIdx != -1 && slashIdx+1 < len(s) {
					tailID := s[slashIdx+1:]
					prefix := s[:slashIdx]
					if prevSlash := strings.LastIndex(prefix, "/"); prevSlash != -1 {
						targetType = prefix[prevSlash+1:]
					} else if prefix != "" && !strings.Contains(prefix, ":") {
						targetType = prefix
					}
					idx.References = append(idx.References, SpidxReference{
						TenantID:   tenantID,
						ResType:    resType,
						ResID:      resID,
						SpName:     spName,
						TargetType: targetType,
						TargetID:   tailID,
					})
				}
				idx.References = append(idx.References, SpidxReference{
					TenantID:   tenantID,
					ResType:    resType,
					ResID:      resID,
					SpName:     spName,
					TargetType: targetType,
					TargetID:   targetID,
				})
			}
		}

	case "google.fhir.r4.core.Quantity", "google.fhir.r4.core.SimpleQuantity", "google.fhir.r4.core.Age", "google.fhir.r4.core.Duration", "google.fhir.r4.core.Distance", "google.fhir.r4.core.Count", "google.fhir.r4.core.Money":
		extractQuantityFromMessage(idx, tenantID, resType, resID, subMsg, fieldName, parentField)

	case "google.fhir.r4.core.Boolean":
		boolSpName := ""
		switch {
		case fieldName == "active":
			boolSpName = "active"
		case fieldName == "deceased" || parentField == "deceased":
			boolSpName = "deceased"
		}
		if boolSpName != "" {
			if valFD := subMsg.Descriptor().Fields().ByName("value"); valFD != nil && subMsg.Has(valFD) && valFD.Kind() == protoreflect.BoolKind {
				bVal := "false"
				if subMsg.Get(valFD).Bool() {
					bVal = "true"
				}
				idx.Tokens = append(idx.Tokens, SpidxToken{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   boolSpName,
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
	var sys, code, display string
	if sysFD := codingMsg.Descriptor().Fields().ByName("system"); sysFD != nil && codingMsg.Has(sysFD) {
		sys = extractWrappedString(codingMsg.Get(sysFD).Message())
	}
	if codeFD := codingMsg.Descriptor().Fields().ByName("code"); codeFD != nil && codingMsg.Has(codeFD) {
		code = extractWrappedString(codingMsg.Get(codeFD).Message())
	}
	if dispFD := codingMsg.Descriptor().Fields().ByName("display"); dispFD != nil && codingMsg.Has(dispFD) {
		display = fhirutil.NormalizeString(extractWrappedString(codingMsg.Get(dispFD).Message()))
	}
	for _, spName := range spNames {
		if code != "" {
			idx.Tokens = append(idx.Tokens, SpidxToken{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   spName,
				SpSystem: sys,
				SpValue:  code,
			})
		}
		if display != "" {
			idx.Strings = append(idx.Strings, SpidxString{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   spName + ":text",
				SpValue:  display,
			})
		}
	}
}

func extractCodingTextStrings(idx *SearchIndexes, tenantID, resType, resID string, codingMsg protoreflect.Message, spNames []string) {
	if !codingMsg.IsValid() {
		return
	}
	if dispFD := codingMsg.Descriptor().Fields().ByName("display"); dispFD != nil && codingMsg.Has(dispFD) {
		if display := fhirutil.NormalizeString(extractWrappedString(codingMsg.Get(dispFD).Message())); display != "" {
			for _, spName := range spNames {
				idx.Strings = append(idx.Strings, SpidxString{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   spName + ":text",
					SpValue:  display,
				})
			}
		}
	}
}

func extractCodeableConceptTextStrings(idx *SearchIndexes, tenantID, resType, resID string, ccMsg protoreflect.Message, spNames []string) {
	if !ccMsg.IsValid() {
		return
	}
	if codingFD := ccMsg.Descriptor().Fields().ByName("coding"); codingFD != nil && ccMsg.Has(codingFD) {
		list := ccMsg.Get(codingFD).List()
		for i := 0; i < list.Len(); i++ {
			extractCodingTextStrings(idx, tenantID, resType, resID, list.Get(i).Message(), spNames)
		}
	}
	if txtFD := ccMsg.Descriptor().Fields().ByName("text"); txtFD != nil && ccMsg.Has(txtFD) {
		if txt := fhirutil.NormalizeString(extractWrappedString(ccMsg.Get(txtFD).Message())); txt != "" {
			for _, spName := range spNames {
				idx.Strings = append(idx.Strings, SpidxString{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   spName + ":text",
					SpValue:  txt,
				})
			}
		}
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
	if txtFD := ccMsg.Descriptor().Fields().ByName("text"); txtFD != nil && ccMsg.Has(txtFD) {
		if txt := fhirutil.NormalizeString(extractWrappedString(ccMsg.Get(txtFD).Message())); txt != "" {
			for _, spName := range spNames {
				idx.Strings = append(idx.Strings, SpidxString{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   spName + ":text",
					SpValue:  txt,
				})
			}
		}
	}
}

func extractIdentifierOfTypeTokens(idx *SearchIndexes, tenantID, resType, resID string, idMsg protoreflect.Message, spName string) {
	if idx == nil || !idMsg.IsValid() {
		return
	}
	_, val := extractIdentifier(idMsg)
	val = strings.TrimSpace(val)
	if val == "" {
		return
	}
	typeFD := idMsg.Descriptor().Fields().ByName("type")
	if typeFD == nil || !idMsg.Has(typeFD) || typeFD.Kind() != protoreflect.MessageKind {
		return
	}
	typeMsg := idMsg.Get(typeFD).Message()
	if !typeMsg.IsValid() {
		return
	}
	codingFD := typeMsg.Descriptor().Fields().ByName("coding")
	if codingFD == nil || !typeMsg.Has(codingFD) {
		return
	}
	list := typeMsg.Get(codingFD).List()
	for i := 0; i < list.Len(); i++ {
		codingMsg := list.Get(i).Message()
		if !codingMsg.IsValid() {
			continue
		}
		var typeSys, typeCode string
		if sysFD := codingMsg.Descriptor().Fields().ByName("system"); sysFD != nil && codingMsg.Has(sysFD) {
			typeSys = strings.TrimSpace(extractWrappedString(codingMsg.Get(sysFD).Message()))
		}
		if codeFD := codingMsg.Descriptor().Fields().ByName("code"); codeFD != nil && codingMsg.Has(codeFD) {
			typeCode = strings.TrimSpace(extractWrappedString(codingMsg.Get(codeFD).Message()))
		}
		if typeCode != "" {
			idx.Tokens = append(idx.Tokens, SpidxToken{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   spName + ":of-type",
				SpSystem: typeSys + "|" + typeCode,
				SpValue:  val,
			})
		}
	}
}

func appendTokenTextFromMessage(idx *SearchIndexes, tenantID, resType, resID string, msg protoreflect.Message, parentField string, depth int) {
	if idx == nil || !msg.IsValid() || depth > 2 {
		return
	}
	if depth == 0 && !strings.Contains(resID, "#") {
		extractExtensionsContainedAndCustomFromRoot(idx, tenantID, resType, resID, msg)
	}
	msg.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		fieldName := string(fd.Name())
		switch fieldName {
		case "text":
			if depth == 0 && fd.Kind() == protoreflect.MessageKind && !fd.IsList() {
				extractNarrativeFromMessage(idx, tenantID, resType, resID, val.Message())
			}
			return true
		case "id", "meta", "implicit_rules", "language", "contained", "extension", "modifier_extension":
			return true
		}
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		visitMsg := func(subMsg protoreflect.Message) {
			if !subMsg.IsValid() {
				return
			}
			switch string(subMsg.Descriptor().FullName()) {
			case "google.fhir.r4.core.Identifier":
				spName := "identifier"
				if fieldName == "accession_identifier" {
					spName = "accession"
				}
				if typeFD := subMsg.Descriptor().Fields().ByName("type"); typeFD != nil && subMsg.Has(typeFD) && typeFD.Kind() == protoreflect.MessageKind {
					extractCodeableConceptTextStrings(idx, tenantID, resType, resID, subMsg.Get(typeFD).Message(), []string{spName})
				}
				extractIdentifierOfTypeTokens(idx, tenantID, resType, resID, subMsg, spName)
			case "google.fhir.r4.core.CodeableConcept":
				spNames := mapCodeableConceptSpNames(resType, fieldName, parentField)
				extractCodeableConceptTextStrings(idx, tenantID, resType, resID, subMsg, spNames)
			case "google.fhir.r4.core.Coding":
				spNames := mapCodeableConceptSpNames(resType, fieldName, parentField)
				extractCodingTextStrings(idx, tenantID, resType, resID, subMsg, spNames)
			case "google.fhir.r4.core.ContactPoint":
				extractContactPointTokens(idx, tenantID, resType, resID, subMsg)
			default:
				nextParent := fieldName
				if parentField != "" {
					nextParent = parentField
				}
				appendTokenTextFromMessage(idx, tenantID, resType, resID, subMsg, nextParent, depth+1)
			}
		}
		if fd.IsList() {
			list := val.List()
			for i := 0; i < list.Len(); i++ {
				visitMsg(list.Get(i).Message())
			}
			return true
		}
		visitMsg(val.Message())
		return true
	})
}

func extractReferenceIndex(idx *SearchIndexes, tenantID, resType, resID string, refMsg protoreflect.Message, spNames []string) {
	if identFD := refMsg.Descriptor().Fields().ByName("identifier"); identFD != nil && refMsg.Has(identFD) && identFD.Kind() == protoreflect.MessageKind {
		sys, val := extractIdentifier(refMsg.Get(identFD).Message())
		if val != "" {
			seenLogical := make(map[string]bool)
			addLogical := func(name string) {
				key := name + ":identifier"
				if seenLogical[key] {
					return
				}
				seenLogical[key] = true
				idx.Tokens = append(idx.Tokens, SpidxToken{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   key,
					SpSystem: sys,
					SpValue:  val,
				})
			}
			for _, spName := range spNames {
				addLogical(spName)
				switch spName {
				case "subject", "beneficiary":
					addLogical("patient")
				case "patient":
					addLogical("subject")
				}
			}
		}
	}

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

func extractContactPointTokens(idx *SearchIndexes, tenantID, resType, resID string, cpMsg protoreflect.Message) {
	if idx == nil || !cpMsg.IsValid() {
		return
	}
	var sys, val, use string
	if sysFD := cpMsg.Descriptor().Fields().ByName("system"); sysFD != nil && cpMsg.Has(sysFD) && sysFD.Kind() == protoreflect.MessageKind {
		sys, _ = extractEnumWrapper(cpMsg.Get(sysFD).Message())
	}
	if valFD := cpMsg.Descriptor().Fields().ByName("value"); valFD != nil && cpMsg.Has(valFD) && valFD.Kind() == protoreflect.MessageKind {
		val = strings.TrimSpace(extractWrappedString(cpMsg.Get(valFD).Message()))
	}
	if useFD := cpMsg.Descriptor().Fields().ByName("use"); useFD != nil && cpMsg.Has(useFD) && useFD.Kind() == protoreflect.MessageKind {
		use, _ = extractEnumWrapper(cpMsg.Get(useFD).Message())
	}
	if val != "" {
		idx.Tokens = append(idx.Tokens, SpidxToken{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   "telecom",
			SpSystem: sys,
			SpValue:  val,
		})
	}
	if use != "" {
		idx.Tokens = append(idx.Tokens, SpidxToken{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   "telecom-use",
			SpValue:  use,
		})
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
		if resType == "Observation" && effective == "code" {
			if parentField == "component" {
				return []string{"component-code", "combo-code"}
			}
			return []string{"code", "combo-code"}
		}
		return []string{"code"}
	case "vaccine_code":
		return []string{"vaccine-code", "code"}
	case "class_value", "class":
		return []string{"class"}
	case "conclusion_code", "conclusion":
		return []string{"conclusion"}
	case "performer_type":
		return []string{"intended-performertype", "performer-type"}
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
	case "value":
		if resType == "Observation" {
			return []string{"value-concept", "combo-value-concept"}
		}
		return []string{"value"}
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
	case "period", "effective", "performed", "occurrence", "onset", "context":
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
	case "value":
		if resType == "Observation" {
			return []string{"value-date"}
		}
		return []string{"date", "value"}
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
	case "string_value":
		if parentField == "value" {
			return []string{"value-string"}
		}
		return nil
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

func extractNarrativeFromMessage(idx *SearchIndexes, tenantID, resType, resID string, narrativeMsg protoreflect.Message) {
	if idx == nil || !narrativeMsg.IsValid() {
		return
	}
	divFD := narrativeMsg.Descriptor().Fields().ByName("div")
	if divFD == nil || !narrativeMsg.Has(divFD) || divFD.Kind() != protoreflect.MessageKind {
		return
	}
	rawDiv := extractWrappedString(narrativeMsg.Get(divFD).Message())
	plain := fhirutil.NormalizeString(stripHTMLTags(rawDiv))
	if plain == "" {
		return
	}
	idx.Strings = append(idx.Strings, SpidxString{
		TenantID: tenantID,
		ResType:  resType,
		ResID:    resID,
		SpName:   "_text",
		SpValue:  plain,
	})
}

func stripHTMLTags(raw string) string {
	var b strings.Builder
	inTag := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '<' {
			inTag = true
			b.WriteByte(' ')
			continue
		}
		if c == '>' {
			inTag = false
			b.WriteByte(' ')
			continue
		}
		if !inTag {
			b.WriteByte(c)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func extractQuantityFromMessage(idx *SearchIndexes, tenantID, resType, resID string, qMsg protoreflect.Message, fieldName, parentField string) {
	if idx == nil || !qMsg.IsValid() {
		return
	}
	valFD := qMsg.Descriptor().Fields().ByName("value")
	if valFD == nil || !qMsg.Has(valFD) || valFD.Kind() != protoreflect.MessageKind {
		return
	}
	valStr := extractWrappedString(qMsg.Get(valFD).Message())
	numVal, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return
	}
	var sys, code, unit string
	if sysFD := qMsg.Descriptor().Fields().ByName("system"); sysFD != nil && qMsg.Has(sysFD) {
		sys = extractWrappedString(qMsg.Get(sysFD).Message())
	}
	if codeFD := qMsg.Descriptor().Fields().ByName("code"); codeFD != nil && qMsg.Has(codeFD) {
		code = extractWrappedString(qMsg.Get(codeFD).Message())
	}
	if unitFD := qMsg.Descriptor().Fields().ByName("unit"); unitFD != nil && qMsg.Has(unitFD) {
		unit = extractWrappedString(qMsg.Get(unitFD).Message())
	}
	primaryCode := code
	if primaryCode == "" {
		primaryCode = unit
	}

	var spNames []string
	switch {
	case parentField == "component":
		spNames = []string{"component-value-quantity", "combo-value-quantity"}
	case parentField == "value" || fieldName == "quantity" || fieldName == "value":
		spNames = []string{"value-quantity", "combo-value-quantity"}
	default:
		spNames = []string{"value-quantity", strings.ReplaceAll(fieldName, "_", "-")}
	}
	for _, spName := range spNames {
		idx.Quantities = append(idx.Quantities, SpidxQuantity{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   spName,
			SpSystem: sys,
			SpCode:   primaryCode,
			SpValue:  numVal,
		})
		if unit != "" && unit != primaryCode {
			idx.Quantities = append(idx.Quantities, SpidxQuantity{
				TenantID: tenantID,
				ResType:  resType,
				ResID:    resID,
				SpName:   spName,
				SpSystem: sys,
				SpCode:   unit,
				SpValue:  numVal,
			})
		}
	}
}

func extractExtensionsContainedAndCustomFromRoot(idx *SearchIndexes, tenantID, resType, resID string, msg protoreflect.Message) {
	if idx == nil || !msg.IsValid() || resID == "" {
		return
	}
	customDefs := CustomSearchParamsForType(resType)
	extractExtensionsFromMessage(idx, tenantID, resType, resID, msg, customDefs, 0)
	extractCustomSearchParamsFromMessage(idx, tenantID, resType, resID, msg, customDefs)
	extractContainedResourcesFromRoot(idx, tenantID, resType, resID, msg)
}

func extractExtensionsFromMessage(idx *SearchIndexes, tenantID, resType, resID string, msg protoreflect.Message, customDefs []CustomSearchParamDef, depth int) {
	if !msg.IsValid() || depth > 2 {
		return
	}
	msg.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		name := string(fd.Name())
		if name == "extension" || name == "modifier_extension" {
			if fd.IsList() {
				list := val.List()
				for i := 0; i < list.Len(); i++ {
					processSingleExtension(idx, tenantID, resType, resID, list.Get(i).Message(), customDefs)
				}
			} else {
				processSingleExtension(idx, tenantID, resType, resID, val.Message(), customDefs)
			}
			return true
		}
		if name == "id" || name == "meta" || name == "text" || name == "contained" {
			return true
		}
		if fd.IsList() {
			list := val.List()
			for i := 0; i < list.Len(); i++ {
				extractExtensionsFromMessage(idx, tenantID, resType, resID, list.Get(i).Message(), customDefs, depth+1)
			}
		} else {
			extractExtensionsFromMessage(idx, tenantID, resType, resID, val.Message(), customDefs, depth+1)
		}
		return true
	})
}

func processSingleExtension(idx *SearchIndexes, tenantID, resType, resID string, extMsg protoreflect.Message, customDefs []CustomSearchParamDef) {
	if !extMsg.IsValid() {
		return
	}
	extURL := extractExtensionURL(extMsg)
	if extURL == "" {
		return
	}

	var matched []struct {
		code string
		typ  string
	}
	switch strings.ToLower(extURL) {
	case "http://hl7.org/fhir/us/core/structuredefinition/us-core-race":
		matched = append(matched, struct{ code, typ string }{"race", "token"})
	case "http://hl7.org/fhir/us/core/structuredefinition/us-core-ethnicity":
		matched = append(matched, struct{ code, typ string }{"ethnicity", "token"})
	case "http://hl7.org/fhir/us/core/structuredefinition/us-core-birthsex":
		matched = append(matched, struct{ code, typ string }{"birthsex", "token"})
	}

	lowerURL := strings.ToLower(extURL)
	for _, def := range customDefs {
		if def.Code == "" || def.Expression == "" {
			continue
		}
		if strings.Contains(strings.ToLower(def.Expression), "'"+lowerURL+"'") ||
			strings.Contains(strings.ToLower(def.Expression), `"`+lowerURL+`"`) {
			already := false
			for _, m := range matched {
				if m.code == def.Code {
					already = true
					break
				}
			}
			if !already {
				matched = append(matched, struct{ code, typ string }{def.Code, def.Type})
			}
		}
	}

	for _, m := range matched {
		extractExtensionValuesIntoIndex(idx, tenantID, resType, resID, extMsg, extURL, m.code, m.typ, 0)
	}
}

func extractExtensionURL(extMsg protoreflect.Message) string {
	if !extMsg.IsValid() {
		return ""
	}
	urlFD := extMsg.Descriptor().Fields().ByName("url")
	if urlFD == nil || !extMsg.Has(urlFD) {
		return ""
	}
	if urlFD.Kind() == protoreflect.StringKind {
		return strings.TrimSpace(extMsg.Get(urlFD).String())
	}
	if urlFD.Kind() == protoreflect.MessageKind {
		return strings.TrimSpace(extractWrappedString(extMsg.Get(urlFD).Message()))
	}
	return ""
}

func extractExtensionValuesIntoIndex(idx *SearchIndexes, tenantID, resType, resID string, extMsg protoreflect.Message, rootExtURL, spName, paramType string, depth int) {
	if !extMsg.IsValid() || depth > 3 {
		return
	}
	subURL := extractExtensionURL(extMsg)
	if valFD := extMsg.Descriptor().Fields().ByName("value"); valFD != nil && extMsg.Has(valFD) && valFD.Kind() == protoreflect.MessageKind {
		valX := extMsg.Get(valFD).Message()
		if valX.IsValid() {
			valX.Range(func(cfd protoreflect.FieldDescriptor, cval protoreflect.Value) bool {
				if cfd.Kind() != protoreflect.MessageKind {
					return true
				}
				extractTypedValueIntoSpName(idx, tenantID, resType, resID, cval.Message(), string(cfd.Name()), rootExtURL, subURL, spName, paramType)
				return true
			})
		}
	}
	if extFD := extMsg.Descriptor().Fields().ByName("extension"); extFD != nil && extMsg.Has(extFD) && extFD.IsList() {
		list := extMsg.Get(extFD).List()
		for i := 0; i < list.Len(); i++ {
			extractExtensionValuesIntoIndex(idx, tenantID, resType, resID, list.Get(i).Message(), rootExtURL, spName, paramType, depth+1)
		}
	}
}

func extractTypedValueIntoSpName(idx *SearchIndexes, tenantID, resType, resID string, vMsg protoreflect.Message, fieldName, rootExtURL, subURL, spName, paramType string) {
	if !vMsg.IsValid() || spName == "" {
		return
	}
	fullName := string(vMsg.Descriptor().FullName())
	switch fullName {
	case "google.fhir.r4.core.Coding":
		extractCodingToken(idx, tenantID, resType, resID, vMsg, []string{spName})
		if paramType == "string" {
			if dispFD := vMsg.Descriptor().Fields().ByName("display"); dispFD != nil && vMsg.Has(dispFD) {
				if disp := fhirutil.NormalizeString(extractWrappedString(vMsg.Get(dispFD).Message())); disp != "" {
					idx.Strings = append(idx.Strings, SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: disp})
				}
			}
			if codeFD := vMsg.Descriptor().Fields().ByName("code"); codeFD != nil && vMsg.Has(codeFD) {
				if code := fhirutil.NormalizeString(extractWrappedString(vMsg.Get(codeFD).Message())); code != "" {
					idx.Strings = append(idx.Strings, SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: code})
				}
			}
		}
	case "google.fhir.r4.core.CodeableConcept":
		extractCodeableConceptTokens(idx, tenantID, resType, resID, vMsg, []string{spName})
		if paramType == "string" {
			if txtFD := vMsg.Descriptor().Fields().ByName("text"); txtFD != nil && vMsg.Has(txtFD) {
				if txt := fhirutil.NormalizeString(extractWrappedString(vMsg.Get(txtFD).Message())); txt != "" {
					idx.Strings = append(idx.Strings, SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: txt})
				}
			}
		}
	case "google.fhir.r4.core.Identifier":
		sys, val := extractIdentifier(vMsg)
		if val != "" {
			idx.Tokens = append(idx.Tokens, SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpSystem: sys, SpValue: val})
			if paramType == "string" {
				idx.Strings = append(idx.Strings, SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: fhirutil.NormalizeString(val)})
			}
		}
	case "google.fhir.r4.core.Reference":
		extractReferenceIndex(idx, tenantID, resType, resID, vMsg, []string{spName})
	case "google.fhir.r4.core.Date", "google.fhir.r4.core.DateTime", "google.fhir.r4.core.Instant":
		if t, ok := extractTemporalTime(vMsg); ok {
			idx.Dates = append(idx.Dates, SpidxDate{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpLow: t, SpHigh: t})
		}
	case "google.fhir.r4.core.Period":
		if startT, endT, ok := extractPeriodTimes(vMsg); ok {
			idx.Dates = append(idx.Dates, SpidxDate{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpLow: startT, SpHigh: endT})
		}
	case "google.fhir.r4.core.Quantity", "google.fhir.r4.core.SimpleQuantity", "google.fhir.r4.core.Age", "google.fhir.r4.core.Duration", "google.fhir.r4.core.Distance", "google.fhir.r4.core.Count", "google.fhir.r4.core.Money":
		extractQuantityIntoNamedSp(idx, tenantID, resType, resID, vMsg, spName)
	case "google.fhir.r4.core.Decimal":
		valStr := strings.TrimSpace(extractWrappedString(vMsg))
		if numVal, err := strconv.ParseFloat(valStr, 64); err == nil {
			idx.Quantities = append(idx.Quantities, SpidxQuantity{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: numVal})
			idx.Tokens = append(idx.Tokens, SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: valStr})
		}
	case "google.fhir.r4.core.Integer", "google.fhir.r4.core.PositiveInt", "google.fhir.r4.core.UnsignedInt":
		if valFD := vMsg.Descriptor().Fields().ByName("value"); valFD != nil && vMsg.Has(valFD) {
			var n int64
			switch valFD.Kind() {
			case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Int64Kind:
				n = vMsg.Get(valFD).Int()
			case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
				n = int64(vMsg.Get(valFD).Uint())
			}
			s := strconv.FormatInt(n, 10)
			idx.Tokens = append(idx.Tokens, SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: s})
			idx.Quantities = append(idx.Quantities, SpidxQuantity{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: float64(n)})
		}
	case "google.fhir.r4.core.Boolean":
		if valFD := vMsg.Descriptor().Fields().ByName("value"); valFD != nil && vMsg.Has(valFD) && valFD.Kind() == protoreflect.BoolKind {
			bVal := "false"
			if vMsg.Get(valFD).Bool() {
				bVal = "true"
			}
			idx.Tokens = append(idx.Tokens, SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: bVal})
		}
	case "google.fhir.r4.core.Uri", "google.fhir.r4.core.Url", "google.fhir.r4.core.Canonical", "google.fhir.r4.core.Oid", "google.fhir.r4.core.Uuid":
		u := strings.TrimSpace(extractWrappedString(vMsg))
		if u != "" {
			idx.URIs = append(idx.URIs, SpidxURI{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: u})
			idx.Tokens = append(idx.Tokens, SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: u})
			if paramType == "string" {
				idx.Strings = append(idx.Strings, SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: fhirutil.NormalizeString(u)})
			}
		}
	case "google.fhir.r4.core.String", "google.fhir.r4.core.Code", "google.fhir.r4.core.Markdown", "google.fhir.r4.core.Id":
		s := strings.TrimSpace(extractWrappedString(vMsg))
		if s == "" {
			return
		}
		norm := fhirutil.NormalizeString(s)
		switch paramType {
		case "string":
			if norm != "" {
				idx.Strings = append(idx.Strings, SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: norm})
			}
		case "uri":
			idx.URIs = append(idx.URIs, SpidxURI{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: s})
			idx.Tokens = append(idx.Tokens, SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: s})
		default: // "token"
			if subURL == "text" {
				if norm != "" {
					idx.Strings = append(idx.Strings, SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName + ":text", SpValue: norm})
				}
				return
			}
			idx.Tokens = append(idx.Tokens, SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpSystem: "", SpValue: s})
			if rootExtURL != "" {
				idx.Tokens = append(idx.Tokens, SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpSystem: rootExtURL, SpValue: s})
			}
			if norm != "" {
				idx.Strings = append(idx.Strings,
					SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: norm},
					SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName + ":text", SpValue: norm},
				)
			}
		}
	default:
		if enumStr, ok := extractEnumWrapper(vMsg); ok {
			idx.Tokens = append(idx.Tokens, SpidxToken{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: enumStr})
			if paramType == "string" {
				idx.Strings = append(idx.Strings, SpidxString{TenantID: tenantID, ResType: resType, ResID: resID, SpName: spName, SpValue: fhirutil.NormalizeString(enumStr)})
			}
		}
	}
}

func extractQuantityIntoNamedSp(idx *SearchIndexes, tenantID, resType, resID string, qMsg protoreflect.Message, spName string) {
	if idx == nil || !qMsg.IsValid() || spName == "" {
		return
	}
	valFD := qMsg.Descriptor().Fields().ByName("value")
	if valFD == nil || !qMsg.Has(valFD) || valFD.Kind() != protoreflect.MessageKind {
		return
	}
	valStr := extractWrappedString(qMsg.Get(valFD).Message())
	numVal, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return
	}
	var sys, code, unit string
	if sysFD := qMsg.Descriptor().Fields().ByName("system"); sysFD != nil && qMsg.Has(sysFD) {
		sys = extractWrappedString(qMsg.Get(sysFD).Message())
	}
	if codeFD := qMsg.Descriptor().Fields().ByName("code"); codeFD != nil && qMsg.Has(codeFD) {
		code = extractWrappedString(qMsg.Get(codeFD).Message())
	}
	if unitFD := qMsg.Descriptor().Fields().ByName("unit"); unitFD != nil && qMsg.Has(unitFD) {
		unit = extractWrappedString(qMsg.Get(unitFD).Message())
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
		SpValue:  numVal,
	})
	if unit != "" && unit != primaryCode {
		idx.Quantities = append(idx.Quantities, SpidxQuantity{
			TenantID: tenantID,
			ResType:  resType,
			ResID:    resID,
			SpName:   spName,
			SpSystem: sys,
			SpCode:   unit,
			SpValue:  numVal,
		})
	}
}

func extractCustomSearchParamsFromMessage(idx *SearchIndexes, tenantID, resType, resID string, msg protoreflect.Message, customDefs []CustomSearchParamDef) {
	if !msg.IsValid() || len(customDefs) == 0 {
		return
	}
	for _, def := range customDefs {
		if def.Code == "" || def.Expression == "" {
			continue
		}
		for branch := range strings.SplitSeq(def.Expression, "|") {
			branch = strings.TrimSpace(branch)
			if branch == "" {
				continue
			}
			// Strip outer parentheses if any
			for strings.HasPrefix(branch, "(") && strings.HasSuffix(branch, ")") {
				branch = strings.TrimSpace(branch[1 : len(branch)-1])
			}
			segments := splitFHIRPathSegments(branch)
			if len(segments) == 0 {
				continue
			}
			first := segments[0]
			if len(first) > 0 && first[0] >= 'A' && first[0] <= 'Z' {
				if !strings.EqualFold(first, resType) && first != "Resource" && first != "DomainResource" {
					continue
				}
				segments = segments[1:]
			}
			if len(segments) == 0 {
				continue
			}
			// Root-level extension.where(url=...) is already extracted by extractExtensionsFromMessage
			if (segments[0] == "extension" || segments[0] == "modifierExtension") && len(segments) <= 2 {
				continue
			}
			walkCustomPathAndExtract(idx, tenantID, resType, resID, msg, segments, def)
		}
	}
}

func splitFHIRPathSegments(expr string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	var quote byte
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			cur.WriteByte(c)
			continue
		}
		if c == '(' {
			depth++
			cur.WriteByte(c)
			continue
		}
		if c == ')' {
			if depth > 0 {
				depth--
			}
			cur.WriteByte(c)
			continue
		}
		if c == '.' && depth == 0 {
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func walkCustomPathAndExtract(idx *SearchIndexes, tenantID, resType, resID string, curMsg protoreflect.Message, segments []string, def CustomSearchParamDef) {
	if !curMsg.IsValid() {
		return
	}
	if len(segments) == 0 {
		extractTypedValueIntoSpName(idx, tenantID, resType, resID, curMsg, "", "", "", def.Code, def.Type)
		return
	}

	seg := strings.TrimSpace(segments[0])
	rest := segments[1:]

	// Support .where(url='...') predicate on extension
	if strings.HasPrefix(seg, "where(") {
		urlFilter := parseWhereURLFilter(seg)
		if urlFilter != "" && !strings.EqualFold(extractExtensionURL(curMsg), urlFilter) {
			return
		}
		walkCustomPathAndExtract(idx, tenantID, resType, resID, curMsg, rest, def)
		return
	}
	if strings.HasPrefix(seg, "extension(") && strings.HasSuffix(seg, ")") {
		urlFilter := strings.Trim(strings.TrimSpace(seg[len("extension("):len(seg)-1]), `"'`)
		for _, extFieldName := range []string{"extension", "modifier_extension"} {
			fd := curMsg.Descriptor().Fields().ByName(protoreflect.Name(extFieldName))
			if fd == nil || !curMsg.Has(fd) || !fd.IsList() {
				continue
			}
			list := curMsg.Get(fd).List()
			for i := 0; i < list.Len(); i++ {
				extMsg := list.Get(i).Message()
				if strings.EqualFold(extractExtensionURL(extMsg), urlFilter) {
					if len(rest) == 0 {
						extractExtensionValuesIntoIndex(idx, tenantID, resType, resID, extMsg, urlFilter, def.Code, def.Type, 0)
					} else {
						walkCustomPathAndExtract(idx, tenantID, resType, resID, extMsg, rest, def)
					}
				}
			}
		}
		return
	}

	// Strip FHIRPath .as(Type) or [x] suffix
	cleanSeg := strings.TrimSuffix(seg, "[x]")
	if openIdx := strings.IndexByte(cleanSeg, '('); openIdx != -1 {
		cleanSeg = strings.TrimSpace(cleanSeg[:openIdx])
	}

	md := curMsg.Descriptor()
	fd := findProtoFieldByFHIRPathSegment(md, cleanSeg)
	if fd == nil {
		// Check if curMsg is a choice[x] wrapper (has oneof "choice") and step into its active choice field
		if md.Oneofs().ByName("choice") != nil {
			curMsg.Range(func(cfd protoreflect.FieldDescriptor, cval protoreflect.Value) bool {
				if cfd.Kind() == protoreflect.MessageKind {
					walkCustomPathAndExtract(idx, tenantID, resType, resID, cval.Message(), segments, def)
				}
				return true
			})
		}
		return
	}
	if !curMsg.Has(fd) || fd.Kind() != protoreflect.MessageKind {
		return
	}

	stepInto := func(childMsg protoreflect.Message) {
		if !childMsg.IsValid() {
			return
		}
		// Unwrap choice[x] wrapper automatically when at leaf or when next segment targets inner fields
		if childMsg.Descriptor().Oneofs().ByName("choice") != nil {
			childMsg.Range(func(cfd protoreflect.FieldDescriptor, cval protoreflect.Value) bool {
				if cfd.Kind() == protoreflect.MessageKind {
					walkCustomPathAndExtract(idx, tenantID, resType, resID, cval.Message(), rest, def)
				}
				return true
			})
			return
		}
		walkCustomPathAndExtract(idx, tenantID, resType, resID, childMsg, rest, def)
	}

	if fd.IsList() {
		list := curMsg.Get(fd).List()
		for i := 0; i < list.Len(); i++ {
			stepInto(list.Get(i).Message())
		}
	} else {
		stepInto(curMsg.Get(fd).Message())
	}
}

func findProtoFieldByFHIRPathSegment(md protoreflect.MessageDescriptor, seg string) protoreflect.FieldDescriptor {
	if md == nil || seg == "" {
		return nil
	}
	fields := md.Fields()
	if fd := fields.ByJSONName(seg); fd != nil {
		return fd
	}
	if fd := fields.ByName(protoreflect.Name(seg)); fd != nil {
		return fd
	}
	snake := strings.ReplaceAll(camelToKebab(seg), "-", "_")
	if fd := fields.ByName(protoreflect.Name(snake)); fd != nil {
		return fd
	}
	if fd := fields.ByName(protoreflect.Name(snake + "_value")); fd != nil {
		return fd
	}
	// Choice[x] prefix match (e.g., "valueQuantity" or "medicationCodeableConcept" -> "value" or "medication")
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		prefix := fd.JSONName()
		if strings.HasPrefix(seg, prefix) && len(seg) > len(prefix) && seg[len(prefix)] >= 'A' && seg[len(prefix)] <= 'Z' {
			if fd.Kind() == protoreflect.MessageKind && fd.Message().Oneofs().ByName("choice") != nil {
				return fd
			}
		}
	}
	return nil
}

func parseWhereURLFilter(whereSeg string) string {
	for _, quote := range []byte{'\'', '"'} {
		first := strings.IndexByte(whereSeg, quote)
		if first != -1 {
			last := strings.LastIndexByte(whereSeg, quote)
			if last > first {
				return strings.TrimSpace(whereSeg[first+1 : last])
			}
		}
	}
	return ""
}

func extractContainedResourcesFromRoot(idx *SearchIndexes, tenantID, resType, resID string, msg protoreflect.Message) {
	if idx == nil || !msg.IsValid() || resID == "" {
		return
	}
	containedFD := msg.Descriptor().Fields().ByName("contained")
	if containedFD == nil || !msg.Has(containedFD) || !containedFD.IsList() {
		return
	}
	list := msg.Get(containedFD).List()
	if list.Len() == 0 {
		return
	}

	r4Descs := fhirutil.AllR4ResourceDescriptors()
	containedTypeByID := make(map[string]string, list.Len())
	containedIdxByID := make(map[string]*SearchIndexes, list.Len())

	for i := 0; i < list.Len(); i++ {
		anyMsg := list.Get(i).Message()
		if !anyMsg.IsValid() {
			continue
		}
		typeURLFD := anyMsg.Descriptor().Fields().ByName("type_url")
		valFD := anyMsg.Descriptor().Fields().ByName("value")
		if typeURLFD == nil || valFD == nil || !anyMsg.Has(typeURLFD) || !anyMsg.Has(valFD) {
			continue
		}
		typeURL := anyMsg.Get(typeURLFD).String()
		valBytes := anyMsg.Get(valFD).Bytes()
		if len(valBytes) == 0 {
			continue
		}
		var containedResType string
		if dotIdx := strings.LastIndexByte(typeURL, '.'); dotIdx != -1 && dotIdx+1 < len(typeURL) {
			containedResType = typeURL[dotIdx+1:]
		} else if slashIdx := strings.LastIndexByte(typeURL, '/'); slashIdx != -1 && slashIdx+1 < len(typeURL) {
			containedResType = typeURL[slashIdx+1:]
		}
		containedMD := r4Descs[containedResType]
		if containedMD == nil {
			continue
		}
		containedMsg := dynamicpb.NewMessage(containedMD)
		if err := proto.Unmarshal(valBytes, containedMsg); err != nil {
			continue
		}
		var containedID string
		if idFD := containedMD.Fields().ByName("id"); idFD != nil && containedMsg.Has(idFD) {
			containedID = strings.TrimPrefix(strings.TrimSpace(extractWrappedString(containedMsg.Get(idFD).Message())), "#")
		}
		if containedID == "" {
			containedID = fmt.Sprintf("contained-%d", i+1)
		}
		syntheticID := resType + "/" + resID + "#" + containedID
		cIdx := &SearchIndexes{}
		extractFromMessage(cIdx, tenantID, containedResType, syntheticID, containedMsg, "", 0)

		idx.Strings = append(idx.Strings, cIdx.Strings...)
		idx.Tokens = append(idx.Tokens, cIdx.Tokens...)
		idx.Dates = append(idx.Dates, cIdx.Dates...)
		idx.Quantities = append(idx.Quantities, cIdx.Quantities...)
		idx.References = append(idx.References, cIdx.References...)
		idx.URIs = append(idx.URIs, cIdx.URIs...)

		// Also index contained strings and tokens under the parent resource for _content full-text search
		for _, sRow := range cIdx.Strings {
			if sRow.SpValue != "" {
				idx.Strings = append(idx.Strings, SpidxString{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   "_content",
					SpValue:  sRow.SpValue,
				})
			}
		}
		for _, tRow := range cIdx.Tokens {
			if tRow.SpValue != "" {
				idx.Tokens = append(idx.Tokens, SpidxToken{
					TenantID: tenantID,
					ResType:  resType,
					ResID:    resID,
					SpName:   "_content",
					SpSystem: tRow.SpSystem,
					SpValue:  tRow.SpValue,
				})
			}
		}

		containedTypeByID[containedID] = containedResType
		containedIdxByID[containedID] = cIdx
	}

	if len(containedTypeByID) > 0 {
		extractFragmentReferencesFromMessage(idx, tenantID, resType, resID, msg, "", 0, containedTypeByID, containedIdxByID)
	}
}

func extractFragmentReferencesFromMessage(
	idx *SearchIndexes,
	tenantID, resType, resID string,
	msg protoreflect.Message,
	parentField string,
	depth int,
	containedTypeByID map[string]string,
	containedIdxByID map[string]*SearchIndexes,
) {
	if !msg.IsValid() || depth > 2 {
		return
	}
	msg.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		fieldName := string(fd.Name())
		switch fieldName {
		case "id", "meta", "text", "contained", "extension", "modifier_extension":
			return true
		}
		visit := func(subMsg protoreflect.Message) {
			if !subMsg.IsValid() {
				return
			}
			if string(subMsg.Descriptor().FullName()) == "google.fhir.r4.core.Reference" {
				var localID string
				if uriFD := subMsg.Descriptor().Fields().ByName("uri"); uriFD != nil && subMsg.Has(uriFD) {
					raw := strings.TrimSpace(extractWrappedString(subMsg.Get(uriFD).Message()))
					if strings.HasPrefix(raw, "#") {
						localID = strings.TrimPrefix(raw, "#")
					}
				}
				if localID == "" {
					if fragFD := subMsg.Descriptor().Fields().ByName("fragment"); fragFD != nil && subMsg.Has(fragFD) {
						localID = strings.TrimPrefix(strings.TrimSpace(extractWrappedString(subMsg.Get(fragFD).Message())), "#")
					}
				}
				if localID == "" {
					return
				}
				spNames := mapReferenceSpNames(resType, fieldName, parentField)
				targetType := containedTypeByID[localID]
				if targetType == "" && len(spNames) > 0 {
					targetType = inferChainedTargetType(spNames[0])
				}
				syntheticID := resType + "/" + resID + "#" + localID
				for _, spName := range spNames {
					idx.References = append(idx.References,
						SpidxReference{
							TenantID:   tenantID,
							ResType:    resType,
							ResID:      resID,
							SpName:     spName,
							TargetType: targetType,
							TargetID:   syntheticID,
						},
						SpidxReference{
							TenantID:   tenantID,
							ResType:    resType,
							ResID:      resID,
							SpName:     spName,
							TargetType: targetType,
							TargetID:   "#" + localID,
						},
						SpidxReference{
							TenantID:   tenantID,
							ResType:    resType,
							ResID:      resID,
							SpName:     spName,
							TargetType: targetType,
							TargetID:   localID,
						},
					)
					if spName == "medication" {
						if cIdx := containedIdxByID[localID]; cIdx != nil {
							for _, tok := range cIdx.Tokens {
								if tok.SpName == "code" {
									idx.Tokens = append(idx.Tokens, SpidxToken{
										TenantID: tenantID,
										ResType:  resType,
										ResID:    resID,
										SpName:   "code",
										SpSystem: tok.SpSystem,
										SpValue:  tok.SpValue,
									})
								}
							}
							for _, str := range cIdx.Strings {
								if str.SpName == "code:text" {
									idx.Strings = append(idx.Strings, SpidxString{
										TenantID: tenantID,
										ResType:  resType,
										ResID:    resID,
										SpName:   "code:text",
										SpValue:  str.SpValue,
									})
								}
							}
						}
					}
				}
				return
			}
			nextParent := fieldName
			if parentField != "" {
				nextParent = parentField
			}
			extractFragmentReferencesFromMessage(idx, tenantID, resType, resID, subMsg, nextParent, depth+1, containedTypeByID, containedIdxByID)
		}
		if fd.IsList() {
			list := val.List()
			for i := 0; i < list.Len(); i++ {
				visit(list.Get(i).Message())
			}
		} else {
			visit(val.Message())
		}
		return true
	})
}
