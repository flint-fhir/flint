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

// MaxSearchCount is the maximum number of resources returned in a single search page.
const MaxSearchCount = 1000

type queryExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SearchResult is a single resource matching a search query.
type SearchResult struct {
	ResType       string
	ResID         string
	ResVersion    int
	LastUpdated   time.Time
	ResourceProto []byte
}

// SearchResults contains the primary matched resources, resolved included resources,
// and the total match count.
type SearchResults struct {
	Matches  []SearchResult
	Includes []SearchResult
	Total    int
}

// PatientEverythingParams specifies parameters for the FHIR R4 Patient/$everything operation.
type PatientEverythingParams struct {
	TenantID      string
	PatientID     string
	ResourceTypes []string   // Optional _type filter (e.g. ["Observation", "Condition"])
	Since         *time.Time // Optional _since filter on last_updated
	Count         int
	Offset        int
}

// SearchParams holds parsed FHIR search parameters for a query.
type SearchParams struct {
	TenantID                 string
	ResType                  string
	ID                       string                          // Optional _id filter (supports comma-separated multipleOr)
	LastUpdated              []DateOp                        // Optional _lastUpdated filter(s) (e.g. ge + le range)
	Strings                  map[string]string               // sp_name → value (prefix, :exact, or :contains)
	StringFilters            map[string][]string             // sp_name → multipleAnd values (each may be comma-separated multipleOr)
	Tokens                   map[string]string               // sp_name → value (exact, system|value, or :not)
	TokenFilters             map[string][]string             // sp_name → multipleAnd values (each may be comma-separated multipleOr)
	Dates                    map[string]DateOp               // sp_name → first date operation
	DateFilters              map[string][]DateOp             // sp_name → all date operations (e.g. ge + le range)
	Quantities               map[string]QuantityOp           // sp_name → first quantity operation
	QuantityFilters          map[string][]QuantityOp         // sp_name → all quantity operations (e.g. ge + le range)
	References               map[string]string               // sp_name → targetType/targetId
	ReferenceFilters         map[string][]string             // sp_name → multipleAnd values (each may be comma-separated multipleOr)
	URIs                     map[string]string               // sp_name → URI value (exact, :below, or :above)
	URIFilters               map[string][]string             // sp_name → multipleAnd values (each may be comma-separated multipleOr)
	Missing                  map[string]bool                 // sp_name → :missing=true/false
	Chained                  []ChainedParam                  // chained search parameters (e.g. patient.name=Smith)
	Has                      []HasParam                      // reverse-chained search parameters (e.g. _has:Observation:patient:code=8867-4)
	List                     []string                        // _list cohort membership filters (e.g. _list=list-1)
	CompositeTokenQuantities [][]CompositeTokenQuantityParam // token$quantity composite filters (e.g. code-value-quantity, component-code-value-quantity)
	Includes                 []IncludeParam                  // _include parameters
	RevIncludes              []IncludeParam                  // _revinclude parameters
	Sort                     []SortSpec                      // _sort specifications
	Summary                  string                          // _summary mode (true, text, data, count, false)
	Elements                 []string                        // _elements projection fields
	TotalMode                string                          // _total mode (none, estimate, accurate)
	Count                    int                             // _count (default 20, max 1000)
	Offset                   int                             // _offset (default 0)
}

// Search executes a FHIR search query using the covering spidx_* tables.
// It performs an intersection (AND) of all provided search parameters,
// and resolves any requested _include or _revinclude resources.
func (s *Store) Search(ctx context.Context, params SearchParams) (*SearchResults, error) {
	var res *SearchResults
	err := s.withTenantTx(ctx, params.TenantID, func(tx *sql.Tx) error {
		var txErr error
		res, txErr = s.searchWithExecutor(ctx, tx, params)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Store) searchWithExecutor(ctx context.Context, exec queryExecer, params SearchParams) (*SearchResults, error) {
	if params.Count <= 0 {
		params.Count = 20
	}
	if params.Count > MaxSearchCount {
		params.Count = MaxSearchCount
	}
	if params.Offset < 0 {
		params.Offset = 0
	}

	args := []any{params.TenantID, params.ResType}
	joins := []string{}
	whereExtra := ""
	joinIdx := 0

	// 1. String search parameters (spidx_string) — supports :exact, :contains, multipleOr (comma), and multipleAnd
	stringFilters := params.StringFilters
	if len(stringFilters) == 0 && len(params.Strings) > 0 {
		stringFilters = make(map[string][]string, len(params.Strings))
		for k, v := range params.Strings {
			stringFilters[k] = []string{v}
		}
	}
	for spKey, filterVals := range stringFilters {
		baseName, modifier := splitParamModifier(spKey)
		for _, rawVal := range filterVals {
			var orConds []string
			joinIdx++
			alias := fmt.Sprintf("s%d", joinIdx)
			spIdx := len(args) + 1
			args = append(args, baseName)
			for part := range strings.SplitSeq(rawVal, ",") {
				v := strings.ToLower(strings.TrimSpace(part))
				if v == "" {
					continue
				}
				switch modifier {
				case "exact":
					orConds = append(orConds, fmt.Sprintf("%s.sp_value = $%d", alias, len(args)+1))
					args = append(args, v)
				case "contains":
					orConds = append(orConds, fmt.Sprintf(`%s.sp_value LIKE $%d ESCAPE '\'`, alias, len(args)+1))
					args = append(args, "%"+escapeLikePattern(v)+"%")
				default:
					orConds = append(orConds, fmt.Sprintf(`%s.sp_value LIKE $%d ESCAPE '\'`, alias, len(args)+1))
					args = append(args, escapeLikePattern(v)+"%")
				}
			}
			if len(orConds) > 0 {
				joins = append(joins, fmt.Sprintf(
					"JOIN spidx_string %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND (%s)",
					alias, alias, alias, alias, alias, spIdx, strings.Join(orConds, " OR "),
				))
			}
		}
	}

	// 2. Token search parameters (spidx_token) — supports system|code, :not, multipleOr (comma), and multipleAnd
	tokenFilters := params.TokenFilters
	if len(tokenFilters) == 0 && len(params.Tokens) > 0 {
		tokenFilters = make(map[string][]string, len(params.Tokens))
		for k, v := range params.Tokens {
			tokenFilters[k] = []string{v}
		}
	}
	for spKey, filterVals := range tokenFilters {
		baseName, modifier := splitParamModifier(spKey)
		for _, rawVal := range filterVals {
			joinIdx++
			alias := fmt.Sprintf("t%d", joinIdx)
			spIdx := len(args) + 1
			args = append(args, baseName)
			var orConds []string
			for part := range strings.SplitSeq(rawVal, ",") {
				item := strings.TrimSpace(part)
				if item == "" {
					continue
				}
				if parts := strings.SplitN(item, "|", 2); len(parts) == 2 {
					sys := parts[0]
					code := parts[1]
					if sys == "" {
						orConds = append(orConds, fmt.Sprintf("(%s.sp_system = '' AND %s.sp_value = $%d)", alias, alias, len(args)+1))
						args = append(args, code)
					} else if code == "" {
						orConds = append(orConds, fmt.Sprintf("(%s.sp_system = $%d)", alias, len(args)+1))
						args = append(args, sys)
					} else if isEnumTokenParam(baseName) {
						normVal := strings.ToLower(strings.ReplaceAll(code, "_", "-"))
						upperVal := strings.ToUpper(strings.ReplaceAll(code, "-", "_"))
						orConds = append(orConds, fmt.Sprintf("((%s.sp_system = $%d OR %s.sp_system = '') AND %s.sp_value IN ($%d, $%d, $%d))",
							alias, len(args)+1, alias, alias, len(args)+2, len(args)+3, len(args)+4))
						args = append(args, sys, code, normVal, upperVal)
					} else {
						orConds = append(orConds, fmt.Sprintf("(%s.sp_system = $%d AND %s.sp_value = $%d)", alias, len(args)+1, alias, len(args)+2))
						args = append(args, sys, code)
					}
				} else if isEnumTokenParam(baseName) {
					normVal := strings.ToLower(strings.ReplaceAll(item, "_", "-"))
					upperVal := strings.ToUpper(strings.ReplaceAll(item, "-", "_"))
					orConds = append(orConds, fmt.Sprintf("(%s.sp_value IN ($%d, $%d, $%d))", alias, len(args)+1, len(args)+2, len(args)+3))
					args = append(args, item, normVal, upperVal)
				} else {
					orConds = append(orConds, fmt.Sprintf("(%s.sp_value = $%d)", alias, len(args)+1))
					args = append(args, item)
				}
			}
			if len(orConds) == 0 {
				continue
			}
			if modifier == "not" {
				whereExtra += fmt.Sprintf(
					" AND NOT EXISTS (SELECT 1 FROM spidx_token %s WHERE %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND (%s))",
					alias, alias, alias, alias, alias, spIdx, strings.Join(orConds, " OR "),
				)
			} else {
				joins = append(joins, fmt.Sprintf(
					"JOIN spidx_token %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND (%s)",
					alias, alias, alias, alias, alias, spIdx, strings.Join(orConds, " OR "),
				))
			}
		}
	}

	// 3. Date search parameters (spidx_date) — supports multi-bound ranges on the same row
	dateFilters := params.DateFilters
	if len(dateFilters) == 0 && len(params.Dates) > 0 {
		dateFilters = make(map[string][]DateOp, len(params.Dates))
		for k, v := range params.Dates {
			dateFilters[k] = []DateOp{v}
		}
	}
	for spName, ops := range dateFilters {
		if len(ops) == 0 {
			continue
		}
		joinIdx++
		alias := fmt.Sprintf("d%d", joinIdx)
		spIdx := len(args) + 1
		args = append(args, spName)
		condParts := make([]string, 0, len(ops))
		for _, dateOp := range ops {
			cond, dateArgs := buildDateCondition(alias, dateOp, len(args))
			condParts = append(condParts, cond)
			args = append(args, dateArgs...)
		}
		joins = append(joins, fmt.Sprintf(
			"JOIN spidx_date %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s",
			alias, alias, alias, alias, alias, spIdx, strings.Join(condParts, " AND "),
		))
	}

	// 4. Quantity search parameters (spidx_quantity) — supports multi-bound ranges on the same row
	quantityFilters := params.QuantityFilters
	if len(quantityFilters) == 0 && len(params.Quantities) > 0 {
		quantityFilters = make(map[string][]QuantityOp, len(params.Quantities))
		for k, v := range params.Quantities {
			quantityFilters[k] = []QuantityOp{v}
		}
	}
	for spName, ops := range quantityFilters {
		if len(ops) == 0 {
			continue
		}
		joinIdx++
		alias := fmt.Sprintf("q%d", joinIdx)
		spIdx := len(args) + 1
		args = append(args, spName)
		condParts := make([]string, 0, len(ops))
		for _, qOp := range ops {
			cond, qArgs := buildQuantityCondition(alias, qOp, len(args))
			condParts = append(condParts, cond)
			args = append(args, qArgs...)
		}
		joins = append(joins, fmt.Sprintf(
			"JOIN spidx_quantity %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s",
			alias, alias, alias, alias, alias, spIdx, strings.Join(condParts, " AND "),
		))
	}

	// 5. Reference search parameters (spidx_reference) — supports TargetType/id, :TargetType modifier, and multipleOr
	referenceFilters := params.ReferenceFilters
	if len(referenceFilters) == 0 && len(params.References) > 0 {
		referenceFilters = make(map[string][]string, len(params.References))
		for k, v := range params.References {
			referenceFilters[k] = []string{v}
		}
	}
	for spKey, filterVals := range referenceFilters {
		baseName, modType := splitParamModifier(spKey)
		for _, rawVal := range filterVals {
			joinIdx++
			alias := fmt.Sprintf("rf%d", joinIdx)
			spIdx := len(args) + 1
			args = append(args, baseName)
			var orConds []string
			for part := range strings.SplitSeq(rawVal, ",") {
				item := strings.TrimSpace(part)
				if item == "" {
					continue
				}
				parts := strings.SplitN(item, "/", 2)
				if len(parts) == 2 {
					orConds = append(orConds, fmt.Sprintf("(%s.target_type = $%d AND %s.target_id = $%d)", alias, len(args)+1, alias, len(args)+2))
					args = append(args, parts[0], parts[1])
				} else if modType != "" {
					orConds = append(orConds, fmt.Sprintf("(%s.target_type = $%d AND %s.target_id = $%d)", alias, len(args)+1, alias, len(args)+2))
					args = append(args, modType, item)
				} else {
					orConds = append(orConds, fmt.Sprintf("(%s.target_id = $%d)", alias, len(args)+1))
					args = append(args, item)
				}
			}
			if len(orConds) > 0 {
				joins = append(joins, fmt.Sprintf(
					"JOIN spidx_reference %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND (%s)",
					alias, alias, alias, alias, alias, spIdx, strings.Join(orConds, " OR "),
				))
			}
		}
	}

	// 5b. URI search parameters (spidx_uri with spidx_token fallback) — supports exact match, :below, :above, multipleOr, and multipleAnd
	uriFilters := params.URIFilters
	if len(uriFilters) == 0 && len(params.URIs) > 0 {
		uriFilters = make(map[string][]string, len(params.URIs))
		for k, v := range params.URIs {
			uriFilters[k] = []string{v}
		}
	}
	for spKey, filterVals := range uriFilters {
		baseName, modifier := splitParamModifier(spKey)
		for _, rawVal := range filterVals {
			joinIdx++
			uAlias := fmt.Sprintf("u%d", joinIdx)
			utAlias := fmt.Sprintf("ut%d", joinIdx)
			spIdx := len(args) + 1
			args = append(args, baseName)
			var uConds []string
			var utConds []string
			for part := range strings.SplitSeq(rawVal, ",") {
				v := strings.TrimSpace(part)
				if v == "" {
					continue
				}
				switch modifier {
				case "below":
					baseURI := strings.TrimSuffix(v, "/")
					p1 := len(args) + 1
					p2 := len(args) + 2
					p3 := len(args) + 3
					args = append(args, v, baseURI, escapeLikePattern(baseURI)+"/%")
					uConds = append(uConds, fmt.Sprintf(`(%s.sp_value = $%d OR %s.sp_value = $%d OR %s.sp_value LIKE $%d ESCAPE '\')`, uAlias, p1, uAlias, p2, uAlias, p3))
					utConds = append(utConds, fmt.Sprintf(`(%s.sp_value = $%d OR %s.sp_value = $%d OR %s.sp_value LIKE $%d ESCAPE '\')`, utAlias, p1, utAlias, p2, utAlias, p3))
				case "above":
					ancestors := uriHierarchyAncestors(v)
					p1 := len(args) + 1
					args = append(args, pq.Array(ancestors))
					uConds = append(uConds, fmt.Sprintf("(%s.sp_value = ANY($%d))", uAlias, p1))
					utConds = append(utConds, fmt.Sprintf("(%s.sp_value = ANY($%d))", utAlias, p1))
				default:
					p1 := len(args) + 1
					args = append(args, v)
					uConds = append(uConds, fmt.Sprintf("(%s.sp_value = $%d)", uAlias, p1))
					utConds = append(utConds, fmt.Sprintf("(%s.sp_value = $%d)", utAlias, p1))
				}
			}
			if len(uConds) > 0 {
				whereExtra += fmt.Sprintf(
					" AND (EXISTS (SELECT 1 FROM spidx_uri %s WHERE %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND (%s)) OR EXISTS (SELECT 1 FROM spidx_token %s WHERE %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND (%s)))",
					uAlias, uAlias, uAlias, uAlias, uAlias, spIdx, strings.Join(uConds, " OR "),
					utAlias, utAlias, utAlias, utAlias, utAlias, spIdx, strings.Join(utConds, " OR "),
				)
			}
		}
	}

	// 6. Chained search parameters (e.g. Observation?patient.name=Smith, Observation?subject:Patient.name:exact=Smith, Observation?patient.organization=Organization/org-1)
	for _, chained := range params.Chained {
		joinIdx++
		refAlias := fmt.Sprintf("crf%d", joinIdx)
		targetAlias := fmt.Sprintf("ctg%d", joinIdx)

		targetParam, targetMod := splitParamModifier(chained.TargetParam)
		if chained.TargetModifier != "" {
			targetMod = chained.TargetModifier
		}

		paramType := chained.ParamType
		if paramType == "" {
			switch {
			case targetParam == "_id":
				paramType = "id"
			case knownStringSearchParams[targetParam]:
				paramType = "string"
			case knownDateSearchParams[targetParam]:
				paramType = "date"
			case knownQuantitySearchParams[targetParam]:
				paramType = "quantity"
			case knownRefSearchParams[targetParam]:
				paramType = "reference"
			default:
				paramType = "token"
			}
		}

		if paramType == "id" {
			var targetIDs []string
			for part := range strings.SplitSeq(chained.Value, ",") {
				tid := strings.TrimSpace(part)
				if tid == "" {
					continue
				}
				if slashIdx := strings.LastIndex(tid, "/"); slashIdx != -1 {
					tid = tid[slashIdx+1:]
				}
				targetIDs = append(targetIDs, tid)
			}
			if len(targetIDs) == 0 {
				continue
			}
			if chained.TargetType != "" {
				joins = append(joins, fmt.Sprintf(
					"JOIN spidx_reference %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND (%s.target_type = $%d OR %s.target_type = '') AND %s.target_id = ANY($%d)",
					refAlias, refAlias, refAlias, refAlias, refAlias, len(args)+1, refAlias, len(args)+2, refAlias, refAlias, len(args)+3,
				))
				args = append(args, chained.RefParam, chained.TargetType, pq.Array(targetIDs))
			} else {
				joins = append(joins, fmt.Sprintf(
					"JOIN spidx_reference %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND %s.target_id = ANY($%d)",
					refAlias, refAlias, refAlias, refAlias, refAlias, len(args)+1, refAlias, len(args)+2,
				))
				args = append(args, chained.RefParam, pq.Array(targetIDs))
			}
			continue
		}

		var targetTypeCond string
		if chained.TargetType != "" {
			joins = append(joins, fmt.Sprintf(
				"JOIN spidx_reference %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d AND (%s.target_type = $%d OR %s.target_type = '')",
				refAlias, refAlias, refAlias, refAlias, refAlias, len(args)+1, refAlias, len(args)+2, refAlias,
			))
			args = append(args, chained.RefParam, chained.TargetType)
			targetTypeCond = fmt.Sprintf("%s.res_type = $%d", targetAlias, len(args))
		} else {
			joins = append(joins, fmt.Sprintf(
				"JOIN spidx_reference %s ON %s.tenant_id = r.tenant_id AND %s.res_type = r.res_type AND %s.res_id = r.res_id AND %s.sp_name = $%d",
				refAlias, refAlias, refAlias, refAlias, refAlias, len(args)+1,
			))
			args = append(args, chained.RefParam)
			targetTypeCond = fmt.Sprintf("%s.res_type = %s.target_type", targetAlias, refAlias)
		}

		switch paramType {
		case "token":
			spIdx := len(args) + 1
			args = append(args, targetParam)
			var orConds []string
			for part := range strings.SplitSeq(chained.Value, ",") {
				item := strings.TrimSpace(part)
				if item == "" {
					continue
				}
				if parts := strings.SplitN(item, "|", 2); len(parts) == 2 {
					sys, code := parts[0], parts[1]
					if sys == "" {
						orConds = append(orConds, fmt.Sprintf("(%s.sp_system = '' AND %s.sp_value = $%d)", targetAlias, targetAlias, len(args)+1))
						args = append(args, code)
					} else if code == "" {
						orConds = append(orConds, fmt.Sprintf("(%s.sp_system = $%d)", targetAlias, len(args)+1))
						args = append(args, sys)
					} else {
						orConds = append(orConds, fmt.Sprintf("(%s.sp_system = $%d AND %s.sp_value = $%d)", targetAlias, len(args)+1, targetAlias, len(args)+2))
						args = append(args, sys, code)
					}
				} else if isEnumTokenParam(targetParam) {
					normVal := strings.ToLower(strings.ReplaceAll(item, "_", "-"))
					upperVal := strings.ToUpper(strings.ReplaceAll(item, "-", "_"))
					orConds = append(orConds, fmt.Sprintf("(%s.sp_value IN ($%d, $%d, $%d))", targetAlias, len(args)+1, len(args)+2, len(args)+3))
					args = append(args, item, normVal, upperVal)
				} else {
					orConds = append(orConds, fmt.Sprintf("(%s.sp_value = $%d)", targetAlias, len(args)+1))
					args = append(args, item)
				}
			}
			if len(orConds) == 0 {
				continue
			}
			if targetMod == "not" {
				whereExtra += fmt.Sprintf(
					" AND NOT EXISTS (SELECT 1 FROM spidx_token %s WHERE %s.tenant_id = r.tenant_id AND %s AND %s.res_id = %s.target_id AND %s.sp_name = $%d AND (%s))",
					targetAlias, targetAlias, targetTypeCond, targetAlias, refAlias, targetAlias, spIdx, strings.Join(orConds, " OR "),
				)
			} else {
				joins = append(joins, fmt.Sprintf(
					"JOIN spidx_token %s ON %s.tenant_id = r.tenant_id AND %s AND %s.res_id = %s.target_id AND %s.sp_name = $%d AND (%s)",
					targetAlias, targetAlias, targetTypeCond, targetAlias, refAlias, targetAlias, spIdx, strings.Join(orConds, " OR "),
				))
			}
		case "date":
			if dOp, err := ParseDateOp(chained.Value); err == nil {
				spIdx := len(args) + 1
				args = append(args, targetParam)
				cond, dateArgs := buildDateCondition(targetAlias, dOp, len(args))
				args = append(args, dateArgs...)
				joins = append(joins, fmt.Sprintf(
					"JOIN spidx_date %s ON %s.tenant_id = r.tenant_id AND %s AND %s.res_id = %s.target_id AND %s.sp_name = $%d AND %s",
					targetAlias, targetAlias, targetTypeCond, targetAlias, refAlias, targetAlias, spIdx, cond,
				))
			}
		case "quantity":
			if qOp, err := ParseQuantityOp(chained.Value); err == nil {
				spIdx := len(args) + 1
				args = append(args, targetParam)
				cond, qArgs := buildQuantityCondition(targetAlias, qOp, len(args))
				args = append(args, qArgs...)
				joins = append(joins, fmt.Sprintf(
					"JOIN spidx_quantity %s ON %s.tenant_id = r.tenant_id AND %s AND %s.res_id = %s.target_id AND %s.sp_name = $%d AND %s",
					targetAlias, targetAlias, targetTypeCond, targetAlias, refAlias, targetAlias, spIdx, cond,
				))
			}
		case "reference":
			spIdx := len(args) + 1
			args = append(args, targetParam)
			var orConds []string
			for part := range strings.SplitSeq(chained.Value, ",") {
				item := strings.TrimSpace(part)
				if item == "" {
					continue
				}
				if parts := strings.SplitN(item, "/", 2); len(parts) == 2 {
					orConds = append(orConds, fmt.Sprintf("(%s.target_type = $%d AND %s.target_id = $%d)", targetAlias, len(args)+1, targetAlias, len(args)+2))
					args = append(args, parts[0], parts[1])
				} else if targetMod != "" {
					orConds = append(orConds, fmt.Sprintf("(%s.target_type = $%d AND %s.target_id = $%d)", targetAlias, len(args)+1, targetAlias, len(args)+2))
					args = append(args, targetMod, item)
				} else {
					orConds = append(orConds, fmt.Sprintf("(%s.target_id = $%d)", targetAlias, len(args)+1))
					args = append(args, item)
				}
			}
			if len(orConds) > 0 {
				joins = append(joins, fmt.Sprintf(
					"JOIN spidx_reference %s ON %s.tenant_id = r.tenant_id AND %s AND %s.res_id = %s.target_id AND %s.sp_name = $%d AND (%s)",
					targetAlias, targetAlias, targetTypeCond, targetAlias, refAlias, targetAlias, spIdx, strings.Join(orConds, " OR "),
				))
			}
		default: // string
			spIdx := len(args) + 1
			args = append(args, targetParam)
			var orConds []string
			for part := range strings.SplitSeq(chained.Value, ",") {
				v := strings.ToLower(strings.TrimSpace(part))
				if v == "" {
					continue
				}
				switch targetMod {
				case "exact":
					orConds = append(orConds, fmt.Sprintf("%s.sp_value = $%d", targetAlias, len(args)+1))
					args = append(args, v)
				case "contains":
					orConds = append(orConds, fmt.Sprintf(`%s.sp_value LIKE $%d ESCAPE '\'`, targetAlias, len(args)+1))
					args = append(args, "%"+escapeLikePattern(v)+"%")
				default:
					orConds = append(orConds, fmt.Sprintf(`%s.sp_value LIKE $%d ESCAPE '\'`, targetAlias, len(args)+1))
					args = append(args, escapeLikePattern(v)+"%")
				}
			}
			if len(orConds) > 0 {
				joins = append(joins, fmt.Sprintf(
					"JOIN spidx_string %s ON %s.tenant_id = r.tenant_id AND %s AND %s.res_id = %s.target_id AND %s.sp_name = $%d AND (%s)",
					targetAlias, targetAlias, targetTypeCond, targetAlias, refAlias, targetAlias, spIdx, strings.Join(orConds, " OR "),
				))
			}
		}
	}

	// 6b. Reverse chained search parameters (_has:<SourceType>:<RefParam>:<SearchParam>=<Value>)
	for _, hp := range params.Has {
		cond, hasArgs := buildHasExistsCondition(hp, len(args))
		if cond != "" {
			whereExtra += " AND " + cond
			args = append(args, hasArgs...)
		}
	}

	// 6c. Cohort membership search (_list=<listId>)
	for _, rawListVal := range params.List {
		var listIDs []string
		for part := range strings.SplitSeq(rawListVal, ",") {
			lid := strings.TrimSpace(part)
			if lid == "" {
				continue
			}
			if slashIdx := strings.LastIndex(lid, "/"); slashIdx != -1 {
				lid = lid[slashIdx+1:]
			}
			listIDs = append(listIDs, lid)
		}
		if len(listIDs) == 0 {
			continue
		}
		listIdx := len(args) + 1
		args = append(args, pq.Array(listIDs))
		whereExtra += fmt.Sprintf(
			" AND EXISTS (SELECT 1 FROM spidx_reference lrf JOIN fhir_resource lr ON lr.tenant_id = lrf.tenant_id AND lr.res_type = lrf.res_type AND lr.res_id = lrf.res_id AND lr.is_deleted = FALSE WHERE lrf.tenant_id = r.tenant_id AND lrf.res_type IN ('List', 'Group') AND lrf.res_id = ANY($%d) AND lrf.sp_name IN ('item', 'member', 'entity') AND lrf.target_id = r.res_id AND (lrf.target_type = r.res_type OR lrf.target_type = ''))",
			listIdx,
		)
	}

	// 6d. Composite token$quantity search parameters (code-value-quantity, component-code-value-quantity, combo-code-value-quantity)
	for _, orGroup := range params.CompositeTokenQuantities {
		var orParts []string
		for _, cp := range orGroup {
			switch cp.ParamName {
			case "component-code-value-quantity":
				compKey := "comp-cvq:" + cp.TokenCode
				if cp.TokenSystem != "" {
					compKey = "comp-cvq:" + cp.TokenSystem + "|" + cp.TokenCode
				}
				spIdx := len(args) + 1
				args = append(args, compKey)
				qCond, qArgs := buildQuantityCondition("cq", cp.Quantity, len(args))
				args = append(args, qArgs...)
				orParts = append(orParts, fmt.Sprintf(
					"EXISTS (SELECT 1 FROM spidx_quantity cq WHERE cq.tenant_id = r.tenant_id AND cq.res_type = r.res_type AND cq.res_id = r.res_id AND cq.sp_name = $%d AND %s)",
					spIdx, qCond,
				))
			case "combo-code-value-quantity":
				compKey := "comp-cvq:" + cp.TokenCode
				if cp.TokenSystem != "" {
					compKey = "comp-cvq:" + cp.TokenSystem + "|" + cp.TokenCode
				}
				spCompIdx := len(args) + 1
				args = append(args, compKey)
				qCompCond, qCompArgs := buildQuantityCondition("cq1", cp.Quantity, len(args))
				args = append(args, qCompArgs...)

				var tokCond string
				if cp.TokenSystem != "" {
					tokCond = fmt.Sprintf("(ct.sp_system = $%d AND ct.sp_value = $%d)", len(args)+1, len(args)+2)
					args = append(args, cp.TokenSystem, cp.TokenCode)
				} else {
					tokCond = fmt.Sprintf("(ct.sp_value = $%d)", len(args)+1)
					args = append(args, cp.TokenCode)
				}
				qTopCond, qTopArgs := buildQuantityCondition("cq2", cp.Quantity, len(args))
				args = append(args, qTopArgs...)
				orParts = append(orParts, fmt.Sprintf(
					"(EXISTS (SELECT 1 FROM spidx_quantity cq1 WHERE cq1.tenant_id = r.tenant_id AND cq1.res_type = r.res_type AND cq1.res_id = r.res_id AND cq1.sp_name = $%d AND %s) OR EXISTS (SELECT 1 FROM spidx_token ct JOIN spidx_quantity cq2 ON cq2.tenant_id = ct.tenant_id AND cq2.res_type = ct.res_type AND cq2.res_id = ct.res_id WHERE ct.tenant_id = r.tenant_id AND ct.res_type = r.res_type AND ct.res_id = r.res_id AND ct.sp_name = 'code' AND %s AND cq2.sp_name = 'value-quantity' AND %s))",
					spCompIdx, qCompCond, tokCond, qTopCond,
				))
			default: // code-value-quantity
				var tokCond string
				if cp.TokenSystem != "" {
					tokCond = fmt.Sprintf("(ct.sp_system = $%d AND ct.sp_value = $%d)", len(args)+1, len(args)+2)
					args = append(args, cp.TokenSystem, cp.TokenCode)
				} else {
					tokCond = fmt.Sprintf("(ct.sp_value = $%d)", len(args)+1)
					args = append(args, cp.TokenCode)
				}
				qCond, qArgs := buildQuantityCondition("cq", cp.Quantity, len(args))
				args = append(args, qArgs...)
				orParts = append(orParts, fmt.Sprintf(
					"EXISTS (SELECT 1 FROM spidx_token ct JOIN spidx_quantity cq ON cq.tenant_id = ct.tenant_id AND cq.res_type = ct.res_type AND cq.res_id = ct.res_id WHERE ct.tenant_id = r.tenant_id AND ct.res_type = r.res_type AND ct.res_id = r.res_id AND ct.sp_name = 'code' AND %s AND cq.sp_name = 'value-quantity' AND %s)",
					tokCond, qCond,
				))
			}
		}
		if len(orParts) > 0 {
			whereExtra += " AND (" + strings.Join(orParts, " OR ") + ")"
		}
	}

	joinClause := strings.Join(joins, "\n")

	if params.ID != "" {
		var ids []string
		for part := range strings.SplitSeq(params.ID, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			if idx := strings.LastIndex(trimmed, "/"); idx != -1 {
				trimmed = trimmed[idx+1:]
			}
			ids = append(ids, trimmed)
		}
		if len(ids) == 1 {
			whereExtra += fmt.Sprintf(" AND r.res_id = $%d", len(args)+1)
			args = append(args, ids[0])
		} else if len(ids) > 1 {
			whereExtra += fmt.Sprintf(" AND r.res_id = ANY($%d)", len(args)+1)
			args = append(args, pq.Array(ids))
		}
	}
	for spName, isMissing := range params.Missing {
		tables := missingTablesForParam(spName)
		spIdx := len(args) + 1
		args = append(args, spName)
		existsParts := make([]string, 0, len(tables))
		for _, tbl := range tables {
			existsParts = append(existsParts, fmt.Sprintf(
				"EXISTS (SELECT 1 FROM %s m WHERE m.tenant_id = r.tenant_id AND m.res_type = r.res_type AND m.res_id = r.res_id AND m.sp_name = $%d)",
				tbl, spIdx,
			))
		}
		combinedExists := "(" + strings.Join(existsParts, " OR ") + ")"
		if isMissing {
			whereExtra += " AND NOT " + combinedExists
		} else {
			whereExtra += " AND " + combinedExists
		}
	}
	for _, luOp := range params.LastUpdated {
		cond, luArgs := buildTimestampColumnCondition("r.last_updated", luOp, len(args))
		whereExtra += " AND " + cond
		args = append(args, luArgs...)
	}

	// Count query
	var total int
	if params.TotalMode != "none" || params.Summary == "count" {
		countSQL := fmt.Sprintf(
			"SELECT COUNT(DISTINCT r.res_id) FROM fhir_resource r %s WHERE r.tenant_id = $1 AND r.res_type = $2 AND r.is_deleted = FALSE%s",
			joinClause, whereExtra,
		)
		if err := exec.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
			return nil, fmt.Errorf("count query: %w", err)
		}
	}
	if params.Summary == "count" {
		return &SearchResults{
			Total: total,
		}, nil
	}

	// Data query with sorting and pagination
	var dataSQL string
	if len(params.Sort) == 0 {
		dataSQL = fmt.Sprintf(
			"SELECT DISTINCT r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto FROM fhir_resource r %s WHERE r.tenant_id = $1 AND r.res_type = $2 AND r.is_deleted = FALSE%s ORDER BY r.res_id LIMIT $%d OFFSET $%d",
			joinClause, whereExtra, len(args)+1, len(args)+2,
		)
	} else {
		orderClause, sortArgs := buildSortOrderClause(params.Sort, len(args))
		args = append(args, sortArgs...)
		dataSQL = fmt.Sprintf(
			"SELECT r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto FROM fhir_resource r %s WHERE r.tenant_id = $1 AND r.res_type = $2 AND r.is_deleted = FALSE%s GROUP BY r.tenant_id, r.res_type, r.res_id ORDER BY %s LIMIT $%d OFFSET $%d",
			joinClause, whereExtra, orderClause, len(args)+1, len(args)+2,
		)
	}
	args = append(args, params.Count, params.Offset)

	rows, err := exec.QueryContext(ctx, dataSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	var matches []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ResType, &r.ResID, &r.ResVersion, &r.LastUpdated, &r.ResourceProto); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		matches = append(matches, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}

	// 7. Resolve _include and _revinclude resources
	var includes []SearchResult
	if len(params.Includes) > 0 || len(params.RevIncludes) > 0 {
		includes, err = s.resolveIncludesWithExecutor(ctx, exec, params.TenantID, matches, params.Includes, params.RevIncludes)
		if err != nil {
			return nil, fmt.Errorf("resolve includes: %w", err)
		}
	}

	return &SearchResults{
		Matches:  matches,
		Includes: includes,
		Total:    total,
	}, nil
}

func buildSortOrderClause(sorts []SortSpec, startIdx int) (string, []any) {
	var parts []string
	var args []any
	for _, s := range sorts {
		dir := "ASC"
		agg := "MIN"
		if s.Desc {
			dir = "DESC"
			agg = "MAX"
		}
		switch {
		case s.Param == "_id":
			parts = append(parts, "r.res_id "+dir)
		case s.Param == "_lastUpdated":
			parts = append(parts, "r.last_updated "+dir)
		case knownDateSearchParams[s.Param]:
			col := "sp_low"
			if s.Desc {
				col = "sp_high"
			}
			idx := startIdx + len(args) + 1
			args = append(args, s.Param)
			parts = append(parts, fmt.Sprintf(
				"(SELECT %s(sd.%s) FROM spidx_date sd WHERE sd.tenant_id = r.tenant_id AND sd.res_type = r.res_type AND sd.res_id = r.res_id AND sd.sp_name = $%d) %s NULLS LAST",
				agg, col, idx, dir,
			))
		case knownQuantitySearchParams[s.Param]:
			idx := startIdx + len(args) + 1
			args = append(args, s.Param)
			parts = append(parts, fmt.Sprintf(
				"(SELECT %s(sq.sp_value) FROM spidx_quantity sq WHERE sq.tenant_id = r.tenant_id AND sq.res_type = r.res_type AND sq.res_id = r.res_id AND sq.sp_name = $%d) %s NULLS LAST",
				agg, idx, dir,
			))
		case knownStringSearchParams[s.Param]:
			idx := startIdx + len(args) + 1
			args = append(args, s.Param)
			parts = append(parts, fmt.Sprintf(
				"(SELECT %s(ss.sp_value) FROM spidx_string ss WHERE ss.tenant_id = r.tenant_id AND ss.res_type = r.res_type AND ss.res_id = r.res_id AND ss.sp_name = $%d) %s NULLS LAST",
				agg, idx, dir,
			))
		case knownRefSearchParams[s.Param]:
			idx := startIdx + len(args) + 1
			args = append(args, s.Param)
			parts = append(parts, fmt.Sprintf(
				"(SELECT %s(sr.target_id) FROM spidx_reference sr WHERE sr.tenant_id = r.tenant_id AND sr.res_type = r.res_type AND sr.res_id = r.res_id AND sr.sp_name = $%d) %s NULLS LAST",
				agg, idx, dir,
			))
		case knownURISearchParams[s.Param]:
			idx := startIdx + len(args) + 1
			args = append(args, s.Param)
			parts = append(parts, fmt.Sprintf(
				"(SELECT %s(su.sp_value) FROM spidx_uri su WHERE su.tenant_id = r.tenant_id AND su.res_type = r.res_type AND su.res_id = r.res_id AND su.sp_name = $%d) %s NULLS LAST",
				agg, idx, dir,
			))
		default:
			idx := startIdx + len(args) + 1
			args = append(args, s.Param)
			parts = append(parts, fmt.Sprintf(
				"(SELECT %s(st.sp_value) FROM spidx_token st WHERE st.tenant_id = r.tenant_id AND st.res_type = r.res_type AND st.res_id = r.res_id AND st.sp_name = $%d) %s NULLS LAST",
				agg, idx, dir,
			))
		}
	}
	parts = append(parts, "r.res_id ASC")
	return strings.Join(parts, ", "), args
}

func buildHasExistsCondition(hp HasParam, startIdx int) (string, []any) {
	paramType := hp.ParamType
	if paramType == "" {
		switch {
		case hp.SearchParam == "_id":
			paramType = "id"
		case knownStringSearchParams[hp.SearchParam]:
			paramType = "string"
		case knownDateSearchParams[hp.SearchParam]:
			paramType = "date"
		case knownQuantitySearchParams[hp.SearchParam]:
			paramType = "quantity"
		case knownRefSearchParams[hp.SearchParam]:
			paramType = "reference"
		default:
			paramType = "token"
		}
	}

	args := []any{hp.SourceType, hp.RefParam}
	srcIdx := startIdx + 1
	refIdx := startIdx + 2

	if paramType == "id" {
		idVal := strings.TrimSpace(hp.Value)
		if slashIdx := strings.LastIndex(idVal, "/"); slashIdx != -1 {
			idVal = idVal[slashIdx+1:]
		}
		args = append(args, idVal)
		idIdx := startIdx + len(args)
		return fmt.Sprintf(
			`EXISTS (SELECT 1 FROM spidx_reference hrf JOIN fhir_resource hr ON hr.tenant_id = hrf.tenant_id AND hr.res_type = hrf.res_type AND hr.res_id = hrf.res_id AND hr.is_deleted = FALSE WHERE hrf.tenant_id = r.tenant_id AND hrf.res_type = $%d AND hrf.sp_name = $%d AND hrf.res_id = $%d AND hrf.target_id = r.res_id AND (hrf.target_type = r.res_type OR hrf.target_type = ''))`,
			srcIdx, refIdx, idIdx,
		), args
	}

	spIdx := startIdx + 3
	args = append(args, hp.SearchParam)

	switch paramType {
	case "string":
		args = append(args, escapeLikePattern(strings.ToLower(strings.TrimSpace(hp.Value)))+"%")
		valIdx := startIdx + len(args)
		return fmt.Sprintf(
			`EXISTS (SELECT 1 FROM spidx_reference hrf JOIN fhir_resource hr ON hr.tenant_id = hrf.tenant_id AND hr.res_type = hrf.res_type AND hr.res_id = hrf.res_id AND hr.is_deleted = FALSE JOIN spidx_string hsp ON hsp.tenant_id = hrf.tenant_id AND hsp.res_type = hrf.res_type AND hsp.res_id = hrf.res_id AND hsp.sp_name = $%d AND hsp.sp_value LIKE $%d ESCAPE '\' WHERE hrf.tenant_id = r.tenant_id AND hrf.res_type = $%d AND hrf.sp_name = $%d AND hrf.target_id = r.res_id AND (hrf.target_type = r.res_type OR hrf.target_type = ''))`,
			spIdx, valIdx, srcIdx, refIdx,
		), args
	case "date":
		dOp, err := ParseDateOp(hp.Value)
		if err != nil {
			return "", nil
		}
		cond, dArgs := buildDateCondition("hsp", dOp, startIdx+len(args))
		args = append(args, dArgs...)
		return fmt.Sprintf(
			`EXISTS (SELECT 1 FROM spidx_reference hrf JOIN fhir_resource hr ON hr.tenant_id = hrf.tenant_id AND hr.res_type = hrf.res_type AND hr.res_id = hrf.res_id AND hr.is_deleted = FALSE JOIN spidx_date hsp ON hsp.tenant_id = hrf.tenant_id AND hsp.res_type = hrf.res_type AND hsp.res_id = hrf.res_id AND hsp.sp_name = $%d AND %s WHERE hrf.tenant_id = r.tenant_id AND hrf.res_type = $%d AND hrf.sp_name = $%d AND hrf.target_id = r.res_id AND (hrf.target_type = r.res_type OR hrf.target_type = ''))`,
			spIdx, cond, srcIdx, refIdx,
		), args
	case "quantity":
		qOp, err := ParseQuantityOp(hp.Value)
		if err != nil {
			return "", nil
		}
		cond, qArgs := buildQuantityCondition("hsp", qOp, startIdx+len(args))
		args = append(args, qArgs...)
		return fmt.Sprintf(
			`EXISTS (SELECT 1 FROM spidx_reference hrf JOIN fhir_resource hr ON hr.tenant_id = hrf.tenant_id AND hr.res_type = hrf.res_type AND hr.res_id = hrf.res_id AND hr.is_deleted = FALSE JOIN spidx_quantity hsp ON hsp.tenant_id = hrf.tenant_id AND hsp.res_type = hrf.res_type AND hsp.res_id = hrf.res_id AND hsp.sp_name = $%d AND %s WHERE hrf.tenant_id = r.tenant_id AND hrf.res_type = $%d AND hrf.sp_name = $%d AND hrf.target_id = r.res_id AND (hrf.target_type = r.res_type OR hrf.target_type = ''))`,
			spIdx, cond, srcIdx, refIdx,
		), args
	case "reference":
		refVal := strings.TrimSpace(hp.Value)
		if parts := strings.SplitN(refVal, "/", 2); len(parts) == 2 {
			args = append(args, parts[0], parts[1])
			tTypeIdx := startIdx + len(args) - 1
			tIDIdx := startIdx + len(args)
			return fmt.Sprintf(
				`EXISTS (SELECT 1 FROM spidx_reference hrf JOIN fhir_resource hr ON hr.tenant_id = hrf.tenant_id AND hr.res_type = hrf.res_type AND hr.res_id = hrf.res_id AND hr.is_deleted = FALSE JOIN spidx_reference hsp ON hsp.tenant_id = hrf.tenant_id AND hsp.res_type = hrf.res_type AND hsp.res_id = hrf.res_id AND hsp.sp_name = $%d AND hsp.target_type = $%d AND hsp.target_id = $%d WHERE hrf.tenant_id = r.tenant_id AND hrf.res_type = $%d AND hrf.sp_name = $%d AND hrf.target_id = r.res_id AND (hrf.target_type = r.res_type OR hrf.target_type = ''))`,
				spIdx, tTypeIdx, tIDIdx, srcIdx, refIdx,
			), args
		}
		args = append(args, refVal)
		tIDIdx := startIdx + len(args)
		return fmt.Sprintf(
			`EXISTS (SELECT 1 FROM spidx_reference hrf JOIN fhir_resource hr ON hr.tenant_id = hrf.tenant_id AND hr.res_type = hrf.res_type AND hr.res_id = hrf.res_id AND hr.is_deleted = FALSE JOIN spidx_reference hsp ON hsp.tenant_id = hrf.tenant_id AND hsp.res_type = hrf.res_type AND hsp.res_id = hrf.res_id AND hsp.sp_name = $%d AND hsp.target_id = $%d WHERE hrf.tenant_id = r.tenant_id AND hrf.res_type = $%d AND hrf.sp_name = $%d AND hrf.target_id = r.res_id AND (hrf.target_type = r.res_type OR hrf.target_type = ''))`,
			spIdx, tIDIdx, srcIdx, refIdx,
		), args
	case "uri":
		args = append(args, strings.TrimSpace(hp.Value))
		valIdx := startIdx + len(args)
		return fmt.Sprintf(
			`EXISTS (SELECT 1 FROM spidx_reference hrf JOIN fhir_resource hr ON hr.tenant_id = hrf.tenant_id AND hr.res_type = hrf.res_type AND hr.res_id = hrf.res_id AND hr.is_deleted = FALSE JOIN spidx_uri hsp ON hsp.tenant_id = hrf.tenant_id AND hsp.res_type = hrf.res_type AND hsp.res_id = hrf.res_id AND hsp.sp_name = $%d AND hsp.sp_value = $%d WHERE hrf.tenant_id = r.tenant_id AND hrf.res_type = $%d AND hrf.sp_name = $%d AND hrf.target_id = r.res_id AND (hrf.target_type = r.res_type OR hrf.target_type = ''))`,
			spIdx, valIdx, srcIdx, refIdx,
		), args
	default: // token
		var orConds []string
		for part := range strings.SplitSeq(hp.Value, ",") {
			item := strings.TrimSpace(part)
			if item == "" {
				continue
			}
			currIdx := startIdx + len(args)
			if parts := strings.SplitN(item, "|", 2); len(parts) == 2 {
				sys, code := parts[0], parts[1]
				if sys == "" {
					orConds = append(orConds, fmt.Sprintf("(hsp.sp_system = '' AND hsp.sp_value = $%d)", currIdx+1))
					args = append(args, code)
				} else if code == "" {
					orConds = append(orConds, fmt.Sprintf("(hsp.sp_system = $%d)", currIdx+1))
					args = append(args, sys)
				} else {
					orConds = append(orConds, fmt.Sprintf("(hsp.sp_system = $%d AND hsp.sp_value = $%d)", currIdx+1, currIdx+2))
					args = append(args, sys, code)
				}
			} else if isEnumTokenParam(hp.SearchParam) {
				normVal := strings.ToLower(strings.ReplaceAll(item, "_", "-"))
				upperVal := strings.ToUpper(strings.ReplaceAll(item, "-", "_"))
				orConds = append(orConds, fmt.Sprintf("(hsp.sp_value IN ($%d, $%d, $%d))", currIdx+1, currIdx+2, currIdx+3))
				args = append(args, item, normVal, upperVal)
			} else {
				orConds = append(orConds, fmt.Sprintf("(hsp.sp_value = $%d)", currIdx+1))
				args = append(args, item)
			}
		}
		if len(orConds) == 0 {
			return "", nil
		}
		return fmt.Sprintf(
			`EXISTS (SELECT 1 FROM spidx_reference hrf JOIN fhir_resource hr ON hr.tenant_id = hrf.tenant_id AND hr.res_type = hrf.res_type AND hr.res_id = hrf.res_id AND hr.is_deleted = FALSE JOIN spidx_token hsp ON hsp.tenant_id = hrf.tenant_id AND hsp.res_type = hrf.res_type AND hsp.res_id = hrf.res_id AND hsp.sp_name = $%d AND (%s) WHERE hrf.tenant_id = r.tenant_id AND hrf.res_type = $%d AND hrf.sp_name = $%d AND hrf.target_id = r.res_id AND (hrf.target_type = r.res_type OR hrf.target_type = ''))`,
			spIdx, strings.Join(orConds, " OR "), srcIdx, refIdx,
		), args
	}
}

// ResolveIncludes fetches referenced resources for _include and referring resources for _revinclude.
func (s *Store) ResolveIncludes(ctx context.Context, tenantID string, matches []SearchResult, includes []IncludeParam, revIncludes []IncludeParam) ([]SearchResult, error) {
	var res []SearchResult
	err := s.withTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		var txErr error
		res, txErr = s.resolveIncludesWithExecutor(ctx, tx, tenantID, matches, includes, revIncludes)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Store) resolveIncludesWithExecutor(ctx context.Context, exec queryExecer, tenantID string, matches []SearchResult, includes []IncludeParam, revIncludes []IncludeParam) ([]SearchResult, error) {
	if len(matches) == 0 || (len(includes) == 0 && len(revIncludes) == 0) {
		return nil, nil
	}

	matchedSet := make(map[string]bool, len(matches))
	for _, m := range matches {
		matchedSet[m.ResType+"/"+m.ResID] = true
	}

	var includedResults []SearchResult
	frontier := matches

	const maxIncludePasses = 4
	for pass := 0; pass < maxIncludePasses && len(frontier) > 0; pass++ {
		var nextFrontier []SearchResult

		// Group frontier IDs by resource type
		idsByType := make(map[string][]string)
		var allFrontierIDs []string
		var frontierTypes []string
		for _, m := range frontier {
			allFrontierIDs = append(allFrontierIDs, m.ResID)
			if m.ResType != "" {
				if _, ok := idsByType[m.ResType]; !ok {
					frontierTypes = append(frontierTypes, m.ResType)
				}
				idsByType[m.ResType] = append(idsByType[m.ResType], m.ResID)
			}
		}

		// Handle _include (Source:param[:Target])
		for _, inc := range includes {
			if pass > 0 && !inc.Iterate {
				continue
			}
			sourceIDs := idsByType[inc.SourceType]
			if inc.SourceType == "*" {
				sourceIDs = allFrontierIDs
			}
			if len(sourceIDs) == 0 {
				continue
			}
			query := `
				SELECT DISTINCT r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto
				FROM spidx_reference rf
				JOIN fhir_resource r ON r.tenant_id = rf.tenant_id AND r.res_type = rf.target_type AND r.res_id = rf.target_id AND r.is_deleted = FALSE
				WHERE rf.tenant_id = $1 AND rf.res_type = $2 AND rf.res_id = ANY($3) AND rf.sp_name = $4
			`
			args := []any{tenantID, inc.SourceType, pq.Array(sourceIDs), inc.ParamName}
			if inc.TargetType != "" {
				query += " AND rf.target_type = $5"
				args = append(args, inc.TargetType)
			}

			rows, err := exec.QueryContext(ctx, query, args...)
			if err != nil {
				return nil, fmt.Errorf("query include %s:%s: %w", inc.SourceType, inc.ParamName, err)
			}
			for rows.Next() {
				var r SearchResult
				if err := rows.Scan(&r.ResType, &r.ResID, &r.ResVersion, &r.LastUpdated, &r.ResourceProto); err != nil {
					rows.Close()
					return nil, err
				}
				key := r.ResType + "/" + r.ResID
				if !matchedSet[key] {
					matchedSet[key] = true
					includedResults = append(includedResults, r)
					nextFrontier = append(nextFrontier, r)
				}
			}
			rows.Close()
		}

		// Handle _revinclude (Source:param[:Target])
		for _, rev := range revIncludes {
			if pass > 0 && !rev.Iterate {
				continue
			}
			targetIDs := allFrontierIDs
			if rev.TargetType != "" {
				targetIDs = idsByType[rev.TargetType]
			}
			if len(targetIDs) == 0 {
				continue
			}
			query := `
				SELECT DISTINCT r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto
				FROM spidx_reference rf
				JOIN fhir_resource r ON r.tenant_id = rf.tenant_id AND r.res_type = rf.res_type AND r.res_id = rf.res_id AND r.is_deleted = FALSE
				WHERE rf.tenant_id = $1 AND rf.target_id = ANY($2) AND rf.res_type = $3 AND rf.sp_name = $4
			`
			args := []any{tenantID, pq.Array(targetIDs), rev.SourceType, rev.ParamName}
			if rev.TargetType != "" {
				query += fmt.Sprintf(" AND (rf.target_type = $%d OR rf.target_type = '')", len(args)+1)
				args = append(args, rev.TargetType)
			} else if len(frontierTypes) > 0 {
				query += fmt.Sprintf(" AND (rf.target_type = ANY($%d) OR rf.target_type = '')", len(args)+1)
				args = append(args, pq.Array(frontierTypes))
			}
			rows, err := exec.QueryContext(ctx, query, args...)
			if err != nil {
				return nil, fmt.Errorf("query revinclude %s:%s: %w", rev.SourceType, rev.ParamName, err)
			}
			for rows.Next() {
				var r SearchResult
				if err := rows.Scan(&r.ResType, &r.ResID, &r.ResVersion, &r.LastUpdated, &r.ResourceProto); err != nil {
					rows.Close()
					return nil, err
				}
				key := r.ResType + "/" + r.ResID
				if !matchedSet[key] {
					matchedSet[key] = true
					includedResults = append(includedResults, r)
					nextFrontier = append(nextFrontier, r)
				}
			}
			rows.Close()
		}

		frontier = nextFrontier
	}

	return includedResults, nil
}

func escapeLikePattern(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func buildTimestampColumnCondition(colExpr string, op DateOp, startIdx int) (string, []any) {
	low := op.Value
	high := op.High
	if high.IsZero() && low.Hour() == 0 && low.Minute() == 0 && low.Second() == 0 && low.Nanosecond() == 0 {
		high = low.Add(24 * time.Hour)
	}
	switch op.Prefix {
	case "lt", "eb":
		return fmt.Sprintf("%s < $%d", colExpr, startIdx+1), []any{low}
	case "le":
		if high.After(low) {
			return fmt.Sprintf("%s < $%d", colExpr, startIdx+1), []any{high}
		}
		return fmt.Sprintf("%s <= $%d", colExpr, startIdx+1), []any{low}
	case "gt", "sa":
		if high.After(low) {
			return fmt.Sprintf("%s >= $%d", colExpr, startIdx+1), []any{high}
		}
		return fmt.Sprintf("%s > $%d", colExpr, startIdx+1), []any{low}
	case "ge":
		return fmt.Sprintf("%s >= $%d", colExpr, startIdx+1), []any{low}
	case "ne":
		if high.After(low) {
			return fmt.Sprintf("(%s < $%d OR %s >= $%d)", colExpr, startIdx+1, colExpr, startIdx+2), []any{low, high}
		}
		return fmt.Sprintf("%s != $%d", colExpr, startIdx+1), []any{low}
	default: // "eq"
		if high.After(low) {
			return fmt.Sprintf("(%s >= $%d AND %s < $%d)", colExpr, startIdx+1, colExpr, startIdx+2), []any{low, high}
		}
		return fmt.Sprintf("%s = $%d", colExpr, startIdx+1), []any{low}
	}
}

func buildDateCondition(alias string, op DateOp, startIdx int) (string, []any) {
	// spidx_date has columns: sp_low, sp_high (TIMESTAMPTZ)
	low := op.Value
	high := op.High
	switch op.Prefix {
	case "lt", "eb": // less than / ends before
		return fmt.Sprintf("%s.sp_high < $%d", alias, startIdx+1), []any{low}
	case "le": // less or equal
		if high.After(low) {
			return fmt.Sprintf("%s.sp_low < $%d", alias, startIdx+1), []any{high}
		}
		return fmt.Sprintf("%s.sp_low <= $%d", alias, startIdx+1), []any{low}
	case "gt": // greater than
		if high.After(low) {
			return fmt.Sprintf("%s.sp_high >= $%d", alias, startIdx+1), []any{high}
		}
		return fmt.Sprintf("%s.sp_high > $%d", alias, startIdx+1), []any{low}
	case "sa": // starts after
		if high.After(low) {
			return fmt.Sprintf("%s.sp_low >= $%d", alias, startIdx+1), []any{high}
		}
		return fmt.Sprintf("%s.sp_low > $%d", alias, startIdx+1), []any{low}
	case "ge": // greater or equal
		return fmt.Sprintf("%s.sp_high >= $%d", alias, startIdx+1), []any{low}
	case "ne": // not equal
		if high.After(low) {
			return fmt.Sprintf("(%s.sp_high < $%d OR %s.sp_low >= $%d)", alias, startIdx+1, alias, startIdx+2), []any{low, high}
		}
		return fmt.Sprintf("(%s.sp_high < $%d OR %s.sp_low > $%d)", alias, startIdx+1, alias, startIdx+1), []any{low}
	default: // "eq" (or unspecified)
		// Overlaps the target date interval [low, high) or point [low, low]
		if high.After(low) {
			return fmt.Sprintf("%s.sp_low < $%d AND %s.sp_high >= $%d", alias, startIdx+1, alias, startIdx+2), []any{high, low}
		}
		return fmt.Sprintf("%s.sp_low <= $%d AND %s.sp_high >= $%d", alias, startIdx+1, alias, startIdx+1), []any{low}
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
	case "ne":
		compSQL = "!="
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

// PatientEverything implements the HL7 FHIR R4 Patient/$everything operation,
// returning the target Patient resource plus all compartment-linked resources
// (Encounter, Condition, Observation, Practitioner, etc.) via spidx_reference.
func (s *Store) PatientEverything(ctx context.Context, params PatientEverythingParams) (*SearchResults, error) {
	if params.Count <= 0 {
		params.Count = 100
	}
	if params.Count > 1000 {
		params.Count = 1000
	}
	offset := max(params.Offset, 0)

	var out *SearchResults
	err := s.withTenantTx(ctx, params.TenantID, func(tx *sql.Tx) error {
		patRec, err := s.readResourceWithMetaInTx(ctx, tx, params.TenantID, "Patient", params.PatientID)
		if err != nil {
			return err
		}

		includePatient := true
		if len(params.ResourceTypes) > 0 {
			includePatient = false
			for _, rt := range params.ResourceTypes {
				if strings.EqualFold(rt, "Patient") {
					includePatient = true
					break
				}
			}
		}
		if params.Since != nil && patRec.LastUpdated.Before(*params.Since) {
			includePatient = false
		}

		var matches []SearchResult
		if includePatient {
			matches = append(matches, SearchResult{
				ResType:       "Patient",
				ResID:         params.PatientID,
				ResVersion:    patRec.ResVersion,
				LastUpdated:   patRec.LastUpdated,
				ResourceProto: patRec.ResourceProto,
			})
		}

		// Build compartment query against spidx_reference:
		// 1. Resources referencing this Patient (e.g., Condition, Encounter, Observation)
		// 2. Resources directly referenced by this Patient (e.g., generalPractitioner, managingOrganization)
		// 3. Resources referenced by clinical resources in this Patient's compartment (e.g., Encounter participant Practitioner)
		whereConds := []string{
			"r.tenant_id = $1",
			"r.is_deleted = FALSE",
			"r.res_type != 'Patient'",
			`(
				EXISTS (
					SELECT 1 FROM spidx_reference rf
					WHERE rf.tenant_id = r.tenant_id
					  AND rf.res_type = r.res_type
					  AND rf.res_id = r.res_id
					  AND rf.target_id = $2
					  AND (rf.target_type = 'Patient' OR rf.target_type = '')
				)
				OR EXISTS (
					SELECT 1 FROM spidx_reference rf
					WHERE rf.tenant_id = r.tenant_id
					  AND rf.res_type = 'Patient'
					  AND rf.res_id = $2
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
						WHERE rf_clin.tenant_id = r.tenant_id
						  AND rf_clin.target_type = r.res_type
						  AND rf_clin.target_id = r.res_id
						  AND rf_pat.target_id = $2
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
						WHERE rf_prov.tenant_id = r.tenant_id
						  AND rf_prov.res_type = 'Provenance'
						  AND rf_prov.res_id = r.res_id
						  AND rf_prov.sp_name = 'target'
						  AND rf_pat.target_id = $2
						  AND (rf_pat.target_type = 'Patient' OR rf_pat.target_type = '')
					)
				)
			)`,
		}
		args := []any{params.TenantID, params.PatientID}

		if len(params.ResourceTypes) > 0 {
			whereConds = append(whereConds, fmt.Sprintf("r.res_type = ANY($%d)", len(args)+1))
			args = append(args, pq.Array(params.ResourceTypes))
		}
		if params.Since != nil {
			whereConds = append(whereConds, fmt.Sprintf("r.last_updated >= $%d", len(args)+1))
			args = append(args, *params.Since)
		}

		whereClause := strings.Join(whereConds, " AND ")

		var compTotal int
		countSQL := "SELECT COUNT(*) FROM fhir_resource r WHERE " + whereClause
		if err := tx.QueryRowContext(ctx, countSQL, args...).Scan(&compTotal); err != nil {
			return fmt.Errorf("patient everything count: %w", err)
		}

		dataSQL := fmt.Sprintf(
			"SELECT r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto FROM fhir_resource r WHERE %s ORDER BY r.res_type ASC, r.res_id ASC LIMIT $%d OFFSET $%d",
			whereClause, len(args)+1, len(args)+2,
		)
		args = append(args, params.Count, offset)

		rows, err := tx.QueryContext(ctx, dataSQL, args...)
		if err != nil {
			return fmt.Errorf("patient everything query: %w", err)
		}
		defer rows.Close()

		var includes []SearchResult
		for rows.Next() {
			var r SearchResult
			if err := rows.Scan(&r.ResType, &r.ResID, &r.ResVersion, &r.LastUpdated, &r.ResourceProto); err != nil {
				return fmt.Errorf("scan patient everything row: %w", err)
			}
			includes = append(includes, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("patient everything rows: %w", err)
		}

		out = &SearchResults{
			Matches:  matches,
			Includes: includes,
			Total:    len(matches) + compTotal,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

var patientCompartmentTypes = map[string]bool{
	"Account":                     true,
	"AdverseEvent":                true,
	"AllergyIntolerance":          true,
	"Appointment":                 true,
	"AppointmentResponse":         true,
	"AuditEvent":                  true,
	"Basic":                       true,
	"BodyStructure":               true,
	"CarePlan":                    true,
	"CareTeam":                    true,
	"ChargeItem":                  true,
	"Claim":                       true,
	"ClaimResponse":               true,
	"ClinicalImpression":          true,
	"Communication":               true,
	"CommunicationRequest":        true,
	"Composition":                 true,
	"Condition":                   true,
	"Consent":                     true,
	"Coverage":                    true,
	"CoverageEligibilityRequest":  true,
	"CoverageEligibilityResponse": true,
	"DetectedIssue":               true,
	"Device":                      true,
	"DeviceRequest":               true,
	"DeviceUseStatement":          true,
	"DiagnosticReport":            true,
	"DocumentManifest":            true,
	"DocumentReference":           true,
	"Encounter":                   true,
	"EnrollmentRequest":           true,
	"EpisodeOfCare":               true,
	"ExplanationOfBenefit":        true,
	"FamilyMemberHistory":         true,
	"Flag":                        true,
	"Goal":                        true,
	"ImagingStudy":                true,
	"Immunization":                true,
	"ImmunizationEvaluation":      true,
	"ImmunizationRecommendation":  true,
	"Invoice":                     true,
	"List":                        true,
	"MeasureReport":               true,
	"Media":                       true,
	"MedicationAdministration":    true,
	"MedicationDispense":          true,
	"MedicationRequest":           true,
	"MedicationStatement":         true,
	"MolecularSequence":           true,
	"NutritionOrder":              true,
	"Observation":                 true,
	"PaymentNotice":               true,
	"PaymentReconciliation":       true,
	"Person":                      true,
	"Procedure":                   true,
	"Provenance":                  true,
	"QuestionnaireResponse":       true,
	"RelatedPerson":               true,
	"RequestGroup":                true,
	"ResearchSubject":             true,
	"RiskAssessment":              true,
	"Schedule":                    true,
	"ServiceRequest":              true,
	"Specimen":                    true,
	"SupplyDelivery":              true,
	"SupplyRequest":               true,
	"Task":                        true,
	"VisionPrescription":          true,
}

// IsPatientCompartmentResourceType returns true if the resource type is a clinical
// resource scoped to the HL7 FHIR R4 Patient compartment (CompartmentDefinition-patient).
func IsPatientCompartmentResourceType(resType string) bool {
	return patientCompartmentTypes[resType]
}

// BelongsToPatientCompartment checks whether an existing resource in Postgres belongs
// to the given patientID's compartment. If the resource does not exist in fhir_resource,
// it returns (true, nil) so the caller can proceed to normal 404 or create-on-update handling.
func (s *Store) BelongsToPatientCompartment(ctx context.Context, tenantID, resType, resID, patientID string) (bool, error) {
	if strings.EqualFold(resType, "Patient") {
		return resID == patientID, nil
	}
	if !IsPatientCompartmentResourceType(resType) {
		return true, nil
	}

	var inCompartment bool
	err := s.withTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		var isDeleted bool
		err := tx.QueryRowContext(ctx, `
			SELECT is_deleted FROM fhir_resource
			WHERE tenant_id = $1 AND res_type = $2 AND res_id = $3
		`, tenantID, resType, resID).Scan(&isDeleted)
		if errors.Is(err, sql.ErrNoRows) {
			inCompartment = false
			return nil
		}
		if err != nil {
			return err
		}

		return tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM spidx_reference
				WHERE tenant_id = $1
				  AND res_type = $2
				  AND res_id = $3
				  AND target_id = $4
				  AND (target_type = 'Patient' OR target_type = '')
			) OR (
				$2 = 'Provenance'
				AND EXISTS (
					SELECT 1 FROM spidx_reference rf_prov
					JOIN spidx_reference rf_pat
					  ON rf_pat.tenant_id = rf_prov.tenant_id
					 AND rf_pat.res_type = rf_prov.target_type
					 AND rf_pat.res_id = rf_prov.target_id
					WHERE rf_prov.tenant_id = $1
					  AND rf_prov.res_type = 'Provenance'
					  AND rf_prov.res_id = $3
					  AND rf_prov.sp_name = 'target'
					  AND rf_pat.target_id = $4
					  AND (rf_pat.target_type = 'Patient' OR rf_pat.target_type = '')
				)
			)
		`, tenantID, resType, resID, patientID).Scan(&inCompartment)
	})
	if err != nil {
		return false, err
	}
	return inCompartment, nil
}

// IndexesBelongToPatientCompartment verifies whether the extracted SearchIndexes of an incoming
// resource link it to the expected patientID compartment.
func IndexesBelongToPatientCompartment(resType, resID string, idx *SearchIndexes, patientID string) bool {
	if strings.EqualFold(resType, "Patient") {
		return resID == "" || resID == patientID
	}
	if !IsPatientCompartmentResourceType(resType) {
		return true
	}
	if idx == nil {
		return false
	}
	matchedOwnPatient := false
	for _, ref := range idx.References {
		if ref.TargetType == "Patient" || ref.TargetType == "" || ref.SpName == "patient" || ref.SpName == "subject" {
			if ref.TargetID != "" && ref.TargetID != patientID {
				return false
			}
			if ref.TargetID == patientID {
				matchedOwnPatient = true
			}
		}
	}
	return matchedOwnPatient
}

// uriHierarchyAncestors computes all hierarchical ancestor URI strings for the FHIR :above modifier.
// For example, "http://example.org/fhir/ValueSet/vs-1" returns:
//   - "http://example.org/fhir/ValueSet/vs-1"
//   - "http://example.org/fhir/ValueSet/"
//   - "http://example.org/fhir/ValueSet"
//   - "http://example.org/fhir/"
//   - "http://example.org/fhir"
//   - "http://example.org/"
//   - "http://example.org"
func uriHierarchyAncestors(rawURI string) []string {
	u := strings.TrimSpace(rawURI)
	if u == "" {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(u)
	add(strings.TrimSuffix(u, "/"))

	schemeEnd := 0
	if idx := strings.Index(u, "://"); idx != -1 {
		schemeEnd = idx + 3
	}
	curr := strings.TrimSuffix(u, "/")
	for {
		slashIdx := strings.LastIndexByte(curr, '/')
		if slashIdx == -1 || slashIdx < schemeEnd {
			break
		}
		prefix := curr[:slashIdx]
		add(prefix + "/")
		add(prefix)
		curr = prefix
	}
	return out
}

// missingTablesForParam returns the covering spidx_* table(s) where spName may be indexed.
// Choice-type [x] and multi-representation parameters check across all candidate tables so
// :missing=true only matches resources where the parameter is absent from every candidate index.
func missingTablesForParam(spName string) []string {
	switch spName {
	case "deceased", "onset", "abatement", "effective", "performed", "occurrence":
		return []string{"spidx_token", "spidx_date", "spidx_string", "spidx_quantity"}
	case "value":
		return []string{"spidx_quantity", "spidx_token", "spidx_string", "spidx_date"}
	case "medication", "code":
		return []string{"spidx_token", "spidx_reference"}
	case "questionnaire", "instantiates-canonical":
		return []string{"spidx_reference", "spidx_uri", "spidx_token"}
	}
	switch {
	case knownURISearchParams[spName]:
		return []string{"spidx_uri", "spidx_token"}
	case knownStringSearchParams[spName]:
		return []string{"spidx_string"}
	case knownDateSearchParams[spName]:
		return []string{"spidx_date"}
	case knownQuantitySearchParams[spName]:
		return []string{"spidx_quantity"}
	case knownRefSearchParams[spName]:
		return []string{"spidx_reference"}
	case knownTokenSearchParams[spName]:
		return []string{"spidx_token"}
	default:
		return []string{"spidx_token", "spidx_string", "spidx_date", "spidx_quantity", "spidx_reference", "spidx_uri"}
	}
}

// PatientMatchCandidateParams specifies index-backed pre-filtering parameters
// for Patient/$match and Patient/$member-match candidate retrieval.
type PatientMatchCandidateParams struct {
	TenantID            string
	RestrictedPatientID string   // Optional: restricts to a single Patient ID when patient-scoped
	ExtraCandidateIDs   []string // Optional: additional candidate Patient IDs (e.g. from matched Coverage beneficiaries)
	Identifiers         []string // Optional: identifier values (case-insensitive)
	Families            []string // Optional: normalized lowercase family names
	Givens              []string // Optional: normalized lowercase given names (matched by prefix)
	BirthDate           string   // Optional: YYYY-MM-DD birthDate
	Limit               int
}

// SearchPatientMatchCandidates pre-filters Patient resources in SQL using spidx_token (identifier),
// spidx_string (family, given), and spidx_date (birthdate) before scoring in Go.
func (s *Store) SearchPatientMatchCandidates(ctx context.Context, params PatientMatchCandidateParams) ([]SearchResult, error) {
	limit := params.Limit
	if limit <= 0 || limit > MaxSearchCount {
		limit = 500
	}

	whereConds := []string{
		"r.tenant_id = $1",
		"r.res_type = 'Patient'",
		"r.is_deleted = FALSE",
	}
	args := []any{params.TenantID}

	if params.RestrictedPatientID != "" {
		args = append(args, params.RestrictedPatientID)
		whereConds = append(whereConds, fmt.Sprintf("r.res_id = $%d", len(args)))
	}

	var orPreds []string
	if len(params.ExtraCandidateIDs) > 0 {
		args = append(args, pq.Array(params.ExtraCandidateIDs))
		orPreds = append(orPreds, fmt.Sprintf("r.res_id = ANY($%d)", len(args)))
	}
	if len(params.Identifiers) > 0 {
		lowerIDs := make([]string, 0, len(params.Identifiers))
		for _, id := range params.Identifiers {
			if trimmed := strings.ToLower(strings.TrimSpace(id)); trimmed != "" {
				lowerIDs = append(lowerIDs, trimmed)
			}
		}
		if len(lowerIDs) > 0 {
			args = append(args, pq.Array(lowerIDs))
			orPreds = append(orPreds, fmt.Sprintf(
				"EXISTS (SELECT 1 FROM spidx_token st WHERE st.tenant_id = r.tenant_id AND st.res_type = 'Patient' AND st.res_id = r.res_id AND st.sp_name = 'identifier' AND LOWER(st.sp_value) = ANY($%d))",
				len(args),
			))
		}
	}
	if len(params.Families) > 0 {
		lowerFams := make([]string, 0, len(params.Families))
		for _, fam := range params.Families {
			if trimmed := strings.ToLower(strings.TrimSpace(fam)); trimmed != "" {
				lowerFams = append(lowerFams, trimmed)
			}
		}
		if len(lowerFams) > 0 {
			args = append(args, pq.Array(lowerFams))
			orPreds = append(orPreds, fmt.Sprintf(
				"EXISTS (SELECT 1 FROM spidx_string sf WHERE sf.tenant_id = r.tenant_id AND sf.res_type = 'Patient' AND sf.res_id = r.res_id AND sf.sp_name = 'family' AND sf.sp_value = ANY($%d))",
				len(args),
			))
		}
	}
	for _, giv := range params.Givens {
		trimmed := strings.ToLower(strings.TrimSpace(giv))
		if trimmed == "" {
			continue
		}
		p1 := len(args) + 1
		p2 := len(args) + 2
		args = append(args, escapeLikePattern(trimmed)+"%", trimmed)
		orPreds = append(orPreds, fmt.Sprintf(
			`EXISTS (SELECT 1 FROM spidx_string sg WHERE sg.tenant_id = r.tenant_id AND sg.res_type = 'Patient' AND sg.res_id = r.res_id AND sg.sp_name = 'given' AND (sg.sp_value LIKE $%d ESCAPE '\' OR $%d LIKE sg.sp_value || '%%'))`,
			p1, p2,
		))
	}
	if params.BirthDate != "" {
		if dOp, err := ParseDateOp(params.BirthDate); err == nil {
			cond, dArgs := buildDateCondition("sd", dOp, len(args))
			args = append(args, dArgs...)
			orPreds = append(orPreds, fmt.Sprintf(
				"EXISTS (SELECT 1 FROM spidx_date sd WHERE sd.tenant_id = r.tenant_id AND sd.res_type = 'Patient' AND sd.res_id = r.res_id AND sd.sp_name = 'birthdate' AND %s)",
				cond,
			))
		}
	}

	if len(orPreds) > 0 {
		whereConds = append(whereConds, "("+strings.Join(orPreds, " OR ")+")")
	}

	args = append(args, limit)
	querySQL := fmt.Sprintf(
		"SELECT r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto FROM fhir_resource r WHERE %s ORDER BY r.res_id ASC LIMIT $%d",
		strings.Join(whereConds, " AND "), len(args),
	)

	var matches []SearchResult
	err := s.withTenantTx(ctx, params.TenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, querySQL, args...)
		if err != nil {
			return fmt.Errorf("patient match candidate query: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var r SearchResult
			if err := rows.Scan(&r.ResType, &r.ResID, &r.ResVersion, &r.LastUpdated, &r.ResourceProto); err != nil {
				return fmt.Errorf("scan patient match candidate: %w", err)
			}
			matches = append(matches, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return matches, nil
}
