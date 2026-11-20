package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flint-fhir/flint/store"
)

// Option configures a SQLite Store.
type Option func(*Store)

// WithJournalMode configures the SQLite journal_mode ("WAL", "MEMORY", or "DELETE").
func WithJournalMode(mode JournalMode) Option {
	return func(s *Store) {
		if mode != "" {
			s.journalMode = mode
		}
	}
}

// WithDSN configures the SQLite file path or URI (e.g. "flint.db" or ":memory:").
func WithDSN(dsn string) Option {
	return func(s *Store) {
		if trimmed := strings.TrimSpace(dsn); trimmed != "" {
			s.dsn = trimmed
		}
	}
}

type sqliteEntry struct {
	tenantID       string
	resType        string
	resID          string
	resVersion     int
	isDeleted      bool
	lastUpdated    time.Time
	idempotencyKey string
	resourceProto  []byte
	patientID      string
	encounterID    string
	indexes        *store.SearchIndexes
}

var sqlitePathWriteLocks sync.Map

// Store implements store.Store and store.AnalyticalStore for embedded SQLite WAL deployments.
//
// In SQLite WAL mode, readers execute concurrently while writes are serialized via a single
// write lock (writeMu) to prevent SQLITE_BUSY errors under concurrent goroutines.
type Store struct {
	db          *sql.DB
	dsn         string
	journalMode JournalMode

	writeMu          sync.Mutex
	mu               sync.RWMutex
	localEpoch       int64
	localAuditEpoch  int64
	metaSyncDisabled atomic.Bool

	resources      map[string]sqliteEntry
	history        map[string][]store.ResourceRecord
	audits         map[string][]store.AuditRecord
	authCodes      map[string]store.OAuthAuthCodeRecord
	refreshTokens  map[string]store.OAuthRefreshTokenRecord
	revokedTokens  map[string]time.Time
	exportJobs     map[string]store.ExportJobRecord
	clusterSecrets map[string][]byte
	clusterStates  map[string]store.ClusterStateRecord
	sqlLog         []string
}

var (
	_ store.Store           = (*Store)(nil)
	_ store.AnalyticalStore = (*Store)(nil)
)

// New creates a new Zero-JSON SQLite WAL store.
func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{
		db:             db,
		dsn:            "flint.db",
		journalMode:    JournalWAL,
		resources:      make(map[string]sqliteEntry),
		history:        make(map[string][]store.ResourceRecord),
		audits:         make(map[string][]store.AuditRecord),
		authCodes:      make(map[string]store.OAuthAuthCodeRecord),
		refreshTokens:  make(map[string]store.OAuthRefreshTokenRecord),
		revokedTokens:  make(map[string]time.Time),
		exportJobs:     make(map[string]store.ExportJobRecord),
		clusterSecrets: make(map[string][]byte),
		clusterStates:  make(map[string]store.ClusterStateRecord),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// BackendName returns "sqlite".
func (s *Store) BackendName() string {
	return string(store.BackendSQLite)
}

// JournalMode returns the configured SQLite journal_mode.
func (s *Store) JournalMode() JournalMode {
	if s == nil || s.journalMode == "" {
		return JournalWAL
	}
	return s.journalMode
}

// DSN returns the configured SQLite file path or URI.
func (s *Store) DSN() string {
	if s == nil || s.dsn == "" {
		return "flint.db"
	}
	return s.dsn
}

// ExecutedStatements returns a snapshot of the SQLite SQL statements executed by this Store.
func (s *Store) ExecutedStatements() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.sqlLog))
	copy(out, s.sqlLog)
	return out
}

func resKey(tenantID, resType, resID string) string {
	return tenantID + "|" + resType + "|" + resID
}

type sqliteTxKey struct{}

func inSQLiteTx(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	tx, ok := ctx.Value(sqliteTxKey{}).(*sql.Tx)
	return ok && tx != nil
}

func (s *Store) writeLock() *sync.Mutex {
	if s.db != nil && s.dsn != "" && s.dsn != ":memory:" {
		clean := filepath.Clean(s.dsn)
		v, _ := sqlitePathWriteLocks.LoadOrStore(clean, &sync.Mutex{})
		return v.(*sync.Mutex)
	}
	return &s.writeMu
}

func (s *Store) readDBMetaSeq(ctx context.Context, metaKey string) (int64, bool) {
	if s == nil || s.db == nil || inSQLiteTx(ctx) || s.metaSyncDisabled.Load() {
		return 0, false
	}
	qCtx := ctx
	if qCtx == nil {
		qCtx = context.Background()
	}
	var seq int64
	err := s.db.QueryRowContext(qCtx, `SELECT meta_seq FROM fhir_sqlite_meta WHERE meta_key = ?`, metaKey).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, true
	}
	if err != nil {
		if ctx == nil || ctx.Err() == nil {
			s.metaSyncDisabled.Store(true)
		}
		return 0, false
	}
	return seq, true
}

func (s *Store) readDBEpoch(ctx context.Context) (int64, bool) {
	return s.readDBMetaSeq(ctx, "epoch")
}

func (s *Store) syncBeforeWriteLocked(ctx context.Context) {
	if s == nil || s.db == nil || inSQLiteTx(ctx) || s.metaSyncDisabled.Load() {
		return
	}
	dbSeq, ok := s.readDBEpoch(ctx)
	if !ok {
		return
	}
	s.mu.RLock()
	cur := s.localEpoch
	s.mu.RUnlock()
	if dbSeq != cur {
		qCtx := ctx
		if qCtx == nil {
			qCtx = context.Background()
		}
		_ = s.loadFromDBInternal(qCtx, false)
	}
}

func (s *Store) syncIfStale(ctx context.Context) {
	if s == nil || s.db == nil || inSQLiteTx(ctx) || s.metaSyncDisabled.Load() {
		return
	}
	dbSeq, ok := s.readDBEpoch(ctx)
	if !ok {
		return
	}
	s.mu.RLock()
	cur := s.localEpoch
	s.mu.RUnlock()
	if dbSeq == cur {
		return
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)
}

func (s *Store) bumpEpochLocked(ctx context.Context) {
	if s == nil || s.db == nil || inSQLiteTx(ctx) || s.metaSyncDisabled.Load() {
		return
	}
	qCtx := ctx
	if qCtx == nil {
		qCtx = context.Background()
	}
	if _, err := s.db.ExecContext(qCtx, `INSERT INTO fhir_sqlite_meta (meta_key, meta_seq) VALUES ('epoch', 1) ON CONFLICT (meta_key) DO UPDATE SET meta_seq = fhir_sqlite_meta.meta_seq + 1`); err != nil {
		if ctx == nil || ctx.Err() == nil {
			s.metaSyncDisabled.Store(true)
		}
		return
	}
	if seq, ok := s.readDBEpoch(qCtx); ok {
		s.mu.Lock()
		s.localEpoch = seq
		s.mu.Unlock()
	}
}

func (s *Store) syncAuditsLocked(ctx context.Context) {
	if s == nil || s.db == nil || inSQLiteTx(ctx) || s.metaSyncDisabled.Load() {
		return
	}
	dbSeq, ok := s.readDBMetaSeq(ctx, "audit_epoch")
	if !ok {
		return
	}
	s.mu.RLock()
	cur := s.localAuditEpoch
	s.mu.RUnlock()
	if dbSeq == cur {
		return
	}
	qCtx := ctx
	if qCtx == nil {
		qCtx = context.Background()
	}
	audits, err := s.queryAuditsFromDB(qCtx)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.audits = audits
	s.localAuditEpoch = dbSeq
	s.mu.Unlock()
}

func (s *Store) syncAuditsIfStale(ctx context.Context) {
	if s == nil || s.db == nil || inSQLiteTx(ctx) || s.metaSyncDisabled.Load() {
		return
	}
	dbSeq, ok := s.readDBMetaSeq(ctx, "audit_epoch")
	if !ok {
		return
	}
	s.mu.RLock()
	cur := s.localAuditEpoch
	s.mu.RUnlock()
	if dbSeq == cur {
		return
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncAuditsLocked(ctx)
}

func (s *Store) bumpAuditEpochLocked(ctx context.Context) {
	if s == nil || s.db == nil || inSQLiteTx(ctx) || s.metaSyncDisabled.Load() {
		return
	}
	qCtx := ctx
	if qCtx == nil {
		qCtx = context.Background()
	}
	if _, err := s.db.ExecContext(qCtx, `INSERT INTO fhir_sqlite_meta (meta_key, meta_seq) VALUES ('audit_epoch', 1) ON CONFLICT (meta_key) DO UPDATE SET meta_seq = fhir_sqlite_meta.meta_seq + 1`); err != nil {
		if ctx == nil || ctx.Err() == nil {
			s.metaSyncDisabled.Store(true)
		}
		return
	}
	if seq, ok := s.readDBMetaSeq(qCtx, "audit_epoch"); ok {
		s.mu.Lock()
		s.localAuditEpoch = seq
		s.mu.Unlock()
	}
}

func (s *Store) execSQLite(ctx context.Context, stmt string, args ...any) error {
	s.mu.Lock()
	if len(s.sqlLog) < 1024 {
		s.sqlLog = append(s.sqlLog, stmt)
	}
	s.mu.Unlock()

	if s.db == nil {
		return nil
	}
	if ctx != nil {
		if tx, ok := ctx.Value(sqliteTxKey{}).(*sql.Tx); ok && tx != nil {
			_, err := tx.ExecContext(ctx, stmt, args...)
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, stmt, args...)
	return err
}

// Ping verifies connectivity to the underlying SQLite database.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.PingContext(ctx)
}

// WriteResource writes a single FHIR resource and its 6 spidx_* B-tree index rows to SQLite.
func (s *Store) WriteResource(ctx context.Context, input store.ResourceInput) error {
	_, err := s.WriteResourceWithMeta(ctx, input)
	return err
}

// WriteResourceWithMeta writes a single FHIR resource under the serialized SQLite WAL writer lock.
func (s *Store) WriteResourceWithMeta(ctx context.Context, input store.ResourceInput) (*store.ResourceRecord, error) {
	if strings.TrimSpace(input.TenantID) == "" {
		return nil, store.ErrNoTenant
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	return s.writeUnlocked(ctx, input)
}

func extractCompartmentLinks(resType, resID string, idx *store.SearchIndexes) (string, string) {
	var patID, encID string
	if strings.EqualFold(resType, "Patient") {
		patID = resID
	}
	if strings.EqualFold(resType, "Encounter") {
		encID = resID
	}
	if idx == nil {
		return patID, encID
	}
	for _, r := range idx.References {
		if patID == "" && (strings.EqualFold(r.TargetType, "Patient") || r.SpName == "patient" || r.SpName == "beneficiary") {
			patID = r.TargetID
		}
		if encID == "" && (strings.EqualFold(r.TargetType, "Encounter") || r.SpName == "encounter" || r.SpName == "context") {
			encID = r.TargetID
		}
	}
	return patID, encID
}

func (s *Store) writeUnlocked(ctx context.Context, input store.ResourceInput) (*store.ResourceRecord, error) {
	key := resKey(input.TenantID, input.ResType, input.ResID)

	s.mu.Lock()
	existing, exists := s.resources[key]
	curVersion := 0
	curDeleted := false
	if exists {
		curVersion = existing.resVersion
		curDeleted = existing.isDeleted
	}

	created := false
	if !exists {
		created = true
		if input.ExpectedVersion > 0 {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: resource does not exist (expected version %d)", store.ErrVersionConflict, input.ExpectedVersion)
		}
	} else {
		if curDeleted {
			created = true
		} else if input.CreateOnly {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: %s/%s already exists", store.ErrResourceAlreadyExists, input.ResType, input.ResID)
		}
		if input.ExpectedVersion > 0 && (curDeleted || curVersion != input.ExpectedVersion) {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: expected version %d, current version %d", store.ErrVersionConflict, input.ExpectedVersion, curVersion)
		}
	}

	newVersion := curVersion + 1
	now := time.Now().UTC()
	patID, encID := extractCompartmentLinks(input.ResType, input.ResID, input.SearchIndexes)
	entry := sqliteEntry{
		tenantID:       input.TenantID,
		resType:        input.ResType,
		resID:          input.ResID,
		resVersion:     newVersion,
		isDeleted:      false,
		lastUpdated:    now,
		idempotencyKey: input.IdempotencyKey,
		resourceProto:  append([]byte(nil), input.ResourceProto...),
		patientID:      patID,
		encounterID:    encID,
		indexes:        cloneSearchIndexes(input.SearchIndexes),
	}
	rec := store.ResourceRecord{
		TenantID:      input.TenantID,
		ResType:       input.ResType,
		ResID:         input.ResID,
		ResVersion:    newVersion,
		LastUpdated:   now,
		ResourceProto: append([]byte(nil), input.ResourceProto...),
		IsDeleted:     false,
		Created:       created,
	}

	s.resources[key] = entry
	s.history[key] = append(s.history[key], rec)
	s.mu.Unlock()

	protoBlob := entry.resourceProto
	if protoBlob == nil {
		protoBlob = []byte{}
	}

	upsertStmt := `INSERT INTO fhir_resource (tenant_id, res_type, res_id, res_version, is_deleted, last_updated, idempotency_key, resource_proto) VALUES (?, ?, ?, ?, 0, ?, ?, ?) ON CONFLICT (tenant_id, res_type, res_id) DO UPDATE SET res_version = excluded.res_version, is_deleted = 0, last_updated = excluded.last_updated, idempotency_key = excluded.idempotency_key, resource_proto = excluded.resource_proto`
	if err := s.execSQLite(ctx, upsertStmt,
		entry.tenantID, entry.resType, entry.resID, entry.resVersion,
		entry.lastUpdated.Format(time.RFC3339Nano), entry.idempotencyKey, protoBlob,
	); err != nil {
		return nil, fmt.Errorf("sqlite upsert resource: %w", err)
	}

	histStmt := `INSERT INTO fhir_resource_history (tenant_id, res_type, res_id, res_version, is_deleted, last_updated, resource_proto) VALUES (?, ?, ?, ?, 0, ?, ?)`
	if err := s.execSQLite(ctx, histStmt,
		entry.tenantID, entry.resType, entry.resID, entry.resVersion,
		entry.lastUpdated.Format(time.RFC3339Nano), protoBlob,
	); err != nil {
		return nil, fmt.Errorf("sqlite insert history: %w", err)
	}

	if input.SearchIndexes != nil {
		for _, tbl := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri"} {
			_ = s.execSQLite(ctx, fmt.Sprintf("DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?", tbl),
				entry.tenantID, entry.resType, entry.resID)
		}
		for _, st := range input.SearchIndexes.Strings {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_string (tenant_id, res_type, res_id, sp_name, sp_value, sp_value_lower) VALUES (?, ?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, st.SpName, st.SpValue, strings.ToLower(st.SpValue))
		}
		for _, tk := range input.SearchIndexes.Tokens {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_token (tenant_id, res_type, res_id, sp_name, sp_system, sp_value) VALUES (?, ?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, tk.SpName, tk.SpSystem, tk.SpValue)
		}
		for _, dt := range input.SearchIndexes.Dates {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_date (tenant_id, res_type, res_id, sp_name, sp_low, sp_high) VALUES (?, ?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, dt.SpName, dt.SpLow.UTC().Format(time.RFC3339Nano), dt.SpHigh.UTC().Format(time.RFC3339Nano))
		}
		for _, rf := range input.SearchIndexes.References {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_reference (tenant_id, res_type, res_id, sp_name, target_type, target_id) VALUES (?, ?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, rf.SpName, rf.TargetType, rf.TargetID)
		}
		for _, qt := range input.SearchIndexes.Quantities {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_quantity (tenant_id, res_type, res_id, sp_name, sp_system, sp_code, sp_value) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, qt.SpName, qt.SpSystem, qt.SpCode, qt.SpValue)
		}
		for _, u := range input.SearchIndexes.URIs {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_uri (tenant_id, res_type, res_id, sp_name, sp_value) VALUES (?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, u.SpName, u.SpValue)
		}
	}

	s.bumpEpochLocked(ctx)
	return &rec, nil
}

// WriteBatch writes a batch of FHIR resources in a single serialized SQLite WAL transaction.
func (s *Store) WriteBatch(ctx context.Context, inputs []store.ResourceInput) error {
	if len(inputs) == 0 {
		return nil
	}
	tenantID := inputs[0].TenantID
	for _, in := range inputs {
		if in.TenantID != tenantID {
			return store.ErrCrossTenantBatch
		}
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)
	for _, in := range inputs {
		if _, err := s.writeUnlocked(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

// ConditionalCreateResource evaluates an If-None-Exist search query under the SQLite write lock.
func (s *Store) ConditionalCreateResource(ctx context.Context, input store.ResourceInput, condParams store.SearchParams) (*store.ResourceRecord, bool, error) {
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	return s.conditionalCreateUnlocked(ctx, input, condParams)
}

func (s *Store) conditionalCreateUnlocked(ctx context.Context, input store.ResourceInput, condParams store.SearchParams) (*store.ResourceRecord, bool, error) {
	sr, err := s.searchSynced(ctx, condParams)
	if err != nil {
		return nil, false, err
	}
	if sr.Total > 1 {
		return nil, false, fmt.Errorf("%w: If-None-Exist matched %d existing %s resources", store.ErrPreconditionFailed, sr.Total, input.ResType)
	}
	if sr.Total == 1 && len(sr.Matches) == 1 {
		rec, err := s.readResourceWithMetaSynced(input.TenantID, input.ResType, sr.Matches[0].ResID)
		if err != nil {
			return nil, false, err
		}
		return rec, false, nil
	}
	rec, err := s.writeUnlocked(ctx, input)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// ConditionalUpdateResource evaluates a conditional update under the SQLite write lock.
func (s *Store) ConditionalUpdateResource(
	ctx context.Context,
	_, resType string,
	condParams store.SearchParams,
	buildInput func(matchedID string, isExisting bool) (store.ResourceInput, error),
) (*store.ResourceRecord, error) {
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	return s.conditionalUpdateUnlocked(ctx, resType, condParams, buildInput)
}

func (s *Store) conditionalUpdateUnlocked(
	ctx context.Context,
	resType string,
	condParams store.SearchParams,
	buildInput func(matchedID string, isExisting bool) (store.ResourceInput, error),
) (*store.ResourceRecord, error) {
	condParams.Count = 2
	condParams.Offset = 0
	sr, err := s.searchSynced(ctx, condParams)
	if err != nil {
		return nil, err
	}
	if sr.Total > 1 {
		return nil, fmt.Errorf("%w: conditional update matched %d existing %s resources", store.ErrPreconditionFailed, sr.Total, resType)
	}
	matchedID := ""
	isExisting := false
	if sr.Total == 1 && len(sr.Matches) == 1 {
		matchedID = sr.Matches[0].ResID
		isExisting = true
	}
	input, err := buildInput(matchedID, isExisting)
	if err != nil {
		return nil, err
	}
	return s.writeUnlocked(ctx, input)
}

// ConditionalDeleteResource evaluates a single-match conditional delete under the SQLite write lock.
func (s *Store) ConditionalDeleteResource(
	ctx context.Context,
	tenantID, resType string,
	condParams store.SearchParams,
	verifyMatch func(matchedID string) error,
) (*store.ResourceRecord, error) {
	recs, err := s.ConditionalDeleteResources(ctx, tenantID, resType, condParams, 1, verifyMatch)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

// ConditionalDeleteResources evaluates a conditional delete under the SQLite write lock.
func (s *Store) ConditionalDeleteResources(
	ctx context.Context,
	tenantID, resType string,
	condParams store.SearchParams,
	maxCount int,
	verifyMatch func(matchedID string) error,
) ([]store.ResourceRecord, error) {
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	if maxCount <= 1 {
		condParams.Count = 2
	} else {
		condParams.Count = maxCount
	}
	condParams.Offset = 0
	sr, err := s.searchSynced(ctx, condParams)
	if err != nil {
		return nil, err
	}
	if maxCount <= 1 && sr.Total > 1 {
		return nil, fmt.Errorf("%w: conditional delete matched %d existing %s resources", store.ErrPreconditionFailed, sr.Total, resType)
	}
	if sr.Total == 0 || len(sr.Matches) == 0 {
		if condParams.ExpectedVersion > 0 {
			return nil, fmt.Errorf("%w: conditional delete matched 0 resources (expected version %d)", store.ErrVersionConflict, condParams.ExpectedVersion)
		}
		return nil, nil
	}

	var deleted []store.ResourceRecord
	for _, m := range sr.Matches {
		if verifyMatch != nil {
			if err := verifyMatch(m.ResID); err != nil {
				return nil, err
			}
		}
		rec, err := s.deleteUnlocked(ctx, tenantID, resType, m.ResID, condParams.ExpectedVersion)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			deleted = append(deleted, *rec)
		}
	}
	return deleted, nil
}

// ExecuteTransactionBundle executes a FHIR transaction bundle atomically under the SQLite write lock.
func (s *Store) ExecuteTransactionBundle(ctx context.Context, ops []store.BundleOperation) ([]store.BundleOpResult, error) {
	if len(ops) == 0 {
		return []store.BundleOpResult{}, nil
	}
	tenantID := ops[0].Input.TenantID
	for _, op := range ops {
		if op.Input.TenantID != tenantID {
			return nil, store.ErrCrossTenantBatch
		}
	}

	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)
	baseCtx := ctx

	s.mu.RLock()
	snapResources := make(map[string]sqliteEntry, len(s.resources))
	for k, v := range s.resources {
		snapResources[k] = v
	}
	snapHistory := make(map[string][]store.ResourceRecord, len(s.history))
	for k, v := range s.history {
		cp := make([]store.ResourceRecord, len(v))
		copy(cp, v)
		snapHistory[k] = cp
	}
	s.mu.RUnlock()

	var sqlTx *sql.Tx
	if s.db != nil {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("sqlite begin transaction: %w", err)
		}
		sqlTx = tx
		ctx = context.WithValue(ctx, sqliteTxKey{}, sqlTx)
	}

	rollback := func() {
		if sqlTx != nil {
			_ = sqlTx.Rollback()
			sqlTx = nil
		}
		s.mu.Lock()
		s.resources = snapResources
		s.history = snapHistory
		s.mu.Unlock()
	}

	results := make([]store.BundleOpResult, 0, len(ops))
	for i, op := range ops {
		if op.OpType == store.BundleOpDelete || strings.EqualFold(op.Method, "DELETE") {
			targetID := op.Input.ResID
			if op.ConditionalParams != nil {
				condParams := *op.ConditionalParams
				condParams.Count = 2
				condParams.Offset = 0
				sr, err := s.searchSynced(ctx, condParams)
				if err != nil {
					rollback()
					return nil, fmt.Errorf("entry[%d] conditional delete search %s: %w", i, op.Input.ResType, err)
				}
				if sr.Total > 1 {
					rollback()
					return nil, fmt.Errorf("entry[%d] conditional delete %s: %w: matched %d existing resources", i, op.Input.ResType, store.ErrPreconditionFailed, sr.Total)
				}
				if sr.Total == 0 || len(sr.Matches) == 0 {
					if op.Input.ExpectedVersion > 0 {
						rollback()
						return nil, fmt.Errorf("entry[%d] conditional delete %s: %w: resource does not exist (expected version %d)", i, op.Input.ResType, store.ErrVersionConflict, op.Input.ExpectedVersion)
					}
					results = append(results, store.BundleOpResult{
						Status:  "204 No Content",
						ResType: op.Input.ResType,
					})
					continue
				}
				targetID = sr.Matches[0].ResID
				if op.VerifyDeleteMatch != nil {
					if err := op.VerifyDeleteMatch(targetID); err != nil {
						rollback()
						return nil, fmt.Errorf("entry[%d] conditional delete %s/%s: %w", i, op.Input.ResType, targetID, err)
					}
				}
			}
			rec, err := s.deleteUnlocked(ctx, op.Input.TenantID, op.Input.ResType, targetID, op.Input.ExpectedVersion)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				rollback()
				return nil, fmt.Errorf("entry[%d] delete %s/%s: %w", i, op.Input.ResType, targetID, err)
			}
			ver := 0
			var lastUpd time.Time
			if rec != nil {
				ver = rec.ResVersion
				lastUpd = rec.LastUpdated
			}
			results = append(results, store.BundleOpResult{
				Status:      "204 No Content",
				ResType:     op.Input.ResType,
				ResID:       targetID,
				ResVersion:  ver,
				LastUpdated: lastUpd,
				Record:      rec,
			})
			continue
		}

		if strings.EqualFold(op.Method, "POST") && op.IfNoneExist != nil {
			rec, created, err := s.conditionalCreateUnlocked(ctx, op.Input, *op.IfNoneExist)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("entry[%d] conditional create %s: %w", i, op.Input.ResType, err)
			}
			status := "200 OK"
			if created {
				status = "201 Created"
			}
			results = append(results, store.BundleOpResult{
				Status:        status,
				ResType:       rec.ResType,
				ResID:         rec.ResID,
				ResVersion:    rec.ResVersion,
				LastUpdated:   rec.LastUpdated,
				ResourceProto: rec.ResourceProto,
				Record:        rec,
				Created:       created,
			})
			continue
		}

		if strings.EqualFold(op.Method, "PUT") && op.ConditionalParams != nil && op.BuildWriteInput != nil {
			rec, err := s.conditionalUpdateUnlocked(ctx, op.Input.ResType, *op.ConditionalParams, op.BuildWriteInput)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("entry[%d] conditional update %s: %w", i, op.Input.ResType, err)
			}
			status := "200 OK"
			if rec.Created {
				status = "201 Created"
			}
			results = append(results, store.BundleOpResult{
				Status:        status,
				ResType:       rec.ResType,
				ResID:         rec.ResID,
				ResVersion:    rec.ResVersion,
				LastUpdated:   rec.LastUpdated,
				ResourceProto: rec.ResourceProto,
				Record:        rec,
				Created:       rec.Created,
			})
			continue
		}

		rec, err := s.writeUnlocked(ctx, op.Input)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("entry[%d] resource %s/%s: %w", i, op.Input.ResType, op.Input.ResID, err)
		}
		status := "200 OK"
		if strings.EqualFold(op.Method, "POST") || rec.Created {
			status = "201 Created"
		}
		results = append(results, store.BundleOpResult{
			Status:        status,
			ResType:       rec.ResType,
			ResID:         rec.ResID,
			ResVersion:    rec.ResVersion,
			LastUpdated:   rec.LastUpdated,
			ResourceProto: rec.ResourceProto,
			Record:        rec,
			Created:       rec.Created,
		})
	}
	if sqlTx != nil {
		if err := sqlTx.Commit(); err != nil {
			rollback()
			return nil, fmt.Errorf("sqlite commit transaction: %w", err)
		}
	}
	s.bumpEpochLocked(baseCtx)
	return results, nil
}

// ReadResource reads the raw proto bytes of an active resource.
func (s *Store) ReadResource(ctx context.Context, tenantID, resType, resID string) ([]byte, error) {
	rec, err := s.ReadResourceWithMeta(ctx, tenantID, resType, resID)
	if err != nil {
		return nil, err
	}
	return rec.ResourceProto, nil
}

// ReadResourceWithMeta reads a single resource and its version metadata using WITHOUT ROWID primary key lookup.
func (s *Store) ReadResourceWithMeta(ctx context.Context, tenantID, resType, resID string) (*store.ResourceRecord, error) {
	s.syncIfStale(ctx)
	return s.readResourceWithMetaSynced(tenantID, resType, resID)
}

func (s *Store) readResourceWithMetaSynced(tenantID, resType, resID string) (*store.ResourceRecord, error) {
	key := resKey(tenantID, resType, resID)
	s.mu.RLock()
	entry, ok := s.resources[key]
	s.mu.RUnlock()
	if !ok {
		return nil, sql.ErrNoRows
	}
	rec := &store.ResourceRecord{
		TenantID:      entry.tenantID,
		ResType:       entry.resType,
		ResID:         entry.resID,
		ResVersion:    entry.resVersion,
		LastUpdated:   entry.lastUpdated,
		ResourceProto: append([]byte(nil), entry.resourceProto...),
		IsDeleted:     entry.isDeleted,
	}
	if rec.IsDeleted {
		return rec, store.ErrResourceDeleted
	}
	return rec, nil
}

// ReadResourceVersion reads a specific historical version of a resource (FHIR vread).
func (s *Store) ReadResourceVersion(ctx context.Context, tenantID, resType, resID string, version int) (*store.ResourceRecord, error) {
	s.syncIfStale(ctx)
	key := resKey(tenantID, resType, resID)
	s.mu.RLock()
	versions := s.history[key]
	s.mu.RUnlock()
	for i := len(versions) - 1; i >= 0; i-- {
		if versions[i].ResVersion == version {
			rec := versions[i]
			rec.ResourceProto = append([]byte(nil), rec.ResourceProto...)
			if rec.IsDeleted {
				return &rec, store.ErrResourceDeleted
			}
			return &rec, nil
		}
	}
	return nil, sql.ErrNoRows
}

// ListResourceHistory returns version history ordered by version descending.
func (s *Store) ListResourceHistory(ctx context.Context, tenantID, resType, resID string, limit, offset int) ([]store.ResourceRecord, int, error) {
	return s.ListHistory(ctx, tenantID, resType, resID, nil, limit, offset)
}

// ListHistory returns version history at the instance, type, or system level.
func (s *Store) ListHistory(ctx context.Context, tenantID, resType, resID string, since *time.Time, limit, offset int) ([]store.ResourceRecord, int, error) {
	return s.ListHistoryWithParams(ctx, store.HistoryParams{
		TenantID: tenantID,
		ResType:  resType,
		ResID:    resID,
		Since:    since,
		Limit:    limit,
		Offset:   offset,
	})
}

// ListHistoryWithParams returns version history records filtered by HistoryParams.
func (s *Store) ListHistoryWithParams(ctx context.Context, params store.HistoryParams) ([]store.ResourceRecord, int, error) {
	s.syncIfStale(ctx)
	limit := params.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > store.MaxSearchCount {
		limit = store.MaxSearchCount
	}
	offset := max(params.Offset, 0)

	s.mu.RLock()
	var matched []store.ResourceRecord
	for _, versions := range s.history {
		for _, rec := range versions {
			if rec.TenantID != params.TenantID {
				continue
			}
			if params.ResType != "" && rec.ResType != params.ResType {
				continue
			}
			if params.ResID != "" && rec.ResID != params.ResID {
				continue
			}
			if params.Since != nil && !params.Since.IsZero() && rec.LastUpdated.Before(*params.Since) {
				continue
			}
			matched = append(matched, rec)
		}
	}
	s.mu.RUnlock()

	sort.Slice(matched, func(i, j int) bool {
		if params.ResType != "" && params.ResID != "" {
			return matched[i].ResVersion > matched[j].ResVersion
		}
		if !matched[i].LastUpdated.Equal(matched[j].LastUpdated) {
			return matched[i].LastUpdated.After(matched[j].LastUpdated)
		}
		return matched[i].ResVersion > matched[j].ResVersion
	})

	total := len(matched)
	if offset >= total {
		return []store.ResourceRecord{}, total, nil
	}
	return matched[offset:min(offset+limit, total)], total, nil
}

// DeleteResource soft-deletes a resource.
func (s *Store) DeleteResource(ctx context.Context, tenantID, resType, resID string) error {
	_, err := s.DeleteResourceWithMeta(ctx, tenantID, resType, resID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// DeleteResourceWithMeta soft-deletes a resource and returns the tombstone version record.
func (s *Store) DeleteResourceWithMeta(ctx context.Context, tenantID, resType, resID string) (*store.ResourceRecord, error) {
	return s.DeleteResourceWithExpectedVersion(ctx, tenantID, resType, resID, 0)
}

// DeleteResourceWithExpectedVersion soft-deletes a resource while enforcing If-Match when expectedVersion > 0.
func (s *Store) DeleteResourceWithExpectedVersion(ctx context.Context, tenantID, resType, resID string, expectedVersion int) (*store.ResourceRecord, error) {
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	return s.deleteUnlocked(ctx, tenantID, resType, resID, expectedVersion)
}

func (s *Store) deleteUnlocked(ctx context.Context, tenantID, resType, resID string, expectedVersion int) (*store.ResourceRecord, error) {
	key := resKey(tenantID, resType, resID)
	s.mu.Lock()
	existing, exists := s.resources[key]
	if !exists {
		s.mu.Unlock()
		if expectedVersion > 0 {
			return nil, fmt.Errorf("%w: resource does not exist (expected version %d)", store.ErrVersionConflict, expectedVersion)
		}
		return nil, sql.ErrNoRows
	}
	if expectedVersion > 0 && (existing.isDeleted || existing.resVersion != expectedVersion) {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: expected version %d, current version %d", store.ErrVersionConflict, expectedVersion, existing.resVersion)
	}
	if existing.isDeleted {
		rec := store.ResourceRecord{
			TenantID:    tenantID,
			ResType:     resType,
			ResID:       resID,
			ResVersion:  existing.resVersion,
			LastUpdated: existing.lastUpdated,
			IsDeleted:   true,
		}
		s.mu.Unlock()
		return &rec, nil
	}

	newVersion := existing.resVersion + 1
	now := time.Now().UTC()
	existing.resVersion = newVersion
	existing.isDeleted = true
	existing.lastUpdated = now
	existing.resourceProto = nil
	existing.indexes = nil

	tombstoneRec := store.ResourceRecord{
		TenantID:    tenantID,
		ResType:     resType,
		ResID:       resID,
		ResVersion:  newVersion,
		LastUpdated: now,
		IsDeleted:   true,
	}
	s.resources[key] = existing
	s.history[key] = append(s.history[key], tombstoneRec)
	s.mu.Unlock()

	_ = s.execSQLite(ctx, `UPDATE fhir_resource SET res_version = ?, is_deleted = 1, last_updated = ?, resource_proto = X'' WHERE tenant_id = ? AND res_type = ? AND res_id = ?`,
		newVersion, now.Format(time.RFC3339Nano), tenantID, resType, resID)
	_ = s.execSQLite(ctx, `INSERT INTO fhir_resource_history (tenant_id, res_type, res_id, res_version, is_deleted, last_updated, resource_proto) VALUES (?, ?, ?, ?, 1, ?, NULL)`,
		tenantID, resType, resID, newVersion, now.Format(time.RFC3339Nano))
	for _, tbl := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri"} {
		_ = s.execSQLite(ctx, fmt.Sprintf("DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?", tbl),
			tenantID, resType, resID)
	}
	s.bumpEpochLocked(ctx)

	return &tombstoneRec, nil
}

// Reindex rebuilds spidx_* B-tree index rows for matching active resources in SQLite.
func (s *Store) Reindex(ctx context.Context, params store.ReindexParams) (*store.ReindexResult, error) {
	if params.ResType != "" && params.ResID != "" {
		if _, err := s.ReadResourceWithMeta(ctx, params.TenantID, params.ResType, params.ResID); err != nil {
			return nil, err
		}
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	out := &store.ReindexResult{
		ByResourceType: make(map[string]int),
	}

	var updatedEntries []sqliteEntry
	s.mu.Lock()
	for k, entry := range s.resources {
		if entry.tenantID != params.TenantID || entry.isDeleted {
			continue
		}
		if params.ResType != "" && entry.resType != params.ResType {
			continue
		}
		if params.ResID != "" && entry.resID != params.ResID {
			continue
		}
		extractor := params.Extractors[entry.resType]
		if extractor == nil {
			continue
		}
		idx, err := extractor(params.TenantID, entry.resID, entry.resourceProto)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		entry.indexes = cloneSearchIndexes(idx)
		entry.patientID, entry.encounterID = extractCompartmentLinks(entry.resType, entry.resID, idx)
		s.resources[k] = entry
		updatedEntries = append(updatedEntries, entry)
		out.ReindexedResources++
		out.ByResourceType[entry.resType]++
	}
	s.mu.Unlock()

	for _, entry := range updatedEntries {
		for _, tbl := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri"} {
			_ = s.execSQLite(ctx, fmt.Sprintf("DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?", tbl),
				entry.tenantID, entry.resType, entry.resID)
		}
		if entry.indexes == nil {
			continue
		}
		for _, st := range entry.indexes.Strings {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_string (tenant_id, res_type, res_id, sp_name, sp_value, sp_value_lower) VALUES (?, ?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, st.SpName, st.SpValue, strings.ToLower(st.SpValue))
		}
		for _, tk := range entry.indexes.Tokens {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_token (tenant_id, res_type, res_id, sp_name, sp_system, sp_value) VALUES (?, ?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, tk.SpName, tk.SpSystem, tk.SpValue)
		}
		for _, dt := range entry.indexes.Dates {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_date (tenant_id, res_type, res_id, sp_name, sp_low, sp_high) VALUES (?, ?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, dt.SpName, dt.SpLow.UTC().Format(time.RFC3339Nano), dt.SpHigh.UTC().Format(time.RFC3339Nano))
		}
		for _, rf := range entry.indexes.References {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_reference (tenant_id, res_type, res_id, sp_name, target_type, target_id) VALUES (?, ?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, rf.SpName, rf.TargetType, rf.TargetID)
		}
		for _, qt := range entry.indexes.Quantities {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_quantity (tenant_id, res_type, res_id, sp_name, sp_system, sp_code, sp_value) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, qt.SpName, qt.SpSystem, qt.SpCode, qt.SpValue)
		}
		for _, u := range entry.indexes.URIs {
			_ = s.execSQLite(ctx, `INSERT INTO spidx_uri (tenant_id, res_type, res_id, sp_name, sp_value) VALUES (?, ?, ?, ?, ?)`,
				entry.tenantID, entry.resType, entry.resID, u.SpName, u.SpValue)
		}
	}
	if len(updatedEntries) > 0 {
		s.bumpEpochLocked(ctx)
	}
	return out, nil
}

// Search executes a FHIR search query across the 6 spidx_* categories.
func (s *Store) Search(ctx context.Context, params store.SearchParams) (*store.SearchResults, error) {
	s.syncIfStale(ctx)
	return s.searchSynced(ctx, params)
}

func (s *Store) searchSynced(ctx context.Context, params store.SearchParams) (*store.SearchResults, error) {
	_ = s.execSQLite(ctx, `SELECT res_type, res_id, res_version, last_updated, resource_proto FROM fhir_resource WHERE tenant_id = ? AND is_deleted = 0`, params.TenantID)

	s.mu.RLock()
	var matches []store.SearchResult
	for _, entry := range s.resources {
		if !s.matchesSearchParams(entry, params) {
			continue
		}
		matches = append(matches, store.SearchResult{
			ResType:       entry.resType,
			ResID:         entry.resID,
			ResVersion:    entry.resVersion,
			LastUpdated:   entry.lastUpdated,
			ResourceProto: append([]byte(nil), entry.resourceProto...),
		})
	}
	store.SortSearchResults(params.TenantID, matches, params.Sort, s.lookupIndexedViewLocked)
	s.mu.RUnlock()

	total := len(matches)
	if strings.EqualFold(params.Summary, "count") {
		return &store.SearchResults{
			Matches: []store.SearchResult{},
			Total:   total,
		}, nil
	}

	count := params.Count
	if count <= 0 {
		count = 20
	}
	if count > store.MaxSearchCount {
		count = store.MaxSearchCount
	}
	offset := max(params.Offset, 0)
	if offset >= total {
		matches = []store.SearchResult{}
	} else {
		matches = matches[offset:min(offset+count, total)]
	}

	var includes []store.SearchResult
	if len(matches) > 0 && (len(params.Includes) > 0 || len(params.RevIncludes) > 0) {
		s.mu.RLock()
		includes = store.ResolveSearchIncludes(
			params.TenantID,
			matches,
			params.Includes,
			params.RevIncludes,
			s.lookupIndexedViewLocked,
			s.listTenantViewsLocked,
		)
		s.mu.RUnlock()
	}

	return &store.SearchResults{
		Matches:  matches,
		Includes: includes,
		Total:    total,
	}, nil
}

func toIndexedView(entry sqliteEntry) store.IndexedResourceView {
	return store.IndexedResourceView{
		TenantID:      entry.tenantID,
		ResType:       entry.resType,
		ResID:         entry.resID,
		ResVersion:    entry.resVersion,
		LastUpdated:   entry.lastUpdated,
		PatientID:     entry.patientID,
		EncounterID:   entry.encounterID,
		ResourceProto: entry.resourceProto,
		Indexes:       entry.indexes,
	}
}

func (s *Store) lookupIndexedViewLocked(tenantID, resType, resID string) (store.IndexedResourceView, bool) {
	if resType != "" {
		if entry, ok := s.resources[resKey(tenantID, resType, resID)]; ok && !entry.isDeleted {
			return toIndexedView(entry), true
		}
		for _, entry := range s.resources {
			if entry.tenantID == tenantID && !entry.isDeleted && entry.resID == resID && strings.EqualFold(entry.resType, resType) {
				return toIndexedView(entry), true
			}
		}
		return store.IndexedResourceView{}, false
	}
	for _, entry := range s.resources {
		if entry.tenantID == tenantID && !entry.isDeleted && entry.resID == resID {
			return toIndexedView(entry), true
		}
	}
	return store.IndexedResourceView{}, false
}

func (s *Store) listTenantViewsLocked(tenantID string) []store.IndexedResourceView {
	out := make([]store.IndexedResourceView, 0, len(s.resources))
	for _, entry := range s.resources {
		if entry.tenantID == tenantID && !entry.isDeleted {
			out = append(out, toIndexedView(entry))
		}
	}
	return out
}

func (s *Store) appendIncludeByTargetLocked(tenantID, targetType, targetID string, seen map[string]bool, includes *[]store.SearchResult) {
	if targetID == "" {
		return
	}
	if targetType != "" {
		key := resKey(tenantID, targetType, targetID)
		if seen[key] {
			return
		}
		if entry, ok := s.resources[key]; ok && !entry.isDeleted {
			seen[key] = true
			*includes = append(*includes, store.SearchResult{
				ResType:       entry.resType,
				ResID:         entry.resID,
				ResVersion:    entry.resVersion,
				LastUpdated:   entry.lastUpdated,
				ResourceProto: append([]byte(nil), entry.resourceProto...),
			})
		}
		return
	}
	for key, entry := range s.resources {
		if entry.tenantID == tenantID && !entry.isDeleted && entry.resID == targetID && !seen[key] {
			seen[key] = true
			*includes = append(*includes, store.SearchResult{
				ResType:       entry.resType,
				ResID:         entry.resID,
				ResVersion:    entry.resVersion,
				LastUpdated:   entry.lastUpdated,
				ResourceProto: append([]byte(nil), entry.resourceProto...),
			})
		}
	}
}

func (s *Store) matchesSearchParams(entry sqliteEntry, params store.SearchParams) bool {
	if entry.tenantID != params.TenantID || entry.isDeleted {
		return false
	}
	if params.ResType != "" && !strings.EqualFold(entry.resType, params.ResType) {
		return false
	}
	if len(params.ResTypes) > 0 {
		found := false
		for _, rt := range params.ResTypes {
			if strings.EqualFold(entry.resType, rt) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if params.ID != "" {
		matchedID := false
		for part := range strings.SplitSeq(params.ID, ",") {
			cleaned := strings.TrimSpace(part)
			if slash := strings.LastIndexByte(cleaned, '/'); slash != -1 {
				cleaned = cleaned[slash+1:]
			}
			if cleaned == entry.resID {
				matchedID = true
				break
			}
		}
		if !matchedID {
			return false
		}
	}

	for spName, val := range params.Tokens {
		if !matchToken(entry.indexes, spName, val) {
			return false
		}
	}
	for spName, vals := range params.TokenFilters {
		for _, v := range vals {
			if !matchToken(entry.indexes, spName, v) {
				return false
			}
		}
	}
	for spName, ref := range params.References {
		if !s.matchRef(params.TenantID, entry.indexes, spName, ref) {
			return false
		}
	}
	for spName, refs := range params.ReferenceFilters {
		for _, r := range refs {
			if !s.matchRef(params.TenantID, entry.indexes, spName, r) {
				return false
			}
		}
	}
	for spName, strVal := range params.Strings {
		if !matchString(entry.indexes, spName, strVal) {
			return false
		}
	}
	for spName, strVals := range params.StringFilters {
		for _, sv := range strVals {
			if !matchString(entry.indexes, spName, sv) {
				return false
			}
		}
	}
	for spName, uriVal := range params.URIs {
		if !matchURI(entry.indexes, spName, uriVal) {
			return false
		}
	}
	for spName, uriVals := range params.URIFilters {
		for _, uv := range uriVals {
			if !matchURI(entry.indexes, spName, uv) {
				return false
			}
		}
	}
	return store.EvaluateAdvancedSearchParams(toIndexedView(entry), params, s.lookupIndexedViewLocked, s.listTenantViewsLocked)
}

func matchURI(idx *store.SearchIndexes, spName, rawURI string) bool {
	if idx == nil {
		return false
	}
	baseName, mod, _ := strings.Cut(spName, ":")
	for item := range strings.SplitSeq(rawURI, ",") {
		want := strings.TrimSpace(item)
		for _, u := range idx.URIs {
			if u.SpName != baseName {
				continue
			}
			switch mod {
			case "below":
				if strings.HasPrefix(u.SpValue, strings.TrimSuffix(want, "/")) {
					return true
				}
			case "above":
				if strings.HasPrefix(strings.TrimSuffix(want, "/"), strings.TrimSuffix(u.SpValue, "/")) {
					return true
				}
			default:
				if u.SpValue == want {
					return true
				}
			}
		}
	}
	return false
}

func matchToken(idx *store.SearchIndexes, spName, rawVal string) bool {
	baseName := spName
	negate := false
	if strings.HasSuffix(baseName, ":not") {
		baseName = strings.TrimSuffix(baseName, ":not")
		negate = true
	}
	isOfType := strings.HasSuffix(baseName, ":of-type")
	matched := false
	if idx != nil {
		for item := range strings.SplitSeq(rawVal, ",") {
			item = strings.TrimSpace(item)
			if isOfType {
				if parts := strings.SplitN(item, "|", 3); len(parts) == 3 {
					sys := parts[0] + "|" + parts[1]
					code := parts[2]
					for _, t := range idx.Tokens {
						if t.SpName == baseName && t.SpSystem == sys && t.SpValue == code {
							matched = true
							break
						}
					}
					if matched {
						break
					}
				}
				continue
			}
			sys := ""
			code := item
			hasSys := false
			if parts := strings.SplitN(item, "|", 2); len(parts) == 2 {
				sys, code = parts[0], parts[1]
				hasSys = true
			}
			for _, t := range idx.Tokens {
				if t.SpName != baseName {
					continue
				}
				if hasSys {
					if t.SpSystem == sys && t.SpValue == code {
						matched = true
						break
					}
				} else if strings.EqualFold(t.SpValue, code) {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
	}
	if negate {
		return !matched
	}
	return matched
}

func (s *Store) matchRef(tenantID string, idx *store.SearchIndexes, spName, rawRef string) bool {
	if idx == nil {
		return false
	}
	if strings.HasSuffix(spName, ":identifier") {
		if matchToken(idx, spName, rawRef) {
			return true
		}
		baseName := strings.TrimSuffix(spName, ":identifier")
		for _, r := range idx.References {
			if r.SpName != baseName {
				continue
			}
			if r.TargetType != "" {
				if target, ok := s.resources[resKey(tenantID, r.TargetType, r.TargetID)]; ok && !target.isDeleted {
					if matchToken(target.indexes, "identifier", rawRef) {
						return true
					}
				}
			} else {
				for _, target := range s.resources {
					if target.tenantID == tenantID && !target.isDeleted && target.resID == r.TargetID {
						if matchToken(target.indexes, "identifier", rawRef) {
							return true
						}
					}
				}
			}
		}
		return false
	}
	for item := range strings.SplitSeq(rawRef, ",") {
		item = strings.TrimSpace(item)
		targetID := item
		if slash := strings.LastIndexByte(item, '/'); slash != -1 {
			targetID = item[slash+1:]
		}
		for _, r := range idx.References {
			if r.SpName == spName && (r.TargetID == targetID || r.TargetType+"/"+r.TargetID == item) {
				return true
			}
		}
	}
	return false
}

func matchString(idx *store.SearchIndexes, spName, rawStr string) bool {
	if idx == nil {
		return false
	}
	baseName := strings.Split(spName, ":")[0]
	exact := strings.HasSuffix(spName, ":exact")
	contains := strings.HasSuffix(spName, ":contains") || strings.HasSuffix(spName, ":text")
	isText := strings.HasSuffix(spName, ":text")
	for _, item := range store.SplitSearchORValues(rawStr) {
		needle := strings.ToLower(strings.TrimSpace(store.UnescapeFHIRSearchString(item)))
		for _, st := range idx.Strings {
			if st.SpName != baseName && (!isText || st.SpName != baseName+":text") {
				continue
			}
			val := strings.ToLower(strings.TrimSpace(st.SpValue))
			if exact && val == needle {
				return true
			}
			if contains && strings.Contains(val, needle) {
				return true
			}
			if !exact && !contains && strings.HasPrefix(val, needle) {
				return true
			}
		}
	}
	return false
}

func matchesEverythingFilters(resType string, lastUpdated time.Time, allowedTypes []string, since, until *time.Time) bool {
	if len(allowedTypes) > 0 {
		found := false
		for _, rt := range allowedTypes {
			if strings.EqualFold(rt, resType) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if since != nil && lastUpdated.Before(*since) {
		return false
	}
	if until != nil && lastUpdated.After(*until) {
		return false
	}
	return true
}

func paginateEverythingIncludes(includes []store.SearchResult, offset, count int) ([]store.SearchResult, int) {
	sort.Slice(includes, func(i, j int) bool {
		if !includes[i].LastUpdated.Equal(includes[j].LastUpdated) {
			return includes[i].LastUpdated.After(includes[j].LastUpdated)
		}
		return includes[i].ResID < includes[j].ResID
	})
	compTotal := len(includes)
	if count <= 0 {
		count = 100
	}
	if count > store.MaxSearchCount {
		count = store.MaxSearchCount
	}
	off := max(offset, 0)
	if off >= compTotal {
		return []store.SearchResult{}, compTotal
	}
	return includes[off:min(off+count, compTotal)], compTotal
}

// PatientEverything returns the target Patient and all compartment-linked resources.
func (s *Store) PatientEverything(ctx context.Context, params store.PatientEverythingParams) (*store.SearchResults, error) {
	patRec, err := s.ReadResourceWithMeta(ctx, params.TenantID, "Patient", params.PatientID)
	if err != nil {
		return nil, err
	}
	var matches []store.SearchResult
	if matchesEverythingFilters("Patient", patRec.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
		matches = append(matches, store.SearchResult{
			ResType:       "Patient",
			ResID:         patRec.ResID,
			ResVersion:    patRec.ResVersion,
			LastUpdated:   patRec.LastUpdated,
			ResourceProto: patRec.ResourceProto,
		})
	}
	s.mu.RLock()
	compKeys := map[string]bool{
		resKey(params.TenantID, "Patient", params.PatientID): true,
	}
	refTargets := make(map[string]bool)
	if patEntry, ok := s.resources[resKey(params.TenantID, "Patient", params.PatientID)]; ok && patEntry.indexes != nil {
		for _, ref := range patEntry.indexes.References {
			if ref.TargetType != "" && ref.TargetID != "" {
				refTargets[resKey(params.TenantID, ref.TargetType, ref.TargetID)] = true
			}
		}
	}
	for key, entry := range s.resources {
		if entry.tenantID != params.TenantID || entry.isDeleted || entry.resType == "Patient" {
			continue
		}
		if entry.patientID == params.PatientID {
			compKeys[key] = true
			if entry.indexes != nil {
				for _, ref := range entry.indexes.References {
					if ref.TargetType != "" && ref.TargetID != "" {
						refTargets[resKey(params.TenantID, ref.TargetType, ref.TargetID)] = true
					}
				}
			}
		}
	}
	for key, entry := range s.resources {
		if entry.tenantID != params.TenantID || entry.isDeleted || compKeys[key] || entry.resType == "Patient" {
			continue
		}
		if refTargets[key] {
			compKeys[key] = true
			continue
		}
		if entry.resType == "Provenance" && entry.indexes != nil {
			for _, ref := range entry.indexes.References {
				if compKeys[resKey(params.TenantID, ref.TargetType, ref.TargetID)] {
					compKeys[key] = true
					break
				}
			}
		}
	}
	var includes []store.SearchResult
	for key := range compKeys {
		entry, ok := s.resources[key]
		if !ok || entry.resType == "Patient" {
			continue
		}
		if !matchesEverythingFilters(entry.resType, entry.lastUpdated, params.ResourceTypes, params.Since, params.Until) {
			continue
		}
		includes = append(includes, store.SearchResult{
			ResType:       entry.resType,
			ResID:         entry.resID,
			ResVersion:    entry.resVersion,
			LastUpdated:   entry.lastUpdated,
			ResourceProto: append([]byte(nil), entry.resourceProto...),
		})
	}
	s.mu.RUnlock()
	pagedIncludes, compTotal := paginateEverythingIncludes(includes, params.Offset, params.Count)
	return &store.SearchResults{
		Matches:  matches,
		Includes: pagedIncludes,
		Total:    len(matches) + compTotal,
	}, nil
}

// EncounterEverything returns the target Encounter and all compartment-linked resources.
func (s *Store) EncounterEverything(ctx context.Context, params store.EncounterEverythingParams) (*store.SearchResults, error) {
	encRec, err := s.ReadResourceWithMeta(ctx, params.TenantID, "Encounter", params.EncounterID)
	if err != nil {
		return nil, err
	}
	var matches []store.SearchResult
	if matchesEverythingFilters("Encounter", encRec.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
		matches = append(matches, store.SearchResult{
			ResType:       "Encounter",
			ResID:         encRec.ResID,
			ResVersion:    encRec.ResVersion,
			LastUpdated:   encRec.LastUpdated,
			ResourceProto: encRec.ResourceProto,
		})
	}
	focalKey := resKey(params.TenantID, "Encounter", params.EncounterID)
	s.mu.RLock()
	compKeys := map[string]bool{focalKey: true}
	refTargets := make(map[string]bool)
	if encEntry, ok := s.resources[focalKey]; ok && encEntry.indexes != nil {
		for _, ref := range encEntry.indexes.References {
			if ref.TargetType != "" && ref.TargetID != "" {
				refTargets[resKey(params.TenantID, ref.TargetType, ref.TargetID)] = true
			}
		}
	}
	for key, entry := range s.resources {
		if entry.tenantID != params.TenantID || entry.isDeleted || key == focalKey {
			continue
		}
		if entry.encounterID == params.EncounterID {
			compKeys[key] = true
			if entry.indexes != nil {
				for _, ref := range entry.indexes.References {
					if ref.TargetType != "" && ref.TargetID != "" {
						refTargets[resKey(params.TenantID, ref.TargetType, ref.TargetID)] = true
					}
				}
			}
		}
	}
	for key, entry := range s.resources {
		if entry.tenantID != params.TenantID || entry.isDeleted || compKeys[key] {
			continue
		}
		if refTargets[key] {
			compKeys[key] = true
		}
	}
	var includes []store.SearchResult
	for key := range compKeys {
		if key == focalKey {
			continue
		}
		entry, ok := s.resources[key]
		if !ok || !matchesEverythingFilters(entry.resType, entry.lastUpdated, params.ResourceTypes, params.Since, params.Until) {
			continue
		}
		includes = append(includes, store.SearchResult{
			ResType:       entry.resType,
			ResID:         entry.resID,
			ResVersion:    entry.resVersion,
			LastUpdated:   entry.lastUpdated,
			ResourceProto: append([]byte(nil), entry.resourceProto...),
		})
	}
	s.mu.RUnlock()
	pagedIncludes, compTotal := paginateEverythingIncludes(includes, params.Offset, params.Count)
	return &store.SearchResults{
		Matches:  matches,
		Includes: pagedIncludes,
		Total:    len(matches) + compTotal,
	}, nil
}

// GroupEverything returns the focal Group and cohort resources.
func (s *Store) GroupEverything(ctx context.Context, params store.GroupEverythingParams) (*store.SearchResults, error) {
	s.syncIfStale(ctx)
	var matches []store.SearchResult
	if grpRec, err := s.ReadResourceWithMeta(ctx, params.TenantID, "Group", params.GroupID); err == nil {
		if matchesEverythingFilters("Group", grpRec.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
			matches = append(matches, store.SearchResult{
				ResType:       "Group",
				ResID:         grpRec.ResID,
				ResVersion:    grpRec.ResVersion,
				LastUpdated:   grpRec.LastUpdated,
				ResourceProto: grpRec.ResourceProto,
			})
		}
	} else if !params.AllPatients {
		return nil, err
	}
	patientSet := make(map[string]bool, len(params.PatientIDs))
	for _, pid := range params.PatientIDs {
		patientSet[pid] = true
	}
	s.mu.RLock()
	var includes []store.SearchResult
	for _, entry := range s.resources {
		if entry.tenantID != params.TenantID || entry.isDeleted || entry.resType == "Group" {
			continue
		}
		inCohort := params.AllPatients || patientSet[entry.patientID] || (entry.resType == "Patient" && patientSet[entry.resID])
		if !inCohort || !matchesEverythingFilters(entry.resType, entry.lastUpdated, params.ResourceTypes, params.Since, params.Until) {
			continue
		}
		includes = append(includes, store.SearchResult{
			ResType:       entry.resType,
			ResID:         entry.resID,
			ResVersion:    entry.resVersion,
			LastUpdated:   entry.lastUpdated,
			ResourceProto: append([]byte(nil), entry.resourceProto...),
		})
	}
	s.mu.RUnlock()
	pagedIncludes, compTotal := paginateEverythingIncludes(includes, params.Offset, params.Count)
	return &store.SearchResults{
		Matches:  matches,
		Includes: pagedIncludes,
		Total:    len(matches) + compTotal,
	}, nil
}

// BelongsToPatientCompartment checks whether a resource belongs to patientID's compartment.
func (s *Store) BelongsToPatientCompartment(ctx context.Context, tenantID, resType, resID, patientID string) (bool, error) {
	if strings.EqualFold(resType, "Patient") {
		return resID == patientID, nil
	}
	if !store.IsPatientCompartmentResourceType(resType) {
		return true, nil
	}
	s.syncIfStale(ctx)
	key := resKey(tenantID, resType, resID)
	s.mu.RLock()
	entry, ok := s.resources[key]
	s.mu.RUnlock()
	if !ok {
		return false, nil
	}
	return entry.patientID == patientID, nil
}

// SearchPatientMatchCandidates returns candidate Patient resources for $match and $member-match.
func (s *Store) SearchPatientMatchCandidates(ctx context.Context, params store.PatientMatchCandidateParams) ([]store.SearchResult, error) {
	s.syncIfStale(ctx)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []store.SearchResult
	for _, entry := range s.resources {
		if entry.tenantID != params.TenantID || entry.isDeleted || entry.resType != "Patient" {
			continue
		}
		if params.RestrictedPatientID != "" && entry.resID != params.RestrictedPatientID {
			continue
		}
		out = append(out, store.SearchResult{
			ResType:       "Patient",
			ResID:         entry.resID,
			ResVersion:    entry.resVersion,
			LastUpdated:   entry.lastUpdated,
			ResourceProto: append([]byte(nil), entry.resourceProto...),
		})
	}
	return out, nil
}

// ResourceGraph traverses GraphDefinition links starting from (ResourceType, ResourceID).
func (s *Store) ResourceGraph(ctx context.Context, params store.ResourceGraphParams) (*store.SearchResults, error) {
	focal, err := s.ReadResourceWithMeta(ctx, params.TenantID, params.ResourceType, params.ResourceID)
	if err != nil {
		return nil, err
	}
	matches := []store.SearchResult{{
		ResType:       focal.ResType,
		ResID:         focal.ResID,
		ResVersion:    focal.ResVersion,
		LastUpdated:   focal.LastUpdated,
		ResourceProto: focal.ResourceProto,
	}}
	focalKey := resKey(params.TenantID, params.ResourceType, params.ResourceID)
	s.mu.RLock()
	seen := map[string]bool{focalKey: true}
	if focalEntry, ok := s.resources[focalKey]; ok && focalEntry.indexes != nil {
		for _, ref := range focalEntry.indexes.References {
			s.appendIncludeByTargetLocked(params.TenantID, ref.TargetType, ref.TargetID, seen, &matches)
		}
	}
	for key, entry := range s.resources {
		if entry.tenantID != params.TenantID || entry.isDeleted || seen[key] {
			continue
		}
		linked := (strings.EqualFold(params.ResourceType, "Patient") && entry.patientID == params.ResourceID) ||
			(strings.EqualFold(params.ResourceType, "Encounter") && entry.encounterID == params.ResourceID)
		if !linked && entry.indexes != nil {
			for _, ref := range entry.indexes.References {
				if ref.TargetID == params.ResourceID && (ref.TargetType == "" || strings.EqualFold(ref.TargetType, params.ResourceType)) {
					linked = true
					break
				}
			}
		}
		if linked {
			seen[key] = true
			matches = append(matches, store.SearchResult{
				ResType:       entry.resType,
				ResID:         entry.resID,
				ResVersion:    entry.resVersion,
				LastUpdated:   entry.lastUpdated,
				ResourceProto: append([]byte(nil), entry.resourceProto...),
			})
		}
	}
	s.mu.RUnlock()
	return &store.SearchResults{
		Matches: matches,
		Total:   len(matches),
	}, nil
}

// AggregateMeta returns distinct _profile, _tag, and _security values across active resources.
func (s *Store) AggregateMeta(ctx context.Context, tenantID, resType string) (*store.AggregatedMeta, error) {
	s.syncIfStale(ctx)
	out := &store.AggregatedMeta{}
	profSeen := make(map[string]bool)
	tagSeen := make(map[string]bool)
	secSeen := make(map[string]bool)

	s.mu.RLock()
	for _, entry := range s.resources {
		if entry.tenantID != tenantID || entry.isDeleted {
			continue
		}
		if resType != "" && entry.resType != resType {
			continue
		}
		if entry.indexes == nil {
			continue
		}
		for _, u := range entry.indexes.URIs {
			if u.SpName == "_profile" && u.SpValue != "" && !profSeen[u.SpValue] {
				profSeen[u.SpValue] = true
				out.Profiles = append(out.Profiles, u.SpValue)
			}
		}
		for _, t := range entry.indexes.Tokens {
			key := t.SpSystem + "|" + t.SpValue
			if t.SpName == "_tag" && !tagSeen[key] {
				tagSeen[key] = true
				out.Tags = append(out.Tags, store.MetaCodingPair{System: t.SpSystem, Code: t.SpValue})
			} else if t.SpName == "_security" && !secSeen[key] {
				secSeen[key] = true
				out.Security = append(out.Security, store.MetaCodingPair{System: t.SpSystem, Code: t.SpValue})
			}
		}
	}
	s.mu.RUnlock()
	sort.Strings(out.Profiles)
	return out, nil
}

// ExportResources retrieves all active FHIR resources matching ExportParams.
func (s *Store) ExportResources(ctx context.Context, params store.ExportParams) ([]store.SearchResult, error) {
	var out []store.SearchResult
	err := s.StreamExportResources(ctx, params, func(r store.SearchResult) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

// StreamExportResources streams active FHIR resources matching ExportParams.
func (s *Store) StreamExportResources(ctx context.Context, params store.ExportParams, emit func(store.SearchResult) error) error {
	return s.StreamAnalyticalExport(ctx, params, emit)
}

// StreamAnalyticalExport implements store.AnalyticalStore for SQLite.
func (s *Store) StreamAnalyticalExport(ctx context.Context, params store.ExportParams, emit func(store.SearchResult) error) error {
	if params.EmptyCohort {
		return nil
	}
	s.syncIfStale(ctx)
	_ = s.execSQLite(ctx, `SELECT res_type, res_id, res_version, last_updated, resource_proto FROM fhir_resource WHERE tenant_id = ? AND is_deleted = 0 ORDER BY res_type ASC, res_id ASC`, params.TenantID)

	typeFilter := make(map[string]bool, len(params.ResourceTypes))
	for _, rt := range params.ResourceTypes {
		typeFilter[rt] = true
	}
	patFilter := make(map[string]bool, len(params.PatientIDs))
	for _, pid := range params.PatientIDs {
		cleaned := strings.TrimSpace(pid)
		if slash := strings.LastIndexByte(cleaned, '/'); slash != -1 {
			cleaned = cleaned[slash+1:]
		}
		if cleaned != "" {
			patFilter[cleaned] = true
		}
	}
	if params.PatientID != "" {
		patFilter[params.PatientID] = true
	}

	s.mu.RLock()
	var results []store.SearchResult
	for _, entry := range s.resources {
		if entry.tenantID != params.TenantID || entry.isDeleted {
			continue
		}
		if len(typeFilter) > 0 && !typeFilter[entry.resType] {
			continue
		}
		if len(patFilter) > 0 && !patFilter[entry.patientID] {
			continue
		}
		if params.Since != nil && entry.lastUpdated.Before(*params.Since) {
			continue
		}
		if params.Until != nil && entry.lastUpdated.After(*params.Until) {
			continue
		}
		results = append(results, store.SearchResult{
			ResType:       entry.resType,
			ResID:         entry.resID,
			ResVersion:    entry.resVersion,
			LastUpdated:   entry.lastUpdated,
			ResourceProto: append([]byte(nil), entry.resourceProto...),
		})
	}
	s.mu.RUnlock()

	sort.Slice(results, func(i, j int) bool {
		if results[i].ResType != results[j].ResType {
			return results[i].ResType < results[j].ResType
		}
		return results[i].ResID < results[j].ResID
	})

	for _, r := range results {
		if err := emit(r); err != nil {
			return err
		}
	}
	return nil
}

// EvaluateObservationStats implements store.AnalyticalStore over SQLite spidx_quantity B-tree indexes.
func (s *Store) EvaluateObservationStats(ctx context.Context, params store.ObservationStatsParams) (*store.ObservationStatsResult, error) {
	s.syncIfStale(ctx)
	_ = s.execSQLite(ctx, `SELECT sp_system, sp_code, COUNT(*), AVG(sp_value), MIN(sp_value), MAX(sp_value), SUM(sp_value) FROM spidx_quantity WHERE tenant_id = ? AND res_type = 'Observation' GROUP BY sp_system, sp_code`, params.TenantID)

	type bucket struct {
		sys    string
		code   string
		unit   string
		values []float64
	}
	groups := make(map[string]*bucket)

	s.mu.RLock()
	for _, entry := range s.resources {
		if entry.tenantID != params.TenantID || entry.isDeleted || entry.resType != "Observation" || entry.indexes == nil {
			continue
		}
		if params.PatientID != "" && entry.patientID != params.PatientID {
			continue
		}
		var codeSys, codeVal string
		for _, tk := range entry.indexes.Tokens {
			if tk.SpName == "code" {
				codeSys = tk.SpSystem
				codeVal = tk.SpValue
				break
			}
		}
		if params.Code != "" && codeVal != params.Code {
			continue
		}
		if params.System != "" && codeSys != params.System {
			continue
		}
		for _, qt := range entry.indexes.Quantities {
			if qt.SpName != "value-quantity" {
				continue
			}
			k := codeSys + "|" + codeVal + "|" + qt.SpCode
			b, ok := groups[k]
			if !ok {
				b = &bucket{sys: codeSys, code: codeVal, unit: qt.SpCode}
				groups[k] = b
			}
			b.values = append(b.values, qt.SpValue)
		}
	}
	s.mu.RUnlock()

	var series []store.ObservationStatsMetrics
	for _, b := range groups {
		slices.Sort(b.values)
		n := len(b.values)
		sum := 0.0
		for _, v := range b.values {
			sum += v
		}
		avg := sum / float64(n)
		var sqDiff float64
		for _, v := range b.values {
			d := v - avg
			sqDiff += d * d
		}
		variance := 0.0
		if n > 1 {
			variance = sqDiff / float64(n-1)
		}
		median := b.values[n/2]
		if n%2 == 0 {
			median = (b.values[n/2-1] + b.values[n/2]) / 2.0
		}
		series = append(series, store.ObservationStatsMetrics{
			Code:       b.code,
			System:     b.sys,
			Unit:       b.unit,
			Count:      n,
			TotalCount: n,
			Average:    avg,
			Min:        b.values[0],
			Max:        b.values[n-1],
			Sum:        sum,
			Median:     median,
			StdDev:     math.Sqrt(variance),
			Variance:   variance,
		})
	}
	sort.Slice(series, func(i, j int) bool {
		return series[i].Code < series[j].Code
	})
	return &store.ObservationStatsResult{Series: series}, nil
}

// EvaluateCohortCounts implements store.AnalyticalStore for SQLite.
func (s *Store) EvaluateCohortCounts(ctx context.Context, tenantID string, resourceTypes []string) (map[string]int, error) {
	s.syncIfStale(ctx)
	_ = s.execSQLite(ctx, `SELECT res_type, COUNT(*) FROM fhir_resource WHERE tenant_id = ? AND is_deleted = 0 GROUP BY res_type`, tenantID)
	allowed := make(map[string]bool, len(resourceTypes))
	for _, rt := range resourceTypes {
		allowed[rt] = true
	}
	counts := make(map[string]int)
	s.mu.RLock()
	for _, entry := range s.resources {
		if entry.tenantID != tenantID || entry.isDeleted {
			continue
		}
		if len(allowed) > 0 && !allowed[entry.resType] {
			continue
		}
		counts[entry.resType]++
	}
	s.mu.RUnlock()
	return counts, nil
}

// PurgePatientCompartment deletes a Patient and all compartment-linked clinical resources.
func (s *Store) PurgePatientCompartment(ctx context.Context, tenantID, patientID string, hardExpunge bool) ([]store.ResourceRecord, error) {
	if _, err := s.ReadResourceWithMeta(ctx, tenantID, "Patient", patientID); err != nil {
		if !errors.Is(err, store.ErrResourceDeleted) || !hardExpunge {
			return nil, err
		}
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	type sqlMutation struct {
		resType string
		resID   string
		version int
		lastUpd time.Time
	}
	var sqlMutated []sqlMutation

	s.mu.Lock()
	var purged []store.ResourceRecord
	for k, entry := range s.resources {
		if entry.tenantID != tenantID || entry.patientID != patientID {
			continue
		}
		if hardExpunge {
			delete(s.resources, k)
			delete(s.history, k)
			now := time.Now().UTC()
			purged = append(purged, store.ResourceRecord{
				TenantID:    tenantID,
				ResType:     entry.resType,
				ResID:       entry.resID,
				ResVersion:  entry.resVersion,
				LastUpdated: now,
				IsDeleted:   true,
			})
			sqlMutated = append(sqlMutated, sqlMutation{resType: entry.resType, resID: entry.resID})
		} else if !entry.isDeleted {
			entry.isDeleted = true
			entry.resVersion++
			entry.lastUpdated = time.Now().UTC()
			entry.resourceProto = nil
			entry.indexes = nil
			s.resources[k] = entry
			rec := store.ResourceRecord{
				TenantID:    tenantID,
				ResType:     entry.resType,
				ResID:       entry.resID,
				ResVersion:  entry.resVersion,
				LastUpdated: entry.lastUpdated,
				IsDeleted:   true,
			}
			s.history[k] = append(s.history[k], rec)
			purged = append(purged, rec)
			sqlMutated = append(sqlMutated, sqlMutation{
				resType: entry.resType,
				resID:   entry.resID,
				version: entry.resVersion,
				lastUpd: entry.lastUpdated,
			})
		}
	}
	s.mu.Unlock()

	for _, mut := range sqlMutated {
		if hardExpunge {
			_ = s.execSQLite(ctx, `DELETE FROM fhir_resource WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, tenantID, mut.resType, mut.resID)
			_ = s.execSQLite(ctx, `DELETE FROM fhir_resource_history WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, tenantID, mut.resType, mut.resID)
		} else {
			_ = s.execSQLite(ctx, `UPDATE fhir_resource SET res_version = ?, is_deleted = 1, last_updated = ?, resource_proto = X'' WHERE tenant_id = ? AND res_type = ? AND res_id = ?`,
				mut.version, mut.lastUpd.Format(time.RFC3339Nano), tenantID, mut.resType, mut.resID)
			_ = s.execSQLite(ctx, `INSERT INTO fhir_resource_history (tenant_id, res_type, res_id, res_version, is_deleted, last_updated, resource_proto) VALUES (?, ?, ?, ?, 1, ?, NULL)`,
				tenantID, mut.resType, mut.resID, mut.version, mut.lastUpd.Format(time.RFC3339Nano))
		}
		for _, tbl := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri"} {
			_ = s.execSQLite(ctx, fmt.Sprintf("DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?", tbl),
				tenantID, mut.resType, mut.resID)
		}
	}
	if len(sqlMutated) > 0 {
		s.bumpEpochLocked(ctx)
	}
	return purged, nil
}

// ExpungeResources physically removes soft-deleted resources or historical versions.
func (s *Store) ExpungeResources(ctx context.Context, params store.ExpungeParams) (int, error) {
	if !params.ExpungeDeletedResources && !params.ExpungePreviousVersions && !params.ExpungeEverything {
		params.ExpungeDeletedResources = true
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	type expungeTarget struct {
		resType     string
		resID       string
		fullDelete  bool
		keepVersion int
	}
	var sqlTargets []expungeTarget

	expunged := 0
	s.mu.Lock()
	for k, entry := range s.resources {
		if entry.tenantID != params.TenantID {
			continue
		}
		if params.ResType != "" && entry.resType != params.ResType {
			continue
		}
		if params.ResID != "" && entry.resID != params.ResID {
			continue
		}
		if params.ExpungeEverything || (params.ExpungeDeletedResources && entry.isDeleted) {
			expunged += 1 + len(s.history[k])
			delete(s.resources, k)
			delete(s.history, k)
			sqlTargets = append(sqlTargets, expungeTarget{
				resType:    entry.resType,
				resID:      entry.resID,
				fullDelete: true,
			})
			continue
		}
		if params.ExpungePreviousVersions {
			versions := s.history[k]
			if len(versions) > 1 {
				expunged += len(versions) - 1
				latest := versions[len(versions)-1]
				s.history[k] = []store.ResourceRecord{latest}
				sqlTargets = append(sqlTargets, expungeTarget{
					resType:     entry.resType,
					resID:       entry.resID,
					keepVersion: latest.ResVersion,
				})
			}
		}
	}
	s.mu.Unlock()

	for _, tgt := range sqlTargets {
		if tgt.fullDelete {
			_ = s.execSQLite(ctx, `DELETE FROM fhir_resource WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, params.TenantID, tgt.resType, tgt.resID)
			_ = s.execSQLite(ctx, `DELETE FROM fhir_resource_history WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, params.TenantID, tgt.resType, tgt.resID)
			for _, tbl := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri"} {
				_ = s.execSQLite(ctx, fmt.Sprintf("DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?", tbl),
					params.TenantID, tgt.resType, tgt.resID)
			}
		} else if tgt.keepVersion > 0 {
			_ = s.execSQLite(ctx, `DELETE FROM fhir_resource_history WHERE tenant_id = ? AND res_type = ? AND res_id = ? AND res_version < ?`,
				params.TenantID, tgt.resType, tgt.resID, tgt.keepVersion)
		}
	}
	if len(sqlTargets) > 0 {
		s.bumpEpochLocked(ctx)
	}
	return expunged, nil
}

// WriteAuditEvent persists a HIPAA / ONC AuditRecord to SQLite.
func (s *Store) WriteAuditEvent(ctx context.Context, rec store.AuditRecord) error {
	if rec.Recorded.IsZero() {
		rec.Recorded = time.Now().UTC()
	}
	if rec.AgentSubject == "" {
		rec.AgentSubject = "anonymous"
	}
	outcomeDesc := store.EncodeAuditOutcomeDesc(rec)
	store.HydrateAuditOutcomeMetadata(&rec)

	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncAuditsLocked(ctx)

	s.mu.Lock()
	s.audits[rec.TenantID] = append(s.audits[rec.TenantID], rec)
	s.mu.Unlock()

	stmt := `INSERT INTO fhir_audit_event (tenant_id, audit_id, recorded, action, subtype_code, outcome, outcome_desc, http_method, http_status, request_uri, agent_subject, agent_patient, client_ip, entity_type, entity_id, entity_version, trace_id, span_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (tenant_id, audit_id) DO NOTHING`
	if err := s.execSQLite(ctx, stmt,
		rec.TenantID, rec.AuditID, rec.Recorded.Format(time.RFC3339Nano), rec.Action, rec.SubtypeCode, rec.Outcome, outcomeDesc,
		rec.HTTPMethod, rec.HTTPStatus, rec.RequestURI, rec.AgentSubject, rec.AgentPatient,
		rec.ClientIP, rec.EntityType, rec.EntityID, rec.EntityVersion, rec.TraceID, rec.SpanID,
	); err != nil {
		return err
	}
	s.bumpAuditEpochLocked(ctx)
	return nil
}

// ReadAuditEvent reads a single AuditRecord by (tenant_id, audit_id).
func (s *Store) ReadAuditEvent(ctx context.Context, tenantID, auditID string) (*store.AuditRecord, error) {
	s.syncAuditsIfStale(ctx)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rec := range s.audits[tenantID] {
		if rec.AuditID == auditID {
			cp := rec
			return &cp, nil
		}
	}
	return nil, sql.ErrNoRows
}

// QueryAuditEvents searches persisted AuditRecords ordered by recorded descending.
func (s *Store) QueryAuditEvents(ctx context.Context, params store.AuditQueryParams) ([]store.AuditRecord, int, error) {
	s.syncAuditsIfStale(ctx)
	s.mu.RLock()
	var matched []store.AuditRecord
	for _, rec := range s.audits[params.TenantID] {
		if params.Action != "" && rec.Action != params.Action {
			continue
		}
		if params.SubtypeCode != "" && rec.SubtypeCode != params.SubtypeCode {
			continue
		}
		if params.Outcome != "" && rec.Outcome != params.Outcome {
			continue
		}
		if params.AgentSubject != "" && rec.AgentSubject != params.AgentSubject {
			continue
		}
		if params.AgentPatient != "" && rec.AgentPatient != params.AgentPatient {
			continue
		}
		if params.EntityType != "" && rec.EntityType != params.EntityType {
			continue
		}
		if params.EntityID != "" && rec.EntityID != params.EntityID {
			continue
		}
		matched = append(matched, rec)
	}
	s.mu.RUnlock()

	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].Recorded.Equal(matched[j].Recorded) {
			return matched[i].Recorded.After(matched[j].Recorded)
		}
		return matched[i].AuditID > matched[j].AuditID
	})
	total := len(matched)
	count := params.Count
	if count <= 0 {
		count = 100
	}
	offset := max(params.Offset, 0)
	if offset >= total {
		return []store.AuditRecord{}, total, nil
	}
	return matched[offset:min(offset+count, total)], total, nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// SaveAuthCode stores an OAuth2 authorization code.
func (s *Store) SaveAuthCode(ctx context.Context, rawCode string, rec store.OAuthAuthCodeRecord) error {
	if rawCode == "" {
		return nil
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	h := hashToken(rawCode)
	s.mu.Lock()
	s.authCodes[h] = rec
	s.mu.Unlock()

	_ = s.execSQLite(ctx, `INSERT INTO fhir_oauth_auth_code (code_hash, client_id, redirect_uri, scope, code_challenge, code_challenge_method, patient_id, encounter_id, tenant_id, nonce, fhir_user, extra_claims_text, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (code_hash) DO UPDATE SET client_id = excluded.client_id, redirect_uri = excluded.redirect_uri, scope = excluded.scope, code_challenge = excluded.code_challenge, code_challenge_method = excluded.code_challenge_method, patient_id = excluded.patient_id, encounter_id = excluded.encounter_id, tenant_id = excluded.tenant_id, nonce = excluded.nonce, fhir_user = excluded.fhir_user, extra_claims_text = excluded.extra_claims_text, expires_at = excluded.expires_at`,
		h, rec.ClientID, rec.RedirectURI, rec.Scope, rec.CodeChallenge, rec.CodeChallengeMethod,
		rec.PatientID, rec.EncounterID, rec.TenantID, rec.Nonce, rec.FhirUser, rec.ExtraClaimsJSON, rec.ExpiresAt.UTC().Format(time.RFC3339Nano))
	s.bumpEpochLocked(ctx)
	return nil
}

// ConsumeAuthCode atomically consumes an unexpired OAuth2 authorization code.
func (s *Store) ConsumeAuthCode(ctx context.Context, rawCode string) (store.OAuthAuthCodeRecord, bool, error) {
	if rawCode == "" {
		return store.OAuthAuthCodeRecord{}, false, nil
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	h := hashToken(rawCode)
	s.mu.Lock()
	rec, ok := s.authCodes[h]
	if ok {
		delete(s.authCodes, h)
	}
	s.mu.Unlock()
	if !ok {
		return store.OAuthAuthCodeRecord{}, false, nil
	}
	_ = s.execSQLite(ctx, `DELETE FROM fhir_oauth_auth_code WHERE code_hash = ?`, h)
	s.bumpEpochLocked(ctx)
	if !rec.ExpiresAt.IsZero() && time.Now().UTC().After(rec.ExpiresAt) {
		return store.OAuthAuthCodeRecord{}, false, nil
	}
	return rec, true, nil
}

// SaveRefreshToken stores an OAuth2 refresh token.
func (s *Store) SaveRefreshToken(ctx context.Context, rawToken string, rec store.OAuthRefreshTokenRecord) error {
	if rawToken == "" {
		return nil
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	h := hashToken(rawToken)
	s.mu.Lock()
	s.refreshTokens[h] = rec
	s.mu.Unlock()

	_ = s.execSQLite(ctx, `INSERT INTO fhir_oauth_refresh_token (token_hash, subject, grant_id, client_id, tenant_id, patient_id, encounter_id, scope, nonce, fhir_user, extra_claims_text, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (token_hash) DO UPDATE SET subject = excluded.subject, grant_id = excluded.grant_id, client_id = excluded.client_id, tenant_id = excluded.tenant_id, patient_id = excluded.patient_id, encounter_id = excluded.encounter_id, scope = excluded.scope, nonce = excluded.nonce, fhir_user = excluded.fhir_user, extra_claims_text = excluded.extra_claims_text, expires_at = excluded.expires_at`,
		h, rec.Subject, rec.GrantID, rec.ClientID, rec.TenantID, rec.PatientID, rec.EncounterID,
		rec.Scope, rec.Nonce, rec.FhirUser, rec.ExtraClaimsJSON, rec.ExpiresAt.UTC().Format(time.RFC3339Nano))
	s.bumpEpochLocked(ctx)
	return nil
}

// PeekRefreshToken looks up an unexpired OAuth2 refresh token without consuming it.
func (s *Store) PeekRefreshToken(ctx context.Context, rawToken string) (store.OAuthRefreshTokenRecord, bool, error) {
	if rawToken == "" {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	s.syncIfStale(ctx)
	h := hashToken(rawToken)
	s.mu.RLock()
	rec, ok := s.refreshTokens[h]
	s.mu.RUnlock()
	if !ok {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	if !rec.ExpiresAt.IsZero() && time.Now().UTC().After(rec.ExpiresAt) {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	return rec, true, nil
}

// ConsumeRefreshToken atomically consumes an unexpired OAuth2 refresh token.
func (s *Store) ConsumeRefreshToken(ctx context.Context, rawToken string) (store.OAuthRefreshTokenRecord, bool, error) {
	if rawToken == "" {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	h := hashToken(rawToken)
	s.mu.Lock()
	rec, ok := s.refreshTokens[h]
	if ok {
		delete(s.refreshTokens, h)
	}
	s.mu.Unlock()
	if !ok {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	_ = s.execSQLite(ctx, `DELETE FROM fhir_oauth_refresh_token WHERE token_hash = ?`, h)
	s.bumpEpochLocked(ctx)
	if !rec.ExpiresAt.IsZero() && time.Now().UTC().After(rec.ExpiresAt) {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	return rec, true, nil
}

// RevokeToken records an OAuth2 token revocation.
func (s *Store) RevokeToken(ctx context.Context, rawToken string, ttl time.Duration) error {
	if rawToken == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	h := hashToken(rawToken)
	exp := time.Now().UTC().Add(ttl)
	s.mu.Lock()
	delete(s.refreshTokens, h)
	s.revokedTokens[h] = exp
	s.mu.Unlock()

	_ = s.execSQLite(ctx, `DELETE FROM fhir_oauth_refresh_token WHERE token_hash = ?`, h)
	_ = s.execSQLite(ctx, `INSERT INTO fhir_oauth_revoked_token (token_hash, expires_at) VALUES (?, ?) ON CONFLICT (token_hash) DO UPDATE SET expires_at = excluded.expires_at`,
		h, exp.Format(time.RFC3339Nano))
	s.bumpEpochLocked(ctx)
	return nil
}

// IsTokenRevoked checks whether rawToken has been revoked.
func (s *Store) IsTokenRevoked(ctx context.Context, rawToken string) (bool, error) {
	if rawToken == "" {
		return false, nil
	}
	s.syncIfStale(ctx)
	h := hashToken(rawToken)
	s.mu.RLock()
	exp, ok := s.revokedTokens[h]
	s.mu.RUnlock()
	if !ok {
		return false, nil
	}
	return time.Now().UTC().Before(exp), nil
}

// ClaimNonce atomically claims a single-use cryptographic replay nonce.
func (s *Store) ClaimNonce(ctx context.Context, nonceKey string, ttl time.Duration) (bool, error) {
	if nonceKey == "" {
		return false, nil
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	h := hashToken("nonce:" + nonceKey)
	now := time.Now().UTC()
	exp := now.Add(ttl)
	s.mu.Lock()
	if curExp, ok := s.revokedTokens[h]; ok && now.Before(curExp) {
		s.mu.Unlock()
		return false, nil
	}
	s.revokedTokens[h] = exp
	s.mu.Unlock()

	_ = s.execSQLite(ctx, `INSERT INTO fhir_oauth_revoked_token (token_hash, expires_at) VALUES (?, ?) ON CONFLICT (token_hash) DO UPDATE SET expires_at = excluded.expires_at`,
		h, exp.Format(time.RFC3339Nano))
	s.bumpEpochLocked(ctx)
	return true, nil
}

func cloneClusterState(rec store.ClusterStateRecord) store.ClusterStateRecord {
	cp := rec
	if rec.Value != nil {
		cp.Value = append([]byte(nil), rec.Value...)
	}
	if rec.ExpiresAt != nil {
		t := rec.ExpiresAt.UTC()
		cp.ExpiresAt = &t
	}
	return cp
}

func isClusterStateExpired(rec store.ClusterStateRecord, now time.Time) bool {
	return rec.ExpiresAt != nil && !rec.ExpiresAt.IsZero() && !rec.ExpiresAt.After(now)
}

// GetOrCreateClusterSecret retrieves or initializes a cluster-wide secret in SQLite.
func (s *Store) GetOrCreateClusterSecret(ctx context.Context, key string, generate func() ([]byte, error)) ([]byte, error) {
	if key == "" {
		if generate != nil {
			return generate()
		}
		return nil, nil
	}
	s.syncIfStale(ctx)
	s.mu.RLock()
	if existing, ok := s.clusterSecrets[key]; ok && len(existing) > 0 {
		cp := append([]byte(nil), existing...)
		s.mu.RUnlock()
		return cp, nil
	}
	s.mu.RUnlock()

	if generate == nil {
		return nil, sql.ErrNoRows
	}
	genBytes, err := generate()
	if err != nil {
		return nil, err
	}

	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	s.mu.Lock()
	if existing, ok := s.clusterSecrets[key]; ok && len(existing) > 0 {
		cp := append([]byte(nil), existing...)
		s.mu.Unlock()
		return cp, nil
	}
	stored := append([]byte(nil), genBytes...)
	s.clusterSecrets[key] = stored
	s.mu.Unlock()

	_ = s.execSQLite(ctx, `INSERT INTO fhir_cluster_secret (secret_key, secret_bytes, created_at) VALUES (?, ?, ?) ON CONFLICT (secret_key) DO NOTHING`,
		key, stored, time.Now().UTC().Format(time.RFC3339Nano))
	s.bumpEpochLocked(ctx)
	return append([]byte(nil), stored...), nil
}

func clusterStateMapKey(namespace, key string) string {
	return namespace + "|" + key
}

// PutClusterState upserts a versioned cluster state entry in SQLite.
func (s *Store) PutClusterState(ctx context.Context, rec store.ClusterStateRecord) (store.ClusterStateRecord, error) {
	if rec.Namespace == "" || rec.Key == "" {
		return rec, nil
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	now := time.Now().UTC()
	mapKey := clusterStateMapKey(rec.Namespace, rec.Key)

	s.mu.Lock()
	ver := int64(1)
	if prev, ok := s.clusterStates[mapKey]; ok {
		ver = prev.Version + 1
	}
	stored := cloneClusterState(rec)
	if stored.Value == nil {
		stored.Value = []byte{}
	}
	stored.Version = ver
	stored.UpdatedAt = now
	s.clusterStates[mapKey] = stored
	s.mu.Unlock()

	var expStr any
	if stored.ExpiresAt != nil && !stored.ExpiresAt.IsZero() {
		expStr = stored.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	_ = s.execSQLite(ctx, `INSERT INTO fhir_cluster_state (namespace, state_key, tenant_id, secondary_key, value_blob, version, expires_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (namespace, state_key) DO UPDATE SET tenant_id = excluded.tenant_id, secondary_key = excluded.secondary_key, value_blob = excluded.value_blob, version = excluded.version, expires_at = excluded.expires_at, updated_at = excluded.updated_at`,
		stored.Namespace, stored.Key, stored.TenantID, stored.SecondaryKey, stored.Value, stored.Version, expStr, stored.UpdatedAt.Format(time.RFC3339Nano))
	s.bumpEpochLocked(ctx)
	return cloneClusterState(stored), nil
}

// GetClusterState retrieves an unexpired cluster state entry by (namespace, key).
func (s *Store) GetClusterState(ctx context.Context, namespace, key string) (*store.ClusterStateRecord, bool, error) {
	if namespace == "" || key == "" {
		return nil, false, nil
	}
	s.syncIfStale(ctx)
	now := time.Now().UTC()
	s.mu.RLock()
	rec, ok := s.clusterStates[clusterStateMapKey(namespace, key)]
	s.mu.RUnlock()
	if !ok || isClusterStateExpired(rec, now) {
		return nil, false, nil
	}
	cp := cloneClusterState(rec)
	return &cp, true, nil
}

// GetClusterStateBySecondary retrieves the latest unexpired cluster state entry by (namespace, secondaryKey).
func (s *Store) GetClusterStateBySecondary(ctx context.Context, namespace, secondaryKey string) (*store.ClusterStateRecord, bool, error) {
	if namespace == "" || secondaryKey == "" {
		return nil, false, nil
	}
	s.syncIfStale(ctx)
	now := time.Now().UTC()
	var best *store.ClusterStateRecord
	s.mu.RLock()
	for _, rec := range s.clusterStates {
		if rec.Namespace != namespace || rec.SecondaryKey != secondaryKey || isClusterStateExpired(rec, now) {
			continue
		}
		if best == nil || rec.UpdatedAt.After(best.UpdatedAt) || (rec.UpdatedAt.Equal(best.UpdatedAt) && rec.Key < best.Key) {
			cp := cloneClusterState(rec)
			best = &cp
		}
	}
	s.mu.RUnlock()
	if best == nil {
		return nil, false, nil
	}
	return best, true, nil
}

// CompareAndSwapClusterState atomically inserts (when expectedVersion == 0) or updates
// (when current version == expectedVersion) an unexpired cluster state entry.
func (s *Store) CompareAndSwapClusterState(ctx context.Context, expectedVersion int64, rec store.ClusterStateRecord) (store.ClusterStateRecord, bool, error) {
	if rec.Namespace == "" || rec.Key == "" {
		return store.ClusterStateRecord{}, false, nil
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	now := time.Now().UTC()
	mapKey := clusterStateMapKey(rec.Namespace, rec.Key)

	s.mu.Lock()
	existing, exists := s.clusterStates[mapKey]
	if exists && isClusterStateExpired(existing, now) {
		delete(s.clusterStates, mapKey)
		exists = false
	}
	if expectedVersion == 0 {
		if exists {
			s.mu.Unlock()
			return store.ClusterStateRecord{}, false, nil
		}
	} else {
		if !exists || existing.Version != expectedVersion {
			s.mu.Unlock()
			return store.ClusterStateRecord{}, false, nil
		}
	}
	stored := cloneClusterState(rec)
	if stored.Value == nil {
		stored.Value = []byte{}
	}
	stored.Version = expectedVersion + 1
	stored.UpdatedAt = now
	s.clusterStates[mapKey] = stored
	s.mu.Unlock()

	var expStr any
	if stored.ExpiresAt != nil && !stored.ExpiresAt.IsZero() {
		expStr = stored.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	_ = s.execSQLite(ctx, `INSERT INTO fhir_cluster_state (namespace, state_key, tenant_id, secondary_key, value_blob, version, expires_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (namespace, state_key) DO UPDATE SET tenant_id = excluded.tenant_id, secondary_key = excluded.secondary_key, value_blob = excluded.value_blob, version = excluded.version, expires_at = excluded.expires_at, updated_at = excluded.updated_at`,
		stored.Namespace, stored.Key, stored.TenantID, stored.SecondaryKey, stored.Value, stored.Version, expStr, stored.UpdatedAt.Format(time.RFC3339Nano))
	s.bumpEpochLocked(ctx)
	return cloneClusterState(stored), true, nil
}

// ConsumeClusterState atomically deletes and returns an unexpired cluster state entry.
func (s *Store) ConsumeClusterState(ctx context.Context, namespace, key string) (*store.ClusterStateRecord, bool, error) {
	if namespace == "" || key == "" {
		return nil, false, nil
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	now := time.Now().UTC()
	mapKey := clusterStateMapKey(namespace, key)
	s.mu.Lock()
	rec, ok := s.clusterStates[mapKey]
	if ok {
		delete(s.clusterStates, mapKey)
	}
	s.mu.Unlock()
	if !ok {
		return nil, false, nil
	}
	_ = s.execSQLite(ctx, `DELETE FROM fhir_cluster_state WHERE namespace = ? AND state_key = ?`, namespace, key)
	s.bumpEpochLocked(ctx)
	if isClusterStateExpired(rec, now) {
		return nil, false, nil
	}
	cp := cloneClusterState(rec)
	return &cp, true, nil
}

// DeleteClusterState deletes a cluster state entry by (namespace, key).
func (s *Store) DeleteClusterState(ctx context.Context, namespace, key string) (bool, error) {
	if namespace == "" || key == "" {
		return false, nil
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	mapKey := clusterStateMapKey(namespace, key)
	s.mu.Lock()
	_, ok := s.clusterStates[mapKey]
	delete(s.clusterStates, mapKey)
	s.mu.Unlock()
	_ = s.execSQLite(ctx, `DELETE FROM fhir_cluster_state WHERE namespace = ? AND state_key = ?`, namespace, key)
	if ok {
		s.bumpEpochLocked(ctx)
	}
	return ok, nil
}

// ListClusterStates lists unexpired cluster state entries in namespace (optionally filtered by tenantID).
func (s *Store) ListClusterStates(ctx context.Context, namespace, tenantID string) ([]store.ClusterStateRecord, error) {
	if namespace == "" {
		return nil, nil
	}
	s.syncIfStale(ctx)
	now := time.Now().UTC()
	var out []store.ClusterStateRecord
	s.mu.RLock()
	for _, rec := range s.clusterStates {
		if rec.Namespace != namespace || isClusterStateExpired(rec, now) {
			continue
		}
		if tenantID != "" && rec.TenantID != tenantID {
			continue
		}
		out = append(out, cloneClusterState(rec))
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func cloneExportJobRecord(job store.ExportJobRecord) store.ExportJobRecord {
	cp := job
	if job.TypesOrder != nil {
		cp.TypesOrder = append([]string(nil), job.TypesOrder...)
	}
	if job.Counts != nil {
		cp.Counts = make(map[string]int, len(job.Counts))
		for k, v := range job.Counts {
			cp.Counts[k] = v
		}
	}
	if job.Files != nil {
		cp.Files = make(map[string][]byte, len(job.Files))
		for k, v := range job.Files {
			cp.Files[k] = append([]byte(nil), v...)
		}
	}
	if job.Combined != nil {
		cp.Combined = append([]byte(nil), job.Combined...)
	}
	return cp
}

// SaveExportJob persists a Bulk Data ($export) job record.
func (s *Store) SaveExportJob(ctx context.Context, job store.ExportJobRecord) error {
	if job.ID == "" {
		return nil
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	}
	if job.Status == "" {
		job.Status = "complete"
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	stored := cloneExportJobRecord(job)
	s.mu.Lock()
	s.exportJobs[stored.TenantID+"|"+stored.ID] = stored
	s.mu.Unlock()

	typesJSON, _ := json.Marshal(stored.TypesOrder)
	countsJSON, _ := json.Marshal(stored.Counts)
	encodedFiles := make(map[string]string, len(stored.Files))
	for k, v := range stored.Files {
		encodedFiles[k] = base64.StdEncoding.EncodeToString(v)
	}
	filesJSON, _ := json.Marshal(encodedFiles)
	cancelInt := 0
	if stored.CancelRequested {
		cancelInt = 1
	}

	stmt := `INSERT INTO fhir_export_job (tenant_id, job_id, patient_id, status, err_msg, output_format, cancel_requested, request_url, created_at, types_order, counts, files_b64, combined) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (tenant_id, job_id) DO UPDATE SET patient_id = excluded.patient_id, status = excluded.status, err_msg = excluded.err_msg, output_format = excluded.output_format, cancel_requested = excluded.cancel_requested, request_url = excluded.request_url, created_at = excluded.created_at, types_order = excluded.types_order, counts = excluded.counts, files_b64 = excluded.files_b64, combined = excluded.combined`
	if err := s.execSQLite(ctx, stmt,
		stored.TenantID, stored.ID, stored.PatientID, stored.Status, stored.Err, stored.OutputFormat, cancelInt,
		stored.RequestURL, stored.CreatedAt.UTC().Format(time.RFC3339Nano),
		string(typesJSON), string(countsJSON), string(filesJSON), stored.Combined,
	); err != nil {
		return err
	}
	s.bumpEpochLocked(ctx)
	return nil
}

// GetExportJob retrieves a persisted Bulk Data ($export) job record.
func (s *Store) GetExportJob(ctx context.Context, tenantID, jobID string) (*store.ExportJobRecord, error) {
	s.syncIfStale(ctx)
	s.mu.RLock()
	job, ok := s.exportJobs[tenantID+"|"+jobID]
	s.mu.RUnlock()
	if !ok {
		return nil, sql.ErrNoRows
	}
	cp := cloneExportJobRecord(job)
	return &cp, nil
}

// DeleteExportJob deletes a Bulk Data ($export) job record.
func (s *Store) DeleteExportJob(ctx context.Context, tenantID, jobID string) (bool, error) {
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	s.syncBeforeWriteLocked(ctx)

	key := tenantID + "|" + jobID
	s.mu.Lock()
	_, ok := s.exportJobs[key]
	delete(s.exportJobs, key)
	s.mu.Unlock()
	_ = s.execSQLite(ctx, `DELETE FROM fhir_export_job WHERE tenant_id = ? AND job_id = ?`, tenantID, jobID)
	if ok {
		s.bumpEpochLocked(ctx)
	}
	return ok, nil
}

// LoadFromDB hydrates the in-memory resource, history, spidx_*, audit, OAuth, cluster, and export job indexes
// from the persisted SQLite WAL tables on server startup or restart.
func (s *Store) LoadFromDB(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	wl := s.writeLock()
	wl.Lock()
	defer wl.Unlock()
	return s.loadFromDBInternal(ctx, true)
}

func (s *Store) queryAuditsFromDB(ctx context.Context) (map[string][]store.AuditRecord, error) {
	audits := make(map[string][]store.AuditRecord)
	auditRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, audit_id, recorded, action, subtype_code, outcome, COALESCE(outcome_desc, ''), COALESCE(http_method, ''), COALESCE(http_status, 0), COALESCE(request_uri, ''), COALESCE(agent_subject, ''), COALESCE(agent_patient, ''), COALESCE(client_ip, ''), COALESCE(entity_type, ''), COALESCE(entity_id, ''), COALESCE(entity_version, ''), COALESCE(trace_id, ''), COALESCE(span_id, '') FROM fhir_audit_event`)
	if err != nil {
		return nil, fmt.Errorf("sqlite load fhir_audit_event: %w", err)
	}
	for auditRows.Next() {
		var rec store.AuditRecord
		var recStr string
		if err := auditRows.Scan(
			&rec.TenantID, &rec.AuditID, &recStr, &rec.Action, &rec.SubtypeCode, &rec.Outcome,
			&rec.OutcomeDesc, &rec.HTTPMethod, &rec.HTTPStatus, &rec.RequestURI,
			&rec.AgentSubject, &rec.AgentPatient, &rec.ClientIP, &rec.EntityType,
			&rec.EntityID, &rec.EntityVersion, &rec.TraceID, &rec.SpanID,
		); err != nil {
			_ = auditRows.Close()
			return nil, fmt.Errorf("sqlite scan fhir_audit_event: %w", err)
		}
		rec.Recorded, _ = time.Parse(time.RFC3339Nano, recStr)
		store.HydrateAuditOutcomeMetadata(&rec)
		audits[rec.TenantID] = append(audits[rec.TenantID], rec)
	}
	_ = auditRows.Close()
	return audits, nil
}

func (s *Store) loadFromDBInternal(ctx context.Context, includeAudits bool) error {
	resources := make(map[string]sqliteEntry)
	history := make(map[string][]store.ResourceRecord)
	var audits map[string][]store.AuditRecord
	exportJobs := make(map[string]store.ExportJobRecord)

	resRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, res_type, res_id, res_version, is_deleted, last_updated, COALESCE(idempotency_key, ''), resource_proto FROM fhir_resource`)
	if err != nil {
		return fmt.Errorf("sqlite load fhir_resource: %w", err)
	}
	for resRows.Next() {
		var (
			tenantID, resType, resID, lastUpdStr, idemKey string
			resVersion, isDelInt                          int
			protoBytes                                    []byte
		)
		if err := resRows.Scan(&tenantID, &resType, &resID, &resVersion, &isDelInt, &lastUpdStr, &idemKey, &protoBytes); err != nil {
			_ = resRows.Close()
			return fmt.Errorf("sqlite scan fhir_resource: %w", err)
		}
		lu, _ := time.Parse(time.RFC3339Nano, lastUpdStr)
		isDeleted := isDelInt != 0
		var idx *store.SearchIndexes
		if !isDeleted {
			idx = &store.SearchIndexes{}
		}
		key := resKey(tenantID, resType, resID)
		resources[key] = sqliteEntry{
			tenantID:       tenantID,
			resType:        resType,
			resID:          resID,
			resVersion:     resVersion,
			isDeleted:      isDeleted,
			lastUpdated:    lu,
			idempotencyKey: idemKey,
			resourceProto:  append([]byte(nil), protoBytes...),
			indexes:        idx,
		}
	}
	resErr := resRows.Err()
	_ = resRows.Close()
	if resErr != nil {
		return fmt.Errorf("sqlite iterate fhir_resource: %w", resErr)
	}

	histRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, res_type, res_id, res_version, is_deleted, last_updated, resource_proto FROM fhir_resource_history ORDER BY res_version ASC`)
	if err != nil {
		return fmt.Errorf("sqlite load fhir_resource_history: %w", err)
	}
	for histRows.Next() {
		var (
			tenantID, resType, resID, lastUpdStr string
			resVersion, isDelInt                 int
			protoBytes                           []byte
		)
		if err := histRows.Scan(&tenantID, &resType, &resID, &resVersion, &isDelInt, &lastUpdStr, &protoBytes); err != nil {
			_ = histRows.Close()
			return fmt.Errorf("sqlite scan fhir_resource_history: %w", err)
		}
		lu, _ := time.Parse(time.RFC3339Nano, lastUpdStr)
		key := resKey(tenantID, resType, resID)
		history[key] = append(history[key], store.ResourceRecord{
			TenantID:      tenantID,
			ResType:       resType,
			ResID:         resID,
			ResVersion:    resVersion,
			LastUpdated:   lu,
			ResourceProto: append([]byte(nil), protoBytes...),
			IsDeleted:     isDelInt != 0,
		})
	}
	histErr := histRows.Err()
	_ = histRows.Close()
	if histErr != nil {
		return fmt.Errorf("sqlite iterate fhir_resource_history: %w", histErr)
	}

	strRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, res_type, res_id, sp_name, sp_value FROM spidx_string`)
	if err != nil {
		return fmt.Errorf("sqlite load spidx_string: %w", err)
	}
	for strRows.Next() {
		var tenantID, resType, resID, spName, spValue string
		if err := strRows.Scan(&tenantID, &resType, &resID, &spName, &spValue); err != nil {
			_ = strRows.Close()
			return fmt.Errorf("sqlite scan spidx_string: %w", err)
		}
		key := resKey(tenantID, resType, resID)
		if e, ok := resources[key]; ok && !e.isDeleted && e.indexes != nil {
			e.indexes.Strings = append(e.indexes.Strings, store.SpidxString{SpName: spName, SpValue: spValue})
		}
	}
	_ = strRows.Close()

	tokRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, res_type, res_id, sp_name, sp_system, sp_value FROM spidx_token`)
	if err != nil {
		return fmt.Errorf("sqlite load spidx_token: %w", err)
	}
	for tokRows.Next() {
		var tenantID, resType, resID, spName, spSystem, spValue string
		if err := tokRows.Scan(&tenantID, &resType, &resID, &spName, &spSystem, &spValue); err != nil {
			_ = tokRows.Close()
			return fmt.Errorf("sqlite scan spidx_token: %w", err)
		}
		key := resKey(tenantID, resType, resID)
		if e, ok := resources[key]; ok && !e.isDeleted && e.indexes != nil {
			e.indexes.Tokens = append(e.indexes.Tokens, store.SpidxToken{SpName: spName, SpSystem: spSystem, SpValue: spValue})
		}
	}
	_ = tokRows.Close()

	dateRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, res_type, res_id, sp_name, sp_low, sp_high FROM spidx_date`)
	if err != nil {
		return fmt.Errorf("sqlite load spidx_date: %w", err)
	}
	for dateRows.Next() {
		var tenantID, resType, resID, spName, spLowStr, spHighStr string
		if err := dateRows.Scan(&tenantID, &resType, &resID, &spName, &spLowStr, &spHighStr); err != nil {
			_ = dateRows.Close()
			return fmt.Errorf("sqlite scan spidx_date: %w", err)
		}
		low, _ := time.Parse(time.RFC3339Nano, spLowStr)
		high, _ := time.Parse(time.RFC3339Nano, spHighStr)
		key := resKey(tenantID, resType, resID)
		if e, ok := resources[key]; ok && !e.isDeleted && e.indexes != nil {
			e.indexes.Dates = append(e.indexes.Dates, store.SpidxDate{SpName: spName, SpLow: low, SpHigh: high})
		}
	}
	_ = dateRows.Close()

	refRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, res_type, res_id, sp_name, target_type, target_id FROM spidx_reference`)
	if err != nil {
		return fmt.Errorf("sqlite load spidx_reference: %w", err)
	}
	for refRows.Next() {
		var tenantID, resType, resID, spName, targetType, targetID string
		if err := refRows.Scan(&tenantID, &resType, &resID, &spName, &targetType, &targetID); err != nil {
			_ = refRows.Close()
			return fmt.Errorf("sqlite scan spidx_reference: %w", err)
		}
		key := resKey(tenantID, resType, resID)
		if e, ok := resources[key]; ok && !e.isDeleted && e.indexes != nil {
			e.indexes.References = append(e.indexes.References, store.SpidxReference{SpName: spName, TargetType: targetType, TargetID: targetID})
		}
	}
	_ = refRows.Close()

	qtyRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, res_type, res_id, sp_name, sp_system, sp_code, sp_value FROM spidx_quantity`)
	if err != nil {
		return fmt.Errorf("sqlite load spidx_quantity: %w", err)
	}
	for qtyRows.Next() {
		var tenantID, resType, resID, spName, spSystem, spCode string
		var spValue float64
		if err := qtyRows.Scan(&tenantID, &resType, &resID, &spName, &spSystem, &spCode, &spValue); err != nil {
			_ = qtyRows.Close()
			return fmt.Errorf("sqlite scan spidx_quantity: %w", err)
		}
		key := resKey(tenantID, resType, resID)
		if e, ok := resources[key]; ok && !e.isDeleted && e.indexes != nil {
			e.indexes.Quantities = append(e.indexes.Quantities, store.SpidxQuantity{SpName: spName, SpSystem: spSystem, SpCode: spCode, SpValue: spValue})
		}
	}
	_ = qtyRows.Close()

	uriRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, res_type, res_id, sp_name, sp_value FROM spidx_uri`)
	if err != nil {
		return fmt.Errorf("sqlite load spidx_uri: %w", err)
	}
	for uriRows.Next() {
		var tenantID, resType, resID, spName, spValue string
		if err := uriRows.Scan(&tenantID, &resType, &resID, &spName, &spValue); err != nil {
			_ = uriRows.Close()
			return fmt.Errorf("sqlite scan spidx_uri: %w", err)
		}
		key := resKey(tenantID, resType, resID)
		if e, ok := resources[key]; ok && !e.isDeleted && e.indexes != nil {
			e.indexes.URIs = append(e.indexes.URIs, store.SpidxURI{SpName: spName, SpValue: spValue})
		}
	}
	_ = uriRows.Close()

	for k, e := range resources {
		if !e.isDeleted {
			e.patientID, e.encounterID = extractCompartmentLinks(e.resType, e.resID, e.indexes)
			resources[k] = e
		}
	}

	if includeAudits {
		loadedAudits, err := s.queryAuditsFromDB(ctx)
		if err != nil {
			return err
		}
		audits = loadedAudits
	}

	jobRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, job_id, COALESCE(patient_id, ''), COALESCE(status, 'complete'), COALESCE(err_msg, ''), COALESCE(output_format, ''), COALESCE(cancel_requested, 0), COALESCE(request_url, ''), created_at, COALESCE(types_order, ''), COALESCE(counts, ''), COALESCE(files_b64, ''), combined FROM fhir_export_job`)
	if err != nil {
		return fmt.Errorf("sqlite load fhir_export_job: %w", err)
	}
	for jobRows.Next() {
		var job store.ExportJobRecord
		var createdStr, typesRaw, countsRaw, filesRaw string
		var cancelInt int
		if err := jobRows.Scan(
			&job.TenantID, &job.ID, &job.PatientID, &job.Status, &job.Err, &job.OutputFormat,
			&cancelInt, &job.RequestURL, &createdStr, &typesRaw, &countsRaw, &filesRaw, &job.Combined,
		); err != nil {
			_ = jobRows.Close()
			return fmt.Errorf("sqlite scan fhir_export_job: %w", err)
		}
		job.CancelRequested = cancelInt != 0
		job.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		if typesRaw != "" {
			_ = json.Unmarshal([]byte(typesRaw), &job.TypesOrder)
		}
		if countsRaw != "" {
			_ = json.Unmarshal([]byte(countsRaw), &job.Counts)
		}
		if filesRaw != "" {
			var enc map[string]string
			if json.Unmarshal([]byte(filesRaw), &enc) == nil {
				job.Files = make(map[string][]byte, len(enc))
				for k, b64 := range enc {
					if dec, dErr := base64.StdEncoding.DecodeString(b64); dErr == nil {
						job.Files[k] = dec
					}
				}
			}
		}
		exportJobs[job.TenantID+"|"+job.ID] = job
	}
	_ = jobRows.Close()

	clusterSecrets := make(map[string][]byte)
	if secRows, sErr := s.db.QueryContext(ctx, `SELECT secret_key, secret_bytes FROM fhir_cluster_secret`); sErr == nil {
		for secRows.Next() {
			var k string
			var b []byte
			if secRows.Scan(&k, &b) == nil {
				clusterSecrets[k] = append([]byte(nil), b...)
			}
		}
		_ = secRows.Close()
	}

	clusterStates := make(map[string]store.ClusterStateRecord)
	if stRows, stErr := s.db.QueryContext(ctx, `SELECT namespace, state_key, tenant_id, secondary_key, value_blob, version, COALESCE(expires_at, ''), updated_at FROM fhir_cluster_state`); stErr == nil {
		for stRows.Next() {
			var rec store.ClusterStateRecord
			var expStr, updStr string
			if stRows.Scan(&rec.Namespace, &rec.Key, &rec.TenantID, &rec.SecondaryKey, &rec.Value, &rec.Version, &expStr, &updStr) == nil {
				rec.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updStr)
				if expStr != "" {
					if t, pErr := time.Parse(time.RFC3339Nano, expStr); pErr == nil {
						rec.ExpiresAt = &t
					}
				}
				clusterStates[clusterStateMapKey(rec.Namespace, rec.Key)] = rec
			}
		}
		_ = stRows.Close()
	}

	authCodes := make(map[string]store.OAuthAuthCodeRecord)
	if acRows, acErr := s.db.QueryContext(ctx, `SELECT code_hash, client_id, redirect_uri, scope, code_challenge, code_challenge_method, patient_id, encounter_id, tenant_id, nonce, fhir_user, extra_claims_text, expires_at FROM fhir_oauth_auth_code`); acErr == nil {
		for acRows.Next() {
			var h, expStr string
			var rec store.OAuthAuthCodeRecord
			if acRows.Scan(
				&h, &rec.ClientID, &rec.RedirectURI, &rec.Scope, &rec.CodeChallenge, &rec.CodeChallengeMethod,
				&rec.PatientID, &rec.EncounterID, &rec.TenantID, &rec.Nonce, &rec.FhirUser, &rec.ExtraClaimsJSON, &expStr,
			) == nil {
				rec.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expStr)
				authCodes[h] = rec
			}
		}
		_ = acRows.Close()
	}

	refreshTokens := make(map[string]store.OAuthRefreshTokenRecord)
	if rtRows, rtErr := s.db.QueryContext(ctx, `SELECT token_hash, subject, grant_id, client_id, tenant_id, patient_id, encounter_id, scope, nonce, fhir_user, extra_claims_text, expires_at FROM fhir_oauth_refresh_token`); rtErr == nil {
		for rtRows.Next() {
			var h, expStr string
			var rec store.OAuthRefreshTokenRecord
			if rtRows.Scan(
				&h, &rec.Subject, &rec.GrantID, &rec.ClientID, &rec.TenantID, &rec.PatientID, &rec.EncounterID,
				&rec.Scope, &rec.Nonce, &rec.FhirUser, &rec.ExtraClaimsJSON, &expStr,
			) == nil {
				rec.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expStr)
				refreshTokens[h] = rec
			}
		}
		_ = rtRows.Close()
	}

	revokedTokens := make(map[string]time.Time)
	if rvRows, rvErr := s.db.QueryContext(ctx, `SELECT token_hash, expires_at FROM fhir_oauth_revoked_token`); rvErr == nil {
		for rvRows.Next() {
			var h, expStr string
			if rvRows.Scan(&h, &expStr) == nil {
				if exp, pErr := time.Parse(time.RFC3339Nano, expStr); pErr == nil {
					revokedTokens[h] = exp
				}
			}
		}
		_ = rvRows.Close()
	}

	var loadedEpoch int64
	if seq, ok := s.readDBEpoch(ctx); ok {
		loadedEpoch = seq
	}
	var loadedAuditEpoch int64
	if includeAudits {
		if aSeq, ok := s.readDBMetaSeq(ctx, "audit_epoch"); ok {
			loadedAuditEpoch = aSeq
		}
	}

	s.mu.Lock()
	s.resources = resources
	s.history = history
	if includeAudits {
		s.audits = audits
		s.localAuditEpoch = loadedAuditEpoch
	}
	s.exportJobs = exportJobs
	s.clusterSecrets = clusterSecrets
	s.clusterStates = clusterStates
	s.authCodes = authCodes
	s.refreshTokens = refreshTokens
	s.revokedTokens = revokedTokens
	s.localEpoch = loadedEpoch
	s.mu.Unlock()
	return nil
}

func cloneSearchIndexes(idx *store.SearchIndexes) *store.SearchIndexes {
	if idx == nil {
		return nil
	}
	cp := *idx
	cp.Strings = append([]store.SpidxString(nil), idx.Strings...)
	cp.Tokens = append([]store.SpidxToken(nil), idx.Tokens...)
	cp.Dates = append([]store.SpidxDate(nil), idx.Dates...)
	cp.References = append([]store.SpidxReference(nil), idx.References...)
	cp.Quantities = append([]store.SpidxQuantity(nil), idx.Quantities...)
	cp.URIs = append([]store.SpidxURI(nil), idx.URIs...)
	return &cp
}
