package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// SearchResult is a single resource matching a search query.
type SearchResult struct {
	ResType       string
	ResID         string
	ResourceProto []byte
}

// SearchResults contains the primary matched resources, resolved included resources,
// and the total match count.
type SearchResults struct {
	Matches  []SearchResult
	Includes []SearchResult
	Total    int
}

// SearchParams holds parsed FHIR search parameters for a query.
type SearchParams struct {
	TenantID    string
	ResType     string
	Strings     map[string]string     // sp_name → value (prefix match)
	Tokens      map[string]string     // sp_name → value (exact or system|value)
	Dates       map[string]DateOp     // sp_name → date operation
	Quantities  map[string]QuantityOp // sp_name → quantity operation
	References  map[string]string     // sp_name → targetType/targetId
	Chained     []ChainedParam        // chained search parameters (e.g. patient.name=Smith)
	Includes    []IncludeParam        // _include parameters
	RevIncludes []IncludeParam        // _revinclude parameters
	Count       int                   // _count (default 20, max 1000)
	Offset      int                   // _offset (default 0)
}

// Search executes a FHIR search query using the covering spidx_* tables.
// It performs an intersection (AND) of all provided search parameters,
// and resolves any requested _include or _revinclude resources.
func (s *Store) Search(ctx context.Context, params SearchParams) (*SearchResults, error) {
	if params.Count <= 0 {
		params.Count = 20
	}
	if params.Count > 1000 {
		params.Count = 1000
	}

	args := []any{params.TenantID, params.ResType}
	joins := []string{}
	joinIdx := 0

	// 1. String search parameters (spidx_string)
	for spName, value := range params.Strings {
		joinIdx++
		alias := fmt.Sprintf("s%d", joinIdx)
		joins = append(joins, fmt.Sprintf(
			"JOIN spidx_string %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s.sp_value LIKE $%d",
			alias, alias, alias, alias, alias, len(args)+1, alias, len(args)+2,
		))
		args = append(args, spName, strings.ToLower(value)+"%")
	}

	// 2. Token search parameters (spidx_token)
	for spName, value := range params.Tokens {
		joinIdx++
		alias := fmt.Sprintf("t%d", joinIdx)
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

	// 3. Date search parameters (spidx_date)
	for spName, dateOp := range params.Dates {
		joinIdx++
		alias := fmt.Sprintf("d%d", joinIdx)
		cond, dateArgs := buildDateCondition(alias, dateOp, len(args)+1)
		joins = append(joins, fmt.Sprintf(
			"JOIN spidx_date %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s",
			alias, alias, alias, alias, alias, len(args)+1, cond,
		))
		args = append(args, spName)
		args = append(args, dateArgs...)
	}

	// 4. Quantity search parameters (spidx_quantity)
	for spName, qOp := range params.Quantities {
		joinIdx++
		alias := fmt.Sprintf("q%d", joinIdx)
		cond, qArgs := buildQuantityCondition(alias, qOp, len(args)+1)
		joins = append(joins, fmt.Sprintf(
			"JOIN spidx_quantity %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s",
			alias, alias, alias, alias, alias, len(args)+1, cond,
		))
		args = append(args, spName)
		args = append(args, qArgs...)
	}

	// 5. Reference search parameters (spidx_reference)
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
		} else {
			joins = append(joins, fmt.Sprintf(
				"JOIN spidx_reference %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s.target_id = $%d",
				alias, alias, alias, alias, alias, len(args)+1, alias, len(args)+2,
			))
			args = append(args, spName, value)
		}
	}

	// 6. Chained search parameters (Observation?patient.name=Smith)
	for _, chained := range params.Chained {
		joinIdx++
		refAlias := fmt.Sprintf("crf%d", joinIdx)
		targetAlias := fmt.Sprintf("ctg%d", joinIdx)

		joins = append(joins, fmt.Sprintf(
			"JOIN spidx_reference %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s.target_type = $%d",
			refAlias, refAlias, refAlias, refAlias, refAlias, len(args)+1, refAlias, len(args)+2,
		))
		args = append(args, chained.RefParam, chained.TargetType)

		joins = append(joins, fmt.Sprintf(
			"JOIN spidx_string %s ON %s.tenant_id = r.tenant_id AND %s.res_type = $%d AND %s.res_id = %s.target_id AND %s.sp_name = $%d AND %s.sp_value LIKE $%d",
			targetAlias, targetAlias, targetAlias, len(args)+1, targetAlias, refAlias, targetAlias, len(args)+2, targetAlias, len(args)+3,
		))
		args = append(args, chained.TargetType, chained.TargetParam, strings.ToLower(chained.Value)+"%")
	}

	joinClause := strings.Join(joins, "\n")

	// Count query
	countSQL := fmt.Sprintf(
		"SELECT COUNT(DISTINCT r.res_id) FROM fhir_resource r %s WHERE r.tenant_id = $1 AND r.res_type = $2 AND r.is_deleted = FALSE",
		joinClause,
	)
	var total int
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count query: %w", err)
	}

	// Data query with pagination
	dataSQL := fmt.Sprintf(
		"SELECT DISTINCT r.res_type, r.res_id, r.resource_proto FROM fhir_resource r %s WHERE r.tenant_id = $1 AND r.res_type = $2 AND r.is_deleted = FALSE ORDER BY r.res_id LIMIT $%d OFFSET $%d",
		joinClause, len(args)+1, len(args)+2,
	)
	args = append(args, params.Count, params.Offset)

	rows, err := s.db.QueryContext(ctx, dataSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	var matches []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ResType, &r.ResID, &r.ResourceProto); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		matches = append(matches, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}

	// 7. Resolve _include and _revinclude resources
	includes, err := s.ResolveIncludes(ctx, params.TenantID, matches, params.Includes, params.RevIncludes)
	if err != nil {
		return nil, fmt.Errorf("resolve includes: %w", err)
	}

	return &SearchResults{
		Matches:  matches,
		Includes: includes,
		Total:    total,
	}, nil
}

// ResolveIncludes fetches referenced resources for _include and referring resources for _revinclude.
func (s *Store) ResolveIncludes(ctx context.Context, tenantID string, matches []SearchResult, includes []IncludeParam, revIncludes []IncludeParam) ([]SearchResult, error) {
	if len(matches) == 0 || (len(includes) == 0 && len(revIncludes) == 0) {
		return nil, nil
	}

	matchedIDs := make([]string, len(matches))
	matchedSet := make(map[string]bool)
	for i, m := range matches {
		matchedIDs[i] = m.ResID
		matchedSet[m.ResType+"/"+m.ResID] = true
	}

	var includedResults []SearchResult

	// Handle _include (Source:param)
	for _, inc := range includes {
		query := `
			SELECT DISTINCT r.res_type, r.res_id, r.resource_proto
			FROM spidx_reference rf
			JOIN fhir_resource r ON r.tenant_id = rf.tenant_id AND r.res_type = rf.target_type AND r.res_id = rf.target_id AND r.is_deleted = FALSE
			WHERE rf.tenant_id = $1 AND rf.res_type = $2 AND rf.res_id = ANY($3) AND rf.sp_name = $4
		`
		args := []any{tenantID, inc.SourceType, pq.Array(matchedIDs), inc.ParamName}
		if inc.TargetType != "" {
			query += " AND rf.target_type = $5"
			args = append(args, inc.TargetType)
		}

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("query include %s:%s: %w", inc.SourceType, inc.ParamName, err)
		}
		for rows.Next() {
			var r SearchResult
			if err := rows.Scan(&r.ResType, &r.ResID, &r.ResourceProto); err != nil {
				rows.Close()
				return nil, err
			}
			key := r.ResType + "/" + r.ResID
			if !matchedSet[key] {
				matchedSet[key] = true
				includedResults = append(includedResults, r)
			}
		}
		rows.Close()
	}

	// Handle _revinclude (Source:param)
	for _, rev := range revIncludes {
		query := `
			SELECT DISTINCT r.res_type, r.res_id, r.resource_proto
			FROM spidx_reference rf
			JOIN fhir_resource r ON r.tenant_id = rf.tenant_id AND r.res_type = rf.res_type AND r.res_id = rf.res_id AND r.is_deleted = FALSE
			WHERE rf.tenant_id = $1 AND rf.target_id = ANY($2) AND rf.res_type = $3 AND rf.sp_name = $4
		`
		args := []any{tenantID, pq.Array(matchedIDs), rev.SourceType, rev.ParamName}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("query revinclude %s:%s: %w", rev.SourceType, rev.ParamName, err)
		}
		for rows.Next() {
			var r SearchResult
			if err := rows.Scan(&r.ResType, &r.ResID, &r.ResourceProto); err != nil {
				rows.Close()
				return nil, err
			}
			key := r.ResType + "/" + r.ResID
			if !matchedSet[key] {
				matchedSet[key] = true
				includedResults = append(includedResults, r)
			}
		}
		rows.Close()
	}

	return includedResults, nil
}

func buildDateCondition(alias string, op DateOp, startIdx int) (string, []any) {
	// spidx_date has columns: sp_low, sp_high (TIMESTAMPTZ)
	val := op.Value
	switch op.Prefix {
	case "lt", "eb": // less than / ends before
		return fmt.Sprintf("%s.sp_high < $%d", alias, startIdx+1), []any{val}
	case "le": // less or equal
		return fmt.Sprintf("%s.sp_low <= $%d", alias, startIdx+1), []any{val}
	case "gt", "sa": // greater than / starts after
		return fmt.Sprintf("%s.sp_low > $%d", alias, startIdx+1), []any{val}
	case "ge": // greater or equal
		return fmt.Sprintf("%s.sp_high >= $%d", alias, startIdx+1), []any{val}
	case "ne": // not equal
		return fmt.Sprintf("(%s.sp_high < $%d OR %s.sp_low > $%d)", alias, startIdx+1, alias, startIdx+1), []any{val}
	default: // "eq" (or unspecified)
		// Overlaps the target date or point
		return fmt.Sprintf("%s.sp_low <= $%d AND %s.sp_high >= $%d", alias, startIdx+1, alias, startIdx+1), []any{val}
	}
}

func buildQuantityCondition(alias string, op QuantityOp, startIdx int) (string, []any) {
	conds := []string{}
	args := []any{}
	currIdx := startIdx + 1

	var compSQL string
	switch op.Prefix {
	case "lt":
		compSQL = "<"
	case "le":
		compSQL = "<="
	case "gt":
		compSQL = ">"
	case "ge":
		compSQL = ">="
	default:
		compSQL = "="
	}

	conds = append(conds, fmt.Sprintf("%s.sp_value %s $%d", alias, compSQL, currIdx))
	args = append(args, op.Value)
	currIdx++

	if op.System != "" {
		conds = append(conds, fmt.Sprintf("%s.sp_system = $%d", alias, currIdx))
		args = append(args, op.System)
		currIdx++
	}
	if op.Code != "" {
		conds = append(conds, fmt.Sprintf("%s.sp_code = $%d", alias, currIdx))
		args = append(args, op.Code)
	}

	return strings.Join(conds, " AND "), args
}
