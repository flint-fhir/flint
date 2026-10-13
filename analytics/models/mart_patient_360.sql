MODEL (
  name flint_marts.mart_patient_360,
  kind FULL,
  grain (tenant_id, patient_id),
  description 'Longitudinal Patient 360 analytical mart combining demographics, encounters, active conditions, observations, medications, immunizations, allergies, procedures, care plans, and coverage'
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
),
medication_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) FILTER (WHERE status = 'active') AS active_medication_count
  FROM flint_marts.fct_medication_request
  GROUP BY tenant_id, patient_id
),
immunization_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) FILTER (WHERE status = 'completed') AS immunization_count
  FROM flint_marts.fct_immunization
  GROUP BY tenant_id, patient_id
),
allergy_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) FILTER (WHERE clinical_status = 'active') AS active_allergy_count
  FROM flint_marts.fct_allergy_intolerance
  GROUP BY tenant_id, patient_id
),
procedure_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) AS procedure_count
  FROM flint_marts.fct_procedure
  GROUP BY tenant_id, patient_id
),
care_plan_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) FILTER (WHERE status = 'active') AS active_care_plan_count
  FROM flint_marts.fct_care_plan
  GROUP BY tenant_id, patient_id
),
coverage_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) FILTER (WHERE status = 'active') AS active_coverage_count
  FROM flint_marts.fct_coverage
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
  o.last_observation_datetime,
  COALESCE(m.active_medication_count, 0) AS active_medication_count,
  COALESCE(i.immunization_count, 0) AS immunization_count,
  COALESCE(a.active_allergy_count, 0) AS active_allergy_count,
  COALESCE(pr.procedure_count, 0) AS procedure_count,
  COALESCE(cp.active_care_plan_count, 0) AS active_care_plan_count,
  COALESCE(cv.active_coverage_count, 0) AS active_coverage_count
FROM flint_marts.dim_patient p
LEFT JOIN encounter_agg e
  ON e.tenant_id = p.tenant_id AND e.patient_id = p.patient_id
LEFT JOIN condition_agg c
  ON c.tenant_id = p.tenant_id AND c.patient_id = p.patient_id
LEFT JOIN observation_agg o
  ON o.tenant_id = p.tenant_id AND o.patient_id = p.patient_id
LEFT JOIN medication_agg m
  ON m.tenant_id = p.tenant_id AND m.patient_id = p.patient_id
LEFT JOIN immunization_agg i
  ON i.tenant_id = p.tenant_id AND i.patient_id = p.patient_id
LEFT JOIN allergy_agg a
  ON a.tenant_id = p.tenant_id AND a.patient_id = p.patient_id
LEFT JOIN procedure_agg pr
  ON pr.tenant_id = p.tenant_id AND pr.patient_id = p.patient_id
LEFT JOIN care_plan_agg cp
  ON cp.tenant_id = p.tenant_id AND cp.patient_id = p.patient_id
LEFT JOIN coverage_agg cv
  ON cv.tenant_id = p.tenant_id AND cv.patient_id = p.patient_id
