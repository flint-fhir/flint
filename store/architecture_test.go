package store_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listTransitiveDeps(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", `{{join .Deps "\n"}}`, pkg)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go list -Deps %s failed: %s", pkg, string(out))
	deps := make(map[string]bool)
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if d := strings.TrimSpace(line); d != "" {
			deps[d] = true
		}
	}
	return deps
}

func TestArchitecture_ZeroCrossBackendImports(t *testing.T) {
	allBackendPkgs := []string{
		"github.com/flint-fhir/flint/store/postgres",
		"github.com/flint-fhir/flint/store/sqlite",
		"github.com/flint-fhir/flint/store/doris",
	}
	allSQLDrivers := []string{
		"github.com/lib/pq",
		"modernc.org/sqlite",
		"github.com/go-sql-driver/mysql",
	}

	rules := []struct {
		pkg       string
		forbidden []string
	}{
		{
			pkg:       "github.com/flint-fhir/flint/store",
			forbidden: append(append([]string{"database/sql"}, allBackendPkgs...), allSQLDrivers...),
		},
		{
			pkg:       "github.com/flint-fhir/flint/server",
			forbidden: append(append([]string{}, allBackendPkgs...), allSQLDrivers...),
		},
		{
			pkg:       "github.com/flint-fhir/flint/pkg/audit",
			forbidden: append(append([]string{}, allBackendPkgs...), allSQLDrivers...),
		},
		{
			pkg:       "github.com/flint-fhir/flint/ingest/activity",
			forbidden: append(append([]string{}, allBackendPkgs...), allSQLDrivers...),
		},
		{
			pkg: "github.com/flint-fhir/flint/store/postgres",
			forbidden: []string{
				"github.com/flint-fhir/flint/store/sqlite",
				"github.com/flint-fhir/flint/store/doris",
				"modernc.org/sqlite",
				"github.com/go-sql-driver/mysql",
			},
		},
		{
			pkg: "github.com/flint-fhir/flint/store/sqlite",
			forbidden: []string{
				"github.com/flint-fhir/flint/store/postgres",
				"github.com/flint-fhir/flint/store/doris",
				"github.com/lib/pq",
				"github.com/go-sql-driver/mysql",
			},
		},
		{
			pkg: "github.com/flint-fhir/flint/store/doris",
			forbidden: []string{
				"github.com/flint-fhir/flint/store/postgres",
				"github.com/flint-fhir/flint/store/sqlite",
				"github.com/lib/pq",
				"modernc.org/sqlite",
			},
		},
	}

	for _, r := range rules {
		t.Run(r.pkg, func(t *testing.T) {
			deps := listTransitiveDeps(t, r.pkg)
			for _, bad := range r.forbidden {
				assert.False(t, deps[bad], "package %s must NOT transitively import %s", r.pkg, bad)
			}
		})
	}
}
