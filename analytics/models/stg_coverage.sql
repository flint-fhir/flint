MODEL (
  name flint_stg.stg_coverage,
  kind FULL,
  grain (tenant_id, coverage_id),
  description 'Deduplicated current-state Coverage staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Coverage/)', '') AS coverage_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Coverage/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.coverage
)
SELECT
  tenant_id,
  coverage_id,
  regexp_replace(
    COALESCE(
      json_extract_string(raw_json, '$.beneficiary.patientId.value'),
      json_extract_string(raw_json, '$.beneficiary.reference.value'),
      json_extract_string(raw_json, '$.beneficiary.reference')
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
  COALESCE(
    json_extract_string(raw_json, '$.payor[0].organizationId.value'),
    json_extract_string(raw_json, '$.payor[0].reference.value'),
    json_extract_string(raw_json, '$.payor[0].reference')
  ) AS payor_reference,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
