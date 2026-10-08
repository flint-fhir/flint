// Flint FHIR Server
//
// Usage:
//
//	FLINT_POSTGRES_DSN="postgres://flint:flint@localhost:5432/flint?sslmode=disable" \
//	  go run ./cmd/flintd
package main

import (
	"database/sql"
	"log/slog"
	"net/http"
	"os"

	_ "github.com/lib/pq"

	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

func main() {
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

	store := postgres.New(db)
	srv := server.New(store, logger)

	// Register known FHIR resource types
	srv.RegisterResourceType("Patient", &patpb.Patient{})
	srv.RegisterResourceType("Condition", &condpb.Condition{})
	srv.RegisterResourceType("Encounter", &encpb.Encounter{})
	srv.RegisterResourceType("Observation", &obspb.Observation{})
	srv.RegisterResourceType("Practitioner", &pracpb.Practitioner{})

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
