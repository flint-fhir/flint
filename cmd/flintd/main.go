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

	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"

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
