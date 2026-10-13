MODEL (
  name flint_marts.fct_document_reference,
  kind FULL,
  grain (tenant_id, document_reference_id),
  description 'Kimball DocumentReference fact table'
);

SELECT
  tenant_id,
  document_reference_id,
  patient_id,
  status,
  type_system,
  type_code,
  document_date,
  updated_at
FROM flint_stg.stg_document_reference
