package postgres

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
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
		case "patient", "subject", "beneficiary":
			targetType = "Patient"
		case "encounter":
			targetType = "Encounter"
		case "performer", "general-practitioner", "requester", "asserter", "recorder", "results-interpreter", "collector", "practitioner", "author", "agent":
			targetType = "Practitioner"
		case "organization", "partof", "manufacturer", "intended-dispenser", "payor", "custodian":
			targetType = "Organization"
		case "location":
			targetType = "Location"
		case "medication":
			targetType = "Medication"
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
		"title": true, "description": true, "udi-carrier": true, "device-name": true,
	}
	knownTokenSearchParams = map[string]bool{
		"identifier": true, "gender": true, "active": true,
		"email": true, "phone": true, "language": true,
		"deceased": true, "status": true, "code": true,
		"category": true, "type": true, "class": true,
		"clinical-status": true, "verification-status": true,
		"lifecycle-status": true, "docstatus": true, "mode": true,
		"intent": true, "priority": true, "criticality": true,
		"severity": true, "manifestation": true, "route": true,
		"reason-code": true, "conclusion": true, "vaccine-code": true,
		"status-reason": true, "target-disease": true,
		"intended-performertype": true, "address-use": true,
		"accession": true, "bodysite": true, "specialty": true,
		"role": true, "relationship": true,
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
	}
	knownDateSearchParams = map[string]bool{
		"birthdate": true, "date": true, "onset-date": true,
		"authoredon": true, "authored": true, "issued": true, "last-date": true,
		"collected": true, "period": true, "recorded": true, "created": true,
		"indexed": true, "target-date": true, "start-date": true,
		"occurrence": true, "whenhandedover": true, "whenprepared": true,
	}
	knownQuantitySearchParams = map[string]bool{
		"value-quantity":           true,
		"component-value-quantity": true,
		"combo-value-quantity":     true,
	}
)

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
	for _, luVal := range q["_lastUpdated"] {
		for part := range strings.SplitSeq(luVal, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				if _, err := ParseDateOp(trimmed); err != nil {
					return fmt.Errorf("invalid _lastUpdated parameter %q: %w", luVal, err)
				}
			}
		}
	}
	for key, values := range q {
		if strings.HasPrefix(key, "_") || len(values) == 0 {
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
		Missing:          make(map[string]bool),
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

		baseKey, mod := splitParamModifier(key)
		if mod == "missing" {
			params.Missing[baseKey] = strings.EqualFold(strings.TrimSpace(value), "true")
			continue
		}

		switch {
		case knownStringSearchParams[baseKey]:
			params.Strings[key] = value
			params.StringFilters[key] = append(params.StringFilters[key], values...)
		case knownTokenSearchParams[baseKey]:
			params.Tokens[key] = value
			params.TokenFilters[key] = append(params.TokenFilters[key], values...)
		case knownRefSearchParams[baseKey]:
			params.References[key] = value
			params.ReferenceFilters[key] = append(params.ReferenceFilters[key], values...)
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
		if len(vals) > 1 && k != "_lastUpdated" && !knownDateSearchParams[k] && !knownQuantitySearchParams[k] {
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
		len(p.Missing) > 0 ||
		len(p.Chained) > 0
}
