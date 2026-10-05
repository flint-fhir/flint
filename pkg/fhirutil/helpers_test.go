package fhirutil

import (
	"testing"
	"time"
)

func TestNormalizeString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Smith", "smith"},
		{"  JONES  ", "jones"},
		{"", ""},
		{"O'Brien", "o'brien"},
	}
	for _, tt := range tests {
		got := NormalizeString(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeString(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFHIRDateToTime(t *testing.T) {
	tests := []struct {
		input string
		want  string // YYYY-MM-DD
	}{
		{"1990-01-15", "1990-01-15"},
		{"1990-01", "1990-01-01"},
		{"1990", "1990-01-01"},
		{"invalid", "0001-01-01"}, // zero time
	}
	for _, tt := range tests {
		got := FHIRDateToTime(tt.input)
		want, _ := time.Parse("2006-01-02", tt.want)
		if !got.Equal(want) {
			t.Errorf("FHIRDateToTime(%q) = %v, want %v", tt.input, got, want)
		}
	}
}

type mockString struct{ val string }

func (m *mockString) GetValue() string { return m.val }

func TestStringValue(t *testing.T) {
	if got := StringValue(&mockString{"hello"}); got != "hello" {
		t.Errorf("StringValue = %q, want hello", got)
	}
	if got := StringValue((*mockString)(nil)); got != "" {
		t.Errorf("StringValue(nil) = %q, want empty", got)
	}
}
