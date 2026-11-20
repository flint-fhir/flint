// Command flint-mcp runs the SMART-on-FHIR v2 authorized Flint Model Context Protocol
// (MCP) server over newline-delimited JSON-RPC 2.0 stdio transport.
//
// Usage:
//
//	# Embedded SQLite WAL mode scoped to a single patient's vital signs:
//	flint-mcp --store=sqlite --sqlite-dsn=flint.db --tenant=demo \
//	  --patient=pat-alice-001 --scopes="patient/Patient.rs patient/Observation.rs?category=http://terminology.hl7.org/CodeSystem/observation-category|vital-signs"
//
//	# Production PostgreSQL + OIDC SMART JWT mode:
//	FLINT_POSTGRES_DSN="postgres://flint:flint@localhost:5432/flint" \
//	FLINT_OIDC_ISSUER="https://idp.example.org" \
//	FLINT_MCP_TOKEN="<bearer-jwt>" \
//	  flint-mcp --store=postgres --tenant=demo
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

var version = server.Version

type mcpCLIOptions struct {
	showVersion bool
	storeName   string
	sqliteDSN   string
	tenant      string
	token       string
	scopes      string
	patientID   string
	subject     string
	requireAuth bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr, logger, sql.Open); err != nil {
		logger.Error("flint-mcp terminated with error", "error", err)
		os.Exit(1)
	}
}

func parseMCPFlags(args []string, errOut io.Writer) (mcpCLIOptions, error) {
	var opts mcpCLIOptions
	fs := flag.NewFlagSet("flint-mcp", flag.ContinueOnError)
	if errOut != nil {
		fs.SetOutput(errOut)
	} else {
		fs.SetOutput(io.Discard)
	}
	fs.BoolVar(&opts.showVersion, "version", false, "Print flint-mcp version and exit")
	fs.StringVar(&opts.storeName, "store", "", "Operational store backend: 'sqlite', 'postgres', or 'doris'")
	fs.StringVar(&opts.sqliteDSN, "sqlite-dsn", "", "SQLite file path or ':memory:' when --store=sqlite")
	fs.StringVar(&opts.tenant, "tenant", "", "Default FHIR tenant ID (default 'default')")
	fs.StringVar(&opts.token, "token", "", "Bearer SMART/OIDC access token for authenticating stdio MCP calls")
	fs.StringVar(&opts.scopes, "scopes", "", "Space-separated SMART v2 scopes when running local scoped stdio sessions")
	fs.StringVar(&opts.patientID, "patient", "", "Optional SMART patient compartment ID (e.g. pat-alice-001)")
	fs.StringVar(&opts.subject, "subject", "", "Actor subject identifier recorded in AuditEvents (default 'mcp-stdio-agent')")
	fs.BoolVar(&opts.requireAuth, "require-auth", false, "Require a valid SMART Bearer token or explicit SMART scopes")
	if err := fs.Parse(args); err != nil {
		return mcpCLIOptions{}, err
	}
	return opts, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func initStoreBackend(
	ctx context.Context,
	opts mcpCLIOptions,
	getenv func(string) string,
	openDB func(driverName, dsn string) (*sql.DB, error),
) (store.Store, func(), error) {
	backend := strings.ToLower(firstNonEmpty(opts.storeName, getenv("FLINT_STORE_BACKEND")))
	if backend == "" {
		if strings.TrimSpace(getenv("FLINT_POSTGRES_DSN")) != "" {
			backend = "postgres"
		} else {
			backend = "sqlite"
		}
	}

	switch backend {
	case "sqlite":
		dsn := firstNonEmpty(opts.sqliteDSN, getenv("FLINT_SQLITE_DSN"), ":memory:")
		journalMode, err := sqlite.ParseJournalMode(getenv("FLINT_SQLITE_JOURNAL_MODE"))
		if err != nil {
			return nil, nil, err
		}
		if dsn == ":memory:" {
			journalMode = sqlite.JournalMemory
		}
		db, err := openDB("sqlite", dsn)
		if err != nil {
			return nil, nil, fmt.Errorf("open sqlite: %w", err)
		}
		db.SetMaxOpenConns(1)
		if err := sqlite.RunMigrations(ctx, db, journalMode); err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("sqlite migrations: %w", err)
		}
		st := sqlite.New(db, sqlite.WithJournalMode(journalMode), sqlite.WithDSN(dsn))
		if dsn != ":memory:" {
			if err := st.LoadFromDB(ctx); err != nil {
				_ = db.Close()
				return nil, nil, fmt.Errorf("load sqlite state: %w", err)
			}
		}
		return st, func() { _ = db.Close() }, nil

	case "postgres":
		pgDSN := strings.TrimSpace(getenv("FLINT_POSTGRES_DSN"))
		if pgDSN == "" {
			return nil, nil, errors.New("FLINT_POSTGRES_DSN is required when --store=postgres")
		}
		isolation, err := postgres.ParseTenantIsolationMode(getenv("FLINT_TENANT_ISOLATION"))
		if err != nil {
			return nil, nil, err
		}
		db, err := openDB("postgres", pgDSN)
		if err != nil {
			return nil, nil, fmt.Errorf("open postgres: %w", err)
		}
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("ping postgres: %w", err)
		}
		if err := postgres.RunMigrations(ctx, db); err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("postgres migrations: %w", err)
		}
		st := postgres.New(db, postgres.WithTenantIsolation(isolation))
		return st, func() { _ = db.Close() }, nil

	case "doris":
		groupCommit, err := doris.ParseGroupCommitMode(getenv("FLINT_DORIS_GROUP_COMMIT"))
		if err != nil {
			return nil, nil, err
		}
		rawIso := strings.TrimSpace(getenv("FLINT_DORIS_TENANT_ISOLATION"))
		if rawIso == "" {
			rawIso = strings.TrimSpace(getenv("FLINT_TENANT_ISOLATION"))
		}
		isolation, err := doris.ParseTenantIsolationMode(rawIso)
		if err != nil {
			return nil, nil, err
		}
		repNum := 1
		if rawRep := strings.TrimSpace(getenv("FLINT_DORIS_REPLICATION_NUM")); rawRep != "" {
			parsedRep, err := strconv.Atoi(rawRep)
			if err != nil || parsedRep < 1 {
				return nil, nil, fmt.Errorf("invalid FLINT_DORIS_REPLICATION_NUM %q: must be >= 1", rawRep)
			}
			repNum = parsedRep
		}
		requireNonRoot := false
		if rawNonRoot := strings.TrimSpace(getenv("FLINT_DORIS_REQUIRE_NON_ROOT")); rawNonRoot != "" {
			parsedNR, err := strconv.ParseBool(rawNonRoot)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid FLINT_DORIS_REQUIRE_NON_ROOT %q: %w", rawNonRoot, err)
			}
			requireNonRoot = parsedNR
		}
		dbName := strings.TrimSpace(getenv("FLINT_DORIS_DATABASE"))
		var (
			db                     *sql.DB
			sessionGroupConfigured bool
		)
		if dsn := strings.TrimSpace(getenv("FLINT_DORIS_DSN")); dsn != "" {
			normalizedDSN, err := doris.NormalizeDSN(dsn, doris.DSNOptions{
				DefaultDatabase: dbName,
				GroupCommit:     groupCommit,
				RequireNonRoot:  requireNonRoot,
			})
			if err != nil {
				return nil, nil, err
			}
			db, err = openDB("mysql", normalizedDSN)
			if err != nil {
				return nil, nil, fmt.Errorf("open doris: %w", err)
			}
			doris.ConfigurePool(db, 0, 0, 0, 0)
			if err := doris.RunMigrationsWithOptions(ctx, db, doris.MigrationOptions{
				Database:        dbName,
				TenantIsolation: isolation,
				ReplicationNum:  repNum,
			}); err != nil {
				_ = db.Close()
				return nil, nil, fmt.Errorf("doris migrations: %w", err)
			}
			sessionGroupConfigured = true
		}
		st := doris.New(
			db,
			doris.WithGroupCommit(groupCommit),
			doris.WithDatabase(dbName),
			doris.WithTenantIsolation(isolation),
			doris.WithAutoProvision(isolation == doris.TenantIsolationDatabase),
			doris.WithReplicationNum(repNum),
			doris.WithSessionGroupCommitConfigured(sessionGroupConfigured),
		)
		cleanup := func() {
			if db != nil {
				_ = db.Close()
			}
		}
		return st, cleanup, nil

	default:
		return nil, nil, fmt.Errorf("unsupported --store=%q (expected sqlite, postgres, or doris)", backend)
	}
}

func run(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	in io.Reader,
	out io.Writer,
	errOut io.Writer,
	logger *slog.Logger,
	openDB func(driverName, dsn string) (*sql.DB, error),
) error {
	opts, err := parseMCPFlags(args, errOut)
	if err != nil {
		return err
	}
	if opts.showVersion {
		_, err := fmt.Fprintf(out, "flint-mcp %s\n", version)
		return err
	}

	st, cleanup, err := initStoreBackend(ctx, opts, getenv, openDB)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := server.New(st, logger)
	defer func() { _ = srv.Close() }()
	server.RegisterDefaultResourceTypes(srv)
	srv.SetAuditRecorder(audit.NewStoreRecorder(st))

	oidcIssuer := strings.TrimSpace(getenv("FLINT_OIDC_ISSUER"))
	if oidcIssuer != "" {
		oidcAud := strings.TrimSpace(getenv("FLINT_OIDC_AUDIENCE"))
		if strings.EqualFold(strings.TrimSpace(getenv("FLINT_AUTH_PROVIDER")), "zitadel") {
			zv, zErr := auth.NewZitadelValidator(auth.ZitadelConfig{
				OIDCConfig: auth.OIDCConfig{Issuer: oidcIssuer, Audience: oidcAud},
			})
			if zErr != nil {
				return fmt.Errorf("init zitadel validator: %w", zErr)
			}
			srv.SetTokenValidator(zv)
		} else {
			ov, oErr := auth.NewOIDCValidator(auth.OIDCConfig{Issuer: oidcIssuer, Audience: oidcAud})
			if oErr != nil {
				return fmt.Errorf("init oidc validator: %w", oErr)
			}
			srv.SetTokenValidator(ov)
		}
	}

	tenant := firstNonEmpty(opts.tenant, getenv("FLINT_TENANT"), "default")
	rawToken := firstNonEmpty(opts.token, getenv("FLINT_MCP_TOKEN"), getenv("FLINT_TOKEN"))
	rawScopes := firstNonEmpty(opts.scopes, getenv("FLINT_MCP_SCOPES"))
	requireAuth := opts.requireAuth || strings.EqualFold(strings.TrimSpace(getenv("FLINT_MCP_REQUIRE_AUTH")), "true")

	if rawToken != "" {
		if oidcIssuer == "" {
			srv.EnableDevTokenEnforcement()
		}
		secCtx, vErr := srv.ValidateBearerToken(ctx, rawToken)
		if vErr != nil {
			return fmt.Errorf("invalid SMART bearer token: %w", vErr)
		}
		ctx = auth.WithSecurityContext(ctx, secCtx)
	} else if rawScopes != "" {
		if oidcIssuer == "" {
			srv.EnableDevTokenEnforcement()
		}
		parsedScopes := auth.ParseScopes(rawScopes)
		if len(parsedScopes) == 0 {
			return fmt.Errorf("no valid SMART scopes parsed from %q", rawScopes)
		}
		subj := firstNonEmpty(opts.subject, getenv("FLINT_MCP_SUBJECT"), "mcp-stdio-agent")
		patID := firstNonEmpty(opts.patientID, getenv("FLINT_MCP_PATIENT_ID"))
		secCtx := &authv1.SecurityContext{
			Subject:   subj,
			TenantId:  tenant,
			PatientId: patID,
			Scopes:    parsedScopes,
		}
		ctx = auth.WithSecurityContext(ctx, secCtx)
	} else if requireAuth {
		return errors.New("--require-auth is enabled, but neither --token nor --scopes was supplied")
	}

	return srv.MCPServer().ServeStdio(ctx, tenant, in, out)
}
