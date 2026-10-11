MODEL (
  name flint_stg.stg_condition,
  kind FULL,
  grain (tenant_id, condition_id),
  description 'Deduplicated current-state Condition staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Condition/)', '') AS condition_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Condition/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.condition
)
SELECT
  tenant_id,
  condition_id,
  regexp_replace(
    COALESCE(
      json_extract_string(raw_json, '$.subject.patientId.value'),
      json_extract_string(raw_json, '$.subject.reference.value'),
      json_extract_string(raw_json, '$.subject.reference')
    ),
    '^Patient/',
    ''
  ) AS patient_id,
  COALESCE(
    json_extract_string(raw_json, '$.code.coding[0].system.value'),
    json_extract_string(raw_json, '$.code.coding[0].system')
  ) AS code_system,
  COALESCE(
    json_extract_string(raw_json, '$.code.coding[0].code.value'),
    json_extract_string(raw_json, '$.code.coding[0].code')
  ) AS code,
  LOWER(COALESCE(
    json_extract_string(raw_json, '$.clinicalStatus.coding[0].code.value'),
    json_extract_string(raw_json, '$.clinicalStatus.coding[0].code'),
    'active'
  )) AS clinical_status,
  LOWER(COALESCE(
    json_extract_string(raw_json, '$.verificationStatus.coding[0].code.value'),
    json_extract_string(raw_json, '$.verificationStatus.coding[0].code'),
    'confirmed'
  )) AS verification_status,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.onsetDateTime.value'),
    json_extract_string(raw_json, '$.onsetDateTime'),
    json_extract_string(raw_json, '$.onset.dateTime.value'),
    json_extract_string(raw_json, '$.onset.dateTime')
  ) AS DATE) AS onset_date,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
