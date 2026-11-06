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
			if isProtoContentType(r) {
				if pm, pErr := s.protoToFHIRMap("Parameters", trimmed); pErr == nil && pm != nil {
					if jb, jErr := json.Marshal(pm); jErr == nil {
						trimmed = jb
					}
				}
			}
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
		if status == http.StatusNotFound {
			writeValidateErrorOutcome(w, r, status, "not-found", err.Error())
		} else {
			writeValidateErrorOutcome(w, r, status, "invalid", err.Error())
		}
		return
	}

	targetType := strings.TrimSpace(opts.viewDef.Resource)
	if _, ok := s.protoRegistry[targetType]; !ok {
		writeValidateErrorOutcome(w, r, http.StatusUnprocessableEntity, "not-supported", fmt.Sprintf("unsupported ViewDefinition target resource type %q", targetType))
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
	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeValidateErrorOutcome(w, r, http.StatusBadRequest, "invalid", "failed to read request body")
			return
		}
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 {
			if isProtoContentType(r) {
				if _, err := s.protoToFHIRMap("Parameters", trimmed); err != nil {
					writeValidateErrorOutcome(w, r, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid Protobuf Parameters payload: %v", err))
					return
				}
			} else {
				var probe struct {
					ResourceType string `json:"resourceType"`
				}
				if err := json.Unmarshal(trimmed, &probe); err != nil || (probe.ResourceType != "" && probe.ResourceType != "Parameters") {
					writeValidateErrorOutcome(w, r, http.StatusBadRequest, "invalid", "invalid Parameters payload")
					return
				}
			}
		}
	}

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
			writeValidateErrorOutcome(w, r, http.StatusInternalServerError, "exception", fmt.Sprintf("analytical sync failed: %v", err))
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
		{"name": "totalSyncedResources", "valueInteger": int(totalSynced)},
		{"name": "replicatedWrites", "valueInteger": int(repWrites)},
		{"name": "replicatedDeletes", "valueInteger": int(repDeletes)},
		{"name": "replicationErrors", "valueInteger": int(repErrors)},
	}
	if lastSyncTime != "" {
		params = append(params, map[string]any{"name": "lastSyncTime", "valueDateTime": lastSyncTime})
	}

	s.writeOperationResponse(w, r, http.StatusOK, "Parameters", map[string]any{
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
		if status == http.StatusNotFound {
			writeValidateErrorOutcome(w, r, status, "not-found", err.Error())
		} else {
			writeValidateErrorOutcome(w, r, status, "invalid", err.Error())
		}
		return
	}

	targetType := strings.TrimSpace(opts.viewDef.Resource)
	if _, ok := s.protoRegistry[targetType]; !ok {
		writeValidateErrorOutcome(w, r, http.StatusUnprocessableEntity, "not-supported", fmt.Sprintf("unsupported ViewDefinition target resource type %q", targetType))
		return
	}

	res, err := s.transpileViewDefinitionToZeroJSONSQL(tenant, opts)
	if err != nil {
		writeValidateErrorOutcome(w, r, http.StatusBadRequest, "invalid", err.Error())
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

	s.writeOperationResponse(w, r, http.StatusOK, "Parameters", map[string]any{
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
			var sqlExpr, sourceCol string
			if col.Collection {
				sqlExpr, sourceCol = dorisZeroJSONCollectionColumnExpr(exprPath)
			} else {
				sqlExpr, sourceCol = dorisZeroJSONForEachColumnExpr(exprPath, forEachPath, isCore)
			}
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
				var sqlExpr string
				if col.Collection {
					sqlExpr, _ = dorisZeroJSONCollectionColumnExpr(exprPath)
				} else {
					sqlExpr, _ = dorisZeroJSONForEachColumnExpr(exprPath, forEachPath, isCore)
				}
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
		var sqlExpr, sourceTbl string
		if col.Collection {
			sqlExpr, sourceTbl = spidxZeroJSONCollectionColumnExpr(exprPath, dialect)
		} else {
			sqlExpr, sourceTbl = spidxZeroJSONForEachColumnExpr(exprPath, forEachPath, dialect)
		}
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
		if wSQL := spidxZeroJSONWhereExprForDialect(wPath, dialect); wSQL != "" {
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
				sqlExpr, _ := spidxZeroJSONColumnExpr(exprPath, dialect)
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
			var sqlExpr string
			if col.Collection {
				sqlExpr, _ = spidxZeroJSONCollectionColumnExpr(exprPath, dialect)
			} else {
				sqlExpr, _ = spidxZeroJSONForEachColumnExpr(exprPath, forEachPath, dialect)
			}
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

func dorisZeroJSONCollectionColumnExpr(fhirPath string) (string, string) {
	p := normalizePolymorphicFHIRPath(strings.TrimSpace(fhirPath))
	if strings.HasSuffix(p, ".sort()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".sort()")])
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("ARRAY_SORT(%s)", collExpr), strings.TrimSuffix(collSrc, "[]") + ".sort[]"
	}
	if strings.HasSuffix(p, ".sortDesc()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".sortDesc()")])
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("ARRAY_REVERSE_SORT(%s)", collExpr), strings.TrimSuffix(collSrc, "[]") + ".sortDesc[]"
	}
	if strings.HasSuffix(p, ".distinct()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".distinct()")])
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("ARRAY_DISTINCT(%s)", collExpr), strings.TrimSuffix(collSrc, "[]") + ".distinct[]"
	}
	if strings.HasSuffix(p, ".tail()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".tail()")])
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("ARRAY_SLICE(%s, 2)", collExpr), strings.TrimSuffix(collSrc, "[]") + ".tail[]"
	}
	if idx := strings.LastIndex(p, ".union("); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(".union("):len(p)-1], ").") {
		basePath := strings.TrimSpace(p[:idx])
		otherPath := strings.TrimSpace(p[idx+len(".union(") : len(p)-1])
		lhsColl, lhsSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		rhsColl, _ := dorisZeroJSONCollectionColumnExpr(otherPath)
		return fmt.Sprintf("ARRAY_UNION(%s, %s)", lhsColl, rhsColl), strings.TrimSuffix(lhsSrc, "[]") + ".union[]"
	}
	if idx := strings.LastIndex(p, ".combine("); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(".combine("):len(p)-1], ").") {
		basePath := strings.TrimSpace(p[:idx])
		otherPath := strings.TrimSpace(p[idx+len(".combine(") : len(p)-1])
		lhsColl, lhsSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		rhsColl, _ := dorisZeroJSONCollectionColumnExpr(otherPath)
		return fmt.Sprintf("ARRAY_CONCAT(%s, %s)", lhsColl, rhsColl), strings.TrimSuffix(lhsSrc, "[]") + ".combine[]"
	}
	if idx := strings.LastIndex(p, ".intersect("); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(".intersect("):len(p)-1], ").") {
		basePath := strings.TrimSpace(p[:idx])
		otherPath := strings.TrimSpace(p[idx+len(".intersect(") : len(p)-1])
		lhsColl, lhsSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		rhsColl, _ := dorisZeroJSONCollectionColumnExpr(otherPath)
		return fmt.Sprintf("ARRAY_INTERSECT(%s, %s)", lhsColl, rhsColl), strings.TrimSuffix(lhsSrc, "[]") + ".intersect[]"
	}
	if idx := strings.LastIndex(p, ".exclude("); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(".exclude("):len(p)-1], ").") {
		basePath := strings.TrimSpace(p[:idx])
		otherPath := strings.TrimSpace(p[idx+len(".exclude(") : len(p)-1])
		lhsColl, lhsSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		rhsColl, _ := dorisZeroJSONCollectionColumnExpr(otherPath)
		return fmt.Sprintf("ARRAY_EXCEPT(%s, %s)", lhsColl, rhsColl), strings.TrimSuffix(lhsSrc, "[]") + ".exclude[]"
	}
	if idx := strings.LastIndex(p, ".split("); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(".split("):len(p)-1], ").") {
		basePath := strings.TrimSpace(p[:idx])
		rawSep := strings.Trim(strings.TrimSpace(p[idx+len(".split("):len(p)-1]), "'\"")
		escapedSep := strings.ReplaceAll(rawSep, "'", "''")
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, false)
		return fmt.Sprintf("SPLIT_BY_STRING(%s, '%s')", colExpr, escapedSep), colSrc + ".split[]"
	}
	if strings.HasSuffix(p, ".toChars()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toChars()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, true)
		return fmt.Sprintf("SPLIT_BY_STRING(%s, '')", colExpr), colSrc + ".toChars[]"
	}
	if idx := strings.LastIndex(p, ".skip("); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(".skip("):len(p)-1], ").") {
		basePath := strings.TrimSpace(p[:idx])
		if n, err := strconv.Atoi(strings.TrimSpace(p[idx+len(".skip(") : len(p)-1])); err == nil && n >= 0 {
			collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
			return fmt.Sprintf("ARRAY_SLICE(%s, %d)", collExpr, n+1), strings.TrimSuffix(collSrc, "[]") + ".skip[]"
		}
	}
	if idx := strings.LastIndex(p, ".take("); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(".take("):len(p)-1], ").") {
		basePath := strings.TrimSpace(p[:idx])
		if n, err := strconv.Atoi(strings.TrimSpace(p[idx+len(".take(") : len(p)-1])); err == nil && n >= 0 {
			collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
			return fmt.Sprintf("ARRAY_SLICE(%s, 1, %d)", collExpr, n), strings.TrimSuffix(collSrc, "[]") + ".take[]"
		}
	}
	if spSys, sysVal, _, okSys := parseSystemFilteredTokenPath(p); okSys {
		escapedSys := strings.ReplaceAll(sysVal, "'", "''")
		return fmt.Sprintf("ARRAY_MAP(x -> SPLIT_PART(x, '|', 2), ARRAY_FILTER(x -> x LIKE '%s=%s|%%', r.code_tokens))", spSys, escapedSys), "code_tokens[" + spSys + "|" + sysVal + "][]"
	}
	sp := normalizeFHIRPathToSearchParam(p)
	if strings.HasPrefix(p, "name") || p == "given" || p == "family" || strings.HasPrefix(p, "address") || strings.HasPrefix(p, "telecom") {
		return fmt.Sprintf("ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE '%s=%%', r.string_tokens))", sp), "string_tokens[" + sp + "][]"
	}
	return fmt.Sprintf("ARRAY_MAP(x -> SPLIT_PART(x, '|', 2), ARRAY_FILTER(x -> x LIKE '%s=|%%', r.code_tokens))", sp), "code_tokens[" + sp + "][]"
}

func splitTopLevelCommaArgs(inner string) []string {
	var out []string
	inQuote := false
	parenDepth := 0
	start := 0
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
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
		if parenDepth == 0 && ch == ',' {
			if part := strings.TrimSpace(inner[start:i]); part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	if tail := strings.TrimSpace(inner[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}

func isSetOpFHIRPath(p string) bool {
	for _, call := range []string{".union(", ".combine(", ".intersect(", ".exclude("} {
		if idx := strings.LastIndex(p, call); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(call):len(p)-1], ").") {
			return true
		}
	}
	return false
}

func spidxSubqueryForPath(fhirPath string) (string, string) {
	p := normalizePolymorphicFHIRPath(strings.TrimSpace(fhirPath))
	for _, setSpec := range []struct {
		call   string
		sqlOp  string
		suffix string
	}{
		{".union(", "UNION", ".union"},
		{".combine(", "UNION ALL", ".combine"},
		{".intersect(", "INTERSECT", ".intersect"},
		{".exclude(", "EXCEPT", ".exclude"},
	} {
		if idx := strings.LastIndex(p, setSpec.call); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(setSpec.call):len(p)-1], ").") {
			basePath := strings.TrimSpace(p[:idx])
			otherPath := strings.TrimSpace(p[idx+len(setSpec.call) : len(p)-1])
			lhsSub, lhsSrc := spidxSubqueryForPath(basePath)
			rhsSub, _ := spidxSubqueryForPath(otherPath)
			return fmt.Sprintf("%s %s %s", lhsSub, setSpec.sqlOp, rhsSub), lhsSrc + setSpec.suffix
		}
	}
	if spSys, sysVal, _, okSys := parseSystemFilteredTokenPath(p); okSys {
		escapedSys := strings.ReplaceAll(sysVal, "'", "''")
		return fmt.Sprintf("SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s'", spSys, escapedSys), "spidx_token"
	}
	sp := normalizeFHIRPathToSearchParam(p)
	isStringParam := strings.HasPrefix(p, "name") || p == "given" || p == "family" || strings.HasPrefix(p, "address") || strings.HasPrefix(p, "telecom")
	if isStringParam {
		return fmt.Sprintf("SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s'", sp), "spidx_string"
	}
	return fmt.Sprintf("SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s'", sp), "spidx_token"
}

type spidxCollectionModifiers struct {
	basePath   string
	isDistinct bool
	sortDir    string
	sortName   string
	isTail     bool
	hasSkip    bool
	hasTake    bool
	offset     int
	takeLimit  int
	tag        string
}

func (m spidxCollectionModifiers) hasSubqueryMods() bool {
	return m.sortDir != "" || m.isTail || m.hasSkip || m.hasTake
}

func (m spidxCollectionModifiers) hasAnyMods() bool {
	return m.isDistinct || m.hasSubqueryMods()
}

func parseSpidxCollectionModifiers(fhirPath string) spidxCollectionModifiers {
	cur := strings.TrimSpace(fhirPath)
	var mods spidxCollectionModifiers
	var tagParts []string
	for {
		if strings.HasSuffix(cur, ".distinct()") {
			mods.isDistinct = true
			tagParts = append([]string{".distinct"}, tagParts...)
			cur = strings.TrimSpace(cur[:len(cur)-len(".distinct()")])
			continue
		}
		if strings.HasSuffix(cur, ".sort()") {
			mods.sortDir = "ASC"
			mods.sortName = "sort"
			tagParts = append([]string{".sort"}, tagParts...)
			cur = strings.TrimSpace(cur[:len(cur)-len(".sort()")])
			continue
		}
		if strings.HasSuffix(cur, ".sortDesc()") {
			mods.sortDir = "DESC"
			mods.sortName = "sortDesc"
			tagParts = append([]string{".sortDesc"}, tagParts...)
			cur = strings.TrimSpace(cur[:len(cur)-len(".sortDesc()")])
			continue
		}
		if strings.HasSuffix(cur, ".tail()") {
			mods.isTail = true
			mods.offset++
			tagParts = append([]string{".tail"}, tagParts...)
			cur = strings.TrimSpace(cur[:len(cur)-len(".tail()")])
			continue
		}
		if idx := strings.LastIndex(cur, ".skip("); idx != -1 && strings.HasSuffix(cur, ")") && !strings.Contains(cur[idx+len(".skip("):len(cur)-1], ").") {
			if n, err := strconv.Atoi(strings.TrimSpace(cur[idx+len(".skip(") : len(cur)-1])); err == nil && n >= 0 {
				mods.hasSkip = true
				mods.offset += n
				tagParts = append([]string{".skip"}, tagParts...)
				cur = strings.TrimSpace(cur[:idx])
				continue
			}
		}
		if idx := strings.LastIndex(cur, ".take("); idx != -1 && strings.HasSuffix(cur, ")") && !strings.Contains(cur[idx+len(".take("):len(cur)-1], ").") {
			if n, err := strconv.Atoi(strings.TrimSpace(cur[idx+len(".take(") : len(cur)-1])); err == nil && n >= 0 {
				if !mods.hasTake || n < mods.takeLimit {
					mods.takeLimit = n
				}
				mods.hasTake = true
				tagParts = append([]string{".take"}, tagParts...)
				cur = strings.TrimSpace(cur[:idx])
				continue
			}
		}
		break
	}
	mods.basePath = cur
	mods.tag = strings.Join(tagParts, "")
	return mods
}

func buildModifiedSpidxSubquery(mods spidxCollectionModifiers, dialect string) (string, string) {
	subQ, subSrc := spidxSubqueryForPath(mods.basePath)
	if mods.isDistinct {
		if strings.Contains(subQ, " UNION ALL ") {
			subQ = strings.ReplaceAll(subQ, " UNION ALL ", " UNION ")
		} else if !strings.Contains(subQ, " UNION ") && !strings.Contains(subQ, " INTERSECT ") && !strings.Contains(subQ, " EXCEPT ") {
			subQ = strings.Replace(subQ, "SELECT ", "SELECT DISTINCT ", 1)
		}
	}
	if mods.sortDir != "" {
		subQ += " ORDER BY sp_value " + mods.sortDir
	}
	if mods.hasTake {
		subQ += fmt.Sprintf(" LIMIT %d", mods.takeLimit)
	} else if mods.offset > 0 && dialect == "sqlite" {
		subQ += " LIMIT -1"
	}
	if mods.offset > 0 {
		subQ += fmt.Sprintf(" OFFSET %d", mods.offset)
	}
	return subQ, subSrc
}

func spidxSetOperandSubquery(fhirPath, dialect string) (string, string) {
	mods := parseSpidxCollectionModifiers(fhirPath)
	subQ, subSrc := buildModifiedSpidxSubquery(mods, dialect)
	if mods.hasSubqueryMods() {
		return fmt.Sprintf("SELECT sub.sp_value FROM (%s) sub", subQ), subSrc + mods.tag
	}
	return subQ, subSrc + mods.tag
}

func spidxZeroJSONCollectionColumnExpr(fhirPath, dialect string) (string, string) {
	p := normalizePolymorphicFHIRPath(strings.TrimSpace(fhirPath))
	if mods := parseSpidxCollectionModifiers(p); mods.hasAnyMods() {
		if mods.hasSubqueryMods() || isSetOpFHIRPath(mods.basePath) {
			subQ, subSrc := buildModifiedSpidxSubquery(mods, dialect)
			if dialect == "sqlite" {
				return fmt.Sprintf("(SELECT json_group_array(sub.sp_value) FROM (%s) sub)", subQ), subSrc + mods.tag + "[]"
			}
			return fmt.Sprintf("(SELECT COALESCE(ARRAY_AGG(sub.sp_value), ARRAY[]::text[]) FROM (%s) sub)", subQ), subSrc + mods.tag + "[]"
		}
		basePath := mods.basePath
		sp := normalizeFHIRPathToSearchParam(basePath)
		isStringParam := strings.HasPrefix(basePath, "name") || basePath == "given" || basePath == "family" || strings.HasPrefix(basePath, "address") || strings.HasPrefix(basePath, "telecom")
		if dialect == "sqlite" {
			if isStringParam {
				return fmt.Sprintf("(SELECT json_group_array(sub.sp_value) FROM (SELECT DISTINCT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s') sub)", sp), "spidx_string.distinct[]"
			}
			return fmt.Sprintf("(SELECT json_group_array(sub.sp_value) FROM (SELECT DISTINCT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s') sub)", sp), "spidx_token.distinct[]"
		}
		if isStringParam {
			return fmt.Sprintf("(SELECT COALESCE(ARRAY_AGG(DISTINCT s.sp_value), ARRAY[]::text[]) FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s')", sp), "spidx_string.distinct[]"
		}
		return fmt.Sprintf("(SELECT COALESCE(ARRAY_AGG(DISTINCT t.sp_value), ARRAY[]::text[]) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s')", sp), "spidx_token.distinct[]"
	}
	if idx := strings.LastIndex(p, ".split("); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(".split("):len(p)-1], ").") {
		basePath := strings.TrimSpace(p[:idx])
		rawSep := strings.Trim(strings.TrimSpace(p[idx+len(".split("):len(p)-1]), "'\"")
		escapedSep := strings.ReplaceAll(rawSep, "'", "''")
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("json_array(%s)", colExpr), colSrc + ".split[]"
		}
		return fmt.Sprintf("STRING_TO_ARRAY(%s, '%s')", colExpr, escapedSep), colSrc + ".split[]"
	}
	if strings.HasSuffix(p, ".toChars()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toChars()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("json_array(%s)", colExpr), colSrc + ".toChars[]"
		}
		return fmt.Sprintf("REGEXP_SPLIT_TO_ARRAY(%s, '')", colExpr), colSrc + ".toChars[]"
	}
	for _, setSpec := range []struct {
		call   string
		sqlOp  string
		suffix string
	}{
		{".union(", "UNION", ".union[]"},
		{".combine(", "UNION ALL", ".combine[]"},
		{".intersect(", "INTERSECT", ".intersect[]"},
		{".exclude(", "EXCEPT", ".exclude[]"},
	} {
		if idx := strings.LastIndex(p, setSpec.call); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(setSpec.call):len(p)-1], ").") {
			basePath := strings.TrimSpace(p[:idx])
			otherPath := strings.TrimSpace(p[idx+len(setSpec.call) : len(p)-1])
			lhsSub, lhsSrc := spidxSubqueryForPath(basePath)
			rhsSub, _ := spidxSubqueryForPath(otherPath)
			if dialect == "sqlite" {
				return fmt.Sprintf("(SELECT json_group_array(u.sp_value) FROM (%s %s %s) u)", lhsSub, setSpec.sqlOp, rhsSub), lhsSrc + setSpec.suffix
			}
			return fmt.Sprintf("(SELECT COALESCE(ARRAY_AGG(u.sp_value), ARRAY[]::text[]) FROM (%s %s %s) u)", lhsSub, setSpec.sqlOp, rhsSub), lhsSrc + setSpec.suffix
		}
	}
	if spSys, sysVal, _, okSys := parseSystemFilteredTokenPath(p); okSys {
		escapedSys := strings.ReplaceAll(sysVal, "'", "''")
		if dialect == "sqlite" {
			return fmt.Sprintf("(SELECT json_group_array(t.sp_value) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s')", spSys, escapedSys), "spidx_token[" + spSys + "][]"
		}
		return fmt.Sprintf("(SELECT COALESCE(ARRAY_AGG(t.sp_value), ARRAY[]::text[]) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s')", spSys, escapedSys), "spidx_token[" + spSys + "][]"
	}
	sp := normalizeFHIRPathToSearchParam(p)
	isStringParam := strings.HasPrefix(p, "name") || p == "given" || p == "family" || strings.HasPrefix(p, "address") || strings.HasPrefix(p, "telecom")
	if dialect == "sqlite" {
		if isStringParam {
			return fmt.Sprintf("(SELECT json_group_array(s.sp_value) FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s')", sp), "spidx_string[]"
		}
		return fmt.Sprintf("(SELECT json_group_array(t.sp_value) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s')", sp), "spidx_token[]"
	}
	if isStringParam {
		return fmt.Sprintf("(SELECT COALESCE(ARRAY_AGG(s.sp_value), ARRAY[]::text[]) FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s')", sp), "spidx_string[]"
	}
	return fmt.Sprintf("(SELECT COALESCE(ARRAY_AGG(t.sp_value), ARRAY[]::text[]) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s')", sp), "spidx_token[]"
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

func spidxZeroJSONForEachColumnExpr(fhirPath, forEachPath, dialect string) (string, string) {
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
	return spidxZeroJSONColumnExpr(fhirPath, dialect)
}

func findMatchingCloseParen(s string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(s) || s[openIdx] != '(' {
		return -1
	}
	inQuote := false
	depth := 0
	for i := openIdx; i < len(s); i++ {
		ch := s[i]
		if ch == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func normalizePolymorphicFHIRPath(expr string) string {
	s := expr
	s = strings.ReplaceAll(s, ".children()", "")
	s = strings.ReplaceAll(s, ".descendants()", "")
	s = strings.TrimPrefix(s, "children().")
	s = strings.TrimPrefix(s, "descendants().")
	for strings.HasPrefix(s, "repeat(") {
		closeIdx := findMatchingCloseParen(s, len("repeat(")-1)
		if closeIdx == -1 {
			break
		}
		inner := strings.TrimPrefix(strings.TrimSpace(s[len("repeat("):closeIdx]), "$this.")
		tail := s[closeIdx+1:]
		if inner == "" || inner == "$this" {
			s = strings.TrimPrefix(tail, ".")
		} else {
			s = inner + tail
		}
	}
	for _, diagCall := range []string{".trace(", ".check(", ".defineVariable("} {
		for {
			idx := strings.Index(s, diagCall)
			if idx == -1 {
				break
			}
			openIdx := idx + len(diagCall) - 1
			closeIdx := findMatchingCloseParen(s, openIdx)
			if closeIdx == -1 {
				break
			}
			s = s[:idx] + s[closeIdx+1:]
		}
	}
	for _, unwrapCall := range []string{".select(", ".repeat("} {
		selectFrom := 0
		for selectFrom < len(s) {
			relIdx := strings.Index(s[selectFrom:], unwrapCall)
			if relIdx == -1 {
				break
			}
			idx := selectFrom + relIdx
			openIdx := idx + len(unwrapCall) - 1
			closeIdx := findMatchingCloseParen(s, openIdx)
			if closeIdx == -1 {
				break
			}
			inner := strings.TrimSpace(s[openIdx+1 : closeIdx])
			if _, _, _, hasComp := parseViewWhereComparison(inner); hasComp {
				selectFrom = closeIdx + 1
				continue
			}
			inner = strings.TrimPrefix(inner, "$this.")
			if inner == "" || inner == "$this" {
				s = s[:idx] + s[closeIdx+1:]
			} else {
				s = s[:idx] + "." + inner + s[closeIdx+1:]
			}
		}
	}
	for _, spec := range []struct {
		call   string
		isPred bool
	}{
		{".ofType(", false},
		{".as(", false},
		{".is(", true},
	} {
		for {
			idx := strings.Index(s, spec.call)
			if idx == -1 {
				break
			}
			rest := s[idx+len(spec.call):]
			closeIdx := strings.IndexByte(rest, ')')
			if closeIdx == -1 {
				break
			}
			typeArg := strings.Trim(strings.TrimSpace(rest[:closeIdx]), "'\"")
			if typeArg == "" {
				break
			}
			capType := strings.ToUpper(typeArg[:1]) + typeArg[1:]
			replacement := capType
			if spec.isPred {
				replacement = capType + ".exists()"
			}
			s = s[:idx] + replacement + rest[closeIdx+1:]
		}
	}
	searchFrom := 0
	for searchFrom < len(s) {
		relIdx := strings.Index(s[searchFrom:], ".where(")
		if relIdx == -1 {
			break
		}
		idx := searchFrom + relIdx
		prefix := strings.TrimSpace(s[:idx])
		openIdx := idx + len(".where(") - 1
		closeIdx := findMatchingCloseParen(s, openIdx)
		if closeIdx == -1 {
			break
		}
		inner := strings.TrimSpace(s[openIdx+1 : closeIdx])
		tail := strings.TrimSpace(s[closeIdx+1:])
		lhs, op, _, okComp := parseViewWhereComparison(inner)
		if prefix == "extension" || (okComp && lhs == "system" && op == "=") {
			searchFrom = closeIdx + 1
			continue
		}
		if tail == ".exists()" {
			s = prefix + ".exists(" + inner + ")"
			break
		}
		if strings.HasPrefix(tail, ".") && tail != ".empty()" {
			s = prefix + tail
			continue
		}
		searchFrom = closeIdx + 1
	}
	if strings.HasPrefix(s, "(") {
		if closeIdx := findMatchingCloseParen(s, 0); closeIdx != -1 {
			inner := s[1:closeIdx]
			tail := s[closeIdx+1:]
			if l, r, okUnion := splitTopLevelPipeUnion(inner); okUnion {
				s = fmt.Sprintf("%s.union(%s)%s", l, r, tail)
			}
		}
	}
	if l, r, okUnion := splitTopLevelPipeUnion(s); okUnion {
		s = fmt.Sprintf("%s.union(%s)", l, r)
	}
	return s
}

func splitTopLevelPipeUnion(expr string) (lhs, rhs string, ok bool) {
	s := stripOuterParens(strings.TrimSpace(expr))
	if len(splitTopLevelBooleanWord(s, "in")) > 1 || len(splitTopLevelBooleanWord(s, "contains")) > 1 {
		return "", "", false
	}
	inQuote := false
	parenDepth := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
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
		if parenDepth == 0 && ch == '|' {
			l := strings.TrimSpace(s[:i])
			r := strings.TrimSpace(s[i+1:])
			if l != "" && r != "" && !strings.HasPrefix(l, "'") && !strings.HasPrefix(r, "'") {
				return l, r, true
			}
		}
	}
	return "", "", false
}

func dorisZeroJSONColumnExpr(fhirPath string, isCore bool) (string, string) {
	p := normalizePolymorphicFHIRPath(stripOuterParens(strings.TrimSpace(fhirPath)))
	if strings.HasPrefix(p, "'") && strings.HasSuffix(p, "'") {
		return p, "constant"
	}
	if _, err := strconv.ParseFloat(p, 64); err == nil {
		return p, "constant"
	}
	if strings.EqualFold(p, "today()") {
		return "CURRENT_DATE()", "today"
	}
	if strings.EqualFold(p, "now()") {
		return "NOW()", "now"
	}
	if strings.EqualFold(p, "timeofday()") {
		return "CURTIME()", "timeOfDay"
	}
	if concatParts := splitTopLevelConcatOp(p); len(concatParts) > 1 {
		exprs := make([]string, 0, len(concatParts))
		for _, cp := range concatParts {
			ce, _ := dorisZeroJSONColumnExpr(cp, isCore)
			exprs = append(exprs, ce)
		}
		return fmt.Sprintf("CONCAT(%s)", strings.Join(exprs, ", ")), "concat"
	}
	if lhsArith, opArith, rhsArith, ok := splitTopLevelArithmeticOp(p); ok {
		lSQL, lSrc := dorisZeroJSONColumnExpr(lhsArith, isCore)
		rSQL, _ := dorisZeroJSONColumnExpr(rhsArith, isCore)
		switch opArith {
		case "div":
			return fmt.Sprintf("CAST((%s / %s) AS BIGINT)", lSQL, rSQL), lSrc + ".div"
		case "mod":
			return fmt.Sprintf("MOD(%s, %s)", lSQL, rSQL), lSrc + ".mod"
		default:
			return fmt.Sprintf("(%s %s %s)", lSQL, opArith, rSQL), lSrc + ".arith"
		}
	}
	if strings.HasSuffix(p, ".toDate()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toDate()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CAST(%s AS DATE)", colExpr), colSrc + ".toDate"
	}
	if strings.HasSuffix(p, ".toDateTime()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toDateTime()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CAST(%s AS DATETIME)", colExpr), colSrc + ".toDateTime"
	}
	if strings.HasSuffix(p, ".toTime()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toTime()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CAST(%s AS TIME)", colExpr), colSrc + ".toTime"
	}
	if strings.HasSuffix(p, ".hasValue()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".hasValue()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("(%s IS NOT NULL AND CAST(%s AS STRING) != '')", colExpr, colExpr), colSrc + ".hasValue"
	}
	if idx := strings.LastIndex(p, ".aggregate("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".aggregate(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		args := splitTopLevelCommaArgs(p[idx+len(".aggregate(") : len(p)-1])
		if len(args) >= 1 {
			aggNoSpace := strings.ReplaceAll(strings.TrimSpace(args[0]), " ", "")
			initVal := "0"
			if len(args) >= 2 {
				initVal = strings.TrimSpace(args[1])
			}
			switch {
			case aggNoSpace == "$total+1" || aggNoSpace == "1+$total":
				cntExpr, cntSrc := dorisZeroJSONColumnExpr(basePath+".count()", isCore)
				if initVal != "0" && initVal != "0.0" {
					return fmt.Sprintf("(%s + %s)", cntExpr, initVal), cntSrc + ".aggregate"
				}
				return cntExpr, cntSrc + ".aggregate"
			case aggNoSpace == "$total+$this" || aggNoSpace == "$this+$total":
				sumExpr, sumSrc := dorisZeroJSONColumnExpr(basePath+".sum()", isCore)
				return fmt.Sprintf("COALESCE(%s, %s)", sumExpr, initVal), sumSrc + ".aggregate"
			case strings.Contains(aggNoSpace, "$this.length()"):
				collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
				return fmt.Sprintf("COALESCE(ARRAY_SUM(ARRAY_MAP(x -> CHAR_LENGTH(x), %s)), %s)", collExpr, initVal), strings.TrimSuffix(collSrc, "[]") + ".aggregate"
			default:
				collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
				return fmt.Sprintf("COALESCE(ARRAY_SIZE(%s), %s)", collExpr, initVal), strings.TrimSuffix(collSrc, "[]") + ".aggregate"
			}
		}
	}
	if basePath, isHigh, precArg, okBound := parseBoundaryCall(p); okBound {
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		if isTemporalFHIRPath(basePath) {
			if isHigh {
				return fmt.Sprintf("LAST_DAY(CAST(%s AS DATE))", colExpr), colSrc + ".highBoundary"
			}
			return fmt.Sprintf("CAST(%s AS DATE)", colExpr), colSrc + ".lowBoundary"
		}
		halfStep := decimalBoundaryHalfStep(precArg)
		if isHigh {
			return fmt.Sprintf("(%s + %s)", colExpr, halfStep), colSrc + ".highBoundary"
		}
		return fmt.Sprintf("(%s - %s)", colExpr, halfStep), colSrc + ".lowBoundary"
	}
	if strings.HasPrefix(strings.ToLower(p), "iif(") && strings.HasSuffix(p, ")") {
		args := splitTopLevelCommaArgs(p[len("iif(") : len(p)-1])
		if len(args) >= 2 {
			condSQL := dorisZeroJSONWhereExpr(args[0], isCore)
			if condSQL == "" {
				condSQL, _ = dorisZeroJSONColumnExpr(args[0], isCore)
			}
			trueExpr, _ := dorisZeroJSONColumnExpr(args[1], isCore)
			falseExpr := "NULL"
			if len(args) >= 3 {
				falseExpr, _ = dorisZeroJSONColumnExpr(args[2], isCore)
			}
			return fmt.Sprintf("CASE WHEN %s THEN %s ELSE %s END", condSQL, trueExpr, falseExpr), "iif"
		}
	}
	if strings.HasPrefix(strings.ToLower(p), "coalesce(") && strings.HasSuffix(p, ")") {
		args := splitTopLevelCommaArgs(p[len("coalesce(") : len(p)-1])
		if len(args) > 0 {
			exprs := make([]string, 0, len(args))
			for _, a := range args {
				ce, _ := dorisZeroJSONColumnExpr(a, isCore)
				exprs = append(exprs, ce)
			}
			return fmt.Sprintf("COALESCE(%s)", strings.Join(exprs, ", ")), "coalesce"
		}
	}
	if idx := strings.LastIndex(p, ".replaceMatches("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		args := splitTopLevelCommaArgs(p[idx+len(".replaceMatches(") : len(p)-1])
		if len(args) == 2 {
			pat := strings.ReplaceAll(strings.Trim(strings.TrimSpace(args[0]), "'\""), "'", "''")
			sub := strings.ReplaceAll(strings.Trim(strings.TrimSpace(args[1]), "'\""), "'", "''")
			colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
			return fmt.Sprintf("REGEXP_REPLACE(%s, '%s', '%s')", colExpr, pat, sub), colSrc + ".replaceMatches"
		}
	}
	if idx := strings.LastIndex(p, ".replace("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		args := splitTopLevelCommaArgs(p[idx+len(".replace(") : len(p)-1])
		if len(args) == 2 {
			pat := strings.ReplaceAll(strings.Trim(strings.TrimSpace(args[0]), "'\""), "'", "''")
			sub := strings.ReplaceAll(strings.Trim(strings.TrimSpace(args[1]), "'\""), "'", "''")
			colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
			return fmt.Sprintf("REPLACE(%s, '%s', '%s')", colExpr, pat, sub), colSrc + ".replace"
		}
	}
	if strings.HasSuffix(p, ".abs()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".abs()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("ABS(%s)", colExpr), colSrc + ".abs"
	}
	if strings.HasSuffix(p, ".ceiling()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".ceiling()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CEIL(%s)", colExpr), colSrc + ".ceiling"
	}
	if strings.HasSuffix(p, ".floor()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".floor()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("FLOOR(%s)", colExpr), colSrc + ".floor"
	}
	if idx := strings.LastIndex(p, ".round("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		argStr := strings.TrimSpace(p[idx+len(".round(") : len(p)-1])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		if argStr == "" {
			return fmt.Sprintf("ROUND(%s)", colExpr), colSrc + ".round"
		}
		if prec, err := strconv.Atoi(argStr); err == nil {
			return fmt.Sprintf("ROUND(%s, %d)", colExpr, prec), colSrc + ".round"
		}
	}
	if strings.HasSuffix(p, ".sqrt()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".sqrt()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("SQRT(%s)", colExpr), colSrc + ".sqrt"
	}
	if strings.HasSuffix(p, ".exp()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".exp()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("EXP(%s)", colExpr), colSrc + ".exp"
	}
	if strings.HasSuffix(p, ".ln()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".ln()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("LN(%s)", colExpr), colSrc + ".ln"
	}
	if idx := strings.LastIndex(p, ".log("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		argStr := strings.TrimSpace(p[idx+len(".log(") : len(p)-1])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		baseExpr, _ := dorisZeroJSONColumnExpr(argStr, isCore)
		return fmt.Sprintf("LOG(%s, %s)", baseExpr, colExpr), colSrc + ".log"
	}
	if idx := strings.LastIndex(p, ".power("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		argStr := strings.TrimSpace(p[idx+len(".power(") : len(p)-1])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		expExpr, _ := dorisZeroJSONColumnExpr(argStr, isCore)
		return fmt.Sprintf("POW(%s, %s)", colExpr, expExpr), colSrc + ".power"
	}
	if strings.HasSuffix(p, ".truncate()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".truncate()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("TRUNCATE(%s, 0)", colExpr), colSrc + ".truncate"
	}
	if strings.HasSuffix(p, ".isDistinct()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".isDistinct()")])
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("(ARRAY_SIZE(ARRAY_DISTINCT(%s)) = ARRAY_SIZE(%s))", collExpr, collExpr), strings.TrimSuffix(collSrc, "[]") + ".isDistinct"
	}
	if idx := strings.LastIndex(p, ".subsetOf("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".subsetOf(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		otherPath := strings.TrimSpace(p[idx+len(".subsetOf(") : len(p)-1])
		lhsColl, lhsSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		rhsColl, _ := dorisZeroJSONCollectionColumnExpr(otherPath)
		return fmt.Sprintf("(ARRAY_SIZE(ARRAY_EXCEPT(%s, %s)) = 0)", lhsColl, rhsColl), strings.TrimSuffix(lhsSrc, "[]") + ".subsetOf"
	}
	if idx := strings.LastIndex(p, ".supersetOf("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".supersetOf(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		otherPath := strings.TrimSpace(p[idx+len(".supersetOf(") : len(p)-1])
		lhsColl, lhsSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		rhsColl, _ := dorisZeroJSONCollectionColumnExpr(otherPath)
		return fmt.Sprintf("(ARRAY_SIZE(ARRAY_EXCEPT(%s, %s)) = 0)", rhsColl, lhsColl), strings.TrimSuffix(lhsSrc, "[]") + ".supersetOf"
	}
	if idx := strings.LastIndex(p, ".split("); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(".split("):len(p)-1], ").") {
		basePath := strings.TrimSpace(p[:idx])
		rawSep := strings.Trim(strings.TrimSpace(p[idx+len(".split("):len(p)-1]), "'\"")
		escapedSep := strings.ReplaceAll(rawSep, "'", "''")
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("SPLIT_BY_STRING(%s, '%s')", colExpr, escapedSep), colSrc + ".split[]"
	}
	if strings.HasSuffix(p, ".toChars()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toChars()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("SPLIT_BY_STRING(%s, '')", colExpr), colSrc + ".toChars[]"
	}
	if idx := strings.LastIndex(p, ".substring("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		args := splitTopLevelCommaArgs(p[idx+len(".substring(") : len(p)-1])
		if len(args) >= 1 {
			if startIdx, err := strconv.Atoi(strings.TrimSpace(args[0])); err == nil && startIdx >= 0 {
				colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
				if len(args) >= 2 {
					if length, err := strconv.Atoi(strings.TrimSpace(args[1])); err == nil && length >= 0 {
						return fmt.Sprintf("SUBSTR(%s, %d, %d)", colExpr, startIdx+1, length), colSrc + ".substring"
					}
				}
				return fmt.Sprintf("SUBSTR(%s, %d)", colExpr, startIdx+1), colSrc + ".substring"
			}
		}
	}
	if strings.HasSuffix(p, ".trim()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".trim()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("TRIM(%s)", colExpr), colSrc + ".trim"
	}
	if idx := strings.LastIndex(p, ".join("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		rawSep := strings.Trim(strings.TrimSpace(p[idx+len(".join("):len(p)-1]), "'\"")
		escapedSep := strings.ReplaceAll(rawSep, "'", "''")
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("ARRAY_JOIN(%s, '%s')", collExpr, escapedSep), strings.TrimSuffix(collSrc, "[]") + ".join"
	}
	if strings.HasSuffix(p, ".last()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".last()")])
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("element_at(%s, -1)", collExpr), strings.TrimSuffix(collSrc, "[]") + ".last"
	}
	if strings.HasSuffix(p, ".single()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".single()")])
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("element_at(%s, 1)", collExpr), strings.TrimSuffix(collSrc, "[]") + ".single"
	}
	if strings.HasSuffix(p, ".first()") && (strings.Contains(p, ".skip(") || strings.Contains(p, ".take(") || strings.Contains(p, ".tail()") || strings.Contains(p, ".sort(") || strings.Contains(p, ".sortDesc(") || strings.Contains(p, ".distinct()") || strings.Contains(p, ".split(") || strings.Contains(p, ".toChars()") || strings.Contains(p, ".union(") || strings.Contains(p, ".combine(") || strings.Contains(p, ".intersect(") || strings.Contains(p, ".exclude(")) {
		basePath := strings.TrimSpace(p[:len(p)-len(".first()")])
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("element_at(%s, 1)", collExpr), strings.TrimSuffix(collSrc, "[]") + ".first"
	}
	if basePath, subIdx, okSub := parseTrailingSubscriptIndex(p); okSub {
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("element_at(%s, %d)", collExpr, subIdx+1), strings.TrimSuffix(collSrc, "[]") + fmt.Sprintf("[%d]", subIdx)
	}
	for _, aggSpec := range []struct {
		suffix  string
		dorisFn string
		tag     string
	}{
		{".sum()", "ARRAY_SUM", ".sum"},
		{".avg()", "ARRAY_AVG", ".avg"},
		{".min()", "ARRAY_MIN", ".min"},
		{".max()", "ARRAY_MAX", ".max"},
	} {
		if strings.HasSuffix(p, aggSpec.suffix) {
			basePath := strings.TrimSpace(p[:len(p)-len(aggSpec.suffix)])
			if mods := parseSpidxCollectionModifiers(basePath); !mods.hasAnyMods() && !isSetOpFHIRPath(mods.basePath) && strings.Contains(basePath, "Quantity") {
				return fmt.Sprintf("%s(ARRAY_MAP(x -> CAST(SPLIT_PART(x, '|', 2) AS DOUBLE), ARRAY_FILTER(x -> x LIKE 'value-quantity=|%%', r.code_tokens)))", aggSpec.dorisFn), "code_tokens[value-quantity]" + aggSpec.tag
			}
			collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
			return fmt.Sprintf("%s(ARRAY_MAP(x -> CAST(x AS DOUBLE), %s))", aggSpec.dorisFn, collExpr), strings.TrimSuffix(collSrc, "[]") + aggSpec.tag
		}
	}
	if strings.HasSuffix(p, ".count()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".count()")])
		if strings.HasSuffix(basePath, ".tail()") {
			rootPath := strings.TrimSpace(basePath[:len(basePath)-len(".tail()")])
			collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(rootPath)
			return fmt.Sprintf("GREATEST(ARRAY_SIZE(%s) - 1, 0)", collExpr), strings.TrimSuffix(collSrc, "[]") + ".tail.count"
		}
		collExpr, collSrc := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("ARRAY_SIZE(%s)", collExpr), strings.TrimSuffix(collSrc, "[]") + ".count"
	}
	if strings.HasSuffix(p, ".exists()") {
		return dorisZeroJSONWhereExpr(p, isCore), "exists"
	}
	if strings.HasSuffix(p, ".empty()") {
		return dorisZeroJSONWhereExpr(p, isCore), "empty"
	}
	if strings.HasSuffix(p, ".anyTrue()") {
		return dorisZeroJSONWhereExpr(p, isCore), "anyTrue"
	}
	if strings.HasSuffix(p, ".allTrue()") {
		return dorisZeroJSONWhereExpr(p, isCore), "allTrue"
	}
	if strings.HasSuffix(p, ".anyFalse()") {
		return dorisZeroJSONWhereExpr(p, isCore), "anyFalse"
	}
	if strings.HasSuffix(p, ".allFalse()") {
		return dorisZeroJSONWhereExpr(p, isCore), "allFalse"
	}
	if _, _, ok := parseCollectionCriteriaCall(p, ".all("); ok {
		return dorisZeroJSONWhereExpr(p, isCore), "all"
	}
	if _, _, ok := parseCollectionCriteriaCall(p, ".exists("); ok {
		return dorisZeroJSONWhereExpr(p, isCore), "exists"
	}
	if (strings.Contains(p, ".startsWith(") || strings.Contains(p, ".endsWith(") || strings.Contains(p, ".contains(") || strings.Contains(p, ".matches(") || strings.Contains(p, ".matchesFull(")) && strings.HasSuffix(p, ")") {
		return dorisZeroJSONWhereExpr(p, isCore), "predicate"
	}
	if extURL, extTail, ok := parseExtensionFilterPath(p); ok {
		if extTail == ".exists()" || extTail == ".empty()" {
			return dorisZeroJSONWhereExpr(p, isCore), "extension"
		}
		extKey := normalizeExtensionKey(extURL)
		return fmt.Sprintf("element_at(array_map(x -> split_part(x, '|', 2), array_filter(x -> x LIKE 'ext:%s=|%%', r.code_tokens)), 1)", extKey), "code_tokens[ext:" + extKey + "]"
	}
	if spSys, sysVal, sysTail, okSys := parseSystemFilteredTokenPath(p); okSys {
		if sysTail == ".exists()" || sysTail == ".empty()" {
			return dorisZeroJSONWhereExpr(p, isCore), "code_tokens[" + spSys + "|" + sysVal + "]"
		}
		escapedSys := strings.ReplaceAll(sysVal, "'", "''")
		return fmt.Sprintf("element_at(array_map(x -> split_part(x, '|', 2), array_filter(x -> x LIKE '%s=%s|%%', r.code_tokens)), 1)", spSys, escapedSys), "code_tokens[" + spSys + "|" + sysVal + "]"
	}
	if strings.HasSuffix(p, ".sort()") || strings.HasSuffix(p, ".sortDesc()") || strings.HasSuffix(p, ".distinct()") || strings.HasSuffix(p, ".tail()") || (strings.Contains(p, ".skip(") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".take(") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".union(") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".combine(") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".intersect(") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".exclude(") && strings.HasSuffix(p, ")")) {
		return dorisZeroJSONCollectionColumnExpr(p)
	}
	if strings.HasSuffix(p, ".lower()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".lower()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("LOWER(%s)", colExpr), colSrc + ".lower"
	}
	if strings.HasSuffix(p, ".upper()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".upper()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("UPPER(%s)", colExpr), colSrc + ".upper"
	}
	if strings.HasSuffix(p, ".length()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".length()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CHAR_LENGTH(%s)", colExpr), colSrc + ".length"
	}
	if strings.HasSuffix(p, ".toBoolean()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toBoolean()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CAST(%s AS BOOLEAN)", colExpr), colSrc + ".toBoolean"
	}
	for _, convSpec := range []struct {
		suffix   string
		castType string
		tag      string
	}{
		{".convertsToBoolean()", "BOOLEAN", ".convertsToBoolean"},
		{".convertsToInteger()", "BIGINT", ".convertsToInteger"},
		{".convertsToDecimal()", "DOUBLE", ".convertsToDecimal"},
		{".convertsToString()", "STRING", ".convertsToString"},
		{".convertsToDate()", "DATE", ".convertsToDate"},
		{".convertsToDateTime()", "DATETIME", ".convertsToDateTime"},
		{".convertsToTime()", "TIME", ".convertsToTime"},
	} {
		if strings.HasSuffix(p, convSpec.suffix) {
			basePath := strings.TrimSpace(p[:len(p)-len(convSpec.suffix)])
			colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
			return fmt.Sprintf("(CAST(%s AS %s) IS NOT NULL)", colExpr, convSpec.castType), colSrc + convSpec.tag
		}
	}
	if strings.HasSuffix(p, ".toInteger()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toInteger()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CAST(%s AS BIGINT)", colExpr), colSrc + ".toInteger"
	}
	if strings.HasSuffix(p, ".toDecimal()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toDecimal()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CAST(%s AS DOUBLE)", colExpr), colSrc + ".toDecimal"
	}
	if idx := strings.LastIndex(p, ".convertsToQuantity("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".convertsToQuantity(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		if basePath == "valueQuantity" || basePath == "component.valueQuantity" {
			basePath += ".value"
		}
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("(CAST(%s AS DOUBLE) IS NOT NULL)", colExpr), colSrc + ".convertsToQuantity"
	}
	if idx := strings.LastIndex(p, ".toQuantity("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".toQuantity(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		if basePath == "valueQuantity" || basePath == "component.valueQuantity" {
			basePath += ".value"
		}
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CAST(%s AS DOUBLE)", colExpr), colSrc + ".toQuantity"
	}
	if strings.HasSuffix(p, ".toString()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toString()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CAST(%s AS STRING)", colExpr), colSrc + ".toString"
	}
	if idx := strings.LastIndex(p, ".encode("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".encode(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		fmtArg := strings.ToLower(strings.Trim(strings.TrimSpace(p[idx+len(".encode("):len(p)-1]), "'\""))
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		if fmtArg == "hex" {
			return fmt.Sprintf("HEX(%s)", colExpr), colSrc + ".encode(hex)"
		}
		return fmt.Sprintf("TO_BASE64(%s)", colExpr), colSrc + ".encode(base64)"
	}
	if idx := strings.LastIndex(p, ".decode("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".decode(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		fmtArg := strings.ToLower(strings.Trim(strings.TrimSpace(p[idx+len(".decode("):len(p)-1]), "'\""))
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		if fmtArg == "hex" {
			return fmt.Sprintf("UNHEX(%s)", colExpr), colSrc + ".decode(hex)"
		}
		return fmt.Sprintf("FROM_BASE64(%s)", colExpr), colSrc + ".decode(base64)"
	}
	if strings.HasSuffix(p, ".precision()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".precision()")])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("CHAR_LENGTH(CAST(%s AS STRING))", colExpr), colSrc + ".precision"
	}
	if idx := strings.LastIndex(p, ".escape("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".escape(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		fmtArg := strings.ToLower(strings.Trim(strings.TrimSpace(p[idx+len(".escape("):len(p)-1]), "'\""))
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		if fmtArg == "json" {
			return fmt.Sprintf("REPLACE(REPLACE(%s, '\\\\', '\\\\\\\\'), '\"', '\\\\\"')", colExpr), colSrc + ".escape(json)"
		}
		return fmt.Sprintf("REPLACE(REPLACE(REPLACE(%s, '&', '&amp;'), '<', '&lt;'), '>', '&gt;')", colExpr), colSrc + ".escape(html)"
	}
	if idx := strings.LastIndex(p, ".unescape("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".unescape(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		fmtArg := strings.ToLower(strings.Trim(strings.TrimSpace(p[idx+len(".unescape("):len(p)-1]), "'\""))
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		if fmtArg == "json" {
			return fmt.Sprintf("REPLACE(REPLACE(%s, '\\\\\"', '\"'), '\\\\\\\\', '\\\\')", colExpr), colSrc + ".unescape(json)"
		}
		return fmt.Sprintf("REPLACE(REPLACE(REPLACE(%s, '&gt;', '>'), '&lt;', '<'), '&amp;', '&')", colExpr), colSrc + ".unescape(html)"
	}
	if idx := strings.LastIndex(p, ".comparable("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".comparable(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("(%s IS NOT NULL)", colExpr), colSrc + ".comparable"
	}
	if idx := strings.LastIndex(p, ".indexOf("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		rawSub := strings.Trim(strings.TrimSpace(p[idx+len(".indexOf("):len(p)-1]), "'\"")
		escapedSub := strings.ReplaceAll(rawSub, "'", "''")
		colExpr, colSrc := dorisZeroJSONColumnExpr(basePath, isCore)
		return fmt.Sprintf("(LOCATE('%s', %s) - 1)", escapedSub, colExpr), colSrc + ".indexOf"
	}
	if strings.HasSuffix(p, ".not()") || (strings.HasPrefix(p, "not(") && strings.HasSuffix(p, ")")) {
		if wExpr := dorisZeroJSONWhereExpr(p, isCore); wExpr != "" {
			return wExpr, "not"
		}
	}
	if len(splitTopLevelBooleanWord(p, "implies")) == 2 || len(splitTopLevelBooleanWord(p, "xor")) == 2 || len(splitTopLevelBooleanWord(p, "or")) > 1 || len(splitTopLevelBooleanWord(p, "and")) > 1 {
		if wExpr := dorisZeroJSONWhereExpr(p, isCore); wExpr != "" {
			return wExpr, "boolean"
		}
	}
	if _, _, okIn := parseTopLevelInOp(p); okIn {
		if wExpr := dorisZeroJSONWhereExpr(p, isCore); wExpr != "" {
			return wExpr, "in"
		}
	}
	if _, _, okContains := parseTopLevelContainsOp(p); okContains {
		if wExpr := dorisZeroJSONWhereExpr(p, isCore); wExpr != "" {
			return wExpr, "contains"
		}
	}
	if _, opComp, _, okComp := parseViewWhereComparison(p); okComp {
		if wExpr := dorisZeroJSONWhereExpr(p, isCore); wExpr != "" {
			return wExpr, "comparison(" + opComp + ")"
		}
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
	case "valueQuantity.value", "component.valueQuantity.value":
		return qtyCol, strings.TrimPrefix(qtyCol, "r.")
	case "valueQuantity.code", "valueQuantity.unit", "component.valueQuantity.code", "component.valueQuantity.unit":
		return unitCodeCol, strings.TrimPrefix(unitCodeCol, "r.")
	case "valueQuantity.system", "component.valueQuantity.system":
		return unitSysCol, strings.TrimPrefix(unitSysCol, "r.")
	case "valueCodeableConcept.coding.code.first()", "valueCodeableConcept.coding.code":
		return "element_at(array_map(x -> split_part(x, '|', 2), array_filter(x -> x LIKE 'value-concept=|%', r.code_tokens)), 1)", "code_tokens[value-concept]"
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
	p := normalizePolymorphicFHIRPath(stripOuterParens(strings.TrimSpace(fhirPath)))
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
	if impParts := splitTopLevelBooleanWord(p, "implies"); len(impParts) == 2 {
		lhsSub := dorisZeroJSONWhereExpr(impParts[0], isCore)
		rhsSub := dorisZeroJSONWhereExpr(impParts[1], isCore)
		if lhsSub != "" && rhsSub != "" {
			return fmt.Sprintf("(NOT (%s) OR (%s))", lhsSub, rhsSub)
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
	if xorParts := splitTopLevelBooleanWord(p, "xor"); len(xorParts) == 2 {
		lhsSub := dorisZeroJSONWhereExpr(xorParts[0], isCore)
		rhsSub := dorisZeroJSONWhereExpr(xorParts[1], isCore)
		if lhsSub != "" && rhsSub != "" {
			return fmt.Sprintf("((%s) <> (%s))", lhsSub, rhsSub)
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
	if inLHS, inVals, okIn := parseTopLevelInOp(p); okIn {
		colExpr, _ := dorisZeroJSONColumnExpr(inLHS, isCore)
		quotedVals := make([]string, 0, len(inVals))
		for _, v := range inVals {
			quotedVals = append(quotedVals, fmt.Sprintf("'%s'", strings.ReplaceAll(v, "'", "''")))
		}
		return fmt.Sprintf("%s IN (%s)", colExpr, strings.Join(quotedVals, ", "))
	}
	if contRHS, contVals, okContains := parseTopLevelContainsOp(p); okContains {
		colExpr, _ := dorisZeroJSONColumnExpr(contRHS, isCore)
		quotedVals := make([]string, 0, len(contVals))
		for _, v := range contVals {
			quotedVals = append(quotedVals, fmt.Sprintf("'%s'", strings.ReplaceAll(v, "'", "''")))
		}
		return fmt.Sprintf("%s IN (%s)", colExpr, strings.Join(quotedVals, ", "))
	}

	codeCol := "r.primary_code"
	qtyCol := "r.primary_quantity"
	dateLowCol := "r.primary_date_low"
	if isCore {
		codeCol = "r.code"
		qtyCol = "r.value_quantity"
		dateLowCol = "r.effective_start"
	}
	if strings.HasSuffix(p, ".isDistinct()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".isDistinct()")])
		collExpr, _ := dorisZeroJSONCollectionColumnExpr(basePath)
		return fmt.Sprintf("(ARRAY_SIZE(ARRAY_DISTINCT(%s)) = ARRAY_SIZE(%s))", collExpr, collExpr)
	}
	if (strings.Contains(p, ".subsetOf(") || strings.Contains(p, ".supersetOf(")) && strings.HasSuffix(p, ")") {
		expr, _ := dorisZeroJSONColumnExpr(p, isCore)
		return expr
	}
	if strings.HasSuffix(p, ".anyTrue()") || strings.HasSuffix(p, ".allTrue()") || strings.HasSuffix(p, ".anyFalse()") || strings.HasSuffix(p, ".allFalse()") {
		fn := ""
		switch {
		case strings.HasSuffix(p, ".anyTrue()"):
			fn = "anyTrue"
		case strings.HasSuffix(p, ".allTrue()"):
			fn = "allTrue"
		case strings.HasSuffix(p, ".anyFalse()"):
			fn = "anyFalse"
		case strings.HasSuffix(p, ".allFalse()"):
			fn = "allFalse"
		}
		basePath := strings.TrimSpace(p[:len(p)-len("."+fn+"()")])
		if selBase, selCrit, ok := parseCollectionCriteriaCall(basePath, ".select("); ok {
			switch fn {
			case "anyTrue":
				return dorisZeroJSONWhereExpr(fmt.Sprintf("%s.exists(%s)", selBase, selCrit), isCore)
			case "allTrue":
				return dorisZeroJSONWhereExpr(fmt.Sprintf("%s.all(%s)", selBase, selCrit), isCore)
			case "anyFalse":
				if sub := dorisZeroJSONWhereExpr(fmt.Sprintf("%s.all(%s)", selBase, selCrit), isCore); sub != "" {
					return "NOT (" + sub + ")"
				}
			case "allFalse":
				if sub := dorisZeroJSONWhereExpr(fmt.Sprintf("%s.exists(%s)", selBase, selCrit), isCore); sub != "" {
					return "NOT (" + sub + ")"
				}
			}
		}
		if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			collExpr, _ := dorisZeroJSONCollectionColumnExpr(basePath)
			switch fn {
			case "anyTrue":
				return fmt.Sprintf("ARRAY_CONTAINS(%s, 'true')", collExpr)
			case "allTrue":
				return fmt.Sprintf("NOT ARRAY_CONTAINS(%s, 'false')", collExpr)
			case "anyFalse":
				return fmt.Sprintf("ARRAY_CONTAINS(%s, 'false')", collExpr)
			case "allFalse":
				return fmt.Sprintf("NOT ARRAY_CONTAINS(%s, 'true')", collExpr)
			}
		}
		sp := normalizeFHIRPathToSearchParam(basePath)
		switch fn {
		case "anyTrue":
			return fmt.Sprintf("array_contains(r.code_tokens, '%s=|true')", sp)
		case "allTrue":
			return fmt.Sprintf("NOT array_contains(r.code_tokens, '%s=|false')", sp)
		case "anyFalse":
			return fmt.Sprintf("array_contains(r.code_tokens, '%s=|false')", sp)
		case "allFalse":
			return fmt.Sprintf("NOT array_contains(r.code_tokens, '%s=|true')", sp)
		}
	}
	if basePath, criteria, ok := parseCollectionCriteriaCall(p, ".all("); ok {
		if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			collExpr, _ := dorisZeroJSONCollectionColumnExpr(basePath)
			if strings.HasSuffix(criteria, ".exists()") {
				return fmt.Sprintf("NOT ARRAY_EXISTS(x -> x = '', %s)", collExpr)
			}
			if cLHS, cOp, cRHS, okComp := parseViewWhereComparison(criteria); okComp {
				negOp := negateComparisonOp(cOp)
				if cLHS == "$this.length()" || strings.HasSuffix(cLHS, ".length()") {
					trimmedRHS := strings.Trim(strings.TrimSpace(cRHS), "'\"")
					return fmt.Sprintf("NOT ARRAY_EXISTS(x -> CHAR_LENGTH(x) %s %s, %s)", negOp, trimmedRHS, collExpr)
				}
				escapedVal := strings.ReplaceAll(strings.Trim(strings.TrimSpace(cRHS), "'\""), "'", "''")
				return fmt.Sprintf("NOT ARRAY_EXISTS(x -> x %s '%s', %s)", negOp, escapedVal, collExpr)
			}
		}
		if strings.HasSuffix(criteria, ".exists()") {
			subField := strings.TrimSpace(criteria[:len(criteria)-len(".exists()")])
			fullPath := basePath
			if subField != "" && subField != "$this" {
				fullPath = basePath + "." + subField
			}
			sp := normalizeFHIRPathToSearchParam(fullPath)
			isStringParam := strings.HasPrefix(fullPath, "name") || fullPath == "given" || fullPath == "family" || strings.HasPrefix(fullPath, "address") || strings.HasPrefix(fullPath, "telecom")
			if isStringParam {
				return fmt.Sprintf("NOT array_exists(x -> x LIKE '%s=%%' AND SPLIT_PART(x, '=', 2) = '', r.string_tokens)", sp)
			}
			return fmt.Sprintf("NOT array_exists(x -> x LIKE '%s=|%%' AND SPLIT_PART(x, '|', 2) = '', r.code_tokens)", sp)
		}
		if cLHS, cOp, cRHS, okComp := parseViewWhereComparison(criteria); okComp {
			negOp := negateComparisonOp(cOp)
			if cLHS == "$this.length()" || strings.HasSuffix(cLHS, ".length()") {
				subField := strings.TrimSpace(cLHS[:len(cLHS)-len(".length()")])
				fullPath := basePath
				if subField != "" && subField != "$this" {
					fullPath = basePath + "." + subField
				}
				sp := normalizeFHIRPathToSearchParam(fullPath)
				trimmedRHS := strings.Trim(strings.TrimSpace(cRHS), "'\"")
				isStringParam := strings.HasPrefix(fullPath, "name") || fullPath == "given" || fullPath == "family" || strings.HasPrefix(fullPath, "address") || strings.HasPrefix(fullPath, "telecom")
				if isStringParam {
					return fmt.Sprintf("NOT array_exists(x -> x LIKE '%s=%%' AND CHAR_LENGTH(SPLIT_PART(x, '=', 2)) %s %s, r.string_tokens)", sp, negOp, trimmedRHS)
				}
				return fmt.Sprintf("NOT array_exists(x -> x LIKE '%s=|%%' AND CHAR_LENGTH(SPLIT_PART(x, '|', 2)) %s %s, r.code_tokens)", sp, negOp, trimmedRHS)
			}
			fullPath := basePath
			if cLHS != "" && cLHS != "$this" {
				fullPath = basePath + "." + cLHS
			}
			sp := normalizeFHIRPathToSearchParam(fullPath)
			escapedVal := strings.ReplaceAll(strings.Trim(strings.TrimSpace(cRHS), "'\""), "'", "''")
			isStringParam := strings.HasPrefix(fullPath, "name") || fullPath == "given" || fullPath == "family" || strings.HasPrefix(fullPath, "address") || strings.HasPrefix(fullPath, "telecom")
			if isStringParam {
				return fmt.Sprintf("NOT array_exists(x -> x LIKE '%s=%%' AND SPLIT_PART(x, '=', 2) %s '%s', r.string_tokens)", sp, negOp, escapedVal)
			}
			return fmt.Sprintf("NOT array_exists(x -> x LIKE '%s=|%%' AND SPLIT_PART(x, '|', 2) %s '%s', r.code_tokens)", sp, negOp, escapedVal)
		}
	}
	if extURL, extTail, ok := parseExtensionFilterPath(p); ok {
		extKey := normalizeExtensionKey(extURL)
		if extTail == ".exists()" || extTail == "" {
			return fmt.Sprintf("array_exists(x -> x LIKE 'ext:%s=|%%', r.code_tokens)", extKey)
		}
		if extTail == ".empty()" {
			return fmt.Sprintf("NOT (array_exists(x -> x LIKE 'ext:%s=|%%', r.code_tokens))", extKey)
		}
	}
	if spSys, sysVal, sysTail, okSys := parseSystemFilteredTokenPath(p); okSys {
		escapedSys := strings.ReplaceAll(sysVal, "'", "''")
		if sysTail == ".exists()" || sysTail == "" {
			return fmt.Sprintf("array_exists(x -> x LIKE '%s=%s|%%', r.code_tokens)", spSys, escapedSys)
		}
		if sysTail == ".empty()" {
			return fmt.Sprintf("NOT (array_exists(x -> x LIKE '%s=%s|%%', r.code_tokens))", spSys, escapedSys)
		}
	}
	if strings.HasSuffix(p, ".toBoolean()") || strings.HasSuffix(p, ".hasValue()") || (strings.Contains(p, ".convertsTo") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".comparable(") && strings.HasSuffix(p, ")")) {
		colExpr, _ := dorisZeroJSONColumnExpr(p, isCore)
		return colExpr
	}
	if strings.HasSuffix(p, ".exists()") {
		fieldPath := strings.TrimSpace(p[:len(p)-len(".exists()")])
		if strings.HasSuffix(fieldPath, ".tail()") && !strings.Contains(fieldPath[:len(fieldPath)-len(".tail()")], "(") {
			rootPath := strings.TrimSpace(fieldPath[:len(fieldPath)-len(".tail()")])
			collExpr, _ := dorisZeroJSONCollectionColumnExpr(rootPath)
			return fmt.Sprintf("(ARRAY_SIZE(%s) > 1)", collExpr)
		}
		if mods := parseSpidxCollectionModifiers(fieldPath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			collExpr, _ := dorisZeroJSONCollectionColumnExpr(fieldPath)
			return fmt.Sprintf("(ARRAY_SIZE(%s) > 0)", collExpr)
		}
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
		if strings.HasSuffix(fieldPath, ".tail()") && !strings.Contains(fieldPath[:len(fieldPath)-len(".tail()")], "(") {
			rootPath := strings.TrimSpace(fieldPath[:len(fieldPath)-len(".tail()")])
			collExpr, _ := dorisZeroJSONCollectionColumnExpr(rootPath)
			return fmt.Sprintf("(ARRAY_SIZE(%s) <= 1)", collExpr)
		}
		if mods := parseSpidxCollectionModifiers(fieldPath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			collExpr, _ := dorisZeroJSONCollectionColumnExpr(fieldPath)
			return fmt.Sprintf("(ARRAY_SIZE(%s) = 0)", collExpr)
		}
		if sub := dorisZeroJSONWhereExpr(fieldPath+".exists()", isCore); sub != "" {
			return "NOT (" + sub + ")"
		}
	}
	if idx := strings.LastIndex(p, ".matchesFull("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".matchesFull("):len(p)-1]), "'\"")
		rawArg = strings.TrimPrefix(strings.TrimSuffix(rawArg, "$"), "^")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		colExpr, _ := dorisZeroJSONColumnExpr(fieldPath, isCore)
		return fmt.Sprintf("REGEXP(%s, '^%s$')", colExpr, escapedArg)
	}
	if idx := strings.LastIndex(p, ".matches("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".matches("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		colExpr, _ := dorisZeroJSONColumnExpr(fieldPath, isCore)
		return fmt.Sprintf("REGEXP(%s, '%s')", colExpr, escapedArg)
	}
	if idx := strings.LastIndex(p, ".startsWith("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".startsWith("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		if isComputedStringFHIRPath(fieldPath) {
			colExpr, _ := dorisZeroJSONColumnExpr(fieldPath, isCore)
			return fmt.Sprintf("%s LIKE '%s%%'", colExpr, escapedArg)
		}
		if fieldPath == "code" || fieldPath == "code.coding.code" {
			return fmt.Sprintf("%s LIKE '%s%%'", codeCol, escapedArg)
		}
		sp := normalizeFHIRPathToSearchParam(fieldPath)
		return fmt.Sprintf("array_exists(x -> x LIKE '%s=%s%%', r.string_tokens)", sp, escapedArg)
	}
	if idx := strings.LastIndex(p, ".contains("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".contains("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		if isComputedStringFHIRPath(fieldPath) {
			colExpr, _ := dorisZeroJSONColumnExpr(fieldPath, isCore)
			return fmt.Sprintf("%s LIKE '%%%s%%'", colExpr, escapedArg)
		}
		if fieldPath == "code" || fieldPath == "code.coding.code" {
			return fmt.Sprintf("%s LIKE '%%%s%%'", codeCol, escapedArg)
		}
		sp := normalizeFHIRPathToSearchParam(fieldPath)
		return fmt.Sprintf("array_exists(x -> x LIKE '%s=%%%s%%', r.string_tokens)", sp, escapedArg)
	}
	if idx := strings.LastIndex(p, ".endsWith("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".endsWith("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		if isComputedStringFHIRPath(fieldPath) {
			colExpr, _ := dorisZeroJSONColumnExpr(fieldPath, isCore)
			return fmt.Sprintf("%s LIKE '%%%s'", colExpr, escapedArg)
		}
		if fieldPath == "code" || fieldPath == "code.coding.code" {
			return fmt.Sprintf("%s LIKE '%%%s'", codeCol, escapedArg)
		}
		sp := normalizeFHIRPathToSearchParam(fieldPath)
		return fmt.Sprintf("array_exists(x -> x LIKE '%s=%%%s', r.string_tokens)", sp, escapedArg)
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
	if basePath, criteria, ok := parseCollectionCriteriaCall(p, ".exists("); ok {
		if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			collExpr, _ := dorisZeroJSONCollectionColumnExpr(basePath)
			if strings.HasSuffix(criteria, ".exists()") {
				return fmt.Sprintf("ARRAY_EXISTS(x -> x <> '', %s)", collExpr)
			}
			if cLHS, cOp, cRHS, okComp := parseViewWhereComparison(criteria); okComp {
				sqlOp := cOp
				if sqlOp == "!=" {
					sqlOp = "<>"
				}
				if cLHS == "$this.length()" || strings.HasSuffix(cLHS, ".length()") {
					trimmedRHS := strings.Trim(strings.TrimSpace(cRHS), "'\"")
					return fmt.Sprintf("ARRAY_EXISTS(x -> CHAR_LENGTH(x) %s %s, %s)", sqlOp, trimmedRHS, collExpr)
				}
				escapedVal := strings.ReplaceAll(strings.Trim(strings.TrimSpace(cRHS), "'\""), "'", "''")
				return fmt.Sprintf("ARRAY_EXISTS(x -> x %s '%s', %s)", sqlOp, escapedVal, collExpr)
			}
		}
		if cLHS, cOp, cRHS, okComp := parseViewWhereComparison(criteria); okComp {
			if cLHS == "$this.length()" || strings.HasSuffix(cLHS, ".length()") {
				subField := strings.TrimSpace(cLHS[:len(cLHS)-len(".length()")])
				fullPath := basePath
				if subField != "" && subField != "$this" {
					fullPath = basePath + "." + subField
				}
				sp := normalizeFHIRPathToSearchParam(fullPath)
				trimmedRHS := strings.Trim(strings.TrimSpace(cRHS), "'\"")
				sqlOp := cOp
				if sqlOp == "!=" {
					sqlOp = "<>"
				}
				isStringParam := strings.HasPrefix(fullPath, "name") || fullPath == "given" || fullPath == "family" || strings.HasPrefix(fullPath, "address") || strings.HasPrefix(fullPath, "telecom")
				if isStringParam {
					return fmt.Sprintf("array_exists(x -> x LIKE '%s=%%' AND CHAR_LENGTH(SPLIT_PART(x, '=', 2)) %s %s, r.string_tokens)", sp, sqlOp, trimmedRHS)
				}
				return fmt.Sprintf("array_exists(x -> x LIKE '%s=|%%' AND CHAR_LENGTH(SPLIT_PART(x, '|', 2)) %s %s, r.code_tokens)", sp, sqlOp, trimmedRHS)
			}
			fullPath := basePath
			if cLHS != "" && cLHS != "$this" {
				fullPath = basePath + "." + cLHS
			}
			sp := normalizeFHIRPathToSearchParam(fullPath)
			escapedVal := strings.ReplaceAll(strings.Trim(strings.TrimSpace(cRHS), "'\""), "'", "''")
			isStringParam := strings.HasPrefix(fullPath, "name") || fullPath == "given" || fullPath == "family" || strings.HasPrefix(fullPath, "address") || strings.HasPrefix(fullPath, "telecom")
			if isStringParam {
				if cOp == "!=" || cOp == "<>" {
					return fmt.Sprintf("array_exists(x -> x LIKE '%s=%%' AND SPLIT_PART(x, '=', 2) <> '%s', r.string_tokens)", sp, escapedVal)
				}
				return fmt.Sprintf("array_exists(x -> x = '%s=%s', r.string_tokens)", sp, escapedVal)
			}
			if cOp == "!=" || cOp == "<>" {
				return fmt.Sprintf("array_exists(x -> x LIKE '%s=|%%' AND SPLIT_PART(x, '|', 2) <> '%s', r.code_tokens)", sp, escapedVal)
			}
			return fmt.Sprintf("array_contains(r.code_tokens, '%s=|%s')", sp, escapedVal)
		}
	}
	if p == "active" || p == "deceasedBoolean" {
		sp := normalizeFHIRPathToSearchParam(p)
		return fmt.Sprintf("array_contains(r.code_tokens, '%s=|true')", sp)
	}
	lhs, op, rhs, ok := parseViewWhereComparison(p)
	if !ok {
		return ""
	}
	trimmedRHS := strings.Trim(strings.TrimSpace(rhs), "'\"")
	escapedRHS := strings.ReplaceAll(trimmedRHS, "'", "''")
	if op == "~" || op == "!~" {
		lhsExpr, _ := dorisZeroJSONColumnExpr(lhs, isCore)
		cmpOp := "="
		if op == "!~" {
			cmpOp = "<>"
		}
		if _, err := strconv.ParseFloat(trimmedRHS, 64); err == nil {
			return fmt.Sprintf("ROUND(%s, 4) %s ROUND(%s, 4)", lhsExpr, cmpOp, trimmedRHS)
		}
		return fmt.Sprintf("LOWER(TRIM(%s)) %s LOWER(TRIM('%s'))", lhsExpr, cmpOp, escapedRHS)
	}
	if extURL, _, okExt := parseExtensionFilterPath(lhs); okExt {
		extKey := normalizeExtensionKey(extURL)
		if op == "!=" || op == "<>" {
			return fmt.Sprintf("NOT array_contains(r.code_tokens, 'ext:%s=|%s')", extKey, escapedRHS)
		}
		return fmt.Sprintf("array_contains(r.code_tokens, 'ext:%s=|%s')", extKey, escapedRHS)
	}
	if spSys, sysVal, _, okSys := parseSystemFilteredTokenPath(lhs); okSys {
		escapedSys := strings.ReplaceAll(sysVal, "'", "''")
		if op == "!=" || op == "<>" {
			return fmt.Sprintf("NOT array_contains(r.code_tokens, '%s=%s|%s')", spSys, escapedSys, escapedRHS)
		}
		return fmt.Sprintf("array_contains(r.code_tokens, '%s=%s|%s')", spSys, escapedSys, escapedRHS)
	}
	_, _, _, isArithLHS := splitTopLevelArithmeticOp(lhs)

	if isArithLHS || strings.HasSuffix(lhs, ".count()") || strings.HasSuffix(lhs, ".sum()") || strings.HasSuffix(lhs, ".avg()") || strings.HasSuffix(lhs, ".min()") || strings.HasSuffix(lhs, ".max()") || strings.HasSuffix(lhs, ".length()") || strings.HasSuffix(lhs, ".precision()") || strings.HasSuffix(lhs, ".toInteger()") || strings.HasSuffix(lhs, ".toDecimal()") || strings.Contains(lhs, ".toQuantity(") || strings.Contains(lhs, ".indexOf(") || strings.HasSuffix(lhs, ".abs()") || strings.HasSuffix(lhs, ".ceiling()") || strings.HasSuffix(lhs, ".floor()") || strings.Contains(lhs, ".round(") || strings.HasSuffix(lhs, ".sqrt()") || strings.HasSuffix(lhs, ".exp()") || strings.HasSuffix(lhs, ".ln()") || strings.Contains(lhs, ".log(") || strings.Contains(lhs, ".power(") || strings.HasSuffix(lhs, ".truncate()") || strings.Contains(lhs, ".aggregate(") || strings.Contains(lhs, ".lowBoundary(") || strings.Contains(lhs, ".highBoundary(") {
		if _, err := strconv.ParseFloat(trimmedRHS, 64); err == nil {
			lhsExpr, _ := dorisZeroJSONColumnExpr(lhs, isCore)
			sqlOp := op
			if sqlOp == "!=" {
				sqlOp = "<>"
			}
			return fmt.Sprintf("%s %s %s", lhsExpr, sqlOp, trimmedRHS)
		}
	}
	_, _, hasSubLHS := parseTrailingSubscriptIndex(lhs)
	if hasSubLHS || len(splitTopLevelConcatOp(lhs)) > 1 || strings.HasPrefix(strings.ToLower(lhs), "coalesce(") || strings.HasPrefix(strings.ToLower(lhs), "iif(") || strings.Contains(lhs, ".join(") || strings.HasSuffix(lhs, ".last()") || strings.HasSuffix(lhs, ".single()") || strings.Contains(lhs, ".sort(") || strings.Contains(lhs, ".sortDesc(") || strings.Contains(lhs, ".distinct()") || strings.Contains(lhs, ".skip(") || strings.Contains(lhs, ".take(") || strings.Contains(lhs, ".tail()") || strings.Contains(lhs, ".split(") || strings.Contains(lhs, ".union(") || strings.Contains(lhs, ".combine(") || strings.Contains(lhs, ".intersect(") || strings.Contains(lhs, ".exclude(") || strings.HasSuffix(lhs, ".lower()") || strings.HasSuffix(lhs, ".upper()") || strings.HasSuffix(lhs, ".trim()") || strings.HasSuffix(lhs, ".toString()") || strings.Contains(lhs, ".replace(") || strings.Contains(lhs, ".replaceMatches(") || strings.Contains(lhs, ".substring(") || strings.Contains(lhs, ".encode(") || strings.Contains(lhs, ".decode(") || strings.Contains(lhs, ".escape(") || strings.Contains(lhs, ".unescape(") {
		lhsExpr, _ := dorisZeroJSONColumnExpr(lhs, isCore)
		sqlOp := op
		if sqlOp == "!=" {
			sqlOp = "<>"
		}
		return fmt.Sprintf("%s %s '%s'", lhsExpr, sqlOp, escapedRHS)
	}
	if strings.HasSuffix(lhs, ".toDate()") || strings.HasSuffix(lhs, ".toDateTime()") || strings.HasSuffix(lhs, ".toTime()") || strings.Contains(lhs, ".lowBoundary(") || strings.Contains(lhs, ".highBoundary(") || strings.EqualFold(trimmedRHS, "today()") || strings.EqualFold(trimmedRHS, "now()") || strings.EqualFold(trimmedRHS, "timeOfDay()") {
		lhsExpr, _ := dorisZeroJSONColumnExpr(lhs, isCore)
		rhsExpr := fmt.Sprintf("'%s'", escapedRHS)
		if strings.EqualFold(trimmedRHS, "today()") {
			rhsExpr = "CURRENT_DATE()"
		} else if strings.EqualFold(trimmedRHS, "now()") {
			rhsExpr = "NOW()"
		} else if strings.EqualFold(trimmedRHS, "timeOfDay()") {
			rhsExpr = "CURTIME()"
		}
		sqlOp := op
		if sqlOp == "!=" {
			sqlOp = "<>"
		}
		return fmt.Sprintf("%s %s %s", lhsExpr, sqlOp, rhsExpr)
	}
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

func negateComparisonOp(op string) string {
	switch strings.TrimSpace(op) {
	case "=":
		return "<>"
	case "!=", "<>":
		return "="
	case ">":
		return "<="
	case ">=":
		return "<"
	case "<":
		return ">="
	case "<=":
		return ">"
	default:
		return "<>"
	}
}

func spidxZeroJSONColumnExpr(fhirPath, dialect string) (string, string) {
	p := normalizePolymorphicFHIRPath(stripOuterParens(strings.TrimSpace(fhirPath)))
	if strings.HasPrefix(p, "'") && strings.HasSuffix(p, "'") {
		return p, "constant"
	}
	if _, err := strconv.ParseFloat(p, 64); err == nil {
		return p, "constant"
	}
	if strings.EqualFold(p, "today()") {
		return "CURRENT_DATE", "today"
	}
	if strings.EqualFold(p, "now()") {
		if dialect == "sqlite" {
			return "CURRENT_TIMESTAMP", "now"
		}
		return "NOW()", "now"
	}
	if strings.EqualFold(p, "timeofday()") {
		return "CURRENT_TIME", "timeOfDay"
	}
	if concatParts := splitTopLevelConcatOp(p); len(concatParts) > 1 {
		exprs := make([]string, 0, len(concatParts))
		for _, cp := range concatParts {
			ce, _ := spidxZeroJSONColumnExpr(cp, dialect)
			if strings.HasPrefix(ce, "'") && strings.HasSuffix(ce, "'") {
				exprs = append(exprs, ce)
			} else {
				exprs = append(exprs, fmt.Sprintf("COALESCE(%s, '')", ce))
			}
		}
		return fmt.Sprintf("(%s)", strings.Join(exprs, " || ")), "concat"
	}
	if lhsArith, opArith, rhsArith, ok := splitTopLevelArithmeticOp(p); ok {
		lSQL, lSrc := spidxZeroJSONColumnExpr(lhsArith, dialect)
		rSQL, _ := spidxZeroJSONColumnExpr(rhsArith, dialect)
		switch opArith {
		case "div":
			if dialect == "sqlite" {
				return fmt.Sprintf("CAST((%s / %s) AS INTEGER)", lSQL, rSQL), lSrc + ".div"
			}
			return fmt.Sprintf("CAST(TRUNC(%s / %s) AS BIGINT)", lSQL, rSQL), lSrc + ".div"
		case "mod":
			if dialect == "sqlite" {
				return fmt.Sprintf("(CAST(%s AS INTEGER) %% CAST(%s AS INTEGER))", lSQL, rSQL), lSrc + ".mod"
			}
			return fmt.Sprintf("MOD(CAST(%s AS NUMERIC), CAST(%s AS NUMERIC))", lSQL, rSQL), lSrc + ".mod"
		default:
			return fmt.Sprintf("(%s %s %s)", lSQL, opArith, rSQL), lSrc + ".arith"
		}
	}
	if strings.HasSuffix(p, ".toDate()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toDate()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("DATE(%s)", colExpr), colSrc + ".toDate"
		}
		return fmt.Sprintf("CAST(%s AS DATE)", colExpr), colSrc + ".toDate"
	}
	if strings.HasSuffix(p, ".toDateTime()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toDateTime()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("DATETIME(%s)", colExpr), colSrc + ".toDateTime"
		}
		return fmt.Sprintf("CAST(%s AS TIMESTAMPTZ)", colExpr), colSrc + ".toDateTime"
	}
	if strings.HasSuffix(p, ".toTime()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toTime()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("TIME(%s)", colExpr), colSrc + ".toTime"
		}
		return fmt.Sprintf("CAST(%s AS TIME)", colExpr), colSrc + ".toTime"
	}
	if strings.HasSuffix(p, ".hasValue()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".hasValue()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("(%s IS NOT NULL AND CAST(%s AS TEXT) != '')", colExpr, colExpr), colSrc + ".hasValue"
	}
	if idx := strings.LastIndex(p, ".aggregate("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".aggregate(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		args := splitTopLevelCommaArgs(p[idx+len(".aggregate(") : len(p)-1])
		if len(args) >= 1 {
			aggNoSpace := strings.ReplaceAll(strings.TrimSpace(args[0]), " ", "")
			initVal := "0"
			if len(args) >= 2 {
				initVal = strings.TrimSpace(args[1])
			}
			switch {
			case aggNoSpace == "$total+1" || aggNoSpace == "1+$total":
				cntExpr, cntSrc := spidxZeroJSONColumnExpr(basePath+".count()", dialect)
				if initVal != "0" && initVal != "0.0" {
					return fmt.Sprintf("(%s + %s)", cntExpr, initVal), cntSrc + ".aggregate"
				}
				return cntExpr, cntSrc + ".aggregate"
			case aggNoSpace == "$total+$this" || aggNoSpace == "$this+$total":
				sumExpr, sumSrc := spidxZeroJSONColumnExpr(basePath+".sum()", dialect)
				return fmt.Sprintf("COALESCE(%s, %s)", sumExpr, initVal), sumSrc + ".aggregate"
			case strings.Contains(aggNoSpace, "$this.length()"):
				var subQ, subSrc string
				if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
					subQ, subSrc = buildModifiedSpidxSubquery(mods, dialect)
					subSrc += mods.tag
				} else {
					subQ, subSrc = spidxSubqueryForPath(basePath)
				}
				if dialect == "sqlite" {
					return fmt.Sprintf("COALESCE((SELECT SUM(LENGTH(sub.sp_value)) FROM (%s) sub), %s)", subQ, initVal), subSrc + ".aggregate"
				}
				return fmt.Sprintf("COALESCE((SELECT SUM(CHAR_LENGTH(sub.sp_value)) FROM (%s) sub), %s)", subQ, initVal), subSrc + ".aggregate"
			default:
				var subQ, subSrc string
				if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
					subQ, subSrc = buildModifiedSpidxSubquery(mods, dialect)
					subSrc += mods.tag
				} else {
					subQ, subSrc = spidxSubqueryForPath(basePath)
				}
				return fmt.Sprintf("COALESCE((SELECT COUNT(*) FROM (%s) sub), %s)", subQ, initVal), subSrc + ".aggregate"
			}
		}
	}
	if basePath, isHigh, precArg, okBound := parseBoundaryCall(p); okBound {
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if isTemporalFHIRPath(basePath) {
			if isHigh {
				if dialect == "sqlite" {
					return fmt.Sprintf("DATE(%s, 'start of month', '+1 month', '-1 day')", colExpr), colSrc + ".highBoundary"
				}
				return fmt.Sprintf("CAST((DATE_TRUNC('month', CAST(%s AS DATE)) + INTERVAL '1 month - 1 day') AS DATE)", colExpr), colSrc + ".highBoundary"
			}
			if dialect == "sqlite" {
				return fmt.Sprintf("DATE(%s)", colExpr), colSrc + ".lowBoundary"
			}
			return fmt.Sprintf("CAST(%s AS DATE)", colExpr), colSrc + ".lowBoundary"
		}
		halfStep := decimalBoundaryHalfStep(precArg)
		if isHigh {
			return fmt.Sprintf("(%s + %s)", colExpr, halfStep), colSrc + ".highBoundary"
		}
		return fmt.Sprintf("(%s - %s)", colExpr, halfStep), colSrc + ".lowBoundary"
	}
	if strings.HasPrefix(strings.ToLower(p), "coalesce(") && strings.HasSuffix(p, ")") {
		args := splitTopLevelCommaArgs(p[len("coalesce(") : len(p)-1])
		if len(args) > 0 {
			exprs := make([]string, 0, len(args))
			for _, a := range args {
				ce, _ := spidxZeroJSONColumnExpr(a, dialect)
				exprs = append(exprs, ce)
			}
			return fmt.Sprintf("COALESCE(%s)", strings.Join(exprs, ", ")), "coalesce"
		}
	}
	if strings.HasPrefix(strings.ToLower(p), "iif(") && strings.HasSuffix(p, ")") {
		args := splitTopLevelCommaArgs(p[len("iif(") : len(p)-1])
		if len(args) == 3 {
			condSQL := spidxZeroJSONWhereExprForDialect(args[0], dialect)
			if condSQL == "" {
				condSQL = "1 = 0"
			}
			trueExpr, _ := spidxZeroJSONColumnExpr(args[1], dialect)
			falseExpr, _ := spidxZeroJSONColumnExpr(args[2], dialect)
			return fmt.Sprintf("CASE WHEN %s THEN %s ELSE %s END", condSQL, trueExpr, falseExpr), "iif"
		}
	}
	if idx := strings.LastIndex(p, ".replaceMatches("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		args := splitTopLevelCommaArgs(p[idx+len(".replaceMatches(") : len(p)-1])
		if len(args) == 2 {
			colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
			pat := strings.ReplaceAll(strings.Trim(strings.TrimSpace(args[0]), "'\""), "'", "''")
			sub := strings.ReplaceAll(strings.Trim(strings.TrimSpace(args[1]), "'\""), "'", "''")
			if dialect == "sqlite" {
				return fmt.Sprintf("REGEXP_REPLACE(%s, '%s', '%s')", colExpr, pat, sub), colSrc + ".replaceMatches"
			}
			return fmt.Sprintf("REGEXP_REPLACE(%s, '%s', '%s', 'g')", colExpr, pat, sub), colSrc + ".replaceMatches"
		}
	}
	if idx := strings.Index(p, ".replace("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		args := splitTopLevelCommaArgs(p[idx+len(".replace(") : len(p)-1])
		if len(args) == 2 {
			colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
			pat := strings.ReplaceAll(strings.Trim(strings.TrimSpace(args[0]), "'\""), "'", "''")
			sub := strings.ReplaceAll(strings.Trim(strings.TrimSpace(args[1]), "'\""), "'", "''")
			return fmt.Sprintf("REPLACE(%s, '%s', '%s')", colExpr, pat, sub), colSrc + ".replace"
		}
	}
	if strings.HasSuffix(p, ".abs()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".abs()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("ABS(%s)", colExpr), colSrc + ".abs"
	}
	if strings.HasSuffix(p, ".ceiling()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".ceiling()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("CEIL(%s)", colExpr), colSrc + ".ceiling"
	}
	if strings.HasSuffix(p, ".floor()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".floor()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("FLOOR(%s)", colExpr), colSrc + ".floor"
	}
	if idx := strings.LastIndex(p, ".round("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		argStr := strings.TrimSpace(p[idx+len(".round(") : len(p)-1])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if argStr == "" {
			return fmt.Sprintf("ROUND(%s)", colExpr), colSrc + ".round"
		}
		if prec, err := strconv.Atoi(argStr); err == nil {
			return fmt.Sprintf("ROUND(%s, %d)", colExpr, prec), colSrc + ".round"
		}
	}
	if strings.HasSuffix(p, ".sqrt()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".sqrt()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("SQRT(%s)", colExpr), colSrc + ".sqrt"
	}
	if strings.HasSuffix(p, ".exp()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".exp()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("EXP(%s)", colExpr), colSrc + ".exp"
	}
	if strings.HasSuffix(p, ".ln()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".ln()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("LN(%s)", colExpr), colSrc + ".ln"
	}
	if idx := strings.LastIndex(p, ".log("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		argStr := strings.TrimSpace(p[idx+len(".log(") : len(p)-1])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		baseExpr, _ := spidxZeroJSONColumnExpr(argStr, dialect)
		return fmt.Sprintf("LOG(%s, %s)", baseExpr, colExpr), colSrc + ".log"
	}
	if idx := strings.LastIndex(p, ".power("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		argStr := strings.TrimSpace(p[idx+len(".power(") : len(p)-1])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		expExpr, _ := spidxZeroJSONColumnExpr(argStr, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("POW(%s, %s)", colExpr, expExpr), colSrc + ".power"
		}
		return fmt.Sprintf("POWER(%s, %s)", colExpr, expExpr), colSrc + ".power"
	}
	if strings.HasSuffix(p, ".truncate()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".truncate()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("CAST(%s AS INTEGER)", colExpr), colSrc + ".truncate"
		}
		return fmt.Sprintf("TRUNC(%s)", colExpr), colSrc + ".truncate"
	}
	if strings.HasSuffix(p, ".isDistinct()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".isDistinct()")])
		if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			subQ, subSrc := buildModifiedSpidxSubquery(mods, dialect)
			return fmt.Sprintf("((SELECT COUNT(DISTINCT sub.sp_value) FROM (%s) sub) = (SELECT COUNT(*) FROM (%s) sub))", subQ, subQ), subSrc + mods.tag + ".isDistinct"
		}
		sp := normalizeFHIRPathToSearchParam(basePath)
		isStringParam := strings.HasPrefix(basePath, "name") || basePath == "given" || basePath == "family" || strings.HasPrefix(basePath, "address") || strings.HasPrefix(basePath, "telecom")
		if isStringParam {
			return fmt.Sprintf("((SELECT COUNT(DISTINCT s.sp_value) FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s') = (SELECT COUNT(*) FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s'))", sp, sp), "spidx_string.isDistinct"
		}
		return fmt.Sprintf("((SELECT COUNT(DISTINCT t.sp_value) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s') = (SELECT COUNT(*) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s'))", sp, sp), "spidx_token.isDistinct"
	}
	if idx := strings.LastIndex(p, ".subsetOf("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".subsetOf(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		otherPath := strings.TrimSpace(p[idx+len(".subsetOf(") : len(p)-1])
		lhsSub, lhsSrc := spidxSetOperandSubquery(basePath, dialect)
		rhsSub, _ := spidxSetOperandSubquery(otherPath, dialect)
		return fmt.Sprintf("NOT EXISTS (%s EXCEPT %s)", lhsSub, rhsSub), lhsSrc + ".subsetOf"
	}
	if idx := strings.LastIndex(p, ".supersetOf("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".supersetOf(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		otherPath := strings.TrimSpace(p[idx+len(".supersetOf(") : len(p)-1])
		lhsSub, lhsSrc := spidxSetOperandSubquery(basePath, dialect)
		rhsSub, _ := spidxSetOperandSubquery(otherPath, dialect)
		return fmt.Sprintf("NOT EXISTS (%s EXCEPT %s)", rhsSub, lhsSub), lhsSrc + ".supersetOf"
	}
	if idx := strings.LastIndex(p, ".split("); idx != -1 && strings.HasSuffix(p, ")") && !strings.Contains(p[idx+len(".split("):len(p)-1], ").") {
		basePath := strings.TrimSpace(p[:idx])
		rawSep := strings.Trim(strings.TrimSpace(p[idx+len(".split("):len(p)-1]), "'\"")
		escapedSep := strings.ReplaceAll(rawSep, "'", "''")
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("json_array(%s)", colExpr), colSrc + ".split[]"
		}
		return fmt.Sprintf("STRING_TO_ARRAY(%s, '%s')", colExpr, escapedSep), colSrc + ".split[]"
	}
	if strings.HasSuffix(p, ".toChars()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toChars()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("json_array(%s)", colExpr), colSrc + ".toChars[]"
		}
		return fmt.Sprintf("REGEXP_SPLIT_TO_ARRAY(%s, '')", colExpr), colSrc + ".toChars[]"
	}
	if idx := strings.Index(p, ".substring("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		args := splitTopLevelCommaArgs(p[idx+len(".substring(") : len(p)-1])
		if len(args) >= 1 {
			if startIdx, err := strconv.Atoi(strings.TrimSpace(args[0])); err == nil && startIdx >= 0 {
				colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
				sqlStart := startIdx + 1
				if len(args) == 2 {
					if length, err := strconv.Atoi(strings.TrimSpace(args[1])); err == nil && length >= 0 {
						return fmt.Sprintf("SUBSTR(%s, %d, %d)", colExpr, sqlStart, length), colSrc + ".substring"
					}
				}
				return fmt.Sprintf("SUBSTR(%s, %d)", colExpr, sqlStart), colSrc + ".substring"
			}
		}
	}
	if strings.HasSuffix(p, ".trim()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".trim()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("TRIM(%s)", colExpr), colSrc + ".trim"
	}
	if idx := strings.LastIndex(p, ".join("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		rawSep := strings.Trim(strings.TrimSpace(p[idx+len(".join("):len(p)-1]), "'\"")
		escapedSep := strings.ReplaceAll(rawSep, "'", "''")
		for _, setSpec := range []struct {
			call   string
			sqlOp  string
			suffix string
		}{
			{".union(", "UNION", ".union.join"},
			{".combine(", "UNION ALL", ".combine.join"},
			{".intersect(", "INTERSECT", ".intersect.join"},
			{".exclude(", "EXCEPT", ".exclude.join"},
		} {
			if sIdx := strings.LastIndex(basePath, setSpec.call); sIdx != -1 && strings.HasSuffix(basePath, ")") && !strings.Contains(basePath[sIdx+len(setSpec.call):len(basePath)-1], ").") {
				lhsPath := strings.TrimSpace(basePath[:sIdx])
				rhsPath := strings.TrimSpace(basePath[sIdx+len(setSpec.call) : len(basePath)-1])
				lhsSub, lhsSrc := spidxSubqueryForPath(lhsPath)
				rhsSub, _ := spidxSubqueryForPath(rhsPath)
				if dialect == "sqlite" {
					return fmt.Sprintf("(SELECT COALESCE(group_concat(u.sp_value, '%s'), '') FROM (%s %s %s) u)", escapedSep, lhsSub, setSpec.sqlOp, rhsSub), lhsSrc + setSpec.suffix
				}
				return fmt.Sprintf("ARRAY_TO_STRING(ARRAY(SELECT u.sp_value FROM (%s %s %s) u), '%s')", lhsSub, setSpec.sqlOp, rhsSub, escapedSep), lhsSrc + setSpec.suffix
			}
		}
		mods := parseSpidxCollectionModifiers(basePath)
		basePath = mods.basePath
		if mods.hasSubqueryMods() || isSetOpFHIRPath(basePath) {
			subQ, subSrc := buildModifiedSpidxSubquery(mods, dialect)
			if dialect == "sqlite" {
				return fmt.Sprintf("(SELECT COALESCE(group_concat(sub.sp_value, '%s'), '') FROM (%s) sub)", escapedSep, subQ), subSrc + mods.tag + ".join"
			}
			return fmt.Sprintf("(SELECT COALESCE(ARRAY_TO_STRING(ARRAY_AGG(sub.sp_value), '%s'), '') FROM (%s) sub)", escapedSep, subQ), subSrc + mods.tag + ".join"
		}
		if spSys, sysVal, _, okSys := parseSystemFilteredTokenPath(basePath); okSys {
			escapedSys := strings.ReplaceAll(sysVal, "'", "''")
			if dialect == "sqlite" {
				return fmt.Sprintf("(SELECT COALESCE(group_concat(t.sp_value, '%s'), '') FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s')", escapedSep, spSys, escapedSys), "spidx_token.join"
			}
			return fmt.Sprintf("(SELECT COALESCE(ARRAY_TO_STRING(ARRAY_AGG(t.sp_value), '%s'), '') FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s')", escapedSep, spSys, escapedSys), "spidx_token.join"
		}
		sp := normalizeFHIRPathToSearchParam(basePath)
		isStringParam := strings.HasPrefix(basePath, "name") || basePath == "given" || basePath == "family" || strings.HasPrefix(basePath, "address") || strings.HasPrefix(basePath, "telecom")
		if mods.isDistinct {
			if dialect == "sqlite" {
				if isStringParam {
					return fmt.Sprintf("(SELECT COALESCE(group_concat(sub.sp_value, '%s'), '') FROM (SELECT DISTINCT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s') sub)", escapedSep, sp), "spidx_string.distinct.join"
				}
				return fmt.Sprintf("(SELECT COALESCE(group_concat(sub.sp_value, '%s'), '') FROM (SELECT DISTINCT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s') sub)", escapedSep, sp), "spidx_token.distinct.join"
			}
			if isStringParam {
				return fmt.Sprintf("(SELECT COALESCE(ARRAY_TO_STRING(ARRAY_AGG(DISTINCT s.sp_value), '%s'), '') FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s')", escapedSep, sp), "spidx_string.distinct.join"
			}
			return fmt.Sprintf("(SELECT COALESCE(ARRAY_TO_STRING(ARRAY_AGG(DISTINCT t.sp_value), '%s'), '') FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s')", escapedSep, sp), "spidx_token.distinct.join"
		}
		if dialect == "sqlite" {
			if isStringParam {
				return fmt.Sprintf("(SELECT COALESCE(group_concat(s.sp_value, '%s'), '') FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s')", escapedSep, sp), "spidx_string.join"
			}
			return fmt.Sprintf("(SELECT COALESCE(group_concat(t.sp_value, '%s'), '') FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s')", escapedSep, sp), "spidx_token.join"
		}
		if isStringParam {
			return fmt.Sprintf("(SELECT COALESCE(ARRAY_TO_STRING(ARRAY_AGG(s.sp_value), '%s'), '') FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s')", escapedSep, sp), "spidx_string.join"
		}
		return fmt.Sprintf("(SELECT COALESCE(ARRAY_TO_STRING(ARRAY_AGG(t.sp_value), '%s'), '') FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s')", escapedSep, sp), "spidx_token.join"
	}
	if strings.HasSuffix(p, ".last()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".last()")])
		if sIdx := strings.LastIndex(basePath, ".split("); sIdx != -1 && strings.HasSuffix(basePath, ")") && !strings.Contains(basePath[sIdx+len(".split("):len(basePath)-1], ").") {
			rootPath := strings.TrimSpace(basePath[:sIdx])
			rawSep := strings.Trim(strings.TrimSpace(basePath[sIdx+len(".split("):len(basePath)-1]), "'\"")
			escapedSep := strings.ReplaceAll(rawSep, "'", "''")
			colExpr, colSrc := spidxZeroJSONColumnExpr(rootPath, dialect)
			if dialect == "sqlite" {
				return fmt.Sprintf("SUBSTR(%s, LENGTH(RTRIM(%s, REPLACE(%s, '%s', ''))) + 1)", colExpr, colExpr, colExpr, escapedSep), colSrc + ".split.last"
			}
			return fmt.Sprintf("(STRING_TO_ARRAY(%s, '%s'))[ARRAY_UPPER(STRING_TO_ARRAY(%s, '%s'), 1)]", colExpr, escapedSep, colExpr, escapedSep), colSrc + ".split.last"
		}
		if mods := parseSpidxCollectionModifiers(basePath); isSetOpFHIRPath(mods.basePath) {
			if mods.sortDir == "DESC" {
				mods.sortDir = "ASC"
			} else {
				mods.sortDir = "DESC"
			}
			mods.hasTake = true
			mods.takeLimit = 1
			subQ, subSrc := buildModifiedSpidxSubquery(mods, dialect)
			return fmt.Sprintf("(%s)", subQ), subSrc + mods.tag + ".last"
		}
		isDistinct := false
		hasSort := false
		sortName := "sort"
		orderDir := "DESC"
		for {
			if strings.HasSuffix(basePath, ".sort()") {
				hasSort = true
				sortName = "sort"
				orderDir = "DESC"
				basePath = strings.TrimSpace(basePath[:len(basePath)-len(".sort()")])
				continue
			}
			if strings.HasSuffix(basePath, ".sortDesc()") {
				hasSort = true
				sortName = "sortDesc"
				orderDir = "ASC"
				basePath = strings.TrimSpace(basePath[:len(basePath)-len(".sortDesc()")])
				continue
			}
			if strings.HasSuffix(basePath, ".distinct()") {
				isDistinct = true
				basePath = strings.TrimSpace(basePath[:len(basePath)-len(".distinct()")])
				continue
			}
			break
		}
		distinctKw := ""
		tag := ".last"
		if isDistinct && hasSort {
			distinctKw = "DISTINCT "
			tag = ".distinct." + sortName + ".last"
		} else if isDistinct {
			distinctKw = "DISTINCT "
			tag = ".distinct.last"
		} else if hasSort {
			tag = "." + sortName + ".last"
		}
		sp := normalizeFHIRPathToSearchParam(basePath)
		isStringParam := strings.HasPrefix(basePath, "name") || basePath == "given" || basePath == "family" || strings.HasPrefix(basePath, "address") || strings.HasPrefix(basePath, "telecom")
		if isStringParam {
			return fmt.Sprintf("(SELECT %ss.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' ORDER BY s.sp_value %s LIMIT 1)", distinctKw, sp, orderDir), "spidx_string" + tag
		}
		return fmt.Sprintf("(SELECT %st.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' ORDER BY t.sp_value %s LIMIT 1)", distinctKw, sp, orderDir), "spidx_token" + tag
	}
	if strings.HasSuffix(p, ".single()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".single()")])
		if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			origTag := mods.tag
			if !mods.hasTake || mods.takeLimit > 1 {
				mods.hasTake = true
				mods.takeLimit = 1
			}
			subQ, subSrc := buildModifiedSpidxSubquery(mods, dialect)
			return fmt.Sprintf("(%s)", subQ), subSrc + origTag + ".single"
		}
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return colExpr, colSrc + ".single"
	}
	if strings.HasSuffix(p, ".first()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".first()")])
		if sIdx := strings.LastIndex(basePath, ".split("); sIdx != -1 && strings.HasSuffix(basePath, ")") && !strings.Contains(basePath[sIdx+len(".split("):len(basePath)-1], ").") {
			rootPath := strings.TrimSpace(basePath[:sIdx])
			rawSep := strings.Trim(strings.TrimSpace(basePath[sIdx+len(".split("):len(basePath)-1]), "'\"")
			escapedSep := strings.ReplaceAll(rawSep, "'", "''")
			colExpr, colSrc := spidxZeroJSONColumnExpr(rootPath, dialect)
			if dialect == "sqlite" {
				return fmt.Sprintf("SUBSTR(%s, 1, CASE WHEN INSTR(%s, '%s') > 0 THEN INSTR(%s, '%s') - 1 ELSE LENGTH(%s) END)", colExpr, colExpr, escapedSep, colExpr, escapedSep, colExpr), colSrc + ".split.first"
			}
			return fmt.Sprintf("SPLIT_PART(%s, '%s', 1)", colExpr, escapedSep), colSrc + ".split.first"
		}
		if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			if isSetOpFHIRPath(mods.basePath) {
				if !mods.hasTake || mods.takeLimit > 1 {
					mods.hasTake = true
					mods.takeLimit = 1
				}
				subQ, subSrc := buildModifiedSpidxSubquery(mods, dialect)
				return fmt.Sprintf("(%s)", subQ), subSrc + mods.tag + ".first"
			}
			rootPath := mods.basePath
			distinctKw := ""
			if mods.isDistinct {
				distinctKw = "DISTINCT "
			}
			sp := normalizeFHIRPathToSearchParam(rootPath)
			isStringParam := strings.HasPrefix(rootPath, "name") || rootPath == "given" || rootPath == "family" || strings.HasPrefix(rootPath, "address") || strings.HasPrefix(rootPath, "telecom")
			orderClause := ""
			if mods.sortDir != "" {
				if isStringParam {
					orderClause = fmt.Sprintf(" ORDER BY s.sp_value %s", mods.sortDir)
				} else {
					orderClause = fmt.Sprintf(" ORDER BY t.sp_value %s", mods.sortDir)
				}
			}
			offsetClause := ""
			if mods.offset > 0 {
				offsetClause = fmt.Sprintf(" OFFSET %d", mods.offset)
			}
			if isStringParam {
				return fmt.Sprintf("(SELECT %ss.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s'%s LIMIT 1%s)", distinctKw, sp, orderClause, offsetClause), "spidx_string" + mods.tag + ".first"
			}
			return fmt.Sprintf("(SELECT %st.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s'%s LIMIT 1%s)", distinctKw, sp, orderClause, offsetClause), "spidx_token" + mods.tag + ".first"
		}
	}
	if basePath, subIdx, okSub := parseTrailingSubscriptIndex(p); okSub {
		if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			origTag := mods.tag
			mods.offset += subIdx
			if mods.hasTake {
				if subIdx >= mods.takeLimit {
					mods.takeLimit = 0
				} else {
					mods.takeLimit = 1
				}
			} else {
				mods.hasTake = true
				mods.takeLimit = 1
			}
			subQ, subSrc := buildModifiedSpidxSubquery(mods, dialect)
			return fmt.Sprintf("(%s)", subQ), fmt.Sprintf("%s%s[%d]", subSrc, origTag, subIdx)
		}
		subQ, subSrc := spidxSubqueryForPath(basePath)
		if subIdx == 0 {
			return fmt.Sprintf("(%s LIMIT 1)", subQ), subSrc + "[0]"
		}
		return fmt.Sprintf("(%s LIMIT 1 OFFSET %d)", subQ, subIdx), subSrc + fmt.Sprintf("[%d]", subIdx)
	}
	for _, aggSpec := range []struct {
		suffix string
		sqlFn  string
		tag    string
	}{
		{".sum()", "SUM", ".sum"},
		{".avg()", "AVG", ".avg"},
		{".min()", "MIN", ".min"},
		{".max()", "MAX", ".max"},
	} {
		if strings.HasSuffix(p, aggSpec.suffix) {
			basePath := strings.TrimSpace(p[:len(p)-len(aggSpec.suffix)])
			if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
				if strings.Contains(mods.basePath, "Quantity") {
					distinctKw := ""
					if mods.isDistinct {
						distinctKw = "DISTINCT "
					}
					subQ := fmt.Sprintf("SELECT %sq.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity'", distinctKw)
					if mods.sortDir != "" {
						subQ += " ORDER BY q.sp_value " + mods.sortDir
					}
					if mods.hasTake {
						subQ += fmt.Sprintf(" LIMIT %d", mods.takeLimit)
					} else if mods.offset > 0 && dialect == "sqlite" {
						subQ += " LIMIT -1"
					}
					if mods.offset > 0 {
						subQ += fmt.Sprintf(" OFFSET %d", mods.offset)
					}
					return fmt.Sprintf("(SELECT %s(sub.sp_value) FROM (%s) sub)", aggSpec.sqlFn, subQ), "spidx_quantity" + mods.tag + aggSpec.tag
				}
				subQ, subSrc := buildModifiedSpidxSubquery(mods, dialect)
				castType := "DOUBLE PRECISION"
				if dialect == "sqlite" {
					castType = "REAL"
				}
				return fmt.Sprintf("(SELECT %s(CAST(sub.sp_value AS %s)) FROM (%s) sub)", aggSpec.sqlFn, castType, subQ), subSrc + mods.tag + aggSpec.tag
			}
			if strings.Contains(basePath, "Quantity") {
				return fmt.Sprintf("(SELECT %s(q.sp_value) FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity')", aggSpec.sqlFn), "spidx_quantity" + aggSpec.tag
			}
			sp := normalizeFHIRPathToSearchParam(basePath)
			castType := "DOUBLE PRECISION"
			if dialect == "sqlite" {
				castType = "REAL"
			}
			return fmt.Sprintf("(SELECT %s(CAST(t.sp_value AS %s)) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s')", aggSpec.sqlFn, castType, sp), "spidx_token" + aggSpec.tag
		}
	}
	if strings.HasSuffix(p, ".count()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".count()")])
		if strings.HasSuffix(basePath, ".tail()") && !strings.Contains(basePath, ".skip(") && !strings.Contains(basePath, ".take(") {
			rootPath := strings.TrimSpace(basePath[:len(basePath)-len(".tail()")])
			subQ, subSrc := spidxSubqueryForPath(rootPath)
			if dialect == "sqlite" {
				return fmt.Sprintf("MAX((SELECT COUNT(*) FROM (%s) sub) - 1, 0)", subQ), subSrc + ".tail.count"
			}
			return fmt.Sprintf("GREATEST((SELECT COUNT(*) FROM (%s) sub) - 1, 0)", subQ), subSrc + ".tail.count"
		}
		for _, setSpec := range []struct {
			call   string
			sqlOp  string
			suffix string
		}{
			{".union(", "UNION", ".union.count"},
			{".combine(", "UNION ALL", ".combine.count"},
			{".intersect(", "INTERSECT", ".intersect.count"},
			{".exclude(", "EXCEPT", ".exclude.count"},
		} {
			if sIdx := strings.LastIndex(basePath, setSpec.call); sIdx != -1 && strings.HasSuffix(basePath, ")") && !strings.Contains(basePath[sIdx+len(setSpec.call):len(basePath)-1], ").") {
				lhsPath := strings.TrimSpace(basePath[:sIdx])
				rhsPath := strings.TrimSpace(basePath[sIdx+len(setSpec.call) : len(basePath)-1])
				lhsSub, lhsSrc := spidxSubqueryForPath(lhsPath)
				rhsSub, _ := spidxSubqueryForPath(rhsPath)
				return fmt.Sprintf("(SELECT COUNT(*) FROM (%s %s %s) u)", lhsSub, setSpec.sqlOp, rhsSub), lhsSrc + setSpec.suffix
			}
		}
		if sIdx := strings.LastIndex(basePath, ".split("); sIdx != -1 && strings.HasSuffix(basePath, ")") && !strings.Contains(basePath[sIdx+len(".split("):len(basePath)-1], ").") {
			rootPath := strings.TrimSpace(basePath[:sIdx])
			rawSep := strings.Trim(strings.TrimSpace(basePath[sIdx+len(".split("):len(basePath)-1]), "'\"")
			escapedSep := strings.ReplaceAll(rawSep, "'", "''")
			colExpr, colSrc := spidxZeroJSONColumnExpr(rootPath, dialect)
			if dialect == "sqlite" {
				sepLen := len(rawSep)
				if sepLen == 0 {
					sepLen = 1
				}
				return fmt.Sprintf("(CASE WHEN %s IS NULL OR %s = '' THEN 0 ELSE CAST((LENGTH(%s) - LENGTH(REPLACE(%s, '%s', ''))) / %d + 1 AS INTEGER) END)", colExpr, colExpr, colExpr, colExpr, escapedSep, sepLen), colSrc + ".split.count"
			}
			return fmt.Sprintf("COALESCE(ARRAY_LENGTH(STRING_TO_ARRAY(%s, '%s'), 1), 0)", colExpr, escapedSep), colSrc + ".split.count"
		}
		mods := parseSpidxCollectionModifiers(basePath)
		if mods.hasSkip || mods.hasTake || mods.isTail || isSetOpFHIRPath(mods.basePath) {
			subQ, subSrc := buildModifiedSpidxSubquery(mods, dialect)
			return fmt.Sprintf("(SELECT COUNT(*) FROM (%s) sub)", subQ), subSrc + mods.tag + ".count"
		}
		basePath = mods.basePath
		if spSys, sysVal, _, okSys := parseSystemFilteredTokenPath(basePath); okSys {
			escapedSys := strings.ReplaceAll(sysVal, "'", "''")
			return fmt.Sprintf("(SELECT COUNT(*) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s')", spSys, escapedSys), "spidx_token.count"
		}
		sp := normalizeFHIRPathToSearchParam(basePath)
		isStringParam := strings.HasPrefix(basePath, "name") || basePath == "given" || basePath == "family" || strings.HasPrefix(basePath, "address") || strings.HasPrefix(basePath, "telecom")
		if mods.isDistinct {
			if isStringParam {
				return fmt.Sprintf("(SELECT COUNT(DISTINCT s.sp_value) FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s')", sp), "spidx_string.distinct.count"
			}
			return fmt.Sprintf("(SELECT COUNT(DISTINCT t.sp_value) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s')", sp), "spidx_token.distinct.count"
		}
		if isStringParam {
			return fmt.Sprintf("(SELECT COUNT(*) FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s')", sp), "spidx_string.count"
		}
		return fmt.Sprintf("(SELECT COUNT(*) FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s')", sp), "spidx_token.count"
	}
	if strings.HasSuffix(p, ".exists()") {
		return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx.exists"
	}
	if strings.HasSuffix(p, ".empty()") {
		return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx.empty"
	}
	if strings.HasSuffix(p, ".anyTrue()") {
		return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx.anyTrue"
	}
	if strings.HasSuffix(p, ".allTrue()") {
		return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx.allTrue"
	}
	if strings.HasSuffix(p, ".anyFalse()") {
		return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx.anyFalse"
	}
	if strings.HasSuffix(p, ".allFalse()") {
		return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx.allFalse"
	}
	if _, _, ok := parseCollectionCriteriaCall(p, ".all("); ok {
		return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx.all"
	}
	if _, _, ok := parseCollectionCriteriaCall(p, ".exists("); ok {
		return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx.exists"
	}
	if (strings.Contains(p, ".startsWith(") || strings.Contains(p, ".endsWith(") || strings.Contains(p, ".contains(") || strings.Contains(p, ".matches(") || strings.Contains(p, ".matchesFull(")) && strings.HasSuffix(p, ")") {
		return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx.predicate"
	}
	if extURL, extTail, ok := parseExtensionFilterPath(p); ok {
		if extTail == ".exists()" || extTail == ".empty()" {
			return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx_token[extension]"
		}
		escapedURL := strings.ReplaceAll(extURL, "'", "''")
		return fmt.Sprintf("(SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_system = '%s' LIMIT 1)", escapedURL), "spidx_token[extension]"
	}
	if spSys, sysVal, sysTail, okSys := parseSystemFilteredTokenPath(p); okSys {
		if sysTail == ".exists()" || sysTail == ".empty()" {
			return spidxZeroJSONWhereExprForDialect(p, dialect), "spidx_token[" + spSys + "]"
		}
		escapedSys := strings.ReplaceAll(sysVal, "'", "''")
		return fmt.Sprintf("(SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s' LIMIT 1)", spSys, escapedSys), "spidx_token[" + spSys + "]"
	}
	if strings.HasSuffix(p, ".sort()") || strings.HasSuffix(p, ".sortDesc()") || strings.HasSuffix(p, ".distinct()") || strings.HasSuffix(p, ".tail()") || (strings.Contains(p, ".skip(") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".take(") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".union(") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".combine(") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".intersect(") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".exclude(") && strings.HasSuffix(p, ")")) {
		return spidxZeroJSONCollectionColumnExpr(p, dialect)
	}
	if strings.HasSuffix(p, ".lower()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".lower()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("LOWER(%s)", colExpr), colSrc + ".lower"
	}
	if strings.HasSuffix(p, ".upper()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".upper()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("UPPER(%s)", colExpr), colSrc + ".upper"
	}
	if strings.HasSuffix(p, ".length()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".length()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("LENGTH(%s)", colExpr), colSrc + ".length"
	}
	if strings.HasSuffix(p, ".toBoolean()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toBoolean()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("(LOWER(CAST(%s AS TEXT)) IN ('true', '1'))", colExpr), colSrc + ".toBoolean"
		}
		return fmt.Sprintf("CAST(%s AS BOOLEAN)", colExpr), colSrc + ".toBoolean"
	}
	for _, convSpec := range []struct {
		suffix string
		tag    string
	}{
		{".convertsToBoolean()", ".convertsToBoolean"},
		{".convertsToInteger()", ".convertsToInteger"},
		{".convertsToDecimal()", ".convertsToDecimal"},
		{".convertsToString()", ".convertsToString"},
		{".convertsToDate()", ".convertsToDate"},
		{".convertsToDateTime()", ".convertsToDateTime"},
		{".convertsToTime()", ".convertsToTime"},
	} {
		if strings.HasSuffix(p, convSpec.suffix) {
			basePath := strings.TrimSpace(p[:len(p)-len(convSpec.suffix)])
			colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
			return fmt.Sprintf("(%s IS NOT NULL)", colExpr), colSrc + convSpec.tag
		}
	}
	if strings.HasSuffix(p, ".toInteger()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toInteger()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("CAST(%s AS INTEGER)", colExpr), colSrc + ".toInteger"
		}
		return fmt.Sprintf("CAST(%s AS BIGINT)", colExpr), colSrc + ".toInteger"
	}
	if strings.HasSuffix(p, ".toDecimal()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toDecimal()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("CAST(%s AS REAL)", colExpr), colSrc + ".toDecimal"
		}
		return fmt.Sprintf("CAST(%s AS DOUBLE PRECISION)", colExpr), colSrc + ".toDecimal"
	}
	if idx := strings.LastIndex(p, ".convertsToQuantity("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".convertsToQuantity(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		if basePath == "valueQuantity" || basePath == "component.valueQuantity" {
			basePath += ".value"
		}
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("(%s IS NOT NULL)", colExpr), colSrc + ".convertsToQuantity"
	}
	if idx := strings.LastIndex(p, ".toQuantity("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".toQuantity(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		if basePath == "valueQuantity" || basePath == "component.valueQuantity" {
			basePath += ".value"
		}
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("CAST(%s AS REAL)", colExpr), colSrc + ".toQuantity"
		}
		return fmt.Sprintf("CAST(%s AS DOUBLE PRECISION)", colExpr), colSrc + ".toQuantity"
	}
	if strings.HasSuffix(p, ".toString()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".toString()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("CAST(%s AS TEXT)", colExpr), colSrc + ".toString"
	}
	if idx := strings.LastIndex(p, ".encode("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".encode(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		fmtArg := strings.ToLower(strings.Trim(strings.TrimSpace(p[idx+len(".encode("):len(p)-1]), "'\""))
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if fmtArg == "hex" {
			if dialect == "sqlite" {
				return fmt.Sprintf("HEX(%s)", colExpr), colSrc + ".encode(hex)"
			}
			return fmt.Sprintf("ENCODE(CAST(%s AS BYTEA), 'hex')", colExpr), colSrc + ".encode(hex)"
		}
		if dialect == "sqlite" {
			return fmt.Sprintf("HEX(%s)", colExpr), colSrc + ".encode(base64)"
		}
		return fmt.Sprintf("ENCODE(CAST(%s AS BYTEA), 'base64')", colExpr), colSrc + ".encode(base64)"
	}
	if idx := strings.LastIndex(p, ".decode("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".decode(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		fmtArg := strings.ToLower(strings.Trim(strings.TrimSpace(p[idx+len(".decode("):len(p)-1]), "'\""))
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if fmtArg == "hex" {
			if dialect == "sqlite" {
				return fmt.Sprintf("CAST(UNHEX(%s) AS TEXT)", colExpr), colSrc + ".decode(hex)"
			}
			return fmt.Sprintf("CONVERT_FROM(DECODE(%s, 'hex'), 'UTF8')", colExpr), colSrc + ".decode(hex)"
		}
		if dialect == "sqlite" {
			return fmt.Sprintf("CAST(UNHEX(%s) AS TEXT)", colExpr), colSrc + ".decode(base64)"
		}
		return fmt.Sprintf("CONVERT_FROM(DECODE(%s, 'base64'), 'UTF8')", colExpr), colSrc + ".decode(base64)"
	}
	if strings.HasSuffix(p, ".precision()") {
		basePath := strings.TrimSpace(p[:len(p)-len(".precision()")])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("LENGTH(CAST(%s AS TEXT))", colExpr), colSrc + ".precision"
		}
		return fmt.Sprintf("CHAR_LENGTH(CAST(%s AS TEXT))", colExpr), colSrc + ".precision"
	}
	if idx := strings.LastIndex(p, ".escape("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".escape(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		fmtArg := strings.ToLower(strings.Trim(strings.TrimSpace(p[idx+len(".escape("):len(p)-1]), "'\""))
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if fmtArg == "json" {
			return fmt.Sprintf("REPLACE(REPLACE(%s, '\\\\', '\\\\\\\\'), '\"', '\\\\\"')", colExpr), colSrc + ".escape(json)"
		}
		return fmt.Sprintf("REPLACE(REPLACE(REPLACE(%s, '&', '&amp;'), '<', '&lt;'), '>', '&gt;')", colExpr), colSrc + ".escape(html)"
	}
	if idx := strings.LastIndex(p, ".unescape("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".unescape(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		fmtArg := strings.ToLower(strings.Trim(strings.TrimSpace(p[idx+len(".unescape("):len(p)-1]), "'\""))
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if fmtArg == "json" {
			return fmt.Sprintf("REPLACE(REPLACE(%s, '\\\\\"', '\"'), '\\\\\\\\', '\\\\')", colExpr), colSrc + ".unescape(json)"
		}
		return fmt.Sprintf("REPLACE(REPLACE(REPLACE(%s, '&gt;', '>'), '&lt;', '<'), '&amp;', '&')", colExpr), colSrc + ".unescape(html)"
	}
	if idx := strings.LastIndex(p, ".comparable("); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(".comparable(")-1) == len(p)-1 {
		basePath := strings.TrimSpace(p[:idx])
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		return fmt.Sprintf("(%s IS NOT NULL)", colExpr), colSrc + ".comparable"
	}
	if idx := strings.LastIndex(p, ".indexOf("); idx != -1 && strings.HasSuffix(p, ")") {
		basePath := strings.TrimSpace(p[:idx])
		rawSub := strings.Trim(strings.TrimSpace(p[idx+len(".indexOf("):len(p)-1]), "'\"")
		escapedSub := strings.ReplaceAll(rawSub, "'", "''")
		colExpr, colSrc := spidxZeroJSONColumnExpr(basePath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("(INSTR(%s, '%s') - 1)", colExpr, escapedSub), colSrc + ".indexOf"
		}
		return fmt.Sprintf("(STRPOS(%s, '%s') - 1)", colExpr, escapedSub), colSrc + ".indexOf"
	}
	if strings.HasSuffix(p, ".not()") || (strings.HasPrefix(p, "not(") && strings.HasSuffix(p, ")")) {
		if wExpr := spidxZeroJSONWhereExprForDialect(p, dialect); wExpr != "" {
			return wExpr, "spidx.not"
		}
	}
	if len(splitTopLevelBooleanWord(p, "implies")) == 2 || len(splitTopLevelBooleanWord(p, "xor")) == 2 || len(splitTopLevelBooleanWord(p, "or")) > 1 || len(splitTopLevelBooleanWord(p, "and")) > 1 {
		if wExpr := spidxZeroJSONWhereExprForDialect(p, dialect); wExpr != "" {
			return wExpr, "spidx.boolean"
		}
	}
	if _, _, okIn := parseTopLevelInOp(p); okIn {
		if wExpr := spidxZeroJSONWhereExprForDialect(p, dialect); wExpr != "" {
			return wExpr, "spidx.in"
		}
	}
	if _, _, okContains := parseTopLevelContainsOp(p); okContains {
		if wExpr := spidxZeroJSONWhereExprForDialect(p, dialect); wExpr != "" {
			return wExpr, "spidx.contains"
		}
	}
	if _, opComp, _, okComp := parseViewWhereComparison(p); okComp {
		if wExpr := spidxZeroJSONWhereExprForDialect(p, dialect); wExpr != "" {
			return wExpr, "spidx.comparison(" + opComp + ")"
		}
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
	case "valueQuantity.value", "component.valueQuantity.value":
		return "(SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1)", "spidx_quantity"
	case "valueQuantity.code", "valueQuantity.unit", "component.valueQuantity.code", "component.valueQuantity.unit":
		return "(SELECT q.sp_code FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1)", "spidx_quantity"
	case "valueCodeableConcept.coding.code.first()", "valueCodeableConcept.coding.code":
		return "(SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'value-concept' LIMIT 1)", "spidx_token"
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

func spidxZeroJSONWhereExprForDialect(fhirPath, dialect string) string {
	p := normalizePolymorphicFHIRPath(stripOuterParens(strings.TrimSpace(fhirPath)))
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "not(") && strings.HasSuffix(p, ")") {
		inner := strings.TrimSpace(p[4 : len(p)-1])
		if sub := spidxZeroJSONWhereExprForDialect(inner, dialect); sub != "" {
			return "NOT (" + sub + ")"
		}
	}
	if strings.HasSuffix(p, ".not()") {
		inner := strings.TrimSpace(p[:len(p)-len(".not()")])
		if sub := spidxZeroJSONWhereExprForDialect(inner, dialect); sub != "" {
			return "NOT (" + sub + ")"
		}
	}
	if impParts := splitTopLevelBooleanWord(p, "implies"); len(impParts) == 2 {
		lhsSub := spidxZeroJSONWhereExprForDialect(impParts[0], dialect)
		rhsSub := spidxZeroJSONWhereExprForDialect(impParts[1], dialect)
		if lhsSub != "" && rhsSub != "" {
			return fmt.Sprintf("(NOT (%s) OR (%s))", lhsSub, rhsSub)
		}
	}
	if orParts := splitTopLevelBooleanWord(p, "or"); len(orParts) > 1 {
		var compiled []string
		for _, part := range orParts {
			if sub := spidxZeroJSONWhereExprForDialect(part, dialect); sub != "" {
				compiled = append(compiled, sub)
			}
		}
		if len(compiled) > 0 {
			return "(" + strings.Join(compiled, " OR ") + ")"
		}
	}
	if xorParts := splitTopLevelBooleanWord(p, "xor"); len(xorParts) == 2 {
		lhsSub := spidxZeroJSONWhereExprForDialect(xorParts[0], dialect)
		rhsSub := spidxZeroJSONWhereExprForDialect(xorParts[1], dialect)
		if lhsSub != "" && rhsSub != "" {
			return fmt.Sprintf("((%s) <> (%s))", lhsSub, rhsSub)
		}
	}
	if andParts := splitTopLevelBooleanWord(p, "and"); len(andParts) > 1 {
		var compiled []string
		for _, part := range andParts {
			if sub := spidxZeroJSONWhereExprForDialect(part, dialect); sub != "" {
				compiled = append(compiled, sub)
			}
		}
		if len(compiled) > 0 {
			return strings.Join(compiled, " AND ")
		}
	}
	if inLHS, inVals, okIn := parseTopLevelInOp(p); okIn {
		colExpr, _ := spidxZeroJSONColumnExpr(inLHS, dialect)
		quotedVals := make([]string, 0, len(inVals))
		for _, v := range inVals {
			quotedVals = append(quotedVals, fmt.Sprintf("'%s'", strings.ReplaceAll(v, "'", "''")))
		}
		return fmt.Sprintf("%s IN (%s)", colExpr, strings.Join(quotedVals, ", "))
	}
	if contRHS, contVals, okContains := parseTopLevelContainsOp(p); okContains {
		colExpr, _ := spidxZeroJSONColumnExpr(contRHS, dialect)
		quotedVals := make([]string, 0, len(contVals))
		for _, v := range contVals {
			quotedVals = append(quotedVals, fmt.Sprintf("'%s'", strings.ReplaceAll(v, "'", "''")))
		}
		return fmt.Sprintf("%s IN (%s)", colExpr, strings.Join(quotedVals, ", "))
	}

	if strings.HasSuffix(p, ".isDistinct()") {
		expr, _ := spidxZeroJSONColumnExpr(p, dialect)
		return expr
	}
	if (strings.Contains(p, ".subsetOf(") || strings.Contains(p, ".supersetOf(")) && strings.HasSuffix(p, ")") {
		expr, _ := spidxZeroJSONColumnExpr(p, dialect)
		return expr
	}
	if strings.HasSuffix(p, ".anyTrue()") || strings.HasSuffix(p, ".allTrue()") || strings.HasSuffix(p, ".anyFalse()") || strings.HasSuffix(p, ".allFalse()") {
		fn := ""
		switch {
		case strings.HasSuffix(p, ".anyTrue()"):
			fn = "anyTrue"
		case strings.HasSuffix(p, ".allTrue()"):
			fn = "allTrue"
		case strings.HasSuffix(p, ".anyFalse()"):
			fn = "anyFalse"
		case strings.HasSuffix(p, ".allFalse()"):
			fn = "allFalse"
		}
		basePath := strings.TrimSpace(p[:len(p)-len("."+fn+"()")])
		if selBase, selCrit, ok := parseCollectionCriteriaCall(basePath, ".select("); ok {
			switch fn {
			case "anyTrue":
				return spidxZeroJSONWhereExprForDialect(fmt.Sprintf("%s.exists(%s)", selBase, selCrit), dialect)
			case "allTrue":
				return spidxZeroJSONWhereExprForDialect(fmt.Sprintf("%s.all(%s)", selBase, selCrit), dialect)
			case "anyFalse":
				if sub := spidxZeroJSONWhereExprForDialect(fmt.Sprintf("%s.all(%s)", selBase, selCrit), dialect); sub != "" {
					return "NOT (" + sub + ")"
				}
			case "allFalse":
				if sub := spidxZeroJSONWhereExprForDialect(fmt.Sprintf("%s.exists(%s)", selBase, selCrit), dialect); sub != "" {
					return "NOT (" + sub + ")"
				}
			}
		}
		if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			subQ, _ := buildModifiedSpidxSubquery(mods, dialect)
			switch fn {
			case "anyTrue":
				return fmt.Sprintf("EXISTS (SELECT 1 FROM (%s) sub WHERE sub.sp_value = 'true')", subQ)
			case "allTrue":
				return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM (%s) sub WHERE sub.sp_value = 'false')", subQ)
			case "anyFalse":
				return fmt.Sprintf("EXISTS (SELECT 1 FROM (%s) sub WHERE sub.sp_value = 'false')", subQ)
			case "allFalse":
				return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM (%s) sub WHERE sub.sp_value = 'true')", subQ)
			}
		}
		sp := normalizeFHIRPathToSearchParam(basePath)
		switch fn {
		case "anyTrue":
			return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_value = 'true')", sp)
		case "allTrue":
			return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_value = 'false')", sp)
		case "anyFalse":
			return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_value = 'false')", sp)
		case "allFalse":
			return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_value = 'true')", sp)
		}
	}
	if basePath, criteria, ok := parseCollectionCriteriaCall(p, ".all("); ok {
		if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			subQ, _ := buildModifiedSpidxSubquery(mods, dialect)
			if strings.HasSuffix(criteria, ".exists()") {
				return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM (%s) sub WHERE sub.sp_value IS NULL OR sub.sp_value = '')", subQ)
			}
			if cLHS, cOp, cRHS, okComp := parseViewWhereComparison(criteria); okComp {
				negOp := negateComparisonOp(cOp)
				if cLHS == "$this.length()" || strings.HasSuffix(cLHS, ".length()") {
					lenFn := "CHAR_LENGTH"
					if dialect == "sqlite" {
						lenFn = "LENGTH"
					}
					trimmedRHS := strings.Trim(strings.TrimSpace(cRHS), "'\"")
					return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM (%s) sub WHERE %s(sub.sp_value) %s %s)", subQ, lenFn, negOp, trimmedRHS)
				}
				escapedVal := strings.ReplaceAll(strings.Trim(strings.TrimSpace(cRHS), "'\""), "'", "''")
				return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM (%s) sub WHERE sub.sp_value %s '%s')", subQ, negOp, escapedVal)
			}
		}
		if strings.HasSuffix(criteria, ".exists()") {
			subField := strings.TrimSpace(criteria[:len(criteria)-len(".exists()")])
			fullPath := basePath
			if subField != "" && subField != "$this" {
				fullPath = basePath + "." + subField
			}
			sp := normalizeFHIRPathToSearchParam(fullPath)
			isStringParam := strings.HasPrefix(fullPath, "name") || fullPath == "given" || fullPath == "family" || strings.HasPrefix(fullPath, "address") || strings.HasPrefix(fullPath, "telecom")
			if isStringParam {
				return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' AND (s.sp_value IS NULL OR s.sp_value = ''))", sp)
			}
			return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND (t.sp_value IS NULL OR t.sp_value = ''))", sp)
		}
		if cLHS, cOp, cRHS, okComp := parseViewWhereComparison(criteria); okComp {
			negOp := negateComparisonOp(cOp)
			if cLHS == "$this.length()" || strings.HasSuffix(cLHS, ".length()") {
				subField := strings.TrimSpace(cLHS[:len(cLHS)-len(".length()")])
				fullPath := basePath
				if subField != "" && subField != "$this" {
					fullPath = basePath + "." + subField
				}
				sp := normalizeFHIRPathToSearchParam(fullPath)
				lenFn := "CHAR_LENGTH"
				if dialect == "sqlite" {
					lenFn = "LENGTH"
				}
				trimmedRHS := strings.Trim(strings.TrimSpace(cRHS), "'\"")
				isStringParam := strings.HasPrefix(fullPath, "name") || fullPath == "given" || fullPath == "family" || strings.HasPrefix(fullPath, "address") || strings.HasPrefix(fullPath, "telecom")
				if isStringParam {
					return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' AND %s(s.sp_value) %s %s)", sp, lenFn, negOp, trimmedRHS)
				}
				return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND %s(t.sp_value) %s %s)", sp, lenFn, negOp, trimmedRHS)
			}
			fullPath := basePath
			if cLHS != "" && cLHS != "$this" {
				fullPath = basePath + "." + cLHS
			}
			sp := normalizeFHIRPathToSearchParam(fullPath)
			escapedVal := strings.ReplaceAll(strings.Trim(strings.TrimSpace(cRHS), "'\""), "'", "''")
			isStringParam := strings.HasPrefix(fullPath, "name") || fullPath == "given" || fullPath == "family" || strings.HasPrefix(fullPath, "address") || strings.HasPrefix(fullPath, "telecom")
			if isStringParam {
				return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' AND s.sp_value %s '%s')", sp, negOp, escapedVal)
			}
			return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_value %s '%s')", sp, negOp, escapedVal)
		}
	}
	if extURL, extTail, ok := parseExtensionFilterPath(p); ok {
		escapedURL := strings.ReplaceAll(extURL, "'", "''")
		if extTail == ".exists()" || extTail == "" {
			return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_system = '%s')", escapedURL)
		}
		if extTail == ".empty()" {
			return fmt.Sprintf("NOT (EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_system = '%s'))", escapedURL)
		}
	}
	if spSys, sysVal, sysTail, okSys := parseSystemFilteredTokenPath(p); okSys {
		escapedSys := strings.ReplaceAll(sysVal, "'", "''")
		if sysTail == ".exists()" || sysTail == "" {
			return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s')", spSys, escapedSys)
		}
		if sysTail == ".empty()" {
			return fmt.Sprintf("NOT (EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s'))", spSys, escapedSys)
		}
	}
	if strings.HasSuffix(p, ".toBoolean()") || strings.HasSuffix(p, ".hasValue()") || (strings.Contains(p, ".convertsTo") && strings.HasSuffix(p, ")")) || (strings.Contains(p, ".comparable(") && strings.HasSuffix(p, ")")) {
		colExpr, _ := spidxZeroJSONColumnExpr(p, dialect)
		return colExpr
	}
	if strings.HasSuffix(p, ".exists()") {
		fieldPath := strings.TrimSpace(p[:len(p)-len(".exists()")])
		if strings.HasSuffix(fieldPath, ".tail()") && !strings.Contains(fieldPath[:len(fieldPath)-len(".tail()")], "(") {
			rootPath := strings.TrimSpace(fieldPath[:len(fieldPath)-len(".tail()")])
			subQ, _ := spidxSubqueryForPath(rootPath)
			return fmt.Sprintf("EXISTS (%s LIMIT 1 OFFSET 1)", subQ)
		}
		if mods := parseSpidxCollectionModifiers(fieldPath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			subQ, _ := buildModifiedSpidxSubquery(mods, dialect)
			return fmt.Sprintf("EXISTS (%s)", subQ)
		}
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
		if sub := spidxZeroJSONWhereExprForDialect(fieldPath+".exists()", dialect); sub != "" {
			return "NOT (" + sub + ")"
		}
	}
	if idx := strings.LastIndex(p, ".matchesFull("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".matchesFull("):len(p)-1]), "'\"")
		rawArg = strings.TrimPrefix(strings.TrimSuffix(rawArg, "$"), "^")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		colExpr, _ := spidxZeroJSONColumnExpr(fieldPath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("%s REGEXP '^%s$'", colExpr, escapedArg)
		}
		return fmt.Sprintf("%s ~ '^%s$'", colExpr, escapedArg)
	}
	if idx := strings.LastIndex(p, ".matches("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".matches("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		colExpr, _ := spidxZeroJSONColumnExpr(fieldPath, dialect)
		if dialect == "sqlite" {
			return fmt.Sprintf("%s REGEXP '%s'", colExpr, escapedArg)
		}
		return fmt.Sprintf("%s ~ '%s'", colExpr, escapedArg)
	}
	if idx := strings.LastIndex(p, ".startsWith("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".startsWith("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		if isComputedStringFHIRPath(fieldPath) {
			colExpr, _ := spidxZeroJSONColumnExpr(fieldPath, dialect)
			return fmt.Sprintf("%s LIKE '%s%%'", colExpr, escapedArg)
		}
		sp := normalizeFHIRPathToSearchParam(fieldPath)
		return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' AND s.sp_value LIKE '%s%%')", sp, escapedArg)
	}
	if idx := strings.LastIndex(p, ".contains("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".contains("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		if isComputedStringFHIRPath(fieldPath) {
			colExpr, _ := spidxZeroJSONColumnExpr(fieldPath, dialect)
			return fmt.Sprintf("%s LIKE '%%%s%%'", colExpr, escapedArg)
		}
		sp := normalizeFHIRPathToSearchParam(fieldPath)
		return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' AND s.sp_value LIKE '%%%s%%')", sp, escapedArg)
	}
	if idx := strings.LastIndex(p, ".endsWith("); idx != -1 && strings.HasSuffix(p, ")") {
		fieldPath := strings.TrimSpace(p[:idx])
		rawArg := strings.Trim(strings.TrimSpace(p[idx+len(".endsWith("):len(p)-1]), "'\"")
		escapedArg := strings.ReplaceAll(rawArg, "'", "''")
		if isComputedStringFHIRPath(fieldPath) {
			colExpr, _ := spidxZeroJSONColumnExpr(fieldPath, dialect)
			return fmt.Sprintf("%s LIKE '%%%s'", colExpr, escapedArg)
		}
		sp := normalizeFHIRPathToSearchParam(fieldPath)
		return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' AND s.sp_value LIKE '%%%s')", sp, escapedArg)
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
	if basePath, criteria, ok := parseCollectionCriteriaCall(p, ".exists("); ok {
		if mods := parseSpidxCollectionModifiers(basePath); mods.hasAnyMods() || isSetOpFHIRPath(mods.basePath) {
			subQ, _ := buildModifiedSpidxSubquery(mods, dialect)
			if strings.HasSuffix(criteria, ".exists()") {
				return fmt.Sprintf("EXISTS (SELECT 1 FROM (%s) sub WHERE sub.sp_value IS NOT NULL AND sub.sp_value <> '')", subQ)
			}
			if cLHS, cOp, cRHS, okComp := parseViewWhereComparison(criteria); okComp {
				sqlOp := cOp
				if sqlOp == "!=" {
					sqlOp = "<>"
				}
				if cLHS == "$this.length()" || strings.HasSuffix(cLHS, ".length()") {
					lenFn := "CHAR_LENGTH"
					if dialect == "sqlite" {
						lenFn = "LENGTH"
					}
					trimmedRHS := strings.Trim(strings.TrimSpace(cRHS), "'\"")
					return fmt.Sprintf("EXISTS (SELECT 1 FROM (%s) sub WHERE %s(sub.sp_value) %s %s)", subQ, lenFn, sqlOp, trimmedRHS)
				}
				escapedVal := strings.ReplaceAll(strings.Trim(strings.TrimSpace(cRHS), "'\""), "'", "''")
				return fmt.Sprintf("EXISTS (SELECT 1 FROM (%s) sub WHERE sub.sp_value %s '%s')", subQ, sqlOp, escapedVal)
			}
		}
		if cLHS, cOp, cRHS, okComp := parseViewWhereComparison(criteria); okComp {
			sqlOp := cOp
			if sqlOp == "!=" {
				sqlOp = "<>"
			}
			if cLHS == "$this.length()" || strings.HasSuffix(cLHS, ".length()") {
				subField := strings.TrimSpace(cLHS[:len(cLHS)-len(".length()")])
				fullPath := basePath
				if subField != "" && subField != "$this" {
					fullPath = basePath + "." + subField
				}
				sp := normalizeFHIRPathToSearchParam(fullPath)
				lenFn := "CHAR_LENGTH"
				if dialect == "sqlite" {
					lenFn = "LENGTH"
				}
				trimmedRHS := strings.Trim(strings.TrimSpace(cRHS), "'\"")
				isStringParam := strings.HasPrefix(fullPath, "name") || fullPath == "given" || fullPath == "family" || strings.HasPrefix(fullPath, "address") || strings.HasPrefix(fullPath, "telecom")
				if isStringParam {
					return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' AND %s(s.sp_value) %s %s)", sp, lenFn, sqlOp, trimmedRHS)
				}
				return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND %s(t.sp_value) %s %s)", sp, lenFn, sqlOp, trimmedRHS)
			}
			fullPath := basePath
			if cLHS != "" && cLHS != "$this" {
				fullPath = basePath + "." + cLHS
			}
			sp := normalizeFHIRPathToSearchParam(fullPath)
			escapedVal := strings.ReplaceAll(strings.Trim(strings.TrimSpace(cRHS), "'\""), "'", "''")
			isStringParam := strings.HasPrefix(fullPath, "name") || fullPath == "given" || fullPath == "family" || strings.HasPrefix(fullPath, "address") || strings.HasPrefix(fullPath, "telecom")
			if isStringParam {
				return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = '%s' AND s.sp_value %s '%s')", sp, sqlOp, escapedVal)
			}
			return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_value %s '%s')", sp, sqlOp, escapedVal)
		}
	}
	if p == "active" || p == "deceasedBoolean" {
		sp := normalizeFHIRPathToSearchParam(p)
		return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_value = 'true')", sp)
	}
	lhs, op, rhs, ok := parseViewWhereComparison(p)
	if !ok {
		return ""
	}
	trimmedRHS := strings.Trim(strings.TrimSpace(rhs), "'\"")
	escapedRHS := strings.ReplaceAll(trimmedRHS, "'", "''")
	if op == "~" || op == "!~" {
		lhsExpr, _ := spidxZeroJSONColumnExpr(lhs, dialect)
		cmpOp := "="
		if op == "!~" {
			cmpOp = "<>"
		}
		if _, err := strconv.ParseFloat(trimmedRHS, 64); err == nil {
			return fmt.Sprintf("ROUND(%s, 4) %s ROUND(%s, 4)", lhsExpr, cmpOp, trimmedRHS)
		}
		return fmt.Sprintf("LOWER(TRIM(%s)) %s LOWER(TRIM('%s'))", lhsExpr, cmpOp, escapedRHS)
	}
	if extURL, _, okExt := parseExtensionFilterPath(lhs); okExt {
		escapedURL := strings.ReplaceAll(extURL, "'", "''")
		if op == "!=" || op == "<>" {
			return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_system = '%s' AND t.sp_value = '%s')", escapedURL, escapedRHS)
		}
		return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_system = '%s' AND t.sp_value = '%s')", escapedURL, escapedRHS)
	}
	if spSys, sysVal, _, okSys := parseSystemFilteredTokenPath(lhs); okSys {
		escapedSys := strings.ReplaceAll(sysVal, "'", "''")
		if op == "!=" || op == "<>" {
			return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s' AND t.sp_value = '%s')", spSys, escapedSys, escapedRHS)
		}
		return fmt.Sprintf("EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = '%s' AND t.sp_system = '%s' AND t.sp_value = '%s')", spSys, escapedSys, escapedRHS)
	}
	_, _, _, isArithLHS := splitTopLevelArithmeticOp(lhs)

	if isArithLHS || strings.HasSuffix(lhs, ".count()") || strings.HasSuffix(lhs, ".sum()") || strings.HasSuffix(lhs, ".avg()") || strings.HasSuffix(lhs, ".min()") || strings.HasSuffix(lhs, ".max()") || strings.HasSuffix(lhs, ".length()") || strings.HasSuffix(lhs, ".precision()") || strings.HasSuffix(lhs, ".toInteger()") || strings.HasSuffix(lhs, ".toDecimal()") || strings.Contains(lhs, ".toQuantity(") || strings.Contains(lhs, ".indexOf(") || strings.HasSuffix(lhs, ".abs()") || strings.HasSuffix(lhs, ".ceiling()") || strings.HasSuffix(lhs, ".floor()") || strings.Contains(lhs, ".round(") || strings.HasSuffix(lhs, ".sqrt()") || strings.HasSuffix(lhs, ".exp()") || strings.HasSuffix(lhs, ".ln()") || strings.Contains(lhs, ".log(") || strings.Contains(lhs, ".power(") || strings.HasSuffix(lhs, ".truncate()") || strings.Contains(lhs, ".aggregate(") || strings.Contains(lhs, ".lowBoundary(") || strings.Contains(lhs, ".highBoundary(") {
		if _, err := strconv.ParseFloat(trimmedRHS, 64); err == nil {
			lhsExpr, _ := spidxZeroJSONColumnExpr(lhs, dialect)
			sqlOp := op
			if sqlOp == "!=" {
				sqlOp = "<>"
			}
			return fmt.Sprintf("%s %s %s", lhsExpr, sqlOp, trimmedRHS)
		}
	}
	_, _, hasSubLHS := parseTrailingSubscriptIndex(lhs)
	if hasSubLHS || len(splitTopLevelConcatOp(lhs)) > 1 || strings.HasPrefix(strings.ToLower(lhs), "coalesce(") || strings.HasPrefix(strings.ToLower(lhs), "iif(") || strings.Contains(lhs, ".join(") || strings.HasSuffix(lhs, ".last()") || strings.HasSuffix(lhs, ".single()") || strings.Contains(lhs, ".sort(") || strings.Contains(lhs, ".sortDesc(") || strings.Contains(lhs, ".distinct()") || strings.Contains(lhs, ".skip(") || strings.Contains(lhs, ".take(") || strings.Contains(lhs, ".tail()") || strings.Contains(lhs, ".split(") || strings.Contains(lhs, ".union(") || strings.Contains(lhs, ".combine(") || strings.Contains(lhs, ".intersect(") || strings.Contains(lhs, ".exclude(") || strings.HasSuffix(lhs, ".lower()") || strings.HasSuffix(lhs, ".upper()") || strings.HasSuffix(lhs, ".trim()") || strings.HasSuffix(lhs, ".toString()") || strings.Contains(lhs, ".replace(") || strings.Contains(lhs, ".replaceMatches(") || strings.Contains(lhs, ".substring(") || strings.Contains(lhs, ".encode(") || strings.Contains(lhs, ".decode(") || strings.Contains(lhs, ".escape(") || strings.Contains(lhs, ".unescape(") {
		lhsExpr, _ := spidxZeroJSONColumnExpr(lhs, dialect)
		sqlOp := op
		if sqlOp == "!=" {
			sqlOp = "<>"
		}
		return fmt.Sprintf("%s %s '%s'", lhsExpr, sqlOp, escapedRHS)
	}
	if strings.HasSuffix(lhs, ".toDate()") || strings.HasSuffix(lhs, ".toDateTime()") || strings.HasSuffix(lhs, ".toTime()") || strings.Contains(lhs, ".lowBoundary(") || strings.Contains(lhs, ".highBoundary(") || strings.EqualFold(trimmedRHS, "today()") || strings.EqualFold(trimmedRHS, "now()") || strings.EqualFold(trimmedRHS, "timeOfDay()") {
		lhsExpr, _ := spidxZeroJSONColumnExpr(lhs, dialect)
		rhsExpr := fmt.Sprintf("'%s'", escapedRHS)
		if strings.EqualFold(trimmedRHS, "today()") {
			rhsExpr = "CURRENT_DATE"
		} else if strings.EqualFold(trimmedRHS, "now()") {
			if dialect == "sqlite" {
				rhsExpr = "CURRENT_TIMESTAMP"
			} else {
				rhsExpr = "NOW()"
			}
		} else if strings.EqualFold(trimmedRHS, "timeOfDay()") {
			rhsExpr = "CURRENT_TIME"
		}
		sqlOp := op
		if sqlOp == "!=" {
			sqlOp = "<>"
		}
		return fmt.Sprintf("%s %s %s", lhsExpr, sqlOp, rhsExpr)
	}
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
			if two == ">=" || two == "<=" || two == "!=" || two == "<>" || two == "!~" {
				return strings.TrimSpace(expr[:i]), two, strings.TrimSpace(expr[i+2:]), true
			}
		}
		if ch == '=' || ch == '>' || ch == '<' || ch == '~' {
			return strings.TrimSpace(expr[:i]), string(ch), strings.TrimSpace(expr[i+1:]), true
		}
	}
	return "", "", "", false
}

func parseTopLevelInOp(expr string) (lhs string, values []string, ok bool) {
	s := stripOuterParens(strings.TrimSpace(expr))
	parts := splitTopLevelBooleanWord(s, "in")
	if len(parts) != 2 {
		return "", nil, false
	}
	lhs = strings.TrimSpace(parts[0])
	rhs := stripOuterParens(strings.TrimSpace(parts[1]))
	if lhs == "" || rhs == "" {
		return "", nil, false
	}
	inQuote := false
	start := 0
	for i := 0; i < len(rhs); i++ {
		ch := rhs[i]
		if ch == '\'' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && (ch == '|' || ch == ',') {
			tok := strings.Trim(strings.TrimSpace(rhs[start:i]), "'\"")
			if tok != "" {
				values = append(values, tok)
			}
			start = i + 1
		}
	}
	if tail := strings.Trim(strings.TrimSpace(rhs[start:]), "'\""); tail != "" {
		values = append(values, tail)
	}
	if len(values) == 0 {
		return "", nil, false
	}
	return lhs, values, true
}

func parseTopLevelContainsOp(expr string) (rhs string, values []string, ok bool) {
	s := stripOuterParens(strings.TrimSpace(expr))
	parts := splitTopLevelBooleanWord(s, "contains")
	if len(parts) != 2 {
		return "", nil, false
	}
	rawLHS := strings.TrimSpace(parts[0])
	rhs = strings.TrimSpace(parts[1])
	if rawLHS == "" || rhs == "" {
		return "", nil, false
	}
	if !strings.HasPrefix(rawLHS, "(") && !strings.HasPrefix(rawLHS, "'") && !strings.Contains(rawLHS, "|") {
		return "", nil, false
	}
	lhs := stripOuterParens(rawLHS)
	inQuote := false
	start := 0
	for i := 0; i < len(lhs); i++ {
		ch := lhs[i]
		if ch == '\'' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && (ch == '|' || ch == ',') {
			tok := strings.Trim(strings.TrimSpace(lhs[start:i]), "'\"")
			if tok != "" {
				values = append(values, tok)
			}
			start = i + 1
		}
	}
	if tail := strings.Trim(strings.TrimSpace(lhs[start:]), "'\""); tail != "" {
		values = append(values, tail)
	}
	if len(values) == 0 {
		return "", nil, false
	}
	return rhs, values, true
}

func parseTrailingSubscriptIndex(expr string) (basePath string, idx int, ok bool) {
	s := strings.TrimSpace(expr)
	if !strings.HasSuffix(s, "]") {
		return "", 0, false
	}
	openIdx := strings.LastIndexByte(s, '[')
	if openIdx <= 0 {
		return "", 0, false
	}
	basePath = strings.TrimSpace(s[:openIdx])
	idxStr := strings.TrimSpace(s[openIdx+1 : len(s)-1])
	if basePath == "" || idxStr == "" {
		return "", 0, false
	}
	n, err := strconv.Atoi(idxStr)
	if err != nil || n < 0 {
		return "", 0, false
	}
	return basePath, n, true
}

func normalizeFHIRPathToSearchParam(path string) string {
	p := normalizePolymorphicFHIRPath(strings.TrimSpace(path))
	switch {
	case strings.HasPrefix(p, "clinicalStatus"):
		return "clinical-status"
	case strings.HasPrefix(p, "verificationStatus"):
		return "verification-status"
	case strings.HasPrefix(p, "valueCodeableConcept"):
		return "value-concept"
	case strings.HasPrefix(p, "valueQuantity") || strings.HasPrefix(p, "component.valueQuantity"):
		return "value-quantity"
	case strings.HasPrefix(p, "valueString"):
		return "value-string"
	case strings.HasPrefix(p, "deceased"):
		return "deceased"
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

func stripOuterParens(s string) string {
	s = strings.TrimSpace(s)
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		inQuote := false
		depth := 0
		matchesOuter := true
		for i := 0; i < len(s); i++ {
			ch := s[i]
			if ch == '\'' {
				inQuote = !inQuote
				continue
			}
			if inQuote {
				continue
			}
			if ch == '(' {
				depth++
			} else if ch == ')' {
				depth--
				if depth == 0 && i < len(s)-1 {
					matchesOuter = false
					break
				}
			}
		}
		if !matchesOuter || depth != 0 {
			break
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

func splitTopLevelConcatOp(expr string) []string {
	s := stripOuterParens(expr)
	var out []string
	inQuote := false
	parenDepth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
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
		if parenDepth == 0 && ch == '&' {
			part := strings.TrimSpace(s[start:i])
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	if len(out) == 0 {
		return nil
	}
	if tail := strings.TrimSpace(s[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}

func splitTopLevelArithmeticOp(expr string) (lhs, op, rhs string, ok bool) {
	s := stripOuterParens(expr)
	lower := strings.ToLower(s)
	for _, passOps := range [][]string{{" + ", " - "}, {" * ", " / ", " div ", " mod "}} {
		inQuote := false
		parenDepth := 0
		lastIdx := -1
		lastOp := ""
		for i := 0; i < len(s); i++ {
			ch := s[i]
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
			if parenDepth == 0 {
				for _, tok := range passOps {
					if i+len(tok) <= len(s) && lower[i:i+len(tok)] == tok {
						lastIdx = i
						lastOp = tok
					}
				}
			}
		}
		if lastIdx != -1 {
			l := strings.TrimSpace(s[:lastIdx])
			r := strings.TrimSpace(s[lastIdx+len(lastOp):])
			if l != "" && r != "" {
				return l, strings.TrimSpace(lastOp), r, true
			}
		}
	}
	return "", "", "", false
}

func parseCollectionCriteriaCall(expr, callPrefix string) (basePath, criteria string, ok bool) {
	s := strings.TrimSpace(expr)
	if !strings.HasSuffix(s, ")") {
		return "", "", false
	}
	idx := strings.LastIndex(s, callPrefix)
	if idx <= 0 {
		return "", "", false
	}
	base := strings.TrimSpace(s[:idx])
	crit := strings.TrimSpace(s[idx+len(callPrefix) : len(s)-1])
	if base == "" || crit == "" {
		return "", "", false
	}
	return base, crit, true
}

func parseSystemFilteredTokenPath(expr string) (spName, sysVal, tail string, ok bool) {
	s := strings.TrimSpace(expr)
	idx := strings.Index(s, ".where(")
	if idx <= 0 {
		return "", "", "", false
	}
	prefix := strings.TrimSpace(s[:idx])
	if prefix == "" || prefix == "extension" {
		return "", "", "", false
	}
	openIdx := idx + len(".where(") - 1
	closeIdx := findMatchingCloseParen(s, openIdx)
	if closeIdx == -1 {
		return "", "", "", false
	}
	inner := strings.TrimSpace(s[openIdx+1 : closeIdx])
	lhs, op, rhs, okComp := parseViewWhereComparison(inner)
	if !okComp || lhs != "system" || op != "=" {
		return "", "", "", false
	}
	u := strings.Trim(strings.TrimSpace(rhs), "'\"")
	if u == "" {
		return "", "", "", false
	}
	sp := normalizeFHIRPathToSearchParam(prefix)
	if prefix == "code.coding" || prefix == "coding" {
		sp = "code"
	}
	return sp, u, strings.TrimSpace(s[closeIdx+1:]), true
}

func parseExtensionFilterPath(expr string) (extURL, tail string, ok bool) {
	s := strings.TrimSpace(expr)
	if strings.HasPrefix(s, "extension(") {
		closeIdx := strings.IndexByte(s, ')')
		if closeIdx > len("extension(") {
			u := strings.Trim(strings.TrimSpace(s[len("extension("):closeIdx]), "'\"")
			if u != "" {
				return u, strings.TrimSpace(s[closeIdx+1:]), true
			}
		}
	}
	if strings.HasPrefix(s, "extension.where(") {
		closeIdx := strings.IndexByte(s, ')')
		if closeIdx > len("extension.where(") {
			inner := strings.TrimSpace(s[len("extension.where("):closeIdx])
			if lhs, op, rhs, okComp := parseViewWhereComparison(inner); okComp && lhs == "url" && op == "=" {
				u := strings.Trim(strings.TrimSpace(rhs), "'\"")
				if u != "" {
					return u, strings.TrimSpace(s[closeIdx+1:]), true
				}
			}
		}
	}
	return "", "", false
}

func normalizeExtensionKey(extURL string) string {
	u := strings.TrimSpace(extURL)
	if slash := strings.LastIndexByte(u, '/'); slash != -1 && slash+1 < len(u) {
		u = u[slash+1:]
	}
	return strings.ToLower(strings.ReplaceAll(u, "'", "''"))
}

func parseBoundaryCall(p string) (basePath string, isHigh bool, precArg string, ok bool) {
	for _, spec := range []struct {
		call   string
		isHigh bool
	}{
		{".lowBoundary(", false},
		{".highBoundary(", true},
	} {
		if idx := strings.LastIndex(p, spec.call); idx != -1 && strings.HasSuffix(p, ")") && findMatchingCloseParen(p, idx+len(spec.call)-1) == len(p)-1 {
			return strings.TrimSpace(p[:idx]), spec.isHigh, strings.TrimSpace(p[idx+len(spec.call) : len(p)-1]), true
		}
	}
	return "", false, "", false
}

func isTemporalFHIRPath(basePath string) bool {
	lower := strings.ToLower(basePath)
	return strings.Contains(lower, "date") || strings.Contains(lower, "time") || strings.Contains(lower, "effective") || strings.Contains(lower, "issued") || strings.Contains(lower, "authored")
}

func isComputedStringFHIRPath(fieldPath string) bool {
	s := strings.TrimSpace(fieldPath)
	return strings.HasSuffix(s, ")") || strings.HasSuffix(s, "]") || len(splitTopLevelConcatOp(s)) > 1
}

func decimalBoundaryHalfStep(precArg string) string {
	prec := 2
	if precArg != "" {
		if n, err := strconv.Atoi(precArg); err == nil && n >= 0 && n <= 8 {
			prec = n
		}
	}
	if prec == 0 {
		return "0.5"
	}
	return "0." + strings.Repeat("0", prec) + "5"
}
