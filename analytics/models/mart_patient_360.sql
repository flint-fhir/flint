MODEL (
  name flint_marts.mart_patient_360,
  kind FULL,
  grain (tenant_id, patient_id),
  description 'Longitudinal Patient 360 analytical mart combining demographics, encounters, active conditions, and observations'
);

WITH encounter_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) AS encounter_count,
    MAX(period_start) AS last_encounter_start
  FROM flint_marts.fct_encounter
  GROUP BY tenant_id, patient_id
),
condition_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) AS total_condition_count,
    COUNT(*) FILTER (WHERE clinical_status = 'active') AS active_condition_count
  FROM flint_marts.fct_condition
  GROUP BY tenant_id, patient_id
),
observation_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) AS observation_count,
    MAX(effective_datetime) AS last_observation_datetime
  FROM flint_marts.fct_observation
  GROUP BY tenant_id, patient_id
)
SELECT
  p.tenant_id,
  p.patient_id,
  p.full_name,
  p.family_name,
  p.given_name,
  p.gender,
  p.birth_date,
  p.active,
  p.postal_code,
  COALESCE(e.encounter_count, 0) AS encounter_count,
  e.last_encounter_start,
  COALESCE(c.total_condition_count, 0) AS total_condition_count,
  COALESCE(c.active_condition_count, 0) AS active_condition_count,
  COALESCE(o.observation_count, 0) AS observation_count,
  o.last_observation_datetime
FROM flint_marts.dim_patient p
LEFT JOIN encounter_agg e
  ON e.tenant_id = p.tenant_id AND e.patient_id = p.patient_id
LEFT JOIN condition_agg c
  ON c.tenant_id = p.tenant_id AND c.patient_id = p.patient_id
LEFT JOIN observation_agg o
  ON o.tenant_id = p.tenant_id AND o.patient_id = p.patient_id
