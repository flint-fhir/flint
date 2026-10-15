MODEL (
  name flint_marts.mart_claims_utilization,
  kind FULL,
  grain (tenant_id, patient_id),
  description 'Gold Financial & Prior Authorization Utilization Mart combining patient demographics, active coverage, submitted claims, adjudicated benefits, and Da Vinci PAS prior authorizations'
);

WITH claim_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) AS total_claim_count,
    COUNT(*) FILTER (WHERE claim_use = 'preauthorization') AS prior_auth_count,
    COUNT(*) FILTER (WHERE adjudication_outcome = 'complete') AS approved_claim_count,
    ROUND(SUM(COALESCE(total_submitted_amount, 0.0)), 2) AS total_submitted_amount,
    ROUND(SUM(COALESCE(adjudicated_benefit_amount, 0.0)), 2) AS total_adjudicated_benefit_amount,
    MAX(created_at) AS latest_claim_created_at
  FROM flint_marts.fct_claim
  WHERE status = 'active'
  GROUP BY tenant_id, patient_id
),
coverage_agg AS (
  SELECT
    tenant_id,
    patient_id,
    COUNT(*) FILTER (WHERE status = 'active') AS active_coverage_count,
    MAX(payor_reference) FILTER (WHERE status = 'active') AS primary_payor_reference
  FROM flint_marts.fct_coverage
  GROUP BY tenant_id, patient_id
)
SELECT
  p.tenant_id,
  p.patient_id,
  p.full_name,
  p.gender,
  p.birth_date,
  COALESCE(cv.active_coverage_count, 0) AS active_coverage_count,
  cv.primary_payor_reference,
  COALESCE(ca.total_claim_count, 0) AS total_claim_count,
  COALESCE(ca.prior_auth_count, 0) AS prior_auth_count,
  COALESCE(ca.approved_claim_count, 0) AS approved_claim_count,
  COALESCE(ca.total_submitted_amount, 0.0) AS total_submitted_amount,
  COALESCE(ca.total_adjudicated_benefit_amount, 0.0) AS total_adjudicated_benefit_amount,
  ca.latest_claim_created_at
FROM flint_marts.dim_patient p
LEFT JOIN coverage_agg cv
  ON cv.tenant_id = p.tenant_id AND cv.patient_id = p.patient_id
LEFT JOIN claim_agg ca
  ON ca.tenant_id = p.tenant_id AND ca.patient_id = p.patient_id
