package validation

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type sdDiscriminator struct {
	discType string // value, pattern, type, profile, exists
	path     string
}

type sdSlicingSpec struct {
	discriminators []sdDiscriminator
	rules          string // open, closed, openAtEnd
	ordered        bool
}

type sdSliceDef struct {
	name     string
	basePath string
	rootElem map[string]any
	children map[string]map[string]any // relative path under basePath -> ElementDefinition
}

type sdChoiceInstance struct {
	jsonKey  string
	fhirType string
	value    any
}

func extractFixedConstraint(elemMap map[string]any) (string, any, bool) {
	for k, v := range elemMap {
		if strings.HasPrefix(k, "fixed") && len(k) > len("fixed") {
			c := k[len("fixed")]
			if c >= 'A' && c <= 'Z' && v != nil {
				return k, v, true
			}
		}
	}
	return "", nil, false
}

func extractPatternConstraint(elemMap map[string]any) (string, any, bool) {
	for k, v := range elemMap {
		if strings.HasPrefix(k, "pattern") && len(k) > len("pattern") {
			c := k[len("pattern")]
			if c >= 'A' && c <= 'Z' && v != nil {
				return k, v, true
			}
		}
	}
	return "", nil, false
}

func toFloat64Comparable(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// equalFHIRFixed evaluates deep equality for ElementDefinition.fixed[x].
func equalFHIRFixed(actual, expected any) bool {
	if cw, ok := actual.(sdChoiceInstance); ok {
		actual = cw.value
	}
	if actual == nil || expected == nil {
		return actual == expected
	}
	if f1, ok1 := toFloat64Comparable(actual); ok1 {
		if f2, ok2 := toFloat64Comparable(expected); ok2 {
			return math.Abs(f1-f2) < 1e-9
		}
		return false
	}
	switch exp := expected.(type) {
	case bool:
		actB, ok := actual.(bool)
		return ok && actB == exp
	case string:
		actS := extractStringOrWrapped(actual)
		return actS == exp
	case map[string]any:
		actMap, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		// Every key in expected must match actual, and actual cannot have extra non-metadata keys
		for k, ev := range exp {
			av, exists := actMap[k]
			if !exists || !equalFHIRFixed(av, ev) {
				return false
			}
		}
		for k := range actMap {
			if _, exists := exp[k]; !exists {
				return false
			}
		}
		return true
	case []any:
		actArr, ok := actual.([]any)
		if !ok || len(actArr) != len(exp) {
			return false
		}
		for i := range exp {
			if !equalFHIRFixed(actArr[i], exp[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// matchesFHIRPattern evaluates HL7 FHIR R4 subset/containment matching for ElementDefinition.pattern[x]:
//   - Map fields in pattern must exist and match in actual (extra fields in actual are permitted).
//   - Array items in pattern must each match at least one item in actual (extra items in actual are permitted).
//   - Scalars must be equal.
func matchesFHIRPattern(actual, pattern any) bool {
	if cw, ok := actual.(sdChoiceInstance); ok {
		actual = cw.value
	}
	if pattern == nil {
		return true
	}
	if actual == nil {
		return false
	}
	if f1, ok1 := toFloat64Comparable(actual); ok1 {
		if f2, ok2 := toFloat64Comparable(pattern); ok2 {
			return math.Abs(f1-f2) < 1e-9
		}
		return false
	}
	switch pat := pattern.(type) {
	case bool:
		actB, ok := actual.(bool)
		return ok && actB == pat
	case string:
		return extractStringOrWrapped(actual) == pat
	case map[string]any:
		actMap, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for k, pv := range pat {
			av, exists := actMap[k]
			if !exists || !matchesFHIRPattern(av, pv) {
				return false
			}
		}
		return true
	case []any:
		actArr, ok := actual.([]any)
		if !ok {
			actArr = []any{actual}
		}
		for _, pItem := range pat {
			matchedAny := false
			for _, aItem := range actArr {
				if matchesFHIRPattern(aItem, pItem) {
					matchedAny = true
					break
				}
			}
			if !matchedAny {
				return false
			}
		}
		return true
	}
	return false
}

func parseMaxCardinality(v any) (int, bool) {
	s := strings.TrimSpace(extractStringOrWrapped(v))
	if s == "" || s == "*" {
		return -1, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return -1, false
	}
	return n, true
}

func normalizeChoiceSuffixToFHIRType(suffix string) string {
	switch suffix {
	case "Boolean":
		return "boolean"
	case "Integer":
		return "integer"
	case "Integer64":
		return "integer64"
	case "Decimal":
		return "decimal"
	case "String":
		return "string"
	case "Uri":
		return "uri"
	case "Url":
		return "url"
	case "Canonical":
		return "canonical"
	case "Base64Binary":
		return "base64Binary"
	case "Instant":
		return "instant"
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

func stepContainerField(container any, seg string) []any {
	if cw, ok := container.(sdChoiceInstance); ok {
		container = cw.value
	}
	m, ok := container.(map[string]any)
	if !ok || m == nil {
		return nil
	}
	if strings.HasSuffix(seg, "[x]") {
		prefix := strings.TrimSuffix(seg, "[x]")
		var out []any
		for k, v := range m {
			if strings.HasPrefix(k, prefix) && len(k) > len(prefix) && v != nil && !isEmptyValue(v) {
				c := k[len(prefix)]
				if c >= 'A' && c <= 'Z' {
					suffix := k[len(prefix):]
					out = append(out, sdChoiceInstance{
						jsonKey:  k,
						fhirType: normalizeChoiceSuffixToFHIRType(suffix),
						value:    v,
					})
				}
			}
		}
		return out
	}
	// Also handle FHIRPath-style choice access (e.g., discriminator path "value" matching "valueQuantity")
	if val, exists := m[seg]; exists && val != nil && !isEmptyValue(val) {
		if arr, isArr := val.([]any); isArr {
			var nonNil []any
			for _, it := range arr {
				if it != nil && !isEmptyValue(it) {
					nonNil = append(nonNil, it)
				}
			}
			return nonNil
		}
		return []any{val}
	}
	var choiceOut []any
	for k, v := range m {
		if strings.HasPrefix(k, seg) && len(k) > len(seg) && v != nil && !isEmptyValue(v) {
			c := k[len(seg)]
			if c >= 'A' && c <= 'Z' {
				suffix := k[len(seg):]
				choiceOut = append(choiceOut, sdChoiceInstance{
					jsonKey:  k,
					fhirType: normalizeChoiceSuffixToFHIRType(suffix),
					value:    v,
				})
			}
		}
	}
	return choiceOut
}

func resolveParentContainers(root map[string]any, relPath string) ([]any, string) {
	parts := strings.Split(relPath, ".")
	if len(parts) == 1 {
		return []any{root}, parts[0]
	}
	containers := []any{root}
	for _, seg := range parts[:len(parts)-1] {
		var next []any
		for _, c := range containers {
			next = append(next, stepContainerField(c, seg)...)
		}
		containers = next
		if len(containers) == 0 {
			break
		}
	}
	return containers, parts[len(parts)-1]
}

func resolveRelativeValues(item any, relPath string) ([]any, string) {
	relPath = strings.TrimSpace(relPath)
	if relPath == "" || relPath == "$this" {
		return []any{item}, ""
	}
	// Handle resolve() or ofType(TypeName) segments used in FHIR R4 discriminator paths
	segs := splitDiscriminatorPath(relPath)
	current := []any{item}
	filterType := ""
	for _, seg := range segs {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "$this" {
			continue
		}
		if seg == "resolve()" {
			continue
		}
		if strings.HasPrefix(seg, "ofType(") && strings.HasSuffix(seg, ")") {
			filterType = strings.TrimSpace(seg[len("ofType(") : len(seg)-1])
			var filtered []any
			for _, it := range current {
				if itemMatchesFHIRType(it, filterType) {
					filtered = append(filtered, it)
				}
			}
			current = filtered
			continue
		}
		var next []any
		for _, c := range current {
			next = append(next, stepContainerField(c, seg)...)
		}
		current = next
		if len(current) == 0 {
			break
		}
	}
	return current, filterType
}

func splitDiscriminatorPath(p string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	for i := 0; i < len(p); i++ {
		ch := p[i]
		switch ch {
		case '(':
			depth++
			cur.WriteByte(ch)
		case ')':
			if depth > 0 {
				depth--
			}
			cur.WriteByte(ch)
		case '.':
			if depth == 0 {
				out = append(out, cur.String())
				cur.Reset()
				continue
			}
			cur.WriteByte(ch)
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func inferRuntimeFHIRItemType(item any) string {
	if cw, ok := item.(sdChoiceInstance); ok && cw.fhirType != "" {
		return cw.fhirType
	}
	switch v := item.(type) {
	case bool:
		return "boolean"
	case int, int64:
		return "integer"
	case float64, json.Number:
		return "decimal"
	case string:
		return "string"
	case map[string]any:
		if rt := strings.TrimSpace(extractStringOrWrapped(v["resourceType"])); rt != "" {
			return rt
		}
		if _, ok := v["coding"]; ok {
			return "CodeableConcept"
		}
		if _, ok := v["reference"]; ok {
			return "Reference"
		}
		if _, ok := v["low"]; ok {
			return "Range"
		}
		if _, ok := v["numerator"]; ok {
			return "Ratio"
		}
		if _, ok := v["start"]; ok {
			return "Period"
		}
		if _, ok := v["end"]; ok {
			return "Period"
		}
		if _, ok := v["family"]; ok {
			return "HumanName"
		}
		if _, ok := v["given"]; ok {
			return "HumanName"
		}
		if _, ok := v["url"]; ok {
			return "Extension"
		}
		if _, hasVal := v["value"]; hasVal {
			if _, hasU := v["unit"]; hasU {
				return "Quantity"
			}
			if _, hasC := v["code"]; hasC {
				return "Quantity"
			}
			if _, hasSys := v["system"]; hasSys {
				return "Identifier"
			}
		}
		if _, hasCode := v["code"]; hasCode {
			if _, hasSys := v["system"]; hasSys {
				return "Coding"
			}
		}
	}
	return ""
}

func itemMatchesFHIRType(item any, expectedType string) bool {
	expectedType = strings.TrimSpace(expectedType)
	if expectedType == "" {
		return true
	}
	actual := inferRuntimeFHIRItemType(item)
	if actual == expectedType {
		return true
	}
	switch expectedType {
	case "Quantity", "SimpleQuantity", "Age", "Distance", "Duration", "Count", "MoneyQuantity":
		return actual == "Quantity" || actual == expectedType
	case "uri", "url", "canonical", "oid", "uuid", "code", "id", "markdown", "string", "date", "dateTime", "instant", "time":
		return actual == "string" || actual == expectedType
	case "unsignedInt", "positiveInt", "integer":
		return actual == "integer" || actual == "decimal" || actual == expectedType
	case "decimal":
		return actual == "decimal" || actual == "integer"
	}
	return false
}

func extractAllowedElementTypes(elemMap map[string]any) []string {
	var out []string
	typesArr, ok := elemMap["type"].([]any)
	if !ok {
		return nil
	}
	for _, tItem := range typesArr {
		tm, ok := tItem.(map[string]any)
		if !ok {
			continue
		}
		if code := strings.TrimSpace(extractStringOrWrapped(tm["code"])); code != "" {
			out = append(out, code)
		}
	}
	return out
}

func extractAllowedElementProfiles(elemMap map[string]any) []string {
	var out []string
	typesArr, ok := elemMap["type"].([]any)
	if !ok {
		return nil
	}
	for _, tItem := range typesArr {
		tm, ok := tItem.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"profile", "targetProfile"} {
			if profs, okP := tm[key].([]any); okP {
				for _, p := range profs {
					if u := strings.TrimSpace(extractStringOrWrapped(p)); u != "" {
						out = append(out, u)
					}
				}
			} else if u := strings.TrimSpace(extractStringOrWrapped(tm[key])); u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

func parseSlicingSpec(elemMap map[string]any) (sdSlicingSpec, bool) {
	sMap, ok := elemMap["slicing"].(map[string]any)
	if !ok || sMap == nil {
		return sdSlicingSpec{}, false
	}
	rules := strings.TrimSpace(extractStringOrWrapped(sMap["rules"]))
	if rules == "" {
		rules = "open"
	}
	ordered, _ := sMap["ordered"].(bool)
	var discs []sdDiscriminator
	if dArr, ok := sMap["discriminator"].([]any); ok {
		for _, dItem := range dArr {
			dm, ok := dItem.(map[string]any)
			if !ok {
				continue
			}
			dt := strings.TrimSpace(extractStringOrWrapped(dm["type"]))
			dp := strings.TrimSpace(extractStringOrWrapped(dm["path"]))
			if dt != "" {
				discs = append(discs, sdDiscriminator{discType: dt, path: dp})
			}
		}
	}
	return sdSlicingSpec{
		discriminators: discs,
		rules:          rules,
		ordered:        ordered,
	}, true
}

func elementMatchesSingleConstraintMap(vals []any, cElem map[string]any) bool {
	if _, fixedVal, hasFixed := extractFixedConstraint(cElem); hasFixed {
		for _, v := range vals {
			if equalFHIRFixed(v, fixedVal) {
				return true
			}
		}
		return false
	}
	if _, patVal, hasPat := extractPatternConstraint(cElem); hasPat {
		for _, v := range vals {
			if matchesFHIRPattern(v, patVal) {
				return true
			}
		}
		return false
	}
	return true
}

func normalizeDiscriminatorElemPath(dPath string) string {
	segs := splitDiscriminatorPath(strings.TrimSpace(dPath))
	var kept []string
	for _, s := range segs {
		s = strings.TrimSpace(s)
		if s == "" || s == "$this" || s == "resolve()" {
			continue
		}
		if strings.HasPrefix(s, "ofType(") && strings.HasSuffix(s, ")") {
			continue
		}
		kept = append(kept, s)
	}
	if len(kept) == 0 {
		return "$this"
	}
	return strings.Join(kept, ".")
}

func lookupSliceTargetElement(slice *sdSliceDef, dPath string) map[string]any {
	if dPath != "$this" {
		if ce, ok := slice.children[dPath]; ok {
			return ce
		}
	}
	norm := normalizeDiscriminatorElemPath(dPath)
	if norm == "$this" {
		return slice.rootElem
	}
	if ce, ok := slice.children[norm]; ok {
		return ce
	}
	if ce, ok := slice.children[norm+"[x]"]; ok {
		return ce
	}
	return slice.rootElem
}

func (e *Engine) itemMatchesDiscriminator(item any, slice *sdSliceDef, disc sdDiscriminator) bool {
	dPath := strings.TrimSpace(disc.path)
	if dPath == "" {
		dPath = "$this"
	}

	switch disc.discType {
	case "value", "pattern":
		elemLookupPath := normalizeDiscriminatorElemPath(dPath)
		if elemLookupPath == "$this" {
			if !elementMatchesSingleConstraintMap([]any{item}, slice.rootElem) {
				return false
			}
			// Also check any child fixed/pattern elements defined under slice
			for childRel, childElem := range slice.children {
				_, _, hasF := extractFixedConstraint(childElem)
				_, _, hasP := extractPatternConstraint(childElem)
				if !hasF && !hasP {
					continue
				}
				vals, _ := resolveRelativeValues(item, childRel)
				if len(vals) == 0 || !elementMatchesSingleConstraintMap(vals, childElem) {
					return false
				}
			}
			return true
		}

		// Case 1: Exact child element at dPath/elemLookupPath has fixed[x] or pattern[x]
		matchedConstraint := false
		if childElem, ok := slice.children[elemLookupPath]; ok {
			_, _, hasF := extractFixedConstraint(childElem)
			_, _, hasP := extractPatternConstraint(childElem)
			if hasF || hasP {
				matchedConstraint = true
				vals, _ := resolveRelativeValues(item, dPath)
				if len(vals) == 0 || !elementMatchesSingleConstraintMap(vals, childElem) {
					return false
				}
			}
		}

		// Case 2: Child elements nested beneath elemLookupPath (e.g. dPath="coding", children have "coding.system" & "coding.code")
		prefix := elemLookupPath + "."
		for childRel, childElem := range slice.children {
			if strings.HasPrefix(childRel, prefix) {
				_, _, hasF := extractFixedConstraint(childElem)
				_, _, hasP := extractPatternConstraint(childElem)
				if !hasF && !hasP {
					continue
				}
				matchedConstraint = true
				vals, _ := resolveRelativeValues(item, childRel)
				if len(vals) == 0 || !elementMatchesSingleConstraintMap(vals, childElem) {
					return false
				}
			}
		}

		// Case 3: Ancestor (including slice.rootElem itself!) holds pattern[x]/fixed[x] covering elemLookupPath
		if !matchedConstraint {
			// Check ancestor child paths from longest to shortest, then rootElem
			parts := strings.Split(elemLookupPath, ".")
			for idx := len(parts) - 1; idx >= 1; idx-- {
				ancPath := strings.Join(parts[:idx], ".")
				if ancElem, ok := slice.children[ancPath]; ok {
					_, _, hasF := extractFixedConstraint(ancElem)
					_, _, hasP := extractPatternConstraint(ancElem)
					if hasF || hasP {
						matchedConstraint = true
						vals, _ := resolveRelativeValues(item, ancPath)
						if len(vals) == 0 || !elementMatchesSingleConstraintMap(vals, ancElem) {
							return false
						}
						break
					}
				}
			}
		}
		if !matchedConstraint {
			// Extension shortcut: slice on extension with fixedUri or type.profile when discriminator is url
			if elemLookupPath == "url" {
				if extURL := extractExtensionSliceURL(slice.rootElem); extURL != "" {
					vals, _ := resolveRelativeValues(item, "url")
					if len(vals) == 0 {
						return false
					}
					return strings.TrimSpace(extractStringOrWrapped(vals[0])) == extURL
				}
			}
			_, _, hasRootF := extractFixedConstraint(slice.rootElem)
			_, _, hasRootP := extractPatternConstraint(slice.rootElem)
			if hasRootF || hasRootP {
				return elementMatchesSingleConstraintMap([]any{item}, slice.rootElem)
			}
		}
		return matchedConstraint

	case "type":
		targetElem := lookupSliceTargetElement(slice, dPath)
		vals, ofTypeFilter := resolveRelativeValues(item, dPath)
		if len(vals) == 0 {
			return false
		}
		allowedTypes := extractAllowedElementTypes(targetElem)
		if len(allowedTypes) == 0 && ofTypeFilter != "" {
			allowedTypes = []string{ofTypeFilter}
		}
		if len(allowedTypes) == 0 {
			return false
		}
		for _, v := range vals {
			for _, at := range allowedTypes {
				if itemMatchesFHIRType(v, at) {
					return true
				}
			}
		}
		return false

	case "profile":
		targetElem := lookupSliceTargetElement(slice, dPath)
		vals, _ := resolveRelativeValues(item, dPath)
		if len(vals) == 0 {
			return false
		}
		profs := extractAllowedElementProfiles(targetElem)
		if len(profs) == 0 {
			return false
		}
		for _, v := range vals {
			rawV := v
			if cw, ok := v.(sdChoiceInstance); ok {
				rawV = cw.value
			}
			vm, ok := rawV.(map[string]any)
			if !ok {
				continue
			}
			// Extension profile match via Extension.url
			if extURL := strings.TrimSpace(extractStringOrWrapped(vm["url"])); extURL != "" {
				for _, p := range profs {
					if p == extURL {
						return true
					}
				}
			}
			// Resource meta.profile match or custom StructureDefinition validation
			if meta, okM := vm["meta"].(map[string]any); okM {
				if mpArr, okP := meta["profile"].([]any); okP {
					for _, mp := range mpArr {
						mps := strings.TrimSpace(extractStringOrWrapped(mp))
						for _, p := range profs {
							if mps == p {
								return true
							}
						}
					}
				}
			}
			for _, p := range profs {
				if sd, hasSD := e.lookupCustomStructureDefinition(p); hasSD {
					subOut := &Outcome{}
					rt := strings.TrimSpace(extractStringOrWrapped(vm["resourceType"]))
					e.validateAgainstCustomStructureDefinition(p, sd, rt, vm, subOut)
					if subOut.IsValid() {
						return true
					}
				}
			}
		}
		return false

	case "exists":
		targetElem := lookupSliceTargetElement(slice, dPath)
		vals, _ := resolveRelativeValues(item, dPath)
		maxCard := strings.TrimSpace(extractStringOrWrapped(targetElem["max"]))
		if maxCard == "0" {
			return len(vals) == 0
		}
		return len(vals) > 0
	}

	return false
}

func inferImplicitDiscriminators(slice *sdSliceDef) []sdDiscriminator {
	if extURL := extractExtensionSliceURL(slice.rootElem); extURL != "" {
		return []sdDiscriminator{{discType: "value", path: "url"}}
	}
	if _, _, hasF := extractFixedConstraint(slice.rootElem); hasF {
		return []sdDiscriminator{{discType: "value", path: "$this"}}
	}
	if _, _, hasP := extractPatternConstraint(slice.rootElem); hasP {
		return []sdDiscriminator{{discType: "pattern", path: "$this"}}
	}
	if types := extractAllowedElementTypes(slice.rootElem); len(types) > 0 {
		return []sdDiscriminator{{discType: "type", path: "$this"}}
	}
	var inferred []sdDiscriminator
	for childRel, childElem := range slice.children {
		if _, _, hasF := extractFixedConstraint(childElem); hasF {
			inferred = append(inferred, sdDiscriminator{discType: "value", path: childRel})
		} else if _, _, hasP := extractPatternConstraint(childElem); hasP {
			inferred = append(inferred, sdDiscriminator{discType: "pattern", path: childRel})
		}
	}
	return inferred
}

func (e *Engine) itemMatchesSlice(item any, slice *sdSliceDef, spec sdSlicingSpec) bool {
	discs := spec.discriminators
	if len(discs) == 0 {
		discs = inferImplicitDiscriminators(slice)
	}
	if len(discs) == 0 {
		return false
	}
	for _, d := range discs {
		if !e.itemMatchesDiscriminator(item, slice, d) {
			return false
		}
	}
	return true
}

func (e *Engine) validateElementConstraintsOnContainer(rawProf string, baseExprPath string, relPath string, elemMap map[string]any, container any, outcome *Outcome) {
	containers, leafSeg := resolveParentContainersFromAny(container, relPath)
	if len(containers) == 0 {
		return
	}

	minCard := extractMinCardinality(elemMap["min"])
	maxCard, hasMax := parseMaxCardinality(elemMap["max"])
	fixedKey, fixedVal, hasFixed := extractFixedConstraint(elemMap)
	patKey, patVal, hasPat := extractPatternConstraint(elemMap)
	reqBindingVS := extractRequiredBindingValueSet(elemMap)
	exprField := strings.TrimSuffix(relPath, "[x]")

	var vsCheck func(vsCanonical, system, code string) (bool, bool)
	if reqBindingVS != "" && e != nil && e.opts.EnforceValueSets {
		e.customMu.RLock()
		vsCheck = e.valueSetValidator
		e.customMu.RUnlock()
	}

	for _, parent := range containers {
		vals := stepContainerField(parent, leafSeg)
		count := len(vals)

		if minCard >= 1 && count < minCard {
			outcome.AddError(CodeRequired,
				fmt.Sprintf("Profile %q requires element %q (min=%d, got=%d)", rawProf, exprField, minCard, count),
				baseExprPath+"."+exprField)
		}
		if hasMax && count > maxCard {
			if maxCard == 0 {
				outcome.AddError(CodeStructure,
					fmt.Sprintf("Profile %q prohibits element %q (max=0)", rawProf, exprField),
					baseExprPath+"."+exprField)
			} else {
				outcome.AddError(CodeStructure,
					fmt.Sprintf("Profile %q allows at most %d item(s) for %q (got %d)", rawProf, maxCard, exprField, count),
					baseExprPath+"."+exprField)
			}
		}
		if hasFixed {
			for _, v := range vals {
				if !equalFHIRFixed(v, fixedVal) {
					outcome.AddError(CodeValue,
						formatConstraintValueError(rawProf, baseExprPath+"."+exprField, fixedKey, fixedVal),
						baseExprPath+"."+exprField)
				}
			}
		}
		if hasPat {
			for _, v := range vals {
				if !matchesFHIRPattern(v, patVal) {
					outcome.AddError(CodeValue,
						formatConstraintValueError(rawProf, baseExprPath+"."+exprField, patKey, patVal),
						baseExprPath+"."+exprField)
				}
			}
		}
		if vsCheck != nil {
			for _, v := range vals {
				validateElementRequiredBinding(rawProf, baseExprPath+"."+exprField, reqBindingVS, v, vsCheck, outcome)
			}
		}
	}
}

func extractRequiredBindingValueSet(elemMap map[string]any) string {
	bMap, ok := elemMap["binding"].(map[string]any)
	if !ok {
		return ""
	}
	strength := strings.ToLower(strings.TrimSpace(extractStringOrWrapped(bMap["strength"])))
	if strength != "required" {
		return ""
	}
	return strings.TrimSpace(extractStringOrWrapped(bMap["valueSet"]))
}

func validateElementRequiredBinding(
	rawProf, fullPath, vsCanonical string,
	rawVal any,
	vsCheck func(vsCanonical, system, code string) (bool, bool),
	outcome *Outcome,
) {
	if cw, ok := rawVal.(sdChoiceInstance); ok {
		rawVal = cw.value
	}
	switch v := rawVal.(type) {
	case string:
		code := strings.TrimSpace(v)
		if code == "" {
			return
		}
		if known, valid := vsCheck(vsCanonical, "", code); known && !valid {
			outcome.AddError(CodeInvalid,
				fmt.Sprintf("Profile %q requires %s code %q to belong to ValueSet %q", rawProf, fullPath, code, vsCanonical),
				fullPath)
		}
	case map[string]any:
		// Check CodeableConcept (coding[]) vs single Coding (system + code)
		if codings, hasCodings := v["coding"].([]any); hasCodings {
			anyKnown := false
			anyValid := false
			var badCode string
			for _, item := range codings {
				cm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				sys := strings.TrimSpace(extractStringOrWrapped(cm["system"]))
				code := strings.TrimSpace(extractStringOrWrapped(cm["code"]))
				if code == "" {
					continue
				}
				known, valid := vsCheck(vsCanonical, sys, code)
				if known {
					anyKnown = true
					if valid {
						anyValid = true
						break
					}
					if badCode == "" {
						badCode = code
					}
				}
			}
			if anyKnown && !anyValid {
				outcome.AddError(CodeInvalid,
					fmt.Sprintf("Profile %q requires %s coding (%q) to belong to ValueSet %q", rawProf, fullPath, badCode, vsCanonical),
					fullPath)
			}
			return
		}
		code := strings.TrimSpace(extractStringOrWrapped(v["code"]))
		if code == "" {
			return
		}
		sys := strings.TrimSpace(extractStringOrWrapped(v["system"]))
		if known, valid := vsCheck(vsCanonical, sys, code); known && !valid {
			outcome.AddError(CodeInvalid,
				fmt.Sprintf("Profile %q requires %s coding %q to belong to ValueSet %q", rawProf, fullPath, code, vsCanonical),
				fullPath)
		}
	}
}

func resolveParentContainersFromAny(root any, relPath string) ([]any, string) {
	if cw, ok := root.(sdChoiceInstance); ok {
		root = cw.value
	}
	m, ok := root.(map[string]any)
	if !ok {
		return nil, relPath
	}
	return resolveParentContainers(m, relPath)
}

func formatConstraintValueError(rawProf, fullPath, key string, expected any) string {
	switch v := expected.(type) {
	case string:
		return fmt.Sprintf("Profile %q requires %s to match %s (%q)", rawProf, fullPath, key, v)
	case bool, int, int64, float64, json.Number:
		return fmt.Sprintf("Profile %q requires %s to match %s (%v)", rawProf, fullPath, key, v)
	default:
		return fmt.Sprintf("Profile %q requires %s to match %s", rawProf, fullPath, key)
	}
}

func isExtensionSlicePath(basePath string) bool {
	return strings.HasSuffix(basePath, ".extension") || strings.HasSuffix(basePath, ".modifierExtension")
}

func (e *Engine) evaluateStructureDefinitionElements(rawProf string, resTypeStr string, resMap map[string]any, elements []map[string]any, outcome *Outcome) {
	slicingSpecs := make(map[string]sdSlicingSpec)
	sliceOrder := make(map[string][]*sdSliceDef)
	sliceLookup := make(map[string]*sdSliceDef) // basePath + ":" + sliceName

	var unslicedElements []map[string]any
	seenConstraints := make(map[string]bool)

	var activeSlice *sdSliceDef

	for _, elemMap := range elements {
		elemPath := strings.TrimSpace(extractStringOrWrapped(elemMap["path"]))
		if elemPath == "" {
			elemPath = resTypeStr
		}
		elemID := strings.TrimSpace(extractStringOrWrapped(elemMap["id"]))
		sliceName := strings.TrimSpace(extractStringOrWrapped(elemMap["sliceName"]))

		if spec, hasSlicing := parseSlicingSpec(elemMap); hasSlicing {
			slicingSpecs[elemPath] = spec
			activeSlice = nil
		}

		// Check if this element starts a named slice
		if sliceName != "" {
			key := elemPath + ":" + sliceName
			sd, exists := sliceLookup[key]
			if !exists {
				sd = &sdSliceDef{
					name:     sliceName,
					basePath: elemPath,
					rootElem: elemMap,
					children: make(map[string]map[string]any),
				}
				sliceLookup[key] = sd
				sliceOrder[elemPath] = append(sliceOrder[elemPath], sd)
				if _, hasSpec := slicingSpecs[elemPath]; !hasSpec {
					slicingSpecs[elemPath] = sdSlicingSpec{rules: "open"}
				}
			}
			activeSlice = sd
			continue
		}

		// Check if this element belongs to a slice via ElementDefinition.id (e.g. "Observation.component:systolic.code")
		matchedByID := false
		if colonIdx := strings.IndexByte(elemID, ':'); colonIdx != -1 {
			baseP := elemID[:colonIdx]
			rest := elemID[colonIdx+1:]
			dotIdx := strings.IndexByte(rest, '.')
			if dotIdx != -1 {
				sName := rest[:dotIdx]
				subRel := rest[dotIdx+1:]
				// Strip any nested slice names in subRel if present
				if !strings.Contains(sName, ".") {
					key := baseP + ":" + sName
					if sd, ok := sliceLookup[key]; ok && subRel != "" {
						sd.children[subRel] = elemMap
						matchedByID = true
					}
				}
			}
		}
		if matchedByID {
			continue
		}

		// Or positional child immediately following an active slice root
		if activeSlice != nil {
			if subRel, ok := strings.CutPrefix(elemPath, activeSlice.basePath+"."); ok && subRel != "" {
				activeSlice.children[subRel] = elemMap
				continue
			}
			activeSlice = nil
		}

		// Top-level / unsliced element: evaluate FHIRPath constraints first
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

				if after, ok := strings.CutPrefix(elemPath, resTypeStr+"."); ok && after != "" {
					vals, _ := resolveRelativeValues(resMap, after)
					if len(vals) == 0 {
						continue
					}
				}
				results := e.fhirPathEval(resMap, expr)
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

		unslicedElements = append(unslicedElements, elemMap)
	}

	// 1. Validate all unsliced element constraints (root children & nested dotted paths)
	for _, elemMap := range unslicedElements {
		elemPath := strings.TrimSpace(extractStringOrWrapped(elemMap["path"]))
		relPath, isChild := strings.CutPrefix(elemPath, resTypeStr+".")
		if !isChild || relPath == "" {
			continue
		}
		e.validateElementConstraintsOnContainer(rawProf, resTypeStr, relPath, elemMap, resMap, outcome)
	}

	// 2. Validate all sliced elements (discriminators, slice min/max, slice children, closed/openAtEnd/ordered rules)
	for basePath, slices := range sliceOrder {
		relBasePath, isChild := strings.CutPrefix(basePath, resTypeStr+".")
		if !isChild || relBasePath == "" {
			continue
		}
		spec := slicingSpecs[basePath]
		parentContainers, leafSeg := resolveParentContainers(resMap, relBasePath)

		// If parent container itself is absent, still enforce slice min >= 1 if parent is root
		if len(parentContainers) == 0 {
			continue
		}

		for _, parent := range parentContainers {
			items := stepContainerField(parent, leafSeg)
			sliceCounts := make([]int, len(slices))
			itemMatchedSliceIdx := make([]int, len(items))
			for i := range items {
				itemMatchedSliceIdx[i] = -1
			}

			for itemIdx, item := range items {
				for sIdx, sd := range slices {
					if e.itemMatchesSlice(item, sd, spec) {
						if itemMatchedSliceIdx[itemIdx] == -1 {
							itemMatchedSliceIdx[itemIdx] = sIdx
						}
						sliceCounts[sIdx]++

						// Validate all child constraints of matched slice against this item
						sliceExprPrefix := fmt.Sprintf("%s:%s", strings.TrimSuffix(basePath, "[x]"), sd.name)
						if fixedKey, fixedVal, hasF := extractFixedConstraint(sd.rootElem); hasF && (fixedKey != "fixedUri" || !isExtensionSlicePath(basePath)) {
							if !equalFHIRFixed(item, fixedVal) {
								outcome.AddError(CodeValue,
									fmt.Sprintf("Profile %q slice %q value mismatch", rawProf, sd.name),
									sliceExprPrefix)
							}
						}
						if _, patVal, hasP := extractPatternConstraint(sd.rootElem); hasP {
							if !matchesFHIRPattern(item, patVal) {
								outcome.AddError(CodeValue,
									fmt.Sprintf("Profile %q slice %q pattern mismatch", rawProf, sd.name),
									sliceExprPrefix)
							}
						}
						for childRel, childElem := range sd.children {
							e.validateElementConstraintsOnContainer(rawProf, sliceExprPrefix, childRel, childElem, item, outcome)
						}
					}
				}
			}

			// Check slice min / max cardinalities
			for sIdx, sd := range slices {
				minC := extractMinCardinality(sd.rootElem["min"])
				maxC, hasMaxC := parseMaxCardinality(sd.rootElem["max"])
				sliceExpr := fmt.Sprintf("%s:%s", strings.TrimSuffix(basePath, "[x]"), sd.name)

				if minC >= 1 && sliceCounts[sIdx] < minC {
					outcome.AddError(CodeRequired,
						fmt.Sprintf("Profile %q requires slice %q on %s (min=%d, matched=%d)", rawProf, sd.name, basePath, minC, sliceCounts[sIdx]),
						sliceExpr)
				}
				if hasMaxC && sliceCounts[sIdx] > maxC {
					outcome.AddError(CodeStructure,
						fmt.Sprintf("Profile %q slice %q on %s allows at most %d item(s) (matched=%d)", rawProf, sd.name, basePath, maxC, sliceCounts[sIdx]),
						sliceExpr)
				}
			}

			// Enforce slicing.rules ("closed", "openAtEnd") and slicing.ordered
			switch spec.rules {
			case "closed":
				for itemIdx, matchedIdx := range itemMatchedSliceIdx {
					if matchedIdx == -1 {
						outcome.AddError(CodeStructure,
							fmt.Sprintf("Profile %q closed slicing on %s rejected unmatched element at index %d", rawProf, basePath, itemIdx),
							fmt.Sprintf("%s[%d]", strings.TrimSuffix(basePath, "[x]"), itemIdx))
					}
				}
			case "openAtEnd":
				seenUnmatched := false
				for itemIdx, matchedIdx := range itemMatchedSliceIdx {
					if matchedIdx == -1 {
						seenUnmatched = true
					} else if seenUnmatched {
						outcome.AddError(CodeStructure,
							fmt.Sprintf("Profile %q openAtEnd slicing on %s prohibits matched slice %q at index %d after unmatched element", rawProf, basePath, slices[matchedIdx].name, itemIdx),
							fmt.Sprintf("%s[%d]", strings.TrimSuffix(basePath, "[x]"), itemIdx))
					}
				}
			}

			if spec.ordered {
				maxSliceSeen := -1
				for itemIdx, matchedIdx := range itemMatchedSliceIdx {
					if matchedIdx == -1 {
						continue
					}
					if matchedIdx < maxSliceSeen {
						outcome.AddError(CodeStructure,
							fmt.Sprintf("Profile %q ordered slicing on %s violated at index %d (slice %q appeared out of order)", rawProf, basePath, itemIdx, slices[matchedIdx].name),
							fmt.Sprintf("%s[%d]", strings.TrimSuffix(basePath, "[x]"), itemIdx))
					} else {
						maxSliceSeen = matchedIdx
					}
				}
			}
		}
	}
}
