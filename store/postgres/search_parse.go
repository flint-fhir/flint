package postgres

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DateOp represents a parsed date search operation.
// Per FHIR R4 §3.1.1.4.5, a date parameter represents an implicit interval [Value, High)
// based on the precision of the input string (YYYY, YYYY-MM, YYYY-MM-DD, or timestamp).
type DateOp struct {
	Prefix string    // eq, ne, lt, le, gt, ge, sa, eb, ap
	Value  time.Time // Normalized UTC start timestamp (inclusive lower bound)
	High   time.Time // Normalized UTC end timestamp (exclusive upper bound for date/month/year precision, or equal to Value for timestamp precision)
}

// QuantityOp represents a parsed quantity search operation.
type QuantityOp struct {
	Prefix string  // eq, ne, lt, le, gt, ge
	Value  float64 // Numeric quantity value
	System string  // Coding system URI (optional)
	Code   string  // Unit code (optional)
}

// IncludeParam represents an _include or _revinclude parameter.
// Example: "Observation:patient" or "Observation:patient:Patient"
type IncludeParam struct {
	SourceType string // e.g. "Observation"
	ParamName  string // e.g. "patient"
	TargetType string // e.g. "Patient" (optional)
	Iterate    bool   // true when requested via _include:iterate or _revinclude:iterate
}

// ChainedHop represents a single reference traversal hop in a chained search parameter
// (e.g. "subject:Patient" or "organization").
type ChainedHop struct {
	RefParam   string // e.g. "patient", "subject", "organization"
	TargetType string // Inferred or explicit target type, e.g. "Patient", "Organization" (empty when polymorphic without :TargetType)
}

// ChainedParam represents a chained parameter like "patient.name=Smith",
// "subject:Patient.name:exact=Smith", or multi-hop "patient.organization.name=Acme".
type ChainedParam struct {
	RefParam       string       // First hop reference param (e.g. "patient" or "subject")
	TargetType     string       // Final hop inferred or explicit target type (empty when polymorphic without :TargetType)
	Hops           []ChainedHop // Ordered reference hops (len >= 1); Hops[len(Hops)-1] leads to TargetParam
	TargetParam    string       // Base target search parameter, e.g. "name", "family", "gender", "organization", "url"
	TargetModifier string       // Optional modifier on targetParam, e.g. "exact", "contains", "not", "below", "above"
	ParamType      string       // "id", "string", "token", "date", "quantity", "reference", "uri"
	Value          string       // Search value
}

// HasParam represents a FHIR R4 reverse-chaining parameter (_has:SourceType:refParam:searchParam=value).
// Example: _has:Observation:patient:code=8867-4 on a Patient search, or multi-hop
// _has:Encounter:patient:_has:Observation:encounter:code=8867-4.
type HasParam struct {
	SourceType  string    // e.g. "Observation"
	RefParam    string    // e.g. "patient" or "subject"
	SearchParam string    // e.g. "code", "date", "value-quantity", or nested "_has:..."
	ParamType   string    // "id", "string", "token", "date", "quantity", "reference", "uri", "has"
	Value       string    // Search value
	Child       *HasParam // Optional nested _has hop when SearchParam starts with "_has:"
}

// SortSpec represents a single sort directive parsed from _sort.
type SortSpec struct {
	Param string // e.g. "_lastUpdated", "_id", "date", "family"
	Desc  bool   // true when descending (-param or param:desc)
}

// CompositeTokenQuantityParam represents a FHIR R4 token$quantity composite search parameter
// (e.g., code-value-quantity=http://loinc.org|8867-4$gt70 or component-code-value-quantity=http://loinc.org|8480-6$lt140).
type CompositeTokenQuantityParam struct {
	ParamName   string     // "code-value-quantity", "component-code-value-quantity", or "combo-code-value-quantity"
	TokenSystem string     // Optional coding system before '|'
	TokenCode   string     // Coding code
	Quantity    QuantityOp // Parsed quantity operation after '$'
}

// CompositeTokenTokenParam represents a FHIR R4 token$token composite search parameter
// (e.g., code-value-concept=http://loinc.org|72166-2$http://snomed.info/sct|449868002).
type CompositeTokenTokenParam struct {
	ParamName   string // "code-value-concept", "component-code-value-concept", or "combo-code-value-concept"
	TokenSystem string // Optional coding system before '|' on the first token
	TokenCode   string // Coding code on the first token
	ValueSystem string // Optional coding system before '|' on the second token
	ValueCode   string // Coding code on the second token
}

// CompositeTokenDateParam represents a FHIR R4 token$date composite search parameter
// (e.g., code-value-date=http://loinc.org|11778-8$ge2026-01-01).
type CompositeTokenDateParam struct {
	ParamName   string // "code-value-date"
	TokenSystem string // Optional coding system before '|'
	TokenCode   string // Coding code
	Date        DateOp // Parsed date operation after '$'
}

// CompositeTokenStringParam represents a FHIR R4 token$string composite search parameter
// (e.g., code-value-string=http://loinc.org|95541-9$digital).
type CompositeTokenStringParam struct {
	ParamName   string // "code-value-string"
	TokenSystem string // Optional coding system before '|'
	TokenCode   string // Coding code
	Value       string // Normalized string prefix/value after '$'
}

var knownDatePrefixes = map[string]bool{
	"eq": true, "ne": true, "lt": true, "le": true,
	"gt": true, "ge": true, "sa": true, "eb": true, "ap": true,
}

var knownQuantityPrefixes = map[string]bool{
	"eq": true, "ne": true, "lt": true, "le": true,
	"gt": true, "ge": true, "sa": true, "eb": true, "ap": true,
}

// FilterExpr represents a node in a parsed FHIR R4 _filter boolean expression AST (§3.1.1.3.1).
type FilterExpr struct {
	Kind     string       // "and", "or", "not", "param"
	Children []FilterExpr // Child expressions for "and", "or", "not"
	Param    string       // Search parameter path for Kind == "param" (e.g. "code", "status", "patient.name")
	Op       string       // Comparison operator: eq, ne, co, sw, ew, gt, lt, ge, le, sa, eb, ap, pr, po, ss, sb, in, ni, re
	Value    string       // Comparison value (unquoted)
}

var knownFilterOps = map[string]bool{
	"eq": true, "ne": true, "co": true, "sw": true, "ew": true,
	"gt": true, "lt": true, "ge": true, "le": true,
	"sa": true, "eb": true, "ap": true, "pr": true, "po": true,
	"ss": true, "sb": true, "in": true, "ni": true, "re": true,
}

var dateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02",
	"2006-01",
	"2006",
}

// ParseDateOp parses a FHIR date query parameter value into a DateOp.
// Format: [prefix][date] (e.g., "ge2023-01-01", "lt1990-05-12T00:00:00Z", "1980-01-01")
func ParseDateOp(raw string) (DateOp, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 {
		return DateOp{}, fmt.Errorf("empty date string")
	}

	prefix := "eq"
	datePart := raw

	if len(raw) > 2 {
		potentialPrefix := raw[:2]
		if knownDatePrefixes[potentialPrefix] {
			prefix = potentialPrefix
			datePart = raw[2:]
		}
	}

	var parsedTime time.Time
	var highTime time.Time
	var parseErr error

	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, datePart); err == nil {
			parsedTime = t.UTC()
			switch layout {
			case "2006":
				highTime = parsedTime.AddDate(1, 0, 0)
			case "2006-01":
				highTime = parsedTime.AddDate(0, 1, 0)
			case "2006-01-02":
				highTime = parsedTime.AddDate(0, 0, 1)
			default:
				highTime = parsedTime
			}
			parseErr = nil
			break
		} else {
			parseErr = err
		}
	}

	if parseErr != nil {
		return DateOp{}, fmt.Errorf("invalid date format %q: %w", datePart, parseErr)
	}

	return DateOp{
		Prefix: prefix,
		Value:  parsedTime,
		High:   highTime,
	}, nil
}

// ParseQuantityOp parses a FHIR quantity query parameter.
// Format: [prefix][number]|[system]|[code] (e.g., "gt120|http://unitsofmeasure.org|mm[Hg]", "5.4")
func ParseQuantityOp(raw string) (QuantityOp, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 {
		return QuantityOp{}, fmt.Errorf("empty quantity string")
	}

	prefix := "eq"
	remainder := raw

	if len(raw) > 2 {
		potentialPrefix := raw[:2]
		if knownQuantityPrefixes[potentialPrefix] {
			prefix = potentialPrefix
			remainder = raw[2:]
		}
	}

	parts := strings.Split(remainder, "|")
	numVal, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return QuantityOp{}, fmt.Errorf("invalid quantity number %q: %w", parts[0], err)
	}

	op := QuantityOp{
		Prefix: prefix,
		Value:  numVal,
	}

	if len(parts) > 1 {
		op.System = parts[1]
	}
	if len(parts) > 2 {
		op.Code = parts[2]
	}

	return op, nil
}

// ParseInclude parses an _include or _revinclude parameter.
// Format: "*", "Source:param", or "Source:param:Target" (where Source or param may be "*").
func ParseInclude(raw string) (IncludeParam, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "*" {
		return IncludeParam{
			SourceType: "*",
			ParamName:  "*",
		}, nil
	}
	parts := strings.Split(trimmed, ":")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return IncludeParam{}, fmt.Errorf("invalid include format %q (expected Source:param or *)", raw)
	}

	param := IncludeParam{
		SourceType: strings.TrimSpace(parts[0]),
		ParamName:  strings.TrimSpace(parts[1]),
	}
	if len(parts) >= 3 {
		param.TargetType = strings.TrimSpace(parts[2])
	}

	return param, nil
}

// ParseChainedParam checks if a query parameter key represents a chained search.
// Supports single-hop ("refParam.targetParam", "refParam:TargetType.targetParam:modifier")
// and multi-hop ("patient.organization.name", "subject:Patient.general-practitioner:Practitioner.identifier") chains.
func ParseChainedParam(key, value string) (*ChainedParam, bool) {
	if !strings.Contains(key, ".") {
		return nil, false
	}

	segments := strings.Split(key, ".")
	if len(segments) < 2 {
		return nil, false
	}

	targetParamRaw := strings.TrimSpace(segments[len(segments)-1])
	if targetParamRaw == "" {
		return nil, false
	}
	targetParam, targetMod := splitParamModifier(targetParamRaw)
	if strings.TrimSpace(targetParam) == "" {
		return nil, false
	}

	hops := make([]ChainedHop, 0, len(segments)-1)
	for _, seg := range segments[:len(segments)-1] {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return nil, false
		}
		refParam := seg
		targetType := ""
		if colonIdx := strings.IndexByte(seg, ':'); colonIdx != -1 {
			refParam = strings.TrimSpace(seg[:colonIdx])
			targetType = strings.TrimSpace(seg[colonIdx+1:])
		}
		if refParam == "" {
			return nil, false
		}
		if targetType == "" {
			targetType = inferChainedTargetType(refParam)
		}
		hops = append(hops, ChainedHop{
			RefParam:   refParam,
			TargetType: targetType,
		})
	}

	paramType := "token"
	switch {
	case targetParam == "_id":
		paramType = "id"
	case targetMod == "text" || knownStringSearchParams[targetParam]:
		paramType = "string"
	case knownDateSearchParams[targetParam]:
		paramType = "date"
	case knownQuantitySearchParams[targetParam]:
		paramType = "quantity"
	case knownRefSearchParams[targetParam]:
		paramType = "reference"
	case knownURISearchParams[targetParam] || ((targetMod == "below" || targetMod == "above") && !knownTokenSearchParams[targetParam] && !strings.Contains(value, "|")):
		paramType = "uri"
	}

	return &ChainedParam{
		RefParam:       hops[0].RefParam,
		TargetType:     hops[len(hops)-1].TargetType,
		Hops:           hops,
		TargetParam:    targetParam,
		TargetModifier: targetMod,
		ParamType:      paramType,
		Value:          value,
	}, true
}

func inferChainedTargetType(refParam string) string {
	switch refParam {
	case "patient", "subject", "beneficiary", "subscriber":
		return "Patient"
	case "encounter":
		return "Encounter"
	case "performer", "general-practitioner", "requester", "asserter", "recorder", "results-interpreter", "collector", "practitioner", "author", "agent", "individual", "participant":
		return "Practitioner"
	case "organization", "partof", "part-of", "manufacturer", "intended-dispenser", "payor", "custodian", "service-provider":
		return "Organization"
	case "location":
		return "Location"
	case "medication":
		return "Medication"
	case "device":
		return "Device"
	case "questionnaire":
		return "Questionnaire"
	default:
		return ""
	}
}

// ParseHasParam parses a FHIR R4 reverse-chaining parameter (_has:SourceType:refParam:searchParam=value),
// including multi-hop recursive chains (_has:Encounter:patient:_has:Observation:encounter:code=8867-4).
func ParseHasParam(key, value string) (*HasParam, bool, error) {
	if !strings.HasPrefix(key, "_has:") {
		return nil, false, nil
	}
	rest := strings.TrimSpace(key[len("_has:"):])
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[2]) == "" {
		return nil, true, fmt.Errorf("invalid _has parameter %q: expected _has:{SourceType}:{refParam}:{searchParam}", key)
	}
	if strings.TrimSpace(value) == "" {
		return nil, true, fmt.Errorf("invalid _has parameter %q: empty value", key)
	}
	sourceType := strings.TrimSpace(parts[0])
	refParam := strings.TrimSpace(parts[1])
	searchParam := strings.TrimSpace(parts[2])

	if strings.HasPrefix(searchParam, "_has:") {
		child, ok, err := ParseHasParam(searchParam, value)
		if err != nil {
			return nil, true, err
		}
		if !ok || child == nil {
			return nil, true, fmt.Errorf("invalid nested _has parameter %q", key)
		}
		return &HasParam{
			SourceType:  sourceType,
			RefParam:    refParam,
			SearchParam: searchParam,
			ParamType:   "has",
			Value:       value,
			Child:       child,
		}, true, nil
	}

	searchBase, searchMod := splitParamModifier(searchParam)
	if searchMod == "of-type" {
		for part := range strings.SplitSeq(value, ",") {
			if err := validateOfTypeValue(key, part); err != nil {
				return nil, true, err
			}
		}
	}

	paramType := "token"
	switch {
	case searchBase == "_id":
		paramType = "id"
	case searchMod == "text" || knownStringSearchParams[searchBase]:
		paramType = "string"
	case knownDateSearchParams[searchBase]:
		paramType = "date"
		if _, err := ParseDateOp(value); err != nil {
			return nil, true, fmt.Errorf("invalid _has date value %s=%q: %w", key, value, err)
		}
	case knownQuantitySearchParams[searchBase]:
		paramType = "quantity"
		if _, err := ParseQuantityOp(value); err != nil {
			return nil, true, fmt.Errorf("invalid _has quantity value %s=%q: %w", key, value, err)
		}
	case knownRefSearchParams[searchBase]:
		paramType = "reference"
	case knownURISearchParams[searchBase]:
		paramType = "uri"
	}

	return &HasParam{
		SourceType:  sourceType,
		RefParam:    refParam,
		SearchParam: searchParam,
		ParamType:   paramType,
		Value:       value,
	}, true, nil
}

func validateOfTypeValue(paramName, raw string) error {
	s := strings.TrimSpace(raw)
	parts := strings.SplitN(s, "|", 3)
	if len(parts) != 3 || strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[2]) == "" {
		return fmt.Errorf("invalid :of-type parameter %s=%q: expected [system]|[code]|[value]", paramName, raw)
	}
	return nil
}

// ParseSortSpecs parses comma-separated _sort directives (e.g. "-date,family,_lastUpdated:desc").
func ParseSortSpecs(raw string) ([]SortSpec, error) {
	var out []SortSpec
	for part := range strings.SplitSeq(raw, ",") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		desc := false
		if strings.HasPrefix(s, "-") {
			desc = true
			s = strings.TrimSpace(s[1:])
		} else if strings.HasPrefix(s, "+") {
			s = strings.TrimSpace(s[1:])
		}
		if colonIdx := strings.IndexByte(s, ':'); colonIdx != -1 {
			dir := strings.ToLower(strings.TrimSpace(s[colonIdx+1:]))
			s = strings.TrimSpace(s[:colonIdx])
			switch dir {
			case "desc":
				desc = true
			case "asc":
				desc = false
			default:
				return nil, fmt.Errorf("invalid _sort direction %q in %q (expected :asc or :desc)", dir, part)
			}
		}
		if s == "" {
			return nil, fmt.Errorf("invalid empty _sort field in %q", raw)
		}
		if strings.EqualFold(s, "birthdate") {
			s = "birthdate"
		}
		out = append(out, SortSpec{Param: s, Desc: desc})
	}
	return out, nil
}

// ParseCompositeTokenQuantity parses a single FHIR R4 token$quantity composite search parameter item.
func ParseCompositeTokenQuantity(paramName, raw string) (CompositeTokenQuantityParam, error) {
	s := strings.TrimSpace(raw)
	parts := strings.SplitN(s, "$", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return CompositeTokenQuantityParam{}, fmt.Errorf("invalid composite parameter %s=%q: expected <token>$<quantity>", paramName, raw)
	}
	tokenPart := strings.TrimSpace(parts[0])
	qtyPart := strings.TrimSpace(parts[1])

	var sys, code string
	if tParts := strings.SplitN(tokenPart, "|", 2); len(tParts) == 2 {
		sys = strings.TrimSpace(tParts[0])
		code = strings.TrimSpace(tParts[1])
	} else {
		code = tokenPart
	}
	if code == "" {
		return CompositeTokenQuantityParam{}, fmt.Errorf("invalid composite parameter %s=%q: missing token code", paramName, raw)
	}
	qOp, err := ParseQuantityOp(qtyPart)
	if err != nil {
		return CompositeTokenQuantityParam{}, fmt.Errorf("invalid composite quantity in %s=%q: %w", paramName, raw, err)
	}
	return CompositeTokenQuantityParam{
		ParamName:   paramName,
		TokenSystem: sys,
		TokenCode:   code,
		Quantity:    qOp,
	}, nil
}

// ParseCompositeTokenToken parses a single FHIR R4 token$token composite search parameter item.
func ParseCompositeTokenToken(paramName, raw string) (CompositeTokenTokenParam, error) {
	s := strings.TrimSpace(raw)
	parts := strings.SplitN(s, "$", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return CompositeTokenTokenParam{}, fmt.Errorf("invalid composite parameter %s=%q: expected <token>$<token>", paramName, raw)
	}
	parseTok := func(p string) (string, string) {
		if tParts := strings.SplitN(p, "|", 2); len(tParts) == 2 {
			return strings.TrimSpace(tParts[0]), strings.TrimSpace(tParts[1])
		}
		return "", strings.TrimSpace(p)
	}
	tSys, tCode := parseTok(parts[0])
	vSys, vCode := parseTok(parts[1])
	if tCode == "" || vCode == "" {
		return CompositeTokenTokenParam{}, fmt.Errorf("invalid composite parameter %s=%q: missing token code", paramName, raw)
	}
	return CompositeTokenTokenParam{
		ParamName:   paramName,
		TokenSystem: tSys,
		TokenCode:   tCode,
		ValueSystem: vSys,
		ValueCode:   vCode,
	}, nil
}

// ParseCompositeTokenDate parses a single FHIR R4 token$date composite search parameter item.
func ParseCompositeTokenDate(paramName, raw string) (CompositeTokenDateParam, error) {
	s := strings.TrimSpace(raw)
	parts := strings.SplitN(s, "$", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return CompositeTokenDateParam{}, fmt.Errorf("invalid composite parameter %s=%q: expected <token>$<date>", paramName, raw)
	}
	var tSys, tCode string
	if tParts := strings.SplitN(strings.TrimSpace(parts[0]), "|", 2); len(tParts) == 2 {
		tSys = strings.TrimSpace(tParts[0])
		tCode = strings.TrimSpace(tParts[1])
	} else {
		tCode = strings.TrimSpace(parts[0])
	}
	if tCode == "" {
		return CompositeTokenDateParam{}, fmt.Errorf("invalid composite parameter %s=%q: missing token code", paramName, raw)
	}
	dOp, err := ParseDateOp(parts[1])
	if err != nil {
		return CompositeTokenDateParam{}, fmt.Errorf("invalid composite date in %s=%q: %w", paramName, raw, err)
	}
	return CompositeTokenDateParam{
		ParamName:   paramName,
		TokenSystem: tSys,
		TokenCode:   tCode,
		Date:        dOp,
	}, nil
}

// ParseCompositeTokenString parses a single FHIR R4 token$string composite search parameter item.
func ParseCompositeTokenString(paramName, raw string) (CompositeTokenStringParam, error) {
	s := strings.TrimSpace(raw)
	parts := strings.SplitN(s, "$", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return CompositeTokenStringParam{}, fmt.Errorf("invalid composite parameter %s=%q: expected <token>$<string>", paramName, raw)
	}
	var tSys, tCode string
	if tParts := strings.SplitN(strings.TrimSpace(parts[0]), "|", 2); len(tParts) == 2 {
		tSys = strings.TrimSpace(tParts[0])
		tCode = strings.TrimSpace(tParts[1])
	} else {
		tCode = strings.TrimSpace(parts[0])
	}
	valStr := strings.TrimSpace(parts[1])
	if tCode == "" || valStr == "" {
		return CompositeTokenStringParam{}, fmt.Errorf("invalid composite parameter %s=%q: missing token code or string value", paramName, raw)
	}
	return CompositeTokenStringParam{
		ParamName:   paramName,
		TokenSystem: tSys,
		TokenCode:   tCode,
		Value:       valStr,
	}, nil
}

var (
	knownStringSearchParams = map[string]bool{
		"family": true, "given": true, "name": true,
		"address": true, "address-city": true, "address-state": true,
		"address-postalcode": true, "address-country": true,
		"phonetic": true, "lot-number": true, "series": true,
		"title": true, "description": true, "udi-carrier": true, "device-name": true,
		"value-string": true,
	}
	knownTokenSearchParams = map[string]bool{
		"identifier": true, "gender": true, "active": true,
		"email": true, "phone": true, "telecom": true, "language": true,
		"deceased": true, "status": true, "code": true,
		"component-code": true, "combo-code": true,
		"category": true, "type": true, "class": true,
		"clinical-status": true, "verification-status": true,
		"lifecycle-status": true, "docstatus": true, "mode": true,
		"intent": true, "priority": true, "criticality": true,
		"severity": true, "manifestation": true, "route": true,
		"reason-code": true, "conclusion": true, "vaccine-code": true,
		"status-reason": true, "target-disease": true,
		"intended-performertype": true, "performer-type": true,
		"address-use": true, "telecom-use": true,
		"accession": true, "bodysite": true, "specialty": true,
		"role": true, "relationship": true, "subscriber-id": true,
		"value-concept": true, "component-value-concept": true, "combo-value-concept": true,
		"race": true, "ethnicity": true, "birthsex": true, "marital-status": true,
		"_tag": true, "_security": true,
	}
	knownRefSearchParams = map[string]bool{
		"organization": true, "general-practitioner": true,
		"subject": true, "patient": true, "beneficiary": true, "encounter": true,
		"performer": true, "asserter": true, "recorder": true,
		"participant": true, "requester": true, "medication": true,
		"intended-dispenser": true, "intended-performer": true,
		"based-on": true, "location": true, "part-of": true, "partof": true,
		"reason-reference": true, "result": true, "results-interpreter": true,
		"specimen": true, "collector": true, "media": true, "manufacturer": true,
		"reaction": true, "endpoint": true, "instantiates-canonical": true,
		"target": true, "agent": true, "author": true, "custodian": true,
		"payor": true, "subscriber": true, "practitioner": true, "device": true,
		"questionnaire": true, "source": true, "care-team": true, "goal": true,
		"focus": true, "owner": true, "for": true, "actor": true,
		"item": true, "member": true, "entity": true, "service-provider": true,
	}
	knownDateSearchParams = map[string]bool{
		"birthdate": true, "date": true, "onset-date": true,
		"authoredon": true, "authored": true, "issued": true, "last-date": true,
		"collected": true, "period": true, "recorded": true, "created": true,
		"indexed": true, "target-date": true, "start-date": true,
		"occurrence": true, "whenhandedover": true, "whenprepared": true,
		"value-date": true,
	}
	knownQuantitySearchParams = map[string]bool{
		"value-quantity":           true,
		"component-value-quantity": true,
		"combo-value-quantity":     true,
	}
	knownCompositeTokenQuantityParams = map[string]bool{
		"code-value-quantity":           true,
		"component-code-value-quantity": true,
		"combo-code-value-quantity":     true,
	}
	knownCompositeTokenTokenParams = map[string]bool{
		"code-value-concept":           true,
		"component-code-value-concept": true,
		"combo-code-value-concept":     true,
	}
	knownCompositeTokenDateParams = map[string]bool{
		"code-value-date": true,
	}
	knownCompositeTokenStringParams = map[string]bool{
		"code-value-string": true,
	}
	knownURISearchParams = map[string]bool{
		"url":              true,
		"system":           true,
		"source-uri":       true,
		"target-uri":       true,
		"instantiates-uri": true,
		"definition":       true,
		"_profile":         true,
		"_source":          true,
	}
	knownControlUnderscoreParams = map[string]bool{
		"_id": true, "_lastUpdated": true, "_tag": true, "_profile": true,
		"_security": true, "_source": true, "_text": true, "_content": true,
		"_list": true, "_has": true, "_type": true, "_sort": true,
		"_count": true, "_offset": true, "_include": true, "_revinclude": true,
		"_summary": true, "_total": true, "_elements": true, "_contained": true,
		"_containedType": true, "_format": true, "_pretty": true, "_filter": true,
	}
)

// CustomSearchParamDef describes a runtime-registered FHIR R4 SearchParameter.
type CustomSearchParamDef struct {
	ID          string   // Resource ID of the SearchParameter
	URL         string   // Canonical URL
	Code        string   // Query parameter name (e.g., "race", "ethnicity", "birthsex", "eye-color")
	Base        []string // Target resource types (e.g., ["Patient"])
	Type        string   // "string", "token", "date", "quantity", "reference", "uri"
	Expression  string   // FHIRPath expression (e.g., "Patient.extension.where(url='...')" or "Patient.maritalStatus")
	Description string
}

var (
	customSearchParamMu     sync.RWMutex
	customSearchParamsByID  = map[string]CustomSearchParamDef{}
	builtinUSCoreExtensions = []CustomSearchParamDef{
		{
			ID:          "us-core-race",
			URL:         "http://hl7.org/fhir/us/core/SearchParameter/us-core-race",
			Code:        "race",
			Base:        []string{"Patient"},
			Type:        "token",
			Expression:  "Patient.extension.where(url = 'http://hl7.org/fhir/us/core/StructureDefinition/us-core-race')",
			Description: "Returns patients with a race extension matching the specified code.",
		},
		{
			ID:          "us-core-ethnicity",
			URL:         "http://hl7.org/fhir/us/core/SearchParameter/us-core-ethnicity",
			Code:        "ethnicity",
			Base:        []string{"Patient"},
			Type:        "token",
			Expression:  "Patient.extension.where(url = 'http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity')",
			Description: "Returns patients with an ethnicity extension matching the specified code.",
		},
		{
			ID:          "us-core-birthsex",
			URL:         "http://hl7.org/fhir/us/core/SearchParameter/us-core-birthsex",
			Code:        "birthsex",
			Base:        []string{"Patient"},
			Type:        "token",
			Expression:  "Patient.extension.where(url = 'http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex')",
			Description: "Returns patients with a birthsex extension matching the specified code.",
		},
	}
)

func cloneBoolMapWithKey(src map[string]bool, key string) map[string]bool {
	dst := make(map[string]bool, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	dst[key] = true
	return dst
}

// RegisterCustomSearchParam registers a custom FHIR R4 SearchParameter definition at runtime,
// making its code searchable, validated, and dynamically extracted across its base resource types.
func RegisterCustomSearchParam(def CustomSearchParamDef) {
	def.Code = strings.TrimSpace(def.Code)
	def.Type = strings.ToLower(strings.TrimSpace(def.Type))
	def.Expression = strings.TrimSpace(def.Expression)
	if def.Code == "" || def.Type == "" {
		return
	}
	key := strings.TrimSpace(def.ID)
	if key == "" {
		key = strings.Join(def.Base, ",") + ":" + def.Code
	}

	customSearchParamMu.Lock()
	defer customSearchParamMu.Unlock()
	customSearchParamsByID[key] = def

	switch def.Type {
	case "string":
		if !knownStringSearchParams[def.Code] {
			knownStringSearchParams = cloneBoolMapWithKey(knownStringSearchParams, def.Code)
		}
	case "token":
		if !knownTokenSearchParams[def.Code] {
			knownTokenSearchParams = cloneBoolMapWithKey(knownTokenSearchParams, def.Code)
		}
	case "date":
		if !knownDateSearchParams[def.Code] {
			knownDateSearchParams = cloneBoolMapWithKey(knownDateSearchParams, def.Code)
		}
	case "quantity":
		if !knownQuantitySearchParams[def.Code] {
			knownQuantitySearchParams = cloneBoolMapWithKey(knownQuantitySearchParams, def.Code)
		}
	case "reference":
		if !knownRefSearchParams[def.Code] {
			knownRefSearchParams = cloneBoolMapWithKey(knownRefSearchParams, def.Code)
		}
	case "uri":
		if !knownURISearchParams[def.Code] {
			knownURISearchParams = cloneBoolMapWithKey(knownURISearchParams, def.Code)
		}
	}
}

// UnregisterCustomSearchParam removes a custom SearchParameter by its resource ID or code.
func UnregisterCustomSearchParam(idOrCode string) {
	idOrCode = strings.TrimSpace(idOrCode)
	if idOrCode == "" {
		return
	}
	customSearchParamMu.Lock()
	defer customSearchParamMu.Unlock()
	delete(customSearchParamsByID, idOrCode)
	for k, def := range customSearchParamsByID {
		if def.ID == idOrCode || def.Code == idOrCode {
			delete(customSearchParamsByID, k)
		}
	}
}

// CustomSearchParamsForType returns all built-in US Core extension SearchParameters and
// runtime-registered custom SearchParameters applicable to resType.
func CustomSearchParamsForType(resType string) []CustomSearchParamDef {
	var out []CustomSearchParamDef
	seenCode := make(map[string]bool)
	for _, b := range builtinUSCoreExtensions {
		for _, base := range b.Base {
			if strings.EqualFold(base, resType) || base == "Resource" || base == "DomainResource" {
				out = append(out, b)
				seenCode[b.Code] = true
				break
			}
		}
	}
	customSearchParamMu.RLock()
	defer customSearchParamMu.RUnlock()
	for _, def := range customSearchParamsByID {
		for _, base := range def.Base {
			if strings.EqualFold(base, resType) || base == "Resource" || base == "DomainResource" {
				if seenCode[def.Code] {
					for i := range out {
						if out[i].Code == def.Code {
							out[i] = def
							break
						}
					}
				} else {
					out = append(out, def)
					seenCode[def.Code] = true
				}
				break
			}
		}
	}
	return out
}

type filterToken struct {
	val    string
	quoted bool
}

func tokenizeFilterExpr(raw string) ([]filterToken, error) {
	var tokens []filterToken
	n := len(raw)
	for i := 0; i < n; {
		ch := raw[i]
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
			i++
			continue
		}
		if ch == '(' || ch == ')' {
			tokens = append(tokens, filterToken{val: string(ch)})
			i++
			continue
		}
		if ch == '"' || ch == '\'' {
			quote := ch
			i++
			var sb strings.Builder
			closed := false
			for i < n {
				c := raw[i]
				if c == '\\' && i+1 < n {
					sb.WriteByte(raw[i+1])
					i += 2
					continue
				}
				if c == quote {
					closed = true
					i++
					break
				}
				sb.WriteByte(c)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated quoted string in _filter %q", raw)
			}
			tokens = append(tokens, filterToken{val: sb.String(), quoted: true})
			continue
		}
		start := i
		for i < n {
			c := raw[i]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '(' || c == ')' || c == '"' || c == '\'' {
				break
			}
			i++
		}
		tokens = append(tokens, filterToken{val: raw[start:i]})
	}
	return tokens, nil
}

// ParseFilterExpr parses a FHIR R4 _filter expression (§3.1.1.3.1) into a FilterExpr AST.
func ParseFilterExpr(raw string) (FilterExpr, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return FilterExpr{}, fmt.Errorf("empty _filter expression")
	}
	tokens, err := tokenizeFilterExpr(trimmed)
	if err != nil {
		return FilterExpr{}, err
	}
	if len(tokens) == 0 {
		return FilterExpr{}, fmt.Errorf("empty _filter expression")
	}

	pos := 0
	var parseOr func() (FilterExpr, error)
	var parseAnd func() (FilterExpr, error)
	var parseUnary func() (FilterExpr, error)
	var parseAtom func() (FilterExpr, error)

	parseOr = func() (FilterExpr, error) {
		left, err := parseAnd()
		if err != nil {
			return FilterExpr{}, err
		}
		children := []FilterExpr{left}
		for pos < len(tokens) && !tokens[pos].quoted && strings.EqualFold(tokens[pos].val, "or") {
			pos++
			right, err := parseAnd()
			if err != nil {
				return FilterExpr{}, err
			}
			children = append(children, right)
		}
		if len(children) == 1 {
			return children[0], nil
		}
		return FilterExpr{Kind: "or", Children: children}, nil
	}

	parseAnd = func() (FilterExpr, error) {
		left, err := parseUnary()
		if err != nil {
			return FilterExpr{}, err
		}
		children := []FilterExpr{left}
		for pos < len(tokens) && !tokens[pos].quoted && strings.EqualFold(tokens[pos].val, "and") {
			pos++
			right, err := parseUnary()
			if err != nil {
				return FilterExpr{}, err
			}
			children = append(children, right)
		}
		if len(children) == 1 {
			return children[0], nil
		}
		return FilterExpr{Kind: "and", Children: children}, nil
	}

	parseUnary = func() (FilterExpr, error) {
		if pos >= len(tokens) {
			return FilterExpr{}, fmt.Errorf("unexpected end of _filter expression %q", raw)
		}
		tok := tokens[pos]
		if !tok.quoted && strings.EqualFold(tok.val, "not") {
			pos++
			child, err := parseUnary()
			if err != nil {
				return FilterExpr{}, err
			}
			return FilterExpr{Kind: "not", Children: []FilterExpr{child}}, nil
		}
		if !tok.quoted && tok.val == "(" {
			pos++
			inner, err := parseOr()
			if err != nil {
				return FilterExpr{}, err
			}
			if pos >= len(tokens) || tokens[pos].quoted || tokens[pos].val != ")" {
				return FilterExpr{}, fmt.Errorf("missing closing ')' in _filter expression %q", raw)
			}
			pos++
			return inner, nil
		}
		return parseAtom()
	}

	parseAtom = func() (FilterExpr, error) {
		if pos+2 >= len(tokens) {
			return FilterExpr{}, fmt.Errorf("expected '<param> <op> <value>' in _filter expression %q", raw)
		}
		paramTok := tokens[pos]
		opTok := tokens[pos+1]
		valTok := tokens[pos+2]
		pos += 3

		if paramTok.quoted || paramTok.val == "(" || paramTok.val == ")" {
			return FilterExpr{}, fmt.Errorf("invalid parameter path %q in _filter expression %q", paramTok.val, raw)
		}
		if !IsKnownSearchParam(paramTok.val) {
			return FilterExpr{}, fmt.Errorf("unknown or unsupported search parameter %q in _filter", paramTok.val)
		}
		op := strings.ToLower(strings.TrimSpace(opTok.val))
		if opTok.quoted || !knownFilterOps[op] {
			return FilterExpr{}, fmt.Errorf("invalid comparison operator %q in _filter expression %q", opTok.val, raw)
		}
		if !valTok.quoted && (valTok.val == "(" || valTok.val == ")") {
			return FilterExpr{}, fmt.Errorf("invalid comparison value %q in _filter expression %q", valTok.val, raw)
		}
		valStr := strings.TrimSpace(valTok.val)
		if valStr == "" {
			return FilterExpr{}, fmt.Errorf("empty comparison value for %s %s in _filter", paramTok.val, op)
		}

		baseParam, _ := splitParamModifier(paramTok.val)
		if strings.Contains(paramTok.val, ".") {
			if ch, ok := ParseChainedParam(paramTok.val, valStr); ok && ch != nil {
				baseParam = ch.TargetParam
			}
		}
		if op == "pr" {
			lower := strings.ToLower(valStr)
			if lower != "true" && lower != "false" {
				return FilterExpr{}, fmt.Errorf("invalid 'pr' value %q for %s in _filter: expected true or false", valStr, paramTok.val)
			}
		} else if paramTok.val == "_lastUpdated" || knownDateSearchParams[baseParam] {
			if _, err := ParseDateOp(valStr); err != nil {
				return FilterExpr{}, fmt.Errorf("invalid date value %q for %s in _filter: %w", valStr, paramTok.val, err)
			}
		} else if knownQuantitySearchParams[baseParam] {
			if _, err := ParseQuantityOp(valStr); err != nil {
				return FilterExpr{}, fmt.Errorf("invalid quantity value %q for %s in _filter: %w", valStr, paramTok.val, err)
			}
		}

		return FilterExpr{
			Kind:  "param",
			Param: paramTok.val,
			Op:    op,
			Value: valStr,
		}, nil
	}

	expr, err := parseOr()
	if err != nil {
		return FilterExpr{}, err
	}
	if pos < len(tokens) {
		return FilterExpr{}, fmt.Errorf("unexpected token %q in _filter expression %q", tokens[pos].val, raw)
	}
	return expr, nil
}

// IsKnownSearchParam reports whether key is a recognized FHIR R4 search or control parameter.
// Used for Prefer: handling=strict / handling=lenient conformance.
func IsKnownSearchParam(key string) bool {
	if strings.HasPrefix(key, "_has:") {
		return true
	}
	base, mod := splitParamModifier(key)
	if strings.HasPrefix(base, "_") {
		return knownControlUnderscoreParams[base] || isSearchableUnderscoreParam(key)
	}
	if strings.Contains(key, ".") {
		ch, ok := ParseChainedParam(key, "x")
		if !ok || ch == nil {
			return false
		}
		return IsKnownSearchParam(ch.TargetParam)
	}
	if mod != "" {
		switch mod {
		case "exact", "contains", "text", "not", "missing", "in", "not-in", "below", "above", "of-type", "identifier":
		default:
			if len(mod) == 0 || mod[0] < 'A' || mod[0] > 'Z' {
				return false
			}
		}
	}
	return knownStringSearchParams[base] ||
		knownTokenSearchParams[base] ||
		knownRefSearchParams[base] ||
		knownDateSearchParams[base] ||
		knownQuantitySearchParams[base] ||
		knownCompositeTokenQuantityParams[base] ||
		knownCompositeTokenTokenParams[base] ||
		knownCompositeTokenDateParams[base] ||
		knownCompositeTokenStringParams[base] ||
		knownURISearchParams[base]
}

// isSearchableUnderscoreParam returns true for standard FHIR R4 Resource.meta search
// parameters (_profile, _tag, _security, _source, and any modifier variants such as :missing, :not, :below, :above).
func isSearchableUnderscoreParam(key string) bool {
	base, _ := splitParamModifier(key)
	switch base {
	case "_profile", "_tag", "_security", "_source":
		return true
	default:
		return false
	}
}

// splitParamModifier splits a search parameter key into its base parameter name
// and optional modifier (e.g. "name:exact" -> "name", "exact"; "status:not" -> "status", "not").
func splitParamModifier(key string) (string, string) {
	if idx := strings.IndexByte(key, ':'); idx != -1 {
		return key[:idx], key[idx+1:]
	}
	return key, ""
}

// ValidateSearchQuery validates that all date, quantity, pagination, chained, and include
// query parameters in q are syntactically well-formed.
func ValidateSearchQuery(q url.Values) error {
	for _, v := range q["_count"] {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			n, err := strconv.Atoi(trimmed)
			if err != nil || n <= 0 {
				return fmt.Errorf("invalid _count parameter %q: must be a positive integer", v)
			}
		}
	}
	for _, v := range q["_offset"] {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			n, err := strconv.Atoi(trimmed)
			if err != nil || n < 0 {
				return fmt.Errorf("invalid _offset parameter %q: must be a non-negative integer", v)
			}
		}
	}
	for _, listVal := range q["_list"] {
		hasNonEmpty := false
		for part := range strings.SplitSeq(listVal, ",") {
			if strings.TrimSpace(part) != "" {
				hasNonEmpty = true
				break
			}
		}
		if !hasNonEmpty {
			return fmt.Errorf("invalid _list parameter %q: empty value", listVal)
		}
	}
	for _, k := range []string{"_include", "_include:iterate", "_include:recurse"} {
		for _, incVal := range q[k] {
			if _, err := ParseInclude(incVal); err != nil {
				return err
			}
		}
	}
	for _, k := range []string{"_revinclude", "_revinclude:iterate", "_revinclude:recurse"} {
		for _, revVal := range q[k] {
			if _, err := ParseInclude(revVal); err != nil {
				return err
			}
		}
	}
	for _, sortVal := range q["_sort"] {
		if _, err := ParseSortSpecs(sortVal); err != nil {
			return err
		}
	}
	for _, sumVal := range q["_summary"] {
		switch strings.ToLower(strings.TrimSpace(sumVal)) {
		case "", "true", "false", "text", "data", "count":
		default:
			return fmt.Errorf("invalid _summary parameter %q: expected true, false, text, data, or count", sumVal)
		}
	}
	for _, totVal := range q["_total"] {
		switch strings.ToLower(strings.TrimSpace(totVal)) {
		case "", "none", "estimate", "accurate":
		default:
			return fmt.Errorf("invalid _total parameter %q: expected none, estimate, or accurate", totVal)
		}
	}
	for _, luVal := range q["_lastUpdated"] {
		for part := range strings.SplitSeq(luVal, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				if _, err := ParseDateOp(trimmed); err != nil {
					return fmt.Errorf("invalid _lastUpdated parameter %q: %w", luVal, err)
				}
			}
		}
	}
	for _, fVal := range q["_filter"] {
		if _, err := ParseFilterExpr(fVal); err != nil {
			return err
		}
	}
	for _, cVal := range q["_content"] {
		if strings.TrimSpace(cVal) == "" {
			return fmt.Errorf("invalid _content parameter %q: empty value", cVal)
		}
	}
	for _, tVal := range q["_text"] {
		if strings.TrimSpace(tVal) == "" {
			return fmt.Errorf("invalid _text parameter %q: empty value", tVal)
		}
	}
	for key, values := range q {
		if strings.HasPrefix(key, "_has:") {
			for _, val := range values {
				if _, _, err := ParseHasParam(key, val); err != nil {
					return err
				}
			}
			continue
		}
		if (strings.HasPrefix(key, "_") && !isSearchableUnderscoreParam(key)) || len(values) == 0 {
			continue
		}
		baseKey, mod := splitParamModifier(key)
		for _, val := range values {
			if mod == "missing" {
				lower := strings.ToLower(strings.TrimSpace(val))
				if lower != "true" && lower != "false" {
					return fmt.Errorf("invalid :missing modifier value %s=%q: expected true or false", key, val)
				}
				continue
			}
			if mod == "of-type" {
				for part := range strings.SplitSeq(val, ",") {
					if err := validateOfTypeValue(key, part); err != nil {
						return err
					}
				}
				continue
			}
			if chained, ok := ParseChainedParam(key, val); ok {
				if chained.TargetModifier == "of-type" {
					for part := range strings.SplitSeq(chained.Value, ",") {
						if err := validateOfTypeValue(key, part); err != nil {
							return err
						}
					}
				}
				switch chained.ParamType {
				case "date":
					if _, err := ParseDateOp(chained.Value); err != nil {
						return fmt.Errorf("invalid chained date parameter %s=%q: %w", key, val, err)
					}
				case "quantity":
					if _, err := ParseQuantityOp(chained.Value); err != nil {
						return fmt.Errorf("invalid chained quantity parameter %s=%q: %w", key, val, err)
					}
				}
				continue
			}
			if knownCompositeTokenQuantityParams[baseKey] {
				for part := range strings.SplitSeq(val, ",") {
					if _, err := ParseCompositeTokenQuantity(baseKey, part); err != nil {
						return err
					}
				}
				continue
			}
			if knownCompositeTokenTokenParams[baseKey] {
				for part := range strings.SplitSeq(val, ",") {
					if _, err := ParseCompositeTokenToken(baseKey, part); err != nil {
						return err
					}
				}
				continue
			}
			if knownCompositeTokenDateParams[baseKey] {
				for part := range strings.SplitSeq(val, ",") {
					if _, err := ParseCompositeTokenDate(baseKey, part); err != nil {
						return err
					}
				}
				continue
			}
			if knownCompositeTokenStringParams[baseKey] {
				for part := range strings.SplitSeq(val, ",") {
					if _, err := ParseCompositeTokenString(baseKey, part); err != nil {
						return err
					}
				}
				continue
			}
			if knownDateSearchParams[baseKey] {
				for part := range strings.SplitSeq(val, ",") {
					if _, err := ParseDateOp(part); err != nil {
						return fmt.Errorf("invalid date parameter %s=%q: %w", key, val, err)
					}
				}
			} else if knownQuantitySearchParams[baseKey] {
				for part := range strings.SplitSeq(val, ",") {
					if _, err := ParseQuantityOp(part); err != nil {
						return fmt.Errorf("invalid quantity parameter %s=%q: %w", key, val, err)
					}
				}
			}
		}
	}
	return nil
}

// BuildSearchParamsFromQuery converts URL query values into a typed SearchParams struct.
func BuildSearchParamsFromQuery(tenant, resType string, q url.Values) SearchParams {
	params := SearchParams{
		TenantID:         tenant,
		ResType:          resType,
		Strings:          make(map[string]string),
		StringFilters:    make(map[string][]string),
		Tokens:           make(map[string]string),
		TokenFilters:     make(map[string][]string),
		Dates:            make(map[string]DateOp),
		DateFilters:      make(map[string][]DateOp),
		Quantities:       make(map[string]QuantityOp),
		QuantityFilters:  make(map[string][]QuantityOp),
		References:       make(map[string]string),
		ReferenceFilters: make(map[string][]string),
		URIs:             make(map[string]string),
		URIFilters:       make(map[string][]string),
		Missing:          make(map[string]bool),
		Summary:          strings.ToLower(strings.TrimSpace(q.Get("_summary"))),
		TotalMode:        strings.ToLower(strings.TrimSpace(q.Get("_total"))),
	}

	for _, typeRaw := range q["_type"] {
		for part := range strings.SplitSeq(typeRaw, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				params.ResTypes = append(params.ResTypes, trimmed)
			}
		}
	}
	for _, listRaw := range q["_list"] {
		if trimmed := strings.TrimSpace(listRaw); trimmed != "" {
			params.List = append(params.List, trimmed)
		}
	}
	for _, contentRaw := range q["_content"] {
		if trimmed := strings.TrimSpace(contentRaw); trimmed != "" {
			params.Content = append(params.Content, trimmed)
		}
	}
	for _, textRaw := range q["_text"] {
		if trimmed := strings.TrimSpace(textRaw); trimmed != "" {
			params.Text = append(params.Text, trimmed)
		}
	}
	for _, filterRaw := range q["_filter"] {
		if expr, err := ParseFilterExpr(filterRaw); err == nil {
			params.Filters = append(params.Filters, expr)
		}
	}
	for _, elRaw := range q["_elements"] {
		for part := range strings.SplitSeq(elRaw, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				params.Elements = append(params.Elements, trimmed)
			}
		}
	}
	for _, sortVal := range q["_sort"] {
		if specs, err := ParseSortSpecs(sortVal); err == nil {
			params.Sort = append(params.Sort, specs...)
		}
	}

	if idVals := q["_id"]; len(idVals) > 0 {
		var cleanedIDs []string
		for _, rawID := range idVals {
			for part := range strings.SplitSeq(rawID, ",") {
				idVal := strings.TrimSpace(part)
				if idVal == "" {
					continue
				}
				if idx := strings.LastIndex(idVal, "/"); idx != -1 {
					idVal = idVal[idx+1:]
				}
				cleanedIDs = append(cleanedIDs, idVal)
			}
		}
		params.ID = strings.Join(cleanedIDs, ",")
	}
	for _, luVal := range q["_lastUpdated"] {
		for part := range strings.SplitSeq(luVal, ",") {
			if dOp, err := ParseDateOp(part); err == nil {
				params.LastUpdated = append(params.LastUpdated, dOp)
			}
		}
	}
	if v := q.Get("_count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Count = n
		}
	}
	if v := q.Get("_offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Offset = n
		}
	}

	for _, incVal := range q["_include"] {
		if inc, err := ParseInclude(incVal); err == nil {
			params.Includes = append(params.Includes, inc)
		}
	}
	for _, k := range []string{"_include:iterate", "_include:recurse"} {
		for _, incVal := range q[k] {
			if inc, err := ParseInclude(incVal); err == nil {
				inc.Iterate = true
				params.Includes = append(params.Includes, inc)
			}
		}
	}
	for _, revVal := range q["_revinclude"] {
		if rev, err := ParseInclude(revVal); err == nil {
			params.RevIncludes = append(params.RevIncludes, rev)
		}
	}
	for _, k := range []string{"_revinclude:iterate", "_revinclude:recurse"} {
		for _, revVal := range q[k] {
			if rev, err := ParseInclude(revVal); err == nil {
				rev.Iterate = true
				params.RevIncludes = append(params.RevIncludes, rev)
			}
		}
	}

	for key, values := range q {
		if strings.HasPrefix(key, "_has:") {
			for _, rawVal := range values {
				if hp, ok, err := ParseHasParam(key, rawVal); ok && err == nil && hp != nil {
					params.Has = append(params.Has, *hp)
				}
			}
			continue
		}
		if (strings.HasPrefix(key, "_") && !isSearchableUnderscoreParam(key)) || len(values) == 0 {
			continue
		}
		value := values[0]

		if _, ok := ParseChainedParam(key, value); ok {
			for _, rawVal := range values {
				if ch, ok := ParseChainedParam(key, rawVal); ok {
					params.Chained = append(params.Chained, *ch)
				}
			}
			continue
		}

		baseKey, mod := splitParamModifier(key)
		if mod == "missing" {
			params.Missing[baseKey] = strings.EqualFold(strings.TrimSpace(value), "true")
			continue
		}

		switch {
		case mod == "text":
			params.Strings[key] = value
			params.StringFilters[key] = append(params.StringFilters[key], values...)
		case knownCompositeTokenQuantityParams[baseKey]:
			for _, rawVal := range values {
				var orGroup []CompositeTokenQuantityParam
				for part := range strings.SplitSeq(rawVal, ",") {
					if cp, err := ParseCompositeTokenQuantity(baseKey, part); err == nil {
						orGroup = append(orGroup, cp)
					}
				}
				if len(orGroup) > 0 {
					params.CompositeTokenQuantities = append(params.CompositeTokenQuantities, orGroup)
				}
			}
		case knownCompositeTokenTokenParams[baseKey]:
			for _, rawVal := range values {
				var orGroup []CompositeTokenTokenParam
				for part := range strings.SplitSeq(rawVal, ",") {
					if cp, err := ParseCompositeTokenToken(baseKey, part); err == nil {
						orGroup = append(orGroup, cp)
					}
				}
				if len(orGroup) > 0 {
					params.CompositeTokenTokens = append(params.CompositeTokenTokens, orGroup)
				}
			}
		case knownCompositeTokenDateParams[baseKey]:
			for _, rawVal := range values {
				var orGroup []CompositeTokenDateParam
				for part := range strings.SplitSeq(rawVal, ",") {
					if cp, err := ParseCompositeTokenDate(baseKey, part); err == nil {
						orGroup = append(orGroup, cp)
					}
				}
				if len(orGroup) > 0 {
					params.CompositeTokenDates = append(params.CompositeTokenDates, orGroup)
				}
			}
		case knownCompositeTokenStringParams[baseKey]:
			for _, rawVal := range values {
				var orGroup []CompositeTokenStringParam
				for part := range strings.SplitSeq(rawVal, ",") {
					if cp, err := ParseCompositeTokenString(baseKey, part); err == nil {
						orGroup = append(orGroup, cp)
					}
				}
				if len(orGroup) > 0 {
					params.CompositeTokenStrings = append(params.CompositeTokenStrings, orGroup)
				}
			}
		case knownURISearchParams[baseKey]:
			params.URIs[key] = value
			params.URIFilters[key] = append(params.URIFilters[key], values...)
		case knownStringSearchParams[baseKey]:
			params.Strings[key] = value
			params.StringFilters[key] = append(params.StringFilters[key], values...)
		case knownTokenSearchParams[baseKey]:
			params.Tokens[key] = value
			params.TokenFilters[key] = append(params.TokenFilters[key], values...)
		case knownRefSearchParams[baseKey]:
			params.References[key] = value
			params.ReferenceFilters[key] = append(params.ReferenceFilters[key], values...)
		case (mod == "below" || mod == "above") && !strings.Contains(value, "|"):
			params.URIs[key] = value
			params.URIFilters[key] = append(params.URIFilters[key], values...)
		case knownDateSearchParams[baseKey]:
			for _, rawVal := range values {
				for part := range strings.SplitSeq(rawVal, ",") {
					if dOp, err := ParseDateOp(part); err == nil {
						if _, exists := params.Dates[baseKey]; !exists {
							params.Dates[baseKey] = dOp
						}
						params.DateFilters[baseKey] = append(params.DateFilters[baseKey], dOp)
					}
				}
			}
		case knownQuantitySearchParams[baseKey]:
			for _, rawVal := range values {
				for part := range strings.SplitSeq(rawVal, ",") {
					if qOp, err := ParseQuantityOp(part); err == nil {
						if _, exists := params.Quantities[baseKey]; !exists {
							params.Quantities[baseKey] = qOp
						}
						params.QuantityFilters[baseKey] = append(params.QuantityFilters[baseKey], qOp)
					}
				}
			}
		default:
			params.Tokens[key] = value
			params.TokenFilters[key] = append(params.TokenFilters[key], values...)
		}
	}

	return params
}

// ParseIfNoneExist parses an HTTP If-None-Exist header (or Bundle entry request.ifNoneExist)
// into a SearchParams query for conditional create evaluation.
func ParseIfNoneExist(tenant, resType, rawHeader string) (SearchParams, error) {
	s := strings.TrimSpace(rawHeader)
	if s == "" {
		return SearchParams{}, fmt.Errorf("empty If-None-Exist value")
	}

	// Support optional resource type prefix (e.g., "Patient?identifier=123" or "?identifier=123")
	if qIdx := strings.IndexByte(s, '?'); qIdx != -1 {
		prefix := strings.TrimSpace(s[:qIdx])
		if prefix != "" && !strings.EqualFold(prefix, resType) {
			return SearchParams{}, fmt.Errorf("If-None-Exist resource prefix %q does not match %q", prefix, resType)
		}
		s = s[qIdx+1:]
	}

	q, err := url.ParseQuery(s)
	if err != nil {
		return SearchParams{}, fmt.Errorf("invalid If-None-Exist query %q: %w", rawHeader, err)
	}
	if len(q) == 0 {
		return SearchParams{}, fmt.Errorf("If-None-Exist query %q contains no search parameters", rawHeader)
	}
	for k, vals := range q {
		if strings.TrimSpace(k) == "" || len(vals) == 0 {
			return SearchParams{}, fmt.Errorf("If-None-Exist query %q contains empty parameter key or value", rawHeader)
		}
		if k == "_include" || k == "_revinclude" {
			return SearchParams{}, fmt.Errorf("If-None-Exist query %q cannot contain %s", rawHeader, k)
		}
		for _, v := range vals {
			if strings.TrimSpace(v) == "" {
				return SearchParams{}, fmt.Errorf("If-None-Exist query %q contains empty parameter value for %q", rawHeader, k)
			}
		}
		if len(vals) > 1 && k != "_lastUpdated" && !knownDateSearchParams[k] && !knownQuantitySearchParams[k] && !knownCompositeTokenQuantityParams[k] {
			if ch, ok := ParseChainedParam(k, vals[0]); !ok || (ch.ParamType != "date" && ch.ParamType != "quantity") {
				return SearchParams{}, fmt.Errorf("If-None-Exist query %q contains multiple values for non-range parameter %q", rawHeader, k)
			}
		}
		if (k == "_id" || knownRefSearchParams[k]) && strings.Contains(vals[0], ",") {
			return SearchParams{}, fmt.Errorf("If-None-Exist query %q cannot contain comma-separated values for %q", rawHeader, k)
		}
	}
	if err := ValidateSearchQuery(q); err != nil {
		return SearchParams{}, fmt.Errorf("invalid If-None-Exist query %q: %w", rawHeader, err)
	}

	params := BuildSearchParamsFromQuery(tenant, resType, q)
	params.Count = 2
	params.Offset = 0
	return params, nil
}

// HasSearchCriteria returns true if SearchParams contains at least one filter criterion.
func HasSearchCriteria(p SearchParams) bool {
	return p.ID != "" ||
		(p.CompartmentType != "" && p.CompartmentID != "") ||
		len(p.ResTypes) > 0 ||
		len(p.LastUpdated) > 0 ||
		len(p.Strings) > 0 ||
		len(p.StringFilters) > 0 ||
		len(p.Tokens) > 0 ||
		len(p.TokenFilters) > 0 ||
		len(p.Dates) > 0 ||
		len(p.DateFilters) > 0 ||
		len(p.Quantities) > 0 ||
		len(p.QuantityFilters) > 0 ||
		len(p.References) > 0 ||
		len(p.ReferenceFilters) > 0 ||
		len(p.URIs) > 0 ||
		len(p.URIFilters) > 0 ||
		len(p.Missing) > 0 ||
		len(p.Chained) > 0 ||
		len(p.Has) > 0 ||
		len(p.List) > 0 ||
		len(p.Content) > 0 ||
		len(p.Text) > 0 ||
		len(p.Filters) > 0 ||
		len(p.CompositeTokenQuantities) > 0 ||
		len(p.CompositeTokenTokens) > 0 ||
		len(p.CompositeTokenDates) > 0 ||
		len(p.CompositeTokenStrings) > 0
}
