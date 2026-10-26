-- =============================================================================
-- Migration 005: Configurable Schema-per-Tenant Provisioning (provision_tenant)
-- =============================================================================

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
