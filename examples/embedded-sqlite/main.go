// Command embedded-sqlite demonstrates embedding Flint's storage-agnostic FHIR R4
// API server (server.Server) over the pure-Go SQLite WAL backend (store/sqlite)
// inside any standard Go application with CGO_ENABLED=0 and zero external services.
//
// Run ephemeral in-memory:
//
//	CGO_ENABLED=0 go run ./examples/embedded-sqlite
//
// Run backed by a persistent SQLite WAL file:
//
//	CGO_ENABLED=0 go run ./examples/embedded-sqlite -dsn flint-embedded.db -addr 127.0.0.1:8080
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/sqlite"
)

// Config configures an embedded pure-Go Flint FHIR R4 server instance.
type Config struct {
	// DSN is ":memory:" (or empty) for an ephemeral in-memory store, or a file
	// path (e.g. "flint.db") for durable SQLite Write-Ahead Logging (WAL) storage.
	DSN string

	// ReferentialIntegrity sets "lenient" or "strict" reference enforcement.
	// Defaults to server.ReferentialIntegrityStrict when empty.
	ReferentialIntegrity server.ReferentialIntegrityMode

	// Logger receives structured FHIR API & storage telemetry.
	Logger *slog.Logger
}

// NewEmbeddedServer constructs a ready-to-serve Flint FHIR R4 *server.Server
// backed by pure-Go SQLite (modernc.org/sqlite, CGO_ENABLED=0) with all 146
// FHIR R4 resource types, US Core v6.1.0/v7.0.0 & HL7 IPA profiles, and
// SQL-on-FHIR v2 analytical pushdown enabled.
//
// Callers mount srv.Handler() on any standard net/http server or router and
// invoke cleanup() during shutdown.
func NewEmbeddedServer(ctx context.Context, cfg Config) (*server.Server, func() error, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	dsn := strings.TrimSpace(cfg.DSN)
	if dsn == "" {
		dsn = ":memory:"
	}

	mode := cfg.ReferentialIntegrity
	if mode == "" {
		mode = server.ReferentialIntegrityStrict
	}

	var sqliteDB *sql.DB
	if dsn != ":memory:" {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, nil, fmt.Errorf("open sqlite %q: %w", dsn, err)
		}
		db.SetMaxOpenConns(1)
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("ping sqlite %q: %w", dsn, err)
		}
		if err := sqlite.RunMigrations(ctx, db, sqlite.JournalWAL); err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("migrate sqlite %q: %w", dsn, err)
		}
		sqliteDB = db
	}

	sqStore := sqlite.New(
		sqliteDB,
		sqlite.WithJournalMode(sqlite.JournalWAL),
		sqlite.WithDSN(dsn),
	)
	if sqliteDB != nil {
		if err := sqStore.LoadFromDB(ctx); err != nil {
			_ = sqliteDB.Close()
			return nil, nil, fmt.Errorf("load persisted sqlite state %q: %w", dsn, err)
		}
	}

	srv := server.New(sqStore, logger)
	srv.SetAnalyticalStore(sqStore)
	srv.SetReferentialIntegrity(mode)
	server.RegisterDefaultResourceTypes(srv)

	cleanup := func() error {
		if sqliteDB != nil {
			return sqliteDB.Close()
		}
		return nil
	}
	return srv, cleanup, nil
}

func serveHTTP(ctx context.Context, addr string, handler http.Handler, ready chan<- string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	if ready != nil {
		ready <- ln.Addr().String()
	}

	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if serveErr := httpSrv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case serveErr := <-errCh:
		return serveErr
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return <-errCh
	}
}

func run(
	ctx context.Context,
	args []string,
	out io.Writer,
	serve func(ctx context.Context, addr string, handler http.Handler) error,
) error {
	fs := flag.NewFlagSet("embedded-sqlite", flag.ContinueOnError)
	fs.SetOutput(out)

	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	dsn := fs.String("dsn", ":memory:", "SQLite file path or :memory:")
	refModeRaw := fs.String("referential-integrity", string(server.ReferentialIntegrityStrict), "Referential integrity mode (strict|lenient)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	refMode, err := server.ParseReferentialIntegrityMode(*refModeRaw)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv, cleanup, err := NewEmbeddedServer(ctx, Config{
		DSN:                  *dsn,
		ReferentialIntegrity: refMode,
		Logger:               logger,
	})
	if err != nil {
		return err
	}
	defer func() {
		_ = cleanup()
	}()

	_, _ = fmt.Fprintf(out, "Flint embedded FHIR R4 server listening on http://%s (dsn=%s, referential_integrity=%s)\n", *addr, *dsn, refMode)
	_, _ = fmt.Fprintf(out, "  CapabilityStatement: http://%s/fhir/r4/demo/metadata\n", *addr)
	_, _ = fmt.Fprintf(out, "  Interactive Sandbox: http://%s/fhir/r4/demo/$docs\n", *addr)
	_, _ = fmt.Fprintf(out, "  137-Check $certify:  http://%s/fhir/r4/demo/$certify\n", *addr)

	return serve(ctx, *addr, srv.Handler())
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, os.Args[1:], os.Stdout, func(ctx context.Context, addr string, handler http.Handler) error {
		return serveHTTP(ctx, addr, handler, nil)
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "embedded-sqlite error: %v\n", err)
		os.Exit(1)
	}
}
