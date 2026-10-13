MODEL (
  name flint_stg.stg_document_reference,
  kind FULL,
  grain (tenant_id, document_reference_id),
  description 'Deduplicated current-state DocumentReference staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|DocumentReference/)', '') AS document_reference_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|DocumentReference/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.document_reference
)
SELECT
  tenant_id,
  document_reference_id,
  regexp_replace(
    COALESCE(
      json_extract_string(raw_json, '$.subject.patientId.value'),
      json_extract_string(raw_json, '$.subject.reference.value'),
      json_extract_string(raw_json, '$.subject.reference')
    ),
    '^Patient/',
    ''
  ) AS patient_id,
  LOWER(COALESCE(
    json_extract_string(raw_json, '$.status.value'),
    json_extract_string(raw_json, '$.status')
  )) AS status,
  COALESCE(
    json_extract_string(raw_json, '$.type.coding[0].system.value'),
    json_extract_string(raw_json, '$.type.coding[0].system')
  ) AS type_system,
  COALESCE(
    json_extract_string(raw_json, '$.type.coding[0].code.value'),
    json_extract_string(raw_json, '$.type.coding[0].code')
  ) AS type_code,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.date.value'),
    json_extract_string(raw_json, '$.date')
  ) AS TIMESTAMPTZ) AS document_date,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
