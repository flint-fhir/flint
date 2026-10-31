package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestDorisStandaloneBackend_EndToEndFHIR(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ds := doris.New(nil)
	srv := server.New(ds, logger)
	server.RegisterDefaultResourceTypes(srv)

	// Verify Doris is registered as both operational Store and AnalyticalStore
	assert.Equal(t, ds, srv.Store())
	require.NotNil(t, srv.AnalyticalStore())
	assert.Equal(t, "doris", srv.AnalyticalStore().BackendName())

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	// 1. POST /Patient on Doris backend
	patJSON := `{
		"resourceType": "Patient",
		"id": "pat-doris-1",
		"identifier": [{"system": "http://hospital.org/mrn", "value": "MRN-DORIS-1"}],
		"name": [{"family": "Smith", "given": ["Alice"]}],
		"gender": "female",
		"birthDate": "1985-04-12"
	}`
	reqCreate, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/demo/Patient", strings.NewReader(patJSON))
	require.NoError(t, err)
	reqCreate.Header.Set("Content-Type", "application/fhir+json")
	respCreate, err := client.Do(reqCreate)
	require.NoError(t, err)
	defer respCreate.Body.Close()
	assert.Equal(t, http.StatusCreated, respCreate.StatusCode)
	assert.Equal(t, `W/"1"`, respCreate.Header.Get("ETag"))

	// 2. GET /Patient/pat-doris-1 (Zero-JSON proto round-trip from Doris)
	respRead, err := client.Get(ts.URL + "/fhir/r4/demo/Patient/pat-doris-1")
	require.NoError(t, err)
	defer respRead.Body.Close()
	assert.Equal(t, http.StatusOK, respRead.StatusCode)
	var readMap map[string]any
	require.NoError(t, json.NewDecoder(respRead.Body).Decode(&readMap))
	assert.Equal(t, "pat-doris-1", readMap["id"])

	// 3. Optimistic Locking (If-Match: W/"99" -> 412 Precondition Failed; If-Match: W/"1" -> 200 OK)
	reqUpdFail, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/demo/Patient/pat-doris-1", strings.NewReader(patJSON))
	require.NoError(t, err)
	reqUpdFail.Header.Set("Content-Type", "application/fhir+json")
	reqUpdFail.Header.Set("If-Match", `W/"99"`)
	respUpdFail, err := client.Do(reqUpdFail)
	require.NoError(t, err)
	defer respUpdFail.Body.Close()
	assert.Equal(t, http.StatusPreconditionFailed, respUpdFail.StatusCode)

	reqUpdOK, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/demo/Patient/pat-doris-1", strings.NewReader(patJSON))
	require.NoError(t, err)
	reqUpdOK.Header.Set("Content-Type", "application/fhir+json")
	reqUpdOK.Header.Set("If-Match", `W/"1"`)
	respUpdOK, err := client.Do(reqUpdOK)
	require.NoError(t, err)
	defer respUpdOK.Body.Close()
	assert.Equal(t, http.StatusOK, respUpdOK.StatusCode)
	assert.Equal(t, `W/"2"`, respUpdOK.Header.Get("ETag"))

	// 4. Conditional Create (If-None-Exist) returns existing Patient with 200 OK
	reqCond, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/demo/Patient", strings.NewReader(patJSON))
	require.NoError(t, err)
	reqCond.Header.Set("Content-Type", "application/fhir+json")
	reqCond.Header.Set("If-None-Exist", "identifier=http://hospital.org/mrn|MRN-DORIS-1")
	respCond, err := client.Do(reqCond)
	require.NoError(t, err)
	defer respCond.Body.Close()
	assert.Equal(t, http.StatusOK, respCond.StatusCode)

	// 5. Inverted-Index Search on Doris
	respSearch, err := client.Get(ts.URL + "/fhir/r4/demo/Patient?family=Smith&identifier=http://hospital.org/mrn|MRN-DORIS-1")
	require.NoError(t, err)
	defer respSearch.Body.Close()
	assert.Equal(t, http.StatusOK, respSearch.StatusCode)
	var searchBundle map[string]any
	require.NoError(t, json.NewDecoder(respSearch.Body).Decode(&searchBundle))
	assert.Equal(t, float64(1), searchBundle["total"])

	// 6. Transaction Bundle Compensating Rollback on Doris
	txBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"request": {"method": "POST", "url": "Patient"},
				"resource": {
					"resourceType": "Patient",
					"id": "pat-doris-rollback",
					"name": [{"family": "Rollback"}]
				}
			},
			{
				"request": {"method": "PUT", "url": "Patient/pat-doris-1", "ifMatch": "W/\"999\""},
				"resource": {
					"resourceType": "Patient",
					"id": "pat-doris-1",
					"name": [{"family": "Smith"}]
				}
			}
		]
	}`
	reqTx, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/demo", strings.NewReader(txBundle))
	require.NoError(t, err)
	reqTx.Header.Set("Content-Type", "application/fhir+json")
	respTx, err := client.Do(reqTx)
	require.NoError(t, err)
	defer respTx.Body.Close()
	assert.Equal(t, http.StatusPreconditionFailed, respTx.StatusCode)

	// Confirm pat-doris-rollback was rolled back on Doris
	respCheckRollback, err := client.Get(ts.URL + "/fhir/r4/demo/Patient/pat-doris-rollback")
	require.NoError(t, err)
	defer respCheckRollback.Body.Close()
	assert.Equal(t, http.StatusNotFound, respCheckRollback.StatusCode)

	// 7. Bulk $export via Doris AnalyticalStore
	reqExp, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/demo/$export?_type=Patient", nil)
	require.NoError(t, err)
	reqExp.Header.Set("Accept", "application/fhir+ndjson")
	respExp, err := client.Do(reqExp)
	require.NoError(t, err)
	defer respExp.Body.Close()
	assert.Equal(t, http.StatusOK, respExp.StatusCode)
	expBytes, err := io.ReadAll(respExp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(expBytes), `"id":"pat-doris-1"`)

	// 8. AuditEvent/$verify-integrity on Doris
	respAuditVerify, err := client.Get(ts.URL + "/fhir/r4/demo/AuditEvent/$verify-integrity")
	require.NoError(t, err)
	defer respAuditVerify.Body.Close()
	assert.Equal(t, http.StatusOK, respAuditVerify.StatusCode)
	var verifyParams map[string]any
	require.NoError(t, json.NewDecoder(respAuditVerify.Body).Decode(&verifyParams))
	assert.Equal(t, "Parameters", verifyParams["resourceType"])
}

func TestHybridPostgresAndDorisAnalyticalStore(t *testing.T) {
	db := openMemPostgres(t)
	defer db.Close()

	pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(pgStore, logger)
	server.RegisterDefaultResourceTypes(srv)

	dorisAnalytical := doris.New(nil)
	srv.SetAnalyticalStore(dorisAnalytical)
	require.NotNil(t, srv.AnalyticalStore())
	assert.Equal(t, "doris", srv.AnalyticalStore().BackendName())

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	patJSON := `{"resourceType":"Patient","id":"hybrid-pat-1","name":[{"family":"Hybrid"}]}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/demo/Patient", bytes.NewBufferString(patJSON))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/fhir+json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	// Replicate into Doris analytical store and execute system $export
	rec, err := srv.Store().ReadResourceWithMeta(req.Context(), "demo", "Patient", "hybrid-pat-1")
	require.NoError(t, err)
	_, err = dorisAnalytical.WriteResourceWithMeta(req.Context(), store.ResourceInput{
		TenantID:      rec.TenantID,
		ResType:       rec.ResType,
		ResID:         rec.ResID,
		ResourceProto: rec.ResourceProto,
	})
	require.NoError(t, err)

	reqExp, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/demo/$export?_type=Patient", nil)
	require.NoError(t, err)
	reqExp.Header.Set("Accept", "application/fhir+ndjson")
	respExp, err := ts.Client().Do(reqExp)
	require.NoError(t, err)
	defer respExp.Body.Close()
	assert.Equal(t, http.StatusOK, respExp.StatusCode)
	ndjsonBytes, err := io.ReadAll(respExp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(ndjsonBytes), `"id":"hybrid-pat-1"`)
	assert.NotEmpty(t, dorisAnalytical.ExecutedStatements())

	// Clear AnalyticalStore and verify SetAnalyticalStore(nil) resets cleanly
	srv.SetAnalyticalStore(nil)
	assert.Nil(t, srv.AnalyticalStore())
}
