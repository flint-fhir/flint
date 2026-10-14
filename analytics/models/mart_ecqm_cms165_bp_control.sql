MODEL (
  name flint_marts.mart_ecqm_cms165_bp_control,
  kind FULL,
  grain (tenant_id, patient_id),
  description 'CMS165v13 eCQM Gold Mart — Controlling High Blood Pressure (<140/90 mmHg) matching Measure/$evaluate-measure'
);

WITH htn_patients AS (
  SELECT DISTINCT
    tenant_id,
    patient_id
  FROM flint_marts.fct_condition
  WHERE clinical_status = 'active'
    AND code IN ('59621000', '38341003', 'I10', '401.9')
),
ranked_systolic AS (
  SELECT
    tenant_id,
    patient_id,
    value_quantity AS systolic_mmhg,
    effective_datetime,
    ROW_NUMBER() OVER (
      PARTITION BY tenant_id, patient_id
      ORDER BY effective_datetime DESC NULLS LAST, observation_id DESC
    ) AS rn
  FROM flint_marts.fct_observation
  WHERE status IN ('final', 'amended', 'corrected')
    AND code IN ('8480-6', '96608-5')
    AND value_quantity IS NOT NULL
),
ranked_diastolic AS (
  SELECT
    tenant_id,
    patient_id,
    value_quantity AS diastolic_mmhg,
    effective_datetime,
    ROW_NUMBER() OVER (
      PARTITION BY tenant_id, patient_id
      ORDER BY effective_datetime DESC NULLS LAST, observation_id DESC
    ) AS rn
  FROM flint_marts.fct_observation
  WHERE status IN ('final', 'amended', 'corrected')
    AND code IN ('8462-4', '96609-3')
    AND value_quantity IS NOT NULL
)
SELECT
  p.tenant_id,
  p.patient_id,
  p.full_name,
  p.gender,
  p.birth_date,
  TRUE AS initial_population,
  TRUE AS denominator,
  FALSE AS denominator_exclusion,
  s.systolic_mmhg AS latest_systolic_mmhg,
  d.diastolic_mmhg AS latest_diastolic_mmhg,
  COALESCE(s.effective_datetime, d.effective_datetime) AS latest_bp_datetime,
  CASE
    WHEN s.systolic_mmhg IS NOT NULL
     AND d.diastolic_mmhg IS NOT NULL
     AND s.systolic_mmhg < 140.0
     AND d.diastolic_mmhg < 90.0
    THEN TRUE
    ELSE FALSE
  END AS numerator,
  CASE
    WHEN s.systolic_mmhg IS NULL OR d.diastolic_mmhg IS NULL THEN 'MISSING_BP_READING'
    WHEN s.systolic_mmhg < 140.0 AND d.diastolic_mmhg < 90.0 THEN 'CONTROLLED'
    ELSE 'UNCONTROLLED_BP'
  END AS care_gap_status
FROM flint_marts.dim_patient p
JOIN htn_patients h
  ON h.tenant_id = p.tenant_id AND h.patient_id = p.patient_id
LEFT JOIN ranked_systolic s
  ON s.tenant_id = p.tenant_id AND s.patient_id = p.patient_id AND s.rn = 1
LEFT JOIN ranked_diastolic d
  ON d.tenant_id = p.tenant_id AND d.patient_id = p.patient_id AND d.rn = 1
