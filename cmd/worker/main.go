// Package main runs the Flint Temporal worker.
// It connects to Temporal, Postgres, and AutoMQ, registers workflows and activities,
// and starts processing IngestFHIRBundle workflows.
package main

import (
	"context"
	"database/sql"
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
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

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
	store := postgres.New(db)
	logger.Info("connected to postgres")

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
			schemaRegistryURL := firstEnvOrDefault([]string{"FLINT_SCHEMA_REGISTRY_URL", "SCHEMA_REGISTRY_URL"}, "")
			producer, err = automq.NewProducer(automq.Config{
				Brokers:           brokers,
				TopicPrefix:       firstEnvOrDefault([]string{"FLINT_AUTOMQ_TOPIC_PREFIX", "AUTOMQ_TOPIC_PREFIX"}, "fhir."),
				SchemaRegistryURL: schemaRegistryURL,
				Logger:            logger,
			})
			if err != nil {
				log.Fatalf("automq connect %v: %v", brokers, err)
			}
			defer producer.Close()
			logger.Info("connected to automq", "brokers", brokers, "schemaRegistryURL", schemaRegistryURL)
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
