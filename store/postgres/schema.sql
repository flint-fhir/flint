-- Flint FHIR Server: Declarative Postgres Operational Store Schema (Desired State)
--
-- Used by Atlas (`atlas migrate diff --env local`) to generate versioned SQL
-- migrations in store/postgres/migrations/ guarded by atlas.sum (ADR-9).
--
-- Design decisions:
--   - Zero JSONB columns anywhere
--   - Resource blob stored as lossless protobuf bytes (resource_proto BYTEA)
--   - Immutable append-only version history in fhir_resource_history for vread & _history
--   - Denormalized HAPI-style search index tables (spidx_*)

CREATE TABLE IF NOT EXISTS fhir_resource (
    tenant_id       TEXT        NOT NULL,
    res_type        TEXT        NOT NULL,
    res_id          TEXT        NOT NULL,
    res_version     INTEGER     NOT NULL DEFAULT 1,
    resource_proto  BYTEA       NOT NULL,
    last_updated    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    is_deleted      BOOLEAN     NOT NULL DEFAULT FALSE,
    idempotency_key TEXT,
    PRIMARY KEY (tenant_id, res_type, res_id)
);

CREATE INDEX IF NOT EXISTS idx_fhir_resource_type
    ON fhir_resource (tenant_id, res_type, last_updated DESC);

CREATE INDEX IF NOT EXISTS idx_fhir_resource_idempotency
    ON fhir_resource (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS fhir_resource_history (
    tenant_id       TEXT        NOT NULL,
    res_type        TEXT        NOT NULL,
    res_id          TEXT        NOT NULL,
    res_version     INTEGER     NOT NULL,
    resource_proto  BYTEA,
    last_updated    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    is_deleted      BOOLEAN     NOT NULL DEFAULT FALSE,
    PRIMARY KEY (tenant_id, res_type, res_id, res_version)
);

CREATE INDEX IF NOT EXISTS idx_fhir_resource_history_lookup
    ON fhir_resource_history (tenant_id, res_type, res_id, res_version DESC);

CREATE TABLE IF NOT EXISTS spidx_string (
    tenant_id   TEXT    NOT NULL,
    res_type    TEXT    NOT NULL,
    res_id      TEXT    NOT NULL,
    sp_name     TEXT    NOT NULL,
    sp_value    TEXT    NOT NULL,
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, sp_value)
);

CREATE INDEX IF NOT EXISTS idx_spidx_string_search
    ON spidx_string (tenant_id, res_type, sp_name, sp_value);

CREATE TABLE IF NOT EXISTS spidx_token (
    tenant_id   TEXT    NOT NULL,
    res_type    TEXT    NOT NULL,
    res_id      TEXT    NOT NULL,
    sp_name     TEXT    NOT NULL,
    sp_system   TEXT    NOT NULL DEFAULT '',
    sp_value    TEXT    NOT NULL,
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, sp_system, sp_value)
);

CREATE INDEX IF NOT EXISTS idx_spidx_token_search
    ON spidx_token (tenant_id, res_type, sp_name, sp_system, sp_value);

CREATE INDEX IF NOT EXISTS idx_spidx_token_value
    ON spidx_token (tenant_id, res_type, sp_name, sp_value);

CREATE TABLE IF NOT EXISTS spidx_date (
    tenant_id   TEXT        NOT NULL,
    res_type    TEXT        NOT NULL,
    res_id      TEXT        NOT NULL,
    sp_name     TEXT        NOT NULL,
    sp_low      TIMESTAMPTZ NOT NULL,
    sp_high     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, sp_low)
);

CREATE INDEX IF NOT EXISTS idx_spidx_date_search
    ON spidx_date (tenant_id, res_type, sp_name, sp_low, sp_high);

CREATE TABLE IF NOT EXISTS spidx_reference (
    tenant_id    TEXT   NOT NULL,
    res_type     TEXT   NOT NULL,
    res_id       TEXT   NOT NULL,
    sp_name      TEXT   NOT NULL,
    target_type  TEXT   NOT NULL,
    target_id    TEXT   NOT NULL,
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, target_type, target_id)
);

CREATE INDEX IF NOT EXISTS idx_spidx_reference_search
    ON spidx_reference (tenant_id, res_type, sp_name, target_type, target_id);

CREATE INDEX IF NOT EXISTS idx_spidx_reference_reverse
    ON spidx_reference (tenant_id, target_type, target_id, res_type);

CREATE TABLE IF NOT EXISTS spidx_quantity (
    tenant_id   TEXT             NOT NULL,
    res_type    TEXT             NOT NULL,
    res_id      TEXT             NOT NULL,
    sp_name     TEXT             NOT NULL,
    sp_system   TEXT             NOT NULL DEFAULT '',
    sp_code     TEXT             NOT NULL DEFAULT '',
    sp_value    DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, sp_system, sp_code, sp_value)
);

CREATE INDEX IF NOT EXISTS idx_spidx_quantity_search
    ON spidx_quantity (tenant_id, res_type, sp_name, sp_system, sp_code, sp_value);

CREATE TABLE IF NOT EXISTS spidx_uri (
    tenant_id   TEXT    NOT NULL,
    res_type    TEXT    NOT NULL,
    res_id      TEXT    NOT NULL,
    sp_name     TEXT    NOT NULL,
    sp_value    TEXT    NOT NULL,
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, sp_value)
);

CREATE INDEX IF NOT EXISTS idx_spidx_uri_search
    ON spidx_uri (tenant_id, res_type, sp_name, sp_value);
