package postgres_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/store/postgres"
)

func TestParseDateOp(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantPrefix string
		wantYear   int
		wantErr    bool
	}{
		{
			name:       "Default eq prefix with date",
			input:      "1980-05-12",
			wantPrefix: "eq",
			wantYear:   1980,
		},
		{
			name:       "Explicit ge prefix with date",
			input:      "ge2023-01-01",
			wantPrefix: "ge",
			wantYear:   2023,
		},
		{
			name:       "Prefix lt with RFC3339",
			input:      "lt2021-08-15T12:00:00Z",
			wantPrefix: "lt",
			wantYear:   2021,
		},
		{
			name:       "Prefix sa (starts after) with year only",
			input:      "sa1990",
			wantPrefix: "sa",
			wantYear:   1990,
		},
		{
			name:       "Prefix eb (ends before) with year-month",
			input:      "eb2020-04",
			wantPrefix: "eb",
			wantYear:   2020,
		},
		{
			name:    "Invalid format",
			input:   "not-a-date",
			wantErr: true,
		},
		{
			name:    "Empty input",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, err := postgres.ParseDateOp(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPrefix, op.Prefix)
			assert.Equal(t, tt.wantYear, op.Value.Year())
			assert.Equal(t, time.UTC, op.Value.Location())
		})
	}
}

func TestParseQuantityOp(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantPrefix string
		wantVal    float64
		wantSystem string
		wantCode   string
		wantErr    bool
	}{
		{
			name:       "Plain number",
			input:      "120",
			wantPrefix: "eq",
			wantVal:    120.0,
		},
		{
			name:       "Prefix gt with decimals",
			input:      "gt5.4",
			wantPrefix: "gt",
			wantVal:    5.4,
		},
		{
			name:       "Number with system and code",
			input:      "120|http://unitsofmeasure.org|mm[Hg]",
			wantPrefix: "eq",
			wantVal:    120.0,
			wantSystem: "http://unitsofmeasure.org",
			wantCode:   "mm[Hg]",
		},
		{
			name:       "Prefix le with code only",
			input:      "le98.6||degF",
			wantPrefix: "le",
			wantVal:    98.6,
			wantSystem: "",
			wantCode:   "degF",
		},
		{
			name:    "Invalid number",
			input:   "invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, err := postgres.ParseQuantityOp(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPrefix, op.Prefix)
			assert.Equal(t, tt.wantVal, op.Value)
			assert.Equal(t, tt.wantSystem, op.System)
			assert.Equal(t, tt.wantCode, op.Code)
		})
	}
}

func TestParseInclude(t *testing.T) {
	t.Run("Standard include Source:param", func(t *testing.T) {
		inc, err := postgres.ParseInclude("Observation:patient")
		require.NoError(t, err)
		assert.Equal(t, "Observation", inc.SourceType)
		assert.Equal(t, "patient", inc.ParamName)
		assert.Empty(t, inc.TargetType)
	})

	t.Run("Include with explicit target Source:param:Target", func(t *testing.T) {
		inc, err := postgres.ParseInclude("Encounter:subject:Patient")
		require.NoError(t, err)
		assert.Equal(t, "Encounter", inc.SourceType)
		assert.Equal(t, "subject", inc.ParamName)
		assert.Equal(t, "Patient", inc.TargetType)
	})

	t.Run("Invalid include format", func(t *testing.T) {
		_, err := postgres.ParseInclude("InvalidFormat")
		assert.Error(t, err)
	})
}

func TestParseChainedParam(t *testing.T) {
	t.Run("Implicit target type", func(t *testing.T) {
		p, ok := postgres.ParseChainedParam("patient.name", "Smith")
		require.True(t, ok)
		assert.Equal(t, "patient", p.RefParam)
		assert.Equal(t, "Patient", p.TargetType)
		assert.Equal(t, "name", p.TargetParam)
		assert.Equal(t, "Smith", p.Value)
	})

	t.Run("Explicit target type", func(t *testing.T) {
		p, ok := postgres.ParseChainedParam("subject:Patient.family", "Doe")
		require.True(t, ok)
		assert.Equal(t, "subject", p.RefParam)
		assert.Equal(t, "Patient", p.TargetType)
		assert.Equal(t, "family", p.TargetParam)
		assert.Equal(t, "Doe", p.Value)
	})

	t.Run("Non-chained param", func(t *testing.T) {
		_, ok := postgres.ParseChainedParam("gender", "male")
		assert.False(t, ok)
	})
}
