MODEL (
  name flint_marts.fct_diagnostic_report,
  kind FULL,
  grain (tenant_id, diagnostic_report_id),
  description 'Kimball DiagnosticReport fact table'
);

SELECT
  tenant_id,
  diagnostic_report_id,
  patient_id,
  status,
  code_system,
  code,
  effective_datetime,
  issued,
  updated_at
FROM flint_stg.stg_diagnostic_report
