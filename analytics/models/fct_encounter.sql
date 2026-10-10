MODEL (
  name flint_marts.fct_encounter,
  kind FULL,
  grain (tenant_id, encounter_id),
  description 'Kimball Encounter fact table with length-of-stay metrics'
);

SELECT
  tenant_id,
  encounter_id,
  patient_id,
  status,
  class_code,
  period_start,
  period_end,
  CASE
    WHEN period_start IS NOT NULL AND period_end IS NOT NULL
    THEN DATE_DIFF('hour', period_start, period_end)
    ELSE NULL
  END AS los_hours,
  updated_at
FROM flint_stg.stg_encounter
