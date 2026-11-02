package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
)

// sqlOnFHIRViewDefinition represents an HL7 SQL-on-FHIR v2 ViewDefinition resource
// (https://build.fhir.org/ig/FHIR/sql-on-fhir-v2/StructureDefinition-ViewDefinition.html).
type sqlOnFHIRViewDefinition struct {
	ResourceType string              `json:"resourceType"`
	ID           string              `json:"id,omitempty"`
	URL          string              `json:"url,omitempty"`
	Name         string              `json:"name,omitempty"`
	Title        string              `json:"title,omitempty"`
	Status       string              `json:"status,omitempty"`
	Resource     string              `json:"resource"`
	Constant     []sqlOnFHIRConstant `json:"constant,omitempty"`
	Where        []sqlOnFHIRWhere    `json:"where,omitempty"`
	Select       []sqlOnFHIRSelect   `json:"select"`
}

type sqlOnFHIRConstant struct {
	Name          string   `json:"name"`
	ValueString   *string  `json:"valueString,omitempty"`
	ValueCode     *string  `json:"valueCode,omitempty"`
	ValueURI      *string  `json:"valueUri,omitempty"`
	ValueDate     *string  `json:"valueDate,omitempty"`
	ValueDateTime *string  `json:"valueDateTime,omitempty"`
	ValueInteger  *int     `json:"valueInteger,omitempty"`
	ValueDecimal  *float64 `json:"valueDecimal,omitempty"`
	ValueBoolean  *bool    `json:"valueBoolean,omitempty"`
}

type sqlOnFHIRWhere struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type sqlOnFHIRSelect struct {
	Column        []sqlOnFHIRColumn `json:"column,omitempty"`
	Select        []sqlOnFHIRSelect `json:"select,omitempty"`
	ForEach       string            `json:"forEach,omitempty"`
	ForEachOrNull string            `json:"forEachOrNull,omitempty"`
	UnionAll      []sqlOnFHIRSelect `json:"unionAll,omitempty"`
}

type sqlOnFHIRColumn struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	Collection  bool   `json:"collection,omitempty"`
}

func builtinViewDefinitions() map[string]sqlOnFHIRViewDefinition {
	return map[string]sqlOnFHIRViewDefinition{
		"patient-demographics": {
			ResourceType: "ViewDefinition",
			ID:           "patient-demographics",
			Name:         "patient_demographics",
			Title:        "US Core Patient Demographics Tabular View",
			Status:       "active",
			Resource:     "Patient",
			Select: []sqlOnFHIRSelect{
				{
					Column: []sqlOnFHIRColumn{
						{Name: "id", Path: "getResourceKey()", Type: "string"},
						{Name: "family", Path: "name.family.first()", Type: "string"},
						{Name: "given", Path: "name.given.first()", Type: "string"},
						{Name: "gender", Path: "gender", Type: "code"},
						{Name: "birth_date", Path: "birthDate", Type: "date"},
						{Name: "active", Path: "active", Type: "boolean"},
					},
				},
			},
		},
		"observation-vitals": {
			ResourceType: "ViewDefinition",
			ID:           "observation-vitals",
			Name:         "observation_vitals",
			Title:        "US Core Vital Signs & Laboratory Observations Tabular View",
			Status:       "active",
			Resource:     "Observation",
			Select: []sqlOnFHIRSelect{
				{
					Column: []sqlOnFHIRColumn{
						{Name: "id", Path: "getResourceKey()", Type: "string"},
						{Name: "patient_id", Path: "subject.getReferenceKey('Patient')", Type: "string"},
						{Name: "status", Path: "status", Type: "code"},
						{Name: "code", Path: "code.coding.code.first()", Type: "code"},
						{Name: "system", Path: "code.coding.system.first()", Type: "uri"},
						{Name: "value", Path: "valueQuantity.value", Type: "decimal"},
						{Name: "unit", Path: "valueQuantity.code", Type: "code"},
						{Name: "effective", Path: "effectiveDateTime", Type: "dateTime"},
					},
				},
			},
		},
		"condition-problems": {
			ResourceType: "ViewDefinition",
			ID:           "condition-problems",
			Name:         "condition_problems",
			Title:        "US Core Condition Problems & Health Concerns Tabular View",
			Status:       "active",
			Resource:     "Condition",
			Select: []sqlOnFHIRSelect{
				{
					Column: []sqlOnFHIRColumn{
						{Name: "id", Path: "getResourceKey()", Type: "string"},
						{Name: "patient_id", Path: "subject.getReferenceKey('Patient')", Type: "string"},
						{Name: "clinical_status", Path: "clinicalStatus.coding.code.first()", Type: "code"},
						{Name: "code", Path: "code.coding.code.first()", Type: "code"},
						{Name: "system", Path: "code.coding.system.first()", Type: "uri"},
					},
				},
			},
		},
	}
}

func (s *Server) saveViewDefinition(tenant string, vd sqlOnFHIRViewDefinition) sqlOnFHIRViewDefinition {
	vd.ResourceType = "ViewDefinition"
	if strings.TrimSpace(vd.ID) == "" {
		vd.ID = uuid.NewString()
	}
	if strings.TrimSpace(vd.Status) == "" {
		vd.Status = "active"
	}
	s.viewDefsMu.Lock()
	if s.viewDefs == nil {
		s.viewDefs = make(map[string]map[string]sqlOnFHIRViewDefinition)
	}
	if s.viewDefs[tenant] == nil {
		s.viewDefs[tenant] = make(map[string]sqlOnFHIRViewDefinition)
	}
	s.viewDefs[tenant][vd.ID] = vd
	s.viewDefsMu.Unlock()
	return vd
}

func (s *Server) resolveViewDefinitionByID(ctx context.Context, tenant, id string) (sqlOnFHIRViewDefinition, bool) {
	id = strings.TrimSpace(id)
	if slash := strings.LastIndexByte(id, '/'); slash != -1 {
		id = id[slash+1:]
	}
	if id == "" {
		return sqlOnFHIRViewDefinition{}, false
	}

	s.viewDefsMu.RLock()
	if byTenant, ok := s.viewDefs[tenant]; ok {
		if vd, found := byTenant[id]; found {
			s.viewDefsMu.RUnlock()
			return vd, true
		}
		for _, vd := range byTenant {
			if strings.EqualFold(vd.Name, id) || (vd.URL != "" && strings.EqualFold(vd.URL, id)) {
				s.viewDefsMu.RUnlock()
				return vd, true
			}
		}
	}
	s.viewDefsMu.RUnlock()

	builtins := builtinViewDefinitions()
	if vd, ok := builtins[id]; ok {
		return vd, true
	}
	for _, vd := range builtins {
		if strings.EqualFold(vd.Name, id) {
			return vd, true
		}
	}

	if s.store != nil {
		if rec, err := s.store.ReadResourceWithMeta(ctx, tenant, "Basic", id); err == nil && rec != nil {
			if m, err := s.protoToFHIRMapWithMeta("Basic", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
				if vd, ok := viewDefinitionFromMap(m); ok {
					return vd, true
				}
			}
		}
	}
	return sqlOnFHIRViewDefinition{}, false
}

func viewDefinitionFromMap(m map[string]any) (sqlOnFHIRViewDefinition, bool) {
	if m == nil {
		return sqlOnFHIRViewDefinition{}, false
	}
	res, _ := m["resource"].(string)
	if res == "" {
		return sqlOnFHIRViewDefinition{}, false
	}
	rawSelects, _ := m["select"].([]any)
	selects := parseViewSelectsFromSlice(rawSelects)
	if len(selects) == 0 {
		return sqlOnFHIRViewDefinition{}, false
	}
	id, _ := m["id"].(string)
	url, _ := m["url"].(string)
	name, _ := m["name"].(string)
	title, _ := m["title"].(string)
	status, _ := m["status"].(string)
	return sqlOnFHIRViewDefinition{
		ResourceType: "ViewDefinition",
		ID:           id,
		URL:          url,
		Name:         name,
		Title:        title,
		Status:       status,
		Resource:     res,
		Select:       selects,
	}, true
}

func parseViewSelectsFromSlice(raw []any) []sqlOnFHIRSelect {
	if len(raw) == 0 {
		return nil
	}
	out := make([]sqlOnFHIRSelect, 0, len(raw))
	for _, item := range raw {
		sm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var sel sqlOnFHIRSelect
		sel.ForEach, _ = sm["forEach"].(string)
		sel.ForEachOrNull, _ = sm["forEachOrNull"].(string)
		if rawCols, ok := sm["column"].([]any); ok {
			for _, rc := range rawCols {
				cm, ok := rc.(map[string]any)
				if !ok {
					continue
				}
				cName, _ := cm["name"].(string)
				cPath, _ := cm["path"].(string)
				cDesc, _ := cm["description"].(string)
				cType, _ := cm["type"].(string)
				cColl, _ := cm["collection"].(bool)
				sel.Column = append(sel.Column, sqlOnFHIRColumn{
					Name:        cName,
					Path:        cPath,
					Description: cDesc,
					Type:        cType,
					Collection:  cColl,
				})
			}
		}
		if rawSub, ok := sm["select"].([]any); ok {
			sel.Select = parseViewSelectsFromSlice(rawSub)
		}
		if rawUnion, ok := sm["unionAll"].([]any); ok {
			sel.UnionAll = parseViewSelectsFromSlice(rawUnion)
		}
		out = append(out, sel)
	}
	return out
}

// handleViewDefinitionCRUD handles RESTful CRUD operations on /fhir/r4/{tenant}/ViewDefinition
// and /fhir/r4/{tenant}/ViewDefinition/{id}.
func (s *Server) handleViewDefinitionCRUD(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	id := strings.TrimSpace(r.PathValue("id"))

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if id != "" {
			vd, ok := s.resolveViewDefinitionByID(r.Context(), tenant, id)
			if !ok {
				writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("ViewDefinition/%s not found", id))
				return
			}
			w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_ = json.NewEncoder(w).Encode(vd)
			}
			return
		}
		var entries []map[string]any
		seen := make(map[string]bool)
		s.viewDefsMu.RLock()
		for k, vd := range s.viewDefs[tenant] {
			seen[k] = true
			entries = append(entries, map[string]any{
				"fullUrl":  fmt.Sprintf("ViewDefinition/%s", vd.ID),
				"resource": vd,
			})
		}
		s.viewDefsMu.RUnlock()
		for k, vd := range builtinViewDefinitions() {
			if !seen[k] {
				entries = append(entries, map[string]any{
					"fullUrl":  fmt.Sprintf("ViewDefinition/%s", vd.ID),
					"resource": vd,
				})
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			return fmt.Sprint(entries[i]["fullUrl"]) < fmt.Sprint(entries[j]["fullUrl"])
		})
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "Bundle",
				"type":         "searchset",
				"total":        len(entries),
				"entry":        entries,
			})
		}

	case http.MethodPost, http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read request body")
			return
		}
		var vd sqlOnFHIRViewDefinition
		if err := json.Unmarshal(body, &vd); err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid ViewDefinition JSON: %v", err))
			return
		}
		if id != "" {
			vd.ID = id
		}
		if err := validateViewDefinition(vd); err != nil {
			writeOperationOutcome(w, http.StatusUnprocessableEntity, "invalid", err.Error())
			return
		}
		_, existed := s.resolveViewDefinitionByID(r.Context(), tenant, vd.ID)
		saved := s.saveViewDefinition(tenant, vd)
		status := http.StatusCreated
		if r.Method == http.MethodPut && existed {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/ViewDefinition/%s", tenant, saved.ID))
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(saved)

	case http.MethodDelete:
		if id == "" {
			writeOperationOutcome(w, http.StatusBadRequest, "required", "ViewDefinition id is required for DELETE")
			return
		}
		s.viewDefsMu.Lock()
		if s.viewDefs != nil && s.viewDefs[tenant] != nil {
			delete(s.viewDefs[tenant], id)
		}
		s.viewDefsMu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", fmt.Sprintf("method %s not allowed", r.Method))
	}
}

func validateViewDefinition(vd sqlOnFHIRViewDefinition) error {
	if strings.TrimSpace(vd.Resource) == "" {
		return fmt.Errorf("ViewDefinition.resource is required")
	}
	if len(vd.Select) == 0 {
		return fmt.Errorf("ViewDefinition.select must contain at least one select clause")
	}
	cols := collectViewDefinitionColumns(vd.Select)
	if len(cols) == 0 {
		return fmt.Errorf("ViewDefinition must declare at least one column")
	}
	seen := make(map[string]bool, len(cols))
	for _, c := range cols {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("ViewDefinition column name is required")
		}
		if strings.TrimSpace(c.Path) == "" {
			return fmt.Errorf("ViewDefinition column %q path is required", c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("duplicate ViewDefinition column name %q", c.Name)
		}
		seen[c.Name] = true
	}
	return nil
}

func collectViewDefinitionColumns(selects []sqlOnFHIRSelect) []sqlOnFHIRColumn {
	var out []sqlOnFHIRColumn
	seen := make(map[string]bool)
	var walk func(sels []sqlOnFHIRSelect, prefix string)
	walk = func(sels []sqlOnFHIRSelect, prefix string) {
		for _, sel := range sels {
			curPrefix := prefix
			fe := strings.TrimSpace(sel.ForEach)
			if fe == "" {
				fe = strings.TrimSpace(sel.ForEachOrNull)
			}
			if fe != "" {
				curPrefix = qualifyForEachColumnPath(prefix, fe)
			}
			for _, col := range sel.Column {
				if !seen[col.Name] {
					seen[col.Name] = true
					c := col
					c.Path = qualifyForEachColumnPath(curPrefix, c.Path)
					out = append(out, c)
				}
			}
			walk(sel.Select, curPrefix)
			walk(sel.UnionAll, curPrefix)
		}
	}
	walk(selects, "")
	return out
}

func qualifyForEachColumnPath(forEachPrefix, colPath string) string {
	p := strings.TrimSpace(colPath)
	prefix := strings.TrimSpace(forEachPrefix)
	if prefix == "" || p == "" {
		return p
	}
	if strings.HasPrefix(p, "'") || strings.HasPrefix(p, "%") || strings.HasPrefix(p, "getResourceKey(") || p == "id" || strings.HasPrefix(p, prefix+".") || p == prefix {
		return p
	}
	if p == "$this" {
		return prefix
	}
	return prefix + "." + p
}

// extractViewForEachSpec returns the first forEach / forEachOrNull path and whether
// outer unnesting (forEachOrNull) was requested.
func extractViewForEachSpec(selects []sqlOnFHIRSelect) (forEachPath string, isOuter bool) {
	var walk func(sels []sqlOnFHIRSelect, prefix string)
	walk = func(sels []sqlOnFHIRSelect, prefix string) {
		for _, sel := range sels {
			if forEachPath != "" {
				return
			}
			fe := strings.TrimSpace(sel.ForEach)
			outer := false
			if fe == "" {
				fe = strings.TrimSpace(sel.ForEachOrNull)
				outer = fe != ""
			}
			curPrefix := prefix
			if fe != "" {
				curPrefix = qualifyForEachColumnPath(prefix, fe)
				forEachPath = curPrefix
				isOuter = outer
				return
			}
			walk(sel.Select, curPrefix)
		}
	}
	walk(selects, "")
	return forEachPath, isOuter
}

// extractViewUnionBranches expands top-level or nested select.unionAll clauses
// into aligned per-branch column slices matching collectViewDefinitionColumns(selects).
// If no unionAll is present, returns a single branch containing collectViewDefinitionColumns(selects).
func extractViewUnionBranches(selects []sqlOnFHIRSelect) [][]sqlOnFHIRColumn {
	allCols := collectViewDefinitionColumns(selects)
	var baseCols []sqlOnFHIRColumn
	var unionBranches []sqlOnFHIRSelect

	var walk func(sels []sqlOnFHIRSelect, prefix string)
	walk = func(sels []sqlOnFHIRSelect, prefix string) {
		for _, sel := range sels {
			curPrefix := prefix
			fe := strings.TrimSpace(sel.ForEach)
			if fe == "" {
				fe = strings.TrimSpace(sel.ForEachOrNull)
			}
			if fe != "" {
				curPrefix = qualifyForEachColumnPath(prefix, fe)
			}
			for _, col := range sel.Column {
				c := col
				c.Path = qualifyForEachColumnPath(curPrefix, c.Path)
				baseCols = append(baseCols, c)
			}
			if len(sel.UnionAll) > 0 && len(unionBranches) == 0 {
				for _, u := range sel.UnionAll {
					uCopy := u
					uFE := strings.TrimSpace(uCopy.ForEach)
					if uFE == "" {
						uFE = strings.TrimSpace(uCopy.ForEachOrNull)
					}
					branchPrefix := curPrefix
					if uFE != "" {
						branchPrefix = qualifyForEachColumnPath(curPrefix, uFE)
					}
					cols := make([]sqlOnFHIRColumn, len(uCopy.Column))
					for i, col := range uCopy.Column {
						c := col
						c.Path = qualifyForEachColumnPath(branchPrefix, c.Path)
						cols[i] = c
					}
					uCopy.Column = cols
					unionBranches = append(unionBranches, uCopy)
				}
			}
			walk(sel.Select, curPrefix)
		}
	}
	walk(selects, "")

	if len(unionBranches) == 0 {
		return [][]sqlOnFHIRColumn{allCols}
	}

	baseMap := make(map[string]sqlOnFHIRColumn, len(baseCols))
	for _, c := range baseCols {
		baseMap[c.Name] = c
	}

	out := make([][]sqlOnFHIRColumn, 0, len(unionBranches))
	for _, ub := range unionBranches {
		branchCols := collectViewDefinitionColumns([]sqlOnFHIRSelect{ub})
		bMap := make(map[string]sqlOnFHIRColumn, len(branchCols))
		for _, c := range branchCols {
			bMap[c.Name] = c
		}
		aligned := make([]sqlOnFHIRColumn, 0, len(allCols))
		for _, schemaCol := range allCols {
			if bc, ok := bMap[schemaCol.Name]; ok {
				aligned = append(aligned, bc)
			} else if baseC, ok := baseMap[schemaCol.Name]; ok {
				aligned = append(aligned, baseC)
			} else {
				aligned = append(aligned, sqlOnFHIRColumn{Name: schemaCol.Name, Path: "''"})
			}
		}
		out = append(out, aligned)
	}
	return out
}

type viewRunOptions struct {
	viewDef     sqlOnFHIRViewDefinition
	format      string
	dialect     string
	limit       int
	patientID   string
	groupID     string
	since       *time.Time
	includeHdrs bool
}

func (s *Server) parseViewRunRequest(r *http.Request, defaultFormat string) (viewRunOptions, int, error) {
	tenant := r.PathValue("tenant")
	instanceID := strings.TrimSpace(r.PathValue("id"))
	q := r.URL.Query()

	opts := viewRunOptions{
		format:      strings.TrimSpace(q.Get("_format")),
		dialect:     strings.ToLower(strings.TrimSpace(q.Get("dialect"))),
		patientID:   strings.TrimSpace(q.Get("patient")),
		groupID:     strings.TrimSpace(q.Get("group")),
		includeHdrs: true,
	}
	if hdrParam := strings.TrimSpace(q.Get("header")); strings.EqualFold(hdrParam, "false") || hdrParam == "0" {
		opts.includeHdrs = false
	}
	if limStr := strings.TrimSpace(q.Get("_limit")); limStr != "" {
		if n, err := strconv.Atoi(limStr); err == nil && n > 0 {
			opts.limit = n
		}
	}
	if sinceStr := strings.TrimSpace(q.Get("_since")); sinceStr != "" {
		if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			opts.since = &t
		}
	}
	viewRef := strings.TrimSpace(q.Get("view"))
	if viewRef == "" {
		viewRef = strings.TrimSpace(q.Get("viewReference"))
	}

	var parsedVD *sqlOnFHIRViewDefinition
	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			return opts, http.StatusBadRequest, fmt.Errorf("failed to read request body")
		}
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 {
			var probe struct {
				ResourceType string `json:"resourceType"`
			}
			if err := json.Unmarshal(trimmed, &probe); err != nil {
				return opts, http.StatusBadRequest, fmt.Errorf("invalid JSON payload: %v", err)
			}
			switch probe.ResourceType {
			case "ViewDefinition":
				var vd sqlOnFHIRViewDefinition
				if err := json.Unmarshal(trimmed, &vd); err != nil {
					return opts, http.StatusBadRequest, fmt.Errorf("invalid ViewDefinition JSON: %v", err)
				}
				parsedVD = &vd
			case "Parameters":
				var params struct {
					Parameter []struct {
						Name           string `json:"name"`
						ValueString    string `json:"valueString"`
						ValueCode      string `json:"valueCode"`
						ValueInteger   *int   `json:"valueInteger"`
						ValueBoolean   *bool  `json:"valueBoolean"`
						ValueInstant   string `json:"valueInstant"`
						ValueDateTime  string `json:"valueDateTime"`
						ValueReference *struct {
							Reference string `json:"reference"`
						} `json:"valueReference"`
						Resource json.RawMessage `json:"resource"`
					} `json:"parameter"`
				}
				if err := json.Unmarshal(trimmed, &params); err != nil {
					return opts, http.StatusBadRequest, fmt.Errorf("invalid Parameters JSON: %v", err)
				}
				for _, p := range params.Parameter {
					switch p.Name {
					case "viewResource", "view":
						if len(p.Resource) > 0 {
							var vd sqlOnFHIRViewDefinition
							if err := json.Unmarshal(p.Resource, &vd); err != nil {
								return opts, http.StatusBadRequest, fmt.Errorf("invalid viewResource parameter: %v", err)
							}
							parsedVD = &vd
						} else if p.ValueString != "" {
							viewRef = p.ValueString
						}
					case "viewReference":
						if p.ValueReference != nil && p.ValueReference.Reference != "" {
							viewRef = p.ValueReference.Reference
						} else if p.ValueString != "" {
							viewRef = p.ValueString
						}
					case "_format", "format":
						if p.ValueCode != "" {
							opts.format = p.ValueCode
						} else if p.ValueString != "" {
							opts.format = p.ValueString
						}
					case "dialect":
						if p.ValueCode != "" {
							opts.dialect = strings.ToLower(strings.TrimSpace(p.ValueCode))
						} else if p.ValueString != "" {
							opts.dialect = strings.ToLower(strings.TrimSpace(p.ValueString))
						}
					case "_limit", "limit":
						if p.ValueInteger != nil && *p.ValueInteger > 0 {
							opts.limit = *p.ValueInteger
						}
					case "patient":
						if p.ValueReference != nil && p.ValueReference.Reference != "" {
							opts.patientID = p.ValueReference.Reference
						} else if p.ValueString != "" {
							opts.patientID = p.ValueString
						}
					case "group":
						if p.ValueReference != nil && p.ValueReference.Reference != "" {
							opts.groupID = p.ValueReference.Reference
						} else if p.ValueString != "" {
							opts.groupID = p.ValueString
						}
					case "_since", "since":
						rawSince := p.ValueInstant
						if rawSince == "" {
							rawSince = p.ValueDateTime
						}
						if rawSince == "" {
							rawSince = p.ValueString
						}
						if t, err := time.Parse(time.RFC3339, strings.TrimSpace(rawSince)); err == nil {
							opts.since = &t
						}
					case "header":
						if p.ValueBoolean != nil {
							opts.includeHdrs = *p.ValueBoolean
						}
					}
				}
			default:
				return opts, http.StatusBadRequest, fmt.Errorf("expected ViewDefinition or Parameters resource, got %q", probe.ResourceType)
			}
		}
	}

	if parsedVD != nil {
		opts.viewDef = *parsedVD
	} else {
		lookupID := instanceID
		if lookupID == "" {
			lookupID = viewRef
		}
		if lookupID == "" {
			return opts, http.StatusBadRequest, fmt.Errorf("ViewDefinition is required (provide instance ID, 'view' query parameter, or inline ViewDefinition / Parameters body)")
		}
		vd, ok := s.resolveViewDefinitionByID(r.Context(), tenant, lookupID)
		if !ok {
			return opts, http.StatusNotFound, fmt.Errorf("ViewDefinition/%s not found", lookupID)
		}
		opts.viewDef = vd
	}

	if err := validateViewDefinition(opts.viewDef); err != nil {
		return opts, http.StatusUnprocessableEntity, err
	}

	if opts.format == "" {
		accept := strings.ToLower(strings.TrimSpace(r.Header.Get("Accept")))
		switch {
		case strings.Contains(accept, "text/tab-separated-values") || strings.Contains(accept, "text/tsv"):
			opts.format = "text/tab-separated-values"
		case strings.Contains(accept, "text/csv"):
			opts.format = "text/csv"
		case strings.Contains(accept, "ndjson"):
			opts.format = "application/ndjson"
		case strings.Contains(accept, "json"):
			opts.format = "application/json"
		default:
			opts.format = defaultFormat
		}
	}
	return opts, http.StatusOK, nil
}

// handleViewDefinitionRun handles GET/POST /fhir/r4/{tenant}/ViewDefinition/$run
// and /fhir/r4/{tenant}/ViewDefinition/{id}/$run per the HL7 SQL-on-FHIR v2 specification.
func (s *Server) handleViewDefinitionRun(w http.ResponseWriter, r *http.Request) {
	s.executeViewDefinition(w, r, "application/json")
}

// handleViewDefinitionExport handles GET/POST /fhir/r4/{tenant}/ViewDefinition/$export
// and /fhir/r4/{tenant}/ViewDefinition/{id}/$export per the HL7 SQL-on-FHIR v2 specification.
func (s *Server) handleViewDefinitionExport(w http.ResponseWriter, r *http.Request) {
	s.executeViewDefinition(w, r, "application/ndjson")
}

func (s *Server) executeViewDefinition(w http.ResponseWriter, r *http.Request, defaultFormat string) {
	tenant := r.PathValue("tenant")
	opts, status, err := s.parseViewRunRequest(r, defaultFormat)
	if err != nil {
		code := "invalid"
		if status == http.StatusNotFound {
			code = "not-found"
		}
		writeOperationOutcome(w, status, code, err.Error())
		return
	}

	targetType := strings.TrimSpace(opts.viewDef.Resource)
	if _, ok := s.protoRegistry[targetType]; !ok {
		writeOperationOutcome(w, http.StatusUnprocessableEntity, "not-supported", fmt.Sprintf("unsupported ViewDefinition target resource type %q", targetType))
		return
	}

	exportParams := store.ExportParams{
		TenantID:      tenant,
		ResourceTypes: []string{targetType},
		Since:         opts.since,
	}
	if opts.patientID != "" {
		patID := opts.patientID
		if slash := strings.LastIndexByte(patID, '/'); slash != -1 {
			patID = patID[slash+1:]
		}
		exportParams.PatientID = patID
	}
	if opts.groupID != "" {
		grpID := opts.groupID
		if slash := strings.LastIndexByte(grpID, '/'); slash != -1 {
			grpID = grpID[slash+1:]
		}
		grpRes, err := s.store.GroupEverything(r.Context(), store.GroupEverythingParams{
			TenantID: tenant,
			GroupID:  grpID,
		})
		if err == nil && grpRes != nil {
			var patIDs []string
			for _, m := range grpRes.Matches {
				if m.ResType == "Patient" {
					patIDs = append(patIDs, m.ResID)
				}
			}
			exportParams.PatientIDs = patIDs
		}
	}

	constMap := buildViewConstantReplacements(opts.viewDef.Constant)
	columns := collectViewDefinitionColumns(opts.viewDef.Select)

	if s.metrics != nil {
		s.metrics.sqlOnFHIRRunsTotal.Add(1)
	}

	var rows []map[string]any
	err = s.streamExportResources(r.Context(), exportParams, func(sr store.SearchResult) error {
		if opts.limit > 0 && len(rows) >= opts.limit {
			return nil
		}
		root, err := s.protoToFHIRMapWithMeta(sr.ResType, sr.ResourceProto, sr.ResVersion, sr.LastUpdated)
		if err != nil {
			return nil
		}
		resRows, evalErr := evaluateViewDefinitionOnResource(root, opts.viewDef, constMap)
		if evalErr != nil {
			return evalErr
		}
		for _, row := range resRows {
			rows = append(rows, row)
			if opts.limit > 0 && len(rows) >= opts.limit {
				break
			}
		}
		return nil
	})
	if err != nil {
		writeOperationOutcome(w, http.StatusUnprocessableEntity, "business-rule", err.Error())
		return
	}

	w.Header().Set("X-Flint-Storage-Model", "zero-json-proto3")
	w.Header().Set("X-Flint-Execution-Mode", "zero-json-protoreflect")
	if s.analyticalStore != nil {
		if backend := strings.TrimSpace(s.analyticalStore.BackendName()); backend != "" {
			w.Header().Set("X-Flint-Analytical-Backend", backend)
		}
	}
	if sqlRes, sqlErr := s.transpileViewDefinitionToZeroJSONSQL(tenant, opts); sqlErr == nil && sqlRes.sql != "" {
		w.Header().Set("X-Flint-Generated-SQL", strings.Join(strings.Fields(sqlRes.sql), " "))
	}

	fmtLower := strings.ToLower(strings.TrimSpace(opts.format))
	switch {
	case strings.Contains(fmtLower, "tab-separated-values") || strings.Contains(fmtLower, "tsv"):
		w.Header().Set("Content-Type", "text/tab-separated-values; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		cw := csv.NewWriter(w)
		cw.Comma = '\t'
		if opts.includeHdrs {
			hdr := make([]string, len(columns))
			for i, c := range columns {
				hdr[i] = c.Name
			}
			_ = cw.Write(hdr)
		}
		for _, row := range rows {
			rec := make([]string, len(columns))
			for i, c := range columns {
				rec[i] = formatCSVCellValue(row[c.Name])
			}
			_ = cw.Write(rec)
		}
		cw.Flush()

	case strings.Contains(fmtLower, "csv"):
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		cw := csv.NewWriter(w)
		if opts.includeHdrs {
			hdr := make([]string, len(columns))
			for i, c := range columns {
				hdr[i] = c.Name
			}
			_ = cw.Write(hdr)
		}
		for _, row := range rows {
			rec := make([]string, len(columns))
			for i, c := range columns {
				rec[i] = formatCSVCellValue(row[c.Name])
			}
			_ = cw.Write(rec)
		}
		cw.Flush()

	case strings.Contains(fmtLower, "ndjson"):
		w.Header().Set("Content-Type", "application/ndjson; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		for _, row := range rows {
			line, _ := marshalOrderedViewRow(row, columns)
			_, _ = w.Write(line)
			_, _ = w.Write([]byte("\n"))
		}

	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, row := range rows {
			if i > 0 {
				buf.WriteByte(',')
			}
			b, _ := marshalOrderedViewRow(row, columns)
			buf.Write(b)
		}
		buf.WriteByte(']')
		_, _ = w.Write(buf.Bytes())
	}
}

func buildViewConstantReplacements(constants []sqlOnFHIRConstant) map[string]string {
	out := make(map[string]string, len(constants))
	for _, c := range constants {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		var lit string
		switch {
		case c.ValueString != nil:
			lit = "'" + strings.ReplaceAll(*c.ValueString, "'", "\\'") + "'"
		case c.ValueCode != nil:
			lit = "'" + strings.ReplaceAll(*c.ValueCode, "'", "\\'") + "'"
		case c.ValueURI != nil:
			lit = "'" + strings.ReplaceAll(*c.ValueURI, "'", "\\'") + "'"
		case c.ValueDate != nil:
			lit = "'" + strings.ReplaceAll(*c.ValueDate, "'", "\\'") + "'"
		case c.ValueDateTime != nil:
			lit = "'" + strings.ReplaceAll(*c.ValueDateTime, "'", "\\'") + "'"
		case c.ValueInteger != nil:
			lit = strconv.Itoa(*c.ValueInteger)
		case c.ValueDecimal != nil:
			lit = strconv.FormatFloat(*c.ValueDecimal, 'f', -1, 64)
		case c.ValueBoolean != nil:
			if *c.ValueBoolean {
				lit = "true"
			} else {
				lit = "false"
			}
		}
		if lit != "" {
			out["%"+name] = lit
		}
	}
	return out
}

func substituteViewConstants(expr string, constMap map[string]string) string {
	if len(constMap) == 0 || !strings.Contains(expr, "%") {
		return expr
	}
	// Replace longest constant names first to avoid prefix collisions
	keys := make([]string, 0, len(constMap))
	for k := range constMap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return len(keys[i]) > len(keys[j])
	})
	for _, k := range keys {
		expr = strings.ReplaceAll(expr, k, constMap[k])
	}
	return expr
}

func evaluateViewDefinitionOnResource(root map[string]any, vd sqlOnFHIRViewDefinition, constMap map[string]string) ([]map[string]any, error) {
	ctx := fhirPathContext{
		root:     root,
		thisItem: root,
		index:    0,
		total:    1,
	}
	for _, w := range vd.Where {
		path := substituteViewConstants(strings.TrimSpace(w.Path), constMap)
		if path == "" {
			continue
		}
		vals := evalFHIRPathExpr([]any{root}, ctx, path)
		b, ok := singletonBool(vals)
		if !ok || !b {
			return nil, nil
		}
	}
	return evaluateSelectList(root, root, vd.Select, constMap)
}

func evaluateSelectList(root map[string]any, focus any, selects []sqlOnFHIRSelect, constMap map[string]string) ([]map[string]any, error) {
	acc := []map[string]any{{}}
	for _, sel := range selects {
		selRows, err := evaluateSingleSelect(root, focus, sel, constMap)
		if err != nil {
			return nil, err
		}
		if len(selRows) == 0 {
			return nil, nil
		}
		acc = cartesianMergeRows(acc, selRows)
	}
	return acc, nil
}

func evaluateSingleSelect(root map[string]any, focus any, sel sqlOnFHIRSelect, constMap map[string]string) ([]map[string]any, error) {
	var focusItems []any
	isForEachOrNull := false

	switch {
	case strings.TrimSpace(sel.ForEach) != "":
		expr := substituteViewConstants(strings.TrimSpace(sel.ForEach), constMap)
		focusItems = evalFHIRPathExpr([]any{focus}, fhirPathContext{root: root, thisItem: focus, index: 0, total: 1}, expr)
		if len(focusItems) == 0 {
			return nil, nil
		}
	case strings.TrimSpace(sel.ForEachOrNull) != "":
		isForEachOrNull = true
		expr := substituteViewConstants(strings.TrimSpace(sel.ForEachOrNull), constMap)
		focusItems = evalFHIRPathExpr([]any{focus}, fhirPathContext{root: root, thisItem: focus, index: 0, total: 1}, expr)
		if len(focusItems) == 0 {
			focusItems = []any{nil}
		}
	default:
		focusItems = []any{focus}
	}

	var out []map[string]any
	for _, item := range focusItems {
		itemRows := []map[string]any{{}}

		if len(sel.Column) > 0 {
			colRow := make(map[string]any, len(sel.Column))
			for _, col := range sel.Column {
				if item == nil && isForEachOrNull {
					if col.Collection {
						colRow[col.Name] = []any{}
					} else {
						colRow[col.Name] = nil
					}
					continue
				}
				expr := substituteViewConstants(strings.TrimSpace(col.Path), constMap)
				vals := evalFHIRPathExpr([]any{item}, fhirPathContext{root: root, thisItem: item, index: 0, total: 1}, expr)
				if col.Collection {
					if vals == nil {
						vals = []any{}
					}
					colRow[col.Name] = vals
				} else {
					if len(vals) > 1 {
						return nil, fmt.Errorf("column %q evaluated to %d values for single-valued column (set collection: true to allow multiple values)", col.Name, len(vals))
					}
					if len(vals) == 1 {
						colRow[col.Name] = vals[0]
					} else {
						colRow[col.Name] = nil
					}
				}
			}
			itemRows = []map[string]any{colRow}
		}

		if len(sel.Select) > 0 {
			if item == nil && isForEachOrNull {
				nestedCols := collectViewDefinitionColumns(sel.Select)
				nullNested := make(map[string]any, len(nestedCols))
				for _, nc := range nestedCols {
					if nc.Collection {
						nullNested[nc.Name] = []any{}
					} else {
						nullNested[nc.Name] = nil
					}
				}
				itemRows = cartesianMergeRows(itemRows, []map[string]any{nullNested})
			} else {
				nestedRows, err := evaluateSelectList(root, item, sel.Select, constMap)
				if err != nil {
					return nil, err
				}
				if len(nestedRows) == 0 {
					continue
				}
				itemRows = cartesianMergeRows(itemRows, nestedRows)
			}
		}

		if len(sel.UnionAll) > 0 {
			var unionRows []map[string]any
			if item == nil && isForEachOrNull {
				uCols := collectViewDefinitionColumns(sel.UnionAll)
				nullUnion := make(map[string]any, len(uCols))
				for _, uc := range uCols {
					if uc.Collection {
						nullUnion[uc.Name] = []any{}
					} else {
						nullUnion[uc.Name] = nil
					}
				}
				unionRows = []map[string]any{nullUnion}
			} else {
				for _, branch := range sel.UnionAll {
					branchRows, err := evaluateSingleSelect(root, item, branch, constMap)
					if err != nil {
						return nil, err
					}
					unionRows = append(unionRows, branchRows...)
				}
			}
			if len(unionRows) == 0 {
				continue
			}
			itemRows = cartesianMergeRows(itemRows, unionRows)
		}

		out = append(out, itemRows...)
	}
	return out, nil
}

func cartesianMergeRows(left, right []map[string]any) []map[string]any {
	if len(left) == 0 || len(right) == 0 {
		return nil
	}
	merged := make([]map[string]any, 0, len(left)*len(right))
	for _, l := range left {
		for _, r := range right {
			m := make(map[string]any, len(l)+len(r))
			for k, v := range l {
				m[k] = v
			}
			for k, v := range r {
				m[k] = v
			}
			merged = append(merged, m)
		}
	}
	return merged
}

func marshalOrderedViewRow(row map[string]any, columns []sqlOnFHIRColumn) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, c := range columns {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyBytes, _ := json.Marshal(c.Name)
		buf.Write(keyBytes)
		buf.WriteByte(':')
		val, exists := row[c.Name]
		if !exists {
			val = nil
		}
		valBytes, err := json.Marshal(val)
		if err != nil {
			buf.WriteString("null")
		} else {
			buf.Write(valBytes)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func formatCSVCellValue(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case []any:
		b, _ := json.Marshal(val)
		return string(b)
	default:
		b, _ := json.Marshal(val)
		return string(b)
	}
}

// handleSyncAnalytics handles POST/GET /fhir/r4/{tenant}/$sync-analytics to backfill or verify
// synchronization between the primary transactional store and the analytical replica store.
func (s *Server) handleSyncAnalytics(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	storageBackend := string(store.BackendPostgres)
	analyticalBackend := "none"

	if s.analyticalStore != nil {
		if b := strings.TrimSpace(s.analyticalStore.BackendName()); b != "" {
			analyticalBackend = b
			w.Header().Set("X-Flint-Analytical-Backend", b)
		}
	}
	if as, ok := s.primaryStore.(store.AnalyticalStore); ok && !store.IsNil(as) {
		if b := strings.TrimSpace(as.BackendName()); b != "" {
			storageBackend = b
		}
	}

	synced := 0
	var repWrites, repDeletes, repErrors, totalSynced uint64
	var lastSyncTime string

	if hyb, ok := s.store.(*store.HybridStore); ok && hyb != nil {
		n, err := hyb.SyncTenant(r.Context(), tenant)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", fmt.Sprintf("analytical sync failed: %v", err))
			return
		}
		synced = n
		repWrites = hyb.ReplicatedWrites()
		repDeletes = hyb.ReplicatedDeletes()
		repErrors = hyb.ReplicationErrors()
		totalSynced = hyb.SyncedResources()
		if ts := hyb.LastSyncTime(); !ts.IsZero() {
			lastSyncTime = ts.Format(time.RFC3339)
		}
	} else if s.store != nil {
		_ = s.streamExportResources(r.Context(), store.ExportParams{TenantID: tenant}, func(_ store.SearchResult) error {
			synced++
			return nil
		})
		totalSynced = uint64(synced)
	}

	params := []map[string]any{
		{"name": "tenant", "valueString": tenant},
		{"name": "storageBackend", "valueCode": storageBackend},
		{"name": "analyticalBackend", "valueCode": analyticalBackend},
		{"name": "syncedResources", "valueInteger": synced},
		{"name": "totalSyncedResources", "valueInteger": totalSynced},
		{"name": "replicatedWrites", "valueInteger": repWrites},
		{"name": "replicatedDeletes", "valueInteger": repDeletes},
		{"name": "replicationErrors", "valueInteger": repErrors},
	}
	if lastSyncTime != "" {
		params = append(params, map[string]any{"name": "lastSyncTime", "valueDateTime": lastSyncTime})
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    params,
	})
}

type zeroJSONTranspiledSQL struct {
	dialect      string
	storageModel string
	targetTable  string
	sql          string
	columnMaps   []map[string]any
}

// handleViewDefinitionSQL handles GET/POST /fhir/r4/{tenant}/ViewDefinition/$sql,
// /fhir/r4/{tenant}/ViewDefinition/{id}/$sql, and /fhir/r4/{tenant}/$sql.
// It transpiles an HL7 SQL-on-FHIR v2 ViewDefinition directly into Zero-JSON SQL
// targeting either Apache Doris Unique Key Merge-on-Write columnar tables
// (fhir_patient, fhir_observation, fhir_condition, fhir_resource) or PostgreSQL/SQLite
// covering spidx_* tables (with zero JSONB / VARIANT / json_extract).
func (s *Server) handleViewDefinitionSQL(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	opts, status, err := s.parseViewRunRequest(r, "application/fhir+json")
	if err != nil {
		code := "invalid"
		if status == http.StatusNotFound {
			code = "not-found"
		}
		writeOperationOutcome(w, status, code, err.Error())
		return
	}

	targetType := strings.TrimSpace(opts.viewDef.Resource)
	if _, ok := s.protoRegistry[targetType]; !ok {
		writeOperationOutcome(w, http.StatusUnprocessableEntity, "not-supported", fmt.Sprintf("unsupported ViewDefinition target resource type %q", targetType))
		return
	}

	res, err := s.transpileViewDefinitionToZeroJSONSQL(tenant, opts)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	w.Header().Set("X-Flint-Storage-Model", res.storageModel)
	if s.analyticalStore != nil {
		if b := strings.TrimSpace(s.analyticalStore.BackendName()); b != "" {
			w.Header().Set("X-Flint-Analytical-Backend", b)
		}
	}

	fmtLower := strings.ToLower(strings.TrimSpace(opts.format))
	acceptLower := strings.ToLower(strings.TrimSpace(r.Header.Get("Accept")))
	if strings.Contains(fmtLower, "sql") || strings.Contains(fmtLower, "text/plain") || strings.Contains(acceptLower, "application/sql") {
		w.Header().Set("Content-Type", "application/sql; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(res.sql + "\n"))
		return
	}

	viewName := opts.viewDef.Name
	if viewName == "" {
		viewName = opts.viewDef.ID
	}
	params := []map[string]any{
		{"name": "viewName", "valueString": viewName},
		{"name": "resource", "valueCode": targetType},
		{"name": "dialect", "valueCode": res.dialect},
		{"name": "storageModel", "valueString": res.storageModel},
		{"name": "targetTable", "valueString": res.targetTable},
		{"name": "sql", "valueString": res.sql},
	}
	params = append(params, res.columnMaps...)

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    params,
	})
}

func (s *Server) transpileViewDefinitionToZeroJSONSQL(tenant string, opts viewRunOptions) (zeroJSONTranspiledSQL, error) {
	dialect := strings.ToLower(strings.TrimSpace(opts.dialect))
	if dialect == "" {
		if s.analyticalStore != nil {
			b := strings.ToLower(strings.TrimSpace(s.analyticalStore.BackendName()))
			switch {
			case strings.Contains(b, "doris"):
				dialect = "doris"
			case strings.Contains(b, "sqlite"):
				dialect = "sqlite"
			default:
				dialect = "postgres"
			}
		} else {
			dialect = "postgres"
		}
	}
	switch dialect {
	case "doris", "postgres", "postgresql", "sqlite":
		if dialect == "postgresql" {
			dialect = "postgres"
		}
	default:
		return zeroJSONTranspiledSQL{}, fmt.Errorf("unsupported SQL dialect %q (valid: doris, postgres, sqlite)", dialect)
	}

	constMap := buildViewConstantReplacements(opts.viewDef.Constant)
	columns := collectViewDefinitionColumns(opts.viewDef.Select)
	branches := extractViewUnionBranches(opts.viewDef.Select)
	forEachPath, forEachOuter := extractViewForEachSpec(opts.viewDef.Select)
	resType := strings.TrimSpace(opts.viewDef.Resource)
	escapedTenant := strings.ReplaceAll(tenant, "'", "''")

	if dialect == "doris" {
		coreTable, isCore := doris.CoreResourceTableName(resType)
		targetTable := "fhir_resource"
		if isCore {
			targetTable = coreTable
		}

		colMaps := make([]map[string]any, 0, len(columns))
		for _, col := range columns {
			exprPath := substituteViewConstants(strings.TrimSpace(col.Path), constMap)
			sqlExpr, sourceCol := dorisZeroJSONForEachColumnExpr(exprPath, forEachPath, isCore)
			colMaps = append(colMaps, map[string]any{
				"name": "column",
				"part": []map[string]any{
					{"name": "name", "valueString": col.Name},
					{"name": "fhirpath", "valueString": exprPath},
					{"name": "storageColumn", "valueString": sourceCol},
					{"name": "sqlExpression", "valueString": sqlExpr},
				},
			})
		}

		var whereConds []string
		whereConds = append(whereConds, fmt.Sprintf("r.tenant_id = '%s'", escapedTenant))
		if !isCore {
			whereConds = append(whereConds, fmt.Sprintf("r.res_type = '%s'", strings.ReplaceAll(resType, "'", "''")))
		}
		whereConds = append(whereConds, "r.is_deleted = FALSE")
		if opts.patientID != "" {
			patID := opts.patientID
			if slash := strings.LastIndexByte(patID, '/'); slash != -1 {
				patID = patID[slash+1:]
			}
			whereConds = append(whereConds, fmt.Sprintf("r.patient_id = '%s'", strings.ReplaceAll(patID, "'", "''")))
		}
		if opts.since != nil && !opts.since.IsZero() {
			whereConds = append(whereConds, fmt.Sprintf("r.last_updated >= '%s'", opts.since.UTC().Format("2006-01-02 15:04:05")))
		}
		for _, w := range opts.viewDef.Where {
			wPath := substituteViewConstants(strings.TrimSpace(w.Path), constMap)
			if wSQL := dorisZeroJSONWhereExpr(wPath, isCore); wSQL != "" {
				whereConds = append(whereConds, wSQL)
			}
		}

		lateralClause := ""
		if forEachPath != "" && len(branches) <= 1 {
			explodeFunc := "EXPLODE"
			if forEachOuter {
				explodeFunc = "OUTER EXPLODE"
			}
			arrCol := "r.code_tokens"
			if strings.HasPrefix(forEachPath, "name") || strings.HasPrefix(forEachPath, "address") || strings.HasPrefix(forEachPath, "telecom") {
				arrCol = "r.string_tokens"
			}
			lateralClause = fmt.Sprintf(" LATERAL VIEW %s(%s) fe_tbl AS fe_tok", explodeFunc, arrCol)
		}

		whereClause := strings.Join(whereConds, " AND ")
		var sqlQuery string
		if len(branches) > 1 {
			branchQueries := make([]string, 0, len(branches))
			for _, bCols := range branches {
				selectExprs := make([]string, 0, len(bCols))
				for _, col := range bCols {
					exprPath := substituteViewConstants(strings.TrimSpace(col.Path), constMap)
					sqlExpr, _ := dorisZeroJSONColumnExpr(exprPath, isCore)
					selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", sqlExpr, col.Name))
				}
				branchQueries = append(branchQueries, fmt.Sprintf("SELECT %s FROM %s r WHERE %s",
					strings.Join(selectExprs, ", "),
					targetTable,
					whereClause,
				))
			}
			sqlQuery = strings.Join(branchQueries, " UNION ALL ")
		} else {
			selectExprs := make([]string, 0, len(columns))
			for _, col := range columns {
				exprPath := substituteViewConstants(strings.TrimSpace(col.Path), constMap)
				sqlExpr, _ := dorisZeroJSONForEachColumnExpr(exprPath, forEachPath, isCore)
				selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", sqlExpr, col.Name))
			}
			sqlQuery = fmt.Sprintf("SELECT %s FROM %s r%s WHERE %s ORDER BY r.res_id ASC",
				strings.Join(selectExprs, ", "),
				targetTable,
				lateralClause,
				whereClause,
			)
		}
		if opts.limit > 0 {
			sqlQuery += fmt.Sprintf(" LIMIT %d", opts.limit)
		}

		return zeroJSONTranspiledSQL{
			dialect:      "doris",
			storageModel: "zero-json-proto3-doris-mow",
			targetTable:  targetTable,
			sql:          sqlQuery,
			columnMaps:   colMaps,
		}, nil
	}

	// PostgreSQL / SQLite Zero-JSONB spidx_* covering index transpilation
	colMaps := make([]map[string]any, 0, len(columns))
	for _, col := range columns {
		exprPath := substituteViewConstants(strings.TrimSpace(col.Path), constMap)
		sqlExpr, sourceTbl := spidxZeroJSONForEachColumnExpr(exprPath, forEachPath)
		colMaps = append(colMaps, map[string]any{
			"name": "column",
			"part": []map[string]any{
				{"name": "name", "valueString": col.Name},
				{"name": "fhirpath", "valueString": exprPath},
				{"name": "storageTable", "valueString": sourceTbl},
				{"name": "sqlExpression", "valueString": sqlExpr},
			},
		})
	}

	var whereConds []string
	whereConds = append(whereConds, fmt.Sprintf("r.tenant_id = '%s'", escapedTenant))
	whereConds = append(whereConds, fmt.Sprintf("r.res_type = '%s'", strings.ReplaceAll(resType, "'", "''")))
	if dialect == "sqlite" {
		whereConds = append(whereConds, "r.is_deleted = 0")
	} else {
		whereConds = append(whereConds, "r.is_deleted = FALSE")
	}
	if opts.patientID != "" {
		patID := opts.patientID
		if slash := strings.LastIndexByte(patID, '/'); slash != -1 {
			patID = patID[slash+1:]
		}
		escapedPat := strings.ReplaceAll(patID, "'", "''")
		if strings.EqualFold(resType, "Patient") {
			whereConds = append(whereConds, fmt.Sprintf("r.res_id = '%s'", escapedPat))
		} else {
			whereConds = append(whereConds, fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_reference ref WHERE ref.tenant_id = r.tenant_id AND ref.res_type = r.res_type AND ref.res_id = r.res_id AND ref.sp_name IN ('patient', 'subject') AND ref.target_id = '%s')", escapedPat))
		}
	}
	for _, w := range opts.viewDef.Where {
		wPath := substituteViewConstants(strings.TrimSpace(w.Path), constMap)
		if wSQL := spidxZeroJSONWhereExpr(wPath); wSQL != "" {
			whereConds = append(whereConds, wSQL)
		}
	}

	joinClause := ""
	if forEachPath != "" && len(branches) <= 1 {
		joinType := "INNER JOIN"
		if forEachOuter {
			joinType = "LEFT JOIN"
		}
		feTable := "spidx_token"
		feSpCond := fmt.Sprintf("fe.sp_name = '%s'", normalizeFHIRPathToSearchParam(forEachPath))
		if strings.HasPrefix(forEachPath, "name") {
			feTable = "spidx_string"
			feSpCond = "fe.sp_name IN ('family', 'given', 'name')"
		} else if strings.HasPrefix(forEachPath, "address") {
			feTable = "spidx_string"
			feSpCond = "fe.sp_name = 'address'"
		}
		joinClause = fmt.Sprintf(" %s %s fe ON fe.tenant_id = r.tenant_id AND fe.res_type = r.res_type AND fe.res_id = r.res_id AND %s", joinType, feTable, feSpCond)
	}

	whereClause := strings.Join(whereConds, " AND ")
	var sqlQuery string
	if len(branches) > 1 {
		branchQueries := make([]string, 0, len(branches))
		for _, bCols := range branches {
			selectExprs := make([]string, 0, len(bCols))
			for _, col := range bCols {
				exprPath := substituteViewConstants(strings.TrimSpace(col.Path), constMap)
				sqlExpr, _ := spidxZeroJSONColumnExpr(exprPath)
				selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", sqlExpr, col.Name))
			}
			branchQueries = append(branchQueries, fmt.Sprintf("SELECT %s FROM fhir_resource r WHERE %s",
				strings.Join(selectExprs, ", "),
				whereClause,
			))
		}
		sqlQuery = strings.Join(branchQueries, " UNION ALL ")
	} else {
		selectExprs := make([]string, 0, len(columns))
		for _, col := range columns {
			exprPath := substituteViewConstants(strings.TrimSpace(col.Path), constMap)
			sqlExpr, _ := spidxZeroJSONForEachColumnExpr(exprPath, forEachPath)
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", sqlExpr, col.Name))
		}
		sqlQuery = fmt.Sprintf("SELECT %s FROM fhir_resource r%s WHERE %s ORDER BY r.res_id ASC",
			strings.Join(selectExprs, ", "),
			joinClause,
			whereClause,
		)
	}
	if opts.limit > 0 {
		sqlQuery += fmt.Sprintf(" LIMIT %d", opts.limit)
	}

	return zeroJSONTranspiledSQL{
		dialect:      dialect,
		storageModel: "zero-jsonb-proto3-spidx",
		targetTable:  "fhir_resource",
		sql:          sqlQuery,
		columnMaps:   colMaps,
	}, nil
}

func dorisZeroJSONForEachColumnExpr(fhirPath, forEachPath string, isCore bool) (string, string) {
	p := strings.TrimSpace(fhirPath)
	fe := strings.TrimSpace(forEachPath)
	if fe != "" && (p == fe || strings.HasPrefix(p, fe+".")) {
		if strings.HasSuffix(p, ".system") {
			return "SPLIT_PART(SPLIT_PART(fe_tok, '=', 2), '|', 1)", "fe_tok"
		}
		if strings.HasPrefix(fe, "name") || strings.HasPrefix(fe, "address") || strings.HasPrefix(fe, "telecom") {
			return "SPLIT_PART(fe_tok, '=', 2)", "fe_tok"
		}
		return "SPLIT_PART(fe_tok, '|', 2)", "fe_tok"
	}
	return dorisZeroJSONColumnExpr(fhirPath, isCore)
}

func spidxZeroJSONForEachColumnExpr(fhirPath, forEachPath string) (string, string) {
	p := strings.TrimSpace(fhirPath)
	fe := strings.TrimSpace(forEachPath)
	if fe != "" && (p == fe || strings.HasPrefix(p, fe+".")) {
		feTable := "spidx_token"
		if strings.HasPrefix(fe, "name") || strings.HasPrefix(fe, "address") {
			feTable = "spidx_string"
		}
		if strings.HasSuffix(p, ".system") {
			return "fe.sp_system", feTable
		}
		return "fe.sp_value", feTable
	}
	return spidxZeroJSONColumnExpr(fhirPath)
}

func dorisZeroJSONColumnExpr(fhirPath string, isCore bool) (string, string) {
	p := strings.TrimSpace(fhirPath)
	if strings.HasPrefix(p, "'") && strings.HasSuffix(p, "'") {
		return p, "constant"
	}
	codeCol := "r.primary_code"
	codeSysCol := "r.primary_code_sys"
	qtyCol := "r.primary_quantity"
	unitCodeCol := "r.primary_unit_code"
	unitSysCol := "r.primary_unit_sys"
	dateLowCol := "r.primary_date_low"
	dateHighCol := "r.primary_date_high"
	if isCore {
		codeCol = "r.code"
		codeSysCol = "r.code_system"
		qtyCol = "r.value_quantity"
		unitCodeCol = "r.value_unit_code"
		unitSysCol = "r.value_unit_sys"
		dateLowCol = "r.effective_start"
		dateHighCol = "r.effective_end"
	}

	switch p {
	case "id", "getResourceKey()":
		return "r.res_id", "res_id"
	case "subject.getReferenceKey('Patient')", "subject.getReferenceKey()", "patient.getReferenceKey('Patient')", "patient.getReferenceKey()":
		return "r.patient_id", "patient_id"
	case "encounter.getReferenceKey('Encounter')", "encounter.getReferenceKey()", "context.getReferenceKey('Encounter')":
		return "r.encounter_id", "encounter_id"
	case "code.coding.code.first()", "code.coding.code", "code.coding.first().code":
		return codeCol, strings.TrimPrefix(codeCol, "r.")
	case "code.coding.system.first()", "code.coding.system", "code.coding.first().system":
		return codeSysCol, strings.TrimPrefix(codeSysCol, "r.")
	case "valueQuantity.value":
		return qtyCol, strings.TrimPrefix(qtyCol, "r.")
	case "valueQuantity.code", "valueQuantity.unit":
		return unitCodeCol, strings.TrimPrefix(unitCodeCol, "r.")
	case "valueQuantity.system":
		return unitSysCol, strings.TrimPrefix(unitSysCol, "r.")
	case "effectiveDateTime", "effectivePeriod.start", "onsetDateTime", "birthDate", "authoredOn", "issued":
		return dateLowCol, strings.TrimPrefix(dateLowCol, "r.")
	case "effectivePeriod.end", "abatementDateTime":
		return dateHighCol, strings.TrimPrefix(dateHighCol, "r.")
	case "name.family.first()", "name.family", "family":
		return "element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE 'family=%', r.string_tokens)), 1)", "string_tokens[family]"
	case "name.given.first()", "name.given", "given":
		return "element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE 'given=%', r.string_tokens)), 1)", "string_tokens[given]"
	case "gender":
		return "element_at(array_map(x -> split_part(x, '|', 2), array_filter(x -> x LIKE 'gender=|%', r.code_tokens)), 1)", "code_tokens[gender]"
	case "status":
		return "element_at(array_map(x -> split_part(x, '|', 2), array_filter(x -> x LIKE 'status=|%', r.code_tokens)), 1)", "code_tokens[status]"
	case "active":
		return "array_contains(r.code_tokens, 'active=|true')", "code_tokens[active]"
	case "clinicalStatus.coding.code.first()", "clinicalStatus.coding.code":
		return "element_at(array_map(x -> split_part(x, '|', 2), array_filter(x -> x LIKE 'clinical-status=|%', r.code_tokens)), 1)", "code_tokens[clinical-status]"
	case "verificationStatus.coding.code.first()", "verificationStatus.coding.code":
		return "element_at(array_map(x -> split_part(x, '|', 2), array_filter(x -> x LIKE 'verification-status=|%', r.code_tokens)), 1)", "code_tokens[verification-status]"
	default:
		sp := normalizeFHIRPathToSearchParam(p)
		return fmt.Sprintf("element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE '%s=%%', r.string_tokens)), 1)", sp), "string_tokens[" + sp + "]"
	}
}

func dorisZeroJSONWhereExpr(fhirPath string, isCore bool) string {
	p := strings.TrimSpace(fhirPath)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "not(") && strings.HasSuffix(p, ")") {
		inner := strings.TrimSpace(p[4 : len(p)-1])
		if sub := dorisZeroJSONWhereExpr(inner, isCore); sub != "" {
			return "NOT (" + sub + ")"
		}
	}
	if strings.HasSuffix(p, ".not()") {
		inner := strings.TrimSpace(p[:len(p)-len(".not()")])
		if sub := dorisZeroJSONWhereExpr(inner, isCore); sub != "" {
			return "NOT (" + sub + ")"
		}
	}
	if orParts := splitTopLevelBooleanWord(p, "or"); len(orParts) > 1 {
		var compiled []string
		for _, part := range orParts {
			if sub := dorisZeroJSONWhereExpr(part, isCore); sub != "" {
				compiled = append(compiled, sub)
			}
		}
		if len(compiled) > 0 {
			return "(" + strings.Join(compiled, " OR ") + ")"
		}
	}
	if andParts := splitTopLevelBooleanWord(p, "and"); len(andParts) > 1 {
		var compiled []string
		for _, part := range andParts {
			if sub := dorisZeroJSONWhereExpr(part, isCore); sub != "" {
				compiled = append(compiled, sub)
			}
		}
		if len(compiled) > 0 {
			return strings.Join(compiled, " AND ")
		}
	}

	codeCol := "r.primary_code"
	qtyCol := "r.primary_quantity"
	dateLowCol := "r.primary_date_low"
	if isCore {
		codeCol = "r.code"
		qtyCol = "r.value_quantity"
		dateLowCol = "r.effective_start"
	}
	if strings.HasSuffix(p, ".exists()") {
		fieldPath := strings.TrimSpace(p[:len(p)-len(".exists()")])
		switch fieldPath {
		case "valueQuantity", "valueQuantity.value":
			return fmt.Sprintf("%s IS NOT NULL", qtyCol)
		case "effectiveDateTime", "effectivePeriod.start", "onsetDateTime", "birthDate", "authoredOn", "issued":
			return fmt.Sprintf("%s IS NOT NULL", dateLowCol)
		case "subject", "patient":
			return "(r.patient_id IS NOT NULL AND r.patient_id <> '')"
		case "encounter", "context":
			return "(r.encounter_id IS NOT NULL AND r.encounter_id <> '')"
		case "code", "code.coding", "code.coding.code":
			return fmt.Sprintf("(%s IS NOT NULL AND %s <> '')", codeCol, codeCol)
		case "name", "name.family", "family", "name.given", "given", "address":
			sp := normalizeFHIRPathToSearchParam(fieldPath)
			return fmt.Sprintf("array_exists(x -> x LIKE '%s=%%', r.string_tokens)", sp)
		default:
			sp := normalizeFHIRPathToSearchParam(fieldPath)
			return fmt.Sprintf("array_exists(x -> x LIKE '%s=|%%', r.code_tokens)", sp)
		}
	}
	if strings.HasSuffix(p, ".empty()") {
		fieldPath := strings.TrimSpace(p[:len(p)-len(".empty()")])
		if sub := dorisZeroJSONWhereExpr(fieldPath+".exists()", isCore); sub != "" {
			return "NOT (" + sub + ")"
		}
	}
	if idx := strings.Index(p, ".startsWith("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".startsWith("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		if fieldPath == "code" || fieldPath == "code.coding.code" {
			return fmt.Sprintf("%s LIKE '%s%%'", codeCol, escapedArg)
		}
		sp := normalizeFHIRPathToSearchParam(fieldPath)
		return fmt.Sprintf("array_exists(x -> x LIKE '%s=%s%%', r.string_tokens)", sp, escapedArg)
	}
	if idx := strings.Index(p, ".contains("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".contains("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		if fieldPath == "code" || fieldPath == "code.coding.code" {
			return fmt.Sprintf("%s LIKE '%%%s%%'", codeCol, escapedArg)
		}
		sp := normalizeFHIRPathToSearchParam(fieldPath)
		return fmt.Sprintf("array_exists(x -> x LIKE '%s=%%%s%%', r.string_tokens)", sp, escapedArg)
	}
	if strings.HasPrefix(p, "code.coding.exists(") && strings.HasSuffix(p, ")") {
		inner := strings.TrimSpace(p[len("code.coding.exists(") : len(p)-1])
		if idx := strings.Index(inner, "code"); idx != -1 {
			if q1 := strings.IndexByte(inner[idx:], '\''); q1 != -1 {
				rest := inner[idx+q1+1:]
				if q2 := strings.IndexByte(rest, '\''); q2 != -1 {
					codeVal := strings.ReplaceAll(rest[:q2], "'", "''")
					return fmt.Sprintf("(%s = '%s' OR array_contains(r.code_tokens, 'code=|%s'))", codeCol, codeVal, codeVal)
				}
			}
		}
	}
	lhs, op, rhs, ok := parseViewWhereComparison(p)
	if !ok {
		return ""
	}
	trimmedRHS := strings.Trim(strings.TrimSpace(rhs), "'\"")
	escapedRHS := strings.ReplaceAll(trimmedRHS, "'", "''")

	if lhs == "valueQuantity.value" {
		if _, err := strconv.ParseFloat(trimmedRHS, 64); err == nil {
			sqlOp := op
			if sqlOp == "!=" {
				sqlOp = "<>"
			}
			return fmt.Sprintf("%s %s %s", qtyCol, sqlOp, trimmedRHS)
		}
	}
	switch lhs {
	case "effectiveDateTime", "effectivePeriod.start", "onsetDateTime", "birthDate", "authoredOn", "issued":
		sqlOp := op
		if sqlOp == "!=" {
			sqlOp = "<>"
		}
		return fmt.Sprintf("%s %s '%s'", dateLowCol, sqlOp, escapedRHS)
	}

	sp := normalizeFHIRPathToSearchParam(lhs)
	if op == "!=" || op == "<>" {
		return fmt.Sprintf("NOT array_contains(r.code_tokens, '%s=|%s')", sp, escapedRHS)
	}
	return fmt.Sprintf("array_contains(r.code_tokens, '%s=|%s')", sp, escapedRHS)
}

func spidxZeroJSONColumnExpr(fhirPath string) (string, string) {
	p := strings.TrimSpace(fhirPath)
	if strings.HasPrefix(p, "'") && strings.HasSuffix(p, "'") {
		return p, "constant"
	}
	switch p {
	case "id", "getResourceKey()":
		return "r.res_id", "fhir_resource.res_id"
	case "subject.getReferenceKey('Patient')", "subject.getReferenceKey()", "patient.getReferenceKey('Patient')", "patient.getReferenceKey()":
		return "(SELECT ref.target_id FROM spidx_reference ref WHERE ref.tenant_id = r.tenant_id AND ref.res_type = r.res_type AND ref.res_id = r.res_id AND ref.sp_name IN ('patient', 'subject') LIMIT 1)", "spidx_reference"
	case "encounter.getReferenceKey('Encounter')", "encounter.getReferenceKey()":
		return "(SELECT ref.target_id FROM spidx_reference ref WHERE ref.tenant_id = r.tenant_id AND ref.res_type = r.res_type AND ref.res_id = r.res_id AND ref.sp_name = 'encounter' LIMIT 1)", "spidx_reference"
	case "code.coding.code.first()", "code.coding.code", "code.coding.first().code":
		return "(SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'code' LIMIT 1)", "spidx_token"
	case "code.coding.system.first()", "code.coding.system", "code.coding.first().system":
		return "(SELECT t.sp_system FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'code' LIMIT 1)", "spidx_token"
	case "valueQuantity.value":
		return "(SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1)", "spidx_quantity"
	case "valueQuantity.code", "valueQuantity.unit":
		return "(SELECT q.sp_code FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1)", "spidx_quantity"
	case "effectiveDateTime", "effectivePeriod.start", "onsetDateTime", "birthDate", "authoredOn":
		sp := "date"
		switch p {
		case "birthDate":
			sp = "birthdate"
		case "onsetDateTime":
			sp = "onset-date"
		case "authoredOn":
			sp = "authoredon"
		}
		return fmt.Sprintf("(SELECT d.sp_low FROM spidx_date d WHERE d.tenant_id = r.tenant_id AND d.res_type = r.res_type AND d.res_id = r.res_id AND d.sp_name = '%s' LIMIT 1)", sp), "spidx_date"
	case "name.family.first()", "name.family", "family":
		return "(SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1)", "spidx_string"
	case "name.given.first()", "name.given", "given":
		return "(SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'given' LIMIT 1)", "spidx_string"
	case "gender", "status", "active", "clinicalStatus.coding.code.first()", "verificationStatus.coding.code.first()":
		sp := normalizeFHIRPathToSearchParam(p)
		return fmt.Sprintf("(SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' LIMIT 1)", sp), "spidx_token"
	default:
		sp := normalizeFHIRPathToSearchParam(p)
		return fmt.Sprintf("(SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' LIMIT 1)", sp), "spidx_string"
	}
}

func spidxZeroJSONWhereExpr(fhirPath string) string {
	p := strings.TrimSpace(fhirPath)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "not(") && strings.HasSuffix(p, ")") {
		inner := strings.TrimSpace(p[4 : len(p)-1])
		if sub := spidxZeroJSONWhereExpr(inner); sub != "" {
			return "NOT (" + sub + ")"
		}
	}
	if strings.HasSuffix(p, ".not()") {
		inner := strings.TrimSpace(p[:len(p)-len(".not()")])
		if sub := spidxZeroJSONWhereExpr(inner); sub != "" {
			return "NOT (" + sub + ")"
		}
	}
	if orParts := splitTopLevelBooleanWord(p, "or"); len(orParts) > 1 {
		var compiled []string
		for _, part := range orParts {
			if sub := spidxZeroJSONWhereExpr(part); sub != "" {
				compiled = append(compiled, sub)
			}
		}
		if len(compiled) > 0 {
			return "(" + strings.Join(compiled, " OR ") + ")"
		}
	}
	if andParts := splitTopLevelBooleanWord(p, "and"); len(andParts) > 1 {
		var compiled []string
		for _, part := range andParts {
			if sub := spidxZeroJSONWhereExpr(part); sub != "" {
				compiled = append(compiled, sub)
			}
		}
		if len(compiled) > 0 {
			return strings.Join(compiled, " AND ")
		}
	}

	if strings.HasSuffix(p, ".exists()") {
		fieldPath := strings.TrimSpace(p[:len(p)-len(".exists()")])
		switch fieldPath {
		case "valueQuantity", "valueQuantity.value":
			return "EXISTS (SELECT 1 FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity')"
		case "effectiveDateTime", "effectivePeriod.start", "onsetDateTime", "birthDate", "authoredOn", "issued":
			sp := "date"
			switch fieldPath {
			case "birthDate":
				sp = "birthdate"
			case "onsetDateTime":
				sp = "onset-date"
			case "authoredOn":
				sp = "authoredon"
			}
			return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_date d WHERE d.tenant_id = r.tenant_id AND d.res_type = r.res_type AND d.res_id = r.res_id AND d.sp_name = '%s')", sp)
		case "subject", "patient":
			return "EXISTS (SELECT 1 FROM spidx_reference ref WHERE ref.tenant_id = r.tenant_id AND ref.res_type = r.res_type AND ref.res_id = r.res_id AND ref.sp_name IN ('patient', 'subject'))"
		case "encounter", "context":
			return "EXISTS (SELECT 1 FROM spidx_reference ref WHERE ref.tenant_id = r.tenant_id AND ref.res_type = r.res_type AND ref.res_id = r.res_id AND ref.sp_name = 'encounter')"
		case "name", "name.family", "family", "name.given", "given", "address":
			sp := normalizeFHIRPathToSearchParam(fieldPath)
			return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s')", sp)
		default:
			sp := normalizeFHIRPathToSearchParam(fieldPath)
			return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s')", sp)
		}
	}
	if strings.HasSuffix(p, ".empty()") {
		fieldPath := strings.TrimSpace(p[:len(p)-len(".empty()")])
		if sub := spidxZeroJSONWhereExpr(fieldPath + ".exists()"); sub != "" {
			return "NOT (" + sub + ")"
		}
	}
	if idx := strings.Index(p, ".startsWith("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".startsWith("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		sp := normalizeFHIRPathToSearchParam(fieldPath)
		return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' AND s.sp_value LIKE '%s%%')", sp, escapedArg)
	}
	if idx := strings.Index(p, ".contains("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".contains("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		sp := normalizeFHIRPathToSearchParam(fieldPath)
		return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' AND s.sp_value LIKE '%%%s%%')", sp, escapedArg)
	}
	if strings.HasPrefix(p, "code.coding.exists(") && strings.HasSuffix(p, ")") {
		inner := strings.TrimSpace(p[len("code.coding.exists(") : len(p)-1])
		if idx := strings.Index(inner, "code"); idx != -1 {
			if q1 := strings.IndexByte(inner[idx:], '\''); q1 != -1 {
				rest := inner[idx+q1+1:]
				if q2 := strings.IndexByte(rest, '\''); q2 != -1 {
					codeVal := strings.ReplaceAll(rest[:q2], "'", "''")
					return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'code' AND t.sp_value = '%s')", codeVal)
				}
			}
		}
	}
	lhs, op, rhs, ok := parseViewWhereComparison(p)
	if !ok {
		return ""
	}
	trimmedRHS := strings.Trim(strings.TrimSpace(rhs), "'\"")
	escapedRHS := strings.ReplaceAll(trimmedRHS, "'", "''")

	if lhs == "valueQuantity.value" {
		if _, err := strconv.ParseFloat(trimmedRHS, 64); err == nil {
			sqlOp := op
			if sqlOp == "!=" {
				sqlOp = "<>"
			}
			return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' AND q.sp_value %s %s)", sqlOp, trimmedRHS)
		}
	}
	switch lhs {
	case "effectiveDateTime", "effectivePeriod.start", "onsetDateTime", "birthDate", "authoredOn", "issued":
		sp := "date"
		switch lhs {
		case "birthDate":
			sp = "birthdate"
		case "onsetDateTime":
			sp = "onset-date"
		case "authoredOn":
			sp = "authoredon"
		}
		sqlOp := op
		if sqlOp == "!=" {
			sqlOp = "<>"
		}
		return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_date d WHERE d.tenant_id = r.tenant_id AND d.res_type = r.res_type AND d.res_id = r.res_id AND d.sp_name = '%s' AND d.sp_low %s '%s')", sp, sqlOp, escapedRHS)
	}

	sp := normalizeFHIRPathToSearchParam(lhs)
	if op == "!=" || op == "<>" {
		return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_value = '%s')", sp, escapedRHS)
	}
	return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_value = '%s')", sp, escapedRHS)
}

func splitTopLevelBooleanWord(expr, keyword string) []string {
	kw := " " + strings.ToLower(keyword) + " "
	lower := strings.ToLower(expr)
	var out []string
	inQuote := false
	parenDepth := 0
	start := 0
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if ch == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		if ch == '(' {
			parenDepth++
			continue
		}
		if ch == ')' && parenDepth > 0 {
			parenDepth--
			continue
		}
		if parenDepth == 0 && i+len(kw) <= len(expr) && lower[i:i+len(kw)] == kw {
			part := strings.TrimSpace(expr[start:i])
			if part != "" {
				out = append(out, part)
			}
			i += len(kw) - 1
			start = i + 1
		}
	}
	if len(out) == 0 {
		return nil
	}
	if tail := strings.TrimSpace(expr[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}

func parseViewWhereComparison(expr string) (lhs, op, rhs string, ok bool) {
	inQuote := false
	parenDepth := 0
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if ch == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		if ch == '(' {
			parenDepth++
			continue
		}
		if ch == ')' && parenDepth > 0 {
			parenDepth--
			continue
		}
		if parenDepth > 0 {
			continue
		}
		if i+2 <= len(expr) {
			two := expr[i : i+2]
			if two == ">=" || two == "<=" || two == "!=" || two == "<>" {
				return strings.TrimSpace(expr[:i]), two, strings.TrimSpace(expr[i+2:]), true
			}
		}
		if ch == '=' || ch == '>' || ch == '<' {
			return strings.TrimSpace(expr[:i]), string(ch), strings.TrimSpace(expr[i+1:]), true
		}
	}
	return "", "", "", false
}

func normalizeFHIRPathToSearchParam(path string) string {
	p := strings.TrimSpace(path)
	switch {
	case strings.HasPrefix(p, "clinicalStatus"):
		return "clinical-status"
	case strings.HasPrefix(p, "verificationStatus"):
		return "verification-status"
	case strings.HasPrefix(p, "name.family"):
		return "family"
	case strings.HasPrefix(p, "name.given"):
		return "given"
	default:
		if dot := strings.IndexByte(p, '.'); dot != -1 {
			p = p[:dot]
		}
		return strings.ToLower(strings.ReplaceAll(p, "'", ""))
	}
}
