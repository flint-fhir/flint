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
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "Print flintd version and exit")
	flag.Parse()
	if showVersion {
		fmt.Printf("flintd %s\n", version)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dsn := os.Getenv("FLINT_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		logger.Error("open db", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		logger.Error("ping db", "error", err)
		os.Exit(1)
	}

	if err := postgres.RunMigrations(context.Background(), db); err != nil {
		logger.Error("run migrations", "error", err)
		os.Exit(1)
	}

	var storeOpts []postgres.Option
	if strings.EqualFold(strings.TrimSpace(os.Getenv("FLINT_TENANT_ROLE_SWITCH")), "true") {
		storeOpts = append(storeOpts, postgres.WithTenantRoleSwitch(true))
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("FLINT_TENANT_AUTO_PROVISION")), "false") {
		storeOpts = append(storeOpts, postgres.WithAutoProvision(false))
	}
	store := postgres.New(db, storeOpts...)
	srv := server.New(store, logger)

	// Register all supported US Core / USCDI v3 FHIR R4 resource types
	server.RegisterDefaultResourceTypes(srv)

	// Optional AutoMQ / Kafka CDC producer configuration
	if rawBrokers := strings.TrimSpace(os.Getenv("FLINT_AUTOMQ_BROKERS")); rawBrokers != "" {
		var brokers []string
		for b := range strings.SplitSeq(rawBrokers, ",") {
			if trimmed := strings.TrimSpace(b); trimmed != "" {
				brokers = append(brokers, trimmed)
			}
		}
		if len(brokers) > 0 {
			schemaRegistryURL := strings.TrimSpace(os.Getenv("FLINT_SCHEMA_REGISTRY_URL"))
			cdcProducer, err := automq.NewProducer(automq.Config{
				Brokers:           brokers,
				TopicPrefix:       "fhir.",
				SchemaRegistryURL: schemaRegistryURL,
				Logger:            logger,
			})
			if err != nil {
				logger.Error("init automq cdc producer", "error", err)
				os.Exit(1)
			}
			defer cdcProducer.Close()
			srv.SetCDCPublisher(cdcProducer)
			logger.Info("AutoMQ CDC streaming enabled", "brokers", brokers, "schemaRegistryURL", schemaRegistryURL)
		}
	}

	// Optional SMART on FHIR Auth & OIDC/Zitadel configuration
	if oidcIssuer := os.Getenv("FLINT_OIDC_ISSUER"); oidcIssuer != "" {
		oidcAudience := os.Getenv("FLINT_OIDC_AUDIENCE")
		idpProvider := os.Getenv("FLINT_AUTH_PROVIDER") // "zitadel" or "oidc"

		var validator auth.TokenValidator
		if idpProvider == "zitadel" {
			zitadelVal, err := auth.NewZitadelValidator(auth.ZitadelConfig{
				OIDCConfig: auth.OIDCConfig{
					Issuer:   oidcIssuer,
					Audience: oidcAudience,
				},
				TenantFromOrgDomain: os.Getenv("FLINT_ZITADEL_TENANT_FROM_DOMAIN") == "true",
			})
			if err != nil {
				logger.Error("init zitadel validator", "error", err)
				os.Exit(1)
			}
			validator = zitadelVal
			logger.Info("SMART on FHIR auth enabled (provider: zitadel)", "issuer", oidcIssuer)
		} else {
			oidcVal, err := auth.NewOIDCValidator(auth.OIDCConfig{
				Issuer:   oidcIssuer,
				Audience: oidcAudience,
			})
			if err != nil {
				logger.Error("init oidc validator", "error", err)
				os.Exit(1)
			}
			validator = oidcVal
			logger.Info("SMART on FHIR auth enabled (provider: oidc)", "issuer", oidcIssuer)
		}

		srv.SetTokenValidator(validator)

		authURL := os.Getenv("FLINT_SMART_AUTH_URL")
		tokenURL := os.Getenv("FLINT_SMART_TOKEN_URL")
		if authURL != "" && tokenURL != "" {
			srv.SetSMARTConfig(server.SMARTConfig{
				Issuer:                oidcIssuer,
				AuthorizationEndpoint: authURL,
				TokenEndpoint:         tokenURL,
			})
		}
	}

	addr := os.Getenv("FLINT_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	logger.Info("starting flint", "addr", addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		logger.Error("server", "error", err)
		os.Exit(1)
	}
}
