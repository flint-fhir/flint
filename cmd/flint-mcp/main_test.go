package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/auth"
)

type mcpStubDriver struct {
	pingErr error
	execErr error
}

type mcpStubConn struct {
	pingErr error
	execErr error
}

type mcpStubTx struct{}
type mcpStubRows struct{}

func (r *mcpStubRows) Columns() []string           { return nil }
func (r *mcpStubRows) Close() error                { return nil }
func (r *mcpStubRows) Next(_ []driver.Value) error { return io.EOF }

func (d *mcpStubDriver) Open(_ string) (driver.Conn, error) {
	return &mcpStubConn{pingErr: d.pingErr, execErr: d.execErr}, nil
}

func (c *mcpStubConn) Prepare(_ string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *mcpStubConn) Close() error                          { return nil }
func (c *mcpStubConn) Begin() (driver.Tx, error)             { return &mcpStubTx{}, nil }
func (c *mcpStubConn) Ping(_ context.Context) error          { return c.pingErr }
func (c *mcpStubConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if c.execErr != nil {
		return nil, c.execErr
	}
	return driver.RowsAffected(1), nil
}
func (c *mcpStubConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	return &mcpStubRows{}, nil
}

func (t *mcpStubTx) Commit() error   { return nil }
func (t *mcpStubTx) Rollback() error { return nil }

var registerMCPStubOnce sync.Once

func ensureMCPStubDrivers() {
	registerMCPStubOnce.Do(func() {
		sql.Register("flint_mcp_stub_ok", &mcpStubDriver{})
		sql.Register("flint_mcp_stub_ping_err", &mcpStubDriver{pingErr: errors.New("ping refused")})
		sql.Register("flint_mcp_stub_mig_err", &mcpStubDriver{execErr: errors.New("migration ddl failed")})
	})
}

func TestFlintMCP_VersionAndFlagErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var out, errOut bytes.Buffer

	_, err := parseMCPFlags([]string{"--version"}, nil)
	require.NoError(t, err)

	err = run(t.Context(), []string{"--version"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut, logger, sql.Open)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "flint-mcp")

	err = run(t.Context(), []string{"--unknown-flag"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut, logger, sql.Open)
	require.Error(t, err)

	err = run(t.Context(), []string{"--store=invalid"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut, logger, sql.Open)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported --store")

	err = run(t.Context(), []string{"--store=postgres"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut, logger, sql.Open)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FLINT_POSTGRES_DSN")

	err = run(t.Context(), []string{"--require-auth"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut, logger, sql.Open)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--require-auth")

	err = run(t.Context(), []string{"--scopes=not-a-valid-smart-scope"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut, logger, sql.Open)
	require.Error(t, err)

	err = run(t.Context(), []string{"--token=invalid.jwt.token"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut, logger, sql.Open)
	require.Error(t, err)
}

func TestFlintMCP_StdioEndToEndWithScopedAuthAndFileSQLite(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dbPath := filepath.Join(t.TempDir(), "mcp-cli.db")

	// Phase 1: Seed Patient/pat-alice-001, Patient/pat-bob-002, and vital-signs + social-history Observations via unrestricted system/*.cruds stdio session
	seedInput := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_or_update_resource","arguments":{"resource":{"resourceType":"Patient","id":"pat-alice-001","name":[{"family":"Smith","given":["Alice"]}]}}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_or_update_resource","arguments":{"resource":{"resourceType":"Patient","id":"pat-bob-002","name":[{"family":"Jones","given":["Bob"]}]}}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"create_or_update_resource","arguments":{"resource":{"resourceType":"Observation","id":"obs-alice-hr","status":"final","category":[{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/observation-category","code":"vital-signs"}]}],"code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/pat-alice-001"},"valueQuantity":{"value":72,"unit":"/min"}}}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"create_or_update_resource","arguments":{"resource":{"resourceType":"Observation","id":"obs-alice-soc","status":"final","category":[{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/observation-category","code":"social-history"}]}],"code":{"coding":[{"system":"http://loinc.org","code":"72166-2"}]},"subject":{"reference":"Patient/pat-alice-001"}}}}}`,
	}, "\n") + "\n"

	var seedOut, errOut bytes.Buffer
	err := run(
		context.Background(),
		[]string{"--store=sqlite", "--sqlite-dsn=" + dbPath, "--tenant=demo", "--scopes=system/*.cruds"},
		func(string) string { return "" },
		strings.NewReader(seedInput),
		&seedOut,
		&errOut,
		logger,
		sql.Open,
	)
	require.NoError(t, err)
	assert.Empty(t, errOut.String(), "stderr must stay clean on normal stdio execution")
	seedLines := strings.Split(strings.TrimSpace(seedOut.String()), "\n")
	require.Len(t, seedLines, 5)

	// Phase 2: Re-open file-backed SQLite with narrow granular patient scope:
	// patient=pat-alice-001, scopes="patient/Patient.rs patient/Observation.rs?category=http://terminology.hl7.org/CodeSystem/observation-category|vital-signs"
	scopedInput := strings.Join([]string{
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"fhir_search","arguments":{"resourceType":"Observation"}}}`,
		`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"fhir_read","arguments":{"resourceType":"Patient","id":"pat-bob-002"}}}`,
		`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"fhir_read","arguments":{"resourceType":"Observation","id":"obs-alice-soc"}}}`,
	}, "\n") + "\n"

	var scopedOut bytes.Buffer
	err = run(
		context.Background(),
		[]string{
			"--store=sqlite",
			"--sqlite-dsn=" + dbPath,
			"--tenant=demo",
			"--patient=pat-alice-001",
			"--scopes=patient/Patient.rs patient/Observation.rs?category=http://terminology.hl7.org/CodeSystem/observation-category|vital-signs",
		},
		func(string) string { return "" },
		strings.NewReader(scopedInput),
		&scopedOut,
		&errOut,
		logger,
		sql.Open,
	)
	require.NoError(t, err)
	scopedLines := strings.Split(strings.TrimSpace(scopedOut.String()), "\n")
	require.Len(t, scopedLines, 3)

	// Line 0 (id=10): Observation search returns obs-alice-hr and omits obs-alice-soc
	assert.Contains(t, scopedLines[0], "obs-alice-hr")
	assert.NotContains(t, scopedLines[0], "obs-alice-soc")

	// Line 1 (id=11): Cross-patient read of pat-bob-002 fails with -32003 Forbidden
	var resp11 struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(scopedLines[1]), &resp11))
	require.NotNil(t, resp11.Error)
	assert.Equal(t, -32003, resp11.Error.Code)

	// Line 2 (id=12): Out-of-category read of obs-alice-soc fails with -32003 Forbidden
	var resp12 struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(scopedLines[2]), &resp12))
	require.NotNil(t, resp12.Error)
	assert.Equal(t, -32003, resp12.Error.Code)
}

func TestFlintMCP_StoreBackendsAndOIDCTokenAuth(t *testing.T) {
	ensureMCPStubDrivers()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pingInput := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"

	// 1. Postgres backend via FLINT_POSTGRES_DSN auto-detection + error branches
	var pgOut, errOut bytes.Buffer
	err := run(
		context.Background(),
		nil,
		func(k string) string {
			if k == "FLINT_POSTGRES_DSN" {
				return "postgres://stub"
			}
			return ""
		},
		strings.NewReader(pingInput),
		&pgOut,
		&errOut,
		logger,
		func(_, dsn string) (*sql.DB, error) {
			return sql.Open("flint_mcp_stub_ok", dsn)
		},
	)
	require.NoError(t, err)
	assert.Contains(t, pgOut.String(), `"result":{}`)

	// Invalid tenant isolation mode
	err = run(
		context.Background(),
		[]string{"--store=postgres"},
		func(k string) string {
			switch k {
			case "FLINT_POSTGRES_DSN":
				return "postgres://stub"
			case "FLINT_TENANT_ISOLATION":
				return "invalid-mode"
			}
			return ""
		},
		strings.NewReader(""),
		&pgOut,
		&errOut,
		logger,
		sql.Open,
	)
	require.Error(t, err)

	// Postgres open, ping, and migration errors
	err = run(
		context.Background(),
		[]string{"--store=postgres"},
		func(k string) string {
			if k == "FLINT_POSTGRES_DSN" {
				return "postgres://stub"
			}
			return ""
		},
		strings.NewReader(""),
		&pgOut,
		&errOut,
		logger,
		func(_, _ string) (*sql.DB, error) { return nil, errors.New("dial failed") },
	)
	require.ErrorContains(t, err, "open postgres")

	err = run(
		context.Background(),
		[]string{"--store=postgres"},
		func(k string) string {
			if k == "FLINT_POSTGRES_DSN" {
				return "postgres://stub"
			}
			return ""
		},
		strings.NewReader(""),
		&pgOut,
		&errOut,
		logger,
		func(_, dsn string) (*sql.DB, error) { return sql.Open("flint_mcp_stub_ping_err", dsn) },
	)
	require.ErrorContains(t, err, "ping postgres")

	err = run(
		context.Background(),
		[]string{"--store=postgres"},
		func(k string) string {
			if k == "FLINT_POSTGRES_DSN" {
				return "postgres://stub"
			}
			return ""
		},
		strings.NewReader(""),
		&pgOut,
		&errOut,
		logger,
		func(_, dsn string) (*sql.DB, error) { return sql.Open("flint_mcp_stub_mig_err", dsn) },
	)
	require.ErrorContains(t, err, "postgres migrations")

	// 2. Doris backend (in-memory, stub DSN ok, open error, migration error, invalid group commit)
	var dorisOut bytes.Buffer
	err = run(
		context.Background(),
		[]string{"--store=doris", "--tenant=demo"},
		func(k string) string {
			if k == "FLINT_DORIS_DSN" {
				return "doris://stub"
			}
			return ""
		},
		strings.NewReader(pingInput),
		&dorisOut,
		&errOut,
		logger,
		func(_, dsn string) (*sql.DB, error) { return sql.Open("flint_mcp_stub_ok", dsn) },
	)
	require.NoError(t, err)
	assert.Contains(t, dorisOut.String(), `"result":{}`)

	err = run(
		context.Background(),
		[]string{"--store=doris"},
		func(k string) string {
			if k == "FLINT_DORIS_GROUP_COMMIT" {
				return "bad-mode"
			}
			return ""
		},
		strings.NewReader(""),
		&dorisOut,
		&errOut,
		logger,
		sql.Open,
	)
	require.Error(t, err)

	err = run(
		context.Background(),
		[]string{"--store=doris"},
		func(k string) string {
			if k == "FLINT_DORIS_DSN" {
				return "doris://stub"
			}
			return ""
		},
		strings.NewReader(""),
		&dorisOut,
		&errOut,
		logger,
		func(_, _ string) (*sql.DB, error) { return nil, errors.New("doris dial failed") },
	)
	require.ErrorContains(t, err, "open doris")

	err = run(
		context.Background(),
		[]string{"--store=doris"},
		func(k string) string {
			if k == "FLINT_DORIS_DSN" {
				return "doris://stub"
			}
			return ""
		},
		strings.NewReader(""),
		&dorisOut,
		&errOut,
		logger,
		func(_, dsn string) (*sql.DB, error) { return sql.Open("flint_mcp_stub_mig_err", dsn) },
	)
	require.ErrorContains(t, err, "doris migrations")

	// 3. SQLite invalid journal mode & open error
	err = run(
		context.Background(),
		[]string{"--store=sqlite"},
		func(k string) string {
			if k == "FLINT_SQLITE_JOURNAL_MODE" {
				return "bad-journal"
			}
			return ""
		},
		strings.NewReader(""),
		&dorisOut,
		&errOut,
		logger,
		sql.Open,
	)
	require.Error(t, err)

	err = run(
		context.Background(),
		[]string{"--store=sqlite"},
		func(string) string { return "" },
		strings.NewReader(""),
		&dorisOut,
		&errOut,
		logger,
		func(_, _ string) (*sql.DB, error) { return nil, errors.New("sqlite open failed") },
	)
	require.ErrorContains(t, err, "open sqlite")

	// 4. Live OIDC & Zitadel RS256 --token authentication over httptest JWKS server
	var oidcURL string
	var mockAuth *auth.MockTokenValidator
	idpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":   oidcURL,
				"jwks_uri": oidcURL + "/jwks",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(mockAuth.JWKS())
	}))
	defer idpSrv.Close()
	oidcURL = idpSrv.URL

	var mErr error
	mockAuth, mErr = auth.NewMockTokenValidator(oidcURL, "https://fhir.example.org")
	require.NoError(t, mErr)
	jwtTok, tErr := mockAuth.IssueToken("dr-hopper", "user/*.rs", "", time.Hour, map[string]any{"tenant_id": "demo"})
	require.NoError(t, tErr)

	for _, provider := range []string{"oidc", "zitadel"} {
		var tokOut bytes.Buffer
		err = run(
			context.Background(),
			[]string{"--store=sqlite", "--tenant=demo", "--token=" + jwtTok, "--require-auth"},
			func(k string) string {
				switch k {
				case "FLINT_OIDC_ISSUER":
					return oidcURL
				case "FLINT_OIDC_AUDIENCE":
					return "https://fhir.example.org"
				case "FLINT_AUTH_PROVIDER":
					return provider
				}
				return ""
			},
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"),
			&tokOut,
			&errOut,
			logger,
			sql.Open,
		)
		require.NoError(t, err)
		assert.Contains(t, tokOut.String(), "fhir_search")
	}
}
