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
		case "performer", "general-practitioner", "requester", "asserter", "recorder", "results-interpreter":
			targetType = "Practitioner"
		case "organization", "partof", "manufacturer", "intended-dispenser":
			targetType = "Organization"
		}
	}

	paramType := "token"
	switch {
	case targetParam == "_id":
		paramType = "id"
	case knownStringSearchParams[targetParam]:
		paramType = "string"
	case knownDateSearchParams[targetParam]:
		paramType = "date"
	case knownQuantitySearchParams[targetParam]:
		paramType = "quantity"
	}

	return &ChainedParam{
		RefParam:    refParam,
		TargetType:  targetType,
		TargetParam: targetParam,
		ParamType:   paramType,
		Value:       value,
	}, true
}

var (
	knownStringSearchParams = map[string]bool{
		"family": true, "given": true, "name": true,
		"address": true, "address-city": true, "address-state": true,
		"address-postalcode": true, "address-country": true,
		"phonetic": true, "lot-number": true, "series": true,
	}
	knownTokenSearchParams = map[string]bool{
		"identifier": true, "gender": true, "active": true,
		"email": true, "phone": true, "language": true,
		"deceased": true, "status": true, "code": true,
		"category": true, "type": true, "class": true,
		"clinical-status": true, "verification-status": true,
		"intent": true, "priority": true, "criticality": true,
		"severity": true, "manifestation": true, "route": true,
		"reason-code": true, "conclusion": true, "vaccine-code": true,
		"status-reason": true, "target-disease": true,
		"intended-performertype": true, "address-use": true,
	}
	knownRefSearchParams = map[string]bool{
		"organization": true, "general-practitioner": true,
		"subject": true, "patient": true, "encounter": true,
		"performer": true, "asserter": true, "recorder": true,
		"participant": true, "requester": true, "medication": true,
		"intended-dispenser": true, "intended-performer": true,
		"based-on": true, "location": true, "part-of": true, "partof": true,
		"reason-reference": true, "result": true, "results-interpreter": true,
		"specimen": true, "media": true, "manufacturer": true,
		"reaction": true, "endpoint": true, "instantiates-canonical": true,
	}
	knownDateSearchParams = map[string]bool{
		"birthdate": true, "date": true, "onset-date": true,
		"authoredon": true, "issued": true, "last-date": true,
	}
	knownQuantitySearchParams = map[string]bool{
		"value-quantity":           true,
		"component-value-quantity": true,
		"combo-value-quantity":     true,
	}
)

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
	for _, incVal := range q["_include"] {
		if _, err := ParseInclude(incVal); err != nil {
			return err
		}
	}
	for _, revVal := range q["_revinclude"] {
		if _, err := ParseInclude(revVal); err != nil {
			return err
		}
	}
	for key, values := range q {
		if strings.HasPrefix(key, "_") || len(values) == 0 {
			continue
		}
		for _, val := range values {
			if chained, ok := ParseChainedParam(key, val); ok {
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
			if knownDateSearchParams[key] {
				if _, err := ParseDateOp(val); err != nil {
					return fmt.Errorf("invalid date parameter %s=%q: %w", key, val, err)
				}
			} else if knownQuantitySearchParams[key] {
				if _, err := ParseQuantityOp(val); err != nil {
					return fmt.Errorf("invalid quantity parameter %s=%q: %w", key, val, err)
				}
			}
		}
	}
	return nil
}

// BuildSearchParamsFromQuery converts URL query values into a typed SearchParams struct.
func BuildSearchParamsFromQuery(tenant, resType string, q url.Values) SearchParams {
	params := SearchParams{
		TenantID:        tenant,
		ResType:         resType,
		Strings:         make(map[string]string),
		Tokens:          make(map[string]string),
		Dates:           make(map[string]DateOp),
		DateFilters:     make(map[string][]DateOp),
		Quantities:      make(map[string]QuantityOp),
		QuantityFilters: make(map[string][]QuantityOp),
		References:      make(map[string]string),
	}

	if idVal := strings.TrimSpace(q.Get("_id")); idVal != "" {
		if idx := strings.LastIndex(idVal, "/"); idx != -1 {
			idVal = idVal[idx+1:]
		}
		params.ID = idVal
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

		if _, ok := ParseChainedParam(key, value); ok {
			for _, rawVal := range values {
				if ch, ok := ParseChainedParam(key, rawVal); ok {
					params.Chained = append(params.Chained, *ch)
				}
			}
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
			for _, rawVal := range values {
				if dOp, err := ParseDateOp(rawVal); err == nil {
					if _, exists := params.Dates[key]; !exists {
						params.Dates[key] = dOp
					}
					params.DateFilters[key] = append(params.DateFilters[key], dOp)
				}
			}
		case knownQuantitySearchParams[key]:
			for _, rawVal := range values {
				if qOp, err := ParseQuantityOp(rawVal); err == nil {
					if _, exists := params.Quantities[key]; !exists {
						params.Quantities[key] = qOp
					}
					params.QuantityFilters[key] = append(params.QuantityFilters[key], qOp)
				}
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
		if len(vals) > 1 && !knownDateSearchParams[k] && !knownQuantitySearchParams[k] {
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
		len(p.Strings) > 0 ||
		len(p.Tokens) > 0 ||
		len(p.Dates) > 0 ||
		len(p.DateFilters) > 0 ||
		len(p.Quantities) > 0 ||
		len(p.QuantityFilters) > 0 ||
		len(p.References) > 0 ||
		len(p.Chained) > 0
}
