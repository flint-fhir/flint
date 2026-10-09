package postgres

import (
	"fmt"
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
