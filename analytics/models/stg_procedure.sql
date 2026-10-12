MODEL (
  name flint_stg.stg_procedure,
  kind FULL,
  grain (tenant_id, procedure_id),
  description 'Deduplicated current-state Procedure staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Procedure/)', '') AS procedure_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Procedure/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.procedure
)
SELECT
  tenant_id,
  procedure_id,
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
    json_extract_string(raw_json, '$.performedDateTime.value'),
    json_extract_string(raw_json, '$.performedDateTime'),
    json_extract_string(raw_json, '$.performed.dateTime.value'),
    json_extract_string(raw_json, '$.performed.dateTime'),
    json_extract_string(raw_json, '$.performedPeriod.start.value'),
    json_extract_string(raw_json, '$.performedPeriod.start'),
    json_extract_string(raw_json, '$.performed.period.start.value'),
    json_extract_string(raw_json, '$.performed.period.start')
  ) AS TIMESTAMPTZ) AS performed_datetime,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
