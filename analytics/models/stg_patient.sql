MODEL (
  name flint_stg.stg_patient,
  kind FULL,
  grain (tenant_id, patient_id),
  description 'Deduplicated current-state Patient staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Patient/)', '') AS patient_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Patient/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.patient
)
SELECT
  tenant_id,
  patient_id,
  COALESCE(
    json_extract_string(raw_json, '$.name[0].family.value'),
    json_extract_string(raw_json, '$.name[0].family')
  ) AS family_name,
  COALESCE(
    json_extract_string(raw_json, '$.name[0].given[0].value'),
    json_extract_string(raw_json, '$.name[0].given[0]')
  ) AS given_name,
  LOWER(COALESCE(
    json_extract_string(raw_json, '$.gender.value'),
    json_extract_string(raw_json, '$.gender')
  )) AS gender,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.birthDate.value'),
    json_extract_string(raw_json, '$.birthDate')
  ) AS DATE) AS birth_date,
  COALESCE(
    TRY_CAST(COALESCE(
      json_extract_string(raw_json, '$.active.value'),
      json_extract_string(raw_json, '$.active')
    ) AS BOOLEAN),
    TRUE
  ) AS active,
  COALESCE(
    json_extract_string(raw_json, '$.address[0].postalCode.value'),
    json_extract_string(raw_json, '$.address[0].postalCode')
  ) AS postal_code,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
