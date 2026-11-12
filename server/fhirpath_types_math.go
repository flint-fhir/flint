package server

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func normalizeExportedFHIRType(ft string, u any) string {
	switch ft {
	case "System.Boolean":
		return "boolean"
	case "System.Integer":
		return "integer"
	case "System.Decimal":
		return "decimal"
	case "System.String":
		return "string"
	case "System.Date":
		return "date"
	case "System.DateTime":
		return "dateTime"
	case "System.Time":
		return "time"
	case "System.Quantity":
		return "Quantity"
	}
	if ft != "" {
		return ft
	}
	return inferGoValueFHIRType(u)
}

func inferGoValueFHIRType(v any) string {
	switch val := v.(type) {
	case bool:
		return "boolean"
	case int, int64:
		return "integer"
	case float64:
		return "decimal"
	case string:
		return "string"
	case map[string]any:
		if rt, _ := val["resourceType"].(string); rt != "" {
			return rt
		}
		if _, hasVal := val["value"]; hasVal {
			if _, hasU := val["unit"]; hasU {
				return "Quantity"
			}
			if _, hasC := val["code"]; hasC {
				return "Quantity"
			}
		}
	}
	return ""
}

func cleanTypeSpecifier(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "'\"")
	if dot := strings.IndexByte(s, '.'); dot != -1 {
		ns := unwrapBacktickIdent(s[:dot])
		name := unwrapBacktickIdent(s[dot+1:])
		return ns + "." + name
	}
	return unwrapBacktickIdent(s)
}

var knownSystemTypes = map[string]bool{
	"Any": true, "Boolean": true, "String": true, "Integer": true,
	"Decimal": true, "Date": true, "DateTime": true, "Time": true, "Quantity": true,
}

var knownFHIRPrimitiveTypes = map[string]string{
	"boolean":      "Element",
	"integer":      "Element",
	"integer64":    "Element",
	"string":       "Element",
	"decimal":      "Element",
	"uri":          "Element",
	"url":          "uri",
	"canonical":    "uri",
	"base64Binary": "Element",
	"instant":      "Element",
	"date":         "Element",
	"dateTime":     "Element",
	"time":         "Element",
	"code":         "string",
	"oid":          "uri",
	"id":           "string",
	"markdown":     "string",
	"unsignedInt":  "integer",
	"positiveInt":  "integer",
	"uuid":         "uri",
	"xhtml":        "Element",
}

var knownFHIRComplexTypes = map[string]string{
	"Base": "Any", "Element": "Base", "BackboneElement": "Element",
	"Resource": "Base", "DomainResource": "Resource",
	"Quantity": "Element", "Age": "Quantity", "Distance": "Quantity",
	"Duration": "Quantity", "Count": "Quantity", "SimpleQuantity": "Quantity", "MoneyQuantity": "Quantity",
	"Address": "Element", "Annotation": "Element", "Attachment": "Element",
	"CodeableConcept": "Element", "Coding": "Element", "ContactDetail": "Element",
	"ContactPoint": "Element", "Contributor": "Element", "DataRequirement": "Element",
	"Dosage": "BackboneElement", "Expression": "Element", "Extension": "Element",
	"HumanName": "Element", "Identifier": "Element", "Meta": "Element",
	"Money": "Element", "Narrative": "Element", "ParameterDefinition": "Element",
	"Period": "Element", "Range": "Element", "Ratio": "Element",
	"Reference": "Element", "RelatedArtifact": "Element", "SampledData": "Element",
	"Signature": "Element", "Timing": "BackboneElement", "TriggerDefinition": "Element",
	"UsageContext": "Element",
}

var knownFHIRResourceTypes = map[string]bool{
	"Account": true, "ActivityDefinition": true, "AdverseEvent": true, "AllergyIntolerance": true,
	"Appointment": true, "AppointmentResponse": true, "AuditEvent": true, "Basic": true,
	"Binary": true, "BiologicallyDerivedProduct": true, "BodyStructure": true, "Bundle": true,
	"CapabilityStatement": true, "CarePlan": true, "CareTeam": true, "CatalogEntry": true,
	"ChargeItem": true, "ChargeItemDefinition": true, "Claim": true, "ClaimResponse": true,
	"ClinicalImpression": true, "CodeSystem": true, "Communication": true, "CommunicationRequest": true,
	"CompartmentDefinition": true, "Composition": true, "ConceptMap": true, "Condition": true,
	"Consent": true, "Contract": true, "Coverage": true, "CoverageEligibilityRequest": true,
	"CoverageEligibilityResponse": true, "DetectedIssue": true, "Device": true, "DeviceDefinition": true,
	"DeviceMetric": true, "DeviceRequest": true, "DeviceUseStatement": true, "DiagnosticReport": true,
	"DocumentManifest": true, "DocumentReference": true, "EffectEvidenceSynthesis": true, "Encounter": true,
	"Endpoint": true, "EnrollmentRequest": true, "EnrollmentResponse": true, "EpisodeOfCare": true,
	"EventDefinition": true, "Evidence": true, "EvidenceVariable": true, "ExampleScenario": true,
	"ExplanationOfBenefit": true, "FamilyMemberHistory": true, "Flag": true, "Goal": true,
	"GraphDefinition": true, "Group": true, "GuidanceResponse": true, "HealthcareService": true,
	"ImagingStudy": true, "Immunization": true, "ImmunizationEvaluation": true, "ImmunizationRecommendation": true,
	"ImplementationGuide": true, "InsurancePlan": true, "Invoice": true, "Library": true,
	"Linkage": true, "List": true, "Location": true, "Measure": true,
	"MeasureReport": true, "Media": true, "Medication": true, "MedicationAdministration": true,
	"MedicationDispense": true, "MedicationKnowledge": true, "MedicationRequest": true, "MedicationStatement": true,
	"MedicinalProduct": true, "MessageDefinition": true, "MessageHeader": true, "MolecularSequence": true,
	"NamingSystem": true, "NutritionOrder": true, "Observation": true, "ObservationDefinition": true,
	"OperationDefinition": true, "OperationOutcome": true, "Organization": true, "OrganizationAffiliation": true,
	"Parameters": true, "Patient": true, "PaymentNotice": true, "PaymentReconciliation": true,
	"Person": true, "PlanDefinition": true, "Practitioner": true, "PractitionerRole": true,
	"Procedure": true, "Provenance": true, "Questionnaire": true, "QuestionnaireResponse": true,
	"RelatedPerson": true, "RequestGroup": true, "ResearchDefinition": true, "ResearchElementDefinition": true,
	"ResearchStudy": true, "ResearchSubject": true, "RiskAssessment": true, "RiskEvidenceSynthesis": true,
	"Schedule": true, "SearchParameter": true, "ServiceRequest": true, "Slot": true,
	"Specimen": true, "SpecimenDefinition": true, "StructureDefinition": true, "StructureMap": true,
	"Subscription": true, "Substance": true, "SupplyDelivery": true, "SupplyRequest": true,
	"Task": true, "TerminologyCapabilities": true, "TestReport": true, "TestScript": true,
	"ValueSet": true, "VerificationResult": true, "VisionPrescription": true, "ViewDefinition": true,
}

func isKnownUnqualifiedFHIRPathTypeName(name string) bool {
	if knownSystemTypes[name] || knownFHIRResourceTypes[name] {
		return true
	}
	if _, ok := knownFHIRPrimitiveTypes[name]; ok {
		return true
	}
	if _, ok := knownFHIRComplexTypes[name]; ok {
		return true
	}
	return false
}

func isKnownFHIRPathTypeName(typeSpec string) bool {
	if dot := strings.IndexByte(typeSpec, '.'); dot != -1 {
		ns := typeSpec[:dot]
		name := typeSpec[dot+1:]
		if ns == "System" || ns == "FHIR" {
			return isKnownUnqualifiedFHIRPathTypeName(name)
		}
		return false
	}
	return isKnownUnqualifiedFHIRPathTypeName(typeSpec)
}

func fhirPathTypeInfo(v any) (string, string) {
	if tv, ok := v.(fhirTypedValue); ok {
		if strings.HasPrefix(tv.fhirType, "System.") {
			return "System", strings.TrimPrefix(tv.fhirType, "System.")
		}
		if tv.fhirType != "" {
			return "FHIR", tv.fhirType
		}
		v = tv.val
	}
	switch val := v.(type) {
	case bool:
		return "System", "Boolean"
	case int, int64:
		return "System", "Integer"
	case float64:
		return "System", "Decimal"
	case string:
		return "System", "String"
	case map[string]any:
		if rt, _ := val["resourceType"].(string); rt != "" {
			return "FHIR", rt
		}
		if _, hasFam := val["family"]; hasFam {
			return "FHIR", "HumanName"
		}
		if _, hasGiv := val["given"]; hasGiv {
			return "FHIR", "HumanName"
		}
		if _, hasCoding := val["coding"]; hasCoding {
			return "FHIR", "CodeableConcept"
		}
		if _, hasVal := val["value"]; hasVal {
			if _, hasU := val["unit"]; hasU {
				return "FHIR", "Quantity"
			}
			if _, hasC := val["code"]; hasC {
				return "FHIR", "Quantity"
			}
		}
		if _, hasStart := val["start"]; hasStart {
			return "FHIR", "Period"
		}
		if _, hasEnd := val["end"]; hasEnd {
			return "FHIR", "Period"
		}
		if _, hasRef := val["reference"]; hasRef {
			return "FHIR", "Reference"
		}
		if _, hasSys := val["system"]; hasSys {
			if _, hasCode := val["code"]; hasCode {
				return "FHIR", "Coding"
			}
			if _, hasV := val["value"]; hasV {
				return "FHIR", "Identifier"
			}
		}
		return "FHIR", "Element"
	}
	return "", ""
}

func isFHIRSubtypeOf(actual, target string) bool {
	if actual == target {
		return true
	}
	if target == "Any" || target == "Base" {
		return true
	}
	if knownFHIRResourceTypes[actual] {
		if target == "Resource" {
			return true
		}
		if target == "DomainResource" && actual != "Bundle" && actual != "Parameters" && actual != "Binary" {
			return true
		}
		return false
	}
	cur := actual
	for cur != "" {
		if p, ok := knownFHIRPrimitiveTypes[cur]; ok {
			if p == target {
				return true
			}
			cur = p
			continue
		}
		if p, ok := knownFHIRComplexTypes[cur]; ok {
			if p == target {
				return true
			}
			cur = p
			continue
		}
		break
	}
	return false
}

func fhirPathMatchesType(v any, typeSpec string) bool {
	ns, name := fhirPathTypeInfo(v)
	if ns == "" || name == "" {
		return false
	}
	if dot := strings.IndexByte(typeSpec, '.'); dot != -1 {
		reqNS := typeSpec[:dot]
		reqName := typeSpec[dot+1:]
		if reqNS != ns {
			return false
		}
		if ns == "System" {
			return name == reqName || reqName == "Any"
		}
		return isFHIRSubtypeOf(name, reqName)
	}
	if typeSpec == "Any" {
		return true
	}
	if ns == "System" {
		return name == typeSpec
	}
	return isFHIRSubtypeOf(name, typeSpec)
}

func fhirPathMatchesExactOrResourceType(v any, typeSpec string) bool {
	ns, name := fhirPathTypeInfo(v)
	if ns == "" || name == "" {
		return false
	}
	if dot := strings.IndexByte(typeSpec, '.'); dot != -1 {
		reqNS := typeSpec[:dot]
		reqName := typeSpec[dot+1:]
		if reqNS != ns {
			return false
		}
		if ns == "FHIR" && knownFHIRResourceTypes[name] && (reqName == "Resource" || reqName == "DomainResource") {
			return isFHIRSubtypeOf(name, reqName)
		}
		if ns == "FHIR" && isFHIRSubtypeOf(name, "Quantity") && reqName == "Quantity" {
			return true
		}
		return name == reqName
	}
	if ns == "System" {
		if name == typeSpec {
			return true
		}
		switch typeSpec {
		case "string":
			return name == "String"
		case "integer":
			return name == "Integer"
		case "decimal":
			return name == "Decimal"
		case "boolean":
			return name == "Boolean"
		case "date":
			return name == "Date"
		case "dateTime":
			return name == "DateTime"
		case "time":
			return name == "Time"
		}
		return false
	}
	if knownFHIRResourceTypes[name] && (typeSpec == "Resource" || typeSpec == "DomainResource") {
		return isFHIRSubtypeOf(name, typeSpec)
	}
	if isFHIRSubtypeOf(name, "Quantity") && typeSpec == "Quantity" {
		return true
	}
	return name == typeSpec
}

func inferFieldFHIRType(parent map[string]any, field string, val any) string {
	switch field {
	case "id":
		return "id"
	case "gender", "status", "use", "intent", "priority", "mode", "type", "content":
		if _, isStr := val.(string); isStr || val == nil {
			if field == "type" {
				if rt, _ := parent["resourceType"].(string); rt == "Bundle" || rt == "" {
					return "code"
				}
			} else {
				return "code"
			}
		}
	case "code":
		if _, isStr := val.(string); isStr {
			return "code"
		}
	case "birthDate":
		return "date"
	case "date":
		if s, isStr := val.(string); isStr {
			if strings.Contains(s, "T") {
				return "dateTime"
			}
			if rt, _ := parent["resourceType"].(string); rt == "Questionnaire" || rt == "ValueSet" || rt == "CodeSystem" {
				return "dateTime"
			}
			return "date"
		}
	case "issued", "lastUpdated":
		return "instant"
	case "start", "end", "created", "authoredOn", "recordedDate", "onsetDateTime", "abatementDateTime", "effectiveDateTime":
		if _, isStr := val.(string); isStr {
			return "dateTime"
		}
	case "url", "system", "fullUrl", "implicitRules":
		if _, isStr := val.(string); isStr {
			if field == "fullUrl" || field == "system" || field == "implicitRules" || field == "url" {
				if _, isExt := parent["url"]; isExt && len(parent) <= 3 {
					for k := range parent {
						if strings.HasPrefix(k, "value") {
							return "uri"
						}
					}
				}
				return "uri"
			}
		}
	case "answerValueSet", "instantiatesCanonical":
		return "canonical"
	}

	switch v := val.(type) {
	case bool:
		return "boolean"
	case string:
		return "string"
	case int, int64:
		return "integer"
	case float64:
		if field == "sequence" || field == "informationSequence" || field == "rank" || field == "total" || field == "count" || field == "offset" || field == "multipleBirthInteger" {
			return "integer"
		}
		if field == "value" {
			if _, hasUnit := parent["unit"]; hasUnit {
				return "decimal"
			}
			if _, hasCode := parent["code"]; hasCode {
				return "decimal"
			}
		}
		if v == float64(int64(v)) && field != "value" && field != "factor" && field != "score" {
			return "integer"
		}
		return "decimal"
	case map[string]any:
		if rt, _ := v["resourceType"].(string); rt != "" {
			return rt
		}
		switch field {
		case "name":
			if _, ok := v["family"]; ok {
				return "HumanName"
			}
			if _, ok := v["given"]; ok {
				return "HumanName"
			}
			if _, ok := v["use"]; ok {
				return "HumanName"
			}
		case "period":
			return "Period"
		case "identifier":
			return "Identifier"
		case "telecom":
			return "ContactPoint"
		case "coding", "answerCoding":
			return "Coding"
		}
	}
	return ""
}

func wrapFieldFHIRValue(parent map[string]any, field string, val any, primMeta map[string]any) any {
	if _, already := val.(fhirTypedValue); already {
		return val
	}
	ft := inferFieldFHIRType(parent, field, val)
	decText := ""
	if pm := primMeta; pm != nil {
		if rawDec, ok := pm["_decimalText"].(string); ok {
			decText = rawDec
		}
	}
	if ft == "integer" || ft == "positiveInt" || ft == "unsignedInt" {
		if fv, isF := val.(float64); isF && fv == float64(int(fv)) {
			val = int(fv)
		}
	}
	if ft == "" && primMeta == nil && decText == "" {
		return val
	}
	return fhirTypedValue{
		val:      val,
		fhirType: ft,
		decText:  decText,
		primMeta: primMeta,
	}
}

func choiceSuffixToFHIRType(suffix string) string {
	switch suffix {
	case "Boolean":
		return "boolean"
	case "Integer":
		return "integer"
	case "Integer64":
		return "integer64"
	case "Decimal":
		return "decimal"
	case "Base64Binary":
		return "base64Binary"
	case "Instant":
		return "instant"
	case "String":
		return "string"
	case "Uri":
		return "uri"
	case "Url":
		return "url"
	case "Canonical":
		return "canonical"
	case "Date":
		return "date"
	case "DateTime":
		return "dateTime"
	case "Time":
		return "time"
	case "Code":
		return "code"
	case "Oid":
		return "oid"
	case "Id":
		return "id"
	case "Markdown":
		return "markdown"
	case "UnsignedInt":
		return "unsignedInt"
	case "PositiveInt":
		return "positiveInt"
	case "Uuid":
		return "uuid"
	default:
		return suffix
	}
}

func wrapChoiceFieldFHIRValue(suffix string, val any, primMeta map[string]any) any {
	ft := choiceSuffixToFHIRType(suffix)
	decText := ""
	if primMeta != nil {
		if rawDec, ok := primMeta["_decimalText"].(string); ok {
			decText = rawDec
		}
	}
	if ft == "integer" || ft == "positiveInt" || ft == "unsignedInt" {
		if fv, isF := val.(float64); isF && fv == float64(int(fv)) {
			val = int(fv)
		}
	}
	if s, isS := val.(string); isS && ft == "decimal" {
		if fv, err := strconv.ParseFloat(s, 64); err == nil {
			if decText == "" {
				decText = s
			}
			val = fv
		}
	}
	return fhirTypedValue{
		val:      val,
		fhirType: ft,
		decText:  decText,
		primMeta: primMeta,
	}
}

func stepMapField(m map[string]any, field string) []any {
	var next []any
	v, exists := m[field]
	metaRaw, hasMeta := m["_"+field]

	if exists || hasMeta {
		if arr, isArr := v.([]any); isArr {
			metaArr, _ := metaRaw.([]any)
			maxLen := len(arr)
			if len(metaArr) > maxLen {
				maxLen = len(metaArr)
			}
			for k := 0; k < maxLen; k++ {
				var elem any
				if k < len(arr) {
					elem = arr[k]
				}
				var elemMeta map[string]any
				if k < len(metaArr) {
					elemMeta, _ = metaArr[k].(map[string]any)
				}
				if elem == nil {
					if elemMeta != nil {
						next = append(next, fhirTypedValue{
							val:      nil,
							fhirType: inferFieldFHIRType(m, field, nil),
							primMeta: elemMeta,
						})
					}
					continue
				}
				next = append(next, wrapFieldFHIRValue(m, field, elem, elemMeta))
			}
			return next
		}
		if exists && v != nil {
			pm, _ := metaRaw.(map[string]any)
			next = append(next, wrapFieldFHIRValue(m, field, v, pm))
			return next
		}
		if hasMeta && metaRaw != nil {
			if metaArr, isMA := metaRaw.([]any); isMA {
				for _, mr := range metaArr {
					if pm, okPM := mr.(map[string]any); okPM {
						next = append(next, fhirTypedValue{
							val:      nil,
							fhirType: inferFieldFHIRType(m, field, nil),
							primMeta: pm,
						})
					}
				}
			} else if pm, okPM := metaRaw.(map[string]any); okPM {
				next = append(next, fhirTypedValue{
					val:      nil,
					fhirType: inferFieldFHIRType(m, field, nil),
					primMeta: pm,
				})
			}
			return next
		}
	}

	if field == "type" {
		if refStr, okRef := m["reference"].(string); okRef && strings.Contains(refStr, "/") && !strings.HasPrefix(refStr, "urn:") {
			parts := strings.Split(strings.TrimSpace(refStr), "/")
			if len(parts) >= 2 && parts[len(parts)-2] != "" {
				return []any{fhirTypedValue{val: parts[len(parts)-2], fhirType: "uri"}}
			}
		}
	}

	// Polymorphic choice[x] resolution strictly on base name (e.g., "value" -> "valueQuantity", NOT "valueQuantity" -> ...)
	keys := make([]string, 0, len(m))
	for k := range m {
		if strings.HasPrefix(k, field) && len(k) > len(field) && !strings.HasPrefix(k, "_") {
			c := k[len(field)]
			if c >= 'A' && c <= 'Z' {
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		cv := m[k]
		if cv == nil {
			continue
		}
		suffix := k[len(field):]
		pm, _ := m["_"+k].(map[string]any)
		if arr, isArr := cv.([]any); isArr {
			for _, elem := range arr {
				if elem != nil {
					next = append(next, wrapChoiceFieldFHIRValue(suffix, elem, pm))
				}
			}
		} else {
			next = append(next, wrapChoiceFieldFHIRValue(suffix, cv, pm))
		}
	}
	return next
}

func fhirElementChildren(it any) []any {
	var out []any
	if tv, isTV := it.(fhirTypedValue); isTV {
		if tv.primMeta != nil {
			if idv, ok := tv.primMeta["id"]; ok && idv != nil {
				out = append(out, fhirTypedValue{val: idv, fhirType: "id"})
			}
			if exts, ok := tv.primMeta["extension"].([]any); ok {
				out = append(out, exts...)
			}
		}
		if m, okM := tv.val.(map[string]any); okM {
			return append(out, mapDirectChildren(m)...)
		}
		return out
	}
	if m, ok := it.(map[string]any); ok {
		return mapDirectChildren(m)
	}
	return nil
}

func mapDirectChildren(m map[string]any) []any {
	keys := make([]string, 0, len(m))
	seenBase := make(map[string]bool, len(m))
	for k := range m {
		if k == "resourceType" || strings.HasPrefix(k, "_") {
			continue
		}
		seenBase[k] = true
		keys = append(keys, k)
	}
	for k := range m {
		if strings.HasPrefix(k, "_") && len(k) > 1 {
			base := k[1:]
			if !seenBase[base] {
				seenBase[base] = true
				keys = append(keys, base)
			}
		}
	}
	sort.Strings(keys)
	var out []any
	for _, k := range keys {
		out = append(out, stepMapField(m, k)...)
	}
	return out
}

func fhirElementDescendants(it any) []any {
	var out []any
	children := fhirElementChildren(it)
	for _, ch := range children {
		out = append(out, ch)
		out = append(out, fhirElementDescendants(ch)...)
	}
	return out
}

func negateDecText(dt string, fv float64) string {
	dt = strings.TrimSpace(dt)
	if dt == "" {
		return ""
	}
	if fv == 0 {
		return strings.TrimPrefix(dt, "-")
	}
	if strings.HasPrefix(dt, "-") {
		return strings.TrimPrefix(dt, "-")
	}
	return "-" + strings.TrimPrefix(dt, "+")
}

func negateFHIRPathValue(v any) []any {
	if tv, isTV := v.(fhirTypedValue); isTV {
		if m, okM := tv.val.(map[string]any); okM && isFHIRSubtypeOf(strings.TrimPrefix(tv.fhirType, "System."), "Quantity") {
			fv, okF := toFloat(m["value"])
			if !okF {
				return nil
			}
			negVal := -fv
			cloned := make(map[string]any, len(m))
			for k, mv := range m {
				cloned[k] = mv
			}
			cloned["value"] = negVal
			if rawDT, okDT := m["_decText"].(string); okDT && rawDT != "" {
				cloned["_decText"] = negateDecText(rawDT, negVal)
			}
			return []any{fhirTypedValue{
				val:      cloned,
				fhirType: tv.fhirType,
			}}
		}
		switch inner := tv.val.(type) {
		case int:
			return []any{fhirTypedValue{val: -inner, fhirType: tv.fhirType, decText: negateDecText(tv.decText, float64(-inner))}}
		case int64:
			return []any{fhirTypedValue{val: -inner, fhirType: tv.fhirType, decText: negateDecText(tv.decText, float64(-inner))}}
		case float64:
			neg := -inner
			if neg == 0 {
				neg = 0
			}
			return []any{fhirTypedValue{val: neg, fhirType: tv.fhirType, decText: negateDecText(tv.decText, neg)}}
		default:
			return nil
		}
	}
	switch val := v.(type) {
	case int:
		return []any{-val}
	case int64:
		return []any{-val}
	case float64:
		if val == 0 {
			return []any{0.0}
		}
		return []any{-val}
	}
	return nil
}

func absFHIRPathValue(v any) (any, bool) {
	if tv, isTV := v.(fhirTypedValue); isTV {
		if m, okM := tv.val.(map[string]any); okM {
			if fv, okF := toFloat(m["value"]); okF {
				cloned := make(map[string]any, len(m))
				for k, mv := range m {
					cloned[k] = mv
				}
				cloned["value"] = math.Abs(fv)
				if rawDT, okDT := m["_decText"].(string); okDT && rawDT != "" {
					cloned["_decText"] = strings.TrimPrefix(rawDT, "-")
				}
				return fhirTypedValue{val: cloned, fhirType: tv.fhirType}, true
			}
		}
		switch inner := tv.val.(type) {
		case int:
			if inner < 0 {
				inner = -inner
			}
			return fhirTypedValue{val: inner, fhirType: tv.fhirType, decText: strings.TrimPrefix(tv.decText, "-")}, true
		case int64:
			if inner < 0 {
				inner = -inner
			}
			return fhirTypedValue{val: inner, fhirType: tv.fhirType, decText: strings.TrimPrefix(tv.decText, "-")}, true
		case float64:
			return fhirTypedValue{val: math.Abs(inner), fhirType: tv.fhirType, decText: strings.TrimPrefix(tv.decText, "-")}, true
		}
		return nil, false
	}
	switch val := v.(type) {
	case int:
		if val < 0 {
			return -val, true
		}
		return val, true
	case int64:
		if val < 0 {
			return -val, true
		}
		return val, true
	case float64:
		return math.Abs(val), true
	}
	return nil, false
}

// fhirPathSingletonBool implements FHIRPath R4 §6.5.1 Singleton Evaluation of Collections:
// empty -> (false, false); 1 boolean -> (b, true); 1 non-boolean -> (true, true); >1 items -> (false, false).
func fhirPathSingletonBool(vals []any) (bool, bool) {
	if len(vals) != 1 {
		return false, false
	}
	u := unwrapFHIRTyped(vals[0])
	if u == nil {
		return false, false
	}
	if b, ok := u.(bool); ok {
		return b, true
	}
	return true, true
}

func singletonBool(vals []any) (bool, bool) {
	if len(vals) != 1 {
		return false, false
	}
	b, ok := unwrapFHIRTyped(vals[0]).(bool)
	return b, ok
}

func strictWhereBool(vals []any) (bool, error) {
	if len(vals) == 0 {
		return false, nil
	}
	if len(vals) > 1 {
		return false, fmt.Errorf("ViewDefinition where expression returned %d items (expected singleton boolean)", len(vals))
	}
	u := unwrapFHIRTyped(vals[0])
	if u == nil {
		return false, nil
	}
	b, ok := u.(bool)
	if !ok {
		return false, fmt.Errorf("ViewDefinition where expression evaluated to %T (expected boolean)", u)
	}
	return b, nil
}

func toIntStrict(v any) (int, bool) {
	if tv, isTV := v.(fhirTypedValue); isTV {
		if tv.fhirType == "System.Decimal" || tv.fhirType == "decimal" {
			return 0, false
		}
		v = tv.val
	}
	switch val := v.(type) {
	case int:
		return val, true
	case int64:
		return int(val), true
	}
	return 0, false
}

func toInt(v any) (int, bool) {
	u := unwrapFHIRTyped(v)
	switch val := u.(type) {
	case int:
		return val, true
	case int64:
		return int(val), true
	case float64:
		if tv, isTV := v.(fhirTypedValue); isTV && (tv.fhirType == "System.Decimal" || tv.fhirType == "decimal") {
			return 0, false
		}
		if val == float64(int(val)) {
			return int(val), true
		}
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	u := unwrapFHIRTyped(v)
	switch val := u.(type) {
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case float64:
		return val, true
	}
	return 0, false
}

func toStringConv(v any) (string, bool) {
	if qStr, okQ := formatQuantityString(v); okQ {
		return qStr, true
	}
	if tv, isTV := v.(fhirTypedValue); isTV {
		if tv.fhirType == "System.Decimal" || tv.fhirType == "decimal" {
			if tv.decText != "" {
				return tv.decText, true
			}
		}
		if tv.fhirType == "System.Time" || tv.fhirType == "time" {
			if s, ok := tv.val.(string); ok {
				return strings.TrimPrefix(s, "T"), true
			}
		}
		v = tv.val
	}
	switch val := v.(type) {
	case string:
		return val, true
	case bool:
		if val {
			return "true", true
		}
		return "false", true
	case int:
		return strconv.Itoa(val), true
	case int64:
		return strconv.FormatInt(val, 10), true
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64), true
	}
	return "", false
}

func toIntegerConv(v any) (int, bool) {
	if tv, isTV := v.(fhirTypedValue); isTV {
		if tv.fhirType == "System.Decimal" || tv.fhirType == "decimal" {
			return 0, false
		}
		if isTemporalFHIRType(tv.fhirType) {
			return 0, false
		}
		v = tv.val
	}
	switch val := v.(type) {
	case int:
		return val, true
	case int64:
		return int(val), true
	case bool:
		if val {
			return 1, true
		}
		return 0, true
	case string:
		s := strings.TrimSpace(val)
		if s == "" || strings.ContainsAny(s, ".eE") {
			return 0, false
		}
		iv, err := strconv.ParseInt(s, 10, 32)
		if err == nil {
			return int(iv), true
		}
	}
	return 0, false
}

func toDecimalTypedConv(v any) (fhirTypedValue, bool) {
	if tv, isTV := v.(fhirTypedValue); isTV {
		if isTemporalFHIRType(tv.fhirType) {
			return fhirTypedValue{}, false
		}
		if tv.fhirType == "System.Decimal" || tv.fhirType == "decimal" {
			return tv, true
		}
		v = tv.val
	}
	switch val := v.(type) {
	case int:
		return fhirTypedValue{val: float64(val), fhirType: "System.Decimal", decText: strconv.Itoa(val)}, true
	case int64:
		return fhirTypedValue{val: float64(val), fhirType: "System.Decimal", decText: strconv.FormatInt(val, 10)}, true
	case float64:
		return fhirTypedValue{val: val, fhirType: "System.Decimal", decText: strconv.FormatFloat(val, 'f', -1, 64)}, true
	case bool:
		if val {
			return fhirTypedValue{val: 1.0, fhirType: "System.Decimal", decText: "1.0"}, true
		}
		return fhirTypedValue{val: 0.0, fhirType: "System.Decimal", decText: "0.0"}, true
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return fhirTypedValue{}, false
		}
		for i := 0; i < len(s); i++ {
			b := s[i]
			if (b >= '0' && b <= '9') || b == '.' || (i == 0 && (b == '+' || b == '-')) {
				continue
			}
			return fhirTypedValue{}, false
		}
		fv, err := strconv.ParseFloat(s, 64)
		if err == nil {
			return fhirTypedValue{val: fv, fhirType: "System.Decimal", decText: s}, true
		}
	}
	return fhirTypedValue{}, false
}

func toBooleanConv(v any) (bool, bool) {
	if tv, isTV := v.(fhirTypedValue); isTV {
		if isTemporalFHIRType(tv.fhirType) {
			return false, false
		}
		v = tv.val
	}
	switch val := v.(type) {
	case bool:
		return val, true
	case int:
		if val == 1 {
			return true, true
		}
		if val == 0 {
			return false, true
		}
	case int64:
		if val == 1 {
			return true, true
		}
		if val == 0 {
			return false, true
		}
	case float64:
		if val == 1.0 {
			return true, true
		}
		if val == 0.0 {
			return false, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "true", "t", "yes", "y", "1", "1.0":
			return true, true
		case "false", "f", "no", "n", "0", "0.0":
			return false, true
		}
	}
	return false, false
}

func unescapeJSONFragment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case '"', '\\', '/':
			b.WriteByte(s[i])
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			if i+4 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) && isHexDigit(s[i+3]) && isHexDigit(s[i+4]) {
				if cp, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil && utf8.ValidRune(rune(cp)) {
					b.WriteRune(rune(cp))
					i += 4
					continue
				}
			}
			b.WriteByte('\\')
			b.WriteByte('u')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func compareScalarForSort(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return 1
	}
	if b == nil {
		return -1
	}
	if f1, ok1 := toFloat(a); ok1 {
		if f2, ok2 := toFloat(b); ok2 {
			if f1 < f2 {
				return -1
			}
			if f1 > f2 {
				return 1
			}
			return 0
		}
	}
	s1 := fmt.Sprintf("%v", unwrapFHIRTyped(a))
	s2 := fmt.Sprintf("%v", unwrapFHIRTyped(b))
	if s1 < s2 {
		return -1
	}
	if s1 > s2 {
		return 1
	}
	return 0
}

type sortCriterion struct {
	expr string
	desc bool
}

func evalSortCollection(current []any, ctx fhirPathContext, fnName string, args []string) []any {
	sorted := make([]any, len(current))
	copy(sorted, current)

	if len(args) == 0 || (len(args) == 1 && (strings.EqualFold(strings.Trim(args[0], "'\""), "desc") || strings.EqualFold(strings.Trim(args[0], "'\""), "asc"))) {
		desc := strings.EqualFold(fnName, "sortdesc")
		if len(args) == 1 && strings.EqualFold(strings.Trim(args[0], "'\""), "desc") {
			desc = true
		}
		sort.SliceStable(sorted, func(i, j int) bool {
			cmp := compareScalarForSort(sorted[i], sorted[j])
			if desc {
				return cmp > 0
			}
			return cmp < 0
		})
		return sorted
	}

	criteria := make([]sortCriterion, 0, len(args))
	for _, rawArg := range args {
		a := strings.TrimSpace(rawArg)
		d := false
		if strings.HasPrefix(a, "-") {
			d = true
			a = strings.TrimSpace(a[1:])
		} else if strings.HasPrefix(a, "+") {
			a = strings.TrimSpace(a[1:])
		}
		criteria = append(criteria, sortCriterion{expr: a, desc: d})
	}

	sort.SliceStable(sorted, func(i, j int) bool {
		for _, c := range criteria {
			ctxI := ctx
			ctxI.thisItem = sorted[i]
			ctxI.index = i
			ctxI.total = len(sorted)
			ctxJ := ctx
			ctxJ.thisItem = sorted[j]
			ctxJ.index = j
			ctxJ.total = len(sorted)
			vI := evalFHIRPathExpr([]any{sorted[i]}, ctxI, c.expr)
			vJ := evalFHIRPathExpr([]any{sorted[j]}, ctxJ, c.expr)
			var kI, kJ any
			if len(vI) > 0 {
				kI = vI[0]
			}
			if len(vJ) > 0 {
				kJ = vJ[0]
			}
			cmp := compareScalarForSort(kI, kJ)
			if cmp != 0 {
				if c.desc {
					return cmp > 0
				}
				return cmp < 0
			}
		}
		return false
	})
	return sorted
}
