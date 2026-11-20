package doris_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"

	_ "modernc.org/sqlite"

	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
)

type recordedSQLOp struct {
	connID int64
	sql    string
}

type dorisSQLiteState struct {
	mu          sync.Mutex
	baseDriver  driver.Driver
	dsnByName   map[string]string
	nextConnID  atomic.Int64
	createdDBs  map[string][]string
	execLog     map[string][]recordedSQLOp
	setVarHints map[string][]string
}

var (
	regDorisSQLiteOnce sync.Once
	sharedDorisSQLite  = &dorisSQLiteState{
		dsnByName:   make(map[string]string),
		createdDBs:  make(map[string][]string),
		execLog:     make(map[string][]recordedSQLOp),
		setVarHints: make(map[string][]string),
	}
)

type dorisSQLiteTestDriver struct {
	state *dorisSQLiteState
}

func (d *dorisSQLiteTestDriver) Open(name string) (driver.Conn, error) {
	d.state.mu.Lock()
	targetDSN, ok := d.state.dsnByName[name]
	baseDrv := d.state.baseDriver
	d.state.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown doris_sqlite_test instance %q", name)
	}
	rawConn, err := baseDrv.Open(targetDSN)
	if err != nil {
		return nil, err
	}
	// Configure busy_timeout so concurrent replicas sharing SQLite WAL serialize cleanly.
	if execer, ok := rawConn.(driver.ExecerContext); ok {
		_, _ = execer.ExecContext(context.Background(), "PRAGMA busy_timeout = 5000", nil)
	}
	return &dorisSQLiteConn{
		instance: name,
		connID:   d.state.nextConnID.Add(1),
		state:    d.state,
		inner:    rawConn,
	}, nil
}

type dorisSQLiteConn struct {
	instance string
	connID   int64
	state    *dorisSQLiteState
	inner    driver.Conn
}

func (c *dorisSQLiteConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(query)
}
func (c *dorisSQLiteConn) Close() error { return c.inner.Close() }
func (c *dorisSQLiteConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *dorisSQLiteConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := c.inner.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}
	return nil, driver.ErrSkip
}

func (c *dorisSQLiteConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *dorisSQLiteConn) ResetSession(ctx context.Context) error {
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

var (
	reQualifiedTbl  = regexp.MustCompile("`([a-zA-Z_][a-zA-Z0-9_]*)`\\.`([a-zA-Z_][a-zA-Z0-9_]*)`")
	reCreateDB      = regexp.MustCompile("^CREATE\\s+DATABASE\\s+IF\\s+NOT\\s+EXISTS\\s+`?([a-zA-Z0-9_]+)`?$")
	reSetVarComment = regexp.MustCompile(`/\*\+\s*SET_VAR\([^)]*\)\s*\*/\s*`)
	reArrayPlace    = regexp.MustCompile(`\[(\?(?:,\s*\?)*)\]`)
	reArrayContains = regexp.MustCompile(`array_contains\(([a-zA-Z_][a-zA-Z0-9_]*),\s*\?\)`)
	reVarcharType   = regexp.MustCompile(`VARCHAR\(\d+\)`)
)

func translateCreateTableDDL(ddl string) string {
	qualified := reQualifiedTbl.ReplaceAllString(ddl, "${1}__${2}")
	const prefix = "CREATE TABLE IF NOT EXISTS "
	idx := strings.Index(qualified, prefix)
	if idx == -1 {
		return qualified
	}
	rest := qualified[idx+len(prefix):]
	end := strings.IndexAny(rest, " (\t\r\n")
	tblName := rest[:end]
	baseTbl := tblName
	if pos := strings.LastIndex(tblName, "__"); pos != -1 {
		baseTbl = tblName[pos+2:]
	}

	pkClause := "PRIMARY KEY (tenant_id, res_id)"
	switch baseTbl {
	case "fhir_resource":
		pkClause = "PRIMARY KEY (tenant_id, res_type, res_id)"
	case "fhir_resource_history":
		pkClause = "PRIMARY KEY (tenant_id, res_type, res_id, res_version)"
	case "fhir_audit_event":
		pkClause = "PRIMARY KEY (tenant_id, audit_id)"
	case "fhir_export_job":
		pkClause = "PRIMARY KEY (tenant_id, job_id)"
	case "fhir_cluster_secret":
		pkClause = "PRIMARY KEY (secret_key)"
	case "fhir_cluster_state":
		pkClause = "PRIMARY KEY (namespace, state_key)"
	}

	bodyEnd := strings.Index(qualified, "\nUNIQUE KEY(")
	if bodyEnd == -1 {
		return qualified
	}
	headerAndCols := qualified[:bodyEnd]
	lines := strings.Split(headerAndCols, "\n")
	var kept []string
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "INDEX ") {
			continue
		}
		if trim == ")" {
			continue
		}
		kept = append(kept, line)
	}
	// Strip trailing comma only on the final retained column line before appending PRIMARY KEY
	if n := len(kept); n > 0 {
		t := strings.TrimRight(kept[n-1], " \t\r")
		if strings.HasSuffix(t, ",") {
			kept[n-1] = strings.TrimSuffix(t, ",")
		}
	}
	sqlBody := strings.Join(kept, "\n") + ",\n    " + pkClause + "\n);"
	sqlBody = strings.ReplaceAll(sqlBody, "ARRAY<VARCHAR(512)>", "TEXT")
	sqlBody = strings.ReplaceAll(sqlBody, "DATETIME(6)", "TEXT")
	sqlBody = reVarcharType.ReplaceAllString(sqlBody, "TEXT")
	sqlBody = strings.ReplaceAll(sqlBody, "BOOLEAN", "INTEGER")
	sqlBody = strings.ReplaceAll(sqlBody, "DOUBLE", "REAL")
	sqlBody = strings.ReplaceAll(sqlBody, "BIGINT", "INTEGER")
	sqlBody = strings.ReplaceAll(sqlBody, " STRING ", " TEXT ")
	return sqlBody
}

func translateDorisSQLForSQLite(rawSQL string) string {
	q := reQualifiedTbl.ReplaceAllString(rawSQL, "${1}__${2}")
	trimUpper := strings.ToUpper(strings.TrimLeft(q, " \t\r\n"))
	if strings.HasPrefix(trimUpper, "CREATE TABLE") {
		return translateCreateTableDDL(q)
	}
	if strings.HasPrefix(trimUpper, "INSERT INTO ") {
		q = "INSERT OR REPLACE INTO " + q[len("INSERT INTO "):]
		q = strings.ReplaceAll(q, ", []", ", '[]'")
		q = strings.ReplaceAll(q, "([]", "('[]'")
		q = reArrayPlace.ReplaceAllString(q, "json_array($1)")
		return q
	}
	if strings.HasPrefix(trimUpper, "SELECT") {
		q = reSetVarComment.ReplaceAllString(q, "")
		q = reArrayContains.ReplaceAllString(q, "EXISTS (SELECT 1 FROM json_each($1) WHERE value = ?)")
		return q
	}
	return q
}

func translateDorisNamedValues(args []driver.NamedValue) []driver.NamedValue {
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

func (c *dorisSQLiteConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.state.mu.Lock()
	c.state.execLog[c.instance] = append(c.state.execLog[c.instance], recordedSQLOp{connID: c.connID, sql: query})
	trimmed := strings.TrimSpace(query)
	if m := reCreateDB.FindStringSubmatch(trimmed); len(m) == 2 {
		c.state.createdDBs[c.instance] = append(c.state.createdDBs[c.instance], m[1])
		c.state.mu.Unlock()
		return driver.RowsAffected(0), nil
	}
	if strings.HasPrefix(strings.ToUpper(trimmed), "SET ") {
		c.state.mu.Unlock()
		return driver.RowsAffected(0), nil
	}
	c.state.mu.Unlock()

	sqliteQuery := translateDorisSQLForSQLite(query)
	execer, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, sqliteQuery, translateDorisNamedValues(args))
}

func (c *dorisSQLiteConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.state.mu.Lock()
	c.state.execLog[c.instance] = append(c.state.execLog[c.instance], recordedSQLOp{connID: c.connID, sql: query})
	if hint := reSetVarComment.FindString(query); hint != "" {
		c.state.setVarHints[c.instance] = append(c.state.setVarHints[c.instance], strings.TrimSpace(hint))
	}
	c.state.mu.Unlock()

	sqliteQuery := translateDorisSQLForSQLite(query)
	queryer, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, sqliteQuery, translateDorisNamedValues(args))
}

func openTestDorisSQLiteDB(t *testing.T, instanceName string) (*sql.DB, string) {
	t.Helper()
	regDorisSQLiteOnce.Do(func() {
		boot, err := sql.Open("sqlite", ":memory:")
		require.NoError(t, err)
		sharedDorisSQLite.baseDriver = boot.Driver()
		require.NoError(t, boot.Close())
		sql.Register("doris_sqlite_test", &dorisSQLiteTestDriver{state: sharedDorisSQLite})
	})

	dbPath := filepath.Join(t.TempDir(), instanceName+".db")
	sqliteDSN := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbPath)

	sharedDorisSQLite.mu.Lock()
	sharedDorisSQLite.dsnByName[instanceName] = sqliteDSN
	sharedDorisSQLite.execLog[instanceName] = nil
	sharedDorisSQLite.setVarHints[instanceName] = nil
	sharedDorisSQLite.createdDBs[instanceName] = nil
	sharedDorisSQLite.mu.Unlock()

	db, err := sql.Open("doris_sqlite_test", instanceName)
	require.NoError(t, err)
	return db, sqliteDSN
}

func openReplicaOnSameSQLiteDB(t *testing.T, existingInstanceName, replicaInstanceName string) *sql.DB {
	t.Helper()
	sharedDorisSQLite.mu.Lock()
	targetDSN := sharedDorisSQLite.dsnByName[existingInstanceName]
	sharedDorisSQLite.dsnByName[replicaInstanceName] = targetDSN
	sharedDorisSQLite.execLog[replicaInstanceName] = nil
	sharedDorisSQLite.setVarHints[replicaInstanceName] = nil
	sharedDorisSQLite.mu.Unlock()

	db, err := sql.Open("doris_sqlite_test", replicaInstanceName)
	require.NoError(t, err)
	return db
}

func TestDorisDirectSQL_MultiReplicaStatelessParity(t *testing.T) {
	ctx := context.Background()
	dbA, _ := openTestDorisSQLiteDB(t, "multi_replica_shared")
	defer dbA.Close()
	dbB := openReplicaOnSameSQLiteDB(t, "multi_replica_shared", "multi_replica_b")
	defer dbB.Close()

	require.NoError(t, doris.RunMigrationsWithOptions(ctx, dbA, doris.MigrationOptions{
		TenantIsolation: doris.TenantIsolationShared,
		ReplicationNum:  1,
	}))

	locker := doris.NewMemoryRowLocker()
	nodeA := doris.New(
		dbA,
		doris.WithGroupCommit(doris.GroupCommitAsync),
		doris.WithRowLocker(locker),
		doris.WithResourceLimits(doris.ResourceLimits{
			WorkloadGroup:   "flint_api",
			ExecMemLimit:    536870912,
			CPUThreads:      4,
			QueryTimeoutSec: 15,
		}),
	)
	nodeB := doris.New(
		dbB,
		doris.WithGroupCommit(doris.GroupCommitSync),
		doris.WithRowLocker(locker),
	)
	assert.True(t, nodeA.DirectSQL())
	assert.NoError(t, nodeA.LoadFromDB(ctx)) // no-op in DirectSQL mode

	patProto, err := proto.Marshal(&patpb.Patient{
		Id: &dtpb.Id{Value: "p-doris-1"},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "Nightingale"},
				Given:  []*dtpb.String{{Value: "Florence"}},
			},
		},
	})
	require.NoError(t, err)

	pat2Proto, err := proto.Marshal(&patpb.Patient{
		Id: &dtpb.Id{Value: "p-doris-2"},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "Lovelace"},
				Given:  []*dtpb.String{{Value: "Ada"}},
			},
		},
	})
	require.NoError(t, err)

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	obs1Proto, err := proto.Marshal(&obspb.Observation{
		Id: &dtpb.Id{Value: "obs-d-1"},
		Code: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{{
				System: &dtpb.Uri{Value: "http://loinc.org"},
				Code:   &dtpb.Code{Value: "8480-6"},
			}},
		},
	})
	require.NoError(t, err)

	// 1. Write on Node A, read/vread/history/update on Node B (zero shared RAM!)
	rec1, err := nodeA.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t-prod",
		ResType:       "Patient",
		ResID:         "p-doris-1",
		CreateOnly:    true,
		ResourceProto: patProto,
		SearchIndexes: &store.SearchIndexes{
			Strings: []store.SpidxString{
				{TenantID: "t-prod", ResType: "Patient", ResID: "p-doris-1", SpName: "family", SpValue: "Nightingale"},
			},
			Tokens: []store.SpidxToken{
				{TenantID: "t-prod", ResType: "Patient", ResID: "p-doris-1", SpName: "identifier", SpSystem: "http://hospital.org/mrn", SpValue: "MRN-101"},
				{TenantID: "t-prod", ResType: "Patient", ResID: "p-doris-1", SpName: "_tag", SpSystem: "http://flint.health/tag", SpValue: "vip"},
			},
			URIs: []store.SpidxURI{
				{TenantID: "t-prod", ResType: "Patient", ResID: "p-doris-1", SpName: "_profile", SpValue: "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, rec1.ResVersion)

	// Verify pinned-connection `SET group_commit = 'async_mode'` ran on the SAME connection as INSERT
	sharedDorisSQLite.mu.Lock()
	opsA := append([]recordedSQLOp(nil), sharedDorisSQLite.execLog["multi_replica_shared"]...)
	sharedDorisSQLite.mu.Unlock()
	var sawPinnedGC bool
	for i := 1; i < len(opsA); i++ {
		if strings.HasPrefix(opsA[i].sql, "INSERT INTO fhir_resource") &&
			strings.Contains(opsA[i-1].sql, "SET group_commit = 'async_mode'") &&
			opsA[i].connID == opsA[i-1].connID {
			sawPinnedGC = true
			break
		}
	}
	assert.True(t, sawPinnedGC, "expected SET group_commit and INSERT to execute on identical pinned connection")

	// Node B sees CreateOnly conflict and reads exact record
	_, err = nodeB.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t-prod",
		ResType:       "Patient",
		ResID:         "p-doris-1",
		CreateOnly:    true,
		ResourceProto: patProto,
	})
	require.ErrorIs(t, err, store.ErrResourceAlreadyExists)

	gotB, err := nodeB.ReadResourceWithMeta(ctx, "t-prod", "Patient", "p-doris-1")
	require.NoError(t, err)
	assert.Equal(t, patProto, gotB.ResourceProto)

	// Update on Node B with If-Match=1 -> version 2; Node A reads v1 and v2 + full history
	rec2, err := nodeB.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:        "t-prod",
		ResType:         "Patient",
		ResID:           "p-doris-1",
		ExpectedVersion: 1,
		ResourceProto:   pat2Proto,
		SearchIndexes: &store.SearchIndexes{
			Strings: []store.SpidxString{
				{TenantID: "t-prod", ResType: "Patient", ResID: "p-doris-1", SpName: "family", SpValue: "Lovelace"},
			},
			Tokens: []store.SpidxToken{
				{TenantID: "t-prod", ResType: "Patient", ResID: "p-doris-1", SpName: "identifier", SpSystem: "http://hospital.org/mrn", SpValue: "MRN-101"},
				{TenantID: "t-prod", ResType: "Patient", ResID: "p-doris-1", SpName: "_tag", SpSystem: "http://flint.health/tag", SpValue: "vip"},
			},
			URIs: []store.SpidxURI{
				{TenantID: "t-prod", ResType: "Patient", ResID: "p-doris-1", SpName: "_profile", SpValue: "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, rec2.ResVersion)

	v1, err := nodeA.ReadResourceVersion(ctx, "t-prod", "Patient", "p-doris-1", 1)
	require.NoError(t, err)
	assert.Equal(t, patProto, v1.ResourceProto)

	hist, histTotal, err := nodeA.ListResourceHistory(ctx, "t-prod", "Patient", "p-doris-1", 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, histTotal)
	assert.Len(t, hist, 2)

	// 2. Write Batch on Node A, query/search/compartments/analytics on Node B
	require.NoError(t, nodeA.WriteBatch(ctx, []store.ResourceInput{
		{
			TenantID:      "t-prod",
			ResType:       "Observation",
			ResID:         "obs-d-1",
			ResourceProto: obs1Proto,
			SearchIndexes: &store.SearchIndexes{
				Tokens: []store.SpidxToken{
					{TenantID: "t-prod", ResType: "Observation", ResID: "obs-d-1", SpName: "code", SpSystem: "http://loinc.org", SpValue: "8480-6"},
				},
				References: []store.SpidxReference{
					{TenantID: "t-prod", ResType: "Observation", ResID: "obs-d-1", SpName: "patient", TargetType: "Patient", TargetID: "p-doris-1"},
				},
				Dates: []store.SpidxDate{
					{TenantID: "t-prod", ResType: "Observation", ResID: "obs-d-1", SpName: "date", SpLow: now, SpHigh: now},
				},
				Quantities: []store.SpidxQuantity{
					{TenantID: "t-prod", ResType: "Observation", ResID: "obs-d-1", SpName: "value-quantity", SpSystem: "http://unitsofmeasure.org", SpCode: "mm[Hg]", SpValue: 120.0},
				},
			},
		},
		{
			TenantID:      "t-prod",
			ResType:       "Observation",
			ResID:         "obs-d-2",
			ResourceProto: obs1Proto,
			SearchIndexes: &store.SearchIndexes{
				Tokens: []store.SpidxToken{
					{TenantID: "t-prod", ResType: "Observation", ResID: "obs-d-2", SpName: "code", SpSystem: "http://loinc.org", SpValue: "8480-6"},
				},
				References: []store.SpidxReference{
					{TenantID: "t-prod", ResType: "Observation", ResID: "obs-d-2", SpName: "patient", TargetType: "Patient", TargetID: "p-doris-1"},
				},
				Quantities: []store.SpidxQuantity{
					{TenantID: "t-prod", ResType: "Observation", ResID: "obs-d-2", SpName: "value-quantity", SpSystem: "http://unitsofmeasure.org", SpCode: "mm[Hg]", SpValue: 140.0},
				},
			},
		},
	}))

	// Node B Searches: inverted index pushdown + :exact + :contains + _include + :identifier
	srExact, err := nodeB.Search(ctx, store.SearchParams{
		TenantID: "t-prod",
		ResType:  "Patient",
		Strings:  map[string]string{"family:exact": "lovelace"},
		Tokens:   map[string]string{"identifier": "http://hospital.org/mrn|MRN-101"},
		URIs:     map[string]string{"_profile": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, srExact.Total)
	assert.Equal(t, "p-doris-1", srExact.Matches[0].ResID)

	srObs, err := nodeB.Search(ctx, store.SearchParams{
		TenantID:   "t-prod",
		ResType:    "Observation",
		References: map[string]string{"patient:identifier": "http://hospital.org/mrn|MRN-101"},
		Includes:   []store.IncludeParam{{SourceType: "Observation", ParamName: "patient"}},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, srObs.Total)
	assert.Len(t, srObs.Includes, 1)

	// Node A Search confirms SET_VAR hint was injected on SELECT and NEVER on INSERT/DELETE
	_, err = nodeA.Search(ctx, store.SearchParams{
		TenantID: "t-prod",
		ResType:  "Patient",
		Strings:  map[string]string{"family": "Love"},
	})
	require.NoError(t, err)
	sharedDorisSQLite.mu.Lock()
	hintsA := append([]string(nil), sharedDorisSQLite.setVarHints["multi_replica_shared"]...)
	opsAfter := append([]recordedSQLOp(nil), sharedDorisSQLite.execLog["multi_replica_shared"]...)
	sharedDorisSQLite.mu.Unlock()
	assert.NotEmpty(t, hintsA)
	for _, h := range hintsA {
		assert.Contains(t, h, "SET_VAR(workload_group='flint_api', exec_mem_limit=536870912, parallel_pipeline_task_num=4, query_timeout=15)")
	}
	for _, op := range opsAfter {
		if strings.HasPrefix(strings.TrimSpace(op.sql), "INSERT") || strings.HasPrefix(strings.TrimSpace(op.sql), "DELETE") {
			assert.NotContains(t, op.sql, "SET_VAR", "SET_VAR hint must never be injected into %s", op.sql)
		}
	}

	// Analytics & Compartments on Node B
	stats, err := nodeB.EvaluateObservationStats(ctx, store.ObservationStatsParams{
		TenantID:  "t-prod",
		PatientID: "p-doris-1",
		Code:      "8480-6",
	})
	require.NoError(t, err)
	require.Len(t, stats.Series, 1)
	assert.InDelta(t, 130.0, stats.Series[0].Average, 0.001)

	counts, err := nodeB.EvaluateCohortCounts(ctx, "t-prod", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, counts["Patient"])
	assert.Equal(t, 2, counts["Observation"])

	exported, err := nodeB.ExportResources(ctx, store.ExportParams{
		TenantID:      "t-prod",
		PatientID:     "p-doris-1",
		ResourceTypes: []string{"Observation"},
	})
	require.NoError(t, err)
	assert.Len(t, exported, 2)

	patEv, err := nodeB.PatientEverything(ctx, store.PatientEverythingParams{TenantID: "t-prod", PatientID: "p-doris-1"})
	require.NoError(t, err)
	assert.Equal(t, 3, patEv.Total)

	belongs, err := nodeB.BelongsToPatientCompartment(ctx, "t-prod", "Observation", "obs-d-1", "p-doris-1")
	require.NoError(t, err)
	assert.True(t, belongs)

	candidates, err := nodeB.SearchPatientMatchCandidates(ctx, store.PatientMatchCandidateParams{TenantID: "t-prod"})
	require.NoError(t, err)
	assert.Len(t, candidates, 1)

	graphRes, err := nodeB.ResourceGraph(ctx, store.ResourceGraphParams{TenantID: "t-prod", ResourceType: "Patient", ResourceID: "p-doris-1"})
	require.NoError(t, err)
	assert.Equal(t, 3, graphRes.Total)

	metaRes, err := nodeB.AggregateMeta(ctx, "t-prod", "")
	require.NoError(t, err)
	assert.Contains(t, metaRes.Profiles, "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient")

	// 3. Transaction Bundle Commit & Compensating Rollback across Node A & Node B
	_, err = nodeA.ExecuteTransactionBundle(ctx, []store.BundleOperation{
		{
			OpType: store.BundleOpWrite,
			Method: "PUT",
			Input: store.ResourceInput{
				TenantID:      "t-prod",
				ResType:       "Patient",
				ResID:         "p-doris-1",
				ResourceProto: patProto,
			},
		},
		{
			OpType: store.BundleOpWrite,
			Method: "POST",
			Input: store.ResourceInput{
				TenantID:      "t-prod",
				ResType:       "Patient",
				ResID:         "p-tx-temp",
				ResourceProto: pat2Proto,
				CreateOnly:    true,
			},
		},
		{
			OpType: store.BundleOpWrite,
			Method: "PUT",
			Input: store.ResourceInput{
				TenantID:        "t-prod",
				ResType:         "Patient",
				ResID:           "p-doris-1",
				ResourceProto:   patProto,
				ExpectedVersion: 99, // triggers rollback!
			},
		},
	})
	require.ErrorIs(t, err, store.ErrVersionConflict)

	// Verify Node B sees p-doris-1 rolled back to v2 and p-tx-temp completely removed
	afterRB, err := nodeB.ReadResourceWithMeta(ctx, "t-prod", "Patient", "p-doris-1")
	require.NoError(t, err)
	assert.Equal(t, 2, afterRB.ResVersion)
	_, err = nodeB.ReadResourceWithMeta(ctx, "t-prod", "Patient", "p-tx-temp")
	require.ErrorIs(t, err, sql.ErrNoRows)

	// Reindex & Expunge & Purge across nodes
	reidx, err := nodeB.Reindex(ctx, store.ReindexParams{
		TenantID: "t-prod",
		Extractors: map[string]store.IndexExtractorFunc{
			"Patient": func(tenantID, resID string, _ []byte) (*store.SearchIndexes, error) {
				return &store.SearchIndexes{
					Strings: []store.SpidxString{{TenantID: tenantID, ResType: "Patient", ResID: resID, SpName: "family", SpValue: "Reindexed"}},
				}, nil
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, reidx.ReindexedResources)

	srReidx, err := nodeA.Search(ctx, store.SearchParams{
		TenantID: "t-prod",
		ResType:  "Patient",
		Strings:  map[string]string{"family:exact": "reindexed"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, srReidx.Total)

	// 4. AuditEvent (with Track A metadata), ExportJob, ClusterSecret, ClusterState, OAuth2, Nonce across Node A <-> Node B
	require.NoError(t, nodeA.WriteAuditEvent(ctx, store.AuditRecord{
		TenantID:            "t-prod",
		AuditID:             "aud-direct-1",
		Action:              "C",
		SubtypeCode:         "create",
		Outcome:             "0",
		AgentSubject:        "svc-azra",
		AgentIssuer:         "https://idp.azra.health",
		ActorSubject:        "dr-smith",
		ActorIssuer:         "https://sso.azra.health",
		OperatorCrossTenant: true,
		RequestID:           "req-999",
		EntityType:          "Patient",
		EntityID:            "p-doris-1",
	}))
	audB, err := nodeB.ReadAuditEvent(ctx, "t-prod", "aud-direct-1")
	require.NoError(t, err)
	assert.Equal(t, "dr-smith", audB.ActorSubject)
	assert.Equal(t, "req-999", audB.RequestID)
	assert.True(t, audB.OperatorCrossTenant)

	audList, audTot, err := nodeB.QueryAuditEvents(ctx, store.AuditQueryParams{TenantID: "t-prod", Action: "C"})
	require.NoError(t, err)
	assert.Equal(t, 1, audTot)
	require.Len(t, audList, 1)

	require.NoError(t, nodeA.SaveExportJob(ctx, store.ExportJobRecord{
		ID:         "job-direct-1",
		TenantID:   "t-prod",
		PatientID:  "p-doris-1",
		TypesOrder: []string{"Patient", "Observation"},
		Counts:     map[string]int{"Patient": 1, "Observation": 2},
		Files:      map[string][]byte{"Patient": []byte(`{"resourceType":"Patient"}`)},
	}))
	jobB, err := nodeB.GetExportJob(ctx, "t-prod", "job-direct-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"Patient", "Observation"}, jobB.TypesOrder)
	assert.Equal(t, 2, jobB.Counts["Observation"])
	delJob, err := nodeA.DeleteExportJob(ctx, "t-prod", "job-direct-1")
	require.NoError(t, err)
	assert.True(t, delJob)

	secA, err := nodeA.GetOrCreateClusterSecret(ctx, "signing-key", func() ([]byte, error) {
		return []byte("secret-bytes-123"), nil
	})
	require.NoError(t, err)
	secB, err := nodeB.GetOrCreateClusterSecret(ctx, "signing-key", func() ([]byte, error) {
		return []byte("should-never-overwrite"), nil
	})
	require.NoError(t, err)
	assert.Equal(t, secA, secB)

	// ClusterState CAS across replicas
	cs1, swapped, err := nodeA.CompareAndSwapClusterState(ctx, 0, store.ClusterStateRecord{
		Namespace:    "idemp",
		Key:          "k1",
		TenantID:     "t-prod",
		SecondaryKey: "sec-k1",
		Value:        []byte("v1"),
	})
	require.NoError(t, err)
	assert.True(t, swapped)
	assert.Equal(t, int64(1), cs1.Version)

	_, swappedDup, err := nodeB.CompareAndSwapClusterState(ctx, 0, store.ClusterStateRecord{
		Namespace: "idemp",
		Key:       "k1",
		Value:     []byte("dup"),
	})
	require.NoError(t, err)
	assert.False(t, swappedDup)

	cs2, swapped2, err := nodeB.CompareAndSwapClusterState(ctx, 1, store.ClusterStateRecord{
		Namespace: "idemp",
		Key:       "k1",
		TenantID:  "t-prod",
		Value:     []byte("v2"),
	})
	require.NoError(t, err)
	assert.True(t, swapped2)
	assert.Equal(t, int64(2), cs2.Version)

	listed, err := nodeA.ListClusterStates(ctx, "idemp", "t-prod")
	require.NoError(t, err)
	assert.Len(t, listed, 1)

	consumedCS, ok, err := nodeB.ConsumeClusterState(ctx, "idemp", "k1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []byte("v2"), consumedCS.Value)

	// OAuth2 + Nonce across replicas
	require.NoError(t, nodeA.SaveAuthCode(ctx, "ac-99", store.OAuthAuthCodeRecord{
		ClientID:  "client-a",
		TenantID:  "t-prod",
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}))
	acB, ok, err := nodeB.ConsumeAuthCode(ctx, "ac-99")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "client-a", acB.ClientID)
	_, ok, err = nodeA.ConsumeAuthCode(ctx, "ac-99")
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, nodeA.SaveRefreshToken(ctx, "rt-99", store.OAuthRefreshTokenRecord{
		Subject:   "user-a",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}))
	rtPeek, ok, err := nodeB.PeekRefreshToken(ctx, "rt-99")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "user-a", rtPeek.Subject)
	require.NoError(t, nodeB.RevokeToken(ctx, "rt-99", time.Hour))
	revoked, err := nodeA.IsTokenRevoked(ctx, "rt-99")
	require.NoError(t, err)
	assert.True(t, revoked)

	claimed1, err := nodeA.ClaimNonce(ctx, "jti-abc", time.Minute)
	require.NoError(t, err)
	assert.True(t, claimed1)
	claimed2, err := nodeB.ClaimNonce(ctx, "jti-abc", time.Minute)
	require.NoError(t, err)
	assert.False(t, claimed2)

	// Purge soft + Expunge across replicas
	purged, err := nodeA.PurgePatientCompartment(ctx, "t-prod", "p-doris-1", false)
	require.NoError(t, err)
	assert.Len(t, purged, 3)

	expCount, err := nodeB.ExpungeResources(ctx, store.ExpungeParams{TenantID: "t-prod", ExpungeDeletedResources: true})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, expCount, 3)
}

func TestDorisDirectSQL_TenantDatabaseIsolationAndZeroBleed(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDorisSQLiteDB(t, "tenant_db_iso")
	defer db.Close()

	require.NoError(t, doris.RunMigrationsWithOptions(ctx, db, doris.MigrationOptions{
		Database:        "flint_cluster",
		TenantIsolation: doris.TenantIsolationDatabase,
		ReplicationNum:  1,
	}))

	ds := doris.New(
		db,
		doris.WithDatabase("flint_cluster"),
		doris.WithTenantIsolation(doris.TenantIsolationDatabase),
		doris.WithAutoProvision(true),
		doris.WithReplicationNum(1),
		doris.WithRowLocker(doris.NewMemoryRowLocker()),
	)
	assert.Equal(t, doris.TenantIsolationDatabase, ds.TenantIsolation())

	require.NoError(t, ds.WriteResource(ctx, store.ResourceInput{
		TenantID:      "mayo-clinic",
		ResType:       "Patient",
		ResID:         "pat-shared-id",
		ResourceProto: []byte("mayo-payload"),
		SearchIndexes: &store.SearchIndexes{
			Strings: []store.SpidxString{{TenantID: "mayo-clinic", ResType: "Patient", ResID: "pat-shared-id", SpName: "family", SpValue: "MayoFamily"}},
		},
	}))
	require.NoError(t, ds.WriteResource(ctx, store.ResourceInput{
		TenantID:      "cedars-sinai",
		ResType:       "Patient",
		ResID:         "pat-shared-id",
		ResourceProto: []byte("cedars-payload"),
		SearchIndexes: &store.SearchIndexes{
			Strings: []store.SpidxString{{TenantID: "cedars-sinai", ResType: "Patient", ResID: "pat-shared-id", SpName: "family", SpValue: "CedarsFamily"}},
		},
	}))

	mayoRec, err := ds.ReadResourceWithMeta(ctx, "mayo-clinic", "Patient", "pat-shared-id")
	require.NoError(t, err)
	assert.Equal(t, []byte("mayo-payload"), mayoRec.ResourceProto)

	cedarsRec, err := ds.ReadResourceWithMeta(ctx, "cedars-sinai", "Patient", "pat-shared-id")
	require.NoError(t, err)
	assert.Equal(t, []byte("cedars-payload"), cedarsRec.ResourceProto)

	// Search must strictly isolate to each tenant_<slug> database
	srMayo, err := ds.Search(ctx, store.SearchParams{TenantID: "mayo-clinic", ResType: "Patient"})
	require.NoError(t, err)
	assert.Equal(t, 1, srMayo.Total)
	assert.Equal(t, []byte("mayo-payload"), srMayo.Matches[0].ResourceProto)

	// Cluster state must remain in flint_cluster
	_, err = ds.PutClusterState(ctx, store.ClusterStateRecord{
		Namespace: "tenants",
		Key:       "global-cfg",
		Value:     []byte("ok"),
	})
	require.NoError(t, err)
	gotCS, ok, err := ds.GetClusterState(ctx, "tenants", "global-cfg")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []byte("ok"), gotCS.Value)

	// Verify physically separate CREATE DATABASE statements & qualified table SQL
	sharedDorisSQLite.mu.Lock()
	createdDBs := append([]string(nil), sharedDorisSQLite.createdDBs["tenant_db_iso"]...)
	ops := append([]recordedSQLOp(nil), sharedDorisSQLite.execLog["tenant_db_iso"]...)
	sharedDorisSQLite.mu.Unlock()

	assert.Contains(t, createdDBs, "flint_cluster")
	assert.Contains(t, createdDBs, "tenant_mayo_clinic")
	assert.Contains(t, createdDBs, "tenant_cedars_sinai")
	var sawQualifiedMayo, sawQualifiedCluster bool
	for _, op := range ops {
		if strings.Contains(op.sql, "`tenant_mayo_clinic`.`fhir_resource`") {
			sawQualifiedMayo = true
		}
		if strings.Contains(op.sql, "`flint_cluster`.`fhir_cluster_state`") {
			sawQualifiedCluster = true
		}
	}
	assert.True(t, sawQualifiedMayo)
	assert.True(t, sawQualifiedCluster)

	// Invalid tenant slug is rejected
	_, err = ds.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID: "bad slug!",
		ResType:  "Patient",
		ResID:    "p1",
	})
	require.ErrorContains(t, err, "invalid doris tenant slug")
}

func TestDorisDirectSQL_FullCoverageAndConditionalOps(t *testing.T) {
	ctx := context.Background()
	dbA, _ := openTestDorisSQLiteDB(t, "full_cov_a")
	defer dbA.Close()
	dbB := openReplicaOnSameSQLiteDB(t, "full_cov_a", "full_cov_b")
	defer dbB.Close()

	require.NoError(t, doris.RunMigrations(ctx, dbA))

	locker := doris.NewMemoryRowLocker()
	nodeA := doris.New(
		dbA,
		doris.WithRowLocker(locker),
		doris.WithLockTTL(5*time.Second),
		doris.WithDirectSQL(true),
		doris.WithSessionGroupCommitConfigured(true),
	)
	nodeB := doris.New(
		dbB,
		doris.WithRowLocker(locker),
		doris.WithLockTTL(5*time.Second),
	)
	assert.Empty(t, nodeA.ResourceLimits())

	baseTime := time.Now().UTC()

	// Seed Patient, Encounter, Observation, Practitioner, and Group across replicas
	_, err := nodeA.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t-cov",
		ResType:       "Practitioner",
		ResID:         "prac-1",
		ResourceProto: []byte{0x01, 'p'},
		SearchIndexes: &store.SearchIndexes{
			Strings: []store.SpidxString{{TenantID: "t-cov", ResType: "Practitioner", ResID: "prac-1", SpName: "family", SpValue: "House"}},
		},
	})
	require.NoError(t, err)

	_, err = nodeA.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t-cov",
		ResType:       "Patient",
		ResID:         "pat-1",
		ResourceProto: []byte{0x02, 'p'},
		SearchIndexes: &store.SearchIndexes{
			Strings: []store.SpidxString{{TenantID: "t-cov", ResType: "Patient", ResID: "pat-1", SpName: "family", SpValue: "curie"}},
			Tokens:  []store.SpidxToken{{TenantID: "t-cov", ResType: "Patient", ResID: "pat-1", SpName: "gender", SpValue: "female"}},
			URIs:    []store.SpidxURI{{TenantID: "t-cov", ResType: "Patient", ResID: "pat-1", SpName: "_profile", SpValue: "http://example.org/ig/base/Patient"}},
		},
	})
	require.NoError(t, err)

	_, err = nodeA.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t-cov",
		ResType:       "Patient",
		ResID:         "pat-2",
		ResourceProto: []byte{0x03, 'p'},
		SearchIndexes: &store.SearchIndexes{
			Strings: []store.SpidxString{{TenantID: "t-cov", ResType: "Patient", ResID: "pat-2", SpName: "family", SpValue: "darwin"}},
			Tokens:  []store.SpidxToken{{TenantID: "t-cov", ResType: "Patient", ResID: "pat-2", SpName: "gender", SpValue: "male"}},
		},
	})
	require.NoError(t, err)

	_, err = nodeA.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t-cov",
		ResType:       "Encounter",
		ResID:         "enc-1",
		ResourceProto: []byte{0x04, 'e'},
		SearchIndexes: &store.SearchIndexes{
			References: []store.SpidxReference{
				{TenantID: "t-cov", ResType: "Encounter", ResID: "enc-1", SpName: "patient", TargetType: "Patient", TargetID: "pat-1"},
				{TenantID: "t-cov", ResType: "Encounter", ResID: "enc-1", SpName: "participant", TargetType: "Practitioner", TargetID: "prac-1"},
			},
		},
	})
	require.NoError(t, err)

	_, err = nodeA.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t-cov",
		ResType:       "Observation",
		ResID:         "obs-cov-1",
		ResourceProto: []byte{0x05, 'o'},
		SearchIndexes: &store.SearchIndexes{
			Tokens: []store.SpidxToken{
				{TenantID: "t-cov", ResType: "Observation", ResID: "obs-cov-1", SpName: "code", SpSystem: "http://loinc.org", SpValue: "29463-7"},
			},
			References: []store.SpidxReference{
				{TenantID: "t-cov", ResType: "Observation", ResID: "obs-cov-1", SpName: "patient", TargetType: "Patient", TargetID: "pat-1"},
				{TenantID: "t-cov", ResType: "Observation", ResID: "obs-cov-1", SpName: "encounter", TargetType: "Encounter", TargetID: "enc-1"},
			},
			Dates: []store.SpidxDate{
				{TenantID: "t-cov", ResType: "Observation", ResID: "obs-cov-1", SpName: "date", SpLow: baseTime, SpHigh: baseTime},
			},
			Quantities: []store.SpidxQuantity{
				{TenantID: "t-cov", ResType: "Observation", ResID: "obs-cov-1", SpName: "value-quantity", SpSystem: "http://unitsofmeasure.org", SpCode: "kg", SpValue: 68.5},
			},
		},
	})
	require.NoError(t, err)

	_, err = nodeA.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t-cov",
		ResType:       "Group",
		ResID:         "grp-1",
		ResourceProto: []byte{0x06, 'g'},
	})
	require.NoError(t, err)

	// EncounterEverything & GroupEverything on Node B
	encEv, err := nodeB.EncounterEverything(ctx, store.EncounterEverythingParams{TenantID: "t-cov", EncounterID: "enc-1"})
	require.NoError(t, err)
	assert.Equal(t, 4, encEv.Total) // Encounter + Patient + Practitioner + Observation

	grpAll, err := nodeB.GroupEverything(ctx, store.GroupEverythingParams{TenantID: "t-cov", GroupID: "grp-1", AllPatients: true})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, grpAll.Total, 5)

	grpSubset, err := nodeB.GroupEverything(ctx, store.GroupEverythingParams{
		TenantID:   "t-cov",
		GroupID:    "grp-1",
		PatientIDs: []string{"pat-1"},
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, grpSubset.Total, 3)

	// Search filters, modifiers, _revinclude, _sort, _summary=count, pagination on Node B
	srComplex, err := nodeB.Search(ctx, store.SearchParams{
		TenantID:      "t-cov",
		ResTypes:      []string{"Patient"},
		ID:            "Patient/pat-1,Patient/pat-2",
		StringFilters: map[string][]string{"family": {"cur"}},
		TokenFilters:  map[string][]string{"gender:not": {"male"}},
		URIFilters:    map[string][]string{"_profile:below": {"http://example.org/ig/"}},
		RevIncludes:   []store.IncludeParam{{SourceType: "Observation", ParamName: "patient"}},
		Sort:          []store.SortSpec{{Param: "_lastUpdated", Desc: true}},
		Count:         10,
		Offset:        0,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, srComplex.Total)
	assert.Equal(t, "pat-1", srComplex.Matches[0].ResID)
	assert.Len(t, srComplex.Includes, 1)

	srCount, err := nodeB.Search(ctx, store.SearchParams{
		TenantID: "t-cov",
		ResType:  "Patient",
		Summary:  "count",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, srCount.Total)
	assert.Empty(t, srCount.Matches)

	srOffsetPast, err := nodeB.Search(ctx, store.SearchParams{
		TenantID: "t-cov",
		ResType:  "Patient",
		Offset:   50,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, srOffsetPast.Total)
	assert.Empty(t, srOffsetPast.Matches)

	// ConditionalCreate / Update / Delete on Node A & Node B
	existingRec, created, err := nodeB.ConditionalCreateResource(ctx, store.ResourceInput{
		TenantID:      "t-cov",
		ResType:       "Patient",
		ResID:         "pat-new-cond",
		ResourceProto: []byte{0x01},
	}, store.SearchParams{
		TenantID: "t-cov", ResType: "Patient", Tokens: map[string]string{"gender": "female"},
	})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, "pat-1", existingRec.ResID)

	createdRec, created, err := nodeA.ConditionalCreateResource(ctx, store.ResourceInput{
		TenantID:      "t-cov",
		ResType:       "Patient",
		ResID:         "pat-new-cond",
		ResourceProto: []byte{0x07, 'c'},
		SearchIndexes: &store.SearchIndexes{
			Tokens: []store.SpidxToken{{TenantID: "t-cov", ResType: "Patient", ResID: "pat-new-cond", SpName: "gender", SpValue: "other"}},
		},
	}, store.SearchParams{
		TenantID: "t-cov", ResType: "Patient", ID: "pat-new-cond",
	})
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, 1, createdRec.ResVersion)

	updCond, err := nodeB.ConditionalUpdateResource(ctx, "t-cov", "Patient", store.SearchParams{
		TenantID: "t-cov", ResType: "Patient", ID: "pat-new-cond",
	}, func(matchedID string, isExisting bool) (store.ResourceInput, error) {
		assert.True(t, isExisting)
		return store.ResourceInput{TenantID: "t-cov", ResType: "Patient", ResID: matchedID, ResourceProto: []byte{0x08}}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, updCond.ResVersion)

	// Delete paths: ExpectedVersion mismatch, match -> vread tombstone (ErrResourceDeleted), idempotent delete, non-existent
	_, err = nodeA.DeleteResourceWithExpectedVersion(ctx, "t-cov", "Patient", "pat-new-cond", 99)
	require.ErrorIs(t, err, store.ErrVersionConflict)

	delCond, err := nodeB.ConditionalDeleteResource(ctx, "t-cov", "Patient", store.SearchParams{
		TenantID: "t-cov", ResType: "Patient", ID: "pat-new-cond",
	}, func(matchedID string) error {
		assert.Equal(t, "pat-new-cond", matchedID)
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, delCond)
	assert.True(t, delCond.IsDeleted)

	_, err = nodeA.ReadResourceVersion(ctx, "t-cov", "Patient", "pat-new-cond", delCond.ResVersion)
	require.ErrorIs(t, err, store.ErrResourceDeleted)
	require.NoError(t, nodeA.DeleteResource(ctx, "t-cov", "Patient", "pat-new-cond"))
	require.NoError(t, nodeA.DeleteResource(ctx, "t-cov", "Patient", "does-not-exist"))

	// Conditional DELETE expectedVersion on 0 match fails ErrVersionConflict
	_, err = nodeA.ConditionalDeleteResource(ctx, "t-cov", "Patient", store.SearchParams{
		TenantID: "t-cov", ResType: "Patient", ID: "does-not-exist", ExpectedVersion: 1,
	}, nil)
	require.ErrorIs(t, err, store.ErrVersionConflict)

	// ExecuteTransactionBundle with conditional POST, conditional PUT, conditional DELETE + rollback after delete
	txOK, err := nodeA.ExecuteTransactionBundle(ctx, []store.BundleOperation{
		{
			OpType:      store.BundleOpWrite,
			Method:      "POST",
			Input:       store.ResourceInput{TenantID: "t-cov", ResType: "Patient", ResID: "pat-tx-new", ResourceProto: []byte{0x11}},
			IfNoneExist: &store.SearchParams{TenantID: "t-cov", ResType: "Patient", ID: "pat-tx-new"},
		},
		{
			OpType:            store.BundleOpWrite,
			Method:            "PUT",
			Input:             store.ResourceInput{TenantID: "t-cov", ResType: "Patient"},
			ConditionalParams: &store.SearchParams{TenantID: "t-cov", ResType: "Patient", ID: "pat-tx-new"},
			BuildWriteInput: func(matchedID string, _ bool) (store.ResourceInput, error) {
				return store.ResourceInput{TenantID: "t-cov", ResType: "Patient", ResID: matchedID, ResourceProto: []byte{0x12}}, nil
			},
		},
		{
			OpType:            store.BundleOpDelete,
			Method:            "DELETE",
			Input:             store.ResourceInput{TenantID: "t-cov", ResType: "Patient"},
			ConditionalParams: &store.SearchParams{TenantID: "t-cov", ResType: "Patient", ID: "pat-tx-new"},
			VerifyDeleteMatch: func(_ string) error { return nil },
		},
	})
	require.NoError(t, err)
	require.Len(t, txOK, 3)

	// System-level ListHistoryWithParams with Since & Offset
	sincePast := time.Now().UTC().Add(-time.Hour)
	sysHist, sysHistTot, err := nodeB.ListHistoryWithParams(ctx, store.HistoryParams{
		TenantID: "t-cov",
		Since:    &sincePast,
		Limit:    10,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, sysHistTot, 6)
	assert.NotEmpty(t, sysHist)

	histPast, _, err := nodeB.ListHistoryWithParams(ctx, store.HistoryParams{TenantID: "t-cov", Offset: 1000})
	require.NoError(t, err)
	assert.Empty(t, histPast)

	// Expunge previous versions on pat-tx-new
	expPrev, err := nodeA.ExpungeResources(ctx, store.ExpungeParams{
		TenantID:                "t-cov",
		ResType:                 "Patient",
		ResID:                   "pat-tx-new",
		ExpungePreviousVersions: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, expPrev)

	// Analytics: EvaluateObservationStats with System/Start/End, EvaluateCohortCounts filtered, StreamAnalyticalExport Since/Until
	obsStats, err := nodeB.EvaluateObservationStats(ctx, store.ObservationStatsParams{
		TenantID: "t-cov",
		Code:     "29463-7",
		System:   "http://loinc.org",
		Start:    &sincePast,
	})
	require.NoError(t, err)
	require.Len(t, obsStats.Series, 1)
	assert.InDelta(t, 68.5, obsStats.Series[0].Median, 0.001)

	filtCounts, err := nodeB.EvaluateCohortCounts(ctx, "t-cov", []string{"Observation"})
	require.NoError(t, err)
	assert.Equal(t, 1, filtCounts["Observation"])

	untilFuture := time.Now().UTC().Add(time.Minute)
	expFiltered, err := nodeA.ExportResources(ctx, store.ExportParams{
		TenantID:      "t-cov",
		PatientIDs:    []string{"Patient/pat-1"},
		ResourceTypes: []string{"Observation"},
		Since:         &sincePast,
		Until:         &untilFuture,
	})
	require.NoError(t, err)
	assert.Len(t, expFiltered, 1)

	// QueryAuditEvents full filter pushdown
	require.NoError(t, nodeA.WriteAuditEvent(ctx, store.AuditRecord{
		TenantID:     "t-cov",
		AuditID:      "aud-cov-1",
		Action:       "R",
		SubtypeCode:  "search-type",
		Outcome:      "0",
		AgentSubject: "alice",
		AgentPatient: "pat-1",
		EntityType:   "Observation",
		EntityID:     "obs-cov-1",
	}))
	audRes, audTot, err := nodeB.QueryAuditEvents(ctx, store.AuditQueryParams{
		TenantID:     "t-cov",
		Action:       "R",
		SubtypeCode:  "search-type",
		Outcome:      "0",
		AgentSubject: "alice",
		AgentPatient: "pat-1",
		EntityType:   "Observation",
		EntityID:     "obs-cov-1",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, audTot)
	require.Len(t, audRes, 1)

	// ClusterState Secondary + DeleteClusterState + ConsumeRefreshToken
	_, err = nodeA.PutClusterState(ctx, store.ClusterStateRecord{
		Namespace:    "launch",
		Key:          "l-1",
		TenantID:     "t-cov",
		SecondaryKey: "ctx-abc",
		Value:        []byte("payload"),
	})
	require.NoError(t, err)
	bySec, ok, err := nodeB.GetClusterStateBySecondary(ctx, "launch", "ctx-abc")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "l-1", bySec.Key)

	deletedCS, err := nodeA.DeleteClusterState(ctx, "launch", "l-1")
	require.NoError(t, err)
	assert.True(t, deletedCS)
	deletedCS2, err := nodeB.DeleteClusterState(ctx, "launch", "l-1")
	require.NoError(t, err)
	assert.False(t, deletedCS2)

	require.NoError(t, nodeA.SaveRefreshToken(ctx, "rt-consume", store.OAuthRefreshTokenRecord{
		Subject:   "user-consume",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}))
	rtRec, ok, err := nodeB.ConsumeRefreshToken(ctx, "rt-consume")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "user-consume", rtRec.Subject)
	_, ok, err = nodeA.ConsumeRefreshToken(ctx, "rt-consume")
	require.NoError(t, err)
	assert.False(t, ok)

	// Hard PurgePatientCompartment on pat-1
	hardPurged, err := nodeA.PurgePatientCompartment(ctx, "t-cov", "pat-1", true)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(hardPurged), 3)
	_, err = nodeB.ReadResourceWithMeta(ctx, "t-cov", "Observation", "obs-cov-1")
	require.ErrorIs(t, err, sql.ErrNoRows)
}
