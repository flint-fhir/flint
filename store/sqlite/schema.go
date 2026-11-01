// Package sqlite implements the embedded, zero-dependency SQLite WAL operational
// and analytical store for Flint, along with a zero-CGO DuckDB sqlite_scanner bridge.
//
// Like store/postgres, package sqlite stores canonical Google FHIR R4 Protocol Buffer
// wire bytes (resource_proto BLOB) with zero JSON at rest, paired with the 6 HAPI-style
// spidx_* B-tree index tables (spidx_string, spidx_token, spidx_date, spidx_reference,
// spidx_quantity, spidx_uri) on WITHOUT ROWID clustered primary keys in WAL mode.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// JournalMode configures the SQLite journal_mode pragma.
type JournalMode string

const (
	// JournalWAL enables SQLite Write-Ahead Logging for concurrent readers + serialized writer.
	JournalWAL JournalMode = "WAL"
	// JournalMemory keeps the rollback journal in RAM for ephemeral / in-memory sandboxes.
	JournalMemory JournalMode = "MEMORY"
	// JournalDelete uses the classic rollback journal.
	JournalDelete JournalMode = "DELETE"
)

// ParseJournalMode parses a SQLite journal mode configuration string.
func ParseJournalMode(raw string) (JournalMode, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "", "WAL":
		return JournalWAL, nil
	case "MEMORY":
		return JournalMemory, nil
	case "DELETE":
		return JournalDelete, nil
	default:
		return "", fmt.Errorf("invalid sqlite journal mode %q (valid: WAL, MEMORY, DELETE)", raw)
	}
}

// PragmaStatements returns the SQLite tuning pragmas for low-latency FHIR OLTP workloads.
func PragmaStatements(mode JournalMode) []string {
	if mode == "" {
		mode = JournalWAL
	}
	return []string{
		fmt.Sprintf("PRAGMA journal_mode = %s;", mode),
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA temp_store = MEMORY;",
	}
}

// SchemaDDL returns the Zero-JSON 6-table spidx_* B-tree DDL statements for SQLite.
func SchemaDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS fhir_resource (
    tenant_id       TEXT    NOT NULL,
    res_type        TEXT    NOT NULL,
    res_id          TEXT    NOT NULL,
    res_version     INTEGER NOT NULL DEFAULT 1,
    is_deleted      INTEGER NOT NULL DEFAULT 0,
    last_updated    TEXT    NOT NULL,
    idempotency_key TEXT    NULL,
    resource_proto  BLOB    NOT NULL,
    PRIMARY KEY (tenant_id, res_type, res_id)
) WITHOUT ROWID;`,
		`CREATE TABLE IF NOT EXISTS fhir_resource_history (
    tenant_id      TEXT    NOT NULL,
    res_type       TEXT    NOT NULL,
    res_id         TEXT    NOT NULL,
    res_version    INTEGER NOT NULL,
    is_deleted     INTEGER NOT NULL DEFAULT 0,
    last_updated   TEXT    NOT NULL,
    resource_proto BLOB    NULL,
    PRIMARY KEY (tenant_id, res_type, res_id, res_version)
) WITHOUT ROWID;`,
		`CREATE TABLE IF NOT EXISTS spidx_string (
    tenant_id      TEXT NOT NULL,
    res_type       TEXT NOT NULL,
    res_id         TEXT NOT NULL,
    sp_name        TEXT NOT NULL,
    sp_value       TEXT NOT NULL,
    sp_value_lower TEXT NOT NULL
);`,
		`CREATE INDEX IF NOT EXISTS idx_spidx_string_lookup ON spidx_string (tenant_id, res_type, sp_name, sp_value_lower);`,
		`CREATE TABLE IF NOT EXISTS spidx_token (
    tenant_id TEXT NOT NULL,
    res_type  TEXT NOT NULL,
    res_id    TEXT NOT NULL,
    sp_name   TEXT NOT NULL,
    sp_system TEXT NOT NULL,
    sp_value  TEXT NOT NULL
);`,
		`CREATE INDEX IF NOT EXISTS idx_spidx_token_lookup ON spidx_token (tenant_id, res_type, sp_name, sp_system, sp_value);`,
		`CREATE TABLE IF NOT EXISTS spidx_date (
    tenant_id TEXT NOT NULL,
    res_type  TEXT NOT NULL,
    res_id    TEXT NOT NULL,
    sp_name   TEXT NOT NULL,
    sp_low    TEXT NOT NULL,
    sp_high   TEXT NOT NULL
);`,
		`CREATE INDEX IF NOT EXISTS idx_spidx_date_lookup ON spidx_date (tenant_id, res_type, sp_name, sp_low, sp_high);`,
		`CREATE TABLE IF NOT EXISTS spidx_reference (
    tenant_id   TEXT NOT NULL,
    res_type    TEXT NOT NULL,
    res_id      TEXT NOT NULL,
    sp_name     TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id   TEXT NOT NULL
);`,
		`CREATE INDEX IF NOT EXISTS idx_spidx_ref_lookup ON spidx_reference (tenant_id, res_type, sp_name, target_type, target_id);`,
		`CREATE TABLE IF NOT EXISTS spidx_quantity (
    tenant_id TEXT NOT NULL,
    res_type  TEXT NOT NULL,
    res_id    TEXT NOT NULL,
    sp_name   TEXT NOT NULL,
    sp_system TEXT NOT NULL,
    sp_code   TEXT NOT NULL,
    sp_value  REAL NOT NULL
);`,
		`CREATE INDEX IF NOT EXISTS idx_spidx_qty_lookup ON spidx_quantity (tenant_id, res_type, sp_name, sp_code, sp_value);`,
		`CREATE TABLE IF NOT EXISTS spidx_uri (
    tenant_id TEXT NOT NULL,
    res_type  TEXT NOT NULL,
    res_id    TEXT NOT NULL,
    sp_name   TEXT NOT NULL,
    sp_value  TEXT NOT NULL
);`,
		`CREATE INDEX IF NOT EXISTS idx_spidx_uri_lookup ON spidx_uri (tenant_id, res_type, sp_name, sp_value);`,
		`CREATE TABLE IF NOT EXISTS fhir_audit_event (
    tenant_id      TEXT    NOT NULL,
    audit_id       TEXT    NOT NULL,
    recorded       TEXT    NOT NULL,
    action         TEXT    NOT NULL,
    subtype_code   TEXT    NOT NULL,
    outcome        TEXT    NOT NULL,
    outcome_desc   TEXT    NULL,
    http_method    TEXT    NULL,
    http_status    INTEGER NULL,
    request_uri    TEXT    NULL,
    agent_subject  TEXT    NULL,
    agent_patient  TEXT    NULL,
    client_ip      TEXT    NULL,
    entity_type    TEXT    NULL,
    entity_id      TEXT    NULL,
    entity_version TEXT    NULL,
    PRIMARY KEY (tenant_id, audit_id)
) WITHOUT ROWID;`,
		`CREATE TABLE IF NOT EXISTS fhir_export_job (
    tenant_id   TEXT NOT NULL,
    job_id      TEXT NOT NULL,
    patient_id  TEXT NULL,
    request_url TEXT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (tenant_id, job_id)
) WITHOUT ROWID;`,
	}
}

// RunMigrations applies the SQLite pragmas and Zero-JSON 6-table spidx_* B-tree schema.
func RunMigrations(ctx context.Context, db *sql.DB, mode JournalMode) error {
	if db == nil {
		return nil
	}
	for i, pragma := range PragmaStatements(mode) {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("sqlite pragma[%d]: %w", i, err)
		}
	}
	for i, stmt := range SchemaDDL() {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite migration[%d]: %w", i, err)
		}
	}
	return nil
}

// DuckDBAttachSQL returns the DuckDB SQL statements that attach a local Flint SQLite WAL database
// file using DuckDB's built-in sqlite_scanner extension and expose vectorized analytical views
// without requiring CGO inside flintd.
func DuckDBAttachSQL(sqlitePath string) []string {
	cleaned := strings.TrimSpace(sqlitePath)
	if cleaned == "" {
		cleaned = "flint.db"
	}
	escaped := strings.ReplaceAll(cleaned, "'", "''")
	return []string{
		"INSTALL sqlite;",
		"LOAD sqlite;",
		fmt.Sprintf("ATTACH '%s' AS flint_sqlite (TYPE SQLITE, READ_ONLY TRUE);", escaped),
		`CREATE OR REPLACE VIEW v_fhir_cohort_counts AS
SELECT tenant_id, res_type, COUNT(*) AS active_count
FROM flint_sqlite.fhir_resource
WHERE is_deleted = 0
GROUP BY tenant_id, res_type;`,
		`CREATE OR REPLACE VIEW v_fhir_observation_stats AS
SELECT q.tenant_id,
       t.sp_system AS code_system,
       t.sp_value  AS code,
       q.sp_code   AS unit_code,
       COUNT(*)    AS obs_count,
       AVG(q.sp_value) AS avg_val,
       MIN(q.sp_value) AS min_val,
       MAX(q.sp_value) AS max_val,
       SUM(q.sp_value) AS sum_val,
       MEDIAN(q.sp_value) AS median_val,
       STDDEV_SAMP(q.sp_value) AS stddev_val
FROM flint_sqlite.spidx_quantity q
JOIN flint_sqlite.fhir_resource r
  ON r.tenant_id = q.tenant_id AND r.res_type = q.res_type AND r.res_id = q.res_id AND r.is_deleted = 0
LEFT JOIN flint_sqlite.spidx_token t
  ON t.tenant_id = q.tenant_id AND t.res_type = q.res_type AND t.res_id = q.res_id AND t.sp_name = 'code'
WHERE q.res_type = 'Observation' AND q.sp_name = 'value-quantity'
GROUP BY q.tenant_id, t.sp_system, t.sp_value, q.sp_code;`,
	}
}
