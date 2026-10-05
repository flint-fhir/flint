-- Tenant provisioning helper
-- Usage: SELECT create_tenant_schema('hca');

CREATE OR REPLACE FUNCTION create_tenant_schema(tenant_name TEXT)
RETURNS void AS $$
BEGIN
    EXECUTE format('CREATE SCHEMA IF NOT EXISTS tenant_%I', tenant_name);
    EXECUTE format('SET search_path = tenant_%I', tenant_name);

    -- Apply the static DDL (8 tables)
    -- In production, read from sql/schema.sql. Inlined here for simplicity.
    RAISE NOTICE 'Schema tenant_% created. Apply sql/schema.sql with search_path set.', tenant_name;
END;
$$ LANGUAGE plpgsql;
