package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"github.com/flint-fhir/flint/ingest/activity"
	"github.com/flint-fhir/flint/store/postgres"
)

type workerStubDriver struct {
	execErr error
}
type workerStubConn struct {
	execErr error
}
type workerStubTx struct{}

func (d *workerStubDriver) Open(_ string) (driver.Conn, error) {
	return &workerStubConn{execErr: d.execErr}, nil
}
func (c *workerStubConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, errors.New("unsupported")
}
func (c *workerStubConn) Close() error              { return nil }
func (c *workerStubConn) Begin() (driver.Tx, error) { return &workerStubTx{}, nil }
func (c *workerStubConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if c.execErr != nil {
		return nil, c.execErr
	}
	return driver.RowsAffected(1), nil
}
func (t *workerStubTx) Commit() error   { return nil }
func (t *workerStubTx) Rollback() error { return nil }

var workerStubOnce sync.Once

func registerWorkerStubDrivers() {
	workerStubOnce.Do(func() {
		sql.Register("worker_stub_ok", &workerStubDriver{})
		sql.Register("worker_stub_mig_err", &workerStubDriver{execErr: errors.New("migration failed")})
	})
}

func TestLoadWorkerConfigAndHelpers(t *testing.T) {
	env := map[string]string{
		"TEMPORAL_ADDRESS":            "temporal.internal:7233",
		"FLINT_POSTGRES_DSN":          "postgres://custom/flint",
		"AUTOMQ_BROKERS":              " broker1:9092 , broker2:9092 ",
		"AUTOMQ_TOPIC_PREFIX":         "cdc.",
		"SCHEMA_REGISTRY_URL":         "http://registry:8081",
		"FLINT_TENANT_ROLE_SWITCH":    "true",
		"FLINT_TENANT_AUTO_PROVISION": "false",
	}
	cfg, err := loadWorkerConfig([]string{"-tenant-isolation", "shared"}, func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, postgres.TenantIsolationShared, cfg.IsolationMode)
	assert.True(t, cfg.RoleSwitch)
	assert.False(t, cfg.AutoProvision)
	assert.Equal(t, "temporal.internal:7233", cfg.TemporalAddr)
	assert.Equal(t, "postgres://custom/flint", cfg.PostgresDSN)
	assert.Equal(t, []string{"broker1:9092", "broker2:9092"}, cfg.Brokers)
	assert.Equal(t, "cdc.", cfg.TopicPrefix)
	assert.Equal(t, "http://registry:8081", cfg.SchemaRegistryURL)

	_, err = loadWorkerConfig([]string{"-tenant-isolation", "bogus"}, func(string) string { return "" })
	require.ErrorContains(t, err, "invalid tenant isolation mode")

	_, err = loadWorkerConfig([]string{"-bad-flag"}, func(string) string { return "" })
	require.Error(t, err)

	assert.Equal(t, "fallback", firstEnvOrDefault([]string{"FLINT_NONEXISTENT_ENV_KEY"}, "fallback"))
}

func TestRunWorker_AllBranches(t *testing.T) {
	registerWorkerStubDrivers()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("temporal dial error", func(t *testing.T) {
		err := runWorker(
			nil,
			func(string) string { return "" },
			logger,
			func(string) (*sql.DB, error) { return sql.Open("worker_stub_ok", "") },
			func(string) (client.Client, error) { return nil, errors.New("dial refused") },
			func(client.Client, *activity.Activities) error { return nil },
		)
		require.ErrorContains(t, err, "temporal connect")
	})

	t.Run("postgres open and migration errors", func(t *testing.T) {
		err := runWorker(
			nil,
			func(string) string { return "" },
			logger,
			func(string) (*sql.DB, error) { return nil, errors.New("db open err") },
			func(string) (client.Client, error) { return nil, nil },
			func(client.Client, *activity.Activities) error { return nil },
		)
		require.ErrorContains(t, err, "postgres open")

		err = runWorker(
			nil,
			func(string) string { return "" },
			logger,
			func(string) (*sql.DB, error) { return sql.Open("worker_stub_mig_err", "") },
			func(string) (client.Client, error) { return nil, nil },
			func(client.Client, *activity.Activities) error { return nil },
		)
		require.ErrorContains(t, err, "postgres migrations")
	})

	t.Run("successful worker startup with and without AutoMQ", func(t *testing.T) {
		var capturedAct *activity.Activities
		err := runWorker(
			nil,
			func(k string) string {
				if k == "FLINT_AUTOMQ_BROKERS" {
					return "127.0.0.1:19092"
				}
				if k == "FLINT_TENANT_ROLE_SWITCH" {
					return "true"
				}
				if k == "FLINT_TENANT_AUTO_PROVISION" {
					return "false"
				}
				return ""
			},
			logger,
			func(string) (*sql.DB, error) { return sql.Open("worker_stub_ok", "") },
			func(string) (client.Client, error) { return nil, nil },
			func(_ client.Client, act *activity.Activities) error {
				capturedAct = act
				return nil
			},
		)
		require.NoError(t, err)
		require.NotNil(t, capturedAct)
		require.NotNil(t, capturedAct.Store)
		require.NotNil(t, capturedAct.Producer)
		assert.GreaterOrEqual(t, len(capturedAct.IndexExtractors), 146)

		// Without AutoMQ brokers + worker run error branch
		err = runWorker(
			nil,
			func(string) string { return "" },
			logger,
			func(string) (*sql.DB, error) { return sql.Open("worker_stub_ok", "") },
			func(string) (client.Client, error) { return nil, nil },
			func(_ client.Client, act *activity.Activities) error {
				assert.Nil(t, act.Producer)
				return errors.New("worker interrupted")
			},
		)
		require.ErrorContains(t, err, "worker run")
	})
}
