MODEL (
  name flint_marts.fct_claim,
  kind FULL,
  grain (tenant_id, claim_id),
  description 'Kimball Claim & Prior Authorization adjudication fact table joining Claim and ClaimResponse'
);

SELECT
  c.tenant_id,
  c.claim_id,
  c.patient_id,
  c.status,
  c.claim_use,
  c.claim_type_code,
  c.insurer_reference,
  c.provider_reference,
  c.total_submitted_amount,
  c.currency,
  c.created_at,
  cr.claim_response_id,
  cr.outcome AS adjudication_outcome,
  cr.disposition AS adjudication_disposition,
  cr.pre_auth_ref,
  COALESCE(cr.adjudicated_benefit_amount, 0.0) AS adjudicated_benefit_amount,
  c.updated_at
FROM flint_stg.stg_claim c
LEFT JOIN flint_stg.stg_claim_response cr
  ON cr.tenant_id = c.tenant_id AND cr.claim_id = c.claim_id
