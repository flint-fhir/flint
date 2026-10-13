MODEL (
  name flint_stg.stg_service_request,
  kind FULL,
  grain (tenant_id, service_request_id),
  description 'Deduplicated current-state ServiceRequest staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|ServiceRequest/)', '') AS service_request_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|ServiceRequest/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.service_request
)
SELECT
  tenant_id,
  service_request_id,
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
    json_extract_string(raw_json, '$.code.coding[0].system.value'),
    json_extract_string(raw_json, '$.code.coding[0].system')
  ) AS code_system,
  COALESCE(
    json_extract_string(raw_json, '$.code.coding[0].code.value'),
    json_extract_string(raw_json, '$.code.coding[0].code')
  ) AS code,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.authoredOn.value'),
    json_extract_string(raw_json, '$.authoredOn')
  ) AS TIMESTAMPTZ) AS authored_on,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
