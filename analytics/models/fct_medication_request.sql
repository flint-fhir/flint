MODEL (
  name flint_marts.fct_medication_request,
  kind FULL,
  grain (tenant_id, medication_request_id),
  description 'Kimball MedicationRequest fact table'
);

SELECT
  tenant_id,
  medication_request_id,
  patient_id,
  status,
  intent,
  medication_system,
  medication_code,
  authored_on,
  updated_at
FROM flint_stg.stg_medication_request
