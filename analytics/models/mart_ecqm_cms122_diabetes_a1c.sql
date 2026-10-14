MODEL (
  name flint_marts.mart_ecqm_cms122_diabetes_a1c,
  kind FULL,
  grain (tenant_id, patient_id),
  description 'CMS122v12 eCQM Gold Mart — Diabetes: Hemoglobin A1c (HbA1c) Poor Control (>9.0%) matching Measure/$evaluate-measure'
);

WITH diabetes_patients AS (
  SELECT DISTINCT
    tenant_id,
    patient_id
  FROM flint_marts.fct_condition
  WHERE clinical_status = 'active'
    AND code IN ('44054006', '73211009', '46635009', 'E11.9', 'E11', 'E10.9', '250.00')
),
ranked_a1c AS (
  SELECT
    tenant_id,
    patient_id,
    value_quantity AS hba1c_pct,
    effective_datetime,
    ROW_NUMBER() OVER (
      PARTITION BY tenant_id, patient_id
      ORDER BY effective_datetime DESC NULLS LAST, observation_id DESC
    ) AS rn
  FROM flint_marts.fct_observation
  WHERE status IN ('final', 'amended', 'corrected')
    AND code IN ('4548-4', '17856-6', '4549-2')
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
  a.hba1c_pct AS latest_hba1c_pct,
  a.effective_datetime AS latest_hba1c_datetime,
  CASE
    WHEN a.hba1c_pct IS NULL OR a.hba1c_pct > 9.0 THEN TRUE
    ELSE FALSE
  END AS numerator_poor_control,
  CASE
    WHEN a.hba1c_pct IS NULL THEN 'MISSING_HBA1C_TEST'
    WHEN a.hba1c_pct > 9.0 THEN 'POOR_CONTROL_GT_9'
    ELSE 'WELL_CONTROLLED'
  END AS care_gap_status
FROM flint_marts.dim_patient p
JOIN diabetes_patients dm
  ON dm.tenant_id = p.tenant_id AND dm.patient_id = p.patient_id
LEFT JOIN ranked_a1c a
  ON a.tenant_id = p.tenant_id AND a.patient_id = p.patient_id AND a.rn = 1
