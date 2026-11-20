package doris_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/store/doris"
)

func TestMemoryRowLocker_MutualExclusionAndTTL(t *testing.T) {
	locker := doris.NewMemoryRowLocker()

	_, err := locker.Acquire(context.Background(), "", time.Second)
	require.Error(t, err)

	// Exclusive critical section across 20 goroutines
	var active atomic.Int32
	var maxActive atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, aErr := locker.Acquire(context.Background(), "res:t1:Patient:p1", 5*time.Second)
			require.NoError(t, aErr)
			cur := active.Add(1)
			for {
				prev := maxActive.Load()
				if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			require.NoError(t, rel(context.Background()))
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), maxActive.Load())

	// TTL expiry takeover + owner-safe release (holder1's late release must NOT drop holder2's lease)
	rel1, err := locker.Acquire(context.Background(), "ttl-key", 25*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(40 * time.Millisecond)

	rel2, err := locker.Acquire(context.Background(), "ttl-key", 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, rel1(context.Background())) // stale owner release

	// Lease must still be held by owner 2, so holder3 times out
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = locker.Acquire(waitCtx, "ttl-key", time.Second)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	require.NoError(t, rel2(context.Background()))
}

type simPGLease struct {
	ownerID   string
	expiresAt time.Time
}

type simPGDriver struct {
	mu     sync.Mutex
	tables map[string]bool
	leases map[string]simPGLease
	errOn  string
}

type simPGConn struct {
	drv *simPGDriver
}

type simPGRows struct {
	cols []string
	vals [][]driver.Value
	idx  int
}

func (r *simPGRows) Columns() []string { return r.cols }
func (r *simPGRows) Close() error      { return nil }
func (r *simPGRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.vals) {
		return io.EOF
	}
	row := r.vals[r.idx]
	r.idx++
	copy(dest, row)
	return nil
}

func (d *simPGDriver) Open(_ string) (driver.Conn, error) {
	return &simPGConn{drv: d}, nil
}

func (c *simPGConn) Prepare(_ string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *simPGConn) Close() error                          { return nil }
func (c *simPGConn) Begin() (driver.Tx, error)             { return nil, errors.New("unsupported") }

func (c *simPGConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.drv.mu.Lock()
	defer c.drv.mu.Unlock()
	if c.drv.errOn != "" && strings.Contains(query, c.drv.errOn) {
		return nil, errors.New("injected pg error")
	}
	if strings.HasPrefix(query, "CREATE UNLOGGED TABLE") {
		if c.drv.tables == nil {
			c.drv.tables = make(map[string]bool)
		}
		c.drv.tables[query] = true
		return driver.RowsAffected(0), nil
	}
	if strings.HasPrefix(query, "DELETE FROM ") && len(args) >= 2 {
		lockKey, _ := args[0].Value.(string)
		ownerID, _ := args[1].Value.(string)
		if cur, ok := c.drv.leases[lockKey]; ok && cur.ownerID == ownerID {
			delete(c.drv.leases, lockKey)
			return driver.RowsAffected(1), nil
		}
		return driver.RowsAffected(0), nil
	}
	return driver.RowsAffected(0), nil
}

func (c *simPGConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.drv.mu.Lock()
	defer c.drv.mu.Unlock()
	if c.drv.errOn != "" && strings.Contains(query, c.drv.errOn) {
		return nil, errors.New("injected pg query error")
	}
	if strings.Contains(query, "ON CONFLICT (lock_key) DO UPDATE") && len(args) >= 3 {
		lockKey, _ := args[0].Value.(string)
		ownerID, _ := args[1].Value.(string)
		ttlMs, _ := args[2].Value.(int64)
		now := time.Now().UTC()
		if cur, ok := c.drv.leases[lockKey]; ok && cur.expiresAt.After(now) {
			return &simPGRows{cols: []string{"owner_id"}}, nil
		}
		if c.drv.leases == nil {
			c.drv.leases = make(map[string]simPGLease)
		}
		c.drv.leases[lockKey] = simPGLease{
			ownerID:   ownerID,
			expiresAt: now.Add(time.Duration(ttlMs) * time.Millisecond),
		}
		return &simPGRows{
			cols: []string{"owner_id"},
			vals: [][]driver.Value{{ownerID}},
		}, nil
	}
	return &simPGRows{cols: []string{"owner_id"}}, nil
}

var (
	regSimPGOnce sync.Once
	simPG        = &simPGDriver{}
)

func TestPostgresRowLocker_LeaseAcquireRetryExpiryAndErrors(t *testing.T) {
	regSimPGOnce.Do(func() {
		sql.Register("doris_pg_locker_stub", simPG)
	})

	_, err := doris.NewPostgresRowLocker(nil)
	require.Error(t, err)

	pgDB, err := sql.Open("doris_pg_locker_stub", "")
	require.NoError(t, err)
	defer pgDB.Close()

	_, err = doris.NewPostgresRowLocker(pgDB, doris.WithLockTableName("bad-table"))
	require.Error(t, err)

	ddl, err := doris.BuildRowLocksTableDDL("")
	require.NoError(t, err)
	assert.Contains(t, ddl, "CREATE UNLOGGED TABLE IF NOT EXISTS flint_doris_row_locks")

	_, err = doris.BuildRowLocksTableDDL("bad;tbl")
	require.Error(t, err)

	locker, err := doris.NewPostgresRowLocker(
		pgDB,
		doris.WithLockTableName("custom_doris_locks"),
		doris.WithLockRetryInterval(5*time.Millisecond),
	)
	require.NoError(t, err)
	require.NoError(t, locker.EnsureTable(context.Background()))

	_, err = locker.Acquire(context.Background(), "", time.Second)
	require.Error(t, err)

	// Acquire lock, launch contended second acquirer that wakes up once first holder releases
	rel1, err := locker.Acquire(context.Background(), "pg-lock-1", 5*time.Second)
	require.NoError(t, err)

	acquiredCh := make(chan struct{})
	go func() {
		rel2, aErr := locker.Acquire(context.Background(), "pg-lock-1", 5*time.Second)
		if assert.NoError(t, aErr) {
			// Pass an already-canceled context to rel2 to verify fallback release still deletes row!
			canceledCtx, cancel := context.WithCancel(context.Background())
			cancel()
			assert.NoError(t, rel2(canceledCtx))
			close(acquiredCh)
		}
	}()

	time.Sleep(15 * time.Millisecond)
	require.NoError(t, rel1(context.Background()))
	select {
	case <-acquiredCh:
	case <-time.After(time.Second):
		t.Fatal("contended acquirer did not proceed after release")
	}

	// TTL expiry takeover + owner-safe release
	staleRel, err := locker.Acquire(context.Background(), "pg-lock-ttl", 25*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(40 * time.Millisecond)

	activeRel, err := locker.Acquire(context.Background(), "pg-lock-ttl", 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, staleRel(context.Background()))

	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelTimeout()
	_, err = locker.Acquire(timeoutCtx, "pg-lock-ttl", time.Second)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, activeRel(context.Background()))
}
