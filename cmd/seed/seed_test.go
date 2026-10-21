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

	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
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
	assert.Len(t, bundle.Entry, 29)

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
	assert.Len(t, seenTypes, 13, "expected all 13 US Core v6.1.0 & v7.0.0 resource types in seed bundle")
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
		assert.Len(t, entries, 29)

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
	assert.Contains(t, buf.String(), `Seeded 29 FHIR R4 resources (all 13 US Core v6.1.0 & v7.0.0 profiles) into tenant "demo"`)
	assert.Contains(t, buf.String(), "Practitioner/prac-cardiology-1")
	assert.Contains(t, buf.String(), "Patient/pat-alice-001/$everything")
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
	assert.Contains(t, buf.String(), `Seeded 29 FHIR R4 resources (all 13 US Core v6.1.0 & v7.0.0 profiles) into tenant "test-seed"`)

	// Verify Patient/$everything for Alice returns Patient + Practitioner + Organization + Location + Encounter + Condition + 4 Observations + MedicationRequest + AllergyIntolerance + Procedure + Specimen + DiagnosticReport (15 resources)
	resp, err := http.Get(ts.URL + "/fhir/r4/test-seed/Patient/pat-alice-001/$everything")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var evBundle map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&evBundle))
	assert.Equal(t, 15, int(evBundle["total"].(float64)))
}
