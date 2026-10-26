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

CREATE TABLE IF NOT EXISTS fhir_audit_event (
    tenant_id       TEXT        NOT NULL,
    audit_id        TEXT        NOT NULL,
    recorded        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    action          TEXT        NOT NULL,
    subtype_code    TEXT        NOT NULL,
    outcome         TEXT        NOT NULL,
    outcome_desc    TEXT        NOT NULL DEFAULT '',
    http_method     TEXT        NOT NULL,
    http_status     INTEGER     NOT NULL,
    request_uri     TEXT        NOT NULL,
    agent_subject   TEXT        NOT NULL DEFAULT 'anonymous',
    agent_patient   TEXT        NOT NULL DEFAULT '',
    client_ip       TEXT        NOT NULL DEFAULT '',
    entity_type     TEXT        NOT NULL DEFAULT '',
    entity_id       TEXT        NOT NULL DEFAULT '',
    entity_version  TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, audit_id)
);

CREATE INDEX IF NOT EXISTS idx_fhir_audit_event_recorded
    ON fhir_audit_event (tenant_id, recorded DESC);

CREATE INDEX IF NOT EXISTS idx_fhir_audit_event_entity
    ON fhir_audit_event (tenant_id, entity_type, entity_id, recorded DESC);

CREATE INDEX IF NOT EXISTS idx_fhir_audit_event_agent
    ON fhir_audit_event (tenant_id, agent_subject, recorded DESC);

CREATE TABLE IF NOT EXISTS fhir_oauth_refresh_token (
    token_hash   TEXT        PRIMARY KEY,
    subject      TEXT        NOT NULL,
    patient_id   TEXT        NOT NULL DEFAULT '',
    encounter_id TEXT        NOT NULL DEFAULT '',
    scope        TEXT        NOT NULL DEFAULT '',
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_oauth_refresh_token_expires
    ON fhir_oauth_refresh_token (expires_at);

CREATE TABLE IF NOT EXISTS fhir_oauth_revoked_token (
    token_hash TEXT        PRIMARY KEY,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_oauth_revoked_token_expires
    ON fhir_oauth_revoked_token (expires_at);

CREATE TABLE IF NOT EXISTS fhir_oauth_auth_code (
    code_hash             TEXT        PRIMARY KEY,
    client_id             TEXT        NOT NULL DEFAULT '',
    redirect_uri          TEXT        NOT NULL DEFAULT '',
    scope                 TEXT        NOT NULL DEFAULT '',
    code_challenge        TEXT        NOT NULL DEFAULT '',
    code_challenge_method TEXT        NOT NULL DEFAULT '',
    patient_id            TEXT        NOT NULL DEFAULT '',
    encounter_id          TEXT        NOT NULL DEFAULT '',
    tenant_id             TEXT        NOT NULL DEFAULT '',
    nonce                 TEXT        NOT NULL DEFAULT '',
    expires_at            TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_oauth_auth_code_expires
    ON fhir_oauth_auth_code (expires_at);

CREATE TABLE IF NOT EXISTS fhir_export_job (
    job_id       TEXT        PRIMARY KEY,
    tenant_id    TEXT        NOT NULL,
    patient_id   TEXT        NOT NULL DEFAULT '',
    request_url  TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    types_order  TEXT        NOT NULL DEFAULT '[]',
    counts       TEXT        NOT NULL DEFAULT '{}',
    files_json   TEXT        NOT NULL DEFAULT '{}',
    combined     BYTEA       NOT NULL DEFAULT ''::bytea
);

CREATE INDEX IF NOT EXISTS idx_export_job_tenant_created
    ON fhir_export_job (tenant_id, created_at DESC);

CREATE OR REPLACE FUNCTION provision_tenant(tenant_name TEXT)
RETURNS TEXT AS $$
DECLARE
    sanitized TEXT;
    schema_ident TEXT;
    tbl TEXT;
BEGIN
    sanitized := lower(regexp_replace(trim(tenant_name), '[^a-zA-Z0-9_]+', '_', 'g'));
    sanitized := trim(both '_' from sanitized);
    IF sanitized = '' THEN
        RAISE EXCEPTION 'invalid tenant_name: %', tenant_name;
    END IF;
    schema_ident := 'tenant_' || sanitized;

    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I', schema_ident);

    FOREACH tbl IN ARRAY ARRAY[
        'fhir_resource',
        'fhir_resource_history',
        'fhir_audit_event',
        'fhir_export_job',
        'spidx_string',
        'spidx_token',
        'spidx_date',
        'spidx_reference',
        'spidx_quantity',
        'spidx_uri'
    ]
    LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I.%I (LIKE public.%I INCLUDING ALL)',
            schema_ident, tbl, tbl
        );
    END LOOP;

    RETURN schema_ident;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION create_tenant_schema(tenant_name TEXT)
RETURNS void AS $$
BEGIN
    PERFORM provision_tenant(tenant_name);
END;
$$ LANGUAGE plpgsql;


