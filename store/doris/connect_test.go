package doris_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/store/doris"
)

func TestNormalizeDSNAndConfigurePool(t *testing.T) {
	normalized, err := doris.NormalizeDSN("flint_svc:secret@tcp(doris-fe.internal:9030)/", doris.DSNOptions{
		DefaultDatabase: "flint_prod",
		GroupCommit:     doris.GroupCommitAsync,
		WorkloadGroup:   "flint_oltp",
		ComputeGroup:    "cg_prod",
		TLSConfig:       "skip-verify",
		RequireNonRoot:  true,
	})
	require.NoError(t, err)

	cfg, err := mysql.ParseDSN(normalized)
	require.NoError(t, err)
	assert.Equal(t, "flint_svc", cfg.User)
	assert.Equal(t, "flint_prod", cfg.DBName)
	assert.True(t, cfg.ParseTime)
	assert.Equal(t, time.UTC, cfg.Loc)
	assert.True(t, cfg.InterpolateParams)
	assert.Equal(t, 10*time.Second, cfg.Timeout)
	assert.Equal(t, 30*time.Second, cfg.ReadTimeout)
	assert.Equal(t, 30*time.Second, cfg.WriteTimeout)
	assert.Equal(t, "skip-verify", cfg.TLSConfig)
	assert.Equal(t, "'async_mode'", cfg.Params["group_commit"])
	assert.Equal(t, "'flint_oltp'", cfg.Params["workload_group"])
	assert.Equal(t, "'cg_prod'", cfg.Params["compute_group"])

	// Existing DBName and custom timeouts in DSN are preserved
	customDSN, err := doris.NormalizeDSN("app:pw@tcp(127.0.0.1:9030)/existing_db?timeout=5s&readTimeout=12s&writeTimeout=15s", doris.DSNOptions{
		DefaultDatabase: "ignored_default",
		ConnectTimeout:  20 * time.Second,
		ReadTimeout:     40 * time.Second,
		WriteTimeout:    50 * time.Second,
	})
	require.NoError(t, err)
	customCfg, err := mysql.ParseDSN(customDSN)
	require.NoError(t, err)
	assert.Equal(t, "existing_db", customCfg.DBName)
	assert.Equal(t, 5*time.Second, customCfg.Timeout)
	assert.Equal(t, 12*time.Second, customCfg.ReadTimeout)
	assert.Equal(t, 15*time.Second, customCfg.WriteTimeout)

	// Rejection cases
	_, err = doris.NormalizeDSN("   ", doris.DSNOptions{})
	require.ErrorContains(t, err, "empty")

	_, err = doris.NormalizeDSN("not-a-valid-dsn", doris.DSNOptions{})
	require.Error(t, err)

	for _, rootUser := range []string{"", "root", "ROOT", "admin"} {
		_, err = doris.NormalizeDSN(rootUser+":pw@tcp(127.0.0.1:9030)/flint", doris.DSNOptions{RequireNonRoot: true})
		require.ErrorContains(t, err, "non-root")
	}

	_, err = doris.NormalizeDSN("flint@tcp(127.0.0.1:9030)/", doris.DSNOptions{DefaultDatabase: "bad;db"})
	require.ErrorContains(t, err, "invalid doris SQL identifier")

	_, err = doris.NormalizeDSN("flint@tcp(127.0.0.1:9030)/bad-db", doris.DSNOptions{})
	require.ErrorContains(t, err, "invalid doris SQL identifier")

	_, err = doris.NormalizeDSN("flint@tcp(127.0.0.1:9030)/flint", doris.DSNOptions{GroupCommit: "bad_gc"})
	require.ErrorContains(t, err, "invalid doris group commit")

	_, err = doris.NormalizeDSN("flint@tcp(127.0.0.1:9030)/flint", doris.DSNOptions{WorkloadGroup: "bad'; DROP"})
	require.ErrorContains(t, err, "invalid doris workload_group")

	_, err = doris.NormalizeDSN("flint@tcp(127.0.0.1:9030)/flint", doris.DSNOptions{ComputeGroup: "bad'; DROP"})
	require.ErrorContains(t, err, "invalid doris compute_group")

	// ConfigurePool nil-safe + clamping
	doris.ConfigurePool(nil, 10, 5, time.Minute, time.Minute)
	registerDorisDrivers()
	db, err := sql.Open("doris_stub_ok", "")
	require.NoError(t, err)
	defer db.Close()
	doris.ConfigurePool(db, 5, 20, 0, 0)
	doris.ConfigurePool(db, 0, 0, 2*time.Minute, 30*time.Second)
}

func TestResourceLimitsAndSetVarHints(t *testing.T) {
	empty := doris.ResourceLimits{}
	require.NoError(t, empty.Validate())
	assert.Equal(t, "", empty.FormatSetVarHint())

	limits := doris.ResourceLimits{
		WorkloadGroup:   "flint_api",
		ComputeGroup:    "cg_west",
		ExecMemLimit:    2147483648,
		CPUThreads:      8,
		QueryTimeoutSec: 30,
	}
	require.NoError(t, limits.Validate())
	hint := limits.FormatSetVarHint()
	assert.Equal(t, "/*+ SET_VAR(workload_group='flint_api', compute_group='cg_west', exec_mem_limit=2147483648, parallel_pipeline_task_num=8, query_timeout=30) */", hint)

	// Injection applies ONLY to SELECT without existing optimizer hint
	assert.Equal(t, "SELECT "+hint+" id FROM fhir_resource", doris.InjectSelectSetVarHint("SELECT id FROM fhir_resource", hint))
	assert.Equal(t, "  select "+hint+" * FROM fhir_resource", doris.InjectSelectSetVarHint("  select\t* FROM fhir_resource", hint))
	assert.Equal(t, "SELECT /*+ existing */ * FROM fhir_resource", doris.InjectSelectSetVarHint("SELECT /*+ existing */ * FROM fhir_resource", hint))
	assert.Equal(t, "INSERT INTO fhir_resource VALUES (?)", doris.InjectSelectSetVarHint("INSERT INTO fhir_resource VALUES (?)", hint))
	assert.Equal(t, "DELETE FROM fhir_resource WHERE tenant_id = ?", doris.InjectSelectSetVarHint("DELETE FROM fhir_resource WHERE tenant_id = ?", hint))
	assert.Equal(t, "SELECT_FOO()", doris.InjectSelectSetVarHint("SELECT_FOO()", hint))
	assert.Equal(t, "SELECT id", doris.InjectSelectSetVarHint("SELECT id", ""))

	// Invalid resource limits fail Validate and yield empty hint
	badWG := doris.ResourceLimits{WorkloadGroup: "evil'; DROP TABLE fhir_resource"}
	require.Error(t, badWG.Validate())
	assert.Equal(t, "", badWG.FormatSetVarHint())

	badCG := doris.ResourceLimits{ComputeGroup: "bad space"}
	require.Error(t, badCG.Validate())

	require.Error(t, (doris.ResourceLimits{ExecMemLimit: -1}).Validate())
	require.Error(t, (doris.ResourceLimits{CPUThreads: -1}).Validate())
	require.Error(t, (doris.ResourceLimits{QueryTimeoutSec: -1}).Validate())
}

func TestTenantIsolationIdentifiersAndSchemaMigrations(t *testing.T) {
	registerDorisDrivers()

	mode, err := doris.ParseTenantIsolationMode("")
	require.NoError(t, err)
	assert.Equal(t, doris.TenantIsolationShared, mode)

	mode, err = doris.ParseTenantIsolationMode("DATABASE")
	require.NoError(t, err)
	assert.Equal(t, doris.TenantIsolationDatabase, mode)

	mode, err = doris.ParseTenantIsolationMode("schema")
	require.NoError(t, err)
	assert.Equal(t, doris.TenantIsolationDatabase, mode)

	_, err = doris.ParseTenantIsolationMode("unknown-mode")
	require.Error(t, err)

	dbName, err := doris.DatabaseFromSlug("Mayo-Clinic")
	require.NoError(t, err)
	assert.Equal(t, "tenant_mayo_clinic", dbName)

	dbName2, err := doris.DatabaseFromSlug("acme_01")
	require.NoError(t, err)
	assert.Equal(t, "tenant_acme_01", dbName2)

	for _, bad := range []string{"", "-lead", "trail-", "bad slug", "../etc", strings.Repeat("a", 57)} {
		_, err = doris.DatabaseFromSlug(bad)
		require.Error(t, err, "slug %q must be rejected", bad)
	}

	tenantDDLs := doris.TenantSchemaDDL(doris.SchemaOptions{ReplicationNum: 3})
	assert.Len(t, tenantDDLs, 17)
	for _, ddl := range tenantDDLs {
		assert.Contains(t, ddl, `"replication_num" = "3"`)
	}
	clusterDDLs := doris.ClusterSchemaDDL(doris.SchemaOptions{ReplicationNum: 3})
	assert.Len(t, clusterDDLs, 2)
	for _, ddl := range clusterDDLs {
		assert.Contains(t, ddl, `"replication_num" = "3"`)
	}

	qDDL, err := doris.QualifyCreateTableDDL("CREATE TABLE IF NOT EXISTS fhir_patient (tenant_id VARCHAR(64));", "tenant_mayo_clinic")
	require.NoError(t, err)
	assert.Contains(t, qDDL, "CREATE TABLE IF NOT EXISTS `tenant_mayo_clinic`.`fhir_patient`")

	_, err = doris.QualifyCreateTableDDL("SELECT 1", "tenant_mayo_clinic")
	require.Error(t, err)
	_, err = doris.QualifyCreateTableDDL("CREATE TABLE IF NOT EXISTS fhir_patient", "bad-db")
	require.Error(t, err)

	dbOK, err := sql.Open("doris_stub_ok", "")
	require.NoError(t, err)
	defer dbOK.Close()

	require.NoError(t, doris.RunMigrationsWithOptions(context.Background(), dbOK, doris.MigrationOptions{
		Database:        "flint_cluster",
		TenantIsolation: doris.TenantIsolationDatabase,
		ReplicationNum:  3,
	}))
	require.NoError(t, doris.RunMigrationsWithOptions(context.Background(), dbOK, doris.MigrationOptions{
		Database:        "flint_shared",
		TenantIsolation: doris.TenantIsolationShared,
		ReplicationNum:  2,
	}))
	require.Error(t, doris.RunMigrationsWithOptions(context.Background(), dbOK, doris.MigrationOptions{
		Database: "bad;db",
	}))
}
