MODEL (
  name flint_stg.stg_medication_request,
  kind FULL,
  grain (tenant_id, medication_request_id),
  description 'Deduplicated current-state MedicationRequest staging model over Iceberg Table Topic CDC events'
);

WITH ranked AS (
  SELECT
    COALESCE(tenant_id, 'default') AS tenant_id,
    regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|MedicationRequest/)', '') AS medication_request_id,
    _kafka_offset AS kafka_offset,
    _kafka_timestamp AS updated_at,
    COALESCE(op, 'U') AS op,
    CAST(_kafka_value AS VARCHAR) AS raw_json,
    ROW_NUMBER() OVER (
      PARTITION BY COALESCE(tenant_id, 'default'), regexp_replace(_kafka_key, '^(' || COALESCE(tenant_id, 'default') || '/|MedicationRequest/)', '')
      ORDER BY _kafka_offset DESC
    ) AS rn
  FROM fhir_raw.medication_request
)
SELECT
  tenant_id,
  medication_request_id,
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
    json_extract_string(raw_json, '$.medicationCodeableConcept.coding[0].system.value'),
    json_extract_string(raw_json, '$.medicationCodeableConcept.coding[0].system'),
    json_extract_string(raw_json, '$.medication.codeableConcept.coding[0].system.value'),
    json_extract_string(raw_json, '$.medication.codeableConcept.coding[0].system')
  ) AS medication_system,
  COALESCE(
    json_extract_string(raw_json, '$.medicationCodeableConcept.coding[0].code.value'),
    json_extract_string(raw_json, '$.medicationCodeableConcept.coding[0].code'),
    json_extract_string(raw_json, '$.medication.codeableConcept.coding[0].code.value'),
    json_extract_string(raw_json, '$.medication.codeableConcept.coding[0].code')
  ) AS medication_code,
  TRY_CAST(COALESCE(
    json_extract_string(raw_json, '$.authoredOn.value'),
    json_extract_string(raw_json, '$.authoredOn')
  ) AS TIMESTAMPTZ) AS authored_on,
  updated_at
FROM ranked
WHERE rn = 1
  AND op != 'D'
  AND raw_json != '[tombstone]'
