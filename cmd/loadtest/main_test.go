package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type loadStubDriver struct{}
type loadStubConn struct{}
type loadStubTx struct{}
type loadStubRows struct {
	done bool
}

func (d *loadStubDriver) Open(_ string) (driver.Conn, error)  { return &loadStubConn{}, nil }
func (c *loadStubConn) Prepare(_ string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *loadStubConn) Close() error                          { return nil }
func (c *loadStubConn) Begin() (driver.Tx, error)             { return &loadStubTx{}, nil }
func (c *loadStubConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (c *loadStubConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "SELECT res_version, is_deleted") {
		return &loadStubRows{done: true}, nil
	}
	return &loadStubRows{}, nil
}
func (t *loadStubTx) Commit() error   { return nil }
func (t *loadStubTx) Rollback() error { return nil }
func (r *loadStubRows) Columns() []string {
	return []string{"res_version", "last_updated"}
}
func (r *loadStubRows) Close() error { return nil }
func (r *loadStubRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	dest[1] = time.Now().UTC()
	return nil
}

var loadStubOnce sync.Once

func registerLoadStubDriver() {
	loadStubOnce.Do(func() {
		sql.Register("loadtest_stub_ok", &loadStubDriver{})
	})
}

func TestParseLoadConfigAndBundleGeneration(t *testing.T) {
	cfg, err := parseLoadConfig([]string{
		"-target", "http",
		"-url", "http://localhost:8080/fhir/r4/t1",
		"-rate", "250",
		"-duration", "2s",
		"-workers", "4",
		"-bundle-size", "5",
		"-tenant", "t1",
	})
	require.NoError(t, err)
	assert.Equal(t, "http", cfg.Target)
	assert.Equal(t, 250, cfg.Rate)
	assert.Equal(t, 2*time.Second, cfg.Duration)
	assert.Equal(t, 4, cfg.Workers)
	assert.Equal(t, 5, cfg.BundleSize)
	assert.Equal(t, "t1", cfg.Tenant)

	_, err = parseLoadConfig([]string{"-invalid-flag"})
	require.Error(t, err)

	for size := 1; size <= 5; size++ {
		payloads := generateBundlePayloads(3, size)
		require.Len(t, payloads, 3)
		var b map[string]any
		require.NoError(t, json.Unmarshal(payloads[0], &b))
		assert.Equal(t, "Bundle", b["resourceType"])
		assert.Equal(t, "transaction", b["type"])
		entries := b["entry"].([]any)
		assert.Len(t, entries, size)
	}
}

func TestMetricsRatioAndReport(t *testing.T) {
	assert.Equal(t, 0.0, ratio(5, 0))
	assert.Equal(t, 75.0, ratio(3, 4))

	m := &Metrics{}
	printReport(Config{BundleSize: 5}, m, 100*time.Millisecond)

	for i := 1; i <= 20; i++ {
		m.recordLatency(time.Duration(i) * time.Millisecond)
	}
	m.totalBundles.Store(20)
	m.totalResources.Store(100)
	m.status2xx.Store(18)
	m.status4xx.Store(1)
	m.status5xx.Store(1)
	printReport(Config{BundleSize: 5}, m, 200*time.Millisecond)
}

func TestExecuteLoadTest_HTTPStoreAndUnknownTarget(t *testing.T) {
	registerLoadStubDriver()

	var reqCount atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := reqCount.Add(1)
		switch n % 5 {
		case 0:
			w.WriteHeader(http.StatusInternalServerError)
		case 1:
			w.WriteHeader(http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusOK)
		}
		_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"transaction-response"}`))
	}))
	defer ts.Close()

	// 1. Rate-limited HTTP load test
	m1, _, err := executeLoadTest(t.Context(), Config{
		Target:     "http",
		URL:        ts.URL,
		Rate:       200,
		Duration:   80 * time.Millisecond,
		Workers:    2,
		BundleSize: 5,
		Tenant:     "loadtest",
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, m1.totalBundles.Load(), int64(1))
	assert.GreaterOrEqual(t, m1.totalResources.Load(), int64(5))

	// 2. Unthrottled HTTP load test (Rate == 0)
	m2, _, err := executeLoadTest(t.Context(), Config{
		Target:     "http",
		URL:        ts.URL,
		Rate:       0,
		Duration:   40 * time.Millisecond,
		Workers:    2,
		BundleSize: 3,
		Tenant:     "loadtest",
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, m2.totalBundles.Load(), int64(1))

	// 3. Store load test with in-process stub SQL driver
	m3, _, err := executeLoadTest(t.Context(), Config{
		Target:     "store",
		DBDriver:   "loadtest_stub_ok",
		PGDSN:      "stub",
		Rate:       100,
		Duration:   60 * time.Millisecond,
		Workers:    2,
		BundleSize: 5,
		Tenant:     "loadtest",
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, m3.totalBundles.Load(), int64(1))

	// 4. Unknown target returns error
	_, _, err = executeLoadTest(t.Context(), Config{
		Target:   "unknown-mode",
		Duration: 10 * time.Millisecond,
	})
	require.ErrorContains(t, err, "unknown target mode")
}
