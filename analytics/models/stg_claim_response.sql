MODEL (
  name flint_stg.stg_claim_response,
  kind FULL,
  grain (tenant_id, claim_response_id),
  description 'Deduplicated current-state ClaimResponse staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|ClaimResponse/)', '') AS claim_response_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|ClaimResponse/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.claim_response
)
SELECT
  tenant_id,
  claim_response_id,
  regexp_replace(
    COALESCE(
      json_extract_string(raw_json, '$.request.claimId.value'),
      json_extract_string(raw_json, '$.request.reference.value'),
      json_extract_string(raw_json, '$.request.reference')
    ),
    '^Claim/',
    ''
  ) AS claim_id,
  regexp_replace(
    COALESCE(
      json_extract_string(raw_json, '$.patient.patientId.value'),
      json_extract_string(raw_json, '$.patient.reference.value'),
      json_extract_string(raw_json, '$.patient.reference')
    ),
    '^Patient/',
    ''
  ) AS patient_id,
  LOWER(COALESCE(
    json_extract_string(raw_json, '$.status.value'),
    json_extract_string(raw_json, '$.status')
  )) AS status,
  LOWER(COALESCE(
    json_extract_string(raw_json, '$.outcome.value'),
    json_extract_string(raw_json, '$.outcome')
  )) AS outcome,
  COALESCE(
    json_extract_string(raw_json, '$.disposition.value'),
    json_extract_string(raw_json, '$.disposition')
  ) AS disposition,
  COALESCE(
    json_extract_string(raw_json, '$.preAuthRef.value'),
    json_extract_string(raw_json, '$.preAuthRef')
  ) AS pre_auth_ref,
  COALESCE(
    TRY_CAST(json_extract_string(raw_json, '$.total[1].amount.value') AS DOUBLE),
    TRY_CAST(json_extract_string(raw_json, '$.total[0].amount.value') AS DOUBLE),
    0.0
  ) AS adjudicated_benefit_amount,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
