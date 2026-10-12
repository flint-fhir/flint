MODEL (
  name flint_marts.fct_immunization,
  kind FULL,
  grain (tenant_id, immunization_id),
  description 'Kimball Immunization fact table'
);

SELECT
  tenant_id,
  immunization_id,
  patient_id,
  status,
  vaccine_system,
  vaccine_code,
  occurrence_datetime,
  updated_at
FROM flint_stg.stg_immunization
