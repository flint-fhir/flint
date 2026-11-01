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
			assert.GreaterOrEqual(t, rep.TotalChecks, 84)
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

	// 5. Verify /healthz, /readyz, and /metrics report storageBackend, analyticalBackend, and hybridReplication
	for _, ep := range []string{"/healthz", "/readyz"} {
		respH, hErr := client.Get(ts.URL + ep)
		require.NoError(t, hErr)
		var hMap map[string]any
		require.NoError(t, json.NewDecoder(respH.Body).Decode(&hMap))
		respH.Body.Close()
		assert.Equal(t, http.StatusOK, respH.StatusCode)
		assert.Equal(t, "hybrid(doris)", hMap["storageBackend"])
		assert.Equal(t, "doris", hMap["analyticalBackend"])
		hybRep, ok := hMap["hybridReplication"].(map[string]any)
		require.True(t, ok, "expected hybridReplication in %s response", ep)
		assert.GreaterOrEqual(t, hybRep["replicatedWrites"].(float64), float64(3))
		assert.Equal(t, float64(0), hybRep["replicationErrors"].(float64))
	}

	respMet, err := client.Get(ts.URL + "/metrics")
	require.NoError(t, err)
	metBody, err := io.ReadAll(respMet.Body)
	respMet.Body.Close()
	require.NoError(t, err)
	assert.Contains(t, string(metBody), `flint_hybrid_replicated_writes_total 3`)
	assert.Contains(t, string(metBody), `flint_hybrid_replicated_deletes_total 0`)
	assert.Contains(t, string(metBody), `flint_hybrid_replication_errors_total 0`)

	// 6. Verify Measure/$evaluate-measure and Measure/$care-gaps invoke analytical pushdown and set X-Flint-Analytical-Backend
	respEval, err := client.Get(ts.URL + "/fhir/r4/demo/Measure/cms122/$evaluate-measure?periodStart=2020-01-01&periodEnd=2030-12-31")
	require.NoError(t, err)
	respEval.Body.Close()
	assert.Equal(t, http.StatusOK, respEval.StatusCode)
	assert.Equal(t, "doris", respEval.Header.Get("X-Flint-Analytical-Backend"))

	respGaps, err := client.Get(ts.URL + "/fhir/r4/demo/Measure/$care-gaps?periodStart=2020-01-01&periodEnd=2030-12-31&subject=Patient/auto-hyb-pat-1&status=open-gap")
	require.NoError(t, err)
	respGaps.Body.Close()
	assert.Equal(t, http.StatusOK, respGaps.StatusCode)
	assert.Equal(t, "doris", respGaps.Header.Get("X-Flint-Analytical-Backend"))
}

func TestWave66_DorisAndSQLite_AdvancedSearchHTTPParity(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range []struct {
		name string
		st   store.Store
	}{
		{"doris", doris.New(nil)},
		{"sqlite", sqlite.New(nil, sqlite.WithDSN(":memory:"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := server.New(tc.st, logger)
			server.RegisterDefaultResourceTypes(srv)

			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			client := ts.Client()

			putJSON := func(path, body string) {
				req, err := http.NewRequest(http.MethodPut, ts.URL+path, bytes.NewBufferString(body))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/fhir+json")
				resp, err := client.Do(req)
				require.NoError(t, err)
				resp.Body.Close()
				require.Contains(t, []int{http.StatusOK, http.StatusCreated}, resp.StatusCode)
			}

			putJSON("/fhir/r4/t1/Organization/org-1", `{"resourceType":"Organization","id":"org-1","name":"General Health System"}`)
			putJSON("/fhir/r4/t1/Patient/pat-1", `{
				"resourceType":"Patient",
				"id":"pat-1",
				"text":{"status":"generated","div":"<div xmlns=\"http://www.w3.org/1999/xhtml\">Narrative for Alice Smith</div>"},
				"name":[{"family":"Smith","given":["Alice"]}],
				"gender":"female",
				"birthDate":"1985-06-15",
				"managingOrganization":{"reference":"Organization/org-1"}
			}`)
			putJSON("/fhir/r4/t1/Patient/pat-2", `{
				"resourceType":"Patient",
				"id":"pat-2",
				"name":[{"family":"Zimmerman","given":["Bob"]}],
				"gender":"male",
				"birthDate":"1970-01-01",
				"deceasedBoolean":true
			}`)
			putJSON("/fhir/r4/t1/Observation/obs-1", `{
				"resourceType":"Observation",
				"id":"obs-1",
				"status":"final",
				"code":{"coding":[{"system":"http://loinc.org","code":"8480-6"}]},
				"subject":{"reference":"Patient/pat-1"},
				"valueQuantity":{"value":70,"unit":"kg","system":"http://unitsofmeasure.org","code":"kg"}
			}`)
			putJSON("/fhir/r4/t1/List/list-1", `{
				"resourceType":"List",
				"id":"list-1",
				"status":"current",
				"mode":"working",
				"entry":[{"item":{"reference":"Patient/pat-1"}}]
			}`)

			searchTotal := func(query string) (int, map[string]any) {
				resp, err := client.Get(ts.URL + "/fhir/r4/t1/" + query)
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, http.StatusOK, resp.StatusCode, "query %s failed", query)
				var b map[string]any
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&b))
				tot, _ := b["total"].(float64)
				return int(tot), b
			}

			// 1. Date range (ge + le)
			tot, _ := searchTotal("Patient?birthdate=ge1980-01-01&birthdate=le1990-12-31")
			assert.Equal(t, 1, tot)

			// 2. Quantity + UCUM unit conversion (70 kg == 70000 g)
			tot, _ = searchTotal("Observation?value-quantity=70000%7Chttp://unitsofmeasure.org%7Cg")
			assert.Equal(t, 1, tot)

			// 3. :missing modifier
			tot, _ = searchTotal("Patient?deceased:missing=true")
			assert.Equal(t, 1, tot)

			// 4. Single-hop and multi-hop chained search
			tot, _ = searchTotal("Observation?subject:Patient.family=Smith")
			assert.Equal(t, 1, tot)
			tot, _ = searchTotal("Observation?subject:Patient.organization:Organization.name=General")
			assert.Equal(t, 1, tot)

			// 5. Reverse chaining (_has)
			tot, _ = searchTotal("Patient?_has:Observation:patient:code=8480-6")
			assert.Equal(t, 1, tot)

			// 6. _filter boolean AST
			tot, _ = searchTotal("Patient?_filter=family%20eq%20%22Smith%22%20and%20not%20(gender%20eq%20%22male%22)")
			assert.Equal(t, 1, tot)

			// 7. Composite token$quantity with UCUM conversion
			tot, _ = searchTotal("Observation?code-value-quantity=http://loinc.org%7C8480-6$70000%7Chttp://unitsofmeasure.org%7Cg")
			assert.Equal(t, 1, tot)

			// 8. _content, _text, _list
			tot, _ = searchTotal("Patient?_content=Smith&_text=Narrative&_list=list-1")
			assert.Equal(t, 1, tot)

			// 9. _summary=count & _sort & _include:iterate
			tot, bCount := searchTotal("Patient?_summary=count")
			assert.Equal(t, 2, tot)
			assert.Nil(t, bCount["entry"])

			tot, bInc := searchTotal("Observation?_include=Observation:patient&_include:iterate=Patient:organization")
			assert.Equal(t, 1, tot)
			entries, _ := bInc["entry"].([]any)
			assert.Len(t, entries, 3, "expected Observation + Patient + iterative Organization")
		})
	}
}
