package server

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type fhirPathContext struct {
	root        map[string]any
	thisItem    any
	index       int
	total       int
	rowIndex    int
	aggTotal    []any
	hasAggTotal bool
	execErr     *error
}

func (c fhirPathContext) setExecErr(msg string) {
	if c.execErr != nil && *c.execErr == nil {
		*c.execErr = fmt.Errorf("FHIRPath execution error: %s", msg)
	}
}

// fhirTypedValue preserves FHIR choice/primitive type metadata, exact decimal scale,
// and JSON "_element" primitive extensions during intermediate FHIRPath evaluation.
type fhirTypedValue struct {
	val      any
	fhirType string
	decText  string
	primMeta map[string]any
}

// FHIRPathValue represents a typed evaluation result from EvalFHIRPathTyped.
type FHIRPathValue struct {
	Value    any
	FHIRType string
	DecText  string
}

func unwrapFHIRTyped(v any) any {
	for {
		tv, ok := v.(fhirTypedValue)
		if !ok {
			return v
		}
		v = tv.val
	}
}

func unwrapFHIRSlice(items []any) []any {
	if len(items) == 0 {
		return nil
	}
	out := make([]any, 0, len(items))
	for _, it := range items {
		u := unwrapFHIRTyped(it)
		if u != nil {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isEscapedAt(s string, pos int) bool {
	bs := 0
	for j := pos - 1; j >= 0 && s[j] == '\\'; j-- {
		bs++
	}
	return bs%2 == 1
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func unescapeFHIRPathString(raw string) (string, error) {
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			b.WriteByte(raw[i])
			continue
		}
		if i+1 >= len(raw) {
			return "", fmt.Errorf("trailing backslash in string literal")
		}
		i++
		switch raw[i] {
		case '\'', '"', '`', '\\', '/':
			b.WriteByte(raw[i])
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'f':
			b.WriteByte('\f')
		case 'u':
			if i+4 >= len(raw) || !isHexDigit(raw[i+1]) || !isHexDigit(raw[i+2]) || !isHexDigit(raw[i+3]) || !isHexDigit(raw[i+4]) {
				return "", fmt.Errorf("invalid unicode escape in string literal")
			}
			cp, err := strconv.ParseUint(raw[i+1:i+5], 16, 32)
			if err != nil || !utf8.ValidRune(rune(cp)) {
				return "", fmt.Errorf("invalid unicode code point in string literal")
			}
			b.WriteRune(rune(cp))
			i += 4
		default:
			b.WriteByte(raw[i])
		}
	}
	return b.String(), nil
}

func unwrapBacktickIdent(seg string) string {
	s := strings.TrimSpace(seg)
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
		if unescaped, err := unescapeFHIRPathString(s[1 : len(s)-1]); err == nil {
			return unescaped
		}
		return s[1 : len(s)-1]
	}
	return s
}

func validateAndCleanFHIRPath(expr string) (string, error) {
	var out strings.Builder
	out.Grow(len(expr))
	inQuote := byte(0)
	parenDepth := 0
	bracketDepth := 0
	braceDepth := 0

	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			out.WriteByte(ch)
			if ch == '\\' && (inQuote == '\'' || inQuote == '`') {
				if i+1 >= len(expr) {
					return "", fmt.Errorf("unterminated escape sequence")
				}
				if expr[i+1] == 'u' {
					if i+5 >= len(expr) || !isHexDigit(expr[i+2]) || !isHexDigit(expr[i+3]) || !isHexDigit(expr[i+4]) || !isHexDigit(expr[i+5]) {
						return "", fmt.Errorf("invalid \\uXXXX escape sequence")
					}
				}
			}
			if ch == inQuote && !isEscapedAt(expr, i) {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			inQuote = ch
			out.WriteByte(ch)
			continue
		}
		if ch == '/' && i+1 < len(expr) && expr[i+1] == '/' {
			i += 2
			for i < len(expr) && expr[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
			continue
		}
		if ch == '/' && i+1 < len(expr) && expr[i+1] == '*' {
			end := strings.Index(expr[i+2:], "*/")
			if end == -1 {
				return "", fmt.Errorf("unclosed block comment")
			}
			i += 2 + end + 1
			out.WriteByte(' ')
			continue
		}
		switch ch {
		case '(':
			parenDepth++
		case ')':
			parenDepth--
			if parenDepth < 0 {
				return "", fmt.Errorf("unmatched ')'")
			}
		case '[':
			bracketDepth++
		case ']':
			bracketDepth--
			if bracketDepth < 0 {
				return "", fmt.Errorf("unmatched ']'")
			}
		case '{':
			braceDepth++
		case '}':
			braceDepth--
			if braceDepth < 0 {
				return "", fmt.Errorf("unmatched '}'")
			}
		case ';', '?':
			return "", fmt.Errorf("unexpected character %q in FHIRPath expression", ch)
		}
		out.WriteByte(ch)
	}
	if inQuote != 0 {
		return "", fmt.Errorf("unterminated literal %q", inQuote)
	}
	if parenDepth != 0 || bracketDepth != 0 || braceDepth != 0 {
		return "", fmt.Errorf("unbalanced delimiters in FHIRPath expression")
	}

	cleaned := strings.TrimSpace(out.String())
	if cleaned == "" {
		return "", fmt.Errorf("empty FHIRPath expression")
	}
	if strings.HasPrefix(cleaned, ".") || strings.HasSuffix(cleaned, ".") {
		return "", fmt.Errorf("leading or trailing '.' in FHIRPath expression")
	}
	if strings.HasPrefix(cleaned, "@@") {
		return "", fmt.Errorf("invalid '@' literal in FHIRPath expression")
	}
	switch cleaned[0] {
	case '*', '/', '=', '!', '~', '<', '>', '|', '&', ',':
		return "", fmt.Errorf("unexpected leading operator %q", cleaned[0])
	}
	if isOperatorTrailing(cleaned) {
		return "", fmt.Errorf("trailing operator in FHIRPath expression")
	}
	lowerCleaned := strings.ToLower(cleaned)
	for _, kw := range []string{" is", " as"} {
		if strings.HasSuffix(lowerCleaned, kw) {
			return "", fmt.Errorf("trailing type operator in FHIRPath expression")
		}
	}
	inQ := byte(0)
	for i := 0; i < len(cleaned); i++ {
		c := cleaned[i]
		if inQ != 0 {
			if c == inQ && !isEscapedAt(cleaned, i) {
				inQ = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			inQ = c
			continue
		}
		if c == '.' && i+1 < len(cleaned) && cleaned[i+1] == '.' {
			return "", fmt.Errorf("invalid '..' in FHIRPath expression")
		}
		if c == '/' && i+1 < len(cleaned) && cleaned[i+1] == '*' {
			return "", fmt.Errorf("invalid '/*' sequence")
		}
		if c == '*' && i+1 < len(cleaned) && cleaned[i+1] == '/' {
			return "", fmt.Errorf("unmatched '*/' comment close")
		}
	}
	return cleaned, nil
}

// UnmarshalFHIRJSONPreservingNumbers decodes a FHIR JSON object into a map[string]any
// while preserving exact decimal/exponent lexical representations (e.g. 9.5e2, 1.500)
// in companion "_<field>._decimalText" metadata for FHIRPath precision/boundary operations.
func UnmarshalFHIRJSONPreservingNumbers(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, err
	}
	m, ok := normalizeFHIRJSONValue(decoded).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected JSON object at root")
	}
	return m, nil
}

func normalizeFHIRJSONValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, elem := range val {
			if jn, isNum := elem.(json.Number); isNum {
				rawText := jn.String()
				fv, err := jn.Float64()
				if err == nil {
					out[k] = fv
					if strings.ContainsAny(rawText, ".eE") && !strings.HasPrefix(k, "_") {
						metaKey := "_" + k
						pm, okPM := val[metaKey].(map[string]any)
						var newPM map[string]any
						if okPM {
							if normPM, okN := normalizeFHIRJSONValue(pm).(map[string]any); okN {
								newPM = normPM
							}
						}
						if newPM == nil {
							newPM = map[string]any{}
						}
						newPM["_decimalText"] = rawText
						out[metaKey] = newPM
					}
					continue
				}
			}
			if strings.HasPrefix(k, "_") {
				if existing, already := out[k].(map[string]any); already {
					if normPM, okN := normalizeFHIRJSONValue(elem).(map[string]any); okN {
						for mk, mv := range normPM {
							if _, exists := existing[mk]; !exists {
								existing[mk] = mv
							}
						}
					}
					continue
				}
			}
			out[k] = normalizeFHIRJSONValue(elem)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, elem := range val {
			if jn, isNum := elem.(json.Number); isNum {
				if fv, err := jn.Float64(); err == nil {
					out[i] = fv
					continue
				}
			}
			out[i] = normalizeFHIRJSONValue(elem)
		}
		return out
	default:
		return v
	}
}

// EvalFHIRPathPredicate evaluates a FHIRPath R4 expression as a boolean predicate
// using FHIRPath Singleton Evaluation of Collections (§6.1):
// empty -> false, single boolean -> value, single non-boolean -> true, >1 items -> error.
func EvalFHIRPathPredicate(root map[string]any, focus []any, expr string) (bool, error) {
	vals, err := EvalFHIRPathTypedWithFocus(root, focus, expr)
	if err != nil {
		return false, err
	}
	if len(vals) == 0 {
		return false, nil
	}
	if len(vals) > 1 {
		return false, fmt.Errorf("predicate expression returned %d items (singleton expected)", len(vals))
	}
	if b, ok := vals[0].Value.(bool); ok {
		return b, nil
	}
	return true, nil
}

// EvalFHIRPathTyped evaluates a FHIRPath R4 expression and returns typed results
// (preserving primitive choice types, temporal types, Quantities, and exact decimal scale).
func EvalFHIRPathTyped(root map[string]any, expr string) ([]FHIRPathValue, error) {
	return EvalFHIRPathTypedWithFocus(root, nil, expr)
}

// EvalFHIRPathTypedWithContextExpr evaluates contextExpr (if non-empty) against root to obtain
// the initial typed focus collection, and then evaluates expr against that focus.
func EvalFHIRPathTypedWithContextExpr(root map[string]any, contextExpr, expr string) ([]FHIRPathValue, error) {
	if strings.TrimSpace(contextExpr) == "" {
		return EvalFHIRPathTypedWithFocus(root, nil, expr)
	}
	cleanedCtx, err := validateAndCleanFHIRPath(contextExpr)
	if err != nil {
		return nil, err
	}
	if root == nil {
		root = map[string]any{}
	}
	var ctxExecErr error
	baseCtx := fhirPathContext{root: root, thisItem: root, index: 0, total: 1, execErr: &ctxExecErr}
	focus := evalFHIRPathExpr([]any{root}, baseCtx, cleanedCtx)
	if ctxExecErr != nil {
		return nil, ctxExecErr
	}
	if len(focus) == 0 {
		cleaned, err := validateAndCleanFHIRPath(expr)
		if err != nil {
			return nil, err
		}
		_ = cleaned
		return nil, nil
	}
	return EvalFHIRPathTypedWithFocus(root, focus, expr)
}

// EvalFHIRPathTypedWithFocus evaluates a FHIRPath R4 expression with an optional explicit focus collection.
func EvalFHIRPathTypedWithFocus(root map[string]any, focus []any, expr string) ([]FHIRPathValue, error) {
	cleaned, err := validateAndCleanFHIRPath(expr)
	if err != nil {
		return nil, err
	}
	if root == nil {
		root = map[string]any{}
	}
	startInput := focus
	var thisItem any = root
	if len(startInput) == 0 {
		startInput = []any{root}
	} else {
		thisItem = startInput[0]
	}
	var execErr error
	ctx := fhirPathContext{
		root:     root,
		thisItem: thisItem,
		index:    0,
		total:    len(startInput),
		execErr:  &execErr,
	}
	raw := evalFHIRPathExpr(startInput, ctx, cleaned)
	if execErr != nil {
		return nil, execErr
	}
	var out []FHIRPathValue
	for _, it := range raw {
		if tv, ok := it.(fhirTypedValue); ok {
			u := unwrapFHIRTyped(tv.val)
			if u == nil {
				continue
			}
			ft := normalizeExportedFHIRType(tv.fhirType, u)
			dt := tv.decText
			if ft == "Quantity" {
				if qStr, okQ := formatQuantityString(it); okQ && dt == "" {
					dt = qStr
				}
			}
			out = append(out, FHIRPathValue{
				Value:    u,
				FHIRType: ft,
				DecText:  dt,
			})
			continue
		}
		if it == nil {
			continue
		}
		out = append(out, FHIRPathValue{
			Value:    it,
			FHIRType: inferGoValueFHIRType(it),
		})
	}
	return out, nil
}

// evalFHIRPath evaluates a full HL7 FHIRPath R4 expression against a root FHIR resource map.
func evalFHIRPath(root map[string]any, expr string) []any {
	cleaned, err := validateAndCleanFHIRPath(expr)
	if err != nil || cleaned == "" || root == nil {
		return nil
	}
	ctx := fhirPathContext{
		root:     root,
		thisItem: root,
		index:    0,
		total:    1,
	}
	return unwrapFHIRSlice(evalFHIRPathExpr([]any{root}, ctx, cleaned))
}

func evalFHIRPathExpr(input []any, ctx fhirPathContext, expr string) []any {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil
	}
	if hasOuterParens(expr) {
		return evalFHIRPathExpr(input, ctx, expr[1:len(expr)-1])
	}

	// 1. implies (lowest precedence)
	if lhs, _, rhs, ok := splitTopLevelKeywordOp(expr, []string{"implies"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		lBool, lHas := fhirPathSingletonBool(lVals)
		if lHas && !lBool {
			return []any{true}
		}
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		rBool, rHas := fhirPathSingletonBool(rVals)
		if lHas {
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
		lBool, lHas := fhirPathSingletonBool(lVals)
		if op == "or" && lHas && lBool {
			return []any{true}
		}
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		rBool, rHas := fhirPathSingletonBool(rVals)
		if op == "or" {
			if (lHas && lBool) || (rHas && rBool) {
				return []any{true}
			}
			if lHas && rHas && !lBool && !rBool {
				return []any{false}
			}
			return nil
		}
		if lHas && rHas {
			return []any{lBool != rBool}
		}
		return nil
	}

	// 3. and (three-valued logic per FHIRPath R4 §6.5)
	if lhs, _, rhs, ok := splitTopLevelKeywordOp(expr, []string{"and"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		lBool, lHas := fhirPathSingletonBool(lVals)
		if lHas && !lBool {
			return []any{false}
		}
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		rBool, rHas := fhirPathSingletonBool(rVals)
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
			if len(lVals) != 1 {
				return nil
			}
			if len(rVals) == 0 {
				return []any{false}
			}
			for _, rv := range rVals {
				if eq, okEq := fhirPathEqualTri(lVals[0], rv); okEq && eq {
					return []any{true}
				}
			}
			return []any{false}
		}
		if len(rVals) != 1 {
			return nil
		}
		if len(lVals) == 0 {
			return []any{false}
		}
		for _, lv := range lVals {
			if eq, okEq := fhirPathEqualTri(lv, rVals[0]); okEq && eq {
				return []any{true}
			}
		}
		return []any{false}
	}

	// 5. Equality / Equivalence: !=, !~, =, ~ (looser than <, <=, >, >= per FHIRPath R4 §6.1)
	if lhs, op, rhs, ok := splitTopLevelSymbolOp(expr, []string{"!=", "!~", "=", "~"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		return evalFHIRPathComparison(ctx, lVals, op, rVals)
	}

	// 6. Ordering Comparison: >=, <=, >, <
	if lhs, op, rhs, ok := splitTopLevelSymbolOp(expr, []string{">=", "<=", ">", "<"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		return evalFHIRPathComparison(ctx, lVals, op, rVals)
	}

	// 7. Union operator: |
	if lhs, _, rhs, ok := splitTopLevelSymbolOp(expr, []string{"|"}); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		return fhirPathDistinct(append(append([]any{}, lVals...), rVals...))
	}

	// 8. Type operators: is, as (tighter than |, looser than +/-/& per FHIRPath R4 §6.1)
	if lhs, op, rhs, ok := splitTopLevelKeywordOp(expr, []string{"is", "as"}); ok {
		typeSpec := cleanTypeSpecifier(rhs)
		if !isKnownFHIRPathTypeName(typeSpec) {
			ctx.setExecErr(fmt.Sprintf("unknown type specifier %q in %q operator", typeSpec, op))
			return nil
		}
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		if len(lVals) > 1 {
			ctx.setExecErr(fmt.Sprintf("%q operator requires singleton input collection", op))
			return nil
		}
		if op == "is" {
			if len(lVals) == 0 {
				return nil
			}
			return []any{fhirPathMatchesType(lVals[0], typeSpec)}
		}
		if len(lVals) == 1 && fhirPathMatchesExactOrResourceType(lVals[0], typeSpec) {
			return lVals
		}
		return nil
	}

	// 9. Additive operators: +, -, &
	if lhs, op, rhs, ok := splitTopLevelAdditiveOp(expr); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		return evalFHIRPathAdditive(ctx, lVals, op, rVals)
	}

	// 10. Multiplicative operators: *, /, div, mod
	if lhs, op, rhs, ok := splitTopLevelMultiplicativeOp(expr); ok {
		lVals := evalFHIRPathExpr(input, ctx, lhs)
		rVals := evalFHIRPathExpr(input, ctx, rhs)
		return evalFHIRPathMultiplicative(lVals, op, rVals)
	}

	// 11. Unary - / +
	if strings.HasPrefix(expr, "-") && len(expr) > 1 {
		sub := evalFHIRPathExpr(input, ctx, expr[1:])
		if len(sub) > 1 {
			ctx.setExecErr("unary '-' requires a singleton numeric operand")
			return nil
		}
		if len(sub) == 1 {
			res := negateFHIRPathValue(sub[0])
			if res == nil {
				ctx.setExecErr("unary '-' is not valid for non-numeric operand")
			}
			return res
		}
		return nil
	}
	if strings.HasPrefix(expr, "+") && len(expr) > 1 {
		sub := evalFHIRPathExpr(input, ctx, expr[1:])
		if len(sub) > 1 {
			ctx.setExecErr("unary '+' requires a singleton numeric operand")
			return nil
		}
		if len(sub) == 1 {
			if _, okF := toFloat(sub[0]); !okF {
				if _, okQ := extractQuantityInfo(sub[0]); !okQ {
					ctx.setExecErr("unary '+' is not valid for non-numeric operand")
					return nil
				}
			}
			return sub
		}
		return nil
	}

	// 12. Path / Function / Literal chain
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
			if ch == inQuote && !isEscapedAt(expr, i) {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
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
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '(' || b == ')' || b == '[' || b == ']' || b == '\'' || b == '"' || b == '`'
}

func splitTopLevelKeywordOp(expr string, keywords []string) (string, string, string, bool) {
	depth := 0
	inQuote := byte(0)
	lastPos := -1
	lastKW := ""

	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			if ch == inQuote && !isEscapedAt(expr, i) {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
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
						prevSpace := prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r' || prev == ')'
						nextSpace := next == ' ' || next == '\t' || next == '\n' || next == '\r' || (next == '(' && kw != "contains" && kw != "in")
						if prevSpace && nextSpace && isWordBoundaryChar(prev) && isWordBoundaryChar(next) {
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
			if ch == inQuote && !isEscapedAt(expr, i) {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
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
				if (ch == '=' || ch == '~') && (expr[i-1] == '!' || expr[i-1] == '<' || expr[i-1] == '>') {
					continue
				}
				if (ch == '<' || ch == '>') && i+1 < len(expr) && expr[i+1] == '=' {
					matched := false
					for _, op := range ops {
						if op == expr[i:i+2] {
							matched = true
							break
						}
					}
					if !matched {
						i++
						continue
					}
				}
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
	case '=', '!', '<', '>', '+', '-', '*', '/', '&', '|', '(', '[', ',', '~':
		return true
	}
	lower := strings.ToLower(s)
	for _, kw := range []string{" and", " or", " xor", " implies", " div", " mod", " in", " contains", " is", " as"} {
		if strings.HasSuffix(lower, kw) {
			return true
		}
	}
	return false
}

func isInsideAtTemporalLiteral(expr string, pos int) bool {
	if pos <= 0 || pos+1 >= len(expr) {
		return false
	}
	if expr[pos+1] < '0' || expr[pos+1] > '9' {
		return false
	}
	j := pos - 1
	for j >= 0 {
		b := expr[j]
		if (b >= '0' && b <= '9') || b == 'T' || b == ':' || b == '.' || b == '-' {
			j--
			continue
		}
		break
	}
	return j >= 0 && expr[j] == '@'
}

func splitTopLevelAdditiveOp(expr string) (string, string, string, bool) {
	depth := 0
	inQuote := byte(0)
	lastPos := -1
	lastOp := ""

	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			if ch == inQuote && !isEscapedAt(expr, i) {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
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
				if (ch == '+' || ch == '-') && isInsideAtTemporalLiteral(expr, i) {
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
			if ch == inQuote && !isEscapedAt(expr, i) {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
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
						if (prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r' || prev == ')') &&
							(next == ' ' || next == '\t' || next == '\n' || next == '\r' || next == '(') {
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
			if litVals, isLit := evalFHIRPathPrimaryLiteral(baseSeg, input, ctx); isLit {
				current = litVals
				for _, ie := range indexExprs {
					current = applyFHIRPathIndexer(current, input, ctx, ie)
				}
				continue
			}
			unwrappedFirst := unwrapBacktickIdent(baseSeg)
			if rt, _ := ctx.root["resourceType"].(string); rt != "" && unwrappedFirst == rt {
				for _, ie := range indexExprs {
					current = applyFHIRPathIndexer(current, input, ctx, ie)
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
			if ch == inQuote && !isEscapedAt(seg, i) {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
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
				if c == q && !isEscapedAt(rest, i) {
					q = 0
				}
				continue
			}
			if c == '\'' || c == '"' || c == '`' {
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

var reValidTimeLiteral = regexp.MustCompile(`^T\d{2}(:\d{2}(:\d{2}(\.\d+)?)?)?$`)

func evalFHIRPathPrimaryLiteral(seg string, input []any, ctx fhirPathContext) ([]any, bool) {
	seg = strings.TrimSpace(seg)
	if seg == "{}" || strings.EqualFold(seg, "null") {
		return nil, true
	}
	if hasOuterParens(seg) {
		return evalFHIRPathExpr(input, ctx, seg[1:len(seg)-1]), true
	}
	if seg == "true" {
		return []any{true}, true
	}
	if seg == "false" {
		return []any{false}, true
	}
	if (strings.HasPrefix(seg, "'") && strings.HasSuffix(seg, "'")) ||
		(strings.HasPrefix(seg, "\"") && strings.HasSuffix(seg, "\"")) {
		if len(seg) >= 2 {
			unquoted, err := unescapeFHIRPathString(seg[1 : len(seg)-1])
			if err != nil {
				return nil, true
			}
			return []any{unquoted}, true
		}
	}
	if strings.HasPrefix(seg, "@") && len(seg) > 1 {
		body := strings.TrimPrefix(seg, "@")
		if strings.HasPrefix(body, "T") {
			if !reValidTimeLiteral.MatchString(body) {
				ctx.setExecErr(fmt.Sprintf("invalid FHIRPath Time literal %q (timezone offset not allowed on Time)", seg))
				return nil, true
			}
			tStr := strings.TrimPrefix(body, "T")
			return []any{fhirTypedValue{val: tStr, fhirType: "System.Time", decText: body}}, true
		}
		if strings.Contains(body, "T") {
			norm := strings.TrimSuffix(body, "T")
			return []any{fhirTypedValue{val: norm, fhirType: "System.DateTime", decText: body}}, true
		}
		return []any{fhirTypedValue{val: body, fhirType: "System.Date", decText: body}}, true
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
		if ctx.hasAggTotal {
			return ctx.aggTotal, true
		}
		return []any{ctx.total}, true
	case "%resource", "%rootResource", "%context":
		if ctx.root != nil {
			return []any{ctx.root}, true
		}
		return nil, true
	case "%rowIndex":
		return []any{ctx.rowIndex}, true
	case "%ucum":
		return []any{"http://unitsofmeasure.org"}, true
	case "%sct":
		return []any{"http://snomed.info/sct"}, true
	case "%loinc":
		return []any{"http://loinc.org"}, true
	}
	if strings.HasPrefix(seg, "%") && len(seg) > 1 {
		envName := unwrapBacktickIdent(seg[1:])
		if strings.HasPrefix(envName, "vs-") {
			return []any{"http://hl7.org/fhir/ValueSet/" + strings.TrimPrefix(envName, "vs-")}, true
		}
		if strings.HasPrefix(envName, "ext-") {
			return []any{"http://hl7.org/fhir/StructureDefinition/" + strings.TrimPrefix(envName, "ext-")}, true
		}
	}
	if qv, okQ := parseQuantityLiteral(seg); okQ {
		return []any{qv}, true
	}
	if !strings.ContainsAny(seg, ".eE ") {
		if iv, err := strconv.ParseInt(seg, 10, 64); err == nil {
			return []any{int(iv)}, true
		}
	}
	if strings.Contains(seg, ".") && !strings.ContainsAny(seg, " \t'") {
		if fv, err := strconv.ParseFloat(seg, 64); err == nil {
			return []any{fhirTypedValue{val: fv, fhirType: "System.Decimal", decText: seg}}, true
		}
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
			if ch == inQuote && !isEscapedAt(inner, i) {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
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

	field := unwrapBacktickIdent(seg)
	var next []any
	for _, it := range current {
		if tv, isTV := it.(fhirTypedValue); isTV {
			if (field == "extension" || field == "id") && tv.primMeta != nil {
				if mv, ok := tv.primMeta[field]; ok && mv != nil {
					if arr, isArr := mv.([]any); isArr {
						next = append(next, arr...)
					} else if field == "id" {
						next = append(next, fhirTypedValue{val: mv, fhirType: "id"})
					} else {
						next = append(next, mv)
					}
					continue
				}
			}
			if m, okM := tv.val.(map[string]any); okM {
				next = append(next, stepMapField(m, field)...)
			}
			continue
		}
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		next = append(next, stepMapField(m, field)...)
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
			subCtx := ctx
			subCtx.thisItem = it
			subCtx.index = i
			subCtx.total = len(current)
			vals := evalFHIRPathExpr([]any{it}, subCtx, args[0])
			if b, ok := fhirPathSingletonBool(vals); ok && b {
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
			subCtx := ctx
			subCtx.thisItem = it
			subCtx.index = i
			subCtx.total = len(current)
			vals := evalFHIRPathExpr([]any{it}, subCtx, args[0])
			if b, ok := fhirPathSingletonBool(vals); !ok || !b {
				return []any{false}
			}
		}
		return []any{true}

	case "alltrue":
		for _, it := range current {
			b, ok := unwrapFHIRTyped(it).(bool)
			if !ok {
				ctx.setExecErr("allTrue() requires all input items to be Boolean")
				return nil
			}
			if !b {
				return []any{false}
			}
		}
		return []any{true}

	case "anytrue":
		for _, it := range current {
			b, ok := unwrapFHIRTyped(it).(bool)
			if !ok {
				ctx.setExecErr("anyTrue() requires all input items to be Boolean")
				return nil
			}
			if b {
				return []any{true}
			}
		}
		return []any{false}

	case "allfalse":
		for _, it := range current {
			b, ok := unwrapFHIRTyped(it).(bool)
			if !ok {
				ctx.setExecErr("allFalse() requires all input items to be Boolean")
				return nil
			}
			if b {
				return []any{false}
			}
		}
		return []any{true}

	case "anyfalse":
		for _, it := range current {
			b, ok := unwrapFHIRTyped(it).(bool)
			if !ok {
				ctx.setExecErr("anyFalse() requires all input items to be Boolean")
				return nil
			}
			if !b {
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
			subCtx := ctx
			subCtx.thisItem = it
			subCtx.index = i
			subCtx.total = len(current)
			vals := evalFHIRPathExpr([]any{it}, subCtx, args[0])
			if b, ok := fhirPathSingletonBool(vals); ok && b {
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
			subCtx := ctx
			subCtx.thisItem = it
			subCtx.index = i
			subCtx.total = len(current)
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
			subCtx := ctx
			subCtx.thisItem = it
			subCtx.index = 0
			subCtx.total = 1
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

	case "children":
		var out []any
		for _, it := range current {
			out = append(out, fhirElementChildren(it)...)
		}
		return out

	case "descendants":
		var out []any
		for _, it := range current {
			out = append(out, fhirElementDescendants(it)...)
		}
		return out

	case "type":
		var out []any
		for _, it := range current {
			ns, name := fhirPathTypeInfo(it)
			if ns != "" && name != "" {
				out = append(out, map[string]any{"namespace": ns, "name": name})
			}
		}
		return out

	case "is":
		if len(args) != 1 {
			return nil
		}
		typeSpec := cleanTypeSpecifier(args[0])
		if !isKnownFHIRPathTypeName(typeSpec) {
			ctx.setExecErr(fmt.Sprintf("unknown type specifier %q in is()", typeSpec))
			return nil
		}
		if len(current) > 1 {
			ctx.setExecErr("is() requires singleton or empty input collection")
			return nil
		}
		if len(current) == 0 {
			return nil
		}
		return []any{fhirPathMatchesType(current[0], typeSpec)}

	case "as":
		if len(args) != 1 {
			return nil
		}
		typeSpec := cleanTypeSpecifier(args[0])
		if !isKnownFHIRPathTypeName(typeSpec) {
			ctx.setExecErr(fmt.Sprintf("unknown type specifier %q in as()", typeSpec))
			return nil
		}
		if len(current) > 1 {
			ctx.setExecErr("as() requires singleton or empty input collection")
			return nil
		}
		if len(current) == 1 && fhirPathMatchesExactOrResourceType(current[0], typeSpec) {
			return current
		}
		return nil

	case "oftype":
		if len(args) != 1 {
			return nil
		}
		typeSpec := cleanTypeSpecifier(args[0])
		if !isKnownFHIRPathTypeName(typeSpec) {
			ctx.setExecErr(fmt.Sprintf("unknown type specifier %q in ofType()", typeSpec))
			return nil
		}
		var matched []any
		for _, it := range current {
			if fhirPathMatchesExactOrResourceType(it, typeSpec) {
				matched = append(matched, it)
			}
		}
		return matched

	case "conformsto":
		if len(args) != 1 || len(current) != 1 {
			if len(current) > 1 {
				ctx.setExecErr("conformsTo() requires singleton input")
			}
			return nil
		}
		uVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(uVals) != 1 {
			return nil
		}
		profURL, ok := unwrapFHIRTyped(uVals[0]).(string)
		if !ok || !strings.HasPrefix(profURL, "http://hl7.org/fhir/StructureDefinition/") {
			ctx.setExecErr(fmt.Sprintf("unknown StructureDefinition profile URL %q in conformsTo()", profURL))
			return nil
		}
		targetType := strings.TrimPrefix(profURL, "http://hl7.org/fhir/StructureDefinition/")
		if idx := strings.IndexByte(targetType, '|'); idx != -1 {
			targetType = targetType[:idx]
		}
		if m, okM := unwrapFHIRTyped(current[0]).(map[string]any); okM {
			if rt, _ := m["resourceType"].(string); rt != "" {
				if rt == targetType || targetType == "Resource" || targetType == "DomainResource" {
					return []any{true}
				}
				if meta, okMeta := m["meta"].(map[string]any); okMeta {
					if profs, okP := meta["profile"].([]any); okP {
						for _, p := range profs {
							if ps, _ := p.(string); ps == profURL {
								return []any{true}
							}
						}
					}
				}
				return []any{false}
			}
		}
		return nil

	case "single":
		if len(current) == 1 {
			return current
		}
		if len(current) > 1 {
			ctx.setExecErr(fmt.Sprintf("single() expected at most 1 item, got %d", len(current)))
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
		if len(current) > 1 {
			ctx.setExecErr("iif() cannot be invoked on a collection with more than 1 item")
			return nil
		}
		subCtx := ctx
		if len(current) == 1 {
			subCtx.thisItem = current[0]
		}
		condVals := evalFHIRPathExpr(current, subCtx, args[0])
		if len(condVals) > 1 {
			ctx.setExecErr("iif() criterion evaluated to more than 1 item")
			return nil
		}
		if len(condVals) == 1 {
			b, ok := unwrapFHIRTyped(condVals[0]).(bool)
			if !ok {
				return nil
			}
			if b {
				return evalFHIRPathExpr(current, subCtx, args[1])
			}
		}
		if len(args) >= 3 {
			return evalFHIRPathExpr(current, subCtx, args[2])
		}
		return nil

	case "coalesce":
		for _, rawArg := range args {
			vals := evalFHIRPathExpr(current, ctx, strings.TrimSpace(rawArg))
			var nonEmpty []any
			for _, v := range vals {
				u := unwrapFHIRTyped(v)
				if u == nil {
					continue
				}
				if s, isStr := u.(string); isStr && s == "" {
					continue
				}
				nonEmpty = append(nonEmpty, v)
			}
			if len(nonEmpty) > 0 {
				return nonEmpty
			}
		}
		return nil

	case "not":
		if len(current) == 0 {
			return nil
		}
		if len(current) > 1 {
			ctx.setExecErr("not() cannot be invoked on a collection with more than 1 item")
			return nil
		}
		b, ok := fhirPathSingletonBool(current)
		if !ok {
			return nil
		}
		return []any{!b}

	case "hasvalue":
		if len(current) != 1 || current[0] == nil {
			return []any{false}
		}
		u := unwrapFHIRTyped(current[0])
		if u == nil {
			return []any{false}
		}
		switch v := u.(type) {
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
		if tv, ok := toDecimalTypedConv(current[0]); ok {
			return []any{tv}
		}
		return nil

	case "convertstodecimal":
		if len(current) != 1 {
			return nil
		}
		_, ok := toDecimalTypedConv(current[0])
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

	case "todate":
		if len(current) != 1 {
			return nil
		}
		if tv, ok := toDateConv(current[0]); ok {
			return []any{tv}
		}
		return nil

	case "convertstodate":
		if len(current) != 1 {
			return nil
		}
		_, ok := toDateConv(current[0])
		return []any{ok}

	case "todatetime":
		if len(current) != 1 {
			return nil
		}
		if tv, ok := toDateTimeConv(current[0]); ok {
			return []any{tv}
		}
		return nil

	case "convertstodatetime":
		if len(current) != 1 {
			return nil
		}
		_, ok := toDateTimeConv(current[0])
		return []any{ok}

	case "totime":
		if len(current) != 1 {
			return nil
		}
		if tv, ok := toTimeConv(current[0]); ok {
			return []any{tv}
		}
		return nil

	case "convertstotime":
		if len(current) != 1 {
			return nil
		}
		_, ok := toTimeConv(current[0])
		return []any{ok}

	case "toquantity":
		if len(current) != 1 {
			return nil
		}
		var targetUnit string
		if len(args) == 1 {
			uVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
			if len(uVals) == 1 {
				targetUnit, _ = unwrapFHIRTyped(uVals[0]).(string)
			}
		}
		if qv, ok := toQuantityConv(current[0], targetUnit); ok {
			return []any{qv}
		}
		return nil

	case "convertstoquantity":
		if len(current) != 1 {
			return nil
		}
		var targetUnit string
		if len(args) == 1 {
			uVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
			if len(uVals) == 1 {
				targetUnit, _ = unwrapFHIRTyped(uVals[0]).(string)
			}
		}
		_, ok := toQuantityConv(current[0], targetUnit)
		return []any{ok}

	case "precision":
		if len(current) != 1 {
			return nil
		}
		if p, ok := evalPrecision(current[0]); ok {
			return []any{p}
		}
		return nil

	case "lowboundary", "highboundary":
		if len(current) != 1 {
			return nil
		}
		hasPrec := false
		prec := -1
		if len(args) >= 1 {
			pVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
			if len(pVals) != 1 {
				return nil
			}
			p, ok := toInt(pVals[0])
			if !ok {
				return nil
			}
			hasPrec = true
			prec = p
		}
		if bv, ok := evalBoundary(current[0], strings.EqualFold(fnName, "lowboundary"), hasPrec, prec); ok {
			return []any{bv}
		}
		return nil

	case "comparable":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		oVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(oVals) != 1 {
			return nil
		}
		if okComp, valid := quantitiesComparable(current[0], oVals[0]); valid {
			return []any{okComp}
		}
		_, isQ1 := extractQuantityInfo(current[0])
		_, isQ2 := extractQuantityInfo(oVals[0])
		if isQ1 || isQ2 {
			return []any{false}
		}
		if tv1, ok1 := current[0].(fhirTypedValue); ok1 && isTemporalFHIRType(tv1.fhirType) {
			if tv2, ok2 := oVals[0].(fhirTypedValue); ok2 && isTemporalFHIRType(tv2.fhirType) {
				return []any{tv1.fhirType == tv2.fhirType}
			}
			return []any{false}
		}
		if tv2, ok2 := oVals[0].(fhirTypedValue); ok2 && isTemporalFHIRType(tv2.fhirType) {
			return []any{false}
		}
		u1 := unwrapFHIRTyped(current[0])
		u2 := unwrapFHIRTyped(oVals[0])
		switch v1 := u1.(type) {
		case string:
			v2, ok := u2.(string)
			return []any{ok && v1 != "" && v2 != ""}
		case int, int64, float64:
			switch u2.(type) {
			case int, int64, float64:
				return []any{true}
			default:
				return []any{false}
			}
		case bool:
			_, ok := u2.(bool)
			return []any{ok}
		default:
			return []any{false}
		}

	// String Functions
	case "indexof":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, okS := unwrapFHIRTyped(current[0]).(string)
		if !okS {
			return nil
		}
		subVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(subVals) != 1 {
			return nil
		}
		sub, ok := unwrapFHIRTyped(subVals[0]).(string)
		if !ok {
			return nil
		}
		if sub == "" {
			return []any{0}
		}
		byteIdx := strings.Index(s, sub)
		if byteIdx < 0 {
			return []any{-1}
		}
		return []any{utf8.RuneCountInString(s[:byteIdx])}

	case "substring":
		if len(current) != 1 || len(args) < 1 {
			return nil
		}
		s, okS := unwrapFHIRTyped(current[0]).(string)
		if !okS {
			return nil
		}
		startVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(startVals) != 1 {
			return nil
		}
		start, ok := toInt(startVals[0])
		if !ok || start < 0 {
			return nil
		}
		runes := []rune(s)
		if start >= len(runes) {
			return nil
		}
		if len(args) >= 2 {
			lenVals := evalFHIRPathExpr(scopeInput, ctx, args[1])
			if len(lenVals) == 0 {
				return []any{string(runes[start:])}
			}
			if len(lenVals) > 1 {
				return nil
			}
			l, okL := toInt(lenVals[0])
			if !okL {
				return nil
			}
			if l <= 0 {
				return []any{""}
			}
			end := len(runes)
			if l < end-start {
				end = start + l
			}
			return []any{string(runes[start:end])}
		}
		return []any{string(runes[start:])}

	case "startswith":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, ok := unwrapFHIRTyped(current[0]).(string)
		if !ok {
			return nil
		}
		pVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(pVals) != 1 {
			return nil
		}
		p, ok := unwrapFHIRTyped(pVals[0]).(string)
		if !ok {
			return nil
		}
		return []any{strings.HasPrefix(s, p)}

	case "endswith":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, ok := unwrapFHIRTyped(current[0]).(string)
		if !ok {
			return nil
		}
		sVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(sVals) != 1 {
			return nil
		}
		suf, ok := unwrapFHIRTyped(sVals[0]).(string)
		if !ok {
			return nil
		}
		return []any{strings.HasSuffix(s, suf)}

	case "contains":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, ok := unwrapFHIRTyped(current[0]).(string)
		if !ok {
			return nil
		}
		cVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(cVals) != 1 {
			return nil
		}
		sub, ok := unwrapFHIRTyped(cVals[0]).(string)
		if !ok {
			return nil
		}
		return []any{strings.Contains(s, sub)}

	case "upper":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if s, ok := unwrapFHIRTyped(item).(string); ok {
				out = append(out, strings.ToUpper(s))
			}
		}
		return out

	case "lower":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if s, ok := unwrapFHIRTyped(item).(string); ok {
				out = append(out, strings.ToLower(s))
			}
		}
		return out

	case "trim":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if s, ok := unwrapFHIRTyped(item).(string); ok {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out

	case "replace":
		if len(current) == 0 || len(args) != 2 {
			return nil
		}
		pVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		rVals := evalFHIRPathExpr(scopeInput, ctx, args[1])
		if len(pVals) != 1 || len(rVals) != 1 {
			return nil
		}
		pat, ok1 := unwrapFHIRTyped(pVals[0]).(string)
		rep, ok2 := unwrapFHIRTyped(rVals[0]).(string)
		if !ok1 || !ok2 {
			return nil
		}
		var out []any
		for _, item := range current {
			if s, ok := unwrapFHIRTyped(item).(string); ok {
				out = append(out, strings.ReplaceAll(s, pat, rep))
			}
		}
		return out

	case "matches", "matchesfull":
		if len(current) != 1 || len(args) != 1 {
			return nil
		}
		s, ok := unwrapFHIRTyped(current[0]).(string)
		if !ok {
			return nil
		}
		rVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(rVals) != 1 {
			return nil
		}
		pat, ok := unwrapFHIRTyped(rVals[0]).(string)
		if !ok {
			return nil
		}
		if strings.EqualFold(fnName, "matchesfull") {
			pat = "(?s)^(?:" + pat + ")$"
		} else {
			pat = "(?s)" + pat
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil
		}
		return []any{re.MatchString(s)}

	case "replacematches":
		if len(current) == 0 || len(args) != 2 {
			return nil
		}
		pVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		rVals := evalFHIRPathExpr(scopeInput, ctx, args[1])
		if len(pVals) != 1 || len(rVals) != 1 {
			return nil
		}
		pat, ok1 := unwrapFHIRTyped(pVals[0]).(string)
		rep, ok2 := unwrapFHIRTyped(rVals[0]).(string)
		if !ok1 || !ok2 {
			return nil
		}
		if pat == "" {
			var out []any
			for _, item := range current {
				if s, ok := unwrapFHIRTyped(item).(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
		re, err := regexp.Compile("(?s)" + pat)
		if err != nil {
			return nil
		}
		var out []any
		for _, item := range current {
			if s, ok := unwrapFHIRTyped(item).(string); ok {
				out = append(out, re.ReplaceAllString(s, rep))
			}
		}
		return out

	case "length":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if s, ok := unwrapFHIRTyped(item).(string); ok {
				out = append(out, len([]rune(s)))
			}
		}
		return out

	case "tochars":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if s, ok := unwrapFHIRTyped(item).(string); ok {
				for _, r := range s {
					out = append(out, string(r))
				}
			}
		}
		return out

	case "split":
		if len(current) == 0 || len(args) != 1 {
			return nil
		}
		sepVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		if len(sepVals) != 1 {
			return nil
		}
		sep, ok := unwrapFHIRTyped(sepVals[0]).(string)
		if !ok {
			return nil
		}
		var out []any
		for _, item := range current {
			if s, ok := unwrapFHIRTyped(item).(string); ok {
				for _, p := range strings.Split(s, sep) {
					out = append(out, p)
				}
			}
		}
		return out

	case "join":
		if len(current) == 0 {
			return nil
		}
		sep := ""
		if len(args) == 1 {
			sepVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
			if len(sepVals) == 1 {
				if sv, ok := unwrapFHIRTyped(sepVals[0]).(string); ok {
					sep = sv
				}
			}
		}
		var strs []string
		for _, it := range current {
			if s, ok := unwrapFHIRTyped(it).(string); ok {
				strs = append(strs, s)
			} else if s, ok := toStringConv(it); ok {
				strs = append(strs, s)
			}
		}
		if len(strs) == 0 {
			return nil
		}
		return []any{strings.Join(strs, sep)}

	// Math Functions
	case "abs":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if av, ok := absFHIRPathValue(item); ok {
				out = append(out, av)
			}
		}
		return out

	case "ceiling":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if fv, ok := toFloat(item); ok {
				out = append(out, int(math.Ceil(fv)))
			}
		}
		return out

	case "floor":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if fv, ok := toFloat(item); ok {
				out = append(out, int(math.Floor(fv)))
			}
		}
		return out

	case "truncate":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if fv, ok := toFloat(item); ok {
				out = append(out, int(math.Trunc(fv)))
			}
		}
		return out

	case "round":
		if len(current) == 0 {
			return nil
		}
		precision := 0
		if len(args) == 1 {
			pVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
			if len(pVals) != 1 {
				return nil
			}
			p, ok := toInt(pVals[0])
			if !ok || p < 0 {
				return nil
			}
			precision = p
		}
		var out []any
		for _, item := range current {
			fv, ok := toFloat(item)
			if !ok {
				continue
			}
			pow := math.Pow(10, float64(precision))
			rounded := math.Round(fv*pow) / pow
			out = append(out, fhirTypedValue{
				val:      rounded,
				fhirType: "System.Decimal",
				decText:  strconv.FormatFloat(rounded, 'f', precision, 64),
			})
		}
		return out

	case "sqrt":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if fv, ok := toFloat(item); ok && fv >= 0 {
				res := math.Sqrt(fv)
				out = append(out, fhirTypedValue{val: res, fhirType: "System.Decimal"})
			}
		}
		return out

	case "exp":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if fv, ok := toFloat(item); ok {
				out = append(out, fhirTypedValue{val: math.Exp(fv), fhirType: "System.Decimal"})
			}
		}
		return out

	case "ln":
		if len(current) == 0 {
			return nil
		}
		var out []any
		for _, item := range current {
			if fv, ok := toFloat(item); ok && fv > 0 {
				out = append(out, fhirTypedValue{val: math.Log(fv), fhirType: "System.Decimal"})
			}
		}
		return out

	case "log":
		if len(current) == 0 || len(args) != 1 {
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
		var out []any
		for _, item := range current {
			if fv, ok := toFloat(item); ok && fv > 0 {
				out = append(out, fhirTypedValue{val: math.Log(fv) / math.Log(base), fhirType: "System.Decimal"})
			}
		}
		return out

	case "power":
		if len(current) == 0 || len(args) != 1 {
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
		expInt, isInt2 := toIntStrict(eVals[0])
		var out []any
		for _, item := range current {
			fv, ok := toFloat(item)
			if !ok {
				continue
			}
			res := math.Pow(fv, expVal)
			if math.IsNaN(res) || math.IsInf(res, 0) {
				continue
			}
			if _, isInt1 := toIntStrict(item); isInt1 && isInt2 && expInt >= 0 {
				out = append(out, int(math.Round(res)))
			} else {
				out = append(out, fhirTypedValue{val: res, fhirType: "System.Decimal"})
			}
		}
		return out

	case "encode":
		if len(current) == 0 || len(args) != 1 {
			return nil
		}
		fVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		format := strings.ToLower(strings.Trim(args[0], "'\""))
		if len(fVals) == 1 {
			if fs, ok := unwrapFHIRTyped(fVals[0]).(string); ok {
				format = strings.ToLower(fs)
			}
		}
		var out []any
		for _, item := range current {
			s, ok := unwrapFHIRTyped(item).(string)
			if !ok {
				continue
			}
			switch format {
			case "hex":
				out = append(out, hex.EncodeToString([]byte(s)))
			case "urlbase64":
				out = append(out, base64.URLEncoding.EncodeToString([]byte(s)))
			default:
				out = append(out, base64.StdEncoding.EncodeToString([]byte(s)))
			}
		}
		return out

	case "decode":
		if len(current) == 0 || len(args) != 1 {
			return nil
		}
		fVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		format := strings.ToLower(strings.Trim(args[0], "'\""))
		if len(fVals) == 1 {
			if fs, ok := unwrapFHIRTyped(fVals[0]).(string); ok {
				format = strings.ToLower(fs)
			}
		}
		var out []any
		for _, item := range current {
			s, ok := unwrapFHIRTyped(item).(string)
			if !ok {
				continue
			}
			switch format {
			case "hex":
				if b, err := hex.DecodeString(s); err == nil {
					out = append(out, string(b))
				}
			case "urlbase64":
				if b, err := base64.URLEncoding.DecodeString(s); err == nil {
					out = append(out, string(b))
				}
			default:
				if b, err := base64.StdEncoding.DecodeString(s); err == nil {
					out = append(out, string(b))
				}
			}
		}
		return out

	case "escape":
		if len(current) == 0 || len(args) != 1 {
			return nil
		}
		fVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		format := strings.ToLower(strings.Trim(args[0], "'\""))
		if len(fVals) == 1 {
			if fs, ok := unwrapFHIRTyped(fVals[0]).(string); ok {
				format = strings.ToLower(fs)
			}
		}
		var out []any
		for _, item := range current {
			s, ok := unwrapFHIRTyped(item).(string)
			if !ok {
				continue
			}
			switch format {
			case "json":
				escaped := strings.ReplaceAll(s, `\`, `\\`)
				escaped = strings.ReplaceAll(escaped, `"`, `\"`)
				out = append(out, escaped)
			default:
				escHTML := strings.ReplaceAll(html.EscapeString(s), "&#34;", "&quot;")
				escHTML = strings.ReplaceAll(escHTML, "&#39;", "&apos;")
				out = append(out, escHTML)
			}
		}
		return out

	case "unescape":
		if len(current) == 0 || len(args) != 1 {
			return nil
		}
		fVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		format := strings.ToLower(strings.Trim(args[0], "'\""))
		if len(fVals) == 1 {
			if fs, ok := unwrapFHIRTyped(fVals[0]).(string); ok {
				format = strings.ToLower(fs)
			}
		}
		var out []any
		for _, item := range current {
			s, ok := unwrapFHIRTyped(item).(string)
			if !ok {
				continue
			}
			switch format {
			case "json":
				out = append(out, unescapeJSONFragment(s))
			default:
				out = append(out, html.UnescapeString(s))
			}
		}
		return out

	case "sort", "sortdesc":
		if len(current) <= 1 {
			return current
		}
		return evalSortCollection(current, ctx, fnName, args)

	case "sum":
		if len(current) == 0 {
			return []any{0}
		}
		sum := 0.0
		allInt := true
		for _, item := range current {
			if _, isI := toIntStrict(item); !isI {
				allInt = false
			}
			if fv, ok := toFloat(item); ok {
				sum += fv
			}
		}
		if allInt {
			return []any{int(math.Round(sum))}
		}
		return []any{sum}

	case "avg":
		if len(current) == 0 {
			return nil
		}
		sum := 0.0
		count := 0
		for _, item := range current {
			if fv, ok := toFloat(item); ok {
				sum += fv
				count++
			}
		}
		if count == 0 {
			return nil
		}
		return []any{sum / float64(count)}

	case "min":
		if len(current) == 0 {
			return nil
		}
		best := current[0]
		for _, item := range current[1:] {
			if compareScalarForSort(item, best) < 0 {
				best = item
			}
		}
		return []any{best}

	case "max":
		if len(current) == 0 {
			return nil
		}
		best := current[0]
		for _, item := range current[1:] {
			if compareScalarForSort(item, best) > 0 {
				best = item
			}
		}
		return []any{best}

	case "aggregate":
		if len(args) == 0 {
			return nil
		}
		var total []any
		if len(args) >= 2 {
			total = evalFHIRPathExpr(scopeInput, ctx, args[1])
		}
		for i, it := range current {
			subCtx := ctx
			subCtx.thisItem = it
			subCtx.index = i
			subCtx.total = len(current)
			subCtx.aggTotal = total
			subCtx.hasAggTotal = true
			total = evalFHIRPathExpr([]any{it}, subCtx, args[0])
		}
		return total

	// FHIR R4 Specific Functions
	case "extension":
		if len(args) != 1 {
			return nil
		}
		uVals := evalFHIRPathExpr(scopeInput, ctx, args[0])
		extURL := strings.Trim(args[0], "'\"")
		if len(uVals) == 1 {
			if s, ok := unwrapFHIRTyped(uVals[0]).(string); ok {
				extURL = s
			}
		}
		var matched []any
		for _, it := range current {
			var extSlice []any
			if tv, isTV := it.(fhirTypedValue); isTV {
				if tv.primMeta != nil {
					extSlice, _ = tv.primMeta["extension"].([]any)
				}
				if len(extSlice) == 0 {
					if m, okM := tv.val.(map[string]any); okM {
						extSlice, _ = m["extension"].([]any)
					}
				}
			} else if m, ok := it.(map[string]any); ok {
				extSlice, _ = m["extension"].([]any)
			}
			for _, e := range extSlice {
				if em, ok := e.(map[string]any); ok {
					if u, _ := em["url"].(string); u == extURL {
						matched = append(matched, em)
					}
				}
			}
		}
		return matched

	case "resolve":
		var resolved []any
		for _, it := range current {
			refStr := ""
			switch v := unwrapFHIRTyped(it).(type) {
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

	case "getresourcekey":
		items := current
		if len(items) == 0 && ctx.root != nil {
			items = []any{ctx.root}
		}
		var keys []any
		for _, it := range items {
			if m, ok := unwrapFHIRTyped(it).(map[string]any); ok {
				if id, ok := m["id"].(string); ok && strings.TrimSpace(id) != "" {
					keys = append(keys, strings.TrimSpace(id))
				}
			}
		}
		if len(keys) == 0 && ctx.root != nil {
			if id, ok := ctx.root["id"].(string); ok && strings.TrimSpace(id) != "" {
				keys = append(keys, strings.TrimSpace(id))
			}
		}
		return keys

	case "getreferencekey":
		targetType := ""
		if len(args) >= 1 {
			targetType = strings.Trim(strings.TrimSpace(args[0]), "'\"")
		}
		var keys []any
		for _, it := range current {
			refStr := ""
			refType := ""
			switch v := unwrapFHIRTyped(it).(type) {
			case string:
				refStr = strings.TrimSpace(v)
			case map[string]any:
				refStr, _ = v["reference"].(string)
				refStr = strings.TrimSpace(refStr)
				refType, _ = v["type"].(string)
			}
			if refStr == "" {
				continue
			}
			if strings.HasPrefix(refStr, "urn:uuid:") {
				if targetType == "" || strings.EqualFold(refType, targetType) {
					keys = append(keys, strings.TrimPrefix(refStr, "urn:uuid:"))
				}
				continue
			}
			parts := strings.Split(refStr, "/")
			if len(parts) >= 2 {
				actualType := parts[len(parts)-2]
				actualID := parts[len(parts)-1]
				if (targetType == "" || strings.EqualFold(actualType, targetType)) && actualID != "" {
					keys = append(keys, actualID)
				}
			} else if len(parts) == 1 && (targetType == "" || strings.EqualFold(refType, targetType)) {
				keys = append(keys, parts[0])
			}
		}
		return keys

	case "today":
		return []any{fhirTypedValue{val: time.Now().UTC().Format("2006-01-02"), fhirType: "System.Date"}}

	case "now":
		return []any{fhirTypedValue{val: time.Now().UTC().Format(time.RFC3339), fhirType: "System.DateTime"}}

	case "timeofday":
		tStr := time.Now().UTC().Format("15:04:05")
		return []any{fhirTypedValue{val: tStr, fhirType: "System.Time", decText: "T" + tStr}}

	case "trace", "definevariable":
		return current

	case "check":
		if len(args) >= 1 {
			subCtx := ctx
			if len(current) == 1 {
				subCtx.thisItem = current[0]
			}
			if b, ok := fhirPathSingletonBool(evalFHIRPathExpr(current, subCtx, args[0])); !ok || !b {
				return nil
			}
		}
		return current
	}

	return nil
}
