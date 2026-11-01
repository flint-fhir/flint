package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/certify"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

func TestAllBackends_82CheckCertifyParity(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backends := []struct {
		name  string
		setup func(t *testing.T) (*server.Server, func())
	}{
		{
			name: "postgres-standalone",
			setup: func(t *testing.T) (*server.Server, func()) {
				db := openMemPostgres(t)
				pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
				srv := server.New(pgStore, logger)
				server.RegisterDefaultResourceTypes(srv)
				return srv, func() { _ = db.Close() }
			},
		},
		{
			name: "doris-option-c-standalone",
			setup: func(t *testing.T) (*server.Server, func()) {
				ds := doris.New(nil)
				srv := server.New(ds, logger)
				server.RegisterDefaultResourceTypes(srv)
				return srv, func() {}
			},
		},
		{
			name: "sqlite-wal-standalone",
			setup: func(t *testing.T) (*server.Server, func()) {
				ss := sqlite.New(nil, sqlite.WithDSN(":memory:"))
				srv := server.New(ss, logger)
				server.RegisterDefaultResourceTypes(srv)
				return srv, func() {}
			},
		},
		{
			name: "hybrid-postgres-oltp-doris-olap",
			setup: func(t *testing.T) (*server.Server, func()) {
				db := openMemPostgres(t)
				pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
				ds := doris.New(nil)
				srv := server.New(pgStore, logger)
				srv.SetAnalyticalStore(ds)
				server.RegisterDefaultResourceTypes(srv)
				return srv, func() { _ = db.Close() }
			},
		},
		{
			name: "hybrid-postgres-oltp-sqlite-olap",
			setup: func(t *testing.T) (*server.Server, func()) {
				db := openMemPostgres(t)
				pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
				ss := sqlite.New(nil, sqlite.WithDSN(":memory:"))
				srv := server.New(pgStore, logger)
				srv.SetAnalyticalStore(ss)
				server.RegisterDefaultResourceTypes(srv)
				return srv, func() { _ = db.Close() }
			},
		},
	}

	for _, tc := range backends {
		t.Run(tc.name, func(t *testing.T) {
			srv, cleanup := tc.setup(t)
			defer cleanup()

			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			rep, err := certify.Run(ctx, certify.Config{
				BaseURL:    ts.URL,
				Tenant:     "parity-" + tc.name,
				HTTPClient: ts.Client(),
			})
			require.NoError(t, err)
			require.NotNil(t, rep)

			for _, ch := range rep.Checks {
				assert.Equalf(t, "pass", ch.Status, "check [%s] %s failed on backend %s: %s", ch.ID, ch.Name, tc.name, ch.Message)
			}
			assert.Equal(t, "pass", rep.Result, "expected 100%% certification pass on %s:\n%s", tc.name, rep.FormatScorecard())
			assert.Equal(t, rep.TotalChecks, rep.Passed)
			assert.Equal(t, 0, rep.Failed)
			assert.GreaterOrEqual(t, rep.TotalChecks, 82)
		})
	}
}

func TestHybridAutoSync_And_ObservationStatsPushdown(t *testing.T) {
	db := openMemPostgres(t)
	defer db.Close()

	pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
	dorisAnalytical := doris.New(nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := server.New(pgStore, logger)
	srv.SetAnalyticalStore(dorisAnalytical)
	server.RegisterDefaultResourceTypes(srv)

	// Verify srv.Store() is wrapped in *store.HybridStore
	hyb, ok := srv.Store().(*store.HybridStore)
	require.True(t, ok, "expected Server.Store() to be *store.HybridStore when distinct AnalyticalStore is configured")
	assert.Equal(t, pgStore, hyb.Primary())
	assert.Equal(t, dorisAnalytical, hyb.Replica())
	assert.Equal(t, "doris", srv.AnalyticalStore().BackendName())

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	// 1. POST /Patient on Postgres primary — verify AUTOMATIC replication to Doris (no manual copy!)
	patJSON := `{"resourceType":"Patient","id":"auto-hyb-pat-1","name":[{"family":"AutoHybrid","given":["Elena"]}],"gender":"female","birthDate":"1990-01-15"}`
	reqPat, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/demo/Patient/auto-hyb-pat-1", bytes.NewBufferString(patJSON))
	require.NoError(t, err)
	reqPat.Header.Set("Content-Type", "application/fhir+json")
	respPat, err := client.Do(reqPat)
	require.NoError(t, err)
	respPat.Body.Close()
	assert.Equal(t, http.StatusCreated, respPat.StatusCode)

	// Confirm Doris replica received the Patient + typed fhir_patient projection automatically
	dorisRec, err := dorisAnalytical.ReadResourceWithMeta(context.Background(), "demo", "Patient", "auto-hyb-pat-1")
	require.NoError(t, err)
	assert.Equal(t, 1, dorisRec.ResVersion)

	// 2. Write two Heart Rate Observations via HTTP and invoke Observation/$stats
	for _, ob := range []struct {
		id  string
		val string
	}{
		{"obs-hr-1", "70"},
		{"obs-hr-2", "80"},
	} {
		obsJSON := `{
			"resourceType": "Observation",
			"id": "` + ob.id + `",
			"status": "final",
			"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
			"subject": {"reference": "Patient/auto-hyb-pat-1"},
			"valueQuantity": {"value": ` + ob.val + `, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
		}`
		reqObs, oErr := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/demo/Observation/"+ob.id, bytes.NewBufferString(obsJSON))
		require.NoError(t, oErr)
		reqObs.Header.Set("Content-Type", "application/fhir+json")
		respObs, oErr := client.Do(reqObs)
		require.NoError(t, oErr)
		respObs.Body.Close()
		assert.Equal(t, http.StatusCreated, respObs.StatusCode)
	}

	// 3. Invoke GET /Observation/$stats and verify X-Flint-Analytical-Backend header & Doris SQL pushdown
	respStats, err := client.Get(ts.URL + "/fhir/r4/demo/Observation/$stats?subject=Patient/auto-hyb-pat-1&code=8867-4&statistic=average,min,max,count")
	require.NoError(t, err)
	defer respStats.Body.Close()
	assert.Equal(t, http.StatusOK, respStats.StatusCode)
	assert.Equal(t, "doris", respStats.Header.Get("X-Flint-Analytical-Backend"))

	var statsParams map[string]any
	require.NoError(t, json.NewDecoder(respStats.Body).Decode(&statsParams))
	assert.Equal(t, "Parameters", statsParams["resourceType"])

	// Verify Doris executed both fhir_observation projection upserts and columnar $stats query
	stmts := dorisAnalytical.ExecutedStatements()
	hasObsProjection := false
	hasObsStatsSQL := false
	for _, s := range stmts {
		if bytes.Contains([]byte(s), []byte("INSERT INTO fhir_observation")) {
			hasObsProjection = true
		}
		if bytes.Contains([]byte(s), []byte("FROM fhir_observation")) {
			hasObsStatsSQL = true
		}
	}
	assert.True(t, hasObsProjection, "expected hybrid sync to project Observation into Doris fhir_observation table")
	assert.True(t, hasObsStatsSQL, "expected Observation/$stats to execute Doris columnar query on fhir_observation")

	// 4. Bulk $export (both system-level and Patient-compartment) streams directly from Doris replica without manual sync
	reqExp, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/demo/Patient/auto-hyb-pat-1/$export", nil)
	require.NoError(t, err)
	reqExp.Header.Set("Accept", "application/fhir+ndjson")
	respExp, err := client.Do(reqExp)
	require.NoError(t, err)
	defer respExp.Body.Close()
	assert.Equal(t, http.StatusOK, respExp.StatusCode)
	expBody, err := io.ReadAll(respExp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(expBody), `"id":"auto-hyb-pat-1"`)
	assert.Contains(t, string(expBody), `"id":"obs-hr-1"`)
	assert.Contains(t, string(expBody), `"id":"obs-hr-2"`)
}
