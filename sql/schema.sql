-- Flint FHIR Server — Static DDL
-- Apply per tenant schema: CREATE SCHEMA tenant_{name}; SET search_path = tenant_{name};
-- 8 tables. For ANY number of FHIR resources. Zero schema changes when adding resources.

-- 1. Resource table (source of truth)
CREATE TABLE resource (
    res_type        TEXT        NOT NULL,
    res_id          TEXT        NOT NULL,
    version_id      TEXT        NOT NULL,
    last_updated    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resource_proto  BYTEA       NOT NULL,
    PRIMARY KEY (res_type, res_id)
);

-- 2. Token index (identifiers, codes, booleans)
CREATE TABLE spidx_token (
    res_type    TEXT NOT NULL,
    res_id      TEXT NOT NULL,
    sp_name     TEXT NOT NULL,
    sp_system   TEXT,
    sp_value    TEXT
);
CREATE INDEX idx_spidx_token ON spidx_token (res_type, sp_name, sp_system, sp_value);
CREATE INDEX idx_spidx_token_res ON spidx_token (res_type, res_id);

-- 3. String index (names, addresses)
CREATE TABLE spidx_string (
    res_type        TEXT NOT NULL,
    res_id          TEXT NOT NULL,
    sp_name         TEXT NOT NULL,
    sp_value_exact  TEXT,
    sp_value_norm   TEXT
);
CREATE INDEX idx_spidx_string ON spidx_string (res_type, sp_name, sp_value_norm);
CREATE INDEX idx_spidx_string_res ON spidx_string (res_type, res_id);

-- 4. Date index (dates, periods)
CREATE TABLE spidx_date (
    res_type    TEXT        NOT NULL,
    res_id      TEXT        NOT NULL,
    sp_name     TEXT        NOT NULL,
    sp_low      TIMESTAMPTZ,
    sp_high     TIMESTAMPTZ
);
CREATE INDEX idx_spidx_date ON spidx_date (res_type, sp_name, sp_low, sp_high);
CREATE INDEX idx_spidx_date_res ON spidx_date (res_type, res_id);

-- 5. Quantity index
CREATE TABLE spidx_quantity (
    res_type    TEXT    NOT NULL,
    res_id      TEXT    NOT NULL,
    sp_name     TEXT    NOT NULL,
    sp_value    NUMERIC,
    sp_system   TEXT,
    sp_code     TEXT
);
CREATE INDEX idx_spidx_quantity ON spidx_quantity (res_type, sp_name, sp_value);
CREATE INDEX idx_spidx_quantity_res ON spidx_quantity (res_type, res_id);

-- 6. Reference index
CREATE TABLE spidx_reference (
    res_type        TEXT NOT NULL,
    res_id          TEXT NOT NULL,
    sp_name         TEXT NOT NULL,
    target_type     TEXT,
    target_id       TEXT
);
CREATE INDEX idx_spidx_ref ON spidx_reference (res_type, sp_name, target_type, target_id);
CREATE INDEX idx_spidx_ref_rev ON spidx_reference (target_type, target_id);
CREATE INDEX idx_spidx_ref_res ON spidx_reference (res_type, res_id);

-- 7. URI index
CREATE TABLE spidx_uri (
    res_type    TEXT NOT NULL,
    res_id      TEXT NOT NULL,
    sp_name     TEXT NOT NULL,
    sp_value    TEXT
);
CREATE INDEX idx_spidx_uri ON spidx_uri (res_type, sp_name, sp_value);
CREATE INDEX idx_spidx_uri_res ON spidx_uri (res_type, res_id);

-- 8. Param-present index (for :missing modifier)
CREATE TABLE spidx_present (
    res_type    TEXT    NOT NULL,
    res_id      TEXT    NOT NULL,
    sp_name     TEXT    NOT NULL,
    present     BOOLEAN NOT NULL
);
CREATE INDEX idx_spidx_present ON spidx_present (res_type, sp_name, present);
CREATE INDEX idx_spidx_present_res ON spidx_present (res_type, res_id);

-- Autovacuum tuning for spidx_* tables (DELETE+INSERT pattern creates dead tuples)
ALTER TABLE spidx_token SET (autovacuum_vacuum_scale_factor = 0.01, autovacuum_analyze_scale_factor = 0.01);
ALTER TABLE spidx_string SET (autovacuum_vacuum_scale_factor = 0.01, autovacuum_analyze_scale_factor = 0.01);
ALTER TABLE spidx_date SET (autovacuum_vacuum_scale_factor = 0.01, autovacuum_analyze_scale_factor = 0.01);
ALTER TABLE spidx_quantity SET (autovacuum_vacuum_scale_factor = 0.01, autovacuum_analyze_scale_factor = 0.01);
ALTER TABLE spidx_reference SET (autovacuum_vacuum_scale_factor = 0.01, autovacuum_analyze_scale_factor = 0.01);
ALTER TABLE spidx_uri SET (autovacuum_vacuum_scale_factor = 0.01, autovacuum_analyze_scale_factor = 0.01);
ALTER TABLE spidx_present SET (autovacuum_vacuum_scale_factor = 0.01, autovacuum_analyze_scale_factor = 0.01);
