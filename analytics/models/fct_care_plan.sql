MODEL (
  name flint_marts.fct_care_plan,
  kind FULL,
  grain (tenant_id, care_plan_id),
  description 'Kimball CarePlan fact table'
);

SELECT
  tenant_id,
  care_plan_id,
  patient_id,
  status,
  intent,
  category_code,
  period_start,
  updated_at
FROM flint_stg.stg_care_plan
