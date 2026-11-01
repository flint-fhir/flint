package store

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/flint-fhir/flint/store/postgres"
)

// Compile-time interface assertions.
var (
	_ Store           = (*HybridStore)(nil)
	_ AnalyticalStore = (*HybridStore)(nil)
)

// HybridStore combines a transactional OLTP primary Store (e.g. *postgres.Store) with a
// columnar/analytical replica Store (e.g. *doris.Store or *sqlite.Store).
//
// All committed operational mutations on the primary store (creates, updates, conditional
// writes, transaction bundles, deletes, purges, expunges, reindexes, and audit events)
// are automatically replicated to the analytical store with extracted search indexes,
// while transactional point reads and OLTP searches execute against the primary store
// and analytical workloads (Observation/$stats, Bulk $export, cohort counts) execute
// against the analytical replica.
type HybridStore struct {
	primary           Store
	replica           Store
	analytical        AnalyticalStore
	extractors        map[string]IndexExtractorFunc
	replicatedWrites  atomic.Uint64
	replicatedDeletes atomic.Uint64
	replicationErrors atomic.Uint64
	syncedResources   atomic.Uint64
	lastSyncUnixNano  atomic.Int64
}

// NewHybridStore creates a new HybridStore backed by primary (OLTP) and replica (OLAP).
func NewHybridStore(primary Store, replica Store) *HybridStore {
	var analytical AnalyticalStore
	if as, ok := replica.(AnalyticalStore); ok && !IsNil(as) {
		analytical = as
	}
	return &HybridStore{
		primary:    primary,
		replica:    replica,
		analytical: analytical,
		extractors: postgres.DefaultIndexExtractors(),
	}
}

// Primary returns the underlying transactional OLTP primary store.
func (h *HybridStore) Primary() Store {
	return h.primary
}

// Replica returns the underlying analytical replica store.
func (h *HybridStore) Replica() Store {
	return h.replica
}

// ReplicatedWrites returns the total number of resource writes replicated to the analytical store.
func (h *HybridStore) ReplicatedWrites() uint64 {
	return h.replicatedWrites.Load()
}

// ReplicatedDeletes returns the total number of resource deletes/purges replicated to the analytical store.
func (h *HybridStore) ReplicatedDeletes() uint64 {
	return h.replicatedDeletes.Load()
}

// ReplicationErrors returns the total number of replication errors encountered on the analytical store.
func (h *HybridStore) ReplicationErrors() uint64 {
	return h.replicationErrors.Load()
}

// SyncedResources returns the cumulative number of resources backfilled via SyncTenant.
func (h *HybridStore) SyncedResources() uint64 {
	return h.syncedResources.Load()
}

// LastSyncTime returns the UTC timestamp of the most recent completed SyncTenant run, or zero time.
func (h *HybridStore) LastSyncTime() time.Time {
	ns := h.lastSyncUnixNano.Load()
	if ns <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// SyncTenant backfills all active resources for tenantID from the primary operational store
// into the analytical replica store in multi-row batches, extracting search indexes automatically.
func (h *HybridStore) SyncTenant(ctx context.Context, tenantID string) (int, error) {
	if h == nil || IsNil(h.primary) || IsNil(h.replica) {
		return 0, nil
	}
	const batchSize = 250
	var batch []ResourceInput
	synced := 0

	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		wErr := h.replica.WriteBatch(ctx, batch)
		h.recordWriteReplication(wErr, uint64(len(batch)))
		if wErr != nil {
			return wErr
		}
		synced += len(batch)
		h.syncedResources.Add(uint64(len(batch)))
		batch = batch[:0]
		return nil
	}

	err := h.primary.StreamExportResources(ctx, ExportParams{TenantID: tenantID}, func(sr SearchResult) error {
		repIn := h.ensureSearchIndexes(ResourceInput{
			TenantID:      tenantID,
			ResType:       sr.ResType,
			ResID:         sr.ResID,
			ResourceProto: sr.ResourceProto,
			CreateOnly:    false,
		})
		batch = append(batch, repIn)
		if len(batch) >= batchSize {
			return flushBatch()
		}
		return nil
	})
	if err != nil {
		return synced, err
	}
	if err := flushBatch(); err != nil {
		return synced, err
	}
	h.lastSyncUnixNano.Store(time.Now().UTC().UnixNano())
	return synced, nil
}

func (h *HybridStore) recordWriteReplication(err error, count uint64) {
	if err != nil {
		h.replicationErrors.Add(1)
		return
	}
	h.replicatedWrites.Add(count)
}

func (h *HybridStore) recordDeleteReplication(err error, count uint64) {
	if err != nil {
		h.replicationErrors.Add(1)
		return
	}
	h.replicatedDeletes.Add(count)
}

func (h *HybridStore) ensureSearchIndexes(in ResourceInput) ResourceInput {
	if in.SearchIndexes == nil && len(in.ResourceProto) > 0 && h.extractors != nil {
		if ext, ok := h.extractors[in.ResType]; ok {
			if idx, err := ext(in.TenantID, in.ResID, in.ResourceProto); err == nil {
				in.SearchIndexes = idx
			}
		}
	}
	return in
}

func (h *HybridStore) replicateRecord(ctx context.Context, baseInput ResourceInput, rec *ResourceRecord) {
	if IsNil(h.replica) || rec == nil {
		return
	}
	repIn := ResourceInput{
		TenantID:      rec.TenantID,
		ResType:       rec.ResType,
		ResID:         rec.ResID,
		ResourceProto: rec.ResourceProto,
		SearchIndexes: baseInput.SearchIndexes,
		CreateOnly:    false,
	}
	if repIn.TenantID == "" {
		repIn.TenantID = baseInput.TenantID
	}
	if repIn.ResType == "" {
		repIn.ResType = baseInput.ResType
	}
	if repIn.ResID == "" {
		repIn.ResID = baseInput.ResID
	}
	if len(repIn.ResourceProto) == 0 {
		repIn.ResourceProto = baseInput.ResourceProto
	}
	repIn = h.ensureSearchIndexes(repIn)
	_, err := h.replica.WriteResourceWithMeta(ctx, repIn)
	h.recordWriteReplication(err, 1)
}

// Ping checks connectivity to both the primary store and the analytical replica.
func (h *HybridStore) Ping(ctx context.Context) error {
	if err := h.primary.Ping(ctx); err != nil {
		return err
	}
	if !IsNil(h.replica) {
		return h.replica.Ping(ctx)
	}
	return nil
}

// WriteResource writes to the primary store and replicates the committed state to the analytical store.
func (h *HybridStore) WriteResource(ctx context.Context, input ResourceInput) error {
	rec, err := h.primary.WriteResourceWithMeta(ctx, input)
	if err != nil {
		return err
	}
	h.replicateRecord(ctx, input, rec)
	return nil
}

// WriteResourceWithMeta writes to the primary store and replicates the committed state to the analytical store.
func (h *HybridStore) WriteResourceWithMeta(ctx context.Context, input ResourceInput) (*ResourceRecord, error) {
	rec, err := h.primary.WriteResourceWithMeta(ctx, input)
	if err != nil {
		return nil, err
	}
	h.replicateRecord(ctx, input, rec)
	return rec, nil
}

// WriteBatch writes a batch of resources to the primary store and replicates to the analytical store.
func (h *HybridStore) WriteBatch(ctx context.Context, inputs []ResourceInput) error {
	if err := h.primary.WriteBatch(ctx, inputs); err != nil {
		return err
	}
	if !IsNil(h.replica) && len(inputs) > 0 {
		repInputs := make([]ResourceInput, len(inputs))
		for i, in := range inputs {
			in.CreateOnly = false
			in.ExpectedVersion = 0
			repInputs[i] = h.ensureSearchIndexes(in)
		}
		err := h.replica.WriteBatch(ctx, repInputs)
		h.recordWriteReplication(err, uint64(len(repInputs)))
	}
	return nil
}

// ConditionalCreateResource executes conditional create on the primary store and replicates if created.
func (h *HybridStore) ConditionalCreateResource(ctx context.Context, input ResourceInput, condParams SearchParams) (*ResourceRecord, bool, error) {
	rec, created, err := h.primary.ConditionalCreateResource(ctx, input, condParams)
	if err != nil {
		return nil, false, err
	}
	if created && rec != nil {
		h.replicateRecord(ctx, input, rec)
	}
	return rec, created, nil
}

// ConditionalUpdateResource executes conditional update on the primary store and replicates the updated record.
func (h *HybridStore) ConditionalUpdateResource(
	ctx context.Context,
	tenantID, resType string,
	condParams SearchParams,
	buildInput func(matchedID string, isExisting bool) (ResourceInput, error),
) (*ResourceRecord, error) {
	var capturedInput ResourceInput
	rec, err := h.primary.ConditionalUpdateResource(ctx, tenantID, resType, condParams, func(matchedID string, isExisting bool) (ResourceInput, error) {
		in, bErr := buildInput(matchedID, isExisting)
		if bErr == nil {
			capturedInput = in
		}
		return in, bErr
	})
	if err != nil {
		return nil, err
	}
	h.replicateRecord(ctx, capturedInput, rec)
	return rec, nil
}

// ConditionalDeleteResource executes conditional delete on the primary store and replicates the soft-delete.
func (h *HybridStore) ConditionalDeleteResource(
	ctx context.Context,
	tenantID, resType string,
	condParams SearchParams,
	verifyMatch func(matchedID string) error,
) (*ResourceRecord, error) {
	rec, err := h.primary.ConditionalDeleteResource(ctx, tenantID, resType, condParams, verifyMatch)
	if err != nil {
		return nil, err
	}
	if !IsNil(h.replica) && rec != nil && rec.ResID != "" {
		_, rErr := h.replica.DeleteResourceWithMeta(ctx, tenantID, resType, rec.ResID)
		h.recordDeleteReplication(rErr, 1)
	}
	return rec, nil
}

// ConditionalDeleteResources executes multi-resource conditional delete on the primary store and replicates to the replica.
func (h *HybridStore) ConditionalDeleteResources(
	ctx context.Context,
	tenantID, resType string,
	condParams SearchParams,
	maxCount int,
	verifyMatch func(matchedID string) error,
) ([]ResourceRecord, error) {
	recs, err := h.primary.ConditionalDeleteResources(ctx, tenantID, resType, condParams, maxCount, verifyMatch)
	if err != nil {
		return nil, err
	}
	if !IsNil(h.replica) {
		for _, rec := range recs {
			if rec.ResID != "" {
				_, rErr := h.replica.DeleteResourceWithMeta(ctx, tenantID, resType, rec.ResID)
				h.recordDeleteReplication(rErr, 1)
			}
		}
	}
	return recs, nil
}

// ExecuteTransactionBundle executes an atomic transaction bundle on the primary store and replicates all committed operations.
func (h *HybridStore) ExecuteTransactionBundle(ctx context.Context, ops []BundleOperation) ([]BundleOpResult, error) {
	results, err := h.primary.ExecuteTransactionBundle(ctx, ops)
	if err != nil {
		return nil, err
	}
	if !IsNil(h.replica) {
		for i, op := range ops {
			if op.OpType == BundleOpDelete || strings.EqualFold(op.Method, "DELETE") {
				resID := op.Input.ResID
				if i < len(results) && results[i].ResID != "" {
					resID = results[i].ResID
				}
				if resID != "" {
					_, rErr := h.replica.DeleteResourceWithMeta(ctx, op.Input.TenantID, op.Input.ResType, resID)
					h.recordDeleteReplication(rErr, 1)
				}
				continue
			}
			if i < len(results) && results[i].Record != nil {
				h.replicateRecord(ctx, op.Input, results[i].Record)
			}
		}
	}
	return results, nil
}

// ReadResource reads from the primary operational store.
func (h *HybridStore) ReadResource(ctx context.Context, tenantID, resType, resID string) ([]byte, error) {
	return h.primary.ReadResource(ctx, tenantID, resType, resID)
}

// ReadResourceWithMeta reads from the primary operational store.
func (h *HybridStore) ReadResourceWithMeta(ctx context.Context, tenantID, resType, resID string) (*ResourceRecord, error) {
	return h.primary.ReadResourceWithMeta(ctx, tenantID, resType, resID)
}

// ReadResourceVersion reads a specific historical version from the primary operational store.
func (h *HybridStore) ReadResourceVersion(ctx context.Context, tenantID, resType, resID string, version int) (*ResourceRecord, error) {
	return h.primary.ReadResourceVersion(ctx, tenantID, resType, resID, version)
}

// ListResourceHistory lists instance history from the primary operational store.
func (h *HybridStore) ListResourceHistory(ctx context.Context, tenantID, resType, resID string, limit, offset int) ([]ResourceRecord, int, error) {
	return h.primary.ListResourceHistory(ctx, tenantID, resType, resID, limit, offset)
}

// ListHistory lists history from the primary operational store.
func (h *HybridStore) ListHistory(ctx context.Context, tenantID, resType, resID string, since *time.Time, limit, offset int) ([]ResourceRecord, int, error) {
	return h.primary.ListHistory(ctx, tenantID, resType, resID, since, limit, offset)
}

// ListHistoryWithParams lists history with filter parameters from the primary operational store.
func (h *HybridStore) ListHistoryWithParams(ctx context.Context, params HistoryParams) ([]ResourceRecord, int, error) {
	return h.primary.ListHistoryWithParams(ctx, params)
}

// DeleteResource soft-deletes a resource on the primary store and replicates to the analytical store.
func (h *HybridStore) DeleteResource(ctx context.Context, tenantID, resType, resID string) error {
	if err := h.primary.DeleteResource(ctx, tenantID, resType, resID); err != nil {
		return err
	}
	if !IsNil(h.replica) {
		rErr := h.replica.DeleteResource(ctx, tenantID, resType, resID)
		h.recordDeleteReplication(rErr, 1)
	}
	return nil
}

// DeleteResourceWithMeta soft-deletes a resource on the primary store and replicates to the analytical store.
func (h *HybridStore) DeleteResourceWithMeta(ctx context.Context, tenantID, resType, resID string) (*ResourceRecord, error) {
	rec, err := h.primary.DeleteResourceWithMeta(ctx, tenantID, resType, resID)
	if err != nil {
		return nil, err
	}
	if !IsNil(h.replica) {
		_, rErr := h.replica.DeleteResourceWithMeta(ctx, tenantID, resType, resID)
		h.recordDeleteReplication(rErr, 1)
	}
	return rec, nil
}

// DeleteResourceWithExpectedVersion performs version-checked delete on the primary store and replicates to the analytical store.
func (h *HybridStore) DeleteResourceWithExpectedVersion(ctx context.Context, tenantID, resType, resID string, expectedVersion int) (*ResourceRecord, error) {
	rec, err := h.primary.DeleteResourceWithExpectedVersion(ctx, tenantID, resType, resID, expectedVersion)
	if err != nil {
		return nil, err
	}
	if !IsNil(h.replica) {
		_, rErr := h.replica.DeleteResourceWithMeta(ctx, tenantID, resType, resID)
		h.recordDeleteReplication(rErr, 1)
	}
	return rec, nil
}

// PurgePatientCompartment purges a patient compartment on both the primary and analytical stores.
func (h *HybridStore) PurgePatientCompartment(ctx context.Context, tenantID, patientID string, hardExpunge bool) ([]ResourceRecord, error) {
	recs, err := h.primary.PurgePatientCompartment(ctx, tenantID, patientID, hardExpunge)
	if err != nil {
		return nil, err
	}
	if !IsNil(h.replica) {
		_, rErr := h.replica.PurgePatientCompartment(ctx, tenantID, patientID, hardExpunge)
		h.recordDeleteReplication(rErr, uint64(len(recs)))
	}
	return recs, nil
}

// ExpungeResources hard-deletes resources on both the primary and analytical stores.
func (h *HybridStore) ExpungeResources(ctx context.Context, params ExpungeParams) (int, error) {
	n, err := h.primary.ExpungeResources(ctx, params)
	if err != nil {
		return 0, err
	}
	if !IsNil(h.replica) {
		_, rErr := h.replica.ExpungeResources(ctx, params)
		h.recordDeleteReplication(rErr, uint64(n))
	}
	return n, nil
}

// Reindex re-extracts search indexes on both the primary and analytical stores.
func (h *HybridStore) Reindex(ctx context.Context, params ReindexParams) (*ReindexResult, error) {
	res, err := h.primary.Reindex(ctx, params)
	if err != nil {
		return nil, err
	}
	if !IsNil(h.replica) {
		_, _ = h.replica.Reindex(ctx, params)
	}
	return res, nil
}

// Search executes FHIR R4 search queries against the primary operational store.
func (h *HybridStore) Search(ctx context.Context, params SearchParams) (*SearchResults, error) {
	return h.primary.Search(ctx, params)
}

// PatientEverything assembles a Patient compartment bundle from the primary store.
func (h *HybridStore) PatientEverything(ctx context.Context, params PatientEverythingParams) (*SearchResults, error) {
	return h.primary.PatientEverything(ctx, params)
}

// EncounterEverything assembles an Encounter compartment bundle from the primary store.
func (h *HybridStore) EncounterEverything(ctx context.Context, params EncounterEverythingParams) (*SearchResults, error) {
	return h.primary.EncounterEverything(ctx, params)
}

// GroupEverything assembles a Group cohort bundle from the primary store.
func (h *HybridStore) GroupEverything(ctx context.Context, params GroupEverythingParams) (*SearchResults, error) {
	return h.primary.GroupEverything(ctx, params)
}

// BelongsToPatientCompartment checks compartment membership on the primary store.
func (h *HybridStore) BelongsToPatientCompartment(ctx context.Context, tenantID, resType, resID, patientID string) (bool, error) {
	return h.primary.BelongsToPatientCompartment(ctx, tenantID, resType, resID, patientID)
}

// SearchPatientMatchCandidates searches MPI patient candidates on the primary store.
func (h *HybridStore) SearchPatientMatchCandidates(ctx context.Context, params PatientMatchCandidateParams) ([]SearchResult, error) {
	return h.primary.SearchPatientMatchCandidates(ctx, params)
}

// ResourceGraph traverses GraphDefinition links on the primary store.
func (h *HybridStore) ResourceGraph(ctx context.Context, params ResourceGraphParams) (*SearchResults, error) {
	return h.primary.ResourceGraph(ctx, params)
}

// AggregateMeta aggregates profiles, security labels, and tags on the primary store.
func (h *HybridStore) AggregateMeta(ctx context.Context, tenantID, resType string) (*AggregatedMeta, error) {
	return h.primary.AggregateMeta(ctx, tenantID, resType)
}

// ExportResources exports resources from the analytical replica if available, otherwise from the primary store.
func (h *HybridStore) ExportResources(ctx context.Context, params ExportParams) ([]SearchResult, error) {
	if !IsNil(h.replica) {
		return h.replica.ExportResources(ctx, params)
	}
	return h.primary.ExportResources(ctx, params)
}

// StreamExportResources streams export resources from the analytical replica if available.
func (h *HybridStore) StreamExportResources(ctx context.Context, params ExportParams, emit func(SearchResult) error) error {
	if h.analytical != nil {
		return h.analytical.StreamAnalyticalExport(ctx, params, emit)
	}
	return h.primary.StreamExportResources(ctx, params, emit)
}

// SaveExportJob persists a bulk export job record on the primary store and replica.
func (h *HybridStore) SaveExportJob(ctx context.Context, job ExportJobRecord) error {
	if err := h.primary.SaveExportJob(ctx, job); err != nil {
		return err
	}
	if !IsNil(h.replica) {
		_ = h.replica.SaveExportJob(ctx, job)
	}
	return nil
}

// GetExportJob retrieves a bulk export job record from the primary store.
func (h *HybridStore) GetExportJob(ctx context.Context, tenantID, jobID string) (*ExportJobRecord, error) {
	return h.primary.GetExportJob(ctx, tenantID, jobID)
}

// DeleteExportJob deletes a bulk export job record from both primary and replica stores.
func (h *HybridStore) DeleteExportJob(ctx context.Context, tenantID, jobID string) (bool, error) {
	deleted, err := h.primary.DeleteExportJob(ctx, tenantID, jobID)
	if err != nil {
		return false, err
	}
	if !IsNil(h.replica) {
		_, _ = h.replica.DeleteExportJob(ctx, tenantID, jobID)
	}
	return deleted, nil
}

// WriteAuditEvent writes a security AuditEvent to the primary store and replicates to the analytical store.
func (h *HybridStore) WriteAuditEvent(ctx context.Context, rec AuditRecord) error {
	if err := h.primary.WriteAuditEvent(ctx, rec); err != nil {
		return err
	}
	if !IsNil(h.replica) {
		_ = h.replica.WriteAuditEvent(ctx, rec)
	}
	return nil
}

// ReadAuditEvent reads a security AuditEvent from the primary store.
func (h *HybridStore) ReadAuditEvent(ctx context.Context, tenantID, auditID string) (*AuditRecord, error) {
	return h.primary.ReadAuditEvent(ctx, tenantID, auditID)
}

// QueryAuditEvents queries security AuditEvents from the primary store.
func (h *HybridStore) QueryAuditEvents(ctx context.Context, params AuditQueryParams) ([]AuditRecord, int, error) {
	return h.primary.QueryAuditEvents(ctx, params)
}

// SaveAuthCode persists an OAuth2 authorization code on the primary store.
func (h *HybridStore) SaveAuthCode(ctx context.Context, rawCode string, rec OAuthAuthCodeRecord) error {
	return h.primary.SaveAuthCode(ctx, rawCode, rec)
}

// ConsumeAuthCode atomically consumes an OAuth2 authorization code on the primary store.
func (h *HybridStore) ConsumeAuthCode(ctx context.Context, rawCode string) (OAuthAuthCodeRecord, bool, error) {
	return h.primary.ConsumeAuthCode(ctx, rawCode)
}

// SaveRefreshToken persists an OAuth2 refresh token on the primary store.
func (h *HybridStore) SaveRefreshToken(ctx context.Context, rawToken string, rec OAuthRefreshTokenRecord) error {
	return h.primary.SaveRefreshToken(ctx, rawToken, rec)
}

// ConsumeRefreshToken atomically consumes an OAuth2 refresh token on the primary store.
func (h *HybridStore) ConsumeRefreshToken(ctx context.Context, rawToken string) (OAuthRefreshTokenRecord, bool, error) {
	return h.primary.ConsumeRefreshToken(ctx, rawToken)
}

// RevokeToken records a revoked OAuth2 token on the primary store.
func (h *HybridStore) RevokeToken(ctx context.Context, rawToken string, ttl time.Duration) error {
	return h.primary.RevokeToken(ctx, rawToken, ttl)
}

// IsTokenRevoked checks whether an OAuth2 token is revoked on the primary store.
func (h *HybridStore) IsTokenRevoked(ctx context.Context, rawToken string) (bool, error) {
	return h.primary.IsTokenRevoked(ctx, rawToken)
}

// BackendName returns the analytical backend identifier (e.g. "doris" or "sqlite").
func (h *HybridStore) BackendName() string {
	if h.analytical != nil {
		return strings.TrimSpace(h.analytical.BackendName())
	}
	return string(BackendPostgres)
}

// EvaluateObservationStats delegates statistical aggregation to the analytical store.
func (h *HybridStore) EvaluateObservationStats(ctx context.Context, params ObservationStatsParams) (*ObservationStatsResult, error) {
	if h.analytical != nil {
		return h.analytical.EvaluateObservationStats(ctx, params)
	}
	return &ObservationStatsResult{}, nil
}

// StreamAnalyticalExport delegates streaming bulk export to the analytical store.
func (h *HybridStore) StreamAnalyticalExport(ctx context.Context, params ExportParams, emit func(SearchResult) error) error {
	if h.analytical != nil {
		return h.analytical.StreamAnalyticalExport(ctx, params, emit)
	}
	return h.primary.StreamExportResources(ctx, params, emit)
}

// EvaluateCohortCounts delegates population cohort counts to the analytical store.
func (h *HybridStore) EvaluateCohortCounts(ctx context.Context, tenantID string, resourceTypes []string) (map[string]int, error) {
	if h.analytical != nil {
		return h.analytical.EvaluateCohortCounts(ctx, tenantID, resourceTypes)
	}
	return map[string]int{}, nil
}
