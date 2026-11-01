// Flint FHIR Server
//
// Usage:
//
//	FLINT_POSTGRES_DSN="postgres://flint:flint@localhost:5432/flint?sslmode=disable" \
//	  go run ./cmd/flintd
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	_ "github.com/lib/pq"

	"github.com/flint-fhir/flint/automq"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

var version = "dev"

type cliOptions struct {
	showVersion      bool
	tenantIsolation  string
	storeBackend     string
	analyticsBackend string
	sqliteDSN        string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	err := run(
		os.Args[1:],
		os.Getenv,
		os.Stdout,
		logger,
		func(dsn string) (*sql.DB, error) { return sql.Open("postgres", dsn) },
		http.ListenAndServe,
	)
	if err != nil {
		logger.Error("flintd failed", "error", err)
		os.Exit(1)
	}
}

func parseCLIOptions(args []string, errOut io.Writer) (cliOptions, error) {
	var opts cliOptions
	fs := flag.NewFlagSet("flintd", flag.ContinueOnError)
	if errOut != nil {
		fs.SetOutput(errOut)
	} else {
		fs.SetOutput(io.Discard)
	}
	fs.BoolVar(&opts.showVersion, "version", false, "Print flintd version and exit")
	fs.StringVar(&opts.tenantIsolation, "tenant-isolation", "", "Multi-tenant PostgreSQL isolation mode: 'schema' (default, schema-per-tenant) or 'shared' (shared public tables)")
	fs.StringVar(&opts.storeBackend, "store", "", "Primary operational store backend: 'postgres' (default), 'doris' (Apache Doris MoW), or 'sqlite' (embedded SQLite WAL)")
	fs.StringVar(&opts.analyticsBackend, "analytics-store", "", "Optional analytical store backend: 'doris', 'sqlite', or 'none' (enables hybrid OLTP + OLAP mode)")
	fs.StringVar(&opts.sqliteDSN, "sqlite-dsn", "", "SQLite file path or URI when --store=sqlite (e.g. 'flint.db' or ':memory:')")
	if err := fs.Parse(args); err != nil {
		return cliOptions{}, err
	}
	return opts, nil
}

func parseFlags(args []string, errOut io.Writer) (showVersion bool, tenantIsolationFlag string, err error) {
	opts, err := parseCLIOptions(args, errOut)
	if err != nil {
		return false, "", err
	}
	return opts.showVersion, opts.tenantIsolation, nil
}

func parseBrokers(rawBrokers string) []string {
	var brokers []string
	for b := range strings.SplitSeq(strings.TrimSpace(rawBrokers), ",") {
		if trimmed := strings.TrimSpace(b); trimmed != "" {
			brokers = append(brokers, trimmed)
		}
	}
	return brokers
}

func buildStoreOptions(isolationMode postgres.TenantIsolationMode, getenv func(string) string) []postgres.Option {
	storeOpts := []postgres.Option{
		postgres.WithTenantIsolation(isolationMode),
	}
	if strings.EqualFold(strings.TrimSpace(getenv("FLINT_TENANT_ROLE_SWITCH")), "true") {
		storeOpts = append(storeOpts, postgres.WithTenantRoleSwitch(true))
	}
	if strings.EqualFold(strings.TrimSpace(getenv("FLINT_TENANT_AUTO_PROVISION")), "false") {
		storeOpts = append(storeOpts, postgres.WithAutoProvision(false))
	}
	return storeOpts
}

func configureAuth(srv *server.Server, logger *slog.Logger, getenv func(string) string) error {
	oidcIssuer := getenv("FLINT_OIDC_ISSUER")
	if oidcIssuer == "" {
		return nil
	}
	oidcAudience := getenv("FLINT_OIDC_AUDIENCE")
	idpProvider := getenv("FLINT_AUTH_PROVIDER") // "zitadel" or "oidc"

	var validator auth.TokenValidator
	if idpProvider == "zitadel" {
		zitadelVal, err := auth.NewZitadelValidator(auth.ZitadelConfig{
			OIDCConfig: auth.OIDCConfig{
				Issuer:   oidcIssuer,
				Audience: oidcAudience,
			},
			TenantFromOrgDomain: getenv("FLINT_ZITADEL_TENANT_FROM_DOMAIN") == "true",
		})
		if err != nil {
			return fmt.Errorf("init zitadel validator: %w", err)
		}
		validator = zitadelVal
		logger.Info("SMART on FHIR auth enabled (provider: zitadel)", "issuer", oidcIssuer)
	} else {
		oidcVal, err := auth.NewOIDCValidator(auth.OIDCConfig{
			Issuer:   oidcIssuer,
			Audience: oidcAudience,
		})
		if err != nil {
			return fmt.Errorf("init oidc validator: %w", err)
		}
		validator = oidcVal
		logger.Info("SMART on FHIR auth enabled (provider: oidc)", "issuer", oidcIssuer)
	}

	srv.SetTokenValidator(validator)

	authURL := getenv("FLINT_SMART_AUTH_URL")
	tokenURL := getenv("FLINT_SMART_TOKEN_URL")
	if authURL != "" && tokenURL != "" {
		srv.SetSMARTConfig(server.SMARTConfig{
			Issuer:                oidcIssuer,
			AuthorizationEndpoint: authURL,
			TokenEndpoint:         tokenURL,
		})
	}
	return nil
}

func openDorisStore(getenv func(string) string, openDB func(dsn string) (*sql.DB, error)) (*doris.Store, *sql.DB, error) {
	groupCommit, err := doris.ParseGroupCommitMode(getenv("FLINT_DORIS_GROUP_COMMIT"))
	if err != nil {
		return nil, nil, err
	}
	var dorisDB *sql.DB
	if dorisDSN := strings.TrimSpace(getenv("FLINT_DORIS_DSN")); dorisDSN != "" {
		dorisDB, err = openDB(dorisDSN)
		if err != nil {
			return nil, nil, fmt.Errorf("open doris db: %w", err)
		}
		if err := dorisDB.Ping(); err != nil {
			_ = dorisDB.Close()
			return nil, nil, fmt.Errorf("ping doris db: %w", err)
		}
		if err := doris.RunMigrations(context.Background(), dorisDB); err != nil {
			_ = dorisDB.Close()
			return nil, nil, fmt.Errorf("run doris migrations: %w", err)
		}
	}
	ds := doris.New(
		dorisDB,
		doris.WithGroupCommit(groupCommit),
		doris.WithDatabase(getenv("FLINT_DORIS_DATABASE")),
	)
	return ds, dorisDB, nil
}

func openSQLiteStore(cliDSN string, getenv func(string) string, openDB func(dsn string) (*sql.DB, error)) (*sqlite.Store, *sql.DB, error) {
	journalMode, err := sqlite.ParseJournalMode(getenv("FLINT_SQLITE_JOURNAL_MODE"))
	if err != nil {
		return nil, nil, err
	}
	sqliteDSN := strings.TrimSpace(cliDSN)
	if sqliteDSN == "" {
		sqliteDSN = strings.TrimSpace(getenv("FLINT_SQLITE_DSN"))
	}
	var sqliteDB *sql.DB
	if sqliteDSN != "" && sqliteDSN != ":memory:" {
		sqliteDB, err = openDB(sqliteDSN)
		if err != nil {
			return nil, nil, fmt.Errorf("open sqlite db: %w", err)
		}
		if err := sqliteDB.Ping(); err != nil {
			_ = sqliteDB.Close()
			return nil, nil, fmt.Errorf("ping sqlite db: %w", err)
		}
		if err := sqlite.RunMigrations(context.Background(), sqliteDB, journalMode); err != nil {
			_ = sqliteDB.Close()
			return nil, nil, fmt.Errorf("run sqlite migrations: %w", err)
		}
	}
	ss := sqlite.New(
		sqliteDB,
		sqlite.WithJournalMode(journalMode),
		sqlite.WithDSN(sqliteDSN),
	)
	return ss, sqliteDB, nil
}

func run(
	args []string,
	getenv func(string) string,
	out io.Writer,
	logger *slog.Logger,
	openDB func(dsn string) (*sql.DB, error),
	serve func(addr string, handler http.Handler) error,
) error {
	cliOpts, err := parseCLIOptions(args, out)
	if err != nil {
		return err
	}
	if cliOpts.showVersion {
		_, _ = fmt.Fprintf(out, "flintd %s\n", version)
		return nil
	}

	rawStore := strings.TrimSpace(cliOpts.storeBackend)
	if rawStore == "" {
		rawStore = strings.TrimSpace(getenv("FLINT_STORE_BACKEND"))
	}
	storeBackend, err := store.ParseBackendType(rawStore)
	if err != nil {
		return fmt.Errorf("invalid store backend: %w", err)
	}

	rawAnalytics := strings.TrimSpace(cliOpts.analyticsBackend)
	if rawAnalytics == "" {
		rawAnalytics = strings.TrimSpace(getenv("FLINT_ANALYTICS_BACKEND"))
	}
	analyticsBackend, err := store.ParseOptionalBackendType(rawAnalytics)
	if err != nil {
		return fmt.Errorf("invalid analytics backend: %w", err)
	}

	var primaryStore store.Store
	var analyticalStore store.AnalyticalStore

	switch storeBackend {
	case store.BackendDoris:
		ds, dorisDB, err := openDorisStore(getenv, openDB)
		if err != nil {
			return err
		}
		if dorisDB != nil {
			defer dorisDB.Close()
		}
		primaryStore = ds
		analyticalStore = ds
		logger.Info("Zero-JSON Apache Doris store initialized", "group_commit", string(ds.GroupCommit()), "database", ds.Database())
	case store.BackendSQLite:
		ss, sqliteDB, err := openSQLiteStore(cliOpts.sqliteDSN, getenv, openDB)
		if err != nil {
			return err
		}
		if sqliteDB != nil {
			defer sqliteDB.Close()
		}
		primaryStore = ss
		analyticalStore = ss
		logger.Info("Embedded Zero-JSON SQLite WAL store initialized", "journal_mode", string(ss.JournalMode()), "dsn", ss.DSN())
	default:
		rawIsolation := strings.TrimSpace(cliOpts.tenantIsolation)
		if rawIsolation == "" {
			rawIsolation = strings.TrimSpace(getenv("FLINT_TENANT_ISOLATION"))
		}
		isolationMode, err := postgres.ParseTenantIsolationMode(rawIsolation)
		if err != nil {
			return fmt.Errorf("invalid tenant isolation mode: %w", err)
		}

		dsn := getenv("FLINT_POSTGRES_DSN")
		if dsn == "" {
			dsn = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
		}

		db, err := openDB(dsn)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer db.Close()

		if err := db.Ping(); err != nil {
			return fmt.Errorf("ping db: %w", err)
		}

		if err := postgres.RunMigrations(context.Background(), db); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}

		storeOpts := buildStoreOptions(isolationMode, getenv)
		pgStore := postgres.New(db, storeOpts...)
		primaryStore = pgStore
		logger.Info("PostgreSQL store initialized", "tenant_isolation", string(isolationMode))

		if analyticsBackend == store.BackendDoris {
			ds, dorisDB, err := openDorisStore(getenv, openDB)
			if err != nil {
				return err
			}
			if dorisDB != nil {
				defer dorisDB.Close()
			}
			analyticalStore = ds
			logger.Info("Hybrid OLTP+OLAP mode enabled: Apache Doris analytical store attached", "group_commit", string(ds.GroupCommit()))
		}
	}

	srv := server.New(primaryStore, logger)
	if analyticalStore != nil {
		srv.SetAnalyticalStore(analyticalStore)
	}

	// Register all supported US Core / USCDI v3 FHIR R4 resource types
	server.RegisterDefaultResourceTypes(srv)

	// Optional AutoMQ / Kafka CDC producer configuration
	if brokers := parseBrokers(getenv("FLINT_AUTOMQ_BROKERS")); len(brokers) > 0 {
		schemaRegistryURL := strings.TrimSpace(getenv("FLINT_SCHEMA_REGISTRY_URL"))
		cdcProducer, err := automq.NewProducer(automq.Config{
			Brokers:           brokers,
			TopicPrefix:       "fhir.",
			SchemaRegistryURL: schemaRegistryURL,
			Logger:            logger,
		})
		if err != nil {
			return fmt.Errorf("init automq cdc producer: %w", err)
		}
		defer cdcProducer.Close()
		srv.SetCDCPublisher(cdcProducer)
		logger.Info("AutoMQ CDC streaming enabled", "brokers", brokers, "schemaRegistryURL", schemaRegistryURL)
	}

	// Optional SMART on FHIR Auth & OIDC/Zitadel configuration
	if err := configureAuth(srv, logger, getenv); err != nil {
		return err
	}

	addr := getenv("FLINT_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	logger.Info("starting flint", "addr", addr)
	if err := serve(addr, srv.Handler()); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}
