MODEL (
  name flint_marts.fct_observation,
  kind FULL,
  grain (tenant_id, observation_id),
  description 'Kimball Observation / Vital Signs / Lab Result fact table'
);

SELECT
  tenant_id,
  observation_id,
  patient_id,
  status,
  code_system,
  code,
  value_quantity,
  value_unit,
  effective_datetime,
  updated_at
FROM flint_stg.stg_observation
