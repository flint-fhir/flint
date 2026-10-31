package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

type stubSQLDriver struct {
	pingErr error
	execErr error
}

type stubSQLConn struct {
	pingErr error
	execErr error
}

type stubSQLTx struct{}

func (d *stubSQLDriver) Open(_ string) (driver.Conn, error) {
	return &stubSQLConn{pingErr: d.pingErr, execErr: d.execErr}, nil
}

func (c *stubSQLConn) Prepare(_ string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *stubSQLConn) Close() error                          { return nil }
func (c *stubSQLConn) Begin() (driver.Tx, error)             { return &stubSQLTx{}, nil }
func (c *stubSQLConn) Ping(_ context.Context) error          { return c.pingErr }
func (c *stubSQLConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if c.execErr != nil {
		return nil, c.execErr
	}
	return driver.RowsAffected(1), nil
}

func (t *stubSQLTx) Commit() error   { return nil }
func (t *stubSQLTx) Rollback() error { return nil }

var registerStubDriversOnce sync.Once

func registerStubDrivers() {
	registerStubDriversOnce.Do(func() {
		sql.Register("flintd_stub_ok", &stubSQLDriver{})
		sql.Register("flintd_stub_ping_err", &stubSQLDriver{pingErr: errors.New("ping refused")})
		sql.Register("flintd_stub_mig_err", &stubSQLDriver{execErr: errors.New("migration ddl failed")})
	})
}

func TestParseFlagsAndBrokers(t *testing.T) {
	var buf bytes.Buffer
	showVer, iso, err := parseFlags([]string{"-version"}, &buf)
	require.NoError(t, err)
	assert.True(t, showVer)
	assert.Equal(t, "", iso)

	showVer, iso, err = parseFlags([]string{"-tenant-isolation", "shared"}, nil)
	require.NoError(t, err)
	assert.False(t, showVer)
	assert.Equal(t, "shared", iso)

	_, _, err = parseFlags([]string{"-unknown-flag"}, io.Discard)
	require.Error(t, err)

	assert.Nil(t, parseBrokers(""))
	assert.Nil(t, parseBrokers(" ,  , "))
	assert.Equal(t, []string{"localhost:9092", "broker2:9092"}, parseBrokers(" localhost:9092 , , broker2:9092 "))
}

func TestBuildStoreOptionsAndConfigureAuth(t *testing.T) {
	env := map[string]string{
		"FLINT_TENANT_ROLE_SWITCH":         "true",
		"FLINT_TENANT_AUTO_PROVISION":      "false",
		"FLINT_OIDC_ISSUER":                "https://auth.example.org",
		"FLINT_OIDC_AUDIENCE":              "https://fhir.example.org",
		"FLINT_AUTH_PROVIDER":              "zitadel",
		"FLINT_ZITADEL_TENANT_FROM_DOMAIN": "true",
		"FLINT_SMART_AUTH_URL":             "https://auth.example.org/oauth/v2/authorize",
		"FLINT_SMART_TOKEN_URL":            "https://auth.example.org/oauth/v2/token",
	}
	getenv := func(k string) string { return env[k] }

	opts := buildStoreOptions(postgres.TenantIsolationShared, getenv)
	assert.Len(t, opts, 3)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(nil, logger)
	require.NoError(t, configureAuth(srv, logger, getenv))

	// Standard OIDC provider branch
	env["FLINT_AUTH_PROVIDER"] = "oidc"
	require.NoError(t, configureAuth(srv, logger, getenv))

	// No OIDC issuer configured branch
	delete(env, "FLINT_OIDC_ISSUER")
	require.NoError(t, configureAuth(srv, logger, getenv))
}

func TestFlintdRun_EndToEndAndErrorBranches(t *testing.T) {
	registerStubDrivers()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("version flag prints version and exits cleanly", func(t *testing.T) {
		var out bytes.Buffer
		err := run(
			[]string{"-version"},
			func(string) string { return "" },
			&out,
			logger,
			func(string) (*sql.DB, error) { return nil, errors.New("should not open db") },
			func(string, http.Handler) error { return errors.New("should not serve") },
		)
		require.NoError(t, err)
		assert.Equal(t, "flintd dev\n", out.String())
	})

	t.Run("invalid tenant isolation returns error", func(t *testing.T) {
		err := run(
			[]string{"-tenant-isolation", "invalid-mode"},
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string) (*sql.DB, error) { return nil, nil },
			func(string, http.Handler) error { return nil },
		)
		require.ErrorContains(t, err, "invalid tenant isolation mode")
	})

	t.Run("openDB error, ping error, and migration error", func(t *testing.T) {
		err := run(
			nil,
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string) (*sql.DB, error) { return nil, errors.New("open failed") },
			func(string, http.Handler) error { return nil },
		)
		require.ErrorContains(t, err, "open db")

		err = run(
			nil,
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string) (*sql.DB, error) { return sql.Open("flintd_stub_ping_err", "") },
			func(string, http.Handler) error { return nil },
		)
		require.ErrorContains(t, err, "ping db")

		err = run(
			nil,
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string) (*sql.DB, error) { return sql.Open("flintd_stub_mig_err", "") },
			func(string, http.Handler) error { return nil },
		)
		require.ErrorContains(t, err, "run migrations")
	})

	t.Run("full server wiring with AutoMQ, OIDC, and custom addr", func(t *testing.T) {
		env := map[string]string{
			"FLINT_TENANT_ISOLATION":    "shared",
			"FLINT_AUTOMQ_BROKERS":      "127.0.0.1:19092",
			"FLINT_SCHEMA_REGISTRY_URL": "http://127.0.0.1:18081",
			"FLINT_OIDC_ISSUER":         "https://idp.example.org",
			"FLINT_OIDC_AUDIENCE":       "https://fhir.example.org",
			"FLINT_SMART_AUTH_URL":      "https://idp.example.org/authorize",
			"FLINT_SMART_TOKEN_URL":     "https://idp.example.org/token",
			"FLINT_ADDR":                ":9099",
		}
		var boundAddr string
		var capturedHandler http.Handler
		err := run(
			nil,
			func(k string) string { return env[k] },
			io.Discard,
			logger,
			func(string) (*sql.DB, error) { return sql.Open("flintd_stub_ok", "") },
			func(addr string, h http.Handler) error {
				boundAddr = addr
				capturedHandler = h
				return nil
			},
		)
		require.NoError(t, err)
		assert.Equal(t, ":9099", boundAddr)
		require.NotNil(t, capturedHandler)

		// Verify /healthz responds 200 OK and /fhir/r4/t1/Patient/123 enforces OIDC 401 when unauthenticated
		reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		recHealth := httptest.NewRecorder()
		capturedHandler.ServeHTTP(recHealth, reqHealth)
		assert.Equal(t, http.StatusOK, recHealth.Code)

		reqFHIR := httptest.NewRequest(http.MethodGet, "/fhir/r4/t1/Patient/123", nil)
		recFHIR := httptest.NewRecorder()
		capturedHandler.ServeHTTP(recFHIR, reqFHIR)
		assert.Equal(t, http.StatusUnauthorized, recFHIR.Code)
	})
}
