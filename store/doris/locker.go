package doris

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"sync"
	"time"
)

const (
	defaultRowLockTTL          = 15 * time.Second
	defaultMemoryLockRetry     = 5 * time.Millisecond
	defaultPostgresLockTable   = "flint_doris_row_locks"
	defaultPostgresRetryWait   = 25 * time.Millisecond
	lockReleaseFallbackTimeout = 5 * time.Second
)

// RowLocker coordinates mutually exclusive distributed leases across stateless Flint
// replicas sharing an Apache Doris cluster when executing CAS, conditional CRUD,
// compensating transactions, secret initialization, or single-use nonce claims.
type RowLocker interface {
	Acquire(ctx context.Context, lockKey string, ttl time.Duration) (release func(context.Context) error, err error)
}

type memoryLockLease struct {
	ownerID   string
	expiresAt time.Time
}

// MemoryRowLocker is a thread-safe in-memory RowLocker suitable for single-replica
// deployments and local concurrency testing.
type MemoryRowLocker struct {
	mu            sync.Mutex
	leases        map[string]memoryLockLease
	retryInterval time.Duration
}

// NewMemoryRowLocker constructs a new single-node MemoryRowLocker.
func NewMemoryRowLocker() *MemoryRowLocker {
	return &MemoryRowLocker{
		leases:        make(map[string]memoryLockLease),
		retryInterval: defaultMemoryLockRetry,
	}
}

func randomLockOwnerID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Acquire acquires an exclusive in-memory lease for lockKey with TTL ttl.
func (l *MemoryRowLocker) Acquire(ctx context.Context, lockKey string, ttl time.Duration) (func(context.Context) error, error) {
	if lockKey == "" {
		return nil, errors.New("lockKey must not be empty")
	}
	if ttl <= 0 {
		ttl = defaultRowLockTTL
	}
	ownerID := randomLockOwnerID()
	for {
		now := time.Now().UTC()
		l.mu.Lock()
		existing, held := l.leases[lockKey]
		if !held || !existing.expiresAt.After(now) {
			l.leases[lockKey] = memoryLockLease{
				ownerID:   ownerID,
				expiresAt: now.Add(ttl),
			}
			l.mu.Unlock()
			return func(_ context.Context) error {
				l.mu.Lock()
				if cur, ok := l.leases[lockKey]; ok && cur.ownerID == ownerID {
					delete(l.leases, lockKey)
				}
				l.mu.Unlock()
				return nil
			}, nil
		}
		l.mu.Unlock()

		wait := l.retryInterval
		if wait <= 0 {
			wait = defaultMemoryLockRetry
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("acquire memory lock %q: %w", lockKey, ctx.Err())
		case <-timer.C:
		}
	}
}

// PostgresLockerOption configures a PostgresRowLocker.
type PostgresLockerOption func(*PostgresRowLocker)

// WithLockTableName sets the Postgres UNLOGGED lease table name (default: "flint_doris_row_locks").
func WithLockTableName(name string) PostgresLockerOption {
	return func(l *PostgresRowLocker) {
		if err := ValidateIdentifier(name); err != nil {
			l.optionErr = err
			return
		}
		l.tableName = name
	}
}

// WithLockRetryInterval sets the base retry wait interval when a lock lease is contended.
func WithLockRetryInterval(d time.Duration) PostgresLockerOption {
	return func(l *PostgresRowLocker) {
		if d > 0 {
			l.retryInterval = d
		}
	}
}

// PostgresRowLocker implements RowLocker using a single-RTT atomic UPSERT against a
// Postgres UNLOGGED table (`INSERT ... ON CONFLICT DO UPDATE ... WHERE expires_at <= NOW()`).
type PostgresRowLocker struct {
	db            *sql.DB
	tableName     string
	retryInterval time.Duration
	optionErr     error
}

// NewPostgresRowLocker creates a distributed lease locker backed by Postgres.
func NewPostgresRowLocker(db *sql.DB, opts ...PostgresLockerOption) (*PostgresRowLocker, error) {
	if db == nil {
		return nil, errors.New("postgres row locker requires non-nil *sql.DB")
	}
	l := &PostgresRowLocker{
		db:            db,
		tableName:     defaultPostgresLockTable,
		retryInterval: defaultPostgresRetryWait,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	if l.optionErr != nil {
		return nil, l.optionErr
	}
	return l, nil
}

// BuildRowLocksTableDDL returns the Postgres UNLOGGED table DDL for the lease table.
func BuildRowLocksTableDDL(tableName string) (string, error) {
	if tableName == "" {
		tableName = defaultPostgresLockTable
	}
	if err := ValidateIdentifier(tableName); err != nil {
		return "", err
	}
	return fmt.Sprintf(`CREATE UNLOGGED TABLE IF NOT EXISTS %s (
    lock_key   TEXT PRIMARY KEY,
    owner_id   TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);`, tableName), nil
}

// EnsureTable creates the UNLOGGED lock lease table in Postgres if it does not already exist.
func (l *PostgresRowLocker) EnsureTable(ctx context.Context) error {
	ddl, err := BuildRowLocksTableDDL(l.tableName)
	if err != nil {
		return err
	}
	if _, err := l.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create postgres row lock table %s: %w", l.tableName, err)
	}
	return nil
}

// Acquire acquires a distributed row lease for lockKey with TTL ttl using bounded jittered retries.
func (l *PostgresRowLocker) Acquire(ctx context.Context, lockKey string, ttl time.Duration) (func(context.Context) error, error) {
	if lockKey == "" {
		return nil, errors.New("lockKey must not be empty")
	}
	if ttl <= 0 {
		ttl = defaultRowLockTTL
	}
	ttlMs := max(ttl.Milliseconds(), 1)
	ownerID := randomLockOwnerID()

	acquireSQL := fmt.Sprintf(
		`INSERT INTO %s (lock_key, owner_id, expires_at) VALUES ($1, $2, NOW() + ($3 * INTERVAL '1 millisecond')) ON CONFLICT (lock_key) DO UPDATE SET owner_id = EXCLUDED.owner_id, expires_at = EXCLUDED.expires_at WHERE %s.expires_at <= NOW() RETURNING owner_id`,
		l.tableName, l.tableName,
	)
	releaseSQL := fmt.Sprintf(`DELETE FROM %s WHERE lock_key = $1 AND owner_id = $2`, l.tableName)

	for {
		var returnedOwner string
		err := l.db.QueryRowContext(ctx, acquireSQL, lockKey, ownerID, ttlMs).Scan(&returnedOwner)
		if err == nil && returnedOwner == ownerID {
			return func(relCtx context.Context) error {
				execCtx := relCtx
				var cancel context.CancelFunc
				if execCtx == nil || execCtx.Err() != nil {
					execCtx, cancel = context.WithTimeout(context.Background(), lockReleaseFallbackTimeout)
					defer cancel()
				}
				_, relErr := l.db.ExecContext(execCtx, releaseSQL, lockKey, ownerID)
				return relErr
			}, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("acquire postgres lock %q: %w", lockKey, err)
		}

		baseWait := l.retryInterval
		if baseWait <= 0 {
			baseWait = defaultPostgresRetryWait
		}
		jitter := time.Duration(mathrand.Int64N(int64(baseWait/2) + 1))
		timer := time.NewTimer(baseWait + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("acquire postgres lock %q: %w", lockKey, ctx.Err())
		case <-timer.C:
		}
	}
}
