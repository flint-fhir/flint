package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type certStubDriver struct {
	pingErr error
	execErr error
}

type certStubConn struct {
	pingErr error
	execErr error
}

type certStubTx struct{}

func (d *certStubDriver) Open(_ string) (driver.Conn, error) {
	return &certStubConn{pingErr: d.pingErr, execErr: d.execErr}, nil
}
func (c *certStubConn) Prepare(_ string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *certStubConn) Close() error                          { return nil }
func (c *certStubConn) Begin() (driver.Tx, error)             { return &certStubTx{}, nil }
func (c *certStubConn) Ping(_ context.Context) error          { return c.pingErr }
func (c *certStubConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if c.execErr != nil {
		return nil, c.execErr
	}
	return driver.RowsAffected(1), nil
}
func (t *certStubTx) Commit() error   { return nil }
func (t *certStubTx) Rollback() error { return nil }

var certStubOnce sync.Once

func registerCertStubDrivers() {
	certStubOnce.Do(func() {
		sql.Register("cert_stub_ok", &certStubDriver{})
		sql.Register("cert_stub_ping_err", &certStubDriver{pingErr: errors.New("ping refused")})
		sql.Register("cert_stub_mig_err", &certStubDriver{execErr: errors.New("migration failed")})
	})
}

func TestParseCLIOptions(t *testing.T) {
	env := map[string]string{
		"FLINT_URL":          "http://localhost:8080",
		"FLINT_POSTGRES_DSN": "postgres://custom/db",
		"FLINT_TENANT":       "env-tenant",
		"FLINT_TOKEN":        "env-tok",
	}
	opts, err := parseCLIOptions(nil, func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8080", opts.URL)
	assert.Equal(t, "postgres://custom/db", opts.DSN)
	assert.Equal(t, "env-tenant", opts.Tenant)
	assert.Equal(t, "env-tok", opts.Token)

	opts2, err := parseCLIOptions(
		[]string{"-url", "http://example.org", "-tenant", "flag-tenant", "-token", "flag-tok", "-out", "/tmp/report.json"},
		func(string) string { return "" },
	)
	require.NoError(t, err)
	assert.Equal(t, "http://example.org", opts2.URL)
	assert.Equal(t, "flag-tenant", opts2.Tenant)
	assert.Equal(t, "flag-tok", opts2.Token)
	assert.Equal(t, "/tmp/report.json", opts2.OutPath)

	_, err = parseCLIOptions([]string{"-invalid-flag"}, func(string) string { return "" })
	require.Error(t, err)
}

func TestRunCertifyCLI_RemoteAndEphemeralPaths(t *testing.T) {
	registerCertStubDrivers()

	t.Run("failing target server writes TestReport file and returns exit code 1", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome"}`))
		}))
		defer ts.Close()

		outFile := filepath.Join(t.TempDir(), "testreport.json")
		var stdout, stderr bytes.Buffer
		code := runCertifyCLI(
			t.Context(),
			cliOptions{
				URL:        ts.URL,
				Tenant:     "cert-cli",
				Token:      "tok",
				OutPath:    outFile,
				HTTPClient: ts.Client(),
			},
			nil,
			&stdout,
			&stderr,
		)
		assert.Equal(t, 1, code)
		assert.Contains(t, stdout.String(), "FLINT FHIR R4 / US CORE v6.1.0 & v7.0.0 / ONC (g)(10) CERTIFICATION SCORECARD")
		assert.Contains(t, stdout.String(), "Wrote FHIR R4 TestReport JSON")

		raw, err := os.ReadFile(outFile)
		require.NoError(t, err)
		var tr map[string]any
		require.NoError(t, json.Unmarshal(raw, &tr))
		assert.Equal(t, "TestReport", tr["resourceType"])
		assert.Equal(t, "fail", tr["result"])
	})

	t.Run("ephemeral mode DB open, ping, migration, and execution paths", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runCertifyCLI(
			t.Context(),
			cliOptions{DSN: "bad"},
			func(string) (*sql.DB, error) { return nil, errors.New("open err") },
			&stdout,
			&stderr,
		)
		assert.Equal(t, 1, code)
		assert.Contains(t, stderr.String(), "open postgres")

		stderr.Reset()
		code = runCertifyCLI(
			t.Context(),
			cliOptions{DSN: "bad"},
			func(string) (*sql.DB, error) { return sql.Open("cert_stub_ping_err", "") },
			&stdout,
			&stderr,
		)
		assert.Equal(t, 1, code)
		assert.Contains(t, stderr.String(), "ping postgres")

		stderr.Reset()
		code = runCertifyCLI(
			t.Context(),
			cliOptions{DSN: "bad"},
			func(string) (*sql.DB, error) { return sql.Open("cert_stub_mig_err", "") },
			&stdout,
			&stderr,
		)
		assert.Equal(t, 1, code)
		assert.Contains(t, stderr.String(), "run migrations")

		// Ephemeral server boot + cleanup with stub driver
		stdout.Reset()
		stderr.Reset()
		code = runCertifyCLI(
			t.Context(),
			cliOptions{DSN: "stub", Tenant: "ephemeral-tenant"},
			func(string) (*sql.DB, error) { return sql.Open("cert_stub_ok", "") },
			&stdout,
			&stderr,
		)
		assert.Equal(t, 1, code)
		assert.Contains(t, stdout.String(), "CERTIFICATION SCORECARD")
	})
}
