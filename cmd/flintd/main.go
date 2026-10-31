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
	"github.com/flint-fhir/flint/store/postgres"
)

var version = "dev"

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

func parseFlags(args []string, errOut io.Writer) (showVersion bool, tenantIsolationFlag string, err error) {
	fs := flag.NewFlagSet("flintd", flag.ContinueOnError)
	if errOut != nil {
		fs.SetOutput(errOut)
	} else {
		fs.SetOutput(io.Discard)
	}
	fs.BoolVar(&showVersion, "version", false, "Print flintd version and exit")
	fs.StringVar(&tenantIsolationFlag, "tenant-isolation", "", "Multi-tenant PostgreSQL isolation mode: 'schema' (default, schema-per-tenant) or 'shared' (shared public tables)")
	if err := fs.Parse(args); err != nil {
		return false, "", err
	}
	return showVersion, tenantIsolationFlag, nil
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

func run(
	args []string,
	getenv func(string) string,
	out io.Writer,
	logger *slog.Logger,
	openDB func(dsn string) (*sql.DB, error),
	serve func(addr string, handler http.Handler) error,
) error {
	showVersion, tenantIsolationFlag, err := parseFlags(args, out)
	if err != nil {
		return err
	}
	if showVersion {
		_, _ = fmt.Fprintf(out, "flintd %s\n", version)
		return nil
	}

	rawIsolation := strings.TrimSpace(tenantIsolationFlag)
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
	store := postgres.New(db, storeOpts...)
	logger.Info("PostgreSQL store initialized", "tenant_isolation", string(isolationMode))
	srv := server.New(store, logger)

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
