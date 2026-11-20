-- =============================================================================
-- Migration 006: Stateless Multi-Replica Clustering & Distributed Trace Columns
-- =============================================================================

ALTER TABLE fhir_audit_event
    ADD COLUMN IF NOT EXISTS trace_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS span_id  TEXT NOT NULL DEFAULT '';

ALTER TABLE fhir_oauth_auth_code
    ADD COLUMN IF NOT EXISTS fhir_user         TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS extra_claims_text TEXT NOT NULL DEFAULT '';

ALTER TABLE fhir_oauth_refresh_token
    ADD COLUMN IF NOT EXISTS grant_id          TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS client_id         TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS tenant_id         TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS nonce             TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS fhir_user         TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS extra_claims_text TEXT NOT NULL DEFAULT '';

ALTER TABLE fhir_export_job
    ADD COLUMN IF NOT EXISTS status           TEXT    NOT NULL DEFAULT 'complete',
    ADD COLUMN IF NOT EXISTS err_msg          TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS output_format    TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS cancel_requested BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS fhir_cluster_secret (
    secret_key   TEXT        PRIMARY KEY,
    secret_bytes BYTEA       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS fhir_cluster_state (
    namespace     TEXT        NOT NULL,
    state_key     TEXT        NOT NULL,
    tenant_id     TEXT        NOT NULL DEFAULT '',
    secondary_key TEXT        NOT NULL DEFAULT '',
    value_blob    BYTEA       NOT NULL DEFAULT ''::bytea,
    version       BIGINT      NOT NULL DEFAULT 1,
    expires_at    TIMESTAMPTZ NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (namespace, state_key)
);

CREATE INDEX IF NOT EXISTS idx_cluster_state_secondary
    ON fhir_cluster_state (namespace, secondary_key)
    WHERE secondary_key <> '';

CREATE INDEX IF NOT EXISTS idx_cluster_state_tenant
    ON fhir_cluster_state (namespace, tenant_id);

CREATE INDEX IF NOT EXISTS idx_cluster_state_expires
    ON fhir_cluster_state (expires_at)
    WHERE expires_at IS NOT NULL;

DO $$
DECLARE
    s RECORD;
BEGIN
    FOR s IN
        SELECT schema_name
        FROM information_schema.schemata
        WHERE schema_name LIKE 'tenant_%'
    LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = s.schema_name AND table_name = 'fhir_audit_event'
        ) THEN
            EXECUTE format(
                'ALTER TABLE %I.fhir_audit_event ADD COLUMN IF NOT EXISTS trace_id TEXT NOT NULL DEFAULT '''', ADD COLUMN IF NOT EXISTS span_id TEXT NOT NULL DEFAULT ''''',
                s.schema_name
            );
        END IF;

        IF EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = s.schema_name AND table_name = 'fhir_export_job'
        ) THEN
            EXECUTE format(
                'ALTER TABLE %I.fhir_export_job ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT ''complete'', ADD COLUMN IF NOT EXISTS err_msg TEXT NOT NULL DEFAULT '''', ADD COLUMN IF NOT EXISTS output_format TEXT NOT NULL DEFAULT '''', ADD COLUMN IF NOT EXISTS cancel_requested BOOLEAN NOT NULL DEFAULT FALSE',
                s.schema_name
            );
        END IF;
    END LOOP;
END;
$$;
