// Package fhirutil provides nil-safe accessors for google/fhir R4 proto types.
package fhirutil

import (
	"reflect"
	"strings"
	"time"
)

// StringValue safely extracts the string value from a google/fhir proto String message.
// Returns empty string if the message or its value is nil.
func StringValue(s interface{ GetValue() string }) string {
	if s == nil || reflect.ValueOf(s).IsNil() {
		return ""
	}
	return s.GetValue()
}

// StringValues extracts string values from a slice of google/fhir proto String messages.
func StringValues[T interface{ GetValue() string }](ss []T) []string {
	if len(ss) == 0 {
		return nil
	}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if v := s.GetValue(); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// NormalizeString lowercases and trims whitespace for search index storage.
func NormalizeString(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// FHIRDateToTime converts a FHIR date string (YYYY, YYYY-MM, or YYYY-MM-DD) to time.Time.
// Returns zero time if parsing fails.
func FHIRDateToTime(dateStr string) time.Time {
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, dateStr); err == nil {
			return t
		}
	}
	return time.Time{}
}
