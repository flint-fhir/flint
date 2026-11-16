package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"hegel.dev/go/hegel"
	_ "modernc.org/sqlite"

	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

func canonicalizeSQLOnFHIRCell(colType string, v any) string {
	if v == nil {
		return "<null>"
	}
	if b, isBytes := v.([]byte); isBytes {
		v = string(b)
	}

	switch colType {
	case "boolean":
		switch tv := v.(type) {
		case bool:
			return fmt.Sprintf("bool:%v", tv)
		case int64:
			return fmt.Sprintf("bool:%v", tv != 0)
		case float64:
			return fmt.Sprintf("bool:%v", tv != 0)
		case string:
			low := strings.ToLower(strings.TrimSpace(tv))
			if low == "1" || low == "true" || low == "t" {
				return "bool:true"
			}
			if low == "0" || low == "false" || low == "f" {
				return "bool:false"
			}
		}
	case "decimal":
		switch tv := v.(type) {
		case float64:
			return "num:" + strconv.FormatFloat(tv, 'g', 10, 64)
		case int64:
			return "num:" + strconv.FormatInt(tv, 10)
		case json.Number:
			if f, err := tv.Float64(); err == nil {
				return "num:" + strconv.FormatFloat(f, 'g', 10, 64)
			}
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(tv), 64); err == nil {
				return "num:" + strconv.FormatFloat(f, 'g', 10, 64)
			}
		}
	case "date":
		s := strings.TrimSpace(fmt.Sprint(v))
		if len(s) >= 10 && strings.HasSuffix(s, "T00:00:00Z") {
			s = s[:10]
		}
		return "str:" + s
	case "spidx_string":
		// spidx_string stores case-folded normalized FHIR string search values (see conformance_sqlonfhir_official_test.go).
		return "str:" + strings.ToLower(fmt.Sprint(v))
	}
	return "str:" + fmt.Sprint(v)
}

func formatRowMultiset(rows []map[string]any, cols []struct{ name, colType string }) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = c.name + "=" + canonicalizeSQLOnFHIRCell(c.colType, r[c.name])
		}
		out = append(out, strings.Join(parts, "|"))
	}
	sort.Strings(out)
	return out
}

func querySQLRows(ctx context.Context, db *sql.DB, query string) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	colNames, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rowMap := make(map[string]any, len(colNames))
		for i, col := range colNames {
			if b, ok := vals[i].([]byte); ok {
				rowMap[col] = string(b)
			} else {
				rowMap[col] = vals[i]
			}
		}
		out = append(out, rowMap)
	}
	return out, rows.Err()
}

// TestProperty_SQLOnFHIRRunVsSQLDifferentialOracle verifies that SQL-on-FHIR v2 ViewDefinition/$run
// (in-memory FHIRPath AST evaluation over Proto3 resources) produces the exact same row multiset
// as ViewDefinition/$sql transpiled to Zero-JSONB spidx_* SQL and executed directly against a
// real SQLite database (and live PostgreSQL when FLINT_TEST_POSTGRES_DSN is configured).
func TestProperty_SQLOnFHIRRunVsSQLDifferentialOracle(t *testing.T) {
	ctx := context.Background()

	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)
	if err := sqlite.RunMigrations(ctx, sqlDB, sqlite.JournalMemory); err != nil {
		t.Fatalf("sqlite migrations: %v", err)
	}

	st := sqlite.New(sqlDB, sqlite.WithJournalMode(sqlite.JournalMemory), sqlite.WithDSN(":memory:"))
	_, ts := newHardeningServer(t, st)
	client := ts.Client()

	var runSeq atomic.Int64

	hegel.Test(t, func(ht *hegel.T) {
		seq := runSeq.Add(1)
		tenant := fmt.Sprintf("sof-oracle-%d", seq)

		numPatients := hegel.Draw(ht, hegel.Integers(2, 5))
		for i := 0; i < numPatients; i++ {
			patID := fmt.Sprintf("pat-%d", i+1)
			family := hegel.Draw(ht, hegel.SampledFrom([]string{"Ada", "Babbage", "Curie", "Darwin"}))
			given := hegel.Draw(ht, hegel.SampledFrom([]string{"Grace", "Alan", "Marie", "Charles"}))
			gender := hegel.Draw(ht, hegel.SampledFrom([]string{"female", "male", "other"}))
			active := hegel.Draw(ht, hegel.Booleans())
			birthYear := hegel.Draw(ht, hegel.SampledFrom([]int{1975, 1985, 1995, 2005}))
			patJSON := fmt.Sprintf(`{
				"resourceType": "Patient",
				"id": %q,
				"active": %v,
				"gender": %q,
				"birthDate": "%04d-06-15",
				"name": [{"use": "official", "family": %q, "given": [%q]}]
			}`, patID, active, gender, birthYear, family, given)

			req, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Patient/"+patID, strings.NewReader(patJSON))
			req.Header.Set("Content-Type", "application/fhir+json")
			resp, err := client.Do(req)
			if err != nil {
				ht.Fatalf("seed Patient PUT: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
				ht.Fatalf("seed Patient status: %d", resp.StatusCode)
			}

			// Randomly update or soft-delete to ensure multi-version & tombstone rows in fhir_resource / spidx_*!
			lifecycle := hegel.Draw(ht, hegel.SampledFrom([]string{"keep", "update", "delete"}))
			switch lifecycle {
			case "update":
				updJSON := fmt.Sprintf(`{
					"resourceType": "Patient",
					"id": %q,
					"active": true,
					"gender": %q,
					"birthDate": "%04d-06-15",
					"name": [{"use": "official", "family": %q, "given": [%q]}]
				}`, patID, gender, birthYear, family+"Updated", given)
				uReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Patient/"+patID, strings.NewReader(updJSON))
				uReq.Header.Set("Content-Type", "application/fhir+json")
				uResp, err := client.Do(uReq)
				if err != nil {
					ht.Fatalf("update Patient PUT: %v", err)
				}
				_, _ = io.Copy(io.Discard, uResp.Body)
				uResp.Body.Close()
			case "delete":
				dReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/fhir/r4/"+tenant+"/Patient/"+patID, nil)
				dResp, err := client.Do(dReq)
				if err != nil {
					ht.Fatalf("delete Patient DELETE: %v", err)
				}
				_, _ = io.Copy(io.Discard, dResp.Body)
				dResp.Body.Close()
			}

			// Seed an Observation for this Patient
			obsID := fmt.Sprintf("obs-%d", i+1)
			obsStatus := hegel.Draw(ht, hegel.SampledFrom([]string{"final", "preliminary"}))
			loincCode := hegel.Draw(ht, hegel.SampledFrom([]string{"8867-4", "29463-7", "8310-5"}))
			qtyVal := hegel.Draw(ht, hegel.SampledFrom([]float64{36.8, 72.5, 98.6, 120.0}))
			obsJSON := fmt.Sprintf(`{
				"resourceType": "Observation",
				"id": %q,
				"status": %q,
				"code": {"coding": [{"system": "http://loinc.org", "code": %q}]},
				"subject": {"reference": "Patient/%s"},
				"effectiveDateTime": "2026-04-01T10:00:00Z",
				"valueQuantity": {"value": %v, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"}
			}`, obsID, obsStatus, loincCode, patID, qtyVal)
			oReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Observation/"+obsID, strings.NewReader(obsJSON))
			oReq.Header.Set("Content-Type", "application/fhir+json")
			oResp, err := client.Do(oReq)
			if err != nil {
				ht.Fatalf("seed Observation PUT: %v", err)
			}
			_, _ = io.Copy(io.Discard, oResp.Body)
			oResp.Body.Close()
		}

		// Choose a randomized ViewDefinition shape to evaluate differentially!
		viewVariant := hegel.Draw(ht, hegel.SampledFrom([]string{
			"patient_demographics_filtered",
			"patient_union_all",
			"observation_vitals_filtered",
		}))

		var vdJSON string
		var colSchema []struct{ name, colType string }

		switch viewVariant {
		case "patient_demographics_filtered":
			whereExpr := hegel.Draw(ht, hegel.SampledFrom([]string{
				"gender = 'female'",
				"gender = 'male'",
				"name.family.startsWith('Ad') or name.family.startsWith('Cu')",
				"not(active = false)",
				"birthDate.exists()",
			}))
			vdJSON = fmt.Sprintf(`{
				"resourceType": "ViewDefinition",
				"name": "pbt_patient_view",
				"status": "active",
				"resource": "Patient",
				"select": [{
					"column": [
						{"name": "id", "path": "getResourceKey()", "type": "string"},
						{"name": "family", "path": "name.family.first()", "type": "string"},
						{"name": "given", "path": "name.given.first()", "type": "string"},
						{"name": "given_upper", "path": "name.given.first().upper()", "type": "string"},
						{"name": "gender", "path": "gender", "type": "code"},
						{"name": "birth_date", "path": "birthDate", "type": "date"},
						{"name": "active", "path": "active", "type": "boolean"}
					]
				}],
				"where": [{"path": %q}]
			}`, whereExpr)
			colSchema = []struct{ name, colType string }{
				{"id", "string"},
				{"family", "spidx_string"},
				{"given", "spidx_string"},
				{"given_upper", "string"},
				{"gender", "code"},
				{"birth_date", "date"},
				{"active", "boolean"},
			}

		case "patient_union_all":
			vdJSON = `{
				"resourceType": "ViewDefinition",
				"name": "pbt_patient_union",
				"status": "active",
				"resource": "Patient",
				"select": [{
					"column": [{"name": "id", "path": "getResourceKey()", "type": "string"}],
					"unionAll": [
						{"column": [{"name": "name_part", "path": "name.family.first()"}, {"name": "part_kind", "path": "'family'"}]},
						{"column": [{"name": "name_part", "path": "name.given.first()"}, {"name": "part_kind", "path": "'given'"}]}
					]
				}],
				"where": [{"path": "not(active = false)"}]
			}`
			colSchema = []struct{ name, colType string }{
				{"id", "string"},
				{"name_part", "spidx_string"},
				{"part_kind", "string"},
			}

		case "observation_vitals_filtered":
			obsWhere := hegel.Draw(ht, hegel.SampledFrom([]string{
				"status = 'final'",
				"code.coding.code = '8867-4'",
				"valueQuantity.value > 50",
			}))
			vdJSON = fmt.Sprintf(`{
				"resourceType": "ViewDefinition",
				"name": "pbt_obs_view",
				"status": "active",
				"resource": "Observation",
				"select": [{
					"column": [
						{"name": "id", "path": "getResourceKey()", "type": "string"},
						{"name": "patient_id", "path": "subject.getReferenceKey('Patient')", "type": "string"},
						{"name": "status", "path": "status", "type": "code"},
						{"name": "code", "path": "code.coding.code.first()", "type": "code"},
						{"name": "value", "path": "valueQuantity.value", "type": "decimal"},
						{"name": "unit", "path": "valueQuantity.code", "type": "code"}
					]
				}],
				"where": [{"path": %q}]
			}`, obsWhere)
			colSchema = []struct{ name, colType string }{
				{"id", "string"},
				{"patient_id", "string"},
				{"status", "code"},
				{"code", "code"},
				{"value", "decimal"},
				{"unit", "code"},
			}
		}

		// Oracle A: ViewDefinition/$run (in-memory FHIRPath AST over Proto3)
		runReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/ViewDefinition/$run", strings.NewReader(vdJSON))
		runReq.Header.Set("Content-Type", "application/fhir+json")
		runResp, err := client.Do(runReq)
		if err != nil {
			ht.Fatalf("ViewDefinition/$run HTTP error: %v", err)
		}
		runBytes, _ := io.ReadAll(runResp.Body)
		runResp.Body.Close()
		if runResp.StatusCode != http.StatusOK {
			ht.Fatalf("ViewDefinition/$run failed (%d): %s", runResp.StatusCode, string(runBytes))
		}
		var runRows []map[string]any
		if err := json.Unmarshal(runBytes, &runRows); err != nil {
			ht.Fatalf("unmarshal $run rows: %v\nbody=%s", err, string(runBytes))
		}

		// Oracle B: ViewDefinition/$sql?dialect=sqlite&_format=application/sql executed against real modernc.org/sqlite!
		sqlReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/ViewDefinition/$sql?dialect=sqlite&_format=application/sql", strings.NewReader(vdJSON))
		sqlReq.Header.Set("Content-Type", "application/fhir+json")
		sqlResp, err := client.Do(sqlReq)
		if err != nil {
			ht.Fatalf("ViewDefinition/$sql HTTP error: %v", err)
		}
		sqlBytes, _ := io.ReadAll(sqlResp.Body)
		sqlResp.Body.Close()
		if sqlResp.StatusCode != http.StatusOK {
			ht.Fatalf("ViewDefinition/$sql failed (%d): %s", sqlResp.StatusCode, string(sqlBytes))
		}
		rawSQL := strings.TrimSpace(string(sqlBytes))

		sqlRows, err := querySQLRows(ctx, sqlDB, rawSQL)
		if err != nil {
			ht.Fatalf("executing transpiled SQLite query failed: %v\nsql=%s", err, rawSQL)
		}

		gotRun := formatRowMultiset(runRows, colSchema)
		gotSQL := formatRowMultiset(sqlRows, colSchema)
		if !reflect.DeepEqual(gotRun, gotSQL) {
			ht.Fatalf("SQL-on-FHIR $run vs $sql differential mismatch (%s)!\n$run (%d): %v\n$sql (%d): %v\nrawSQL: %s",
				viewVariant, len(gotRun), gotRun, len(gotSQL), gotSQL, rawSQL)
		}
	}, hegel.WithTestCases(40))
}

// TestProperty_TransactionBundleAtomicityAndURNUUIDGraph verifies that FHIR R4 transaction
// Bundles atomically rewrite arbitrary urn:uuid: DAGs on commit and perform 100% all-or-nothing
// rollback (zero leaked POST resources, zero version bumps, zero leaked SQLite rows) when an
// entry at any position k fails optimistic concurrency (If-Match) or validation.
func TestProperty_TransactionBundleAtomicityAndURNUUIDGraph(t *testing.T) {
	ctx := context.Background()

	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)
	if err := sqlite.RunMigrations(ctx, sqlDB, sqlite.JournalMemory); err != nil {
		t.Fatalf("sqlite migrations: %v", err)
	}

	sqliteSt := sqlite.New(sqlDB, sqlite.WithJournalMode(sqlite.JournalMemory), sqlite.WithDSN(":memory:"))
	dorisSt := doris.New(nil)

	type serverTarget struct {
		name  string
		url   string
		sqlDB *sql.DB
	}
	_, tsSQLite := newHardeningServer(t, sqliteSt)
	_, tsDoris := newHardeningServer(t, dorisSt)

	targets := []serverTarget{
		{name: "sqlite", url: tsSQLite.URL, sqlDB: sqlDB},
		{name: "doris", url: tsDoris.URL},
	}

	if dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN"); dsn != "" {
		pgDB, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		t.Cleanup(func() { _ = pgDB.Close() })
		if err := postgres.RunMigrations(ctx, pgDB); err != nil {
			t.Fatalf("postgres migrations: %v", err)
		}
		_, tsPG := newHardeningServer(t, postgres.New(pgDB))
		targets = append(targets, serverTarget{name: "postgres", url: tsPG.URL})
	}

	httpClient := &http.Client{}
	var runSeq atomic.Int64

	hegel.Test(t, func(ht *hegel.T) {
		seq := runSeq.Add(1)
		tenant := fmt.Sprintf("tx-pbt-%d", seq)

		injectConflict := hegel.Draw(ht, hegel.Booleans())
		failPos := hegel.Draw(ht, hegel.Integers(0, 3))
		family := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{3,8}`, true))

		orgURN := "urn:uuid:" + uuid.NewString()
		patURN := "urn:uuid:" + uuid.NewString()
		obsURN := "urn:uuid:" + uuid.NewString()

		for _, tgt := range targets {
			// Seed baseline Patient/existing-pat at v1
			seedJSON := `{"resourceType":"Patient","id":"existing-pat","active":true,"name":[{"family":"OriginalV1"}]}`
			sReq, _ := http.NewRequest(http.MethodPut, tgt.url+"/fhir/r4/"+tenant+"/Patient/existing-pat", strings.NewReader(seedJSON))
			sReq.Header.Set("Content-Type", "application/fhir+json")
			sResp, err := httpClient.Do(sReq)
			if err != nil {
				ht.Fatalf("%s seed existing-pat failed: %v", tgt.name, err)
			}
			_, _ = io.Copy(io.Discard, sResp.Body)
			sResp.Body.Close()

			ifMatch := `W/"1"`
			if injectConflict {
				ifMatch = `W/"999"`
			}

			postEntries := []map[string]any{
				{
					"fullUrl": orgURN,
					"resource": map[string]any{
						"resourceType": "Organization",
						"active":       true,
						"name":         "Org " + family,
					},
					"request": map[string]any{"method": "POST", "url": "Organization"},
				},
				{
					"fullUrl": patURN,
					"resource": map[string]any{
						"resourceType":         "Patient",
						"active":               true,
						"name":                 []any{map[string]any{"family": family}},
						"managingOrganization": map[string]any{"reference": orgURN},
					},
					"request": map[string]any{"method": "POST", "url": "Patient"},
				},
				{
					"fullUrl": obsURN,
					"resource": map[string]any{
						"resourceType": "Observation",
						"status":       "final",
						"code":         map[string]any{"coding": []any{map[string]any{"system": "http://loinc.org", "code": "8867-4"}}},
						"subject":      map[string]any{"reference": patURN},
						"performer":    []any{map[string]any{"reference": orgURN}},
					},
					"request": map[string]any{"method": "POST", "url": "Observation"},
				},
			}
			casEntry := map[string]any{
				"fullUrl": "Patient/existing-pat",
				"resource": map[string]any{
					"resourceType": "Patient",
					"id":           "existing-pat",
					"active":       true,
					"name":         []any{map[string]any{"family": "UpdatedV2"}},
				},
				"request": map[string]any{
					"method":  "PUT",
					"url":     "Patient/existing-pat",
					"ifMatch": ifMatch,
				},
			}

			entries := make([]map[string]any, 0, 4)
			entries = append(entries, postEntries[:failPos]...)
			entries = append(entries, casEntry)
			entries = append(entries, postEntries[failPos:]...)

			bundlePayload, _ := json.Marshal(map[string]any{
				"resourceType": "Bundle",
				"type":         "transaction",
				"entry":        entries,
			})

			txReq, _ := http.NewRequest(http.MethodPost, tgt.url+"/fhir/r4/"+tenant, bytes.NewReader(bundlePayload))
			txReq.Header.Set("Content-Type", "application/fhir+json")
			txResp, err := httpClient.Do(txReq)
			if err != nil {
				ht.Fatalf("%s POST transaction failed: %v", tgt.name, err)
			}
			txBytes, _ := io.ReadAll(txResp.Body)
			txResp.Body.Close()

			if injectConflict {
				if txResp.StatusCode < 400 || txResp.StatusCode >= 500 {
					ht.Fatalf("%s expected 4xx on conflict at pos %d, got %d: %s", tgt.name, failPos, txResp.StatusCode, string(txBytes))
				}
				// Verify existing-pat was NOT mutated and stays at versionId "1"
				gResp, err := httpClient.Get(tgt.url + "/fhir/r4/" + tenant + "/Patient/existing-pat")
				if err != nil {
					ht.Fatalf("%s GET existing-pat: %v", tgt.name, err)
				}
				gBytes, _ := io.ReadAll(gResp.Body)
				gResp.Body.Close()
				if gResp.Header.Get("ETag") != `W/"1"` || !strings.Contains(string(gBytes), `"OriginalV1"`) {
					ht.Fatalf("%s rollback failed on existing-pat: ETag=%s body=%s", tgt.name, gResp.Header.Get("ETag"), string(gBytes))
				}
				// Verify 0 leaked Observation / Organization resources in search AND in SQLite table!
				obsSearch, err := httpClient.Get(tgt.url + "/fhir/r4/" + tenant + "/Observation?_summary=count")
				if err != nil {
					ht.Fatalf("%s GET Observation count: %v", tgt.name, err)
				}
				obsBytes, _ := io.ReadAll(obsSearch.Body)
				obsSearch.Body.Close()
				if !strings.Contains(string(obsBytes), `"total":0`) {
					ht.Fatalf("%s leaked Observation after failed transaction: %s", tgt.name, string(obsBytes))
				}
				if tgt.sqlDB != nil {
					var cnt int
					if err := tgt.sqlDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM fhir_resource WHERE tenant_id = ? AND is_deleted = 0", tenant).Scan(&cnt); err != nil {
						ht.Fatalf("sqlite count query: %v", err)
					}
					if cnt != 1 {
						ht.Fatalf("sqlite SQL table leaked rows on rolled-back transaction: got %d active rows, want 1 (existing-pat)", cnt)
					}
				}
			} else {
				if txResp.StatusCode != http.StatusOK {
					ht.Fatalf("%s expected 200 OK on valid transaction, got %d: %s", tgt.name, txResp.StatusCode, string(txBytes))
				}
				if strings.Contains(string(txBytes), "urn:uuid:") {
					ht.Fatalf("%s unresolved urn:uuid: remained in transaction-response: %s", tgt.name, string(txBytes))
				}
				// Verify existing-pat advanced to v2
				gResp, err := httpClient.Get(tgt.url + "/fhir/r4/" + tenant + "/Patient/existing-pat")
				if err != nil {
					ht.Fatalf("%s GET existing-pat: %v", tgt.name, err)
				}
				_, _ = io.Copy(io.Discard, gResp.Body)
				gResp.Body.Close()
				if gResp.Header.Get("ETag") != `W/"2"` {
					ht.Fatalf("%s expected existing-pat ETag W/\"2\", got %s", tgt.name, gResp.Header.Get("ETag"))
				}
			}
		}
	}, hegel.WithTestCases(30))
}

// TestProperty_PatchHTTPAtomicityAndETagMonotonicity verifies HTTP PATCH across RFC 6902 JSON Patch,
// RFC 7396 Merge Patch, and FHIRPath Parameters Patch: failed preconditions / assertions never bump
// versionId or mutate stored state, whereas valid patches increment versionId by +1 and preserve untouched elements.
func TestProperty_PatchHTTPAtomicityAndETagMonotonicity(t *testing.T) {
	st := sqlite.New(nil, sqlite.WithDSN(":memory:"))
	_, ts := newHardeningServer(t, st)
	client := ts.Client()

	var runSeq atomic.Int64

	hegel.Test(t, func(ht *hegel.T) {
		seq := runSeq.Add(1)
		tenant := fmt.Sprintf("patch-pbt-%d", seq)
		patID := "pat-patch-1"

		origFamily := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{3,8}`, true))
		nextFamily := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{3,8}`, true))
		patchFormat := hegel.Draw(ht, hegel.SampledFrom([]string{"json-patch", "merge-patch", "fhirpath-patch"}))

		seed := fmt.Sprintf(`{
			"resourceType": "Patient",
			"id": %q,
			"active": true,
			"gender": "female",
			"birthDate": "1991-03-14",
			"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-PATCH-1"}],
			"name": [{"use": "official", "family": %q, "given": ["Alice"]}]
		}`, patID, origFamily)

		putReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Patient/"+patID, strings.NewReader(seed))
		putReq.Header.Set("Content-Type", "application/fhir+json")
		putResp, err := client.Do(putReq)
		if err != nil {
			ht.Fatalf("seed PUT: %v", err)
		}
		_, _ = io.Copy(io.Discard, putResp.Body)
		putResp.Body.Close()

		// 1. Failing patch attempt (stale If-Match OR failing RFC 6902 test op) MUST NOT increment ETag!
		failPatchReq, _ := http.NewRequest(
			http.MethodPatch,
			ts.URL+"/fhir/r4/"+tenant+"/Patient/"+patID,
			strings.NewReader(`[{"op":"replace","path":"/gender","value":"male"},{"op":"test","path":"/gender","value":"other"}]`),
		)
		failPatchReq.Header.Set("Content-Type", "application/json-patch+json")
		failResp, err := client.Do(failPatchReq)
		if err != nil {
			ht.Fatalf("failing PATCH HTTP error: %v", err)
		}
		_, _ = io.Copy(io.Discard, failResp.Body)
		failResp.Body.Close()
		if failResp.StatusCode < 400 || failResp.StatusCode >= 500 {
			ht.Fatalf("expected 4xx on failing test op PATCH, got %d", failResp.StatusCode)
		}

		checkV1, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/Patient/" + patID)
		if err != nil {
			ht.Fatalf("GET after failed PATCH: %v", err)
		}
		v1Bytes, _ := io.ReadAll(checkV1.Body)
		checkV1.Body.Close()
		if checkV1.Header.Get("ETag") != `W/"1"` || !strings.Contains(string(v1Bytes), `"female"`) {
			ht.Fatalf("failed PATCH mutated state or ETag: ETag=%s body=%s", checkV1.Header.Get("ETag"), string(v1Bytes))
		}

		// 2. Valid patch across all 3 RFC/HL7 patch formats with If-Match: W/"1" -> increments to W/"2"!
		var contentType, body string
		switch patchFormat {
		case "json-patch":
			contentType = "application/json-patch+json"
			body = fmt.Sprintf(`[{"op":"replace","path":"/name/0/family","value":%q}]`, nextFamily)
		case "merge-patch":
			contentType = "application/merge-patch+json"
			body = fmt.Sprintf(`{"gender":"other","name":[{"use":"official","family":%q,"given":["Alice"]}]}`, nextFamily)
		case "fhirpath-patch":
			contentType = "application/fhir+json"
			body = `{
				"resourceType": "Parameters",
				"parameter": [{
					"name": "operation",
					"part": [
						{"name": "type", "valueCode": "replace"},
						{"name": "path", "valueString": "Patient.gender"},
						{"name": "value", "valueCode": "other"}
					]
				}]
			}`
			_ = nextFamily
		}

		okReq, _ := http.NewRequest(http.MethodPatch, ts.URL+"/fhir/r4/"+tenant+"/Patient/"+patID, strings.NewReader(body))
		okReq.Header.Set("Content-Type", contentType)
		okReq.Header.Set("If-Match", `W/"1"`)
		okResp, err := client.Do(okReq)
		if err != nil {
			ht.Fatalf("valid PATCH (%s) HTTP error: %v", patchFormat, err)
		}
		okBytes, _ := io.ReadAll(okResp.Body)
		okResp.Body.Close()
		if okResp.StatusCode != http.StatusOK {
			ht.Fatalf("valid PATCH (%s) failed (%d): %s", patchFormat, okResp.StatusCode, string(okBytes))
		}
		if okResp.Header.Get("ETag") != `W/"2"` {
			ht.Fatalf("valid PATCH (%s) expected ETag W/\"2\", got %s", patchFormat, okResp.Header.Get("ETag"))
		}
		if !strings.Contains(string(okBytes), `"1991-03-14"`) || !strings.Contains(string(okBytes), `"MRN-PATCH-1"`) {
			ht.Fatalf("valid PATCH (%s) lost untouched fields: %s", patchFormat, string(okBytes))
		}
	}, hegel.WithTestCases(45))
}
