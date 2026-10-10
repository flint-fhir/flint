package postgres_test

import (
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/flint-fhir/flint/store/postgres"
)

func stripSQLComments(sqlText string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(sqlText, "\n") {
		if idx := strings.Index(line, "--"); idx != -1 {
			line = line[:idx]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestSchema_NoJSONBAndAtlasSumSync(t *testing.T) {
	schemaBytes, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	schemaBody := stripSQLComments(string(schemaBytes))
	if strings.Contains(strings.ToLower(schemaBody), "jsonb") {
		t.Fatal("schema.sql must not contain JSONB columns")
	}

	requiredTables := []string{
		"fhir_resource",
		"fhir_resource_history",
		"fhir_audit_event",
		"spidx_string",
		"spidx_token",
		"spidx_date",
		"spidx_reference",
		"spidx_quantity",
		"spidx_uri",
	}
	for _, tbl := range requiredTables {
		if !strings.Contains(schemaBody, tbl) {
			t.Errorf("schema.sql is missing required table %q", tbl)
		}
	}

	sumBytes, err := os.ReadFile("migrations/atlas.sum")
	if err != nil {
		t.Fatalf("read migrations/atlas.sum: %v", err)
	}
	sumText := string(sumBytes)

	entries, err := fs.ReadDir(postgres.MigrationsFS, "migrations")
	if err != nil {
		t.Fatalf("ReadDir migrations: %v", err)
	}
	sqlCount := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		sqlCount++
		raw, err := postgres.MigrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if strings.Contains(strings.ToLower(stripSQLComments(string(raw))), "jsonb") {
			t.Errorf("migration %s must not contain JSONB columns", e.Name())
		}
		if !strings.Contains(sumText, e.Name()+" h1:") {
			t.Errorf("migration %s is missing from migrations/atlas.sum (run `atlas migrate hash`)", e.Name())
		}
	}
	if sqlCount < 3 {
		t.Fatalf("expected at least 3 versioned SQL migrations, found %d", sqlCount)
	}
}
