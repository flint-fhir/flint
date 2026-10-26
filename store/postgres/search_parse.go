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
// Example: _has:Observation:patient:code=8867-4 on a Patient search.
type HasParam struct {
	SourceType  string // e.g. "Observation"
	RefParam    string // e.g. "patient" or "subject"
	SearchParam string // e.g. "code", "date", "value-quantity"
	ParamType   string // "id", "string", "token", "date", "quantity", "reference"
	Value       string // Search value
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
	case knownStringSearchParams[targetParam]:
		paramType = "string"
	case knownDateSearchParams[targetParam]:
		paramType = "date"
	case knownQuantitySearchParams[targetParam]:
		paramType = "quantity"
	case knownRefSearchParams[targetParam]:
		paramType = "reference"
	case knownURISearchParams[targetParam] || targetMod == "below" || targetMod == "above":
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

// ParseHasParam parses a FHIR R4 reverse-chaining parameter (_has:SourceType:refParam:searchParam=value).
func ParseHasParam(key, value string) (*HasParam, bool, error) {
	if !strings.HasPrefix(key, "_has:") {
		return nil, false, nil
	}
	rest := strings.TrimSpace(key[len("_has:"):])
	parts := strings.Split(rest, ":")
	if len(parts) != 3 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[2]) == "" {
		return nil, true, fmt.Errorf("invalid _has parameter %q: expected _has:{SourceType}:{refParam}:{searchParam}", key)
	}
	if strings.TrimSpace(value) == "" {
		return nil, true, fmt.Errorf("invalid _has parameter %q: empty value", key)
	}
	sourceType := strings.TrimSpace(parts[0])
	refParam := strings.TrimSpace(parts[1])
	searchParam := strings.TrimSpace(parts[2])

	paramType := "token"
	switch {
	case searchParam == "_id":
		paramType = "id"
	case knownStringSearchParams[searchParam]:
		paramType = "string"
	case knownDateSearchParams[searchParam]:
		paramType = "date"
		if _, err := ParseDateOp(value); err != nil {
			return nil, true, fmt.Errorf("invalid _has date value %s=%q: %w", key, value, err)
		}
	case knownQuantitySearchParams[searchParam]:
		paramType = "quantity"
		if _, err := ParseQuantityOp(value); err != nil {
			return nil, true, fmt.Errorf("invalid _has quantity value %s=%q: %w", key, value, err)
		}
	case knownRefSearchParams[searchParam]:
		paramType = "reference"
	case knownURISearchParams[searchParam]:
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
		"component-code": true, "combo-code": true,
		"category": true, "type": true, "class": true,
		"clinical-status": true, "verification-status": true,
		"lifecycle-status": true, "docstatus": true, "mode": true,
		"intent": true, "priority": true, "criticality": true,
		"severity": true, "manifestation": true, "route": true,
		"reason-code": true, "conclusion": true, "vaccine-code": true,
		"status-reason": true, "target-disease": true,
		"intended-performertype": true, "address-use": true,
		"accession": true, "bodysite": true, "specialty": true,
		"role": true, "relationship": true, "subscriber-id": true,
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
		"item": true, "member": true, "entity": true,
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
	knownCompositeTokenQuantityParams = map[string]bool{
		"code-value-quantity":           true,
		"component-code-value-quantity": true,
		"combo-code-value-quantity":     true,
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
)

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
			if knownCompositeTokenQuantityParams[baseKey] {
				for part := range strings.SplitSeq(val, ",") {
					if _, err := ParseCompositeTokenQuantity(baseKey, part); err != nil {
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

	for _, listRaw := range q["_list"] {
		if trimmed := strings.TrimSpace(listRaw); trimmed != "" {
			params.List = append(params.List, trimmed)
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
		case knownURISearchParams[baseKey] || mod == "below" || mod == "above":
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
		len(p.CompositeTokenQuantities) > 0
}
