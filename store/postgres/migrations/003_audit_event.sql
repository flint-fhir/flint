-- Flint FHIR Server: Postgres Operational Store
-- Migration 003: Append-only HIPAA / ONC security audit table (FHIR R4 AuditEvent)

CREATE TABLE IF NOT EXISTS fhir_audit_event (
    tenant_id       TEXT        NOT NULL,
    audit_id        TEXT        NOT NULL,
    recorded        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    action          TEXT        NOT NULL,   -- FHIR AuditEventAction: C, R, U, D, E
    subtype_code    TEXT        NOT NULL,   -- FHIR restful-interaction: read, vread, update, delete, history-instance, create, search-type, operation, batch
    outcome         TEXT        NOT NULL,   -- FHIR AuditEventOutcome: 0 (success), 4 (minor/4xx), 8 (serious/5xx)
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
