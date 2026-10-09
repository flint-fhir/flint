package validation

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Validator validates FHIR resources against StructureDefinitions and ValueSets.
type Validator interface {
	ValidateJSON(expectedResType string, rawJSON []byte) *Outcome
	ValidateResource(expectedResType string, resMap map[string]any) *Outcome
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
	schemas map[string]ResourceSchema
	opts    Options
}

// NewEngine creates a new validation engine with default Big 5 schemas.
func NewEngine(opts Options) *Engine {
	return &Engine{
		schemas: DefaultSchemas(),
		opts:    opts,
	}
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
				outcome.AddError(CodeRequired,
					fmt.Sprintf("Missing required element %q on %s", reqField, resTypeStr),
					resTypeStr+"."+reqField)
			}
		}
	}

	// 5. Primitive type format checks
	if e.opts.EnforcePrimitiveFormats {
		e.validatePrimitiveFormats(resTypeStr, resMap, outcome)
	}

	// 6. ValueSet code bindings check
	if e.opts.EnforceValueSets {
		e.validateCodeBindings(resTypeStr, schema, resMap, outcome)
	}

	return outcome
}

func (e *Engine) validatePrimitiveFormats(resType string, resMap map[string]any, outcome *Outcome) {
	// id format
	if idVal, ok := resMap["id"]; ok && idVal != nil {
		idStr := extractStringOrWrapped(idVal)
		if idStr != "" && !IDRegex.MatchString(idStr) {
			outcome.AddError(CodeValue,
				fmt.Sprintf("Invalid resource id format %q (must match regex %s)", idStr, IDRegex.String()),
				resType+".id")
		}
	}

	// birthDate format
	if bVal, ok := resMap["birthDate"]; ok && bVal != nil {
		bStr := extractStringOrWrapped(bVal)
		if bStr != "" && !DateRegex.MatchString(bStr) {
			outcome.AddError(CodeValue,
				fmt.Sprintf("Invalid birthDate format %q (must be YYYY, YYYY-MM, or YYYY-MM-DD)", bStr),
				resType+".birthDate")
		}
	}

	// effectiveDateTime format
	if dtVal, ok := resMap["effectiveDateTime"]; ok && dtVal != nil {
		dtStr := extractStringOrWrapped(dtVal)
		if dtStr != "" && !DateTimeRegex.MatchString(dtStr) {
			outcome.AddError(CodeValue,
				fmt.Sprintf("Invalid effectiveDateTime format %q", dtStr),
				resType+".effectiveDateTime")
		}
	}

	// issued format (instant)
	if instVal, ok := resMap["issued"]; ok && instVal != nil {
		instStr := extractStringOrWrapped(instVal)
		if instStr != "" && !InstantRegex.MatchString(instStr) {
			outcome.AddError(CodeValue,
				fmt.Sprintf("Invalid issued instant format %q (requires full timestamp with timezone)", instStr),
				resType+".issued")
		}
	}
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
			if !validCodes[v] {
				outcome.AddError(CodeInvalid,
					fmt.Sprintf("Code %q is not in required ValueSet %s", v, uri),
					path)
			}
		case map[string]any:
			// Check protojson wrapped value: {"value": "male"}
			if wrappedVal, ok := v["value"].(string); ok {
				if !validCodes[wrappedVal] {
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
						if codeStr != "" && validCodes[codeStr] {
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
				if codeStr != "" && !validCodes[codeStr] {
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
