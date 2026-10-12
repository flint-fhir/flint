MODEL (
  name flint_stg.stg_organization,
  kind FULL,
  grain (tenant_id, organization_id),
  description 'Deduplicated current-state Organization staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Organization/)', '') AS organization_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|Organization/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.organization
)
SELECT
  tenant_id,
  organization_id,
  COALESCE(
    json_extract_string(raw_json, '$.name.value'),
    json_extract_string(raw_json, '$.name')
  ) AS name,
  COALESCE(
    TRY_CAST(COALESCE(
      json_extract_string(raw_json, '$.active.value'),
      json_extract_string(raw_json, '$.active')
    ) AS BOOLEAN),
    TRUE
  ) AS active,
  COALESCE(
    json_extract_string(raw_json, '$.type[0].coding[0].code.value'),
    json_extract_string(raw_json, '$.type[0].coding[0].code')
  ) AS type_code,
  COALESCE(
    json_extract_string(raw_json, '$.address[0].city.value'),
    json_extract_string(raw_json, '$.address[0].city')
  ) AS city,
  COALESCE(
    json_extract_string(raw_json, '$.address[0].state.value'),
    json_extract_string(raw_json, '$.address[0].state')
  ) AS state,
  COALESCE(
    json_extract_string(raw_json, '$.address[0].postalCode.value'),
    json_extract_string(raw_json, '$.address[0].postalCode')
  ) AS postal_code,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
