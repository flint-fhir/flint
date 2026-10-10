package postgres

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DateOp represents a parsed date search operation.
type DateOp struct {
	Prefix string    // eq, ne, lt, le, gt, ge, sa, eb, ap
	Value  time.Time // Normalized UTC timestamp
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
}

// ChainedParam represents a chained parameter like "patient.name=Smith".
type ChainedParam struct {
	RefParam    string // e.g. "patient" or "subject"
	TargetType  string // Inferred or known target type, e.g. "Patient"
	TargetParam string // e.g. "name", "family", "gender"
	ParamType   string // "string", "token", "date", "quantity"
	Value       string // Search value
}

var knownDatePrefixes = map[string]bool{
	"eq": true, "ne": true, "lt": true, "le": true,
	"gt": true, "ge": true, "sa": true, "eb": true, "ap": true,
}

var knownQuantityPrefixes = map[string]bool{
	"eq": true, "ne": true, "lt": true, "le": true,
	"gt": true, "ge": true,
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
	var parseErr error

	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, datePart); err == nil {
			parsedTime = t.UTC()
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
// Format: Source:param or Source:param:Target
func ParseInclude(raw string) (IncludeParam, error) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) < 2 {
		return IncludeParam{}, fmt.Errorf("invalid include format %q (expected Source:param)", raw)
	}

	param := IncludeParam{
		SourceType: parts[0],
		ParamName:  parts[1],
	}
	if len(parts) >= 3 {
		param.TargetType = parts[2]
	}

	return param, nil
}

// ParseChainedParam checks if a query parameter key represents a chained search.
// Format: "refParam.targetParam" or "refParam:TargetType.targetParam"
func ParseChainedParam(key, value string) (*ChainedParam, bool) {
	dotIdx := strings.IndexByte(key, '.')
	if dotIdx == -1 {
		return nil, false
	}

	left := key[:dotIdx]
	targetParam := key[dotIdx+1:]

	refParam := left
	targetType := ""

	if colonIdx := strings.IndexByte(left, ':'); colonIdx != -1 {
		refParam = left[:colonIdx]
		targetType = left[colonIdx+1:]
	}

	// Default inferred target types for standard FHIR references if not explicit
	if targetType == "" {
		switch refParam {
		case "patient", "subject":
			targetType = "Patient"
		case "encounter":
			targetType = "Encounter"
		case "performer", "general-practitioner":
			targetType = "Practitioner"
		case "organization":
			targetType = "Organization"
		}
	}

	return &ChainedParam{
		RefParam:    refParam,
		TargetType:  targetType,
		TargetParam: targetParam,
		Value:       value,
	}, true
}

var (
	knownStringSearchParams = map[string]bool{
		"family": true, "given": true, "name": true,
		"address": true, "address-city": true, "address-state": true,
		"address-postalcode": true, "address-country": true,
	}
	knownTokenSearchParams = map[string]bool{
		"identifier": true, "gender": true, "active": true,
		"email": true, "phone": true, "language": true,
		"deceased": true, "status": true, "code": true,
		"category": true, "type": true, "class": true,
		"clinical-status": true, "verification-status": true,
	}
	knownRefSearchParams = map[string]bool{
		"organization": true, "general-practitioner": true,
		"subject": true, "patient": true, "encounter": true,
		"performer": true, "asserter": true, "recorder": true,
		"participant": true,
	}
	knownDateSearchParams = map[string]bool{
		"birthdate": true, "date": true, "onset-date": true,
	}
	knownQuantitySearchParams = map[string]bool{
		"value-quantity": true,
	}
)

// BuildSearchParamsFromQuery converts URL query values into a typed SearchParams struct.
func BuildSearchParamsFromQuery(tenant, resType string, q url.Values) SearchParams {
	params := SearchParams{
		TenantID:   tenant,
		ResType:    resType,
		Strings:    make(map[string]string),
		Tokens:     make(map[string]string),
		Dates:      make(map[string]DateOp),
		Quantities: make(map[string]QuantityOp),
		References: make(map[string]string),
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
	for _, revVal := range q["_revinclude"] {
		if rev, err := ParseInclude(revVal); err == nil {
			params.RevIncludes = append(params.RevIncludes, rev)
		}
	}

	for key, values := range q {
		if strings.HasPrefix(key, "_") || len(values) == 0 {
			continue
		}
		value := values[0]

		if chained, ok := ParseChainedParam(key, value); ok {
			params.Chained = append(params.Chained, *chained)
			continue
		}

		switch {
		case knownStringSearchParams[key]:
			params.Strings[key] = value
		case knownTokenSearchParams[key]:
			params.Tokens[key] = value
		case knownRefSearchParams[key]:
			params.References[key] = value
		case knownDateSearchParams[key]:
			if dOp, err := ParseDateOp(value); err == nil {
				params.Dates[key] = dOp
			}
		case knownQuantitySearchParams[key]:
			if qOp, err := ParseQuantityOp(value); err == nil {
				params.Quantities[key] = qOp
			}
		default:
			params.Tokens[key] = value
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
		if strings.TrimSpace(k) == "" || len(vals) == 0 || strings.TrimSpace(vals[0]) == "" {
			return SearchParams{}, fmt.Errorf("If-None-Exist query %q contains empty parameter key or value", rawHeader)
		}
	}

	params := BuildSearchParamsFromQuery(tenant, resType, q)
	params.Count = 2
	params.Offset = 0
	return params, nil
}
