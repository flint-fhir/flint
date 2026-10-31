// Package main runs the Flint Temporal worker.
// It connects to Temporal, Postgres, and AutoMQ, registers workflows and activities,
// and starts processing IngestFHIRBundle workflows.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
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

type workerConfig struct {
	IsolationMode     postgres.TenantIsolationMode
	RoleSwitch        bool
	AutoProvision     bool
	TemporalAddr      string
	PostgresDSN       string
	Brokers           []string
	TopicPrefix       string
	SchemaRegistryURL string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	err := runWorker(
		os.Args[1:],
		os.Getenv,
		logger,
		func(dsn string) (*sql.DB, error) { return sql.Open("postgres", dsn) },
		func(addr string) (client.Client, error) { return client.Dial(client.Options{HostPort: addr}) },
		func(c client.Client, act *activity.Activities) error {
			w := worker.New(c, workflow.TaskQueue, worker.Options{})
			w.RegisterWorkflow(workflow.IngestFHIRBundle)
			w.RegisterActivity(act)
			return w.Run(worker.InterruptCh())
		},
	)
	if err != nil {
		log.Fatalf("%v", err)
	}
}

func loadWorkerConfig(args []string, getenv func(string) string) (workerConfig, error) {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var tenantIsolationFlag string
	fs.StringVar(&tenantIsolationFlag, "tenant-isolation", "", "Multi-tenant PostgreSQL isolation mode: 'schema' (default, schema-per-tenant) or 'shared' (shared public tables)")
	if err := fs.Parse(args); err != nil {
		return workerConfig{}, err
	}

	rawIsolation := strings.TrimSpace(tenantIsolationFlag)
	if rawIsolation == "" {
		rawIsolation = strings.TrimSpace(getenv("FLINT_TENANT_ISOLATION"))
	}
	isolationMode, err := postgres.ParseTenantIsolationMode(rawIsolation)
	if err != nil {
		return workerConfig{}, fmt.Errorf("invalid tenant isolation mode: %w", err)
	}

	rawBrokers := firstEnvOrDefaultWith(getenv, []string{"FLINT_AUTOMQ_BROKERS", "AUTOMQ_BROKERS"}, "")
	var brokers []string
	if rawBrokers != "" {
		for b := range strings.SplitSeq(rawBrokers, ",") {
			if trimmed := strings.TrimSpace(b); trimmed != "" {
				brokers = append(brokers, trimmed)
			}
		}
	}

	autoProvision := !strings.EqualFold(strings.TrimSpace(getenv("FLINT_TENANT_AUTO_PROVISION")), "false")

	return workerConfig{
		IsolationMode:     isolationMode,
		RoleSwitch:        strings.EqualFold(strings.TrimSpace(getenv("FLINT_TENANT_ROLE_SWITCH")), "true"),
		AutoProvision:     autoProvision,
		TemporalAddr:      firstEnvOrDefaultWith(getenv, []string{"FLINT_TEMPORAL_ADDR", "TEMPORAL_ADDRESS"}, "localhost:7233"),
		PostgresDSN:       firstEnvOrDefaultWith(getenv, []string{"FLINT_POSTGRES_DSN"}, "postgres://flint:flint@localhost:5432/flint?sslmode=disable"),
		Brokers:           brokers,
		TopicPrefix:       firstEnvOrDefaultWith(getenv, []string{"FLINT_AUTOMQ_TOPIC_PREFIX", "AUTOMQ_TOPIC_PREFIX"}, "fhir."),
		SchemaRegistryURL: firstEnvOrDefaultWith(getenv, []string{"FLINT_SCHEMA_REGISTRY_URL", "SCHEMA_REGISTRY_URL"}, ""),
	}, nil
}

func buildWorkerActivities(store *postgres.Store, producer *automq.Producer, logger *slog.Logger) *activity.Activities {
	return &activity.Activities{
		Store:           store,
		Producer:        producer,
		Logger:          logger,
		IndexExtractors: postgres.DefaultIndexExtractors(),
	}
}

func runWorker(
	args []string,
	getenv func(string) string,
	logger *slog.Logger,
	openDB func(dsn string) (*sql.DB, error),
	dialTemporal func(addr string) (client.Client, error),
	startWorker func(c client.Client, act *activity.Activities) error,
) error {
	cfg, err := loadWorkerConfig(args, getenv)
	if err != nil {
		return err
	}

	c, err := dialTemporal(cfg.TemporalAddr)
	if err != nil {
		return fmt.Errorf("temporal connect %s: %w", cfg.TemporalAddr, err)
	}
	if c != nil {
		defer c.Close()
	}
	logger.Info("connected to temporal", "addr", cfg.TemporalAddr)

	db, err := openDB(cfg.PostgresDSN)
	if err != nil {
		return fmt.Errorf("postgres open: %w", err)
	}
	defer db.Close()
	if err := postgres.RunMigrations(context.Background(), db); err != nil {
		return fmt.Errorf("postgres migrations: %w", err)
	}

	storeOpts := []postgres.Option{
		postgres.WithTenantIsolation(cfg.IsolationMode),
	}
	if cfg.RoleSwitch {
		storeOpts = append(storeOpts, postgres.WithTenantRoleSwitch(true))
	}
	if !cfg.AutoProvision {
		storeOpts = append(storeOpts, postgres.WithAutoProvision(false))
	}
	store := postgres.New(db, storeOpts...)
	logger.Info("connected to postgres", "tenant_isolation", string(cfg.IsolationMode))

	var producer *automq.Producer
	if len(cfg.Brokers) > 0 {
		producer, err = automq.NewProducer(automq.Config{
			Brokers:           cfg.Brokers,
			TopicPrefix:       cfg.TopicPrefix,
			SchemaRegistryURL: cfg.SchemaRegistryURL,
			Logger:            logger,
		})
		if err != nil {
			return fmt.Errorf("automq connect %v: %w", cfg.Brokers, err)
		}
		defer producer.Close()
		logger.Info("connected to automq", "brokers", cfg.Brokers, "schemaRegistryURL", cfg.SchemaRegistryURL)
	} else {
		logger.Warn("AUTOMQ_BROKERS not set, AutoMQ publishing disabled")
	}

	activities := buildWorkerActivities(store, producer, logger)
	logger.Info("starting flint worker", "taskQueue", workflow.TaskQueue)
	if err := startWorker(c, activities); err != nil {
		return fmt.Errorf("worker run: %w", err)
	}
	return nil
}

func firstEnvOrDefault(keys []string, def string) string {
	return firstEnvOrDefaultWith(os.Getenv, keys, def)
}

func firstEnvOrDefaultWith(getenv func(string) string, keys []string, def string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
	}
	return def
}
