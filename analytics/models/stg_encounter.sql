MODEL (
  name flint_stg.stg_encounter,
  kind FULL,
  grain (tenant_id, encounter_id),
  description 'Deduplicated current-state Encounter staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    _kafka_key AS encounter_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), _kafka_key
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.encounter
)
SELECT
  tenant_id,
  encounter_id,
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
    json_extract_string(raw_json, '$.class.code.value'),
    json_extract_string(raw_json, '$.class.code')
  ) AS class_code,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.period.start.value'),
    json_extract_string(raw_json, '$.period.start')
  ) AS TIMESTAMPTZ) AS period_start,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.period.end.value'),
    json_extract_string(raw_json, '$.period.end')
  ) AS TIMESTAMPTZ) AS period_end,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
