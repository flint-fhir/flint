package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/hl7v2"
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
type stubSQLRows struct{}

func (r *stubSQLRows) Columns() []string           { return nil }
func (r *stubSQLRows) Close() error                { return nil }
func (r *stubSQLRows) Next(_ []driver.Value) error { return io.EOF }

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
func (c *stubSQLConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	return &stubSQLRows{}, nil
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

	// Built-in SMART v2 OAuth2 bearer enforcement branch
	env["FLINT_SMART_ENFORCE_DEV_AUTH"] = "true"
	require.NoError(t, configureAuth(srv, logger, getenv))

	// CDS Hooks 2.0 JWS configuration branches (single-issuer, multi-issuer, JKU allowlist, and invalid inputs)
	env["FLINT_CDS_HOOKS_REQUIRE_JWT"] = "true"
	env["FLINT_CDS_HOOKS_JKU_ALLOWLIST"] = "https://ehr1.example.org/.well-known, https://ehr2.example.org/jwks"
	env["FLINT_CDS_HOOKS_JWKS_JSON"] = `{"issuer":"https://ehr1.example.org","keys":[{"kty":"EC","crv":"P-256","kid":"k1","x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","y":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}]}`
	require.NoError(t, configureAuth(srv, logger, getenv))

	env["FLINT_CDS_HOOKS_JWKS_JSON"] = `{"https://ehr1.example.org":{"keys":[{"kty":"EC","crv":"P-256","kid":"k1","x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","y":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}]}}`
	require.NoError(t, configureAuth(srv, logger, getenv))

	env["FLINT_CDS_HOOKS_JWKS_JSON"] = `{"https://ehr1.example.org":{"keys":[]}}`
	require.ErrorContains(t, configureAuth(srv, logger, getenv), "non-empty issuer and keys required")

	env["FLINT_CDS_HOOKS_JWKS_JSON"] = `{not-json`
	require.ErrorContains(t, configureAuth(srv, logger, getenv), "invalid FLINT_CDS_HOOKS_JWKS_JSON")

	delete(env, "FLINT_CDS_HOOKS_JWKS_JSON")
	env["FLINT_CDS_HOOKS_REQUIRE_JWT"] = "not-a-bool"
	require.ErrorContains(t, configureAuth(srv, logger, getenv), "invalid FLINT_CDS_HOOKS_REQUIRE_JWT")
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
			func(string, string) (*sql.DB, error) { return nil, errors.New("should not open db") },
			func(string, http.Handler) error { return errors.New("should not serve") },
		)
		require.NoError(t, err)
		assert.Equal(t, "flintd "+server.Version+"\n", out.String())
	})

	t.Run("invalid tenant isolation returns error", func(t *testing.T) {
		err := run(
			[]string{"-tenant-isolation", "invalid-mode"},
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string, string) (*sql.DB, error) { return nil, nil },
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
			func(string, string) (*sql.DB, error) { return nil, errors.New("open failed") },
			func(string, http.Handler) error { return nil },
		)
		require.ErrorContains(t, err, "open db")

		err = run(
			nil,
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string, string) (*sql.DB, error) { return sql.Open("flintd_stub_ping_err", "") },
			func(string, http.Handler) error { return nil },
		)
		require.ErrorContains(t, err, "ping db")

		err = run(
			nil,
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string, string) (*sql.DB, error) { return sql.Open("flintd_stub_mig_err", "") },
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
			func(string, string) (*sql.DB, error) { return sql.Open("flintd_stub_ok", "") },
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

	t.Run("standalone doris store and hybrid postgres+doris analytics wiring", func(t *testing.T) {
		var capturedHandler http.Handler
		err := run(
			[]string{"-store", "doris"},
			func(k string) string {
				if k == "FLINT_DORIS_DSN" {
					return "root:@tcp(127.0.0.1:9030)/flint"
				}
				if k == "FLINT_DORIS_GROUP_COMMIT" {
					return "async_mode"
				}
				return ""
			},
			io.Discard,
			logger,
			func(string, string) (*sql.DB, error) { return sql.Open("flintd_stub_ok", "") },
			func(_ string, h http.Handler) error {
				capturedHandler = h
				return nil
			},
		)
		require.NoError(t, err)
		require.NotNil(t, capturedHandler)

		// Hybrid mode: --store=postgres --analytics-store=doris
		err = run(
			[]string{"-store", "postgres", "-analytics-store", "doris"},
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string, string) (*sql.DB, error) { return sql.Open("flintd_stub_ok", "") },
			func(_ string, _ http.Handler) error { return nil },
		)
		require.NoError(t, err)

		// Embedded SQLite mode: --store=sqlite --sqlite-dsn=flint.db
		err = run(
			[]string{"-store", "sqlite", "-sqlite-dsn", "flint.db"},
			func(k string) string {
				if k == "FLINT_SQLITE_JOURNAL_MODE" {
					return "WAL"
				}
				return ""
			},
			io.Discard,
			logger,
			func(string, string) (*sql.DB, error) { return sql.Open("flintd_stub_ok", "") },
			func(_ string, _ http.Handler) error { return nil },
		)
		require.NoError(t, err)

		// Hybrid mode: --store=postgres --analytics-store=sqlite
		err = run(
			[]string{"-store", "postgres", "-analytics-store", "sqlite"},
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string, string) (*sql.DB, error) { return sql.Open("flintd_stub_ok", "") },
			func(_ string, _ http.Handler) error { return nil },
		)
		require.NoError(t, err)

		// Invalid store backend or analytics backend
		err = run(
			[]string{"-store", "invalid-backend"},
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string, string) (*sql.DB, error) { return nil, nil },
			func(string, http.Handler) error { return nil },
		)
		require.ErrorContains(t, err, "invalid store backend")

		err = run(
			[]string{"-analytics-store", "invalid-analytics"},
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string, string) (*sql.DB, error) { return nil, nil },
			func(string, http.Handler) error { return nil },
		)
		require.ErrorContains(t, err, "invalid analytics backend")

		// Invalid referential integrity mode via CLI or env
		err = run(
			[]string{"-referential-integrity", "invalid-mode"},
			func(string) string { return "" },
			io.Discard,
			logger,
			func(string, string) (*sql.DB, error) { return nil, nil },
			func(string, http.Handler) error { return nil },
		)
		require.ErrorContains(t, err, "invalid referential integrity mode")

		// Strict referential integrity via FLINT_REFERENTIAL_INTEGRITY env var
		err = run(
			[]string{"-store", "sqlite", "-sqlite-dsn", ":memory:"},
			func(k string) string {
				if k == "FLINT_REFERENTIAL_INTEGRITY" {
					return "strict"
				}
				return ""
			},
			io.Discard,
			logger,
			sql.Open,
			func(_ string, h http.Handler) error {
				req := httptest.NewRequest(
					http.MethodPost,
					"/fhir/r4/demo/Observation",
					strings.NewReader(`{"resourceType":"Observation","status":"final","code":{"text":"HR"},"subject":{"reference":"Patient/missing-pat"}}`),
				)
				req.Header.Set("Content-Type", "application/fhir+json")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
				return nil
			},
		)
		require.NoError(t, err)
	})

	t.Run("real modernc sqlite file-backed store startup, HTTP CRUD, and WAL reload", func(t *testing.T) {
		dbFile := filepath.Join(t.TempDir(), "flint-e2e.db")

		// Session 1: boot with real sql.Open("sqlite", dbFile), create Patient over HTTP
		err := run(
			[]string{"-store", "sqlite", "-sqlite-dsn", dbFile},
			func(string) string { return "" },
			io.Discard,
			logger,
			sql.Open,
			func(_ string, h http.Handler) error {
				req := httptest.NewRequest(
					http.MethodPut,
					"/fhir/r4/demo/Patient/pat-wal-1",
					strings.NewReader(`{"resourceType":"Patient","id":"pat-wal-1","active":true,"name":[{"family":"hopper","given":["grace"]}]}`),
				)
				req.Header.Set("Content-Type", "application/fhir+json")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				return nil
			},
		)
		require.NoError(t, err)

		// Session 2: reboot against the exact same dbFile and verify LoadFromDB hydrated Patient/pat-wal-1
		err = run(
			[]string{"-store", "sqlite", "-sqlite-dsn", dbFile},
			func(string) string { return "" },
			io.Discard,
			logger,
			sql.Open,
			func(_ string, h http.Handler) error {
				req := httptest.NewRequest(http.MethodGet, "/fhir/r4/demo/Patient/pat-wal-1", nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), `"hopper"`)

				reqSearch := httptest.NewRequest(http.MethodGet, "/fhir/r4/demo/Patient?family=hopper", nil)
				recSearch := httptest.NewRecorder()
				h.ServeHTTP(recSearch, reqSearch)
				require.Equal(t, http.StatusOK, recSearch.Code, recSearch.Body.String())
				assert.Contains(t, recSearch.Body.String(), `"total":1`)
				return nil
			},
		)
		require.NoError(t, err)
	})

	t.Run("startup --ig-package and FLINT_IG_PACKAGES loading with examples and error handling", func(t *testing.T) {
		tmpDir := t.TempDir()
		tgzPath := filepath.Join(tmpDir, "acme.cardio-1.0.0.tgz")
		writeTestIGArchive(t, tgzPath, map[string]string{
			"package/package.json": `{
				"name": "acme.cardio.r4",
				"version": "1.0.0",
				"canonical": "http://acme.org/fhir/ig/cardio",
				"fhirVersions": ["4.0.1"],
				"title": "Acme Cardiology IG"
			}`,
			"package/StructureDefinition-acme-patient.json": `{
				"resourceType": "StructureDefinition",
				"id": "acme-patient",
				"url": "http://acme.org/fhir/ig/cardio/StructureDefinition/acme-patient",
				"version": "1.0.0",
				"name": "AcmePatient",
				"status": "active",
				"kind": "resource",
				"abstract": false,
				"type": "Patient",
				"baseDefinition": "http://hl7.org/fhir/StructureDefinition/Patient",
				"derivation": "constraint"
			}`,
			"package/examples/Patient-ig-example-pat-1.json": `{
				"resourceType": "Patient",
				"id": "ig-example-pat-1",
				"active": true,
				"name": [{"family": "lovelace", "given": ["ada"]}]
			}`,
		})

		err := run(
			[]string{
				"-store", "sqlite",
				"-sqlite-dsn", ":memory:",
				"-ig-package", tgzPath,
				"-ig-load-examples",
			},
			func(k string) string {
				switch k {
				case "FLINT_ALLOW_LOCAL_IG_PATH_INSTALL":
					return "true"
				case "FLINT_IG_TENANT":
					return "default"
				default:
					return ""
				}
			},
			io.Discard,
			logger,
			sql.Open,
			func(_ string, h http.Handler) error {
				reqIGs := httptest.NewRequest(http.MethodGet, "/fhir/r4/$igs", nil)
				recIGs := httptest.NewRecorder()
				h.ServeHTTP(recIGs, reqIGs)
				require.Equal(t, http.StatusOK, recIGs.Code, recIGs.Body.String())
				assert.Contains(t, recIGs.Body.String(), `"acme.cardio.r4"`)
				assert.Contains(t, recIGs.Body.String(), `"http://acme.org/fhir/ig/cardio"`)

				reqMeta := httptest.NewRequest(http.MethodGet, "/fhir/r4/default/metadata", nil)
				recMeta := httptest.NewRecorder()
				h.ServeHTTP(recMeta, reqMeta)
				require.Equal(t, http.StatusOK, recMeta.Code, recMeta.Body.String())
				assert.Contains(t, recMeta.Body.String(), `"http://acme.org/fhir/ig/cardio/StructureDefinition/acme-patient"`)

				reqEx := httptest.NewRequest(http.MethodGet, "/fhir/r4/default/Patient/ig-example-pat-1", nil)
				recEx := httptest.NewRecorder()
				h.ServeHTTP(recEx, reqEx)
				require.Equal(t, http.StatusOK, recEx.Code, recEx.Body.String())
				assert.Contains(t, recEx.Body.String(), `"lovelace"`)
				return nil
			},
		)
		require.NoError(t, err)

		// Verify FLINT_IG_PACKAGES + FLINT_IG_LOAD_EXAMPLES env resolution and stringListFlag formatting
		var sl stringListFlag
		assert.Equal(t, "", (*stringListFlag)(nil).String())
		require.NoError(t, sl.Set("a.tgz, b.tgz"))
		assert.Equal(t, "a.tgz,b.tgz", sl.String())
		paths, loadEx := resolveIGConfig(cliOptions{igPackages: sl}, func(k string) string {
			switch k {
			case "FLINT_IG_PACKAGES":
				return "c.tgz , d.tgz"
			case "FLINT_IG_LOAD_EXAMPLES":
				return "true"
			default:
				return ""
			}
		})
		assert.Equal(t, []string{"a.tgz", "b.tgz", "c.tgz", "d.tgz"}, paths)
		assert.True(t, loadEx)

		// Startup fails cleanly if a configured IG package path does not exist
		err = run(
			[]string{"-store", "sqlite", "-sqlite-dsn", ":memory:", "-ig-package", filepath.Join(tmpDir, "missing.tgz")},
			func(string) string { return "" },
			io.Discard,
			logger,
			sql.Open,
			func(string, http.Handler) error { return nil },
		)
		require.ErrorContains(t, err, "load ig packages")
	})
}

func writeTestIGArchive(t *testing.T, outPath string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, content := range files {
		raw := []byte(content)
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(raw)),
		}))
		_, err := tw.Write(raw)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	require.NoError(t, os.WriteFile(outPath, buf.Bytes(), 0o600))
}

func TestConfigureTerminologyClientFlagsAndEnv(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Disabled when unset
	srv := server.New(nil, logger)
	require.NoError(t, configureTerminologyClient(srv, cliOptions{}, logger, func(string) string { return "" }))
	assert.Nil(t, srv.TerminologyClient())

	// Configured via CLI flags
	opts, err := parseCLIOptions([]string{
		"--tx-server-url=https://tx.fhir.org/r4/",
		"--tx-timeout=3s",
		"--tx-cache-ttl=10m",
		"--tx-cache-max-entries=2048",
		"--tx-auth-header=Bearer secret",
		"--tx-fail-open=false",
	}, io.Discard)
	require.NoError(t, err)
	require.NoError(t, configureTerminologyClient(srv, opts, logger, func(string) string { return "" }))
	require.NotNil(t, srv.TerminologyClient())
	assert.Equal(t, "https://tx.fhir.org/r4", srv.TerminologyClient().BaseURL())
	assert.False(t, srv.TerminologyClient().FailOpen())

	// Configured via FLINT_TX_* environment variables inside run(...)
	env := map[string]string{
		"FLINT_STORE_BACKEND":        "sqlite",
		"FLINT_SQLITE_DSN":           ":memory:",
		"FLINT_TX_SERVER_URL":        "https://tx-env.fhir.org/r4",
		"FLINT_TX_TIMEOUT":           "4s",
		"FLINT_TX_CACHE_TTL":         "5m",
		"FLINT_TX_CACHE_MAX_ENTRIES": "512",
		"FLINT_TX_AUTH_HEADER":       "X-Api-Key: k1",
		"FLINT_TX_FAIL_OPEN":         "true",
	}
	err = run(
		nil,
		func(k string) string { return env[k] },
		io.Discard,
		logger,
		sql.Open,
		func(string, http.Handler) error { return nil },
	)
	require.NoError(t, err)

	// Invalid configuration error branches
	badCases := []map[string]string{
		{"FLINT_TX_SERVER_URL": "ftp://bad"},
		{"FLINT_TX_SERVER_URL": "https://tx.fhir.org/r4", "FLINT_TX_TIMEOUT": "bad"},
		{"FLINT_TX_SERVER_URL": "https://tx.fhir.org/r4", "FLINT_TX_CACHE_TTL": "bad"},
		{"FLINT_TX_SERVER_URL": "https://tx.fhir.org/r4", "FLINT_TX_CACHE_MAX_ENTRIES": "bad"},
		{"FLINT_TX_SERVER_URL": "https://tx.fhir.org/r4", "FLINT_TX_FAIL_OPEN": "not-bool"},
	}
	for _, tc := range badCases {
		err := configureTerminologyClient(server.New(nil, logger), cliOptions{}, logger, func(k string) string { return tc[k] })
		require.Error(t, err)
	}
}

func TestConfigureTelemetryFlagsAndEnv(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Disabled when unset
	srv := server.New(nil, logger)
	require.NoError(t, configureTelemetry(srv, cliOptions{}, logger, func(string) string { return "" }))
	assert.Nil(t, srv.Tracer())

	// Configured via CLI flags
	opts, err := parseCLIOptions([]string{
		"--otlp-endpoint=http://127.0.0.1:4318",
		"--otlp-protocol=http/json",
		"--otlp-service-name=flint-cluster-node",
		"--otlp-sample-ratio=0.5",
		"--otlp-headers=Authorization=Bearer secret",
	}, io.Discard)
	require.NoError(t, err)
	require.NoError(t, configureTelemetry(srv, opts, logger, func(string) string { return "" }))
	require.NotNil(t, srv.Tracer())
	assert.Equal(t, "http://127.0.0.1:4318/v1/traces", srv.Tracer().Endpoint())
	assert.Equal(t, "http/json", srv.Tracer().Protocol())
	require.NoError(t, srv.Close())

	// Configured via OTEL_EXPORTER_OTLP_* environment variables inside run(...)
	env := map[string]string{
		"FLINT_STORE_BACKEND":         "sqlite",
		"FLINT_SQLITE_DSN":            ":memory:",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		"OTEL_SERVICE_NAME":           "flint-env",
		"OTEL_TRACES_SAMPLER_ARG":     "1.0",
	}
	err = run(
		nil,
		func(k string) string { return env[k] },
		io.Discard,
		logger,
		sql.Open,
		func(string, http.Handler) error { return nil },
	)
	require.NoError(t, err)

	// Invalid sample ratio and endpoint error branches
	require.Error(t, configureTelemetry(server.New(nil, logger), cliOptions{}, logger, func(k string) string {
		switch k {
		case "FLINT_OTLP_ENDPOINT":
			return "http://127.0.0.1:4318"
		case "FLINT_OTLP_SAMPLE_RATIO":
			return "2.5"
		default:
			return ""
		}
	}))
	require.Error(t, configureTelemetry(server.New(nil, logger), cliOptions{}, logger, func(k string) string {
		if k == "FLINT_OTLP_ENDPOINT" {
			return "ftp://bad"
		}
		return ""
	}))
}

func TestMLLPListenerFlagsAndLiveRoundTrip(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Disabled when unset
	srvUnset := server.New(nil, logger)
	mllpUnset, err := startMLLPServerIfConfigured(srvUnset, cliOptions{}, logger, func(string) string { return "" })
	require.NoError(t, err)
	assert.Nil(t, mllpUnset)

	// Configured via CLI flags with live MLLP TCP frame round-trip against SQLite WAL store
	dbPath := filepath.Join(t.TempDir(), "mllp-main-test.db")
	st, sqliteDB, err := openSQLiteStore(dbPath, func(string) string { return "" }, sql.Open)
	require.NoError(t, err)
	defer sqliteDB.Close()

	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)
	defer func() { _ = srv.Close() }()

	opts, err := parseCLIOptions([]string{
		"--mllp-addr=127.0.0.1:0",
		"--mllp-tenant=mllp-test",
	}, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:0", opts.mllpAddr)
	assert.Equal(t, "mllp-test", opts.mllpTenant)

	mllpSrv, err := startMLLPServerIfConfigured(srv, opts, logger, func(string) string { return "" })
	require.NoError(t, err)
	require.NotNil(t, mllpSrv)
	defer func() { _ = mllpSrv.Close() }()

	conn, err := net.Dial("tcp", mllpSrv.Addr())
	require.NoError(t, err)
	defer conn.Close()

	msg := strings.Join([]string{
		"MSH|^~\\&|EHR|CLINIC|FLINT|HOSP|20261119120000+0000||ADT^A01^ADT_A01|FLINTD-MLLP-1|P|2.5.1",
		"PID|1||MRN-FLINTD-1^^^CLINIC^MR||Doe^Jane||19900102|F",
		"PV1|1|I|WARD^101^A||||1234^Physician^Primary",
	}, "\r") + "\r"
	require.NoError(t, hl7v2.WriteMLLPFrame(conn, []byte(msg)))

	ackBytes, err := hl7v2.ReadMLLPFrame(bufio.NewReader(conn), hl7v2.MaxMessageBytes)
	require.NoError(t, err)
	assert.Contains(t, string(ackBytes), "MSA|AA|FLINTD-MLLP-1")

	require.NoError(t, mllpSrv.Close())

	// Configured via FLINT_MLLP_* environment variables inside run(...)
	env := map[string]string{
		"FLINT_STORE_BACKEND": "sqlite",
		"FLINT_SQLITE_DSN":    ":memory:",
		"FLINT_MLLP_ADDR":     "127.0.0.1:0",
		"FLINT_MLLP_TENANT":   "mllp-env",
	}
	err = run(
		nil,
		func(k string) string { return env[k] },
		io.Discard,
		logger,
		sql.Open,
		func(string, http.Handler) error { return nil },
	)
	require.NoError(t, err)

	// Invalid bind address returns an error from both helper and run(...)
	_, err = startMLLPServerIfConfigured(server.New(nil, logger), cliOptions{}, logger, func(k string) string {
		if k == "FLINT_MLLP_ADDR" {
			return "127.0.0.1:999999"
		}
		return ""
	})
	require.ErrorContains(t, err, "start hl7v2 mllp listener")

	err = run(
		[]string{"--store=sqlite", "--sqlite-dsn=:memory:", "--mllp-addr=127.0.0.1:999999"},
		func(string) string { return "" },
		io.Discard,
		logger,
		sql.Open,
		func(string, http.Handler) error { return nil },
	)
	require.ErrorContains(t, err, "start hl7v2 mllp listener")
}

func TestConfigureAuthAndSecurityPolicyHardening(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 1. FLINT_REQUIRE_AUTH=true must fail closed if no TokenValidator is configured
	err := run(
		[]string{"--store=sqlite", "--sqlite-dsn=:memory:"},
		func(k string) string {
			if k == "FLINT_REQUIRE_AUTH" {
				return "true"
			}
			return ""
		},
		io.Discard,
		logger,
		sql.Open,
		func(string, http.Handler) error { return nil },
	)
	require.ErrorContains(t, err, "RequireAuth")

	// 2. Valid PLATFORM_IDENTITY_TRUST_FILE + full security policy env configuration succeeds
	trustPath := filepath.Join(t.TempDir(), "trust.json")
	trustJSON := `{
		"issuers": [{
			"issuer": "https://idp.azra.example",
			"audiences": ["azra-fhir"],
			"jwksUrl": "https://idp.azra.example/.well-known/jwks.json",
			"signingAlgorithms": ["RS256", "PS256"],
			"claims": {
				"shape": "grantMap",
				"grantsClaim": "urn:zitadel:iam:org:project:roles",
				"activeTenantClaim": "active_tenant",
				"operatorRole": "azra-operator",
				"platformTenant": "azra-platform",
				"actorClaim": "act"
			}
		}]
	}`
	require.NoError(t, os.WriteFile(trustPath, []byte(trustJSON), 0o600))

	srv := server.New(nil, logger)
	env := map[string]string{
		"PLATFORM_IDENTITY_TRUST_FILE":     trustPath,
		"FLINT_OIDC_ROLE_TO_SCOPES":        `clinician=user/Patient.rs;azra-operator=system/*.*`,
		"FLINT_REQUIRE_AUTH":               "true",
		"FLINT_REQUIRE_TENANT_CLAIM":       "true",
		"FLINT_DISABLE_UNAUTH_ENDPOINTS":   "true",
		"FLINT_ALLOWED_ORIGINS":            "https://app.azra.health, https://admin.azra.health",
		"FLINT_TENANT_HEADER":              "X-Azra-Tenant-Id",
		"FLINT_REQUIRE_TENANT_HEADER":      "true",
		"FLINT_LOCK_REFERENTIAL_INTEGRITY": "true",
		"FLINT_LOCAL_REFERENCES_ONLY":      "true",
		"FLINT_MAX_BUNDLE_BYTES":           "1048576",
		"FLINT_MAX_BUNDLE_ENTRIES":         "50",
		"FLINT_DENIED_RESOURCE_TYPES":      "MedicationKnowledge,TerminologyCapabilities",
		"FLINT_RATE_LIMIT_RPS":             "25",
		"FLINT_RATE_LIMIT_BURST":           "50",
		"FLINT_IDEMPOTENCY_TTL":            "12h",
	}
	getenv := func(k string) string { return env[k] }
	require.NoError(t, configureAuth(srv, logger, getenv))
	require.NoError(t, configureSecurityPolicy(srv, logger, getenv))
	assert.True(t, srv.HasTokenValidator())

	pol := srv.SecurityPolicy()
	assert.True(t, pol.RequireAuth)
	assert.True(t, pol.RequireTenantClaim)
	assert.True(t, pol.DisableUnauthenticatedEndpoints)
	assert.Equal(t, []string{"https://app.azra.health", "https://admin.azra.health"}, pol.AllowedOrigins)
	assert.Equal(t, "X-Azra-Tenant-Id", pol.TenantHeaderName)
	assert.True(t, pol.RequireTenantHeader)
	assert.True(t, pol.LockReferentialIntegrity)
	assert.True(t, pol.EnforceLocalReferencesOnly)
	assert.Equal(t, int64(1048576), pol.MaxBundleBytes)
	assert.Equal(t, 50, pol.MaxBundleEntries)
	assert.Equal(t, []string{"MedicationKnowledge", "TerminologyCapabilities"}, pol.DeniedResourceTypes)
	assert.Equal(t, 25.0, pol.RateLimitPerSecond)
	assert.Equal(t, 50, pol.RateLimitBurst)
	assert.Equal(t, "12h0m0s", pol.IdempotencyTTL.String())

	// 3. Invalid env values fail fast cleanly
	badSrv := server.New(nil, logger)
	require.ErrorContains(t, configureSecurityPolicy(badSrv, logger, func(k string) string {
		if k == "FLINT_MAX_BUNDLE_BYTES" {
			return "not-a-number"
		}
		return ""
	}), "FLINT_MAX_BUNDLE_BYTES")
	require.ErrorContains(t, configureSecurityPolicy(badSrv, logger, func(k string) string {
		if k == "FLINT_IDEMPOTENCY_TTL" {
			return "bad-duration"
		}
		return ""
	}), "FLINT_IDEMPOTENCY_TTL")
}
