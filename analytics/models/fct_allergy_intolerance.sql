MODEL (
  name flint_marts.fct_allergy_intolerance,
  kind FULL,
  grain (tenant_id, allergy_intolerance_id),
  description 'Kimball AllergyIntolerance fact table'
);

SELECT
  tenant_id,
  allergy_intolerance_id,
  patient_id,
  clinical_status,
  verification_status,
  criticality,
  code_system,
  code,
  updated_at
FROM flint_stg.stg_allergy_intolerance
