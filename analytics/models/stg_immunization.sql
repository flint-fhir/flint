MODEL (
  name flint_stg.stg_immunization,
  kind FULL,
  grain (tenant_id, immunization_id),
  description 'Deduplicated current-state Immunization staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Immunization/)', '') AS immunization_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Immunization/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.immunization
)
SELECT
  tenant_id,
  immunization_id,
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
  COALESCE(
    json_extract_string(raw_json, '$.vaccineCode.coding[0].system.value'),
    json_extract_string(raw_json, '$.vaccineCode.coding[0].system')
  ) AS vaccine_system,
  COALESCE(
    json_extract_string(raw_json, '$.vaccineCode.coding[0].code.value'),
    json_extract_string(raw_json, '$.vaccineCode.coding[0].code')
  ) AS vaccine_code,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.occurrenceDateTime.value'),
    json_extract_string(raw_json, '$.occurrenceDateTime'),
    json_extract_string(raw_json, '$.occurrence.dateTime.value'),
    json_extract_string(raw_json, '$.occurrence.dateTime')
  ) AS TIMESTAMPTZ) AS occurrence_datetime,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
