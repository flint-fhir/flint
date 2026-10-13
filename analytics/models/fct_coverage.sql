MODEL (
  name flint_marts.fct_coverage,
  kind FULL,
  grain (tenant_id, coverage_id),
  description 'Kimball Coverage fact table'
);

SELECT
  tenant_id,
  coverage_id,
  patient_id,
  status,
  type_system,
  type_code,
  payor_reference,
  updated_at
FROM flint_stg.stg_coverage
