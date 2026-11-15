// Package fhirxml implements a pure-Go bidirectional codec between canonical
// HL7 FHIR R4 JSON/maps and HL7 FHIR R4 XML (application/fhir+xml), driven by
// google/fhir R4 protobuf descriptors across all ~146 official resource types.
package fhirxml

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/flint-fhir/flint/pkg/fhirutil"
)

const (
	// FHIRNamespace is the canonical HL7 FHIR XML namespace URI.
	FHIRNamespace = "http://hl7.org/fhir"
	// XHTMLNamespace is the canonical W3C XHTML namespace URI for Narrative.div.
	XHTMLNamespace = "http://www.w3.org/1999/xhtml"
	// xmlSchemaInstanceNamespace is the W3C XML Schema instance namespace (xsi:schemaLocation).
	xmlSchemaInstanceNamespace = "http://www.w3.org/2001/XMLSchema-instance"

	maxXMLDepth = 64
)

var (
	decimalSyntaxRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
	integerSyntaxRe = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)
	posIntSyntaxRe  = regexp.MustCompile(`^\+?[1-9][0-9]*$`)
	uIntSyntaxRe    = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

	cachedDescOnce sync.Once
	cachedExtDesc  protoreflect.MessageDescriptor
	cachedStrDesc  protoreflect.MessageDescriptor
)

func initCachedDescriptors() {
	cachedDescOnce.Do(func() {
		if patMD := fhirutil.AllR4ResourceDescriptors()["Patient"]; patMD != nil {
			if extFD := patMD.Fields().ByName("extension"); extFD != nil {
				cachedExtDesc = extFD.Message()
			}
			if idFD := patMD.Fields().ByName("id"); idFD != nil {
				if idExtFD := idFD.Message().Fields().ByName("id"); idExtFD != nil {
					cachedStrDesc = idExtFD.Message()
				}
			}
		}
	})
}

func extensionDescriptor() protoreflect.MessageDescriptor {
	initCachedDescriptors()
	return cachedExtDesc
}

func stringWrapperDescriptor() protoreflect.MessageDescriptor {
	initCachedDescriptors()
	return cachedStrDesc
}

// MarshalFHIRJSON transcodes canonical HL7 FHIR R4 JSON bytes into HL7 FHIR R4 XML bytes.
// Uses json.Decoder.UseNumber() so exact FHIR Decimal trailing zeroes (e.g. 98.60) are preserved.
func MarshalFHIRJSON(jsonBytes []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(jsonBytes)
	if len(trimmed) == 0 {
		return nil, errors.New("empty FHIR JSON payload")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("invalid FHIR JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("unexpected trailing data after root FHIR JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("unexpected trailing token after root FHIR JSON object")
	}
	return MarshalFHIRMap(root)
}

// MarshalFHIRMap serializes an in-memory HL7 FHIR R4 resource map into HL7 FHIR R4 XML bytes.
func MarshalFHIRMap(resource map[string]any) ([]byte, error) {
	if len(resource) == 0 {
		return nil, errors.New("empty FHIR resource map")
	}
	resType, _ := resource["resourceType"].(string)
	resType = strings.TrimSpace(resType)
	if resType == "" {
		return nil, errors.New("missing required resourceType in FHIR resource")
	}
	if !isValidXMLIdent(resType) {
		return nil, fmt.Errorf("invalid FHIR resourceType %q", resType)
	}

	md := fhirutil.AllR4ResourceDescriptors()[resType]
	var buf bytes.Buffer
	buf.Grow(512)
	buf.WriteString(xml.Header)
	if err := writeResourceElement(&buf, resType, md, resource, true, 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalFHIRJSON transcodes HL7 FHIR R4 XML bytes into canonical HL7 FHIR R4 JSON bytes,
// preserving exact Decimal precision and rejecting DTD/DOCTYPE/ENTITY payloads.
func UnmarshalFHIRJSON(xmlBytes []byte) ([]byte, error) {
	m, err := unmarshalXMLInternal(xmlBytes, true)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("marshal FHIR JSON: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// UnmarshalFHIRMap decodes HL7 FHIR R4 XML bytes into an in-memory HL7 FHIR R4 resource map
// matching standard encoding/json Go types (float64 numbers, bool booleans, string strings).
func UnmarshalFHIRMap(xmlBytes []byte) (map[string]any, error) {
	return unmarshalXMLInternal(xmlBytes, false)
}

func writeResourceElement(buf *bytes.Buffer, resType string, md protoreflect.MessageDescriptor, m map[string]any, rootNS bool, depth int) error {
	if depth > maxXMLDepth {
		return fmt.Errorf("maximum FHIR XML nesting depth (%d) exceeded", maxXMLDepth)
	}
	if !isValidXMLIdent(resType) {
		return fmt.Errorf("invalid XML element name %q", resType)
	}

	var children bytes.Buffer
	if err := writeMessageChildren(&children, md, m, true, depth+1); err != nil {
		return err
	}

	buf.WriteByte('<')
	buf.WriteString(resType)
	if rootNS {
		buf.WriteString(` xmlns="`)
		buf.WriteString(FHIRNamespace)
		buf.WriteByte('"')
	}
	if children.Len() == 0 {
		buf.WriteString("/>")
		return nil
	}
	buf.WriteByte('>')
	buf.Write(children.Bytes())
	buf.WriteString("</")
	buf.WriteString(resType)
	buf.WriteByte('>')
	return nil
}

func writeMessageChildren(buf *bytes.Buffer, md protoreflect.MessageDescriptor, m map[string]any, isResource bool, depth int) error {
	if depth > maxXMLDepth {
		return fmt.Errorf("maximum FHIR XML nesting depth (%d) exceeded", maxXMLDepth)
	}
	if len(m) == 0 {
		return nil
	}

	emitted := map[string]bool{
		"resourceType": true,
	}
	if !isResource {
		emitted["id"] = true
		emitted["_id"] = true
	}
	if isExtensionMessage(md) {
		emitted["url"] = true
		emitted["_url"] = true
	}

	if md != nil && string(md.FullName()) == "google.fhir.r4.core.Reference" {
		refOrder := []struct {
			name   string
			subMD  protoreflect.MessageDescriptor
			isList bool
		}{
			{"extension", extensionDescriptor(), true},
			{"reference", stringWrapperDescriptor(), false},
			{"type", md.Fields().ByName("type").Message(), false},
			{"identifier", md.Fields().ByName("identifier").Message(), false},
			{"display", md.Fields().ByName("display").Message(), false},
		}
		for _, item := range refOrder {
			emitted[item.name] = true
			emitted["_"+item.name] = true
			val, hasVal := m[item.name]
			extVal, hasExt := m["_"+item.name]
			if (!hasVal || val == nil) && (!hasExt || extVal == nil) {
				continue
			}
			if err := writeFieldValue(buf, item.name, item.subMD, item.isList, val, extVal, depth); err != nil {
				return err
			}
		}
	} else if md != nil {
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			elemName := fhirElementName(fd)
			if emitted[elemName] {
				continue
			}

			if fd.Kind() == protoreflect.MessageKind && fd.Message().Oneofs().ByName("choice") != nil {
				choiceFields := fd.Message().Oneofs().ByName("choice").Fields()
				for j := 0; j < choiceFields.Len(); j++ {
					subFD := choiceFields.Get(j)
					flatKey := choiceJSONKey(fd, subFD)
					emitted[flatKey] = true
					emitted["_"+flatKey] = true
					val, hasVal := m[flatKey]
					extVal, hasExt := m["_"+flatKey]
					if (!hasVal || val == nil) && (!hasExt || extVal == nil) {
						continue
					}
					if err := writeFieldValue(buf, flatKey, subFD.Message(), false, val, extVal, depth); err != nil {
						return err
					}
				}
				continue
			}

			emitted[elemName] = true
			emitted["_"+elemName] = true
			val, hasVal := m[elemName]
			if !hasVal && elemName == "class" {
				val, hasVal = m["classValue"]
				emitted["classValue"] = true
			}
			extVal, hasExt := m["_"+elemName]
			if (!hasVal || val == nil) && (!hasExt || extVal == nil) {
				continue
			}

			var subMD protoreflect.MessageDescriptor
			if fd.Kind() == protoreflect.MessageKind {
				subMD = fd.Message()
			}
			if err := writeFieldValue(buf, elemName, subMD, fd.IsList(), val, extVal, depth); err != nil {
				return err
			}
		}
	}

	// Deterministic fallback for any custom/unknown elements or untyped maps.
	var extraKeys []string
	seenExtra := make(map[string]bool)
	for k, v := range m {
		if emitted[k] || v == nil {
			continue
		}
		baseKey := k
		if strings.HasPrefix(k, "_") && len(k) > 1 {
			baseKey = k[1:]
			if emitted[baseKey] {
				continue
			}
		}
		if !seenExtra[baseKey] {
			seenExtra[baseKey] = true
			extraKeys = append(extraKeys, baseKey)
		}
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		if err := writeUntypedFieldValue(buf, k, m[k], m["_"+k], depth); err != nil {
			return err
		}
	}

	return nil
}

func writeFieldValue(
	buf *bytes.Buffer,
	elemName string,
	msgDesc protoreflect.MessageDescriptor,
	isList bool,
	val any,
	extVal any,
	depth int,
) error {
	if !isValidXMLIdent(elemName) {
		return fmt.Errorf("invalid XML element name %q", elemName)
	}

	if msgDesc != nil {
		fullName := string(msgDesc.FullName())
		if fullName == "google.fhir.r4.core.Xhtml" {
			return writeXHTMLDiv(buf, val, extVal)
		}
		if fullName == "google.fhir.r4.core.ContainedResource" || fullName == "google.protobuf.Any" {
			if isList {
				items, ok := toAnySlice(val)
				if !ok {
					return nil
				}
				for _, item := range items {
					subMap, ok := toStringAnyMap(item)
					if !ok {
						continue
					}
					if err := writeInlineResourceWrapper(buf, elemName, subMap, depth); err != nil {
						return err
					}
				}
				return nil
			}
			subMap, ok := toStringAnyMap(val)
			if !ok {
				return nil
			}
			return writeInlineResourceWrapper(buf, elemName, subMap, depth)
		}
		if isPrimitiveWrapperDescriptor(msgDesc) {
			return writePrimitiveField(buf, elemName, isList, val, extVal, depth)
		}
	}

	if msgDesc == nil {
		return writeUntypedFieldValue(buf, elemName, val, extVal, depth)
	}

	if isList {
		items, ok := toAnySlice(val)
		if !ok {
			if singleMap, okMap := toStringAnyMap(val); okMap {
				return writeComplexElement(buf, elemName, msgDesc, singleMap, depth)
			}
			return nil
		}
		for _, item := range items {
			subMap, ok := toStringAnyMap(item)
			if !ok {
				continue
			}
			if err := writeComplexElement(buf, elemName, msgDesc, subMap, depth); err != nil {
				return err
			}
		}
		return nil
	}

	subMap, ok := toStringAnyMap(val)
	if !ok {
		return writePrimitiveField(buf, elemName, false, val, extVal, depth)
	}
	return writeComplexElement(buf, elemName, msgDesc, subMap, depth)
}

func writeUntypedFieldValue(buf *bytes.Buffer, elemName string, val any, extVal any, depth int) error {
	if !isValidXMLIdent(elemName) {
		return fmt.Errorf("invalid XML element name %q", elemName)
	}
	if elemName == "div" {
		if s, ok := val.(string); ok && strings.Contains(s, "<") {
			return writeXHTMLDiv(buf, val, extVal)
		}
	}
	if elemName == "contained" {
		if items, ok := toAnySlice(val); ok {
			for _, item := range items {
				if subMap, okMap := toStringAnyMap(item); okMap {
					if err := writeInlineResourceWrapper(buf, elemName, subMap, depth); err != nil {
						return err
					}
				}
			}
			return nil
		}
	}
	if elemName == "extension" || elemName == "modifierExtension" {
		return writeFieldValue(buf, elemName, extensionDescriptor(), true, val, extVal, depth)
	}
	if subMap, ok := toStringAnyMap(val); ok {
		if rt, _ := subMap["resourceType"].(string); rt != "" && (elemName == "resource" || elemName == "outcome") {
			return writeInlineResourceWrapper(buf, elemName, subMap, depth)
		}
		return writeComplexElement(buf, elemName, nil, subMap, depth)
	}
	if items, ok := toAnySlice(val); ok {
		hasMap := false
		for _, item := range items {
			if _, okMap := toStringAnyMap(item); okMap {
				hasMap = true
				break
			}
		}
		if hasMap {
			for _, item := range items {
				if subMap, okMap := toStringAnyMap(item); okMap {
					if err := writeComplexElement(buf, elemName, nil, subMap, depth); err != nil {
						return err
					}
				}
			}
			return nil
		}
		return writePrimitiveField(buf, elemName, true, val, extVal, depth)
	}
	return writePrimitiveField(buf, elemName, false, val, extVal, depth)
}

func writeInlineResourceWrapper(buf *bytes.Buffer, wrapperName string, subMap map[string]any, depth int) error {
	rt, _ := subMap["resourceType"].(string)
	rt = strings.TrimSpace(rt)
	if rt == "" {
		return fmt.Errorf("missing resourceType inside inline <%s> wrapper", wrapperName)
	}
	subMD := fhirutil.AllR4ResourceDescriptors()[rt]
	buf.WriteByte('<')
	buf.WriteString(wrapperName)
	buf.WriteByte('>')
	if err := writeResourceElement(buf, rt, subMD, subMap, false, depth+1); err != nil {
		return err
	}
	buf.WriteString("</")
	buf.WriteString(wrapperName)
	buf.WriteByte('>')
	return nil
}

func writeComplexElement(buf *bytes.Buffer, elemName string, msgDesc protoreflect.MessageDescriptor, subMap map[string]any, depth int) error {
	if len(subMap) == 0 {
		return nil
	}
	isExt := isExtensionMessage(msgDesc) || elemName == "extension" || elemName == "modifierExtension"
	idAttr, _ := subMap["id"].(string)
	var urlAttr string
	var hasURL bool
	if isExt {
		if u, ok := subMap["url"].(string); ok {
			urlAttr = u
			hasURL = true
		}
	}

	var children bytes.Buffer
	effectiveDesc := msgDesc
	if isExt && effectiveDesc == nil {
		effectiveDesc = extensionDescriptor()
	}
	if err := writeMessageChildren(&children, effectiveDesc, subMap, false, depth+1); err != nil {
		return err
	}
	if idAttr == "" && !hasURL && children.Len() == 0 {
		return nil
	}

	buf.WriteByte('<')
	buf.WriteString(elemName)
	if idAttr != "" {
		buf.WriteString(` id="`)
		writeEscapedAttr(buf, idAttr)
		buf.WriteByte('"')
	}
	if hasURL {
		buf.WriteString(` url="`)
		writeEscapedAttr(buf, urlAttr)
		buf.WriteByte('"')
	}
	if children.Len() == 0 {
		buf.WriteString("/>")
		return nil
	}
	buf.WriteByte('>')
	buf.Write(children.Bytes())
	buf.WriteString("</")
	buf.WriteString(elemName)
	buf.WriteByte('>')
	return nil
}

func writePrimitiveField(buf *bytes.Buffer, elemName string, isList bool, val any, extVal any, depth int) error {
	if isList {
		valSlice, _ := toAnySlice(val)
		extSlice, _ := toAnySlice(extVal)
		n := max(len(valSlice), len(extSlice))
		for i := 0; i < n; i++ {
			var vItem, eItem any
			if i < len(valSlice) {
				vItem = valSlice[i]
			}
			if i < len(extSlice) {
				eItem = extSlice[i]
			}
			if err := writeSinglePrimitive(buf, elemName, vItem, eItem, depth); err != nil {
				return err
			}
		}
		return nil
	}
	return writeSinglePrimitive(buf, elemName, val, extVal, depth)
}

func writeSinglePrimitive(buf *bytes.Buffer, elemName string, val any, extVal any, depth int) error {
	valStr, hasVal := formatPrimitiveString(val)
	var idAttr string
	var exts []any
	if extMap, ok := toStringAnyMap(extVal); ok {
		idAttr, _ = extMap["id"].(string)
		exts, _ = toAnySlice(extMap["extension"])
	}
	if !hasVal && idAttr == "" && len(exts) == 0 {
		return nil
	}

	buf.WriteByte('<')
	buf.WriteString(elemName)
	if idAttr != "" {
		buf.WriteString(` id="`)
		writeEscapedAttr(buf, idAttr)
		buf.WriteByte('"')
	}
	if hasVal {
		buf.WriteString(` value="`)
		writeEscapedAttr(buf, valStr)
		buf.WriteByte('"')
	}
	if len(exts) == 0 {
		buf.WriteString("/>")
		return nil
	}
	buf.WriteByte('>')
	extDesc := extensionDescriptor()
	for _, rawExt := range exts {
		if em, ok := toStringAnyMap(rawExt); ok {
			if err := writeComplexElement(buf, "extension", extDesc, em, depth+1); err != nil {
				return err
			}
		}
	}
	buf.WriteString("</")
	buf.WriteString(elemName)
	buf.WriteByte('>')
	return nil
}

func writeXHTMLDiv(buf *bytes.Buffer, val any, extVal any) error {
	rawDiv, _ := val.(string)
	rawDiv = strings.TrimSpace(rawDiv)
	var idAttr string
	if extMap, ok := toStringAnyMap(extVal); ok {
		idAttr, _ = extMap["id"].(string)
	}
	if rawDiv == "" {
		if idAttr == "" {
			return nil
		}
		buf.WriteString(`<div xmlns="`)
		buf.WriteString(XHTMLNamespace)
		buf.WriteString(`" id="`)
		writeEscapedAttr(buf, idAttr)
		buf.WriteString(`"/>`)
		return nil
	}

	// Parse rawDiv through a strict XML decoder so malformed XHTML or DTDs cannot break output XML.
	dec := xml.NewDecoder(strings.NewReader(rawDiv))
	dec.Strict = true
	tok, err := nextNonTriviaToken(dec)
	if err == nil {
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "div" {
			if idAttr != "" && getAttrValue(start.Attr, "id") == "" {
				start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "id"}, Value: idAttr})
			}
			rendered, rErr := readRawXHTMLDiv(dec, start, 1)
			if rErr == nil {
				// Ensure no trailing unparsed tokens follow </div>.
				if _, tailErr := nextNonTriviaToken(dec); errors.Is(tailErr, io.EOF) {
					buf.WriteString(rendered)
					return nil
				}
			}
		}
	}

	// Fallback: wrap plain text safely inside <div xmlns="http://www.w3.org/1999/xhtml">...</div>.
	buf.WriteString(`<div xmlns="`)
	buf.WriteString(XHTMLNamespace)
	buf.WriteByte('"')
	if idAttr != "" {
		buf.WriteString(` id="`)
		writeEscapedAttr(buf, idAttr)
		buf.WriteByte('"')
	}
	buf.WriteByte('>')
	_ = xml.EscapeText(buf, []byte(rawDiv))
	buf.WriteString("</div>")
	return nil
}

// =============================================================================
// XML -> FHIR JSON / Map Unmarshaler
// =============================================================================

func unmarshalXMLInternal(xmlBytes []byte, useJSONNumber bool) (map[string]any, error) {
	trimmed := bytes.TrimPrefix(xmlBytes, []byte("\xef\xbb\xbf"))
	trimmed = bytes.TrimSpace(trimmed)
	if len(trimmed) == 0 {
		return nil, errors.New("empty FHIR XML payload")
	}

	dec := xml.NewDecoder(bytes.NewReader(trimmed))
	dec.Strict = true

	tok, err := nextRootToken(dec)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty FHIR XML document")
		}
		return nil, err
	}
	start, ok := tok.(xml.StartElement)
	if !ok {
		return nil, errors.New("expected root XML element")
	}

	if start.Name.Space != "" && start.Name.Space != FHIRNamespace {
		return nil, fmt.Errorf("invalid root XML namespace %q; expected %q", start.Name.Space, FHIRNamespace)
	}
	resType := start.Name.Local
	if !isValidXMLIdent(resType) {
		return nil, fmt.Errorf("invalid root FHIR resourceType %q", resType)
	}
	if err := validateResourceRootAttrs(start.Attr); err != nil {
		return nil, err
	}

	md := fhirutil.AllR4ResourceDescriptors()[resType]
	rootMap, err := decodeMessageChildren(dec, md, true, 1, useJSONNumber)
	if err != nil {
		return nil, err
	}
	rootMap["resourceType"] = resType

	// Verify no trailing root elements or directives after root closing tag.
	for {
		tailTok, tailErr := dec.Token()
		if errors.Is(tailErr, io.EOF) {
			break
		}
		if tailErr != nil {
			return nil, fmt.Errorf("invalid trailing XML content: %w", tailErr)
		}
		switch t := tailTok.(type) {
		case xml.Directive:
			return nil, errors.New("XML DTD/DOCTYPE/ENTITY directives are forbidden")
		case xml.StartElement:
			return nil, fmt.Errorf("unexpected second root XML element <%s>", t.Name.Local)
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return nil, errors.New("unexpected trailing character data after root XML element")
			}
		}
	}

	return rootMap, nil
}

func nextRootToken(dec *xml.Decoder) (xml.Token, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("malformed XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.Directive:
			return nil, errors.New("XML DTD/DOCTYPE/ENTITY directives are forbidden")
		case xml.Comment, xml.ProcInst:
			continue
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return nil, errors.New("unexpected character data before root XML element")
			}
		case xml.StartElement:
			return xml.CopyToken(t), nil
		default:
			return nil, errors.New("unexpected XML token before root element")
		}
	}
}

func nextNonTriviaToken(dec *xml.Decoder) (xml.Token, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.Directive:
			return nil, errors.New("XML DTD/DOCTYPE/ENTITY directives are forbidden")
		case xml.Comment, xml.ProcInst:
			continue
		case xml.CharData:
			if strings.TrimSpace(string(t)) == "" {
				continue
			}
			return xml.CopyToken(t), nil
		default:
			return xml.CopyToken(t), nil
		}
	}
}

type resolvedXMLField struct {
	jsonKey          string
	choiceGroup      string
	subMD            protoreflect.MessageDescriptor
	isList           bool
	isInlineResource bool
	isPrimitive      bool
	known            bool
}

func resolveXMLChildField(md protoreflect.MessageDescriptor, childName string) resolvedXMLField {
	if md == nil {
		return resolvedXMLField{jsonKey: childName}
	}

	if string(md.FullName()) == "google.fhir.r4.core.Reference" && childName == "reference" {
		return resolvedXMLField{
			jsonKey:     "reference",
			subMD:       stringWrapperDescriptor(),
			isPrimitive: true,
			known:       true,
		}
	}

	fields := md.Fields()
	if fd := findXMLFieldDescriptor(fields, childName); fd != nil {
		if fd.Kind() == protoreflect.MessageKind && fd.Message().Oneofs().ByName("choice") != nil {
			// A bare choice base name (e.g. <value> instead of <valueString>) is not a direct FHIR element.
			return resolvedXMLField{jsonKey: childName}
		}
		var subMD protoreflect.MessageDescriptor
		var isInline, isPrim bool
		if fd.Kind() == protoreflect.MessageKind {
			subMD = fd.Message()
			fullName := string(subMD.FullName())
			isInline = fullName == "google.fhir.r4.core.ContainedResource" || fullName == "google.protobuf.Any"
			isPrim = isPrimitiveWrapperDescriptor(subMD)
		}
		return resolvedXMLField{
			jsonKey:          fhirElementName(fd),
			subMD:            subMD,
			isList:           fd.IsList(),
			isInlineResource: isInline,
			isPrimitive:      isPrim,
			known:            true,
		}
	}

	// Check FHIR choice[x] fields on md.
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Kind() != protoreflect.MessageKind || fd.Message().Oneofs().ByName("choice") == nil {
			continue
		}
		prefix := fd.JSONName()
		if !strings.HasPrefix(childName, prefix) || len(childName) <= len(prefix) {
			continue
		}
		suffix := childName[len(prefix):]
		if suffix[0] < 'A' || suffix[0] > 'Z' {
			continue
		}
		choiceFieldJSON := strings.ToLower(suffix[:1]) + suffix[1:]
		choiceFields := fd.Message().Oneofs().ByName("choice").Fields()
		subFD := findXMLFieldDescriptor(choiceFields, choiceFieldJSON)
		if subFD == nil && choiceFieldJSON == "string" {
			subFD = findXMLFieldDescriptor(choiceFields, "stringValue")
		}
		if subFD != nil && subFD.Kind() == protoreflect.MessageKind {
			subMD := subFD.Message()
			return resolvedXMLField{
				jsonKey:     childName,
				choiceGroup: prefix,
				subMD:       subMD,
				isList:      false,
				isPrimitive: isPrimitiveWrapperDescriptor(subMD),
				known:       true,
			}
		}
	}

	return resolvedXMLField{jsonKey: childName}
}

func decodeMessageChildren(
	dec *xml.Decoder,
	md protoreflect.MessageDescriptor,
	isResource bool,
	depth int,
	useJSONNumber bool,
) (map[string]any, error) {
	if depth > maxXMLDepth {
		return nil, fmt.Errorf("maximum FHIR XML nesting depth (%d) exceeded", maxXMLDepth)
	}

	out := make(map[string]any)
	var primListKeys []string
	primListVals := make(map[string][]any)
	primListExts := make(map[string][]any)
	var untypedPrimKeys []string
	untypedPrimVals := make(map[string][]any)
	untypedPrimExts := make(map[string][]any)
	untypedKinds := make(map[string]string)
	seenScalars := make(map[string]bool)
	seenChoices := make(map[string]string)

	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("malformed XML: %w", err)
		}

		switch t := tok.(type) {
		case xml.Directive:
			return nil, errors.New("XML DTD/DOCTYPE/ENTITY directives are forbidden")
		case xml.Comment, xml.ProcInst:
			continue
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return nil, errors.New("unexpected character data inside FHIR XML element; primitive values must use value=\"...\" attribute")
			}
		case xml.EndElement:
			for _, k := range primListKeys {
				vals := primListVals[k]
				exts := primListExts[k]
				hasAnyVal := false
				for _, v := range vals {
					if v != nil {
						hasAnyVal = true
						break
					}
				}
				hasAnyExt := false
				for _, e := range exts {
					if e != nil {
						hasAnyExt = true
						break
					}
				}
				if hasAnyVal {
					out[k] = vals
				}
				if hasAnyExt {
					out["_"+k] = exts
				}
			}
			for _, k := range untypedPrimKeys {
				vals := untypedPrimVals[k]
				exts := untypedPrimExts[k]
				if len(vals) == 1 {
					if vals[0] != nil {
						out[k] = vals[0]
					}
					if exts[0] != nil {
						out["_"+k] = exts[0]
					}
					continue
				}
				hasAnyVal := false
				for _, v := range vals {
					if v != nil {
						hasAnyVal = true
						break
					}
				}
				hasAnyExt := false
				for _, e := range exts {
					if e != nil {
						hasAnyExt = true
						break
					}
				}
				if hasAnyVal {
					out[k] = vals
				}
				if hasAnyExt {
					out["_"+k] = exts
				}
			}
			return out, nil

		case xml.StartElement:
			childName := t.Name.Local
			if !isValidXMLIdent(childName) {
				return nil, fmt.Errorf("invalid XML element name %q", childName)
			}

			// Handle Narrative.div inline XHTML (<div xmlns="http://www.w3.org/1999/xhtml">...</div>).
			isNarrativeDiv := childName == "div" &&
				((md != nil && string(md.FullName()) == "google.fhir.r4.core.Narrative") ||
					(md == nil && t.Name.Space == XHTMLNamespace))
			if isNarrativeDiv {
				if hasAttr(t.Attr, "value") {
					return nil, errors.New("unexpected value attribute on Narrative <div>")
				}
				if t.Name.Space != "" && t.Name.Space != XHTMLNamespace && t.Name.Space != FHIRNamespace {
					return nil, fmt.Errorf("invalid namespace %q on Narrative <div>", t.Name.Space)
				}
				if seenScalars["div"] {
					return nil, errors.New("duplicate FHIR XML element <div>")
				}
				seenScalars["div"] = true
				if hasAttr(t.Attr, "id") && getAttrValue(t.Attr, "id") == "" {
					return nil, errors.New("empty id attribute on Narrative <div>")
				}
				idAttr := getAttrValue(t.Attr, "id")
				divHTML, err := readRawXHTMLDiv(dec, t, depth+1)
				if err != nil {
					return nil, err
				}
				out["div"] = divHTML
				if idAttr != "" {
					out["_div"] = map[string]any{"id": idAttr}
				}
				continue
			}

			if t.Name.Space != "" && t.Name.Space != FHIRNamespace {
				return nil, fmt.Errorf("invalid XML namespace %q on element <%s>", t.Name.Space, childName)
			}

			rf := resolveXMLChildField(md, childName)
			if rf.choiceGroup != "" {
				if prev, dup := seenChoices[rf.choiceGroup]; dup {
					return nil, fmt.Errorf("conflicting choice[x] elements <%s> and <%s>", prev, childName)
				}
				seenChoices[rf.choiceGroup] = childName
			}

			if rf.known {
				if !rf.isList {
					if seenScalars[rf.jsonKey] {
						return nil, fmt.Errorf("duplicate non-repeating FHIR XML element <%s>", childName)
					}
					seenScalars[rf.jsonKey] = true
				}

				if rf.isInlineResource {
					inlineRes, err := decodeInlineResourceWrapper(dec, childName, t.Attr, depth+1, useJSONNumber)
					if err != nil {
						return nil, err
					}
					if rf.isList {
						arr, _ := out[rf.jsonKey].([]any)
						out[rf.jsonKey] = append(arr, inlineRes)
					} else {
						out[rf.jsonKey] = inlineRes
					}
					continue
				}

				if rf.isPrimitive {
					pVal, pExt, hasVal, err := decodePrimitiveElement(dec, childName, t.Attr, rf.subMD, depth+1, useJSONNumber)
					if err != nil {
						return nil, err
					}
					if rf.isList {
						if _, seen := primListVals[rf.jsonKey]; !seen {
							primListKeys = append(primListKeys, rf.jsonKey)
						}
						if hasVal {
							primListVals[rf.jsonKey] = append(primListVals[rf.jsonKey], pVal)
						} else {
							primListVals[rf.jsonKey] = append(primListVals[rf.jsonKey], nil)
						}
						if len(pExt) > 0 {
							primListExts[rf.jsonKey] = append(primListExts[rf.jsonKey], pExt)
						} else {
							primListExts[rf.jsonKey] = append(primListExts[rf.jsonKey], nil)
						}
					} else {
						if hasVal {
							out[rf.jsonKey] = pVal
						}
						if len(pExt) > 0 {
							out["_"+rf.jsonKey] = pExt
						}
					}
					continue
				}

				// Known complex / BackboneElement / Extension child
				cMap, err := decodeComplexElement(dec, childName, t.Attr, rf.subMD, depth+1, useJSONNumber)
				if err != nil {
					return nil, err
				}
				if rf.isList {
					arr, _ := out[rf.jsonKey].([]any)
					out[rf.jsonKey] = append(arr, cMap)
				} else {
					out[rf.jsonKey] = cMap
				}
				continue
			}

			// Unknown or untyped element fallback (preserves unknown fields so $validate reports exact FHIRPath errors).
			if err := decodeUntypedChildElement(dec, childName, t.Attr, out, &untypedPrimKeys, untypedPrimVals, untypedPrimExts, untypedKinds, depth+1, useJSONNumber); err != nil {
				return nil, err
			}
		}
	}
}

func decodeInlineResourceWrapper(dec *xml.Decoder, wrapperName string, attrs []xml.Attr, depth int, useJSONNumber bool) (map[string]any, error) {
	if err := validateResourceRootAttrs(attrs); err != nil {
		return nil, fmt.Errorf("invalid attribute on <%s>: %w", wrapperName, err)
	}
	var resMap map[string]any
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("malformed XML inside <%s>: %w", wrapperName, err)
		}
		switch t := tok.(type) {
		case xml.Directive:
			return nil, errors.New("XML DTD/DOCTYPE/ENTITY directives are forbidden")
		case xml.Comment, xml.ProcInst:
			continue
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return nil, fmt.Errorf("unexpected character data inside <%s>", wrapperName)
			}
		case xml.StartElement:
			if resMap != nil {
				return nil, fmt.Errorf("multiple inline resources inside <%s>", wrapperName)
			}
			if t.Name.Space != "" && t.Name.Space != FHIRNamespace {
				return nil, fmt.Errorf("invalid namespace %q on inline resource <%s>", t.Name.Space, t.Name.Local)
			}
			rt := t.Name.Local
			if !isValidXMLIdent(rt) {
				return nil, fmt.Errorf("invalid inline resourceType %q", rt)
			}
			if err := validateResourceRootAttrs(t.Attr); err != nil {
				return nil, err
			}
			subMD := fhirutil.AllR4ResourceDescriptors()[rt]
			m, err := decodeMessageChildren(dec, subMD, true, depth+1, useJSONNumber)
			if err != nil {
				return nil, err
			}
			m["resourceType"] = rt
			resMap = m
		case xml.EndElement:
			if resMap == nil {
				return nil, fmt.Errorf("empty inline resource wrapper <%s>", wrapperName)
			}
			return resMap, nil
		}
	}
}

func decodePrimitiveElement(
	dec *xml.Decoder,
	elemName string,
	attrs []xml.Attr,
	primMD protoreflect.MessageDescriptor,
	depth int,
	useJSONNumber bool,
) (any, map[string]any, bool, error) {
	var rawVal string
	var hasVal bool
	var idAttr string

	for _, a := range attrs {
		if isIgnoredXMLNSAttr(a) {
			continue
		}
		if a.Name.Space != "" {
			return nil, nil, false, fmt.Errorf("unexpected namespaced attribute %s:%s on <%s>", a.Name.Space, a.Name.Local, elemName)
		}
		switch a.Name.Local {
		case "value":
			if a.Value == "" {
				return nil, nil, false, fmt.Errorf("empty value attribute on primitive element <%s>", elemName)
			}
			rawVal = a.Value
			hasVal = true
		case "id":
			if a.Value == "" {
				return nil, nil, false, fmt.Errorf("empty id attribute on primitive element <%s>", elemName)
			}
			idAttr = a.Value
		default:
			return nil, nil, false, fmt.Errorf("unexpected XML attribute %q on primitive element <%s>", a.Name.Local, elemName)
		}
	}

	var exts []any
	extDesc := extensionDescriptor()
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, false, fmt.Errorf("malformed XML inside <%s>: %w", elemName, err)
		}
		switch t := tok.(type) {
		case xml.Directive:
			return nil, nil, false, errors.New("XML DTD/DOCTYPE/ENTITY directives are forbidden")
		case xml.Comment, xml.ProcInst:
			continue
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return nil, nil, false, fmt.Errorf("unexpected character data inside primitive <%s>; use value=\"...\" attribute", elemName)
			}
		case xml.StartElement:
			if t.Name.Local != "extension" || (t.Name.Space != "" && t.Name.Space != FHIRNamespace) {
				return nil, nil, false, fmt.Errorf("unexpected child element <%s> inside primitive <%s>", t.Name.Local, elemName)
			}
			em, err := decodeComplexElement(dec, "extension", t.Attr, extDesc, depth+1, useJSONNumber)
			if err != nil {
				return nil, nil, false, err
			}
			exts = append(exts, em)
		case xml.EndElement:
			if !hasVal && idAttr == "" && len(exts) == 0 {
				return nil, nil, false, fmt.Errorf("empty FHIR XML primitive element <%s> must have value, id, or extension", elemName)
			}
			var typedVal any
			if hasVal {
				var convErr error
				typedVal, convErr = parseTypedPrimitiveValue(elemName, rawVal, primMD, useJSONNumber)
				if convErr != nil {
					return nil, nil, false, convErr
				}
			}
			var extOut map[string]any
			if idAttr != "" || len(exts) > 0 {
				extOut = make(map[string]any)
				if idAttr != "" {
					extOut["id"] = idAttr
				}
				if len(exts) > 0 {
					extOut["extension"] = exts
				}
			}
			return typedVal, extOut, hasVal, nil
		}
	}
}

func parseTypedPrimitiveValue(elemName, raw string, primMD protoreflect.MessageDescriptor, useJSONNumber bool) (any, error) {
	if primMD == nil {
		return raw, nil
	}
	switch string(primMD.FullName()) {
	case "google.fhir.r4.core.Boolean":
		switch raw {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return nil, fmt.Errorf("invalid FHIR boolean value %q on <%s>", raw, elemName)
		}

	case "google.fhir.r4.core.Integer":
		if !integerSyntaxRe.MatchString(raw) {
			return nil, fmt.Errorf("invalid FHIR integer value %q on <%s>", raw, elemName)
		}
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("FHIR integer overflow %q on <%s>: %w", raw, elemName, err)
		}
		if useJSONNumber {
			return json.Number(raw), nil
		}
		return float64(n), nil

	case "google.fhir.r4.core.PositiveInt":
		if !posIntSyntaxRe.MatchString(raw) {
			return nil, fmt.Errorf("invalid FHIR positiveInt value %q on <%s>", raw, elemName)
		}
		cleaned := strings.TrimPrefix(raw, "+")
		n, err := strconv.ParseInt(cleaned, 10, 32)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("FHIR positiveInt out of range %q on <%s>", raw, elemName)
		}
		if useJSONNumber {
			return json.Number(cleaned), nil
		}
		return float64(n), nil

	case "google.fhir.r4.core.UnsignedInt":
		if !uIntSyntaxRe.MatchString(raw) {
			return nil, fmt.Errorf("invalid FHIR unsignedInt value %q on <%s>", raw, elemName)
		}
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("FHIR unsignedInt out of range %q on <%s>", raw, elemName)
		}
		if useJSONNumber {
			return json.Number(raw), nil
		}
		return float64(n), nil

	case "google.fhir.r4.core.Decimal":
		if !decimalSyntaxRe.MatchString(raw) {
			return nil, fmt.Errorf("invalid FHIR decimal value %q on <%s>", raw, elemName)
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("FHIR decimal out of range %q on <%s>: %w", raw, elemName, err)
		}
		if useJSONNumber {
			return json.Number(raw), nil
		}
		return f, nil

	default:
		return raw, nil
	}
}

func decodeComplexElement(
	dec *xml.Decoder,
	elemName string,
	attrs []xml.Attr,
	subMD protoreflect.MessageDescriptor,
	depth int,
	useJSONNumber bool,
) (map[string]any, error) {
	isExt := isExtensionMessage(subMD) || elemName == "extension" || elemName == "modifierExtension"
	var idAttr, urlAttr string
	var hasURL bool

	for _, a := range attrs {
		if isIgnoredXMLNSAttr(a) {
			continue
		}
		if a.Name.Space != "" {
			return nil, fmt.Errorf("unexpected namespaced attribute %s:%s on <%s>", a.Name.Space, a.Name.Local, elemName)
		}
		switch a.Name.Local {
		case "id":
			if a.Value == "" {
				return nil, fmt.Errorf("empty id attribute on element <%s>", elemName)
			}
			idAttr = a.Value
		case "url":
			if !isExt {
				return nil, fmt.Errorf("unexpected url attribute on non-extension element <%s>", elemName)
			}
			if a.Value == "" {
				return nil, fmt.Errorf("empty url attribute on extension element <%s>", elemName)
			}
			urlAttr = a.Value
			hasURL = true
		default:
			return nil, fmt.Errorf("unexpected XML attribute %q on element <%s>", a.Name.Local, elemName)
		}
	}

	effectiveMD := subMD
	if isExt && effectiveMD == nil {
		effectiveMD = extensionDescriptor()
	}

	m, err := decodeMessageChildren(dec, effectiveMD, false, depth, useJSONNumber)
	if err != nil {
		return nil, err
	}
	if idAttr != "" {
		m["id"] = idAttr
	}
	if hasURL {
		m["url"] = urlAttr
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("empty FHIR XML element <%s> must have value, id, url, or child elements", elemName)
	}
	return m, nil
}

func decodeUntypedChildElement(
	dec *xml.Decoder,
	elemName string,
	attrs []xml.Attr,
	parent map[string]any,
	untypedPrimKeys *[]string,
	untypedPrimVals map[string][]any,
	untypedPrimExts map[string][]any,
	untypedKinds map[string]string,
	depth int,
	useJSONNumber bool,
) error {
	if elemName == "contained" {
		inlineRes, err := decodeInlineResourceWrapper(dec, elemName, attrs, depth, useJSONNumber)
		if err != nil {
			return err
		}
		arr, _ := parent[elemName].([]any)
		parent[elemName] = append(arr, inlineRes)
		return nil
	}
	if elemName == "extension" || elemName == "modifierExtension" {
		em, err := decodeComplexElement(dec, elemName, attrs, extensionDescriptor(), depth, useJSONNumber)
		if err != nil {
			return err
		}
		arr, _ := parent[elemName].([]any)
		parent[elemName] = append(arr, em)
		return nil
	}

	if hasAttr(attrs, "value") {
		if prev := untypedKinds[elemName]; prev != "" && prev != "primitive" {
			return fmt.Errorf("conflicting primitive and complex XML representations for <%s>", elemName)
		}
		untypedKinds[elemName] = "primitive"
		val, extMap, hasVal, err := decodePrimitiveElement(dec, elemName, attrs, nil, depth, useJSONNumber)
		if err != nil {
			return err
		}
		if _, seen := untypedPrimVals[elemName]; !seen {
			*untypedPrimKeys = append(*untypedPrimKeys, elemName)
		}
		if hasVal {
			untypedPrimVals[elemName] = append(untypedPrimVals[elemName], val)
		} else {
			untypedPrimVals[elemName] = append(untypedPrimVals[elemName], nil)
		}
		if len(extMap) > 0 {
			untypedPrimExts[elemName] = append(untypedPrimExts[elemName], extMap)
		} else {
			untypedPrimExts[elemName] = append(untypedPrimExts[elemName], nil)
		}
		return nil
	}

	if prev := untypedKinds[elemName]; prev != "" && prev != "complex" {
		return fmt.Errorf("conflicting primitive and complex XML representations for <%s>", elemName)
	}
	untypedKinds[elemName] = "complex"

	if (elemName == "resource" || elemName == "outcome") && !hasAttr(attrs, "id") {
		inlineRes, err := decodeInlineResourceWrapper(dec, elemName, attrs, depth, useJSONNumber)
		if err != nil {
			return err
		}
		if _, exists := parent[elemName]; exists {
			return fmt.Errorf("duplicate inline resource element <%s>", elemName)
		}
		parent[elemName] = inlineRes
		return nil
	}

	m, err := decodeComplexElement(dec, elemName, attrs, nil, depth, useJSONNumber)
	if err != nil {
		return err
	}
	appendOrSetUntyped(parent, elemName, m)
	return nil
}

func appendOrSetUntyped(parent map[string]any, key string, val any) {
	existing, exists := parent[key]
	if !exists {
		parent[key] = val
		return
	}
	if arr, ok := existing.([]any); ok {
		parent[key] = append(arr, val)
		return
	}
	parent[key] = []any{existing, val}
}

func readRawXHTMLDiv(dec *xml.Decoder, rootStart xml.StartElement, depth int) (string, error) {
	if depth > maxXMLDepth {
		return "", fmt.Errorf("maximum XHTML nesting depth (%d) exceeded", maxXMLDepth)
	}
	var buf bytes.Buffer
	writeXHTMLStartTag(&buf, rootStart, true)

	curDepth := 1
	for curDepth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("malformed XHTML inside Narrative <div>: %w", err)
		}
		switch t := tok.(type) {
		case xml.Directive:
			return "", errors.New("XML DTD/DOCTYPE/ENTITY directives inside Narrative <div> are forbidden")
		case xml.Comment, xml.ProcInst:
			continue
		case xml.CharData:
			_ = xml.EscapeText(&buf, t)
		case xml.StartElement:
			curDepth++
			if depth+curDepth > maxXMLDepth {
				return "", fmt.Errorf("maximum XHTML nesting depth (%d) exceeded", maxXMLDepth)
			}
			if !isValidXMLIdent(t.Name.Local) {
				return "", fmt.Errorf("invalid XHTML tag <%s>", t.Name.Local)
			}
			writeXHTMLStartTag(&buf, t, false)
		case xml.EndElement:
			buf.WriteString("</")
			buf.WriteString(t.Name.Local)
			buf.WriteByte('>')
			curDepth--
		}
	}
	return buf.String(), nil
}

func writeXHTMLStartTag(buf *bytes.Buffer, start xml.StartElement, isRootDiv bool) {
	buf.WriteByte('<')
	buf.WriteString(start.Name.Local)
	if isRootDiv {
		buf.WriteString(` xmlns="`)
		buf.WriteString(XHTMLNamespace)
		buf.WriteByte('"')
	}
	for _, a := range start.Attr {
		if a.Name.Local == "xmlns" || a.Name.Space == "xmlns" {
			continue
		}
		if isRootDiv && a.Name.Space == xmlSchemaInstanceNamespace {
			continue
		}
		if !isValidXMLIdent(a.Name.Local) {
			continue
		}
		buf.WriteByte(' ')
		buf.WriteString(a.Name.Local)
		buf.WriteString(`="`)
		writeEscapedAttr(buf, a.Value)
		buf.WriteByte('"')
	}
	buf.WriteByte('>')
}

// =============================================================================
// Descriptor & Type Helpers
// =============================================================================

func fhirElementName(fd protoreflect.FieldDescriptor) string {
	if string(fd.Name()) == "class_value" {
		return "class"
	}
	return fd.JSONName()
}

func choiceJSONKey(choiceFD, subFD protoreflect.FieldDescriptor) string {
	suffix := subFD.JSONName()
	if suffix == "stringValue" {
		suffix = "string"
	}
	return choiceFD.JSONName() + strings.ToUpper(suffix[:1]) + suffix[1:]
}

func findXMLFieldDescriptor(fields protoreflect.FieldDescriptors, name string) protoreflect.FieldDescriptor {
	if fd := fields.ByJSONName(name); fd != nil {
		return fd
	}
	if fd := fields.ByName(protoreflect.Name(name)); fd != nil {
		return fd
	}
	if fd := fields.ByJSONName(name + "Value"); fd != nil {
		return fd
	}
	if fd := fields.ByName(protoreflect.Name(name + "_value")); fd != nil {
		return fd
	}
	return nil
}

func isExtensionMessage(md protoreflect.MessageDescriptor) bool {
	return md != nil && string(md.FullName()) == "google.fhir.r4.core.Extension"
}

func isPrimitiveWrapperDescriptor(md protoreflect.MessageDescriptor) bool {
	if md == nil {
		return false
	}
	fullName := string(md.FullName())
	if !strings.HasPrefix(fullName, "google.fhir.r4.core.") || fullName == "google.fhir.r4.core.Xhtml" {
		return false
	}
	switch fullName {
	case "google.fhir.r4.core.Date", "google.fhir.r4.core.DateTime", "google.fhir.r4.core.Instant":
		return true
	}
	valFD := md.Fields().ByName("value")
	if valFD == nil || valFD.Kind() == protoreflect.MessageKind {
		return false
	}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		switch string(fields.Get(i).Name()) {
		case "value", "id", "extension":
		default:
			return false
		}
	}
	return true
}

func validateResourceRootAttrs(attrs []xml.Attr) error {
	for _, a := range attrs {
		if isIgnoredXMLNSAttr(a) {
			continue
		}
		return fmt.Errorf("unexpected XML attribute %q on FHIR resource element", a.Name.Local)
	}
	return nil
}

func isIgnoredXMLNSAttr(a xml.Attr) bool {
	if a.Name.Local == "xmlns" && a.Name.Space == "" {
		return true
	}
	if a.Name.Space == "xmlns" {
		return true
	}
	if a.Name.Space == xmlSchemaInstanceNamespace && (a.Name.Local == "schemaLocation" || a.Name.Local == "noNamespaceSchemaLocation") {
		return true
	}
	return false
}

func getAttrValue(attrs []xml.Attr, local string) string {
	for _, a := range attrs {
		if a.Name.Space == "" && a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

func hasAttr(attrs []xml.Attr, local string) bool {
	for _, a := range attrs {
		if a.Name.Space == "" && a.Name.Local == local {
			return true
		}
	}
	return false
}

func isValidXMLIdent(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isAlpha := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if i == 0 {
			if !isAlpha {
				return false
			}
			continue
		}
		if !isAlpha && !isDigit && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func writeEscapedAttr(buf *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			buf.WriteString("&amp;")
		case '<':
			buf.WriteString("&lt;")
		case '>':
			buf.WriteString("&gt;")
		case '"':
			buf.WriteString("&quot;")
		case '\t':
			buf.WriteString("&#x9;")
		case '\n':
			buf.WriteString("&#xA;")
		case '\r':
			buf.WriteString("&#xD;")
		default:
			buf.WriteByte(s[i])
		}
	}
}

func formatPrimitiveString(val any) (string, bool) {
	if val == nil {
		return "", false
	}
	switch v := val.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case json.Number:
		s := v.String()
		return s, s != ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case uint:
		return strconv.FormatUint(uint64(v), 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	case uint32:
		return strconv.FormatUint(uint64(v), 10), true
	default:
		return fmt.Sprint(v), true
	}
}

func toStringAnyMap(v any) (map[string]any, bool) {
	if v == nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	if rm, ok := v.(json.RawMessage); ok && len(rm) > 0 {
		dec := json.NewDecoder(bytes.NewReader(rm))
		dec.UseNumber()
		var out map[string]any
		if dec.Decode(&out) == nil && out != nil {
			return out, true
		}
	}
	if ms, ok := v.(map[string]string); ok {
		out := make(map[string]any, len(ms))
		for k, val := range ms {
			out[k] = val
		}
		return out, true
	}
	return nil, false
}

func toAnySlice(v any) ([]any, bool) {
	if v == nil {
		return nil, false
	}
	switch s := v.(type) {
	case []any:
		return s, true
	case []map[string]any:
		out := make([]any, len(s))
		for i := range s {
			out[i] = s[i]
		}
		return out, true
	case []map[string]string:
		out := make([]any, len(s))
		for i := range s {
			m := make(map[string]any, len(s[i]))
			for k, val := range s[i] {
				m[k] = val
			}
			out[i] = m
		}
		return out, true
	case []string:
		out := make([]any, len(s))
		for i := range s {
			out[i] = s[i]
		}
		return out, true
	case []int:
		out := make([]any, len(s))
		for i := range s {
			out[i] = s[i]
		}
		return out, true
	case []float64:
		out := make([]any, len(s))
		for i := range s {
			out[i] = s[i]
		}
		return out, true
	case []bool:
		out := make([]any, len(s))
		for i := range s {
			out[i] = s[i]
		}
		return out, true
	default:
		return nil, false
	}
}
