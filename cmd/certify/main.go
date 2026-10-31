// Command certify executes the automated HL7 FHIR R4, US Core v6.1.0 (USCDI v3),
// and ONC §170.315(g)(10) Standardized API certification test suite against a
// running Flint server (or an ephemeral in-process Flint server backed by Postgres),
// prints the Certification Scorecard, and optionally writes a FHIR R4 TestReport JSON.
//
// Usage:
//
//	go run ./cmd/certify -tenant certify -out testreport.json
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/certify"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

type cliOptions struct {
	URL        string
	DSN        string
	Tenant     string
	Token      string
	OutPath    string
	HTTPClient *http.Client
}

func main() {
	opts, err := parseCLIOptions(os.Args[1:], os.Getenv)
	if err != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	code := runCertifyCLI(
		ctx,
		opts,
		func(dsn string) (*sql.DB, error) { return sql.Open("postgres", dsn) },
		os.Stdout,
		os.Stderr,
	)
	if code != 0 {
		os.Exit(code)
	}
}

func parseCLIOptions(args []string, getenv func(string) string) (cliOptions, error) {
	defaultURL := getenv("FLINT_URL")
	defaultDSN := getenv("FLINT_POSTGRES_DSN")
	if defaultDSN == "" {
		defaultDSN = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
	}
	defaultTenant := getenv("FLINT_TENANT")
	if defaultTenant == "" {
		defaultTenant = "certify"
	}

	fs := flag.NewFlagSet("certify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	urlFlag := fs.String("url", defaultURL, "Target Flint server URL (if empty, boots an ephemeral server over -dsn)")
	dsnFlag := fs.String("dsn", defaultDSN, "Postgres DSN for ephemeral server mode")
	tenantFlag := fs.String("tenant", defaultTenant, "Target FHIR tenant ID")
	tokenFlag := fs.String("token", getenv("FLINT_TOKEN"), "Optional SMART on FHIR Bearer token")
	outFlag := fs.String("out", "", "Optional file path to write FHIR R4 TestReport JSON")
	if err := fs.Parse(args); err != nil {
		return cliOptions{}, err
	}

	return cliOptions{
		URL:        strings.TrimSpace(*urlFlag),
		DSN:        *dsnFlag,
		Tenant:     *tenantFlag,
		Token:      *tokenFlag,
		OutPath:    strings.TrimSpace(*outFlag),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func runCertifyCLI(
	ctx context.Context,
	opts cliOptions,
	openDB func(dsn string) (*sql.DB, error),
	stdout, stderr io.Writer,
) int {
	targetURL := strings.TrimSpace(opts.URL)
	var cleanup func()

	if targetURL == "" {
		db, err := openDB(opts.DSN)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "certify failed: open postgres: %v\n", err)
			return 1
		}
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			_, _ = fmt.Fprintf(stderr, "certify failed: ping postgres (%s): %v\n", opts.DSN, err)
			return 1
		}
		if err := postgres.RunMigrations(ctx, db); err != nil {
			db.Close()
			_, _ = fmt.Fprintf(stderr, "certify failed: run migrations: %v\n", err)
			return 1
		}
		st := postgres.New(db)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := server.New(st, logger)
		server.RegisterDefaultResourceTypes(srv)
		srv.SetAuditRecorder(audit.NewStoreRecorder(st))

		ts := httptest.NewServer(srv.Handler())
		targetURL = ts.URL
		cleanup = func() {
			ts.Close()
			_ = postgres.PurgeTenant(context.Background(), db, opts.Tenant)
			db.Close()
		}
	}
	if cleanup != nil {
		defer cleanup()
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	rep, err := certify.Run(ctx, certify.Config{
		BaseURL:     targetURL,
		Tenant:      opts.Tenant,
		BearerToken: opts.Token,
		HTTPClient:  client,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "certify failed: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprint(stdout, rep.FormatScorecard())

	if outPath := strings.TrimSpace(opts.OutPath); outPath != "" {
		raw, err := json.MarshalIndent(rep.ToFHIRTestReport(), "", "  ")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "marshal TestReport: %v\n", err)
			return 1
		}
		if err := os.WriteFile(outPath, raw, 0o600); err != nil {
			_, _ = fmt.Fprintf(stderr, "write TestReport to %s: %v\n", outPath, err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "\n📄 Wrote FHIR R4 TestReport JSON to %s\n", outPath)
	}

	if rep.Result != "pass" {
		return 1
	}
	return 0
}
