// Package main runs the Flint Temporal worker.
// It connects to Temporal, Postgres, and AutoMQ, registers workflows and activities,
// and starts processing IngestFHIRBundle workflows.
package main

import (
	"database/sql"
	"log"
	"log/slog"
	"os"

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
	temporalAddr := envOrDefault("TEMPORAL_ADDRESS", "localhost:7233")
	c, err := client.Dial(client.Options{HostPort: temporalAddr})
	if err != nil {
		log.Fatalf("temporal connect %s: %v", temporalAddr, err)
	}
	defer c.Close()
	logger.Info("connected to temporal", "addr", temporalAddr)

	// Connect to Postgres
	pgDSN := envOrDefault("FLINT_POSTGRES_DSN", "postgres://flint:flint@localhost:5432/flint?sslmode=disable")
	db, err := sql.Open("postgres", pgDSN)
	if err != nil {
		log.Fatalf("postgres open: %v", err)
	}
	defer db.Close()
	store := postgres.New(db)
	logger.Info("connected to postgres")

	// Connect to AutoMQ (optional)
	var producer *automq.Producer
	brokers := os.Getenv("AUTOMQ_BROKERS")
	if brokers != "" {
		producer, err = automq.NewProducer(automq.Config{
			Brokers:     []string{brokers},
			TopicPrefix: envOrDefault("AUTOMQ_TOPIC_PREFIX", "fhir."),
			Logger:      logger,
		})
		if err != nil {
			log.Fatalf("automq connect %s: %v", brokers, err)
		}
		defer producer.Close()
		logger.Info("connected to automq", "brokers", brokers)
	} else {
		logger.Warn("AUTOMQ_BROKERS not set, AutoMQ publishing disabled")
	}

	// Create activities with all dependencies
	activities := &activity.Activities{
		Store:           store,
		Producer:        producer,
		Logger:          logger,
		// IndexExtractors populated with Big 5 proto2type extractors
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

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
