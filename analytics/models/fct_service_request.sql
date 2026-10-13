MODEL (
  name flint_marts.fct_service_request,
  kind FULL,
  grain (tenant_id, service_request_id),
  description 'Kimball ServiceRequest fact table'
);

SELECT
  tenant_id,
  service_request_id,
  patient_id,
  status,
  intent,
  code_system,
  code,
  authored_on,
  updated_at
FROM flint_stg.stg_service_request
