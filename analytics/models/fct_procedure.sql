MODEL (
  name flint_marts.fct_procedure,
  kind FULL,
  grain (tenant_id, procedure_id),
  description 'Kimball Procedure fact table'
);

SELECT
  tenant_id,
  procedure_id,
  patient_id,
  status,
  code_system,
  code,
  performed_datetime,
  updated_at
FROM flint_stg.stg_procedure
