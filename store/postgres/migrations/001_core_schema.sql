-- Flint FHIR Server: Postgres Operational Store
-- Migration 001: Core schema for FHIR resources and HAPI-style search indexes
--
-- Design decisions:
--   - No JSONB anywhere (user constraint)
--   - Resource blob stored as protobuf bytes (not JSON)
--   - Search indexes are denormalized HAPI-style tables (spidx_*)
--   - Multi-tenant via tenant_id column on all tables
--   - Idempotent upserts via ON CONFLICT
--   - Search indexes are deleted + re-inserted on each resource update

-- ============================================================================
-- Resource table: stores the proto-encoded FHIR resource blob
-- ============================================================================
CREATE TABLE IF NOT EXISTS fhir_resource (
    tenant_id       TEXT        NOT NULL,
    res_type        TEXT        NOT NULL,   -- e.g. 'Patient', 'Encounter'
    res_id          TEXT        NOT NULL,   -- FHIR resource ID
    res_version     INTEGER     NOT NULL DEFAULT 1,
    resource_proto  BYTEA       NOT NULL,   -- proto-encoded FHIR resource
    last_updated    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    is_deleted      BOOLEAN     NOT NULL DEFAULT FALSE,
    idempotency_key TEXT,                   -- Bundle ID for dedup
    PRIMARY KEY (tenant_id, res_type, res_id)
);

-- Index for listing resources by type
CREATE INDEX IF NOT EXISTS idx_fhir_resource_type
    ON fhir_resource (tenant_id, res_type, last_updated DESC);

-- Index for idempotency dedup
CREATE INDEX IF NOT EXISTS idx_fhir_resource_idempotency
    ON fhir_resource (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- ============================================================================
-- Search index tables: HAPI-style spidx_* tables
-- One row per extracted search parameter value per resource
-- ============================================================================

-- spidx_string: string search parameters (family, given, address, etc.)
CREATE TABLE IF NOT EXISTS spidx_string (
    tenant_id   TEXT    NOT NULL,
    res_type    TEXT    NOT NULL,
    res_id      TEXT    NOT NULL,
    sp_name     TEXT    NOT NULL,    -- search param name (e.g. 'family')
    sp_value    TEXT    NOT NULL,    -- lowercased value
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, sp_value)
);

-- Covering index for string search: exact and prefix match
CREATE INDEX IF NOT EXISTS idx_spidx_string_search
    ON spidx_string (tenant_id, res_type, sp_name, sp_value);

-- spidx_token: token search parameters (identifier, code, boolean, etc.)
CREATE TABLE IF NOT EXISTS spidx_token (
    tenant_id   TEXT    NOT NULL,
    res_type    TEXT    NOT NULL,
    res_id      TEXT    NOT NULL,
    sp_name     TEXT    NOT NULL,    -- search param name (e.g. 'identifier')
    sp_system   TEXT    NOT NULL DEFAULT '',  -- system URI (e.g. 'http://hl7.org/fhir/sid/mrn')
    sp_value    TEXT    NOT NULL,    -- value (e.g. 'MRN-456')
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, sp_system, sp_value)
);

-- Covering index for token search: system|value
CREATE INDEX IF NOT EXISTS idx_spidx_token_search
    ON spidx_token (tenant_id, res_type, sp_name, sp_system, sp_value);

-- Value-only token search (no system)
CREATE INDEX IF NOT EXISTS idx_spidx_token_value
    ON spidx_token (tenant_id, res_type, sp_name, sp_value);

-- spidx_date: date/dateTime search parameters (birthdate, etc.)
CREATE TABLE IF NOT EXISTS spidx_date (
    tenant_id   TEXT        NOT NULL,
    res_type    TEXT        NOT NULL,
    res_id      TEXT        NOT NULL,
    sp_name     TEXT        NOT NULL,
    sp_low      TIMESTAMPTZ NOT NULL,   -- lower bound of the date range
    sp_high     TIMESTAMPTZ NOT NULL,   -- upper bound of the date range
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, sp_low)
);

-- Date range search: supports eq, lt, gt, ge, le
CREATE INDEX IF NOT EXISTS idx_spidx_date_search
    ON spidx_date (tenant_id, res_type, sp_name, sp_low, sp_high);

-- spidx_reference: reference search parameters (subject, organization, etc.)
CREATE TABLE IF NOT EXISTS spidx_reference (
    tenant_id    TEXT   NOT NULL,
    res_type     TEXT   NOT NULL,
    res_id       TEXT   NOT NULL,
    sp_name      TEXT   NOT NULL,
    target_type  TEXT   NOT NULL,    -- e.g. 'Organization'
    target_id    TEXT   NOT NULL,    -- e.g. 'org-789'
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, target_type, target_id)
);

-- Forward reference search: find resources that reference a target
CREATE INDEX IF NOT EXISTS idx_spidx_reference_search
    ON spidx_reference (tenant_id, res_type, sp_name, target_type, target_id);

-- Reverse reference search: find all resources referencing a given resource
CREATE INDEX IF NOT EXISTS idx_spidx_reference_reverse
    ON spidx_reference (tenant_id, target_type, target_id, res_type);

-- spidx_quantity: quantity search parameters (value-quantity, dosage, etc.)
CREATE TABLE IF NOT EXISTS spidx_quantity (
    tenant_id   TEXT            NOT NULL,
    res_type    TEXT            NOT NULL,
    res_id      TEXT            NOT NULL,
    sp_name     TEXT            NOT NULL,
    sp_system   TEXT            NOT NULL DEFAULT '',
    sp_code     TEXT            NOT NULL DEFAULT '',
    sp_value    DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (tenant_id, res_type, res_id, sp_name, sp_system, sp_code, sp_value)
);

CREATE INDEX IF NOT EXISTS idx_spidx_quantity_search
    ON spidx_quantity (tenant_id, res_type, sp_name, sp_system, sp_code, sp_value);

-- spidx_uri: URI search parameters (url, etc.)
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
