package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ExportParams specifies filters for FHIR Bulk Data Access ($export).
type ExportParams struct {
	TenantID           string
	PatientID          string     // Optional: scopes export to a single Patient/{id} compartment
	PatientIDs         []string   // Optional: scopes export to a specific cohort of Patient IDs (Group/{id}/$export or patient parameter)
	EmptyCohort        bool       // Optional: when true, cohort resolved to 0 active patients so export yields 0 resources
	PatientCompartment bool       // Optional: when true (and PatientID == ""), scopes export to active Patient compartments
	ResourceTypes      []string   // Optional: _type filter (e.g. ["Patient", "Observation"])
	Since              *time.Time // Optional: _since filter (last_updated >= Since)
}

// ExportResources retrieves all active FHIR resources matching ExportParams ordered
// deterministically by (res_type, res_id) for NDJSON bulk export.
func (s *Store) ExportResources(ctx context.Context, params ExportParams) ([]SearchResult, error) {
	var out []SearchResult
	err := s.StreamExportResources(ctx, params, func(r SearchResult) error {
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StreamExportResources streams active FHIR resources matching ExportParams in
// deterministic paginated batches ordered by (res_type, res_id), avoiding unbounded
// single-query result buffering.
func (s *Store) StreamExportResources(ctx context.Context, params ExportParams, emit func(SearchResult) error) error {
	if params.EmptyCohort {
		return nil
	}
	if params.PatientID != "" && len(params.PatientIDs) == 0 {
		return s.streamSinglePatientCompartment(ctx, params, emit)
	}

	whereConds := []string{
		"r.tenant_id = $1",
		"r.is_deleted = FALSE",
	}
	baseArgs := []any{params.TenantID}

	if len(params.PatientIDs) > 0 {
		cleanCohort := make([]string, 0, len(params.PatientIDs))
		seen := make(map[string]bool, len(params.PatientIDs))
		for _, pid := range params.PatientIDs {
			pid = strings.TrimSpace(pid)
			if slash := strings.LastIndexByte(pid, '/'); slash != -1 {
				pid = pid[slash+1:]
			}
			if pid != "" && !seen[pid] {
				seen[pid] = true
				cleanCohort = append(cleanCohort, pid)
			}
		}
		if len(cleanCohort) == 0 {
			return nil
		}
		patIdx := len(baseArgs) + 1
		baseArgs = append(baseArgs, pq.Array(cleanCohort))
		whereConds = append(whereConds, fmt.Sprintf(`(
			(r.res_type = 'Patient' AND r.res_id = ANY($%[1]d))
			OR (
				r.res_type != 'Patient'
				AND (
					EXISTS (
						SELECT 1 FROM spidx_reference rf
						JOIN fhir_resource p
						  ON p.tenant_id = rf.tenant_id
						 AND p.res_type = 'Patient'
						 AND p.res_id = rf.target_id
						 AND p.is_deleted = FALSE
						WHERE rf.tenant_id = r.tenant_id
						  AND rf.res_type = r.res_type
						  AND rf.res_id = r.res_id
						  AND rf.target_id = ANY($%[1]d)
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
						  AND rf.res_type = 'Patient'
						  AND rf.res_id = ANY($%[1]d)
						  AND rf.target_type = r.res_type
						  AND rf.target_id = r.res_id
					)
					OR (
						r.res_type IN ('Organization', 'Practitioner', 'PractitionerRole', 'Location', 'Medication', 'HealthcareService', 'Endpoint', 'Substance')
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
							  AND rf_pat.target_id = ANY($%[1]d)
							  AND (rf_pat.target_type = 'Patient' OR rf_pat.target_type = '')
						)
					)
					OR (
						r.res_type = 'Provenance'
						AND EXISTS (
							SELECT 1 FROM spidx_reference rf_prov
							JOIN spidx_reference rf_pat
							  ON rf_pat.tenant_id = rf_prov.tenant_id
							 AND rf_pat.res_type = rf_prov.target_type
							 AND rf_pat.res_id = rf_prov.target_id
							JOIN fhir_resource p
							  ON p.tenant_id = rf_pat.tenant_id
							 AND p.res_type = 'Patient'
							 AND p.res_id = rf_pat.target_id
							 AND p.is_deleted = FALSE
							WHERE rf_prov.tenant_id = r.tenant_id
							  AND rf_prov.res_type = 'Provenance'
							  AND rf_prov.res_id = r.res_id
							  AND rf_prov.sp_name = 'target'
							  AND rf_pat.target_id = ANY($%[1]d)
							  AND (rf_pat.target_type = 'Patient' OR rf_pat.target_type = '')
						)
					)
				)
			)
		)`, patIdx))
	} else if params.PatientCompartment {
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
				r.res_type IN ('Organization', 'Practitioner', 'PractitionerRole', 'Location', 'Medication', 'HealthcareService', 'Endpoint', 'Substance')
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
			OR (
				r.res_type = 'Provenance'
				AND EXISTS (
					SELECT 1 FROM spidx_reference rf_prov
					JOIN spidx_reference rf_pat
					  ON rf_pat.tenant_id = rf_prov.tenant_id
					 AND rf_pat.res_type = rf_prov.target_type
					 AND rf_pat.res_id = rf_prov.target_id
					JOIN fhir_resource p
					  ON p.tenant_id = rf_pat.tenant_id
					 AND p.res_type = 'Patient'
					 AND p.res_id = rf_pat.target_id
					 AND p.is_deleted = FALSE
					WHERE rf_prov.tenant_id = r.tenant_id
					  AND rf_prov.res_type = 'Provenance'
					  AND rf_prov.res_id = r.res_id
					  AND rf_prov.sp_name = 'target'
					  AND (rf_pat.target_type = 'Patient' OR rf_pat.target_type = '')
				)
			)
		)`)
	}

	if len(params.ResourceTypes) > 0 {
		whereConds = append(whereConds, fmt.Sprintf("r.res_type = ANY($%d)", len(baseArgs)+1))
		baseArgs = append(baseArgs, pq.Array(params.ResourceTypes))
	}
	if params.Since != nil {
		whereConds = append(whereConds, fmt.Sprintf("r.last_updated >= $%d", len(baseArgs)+1))
		baseArgs = append(baseArgs, *params.Since)
	}

	const exportBatchSize = 500
	lastType := ""
	lastID := ""

	for {
		conds := append([]string{}, whereConds...)
		args := append([]any{}, baseArgs...)
		if lastType != "" || lastID != "" {
			tIdx := len(args) + 1
			idIdx := len(args) + 2
			args = append(args, lastType, lastID)
			conds = append(conds, fmt.Sprintf("(r.res_type > $%d OR (r.res_type = $%d AND r.res_id > $%d))", tIdx, tIdx, idIdx))
		}
		limitIdx := len(args) + 1
		args = append(args, exportBatchSize)

		querySQL := fmt.Sprintf(
			"SELECT r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto FROM fhir_resource r WHERE %s ORDER BY r.res_type ASC, r.res_id ASC LIMIT $%d",
			strings.Join(conds, " AND "), limitIdx,
		)

		batch := make([]SearchResult, 0, exportBatchSize)
		err := s.withTenantTx(ctx, params.TenantID, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, querySQL, args...)
			if err != nil {
				return fmt.Errorf("export resources query: %w", err)
			}
			defer rows.Close()

			for rows.Next() {
				var r SearchResult
				if err := rows.Scan(&r.ResType, &r.ResID, &r.ResVersion, &r.LastUpdated, &r.ResourceProto); err != nil {
					return fmt.Errorf("scan export resource: %w", err)
				}
				batch = append(batch, r)
			}
			return rows.Err()
		})
		if err != nil {
			return err
		}

		for _, item := range batch {
			if err := emit(item); err != nil {
				return err
			}
			lastType = item.ResType
			lastID = item.ResID
		}
		if len(batch) < exportBatchSize {
			break
		}
	}
	return nil
}

func (s *Store) streamSinglePatientCompartment(ctx context.Context, params ExportParams, emit func(SearchResult) error) error {
	const pageSize = 500
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
			return err
		}
		if offset == 0 {
			for _, m := range sr.Matches {
				if err := emit(m); err != nil {
					return err
				}
			}
		}
		for _, inc := range sr.Includes {
			if err := emit(inc); err != nil {
				return err
			}
		}
		if len(sr.Includes) < pageSize {
			break
		}
		offset += pageSize
	}
	return nil
}
