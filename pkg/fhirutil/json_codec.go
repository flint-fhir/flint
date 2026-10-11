package fhirutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
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

// DenormalizeProtoJSONToFHIRJSON converts google/fhir binary proto bytes or protojson output
// into standard flat HL7 FHIR R4 JSON (unwrapping primitive wrappers, formatting temporal
// fields as ISO-8601 strings, converting typed Reference fields to {"reference": "Type/id"},
// flattening choice[x] containers like valueQuantity/effectiveDateTime, and injecting
// resourceType and server meta.versionId / meta.lastUpdated).
func DenormalizeProtoJSONToFHIRJSON(md protoreflect.MessageDescriptor, resType string, inputBytes []byte, version int, lastUpdated time.Time) ([]byte, error) {
	protoJSONBytes := inputBytes
	trimmed := bytes.TrimSpace(inputBytes)
	if (len(trimmed) == 0 || trimmed[0] != '{') && md != nil {
		msg := dynamicpb.NewMessage(md)
		if err := proto.Unmarshal(inputBytes, msg); err != nil {
			return nil, fmt.Errorf("unmarshal %s proto: %w", resType, err)
		}
		marshaler := protojson.MarshalOptions{
			UseProtoNames:   false,
			EmitUnpopulated: false,
		}
		var err error
		protoJSONBytes, err = marshaler.Marshal(msg)
		if err != nil {
			return nil, err
		}
	}

	var m map[string]any
	if err := json.Unmarshal(protoJSONBytes, &m); err != nil {
		return protoJSONBytes, err
	}
	if md != nil {
		denormalizeMessageMap(md, m)
	}
	if resType != "" {
		m["resourceType"] = resType
	}
	if version > 0 || !lastUpdated.IsZero() {
		metaMap, ok := m["meta"].(map[string]any)
		if !ok || metaMap == nil {
			metaMap = make(map[string]any)
		}
		if version > 0 {
			metaMap["versionId"] = strconv.Itoa(version)
		}
		if !lastUpdated.IsZero() {
			metaMap["lastUpdated"] = lastUpdated.UTC().Format(time.RFC3339)
		}
		m["meta"] = metaMap
	}
	return json.Marshal(m)
}

// InjectResourceType ensures a protojson-marshaled FHIR resource includes "resourceType".
func InjectResourceType(resType string, protoJSONBytes []byte) ([]byte, error) {
	return DenormalizeProtoJSONToFHIRJSON(nil, resType, protoJSONBytes, 0, time.Time{})
}

// RewriteURNReferences recursively replaces intra-bundle urn:uuid:... reference strings
// in a raw FHIR JSON resource using the provided urnToTarget map (e.g., "urn:uuid:..." -> "Patient/123").
func RewriteURNReferences(rawJSON []byte, urnToTarget map[string]string) ([]byte, error) {
	if len(urnToTarget) == 0 || len(rawJSON) == 0 {
		return rawJSON, nil
	}
	var root any
	if err := json.Unmarshal(rawJSON, &root); err != nil {
		return rawJSON, err
	}
	if rewriteURNNode(root, urnToTarget, "") {
		return json.Marshal(root)
	}
	return rawJSON, nil
}

func rewriteURNNode(node any, urnToTarget map[string]string, parentKey string) bool {
	changed := false
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if s, ok := child.(string); ok {
				if resolved, exists := urnToTarget[s]; exists {
					if k == "id" || (k == "value" && parentKey == "id") {
						if slashIdx := strings.LastIndex(resolved, "/"); slashIdx != -1 {
							resolved = resolved[slashIdx+1:]
						}
					}
					v[k] = resolved
					changed = true
				}
			} else if rewriteURNNode(child, urnToTarget, k) {
				changed = true
			}
		}
	case []any:
		for _, elem := range v {
			if rewriteURNNode(elem, urnToTarget, parentKey) {
				changed = true
			}
		}
	}
	return changed
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
	// Snapshot keys to safely mutate map during iteration
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	for _, key := range keys {
		val, exists := m[key]
		if !exists || strings.HasPrefix(key, "_") || key == "resourceType" || val == nil {
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

		targetKey := fd.JSONName()
		msgDesc := fd.Message()
		if fd.IsList() {
			arr, ok := val.([]any)
			if !ok {
				continue
			}
			for i, elem := range arr {
				arr[i] = normalizeSingleValue(msgDesc, elem, nil)
			}
			delete(m, key)
			m[targetKey] = arr
		} else {
			var extMap map[string]any
			if rawExt, ok := m["_"+key].(map[string]any); ok {
				extMap = rawExt
			}
			normalized := normalizeSingleValue(msgDesc, val, extMap)
			delete(m, key)
			m[targetKey] = normalized
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
		if subFD == nil && choiceFieldJSON == "string" {
			// Also try "stringValue" for "String" choice in google/fhir
			subFD = findFieldDescriptor(choiceMsgDesc.Fields(), "stringValue")
		}
		if subFD == nil || subFD.Kind() != protoreflect.MessageKind {
			continue
		}

		normalizedSub := normalizeSingleValue(subFD.Message(), val, nil)
		delete(m, key)
		m[prefix] = map[string]any{
			subFD.JSONName(): normalizedSub,
		}
		return true
	}
	return false
}

func denormalizeMessageMap(md protoreflect.MessageDescriptor, m map[string]any) {
	if md == nil || m == nil {
		return
	}

	if string(md.FullName()) == "google.fhir.r4.core.Reference" {
		denormalizeReferenceMap(md, m)
	}

	fields := md.Fields()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	for _, key := range keys {
		val, exists := m[key]
		if !exists || key == "resourceType" || val == nil {
			continue
		}

		fd := findFieldDescriptor(fields, key)
		if fd == nil || fd.Kind() != protoreflect.MessageKind {
			continue
		}

		msgDesc := fd.Message()
		// Check if this field is a FHIR choice[x] wrapper message (has oneof "choice")
		if msgDesc.Oneofs().ByName("choice") != nil {
			if choiceMap, ok := val.(map[string]any); ok {
				for subKey, subVal := range choiceMap {
					subFD := findFieldDescriptor(msgDesc.Fields(), subKey)
					if subFD != nil && subFD.Kind() == protoreflect.MessageKind {
						denormSub := denormalizeSingleValue(subFD.Message(), subVal)
						suffixName := subFD.JSONName()
						if suffixName == "stringValue" {
							suffixName = "string"
						}
						flatKey := fd.JSONName() + strings.ToUpper(suffixName[:1]) + suffixName[1:]
						delete(m, key)
						m[flatKey] = denormSub
						break
					}
				}
				continue
			}
		}

		if fd.IsList() {
			arr, ok := val.([]any)
			if !ok {
				continue
			}
			for i, elem := range arr {
				arr[i] = denormalizeSingleValue(msgDesc, elem)
			}
			m[key] = arr
		} else {
			denormVal := denormalizeSingleValue(msgDesc, val)
			outKey := key
			if string(fd.Name()) == "class_value" {
				outKey = "class"
			}
			delete(m, key)
			m[outKey] = denormVal
		}
	}
}

func denormalizeSingleValue(msgDesc protoreflect.MessageDescriptor, val any) any {
	if val == nil || msgDesc == nil {
		return val
	}
	vMap, ok := val.(map[string]any)
	if !ok {
		return val
	}

	fullName := string(msgDesc.FullName())
	if isTemporalMessage(fullName) {
		if formatted := formatProtoTemporalToISO(vMap); formatted != "" {
			return formatted
		}
	}

	if fullName == "google.fhir.r4.core.Decimal" {
		if rawStr, ok := vMap["value"].(string); ok {
			if num, err := strconv.ParseFloat(rawStr, 64); err == nil {
				return num
			}
		}
	}

	valFD := msgDesc.Fields().ByName("value")
	if valFD != nil && valFD.Kind() != protoreflect.MessageKind {
		if rawVal, hasVal := vMap["value"]; hasVal && len(vMap) == 1 {
			if valFD.Kind() == protoreflect.EnumKind {
				if s, ok := rawVal.(string); ok {
					return strings.ToLower(strings.ReplaceAll(s, "_", "-"))
				}
			}
			return rawVal
		}
	}

	denormalizeMessageMap(msgDesc, vMap)
	return vMap
}

func denormalizeReferenceMap(md protoreflect.MessageDescriptor, m map[string]any) {
	if rawURI, ok := m["uri"].(map[string]any); ok {
		if s, ok := rawURI["value"].(string); ok && s != "" {
			delete(m, "uri")
			m["reference"] = s
			return
		}
	}

	refOneof := md.Oneofs().ByName("reference")
	if refOneof == nil {
		return
	}
	fields := refOneof.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		jsonName := fd.JSONName()
		if !strings.HasSuffix(jsonName, "Id") {
			continue
		}
		if subMap, ok := m[jsonName].(map[string]any); ok {
			if idStr, ok := subMap["value"].(string); ok && idStr != "" {
				typePrefix := strings.ToUpper(jsonName[:1]) + jsonName[1:len(jsonName)-2]
				delete(m, jsonName)
				m["reference"] = typePrefix + "/" + idStr
				return
			}
		}
	}
}

func formatProtoTemporalToISO(vMap map[string]any) string {
	var us int64
	switch raw := vMap["valueUs"].(type) {
	case string:
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return ""
		}
		us = parsed
	case float64:
		us = int64(raw)
	default:
		return ""
	}

	t := time.UnixMicro(us).UTC()
	precision, _ := vMap["precision"].(string)
	switch precision {
	case "YEAR":
		return t.Format("2006")
	case "MONTH":
		return t.Format("2006-01")
	case "DAY":
		return t.Format("2006-01-02")
	case "SECOND":
		return t.Format("2006-01-02T15:04:05Z")
	case "MILLISECOND", "MICROSECOND":
		return t.Format(time.RFC3339Nano)
	default:
		if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
			return t.Format("2006-01-02")
		}
		return t.Format(time.RFC3339)
	}
}

func findFieldDescriptor(fields protoreflect.FieldDescriptors, jsonKey string) protoreflect.FieldDescriptor {
	if fd := fields.ByJSONName(jsonKey); fd != nil {
		return fd
	}
	if fd := fields.ByName(protoreflect.Name(jsonKey)); fd != nil {
		return fd
	}
	// Support google/fhir suffixed field names such as classValue <-> class_value ("class")
	if strings.HasSuffix(jsonKey, "Value") {
		base := jsonKey[:len(jsonKey)-5]
		if fd := fields.ByJSONName(base); fd != nil {
			return fd
		}
		if fd := fields.ByName(protoreflect.Name(base + "_value")); fd != nil {
			return fd
		}
	}
	if fd := fields.ByJSONName(jsonKey + "Value"); fd != nil {
		return fd
	}
	if fd := fields.ByName(protoreflect.Name(jsonKey + "_value")); fd != nil {
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
