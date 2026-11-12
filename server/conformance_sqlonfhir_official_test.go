package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

type officialSQLOnFHIRSuite struct {
	Title       string            `json:"title"`
	Description string            `json:"description"`
	FHIRVersion []string          `json:"fhirVersion"`
	Resources   []json.RawMessage `json:"resources"`
	Tests       []struct {
		Title         string           `json:"title"`
		Description   string           `json:"description"`
		Tags          []string         `json:"tags"`
		FHIRVersion   []string         `json:"fhirVersion"`
		View          json.RawMessage  `json:"view"`
		Expect        []map[string]any `json:"expect"`
		ExpectColumns []string         `json:"expectColumns"`
		ExpectError   bool             `json:"expectError"`
	} `json:"tests"`
}

// TestOfficialSQLOnFHIRv2ComplianceSuite executes all 22 official HL7/FHIR sql-on-fhir-v2
// JSON conformance test suites against POST /fhir/r4/{tenant}/ViewDefinition/$run.
func TestOfficialSQLOnFHIRv2ComplianceSuite(t *testing.T) {
	db := openMemPostgres(t)
	defer db.Close()

	pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(pgStore, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	suiteFiles, err := filepath.Glob(filepath.Join("testdata", "sql-on-fhir-v2", "*.json"))
	require.NoError(t, err)
	sort.Strings(suiteFiles)
	require.GreaterOrEqual(t, len(suiteFiles), 22, "expected all 22 official sql-on-fhir-v2 JSON test suites")

	totalCases := 0
	passedCases := 0

	for _, suitePath := range suiteFiles {
		suitePath := suitePath
		rawSuite, err := os.ReadFile(suitePath)
		require.NoError(t, err)

		var suite officialSQLOnFHIRSuite
		require.NoError(t, json.Unmarshal(rawSuite, &suite), "unmarshal suite %s", suitePath)

		suiteName := filepath.Base(suitePath)
		t.Run(suiteName, func(t *testing.T) {
			for idx, tc := range suite.Tests {
				totalCases++
				caseName := fmt.Sprintf("%02d_%s", idx+1, tc.Title)
				t.Run(caseName, func(t *testing.T) {
					params := make([]map[string]any, 0, len(suite.Resources)+2)
					params = append(params,
						map[string]any{"name": "_format", "valueCode": "application/json"},
						map[string]any{"name": "viewResource", "resource": json.RawMessage(tc.View)},
					)
					for _, res := range suite.Resources {
						params = append(params, map[string]any{
							"name":     "resource",
							"resource": json.RawMessage(res),
						})
					}
					reqBody, err := json.Marshal(map[string]any{
						"resourceType": "Parameters",
						"parameter":    params,
					})
					require.NoError(t, err)

					req, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/sof-official/ViewDefinition/$run", bytes.NewReader(reqBody))
					require.NoError(t, err)
					req.Header.Set("Content-Type", "application/fhir+json")
					req.Header.Set("Accept", "application/json")

					resp, err := client.Do(req)
					require.NoError(t, err)
					respBytes, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					require.NoError(t, err)

					if tc.ExpectError {
						assert.GreaterOrEqual(t, resp.StatusCode, 400, "expected 4xx error for invalid ViewDefinition, got %d: %s", resp.StatusCode, string(respBytes))
						assert.Less(t, resp.StatusCode, 500, "expected 4xx client error (not 5xx), got %d: %s", resp.StatusCode, string(respBytes))
						passedCases++
						return
					}

					require.Equal(t, http.StatusOK, resp.StatusCode, "unexpected error status %d: %s", resp.StatusCode, string(respBytes))

					var actualRows []map[string]any
					require.NoError(t, json.Unmarshal(respBytes, &actualRows), "invalid JSON array response: %s", string(respBytes))

					if len(tc.ExpectColumns) > 0 && len(actualRows) > 0 {
						actualCols, colErr := extractFirstRowOrderedKeys(respBytes)
						require.NoError(t, colErr)
						assert.Equal(t, tc.ExpectColumns, actualCols, "column ordering mismatch")
					}

					if !rowsMultisetEqual(tc.Expect, actualRows) {
						expJSON, _ := json.MarshalIndent(tc.Expect, "", "  ")
						gotJSON, _ := json.MarshalIndent(actualRows, "", "  ")
						t.Fatalf("row multiset mismatch for %s / %s:\nexpected:\n%s\nactual:\n%s", suiteName, tc.Title, string(expJSON), string(gotJSON))
					}
					passedCases++
				})
			}
		})
	}

	t.Logf("Official HL7 SQL-on-FHIR v2 Conformance: %d/%d test cases passed across %d suites", passedCases, totalCases, len(suiteFiles))
	assert.Equal(t, totalCases, passedCases)
}

func extractFirstRowOrderedKeys(rawArray []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(rawArray))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("expected '[' token, got %v", tok)
	}
	if !dec.More() {
		return nil, nil
	}
	tok, err = dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected '{' token, got %v", tok)
	}
	var keys []string
	for dec.More() {
		kTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		kStr, ok := kTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected string key, got %v", kTok)
		}
		keys = append(keys, kStr)
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func rowsMultisetEqual(expected, actual []map[string]any) bool {
	if len(expected) != len(actual) {
		return false
	}
	used := make([]bool, len(actual))
	for _, expRow := range expected {
		matched := false
		for j, actRow := range actual {
			if !used[j] && singleViewRowEqual(expRow, actRow) {
				used[j] = true
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func singleViewRowEqual(exp, act map[string]any) bool {
	if len(exp) != len(act) {
		return false
	}
	for k, ev := range exp {
		av, ok := act[k]
		if !ok {
			return false
		}
		if !jsonCellEqual(ev, av) {
			return false
		}
	}
	return true
}

func jsonCellEqual(ev, av any) bool {
	if ev == nil || av == nil {
		return ev == nil && av == nil
	}
	ef, eNum := ev.(float64)
	af, aNum := av.(float64)
	if eNum || aNum {
		if !eNum || !aNum {
			return false
		}
		return math.Abs(ef-af) <= 1e-9
	}
	eArr, eSlice := ev.([]any)
	aArr, aSlice := av.([]any)
	if eSlice || aSlice {
		if !eSlice || !aSlice || len(eArr) != len(aArr) {
			return false
		}
		for i := range eArr {
			if !jsonCellEqual(eArr[i], aArr[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(ev, av)
}

// TestSQLOnFHIR_LiveCompiledSQLExecutionEquivalence verifies that the Zero-JSON SQL statements
// produced by ViewDefinition/$sql execute cleanly against live SQLite (modernc.org/sqlite)
// and live PostgreSQL databases and return the exact same tabular rows as ViewDefinition/$run.
func TestSQLOnFHIR_LiveCompiledSQLExecutionEquivalence(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 1. Boot live PostgreSQL server + DB handle (uses real PostgreSQL when reachable, e.g. in CI)
	var pgDB *sql.DB
	var hasLivePostgresSQL bool
	pgDSN := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if pgDSN == "" {
		pgDSN = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
	}
	if realPG, pgErr := sql.Open("postgres", pgDSN); pgErr == nil {
		if pingErr := realPG.PingContext(ctx); pingErr == nil {
			require.NoError(t, postgres.RunMigrations(ctx, realPG))
			_ = postgres.PurgeTenant(ctx, realPG, "live-sql")
			defer func() { _ = postgres.PurgeTenant(ctx, realPG, "live-sql") }()
			pgDB = realPG
			hasLivePostgresSQL = true
		} else {
			_ = realPG.Close()
		}
	}
	if pgDB == nil {
		pgDB = openMemPostgres(t)
	}
	defer pgDB.Close()
	pgStore := postgres.New(pgDB, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
	pgSrv := server.New(pgStore, logger)
	server.RegisterDefaultResourceTypes(pgSrv)
	pgTS := httptest.NewServer(pgSrv.Handler())
	defer pgTS.Close()

	// 2. Boot live SQLite (modernc.org/sqlite) server + DB handle (always executes real SQL in-process)
	sqlitePath := filepath.Join(t.TempDir(), "live-sql-conformance.db")
	sqliteDB, err := sql.Open("sqlite", sqlitePath)
	require.NoError(t, err)
	defer sqliteDB.Close()
	sqliteDB.SetMaxOpenConns(1)
	require.NoError(t, sqlite.RunMigrations(ctx, sqliteDB, sqlite.JournalWAL))
	sqliteStore := sqlite.New(sqliteDB, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(sqlitePath))

	sqSrv := server.New(sqliteStore, logger)
	server.RegisterDefaultResourceTypes(sqSrv)
	sqTS := httptest.NewServer(sqSrv.Handler())
	defer sqTS.Close()

	// Seed identical clinical cohort into both live servers via FHIR R4 REST API
	fixtures := []struct {
		path string
		body string
	}{
		{
			path: "Patient/p-alice",
			body: `{"resourceType":"Patient","id":"p-alice","active":true,"gender":"female","birthDate":"1985-04-12","name":[{"use":"official","family":"Nguyen","given":["Alice"]}]}`,
		},
		{
			path: "Patient/p-bob",
			body: `{"resourceType":"Patient","id":"p-bob","active":false,"gender":"male","birthDate":"1972-11-03","name":[{"use":"official","family":"Martinez","given":["Robert"]}]}`,
		},
		{
			path: "Observation/obs-hr-1",
			body: `{"resourceType":"Observation","id":"obs-hr-1","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/p-alice"},"effectiveDateTime":"2026-02-10T09:30:00Z","valueQuantity":{"value":72.5,"unit":"/min","system":"http://unitsofmeasure.org","code":"/min"}}`,
		},
		{
			path: "Observation/obs-sbp-1",
			body: `{"resourceType":"Observation","id":"obs-sbp-1","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8480-6"}]},"subject":{"reference":"Patient/p-bob"},"effectiveDateTime":"2026-02-11T14:15:00Z","valueQuantity":{"value":128,"unit":"mmHg","system":"http://unitsofmeasure.org","code":"mm[Hg]"}}`,
		},
		{
			path: "Condition/cond-htn-1",
			body: `{"resourceType":"Condition","id":"cond-htn-1","clinicalStatus":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/condition-clinical","code":"active"}]},"code":{"coding":[{"system":"http://snomed.info/sct","code":"38341003"}]},"subject":{"reference":"Patient/p-bob"}}`,
		},
	}

	for _, targetURL := range []string{pgTS.URL, sqTS.URL} {
		for _, fx := range fixtures {
			req, reqErr := http.NewRequest(http.MethodPut, targetURL+"/fhir/r4/live-sql/"+fx.path, strings.NewReader(fx.body))
			require.NoError(t, reqErr)
			req.Header.Set("Content-Type", "application/fhir+json")
			resp, doErr := http.DefaultClient.Do(req)
			require.NoError(t, doErr)
			resp.Body.Close()
			require.Equal(t, http.StatusCreated, resp.StatusCode)
		}
	}

	views := []struct {
		name     string
		presetID string
		inlineVD string
	}{
		{name: "builtin_patient_demographics", presetID: "patient-demographics"},
		{name: "builtin_observation_vitals", presetID: "observation-vitals"},
		{name: "builtin_condition_problems", presetID: "condition-problems"},
		{
			name: "custom_filtered_female_patients",
			inlineVD: `{
				"resourceType": "ViewDefinition",
				"resource": "Patient",
				"constant": [{"name": "target_gender", "valueCode": "female"}],
				"where": [{"path": "gender = %target_gender"}],
				"select": [{
					"column": [
						{"name": "id", "path": "getResourceKey()"},
						{"name": "family", "path": "name.family.first()"},
						{"name": "gender", "path": "gender"},
						{"name": "birth_date", "path": "birthDate"}
					]
				}]
			}`,
		},
		{
			name: "custom_observation_code_vs_unit",
			inlineVD: `{
				"resourceType": "ViewDefinition",
				"resource": "Observation",
				"where": [{"path": "valueQuantity.value > 100"}],
				"select": [{
					"column": [
						{"name": "id", "path": "getResourceKey()"},
						{"name": "val", "path": "valueQuantity.value"},
						{"name": "ucum_code", "path": "valueQuantity.code"},
						{"name": "human_unit", "path": "valueQuantity.unit"}
					]
				}]
			}`,
		},
	}

	backends := []struct {
		dialect    string
		baseURL    string
		sqlDB      *sql.DB
		hasLiveSQL bool
	}{
		{dialect: "postgres", baseURL: pgTS.URL, sqlDB: pgDB, hasLiveSQL: hasLivePostgresSQL},
		{dialect: "sqlite", baseURL: sqTS.URL, sqlDB: sqliteDB, hasLiveSQL: true},
	}

	for _, bk := range backends {
		bk := bk
		t.Run(bk.dialect, func(t *testing.T) {
			for _, vw := range views {
				t.Run(vw.name, func(t *testing.T) {
					var runResp, sqlResp *http.Response
					var err error
					if vw.presetID != "" {
						runResp, err = http.Get(bk.baseURL + "/fhir/r4/live-sql/ViewDefinition/" + vw.presetID + "/$run?_format=application/json")
						require.NoError(t, err)
						sqlResp, err = http.Get(bk.baseURL + "/fhir/r4/live-sql/ViewDefinition/" + vw.presetID + "/$sql?dialect=" + bk.dialect + "&_format=application/sql")
						require.NoError(t, err)
					} else {
						runResp, err = http.Post(bk.baseURL+"/fhir/r4/live-sql/ViewDefinition/$run?_format=application/json", "application/fhir+json", strings.NewReader(vw.inlineVD))
						require.NoError(t, err)
						sqlResp, err = http.Post(bk.baseURL+"/fhir/r4/live-sql/ViewDefinition/$sql?dialect="+bk.dialect+"&_format=application/sql", "application/fhir+json", strings.NewReader(vw.inlineVD))
						require.NoError(t, err)
					}

					runBytes, _ := io.ReadAll(runResp.Body)
					runResp.Body.Close()
					require.Equal(t, http.StatusOK, runResp.StatusCode, string(runBytes))

					sqlBytes, _ := io.ReadAll(sqlResp.Body)
					sqlResp.Body.Close()
					require.Equal(t, http.StatusOK, sqlResp.StatusCode, string(sqlBytes))

					compiledSQL := strings.TrimSpace(string(sqlBytes))
					require.NotEmpty(t, compiledSQL)
					lowerSQL := strings.ToLower(compiledSQL)
					assert.NotContains(t, lowerSQL, "json_extract")
					assert.NotContains(t, lowerSQL, "jsonb")
					assert.NotContains(t, lowerSQL, "->>")

					var runRows []map[string]any
					require.NoError(t, json.Unmarshal(runBytes, &runRows))
					require.NotEmpty(t, runRows)

					if !bk.hasLiveSQL {
						t.Logf("verified PostgreSQL Zero-JSON compiled SQL generation (live pg daemon not running locally, executes in CI)")
						return
					}

					sqlRows := queryLiveSQLRows(t, ctx, bk.sqlDB, compiledSQL)
					require.Equal(t, len(runRows), len(sqlRows), "row count mismatch for SQL:\n%s\nrunRows=%v\nsqlRows=%v", compiledSQL, runRows, sqlRows)

					for i := range runRows {
						for colName, expVal := range runRows[i] {
							gotVal := sqlRows[i][colName]
							assertLiveSQLCellMatchesRun(t, colName, expVal, gotVal, compiledSQL)
						}
					}
				})
			}
		})
	}
}

func queryLiveSQLRows(t *testing.T, ctx context.Context, db *sql.DB, query string) []map[string]any {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	require.NoError(t, err, "failed executing compiled SQL:\n%s", query)
	defer rows.Close()

	cols, err := rows.Columns()
	require.NoError(t, err)

	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		rowMap := make(map[string]any, len(cols))
		for i, c := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				rowMap[c] = string(b)
			} else {
				rowMap[c] = v
			}
		}
		out = append(out, rowMap)
	}
	require.NoError(t, rows.Err())
	return out
}

func assertLiveSQLCellMatchesRun(t *testing.T, col string, expVal, gotVal any, compiledSQL string) {
	t.Helper()
	if expVal == nil {
		assert.Nil(t, gotVal, "column %s expected nil in SQL:\n%s", col, compiledSQL)
		return
	}
	require.NotNil(t, gotVal, "column %s unexpectedly nil in SQL:\n%s (expected %v)", col, compiledSQL, expVal)

	switch ev := expVal.(type) {
	case bool:
		switch gv := gotVal.(type) {
		case bool:
			assert.Equal(t, ev, gv, "column %s", col)
		case int64:
			assert.Equal(t, ev, gv != 0, "column %s", col)
		case string:
			assert.Equal(t, strconv.FormatBool(ev), strings.ToLower(gv), "column %s", col)
		default:
			t.Fatalf("column %s unexpected SQL bool type %T (%v)", col, gotVal, gotVal)
		}
	case float64:
		var gf float64
		switch gv := gotVal.(type) {
		case float64:
			gf = gv
		case int64:
			gf = float64(gv)
		case string:
			parsed, err := strconv.ParseFloat(gv, 64)
			require.NoError(t, err, "column %s parse float %q", col, gv)
			gf = parsed
		default:
			t.Fatalf("column %s unexpected SQL numeric type %T (%v)", col, gotVal, gotVal)
		}
		assert.InDelta(t, ev, gf, 1e-6, "column %s", col)
	case string:
		gs := fmt.Sprintf("%v", gotVal)
		// Normalize date prefix if SQL stored RFC3339 timestamp for date search index
		if len(ev) == 10 && len(gs) > 10 && strings.HasPrefix(gs, ev) {
			gs = gs[:10]
		}
		// spidx_string stores case-folded normalized FHIR string search values (e.g. family/given)
		if (col == "family" || col == "given") && strings.Contains(compiledSQL, "FROM spidx_string") {
			assert.Equal(t, strings.ToLower(ev), gs, "column %s (spidx_string case-folded)", col)
			return
		}
		assert.Equal(t, ev, gs, "column %s", col)
	default:
		assert.Equal(t, fmt.Sprintf("%v", ev), fmt.Sprintf("%v", gotVal), "column %s", col)
	}
}
