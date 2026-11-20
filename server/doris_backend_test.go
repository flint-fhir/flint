package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
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

func TestSQLiteStandaloneBackend_EndToEndFHIR(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ss := sqlite.New(nil, sqlite.WithDSN(":memory:"))
	srv := server.New(ss, logger)
	server.RegisterDefaultResourceTypes(srv)

	assert.Equal(t, ss, srv.Store())
	require.NotNil(t, srv.AnalyticalStore())
	assert.Equal(t, "sqlite", srv.AnalyticalStore().BackendName())

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	patJSON := `{
		"resourceType": "Patient",
		"id": "pat-sqlite-1",
		"name": [{"family": "SQLiteFamily", "given": ["Grace"]}],
		"gender": "female",
		"birthDate": "1992-07-19"
	}`
	reqCreate, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/demo/Patient/pat-sqlite-1", strings.NewReader(patJSON))
	require.NoError(t, err)
	reqCreate.Header.Set("Content-Type", "application/fhir+json")
	respCreate, err := client.Do(reqCreate)
	require.NoError(t, err)
	respCreate.Body.Close()
	assert.Equal(t, http.StatusCreated, respCreate.StatusCode)

	respSearch, err := client.Get(ts.URL + "/fhir/r4/demo/Patient?family=SQLiteFamily")
	require.NoError(t, err)
	defer respSearch.Body.Close()
	assert.Equal(t, http.StatusOK, respSearch.StatusCode)
	var searchBundle map[string]any
	require.NoError(t, json.NewDecoder(respSearch.Body).Decode(&searchBundle))
	assert.Equal(t, float64(1), searchBundle["total"])

	reqExp, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/demo/$export?_type=Patient", nil)
	require.NoError(t, err)
	reqExp.Header.Set("Accept", "application/fhir+ndjson")
	respExp, err := client.Do(reqExp)
	require.NoError(t, err)
	defer respExp.Body.Close()
	assert.Equal(t, http.StatusOK, respExp.StatusCode)
	expBytes, err := io.ReadAll(respExp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(expBytes), `"id":"pat-sqlite-1"`)
}

type srvDorisSQLiteState struct {
	mu      sync.Mutex
	baseDrv driver.Driver
	dsns    map[string]string
}

var (
	regSrvDorisOnce  sync.Once
	srvDorisState    = &srvDorisSQLiteState{dsns: make(map[string]string)}
	srvReArrayPlace  = regexp.MustCompile(`\[(\?(?:,\s*\?)*)\]`)
	srvReArrContains = regexp.MustCompile(`array_contains\(([a-zA-Z_][a-zA-Z0-9_]*),\s*\?\)`)
	srvReVarchar     = regexp.MustCompile(`VARCHAR\(\d+\)`)
)

type srvDorisDriver struct{ st *srvDorisSQLiteState }
type srvDorisConn struct{ inner driver.Conn }

func (d *srvDorisDriver) Open(name string) (driver.Conn, error) {
	d.st.mu.Lock()
	dsn, ok := d.st.dsns[name]
	base := d.st.baseDrv
	d.st.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown instance %q", name)
	}
	c, err := base.Open(dsn)
	if err != nil {
		return nil, err
	}
	if ex, ok := c.(driver.ExecerContext); ok {
		_, _ = ex.ExecContext(context.Background(), "PRAGMA busy_timeout = 5000", nil)
	}
	return &srvDorisConn{inner: c}, nil
}

func (c *srvDorisConn) Prepare(q string) (driver.Stmt, error) { return c.inner.Prepare(q) }
func (c *srvDorisConn) Close() error                          { return c.inner.Close() }
func (c *srvDorisConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *srvDorisConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := c.inner.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}
	return nil, driver.ErrSkip
}

func srvTranslateSQL(q string) string {
	up := strings.ToUpper(strings.TrimLeft(q, " \t\r\n"))
	if strings.HasPrefix(up, "CREATE TABLE") {
		idx := strings.Index(q, "CREATE TABLE IF NOT EXISTS ")
		rest := q[idx+len("CREATE TABLE IF NOT EXISTS "):]
		tbl := rest[:strings.IndexAny(rest, " (\t\r\n")]
		pk := "PRIMARY KEY (tenant_id, res_id)"
		switch tbl {
		case "fhir_resource":
			pk = "PRIMARY KEY (tenant_id, res_type, res_id)"
		case "fhir_resource_history":
			pk = "PRIMARY KEY (tenant_id, res_type, res_id, res_version)"
		case "fhir_audit_event":
			pk = "PRIMARY KEY (tenant_id, audit_id)"
		case "fhir_export_job":
			pk = "PRIMARY KEY (tenant_id, job_id)"
		case "fhir_cluster_secret":
			pk = "PRIMARY KEY (secret_key)"
		case "fhir_cluster_state":
			pk = "PRIMARY KEY (namespace, state_key)"
		}
		if bodyEnd := strings.Index(q, "\nUNIQUE KEY("); bodyEnd != -1 {
			var kept []string
			for _, line := range strings.Split(q[:bodyEnd], "\n") {
				tr := strings.TrimSpace(line)
				if strings.HasPrefix(tr, "INDEX ") || tr == ")" {
					continue
				}
				kept = append(kept, line)
			}
			if n := len(kept); n > 0 && strings.HasSuffix(strings.TrimRight(kept[n-1], " \t\r"), ",") {
				kept[n-1] = strings.TrimSuffix(strings.TrimRight(kept[n-1], " \t\r"), ",")
			}
			out := strings.Join(kept, "\n") + ",\n    " + pk + "\n);"
			out = strings.ReplaceAll(out, "ARRAY<VARCHAR(512)>", "TEXT")
			out = strings.ReplaceAll(out, "DATETIME(6)", "TEXT")
			out = srvReVarchar.ReplaceAllString(out, "TEXT")
			out = strings.ReplaceAll(out, "BOOLEAN", "INTEGER")
			out = strings.ReplaceAll(out, "DOUBLE", "REAL")
			out = strings.ReplaceAll(out, "BIGINT", "INTEGER")
			return strings.ReplaceAll(out, " STRING ", " TEXT ")
		}
	}
	if strings.HasPrefix(up, "INSERT INTO ") {
		q = "INSERT OR REPLACE INTO " + q[len("INSERT INTO "):]
		q = strings.ReplaceAll(q, ", []", ", '[]'")
		return srvReArrayPlace.ReplaceAllString(q, "json_array($1)")
	}
	if strings.HasPrefix(up, "SELECT") {
		return srvReArrContains.ReplaceAllString(q, "EXISTS (SELECT 1 FROM json_each($1) WHERE value = ?)")
	}
	return q
}

func srvTranslateArgs(args []driver.NamedValue) []driver.NamedValue {
	if len(args) == 0 {
		return nil
	}
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = a
		if t, ok := a.Value.(time.Time); ok {
			out[i].Value = t.UTC().Format("2006-01-02T15:04:05.000000Z")
		}
	}
	return out
}

func (c *srvDorisConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "SET ") {
		return driver.RowsAffected(0), nil
	}
	return c.inner.(driver.ExecerContext).ExecContext(ctx, srvTranslateSQL(query), srvTranslateArgs(args))
}

func (c *srvDorisConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.inner.(driver.QueryerContext).QueryContext(ctx, srvTranslateSQL(query), srvTranslateArgs(args))
}

func TestDorisDirectSQL_MultiReplicaHTTPParity(t *testing.T) {
	regSrvDorisOnce.Do(func() {
		boot, err := sql.Open("sqlite", ":memory:")
		require.NoError(t, err)
		srvDorisState.baseDrv = boot.Driver()
		require.NoError(t, boot.Close())
		sql.Register("srv_doris_sqlite", &srvDorisDriver{st: srvDorisState})
	})

	dbFile := filepath.Join(t.TempDir(), "srv_doris.db")
	sqliteDSN := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbFile)
	srvDorisState.mu.Lock()
	srvDorisState.dsns["repA"] = sqliteDSN
	srvDorisState.dsns["repB"] = sqliteDSN
	srvDorisState.mu.Unlock()

	dbA, err := sql.Open("srv_doris_sqlite", "repA")
	require.NoError(t, err)
	defer dbA.Close()
	dbB, err := sql.Open("srv_doris_sqlite", "repB")
	require.NoError(t, err)
	defer dbB.Close()

	require.NoError(t, doris.RunMigrations(context.Background(), dbA))

	locker := doris.NewMemoryRowLocker()
	nodeA := doris.New(dbA, doris.WithRowLocker(locker), doris.WithSessionGroupCommitConfigured(true))
	nodeB := doris.New(dbB, doris.WithRowLocker(locker), doris.WithSessionGroupCommitConfigured(true))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srvA := server.New(nodeA, logger)
	srvB := server.New(nodeB, logger)
	server.RegisterDefaultResourceTypes(srvA)
	server.RegisterDefaultResourceTypes(srvB)
	srvA.SetSecurityPolicy(server.SecurityPolicy{IdempotencyTTL: time.Hour})
	srvB.SetSecurityPolicy(server.SecurityPolicy{IdempotencyTTL: time.Hour})

	tsA := httptest.NewServer(srvA.Handler())
	defer tsA.Close()
	tsB := httptest.NewServer(srvB.Handler())
	defer tsB.Close()

	patBody := `{"resourceType":"Patient","id":"http-doris-1","name":[{"family":"Stateless","given":["Doris"]}],"gender":"female"}`
	reqPost, err := http.NewRequest(http.MethodPost, tsA.URL+"/fhir/r4/demo/Patient", strings.NewReader(patBody))
	require.NoError(t, err)
	reqPost.Header.Set("Content-Type", "application/fhir+json")
	reqPost.Header.Set("Idempotency-Key", "doris-idem-1")
	respPost, err := tsA.Client().Do(reqPost)
	require.NoError(t, err)
	respPost.Body.Close()
	assert.Equal(t, http.StatusCreated, respPost.StatusCode)

	// Replay same Idempotency-Key against Node B -> X-Idempotency-Replayed: true
	reqReplay, err := http.NewRequest(http.MethodPost, tsB.URL+"/fhir/r4/demo/Patient", strings.NewReader(patBody))
	require.NoError(t, err)
	reqReplay.Header.Set("Content-Type", "application/fhir+json")
	reqReplay.Header.Set("Idempotency-Key", "doris-idem-1")
	respReplay, err := tsB.Client().Do(reqReplay)
	require.NoError(t, err)
	respReplay.Body.Close()
	assert.Equal(t, http.StatusCreated, respReplay.StatusCode)
	assert.Equal(t, "true", respReplay.Header.Get("X-Idempotency-Replayed"))

	// Read and search via Node B
	respGet, err := tsB.Client().Get(tsB.URL + "/fhir/r4/demo/Patient/http-doris-1")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, respGet.StatusCode)
	respGet.Body.Close()

	respSearch, err := tsB.Client().Get(tsB.URL + "/fhir/r4/demo/Patient?family=Stateless")
	require.NoError(t, err)
	var searchBundle map[string]any
	require.NoError(t, json.NewDecoder(respSearch.Body).Decode(&searchBundle))
	respSearch.Body.Close()
	assert.Equal(t, float64(1), searchBundle["total"])

	// Update & Delete via Node B -> 410 Gone on Node A
	reqPut, err := http.NewRequest(http.MethodPut, tsB.URL+"/fhir/r4/demo/Patient/http-doris-1", strings.NewReader(`{"resourceType":"Patient","id":"http-doris-1","active":true}`))
	require.NoError(t, err)
	reqPut.Header.Set("Content-Type", "application/fhir+json")
	respPut, err := tsB.Client().Do(reqPut)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, respPut.StatusCode)
	assert.Equal(t, `W/"2"`, respPut.Header.Get("ETag"))
	respPut.Body.Close()

	reqDel, err := http.NewRequest(http.MethodDelete, tsB.URL+"/fhir/r4/demo/Patient/http-doris-1", nil)
	require.NoError(t, err)
	respDel, err := tsB.Client().Do(reqDel)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, respDel.StatusCode)
	respDel.Body.Close()

	respGone, err := tsA.Client().Get(tsA.URL + "/fhir/r4/demo/Patient/http-doris-1")
	require.NoError(t, err)
	assert.Equal(t, http.StatusGone, respGone.StatusCode)
	respGone.Body.Close()

	// AuditEvent written on Node A is queryable on Node B
	respAudit, err := tsB.Client().Get(tsB.URL + "/fhir/r4/demo/AuditEvent")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, respAudit.StatusCode)
	var auditBundle map[string]any
	require.NoError(t, json.NewDecoder(respAudit.Body).Decode(&auditBundle))
	respAudit.Body.Close()
	assert.GreaterOrEqual(t, auditBundle["total"], float64(3))
}
