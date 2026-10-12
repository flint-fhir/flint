MODEL (
  name flint_marts.dim_organization,
  kind FULL,
  grain (tenant_id, organization_id),
  description 'Kimball Organization dimension table'
);

SELECT
  tenant_id,
  organization_id,
  name,
  active,
  type_code,
  city,
  state,
  postal_code,
  updated_at
FROM flint_stg.stg_organization
