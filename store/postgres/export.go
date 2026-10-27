package postgres

import (
	"context"
	"database/sql"
	"errors"
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
				r.res_type NOT IN ('Patient', 'Group')
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
							  AND rf_clin.res_type != 'Group'
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
							  AND rf_pat.res_type != 'Group'
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
			OR (
				r.res_type != 'Group'
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
							  AND rf_clin.res_type != 'Group'
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
							  AND rf_pat.res_type != 'Group'
							  AND (rf_pat.target_type = 'Patient' OR rf_pat.target_type = '')
						)
					)
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

// PurgePatientCompartment deletes a Patient resource and all clinical resources belonging
// to that patient's compartment (excluding shared non-compartment resources such as
// Practitioner, Organization, Location, Medication, or Group).
// When hardExpunge is false, resources are soft-deleted with version tombstones;
// when hardExpunge is true, resources, history, and search indexes are physically removed.
func (s *Store) PurgePatientCompartment(ctx context.Context, tenantID, patientID string, hardExpunge bool) ([]ResourceRecord, error) {
	compTypes := make([]string, 0, len(patientCompartmentTypes))
	for rt := range patientCompartmentTypes {
		compTypes = append(compTypes, rt)
	}

	type targetRef struct {
		resType    string
		resID      string
		resVersion int
	}

	var purged []ResourceRecord
	err := s.withTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		var patVer int
		var patDeleted bool
		err := tx.QueryRowContext(ctx, `
			SELECT res_version, is_deleted
			FROM fhir_resource
			WHERE tenant_id = $1 AND res_type = 'Patient' AND res_id = $2
			FOR UPDATE
		`, tenantID, patientID).Scan(&patVer, &patDeleted)
		if err != nil {
			return err
		}
		if patDeleted && !hardExpunge {
			return ErrResourceDeleted
		}

		activeFilter := "AND r.is_deleted = FALSE"
		if hardExpunge {
			activeFilter = ""
		}

		querySQL := fmt.Sprintf(`
			SELECT r.res_type, r.res_id, r.res_version
			FROM fhir_resource r
			WHERE r.tenant_id = $1
			  AND r.res_type = ANY($3)
			  %s
			  AND (
				EXISTS (
					SELECT 1 FROM spidx_reference rf
					WHERE rf.tenant_id = r.tenant_id
					  AND rf.res_type = r.res_type
					  AND rf.res_id = r.res_id
					  AND rf.target_id = $2
					  AND (rf.target_type = 'Patient' OR rf.target_type = '')
				)
				OR (
					r.res_type = 'Provenance'
					AND EXISTS (
						SELECT 1 FROM spidx_reference rf_prov
						JOIN spidx_reference rf_pat
						  ON rf_pat.tenant_id = rf_prov.tenant_id
						 AND rf_pat.res_type = rf_prov.target_type
						 AND rf_pat.res_id = rf_prov.target_id
						WHERE rf_prov.tenant_id = r.tenant_id
						  AND rf_prov.res_type = 'Provenance'
						  AND rf_prov.res_id = r.res_id
						  AND rf_prov.sp_name = 'target'
						  AND rf_pat.res_type != 'Group'
						  AND rf_pat.target_id = $2
						  AND (rf_pat.target_type = 'Patient' OR rf_pat.target_type = '')
					)
				)
			  )
			ORDER BY r.res_type ASC, r.res_id ASC
		`, activeFilter)

		rows, err := tx.QueryContext(ctx, querySQL, tenantID, patientID, pq.Array(compTypes))
		if err != nil {
			return fmt.Errorf("find compartment resources for purge: %w", err)
		}
		var targets []targetRef
		for rows.Next() {
			var t targetRef
			if err := rows.Scan(&t.resType, &t.resID, &t.resVersion); err != nil {
				rows.Close()
				return fmt.Errorf("scan compartment resource for purge: %w", err)
			}
			targets = append(targets, t)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		targets = append(targets, targetRef{
			resType:    "Patient",
			resID:      patientID,
			resVersion: patVer,
		})

		purged = make([]ResourceRecord, 0, len(targets))
		for _, t := range targets {
			if hardExpunge {
				if _, err := hardDeleteResourceInTx(ctx, tx, tenantID, t.resType, t.resID); err != nil {
					return err
				}
				purged = append(purged, ResourceRecord{
					TenantID:    tenantID,
					ResType:     t.resType,
					ResID:       t.resID,
					ResVersion:  t.resVersion,
					LastUpdated: time.Now().UTC(),
					IsDeleted:   true,
				})
			} else {
				rec, err := s.deleteResourceInTx(ctx, tx, tenantID, t.resType, t.resID, 0)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if rec != nil {
					purged = append(purged, *rec)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return purged, nil
}

// ExpungeParams specifies filters and options for the physical $expunge operation.
type ExpungeParams struct {
	TenantID                string
	ResType                 string // Optional: restricts expunge to a specific FHIR resource type
	ResID                   string // Optional: restricts expunge to a specific FHIR resource instance
	ExpungeDeletedResources bool   // Physically remove soft-deleted resources and their history
	ExpungePreviousVersions bool   // Physically remove non-current historical versions of active resources
	ExpungeEverything       bool   // Physically remove matching active + deleted resources, history, and indexes
	Limit                   int    // Maximum number of resources to expunge (default 10000)
}

// ExpungeResources physically deletes soft-deleted resources, historical versions, or all matching
// resources from fhir_resource, fhir_resource_history, and spidx_* tables, returning the count of expunged rows.
func (s *Store) ExpungeResources(ctx context.Context, params ExpungeParams) (int, error) {
	if !params.ExpungeDeletedResources && !params.ExpungePreviousVersions && !params.ExpungeEverything {
		params.ExpungeDeletedResources = true
	}
	limit := params.Limit
	if limit <= 0 || limit > 50000 {
		limit = 10000
	}

	type targetKey struct {
		resType string
		resID   string
	}

	totalExpunged := 0
	err := s.withTenantTx(ctx, params.TenantID, func(tx *sql.Tx) error {
		if params.ExpungeEverything || params.ExpungeDeletedResources {
			where := []string{"tenant_id = $1"}
			args := []any{params.TenantID}
			if !params.ExpungeEverything {
				where = append(where, "is_deleted = TRUE")
			}
			if params.ResType != "" {
				args = append(args, params.ResType)
				where = append(where, fmt.Sprintf("res_type = $%d", len(args)))
			}
			if params.ResID != "" {
				args = append(args, params.ResID)
				where = append(where, fmt.Sprintf("res_id = $%d", len(args)))
			}
			args = append(args, limit)
			q := fmt.Sprintf("SELECT res_type, res_id FROM fhir_resource WHERE %s ORDER BY res_type ASC, res_id ASC LIMIT $%d",
				strings.Join(where, " AND "), len(args))

			rows, err := tx.QueryContext(ctx, q, args...)
			if err != nil {
				return fmt.Errorf("select resources for expunge: %w", err)
			}
			var targets []targetKey
			for rows.Next() {
				var tk targetKey
				if err := rows.Scan(&tk.resType, &tk.resID); err != nil {
					rows.Close()
					return err
				}
				targets = append(targets, tk)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()

			for _, tk := range targets {
				n, err := hardDeleteResourceInTx(ctx, tx, params.TenantID, tk.resType, tk.resID)
				if err != nil {
					return err
				}
				totalExpunged += n
			}
		}

		if params.ExpungePreviousVersions && !params.ExpungeEverything {
			where := []string{
				"h.tenant_id = r.tenant_id",
				"h.res_type = r.res_type",
				"h.res_id = r.res_id",
				"h.res_version < r.res_version",
				"h.tenant_id = $1",
			}
			args := []any{params.TenantID}
			if params.ResType != "" {
				args = append(args, params.ResType)
				where = append(where, fmt.Sprintf("h.res_type = $%d", len(args)))
			}
			if params.ResID != "" {
				args = append(args, params.ResID)
				where = append(where, fmt.Sprintf("h.res_id = $%d", len(args)))
			}
			delSQL := fmt.Sprintf("DELETE FROM fhir_resource_history h USING fhir_resource r WHERE %s", strings.Join(where, " AND "))
			res, err := tx.ExecContext(ctx, delSQL, args...)
			if err != nil {
				return fmt.Errorf("expunge previous versions: %w", err)
			}
			if n, err := res.RowsAffected(); err == nil {
				totalExpunged += int(n)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return totalExpunged, nil
}

func hardDeleteResourceInTx(ctx context.Context, tx *sql.Tx, tenantID, resType, resID string) (int, error) {
	if err := deleteSearchIndexes(ctx, tx, tenantID, resType, resID); err != nil {
		return 0, err
	}
	removed := 0
	resHist, err := tx.ExecContext(ctx, `
		DELETE FROM fhir_resource_history
		WHERE tenant_id = $1 AND res_type = $2 AND res_id = $3
	`, tenantID, resType, resID)
	if err != nil {
		return 0, fmt.Errorf("hard delete history %s/%s: %w", resType, resID, err)
	}
	if n, err := resHist.RowsAffected(); err == nil {
		removed += int(n)
	}
	resCur, err := tx.ExecContext(ctx, `
		DELETE FROM fhir_resource
		WHERE tenant_id = $1 AND res_type = $2 AND res_id = $3
	`, tenantID, resType, resID)
	if err != nil {
		return 0, fmt.Errorf("hard delete resource %s/%s: %w", resType, resID, err)
	}
	if n, err := resCur.RowsAffected(); err == nil {
		removed += int(n)
	}
	return removed, nil
}
