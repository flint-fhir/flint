package fhirutil

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// ProtoDescriptorToOpenAPISchema compiles a google/fhir R4 protoreflect.MessageDescriptor
// directly into an OpenAPI 3.1.0 JSON Schema object representing the flat HL7 FHIR R4 wire format.
//
// It inspects protobuf fields, repeated cardinalities, primitive wrappers, ValueSet EnumDescriptors,
// typed Reference oneofs, and choice[x] oneof containers (such as Observation.value[x],
// Observation.effective[x], Condition.onset[x], Condition.abatement[x], and Patient.deceased[x]).
func ProtoDescriptorToOpenAPISchema(resType string, md protoreflect.MessageDescriptor, requiredFields []string) map[string]any {
	props := map[string]any{
		"resourceType": map[string]any{
			"type":        "string",
			"enum":        []string{resType},
			"description": fmt.Sprintf("FHIR R4 resource type discriminator (%s)", resType),
		},
	}

	if md != nil {
		compileProtoFieldsIntoProperties(md, props, 0)
	}

	reqSet := map[string]bool{"resourceType": true}
	reqList := []string{"resourceType"}
	for _, rf := range requiredFields {
		if rf != "" && !reqSet[rf] {
			reqSet[rf] = true
			reqList = append(reqList, rf)
		}
	}

	schema := map[string]any{
		"type":        "object",
		"description": fmt.Sprintf("HL7 FHIR R4 %s resource (compiled from protobuf descriptor)", resType),
		"required":    reqList,
		"properties":  props,
	}
	if md != nil {
		schema["x-protobuf-full-name"] = string(md.FullName())
	}
	return schema
}

func compileProtoFieldsIntoProperties(md protoreflect.MessageDescriptor, props map[string]any, depth int) {
	if md == nil {
		return
	}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Kind() != protoreflect.MessageKind {
			continue
		}
		msgDesc := fd.Message()

		// Expand FHIR choice[x] container messages (messages with oneof "choice") into flat camelCase properties
		if choiceOneof := msgDesc.Oneofs().ByName("choice"); choiceOneof != nil {
			choiceFields := choiceOneof.Fields()
			for j := 0; j < choiceFields.Len(); j++ {
				subFD := choiceFields.Get(j)
				if subFD.Kind() != protoreflect.MessageKind {
					continue
				}
				suffixName := subFD.JSONName()
				if suffixName == "stringValue" {
					suffixName = "string"
				}
				if len(suffixName) == 0 {
					continue
				}
				flatKey := fd.JSONName() + strings.ToUpper(suffixName[:1]) + suffixName[1:]
				subSchema := compileProtoMessageToSchema(subFD.Message(), depth+1)
				subSchema["x-protobuf-field"] = string(fd.Name()) + "." + string(subFD.Name())
				subSchema["x-protobuf-oneof"] = string(msgDesc.FullName())
				subSchema["x-protobuf-number"] = int(subFD.Number())
				props[flatKey] = subSchema
			}
			continue
		}

		propKey := fd.JSONName()
		if string(fd.Name()) == "class_value" {
			propKey = "class"
		}

		itemSchema := compileProtoMessageToSchema(msgDesc, depth+1)
		if fd.IsList() {
			props[propKey] = map[string]any{
				"type":              "array",
				"items":             itemSchema,
				"x-protobuf-field":  string(fd.Name()),
				"x-protobuf-number": int(fd.Number()),
			}
		} else {
			itemSchema["x-protobuf-field"] = string(fd.Name())
			itemSchema["x-protobuf-number"] = int(fd.Number())
			props[propKey] = itemSchema
		}
	}
}

func compileProtoMessageToSchema(msgDesc protoreflect.MessageDescriptor, depth int) map[string]any {
	if msgDesc == nil {
		return map[string]any{"type": "object"}
	}
	fullName := string(msgDesc.FullName())

	switch fullName {
	case "google.fhir.r4.core.Boolean":
		return map[string]any{
			"type":               "boolean",
			"x-protobuf-message": fullName,
		}
	case "google.fhir.r4.core.Integer", "google.fhir.r4.core.PositiveInt", "google.fhir.r4.core.UnsignedInt":
		return map[string]any{
			"type":               "integer",
			"x-protobuf-message": fullName,
		}
	case "google.fhir.r4.core.Decimal":
		return map[string]any{
			"type":               "number",
			"x-protobuf-message": fullName,
		}
	case "google.fhir.r4.core.Date":
		return map[string]any{
			"type":               "string",
			"format":             "date",
			"x-protobuf-message": fullName,
		}
	case "google.fhir.r4.core.DateTime", "google.fhir.r4.core.Instant":
		return map[string]any{
			"type":               "string",
			"format":             "date-time",
			"x-protobuf-message": fullName,
		}
	case "google.fhir.r4.core.Time":
		return map[string]any{
			"type":               "string",
			"format":             "time",
			"x-protobuf-message": fullName,
		}
	case "google.fhir.r4.core.Uri", "google.fhir.r4.core.Url", "google.fhir.r4.core.Canonical", "google.fhir.r4.core.Oid", "google.fhir.r4.core.Uuid":
		return map[string]any{
			"type":               "string",
			"format":             "uri",
			"x-protobuf-message": fullName,
		}
	case "google.fhir.r4.core.String", "google.fhir.r4.core.Id", "google.fhir.r4.core.Code", "google.fhir.r4.core.Markdown", "google.fhir.r4.core.Xhtml", "google.fhir.r4.core.Base64Binary":
		return map[string]any{
			"type":               "string",
			"x-protobuf-message": fullName,
		}
	case "google.fhir.r4.core.Reference":
		return compileReferenceProtoSchema(msgDesc)
	}

	// Check if this message is a google/fhir typed Code/Enum wrapper (has field "value" of EnumKind)
	if valFD := msgDesc.Fields().ByName("value"); valFD != nil {
		if valFD.Kind() == protoreflect.EnumKind {
			enumDesc := valFD.Enum()
			enumVals := enumDesc.Values()
			codes := make([]string, 0, enumVals.Len())
			for i := 0; i < enumVals.Len(); i++ {
				ev := enumVals.Get(i)
				name := string(ev.Name())
				if ev.Number() == 0 || name == "INVALID_UNINITIALIZED" {
					continue
				}
				codes = append(codes, strings.ToLower(strings.ReplaceAll(name, "_", "-")))
			}
			sort.Strings(codes)
			return map[string]any{
				"type":               "string",
				"enum":               codes,
				"x-protobuf-message": fullName,
				"x-protobuf-enum":    string(enumDesc.FullName()),
			}
		}
		if valFD.Kind() == protoreflect.StringKind {
			return map[string]any{
				"type":               "string",
				"x-protobuf-message": fullName,
			}
		}
	}

	// Composite FHIR complex type or BackboneElement
	out := map[string]any{
		"type":               "object",
		"x-protobuf-message": fullName,
	}
	if depth <= 2 {
		nestedProps := make(map[string]any)
		compileProtoFieldsIntoProperties(msgDesc, nestedProps, depth)
		if len(nestedProps) > 0 {
			out["properties"] = nestedProps
		}
	}
	return out
}

func compileReferenceProtoSchema(md protoreflect.MessageDescriptor) map[string]any {
	var targetTypes []string
	if refOneof := md.Oneofs().ByName("reference"); refOneof != nil {
		fields := refOneof.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			jsonName := fd.JSONName()
			if strings.HasSuffix(jsonName, "Id") && len(jsonName) > 2 {
				targetType := strings.ToUpper(jsonName[:1]) + jsonName[1:len(jsonName)-2]
				targetTypes = append(targetTypes, targetType)
			}
		}
		sort.Strings(targetTypes)
	}

	out := map[string]any{
		"type":               "object",
		"x-protobuf-message": string(md.FullName()),
		"properties": map[string]any{
			"reference":  map[string]any{"type": "string", "description": "Literal reference (e.g. Patient/pat-001)"},
			"type":       map[string]any{"type": "string", "description": "Target resource type"},
			"display":    map[string]any{"type": "string", "description": "Text alternative for the resource"},
			"identifier": map[string]any{"type": "object", "description": "Logical reference identifier"},
		},
	}
	if len(targetTypes) > 0 {
		out["x-fhir-target-types-count"] = len(targetTypes)
	}
	return out
}
