// Package doris implements the Zero-JSON Apache Doris FHIR R4 operational and
// analytical store for Flint.
//
// Unlike JSON/VARIANT document stores or 6-table relational index fan-out,
// package doris pairs lossless Google FHIR Protocol Buffer bytes (resource_proto)
// with strongly-typed columnar scalars and native ARRAY<VARCHAR> columns backed
// by Doris Lucene Inverted Indexes (USING INVERTED) on Unique Key Merge-on-Write (MoW) tables.
package doris

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/flint-fhir/flint/store"
)

// GroupCommitMode configures Apache Doris Group Commit behavior for high-frequency point writes.
type GroupCommitMode string

const (
	// GroupCommitSync batches concurrent writes on the Doris BE and returns once committed.
	GroupCommitSync GroupCommitMode = "sync_mode"
	// GroupCommitAsync buffers writes in Doris WAL and flushes asynchronously for maximum throughput.
	GroupCommitAsync GroupCommitMode = "async_mode"
	// GroupCommitOff disables Doris group commit.
	GroupCommitOff GroupCommitMode = "off_mode"
)

// ParseGroupCommitMode parses a Doris group_commit configuration string.
func ParseGroupCommitMode(raw string) (GroupCommitMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "sync", "sync_mode":
		return GroupCommitSync, nil
	case "async", "async_mode":
		return GroupCommitAsync, nil
	case "off", "off_mode", "false", "disabled", "none":
		return GroupCommitOff, nil
	default:
		return "", fmt.Errorf("invalid doris group commit mode %q (valid: sync_mode, async_mode, off_mode)", raw)
	}
}

// DorisResourceRow is the Zero-JSON, strongly-typed columnar representation of a FHIR R4 resource
// in Apache Doris. Repeating search parameters are stored inline as deduplicated ARRAY<VARCHAR>
// columns indexed with USING INVERTED, eliminating multi-table spidx_* JOINs.
type DorisResourceRow struct {
	TenantID       string
	ResType        string
	ResID          string
	ResVersion     int
	IsDeleted      bool
	LastUpdated    time.Time
	IdempotencyKey string

	// Canonical Google FHIR Protocol Buffer wire bytes (zero JSON at rest)
	ResourceProto    []byte
	ResourceProtoHex string

	// Inline ARRAY<VARCHAR> columns for Doris Inverted Index bitmap intersection
	StringTokens    []string // formatted as "<sp_name>=<lowercase_value>"
	CodeTokens      []string // formatted as "<sp_name>=<system>|<code_or_value>" and "<sp_name>=|<code_or_value>"
	ReferenceTokens []string // formatted as "<sp_name>=<target_type>/<target_id>" and "patient=<id>"
	URITokens       []string // formatted as "<sp_name>=<uri>"

	// Primary clinical date & quantity scalars for vectorized range & $stats pushdown
	PrimaryDateLow  *time.Time
	PrimaryDateHigh *time.Time
	PrimaryQuantity *float64
	PrimaryUnitCode string
	PrimaryUnitSys  string
	PrimaryCode     string
	PrimaryCodeSys  string
	PatientID       string
	EncounterID     string
}

// ExtractRow converts a store.ResourceInput and its proto2type-extracted SearchIndexes
// into a strongly-typed DorisResourceRow without any JSON serialization.
func ExtractRow(input store.ResourceInput, version int, lastUpdated time.Time, isDeleted bool) DorisResourceRow {
	if lastUpdated.IsZero() {
		lastUpdated = time.Now().UTC()
	}
	if version <= 0 {
		version = 1
	}
	row := DorisResourceRow{
		TenantID:         input.TenantID,
		ResType:          input.ResType,
		ResID:            input.ResID,
		ResVersion:       version,
		IsDeleted:        isDeleted,
		LastUpdated:      lastUpdated.UTC(),
		IdempotencyKey:   input.IdempotencyKey,
		ResourceProto:    append([]byte(nil), input.ResourceProto...),
		ResourceProtoHex: hex.EncodeToString(input.ResourceProto),
	}
	if strings.EqualFold(input.ResType, "Patient") {
		row.PatientID = input.ResID
	}
	if strings.EqualFold(input.ResType, "Encounter") {
		row.EncounterID = input.ResID
	}

	idx := input.SearchIndexes
	if idx == nil || isDeleted {
		return row
	}

	seenStr := make(map[string]bool)
	for _, s := range idx.Strings {
		val := strings.ToLower(strings.TrimSpace(s.SpValue))
		if val == "" {
			continue
		}
		tok := s.SpName + "=" + val
		if !seenStr[tok] {
			seenStr[tok] = true
			row.StringTokens = append(row.StringTokens, tok)
		}
	}
	sort.Strings(row.StringTokens)

	seenCode := make(map[string]bool)
	for _, t := range idx.Tokens {
		full := t.SpName + "=" + t.SpSystem + "|" + t.SpValue
		if !seenCode[full] {
			seenCode[full] = true
			row.CodeTokens = append(row.CodeTokens, full)
		}
		bare := t.SpName + "=|" + t.SpValue
		if !seenCode[bare] {
			seenCode[bare] = true
			row.CodeTokens = append(row.CodeTokens, bare)
		}
		if row.PrimaryCode == "" && (t.SpName == "code" || t.SpName == "vaccine-code") && t.SpValue != "" {
			row.PrimaryCode = t.SpValue
			row.PrimaryCodeSys = t.SpSystem
		}
	}
	sort.Strings(row.CodeTokens)

	seenRef := make(map[string]bool)
	for _, r := range idx.References {
		refVal := r.TargetID
		if r.TargetType != "" {
			refVal = r.TargetType + "/" + r.TargetID
		}
		entry := r.SpName + "=" + refVal
		if !seenRef[entry] {
			seenRef[entry] = true
			row.ReferenceTokens = append(row.ReferenceTokens, entry)
		}
		bare := r.SpName + "=" + r.TargetID
		if !seenRef[bare] {
			seenRef[bare] = true
			row.ReferenceTokens = append(row.ReferenceTokens, bare)
		}
		if row.PatientID == "" && (strings.EqualFold(r.TargetType, "Patient") || r.SpName == "patient" || r.SpName == "beneficiary") {
			row.PatientID = r.TargetID
		}
		if row.EncounterID == "" && (strings.EqualFold(r.TargetType, "Encounter") || r.SpName == "encounter" || r.SpName == "context") {
			row.EncounterID = r.TargetID
		}
	}
	sort.Strings(row.ReferenceTokens)

	seenURI := make(map[string]bool)
	for _, u := range idx.URIs {
		if u.SpValue == "" {
			continue
		}
		entry := u.SpName + "=" + u.SpValue
		if !seenURI[entry] {
			seenURI[entry] = true
			row.URITokens = append(row.URITokens, entry)
		}
	}
	sort.Strings(row.URITokens)

	for _, d := range idx.Dates {
		if row.PrimaryDateLow == nil || (d.SpName == "date" || d.SpName == "effective" || d.SpName == "birthdate" || d.SpName == "authoredon") {
			low := d.SpLow.UTC()
			high := d.SpHigh.UTC()
			row.PrimaryDateLow = &low
			row.PrimaryDateHigh = &high
			if d.SpName == "date" || d.SpName == "effective" {
				break
			}
		}
	}

	for _, q := range idx.Quantities {
		if row.PrimaryQuantity == nil || q.SpName == "value-quantity" {
			val := q.SpValue
			row.PrimaryQuantity = &val
			row.PrimaryUnitCode = q.SpCode
			row.PrimaryUnitSys = q.SpSystem
			if q.SpName == "value-quantity" {
				break
			}
		}
	}

	return row
}

// USCoreResourceTypes lists the 13 core US Core FHIR R4 resource types defined in buf.gen.yaml
// that receive both dedicated physical LIST partitions in fhir_resource and strongly-typed
// zero-blob projection tables (fhir_patient, fhir_observation, etc.) in Apache Doris.
var USCoreResourceTypes = []string{
	"Patient",
	"Encounter",
	"Observation",
	"Condition",
	"Practitioner",
	"MedicationRequest",
	"AllergyIntolerance",
	"Procedure",
	"DiagnosticReport",
	"Immunization",
	"Organization",
	"Specimen",
	"Location",
}

var usCoreTableNames = map[string]string{
	"patient":            "fhir_patient",
	"encounter":          "fhir_encounter",
	"observation":        "fhir_observation",
	"condition":          "fhir_condition",
	"practitioner":       "fhir_practitioner",
	"medicationrequest":  "fhir_medication_request",
	"allergyintolerance": "fhir_allergy_intolerance",
	"procedure":          "fhir_procedure",
	"diagnosticreport":   "fhir_diagnostic_report",
	"immunization":       "fhir_immunization",
	"organization":       "fhir_organization",
	"specimen":           "fhir_specimen",
	"location":           "fhir_location",
}

// CoreResourceTableName returns the strongly-typed Apache Doris table name (e.g. "fhir_observation")
// for a US Core FHIR R4 resource type, or ("", false) for long-tail resource types.
func CoreResourceTableName(resType string) (string, bool) {
	tbl, ok := usCoreTableNames[strings.ToLower(strings.TrimSpace(resType))]
	return tbl, ok
}

// GenerateResourceTableDDL generates the Zero-JSON Apache Doris Unique Key Merge-on-Write (MoW)
// table DDL for a specific FHIR R4 resource type (or the PARTITION BY LIST (res_type) unified
// fhir_resource table when resType == "").
func GenerateResourceTableDDL(resType string) string {
	tableName := "fhir_resource"
	partitionClause := `
PARTITION BY LIST (res_type) (
    PARTITION p_patient             VALUES IN ("Patient"),
    PARTITION p_encounter           VALUES IN ("Encounter"),
    PARTITION p_observation         VALUES IN ("Observation"),
    PARTITION p_condition           VALUES IN ("Condition"),
    PARTITION p_practitioner        VALUES IN ("Practitioner"),
    PARTITION p_medication_request  VALUES IN ("MedicationRequest"),
    PARTITION p_allergy_intolerance VALUES IN ("AllergyIntolerance"),
    PARTITION p_procedure           VALUES IN ("Procedure"),
    PARTITION p_diagnostic_report   VALUES IN ("DiagnosticReport"),
    PARTITION p_immunization        VALUES IN ("Immunization"),
    PARTITION p_organization        VALUES IN ("Organization"),
    PARTITION p_specimen            VALUES IN ("Specimen"),
    PARTITION p_location            VALUES IN ("Location")
)`
	if cleaned := strings.ToLower(strings.TrimSpace(resType)); cleaned != "" {
		if tbl, ok := CoreResourceTableName(cleaned); ok {
			tableName = tbl
		} else {
			tableName = "fhir_" + cleaned
		}
		partitionClause = ""
	}
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
    tenant_id          VARCHAR(64)    NOT NULL,
    res_type           VARCHAR(64)    NOT NULL,
    res_id             VARCHAR(64)    NOT NULL,
    res_version        INT            NOT NULL DEFAULT 1,
    is_deleted         BOOLEAN        NOT NULL DEFAULT FALSE,
    last_updated       DATETIME(6)    NOT NULL,
    idempotency_key    VARCHAR(128)   NULL,
    patient_id         VARCHAR(64)    NULL,
    encounter_id       VARCHAR(64)    NULL,
    primary_code_sys   VARCHAR(256)   NULL,
    primary_code       VARCHAR(128)   NULL,
    primary_date_low   DATETIME(6)    NULL,
    primary_date_high  DATETIME(6)    NULL,
    primary_quantity   DOUBLE         NULL,
    primary_unit_sys   VARCHAR(128)   NULL,
    primary_unit_code  VARCHAR(64)    NULL,
    string_tokens      ARRAY<VARCHAR(512)> NULL,
    code_tokens        ARRAY<VARCHAR(512)> NULL,
    reference_tokens   ARRAY<VARCHAR(512)> NULL,
    uri_tokens         ARRAY<VARCHAR(512)> NULL,
    resource_proto_hex STRING         NOT NULL,
    INDEX idx_patient_id (patient_id) USING INVERTED,
    INDEX idx_encounter_id (encounter_id) USING INVERTED,
    INDEX idx_primary_code (primary_code) USING INVERTED,
    INDEX idx_string_tokens (string_tokens) USING INVERTED,
    INDEX idx_code_tokens (code_tokens) USING INVERTED,
    INDEX idx_reference_tokens (reference_tokens) USING INVERTED,
    INDEX idx_uri_tokens (uri_tokens) USING INVERTED
)
UNIQUE KEY(tenant_id, res_type, res_id)%s
DISTRIBUTED BY HASH(tenant_id, res_id) BUCKETS 16
PROPERTIES (
    "enable_unique_key_merge_on_write" = "true",
    "function_column.sequence_col" = "res_version",
    "store_row_column" = "true",
    "replication_num" = "1"
);`, tableName, partitionClause)
}

// CoreResourceTableDDL generates the strongly-typed, zero-blob Apache Doris Unique Key MoW table DDL
// for one of the 13 US Core clinical resource types (Option C). Because fhir_resource already stores
// resource_proto_hex, these 13 tables omit the proto payload and store only typed clinical scalars
// and inverted-index arrays (~15 bytes/row compressed) for vectorized SQLMesh, CQL, and $stats queries.
func CoreResourceTableDDL(resType string) (string, bool) {
	tableName, ok := CoreResourceTableName(resType)
	if !ok {
		return "", false
	}
	buckets := 8
	if strings.EqualFold(resType, "Observation") {
		buckets = 16
	}
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
    tenant_id          VARCHAR(64)    NOT NULL,
    res_id             VARCHAR(64)    NOT NULL,
    res_version        INT            NOT NULL DEFAULT 1,
    is_deleted         BOOLEAN        NOT NULL DEFAULT FALSE,
    last_updated       DATETIME(6)    NOT NULL,
    patient_id         VARCHAR(64)    NULL,
    encounter_id       VARCHAR(64)    NULL,
    code_system        VARCHAR(256)   NULL,
    code               VARCHAR(128)   NULL,
    effective_start    DATETIME(6)    NULL,
    effective_end      DATETIME(6)    NULL,
    value_quantity     DOUBLE         NULL,
    value_unit_sys     VARCHAR(128)   NULL,
    value_unit_code    VARCHAR(64)    NULL,
    string_tokens      ARRAY<VARCHAR(512)> NULL,
    code_tokens        ARRAY<VARCHAR(512)> NULL,
    reference_tokens   ARRAY<VARCHAR(512)> NULL,
    uri_tokens         ARRAY<VARCHAR(512)> NULL,
    INDEX idx_patient_id (patient_id) USING INVERTED,
    INDEX idx_encounter_id (encounter_id) USING INVERTED,
    INDEX idx_code (code) USING INVERTED,
    INDEX idx_string_tokens (string_tokens) USING INVERTED,
    INDEX idx_code_tokens (code_tokens) USING INVERTED,
    INDEX idx_reference_tokens (reference_tokens) USING INVERTED
)
UNIQUE KEY(tenant_id, res_id)
DISTRIBUTED BY HASH(tenant_id, res_id) BUCKETS %d
PROPERTIES (
    "enable_unique_key_merge_on_write" = "true",
    "function_column.sequence_col" = "res_version",
    "replication_num" = "1"
);`, tableName, buckets), true
}

// CoreResourceTableDDLs returns the 13 strongly-typed US Core Apache Doris table DDL statements.
func CoreResourceTableDDLs() []string {
	ddls := make([]string, 0, len(USCoreResourceTypes))
	for _, rt := range USCoreResourceTypes {
		if ddl, ok := CoreResourceTableDDL(rt); ok {
			ddls = append(ddls, ddl)
		}
	}
	return ddls
}

// ProjectCoreResourceUpsertSQL builds the INSERT statement into the strongly-typed US Core Doris table
// (e.g. fhir_patient, fhir_observation) when row.ResType is one of the 13 US Core resource types.
func ProjectCoreResourceUpsertSQL(row DorisResourceRow) (string, []any, bool) {
	tableName, ok := CoreResourceTableName(row.ResType)
	if !ok {
		return "", nil, false
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s (tenant_id, res_id, res_version, is_deleted, last_updated, patient_id, encounter_id, code_system, code, effective_start, effective_end, value_quantity, value_unit_sys, value_unit_code) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tableName,
	)
	args := []any{
		row.TenantID, row.ResID, row.ResVersion, row.IsDeleted, row.LastUpdated,
		row.PatientID, row.EncounterID, row.PrimaryCodeSys, row.PrimaryCode,
		row.PrimaryDateLow, row.PrimaryDateHigh, row.PrimaryQuantity, row.PrimaryUnitSys, row.PrimaryUnitCode,
	}
	return stmt, args, true
}

// ProjectCoreResourceDeleteSQL builds the Merge-on-Write tombstone INSERT statement for a US Core Doris table.
func ProjectCoreResourceDeleteSQL(tenantID, resType, resID string, version int, now time.Time) (string, []any, bool) {
	tableName, ok := CoreResourceTableName(resType)
	if !ok {
		return "", nil, false
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s (tenant_id, res_id, res_version, is_deleted, last_updated) VALUES (?, ?, ?, TRUE, ?)`,
		tableName,
	)
	return stmt, []any{tenantID, resID, version, now}, true
}

// SchemaDDL returns all Apache Doris Unique Key Merge-on-Write DDL statements required
// to initialize the Flint operational and analytical store (Option C: LIST-partitioned
// fhir_resource + 13 strongly-typed US Core tables + history, audit, and export tables).
func SchemaDDL() []string {
	stmts := []string{
		GenerateResourceTableDDL(""),
		`CREATE TABLE IF NOT EXISTS fhir_resource_history (
    tenant_id          VARCHAR(64)  NOT NULL,
    res_type           VARCHAR(64)  NOT NULL,
    res_id             VARCHAR(64)  NOT NULL,
    res_version        INT          NOT NULL,
    is_deleted         BOOLEAN      NOT NULL DEFAULT FALSE,
    last_updated       DATETIME(6)  NOT NULL,
    resource_proto_hex STRING       NULL
)
UNIQUE KEY(tenant_id, res_type, res_id, res_version)
DISTRIBUTED BY HASH(tenant_id, res_id) BUCKETS 16
PROPERTIES (
    "enable_unique_key_merge_on_write" = "true",
    "store_row_column" = "true",
    "replication_num" = "1"
);`,
		`CREATE TABLE IF NOT EXISTS fhir_audit_event (
    tenant_id      VARCHAR(64)   NOT NULL,
    audit_id       VARCHAR(64)   NOT NULL,
    recorded       DATETIME(6)   NOT NULL,
    action         VARCHAR(16)   NOT NULL,
    subtype_code   VARCHAR(64)   NOT NULL,
    outcome        VARCHAR(16)   NOT NULL,
    outcome_desc   VARCHAR(1024) NULL,
    http_method    VARCHAR(16)   NULL,
    http_status    INT           NULL,
    request_uri    VARCHAR(1024) NULL,
    agent_subject  VARCHAR(256)  NULL,
    agent_patient  VARCHAR(128)  NULL,
    client_ip      VARCHAR(128)  NULL,
    entity_type    VARCHAR(64)   NULL,
    entity_id      VARCHAR(128)  NULL,
    entity_version VARCHAR(32)   NULL,
    INDEX idx_audit_action (action) USING INVERTED,
    INDEX idx_audit_entity (entity_type) USING INVERTED,
    INDEX idx_audit_patient (agent_patient) USING INVERTED
)
UNIQUE KEY(tenant_id, audit_id)
DISTRIBUTED BY HASH(tenant_id, audit_id) BUCKETS 8
PROPERTIES (
    "enable_unique_key_merge_on_write" = "true",
    "replication_num" = "1"
);`,
		`CREATE TABLE IF NOT EXISTS fhir_export_job (
    tenant_id   VARCHAR(64)   NOT NULL,
    job_id      VARCHAR(128)  NOT NULL,
    patient_id  VARCHAR(64)   NULL,
    request_url VARCHAR(1024) NULL,
    created_at  DATETIME(6)   NOT NULL,
    types_order STRING        NULL,
    counts      STRING        NULL,
    files_b64   STRING        NULL
)
UNIQUE KEY(tenant_id, job_id)
DISTRIBUTED BY HASH(tenant_id, job_id) BUCKETS 4
PROPERTIES (
    "enable_unique_key_merge_on_write" = "true",
    "replication_num" = "1"
);`,
	}
	return append(stmts, CoreResourceTableDDLs()...)
}

// RunMigrations applies the Apache Doris Unique Key Merge-on-Write schema DDL statements.
func RunMigrations(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	for i, stmt := range SchemaDDL() {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("doris migration[%d]: %w", i, err)
		}
	}
	return nil
}

// BuildInvertedSearchSQL compiles a store.SearchParams into a single-table, zero-join
// Apache Doris SQL query using Inverted Index predicates (array_contains) and columnar filters.
func BuildInvertedSearchSQL(params store.SearchParams) (string, []any) {
	var conds []string
	var args []any

	conds = append(conds, "tenant_id = ?")
	args = append(args, params.TenantID)

	conds = append(conds, "is_deleted = FALSE")

	if params.ResType != "" {
		conds = append(conds, "res_type = ?")
		args = append(args, params.ResType)
	} else if len(params.ResTypes) > 0 {
		placeholders := make([]string, len(params.ResTypes))
		for i, rt := range params.ResTypes {
			placeholders[i] = "?"
			args = append(args, rt)
		}
		conds = append(conds, "res_type IN ("+strings.Join(placeholders, ", ")+")")
	}

	if params.ID != "" {
		var ids []string
		for part := range strings.SplitSeq(params.ID, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				ids = append(ids, trimmed)
			}
		}
		if len(ids) == 1 {
			conds = append(conds, "res_id = ?")
			args = append(args, ids[0])
		} else if len(ids) > 1 {
			ph := make([]string, len(ids))
			for i, id := range ids {
				ph[i] = "?"
				args = append(args, id)
			}
			conds = append(conds, "res_id IN ("+strings.Join(ph, ", ")+")")
		}
	}

	for spName, val := range params.Tokens {
		sys := ""
		code := val
		if parts := strings.SplitN(val, "|", 2); len(parts) == 2 {
			sys, code = parts[0], parts[1]
		}
		conds = append(conds, "array_contains(code_tokens, ?)")
		args = append(args, spName+"="+sys+"|"+code)
	}

	for spName, ref := range params.References {
		conds = append(conds, "array_contains(reference_tokens, ?)")
		args = append(args, spName+"="+ref)
	}

	for spName, strVal := range params.Strings {
		conds = append(conds, "array_contains(string_tokens, ?)")
		args = append(args, spName+"="+strings.ToLower(strings.TrimSpace(strVal)))
	}

	for spName, uriVal := range params.URIs {
		conds = append(conds, "array_contains(uri_tokens, ?)")
		args = append(args, spName+"="+uriVal)
	}

	count := params.Count
	if count <= 0 {
		count = 20
	}
	if count > store.MaxSearchCount {
		count = store.MaxSearchCount
	}
	offset := max(params.Offset, 0)

	query := fmt.Sprintf(
		"SELECT res_type, res_id, res_version, last_updated, resource_proto_hex FROM fhir_resource WHERE %s ORDER BY last_updated DESC, res_id ASC LIMIT %d OFFSET %d",
		strings.Join(conds, " AND "), count, offset,
	)
	return query, args
}
