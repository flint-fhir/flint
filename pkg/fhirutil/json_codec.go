package fhirutil

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// NormalizeFHIRJSONToProtoJSON normalizes incoming FHIR JSON (supporting both standard
// flat HL7 FHIR R4 JSON and google/fhir wrapped protojson) into the structure expected
// by protojson.Unmarshal for a given google/fhir R4 MessageDescriptor.
func NormalizeFHIRJSONToProtoJSON(md protoreflect.MessageDescriptor, rawJSON []byte) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(rawJSON, &root); err != nil {
		return nil, err
	}
	normalizeMessageMap(md, root)
	return json.Marshal(root)
}

// InjectResourceTypeAndUnwrap ensures a protojson-marshaled FHIR resource includes
// "resourceType".
func InjectResourceType(resType string, protoJSONBytes []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(protoJSONBytes, &m); err != nil {
		return protoJSONBytes, err
	}
	if _, exists := m["resourceType"]; !exists && resType != "" {
		m["resourceType"] = resType
	}
	return json.Marshal(m)
}

func normalizeMessageMap(md protoreflect.MessageDescriptor, m map[string]any) {
	if md == nil || m == nil {
		return
	}

	// Special handling for google.fhir.r4.core.Reference:
	// Convert standard FHIR {"reference": "Patient/123"} into google/fhir typed reference oneof
	// (e.g., {"patientId": {"value": "123"}}) so proto2type reference extractors work seamlessly.
	if string(md.FullName()) == "google.fhir.r4.core.Reference" {
		normalizeReferenceMap(md, m)
	}

	fields := md.Fields()
	for key, val := range m {
		if strings.HasPrefix(key, "_") || key == "resourceType" || val == nil {
			continue
		}

		fd := findFieldDescriptor(fields, key)
		if fd == nil {
			// Check FHIR choice[x] fields (e.g., "valueQuantity" -> field "value" with message containing "quantity")
			if normalizeChoiceField(fields, m, key, val) {
				continue
			}
			continue
		}

		if fd.Kind() != protoreflect.MessageKind {
			continue
		}

		msgDesc := fd.Message()
		if fd.IsList() {
			arr, ok := val.([]any)
			if !ok {
				continue
			}
			for i, elem := range arr {
				arr[i] = normalizeSingleValue(msgDesc, elem, nil)
			}
			m[key] = arr
		} else {
			var extMap map[string]any
			if rawExt, ok := m["_"+key].(map[string]any); ok {
				extMap = rawExt
			}
			m[key] = normalizeSingleValue(msgDesc, val, extMap)
			if extMap != nil {
				delete(m, "_"+key)
			}
		}
	}
}

func normalizeSingleValue(msgDesc protoreflect.MessageDescriptor, val any, extMap map[string]any) any {
	if val == nil || msgDesc == nil {
		return val
	}

	fullName := string(msgDesc.FullName())

	// Handle Date / DateTime / Instant flat ISO strings or {"value": "1990-01-15"} wrappers
	if isTemporalMessage(fullName) {
		switch v := val.(type) {
		case string:
			if converted := parseTemporalToProtoMap(fullName, v, extMap); converted != nil {
				return converted
			}
		case map[string]any:
			if rawStr, ok := v["value"].(string); ok && v["valueUs"] == nil && v["value_us"] == nil {
				if converted := parseTemporalToProtoMap(fullName, rawStr, extMap); converted != nil {
					return converted
				}
			}
		}
	}

	valFD := msgDesc.Fields().ByName("value")

	switch v := val.(type) {
	case map[string]any:
		// Already a map: if it wraps an enum in "value", normalize enum casing (e.g. "in-progress" -> "IN_PROGRESS")
		if valFD != nil && valFD.Kind() == protoreflect.EnumKind {
			if s, ok := v["value"].(string); ok {
				v["value"] = normalizeEnumString(s)
			}
		}
		// If it wraps a Decimal where value is a string in google/fhir Decimal proto
		if fullName == "google.fhir.r4.core.Decimal" {
			if num, ok := v["value"].(float64); ok {
				v["value"] = strconv.FormatFloat(num, 'f', -1, 64)
			}
		}
		mergeExtensions(v, extMap)
		normalizeMessageMap(msgDesc, v)
		return v

	case string:
		if valFD != nil {
			wrappedVal := any(v)
			if valFD.Kind() == protoreflect.EnumKind {
				wrappedVal = normalizeEnumString(v)
			}
			out := map[string]any{"value": wrappedVal}
			mergeExtensions(out, extMap)
			return out
		}

	case bool:
		if valFD != nil {
			out := map[string]any{"value": v}
			mergeExtensions(out, extMap)
			return out
		}

	case float64:
		if valFD != nil {
			wrappedVal := any(v)
			if fullName == "google.fhir.r4.core.Decimal" || valFD.Kind() == protoreflect.StringKind {
				wrappedVal = strconv.FormatFloat(v, 'f', -1, 64)
			}
			out := map[string]any{"value": wrappedVal}
			mergeExtensions(out, extMap)
			return out
		}
	}

	return val
}

func normalizeReferenceMap(md protoreflect.MessageDescriptor, m map[string]any) {
	rawRef, exists := m["reference"]
	if !exists {
		return
	}
	var refStr string
	switch v := rawRef.(type) {
	case string:
		refStr = v
	case map[string]any:
		refStr, _ = v["value"].(string)
	}
	if refStr == "" {
		return
	}

	// Split "ResourceType/id" (e.g. "Patient/pat-1")
	parts := strings.SplitN(refStr, "/", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		resType := parts[0]
		resID := parts[1]
		// Lowercase first letter + rest + "Id" (e.g. "Patient" -> "patientId", "Practitioner" -> "practitionerId")
		fieldJSONName := strings.ToLower(resType[:1]) + resType[1:] + "Id"
		if fd := findFieldDescriptor(md.Fields(), fieldJSONName); fd != nil {
			delete(m, "reference")
			m[fieldJSONName] = map[string]any{"value": resID}
			return
		}
	}

	// Fallback to uri field in Reference oneof
	delete(m, "reference")
	m["uri"] = map[string]any{"value": refStr}
}

func normalizeChoiceField(fields protoreflect.FieldDescriptors, m map[string]any, key string, val any) bool {
	// Look for a choice[x] container field whose name is a prefix of key (e.g., "valueQuantity" -> "value", "effectiveDateTime" -> "effective", "onsetDateTime" -> "onset", "deceasedBoolean" -> "deceased")
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Kind() != protoreflect.MessageKind {
			continue
		}
		prefix := fd.JSONName()
		if !strings.HasPrefix(key, prefix) || len(key) <= len(prefix) {
			continue
		}
		suffix := key[len(prefix):]
		// Suffix must start with an uppercase letter in camelCase (e.g. "Quantity", "DateTime", "Boolean")
		if suffix[0] < 'A' || suffix[0] > 'Z' {
			continue
		}
		choiceFieldJSON := strings.ToLower(suffix[:1]) + suffix[1:]
		choiceMsgDesc := fd.Message()
		subFD := findFieldDescriptor(choiceMsgDesc.Fields(), choiceFieldJSON)
		if subFD == nil {
			// Also try "stringValue" for "String" choice in google/fhir
			if choiceFieldJSON == "string" {
				subFD = findFieldDescriptor(choiceMsgDesc.Fields(), "stringValue")
				if subFD != nil {
					choiceFieldJSON = "stringValue"
				}
			}
		}
		if subFD == nil || subFD.Kind() != protoreflect.MessageKind {
			continue
		}

		normalizedSub := normalizeSingleValue(subFD.Message(), val, nil)
		delete(m, key)
		m[prefix] = map[string]any{
			choiceFieldJSON: normalizedSub,
		}
		return true
	}
	return false
}

func findFieldDescriptor(fields protoreflect.FieldDescriptors, jsonKey string) protoreflect.FieldDescriptor {
	if fd := fields.ByJSONName(jsonKey); fd != nil {
		return fd
	}
	if fd := fields.ByName(protoreflect.Name(jsonKey)); fd != nil {
		return fd
	}
	return nil
}

func normalizeEnumString(s string) string {
	trimmed := strings.TrimSpace(s)
	return strings.ToUpper(strings.ReplaceAll(trimmed, "-", "_"))
}

func isTemporalMessage(fullName string) bool {
	switch fullName {
	case "google.fhir.r4.core.Date", "google.fhir.r4.core.DateTime", "google.fhir.r4.core.Instant":
		return true
	default:
		return false
	}
}

func parseTemporalToProtoMap(fullName, s string, extMap map[string]any) map[string]any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	var t time.Time
	var precision string
	var err error

	switch {
	case len(s) == 4:
		t, err = time.Parse("2006", s)
		precision = "YEAR"
	case len(s) == 7:
		t, err = time.Parse("2006-01", s)
		precision = "MONTH"
	case len(s) == 10:
		t, err = time.Parse("2006-01-02", s)
		precision = "DAY"
	default:
		t, err = time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t, err = time.Parse("2006-01-02T15:04:05", s)
		}
		precision = "SECOND"
		if strings.Contains(s, ".") {
			precision = "MILLISECOND"
			if fullName == "google.fhir.r4.core.Instant" {
				precision = "MICROSECOND"
			}
		}
	}
	if err != nil {
		return nil
	}

	out := map[string]any{
		"valueUs":   strconv.FormatInt(t.UTC().UnixMicro(), 10),
		"precision": precision,
		"timezone":  "UTC",
	}
	mergeExtensions(out, extMap)
	return out
}

func mergeExtensions(target map[string]any, extMap map[string]any) {
	for k, v := range extMap {
		if _, exists := target[k]; !exists {
			target[k] = v
		}
	}
}
