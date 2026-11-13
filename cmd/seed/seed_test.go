package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

func TestCohortTransactionBundle_ValidatesAllResources(t *testing.T) {
	bundleBytes, err := CohortTransactionBundleJSON()
	require.NoError(t, err)

	var bundle struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Entry        []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	require.NoError(t, json.Unmarshal(bundleBytes, &bundle))
	assert.Equal(t, "Bundle", bundle.ResourceType)
	assert.Equal(t, "transaction", bundle.Type)
	assert.Len(t, bundle.Entry, 62)

	engine := validation.NewEngine(validation.DefaultOptions())
	seenTypes := make(map[string]bool)
	for _, entry := range bundle.Entry {
		var meta struct {
			ResourceType string `json:"resourceType"`
		}
		require.NoError(t, json.Unmarshal(entry.Resource, &meta))
		seenTypes[meta.ResourceType] = true

		outcome := engine.ValidateJSON("", entry.Resource)
		assert.True(t, outcome.IsValid(), "expected seeded resource %s to pass FHIR R4 validation: %v", entry.FullURL, outcome.Issues)
	}
	assert.Len(t, seenTypes, 27, "expected all 26 US Core v6.1.0 & v7.0.0 resource types + MedicationStatement (HL7 IPA) in seed bundle")
	for _, rt := range fhirutil.USCoreResourceTypes() {
		assert.True(t, seenTypes[rt], "missing US Core resource type %s in seed cohort", rt)
	}
	assert.True(t, seenTypes["MedicationStatement"], "missing HL7 IPA MedicationStatement in seed cohort")
}

func TestSeedRun_UnitHTTP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fhir/r4/metadata", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement"}`))
	})
	mux.HandleFunc("POST /fhir/r4/demo", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/fhir+json", r.Header.Get("Content-Type"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var b map[string]any
		require.NoError(t, json.Unmarshal(body, &b))
		entries := b["entry"].([]any)
		assert.Len(t, entries, 62)

		respEntries := make([]map[string]any, len(entries))
		for i, rawEntry := range entries {
			entryMap := rawEntry.(map[string]any)
			respEntries[i] = map[string]any{
				"fullUrl": entryMap["fullUrl"],
				"response": map[string]any{
					"status":   "200 OK",
					"location": entryMap["fullUrl"],
					"etag":     `W/"1"`,
				},
			}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resourceType": "Bundle",
			"type":         "transaction-response",
			"entry":        respEntries,
		})
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	var buf bytes.Buffer
	err := Run(t.Context(), Config{
		BaseURL:     ts.URL + "/",
		Tenant:      "demo",
		BearerToken: "test-token",
		HTTPClient:  ts.Client(),
		Out:         &buf,
	})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), `Seeded 62 FHIR R4 resources (all 26 US Core v6.1.0 & v7.0.0 + HL7 IPA profiles) into tenant "demo"`)
	assert.Contains(t, buf.String(), "Practitioner/prac-cardiology-1")
	assert.Contains(t, buf.String(), "Patient/pat-alice-001/$everything")
}

func TestParseSeedConfigAndErrorBranches(t *testing.T) {
	cfg, err := parseSeedConfig(nil, func(string) string { return "" })
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8080", cfg.BaseURL)
	assert.Equal(t, "demo", cfg.Tenant)

	cfgEnv, err := parseSeedConfig([]string{"-url", "http://example.org", "-tenant", "t1", "-token", "tok1"}, func(k string) string {
		if k == "FLINT_URL" {
			return "http://env.example.org"
		}
		return "env-val"
	})
	require.NoError(t, err)
	assert.Equal(t, "http://example.org", cfgEnv.BaseURL)
	assert.Equal(t, "t1", cfgEnv.Tenant)
	assert.Equal(t, "tok1", cfgEnv.BearerToken)

	_, err = parseSeedConfig([]string{"-unknown-flag"}, func(string) string { return "" })
	require.Error(t, err)

	// Non-200 HTTP response
	ts500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal failure"))
	}))
	defer ts500.Close()
	err = Run(t.Context(), Config{BaseURL: ts500.URL})
	require.ErrorContains(t, err, "returned HTTP 500")

	// Invalid JSON in 200 response
	tsBadJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not-json"))
	}))
	defer tsBadJSON.Close()
	err = Run(t.Context(), Config{BaseURL: tsBadJSON.URL})
	require.ErrorContains(t, err, "decode transaction-response")

	// Invalid URL & unreachable endpoint
	err = Run(t.Context(), Config{BaseURL: "://bad-url"})
	require.Error(t, err)
}

func TestSeedRun_EndToEndIntegration(t *testing.T) {
	dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	if err := db.PingContext(t.Context()); err != nil {
		t.Skipf("skipping postgres integration test: %v", err)
	}
	require.NoError(t, postgres.RunMigrations(t.Context(), db))

	store := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(store, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := t.Context()
	defer func() {
		_ = postgres.PurgeTenant(ctx, db, "test-seed")
	}()

	var buf bytes.Buffer
	err = Run(ctx, Config{
		BaseURL:    ts.URL,
		Tenant:     "test-seed",
		HTTPClient: ts.Client(),
		Out:        &buf,
	})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), `Seeded 62 FHIR R4 resources (all 26 US Core v6.1.0 & v7.0.0 + HL7 IPA profiles) into tenant "test-seed"`)

	// Verify Patient/$everything for Alice returns full longitudinal chart across all linked US Core & IPA profiles
	resp, err := http.Get(ts.URL + "/fhir/r4/test-seed/Patient/pat-alice-001/$everything")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var evBundle map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&evBundle))
	assert.Equal(t, 47, int(evBundle["total"].(float64)))
}

func TestSeedRun_SQLiteRoundTripAndUSCoreConformance(t *testing.T) {
	sqStore := sqlite.New(nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(sqStore, logger)
	server.RegisterDefaultResourceTypes(srv)
	srv.SetReferentialIntegrity(server.ReferentialIntegrityStrict)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var buf bytes.Buffer
	require.NoError(t, Run(t.Context(), Config{
		BaseURL:    ts.URL,
		Tenant:     "demo",
		HTTPClient: ts.Client(),
		Out:        &buf,
	}))
	assert.Contains(t, buf.String(), `Seeded 62 FHIR R4 resources (all 26 US Core v6.1.0 & v7.0.0 + HL7 IPA profiles) into tenant "demo"`)

	engine := validation.NewEngine(validation.DefaultOptions())

	getRaw := func(path string) []byte {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", path)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return body
	}

	// 1. CapabilityStatement (/metadata) must pass strict FHIR R4 validation with 0 issues
	metaBytes := getRaw("/fhir/r4/demo/metadata")
	metaOutcome := engine.ValidateJSON("CapabilityStatement", metaBytes)
	assert.True(t, metaOutcome.IsValid() && len(metaOutcome.Issues) == 0, "expected /metadata CapabilityStatement to have 0 validation issues: %+v", metaOutcome.Issues)
	assert.NotContains(t, string(metaBytes), `"conditionalPatch"`)

	// 2. Practitioner.address MustSupport round-trip
	pracBytes := getRaw("/fhir/r4/demo/Practitioner/prac-cardiology-1")
	assert.Contains(t, string(pracBytes), `"postalCode":"02115"`)
	assert.Contains(t, string(pracBytes), `"country":"US"`)
	pracOutcome := engine.ValidateJSONWithProfiles("Practitioner", pracBytes, []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitioner"}, "")
	assert.True(t, pracOutcome.IsValid(), "Practitioner US Core validation failed: %+v", pracOutcome.Issues)

	// 3. DataAbsentReason CodeSystem round-trip on Blood Pressure component
	bpBytes := getRaw("/fhir/r4/demo/Observation/obs-alice-bp")
	assert.Contains(t, string(bpBytes), `"http://terminology.hl7.org/CodeSystem/data-absent-reason"`)
	assert.Contains(t, string(bpBytes), `"unknown"`)
	bpOutcome := engine.ValidateJSONWithProfiles("Observation", bpBytes, []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-blood-pressure"}, "")
	assert.True(t, bpOutcome.IsValid(), "Blood Pressure US Core validation failed: %+v", bpOutcome.Issues)

	// 4. DataAbsentReason Extension round-trip on primitive and complex elements
	condBytes := getRaw("/fhir/r4/demo/Condition/cond-robert-t2d")
	assert.Contains(t, string(condBytes), `"http://hl7.org/fhir/StructureDefinition/data-absent-reason"`)
	condOutcome := engine.ValidateJSONWithProfiles("Condition", condBytes, []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-condition-encounter-diagnosis"}, "")
	assert.True(t, condOutcome.IsValid(), "Condition US Core validation failed: %+v", condOutcome.Issues)

	encBytes := getRaw("/fhir/r4/demo/Encounter/enc-elena-001")
	assert.Contains(t, string(encBytes), `"http://hl7.org/fhir/StructureDefinition/data-absent-reason"`)
	assert.Contains(t, string(encBytes), `"reasonCode"`)
	encOutcome := engine.ValidateJSONWithProfiles("Encounter", encBytes, []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-encounter"}, "")
	assert.True(t, encOutcome.IsValid(), "Encounter US Core validation failed: %+v", encOutcome.Issues)

	// 5. Exact RFC3339 timestamp searches on SQLite (midnight + non-midnight)
	for _, query := range []string{
		"/fhir/r4/demo/Condition?patient=Patient/pat-alice-001&onset-date=2024-06-01T00:00:00Z",
		"/fhir/r4/demo/Observation?patient=Patient/pat-alice-001&code=http://loinc.org|85354-9&date=2026-01-15T09:16:00Z",
	} {
		searchBytes := getRaw(query)
		var b map[string]any
		require.NoError(t, json.Unmarshal(searchBytes, &b))
		assert.Equal(t, 1, int(b["total"].(float64)), "expected exact timestamp search %s to return 1 resource", query)
	}

	// 6. Patient/$everything for Alice returns all 47 longitudinal resources
	evBytes := getRaw("/fhir/r4/demo/Patient/pat-alice-001/$everything")
	var evBundle map[string]any
	require.NoError(t, json.Unmarshal(evBytes, &evBundle))
	assert.Equal(t, 47, int(evBundle["total"].(float64)))
}
