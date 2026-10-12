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

func main() {
	defaultURL := os.Getenv("FLINT_URL")
	defaultDSN := os.Getenv("FLINT_POSTGRES_DSN")
	if defaultDSN == "" {
		defaultDSN = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
	}
	defaultTenant := os.Getenv("FLINT_TENANT")
	if defaultTenant == "" {
		defaultTenant = "certify"
	}

	urlFlag := flag.String("url", defaultURL, "Target Flint server URL (if empty, boots an ephemeral server over -dsn)")
	dsnFlag := flag.String("dsn", defaultDSN, "Postgres DSN for ephemeral server mode")
	tenantFlag := flag.String("tenant", defaultTenant, "Target FHIR tenant ID")
	tokenFlag := flag.String("token", os.Getenv("FLINT_TOKEN"), "Optional SMART on FHIR Bearer token")
	outFlag := flag.String("out", "", "Optional file path to write FHIR R4 TestReport JSON")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	targetURL := strings.TrimSpace(*urlFlag)
	var cleanup func()

	if targetURL == "" {
		db, err := sql.Open("postgres", *dsnFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "certify failed: open postgres: %v\n", err)
			os.Exit(1)
		}
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			fmt.Fprintf(os.Stderr, "certify failed: ping postgres (%s): %v\n", *dsnFlag, err)
			os.Exit(1)
		}
		if err := postgres.RunMigrations(ctx, db); err != nil {
			db.Close()
			fmt.Fprintf(os.Stderr, "certify failed: run migrations: %v\n", err)
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
			for _, table := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri", "fhir_audit_event", "fhir_resource_history", "fhir_resource"} {
				_, _ = db.ExecContext(context.Background(), "DELETE FROM "+table+" WHERE tenant_id = $1", *tenantFlag)
			}
			db.Close()
		}
	}
	if cleanup != nil {
		defer cleanup()
	}

	rep, err := certify.Run(ctx, certify.Config{
		BaseURL:     targetURL,
		Tenant:      *tenantFlag,
		BearerToken: *tokenFlag,
		HTTPClient:  &http.Client{Timeout: 30 * time.Second},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "certify failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(rep.FormatScorecard())

	if outPath := strings.TrimSpace(*outFlag); outPath != "" {
		raw, err := json.MarshalIndent(rep.ToFHIRTestReport(), "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal TestReport: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(outPath, raw, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write TestReport to %s: %v\n", outPath, err)
			os.Exit(1)
		}
		fmt.Printf("\n📄 Wrote FHIR R4 TestReport JSON to %s\n", outPath)
	}

	if rep.Result != "pass" {
		os.Exit(1)
	}
}
