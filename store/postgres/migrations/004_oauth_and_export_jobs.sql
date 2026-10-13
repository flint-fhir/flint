-- 004_oauth_and_export_jobs.sql
-- Persistent storage for OAuth2 refresh tokens, RFC 7009 token revocations,
-- and asynchronous FHIR Bulk Data ($export) job state across replicas.

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

