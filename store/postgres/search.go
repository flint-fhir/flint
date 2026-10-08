package postgres

import (
	"context"
	"fmt"
	"strings"
)

// SearchResult is a single resource matching a search query.
type SearchResult struct {
	ResType       string
	ResID         string
	ResourceProto []byte
}

// SearchParams holds parsed FHIR search parameters for a query.
type SearchParams struct {
	TenantID   string
	ResType    string
	Strings    map[string]string // sp_name → value (prefix match)
	Tokens     map[string]string // sp_name → value (exact or system|value)
	Dates      map[string]DateOp // sp_name → date operation
	References map[string]string // sp_name → targetType/targetId
	Count      int               // _count (default 20, max 1000)
	Offset     int               // _offset (default 0)
}

// DateOp represents a date search operation.
type DateOp struct {
	Prefix string // eq, lt, gt, le, ge (default: eq)
	Value  string // ISO date string, parsed to time.Time before query
}

// Search executes a FHIR search query using the spidx_* tables.
// It performs an intersection (AND) of all provided search parameters.
func (s *Store) Search(ctx context.Context, params SearchParams) ([]SearchResult, int, error) {
	if params.Count <= 0 {
		params.Count = 20
	}
	if params.Count > 1000 {
		params.Count = 1000
	}

	// Build the query: start with fhir_resource, JOIN each search index table
	args := []any{params.TenantID, params.ResType}
	joins := []string{}
	joinIdx := 0

	// String search params
	for spName, value := range params.Strings {
		joinIdx++
		alias := fmt.Sprintf("s%d", joinIdx)
		joins = append(joins, fmt.Sprintf(
			"JOIN spidx_string %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s.sp_value LIKE $%d",
			alias, alias, alias, alias, alias, len(args)+1, alias, len(args)+2,
		))
		args = append(args, spName, strings.ToLower(value)+"%") // prefix match
	}

	// Token search params
	for spName, value := range params.Tokens {
		joinIdx++
		alias := fmt.Sprintf("t%d", joinIdx)
		// Support system|value format
		if parts := strings.SplitN(value, "|", 2); len(parts) == 2 {
			joins = append(joins, fmt.Sprintf(
				"JOIN spidx_token %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s.sp_system = $%d AND %s.sp_value = $%d",
				alias, alias, alias, alias, alias, len(args)+1, alias, len(args)+2, alias, len(args)+3,
			))
			args = append(args, spName, parts[0], parts[1])
		} else {
			joins = append(joins, fmt.Sprintf(
				"JOIN spidx_token %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s.sp_value = $%d",
				alias, alias, alias, alias, alias, len(args)+1, alias, len(args)+2,
			))
			args = append(args, spName, value)
		}
	}

	// Reference search params
	for spName, value := range params.References {
		joinIdx++
		alias := fmt.Sprintf("rf%d", joinIdx)
		parts := strings.SplitN(value, "/", 2)
		if len(parts) == 2 {
			joins = append(joins, fmt.Sprintf(
				"JOIN spidx_reference %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s.target_type = $%d AND %s.target_id = $%d",
				alias, alias, alias, alias, alias, len(args)+1, alias, len(args)+2, alias, len(args)+3,
			))
			args = append(args, spName, parts[0], parts[1])
		}
	}

	// Build the query
	joinClause := strings.Join(joins, "\n")

	// Count query
	countSQL := fmt.Sprintf(
		"SELECT COUNT(DISTINCT r.res_id) FROM fhir_resource r %s WHERE r.tenant_id = $1 AND r.res_type = $2 AND r.is_deleted = FALSE",
		joinClause,
	)
	var total int
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count query: %w", err)
	}

	// Data query with pagination
	dataSQL := fmt.Sprintf(
		"SELECT DISTINCT r.res_type, r.res_id, r.resource_proto FROM fhir_resource r %s WHERE r.tenant_id = $1 AND r.res_type = $2 AND r.is_deleted = FALSE ORDER BY r.res_id LIMIT $%d OFFSET $%d",
		joinClause, len(args)+1, len(args)+2,
	)
	args = append(args, params.Count, params.Offset)

	rows, err := s.db.QueryContext(ctx, dataSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ResType, &r.ResID, &r.ResourceProto); err != nil {
			return nil, 0, fmt.Errorf("scan: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("rows: %w", err)
	}

	return results, total, nil
}
