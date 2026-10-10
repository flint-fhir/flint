MODEL (
  name flint_marts.dim_patient,
  kind FULL,
  grain (tenant_id, patient_id),
  description 'Kimball Patient dimension table enriched with normalized demographics'
);

SELECT
  tenant_id,
  patient_id,
  TRIM(CONCAT(COALESCE(given_name, ''), ' ', COALESCE(family_name, ''))) AS full_name,
  family_name,
  given_name,
  COALESCE(gender, 'unknown') AS gender,
  birth_date,
  active,
  postal_code,
  updated_at
FROM flint_stg.stg_patient
