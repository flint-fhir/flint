package server

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

type fhirPathContext struct {
	root     map[string]any
	thisItem any
	index    int
	total    int
}

// evalFHIRPath evaluates a full HL7 FHIRPath R4 (http://hl7.org/fhirpath/N1/) expression
// against a root FHIR resource map.
func evalFHIRPath(root map[string]any, expr string) []any {
	expr = strings.TrimSpace(expr)
	if expr == "" || root == nil {
		return nil
	}
	ctx := fhirPathContext{
		root:     root,
		thisItem: root,
		index:    0,
		total:    1,
	}
	return evalFHIRPathExpr([]any{root}, ctx, expr)
}

func evalFHIRPathExpr(input []any, ctx fhirPathContext, expr string) []any {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil
	}
	if hasOuterParens(expr) {
		return evalFHIRPathExpr(input, ctx, expr[1:len(expr)-1])
	}

	// 1. implies (lowest precedence, right-to-left)
	if lhs, _, rhs, ok := splitTopLevelKeywordOp(expr, []string{"implies"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		lBool, lHas := singletonBool(lVals)
		rBool, rHas := singletonBool(rVals)
		if lHas {
			if !lBool {
				return []any{true}
			}
			if rHas {
				return []any{rBool}
			}
			return nil
		}
		if rHas && rBool {
			return []any{true}
		}
		return nil
	}

	// 2. or, xor (three-valued logic per FHIRPath R4 §6.5)
	if lhs, op, rhs, ok := splitTopLevelKeywordOp(expr, []string{"or", "xor"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		lBool, lHas := singletonBool(lVals)
		rBool, rHas := singletonBool(rVals)
		if op == "or" {
			if (lHas && lBool) || (rHas && rBool) {
				return []any{true}
			}
			if lHas && rHas && !lBool && !rBool {
				return []any{false}
			}
			return nil
		}
		// xor
		if lHas && rHas {
			return []any{lBool != rBool}
		}
		return nil
	}

	// 3. and (three-valued logic per FHIRPath R4 §6.5)
	if lhs, _, rhs, ok := splitTopLevelKeywordOp(expr, []string{"and"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		lBool, lHas := singletonBool(lVals)
		rBool, rHas := singletonBool(rVals)
		if (lHas && !lBool) || (rHas && !rBool) {
			return []any{false}
		}
		if lHas && rHas && lBool && rBool {
			return []any{true}
		}
		return nil
	}

	// 4. Membership: in, contains
	if lhs, op, rhs, ok := splitTopLevelKeywordOp(expr, []string{"in", "contains"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		if op == "in" {
			if len(lVals) == 0 {
				return nil
			}
			if len(rVals) == 0 {
				return []any{false}
			}
			for _, rv := range rVals {
				if fhirPathEqual(lVals[0], rv) {
					return []any{true}
				}
			}
			return []any{false}
		}
		// contains
		if len(rVals) == 0 {
			return nil
		}
		if len(lVals) == 0 {
			return []any{false}
		}
		for _, lv := range lVals {
			if fhirPathEqual(lv, rVals[0]) {
				return []any{true}
			}
		}
		return []any{false}
	}

	// 5. Equality / Equivalence / Comparison: !=, !~, >=, <=, =, ~, >, <
	if lhs, op, rhs, ok := splitTopLevelSymbolOp(expr, []string{"!=", "!~", ">=", "<=", "=", "~", ">", "<"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		return evalFHIRPathComparison(lVals, op, rVals)
	}

	// 6. Union operator: |
	if lhs, _, rhs, ok := splitTopLevelSymbolOp(expr, []string{"|"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		return fhirPathDistinct(append(append([]any{}, lVals...), rVals...))
	}

	// 7. Additive operators: +, -, &
	if lhs, op, rhs, ok := splitTopLevelAdditiveOp(expr); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		return evalFHIRPathAdditive(lVals, op, rVals)
	}

	// 8. Multiplicative operators: *, /, div, mod
	if lhs, op, rhs, ok := splitTopLevelMultiplicativeOp(expr); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		return evalFHIRPathMultiplicative(lVals, op, rVals)
	}

	// 9. Type operators: is, as
	if lhs, op, rhs, ok := splitTopLevelKeywordOp(expr, []string{"is", "as"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		typeName := strings.Trim(strings.TrimSpace(rhs), "'\"")
		if op == "is" {
			if len(lVals) == 0 {
				return nil
			}
			return []any{fhirPathMatchesType(lVals[0], typeName)}
		}
		// as
		if len(lVals) == 1 && fhirPathMatchesType(lVals[0], typeName) {
			return lVals
		}
		return nil
	}

	// 10. Unary + / -
	if strings.HasPrefix(expr, "-") && len(expr) > 1 {
		sub := evalFHIRPathExpr(input, ctx, expr[1:])
		if len(sub) == 1 {
			switch v := sub[0].(type) {
			case int:
				return []any{-v}
			case int64:
				return []any{-v}
			case float64:
				return []any{-v}
			}
		}
		return nil
	}
	if strings.HasPrefix(expr, "+") && len(expr) > 1 {
		return evalFHIRPathExpr(input, ctx, expr[1:])
	}

	// 11. Path / Function / Literal chain
	return evalFHIRPathChain(input, ctx, expr)
}

func hasOuterParens(expr string) bool {
	if len(expr) < 2 || expr[0] != '(' || expr[len(expr)-1] != ')' {
		return false
	}
	depth := 0
	inQuote := byte(0)
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			if ch == inQuote && (i == 0 || expr[i-1] != '\\') {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			continue
		}
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i < len(expr)-1 {
				return false
			}
		}
	}
	return depth == 0
}

func isWordBoundaryChar(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '(' || b == ')' || b == '[' || b == ']' || b == '\'' || b == '"'
}

func splitTopLevelKeywordOp(expr string, keywords []string) (string, string, string, bool) {
	depth := 0
	inQuote := byte(0)
	lastPos := -1
	lastKW := ""

	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			if ch == inQuote && (i == 0 || expr[i-1] != '\\') {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && i > 0 {
				for _, kw := range keywords {
					if i+len(kw) < len(expr) && strings.EqualFold(expr[i:i+len(kw)], kw) {
						prev := expr[i-1]
						next := expr[i+len(kw)]
						// Require whitespace before and after keyword operator (or paren boundary with space on at least one side)
						prevSpace := prev == ' ' || prev == '\t' || prev == '\n' || prev == ')'
						nextSpace := next == ' ' || next == '\t' || next == '\n' || (next == '(' && kw != "contains" && kw != "in")
						if prevSpace && nextSpace && isWordBoundaryChar(prev) && isWordBoundaryChar(next) {
							// Make sure it's not preceded by '.' (like .contains(...))
							trimmedLeft := strings.TrimSpace(expr[:i])
							if trimmedLeft != "" && !strings.HasSuffix(trimmedLeft, ".") {
								lastPos = i
								lastKW = strings.ToLower(kw)
							}
						}
					}
				}
			}
		}
	}
	if lastPos != -1 {
		return strings.TrimSpace(expr[:lastPos]), lastKW, strings.TrimSpace(expr[lastPos+len(lastKW):]), true
	}
	return "", "", "", false
}

func splitTopLevelSymbolOp(expr string, ops []string) (string, string, string, bool) {
	depth := 0
	inQuote := byte(0)
	lastPos := -1
	lastOp := ""

	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			if ch == inQuote && (i == 0 || expr[i-1] != '\\') {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && i > 0 {
				for _, op := range ops {
					if strings.HasPrefix(expr[i:], op) {
						lhs := strings.TrimSpace(expr[:i])
						rhs := strings.TrimSpace(expr[i+len(op):])
						if lhs != "" && rhs != "" {
							lastPos = i
							lastOp = op
							i += len(op) - 1
							break
						}
					}
				}
			}
		}
	}
	if lastPos != -1 {
		return strings.TrimSpace(expr[:lastPos]), lastOp, strings.TrimSpace(expr[lastPos+len(lastOp):]), true
	}
	return "", "", "", false
}

func isOperatorTrailing(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	last := s[len(s)-1]
	switch last {
	case '=', '!', '<', '>', '+', '-', '*', '/', '&', '|', '(', '[', ',':
		return true
	}
	lower := strings.ToLower(s)
	for _, kw := range []string{" and", " or", " xor", " implies", " div", " mod", " in", " contains"} {
		if strings.HasSuffix(lower, kw) {
			return true
		}
	}
	return false
}

func splitTopLevelAdditiveOp(expr string) (string, string, string, bool) {
	depth := 0
	inQuote := byte(0)
	lastPos := -1
	lastOp := ""

	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			if ch == inQuote && (i == 0 || expr[i-1] != '\\') {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case '+', '-', '&':
			if depth == 0 && i > 0 && i < len(expr)-1 {
				lhs := expr[:i]
				rhs := strings.TrimSpace(expr[i+1:])
				if rhs == "" || isOperatorTrailing(lhs) {
					continue
				}
				// Avoid splitting inside @date literals like @2026-01-01
				trimmedLHS := strings.TrimSpace(lhs)
				if ch == '-' && strings.HasPrefix(trimmedLHS, "@") && !strings.ContainsAny(trimmedLHS, " \t)") {
					continue
				}
				lastPos = i
				lastOp = string(ch)
			}
		}
	}
	if lastPos != -1 {
		return strings.TrimSpace(expr[:lastPos]), lastOp, strings.TrimSpace(expr[lastPos+1:]), true
	}
	return "", "", "", false
}

func splitTopLevelMultiplicativeOp(expr string) (string, string, string, bool) {
	depth := 0
	inQuote := byte(0)
	lastPos := -1
	lastOp := ""

	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			if ch == inQuote && (i == 0 || expr[i-1] != '\\') {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case '*', '/':
			if depth == 0 && i > 0 && i < len(expr)-1 {
				lhs := expr[:i]
				rhs := strings.TrimSpace(expr[i+1:])
				if rhs != "" && !isOperatorTrailing(lhs) {
					lastPos = i
					lastOp = string(ch)
				}
			}
		default:
			if depth == 0 && i > 0 {
				for _, kw := range []string{"div", "mod"} {
					if i+len(kw) < len(expr) && strings.EqualFold(expr[i:i+len(kw)], kw) {
						prev := expr[i-1]
						next := expr[i+len(kw)]
						if (prev == ' ' || prev == '\t' || prev == ')') && (next == ' ' || next == '\t' || next == '(') {
							trimmedLeft := strings.TrimSpace(expr[:i])
							if trimmedLeft != "" && !strings.HasSuffix(trimmedLeft, ".") {
								lastPos = i
								lastOp = strings.ToLower(kw)
							}
						}
					}
				}
			}
		}
	}
	if lastPos != -1 {
		return strings.TrimSpace(expr[:lastPos]), lastOp, strings.TrimSpace(expr[lastPos+len(lastOp):]), true
	}
	return "", "", "", false
}

func evalFHIRPathChain(input []any, ctx fhirPathContext, expr string) []any {
	segs := splitFHIRPathSegments(expr)
	if len(segs) == 0 {
		return nil
	}

	current := input
	for idx, rawSeg := range segs {
		baseSeg, indexExprs := splitSegmentIndexers(rawSeg)
		if idx == 0 {
			// Check if baseSeg is a literal, environment variable, parenthesized expression, or resourceType
			if litVals, isLit := evalFHIRPathPrimaryLiteral(baseSeg, input, ctx); isLit {
				current = litVals
				for _, ie := range indexExprs {
					current = applyFHIRPathIndexer(current, input, ctx, ie)
				}
				continue
			}
			if rt, _ := ctx.root["resourceType"].(string); rt != "" && strings.EqualFold(baseSeg, rt) {
				if len(indexExprs) > 0 {
					for _, ie := range indexExprs {
						current = applyFHIRPathIndexer(current, input, ctx, ie)
					}
				}
				continue
			}
		}

		if baseSeg != "" {
			current = evalFHIRPathStep(current, input, ctx, baseSeg)
		}
		for _, ie := range indexExprs {
			current = applyFHIRPathIndexer(current, input, ctx, ie)
		}
	}
	return current
}

func splitSegmentIndexers(seg string) (string, []string) {
	seg = strings.TrimSpace(seg)
	depthParens := 0
	inQuote := byte(0)
	bracketStart := -1
	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		if inQuote != 0 {
			if ch == inQuote && (i == 0 || seg[i-1] != '\\') {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			continue
		}
		switch ch {
		case '(':
			depthParens++
		case ')':
			if depthParens > 0 {
				depthParens--
			}
		case '[':
			if depthParens == 0 {
				bracketStart = i
				goto foundBracket
			}
		}
	}
foundBracket:
	if bracketStart == -1 {
		return seg, nil
	}
	base := strings.TrimSpace(seg[:bracketStart])
	rest := seg[bracketStart:]
	var idxExprs []string
	for len(rest) > 0 {
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "[") {
			break
		}
		d := 0
		q := byte(0)
		endIdx := -1
		for i := 0; i < len(rest); i++ {
			c := rest[i]
			if q != 0 {
				if c == q && (i == 0 || rest[i-1] != '\\') {
					q = 0
				}
				continue
			}
			if c == '\'' || c == '"' {
				q = c
				continue
			}
			if c == '[' {
				d++
			} else if c == ']' {
				d--
				if d == 0 {
					endIdx = i
					break
				}
			}
		}
		if endIdx == -1 {
			break
		}
		idxExprs = append(idxExprs, strings.TrimSpace(rest[1:endIdx]))
		rest = rest[endIdx+1:]
	}
	return base, idxExprs
}

func applyFHIRPathIndexer(current []any, scopeInput []any, ctx fhirPathContext, idxExpr string) []any {
	idxVals := evalFHIRPathExpr(scopeInput, ctx, idxExpr)
	if len(idxVals) != 1 {
		return nil
	}
	idx, ok := toInt(idxVals[0])
	if !ok || idx < 0 || idx >= len(current) {
		return nil
	}
	return []any{current[idx]}
}

func evalFHIRPathPrimaryLiteral(seg string, input []any, ctx fhirPathContext) ([]any, bool) {
	seg = strings.TrimSpace(seg)
	if seg == "{}" {
		return nil, true
	}
	if hasOuterParens(seg) {
		return evalFHIRPathExpr(input, ctx, seg[1:len(seg)-1]), true
	}
	if strings.EqualFold(seg, "true") {
		return []any{true}, true
	}
	if strings.EqualFold(seg, "false") {
		return []any{false}, true
	}
	if (strings.HasPrefix(seg, "'") && strings.HasSuffix(seg, "'")) ||
		(strings.HasPrefix(seg, "\"") && strings.HasSuffix(seg, "\"")) {
		if len(seg) >= 2 {
			unquoted := seg[1 : len(seg)-1]
			unquoted = strings.ReplaceAll(unquoted, `\'`, `'`)
			unquoted = strings.ReplaceAll(unquoted, `\"`, `"`)
			unquoted = strings.ReplaceAll(unquoted, `\\`, `\`)
			return []any{unquoted}, true
		}
	}
	if strings.HasPrefix(seg, "@") && len(seg) > 1 {
		return []any{strings.TrimPrefix(seg, "@")}, true
	}
	switch seg {
	case "$this":
		if ctx.thisItem != nil {
			return []any{ctx.thisItem}, true
		}
		return input, true
	case "$index":
		return []any{ctx.index}, true
	case "$total":
		return []any{ctx.total}, true
	case "%resource", "%rootResource", "%context":
		if ctx.root != nil {
			return []any{ctx.root}, true
		}
		return nil, true
	case "%ucum":
		return []any{"http://unitsofmeasure.org"}, true
	case "%sct":
		return []any{"http://snomed.info/sct"}, true
	case "%loinc":
		return []any{"http://loinc.org"}, true
	}
	if iv, err := strconv.Atoi(seg); err == nil {
		return []any{iv}, true
	}
	if fv, err := strconv.ParseFloat(seg, 64); err == nil {
		return []any{fv}, true
	}
	return nil, false
}

func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

func parseFunctionCall(seg string) (string, []string, bool) {
	seg = strings.TrimSpace(seg)
	openIdx := strings.IndexByte(seg, '(')
	if openIdx <= 0 || !strings.HasSuffix(seg, ")") {
		return "", nil, false
	}
	fnName := strings.TrimSpace(seg[:openIdx])
	for i := 0; i < len(fnName); i++ {
		if !isIdentChar(fnName[i]) {
			return "", nil, false
		}
	}
	inner := strings.TrimSpace(seg[openIdx+1 : len(seg)-1])
	if inner == "" {
		return fnName, nil, true
	}
	var args []string
	var cur strings.Builder
	depth := 0
	inQuote := byte(0)
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		if inQuote != 0 {
			cur.WriteByte(ch)
			if ch == inQuote && (i == 0 || inner[i-1] != '\\') {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			cur.WriteByte(ch)
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
			cur.WriteByte(ch)
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
			cur.WriteByte(ch)
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(cur.String()))
				cur.Reset()
				continue
			}
			cur.WriteByte(ch)
		default:
			cur.WriteByte(ch)
		}
	}
	args = append(args, strings.TrimSpace(cur.String()))
	return fnName, args, true
}

func evalFHIRPathStep(current []any, scopeInput []any, ctx fhirPathContext, seg string) []any {
	if fnName, args, isFn := parseFunctionCall(seg); isFn {
		return evalFHIRPathFunction(current, scopeInput, ctx, fnName, args)
	}

	var next []any
	for _, it := range current {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if v, exists := m[seg]; exists {
			if arr, isArr := v.([]any); isArr {
				next = append(next, arr...)
			} else if v != nil {
				next = append(next, v)
			}
			continue
		}
		// Polymorphic choice[x] resolution: e.g., "value" -> valueQuantity, valueString, etc., or "effective" -> effectiveDateTime
		prefix := seg
		matchedChoice := false
		for k, v := range m {
			if strings.HasPrefix(k, prefix) && len(k) > len(prefix) && v != nil {
				nextChar := k[len(prefix)]
				if nextChar >= 'A' && nextChar <= 'Z' {
					if arr, isArr := v.([]any); isArr {
						next = append(next, arr...)
					} else {
						next = append(next, v)
					}
					matchedChoice = true
				}
			}
		}
		if matchedChoice {
			continue
		}
	}
	return next
}

func evalFHIRPathFunction(current []any, scopeInput []any, ctx fhirPathContext, fnName string, args []string) []any {
	switch strings.ToLower(fnName) {
	case "exists":
		if len(args) == 0 {
			return []any{len(current) > 0}
		}
		for i, it := range current {
			subCtx := fhirPathContext{root: ctx.root, thisItem: it, index: i, total: len(current)}
			vals := evalFHIRPathExpr([]any{it}, subCtx, args[0])
			if b, ok := singletonBool(vals); ok && b {
				return []any{true}
			}
		}
		return []any{false}

	case "empty":
		return []any{len(current) == 0}

	case "all":
		if len(args) == 0 {
			return []any{true}
		}
		for i, it := range current {
			subCtx := fhirPathContext{root: ctx.root, thisItem: it, index: i, total: len(current)}
			vals := evalFHIRPathExpr([]any{it}, subCtx, args[0])
			if b, ok := singletonBool(vals); !ok || !b {
				return []any{false}
			}
		}
		return []any{true}

	case "alltrue":
		for _, it := range current {
			b, ok := it.(bool)
			if !ok || !b {
				return []any{false}
			}
		}
		return []any{true}

	case "anytrue":
		for _, it := range current {
			if b, ok := it.(bool); ok && b {
				return []any{true}
			}
		}
		return []any{false}

	case "allfalse":
		for _, it := range current {
			b, ok := it.(bool)
			if !ok || b {
				return []any{false}
			}
		}
		return []any{true}

	case "anyfalse":
		for _, it := range current {
			if b, ok := it.(bool); ok && !b {
				return []any{true}
			}
		}
		return []any{false}

	case "subsetof":
		if len(args) != 1 {
			return nil
		}
		other := evalFHIRPathExpr(scopeInput, ctx, args[0])
		for _, it := range current {
			found := false
			for _, ov := range other {
				if fhirPathEqual(it, ov) {
					found = true
					break
				}
			}
			if !found {
				return []any{false}
			}
		}
		return []any{true}

	case "supersetof":
		if len(args) != 1 {
			return nil
		}
		other := evalFHIRPathExpr(scopeInput, ctx, args[0])
		for _, ov := range other {
			found := false
			for _, it := range current {
				if fhirPathEqual(it, ov) {
					found = true
					break
				}
			}
			if !found {
				return []any{false}
			}
		}
		return []any{true}

	case "count":
		return []any{len(current)}

	case "distinct":
		return fhirPathDistinct(current)

	case "isdistinct":
		return []any{len(fhirPathDistinct(current)) == len(current)}

	case "where":
		if len(args) == 0 {
			return current
		}
		var filtered []any
		for i, it := range current {
			subCtx := fhirPathContext{root: ctx.root, thisItem: it, index: i, total: len(current)}
			vals := evalFHIRPathExpr([]any{it}, subCtx, args[0])
			if b, ok := singletonBool(vals); ok && b {
				filtered = append(filtered, it)
			}
		}
		return filtered

	case "select":
		if len(args) == 0 {
			return current
		}
		var projected []any
		for i, it := range current {
			subCtx := fhirPathContext{root: ctx.root, thisItem: it, index: i, total: len(current)}
			vals := evalFHIRPathExpr([]any{it}, subCtx, args[0])
			projected = append(projected, vals...)
		}
		return projected

	case "repeat":
		if len(args) == 0 {
			return current
		}
		var result []any
		queue := append([]any{}, current...)
		for len(queue) > 0 {
			it := queue[0]
			queue = queue[1:]
			subCtx := fhirPathContext{root: ctx.root, thisItem: it, index: 0, total: 1}
			children := evalFHIRPathExpr([]any{it}, subCtx, args[0])
			for _, ch := range children {
				already := false
				for _, existing := range result {
					if fhirPathEqual(existing, ch) {
						already = true
						break
					}
				}
				if !already {
					result = append(result, ch)
					queue = append(queue, ch)
				}
			}
		}
		return result

	case "oftype":
		if len(args) != 1 {
			return nil
		}
		typeName := strings.Trim(args[0], "'\"")
		var matched []any
		for _, it := range current {
			if fhirPathMatchesType(it, typeName) {
				matched = append(matched, it)
			}
		}
		return matched

	case "single":
		if len(current) == 1 {
			return current
		}
		return nil

	case "first":
		if len(current) > 0 {
			return []any{current[0]}
		}
		return nil

	case "last":
		if len(current) > 0 {
			return []any{current[len(current)-1]}
		}
		return nil

	case "tail":
		if len(current) > 1 {
			return current[1:]
		}
		return nil

	case "skip":
		if len(args) != 1 {
			return current
		}
		nVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(nVals) != 1 {
			return nil
		}
		n, ok := toInt(nVals[0])
		if !ok {
			return nil
		}
		if n <= 0 {
			return current
		}
		if n >= len(current) {
			return nil
		}
		return current[n:]

	case "take":
		if len(args) != 1 {
			return nil
		}
		nVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(nVals) != 1 {
			return nil
		}
		n, ok := toInt(nVals[0])
		if !ok || n <= 0 {
			return nil
		}
		if n >= len(current) {
			return current
		}
		return current[:n]

	case "union":
		if len(args) != 1 {
			return current
		}
		other := evalFHIRPathExpr(scopeInput, ctx, args[0])
		return fhirPathDistinct(append(append([]any{}, current...), other...))

	case "combine":
		if len(args) != 1 {
			return current
		}
		other := evalFHIRPathExpr(scopeInput, ctx, args[0])
		return append(append([]any{}, current...), other...)

	case "intersect":
		if len(args) != 1 {
			return nil
		}
		other := evalFHIRPathExpr(scopeInput, ctx, args[0])
		var out []any
		for _, it := range current {
			for _, ov := range other {
				if fhirPathEqual(it, ov) {
					out = append(out, it)
					break
				}
			}
		}
		return fhirPathDistinct(out)

	case "exclude":
		if len(args) != 1 {
			return current
		}
		other := evalFHIRPathExpr(scopeInput, ctx, args[0])
		var out []any
		for _, it := range current {
			found := false
			for _, ov := range other {
				if fhirPathEqual(it, ov) {
					found = true
					break
				}
			}
			if !found {
				out = append(out, it)
			}
		}
		return out

	case "iif":
		if len(args) < 2 {
			return nil
		}
		condVals := evalFHIRPathExpr(current, ctx, args[0])
		if b, ok := singletonBool(condVals); ok && b {
			return evalFHIRPathExpr(current, ctx, args[1])
		}
		if len(args) >= 3 {
			return evalFHIRPathExpr(current, ctx, args[2])
		}
		return nil

	case "not":
		if len(current) != 1 {
			return nil
		}
		if b, ok := current[0].(bool); ok {
			return []any{!b}
		}
		return nil

	case "hasvalue":
		if len(current) != 1 || current[0] == nil {
			return []any{false}
		}
		switch v := current[0].(type) {
		case string:
			return []any{v != ""}
		case bool, int, int64, float64:
			return []any{true}
		default:
			return []any{false}
		}

	case "tostring":
		if len(current) != 1 {
			return nil
		}
		if s, ok := toStringConv(current[0]); ok {
			return []any{s}
		}
		return nil

	case "convertstostring":
		if len(current) != 1 {
			return nil
		}
		_, ok := toStringConv(current[0])
		return []any{ok}

	case "tointeger":
		if len(current) != 1 {
			return nil
		}
		if iv, ok := toIntegerConv(current[0]); ok {
			return []any{iv}
		}
		return nil

	case "convertstointeger":
		if len(current) != 1 {
			return nil
		}
		_, ok := toIntegerConv(current[0])
		return []any{ok}

	case "todecimal":
		if len(current) != 1 {
			return nil
		}
		if fv, ok := toDecimalConv(current[0]); ok {
			return []any{fv}
		}
		return nil

	case "convertstodecimal":
		if len(current) != 1 {
			return nil
		}
		_, ok := toDecimalConv(current[0])
		return []any{ok}

	case "toboolean":
		if len(current) != 1 {
			return nil
		}
		if bv, ok := toBooleanConv(current[0]); ok {
			return []any{bv}
		}
		return nil

	case "convertstoboolean":
		if len(current) != 1 {
			return nil
		}
		_, ok := toBooleanConv(current[0])
		return []any{ok}

	// String Functions
	case "indexof":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, ok := current[0].(string)
		if !ok {
			return nil
		}
		subVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(subVals) != 1 {
			return nil
		}
		sub, ok := subVals[0].(string)
		if !ok {
			return nil
		}
		if sub == "" {
			return []any{0}
		}
		return []any{strings.Index(s, sub)}

	case "substring":
		if len(current) != 1 || len(args) < 1 {
			return nil
		}
		s, ok := current[0].(string)
		if !ok {
			return nil
		}
		runes := []rune(s)
		startVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(startVals) != 1 {
			return nil
		}
		start, ok := toInt(startVals[0])
		if !ok || start < 0 || start >= len(runes) {
			return nil
		}
		if len(args) >= 2 {
			lenVals := evalFHIRPathExpr(scopeInput, ctx, args[1])
			if len(lenVals) != 1 {
				return nil
			}
			length, ok := toInt(lenVals[0])
			if !ok || length < 0 {
				return nil
			}
			end := start + length
			if end > len(runes) {
				end = len(runes)
			}
			return []any{string(runes[start:end])}
		}
		return []any{string(runes[start:])}

	case "startswith":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, ok := current[0].(string)
		if !ok {
			return nil
		}
		pVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(pVals) != 1 {
			return nil
		}
		p, ok := pVals[0].(string)
		if !ok {
			return nil
		}
		return []any{strings.HasPrefix(s, p)}

	case "endswith":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, ok := current[0].(string)
		if !ok {
			return nil
		}
		sVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(sVals) != 1 {
			return nil
		}
		suf, ok := sVals[0].(string)
		if !ok {
			return nil
		}
		return []any{strings.HasSuffix(s, suf)}

	case "contains":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, ok := current[0].(string)
		if !ok {
			return nil
		}
		cVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(cVals) != 1 {
			return nil
		}
		sub, ok := cVals[0].(string)
		if !ok {
			return nil
		}
		return []any{strings.Contains(s, sub)}

	case "upper":
		if len(current) != 1 {
			return nil
		}
		if s, ok := current[0].(string); ok {
			return []any{strings.ToUpper(s)}
		}
		return nil

	case "lower":
		if len(current) != 1 {
			return nil
		}
		if s, ok := current[0].(string); ok {
			return []any{strings.ToLower(s)}
		}
		return nil

	case "trim":
		if len(current) != 1 {
			return nil
		}
		if s, ok := current[0].(string); ok {
			return []any{strings.TrimSpace(s)}
		}
		return nil

	case "replace":
		if len(current) != 1 || len(args) != 2 {
			return nil
		}
		s, ok := current[0].(string)
		if !ok {
			return nil
		}
		pVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		rVals := evalFHIRPathExpr(scopeInput, ctx, args[1])
		if len(pVals) != 1 || len(rVals) != 1 {
			return nil
		}
		pat, ok1 := pVals[0].(string)
		rep, ok2 := rVals[0].(string)
		if !ok1 || !ok2 {
			return nil
		}
		return []any{strings.ReplaceAll(s, pat, rep)}

	case "matches":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, ok := current[0].(string)
		if !ok {
			return nil
		}
		rVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(rVals) != 1 {
			return nil
		}
		pat, ok := rVals[0].(string)
		if !ok {
			return nil
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil
		}
		return []any{re.MatchString(s)}

	case "replacematches":
		if len(current) != 1 || len(args) != 2 {
			return nil
		}
		s, ok := current[0].(string)
		if !ok {
			return nil
		}
		pVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		rVals := evalFHIRPathExpr(scopeInput, ctx, args[1])
		if len(pVals) != 1 || len(rVals) != 1 {
			return nil
		}
		pat, ok1 := pVals[0].(string)
		rep, ok2 := rVals[0].(string)
		if !ok1 || !ok2 {
			return nil
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil
		}
		return []any{re.ReplaceAllString(s, rep)}

	case "length":
		if len(current) != 1 {
			return nil
		}
		if s, ok := current[0].(string); ok {
			return []any{len([]rune(s))}
		}
		return nil

	case "tochars":
		if len(current) != 1 {
			return nil
		}
		if s, ok := current[0].(string); ok {
			runes := []rune(s)
			out := make([]any, len(runes))
			for i, r := range runes {
				out[i] = string(r)
			}
			return out
		}
		return nil

	case "split":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, ok := current[0].(string)
		if !ok {
			return nil
		}
		sepVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(sepVals) != 1 {
			return nil
		}
		sep, ok := sepVals[0].(string)
		if !ok {
			return nil
		}
		parts := strings.Split(s, sep)
		out := make([]any, len(parts))
		for i, p := range parts {
			out[i] = p
		}
		return out

	case "join":
		sep := ""
		if len(args) == 1 {
			sepVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
			if len(sepVals) == 1 {
				if sv, ok := sepVals[0].(string); ok {
					sep = sv
				}
			}
		}
		var strs []string
		for _, it := range current {
			if s, ok := it.(string); ok {
				strs = append(strs, s)
			}
		}
		return []any{strings.Join(strs, sep)}

	// Math Functions
	case "abs":
		if len(current) != 1 {
			return nil
		}
		switch v := current[0].(type) {
		case int:
			if v < 0 {
				return []any{-v}
			}
			return []any{v}
		case int64:
			if v < 0 {
				return []any{-v}
			}
			return []any{v}
		case float64:
			return []any{math.Abs(v)}
		}
		return nil

	case "ceiling":
		if len(current) != 1 {
			return nil
		}
		if fv, ok := toFloat(current[0]); ok {
			return []any{int(math.Ceil(fv))}
		}
		return nil

	case "floor":
		if len(current) != 1 {
			return nil
		}
		if fv, ok := toFloat(current[0]); ok {
			return []any{int(math.Floor(fv))}
		}
		return nil

	case "truncate":
		if len(current) != 1 {
			return nil
		}
		if fv, ok := toFloat(current[0]); ok {
			return []any{int(math.Trunc(fv))}
		}
		return nil

	case "round":
		if len(current) != 1 {
			return nil
		}
		fv, ok := toFloat(current[0])
		if !ok {
			return nil
		}
		if len(args) == 1 {
			pVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
			if len(pVals) == 1 {
				if p, ok := toInt(pVals[0]); ok && p >= 0 {
					pow := math.Pow(10, float64(p))
					return []any{math.Round(fv*pow) / pow}
				}
			}
		}
		return []any{int(math.Round(fv))}

	case "sqrt":
		if len(current) != 1 {
			return nil
		}
		if fv, ok := toFloat(current[0]); ok && fv >= 0 {
			return []any{math.Sqrt(fv)}
		}
		return nil

	case "exp":
		if len(current) != 1 {
			return nil
		}
		if fv, ok := toFloat(current[0]); ok {
			return []any{math.Exp(fv)}
		}
		return nil

	case "ln":
		if len(current) != 1 {
			return nil
		}
		if fv, ok := toFloat(current[0]); ok && fv > 0 {
			return []any{math.Log(fv)}
		}
		return nil

	case "log":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		fv, ok := toFloat(current[0])
		if !ok || fv <= 0 {
			return nil
		}
		bVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(bVals) != 1 {
			return nil
		}
		base, ok := toFloat(bVals[0])
		if !ok || base <= 0 || base == 1 {
			return nil
		}
		return []any{math.Log(fv) / math.Log(base)}

	case "power":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		fv, ok := toFloat(current[0])
		if !ok {
			return nil
		}
		eVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(eVals) != 1 {
			return nil
		}
		expVal, ok := toFloat(eVals[0])
		if !ok {
			return nil
		}
		res := math.Pow(fv, expVal)
		if math.IsNaN(res) || math.IsInf(res, 0) {
			return nil
		}
		if _, isInt1 := toInt(current[0]); isInt1 {
			if expInt, isInt2 := toInt(eVals[0]); isInt2 && expInt >= 0 {
				return []any{int(math.Round(res))}
			}
		}
		return []any{res}

	// FHIR R4 Specific Functions
	case "extension":
		if len(args) != 1 {
			return nil
		}
		uVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		extURL := strings.Trim(args[0], "'\"")
		if len(uVals) == 1 {
			if s, ok := uVals[0].(string); ok {
				extURL = s
			}
		}
		var matched []any
		for _, it := range current {
			if m, ok := it.(map[string]any); ok {
				if exts, ok := m["extension"].([]any); ok {
					for _, e := range exts {
						if em, ok := e.(map[string]any); ok {
							if u, _ := em["url"].(string); u == extURL {
								matched = append(matched, em)
							}
						}
					}
				}
			}
		}
		return matched

	case "resolve":
		var resolved []any
		for _, it := range current {
			refStr := ""
			switch v := it.(type) {
			case string:
				refStr = v
			case map[string]any:
				refStr, _ = v["reference"].(string)
			}
			if refStr == "" {
				continue
			}
			if strings.HasPrefix(refStr, "#") && ctx.root != nil {
				targetID := strings.TrimPrefix(refStr, "#")
				if contained, ok := ctx.root["contained"].([]any); ok {
					for _, c := range contained {
						if cm, ok := c.(map[string]any); ok {
							if cid, _ := cm["id"].(string); cid == targetID {
								resolved = append(resolved, cm)
							}
						}
					}
				}
			} else {
				resolved = append(resolved, refStr)
			}
		}
		return resolved
	}

	return nil
}

func evalFHIRPathComparison(lVals []any, op string, rVals []any) []any {
	switch op {
	case "=":
		if len(lVals) == 0 || len(rVals) == 0 {
			return nil
		}
		if len(lVals) != len(rVals) {
			return []any{false}
		}
		for i := range lVals {
			if !fhirPathEqual(lVals[i], rVals[i]) {
				return []any{false}
			}
		}
		return []any{true}

	case "!=":
		if len(lVals) == 0 || len(rVals) == 0 {
			return nil
		}
		if len(lVals) != len(rVals) {
			return []any{true}
		}
		for i := range lVals {
			if !fhirPathEqual(lVals[i], rVals[i]) {
				return []any{true}
			}
		}
		return []any{false}

	case "~":
		if len(lVals) == 0 && len(rVals) == 0 {
			return []any{true}
		}
		if len(lVals) != len(rVals) {
			return []any{false}
		}
		for i := range lVals {
			if !fhirPathEquivalent(lVals[i], rVals[i]) {
				return []any{false}
			}
		}
		return []any{true}

	case "!~":
		eq := evalFHIRPathComparison(lVals, "~", rVals)
		if b, ok := singletonBool(eq); ok {
			return []any{!b}
		}
		return []any{true}

	case ">", ">=", "<", "<=":
		if len(lVals) == 0 || len(rVals) == 0 {
			return nil
		}
		for _, lv := range lVals {
			for _, rv := range rVals {
				if lf, ok1 := toFloat(lv); ok1 {
					if rf, ok2 := toFloat(rv); ok2 {
						switch op {
						case ">":
							if lf > rf {
								return []any{true}
							}
						case ">=":
							if lf >= rf {
								return []any{true}
							}
						case "<":
							if lf < rf {
								return []any{true}
							}
						case "<=":
							if lf <= rf {
								return []any{true}
							}
						}
					}
				}
				if ls, ok1 := lv.(string); ok1 {
					if rs, ok2 := rv.(string); ok2 {
						switch op {
						case ">":
							if ls > rs {
								return []any{true}
							}
						case ">=":
							if ls >= rs {
								return []any{true}
							}
						case "<":
							if ls < rs {
								return []any{true}
							}
						case "<=":
							if ls <= rs {
								return []any{true}
							}
						}
					}
				}
			}
		}
		return []any{false}
	}
	return nil
}

func evalFHIRPathAdditive(lVals []any, op string, rVals []any) []any {
	if op == "&" {
		ls := ""
		if len(lVals) == 1 {
			if s, ok := lVals[0].(string); ok {
				ls = s
			} else if s, ok := toStringConv(lVals[0]); ok {
				ls = s
			}
		}
		rs := ""
		if len(rVals) == 1 {
			if s, ok := rVals[0].(string); ok {
				rs = s
			} else if s, ok := toStringConv(rVals[0]); ok {
				rs = s
			}
		}
		return []any{ls + rs}
	}
	if len(lVals) != 1 || len(rVals) != 1 {
		return nil
	}
	if op == "+" {
		if ls, ok1 := lVals[0].(string); ok1 {
			if rs, ok2 := rVals[0].(string); ok2 {
				return []any{ls + rs}
			}
		}
	}
	li, lokInt := toInt(lVals[0])
	ri, rokInt := toInt(rVals[0])
	if lokInt && rokInt {
		if op == "+" {
			return []any{li + ri}
		}
		return []any{li - ri}
	}
	lf, lok := toFloat(lVals[0])
	rf, rok := toFloat(rVals[0])
	if lok && rok {
		if op == "+" {
			return []any{lf + rf}
		}
		return []any{lf - rf}
	}
	return nil
}

func evalFHIRPathMultiplicative(lVals []any, op string, rVals []any) []any {
	if len(lVals) != 1 || len(rVals) != 1 {
		return nil
	}
	li, lokInt := toInt(lVals[0])
	ri, rokInt := toInt(rVals[0])
	lf, lok := toFloat(lVals[0])
	rf, rok := toFloat(rVals[0])
	if !lok || !rok {
		return nil
	}
	switch op {
	case "*":
		if lokInt && rokInt {
			return []any{li * ri}
		}
		return []any{lf * rf}
	case "/":
		if rf == 0 {
			return nil
		}
		return []any{lf / rf}
	case "div":
		if rf == 0 {
			return nil
		}
		return []any{int(math.Trunc(lf / rf))}
	case "mod":
		if rf == 0 {
			return nil
		}
		if lokInt && rokInt {
			return []any{li % ri}
		}
		return []any{math.Mod(lf, rf)}
	}
	return nil
}

func singletonBool(vals []any) (bool, bool) {
	if len(vals) != 1 {
		return false, false
	}
	b, ok := vals[0].(bool)
	return b, ok
}

func toInt(v any) (int, bool) {
	switch val := v.(type) {
	case int:
		return val, true
	case int64:
		return int(val), true
	case float64:
		if val == float64(int(val)) {
			return int(val), true
		}
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch val := v.(type) {
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
	switch val := v.(type) {
	case int:
		return val, true
	case int64:
		return int(val), true
	case float64:
		if val == float64(int(val)) {
			return int(val), true
		}
	case bool:
		if val {
			return 1, true
		}
		return 0, true
	case string:
		iv, err := strconv.Atoi(strings.TrimSpace(val))
		if err == nil {
			return iv, true
		}
	}
	return 0, false
}

func toDecimalConv(v any) (float64, bool) {
	switch val := v.(type) {
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case float64:
		return val, true
	case bool:
		if val {
			return 1.0, true
		}
		return 0.0, true
	case string:
		fv, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err == nil {
			return fv, true
		}
	}
	return 0, false
}

func toBooleanConv(v any) (bool, bool) {
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

func fhirPathEqual(a, b any) bool {
	if af, ok1 := toFloat(a); ok1 {
		if bf, ok2 := toFloat(b); ok2 {
			return af == bf
		}
		return false
	}
	if as, ok1 := a.(string); ok1 {
		if bs, ok2 := b.(string); ok2 {
			return as == bs
		}
		return false
	}
	if ab, ok1 := a.(bool); ok1 {
		if bb, ok2 := b.(bool); ok2 {
			return ab == bb
		}
		return false
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func fhirPathEquivalent(a, b any) bool {
	if as, ok1 := a.(string); ok1 {
		if bs, ok2 := b.(string); ok2 {
			normA := strings.Join(strings.Fields(strings.ToLower(as)), " ")
			normB := strings.Join(strings.Fields(strings.ToLower(bs)), " ")
			return normA == normB
		}
		return false
	}
	if af, ok1 := toFloat(a); ok1 {
		if bf, ok2 := toFloat(b); ok2 {
			return math.Abs(af-bf) < 1e-6
		}
		return false
	}
	return fhirPathEqual(a, b)
}

func fhirPathDistinct(items []any) []any {
	var out []any
	for _, it := range items {
		seen := false
		for _, ex := range out {
			if fhirPathEqual(ex, it) {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, it)
		}
	}
	return out
}

func fhirPathMatchesType(v any, typeName string) bool {
	tn := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(typeName, "FHIR."), "System."))
	switch val := v.(type) {
	case string:
		return tn == "string" || tn == "code" || tn == "id" || tn == "uri" || tn == "url" || tn == "canonical" || tn == "markdown" || tn == "date" || tn == "datetime" || tn == "instant" || tn == "time"
	case bool:
		return tn == "boolean"
	case int, int64:
		return tn == "integer" || tn == "positiveint" || tn == "unsignedint"
	case float64:
		if val == float64(int64(val)) {
			return tn == "integer" || tn == "decimal"
		}
		return tn == "decimal"
	case map[string]any:
		if rt, _ := val["resourceType"].(string); rt != "" {
			return strings.EqualFold(rt, tn) || tn == "resource" || tn == "domainresource"
		}
		switch tn {
		case "quantity":
			_, hasVal := val["value"]
			_, hasUnit := val["unit"]
			_, hasCode := val["code"]
			return hasVal && (hasUnit || hasCode)
		case "codeableconcept":
			_, hasCoding := val["coding"]
			_, hasText := val["text"]
			return hasCoding || hasText
		case "coding":
			_, hasSys := val["system"]
			_, hasCode := val["code"]
			return hasSys || hasCode
		case "period":
			_, hasStart := val["start"]
			_, hasEnd := val["end"]
			return hasStart || hasEnd
		case "identifier":
			_, hasSys := val["system"]
			_, hasVal := val["value"]
			return hasSys || hasVal
		case "humanname":
			_, hasFam := val["family"]
			_, hasGiv := val["given"]
			return hasFam || hasGiv
		case "reference":
			_, hasRef := val["reference"]
			return hasRef
		}
	}
	return false
}
