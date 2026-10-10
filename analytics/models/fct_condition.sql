MODEL (
  name flint_marts.fct_condition,
  kind FULL,
  grain (tenant_id, condition_id),
  description 'Kimball Condition / Diagnosis fact table'
);

SELECT
  tenant_id,
  condition_id,
  patient_id,
  code_system,
  code,
  clinical_status,
  verification_status,
  onset_date,
  updated_at
FROM flint_stg.stg_condition
