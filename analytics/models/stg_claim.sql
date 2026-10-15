MODEL (
  name flint_stg.stg_claim,
  kind FULL,
  grain (tenant_id, claim_id),
  description 'Deduplicated current-state Claim staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Claim/)', '') AS claim_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Claim/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.claim
)
SELECT
  tenant_id,
  claim_id,
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
    json_extract_string(raw_json, '$.use.value'),
    json_extract_string(raw_json, '$.use'),
    'claim'
  )) AS claim_use,
  COALESCE(
    json_extract_string(raw_json, '$.type.coding[0].code.value'),
    json_extract_string(raw_json, '$.type.coding[0].code')
  ) AS claim_type_code,
  COALESCE(
    json_extract_string(raw_json, '$.insurer.organizationId.value'),
    json_extract_string(raw_json, '$.insurer.reference.value'),
    json_extract_string(raw_json, '$.insurer.reference')
  ) AS insurer_reference,
  COALESCE(
    json_extract_string(raw_json, '$.provider.practitionerId.value'),
    json_extract_string(raw_json, '$.provider.organizationId.value'),
    json_extract_string(raw_json, '$.provider.reference.value'),
    json_extract_string(raw_json, '$.provider.reference')
  ) AS provider_reference,
  COALESCE(
    TRY_CAST(json_extract_string(raw_json, '$.total.value') AS DOUBLE),
    TRY_CAST(json_extract_string(raw_json, '$.total.value.value') AS DOUBLE),
    0.0
  ) AS total_submitted_amount,
  COALESCE(
    json_extract_string(raw_json, '$.total.currency.value'),
    json_extract_string(raw_json, '$.total.currency'),
    'USD'
  ) AS currency,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.created.value'),
    json_extract_string(raw_json, '$.created')
  ) AS TIMESTAMPTZ) AS created_at,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
