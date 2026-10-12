package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ExportParams specifies filters for FHIR Bulk Data Access ($export).
type ExportParams struct {
	TenantID           string
	PatientID          string     // Optional: scopes export to a single Patient/{id} compartment
	PatientCompartment bool       // Optional: when true (and PatientID == ""), scopes export to active Patient compartments
	ResourceTypes      []string   // Optional: _type filter (e.g. ["Patient", "Observation"])
	Since              *time.Time // Optional: _since filter (last_updated >= Since)
}

// ExportResources retrieves all active FHIR resources matching ExportParams ordered
// deterministically by (res_type, res_id) for NDJSON bulk export.
func (s *Store) ExportResources(ctx context.Context, params ExportParams) ([]SearchResult, error) {
	if params.PatientID != "" {
		return s.exportSinglePatientCompartment(ctx, params)
	}

	whereConds := []string{
		"r.tenant_id = $1",
		"r.is_deleted = FALSE",
	}
	args := []any{params.TenantID}

	if params.PatientCompartment {
		whereConds = append(whereConds, `(
			r.res_type = 'Patient'
			OR EXISTS (
				SELECT 1 FROM spidx_reference rf
				JOIN fhir_resource p
				  ON p.tenant_id = rf.tenant_id
				 AND p.res_type = 'Patient'
				 AND p.res_id = rf.target_id
				 AND p.is_deleted = FALSE
				WHERE rf.tenant_id = r.tenant_id
				  AND rf.res_type = r.res_type
				  AND rf.res_id = r.res_id
				  AND (rf.target_type = 'Patient' OR rf.target_type = '')
			)
			OR EXISTS (
				SELECT 1 FROM spidx_reference rf
				JOIN fhir_resource p
				  ON p.tenant_id = rf.tenant_id
				 AND p.res_type = 'Patient'
				 AND p.res_id = rf.res_id
				 AND p.is_deleted = FALSE
				WHERE rf.tenant_id = r.tenant_id
				  AND rf.target_type = r.res_type
				  AND rf.target_id = r.res_id
			)
			OR (
				r.res_type NOT IN ('Patient', 'Condition', 'Encounter', 'Observation', 'MedicationRequest', 'AllergyIntolerance', 'Procedure', 'DiagnosticReport', 'Immunization', 'Specimen')
				AND EXISTS (
					SELECT 1 FROM spidx_reference rf_clin
					JOIN spidx_reference rf_pat
					  ON rf_pat.tenant_id = rf_clin.tenant_id
					 AND rf_pat.res_type = rf_clin.res_type
					 AND rf_pat.res_id = rf_clin.res_id
					JOIN fhir_resource p
					  ON p.tenant_id = rf_pat.tenant_id
					 AND p.res_type = 'Patient'
					 AND p.res_id = rf_pat.target_id
					 AND p.is_deleted = FALSE
					WHERE rf_clin.tenant_id = r.tenant_id
					  AND rf_clin.target_type = r.res_type
					  AND rf_clin.target_id = r.res_id
					  AND (rf_pat.target_type = 'Patient' OR rf_pat.target_type = '')
				)
			)
		)`)
	}

	if len(params.ResourceTypes) > 0 {
		whereConds = append(whereConds, fmt.Sprintf("r.res_type = ANY($%d)", len(args)+1))
		args = append(args, pq.Array(params.ResourceTypes))
	}
	if params.Since != nil {
		whereConds = append(whereConds, fmt.Sprintf("r.last_updated >= $%d", len(args)+1))
		args = append(args, *params.Since)
	}

	querySQL := fmt.Sprintf(
		"SELECT r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto FROM fhir_resource r WHERE %s ORDER BY r.res_type ASC, r.res_id ASC",
		strings.Join(whereConds, " AND "),
	)

	rows, err := s.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, fmt.Errorf("export resources query: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ResType, &r.ResID, &r.ResVersion, &r.LastUpdated, &r.ResourceProto); err != nil {
			return nil, fmt.Errorf("scan export resource: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) exportSinglePatientCompartment(ctx context.Context, params ExportParams) ([]SearchResult, error) {
	const pageSize = 1000
	var all []SearchResult
	offset := 0

	for {
		sr, err := s.PatientEverything(ctx, PatientEverythingParams{
			TenantID:      params.TenantID,
			PatientID:     params.PatientID,
			ResourceTypes: params.ResourceTypes,
			Since:         params.Since,
			Count:         pageSize,
			Offset:        offset,
		})
		if err != nil {
			return nil, err
		}
		if offset == 0 && len(sr.Matches) > 0 {
			all = append(all, sr.Matches...)
		}
		all = append(all, sr.Includes...)
		if len(sr.Includes) < pageSize {
			break
		}
		offset += pageSize
	}
	return all, nil
}
