// Package main runs the Flint Temporal worker.
// It connects to Temporal, Postgres, and AutoMQ, registers workflows and activities,
// and starts processing IngestFHIRBundle workflows.
package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"log/slog"
	"os"
	"strings"

	_ "github.com/lib/pq"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/flint-fhir/flint/automq"
	"github.com/flint-fhir/flint/ingest/activity"
	"github.com/flint-fhir/flint/ingest/workflow"
	"github.com/flint-fhir/flint/store/postgres"
)

func main() {
	var tenantIsolationFlag string
	flag.StringVar(&tenantIsolationFlag, "tenant-isolation", "", "Multi-tenant PostgreSQL isolation mode: 'schema' (default, schema-per-tenant) or 'shared' (shared public tables)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	rawIsolation := strings.TrimSpace(tenantIsolationFlag)
	if rawIsolation == "" {
		rawIsolation = strings.TrimSpace(os.Getenv("FLINT_TENANT_ISOLATION"))
	}
	isolationMode, err := postgres.ParseTenantIsolationMode(rawIsolation)
	if err != nil {
		log.Fatalf("invalid tenant isolation mode: %v", err)
	}

	// Connect to Temporal
	temporalAddr := firstEnvOrDefault([]string{"FLINT_TEMPORAL_ADDR", "TEMPORAL_ADDRESS"}, "localhost:7233")
	c, err := client.Dial(client.Options{HostPort: temporalAddr})
	if err != nil {
		log.Fatalf("temporal connect %s: %v", temporalAddr, err)
	}
	defer c.Close()
	logger.Info("connected to temporal", "addr", temporalAddr)

	// Connect to Postgres and run migrations
	pgDSN := firstEnvOrDefault([]string{"FLINT_POSTGRES_DSN"}, "postgres://flint:flint@localhost:5432/flint?sslmode=disable")
	db, err := sql.Open("postgres", pgDSN)
	if err != nil {
		log.Fatalf("postgres open: %v", err)
	}
	defer db.Close()
	if err := postgres.RunMigrations(context.Background(), db); err != nil {
		log.Fatalf("postgres migrations: %v", err)
	}
	storeOpts := []postgres.Option{
		postgres.WithTenantIsolation(isolationMode),
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("FLINT_TENANT_ROLE_SWITCH")), "true") {
		storeOpts = append(storeOpts, postgres.WithTenantRoleSwitch(true))
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("FLINT_TENANT_AUTO_PROVISION")), "false") {
		storeOpts = append(storeOpts, postgres.WithAutoProvision(false))
	}
	store := postgres.New(db, storeOpts...)
	logger.Info("connected to postgres", "tenant_isolation", string(isolationMode))

	// Connect to AutoMQ (optional)
	var producer *automq.Producer
	rawBrokers := firstEnvOrDefault([]string{"FLINT_AUTOMQ_BROKERS", "AUTOMQ_BROKERS"}, "")
	if rawBrokers != "" {
		var brokers []string
		for b := range strings.SplitSeq(rawBrokers, ",") {
			if trimmed := strings.TrimSpace(b); trimmed != "" {
				brokers = append(brokers, trimmed)
			}
		}
		if len(brokers) > 0 {
			producer, err = automq.NewProducer(automq.Config{
				Brokers:     brokers,
				TopicPrefix: firstEnvOrDefault([]string{"FLINT_AUTOMQ_TOPIC_PREFIX", "AUTOMQ_TOPIC_PREFIX"}, "fhir."),
				Logger:      logger,
			})
			if err != nil {
				log.Fatalf("automq connect %v: %v", brokers, err)
			}
			defer producer.Close()
			logger.Info("connected to automq", "brokers", brokers)
		}
	} else {
		logger.Warn("AUTOMQ_BROKERS not set, AutoMQ publishing disabled")
	}

	// Create activities with all dependencies (all 146 FHIR R4 resource types indexed)
	activities := &activity.Activities{
		Store:           store,
		Producer:        producer,
		Logger:          logger,
		IndexExtractors: postgres.DefaultIndexExtractors(),
	}

	// Create and start worker
	w := worker.New(c, workflow.TaskQueue, worker.Options{})
	w.RegisterWorkflow(workflow.IngestFHIRBundle)
	w.RegisterActivity(activities)

	logger.Info("starting flint worker", "taskQueue", workflow.TaskQueue)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker run: %v", err)
	}
}

func firstEnvOrDefault(keys []string, def string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return def
}
