MODEL (
  name flint_stg.stg_diagnostic_report,
  kind FULL,
  grain (tenant_id, diagnostic_report_id),
  description 'Deduplicated current-state DiagnosticReport staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|DiagnosticReport/)', '') AS diagnostic_report_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|DiagnosticReport/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.diagnostic_report
)
SELECT
  tenant_id,
  diagnostic_report_id,
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
    json_extract_string(raw_json, '$.code.coding[0].system.value'),
    json_extract_string(raw_json, '$.code.coding[0].system')
  ) AS code_system,
  COALESCE(
    json_extract_string(raw_json, '$.code.coding[0].code.value'),
    json_extract_string(raw_json, '$.code.coding[0].code')
  ) AS code,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.effectiveDateTime.value'),
    json_extract_string(raw_json, '$.effectiveDateTime'),
    json_extract_string(raw_json, '$.effective.dateTime.value'),
    json_extract_string(raw_json, '$.effective.dateTime')
  ) AS TIMESTAMPTZ) AS effective_datetime,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.issued.value'),
    json_extract_string(raw_json, '$.issued')
  ) AS TIMESTAMPTZ) AS issued,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
