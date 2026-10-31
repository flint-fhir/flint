package validation

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// Validator validates FHIR resources against StructureDefinitions and ValueSets.
type Validator interface {
	ValidateJSON(expectedResType string, rawJSON []byte) *Outcome
	ValidateResource(expectedResType string, resMap map[string]any) *Outcome
}

// ProfileValidator extends Validator with US Core / StructureDefinition profile and mode validation.
type ProfileValidator interface {
	Validator
	ValidateJSONWithProfiles(expectedResType string, rawJSON []byte, profiles []string, mode string) *Outcome
	ValidateResourceWithProfiles(expectedResType string, resMap map[string]any, profiles []string, mode string) *Outcome
}

// Options configures validation strictness.
type Options struct {
	DisallowUnknownFields   bool // If true, undeclared elements cause validation errors
	EnforceRequiredElements bool // If true, 1..1 minimum cardinality fields are mandatory
	EnforceValueSets        bool // If true, required ValueSet code bindings are enforced
	EnforcePrimitiveFormats bool // If true, id, date, dateTime, and instant formats are verified
}

// DefaultOptions returns standard production-grade validation options.
func DefaultOptions() Options {
	return Options{
		DisallowUnknownFields:   true,
		EnforceRequiredElements: true,
		EnforceValueSets:        true,
		EnforcePrimitiveFormats: true,
	}
}

// Engine implements the Validator interface.
type Engine struct {
	schemas        map[string]ResourceSchema
	opts           Options
	customMu       sync.RWMutex
	customProfiles map[string]map[string]any
	fhirPathEval   func(root map[string]any, expr string) []any
}

// NewEngine creates a new validation engine with default Big 5 schemas.
func NewEngine(opts Options) *Engine {
	return &Engine{
		schemas:        DefaultSchemas(),
		opts:           opts,
		customProfiles: make(map[string]map[string]any),
	}
}

// SetFHIRPathEvaluator configures an optional HL7 FHIRPath R4 evaluator used to
// evaluate StructureDefinition constraint.expression invariants during validation.
func (e *Engine) SetFHIRPathEvaluator(fn func(root map[string]any, expr string) []any) {
	e.customMu.Lock()
	defer e.customMu.Unlock()
	e.fhirPathEval = fn
}

// RegisterStructureDefinition registers a custom HL7 FHIR R4 StructureDefinition
// resource map (by canonical url and/or id) for runtime profile validation.
func (e *Engine) RegisterStructureDefinition(sdMap map[string]any) {
	if e == nil || sdMap == nil {
		return
	}
	e.customMu.Lock()
	defer e.customMu.Unlock()
	if e.customProfiles == nil {
		e.customProfiles = make(map[string]map[string]any)
	}
	if u := strings.TrimSpace(extractStringOrWrapped(sdMap["url"])); u != "" {
		e.customProfiles[u] = sdMap
	}
	if id := strings.TrimSpace(extractStringOrWrapped(sdMap["id"])); id != "" {
		e.customProfiles["StructureDefinition/"+id] = sdMap
		e.customProfiles[id] = sdMap
	}
}

func (e *Engine) lookupCustomStructureDefinition(profile string) (map[string]any, bool) {
	e.customMu.RLock()
	defer e.customMu.RUnlock()
	if e.customProfiles == nil {
		return nil, false
	}
	sd, ok := e.customProfiles[strings.TrimSpace(profile)]
	return sd, ok
}

// RegisterSchema registers or overrides a ResourceSchema in the engine.
func (e *Engine) RegisterSchema(schema ResourceSchema) {
	e.schemas[schema.ResourceType] = schema
}

// ValidateJSON validates a raw JSON byte slice for a given FHIR resource type.
func (e *Engine) ValidateJSON(expectedResType string, rawJSON []byte) *Outcome {
	outcome := NewOutcome()

	if len(rawJSON) == 0 {
		outcome.AddError(CodeStructure, "Payload body is empty")
		return outcome
	}

	var resMap map[string]any
	if err := json.Unmarshal(rawJSON, &resMap); err != nil {
		outcome.AddError(CodeStructure, fmt.Sprintf("Malformed FHIR JSON: %v", err))
		return outcome
	}

	return e.ValidateResource(expectedResType, resMap)
}

// ValidateResource validates an unmarshaled JSON map.
func (e *Engine) ValidateResource(expectedResType string, resMap map[string]any) *Outcome {
	outcome := NewOutcome()

	// 1. Verify resourceType
	rawType, ok := resMap["resourceType"]
	if !ok || rawType == nil {
		outcome.AddError(CodeRequired, "Missing 'resourceType' in resource object", expectedResType)
		return outcome
	}

	resTypeStr, ok := rawType.(string)
	if !ok || resTypeStr == "" {
		outcome.AddError(CodeStructure, "'resourceType' must be a non-empty string", expectedResType)
		return outcome
	}

	if expectedResType != "" && resTypeStr != expectedResType {
		outcome.AddError(CodeInvariant,
			fmt.Sprintf("Resource type %q does not match expected endpoint type %q", resTypeStr, expectedResType),
			resTypeStr+".resourceType")
		return outcome
	}

	// 2. Lookup Schema
	schema, ok := e.schemas[resTypeStr]
	if !ok {
		// If unknown resource type
		outcome.AddWarning(CodeNotSupported,
			fmt.Sprintf("Resource type %q is not registered for full StructureDefinition validation", resTypeStr),
			resTypeStr)
		return outcome
	}

	// 3. Disallow unknown fields
	if e.opts.DisallowUnknownFields {
		for key := range resMap {
			cleanKey := strings.TrimPrefix(key, "_")
			if !schema.AllowedFields[cleanKey] {
				outcome.AddError(CodeStructure,
					fmt.Sprintf("Unrecognized element %q for resource %s", key, resTypeStr),
					resTypeStr+"."+key)
			}
		}
	}

	// 4. Required elements check (1..1 min cardinality)
	if e.opts.EnforceRequiredElements {
		for _, reqField := range schema.RequiredFields {
			val, present := resMap[reqField]
			if !present || val == nil || isEmptyValue(val) {
				switch reqField {
				case "class":
					val, present = resMap["classValue"]
				case "medication":
					if v, ok := resMap["medicationCodeableConcept"]; ok && v != nil && !isEmptyValue(v) {
						val, present = v, true
					} else if v, ok := resMap["medicationReference"]; ok && v != nil && !isEmptyValue(v) {
						val, present = v, true
					}
				case "occurrence":
					if v, ok := resMap["occurrenceDateTime"]; ok && v != nil && !isEmptyValue(v) {
						val, present = v, true
					} else if v, ok := resMap["occurrenceString"]; ok && v != nil && !isEmptyValue(v) {
						val, present = v, true
					}
				}
			}
			if !present || val == nil || isEmptyValue(val) {
				outcome.AddError(CodeRequired,
					fmt.Sprintf("Missing required element %q on %s", reqField, resTypeStr),
					resTypeStr+"."+reqField)
			}
		}
	}

	// 5. Primitive type format checks
	if e.opts.EnforcePrimitiveFormats {
		e.validatePrimitiveFormats(resTypeStr, schema, resMap, outcome)
	}

	// 6. ValueSet code bindings check
	if e.opts.EnforceValueSets {
		e.validateCodeBindings(resTypeStr, schema, resMap, outcome)
	}

	// 7. Recursive BackboneElement, complex datatype, and embedded ContainedResource validation
	e.validateNestedChildren(resTypeStr, schema.ChildDescriptors, resMap, outcome, 0)

	// 8. Core HL7 FHIR R4 cross-element invariants (obs-6, obs-7, pat-1, dom-3, bdl-3)
	e.validateR4CoreInvariants(resTypeStr, resMap, outcome)

	// 9. Enforce any custom StructureDefinition profiles declared in meta.profile
	if metaMap, ok := resMap["meta"].(map[string]any); ok {
		if profArr, ok := metaMap["profile"].([]any); ok {
			for _, item := range profArr {
				rawProf := strings.TrimSpace(extractStringOrWrapped(item))
				if rawProf == "" {
					continue
				}
				baseProf := rawProf
				if pipeIdx := strings.IndexByte(rawProf, '|'); pipeIdx != -1 {
					baseProf = strings.TrimSpace(rawProf[:pipeIdx])
				}
				if sdMap, ok := e.lookupCustomStructureDefinition(baseProf); ok {
					e.validateAgainstCustomStructureDefinition(rawProf, sdMap, resTypeStr, resMap, outcome)
				}
			}
		}
	}

	return outcome
}

func (e *Engine) validatePrimitiveFormats(resType string, schema ResourceSchema, resMap map[string]any, outcome *Outcome) {
	// id format
	if idVal, ok := resMap["id"]; ok && idVal != nil {
		idStr := extractStringOrWrapped(idVal)
		if idStr != "" && !IDRegex.MatchString(idStr) {
			outcome.AddError(CodeValue,
				fmt.Sprintf("Invalid resource id format %q (must match regex %s)", idStr, IDRegex.String()),
				resType+".id")
		}
	}

	checkedDate := make(map[string]bool)
	for dField := range schema.DateFields {
		checkedDate[dField] = true
		if dVal, ok := resMap[dField]; ok && dVal != nil {
			dStr := extractStringOrWrapped(dVal)
			if dStr != "" && !DateRegex.MatchString(dStr) {
				outcome.AddError(CodeValue,
					fmt.Sprintf("Invalid %s format %q (must be YYYY, YYYY-MM, or YYYY-MM-DD)", dField, dStr),
					resType+"."+dField)
			}
		}
	}
	if !checkedDate["birthDate"] {
		if bVal, ok := resMap["birthDate"]; ok && bVal != nil {
			bStr := extractStringOrWrapped(bVal)
			if bStr != "" && !DateRegex.MatchString(bStr) {
				outcome.AddError(CodeValue,
					fmt.Sprintf("Invalid birthDate format %q (must be YYYY, YYYY-MM, or YYYY-MM-DD)", bStr),
					resType+".birthDate")
			}
		}
	}

	checkedDateTime := make(map[string]bool)
	for dtField := range schema.DateTimeFields {
		checkedDateTime[dtField] = true
		if dtVal, ok := resMap[dtField]; ok && dtVal != nil {
			dtStr := extractStringOrWrapped(dtVal)
			if dtStr != "" && !DateTimeRegex.MatchString(dtStr) {
				outcome.AddError(CodeValue,
					fmt.Sprintf("Invalid %s format %q", dtField, dtStr),
					resType+"."+dtField)
			}
		}
	}
	for _, dtField := range []string{"effectiveDateTime", "authoredOn", "recordedDate", "occurrenceDateTime", "performedDateTime", "receivedTime"} {
		if checkedDateTime[dtField] {
			continue
		}
		if dtVal, ok := resMap[dtField]; ok && dtVal != nil {
			dtStr := extractStringOrWrapped(dtVal)
			if dtStr != "" && !DateTimeRegex.MatchString(dtStr) {
				outcome.AddError(CodeValue,
					fmt.Sprintf("Invalid %s format %q", dtField, dtStr),
					resType+"."+dtField)
			}
		}
	}

	checkedInstant := make(map[string]bool)
	for instField := range schema.InstantFields {
		checkedInstant[instField] = true
		if instVal, ok := resMap[instField]; ok && instVal != nil {
			instStr := extractStringOrWrapped(instVal)
			if instStr != "" && !InstantRegex.MatchString(instStr) {
				outcome.AddError(CodeValue,
					fmt.Sprintf("Invalid %s instant format %q (requires full timestamp with timezone)", instField, instStr),
					resType+"."+instField)
			}
		}
	}
	if !checkedInstant["issued"] {
		if instVal, ok := resMap["issued"]; ok && instVal != nil {
			instStr := extractStringOrWrapped(instVal)
			if instStr != "" && !InstantRegex.MatchString(instStr) {
				outcome.AddError(CodeValue,
					fmt.Sprintf("Invalid issued instant format %q (requires full timestamp with timezone)", instStr),
					resType+".issued")
			}
		}
	}
}

func isValidCodeInSet(validCodes map[string]bool, code string) bool {
	if validCodes[code] {
		return true
	}
	normalized := strings.ToLower(strings.ReplaceAll(code, "_", "-"))
	return validCodes[normalized]
}

func (e *Engine) validateCodeBindings(resType string, schema ResourceSchema, resMap map[string]any, outcome *Outcome) {
	for field, validCodes := range schema.CodeBindings {
		val, present := resMap[field]
		if !present || val == nil {
			continue
		}

		path := resType + "." + field
		uri := schema.ValueSetURIs[field]

		switch v := val.(type) {
		case string:
			if !isValidCodeInSet(validCodes, v) {
				outcome.AddError(CodeInvalid,
					fmt.Sprintf("Code %q is not in required ValueSet %s", v, uri),
					path)
			}
		case map[string]any:
			// Check protojson wrapped value: {"value": "male"} or {"value": "MALE"}
			if wrappedVal, ok := v["value"].(string); ok {
				if !isValidCodeInSet(validCodes, wrappedVal) {
					outcome.AddError(CodeInvalid,
						fmt.Sprintf("Code %q is not in required ValueSet %s", wrappedVal, uri),
						path)
				}
				continue
			}
			// Handle CodeableConcept with coding array
			if codingList, ok := v["coding"].([]any); ok && len(codingList) > 0 {
				foundValid := false
				for _, cItem := range codingList {
					if cMap, ok := cItem.(map[string]any); ok {
						codeStr := extractStringOrWrapped(cMap["code"])
						if codeStr != "" && isValidCodeInSet(validCodes, codeStr) {
							foundValid = true
							break
						}
					}
				}
				if !foundValid {
					outcome.AddError(CodeInvalid,
						fmt.Sprintf("None of the provided codings match required ValueSet %s", uri),
						path)
				}
			} else {
				codeStr := extractStringOrWrapped(v["code"])
				if codeStr != "" && !isValidCodeInSet(validCodes, codeStr) {
					outcome.AddError(CodeInvalid,
						fmt.Sprintf("Code %q is not in required ValueSet %s", codeStr, uri),
						path)
				}
			}
		}
	}
}

func extractStringOrWrapped(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["value"].(string); ok {
			return s
		}
	}
	return ""
}

func isEmptyValue(v any) bool {
	switch val := v.(type) {
	case string:
		return val == ""
	case []any:
		return len(val) == 0
	case map[string]any:
		return len(val) == 0
	default:
		return false
	}
}

var (
	nestedDetailsMu    sync.RWMutex
	nestedDetailsCache = make(map[protoreflect.FullName]protoSchemaDetails)
)

func cachedProtoSchemaDetails(md protoreflect.MessageDescriptor) protoSchemaDetails {
	if md == nil {
		return protoSchemaDetails{}
	}
	fn := md.FullName()
	nestedDetailsMu.RLock()
	det, ok := nestedDetailsCache[fn]
	nestedDetailsMu.RUnlock()
	if ok {
		return det
	}
	det = deriveProtoSchemaDetails(md)
	nestedDetailsMu.Lock()
	nestedDetailsCache[fn] = det
	nestedDetailsMu.Unlock()
	return det
}

func (e *Engine) validateNestedChildren(
	parentPath string,
	childDescs map[string]protoreflect.MessageDescriptor,
	parentMap map[string]any,
	outcome *Outcome,
	depth int,
) {
	if depth > 6 || len(childDescs) == 0 {
		return
	}
	for key, val := range parentMap {
		if val == nil {
			continue
		}
		cleanKey := strings.TrimPrefix(key, "_")
		childMD, ok := childDescs[cleanKey]
		if !ok || childMD == nil {
			continue
		}
		fieldPath := parentPath + "." + key
		if string(childMD.FullName()) == "google.fhir.r4.core.ContainedResource" || string(childMD.FullName()) == "google.protobuf.Any" {
			// Skip full resource schema check on PATCH entry payloads inside Bundles
			if reqMap, hasReq := parentMap["request"].(map[string]any); hasReq {
				if strings.EqualFold(extractStringOrWrapped(reqMap["method"]), "PATCH") {
					continue
				}
			}
			switch v := val.(type) {
			case map[string]any:
				e.validateEmbeddedResource(fieldPath, v, outcome)
			case []any:
				for idx, item := range v {
					if itemMap, ok := item.(map[string]any); ok {
						e.validateEmbeddedResource(fmt.Sprintf("%s[%d]", fieldPath, idx), itemMap, outcome)
					}
				}
			}
			continue
		}

		switch v := val.(type) {
		case map[string]any:
			e.validateComplexElement(fieldPath, childMD, v, outcome, depth+1)
		case []any:
			for idx, item := range v {
				if itemMap, ok := item.(map[string]any); ok {
					e.validateComplexElement(fmt.Sprintf("%s[%d]", fieldPath, idx), childMD, itemMap, outcome, depth+1)
				}
			}
		}
	}
}

func (e *Engine) validateEmbeddedResource(fieldPath string, resMap map[string]any, outcome *Outcome) {
	subOutcome := e.ValidateResource("", resMap)
	for _, iss := range subOutcome.Issues {
		exprs := make([]string, 0, len(iss.Expression))
		for _, ex := range iss.Expression {
			if dotIdx := strings.IndexByte(ex, '.'); dotIdx != -1 {
				exprs = append(exprs, fieldPath+ex[dotIdx:])
			} else {
				exprs = append(exprs, fieldPath)
			}
		}
		if len(exprs) == 0 {
			exprs = []string{fieldPath}
		}
		outcome.Issues = append(outcome.Issues, Issue{
			Severity:    iss.Severity,
			Code:        iss.Code,
			Diagnostics: iss.Diagnostics,
			Expression:  exprs,
		})
	}
}

func (e *Engine) validateComplexElement(
	elementPath string,
	md protoreflect.MessageDescriptor,
	elemMap map[string]any,
	outcome *Outcome,
	depth int,
) {
	if md == nil || depth > 6 {
		return
	}
	det := cachedProtoSchemaDetails(md)

	if e.opts.DisallowUnknownFields {
		for key := range elemMap {
			cleanKey := strings.TrimPrefix(key, "_")
			if !det.allowedFields[cleanKey] {
				outcome.AddError(CodeStructure,
					fmt.Sprintf("Unrecognized nested element %q at %s", key, elementPath),
					elementPath+"."+key)
			}
		}
	}

	if e.opts.EnforcePrimitiveFormats {
		for dField := range det.dateFields {
			if dVal, ok := elemMap[dField]; ok && dVal != nil {
				dStr := extractStringOrWrapped(dVal)
				if dStr != "" && !DateRegex.MatchString(dStr) {
					outcome.AddError(CodeValue,
						fmt.Sprintf("Invalid %s format %q at %s.%s (must be YYYY, YYYY-MM, or YYYY-MM-DD)", dField, dStr, elementPath, dField),
						elementPath+"."+dField)
				}
			}
		}
		for dtField := range det.dateTimeFields {
			if dtVal, ok := elemMap[dtField]; ok && dtVal != nil {
				dtStr := extractStringOrWrapped(dtVal)
				if dtStr != "" && !DateTimeRegex.MatchString(dtStr) {
					outcome.AddError(CodeValue,
						fmt.Sprintf("Invalid %s format %q at %s.%s", dtField, dtStr, elementPath, dtField),
						elementPath+"."+dtField)
				}
			}
		}
		for instField := range det.instantFields {
			if instVal, ok := elemMap[instField]; ok && instVal != nil {
				instStr := extractStringOrWrapped(instVal)
				if instStr != "" && !InstantRegex.MatchString(instStr) {
					outcome.AddError(CodeValue,
						fmt.Sprintf("Invalid %s instant format %q at %s.%s (requires full timestamp with timezone)", instField, instStr, elementPath, instField),
						elementPath+"."+instField)
				}
			}
		}
	}

	e.validateNestedChildren(elementPath, det.childDescriptors, elemMap, outcome, depth)
}

// ValidateJSONWithProfiles validates a raw JSON byte slice against base FHIR R4 rules,
// optional $validate operation mode ("create", "update", "delete"), and explicit or
// meta.profile-declared US Core v6.1.0 / v7.0.0 StructureDefinition profiles.
func (e *Engine) ValidateJSONWithProfiles(expectedResType string, rawJSON []byte, profiles []string, mode string) *Outcome {
	outcome := NewOutcome()
	if len(rawJSON) == 0 {
		outcome.AddError(CodeStructure, "Payload body is empty")
		return outcome
	}
	var resMap map[string]any
	if err := json.Unmarshal(rawJSON, &resMap); err != nil {
		outcome.AddError(CodeStructure, fmt.Sprintf("Malformed FHIR JSON: %v", err))
		return outcome
	}
	return e.ValidateResourceWithProfiles(expectedResType, resMap, profiles, mode)
}

// ValidateResourceWithProfiles validates an unmarshaled JSON map against base FHIR R4 rules,
// optional $validate operation mode ("create", "update", "delete"), and explicit or
// meta.profile-declared US Core v6.1.0 / v7.0.0 StructureDefinition profiles.
func (e *Engine) ValidateResourceWithProfiles(expectedResType string, resMap map[string]any, profiles []string, mode string) *Outcome {
	outcome := e.ValidateResource(expectedResType, resMap)
	resTypeStr, _ := resMap["resourceType"].(string)
	if resTypeStr == "" {
		return outcome
	}

	if trimmedMode := strings.ToLower(strings.TrimSpace(mode)); trimmedMode != "" {
		switch trimmedMode {
		case "create":
		case "update", "delete":
			if extractStringOrWrapped(resMap["id"]) == "" {
				outcome.AddError(CodeRequired,
					fmt.Sprintf("Resource id is required when $validate mode is %q", trimmedMode),
					resTypeStr+".id")
			}
		default:
			outcome.AddError(CodeValue,
				fmt.Sprintf("Unsupported $validate mode %q (expected create, update, or delete)", mode),
				resTypeStr)
		}
	}

	var allProfiles []string
	seen := make(map[string]bool)
	metaProfileSeen := make(map[string]bool)
	addProf := func(p string) {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			allProfiles = append(allProfiles, p)
		}
	}
	for _, p := range profiles {
		for part := range strings.SplitSeq(p, ",") {
			addProf(part)
		}
	}
	if metaMap, ok := resMap["meta"].(map[string]any); ok {
		if profArr, ok := metaMap["profile"].([]any); ok {
			for _, item := range profArr {
				rawMetaProf := strings.TrimSpace(extractStringOrWrapped(item))
				if rawMetaProf != "" {
					baseMetaProf := rawMetaProf
					if pipeIdx := strings.IndexByte(rawMetaProf, '|'); pipeIdx != -1 {
						baseMetaProf = strings.TrimSpace(rawMetaProf[:pipeIdx])
					}
					metaProfileSeen[baseMetaProf] = true
					addProf(rawMetaProf)
				}
			}
		}
	}

	for _, rawProf := range allProfiles {
		baseProf := rawProf
		if pipeIdx := strings.IndexByte(rawProf, '|'); pipeIdx != -1 {
			baseProf = strings.TrimSpace(rawProf[:pipeIdx])
		}
		if after, ok := strings.CutPrefix(baseProf, "http://hl7.org/fhir/StructureDefinition/"); ok {
			targetRT := strings.TrimSpace(after)
			if targetRT != "" && targetRT != "Resource" && targetRT != "DomainResource" && targetRT != resTypeStr {
				outcome.AddError(CodeInvariant,
					fmt.Sprintf("Resource of type %q does not conform to profile %q (expected resourceType %q)", resTypeStr, rawProf, targetRT),
					resTypeStr+".resourceType")
			}
			continue
		}

		spec, known := usCoreProfileSpecs[baseProf]
		if !known {
			if sdMap, ok := e.lookupCustomStructureDefinition(baseProf); ok {
				if !metaProfileSeen[baseProf] {
					e.validateAgainstCustomStructureDefinition(rawProf, sdMap, resTypeStr, resMap, outcome)
				}
				continue
			}
			outcome.AddWarning(CodeNotSupported,
				fmt.Sprintf("Profile %q is not registered in the US Core StructureDefinition registry; validated against base %s schema", rawProf, resTypeStr),
				resTypeStr+".meta.profile")
			continue
		}

		if spec.ResourceType != resTypeStr {
			outcome.AddError(CodeInvariant,
				fmt.Sprintf("Resource of type %q does not conform to %s profile %q", resTypeStr, spec.ResourceType, rawProf),
				resTypeStr+".resourceType")
			continue
		}

		for _, reqField := range spec.RequiredFields {
			if !hasUSCoreRequiredElement(resMap, reqField) {
				exprField := strings.TrimSuffix(reqField, "[x]")
				if idx := strings.IndexByte(exprField, '|'); idx != -1 {
					exprField = exprField[:idx]
				}
				outcome.AddError(CodeRequired,
					fmt.Sprintf("US Core profile %s requires element %q on %s", spec.Slug, reqField, resTypeStr),
					resTypeStr+"."+exprField)
			}
		}

		if spec.CustomCheck != nil {
			spec.CustomCheck(resTypeStr, resMap, outcome)
		}
	}

	return outcome
}

type usCoreProfileSpec struct {
	Slug           string
	ResourceType   string
	RequiredFields []string
	CustomCheck    func(resType string, resMap map[string]any, outcome *Outcome)
}

func hasUSCoreRequiredElement(resMap map[string]any, fieldSpec string) bool {
	for candidate := range strings.SplitSeq(fieldSpec, "|") {
		candidate = strings.TrimSpace(candidate)
		switch candidate {
		case "effective[x]":
			for _, k := range []string{"effectiveDateTime", "effectivePeriod", "effectiveTiming", "effectiveInstant"} {
				if v, ok := resMap[k]; ok && v != nil && !isEmptyValue(v) {
					return true
				}
			}
		case "value[x]":
			for _, k := range []string{
				"valueQuantity", "valueCodeableConcept", "valueString", "valueBoolean",
				"valueInteger", "valueRange", "valueRatio", "valueSampledData",
				"valueTime", "valueDateTime", "valuePeriod",
			} {
				if v, ok := resMap[k]; ok && v != nil && !isEmptyValue(v) {
					return true
				}
			}
		case "medication[x]":
			for _, k := range []string{"medicationCodeableConcept", "medicationReference"} {
				if v, ok := resMap[k]; ok && v != nil && !isEmptyValue(v) {
					return true
				}
			}
		case "occurrence[x]":
			for _, k := range []string{"occurrenceDateTime", "occurrenceString"} {
				if v, ok := resMap[k]; ok && v != nil && !isEmptyValue(v) {
					return true
				}
			}
		case "performed[x]":
			for _, k := range []string{"performedDateTime", "performedPeriod", "performedString", "performedAge", "performedRange"} {
				if v, ok := resMap[k]; ok && v != nil && !isEmptyValue(v) {
					return true
				}
			}
		case "class":
			for _, k := range []string{"class", "classValue"} {
				if v, ok := resMap[k]; ok && v != nil && !isEmptyValue(v) {
					return true
				}
			}
		default:
			if v, ok := resMap[candidate]; ok && v != nil && !isEmptyValue(v) {
				return true
			}
		}
	}
	return false
}

func extractCodesFromElement(v any) []string {
	switch val := v.(type) {
	case string:
		if val != "" {
			return []string{val}
		}
	case map[string]any:
		var out []string
		if s := extractStringOrWrapped(val["code"]); s != "" {
			out = append(out, s)
		} else if s := extractStringOrWrapped(val["value"]); s != "" {
			out = append(out, s)
		}
		if codings, ok := val["coding"].([]any); ok {
			for _, item := range codings {
				if cm, ok := item.(map[string]any); ok {
					if c := extractStringOrWrapped(cm["code"]); c != "" {
						out = append(out, c)
					}
				}
			}
		}
		return out
	}
	return nil
}

func categoryContainsCode(resMap map[string]any, wantCode string) bool {
	cats, ok := resMap["category"].([]any)
	if !ok {
		return false
	}
	for _, item := range cats {
		cc, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, code := range extractCodesFromElement(cc) {
			if strings.EqualFold(code, wantCode) {
				return true
			}
		}
	}
	return false
}

func verificationStatusIsEnteredInError(resMap map[string]any) bool {
	vs, ok := resMap["verificationStatus"]
	if !ok || vs == nil {
		return false
	}
	for _, c := range extractCodesFromElement(vs) {
		if strings.EqualFold(c, "entered-in-error") {
			return true
		}
	}
	return false
}

var usCoreProfileSpecs = map[string]usCoreProfileSpec{
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient": {
		Slug:           "us-core-patient",
		ResourceType:   "Patient",
		RequiredFields: []string{"identifier", "name", "gender"},
		CustomCheck: func(resType string, resMap map[string]any, outcome *Outcome) {
			if ids, ok := resMap["identifier"].([]any); ok {
				hasSysAndVal := false
				for _, it := range ids {
					if m, ok := it.(map[string]any); ok {
						if extractStringOrWrapped(m["system"]) != "" && extractStringOrWrapped(m["value"]) != "" {
							hasSysAndVal = true
							break
						}
					}
				}
				if !hasSysAndVal {
					outcome.AddError(CodeRequired,
						"US Core profile us-core-patient requires at least one identifier with both system and value",
						"Patient.identifier")
				}
			}
			if names, ok := resMap["name"].([]any); ok {
				hasNamePart := false
				for _, it := range names {
					if m, ok := it.(map[string]any); ok {
						if extractStringOrWrapped(m["family"]) != "" || (m["given"] != nil && !isEmptyValue(m["given"])) {
							hasNamePart = true
							break
						}
					}
				}
				if !hasNamePart {
					outcome.AddError(CodeRequired,
						"US Core profile us-core-patient requires at least one name with family or given",
						"Patient.name")
				}
			}
		},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-vital-signs":            vitalSignsSpec("us-core-vital-signs", nil),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-heart-rate":             vitalSignsSpec("us-core-heart-rate", nil),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-respiratory-rate":       vitalSignsSpec("us-core-respiratory-rate", nil),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-body-temperature":       vitalSignsSpec("us-core-body-temperature", nil),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-body-weight":            vitalSignsSpec("us-core-body-weight", nil),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-body-height":            vitalSignsSpec("us-core-body-height", nil),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-bmi":                    vitalSignsSpec("us-core-bmi", nil),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-pulse-oximetry":         vitalSignsSpec("us-core-pulse-oximetry", nil),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-blood-pressure":         vitalSignsSpec("us-core-blood-pressure", checkBloodPressureComponents),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-average-blood-pressure": vitalSignsSpec("us-core-average-blood-pressure", checkBloodPressureComponents),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-lab": {
		Slug:           "us-core-observation-lab",
		ResourceType:   "Observation",
		RequiredFields: []string{"status", "category", "code", "subject", "value[x]|dataAbsentReason|hasMember|component"},
		CustomCheck: func(resType string, resMap map[string]any, outcome *Outcome) {
			if _, ok := resMap["category"]; ok && !categoryContainsCode(resMap, "laboratory") {
				outcome.AddError(CodeInvalid,
					"US Core profile us-core-observation-lab requires category code 'laboratory'",
					"Observation.category")
			}
		},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-smokingstatus": {
		Slug:           "us-core-smokingstatus",
		ResourceType:   "Observation",
		RequiredFields: []string{"status", "code", "subject", "valueCodeableConcept"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-care-experience-preference": {
		Slug:           "us-core-care-experience-preference",
		ResourceType:   "Observation",
		RequiredFields: []string{"status", "code", "subject", "effective[x]", "value[x]|dataAbsentReason"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-treatment-intervention-preference": {
		Slug:           "us-core-treatment-intervention-preference",
		ResourceType:   "Observation",
		RequiredFields: []string{"status", "code", "subject", "effective[x]", "value[x]|dataAbsentReason"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-simple-observation": {
		Slug:           "us-core-simple-observation",
		ResourceType:   "Observation",
		RequiredFields: []string{"status", "category", "code", "subject"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-screening-assessment": {
		Slug:           "us-core-observation-screening-assessment",
		ResourceType:   "Observation",
		RequiredFields: []string{"status", "category", "code", "subject", "effective[x]"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-clinical-result": {
		Slug:           "us-core-observation-clinical-result",
		ResourceType:   "Observation",
		RequiredFields: []string{"status", "category", "code", "subject"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-condition-problems-health-concerns": conditionProfileSpec("us-core-condition-problems-health-concerns"),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-condition-encounter-diagnosis":      conditionProfileSpec("us-core-condition-encounter-diagnosis"),
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-encounter": {
		Slug:           "us-core-encounter",
		ResourceType:   "Encounter",
		RequiredFields: []string{"status", "class", "type", "subject"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medicationrequest": {
		Slug:           "us-core-medicationrequest",
		ResourceType:   "MedicationRequest",
		RequiredFields: []string{"status", "intent", "medication[x]", "subject", "requester"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-allergyintolerance": {
		Slug:           "us-core-allergyintolerance",
		ResourceType:   "AllergyIntolerance",
		RequiredFields: []string{"code", "patient"},
		CustomCheck: func(resType string, resMap map[string]any, outcome *Outcome) {
			if !verificationStatusIsEnteredInError(resMap) && !hasUSCoreRequiredElement(resMap, "clinicalStatus") {
				outcome.AddError(CodeRequired,
					"US Core profile us-core-allergyintolerance requires element \"clinicalStatus\" unless verificationStatus is entered-in-error",
					"AllergyIntolerance.clinicalStatus")
			}
		},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-procedure": {
		Slug:           "us-core-procedure",
		ResourceType:   "Procedure",
		RequiredFields: []string{"status", "code", "subject", "performed[x]"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-diagnosticreport-lab": {
		Slug:           "us-core-diagnosticreport-lab",
		ResourceType:   "DiagnosticReport",
		RequiredFields: []string{"status", "category", "code", "subject", "effective[x]|issued"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-diagnosticreport-note": {
		Slug:           "us-core-diagnosticreport-note",
		ResourceType:   "DiagnosticReport",
		RequiredFields: []string{"status", "category", "code", "subject", "effective[x]|issued"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-immunization": {
		Slug:           "us-core-immunization",
		ResourceType:   "Immunization",
		RequiredFields: []string{"status", "vaccineCode", "patient", "occurrence[x]"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitioner": {
		Slug:           "us-core-practitioner",
		ResourceType:   "Practitioner",
		RequiredFields: []string{"identifier", "name"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-organization": {
		Slug:           "us-core-organization",
		ResourceType:   "Organization",
		RequiredFields: []string{"active", "name"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-documentreference": {
		Slug:           "us-core-documentreference",
		ResourceType:   "DocumentReference",
		RequiredFields: []string{"status", "type", "category", "subject", "content"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-provenance": {
		Slug:           "us-core-provenance",
		ResourceType:   "Provenance",
		RequiredFields: []string{"target", "recorded", "agent"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-careplan": {
		Slug:           "us-core-careplan",
		ResourceType:   "CarePlan",
		RequiredFields: []string{"status", "intent", "category", "subject"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-careteam": {
		Slug:           "us-core-careteam",
		ResourceType:   "CareTeam",
		RequiredFields: []string{"status", "subject", "participant"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-goal": {
		Slug:           "us-core-goal",
		ResourceType:   "Goal",
		RequiredFields: []string{"lifecycleStatus", "description", "subject"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-servicerequest": {
		Slug:           "us-core-servicerequest",
		ResourceType:   "ServiceRequest",
		RequiredFields: []string{"status", "intent", "code", "subject"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-coverage": {
		Slug:           "us-core-coverage",
		ResourceType:   "Coverage",
		RequiredFields: []string{"status", "beneficiary", "payor"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-questionnaireresponse": {
		Slug:           "us-core-questionnaireresponse",
		ResourceType:   "QuestionnaireResponse",
		RequiredFields: []string{"status", "questionnaire", "subject"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medicationdispense": {
		Slug:           "us-core-medicationdispense",
		ResourceType:   "MedicationDispense",
		RequiredFields: []string{"status", "medication[x]", "subject"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medication": {
		Slug:           "us-core-medication",
		ResourceType:   "Medication",
		RequiredFields: []string{"code"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-implantable-device": {
		Slug:           "us-core-implantable-device",
		ResourceType:   "Device",
		RequiredFields: []string{"type", "patient"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitionerrole": {
		Slug:           "us-core-practitionerrole",
		ResourceType:   "PractitionerRole",
		RequiredFields: []string{"practitioner|organization|code|specialty"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-relatedperson": {
		Slug:           "us-core-relatedperson",
		ResourceType:   "RelatedPerson",
		RequiredFields: []string{"patient", "relationship", "name"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-specimen": {
		Slug:           "us-core-specimen",
		ResourceType:   "Specimen",
		RequiredFields: []string{"type", "subject"},
	},
	"http://hl7.org/fhir/us/core/StructureDefinition/us-core-location": {
		Slug:           "us-core-location",
		ResourceType:   "Location",
		RequiredFields: []string{"name"},
	},
}

func vitalSignsSpec(slug string, extra func(resType string, resMap map[string]any, outcome *Outcome)) usCoreProfileSpec {
	return usCoreProfileSpec{
		Slug:           slug,
		ResourceType:   "Observation",
		RequiredFields: []string{"status", "category", "code", "subject", "effective[x]", "value[x]|dataAbsentReason|component"},
		CustomCheck: func(resType string, resMap map[string]any, outcome *Outcome) {
			if _, ok := resMap["category"]; ok && !categoryContainsCode(resMap, "vital-signs") {
				outcome.AddError(CodeInvalid,
					fmt.Sprintf("US Core profile %s requires category code 'vital-signs'", slug),
					"Observation.category")
			}
			if extra != nil {
				extra(resType, resMap, outcome)
			}
		},
	}
}

func checkBloodPressureComponents(resType string, resMap map[string]any, outcome *Outcome) {
	comps, ok := resMap["component"].([]any)
	if !ok || len(comps) == 0 {
		outcome.AddError(CodeRequired,
			"US Core blood pressure profile requires systolic (8480-6) and diastolic (8462-4) components",
			"Observation.component")
		return
	}
	hasSystolic := false
	hasDiastolic := false
	for _, c := range comps {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		for _, code := range extractCodesFromElement(cm["code"]) {
			switch code {
			case "8480-6":
				hasSystolic = true
			case "8462-4":
				hasDiastolic = true
			}
		}
	}
	if !hasSystolic || !hasDiastolic {
		outcome.AddError(CodeRequired,
			"US Core blood pressure profile requires both systolic (8480-6) and diastolic (8462-4) components",
			"Observation.component")
	}
}

func conditionProfileSpec(slug string) usCoreProfileSpec {
	return usCoreProfileSpec{
		Slug:           slug,
		ResourceType:   "Condition",
		RequiredFields: []string{"category", "code", "subject"},
		CustomCheck: func(resType string, resMap map[string]any, outcome *Outcome) {
			if !verificationStatusIsEnteredInError(resMap) && !hasUSCoreRequiredElement(resMap, "clinicalStatus") {
				outcome.AddError(CodeRequired,
					fmt.Sprintf("US Core profile %s requires element \"clinicalStatus\" unless verificationStatus is entered-in-error", slug),
					"Condition.clinicalStatus")
			}
		},
	}
}

// validateR4CoreInvariants enforces high-value HL7 FHIR R4 cross-element invariants:
//   - obs-6: dataAbsentReason SHALL only be present if Observation.value[x] is not present
//   - obs-7: If Observation.code is the same as an Observation.component.code then Observation.value[x] SHALL NOT be present
//   - pat-1: Patient.contact SHALL at least contain a contact's details (name, telecom, address) or a reference to an organization
//   - dom-3: Contained resources SHALL be referenced from elsewhere in the resource or refer to the containing resource (#)
//   - bdl-3: Bundle.entry.request is prohibited for document, message, searchset, and collection bundles
func (e *Engine) validateR4CoreInvariants(resTypeStr string, resMap map[string]any, outcome *Outcome) {
	// dom-3 applies to any DomainResource with contained[]
	if containedArr, ok := resMap["contained"].([]any); ok && len(containedArr) > 0 {
		for i, item := range containedArr {
			cm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			cid := strings.TrimSpace(extractStringOrWrapped(cm["id"]))
			if cid == "" {
				continue
			}
			targetRef := "#" + cid
			referencedInOuter := containsReferenceStringOutsideContainedID(resMap, targetRef, i)
			refersToContainer := containsReferenceString(cm, "#")
			if !referencedInOuter && !refersToContainer {
				outcome.AddError(CodeInvariant,
					fmt.Sprintf("%s violated invariant dom-3: contained resource %s is neither referenced from elsewhere in the resource nor refers to the containing resource", resTypeStr, targetRef),
					fmt.Sprintf("%s.contained[%d]", resTypeStr, i))
			}
		}
	}

	switch resTypeStr {
	case "Observation":
		hasRootValue := hasAnyValueX(resMap)
		if resMap["dataAbsentReason"] != nil && hasRootValue {
			outcome.AddError(CodeInvariant,
				"Observation violated invariant obs-6: dataAbsentReason SHALL only be present if Observation.value[x] is not present",
				"Observation")
		}
		rootCodings := extractCodingPairsFromElement(resMap["code"])
		if comps, ok := resMap["component"].([]any); ok {
			obs7Triggered := false
			for i, compItem := range comps {
				compMap, ok := compItem.(map[string]any)
				if !ok {
					continue
				}
				if compMap["dataAbsentReason"] != nil && hasAnyValueX(compMap) {
					outcome.AddError(CodeInvariant,
						"Observation.component violated invariant obs-6: dataAbsentReason SHALL only be present if Observation.component.value[x] is not present",
						fmt.Sprintf("Observation.component[%d]", i))
				}
				if hasRootValue && !obs7Triggered && len(rootCodings) > 0 {
					compCodings := extractCodingPairsFromElement(compMap["code"])
					if codingPairsOverlap(rootCodings, compCodings) {
						obs7Triggered = true
						outcome.AddError(CodeInvariant,
							"Observation violated invariant obs-7: If Observation.code is the same as an Observation.component.code then Observation.value[x] SHALL NOT be present",
							"Observation")
					}
				}
			}
		}

	case "Patient":
		if contacts, ok := resMap["contact"].([]any); ok {
			for i, item := range contacts {
				cMap, ok := item.(map[string]any)
				if !ok {
					continue
				}
				hasDetails := hasUSCoreRequiredElement(cMap, "name") ||
					hasUSCoreRequiredElement(cMap, "telecom") ||
					hasUSCoreRequiredElement(cMap, "address") ||
					hasUSCoreRequiredElement(cMap, "organization")
				if !hasDetails {
					outcome.AddError(CodeInvariant,
						"Patient.contact violated invariant pat-1: SHALL at least contain a contact's details (name, telecom, or address) or a reference to an organization",
						fmt.Sprintf("Patient.contact[%d]", i))
				}
			}
		}

	case "Bundle":
		bType := strings.TrimSpace(extractStringOrWrapped(resMap["type"]))
		switch bType {
		case "document", "message", "searchset", "collection":
			if entries, ok := resMap["entry"].([]any); ok {
				for i, item := range entries {
					eMap, ok := item.(map[string]any)
					if !ok {
						continue
					}
					if eMap["request"] != nil {
						outcome.AddError(CodeInvariant,
							fmt.Sprintf("Bundle violated invariant bdl-3: entry.request is prohibited when Bundle.type is %q", bType),
							fmt.Sprintf("Bundle.entry[%d].request", i))
					}
				}
			}
		}
	}
}

func hasAnyValueX(m map[string]any) bool {
	for k, v := range m {
		if strings.HasPrefix(k, "value") && len(k) > len("value") && v != nil {
			return true
		}
	}
	return false
}

type codingPair struct {
	system string
	code   string
}

func extractCodingPairsFromElement(val any) []codingPair {
	m, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	codings, ok := m["coding"].([]any)
	if !ok {
		return nil
	}
	var out []codingPair
	for _, item := range codings {
		cm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		code := strings.TrimSpace(extractStringOrWrapped(cm["code"]))
		if code == "" {
			continue
		}
		sys := strings.TrimSpace(extractStringOrWrapped(cm["system"]))
		out = append(out, codingPair{system: sys, code: code})
	}
	return out
}

func codingPairsOverlap(a, b []codingPair) bool {
	for _, pa := range a {
		for _, pb := range b {
			if pa.code == pb.code && (pa.system == pb.system || pa.system == "" || pb.system == "") {
				return true
			}
		}
	}
	return false
}

func containsReferenceStringOutsideContainedID(root map[string]any, targetRef string, containedIdx int) bool {
	for k, v := range root {
		if k == "contained" {
			if arr, ok := v.([]any); ok {
				for idx, item := range arr {
					if idx == containedIdx {
						continue
					}
					if containsReferenceString(item, targetRef) {
						return true
					}
				}
			}
			continue
		}
		if containsReferenceString(v, targetRef) {
			return true
		}
	}
	return false
}

func containsReferenceString(v any, targetRef string) bool {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val) == targetRef || strings.Contains(val, targetRef)
	case map[string]any:
		for k, child := range val {
			if k == "reference" || k == "url" || k == "valueUri" || k == "valueCanonical" {
				if s := strings.TrimSpace(extractStringOrWrapped(child)); s == targetRef {
					return true
				}
			}
			if containsReferenceString(child, targetRef) {
				return true
			}
		}
	case []any:
		for _, item := range val {
			if containsReferenceString(item, targetRef) {
				return true
			}
		}
	}
	return false
}

var ignoredBaseConstraintKeys = map[string]bool{
	"ele-1": true,
	"ext-1": true,
	"dom-1": true,
	"dom-2": true,
	"dom-3": true,
	"dom-4": true,
	"dom-5": true,
	"dom-6": true,
	"rng-2": true,
}

// validateAgainstCustomStructureDefinition validates a resource against a dynamically
// registered FHIR R4 StructureDefinition (snapshot/differential elements & FHIRPath constraints).
func (e *Engine) validateAgainstCustomStructureDefinition(rawProf string, sdMap map[string]any, resTypeStr string, resMap map[string]any, outcome *Outcome) {
	sdType := strings.TrimSpace(extractStringOrWrapped(sdMap["type"]))
	if sdType != "" && sdType != "Resource" && sdType != "DomainResource" && sdType != resTypeStr {
		outcome.AddError(CodeInvariant,
			fmt.Sprintf("Resource of type %q does not conform to profile %q (expected resourceType %q)", resTypeStr, rawProf, sdType),
			resTypeStr+".resourceType")
		return
	}

	var elements []map[string]any
	appendElements := func(containerKey string) {
		if container, ok := sdMap[containerKey].(map[string]any); ok {
			if arr, ok := container["element"].([]any); ok {
				for _, item := range arr {
					if em, ok := item.(map[string]any); ok {
						elements = append(elements, em)
					}
				}
			}
		}
	}
	// Prefer differential elements if present; also include snapshot elements if differential is absent.
	appendElements("differential")
	if len(elements) == 0 {
		appendElements("snapshot")
	}

	seenConstraints := make(map[string]bool)
	for _, elemMap := range elements {
		elemPath := strings.TrimSpace(extractStringOrWrapped(elemMap["path"]))
		if elemPath == "" {
			elemPath = resTypeStr
		}
		sliceName := strings.TrimSpace(extractStringOrWrapped(elemMap["sliceName"]))

		// 1. Evaluate custom FHIRPath constraints declared on this element
		if constraints, ok := elemMap["constraint"].([]any); ok && e.fhirPathEval != nil {
			for _, cItem := range constraints {
				cMap, ok := cItem.(map[string]any)
				if !ok {
					continue
				}
				key := strings.TrimSpace(extractStringOrWrapped(cMap["key"]))
				if ignoredBaseConstraintKeys[key] {
					continue
				}
				expr := strings.TrimSpace(extractStringOrWrapped(cMap["expression"]))
				if expr == "" {
					continue
				}
				dedupKey := elemPath + "|" + key + "|" + expr
				if seenConstraints[dedupKey] {
					continue
				}
				seenConstraints[dedupKey] = true

				human := strings.TrimSpace(extractStringOrWrapped(cMap["human"]))
				if human == "" {
					human = fmt.Sprintf("FHIRPath constraint %q (%s) failed", key, expr)
				}
				severity := strings.ToLower(strings.TrimSpace(extractStringOrWrapped(cMap["severity"])))

				evalExpr := expr
				if after, ok := strings.CutPrefix(elemPath, resTypeStr+"."); ok && after != "" {
					// If expression isn't already rooted at resourceType or child path, scope or check existence
					if !hasUSCoreRequiredElement(resMap, after) {
						continue
					}
				}
				results := e.fhirPathEval(resMap, evalExpr)
				if !fhirPathResultTruthy(results) {
					msg := fmt.Sprintf("Profile %s constraint %s failed: %s (expression: %s)", rawProf, key, human, expr)
					if severity == "warning" {
						outcome.AddWarning(CodeInvariant, msg, elemPath)
					} else {
						outcome.AddError(CodeInvariant, msg, elemPath)
					}
				}
			}
		}

		// 2. Enforce direct child cardinality (min >= 1, max == "0") and fixed[x] values
		relPath, isChild := strings.CutPrefix(elemPath, resTypeStr+".")
		if !isChild || relPath == "" {
			continue
		}

		minCard := extractMinCardinality(elemMap["min"])
		maxCard := strings.TrimSpace(extractStringOrWrapped(elemMap["max"]))

		// Handle extension slice requirements (e.g., Patient.extension:mySlice with fixedUri or type.profile)
		if relPath == "extension" && sliceName != "" {
			reqExtURL := extractExtensionSliceURL(elemMap)
			if minCard >= 1 && reqExtURL != "" {
				if !resourceHasExtensionURL(resMap, reqExtURL) {
					outcome.AddError(CodeRequired,
						fmt.Sprintf("Profile %q requires extension slice %q (%s) on %s", rawProf, sliceName, reqExtURL, resTypeStr),
						elemPath)
				}
			}
			continue
		}

		if sliceName != "" || strings.Contains(relPath, ".") {
			continue
		}

		if minCard >= 1 && !hasUSCoreRequiredElement(resMap, relPath) {
			exprField := strings.TrimSuffix(relPath, "[x]")
			outcome.AddError(CodeRequired,
				fmt.Sprintf("Profile %q requires element %q (min=%d) on %s", rawProf, relPath, minCard, resTypeStr),
				resTypeStr+"."+exprField)
		}

		if maxCard == "0" && hasUSCoreRequiredElement(resMap, relPath) {
			exprField := strings.TrimSuffix(relPath, "[x]")
			outcome.AddError(CodeStructure,
				fmt.Sprintf("Profile %q prohibits element %q (max=0) on %s", rawProf, relPath, resTypeStr),
				resTypeStr+"."+exprField)
		}

		for _, fixedKey := range []string{"fixedCode", "fixedString", "fixedUri", "fixedCanonical"} {
			if expectedFixed := strings.TrimSpace(extractStringOrWrapped(elemMap[fixedKey])); expectedFixed != "" {
				if actualVal, exists := resMap[relPath]; exists && actualVal != nil {
					actualStr := strings.TrimSpace(extractStringOrWrapped(actualVal))
					if actualStr != expectedFixed {
						outcome.AddError(CodeValue,
							fmt.Sprintf("Profile %q requires %s=%q (got %q)", rawProf, elemPath, expectedFixed, actualStr),
							elemPath)
					}
				}
			}
		}
	}
}

func extractMinCardinality(v any) int {
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	case int64:
		return int(val)
	case json.Number:
		if n, err := val.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

func fhirPathResultTruthy(results []any) bool {
	if len(results) == 0 {
		return false
	}
	if len(results) == 1 {
		if b, ok := results[0].(bool); ok {
			return b
		}
	}
	return true
}

func extractExtensionSliceURL(elemMap map[string]any) string {
	if u := strings.TrimSpace(extractStringOrWrapped(elemMap["fixedUri"])); u != "" {
		return u
	}
	if types, ok := elemMap["type"].([]any); ok {
		for _, tItem := range types {
			tm, ok := tItem.(map[string]any)
			if !ok {
				continue
			}
			if profs, ok := tm["profile"].([]any); ok {
				for _, p := range profs {
					if u := strings.TrimSpace(extractStringOrWrapped(p)); u != "" {
						return u
					}
				}
			}
		}
	}
	return ""
}

func resourceHasExtensionURL(resMap map[string]any, targetURL string) bool {
	exts, ok := resMap["extension"].([]any)
	if !ok {
		return false
	}
	for _, item := range exts {
		em, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(extractStringOrWrapped(em["url"])) == targetURL {
			return true
		}
	}
	return false
}
