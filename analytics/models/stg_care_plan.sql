MODEL (
  name flint_stg.stg_care_plan,
  kind FULL,
  grain (tenant_id, care_plan_id),
  description 'Deduplicated current-state CarePlan staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|CarePlan/)', '') AS care_plan_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|CarePlan/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.care_plan
)
SELECT
  tenant_id,
  care_plan_id,
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
  LOWER(COALESCE(
    json_extract_string(raw_json, '$.intent.value'),
    json_extract_string(raw_json, '$.intent')
  )) AS intent,
  COALESCE(
    json_extract_string(raw_json, '$.category[0].coding[0].code.value'),
    json_extract_string(raw_json, '$.category[0].coding[0].code')
  ) AS category_code,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.period.start.value'),
    json_extract_string(raw_json, '$.period.start')
  ) AS TIMESTAMPTZ) AS period_start,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
