package server_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/fhirxml"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

func openTestScriptSQLiteStore(t *testing.T) store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "testscript-corpus.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, sqlite.RunMigrations(t.Context(), sqlDB, sqlite.JournalWAL))
	return sqlite.New(sqlDB, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(dbPath))
}

func assertTestReportAllPass(t *testing.T, scriptFile string, report map[string]any) {
	t.Helper()
	assert.Equal(t, "TestReport", report["resourceType"])
	assert.Equal(t, "completed", report["status"])

	result, _ := report["result"].(string)
	score, _ := report["score"].(float64)
	if result != "pass" || score < 100.0 {
		pretty, _ := json.MarshalIndent(report, "", "  ")
		t.Fatalf("TestScript %s did not achieve 100%% pass (result=%q score=%.1f):\n%s",
			scriptFile, result, score, string(pretty))
	}
}

func runVendoredTestScriptCorpusOnStore(t *testing.T, backendName string, st store.Store) {
	t.Helper()
	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	corpusDir := filepath.Join("testdata", "testscripts-r4")
	entries, err := os.ReadDir(corpusDir)
	require.NoError(t, err)

	var scriptFiles []string
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".json") {
			scriptFiles = append(scriptFiles, ent.Name())
		}
	}
	require.GreaterOrEqual(t, len(scriptFiles), 4, "expected >= 4 vendored R4 TestScript suites")

	for idx, sf := range scriptFiles {
		t.Run(sf, func(t *testing.T) {
			rawScript, err := os.ReadFile(filepath.Join(corpusDir, sf))
			require.NoError(t, err)

			var scriptHeader struct {
				ID string `json:"id"`
			}
			require.NoError(t, json.Unmarshal(rawScript, &scriptHeader))
			require.NotEmpty(t, scriptHeader.ID)

			// 1. Inline POST /TestScript/$evaluate in isolated tenant
			inlineTenant := fmt.Sprintf("ts-inline-%s-%d", backendName, idx+1)
			postReq := httptest.NewRequest(
				http.MethodPost,
				"/fhir/r4/"+inlineTenant+"/TestScript/$evaluate",
				bytes.NewReader(rawScript),
			)
			postReq.Header.Set("Content-Type", "application/fhir+json")
			postReq.Header.Set("Accept", "application/fhir+json")
			postRec := httptest.NewRecorder()
			h.ServeHTTP(postRec, postReq)
			require.Equalf(t, http.StatusOK, postRec.Code, "inline $evaluate HTTP status: %s", postRec.Body.String())

			var inlineReport map[string]any
			require.NoError(t, json.Unmarshal(postRec.Body.Bytes(), &inlineReport))
			assertTestReportAllPass(t, sf+" (inline)", inlineReport)

			// 2. Persisted PUT /TestScript/{id} + GET /TestScript/{id}/$evaluate (including XML TestReport wire check)
			storedTenant := fmt.Sprintf("ts-stored-%s-%d", backendName, idx+1)
			putReq := httptest.NewRequest(
				http.MethodPut,
				"/fhir/r4/"+storedTenant+"/TestScript/"+scriptHeader.ID,
				bytes.NewReader(rawScript),
			)
			putReq.Header.Set("Content-Type", "application/fhir+json")
			putRec := httptest.NewRecorder()
			h.ServeHTTP(putRec, putReq)
			require.Contains(t, []int{http.StatusOK, http.StatusCreated}, putRec.Code,
				"PUT TestScript/%s failed: %s", scriptHeader.ID, putRec.Body.String())

			evalReq := httptest.NewRequest(
				http.MethodGet,
				"/fhir/r4/"+storedTenant+"/TestScript/"+scriptHeader.ID+"/$evaluate",
				nil,
			)
			evalReq.Header.Set("Accept", "application/fhir+xml")
			evalRec := httptest.NewRecorder()
			h.ServeHTTP(evalRec, evalReq)
			require.Equalf(t, http.StatusOK, evalRec.Code, "stored XML $evaluate HTTP status: %s", evalRec.Body.String())
			assert.Contains(t, strings.ToLower(evalRec.Header().Get("Content-Type")), "application/fhir+xml")

			storedReport, err := fhirxml.UnmarshalFHIRMap(evalRec.Body.Bytes())
			require.NoError(t, err, "decode XML TestReport")
			assertTestReportAllPass(t, sf+" (stored+xml)", storedReport)
		})
	}
}

// TestOfficialHL7R4TestScriptCorpus executes the vendored declarative HL7 FHIR R4
// TestScript corpus (server/testdata/testscripts-r4/*.json) across SQLite WAL and
// PostgreSQL (when FLINT_TEST_POSTGRES_DSN is configured).
func TestOfficialHL7R4TestScriptCorpus(t *testing.T) {
	t.Run("SQLite", func(t *testing.T) {
		st := openTestScriptSQLiteStore(t)
		runVendoredTestScriptCorpusOnStore(t, "sqlite", st)
	})

	if dsn := strings.TrimSpace(os.Getenv("FLINT_TEST_POSTGRES_DSN")); dsn != "" {
		t.Run("PostgreSQL", func(t *testing.T) {
			db, err := sql.Open("pgx", dsn)
			require.NoError(t, err)
			defer db.Close()
			ctx := t.Context()
			require.NoError(t, postgres.RunMigrations(ctx, db))
			for i := 1; i <= 8; i++ {
				_ = postgres.PurgeTenant(ctx, db, fmt.Sprintf("ts-inline-postgres-%d", i))
				_ = postgres.PurgeTenant(ctx, db, fmt.Sprintf("ts-stored-postgres-%d", i))
			}
			t.Cleanup(func() {
				for i := 1; i <= 8; i++ {
					_ = postgres.PurgeTenant(t.Context(), db, fmt.Sprintf("ts-inline-postgres-%d", i))
					_ = postgres.PurgeTenant(t.Context(), db, fmt.Sprintf("ts-stored-postgres-%d", i))
				}
			})
			st := postgres.New(db)
			runVendoredTestScriptCorpusOnStore(t, "postgres", st)
		})
	}
}
