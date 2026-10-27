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
		require.Len(t, p.Hops, 1)
		assert.Equal(t, "family", p.TargetParam)
		assert.Equal(t, "Doe", p.Value)
	})

	t.Run("Multi-hop chained param with implicit and explicit target types", func(t *testing.T) {
		p, ok := postgres.ParseChainedParam("patient.organization.name:exact", "Acme Health")
		require.True(t, ok)
		assert.Equal(t, "patient", p.RefParam)
		assert.Equal(t, "Organization", p.TargetType)
		require.Len(t, p.Hops, 2)
		assert.Equal(t, postgres.ChainedHop{RefParam: "patient", TargetType: "Patient"}, p.Hops[0])
		assert.Equal(t, postgres.ChainedHop{RefParam: "organization", TargetType: "Organization"}, p.Hops[1])
		assert.Equal(t, "name", p.TargetParam)
		assert.Equal(t, "exact", p.TargetModifier)
		assert.Equal(t, "string", p.ParamType)
		assert.Equal(t, "Acme Health", p.Value)

		p2, ok := postgres.ParseChainedParam("subject:Patient.general-practitioner:Practitioner.identifier", "http://hl7.org/fhir/sid/us-npi|12345")
		require.True(t, ok)
		require.Len(t, p2.Hops, 2)
		assert.Equal(t, postgres.ChainedHop{RefParam: "subject", TargetType: "Patient"}, p2.Hops[0])
		assert.Equal(t, postgres.ChainedHop{RefParam: "general-practitioner", TargetType: "Practitioner"}, p2.Hops[1])
		assert.Equal(t, "identifier", p2.TargetParam)
		assert.Equal(t, "token", p2.ParamType)
	})

	t.Run("Chained URI param with :below modifier", func(t *testing.T) {
		p, ok := postgres.ParseChainedParam("questionnaire.url:below", "http://example.org/fhir/Questionnaire")
		require.True(t, ok)
		assert.Equal(t, "questionnaire", p.RefParam)
		assert.Equal(t, "Questionnaire", p.TargetType)
		assert.Equal(t, "url", p.TargetParam)
		assert.Equal(t, "below", p.TargetModifier)
		assert.Equal(t, "uri", p.ParamType)
	})

	t.Run("Non-chained param", func(t *testing.T) {
		_, ok := postgres.ParseChainedParam("gender", "male")
		assert.False(t, ok)
	})
}

func TestParseIfNoneExist(t *testing.T) {
	t.Run("Valid If-None-Exist query", func(t *testing.T) {
		sp, err := postgres.ParseIfNoneExist("t1", "Patient", "Patient?identifier=http://mrn|123")
		require.NoError(t, err)
		assert.Equal(t, "http://mrn|123", sp.Tokens["identifier"])
		assert.Equal(t, 2, sp.Count)
	})

	t.Run("Rejects multi-value reference or _id smuggling, comma-separated references, and _include", func(t *testing.T) {
		invalidHeaders := []string{
			"",
			"Observation?code=8867-4", // mismatched prefix for Patient
			"patient=pat-1&patient=pat-2",
			"subject=Patient/pat-1,Patient/pat-2",
			"_id=pat-1&_id=pat-2",
			"_id=pat-1,pat-2",
			"identifier=123&_include=Patient:organization",
			"identifier=123&_revinclude=Observation:patient",
			"identifier=",
		}
		for _, h := range invalidHeaders {
			_, err := postgres.ParseIfNoneExist("t1", "Patient", h)
			assert.Error(t, err, "expected ParseIfNoneExist to reject %q", h)
		}
	})
}

func TestBuildSearchParamsFromQuery_LastUpdated(t *testing.T) {
	q := map[string][]string{
		"_lastUpdated": {"ge2026-01-01", "le2026-12-31T23:59:59Z"},
		"patient":      {"Patient/pat-1"},
	}
	require.NoError(t, postgres.ValidateSearchQuery(q))
	sp := postgres.BuildSearchParamsFromQuery("t1", "Observation", q)
	require.Len(t, sp.LastUpdated, 2)
	assert.Equal(t, "ge", sp.LastUpdated[0].Prefix)
	assert.Equal(t, "le", sp.LastUpdated[1].Prefix)

	badQ := map[string][]string{
		"_lastUpdated": {"genot-a-date"},
	}
	err := postgres.ValidateSearchQuery(badQ)
	assert.Error(t, err, "expected invalid _lastUpdated date to return an error")
}

func TestParseCompositeTokenQuantity(t *testing.T) {
	t.Run("Valid code-value-quantity with system|code and UCUM quantity", func(t *testing.T) {
		p, err := postgres.ParseCompositeTokenQuantity("code-value-quantity", "http://loinc.org|4548-4$ge8.0|http://unitsofmeasure.org|%")
		require.NoError(t, err)
		assert.Equal(t, "code-value-quantity", p.ParamName)
		assert.Equal(t, "http://loinc.org", p.TokenSystem)
		assert.Equal(t, "4548-4", p.TokenCode)
		assert.Equal(t, "ge", p.Quantity.Prefix)
		assert.Equal(t, 8.0, p.Quantity.Value)
		assert.Equal(t, "http://unitsofmeasure.org", p.Quantity.System)
		assert.Equal(t, "%", p.Quantity.Code)
	})

	t.Run("Valid component-code-value-quantity with code-only token", func(t *testing.T) {
		p, err := postgres.ParseCompositeTokenQuantity("component-code-value-quantity", "8480-6$ge140")
		require.NoError(t, err)
		assert.Equal(t, "component-code-value-quantity", p.ParamName)
		assert.Empty(t, p.TokenSystem)
		assert.Equal(t, "8480-6", p.TokenCode)
		assert.Equal(t, "ge", p.Quantity.Prefix)
		assert.Equal(t, 140.0, p.Quantity.Value)
	})

	t.Run("Rejects invalid composite format", func(t *testing.T) {
		_, err := postgres.ParseCompositeTokenQuantity("code-value-quantity", "no-dollar-separator")
		assert.Error(t, err)

		_, err = postgres.ParseCompositeTokenQuantity("code-value-quantity", "8480-6$not-a-number")
		assert.Error(t, err)
	})

	t.Run("BuildSearchParamsFromQuery parses AND/OR composite groups", func(t *testing.T) {
		q := map[string][]string{
			"component-code-value-quantity": {
				"8480-6$ge140,8480-6$ge160",
				"8462-4$ge90",
			},
		}
		require.NoError(t, postgres.ValidateSearchQuery(q))
		sp := postgres.BuildSearchParamsFromQuery("t1", "Observation", q)
		require.Len(t, sp.CompositeTokenQuantities, 2)
		require.Len(t, sp.CompositeTokenQuantities[0], 2)
		require.Len(t, sp.CompositeTokenQuantities[1], 1)
		assert.True(t, postgres.HasSearchCriteria(sp))
	})

	t.Run("BuildSearchParamsFromQuery parses URI params and :below/:above modifiers", func(t *testing.T) {
		q := map[string][]string{
			"url:below": {"http://hl7.org/fhir/ValueSet"},
			"system":    {"http://loinc.org"},
		}
		require.NoError(t, postgres.ValidateSearchQuery(q))
		sp := postgres.BuildSearchParamsFromQuery("t1", "ValueSet", q)
		require.Len(t, sp.URIFilters["url:below"], 1)
		assert.Equal(t, "http://hl7.org/fhir/ValueSet", sp.URIFilters["url:below"][0])
		assert.Equal(t, "http://loinc.org", sp.URIs["system"])
		assert.True(t, postgres.HasSearchCriteria(sp))
	})

	t.Run("BuildSearchParamsFromQuery routes token :in, :not-in, :below, :above and reference :identifier", func(t *testing.T) {
		q := map[string][]string{
			"code:in":            {"http://hl7.org/fhir/ValueSet/observation-vitalsignresult"},
			"status:not-in":      {"http://hl7.org/fhir/ValueSet/administrative-gender"},
			"code:below":         {"http://snomed.info/sct|73211009"},
			"code:above":         {"http://snomed.info/sct|44054006"},
			"patient:identifier": {"http://hospital.example.org/mrn|MRN-001"},
			"_profile:below":     {"http://hl7.org/fhir/us/core/StructureDefinition/"},
		}
		require.NoError(t, postgres.ValidateSearchQuery(q))
		sp := postgres.BuildSearchParamsFromQuery("t1", "Observation", q)
		assert.Equal(t, "http://hl7.org/fhir/ValueSet/observation-vitalsignresult", sp.Tokens["code:in"])
		assert.Equal(t, "http://hl7.org/fhir/ValueSet/administrative-gender", sp.Tokens["status:not-in"])
		assert.Equal(t, "http://snomed.info/sct|73211009", sp.Tokens["code:below"])
		assert.Equal(t, "http://snomed.info/sct|44054006", sp.Tokens["code:above"])
		assert.Equal(t, "http://hospital.example.org/mrn|MRN-001", sp.References["patient:identifier"])
		assert.Equal(t, "http://hl7.org/fhir/us/core/StructureDefinition/", sp.URIs["_profile:below"])
		assert.True(t, postgres.HasSearchCriteria(sp))
	})

	t.Run("BuildSearchParamsFromQuery parses _type, token :text modifier, chained :text, and _has :text", func(t *testing.T) {
		q := map[string][]string{
			"_type":                            {"Patient,Observation", "Condition"},
			"code:text":                        {"hypertension"},
			"category:text":                    {"vital"},
			"subject:Patient.identifier:text":  {"Medical Record"},
			"_has:Condition:patient:code:text": {"diabetes"},
		}
		require.NoError(t, postgres.ValidateSearchQuery(q))
		sp := postgres.BuildSearchParamsFromQuery("t1", "", q)
		assert.Equal(t, []string{"Patient", "Observation", "Condition"}, sp.ResTypes)
		assert.Equal(t, "hypertension", sp.Strings["code:text"])
		assert.Equal(t, "vital", sp.Strings["category:text"])
		require.Len(t, sp.Chained, 1)
		assert.Equal(t, "identifier", sp.Chained[0].TargetParam)
		assert.Equal(t, "text", sp.Chained[0].TargetModifier)
		assert.Equal(t, "string", sp.Chained[0].ParamType)
		require.Len(t, sp.Has, 1)
		assert.Equal(t, "Condition", sp.Has[0].SourceType)
		assert.Equal(t, "code:text", sp.Has[0].SearchParam)
		assert.Equal(t, "string", sp.Has[0].ParamType)
		assert.True(t, postgres.HasSearchCriteria(sp))

		links := postgres.DefaultGraphLinksForResource("Patient")
		assert.NotEmpty(t, links)
	})
}

func TestTenantIsolationConfiguration(t *testing.T) {
	mode, err := postgres.ParseTenantIsolationMode("")
	require.NoError(t, err)
	assert.Equal(t, postgres.TenantIsolationSchema, mode)

	mode, err = postgres.ParseTenantIsolationMode("shared")
	require.NoError(t, err)
	assert.Equal(t, postgres.TenantIsolationShared, mode)

	mode, err = postgres.ParseTenantIsolationMode("schema")
	require.NoError(t, err)
	assert.Equal(t, postgres.TenantIsolationSchema, mode)

	mode, err = postgres.ParseTenantIsolationMode("schema-per-tenant")
	require.NoError(t, err)
	assert.Equal(t, postgres.TenantIsolationSchema, mode)

	_, err = postgres.ParseTenantIsolationMode("invalid-mode")
	assert.Error(t, err)

	schemaName, err := postgres.TenantSchemaName("HCA-West-01")
	require.NoError(t, err)
	assert.Equal(t, "tenant_hca_west_01", schemaName)

	_, err = postgres.TenantSchemaName("   ")
	assert.Error(t, err)

	st := postgres.New(nil, postgres.WithTenantIsolation(postgres.TenantIsolationSchema))
	assert.Equal(t, postgres.TenantIsolationSchema, st.TenantIsolation())
	st.SetTenantIsolation(postgres.TenantIsolationShared)
	assert.Equal(t, postgres.TenantIsolationShared, st.TenantIsolation())
}
