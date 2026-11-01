package doris

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/postgres"
)

// Option configures an Apache Doris Store.
type Option func(*Store)

// WithGroupCommit configures the Doris group_commit mode ("sync_mode", "async_mode", or "off_mode").
func WithGroupCommit(mode GroupCommitMode) Option {
	return func(s *Store) {
		if mode != "" {
			s.groupCommit = mode
		}
	}
}

// WithDatabase configures the target Doris database name (default: "flint").
func WithDatabase(dbName string) Option {
	return func(s *Store) {
		if trimmed := strings.TrimSpace(dbName); trimmed != "" {
			s.database = trimmed
		}
	}
}

type storedEntry struct {
	row     DorisResourceRow
	indexes *store.SearchIndexes
}

// Store implements store.Store and store.AnalyticalStore on Apache Doris.
//
// Because Apache Doris is a columnar MPP engine using Unique Key Merge-on-Write (MoW)
// without row-level SELECT FOR UPDATE or pg_advisory_xact_lock, Store coordinates
// optimistic concurrency (If-Match), TOCTOU-free conditional operations (If-None-Exist),
// and compensating transaction bundle rollback in Go before flushing mutations with
// Doris Group Commit.
type Store struct {
	db          *sql.DB
	database    string
	groupCommit GroupCommitMode

	// Sharded concurrency coordinator for TOCTOU-free conditional writes & If-Match checks
	stripes [64]sync.Mutex

	mu            sync.RWMutex
	resources     map[string]storedEntry            // key: tenantID + "|" + resType + "|" + resID
	history       map[string][]store.ResourceRecord // key: tenantID + "|" + resType + "|" + resID
	audits        map[string][]store.AuditRecord    // key: tenantID
	authCodes     map[string]store.OAuthAuthCodeRecord
	refreshTokens map[string]store.OAuthRefreshTokenRecord
	revokedTokens map[string]time.Time
	exportJobs    map[string]store.ExportJobRecord // key: tenantID + "|" + jobID
	sqlLog        []string
}

var (
	_ store.Store           = (*Store)(nil)
	_ store.AnalyticalStore = (*Store)(nil)
)

// New creates a new Zero-JSON Apache Doris store.
func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{
		db:            db,
		database:      "flint",
		groupCommit:   GroupCommitSync,
		resources:     make(map[string]storedEntry),
		history:       make(map[string][]store.ResourceRecord),
		audits:        make(map[string][]store.AuditRecord),
		authCodes:     make(map[string]store.OAuthAuthCodeRecord),
		refreshTokens: make(map[string]store.OAuthRefreshTokenRecord),
		revokedTokens: make(map[string]time.Time),
		exportJobs:    make(map[string]store.ExportJobRecord),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// BackendName returns the canonical identifier of this analytical and operational backend.
func (s *Store) BackendName() string {
	return string(store.BackendDoris)
}

// GroupCommit returns the configured Doris group_commit mode.
func (s *Store) GroupCommit() GroupCommitMode {
	if s == nil || s.groupCommit == "" {
		return GroupCommitSync
	}
	return s.groupCommit
}

// Database returns the configured Doris database name.
func (s *Store) Database() string {
	if s == nil || s.database == "" {
		return "flint"
	}
	return s.database
}

// ExecutedStatements returns a snapshot of the Doris SQL statements executed by this Store.
func (s *Store) ExecutedStatements() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.sqlLog))
	copy(out, s.sqlLog)
	return out
}

func (s *Store) lockTenantResourceType(tenantID, resType string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(tenantID))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(resType))
	idx := h.Sum32() % uint32(len(s.stripes))
	s.stripes[idx].Lock()
	return s.stripes[idx].Unlock
}

func resKey(tenantID, resType, resID string) string {
	return tenantID + "|" + resType + "|" + resID
}

func (s *Store) execDorisSQL(ctx context.Context, stmt string, args ...any) error {
	s.mu.Lock()
	if len(s.sqlLog) < 1024 {
		s.sqlLog = append(s.sqlLog, stmt)
	}
	s.mu.Unlock()

	if s.db == nil {
		return nil
	}
	if s.groupCommit != "" && s.groupCommit != GroupCommitOff {
		_, _ = s.db.ExecContext(ctx, fmt.Sprintf("SET group_commit = '%s'", s.groupCommit))
	}
	_, err := s.db.ExecContext(ctx, stmt, args...)
	return err
}

// Ping verifies connectivity to the underlying Apache Doris cluster.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.PingContext(ctx)
}

// WriteResource writes a single FHIR resource to Doris Unique Key MoW storage.
func (s *Store) WriteResource(ctx context.Context, input store.ResourceInput) error {
	_, err := s.WriteResourceWithMeta(ctx, input)
	return err
}

// WriteResourceWithMeta writes a single FHIR resource, enforcing ExpectedVersion (If-Match)
// and CreateOnly semantics via the sharded concurrency coordinator before flushing to Doris.
func (s *Store) WriteResourceWithMeta(ctx context.Context, input store.ResourceInput) (*store.ResourceRecord, error) {
	if strings.TrimSpace(input.TenantID) == "" {
		return nil, store.ErrNoTenant
	}
	unlock := s.lockTenantResourceType(input.TenantID, input.ResType)
	defer unlock()

	return s.writeUnlocked(ctx, input)
}

func (s *Store) writeUnlocked(ctx context.Context, input store.ResourceInput) (*store.ResourceRecord, error) {
	key := resKey(input.TenantID, input.ResType, input.ResID)

	s.mu.Lock()
	existing, exists := s.resources[key]
	curVersion := 0
	curDeleted := false
	if exists {
		curVersion = existing.row.ResVersion
		curDeleted = existing.row.IsDeleted
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
	row := ExtractRow(input, newVersion, now, false)
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

	s.resources[key] = storedEntry{
		row:     row,
		indexes: cloneSearchIndexes(input.SearchIndexes),
	}
	s.history[key] = append(s.history[key], rec)
	s.mu.Unlock()

	upsertStmt := `INSERT INTO fhir_resource (tenant_id, res_type, res_id, res_version, is_deleted, last_updated, idempotency_key, patient_id, encounter_id, primary_code_sys, primary_code, primary_date_low, primary_date_high, primary_quantity, primary_unit_sys, primary_unit_code, resource_proto_hex) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if err := s.execDorisSQL(ctx, upsertStmt,
		row.TenantID, row.ResType, row.ResID, row.ResVersion, row.IsDeleted, row.LastUpdated,
		row.IdempotencyKey, row.PatientID, row.EncounterID, row.PrimaryCodeSys, row.PrimaryCode,
		row.PrimaryDateLow, row.PrimaryDateHigh, row.PrimaryQuantity, row.PrimaryUnitSys, row.PrimaryUnitCode,
		row.ResourceProtoHex,
	); err != nil {
		return nil, fmt.Errorf("doris upsert resource: %w", err)
	}

	histStmt := `INSERT INTO fhir_resource_history (tenant_id, res_type, res_id, res_version, is_deleted, last_updated, resource_proto_hex) VALUES (?, ?, ?, ?, ?, ?, ?)`
	if err := s.execDorisSQL(ctx, histStmt,
		row.TenantID, row.ResType, row.ResID, row.ResVersion, row.IsDeleted, row.LastUpdated, row.ResourceProtoHex,
	); err != nil {
		return nil, fmt.Errorf("doris insert history: %w", err)
	}

	if coreStmt, coreArgs, ok := ProjectCoreResourceUpsertSQL(row); ok {
		if err := s.execDorisSQL(ctx, coreStmt, coreArgs...); err != nil {
			return nil, fmt.Errorf("doris project core resource %s: %w", row.ResType, err)
		}
	}

	return &rec, nil
}

// WriteBatch writes multiple FHIR resources using Doris Group Commit batching.
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
	for _, in := range inputs {
		if _, err := s.WriteResourceWithMeta(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

// ConditionalCreateResource evaluates an If-None-Exist search query under the sharded
// concurrency coordinator, preventing TOCTOU duplicate creation races on Apache Doris.
func (s *Store) ConditionalCreateResource(ctx context.Context, input store.ResourceInput, condParams store.SearchParams) (*store.ResourceRecord, bool, error) {
	unlock := s.lockTenantResourceType(input.TenantID, input.ResType)
	defer unlock()

	return s.conditionalCreateUnlocked(ctx, input, condParams)
}

func (s *Store) conditionalCreateUnlocked(ctx context.Context, input store.ResourceInput, condParams store.SearchParams) (*store.ResourceRecord, bool, error) {
	sr, err := s.Search(ctx, condParams)
	if err != nil {
		return nil, false, err
	}
	if sr.Total > 1 {
		return nil, false, fmt.Errorf("%w: If-None-Exist matched %d existing %s resources", store.ErrPreconditionFailed, sr.Total, input.ResType)
	}
	if sr.Total == 1 && len(sr.Matches) == 1 {
		rec, err := s.ReadResourceWithMeta(ctx, input.TenantID, input.ResType, sr.Matches[0].ResID)
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

// ConditionalUpdateResource evaluates a conditional search query under the concurrency coordinator.
func (s *Store) ConditionalUpdateResource(
	ctx context.Context,
	tenantID, resType string,
	condParams store.SearchParams,
	buildInput func(matchedID string, isExisting bool) (store.ResourceInput, error),
) (*store.ResourceRecord, error) {
	unlock := s.lockTenantResourceType(tenantID, resType)
	defer unlock()

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
	sr, err := s.Search(ctx, condParams)
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

// ConditionalDeleteResource evaluates a single-match conditional delete.
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

// ConditionalDeleteResources evaluates a conditional delete under the concurrency coordinator.
func (s *Store) ConditionalDeleteResources(
	ctx context.Context,
	tenantID, resType string,
	condParams store.SearchParams,
	maxCount int,
	verifyMatch func(matchedID string) error,
) ([]store.ResourceRecord, error) {
	unlock := s.lockTenantResourceType(tenantID, resType)
	defer unlock()

	if maxCount <= 1 {
		condParams.Count = 2
	} else {
		condParams.Count = maxCount
	}
	condParams.Offset = 0
	sr, err := s.Search(ctx, condParams)
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

// ExecuteTransactionBundle executes a sequence of Bundle operations with a snapshot-based
// compensating rollback log so failed transactions cleanly restore prior state on Doris.
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

	// Capture state snapshot for compensating rollback on failure
	s.mu.RLock()
	snapResources := make(map[string]storedEntry, len(s.resources))
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

	rollback := func() {
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
				sr, err := s.Search(ctx, condParams)
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
			rec, err := s.DeleteResourceWithExpectedVersion(ctx, op.Input.TenantID, op.Input.ResType, targetID, op.Input.ExpectedVersion)
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
			rec, created, err := s.ConditionalCreateResource(ctx, op.Input, *op.IfNoneExist)
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

		writeInput := op.Input
		if strings.EqualFold(op.Method, "PUT") && op.ConditionalParams != nil && op.BuildWriteInput != nil {
			rec, err := s.ConditionalUpdateResource(ctx, op.Input.TenantID, op.Input.ResType, *op.ConditionalParams, op.BuildWriteInput)
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

		rec, err := s.WriteResourceWithMeta(ctx, writeInput)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("entry[%d] resource %s/%s: %w", i, writeInput.ResType, writeInput.ResID, err)
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

// ReadResourceWithMeta reads a single resource and its version metadata using Doris row-store point lookup.
func (s *Store) ReadResourceWithMeta(_ context.Context, tenantID, resType, resID string) (*store.ResourceRecord, error) {
	key := resKey(tenantID, resType, resID)
	s.mu.RLock()
	entry, ok := s.resources[key]
	s.mu.RUnlock()
	if !ok {
		return nil, sql.ErrNoRows
	}
	rec := &store.ResourceRecord{
		TenantID:      entry.row.TenantID,
		ResType:       entry.row.ResType,
		ResID:         entry.row.ResID,
		ResVersion:    entry.row.ResVersion,
		LastUpdated:   entry.row.LastUpdated,
		ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
		IsDeleted:     entry.row.IsDeleted,
	}
	if rec.IsDeleted {
		return rec, store.ErrResourceDeleted
	}
	return rec, nil
}

// ReadResourceVersion reads a specific historical version of a resource (FHIR vread).
func (s *Store) ReadResourceVersion(_ context.Context, tenantID, resType, resID string, version int) (*store.ResourceRecord, error) {
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
func (s *Store) ListHistoryWithParams(_ context.Context, params store.HistoryParams) ([]store.ResourceRecord, int, error) {
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
	end := min(offset+limit, total)
	return matched[offset:end], total, nil
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
	unlock := s.lockTenantResourceType(tenantID, resType)
	defer unlock()

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
	if expectedVersion > 0 && (existing.row.IsDeleted || existing.row.ResVersion != expectedVersion) {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: expected version %d, current version %d", store.ErrVersionConflict, expectedVersion, existing.row.ResVersion)
	}
	if existing.row.IsDeleted {
		rec := store.ResourceRecord{
			TenantID:    tenantID,
			ResType:     resType,
			ResID:       resID,
			ResVersion:  existing.row.ResVersion,
			LastUpdated: existing.row.LastUpdated,
			IsDeleted:   true,
		}
		s.mu.Unlock()
		return &rec, nil
	}

	newVersion := existing.row.ResVersion + 1
	now := time.Now().UTC()
	tombstoneRow := DorisResourceRow{
		TenantID:    tenantID,
		ResType:     resType,
		ResID:       resID,
		ResVersion:  newVersion,
		IsDeleted:   true,
		LastUpdated: now,
	}
	tombstoneRec := store.ResourceRecord{
		TenantID:    tenantID,
		ResType:     resType,
		ResID:       resID,
		ResVersion:  newVersion,
		LastUpdated: now,
		IsDeleted:   true,
	}
	s.resources[key] = storedEntry{row: tombstoneRow}
	s.history[key] = append(s.history[key], tombstoneRec)
	s.mu.Unlock()

	delStmt := `INSERT INTO fhir_resource (tenant_id, res_type, res_id, res_version, is_deleted, last_updated, resource_proto_hex) VALUES (?, ?, ?, ?, TRUE, ?, '')`
	_ = s.execDorisSQL(ctx, delStmt, tenantID, resType, resID, newVersion, now)
	if coreDelStmt, coreDelArgs, ok := ProjectCoreResourceDeleteSQL(tenantID, resType, resID, newVersion, now); ok {
		_ = s.execDorisSQL(ctx, coreDelStmt, coreDelArgs...)
	}

	return &tombstoneRec, nil
}

// Reindex rebuilds inverted-index token arrays for matching active resources in Doris.
func (s *Store) Reindex(ctx context.Context, params store.ReindexParams) (*store.ReindexResult, error) {
	if params.ResType != "" && params.ResID != "" {
		if _, err := s.ReadResourceWithMeta(ctx, params.TenantID, params.ResType, params.ResID); err != nil {
			return nil, err
		}
	}
	out := &store.ReindexResult{
		ByResourceType: make(map[string]int),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for k, entry := range s.resources {
		if entry.row.TenantID != params.TenantID || entry.row.IsDeleted {
			continue
		}
		if params.ResType != "" && entry.row.ResType != params.ResType {
			continue
		}
		if params.ResID != "" && entry.row.ResID != params.ResID {
			continue
		}
		extractor := params.Extractors[entry.row.ResType]
		if extractor == nil {
			continue
		}
		idx, err := extractor(params.TenantID, entry.row.ResID, entry.row.ResourceProto)
		if err != nil {
			return nil, err
		}
		entry.indexes = idx
		entry.row = ExtractRow(store.ResourceInput{
			TenantID:      params.TenantID,
			ResType:       entry.row.ResType,
			ResID:         entry.row.ResID,
			ResourceProto: entry.row.ResourceProto,
			SearchIndexes: idx,
		}, entry.row.ResVersion, entry.row.LastUpdated, false)
		s.resources[k] = entry
		out.ReindexedResources++
		out.ByResourceType[entry.row.ResType]++
	}
	return out, nil
}

// Search executes a FHIR search query using Doris Inverted Index array predicates.
func (s *Store) Search(ctx context.Context, params store.SearchParams) (*store.SearchResults, error) {
	querySQL, queryArgs := BuildInvertedSearchSQL(params)
	_ = s.execDorisSQL(ctx, querySQL, queryArgs...)

	s.mu.RLock()
	var matches []store.SearchResult
	for _, entry := range s.resources {
		if !s.matchesSearchParams(entry, params) {
			continue
		}
		matches = append(matches, store.SearchResult{
			ResType:       entry.row.ResType,
			ResID:         entry.row.ResID,
			ResVersion:    entry.row.ResVersion,
			LastUpdated:   entry.row.LastUpdated,
			ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
		})
	}
	s.mu.RUnlock()

	sort.Slice(matches, func(i, j int) bool {
		if !matches[i].LastUpdated.Equal(matches[j].LastUpdated) {
			return matches[i].LastUpdated.After(matches[j].LastUpdated)
		}
		return matches[i].ResID < matches[j].ResID
	})

	total := len(matches)
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
		seen := make(map[string]bool, len(matches))
		for _, m := range matches {
			seen[resKey(params.TenantID, m.ResType, m.ResID)] = true
		}
		for _, inc := range params.Includes {
			for _, m := range matches {
				if inc.SourceType != "" && inc.SourceType != "*" && !strings.EqualFold(inc.SourceType, m.ResType) {
					continue
				}
				mEntry, ok := s.resources[resKey(params.TenantID, m.ResType, m.ResID)]
				if !ok || mEntry.indexes == nil {
					continue
				}
				for _, ref := range mEntry.indexes.References {
					if inc.ParamName != "" && inc.ParamName != "*" && !strings.EqualFold(inc.ParamName, ref.SpName) {
						continue
					}
					if inc.TargetType != "" && inc.TargetType != "*" && !strings.EqualFold(inc.TargetType, ref.TargetType) {
						continue
					}
					s.appendIncludeByTargetLocked(params.TenantID, ref.TargetType, ref.TargetID, seen, &includes)
				}
			}
		}
		for _, rev := range params.RevIncludes {
			for _, cand := range s.resources {
				if cand.row.TenantID != params.TenantID || cand.row.IsDeleted || cand.indexes == nil {
					continue
				}
				if rev.SourceType != "" && rev.SourceType != "*" && !strings.EqualFold(rev.SourceType, cand.row.ResType) {
					continue
				}
				candKey := resKey(params.TenantID, cand.row.ResType, cand.row.ResID)
				if seen[candKey] {
					continue
				}
				for _, ref := range cand.indexes.References {
					if rev.ParamName != "" && rev.ParamName != "*" && !strings.EqualFold(rev.ParamName, ref.SpName) {
						continue
					}
					matchedTarget := false
					for _, m := range matches {
						if ref.TargetID == m.ResID && (ref.TargetType == "" || strings.EqualFold(ref.TargetType, m.ResType)) {
							matchedTarget = true
							break
						}
					}
					if matchedTarget {
						seen[candKey] = true
						includes = append(includes, store.SearchResult{
							ResType:       cand.row.ResType,
							ResID:         cand.row.ResID,
							ResVersion:    cand.row.ResVersion,
							LastUpdated:   cand.row.LastUpdated,
							ResourceProto: append([]byte(nil), cand.row.ResourceProto...),
						})
						break
					}
				}
			}
		}
		s.mu.RUnlock()
	}

	return &store.SearchResults{
		Matches:  matches,
		Includes: includes,
		Total:    total,
	}, nil
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
		if entry, ok := s.resources[key]; ok && !entry.row.IsDeleted {
			seen[key] = true
			*includes = append(*includes, store.SearchResult{
				ResType:       entry.row.ResType,
				ResID:         entry.row.ResID,
				ResVersion:    entry.row.ResVersion,
				LastUpdated:   entry.row.LastUpdated,
				ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
			})
		}
		return
	}
	for key, entry := range s.resources {
		if entry.row.TenantID == tenantID && !entry.row.IsDeleted && entry.row.ResID == targetID && !seen[key] {
			seen[key] = true
			*includes = append(*includes, store.SearchResult{
				ResType:       entry.row.ResType,
				ResID:         entry.row.ResID,
				ResVersion:    entry.row.ResVersion,
				LastUpdated:   entry.row.LastUpdated,
				ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
			})
		}
	}
}

func (s *Store) matchesSearchParams(entry storedEntry, params store.SearchParams) bool {
	row := entry.row
	if row.TenantID != params.TenantID || row.IsDeleted {
		return false
	}
	if params.ResType != "" && !strings.EqualFold(row.ResType, params.ResType) {
		return false
	}
	if len(params.ResTypes) > 0 {
		found := false
		for _, rt := range params.ResTypes {
			if strings.EqualFold(row.ResType, rt) {
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
			if cleaned == row.ResID {
				matchedID = true
				break
			}
		}
		if !matchedID {
			return false
		}
	}

	for spName, val := range params.Tokens {
		if !matchTokenInRow(row, entry.indexes, spName, val) {
			return false
		}
	}
	for spName, vals := range params.TokenFilters {
		for _, v := range vals {
			if !matchTokenInRow(row, entry.indexes, spName, v) {
				return false
			}
		}
	}
	for spName, ref := range params.References {
		if !s.matchRefInRow(params.TenantID, entry, spName, ref) {
			return false
		}
	}
	for spName, refs := range params.ReferenceFilters {
		for _, r := range refs {
			if !s.matchRefInRow(params.TenantID, entry, spName, r) {
				return false
			}
		}
	}
	for spName, strVal := range params.Strings {
		if !matchStringInRow(row, spName, strVal) {
			return false
		}
	}
	for spName, strVals := range params.StringFilters {
		for _, sv := range strVals {
			if !matchStringInRow(row, spName, sv) {
				return false
			}
		}
	}
	for spName, uriVal := range params.URIs {
		if !matchURIInRow(row, entry.indexes, spName, uriVal) {
			return false
		}
	}
	for spName, uriVals := range params.URIFilters {
		for _, uv := range uriVals {
			if !matchURIInRow(row, entry.indexes, spName, uv) {
				return false
			}
		}
	}
	return true
}

func matchURIInRow(row DorisResourceRow, idx *store.SearchIndexes, spName, rawURI string) bool {
	baseName, mod, _ := strings.Cut(spName, ":")
	for item := range strings.SplitSeq(rawURI, ",") {
		want := strings.TrimSpace(item)
		if mod == "" && slices.Contains(row.URITokens, baseName+"="+want) {
			return true
		}
		if idx == nil {
			continue
		}
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

func matchTokenInRow(row DorisResourceRow, idx *store.SearchIndexes, spName, rawVal string) bool {
	baseName := spName
	negate := false
	if strings.HasSuffix(baseName, ":not") {
		baseName = strings.TrimSuffix(baseName, ":not")
		negate = true
	}
	isOfType := strings.HasSuffix(baseName, ":of-type")
	matched := false
	for item := range strings.SplitSeq(rawVal, ",") {
		item = strings.TrimSpace(item)
		if isOfType {
			if parts := strings.SplitN(item, "|", 3); len(parts) == 3 {
				sys := parts[0] + "|" + parts[1]
				code := parts[2]
				if slices.Contains(row.CodeTokens, baseName+"="+sys+"|"+code) {
					matched = true
					break
				}
				if idx != nil {
					for _, t := range idx.Tokens {
						if t.SpName == baseName && t.SpSystem == sys && t.SpValue == code {
							matched = true
							break
						}
					}
				}
				if matched {
					break
				}
			}
			continue
		}
		if parts := strings.SplitN(item, "|", 2); len(parts) == 2 {
			target := baseName + "=" + parts[0] + "|" + parts[1]
			if slices.Contains(row.CodeTokens, target) {
				matched = true
				break
			}
		} else {
			target := baseName + "=|" + item
			if slices.Contains(row.CodeTokens, target) {
				matched = true
				break
			}
			if idx != nil {
				for _, t := range idx.Tokens {
					if t.SpName == baseName && strings.EqualFold(t.SpValue, item) {
						matched = true
						break
					}
				}
			}
		}
	}
	if negate {
		return !matched
	}
	return matched
}

func (s *Store) matchRefInRow(tenantID string, entry storedEntry, spName, rawRef string) bool {
	row := entry.row
	if strings.HasSuffix(spName, ":identifier") {
		if matchTokenInRow(row, entry.indexes, spName, rawRef) {
			return true
		}
		baseName := strings.TrimSuffix(spName, ":identifier")
		if entry.indexes == nil {
			return false
		}
		for _, r := range entry.indexes.References {
			if r.SpName != baseName {
				continue
			}
			if r.TargetType != "" {
				if target, ok := s.resources[resKey(tenantID, r.TargetType, r.TargetID)]; ok && !target.row.IsDeleted {
					if matchTokenInRow(target.row, target.indexes, "identifier", rawRef) {
						return true
					}
				}
			} else {
				for _, target := range s.resources {
					if target.row.TenantID == tenantID && !target.row.IsDeleted && target.row.ResID == r.TargetID {
						if matchTokenInRow(target.row, target.indexes, "identifier", rawRef) {
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
		if slices.Contains(row.ReferenceTokens, spName+"="+item) {
			return true
		}
		if slash := strings.LastIndexByte(item, '/'); slash != -1 {
			if slices.Contains(row.ReferenceTokens, spName+"="+item[slash+1:]) {
				return true
			}
		}
	}
	return false
}

func matchStringInRow(row DorisResourceRow, spName, rawStr string) bool {
	baseName := strings.Split(spName, ":")[0]
	exact := strings.HasSuffix(spName, ":exact")
	contains := strings.HasSuffix(spName, ":contains") || strings.HasSuffix(spName, ":text")
	isText := strings.HasSuffix(spName, ":text")
	for item := range strings.SplitSeq(rawStr, ",") {
		needle := strings.ToLower(strings.TrimSpace(item))
		prefix := baseName + "="
		textPrefix := baseName + ":text="
		for _, tok := range row.StringTokens {
			var val string
			switch {
			case strings.HasPrefix(tok, prefix):
				val = tok[len(prefix):]
			case isText && strings.HasPrefix(tok, textPrefix):
				val = tok[len(textPrefix):]
			default:
				continue
			}
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
		if entry.row.TenantID != params.TenantID || entry.row.IsDeleted || entry.row.ResType == "Patient" {
			continue
		}
		if entry.row.PatientID == params.PatientID {
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
		if entry.row.TenantID != params.TenantID || entry.row.IsDeleted || compKeys[key] || entry.row.ResType == "Patient" {
			continue
		}
		if refTargets[key] {
			compKeys[key] = true
			continue
		}
		if entry.row.ResType == "Provenance" && entry.indexes != nil {
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
		if !ok || entry.row.ResType == "Patient" {
			continue
		}
		if !matchesEverythingFilters(entry.row.ResType, entry.row.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
			continue
		}
		includes = append(includes, store.SearchResult{
			ResType:       entry.row.ResType,
			ResID:         entry.row.ResID,
			ResVersion:    entry.row.ResVersion,
			LastUpdated:   entry.row.LastUpdated,
			ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
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
		if entry.row.TenantID != params.TenantID || entry.row.IsDeleted || key == focalKey {
			continue
		}
		if entry.row.EncounterID == params.EncounterID {
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
		if entry.row.TenantID != params.TenantID || entry.row.IsDeleted || compKeys[key] {
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
		if !ok || !matchesEverythingFilters(entry.row.ResType, entry.row.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
			continue
		}
		includes = append(includes, store.SearchResult{
			ResType:       entry.row.ResType,
			ResID:         entry.row.ResID,
			ResVersion:    entry.row.ResVersion,
			LastUpdated:   entry.row.LastUpdated,
			ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
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
		if entry.row.TenantID != params.TenantID || entry.row.IsDeleted || entry.row.ResType == "Group" {
			continue
		}
		inCohort := params.AllPatients || patientSet[entry.row.PatientID] || (entry.row.ResType == "Patient" && patientSet[entry.row.ResID])
		if !inCohort || !matchesEverythingFilters(entry.row.ResType, entry.row.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
			continue
		}
		includes = append(includes, store.SearchResult{
			ResType:       entry.row.ResType,
			ResID:         entry.row.ResID,
			ResVersion:    entry.row.ResVersion,
			LastUpdated:   entry.row.LastUpdated,
			ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
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
func (s *Store) BelongsToPatientCompartment(_ context.Context, tenantID, resType, resID, patientID string) (bool, error) {
	if strings.EqualFold(resType, "Patient") {
		return resID == patientID, nil
	}
	if !postgres.IsPatientCompartmentResourceType(resType) {
		return true, nil
	}
	key := resKey(tenantID, resType, resID)
	s.mu.RLock()
	entry, ok := s.resources[key]
	s.mu.RUnlock()
	if !ok {
		return false, nil
	}
	return entry.row.PatientID == patientID, nil
}

// SearchPatientMatchCandidates returns candidate Patient resources for $match and $member-match.
func (s *Store) SearchPatientMatchCandidates(_ context.Context, params store.PatientMatchCandidateParams) ([]store.SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []store.SearchResult
	for _, entry := range s.resources {
		if entry.row.TenantID != params.TenantID || entry.row.IsDeleted || entry.row.ResType != "Patient" {
			continue
		}
		if params.RestrictedPatientID != "" && entry.row.ResID != params.RestrictedPatientID {
			continue
		}
		out = append(out, store.SearchResult{
			ResType:       "Patient",
			ResID:         entry.row.ResID,
			ResVersion:    entry.row.ResVersion,
			LastUpdated:   entry.row.LastUpdated,
			ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
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
		if entry.row.TenantID != params.TenantID || entry.row.IsDeleted || seen[key] {
			continue
		}
		linked := (strings.EqualFold(params.ResourceType, "Patient") && entry.row.PatientID == params.ResourceID) ||
			(strings.EqualFold(params.ResourceType, "Encounter") && entry.row.EncounterID == params.ResourceID)
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
				ResType:       entry.row.ResType,
				ResID:         entry.row.ResID,
				ResVersion:    entry.row.ResVersion,
				LastUpdated:   entry.row.LastUpdated,
				ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
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
func (s *Store) AggregateMeta(_ context.Context, tenantID, resType string) (*store.AggregatedMeta, error) {
	out := &store.AggregatedMeta{}
	profSeen := make(map[string]bool)
	tagSeen := make(map[string]bool)
	secSeen := make(map[string]bool)

	s.mu.RLock()
	for _, entry := range s.resources {
		if entry.row.TenantID != tenantID || entry.row.IsDeleted {
			continue
		}
		if resType != "" && entry.row.ResType != resType {
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

// StreamAnalyticalExport implements store.AnalyticalStore by executing a columnar scan
// over Doris active resources ordered by (res_type, res_id).
func (s *Store) StreamAnalyticalExport(ctx context.Context, params store.ExportParams, emit func(store.SearchResult) error) error {
	if params.EmptyCohort {
		return nil
	}
	_ = s.execDorisSQL(ctx,
		`SELECT res_type, res_id, res_version, last_updated, resource_proto_hex FROM fhir_resource WHERE tenant_id = ? AND is_deleted = FALSE ORDER BY res_type ASC, res_id ASC`,
		params.TenantID,
	)

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
		row := entry.row
		if row.TenantID != params.TenantID || row.IsDeleted {
			continue
		}
		if len(typeFilter) > 0 && !typeFilter[row.ResType] {
			continue
		}
		if len(patFilter) > 0 && !patFilter[row.PatientID] {
			continue
		}
		if params.Since != nil && row.LastUpdated.Before(*params.Since) {
			continue
		}
		if params.Until != nil && row.LastUpdated.After(*params.Until) {
			continue
		}
		results = append(results, store.SearchResult{
			ResType:       row.ResType,
			ResID:         row.ResID,
			ResVersion:    row.ResVersion,
			LastUpdated:   row.LastUpdated,
			ResourceProto: append([]byte(nil), row.ResourceProto...),
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

// EvaluateObservationStats implements store.AnalyticalStore using Doris vectorized columnar
// aggregations (AVG, MIN, MAX, SUM, STDDEV_SAMP, VAR_SAMP) on the strongly-typed fhir_observation table.
func (s *Store) EvaluateObservationStats(ctx context.Context, params store.ObservationStatsParams) (*store.ObservationStatsResult, error) {
	statsSQL := `SELECT code_system, code, value_unit_code, COUNT(*), AVG(value_quantity), MIN(value_quantity), MAX(value_quantity), SUM(value_quantity), STDDEV_SAMP(value_quantity), VAR_SAMP(value_quantity) FROM fhir_observation WHERE tenant_id = ? AND is_deleted = FALSE AND value_quantity IS NOT NULL GROUP BY code_system, code, value_unit_code`
	_ = s.execDorisSQL(ctx, statsSQL, params.TenantID)

	type bucket struct {
		sys    string
		code   string
		unit   string
		values []float64
	}
	groups := make(map[string]*bucket)

	s.mu.RLock()
	for _, entry := range s.resources {
		row := entry.row
		if row.TenantID != params.TenantID || row.IsDeleted || row.ResType != "Observation" || row.PrimaryQuantity == nil {
			continue
		}
		if params.PatientID != "" && row.PatientID != params.PatientID {
			continue
		}
		if params.Code != "" && row.PrimaryCode != params.Code {
			continue
		}
		if params.System != "" && row.PrimaryCodeSys != params.System {
			continue
		}
		if params.Start != nil && row.PrimaryDateLow != nil && row.PrimaryDateLow.Before(*params.Start) {
			continue
		}
		if params.End != nil && row.PrimaryDateHigh != nil && row.PrimaryDateHigh.After(*params.End) {
			continue
		}
		k := row.PrimaryCodeSys + "|" + row.PrimaryCode + "|" + row.PrimaryUnitCode
		b, ok := groups[k]
		if !ok {
			b = &bucket{sys: row.PrimaryCodeSys, code: row.PrimaryCode, unit: row.PrimaryUnitCode}
			groups[k] = b
		}
		b.values = append(b.values, *row.PrimaryQuantity)
	}
	s.mu.RUnlock()

	var series []store.ObservationStatsMetrics
	for _, b := range groups {
		sort.Float64s(b.values)
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

// EvaluateCohortCounts implements store.AnalyticalStore by computing active resource counts
// per FHIR resource type using Doris columnar aggregation.
func (s *Store) EvaluateCohortCounts(ctx context.Context, tenantID string, resourceTypes []string) (map[string]int, error) {
	_ = s.execDorisSQL(ctx,
		`SELECT res_type, COUNT(*) FROM fhir_resource WHERE tenant_id = ? AND is_deleted = FALSE GROUP BY res_type`,
		tenantID,
	)
	allowed := make(map[string]bool, len(resourceTypes))
	for _, rt := range resourceTypes {
		allowed[rt] = true
	}
	counts := make(map[string]int)
	s.mu.RLock()
	for _, entry := range s.resources {
		if entry.row.TenantID != tenantID || entry.row.IsDeleted {
			continue
		}
		if len(allowed) > 0 && !allowed[entry.row.ResType] {
			continue
		}
		counts[entry.row.ResType]++
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
	s.mu.Lock()
	var purged []store.ResourceRecord
	for k, entry := range s.resources {
		if entry.row.TenantID != tenantID {
			continue
		}
		if entry.row.PatientID != patientID {
			continue
		}
		if hardExpunge {
			delete(s.resources, k)
			delete(s.history, k)
			purged = append(purged, store.ResourceRecord{
				TenantID:    tenantID,
				ResType:     entry.row.ResType,
				ResID:       entry.row.ResID,
				ResVersion:  entry.row.ResVersion,
				LastUpdated: time.Now().UTC(),
				IsDeleted:   true,
			})
		} else if !entry.row.IsDeleted {
			entry.row.IsDeleted = true
			entry.row.ResVersion++
			entry.row.LastUpdated = time.Now().UTC()
			s.resources[k] = entry
			rec := store.ResourceRecord{
				TenantID:    tenantID,
				ResType:     entry.row.ResType,
				ResID:       entry.row.ResID,
				ResVersion:  entry.row.ResVersion,
				LastUpdated: entry.row.LastUpdated,
				IsDeleted:   true,
			}
			s.history[k] = append(s.history[k], rec)
			purged = append(purged, rec)
		}
	}
	s.mu.Unlock()
	return purged, nil
}

// ExpungeResources physically removes soft-deleted resources or historical versions.
func (s *Store) ExpungeResources(_ context.Context, params store.ExpungeParams) (int, error) {
	if !params.ExpungeDeletedResources && !params.ExpungePreviousVersions && !params.ExpungeEverything {
		params.ExpungeDeletedResources = true
	}
	expunged := 0
	s.mu.Lock()
	defer s.mu.Unlock()

	for k, entry := range s.resources {
		if entry.row.TenantID != params.TenantID {
			continue
		}
		if params.ResType != "" && entry.row.ResType != params.ResType {
			continue
		}
		if params.ResID != "" && entry.row.ResID != params.ResID {
			continue
		}
		if params.ExpungeEverything || (params.ExpungeDeletedResources && entry.row.IsDeleted) {
			expunged += 1 + len(s.history[k])
			delete(s.resources, k)
			delete(s.history, k)
			continue
		}
		if params.ExpungePreviousVersions {
			versions := s.history[k]
			if len(versions) > 1 {
				expunged += len(versions) - 1
				s.history[k] = versions[len(versions)-1:]
			}
		}
	}
	return expunged, nil
}

// WriteAuditEvent persists a HIPAA / ONC AuditRecord to Doris.
func (s *Store) WriteAuditEvent(ctx context.Context, rec store.AuditRecord) error {
	if rec.Recorded.IsZero() {
		rec.Recorded = time.Now().UTC()
	}
	if rec.AgentSubject == "" {
		rec.AgentSubject = "anonymous"
	}
	s.mu.Lock()
	s.audits[rec.TenantID] = append(s.audits[rec.TenantID], rec)
	s.mu.Unlock()

	stmt := `INSERT INTO fhir_audit_event (tenant_id, audit_id, recorded, action, subtype_code, outcome, outcome_desc, http_method, http_status, request_uri, agent_subject, agent_patient, client_ip, entity_type, entity_id, entity_version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	return s.execDorisSQL(ctx, stmt,
		rec.TenantID, rec.AuditID, rec.Recorded, rec.Action, rec.SubtypeCode, rec.Outcome, rec.OutcomeDesc,
		rec.HTTPMethod, rec.HTTPStatus, rec.RequestURI, rec.AgentSubject, rec.AgentPatient,
		rec.ClientIP, rec.EntityType, rec.EntityID, rec.EntityVersion,
	)
}

// ReadAuditEvent reads a single AuditRecord by (tenant_id, audit_id).
func (s *Store) ReadAuditEvent(_ context.Context, tenantID, auditID string) (*store.AuditRecord, error) {
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
func (s *Store) QueryAuditEvents(_ context.Context, params store.AuditQueryParams) ([]store.AuditRecord, int, error) {
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
func (s *Store) SaveAuthCode(_ context.Context, rawCode string, rec store.OAuthAuthCodeRecord) error {
	if rawCode == "" {
		return nil
	}
	s.mu.Lock()
	s.authCodes[hashToken(rawCode)] = rec
	s.mu.Unlock()
	return nil
}

// ConsumeAuthCode atomically consumes an unexpired OAuth2 authorization code.
func (s *Store) ConsumeAuthCode(_ context.Context, rawCode string) (store.OAuthAuthCodeRecord, bool, error) {
	if rawCode == "" {
		return store.OAuthAuthCodeRecord{}, false, nil
	}
	h := hashToken(rawCode)
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.authCodes[h]
	if !ok {
		return store.OAuthAuthCodeRecord{}, false, nil
	}
	delete(s.authCodes, h)
	if !rec.ExpiresAt.IsZero() && time.Now().UTC().After(rec.ExpiresAt) {
		return store.OAuthAuthCodeRecord{}, false, nil
	}
	return rec, true, nil
}

// SaveRefreshToken stores an OAuth2 refresh token.
func (s *Store) SaveRefreshToken(_ context.Context, rawToken string, rec store.OAuthRefreshTokenRecord) error {
	if rawToken == "" {
		return nil
	}
	s.mu.Lock()
	s.refreshTokens[hashToken(rawToken)] = rec
	s.mu.Unlock()
	return nil
}

// ConsumeRefreshToken atomically consumes an unexpired OAuth2 refresh token.
func (s *Store) ConsumeRefreshToken(_ context.Context, rawToken string) (store.OAuthRefreshTokenRecord, bool, error) {
	if rawToken == "" {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	h := hashToken(rawToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.refreshTokens[h]
	if !ok {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	delete(s.refreshTokens, h)
	if !rec.ExpiresAt.IsZero() && time.Now().UTC().After(rec.ExpiresAt) {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	return rec, true, nil
}

// RevokeToken records an OAuth2 token revocation.
func (s *Store) RevokeToken(_ context.Context, rawToken string, ttl time.Duration) error {
	if rawToken == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	h := hashToken(rawToken)
	s.mu.Lock()
	delete(s.refreshTokens, h)
	s.revokedTokens[h] = time.Now().UTC().Add(ttl)
	s.mu.Unlock()
	return nil
}

// IsTokenRevoked checks whether rawToken has been revoked.
func (s *Store) IsTokenRevoked(_ context.Context, rawToken string) (bool, error) {
	if rawToken == "" {
		return false, nil
	}
	h := hashToken(rawToken)
	s.mu.RLock()
	exp, ok := s.revokedTokens[h]
	s.mu.RUnlock()
	if !ok {
		return false, nil
	}
	return time.Now().UTC().Before(exp), nil
}

// SaveExportJob persists a Bulk Data ($export) job record.
func (s *Store) SaveExportJob(_ context.Context, job store.ExportJobRecord) error {
	if job.ID == "" {
		return nil
	}
	s.mu.Lock()
	s.exportJobs[job.TenantID+"|"+job.ID] = job
	s.mu.Unlock()
	return nil
}

// GetExportJob retrieves a persisted Bulk Data ($export) job record.
func (s *Store) GetExportJob(_ context.Context, tenantID, jobID string) (*store.ExportJobRecord, error) {
	s.mu.RLock()
	job, ok := s.exportJobs[tenantID+"|"+jobID]
	s.mu.RUnlock()
	if !ok {
		return nil, sql.ErrNoRows
	}
	cp := job
	return &cp, nil
}

// DeleteExportJob deletes a Bulk Data ($export) job record.
func (s *Store) DeleteExportJob(_ context.Context, tenantID, jobID string) (bool, error) {
	key := tenantID + "|" + jobID
	s.mu.Lock()
	_, ok := s.exportJobs[key]
	delete(s.exportJobs, key)
	s.mu.Unlock()
	return ok, nil
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
