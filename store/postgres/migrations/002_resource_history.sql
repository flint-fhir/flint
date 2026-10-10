-- Flint FHIR Server: Postgres Operational Store
-- Migration 002: Append-only resource version history table for FHIR vread and _history

CREATE TABLE IF NOT EXISTS fhir_resource_history (
    tenant_id       TEXT        NOT NULL,
    res_type        TEXT        NOT NULL,
    res_id          TEXT        NOT NULL,
    res_version     INTEGER     NOT NULL,
    resource_proto  BYTEA,                  -- Nullable for deletion tombstones (is_deleted = TRUE)
    last_updated    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    is_deleted      BOOLEAN     NOT NULL DEFAULT FALSE,
    PRIMARY KEY (tenant_id, res_type, res_id, res_version)
);

CREATE INDEX IF NOT EXISTS idx_fhir_resource_history_lookup
    ON fhir_resource_history (tenant_id, res_type, res_id, res_version DESC);
