package server

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/flint-fhir/flint/pkg/fhirutil"
)

type fhirModelFieldInfo struct {
	types  []string
	isList bool
}

type fhirModelTypeInfo struct {
	name             string
	parent           string
	fields           map[string]fhirModelFieldInfo
	disallowedChoice map[string]string // e.g. "valueQuantity" -> "value"
}

type fhirModelRegistry struct {
	types            map[string]*fhirModelTypeInfo
	descendantsCache sync.Map // string -> []string
}

var (
	globalFHIRModelOnce sync.Once
	globalFHIRModel     *fhirModelRegistry
)

var protoWrapperToFHIRPrimitive = map[string]string{
	"Boolean":      "boolean",
	"Integer":      "integer",
	"Integer64":    "integer64",
	"String":       "string",
	"Decimal":      "decimal",
	"Uri":          "uri",
	"Url":          "url",
	"Canonical":    "canonical",
	"Base64Binary": "base64Binary",
	"Instant":      "instant",
	"Date":         "date",
	"DateTime":     "dateTime",
	"Time":         "time",
	"Code":         "code",
	"Oid":          "oid",
	"Id":           "id",
	"Markdown":     "markdown",
	"UnsignedInt":  "unsignedInt",
	"PositiveInt":  "positiveInt",
	"Uuid":         "uuid",
	"Xhtml":        "xhtml",
}

func getFHIRModelRegistry() *fhirModelRegistry {
	globalFHIRModelOnce.Do(func() {
		globalFHIRModel = buildFHIRModelRegistry()
	})
	return globalFHIRModel
}

func buildFHIRModelRegistry() *fhirModelRegistry {
	reg := &fhirModelRegistry{
		types: make(map[string]*fhirModelTypeInfo, 512),
	}

	// Base abstract & synthetic FHIRPath types
	reg.types["Base"] = &fhirModelTypeInfo{
		name:             "Base",
		parent:           "Any",
		fields:           map[string]fhirModelFieldInfo{},
		disallowedChoice: map[string]string{},
	}
	reg.types["Element"] = &fhirModelTypeInfo{
		name:   "Element",
		parent: "Base",
		fields: map[string]fhirModelFieldInfo{
			"id":        {types: []string{"string"}, isList: false},
			"extension": {types: []string{"Extension"}, isList: true},
		},
		disallowedChoice: map[string]string{},
	}
	reg.types["BackboneElement"] = &fhirModelTypeInfo{
		name:   "BackboneElement",
		parent: "Element",
		fields: map[string]fhirModelFieldInfo{
			"modifierExtension": {types: []string{"Extension"}, isList: true},
		},
		disallowedChoice: map[string]string{},
	}
	reg.types["Resource"] = &fhirModelTypeInfo{
		name:   "Resource",
		parent: "Base",
		fields: map[string]fhirModelFieldInfo{
			"id":            {types: []string{"id"}, isList: false},
			"meta":          {types: []string{"Meta"}, isList: false},
			"implicitRules": {types: []string{"uri"}, isList: false},
			"language":      {types: []string{"code"}, isList: false},
		},
		disallowedChoice: map[string]string{},
	}
	reg.types["DomainResource"] = &fhirModelTypeInfo{
		name:   "DomainResource",
		parent: "Resource",
		fields: map[string]fhirModelFieldInfo{
			"text":              {types: []string{"Narrative"}, isList: false},
			"contained":         {types: []string{"Resource"}, isList: true},
			"extension":         {types: []string{"Extension"}, isList: true},
			"modifierExtension": {types: []string{"Extension"}, isList: true},
		},
		disallowedChoice: map[string]string{},
	}
	reg.types["System.TypeInfo"] = &fhirModelTypeInfo{
		name:   "System.TypeInfo",
		parent: "Any",
		fields: map[string]fhirModelFieldInfo{
			"namespace": {types: []string{"System.String"}, isList: false},
			"name":      {types: []string{"System.String"}, isList: false},
		},
		disallowedChoice: map[string]string{},
	}

	// Register all FHIR primitive types (inherit from Element)
	for prim, parent := range knownFHIRPrimitiveTypes {
		if _, exists := reg.types[prim]; !exists {
			reg.types[prim] = &fhirModelTypeInfo{
				name:             prim,
				parent:           parent,
				fields:           map[string]fhirModelFieldInfo{},
				disallowedChoice: map[string]string{},
			}
		}
	}

	// Walk all compiled R4 protobuf descriptors (resources, datatypes, nested BackboneElements)
	allDescs := fhirutil.AllR4ResourceDescriptors()
	for _, md := range allDescs {
		reg.registerMessageDescriptor(md, true)
	}

	// Ensure Quantity profile subtypes exist and inherit Quantity's fields via parent chain
	for ct, parent := range knownFHIRComplexTypes {
		if existing, ok := reg.types[ct]; ok {
			if existing.parent == "" {
				existing.parent = parent
			}
		} else {
			reg.types[ct] = &fhirModelTypeInfo{
				name:             ct,
				parent:           parent,
				fields:           map[string]fhirModelFieldInfo{},
				disallowedChoice: map[string]string{},
			}
		}
	}

	// ViewDefinition (SQL-on-FHIR v2 custom logical resource supported by Flint)
	if _, ok := reg.types["ViewDefinition"]; !ok {
		reg.types["ViewDefinition"] = &fhirModelTypeInfo{
			name:   "ViewDefinition",
			parent: "DomainResource",
			fields: map[string]fhirModelFieldInfo{
				"url":      {types: []string{"uri"}, isList: false},
				"name":     {types: []string{"string"}, isList: false},
				"status":   {types: []string{"code"}, isList: false},
				"resource": {types: []string{"code"}, isList: false},
				"constant": {types: []string{"BackboneElement"}, isList: true},
				"select":   {types: []string{"BackboneElement"}, isList: true},
				"where":    {types: []string{"BackboneElement"}, isList: true},
			},
			disallowedChoice: map[string]string{},
		}
	}

	return reg
}

func descriptorFHIRTypeName(md protoreflect.MessageDescriptor) (string, bool) {
	if md == nil {
		return "", false
	}
	full := string(md.FullName())
	const prefix = "google.fhir.r4.core."
	rest := strings.TrimPrefix(full, prefix)
	if rest == "ContainedResource" {
		return "Resource", false
	}
	if !strings.Contains(rest, ".") {
		if prim, isPrim := protoWrapperToFHIRPrimitive[rest]; isPrim {
			return prim, false
		}
	}
	// Bound code wrapper messages (e.g. Patient.GenderCode, Observation.StatusCode) have an Enum "value" field
	if valFD := md.Fields().ByName("value"); valFD != nil && valFD.Kind() == protoreflect.EnumKind {
		return "code", false
	}
	// Primitive wrapper messages with value_us or scalar value
	if md.Fields().ByName("value_us") != nil {
		if prim, ok := protoWrapperToFHIRPrimitive[string(md.Name())]; ok {
			return prim, false
		}
		return "dateTime", false
	}
	if valFD := md.Fields().ByName("value"); valFD != nil && valFD.Kind() != protoreflect.MessageKind {
		if prim, ok := protoWrapperToFHIRPrimitive[string(md.Name())]; ok {
			return prim, false
		}
		switch valFD.Kind() {
		case protoreflect.BoolKind:
			return "boolean", false
		case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Uint32Kind:
			return "integer", false
		case protoreflect.StringKind:
			return "string", false
		default:
			return "string", false
		}
	}
	return rest, true
}

func normalizeProtoJSONFieldName(jsonName string) string {
	switch jsonName {
	case "classValue":
		return "class"
	case "stringValue":
		return "string"
	default:
		return jsonName
	}
}

func (r *fhirModelRegistry) registerMessageDescriptor(md protoreflect.MessageDescriptor, isTopResource bool) string {
	typeName, isComplexOrResource := descriptorFHIRTypeName(md)
	if !isComplexOrResource || typeName == "" {
		return typeName
	}
	if _, exists := r.types[typeName]; exists {
		return typeName
	}

	parent := "BackboneElement"
	if isTopResource || knownFHIRResourceTypes[typeName] {
		if typeName == "Bundle" || typeName == "Parameters" || typeName == "Binary" {
			parent = "Resource"
		} else {
			parent = "DomainResource"
		}
	} else if p, ok := knownFHIRComplexTypes[typeName]; ok {
		parent = p
	}

	info := &fhirModelTypeInfo{
		name:             typeName,
		parent:           parent,
		fields:           make(map[string]fhirModelFieldInfo),
		disallowedChoice: make(map[string]string),
	}
	r.types[typeName] = info

	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		fieldName := normalizeProtoJSONFieldName(fd.JSONName())
		if fd.Kind() != protoreflect.MessageKind || fd.Message() == nil {
			var scalarType string
			switch fd.Kind() {
			case protoreflect.BoolKind:
				scalarType = "boolean"
			case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Int64Kind:
				scalarType = "integer"
			default:
				scalarType = "string"
			}
			info.fields[fieldName] = fhirModelFieldInfo{types: []string{scalarType}, isList: fd.IsList()}
			continue
		}

		fmd := fd.Message()
		if fmd.Oneofs().ByName("choice") != nil {
			var choiceTypes []string
			choiceFields := fmd.Fields()
			for j := 0; j < choiceFields.Len(); j++ {
				cfd := choiceFields.Get(j)
				optName := normalizeProtoJSONFieldName(cfd.JSONName())
				if len(optName) > 0 {
					expanded := fieldName + strings.ToUpper(optName[:1]) + optName[1:]
					info.disallowedChoice[expanded] = fieldName
				}
				if cfd.Kind() == protoreflect.MessageKind && cfd.Message() != nil {
					ct := r.registerMessageDescriptor(cfd.Message(), false)
					if ct != "" {
						choiceTypes = appendUniqueType(choiceTypes, ct)
					}
				}
			}
			info.fields[fieldName] = fhirModelFieldInfo{types: choiceTypes, isList: fd.IsList()}
			continue
		}

		childType := r.registerMessageDescriptor(fmd, false)
		if childType != "" {
			// Refine date/instant ambiguity where needed ( matches schemas.go )
			info.fields[fieldName] = fhirModelFieldInfo{types: []string{childType}, isList: fd.IsList()}
		}
	}

	return typeName
}

func (r *fhirModelRegistry) lookupFieldOnType(typeName, field string) ([]string, string, bool) {
	if strings.HasPrefix(typeName, "System.") && typeName != "System.TypeInfo" {
		if typeName == "System.Quantity" {
			typeName = "Quantity"
		} else {
			return nil, "", false
		}
	}
	visited := make(map[string]bool, 8)
	cur := typeName
	for cur != "" && !visited[cur] {
		visited[cur] = true
		ti, ok := r.types[cur]
		if !ok {
			if p, okP := knownFHIRPrimitiveTypes[cur]; okP {
				cur = p
				continue
			}
			if p, okC := knownFHIRComplexTypes[cur]; okC {
				cur = p
				continue
			}
			break
		}
		if baseChoice, bad := ti.disallowedChoice[field]; bad {
			return nil, baseChoice, false
		}
		if fi, hasField := ti.fields[field]; hasField {
			return fi.types, "", true
		}
		cur = ti.parent
	}
	return nil, "", false
}

func (r *fhirModelRegistry) directChildTypes(typeName string) []string {
	var out []string
	visited := make(map[string]bool, 8)
	cur := typeName
	for cur != "" && !visited[cur] {
		visited[cur] = true
		ti, ok := r.types[cur]
		if !ok {
			break
		}
		for _, fi := range ti.fields {
			for _, ft := range fi.types {
				out = appendUniqueType(out, ft)
			}
		}
		cur = ti.parent
	}
	return out
}

func (r *fhirModelRegistry) descendantTypes(typeName string) []string {
	if cached, ok := r.descendantsCache.Load(typeName); ok {
		return cached.([]string)
	}
	seen := make(map[string]bool, 64)
	var queue []string
	for _, ct := range r.directChildTypes(typeName) {
		if !seen[ct] {
			seen[ct] = true
			queue = append(queue, ct)
		}
	}
	for len(queue) > 0 {
		head := queue[0]
		queue = queue[1:]
		if head == "Resource" || head == "Any" {
			seen["Any"] = true
			continue
		}
		for _, ct := range r.directChildTypes(head) {
			if !seen[ct] {
				seen[ct] = true
				queue = append(queue, ct)
			}
		}
	}
	res := make([]string, 0, len(seen))
	for t := range seen {
		res = append(res, t)
	}
	sort.Strings(res)
	r.descendantsCache.Store(typeName, res)
	return res
}

func (r *fhirModelRegistry) lookupFieldWildcard(field string) ([]string, bool) {
	var out []string
	for _, ti := range r.types {
		if fi, ok := ti.fields[field]; ok {
			for _, ft := range fi.types {
				out = appendUniqueType(out, ft)
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	sort.Strings(out)
	return out, true
}

type fhirStaticCollection struct {
	types   []string
	ordered bool
}

type fhirStaticScope struct {
	reg       *fhirModelRegistry
	rootType  string
	ctxTypes  []string
	thisTypes []string
	totalCols []string
}

func appendUniqueType(list []string, t string) []string {
	if t == "" {
		return list
	}
	for _, existing := range list {
		if existing == t {
			return list
		}
	}
	return append(list, t)
}

func unionStaticTypes(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for _, t := range a {
		out = appendUniqueType(out, t)
	}
	for _, t := range b {
		out = appendUniqueType(out, t)
	}
	return out
}

func intersectStaticTypes(a, b []string) []string {
	var out []string
	for _, t := range a {
		for _, u := range b {
			if t == u {
				out = appendUniqueType(out, t)
				break
			}
		}
	}
	return out
}

func normalizeStaticTypeSpec(raw string) string {
	spec := cleanTypeSpecifier(raw)
	if strings.HasPrefix(spec, "FHIR.") {
		return strings.TrimPrefix(spec, "FHIR.")
	}
	if strings.HasPrefix(spec, "System.") {
		if spec == "System.Quantity" {
			return "Quantity"
		}
		return spec
	}
	switch spec {
	case "Boolean", "String", "Integer", "Decimal", "Date", "DateTime", "Time":
		return "System." + spec
	default:
		return spec
	}
}

func staticTypeMatchesIs(actual, rawTarget string) bool {
	if actual == "Any" {
		return true
	}
	spec := cleanTypeSpecifier(rawTarget)
	if spec == "Any" || spec == "System.Any" {
		return true
	}
	isActualSystem := strings.HasPrefix(actual, "System.")
	actualBare := strings.TrimPrefix(actual, "System.")

	if dot := strings.IndexByte(spec, '.'); dot != -1 {
		ns := spec[:dot]
		reqName := spec[dot+1:]
		if ns == "System" {
			if !isActualSystem {
				if reqName == "Quantity" && isFHIRSubtypeOf(actual, "Quantity") {
					return true
				}
				return false
			}
			return actualBare == reqName || reqName == "Any"
		}
		if ns == "FHIR" {
			if isActualSystem {
				return false
			}
			return isFHIRSubtypeOf(actual, reqName) || (strings.Contains(actual, ".") && isFHIRSubtypeOf("BackboneElement", reqName))
		}
		return false
	}

	if isActualSystem {
		return actualBare == spec
	}
	if strings.Contains(actual, ".") {
		return actual == spec || isFHIRSubtypeOf("BackboneElement", spec)
	}
	return isFHIRSubtypeOf(actual, spec)
}

func NarrowStaticTypesForIs(types []string, rawTarget string) []string {
	var out []string
	normTarget := normalizeStaticTypeSpec(rawTarget)
	for _, t := range types {
		if staticTypeMatchesIs(t, rawTarget) {
			out = appendUniqueType(out, t)
		} else if t == "Any" || t == "Base" || t == "Element" || t == "Resource" || t == "DomainResource" {
			if staticTypeMatchesIs(normTarget, t) {
				out = appendUniqueType(out, normTarget)
			}
		}
	}
	return out
}

func NarrowStaticTypesForIsNot(types []string, rawTarget string) []string {
	var out []string
	for _, t := range types {
		if !staticTypeMatchesIs(t, rawTarget) {
			out = appendUniqueType(out, t)
		}
	}
	return out
}

func narrowTypesWhenTrue(types []string, guardExpr string) []string {
	g := strings.TrimSpace(guardExpr)
	for hasOuterParens(g) {
		g = strings.TrimSpace(g[1 : len(g)-1])
	}
	if g == "" {
		return types
	}
	if _, _, _, ok := splitTopLevelKeywordOp(g, []string{"implies"}); ok {
		return types
	}
	if lhs, op, rhs, ok := splitTopLevelKeywordOp(g, []string{"or", "xor"}); ok {
		if op == "xor" {
			return types
		}
		return unionStaticTypes(narrowTypesWhenTrue(types, lhs), narrowTypesWhenTrue(types, rhs))
	}
	if lhs, _, rhs, ok := splitTopLevelKeywordOp(g, []string{"and"}); ok {
		return intersectStaticTypes(narrowTypesWhenTrue(types, lhs), narrowTypesWhenTrue(types, rhs))
	}
	if _, _, _, ok := splitTopLevelKeywordOp(g, []string{"in", "contains"}); ok {
		return types
	}
	if _, _, _, ok := splitTopLevelSymbolOp(g, []string{"!=", "!~", "=", "~", ">=", "<=", ">", "<", "|"}); ok {
		return types
	}
	if lhs, op, rhs, ok := splitTopLevelKeywordOp(g, []string{"is", "as"}); ok {
		if op == "is" {
			lClean := strings.TrimSpace(lhs)
			for hasOuterParens(lClean) {
				lClean = strings.TrimSpace(lClean[1 : len(lClean)-1])
			}
			if lClean == "$this" {
				return NarrowStaticTypesForIs(types, rhs)
			}
		}
		return types
	}
	segs := splitFHIRPathSegments(g)
	if len(segs) >= 2 && strings.EqualFold(strings.TrimSpace(segs[len(segs)-1]), "not()") {
		inner := strings.Join(segs[:len(segs)-1], ".")
		return narrowTypesWhenFalse(types, inner)
	}
	if len(segs) == 2 {
		first := strings.TrimSpace(segs[0])
		for hasOuterParens(first) {
			first = strings.TrimSpace(first[1 : len(first)-1])
		}
		if first == "$this" {
			if fnName, args, isFn := parseFunctionCall(segs[1]); isFn && strings.EqualFold(fnName, "is") && len(args) == 1 {
				return NarrowStaticTypesForIs(types, args[0])
			}
		}
	}
	return types
}

func narrowTypesWhenFalse(types []string, guardExpr string) []string {
	g := strings.TrimSpace(guardExpr)
	for hasOuterParens(g) {
		g = strings.TrimSpace(g[1 : len(g)-1])
	}
	if g == "" {
		return types
	}
	if _, _, _, ok := splitTopLevelKeywordOp(g, []string{"implies"}); ok {
		return types
	}
	if lhs, op, rhs, ok := splitTopLevelKeywordOp(g, []string{"or", "xor"}); ok {
		if op == "xor" {
			return types
		}
		return intersectStaticTypes(narrowTypesWhenFalse(types, lhs), narrowTypesWhenFalse(types, rhs))
	}
	if lhs, _, rhs, ok := splitTopLevelKeywordOp(g, []string{"and"}); ok {
		return unionStaticTypes(narrowTypesWhenFalse(types, lhs), narrowTypesWhenFalse(types, rhs))
	}
	if _, _, _, ok := splitTopLevelKeywordOp(g, []string{"in", "contains"}); ok {
		return types
	}
	if _, _, _, ok := splitTopLevelSymbolOp(g, []string{"!=", "!~", "=", "~", ">=", "<=", ">", "<", "|"}); ok {
		return types
	}
	if lhs, op, rhs, ok := splitTopLevelKeywordOp(g, []string{"is", "as"}); ok {
		if op == "is" {
			lClean := strings.TrimSpace(lhs)
			for hasOuterParens(lClean) {
				lClean = strings.TrimSpace(lClean[1 : len(lClean)-1])
			}
			if lClean == "$this" {
				return NarrowStaticTypesForIsNot(types, rhs)
			}
		}
		return types
	}
	segs := splitFHIRPathSegments(g)
	if len(segs) >= 2 && strings.EqualFold(strings.TrimSpace(segs[len(segs)-1]), "not()") {
		inner := strings.Join(segs[:len(segs)-1], ".")
		return narrowTypesWhenTrue(types, inner)
	}
	if len(segs) == 2 {
		first := strings.TrimSpace(segs[0])
		for hasOuterParens(first) {
			first = strings.TrimSpace(first[1 : len(first)-1])
		}
		if first == "$this" {
			if fnName, args, isFn := parseFunctionCall(segs[1]); isFn && strings.EqualFold(fnName, "is") && len(args) == 1 {
				return NarrowStaticTypesForIsNot(types, args[0])
			}
		}
	}
	return types
}

func isStaticBooleanType(t string) bool {
	return t == "Any" || t == "System.Boolean" || t == "Boolean" || t == "boolean"
}

func typeSetIncludesBoolean(types []string) bool {
	for _, t := range types {
		if isStaticBooleanType(t) {
			return true
		}
	}
	return false
}

func isStaticStringCompatible(t string) bool {
	if t == "Any" || t == "System.String" || t == "String" || t == "string" {
		return true
	}
	switch t {
	case "code", "id", "markdown", "uri", "url", "canonical", "oid", "uuid", "xhtml", "base64Binary":
		return true
	}
	return false
}

func typeSetIncludesString(types []string) bool {
	for _, t := range types {
		if isStaticStringCompatible(t) {
			return true
		}
	}
	return false
}

func isStaticToStringCompatible(t string) bool {
	if t == "Any" {
		return true
	}
	if strings.HasPrefix(t, "System.") {
		return t != "System.TypeInfo"
	}
	if knownSystemTypes[t] {
		return true
	}
	if _, isPrim := knownFHIRPrimitiveTypes[t]; isPrim {
		return true
	}
	return isFHIRSubtypeOf(t, "Quantity")
}

func typeSetIncludesToStringCompatible(types []string) bool {
	for _, t := range types {
		if isStaticToStringCompatible(t) {
			return true
		}
	}
	return false
}

func isStaticTemporalType(t string) bool {
	switch t {
	case "System.Date", "Date", "date",
		"System.DateTime", "DateTime", "dateTime", "instant",
		"System.Time", "Time", "time":
		return true
	}
	return false
}

func allStaticTypesTemporal(types []string) bool {
	if len(types) == 0 {
		return false
	}
	for _, t := range types {
		if !isStaticTemporalType(t) {
			return false
		}
	}
	return true
}

func isStaticQuantityType(t string) bool {
	if t == "Any" || t == "System.Quantity" || t == "Quantity" {
		return true
	}
	return isFHIRSubtypeOf(t, "Quantity")
}

func typeSetIncludesQuantity(types []string) bool {
	for _, t := range types {
		if isStaticQuantityType(t) {
			return true
		}
	}
	return false
}

// ValidateFHIRPathSemantics performs static compile-time semantic type-checking of a
// FHIRPath R4 expression against the compiled FHIR R4 model descriptors, optional
// context expression, and optional union contextType (e.g. "Period|Timing").
func ValidateFHIRPathSemantics(root map[string]any, contextExpr, contextType, rawExpr string) error {
	expr, err := validateAndCleanFHIRPath(rawExpr)
	if err != nil {
		return err
	}
	reg := getFHIRModelRegistry()

	rootType := "Resource"
	if root != nil {
		if rt, ok := root["resourceType"].(string); ok && strings.TrimSpace(rt) != "" {
			rootType = strings.TrimSpace(rt)
		}
	}

	focus := fhirStaticCollection{
		types:   []string{rootType},
		ordered: true,
	}

	if ctxClean := strings.TrimSpace(contextExpr); ctxClean != "" {
		cleanedCtx, errCtx := validateAndCleanFHIRPath(ctxClean)
		if errCtx != nil {
			return errCtx
		}
		ctxScope := fhirStaticScope{
			reg:       reg,
			rootType:  rootType,
			ctxTypes:  focus.types,
			thisTypes: focus.types,
		}
		derivedFocus, errFocus := checkFHIRPathExprSemantics(focus, ctxScope, cleanedCtx)
		if errFocus != nil {
			return errFocus
		}
		focus = derivedFocus
	}

	if ctRaw := strings.TrimSpace(contextType); ctRaw != "" {
		var parsed []string
		for _, part := range strings.Split(ctRaw, "|") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			if !isKnownFHIRPathTypeName(cleanTypeSpecifier(p)) {
				return fmt.Errorf("FHIRPath semantic error: unknown contextType %q", p)
			}
			parsed = appendUniqueType(parsed, normalizeStaticTypeSpec(p))
		}
		focus = fhirStaticCollection{
			types:   parsed,
			ordered: true,
		}
	}

	scope := fhirStaticScope{
		reg:       reg,
		rootType:  rootType,
		ctxTypes:  focus.types,
		thisTypes: focus.types,
	}
	_, err = checkFHIRPathExprSemantics(focus, scope, expr)
	return err
}

// EvalFHIRPathStrict validates static FHIRPath R4 semantics first and then evaluates
// the expression at runtime.
func EvalFHIRPathStrict(root map[string]any, contextExpr, contextType, rawExpr string) ([]FHIRPathValue, error) {
	if err := ValidateFHIRPathSemantics(root, contextExpr, contextType, rawExpr); err != nil {
		return nil, err
	}
	return EvalFHIRPathTypedWithContextExpr(root, contextExpr, rawExpr)
}

func checkFHIRPathExprSemantics(input fhirStaticCollection, scope fhirStaticScope, expr string) (fhirStaticCollection, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return fhirStaticCollection{ordered: true}, nil
	}

	for hasOuterParens(expr) {
		expr = strings.TrimSpace(expr[1 : len(expr)-1])
	}
	if expr == "" || expr == "{}" {
		return fhirStaticCollection{ordered: true}, nil
	}

	// 1. implies (guards RHS assuming LHS is true)
	if lhs, _, rhs, ok := splitTopLevelKeywordOp(expr, []string{"implies"}); ok {
		if _, err := checkFHIRPathExprSemantics(input, scope, lhs); err != nil {
			return fhirStaticCollection{}, err
		}
		rhsScope := scope
		rhsScope.thisTypes = narrowTypesWhenTrue(scope.thisTypes, lhs)
		rhsInput := input
		if slicesEqual(input.types, scope.thisTypes) {
			rhsInput.types = rhsScope.thisTypes
		}
		if _, err := checkFHIRPathExprSemantics(rhsInput, rhsScope, rhs); err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil
	}

	// 2. or, xor ('or' guards RHS assuming LHS is false; 'xor' does NOT narrow)
	if lhs, op, rhs, ok := splitTopLevelKeywordOp(expr, []string{"or", "xor"}); ok {
		if _, err := checkFHIRPathExprSemantics(input, scope, lhs); err != nil {
			return fhirStaticCollection{}, err
		}
		rhsScope := scope
		rhsInput := input
		if op == "or" {
			rhsScope.thisTypes = narrowTypesWhenFalse(scope.thisTypes, lhs)
			if slicesEqual(input.types, scope.thisTypes) {
				rhsInput.types = rhsScope.thisTypes
			}
		}
		if _, err := checkFHIRPathExprSemantics(rhsInput, rhsScope, rhs); err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil
	}

	// 3. and (guards RHS assuming LHS is true)
	if lhs, _, rhs, ok := splitTopLevelKeywordOp(expr, []string{"and"}); ok {
		if _, err := checkFHIRPathExprSemantics(input, scope, lhs); err != nil {
			return fhirStaticCollection{}, err
		}
		rhsScope := scope
		rhsScope.thisTypes = narrowTypesWhenTrue(scope.thisTypes, lhs)
		rhsInput := input
		if slicesEqual(input.types, scope.thisTypes) {
			rhsInput.types = rhsScope.thisTypes
		}
		if _, err := checkFHIRPathExprSemantics(rhsInput, rhsScope, rhs); err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil
	}

	// 4. Membership: in, contains
	if lhs, _, rhs, ok := splitTopLevelKeywordOp(expr, []string{"in", "contains"}); ok {
		if _, err := checkFHIRPathExprSemantics(input, scope, lhs); err != nil {
			return fhirStaticCollection{}, err
		}
		if _, err := checkFHIRPathExprSemantics(input, scope, rhs); err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil
	}

	// 5. Equality / Equivalence
	if lhs, _, rhs, ok := splitTopLevelSymbolOp(expr, []string{"!=", "!~", "=", "~"}); ok {
		if _, err := checkFHIRPathExprSemantics(input, scope, lhs); err != nil {
			return fhirStaticCollection{}, err
		}
		if _, err := checkFHIRPathExprSemantics(input, scope, rhs); err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil
	}

	// 6. Ordering Comparison
	if lhs, _, rhs, ok := splitTopLevelSymbolOp(expr, []string{">=", "<=", ">", "<"}); ok {
		if _, err := checkFHIRPathExprSemantics(input, scope, lhs); err != nil {
			return fhirStaticCollection{}, err
		}
		if _, err := checkFHIRPathExprSemantics(input, scope, rhs); err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil
	}

	// 7. Union operator: |
	if lhs, _, rhs, ok := splitTopLevelSymbolOp(expr, []string{"|"}); ok {
		lCol, err := checkFHIRPathExprSemantics(input, scope, lhs)
		if err != nil {
			return fhirStaticCollection{}, err
		}
		rCol, err := checkFHIRPathExprSemantics(input, scope, rhs)
		if err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{
			types:   unionStaticTypes(lCol.types, rCol.types),
			ordered: lCol.ordered && rCol.ordered,
		}, nil
	}

	// 8. Type operators: is, as
	if lhs, op, rhs, ok := splitTopLevelKeywordOp(expr, []string{"is", "as"}); ok {
		typeSpec := cleanTypeSpecifier(rhs)
		if !isKnownFHIRPathTypeName(typeSpec) {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: unknown type specifier %q in %q operator", typeSpec, op)
		}
		if _, err := checkFHIRPathExprSemantics(input, scope, lhs); err != nil {
			return fhirStaticCollection{}, err
		}
		if op == "is" {
			return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil
		}
		return fhirStaticCollection{types: []string{normalizeStaticTypeSpec(rhs)}, ordered: true}, nil
	}

	// 9. Additive operators: +, -, &
	if lhs, op, rhs, ok := splitTopLevelAdditiveOp(expr); ok {
		lCol, err := checkFHIRPathExprSemantics(input, scope, lhs)
		if err != nil {
			return fhirStaticCollection{}, err
		}
		rCol, err := checkFHIRPathExprSemantics(input, scope, rhs)
		if err != nil {
			return fhirStaticCollection{}, err
		}
		if op == "&" {
			return fhirStaticCollection{types: []string{"System.String"}, ordered: true}, nil
		}
		if len(lCol.types) > 0 && len(rCol.types) > 0 && allStaticTypesTemporal(lCol.types) && !typeSetIncludesQuantity(rCol.types) {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: operator %q on temporal operand %v requires a Quantity duration, got %v", op, lCol.types, rCol.types)
		}
		return fhirStaticCollection{
			types:   inferAdditiveStaticTypes(lCol.types, op, rCol.types),
			ordered: true,
		}, nil
	}

	// 10. Multiplicative operators: *, /, div, mod
	if lhs, op, rhs, ok := splitTopLevelMultiplicativeOp(expr); ok {
		lCol, err := checkFHIRPathExprSemantics(input, scope, lhs)
		if err != nil {
			return fhirStaticCollection{}, err
		}
		rCol, err := checkFHIRPathExprSemantics(input, scope, rhs)
		if err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{
			types:   inferMultiplicativeStaticTypes(lCol.types, op, rCol.types),
			ordered: true,
		}, nil
	}

	// 11. Unary - / +
	if (strings.HasPrefix(expr, "-") || strings.HasPrefix(expr, "+")) && len(expr) > 1 {
		sub, err := checkFHIRPathExprSemantics(input, scope, expr[1:])
		if err != nil {
			return fhirStaticCollection{}, err
		}
		return sub, nil
	}

	// 12. Chain of segments
	return checkFHIRPathChainSemantics(input, scope, expr)
}

func inferAdditiveStaticTypes(lTypes []string, op string, rTypes []string) []string {
	if len(lTypes) == 0 || len(rTypes) == 0 {
		return unionStaticTypes(lTypes, rTypes)
	}
	if allStaticTypesTemporal(lTypes) {
		return lTypes
	}
	if op == "+" && typeSetIncludesString(lTypes) && typeSetIncludesString(rTypes) {
		return []string{"System.String"}
	}
	if typeSetIncludesQuantity(lTypes) || typeSetIncludesQuantity(rTypes) {
		return []string{"Quantity"}
	}
	for _, t := range append(append([]string{}, lTypes...), rTypes...) {
		if t == "System.Decimal" || t == "decimal" {
			return []string{"System.Decimal"}
		}
	}
	return []string{"System.Integer"}
}

func inferMultiplicativeStaticTypes(lTypes []string, op string, rTypes []string) []string {
	if op == "div" {
		return []string{"System.Integer"}
	}
	if typeSetIncludesQuantity(lTypes) || typeSetIncludesQuantity(rTypes) {
		return []string{"Quantity"}
	}
	if op == "/" {
		return []string{"System.Decimal"}
	}
	for _, t := range append(append([]string{}, lTypes...), rTypes...) {
		if t == "System.Decimal" || t == "decimal" {
			return []string{"System.Decimal"}
		}
	}
	return []string{"System.Integer"}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func checkFHIRPathChainSemantics(input fhirStaticCollection, scope fhirStaticScope, expr string) (fhirStaticCollection, error) {
	segs := splitFHIRPathSegments(expr)
	if len(segs) == 0 {
		return fhirStaticCollection{ordered: true}, nil
	}

	current := input
	for idx, rawSeg := range segs {
		baseSeg, indexExprs := splitSegmentIndexers(rawSeg)
		if idx == 0 {
			if litCol, isLit, errLit := checkFHIRPathPrimaryLiteralSemantics(baseSeg, input, scope); isLit {
				if errLit != nil {
					return fhirStaticCollection{}, errLit
				}
				current = litCol
				var err error
				current, err = checkFHIRPathIndexersSemantics(current, input, scope, indexExprs)
				if err != nil {
					return fhirStaticCollection{}, err
				}
				continue
			}

			if _, _, isFn := parseFunctionCall(baseSeg); !isFn {
				unwrappedFirst := unwrapBacktickIdent(baseSeg)
				if knownFHIRResourceTypes[unwrappedFirst] {
					if !resourceContextCompatible(unwrappedFirst, scope.rootType, current.types) {
						return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: root resource specifier %q does not match context resource type %q", unwrappedFirst, scope.rootType)
					}
					current = fhirStaticCollection{types: []string{unwrappedFirst}, ordered: true}
					var err error
					current, err = checkFHIRPathIndexersSemantics(current, input, scope, indexExprs)
					if err != nil {
						return fhirStaticCollection{}, err
					}
					continue
				}
			}
		}

		if baseSeg != "" {
			var err error
			current, err = checkFHIRPathStepSemantics(current, input, scope, baseSeg)
			if err != nil {
				return fhirStaticCollection{}, err
			}
		}
		var err error
		current, err = checkFHIRPathIndexersSemantics(current, input, scope, indexExprs)
		if err != nil {
			return fhirStaticCollection{}, err
		}
	}
	return current, nil
}

func resourceContextCompatible(specifier, rootType string, focusTypes []string) bool {
	if specifier == rootType || rootType == "Resource" || rootType == "DomainResource" {
		return true
	}
	for _, ft := range focusTypes {
		if ft == specifier || ft == "Resource" || ft == "DomainResource" || ft == "Any" {
			return true
		}
	}
	return false
}

func checkFHIRPathIndexersSemantics(current fhirStaticCollection, scopeInput fhirStaticCollection, scope fhirStaticScope, indexExprs []string) (fhirStaticCollection, error) {
	for _, ie := range indexExprs {
		if !current.ordered {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: indexer [%s] cannot be applied to an unordered collection", ie)
		}
		idxCol, err := checkFHIRPathExprSemantics(scopeInput, scope, ie)
		if err != nil {
			return fhirStaticCollection{}, err
		}
		if len(idxCol.types) > 0 {
			hasInt := false
			for _, t := range idxCol.types {
				if t == "Any" || t == "System.Integer" || t == "Integer" || t == "integer" || t == "positiveInt" || t == "unsignedInt" {
					hasInt = true
					break
				}
			}
			if !hasInt {
				return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: indexer [%s] must evaluate to Integer, got %v", ie, idxCol.types)
			}
		}
	}
	return current, nil
}

func checkFHIRPathPrimaryLiteralSemantics(seg string, input fhirStaticCollection, scope fhirStaticScope) (fhirStaticCollection, bool, error) {
	seg = strings.TrimSpace(seg)
	if seg == "{}" || strings.EqualFold(seg, "null") {
		return fhirStaticCollection{ordered: true}, true, nil
	}
	if hasOuterParens(seg) {
		col, err := checkFHIRPathExprSemantics(input, scope, seg[1:len(seg)-1])
		return col, true, err
	}
	if seg == "true" || seg == "false" {
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, true, nil
	}
	if (strings.HasPrefix(seg, "'") && strings.HasSuffix(seg, "'")) ||
		(strings.HasPrefix(seg, "\"") && strings.HasSuffix(seg, "\"")) {
		return fhirStaticCollection{types: []string{"System.String"}, ordered: true}, true, nil
	}
	if strings.HasPrefix(seg, "@") && len(seg) > 1 {
		body := strings.TrimPrefix(seg, "@")
		if strings.HasPrefix(body, "T") {
			return fhirStaticCollection{types: []string{"System.Time"}, ordered: true}, true, nil
		}
		if strings.Contains(body, "T") {
			return fhirStaticCollection{types: []string{"System.DateTime"}, ordered: true}, true, nil
		}
		return fhirStaticCollection{types: []string{"System.Date"}, ordered: true}, true, nil
	}
	switch seg {
	case "$this":
		return fhirStaticCollection{types: scope.thisTypes, ordered: true}, true, nil
	case "$index", "%rowIndex":
		return fhirStaticCollection{types: []string{"System.Integer"}, ordered: true}, true, nil
	case "$total":
		if len(scope.totalCols) > 0 {
			return fhirStaticCollection{types: scope.totalCols, ordered: true}, true, nil
		}
		return fhirStaticCollection{types: []string{"Any"}, ordered: true}, true, nil
	case "%resource", "%rootResource":
		return fhirStaticCollection{types: []string{scope.rootType}, ordered: true}, true, nil
	case "%context":
		if len(scope.ctxTypes) > 0 {
			return fhirStaticCollection{types: scope.ctxTypes, ordered: true}, true, nil
		}
		return fhirStaticCollection{types: []string{scope.rootType}, ordered: true}, true, nil
	case "%ucum", "%sct", "%loinc":
		return fhirStaticCollection{types: []string{"System.String"}, ordered: true}, true, nil
	}
	if strings.HasPrefix(seg, "%") && len(seg) > 1 {
		return fhirStaticCollection{types: []string{"Any"}, ordered: true}, true, nil
	}
	if _, okQ := parseQuantityLiteral(seg); okQ {
		return fhirStaticCollection{types: []string{"Quantity"}, ordered: true}, true, nil
	}
	var dummyCtx fhirPathContext
	if litVals, isLit := evalFHIRPathPrimaryLiteral(seg, nil, dummyCtx); isLit {
		if len(litVals) == 1 {
			ns, name := fhirPathTypeInfo(litVals[0])
			if ns == "System" && name != "" {
				return fhirStaticCollection{types: []string{"System." + name}, ordered: true}, true, nil
			}
			if name != "" {
				return fhirStaticCollection{types: []string{name}, ordered: true}, true, nil
			}
		}
		return fhirStaticCollection{ordered: true}, true, nil
	}
	return fhirStaticCollection{}, false, nil
}

func checkFHIRPathStepSemantics(current fhirStaticCollection, scopeInput fhirStaticCollection, scope fhirStaticScope, seg string) (fhirStaticCollection, error) {
	if fnName, args, isFn := parseFunctionCall(seg); isFn {
		return checkFHIRPathFunctionSemantics(current, scopeInput, scope, fnName, args)
	}

	field := unwrapBacktickIdent(seg)
	if len(current.types) == 0 {
		return fhirStaticCollection{ordered: current.ordered}, nil
	}

	var outTypes []string
	var disallowedBase string
	hasWildcard := false
	for _, t := range current.types {
		if t == "Any" || t == "Resource" || t == "DomainResource" {
			if ft, _, ok := scope.reg.lookupFieldOnType(t, field); ok {
				outTypes = unionStaticTypes(outTypes, ft)
				continue
			}
			hasWildcard = true
			continue
		}
		ft, badBase, ok := scope.reg.lookupFieldOnType(t, field)
		if ok {
			outTypes = unionStaticTypes(outTypes, ft)
		} else if badBase != "" && disallowedBase == "" {
			disallowedBase = badBase
		}
	}

	if len(outTypes) == 0 && hasWildcard {
		if wt, ok := scope.reg.lookupFieldWildcard(field); ok {
			outTypes = wt
		} else {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: unknown property %q", field)
		}
	}

	if len(outTypes) == 0 {
		if disallowedBase != "" {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: polymorphic property %q is invalid in strict FHIRPath; use %q instead", field, disallowedBase)
		}
		return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: unknown property %q on %v", field, current.types)
	}

	return fhirStaticCollection{
		types:   outTypes,
		ordered: current.ordered,
	}, nil
}

func checkFHIRPathFunctionSemantics(current fhirStaticCollection, scopeInput fhirStaticCollection, scope fhirStaticScope, fnName string, args []string) (fhirStaticCollection, error) {
	lowerFn := strings.ToLower(fnName)

	itemScope := scope
	itemScope.thisTypes = current.types
	itemInput := fhirStaticCollection{types: current.types, ordered: true}

	switch lowerFn {
	case "first", "last", "tail", "skip", "take":
		if !current.ordered {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: %s() cannot be invoked on an unordered collection", fnName)
		}
		if lowerFn == "skip" || lowerFn == "take" {
			for _, a := range args {
				if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
					return fhirStaticCollection{}, err
				}
			}
		}
		return fhirStaticCollection{types: current.types, ordered: true}, nil

	case "single", "distinct":
		return fhirStaticCollection{types: current.types, ordered: current.ordered}, nil

	case "children":
		var childTypes []string
		for _, t := range current.types {
			if t == "Any" || t == "Resource" {
				childTypes = appendUniqueType(childTypes, "Any")
				continue
			}
			childTypes = unionStaticTypes(childTypes, scope.reg.directChildTypes(t))
		}
		return fhirStaticCollection{types: childTypes, ordered: false}, nil

	case "descendants":
		var descTypes []string
		for _, t := range current.types {
			if t == "Any" || t == "Resource" {
				descTypes = appendUniqueType(descTypes, "Any")
				continue
			}
			descTypes = unionStaticTypes(descTypes, scope.reg.descendantTypes(t))
		}
		return fhirStaticCollection{types: descTypes, ordered: false}, nil

	case "exists", "all":
		if len(args) >= 1 {
			if _, err := checkFHIRPathExprSemantics(itemInput, itemScope, args[0]); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil

	case "empty", "alltrue", "anytrue", "allfalse", "anyfalse", "isdistinct", "not", "hasvalue":
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil

	case "subsetof", "supersetof", "comparable", "conformsto":
		for _, a := range args {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil

	case "count", "precision":
		return fhirStaticCollection{types: []string{"System.Integer"}, ordered: true}, nil

	case "where", "check":
		if len(args) >= 1 {
			if _, err := checkFHIRPathExprSemantics(itemInput, itemScope, args[0]); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return current, nil

	case "select":
		if len(args) == 0 {
			return current, nil
		}
		proj, err := checkFHIRPathExprSemantics(itemInput, itemScope, args[0])
		if err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{types: proj.types, ordered: current.ordered}, nil

	case "repeat":
		if len(args) == 0 {
			return current, nil
		}
		accTypes := current.types
		var outTypes []string
		for iter := 0; iter < 4; iter++ {
			rScope := scope
			rScope.thisTypes = accTypes
			rInput := fhirStaticCollection{types: accTypes, ordered: true}
			stepCol, err := checkFHIRPathExprSemantics(rInput, rScope, args[0])
			if err != nil {
				return fhirStaticCollection{}, err
			}
			prevLen := len(outTypes)
			outTypes = unionStaticTypes(outTypes, stepCol.types)
			accTypes = unionStaticTypes(accTypes, stepCol.types)
			if len(outTypes) == prevLen {
				break
			}
		}
		return fhirStaticCollection{types: outTypes, ordered: false}, nil

	case "aggregate":
		if len(args) == 0 {
			return fhirStaticCollection{ordered: true}, nil
		}
		totalTypes := current.types
		if len(args) >= 2 {
			initCol, err := checkFHIRPathExprSemantics(scopeInput, scope, args[1])
			if err != nil {
				return fhirStaticCollection{}, err
			}
			if len(initCol.types) > 0 {
				totalTypes = initCol.types
			}
		}
		aggScope := itemScope
		aggScope.totalCols = totalTypes
		resCol, err := checkFHIRPathExprSemantics(itemInput, aggScope, args[0])
		if err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{types: resCol.types, ordered: true}, nil

	case "union", "combine":
		if len(args) == 0 {
			return current, nil
		}
		other, err := checkFHIRPathExprSemantics(scopeInput, scope, args[0])
		if err != nil {
			return fhirStaticCollection{}, err
		}
		return fhirStaticCollection{
			types:   unionStaticTypes(current.types, other.types),
			ordered: current.ordered && other.ordered,
		}, nil

	case "intersect", "exclude":
		if len(args) >= 1 {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, args[0]); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return current, nil

	case "iif":
		if len(args) < 2 {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: iif() requires at least 2 arguments")
		}
		iifScope := scope
		iifScope.thisTypes = current.types
		iifInput := fhirStaticCollection{types: current.types, ordered: true}

		critCol, err := checkFHIRPathExprSemantics(iifInput, iifScope, args[0])
		if err != nil {
			return fhirStaticCollection{}, err
		}
		if len(critCol.types) > 0 && !typeSetIncludesBoolean(critCol.types) {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: iif() criterion must be Boolean, got %v", critCol.types)
		}

		trueScope := iifScope
		trueScope.thisTypes = narrowTypesWhenTrue(iifScope.thisTypes, args[0])
		trueInput := fhirStaticCollection{types: trueScope.thisTypes, ordered: true}
		trueCol, err := checkFHIRPathExprSemantics(trueInput, trueScope, args[1])
		if err != nil {
			return fhirStaticCollection{}, err
		}

		outTypes := trueCol.types
		if len(args) >= 3 {
			elseScope := iifScope
			elseScope.thisTypes = narrowTypesWhenFalse(iifScope.thisTypes, args[0])
			elseInput := fhirStaticCollection{types: elseScope.thisTypes, ordered: true}
			elseCol, err := checkFHIRPathExprSemantics(elseInput, elseScope, args[2])
			if err != nil {
				return fhirStaticCollection{}, err
			}
			outTypes = unionStaticTypes(outTypes, elseCol.types)
		}
		return fhirStaticCollection{types: outTypes, ordered: true}, nil

	case "coalesce":
		var outTypes []string
		for _, a := range args {
			c, err := checkFHIRPathExprSemantics(current, scope, a)
			if err != nil {
				return fhirStaticCollection{}, err
			}
			outTypes = unionStaticTypes(outTypes, c.types)
		}
		return fhirStaticCollection{types: outTypes, ordered: true}, nil

	case "type":
		return fhirStaticCollection{types: []string{"System.TypeInfo"}, ordered: true}, nil

	case "is":
		if len(args) != 1 {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: is() requires 1 type argument")
		}
		typeSpec := cleanTypeSpecifier(args[0])
		if !isKnownFHIRPathTypeName(typeSpec) {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: unknown type specifier %q in is()", typeSpec)
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil

	case "as", "oftype":
		if len(args) != 1 {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: %s() requires 1 type argument", fnName)
		}
		typeSpec := cleanTypeSpecifier(args[0])
		if !isKnownFHIRPathTypeName(typeSpec) {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: unknown type specifier %q in %s()", typeSpec, fnName)
		}
		return fhirStaticCollection{types: []string{normalizeStaticTypeSpec(args[0])}, ordered: current.ordered}, nil

	case "tostring":
		if len(current.types) > 0 && !typeSetIncludesToStringCompatible(current.types) {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: toString() is not supported on complex type %v", current.types)
		}
		return fhirStaticCollection{types: []string{"System.String"}, ordered: true}, nil

	case "convertstostring", "convertstointeger", "convertstodecimal", "convertstoboolean",
		"convertstodate", "convertstodatetime", "convertstotime", "convertstoquantity":
		for _, a := range args {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil

	case "tointeger":
		return fhirStaticCollection{types: []string{"System.Integer"}, ordered: true}, nil
	case "todecimal":
		return fhirStaticCollection{types: []string{"System.Decimal"}, ordered: true}, nil
	case "toboolean":
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil
	case "todate", "today":
		return fhirStaticCollection{types: []string{"System.Date"}, ordered: true}, nil
	case "todatetime", "now":
		return fhirStaticCollection{types: []string{"System.DateTime"}, ordered: true}, nil
	case "totime", "timeofday":
		return fhirStaticCollection{types: []string{"System.Time"}, ordered: true}, nil
	case "toquantity":
		for _, a := range args {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return fhirStaticCollection{types: []string{"Quantity"}, ordered: true}, nil

	case "lowboundary", "highboundary":
		for _, a := range args {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		if len(current.types) > 0 {
			return fhirStaticCollection{types: current.types, ordered: true}, nil
		}
		return fhirStaticCollection{types: []string{"System.Decimal"}, ordered: true}, nil

	case "startswith", "endswith", "contains", "matches", "matchesfull":
		if len(current.types) > 0 && !typeSetIncludesString(current.types) {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: %s() requires a string receiver, got %v", fnName, current.types)
		}
		for _, a := range args {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return fhirStaticCollection{types: []string{"System.Boolean"}, ordered: true}, nil

	case "indexof", "length":
		if len(current.types) > 0 && !typeSetIncludesString(current.types) {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: %s() requires a string receiver, got %v", fnName, current.types)
		}
		for _, a := range args {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return fhirStaticCollection{types: []string{"System.Integer"}, ordered: true}, nil

	case "substring", "upper", "lower", "trim", "replace", "replacematches", "tochars", "split",
		"encode", "decode", "escape", "unescape":
		if len(current.types) > 0 && !typeSetIncludesString(current.types) {
			return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: %s() requires a string receiver, got %v", fnName, current.types)
		}
		for _, a := range args {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return fhirStaticCollection{types: []string{"System.String"}, ordered: true}, nil

	case "join":
		for _, a := range args {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return fhirStaticCollection{types: []string{"System.String"}, ordered: true}, nil

	case "abs":
		return fhirStaticCollection{types: current.types, ordered: true}, nil
	case "ceiling", "floor", "truncate":
		return fhirStaticCollection{types: []string{"System.Integer"}, ordered: true}, nil
	case "round", "sqrt", "exp", "ln", "log", "power":
		for _, a := range args {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		if lowerFn == "power" {
			return fhirStaticCollection{types: []string{"System.Integer", "System.Decimal"}, ordered: true}, nil
		}
		return fhirStaticCollection{types: []string{"System.Decimal"}, ordered: true}, nil

	case "sort", "sortdesc":
		for _, rawArg := range args {
			a := strings.TrimSpace(rawArg)
			if strings.EqualFold(strings.Trim(a, "'\""), "asc") || strings.EqualFold(strings.Trim(a, "'\""), "desc") {
				continue
			}
			a = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "+"))
			if _, err := checkFHIRPathExprSemantics(itemInput, itemScope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return fhirStaticCollection{types: current.types, ordered: true}, nil

	case "sum", "avg":
		return fhirStaticCollection{types: []string{"System.Integer", "System.Decimal"}, ordered: true}, nil
	case "min", "max":
		return fhirStaticCollection{types: current.types, ordered: true}, nil

	case "extension":
		for _, a := range args {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, a); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return fhirStaticCollection{types: []string{"Extension"}, ordered: true}, nil

	case "resolve":
		return fhirStaticCollection{types: []string{"Resource"}, ordered: true}, nil

	case "getresourcekey", "getreferencekey":
		return fhirStaticCollection{types: []string{"System.String"}, ordered: true}, nil

	case "trace", "definevariable":
		if len(args) >= 1 {
			if _, err := checkFHIRPathExprSemantics(scopeInput, scope, args[0]); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		if len(args) >= 2 {
			if _, err := checkFHIRPathExprSemantics(itemInput, itemScope, args[1]); err != nil {
				return fhirStaticCollection{}, err
			}
		}
		return current, nil
	}

	return fhirStaticCollection{}, fmt.Errorf("FHIRPath semantic error: unknown function %s()", fnName)
}
