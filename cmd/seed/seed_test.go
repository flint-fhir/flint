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

	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"

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
	assert.Len(t, bundle.Entry, 15)

	engine := validation.NewEngine(validation.DefaultOptions())
	for _, entry := range bundle.Entry {
		outcome := engine.ValidateJSON("", entry.Resource)
		assert.True(t, outcome.IsValid(), "expected seeded resource %s to pass FHIR R4 validation: %v", entry.FullURL, outcome.Issues)
	}
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
		assert.Len(t, entries, 15)

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
	assert.Contains(t, buf.String(), `Seeded 15 FHIR R4 resources into tenant "demo"`)
	assert.Contains(t, buf.String(), "Practitioner/prac-cardiology-1")
	assert.Contains(t, buf.String(), "Patient/pat-alice-001/$everything")
}

func TestSeedRun_EndToEndIntegration(t *testing.T) {
	dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("FLINT_TEST_POSTGRES_DSN not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, postgres.RunMigrations(t.Context(), db))

	store := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(store, logger)
	srv.RegisterResourceType("Patient", &patpb.Patient{})
	srv.RegisterResourceType("Condition", &condpb.Condition{})
	srv.RegisterResourceType("Encounter", &encpb.Encounter{})
	srv.RegisterResourceType("Observation", &obspb.Observation{})
	srv.RegisterResourceType("Practitioner", &pracpb.Practitioner{})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := t.Context()
	defer func() {
		for _, table := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri", "fhir_audit_event", "fhir_resource_history", "fhir_resource"} {
			db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = 'test-seed'")
		}
	}()

	var buf bytes.Buffer
	err = Run(ctx, Config{
		BaseURL:    ts.URL,
		Tenant:     "test-seed",
		HTTPClient: ts.Client(),
		Out:        &buf,
	})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), `Seeded 15 FHIR R4 resources into tenant "test-seed"`)

	// Verify Patient/$everything for Alice returns Patient + Practitioner + Encounter + Condition + 2 Observations (6 resources)
	resp, err := http.Get(ts.URL + "/fhir/r4/test-seed/Patient/pat-alice-001/$everything")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var evBundle map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&evBundle))
	assert.Equal(t, 6, int(evBundle["total"].(float64)))
}
