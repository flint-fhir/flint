package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/flint-fhir/flint/pkg/telemetry"
)

type tracingStore struct {
	inner  Store
	tracer *telemetry.Tracer
}

type tracingAnalyticalStore struct {
	*tracingStore
	analytical AnalyticalStore
}

// WrapWithTracing wraps s with OpenTelemetry SpanKindClient spans for storage operations.
// If s is nil or tracer is nil/disabled, s is returned unchanged. If s implements
// AnalyticalStore, the returned Store also implements AnalyticalStore.
func WrapWithTracing(s Store, tracer *telemetry.Tracer) Store {
	if IsNil(s) || tracer == nil || !tracer.Enabled() {
		return s
	}
	ts := &tracingStore{inner: s, tracer: tracer}
	if as, ok := s.(AnalyticalStore); ok {
		return &tracingAnalyticalStore{tracingStore: ts, analytical: as}
	}
	return ts
}

// Unwrap returns the underlying Store wrapped by tracingStore.
func (t *tracingStore) Unwrap() Store {
	if t == nil {
		return nil
	}
	return t.inner
}

func finishStoreSpan(sp *telemetry.Span, err error) {
	if err != nil {
		if errors.Is(err, ErrResourceDeleted) || strings.Contains(err.Error(), "no rows in result set") {
			sp.SetStatus(telemetry.StatusOK, "")
		} else {
			sp.SetStatus(telemetry.StatusError, err.Error())
		}
	} else {
		sp.SetStatus(telemetry.StatusOK, "")
	}
	sp.End()
}

func (t *tracingStore) startOp(ctx context.Context, op, tenantID, resType string) (context.Context, *telemetry.Span) {
	attrs := []telemetry.Attribute{
		telemetry.String("db.operation", op),
	}
	if tenantID != "" {
		attrs = append(attrs, telemetry.String("fhir.tenant", tenantID))
	}
	if resType != "" {
		attrs = append(attrs, telemetry.String("fhir.resource_type", resType))
	}
	return t.tracer.Start(ctx, "store."+op, telemetry.SpanKindClient, attrs...)
}

func (t *tracingStore) Ping(ctx context.Context) error {
	ctx, sp := t.startOp(ctx, "Ping", "", "")
	err := t.inner.Ping(ctx)
	finishStoreSpan(sp, err)
	return err
}

func (t *tracingStore) WriteResource(ctx context.Context, input ResourceInput) error {
	ctx, sp := t.startOp(ctx, "WriteResource", input.TenantID, input.ResType)
	err := t.inner.WriteResource(ctx, input)
	finishStoreSpan(sp, err)
	return err
}

func (t *tracingStore) WriteResourceWithMeta(ctx context.Context, input ResourceInput) (*ResourceRecord, error) {
	ctx, sp := t.startOp(ctx, "WriteResourceWithMeta", input.TenantID, input.ResType)
	rec, err := t.inner.WriteResourceWithMeta(ctx, input)
	finishStoreSpan(sp, err)
	return rec, err
}

func (t *tracingStore) WriteBatch(ctx context.Context, inputs []ResourceInput) error {
	var tenant string
	if len(inputs) > 0 {
		tenant = inputs[0].TenantID
	}
	ctx, sp := t.startOp(ctx, "WriteBatch", tenant, "")
	sp.SetAttributes(telemetry.Int("fhir.batch_size", len(inputs)))
	err := t.inner.WriteBatch(ctx, inputs)
	finishStoreSpan(sp, err)
	return err
}

func (t *tracingStore) ConditionalCreateResource(ctx context.Context, input ResourceInput, condParams SearchParams) (*ResourceRecord, bool, error) {
	ctx, sp := t.startOp(ctx, "ConditionalCreateResource", input.TenantID, input.ResType)
	rec, created, err := t.inner.ConditionalCreateResource(ctx, input, condParams)
	finishStoreSpan(sp, err)
	return rec, created, err
}

func (t *tracingStore) ConditionalUpdateResource(
	ctx context.Context,
	tenantID, resType string,
	condParams SearchParams,
	buildInput func(matchedID string, isExisting bool) (ResourceInput, error),
) (*ResourceRecord, error) {
	ctx, sp := t.startOp(ctx, "ConditionalUpdateResource", tenantID, resType)
	rec, err := t.inner.ConditionalUpdateResource(ctx, tenantID, resType, condParams, buildInput)
	finishStoreSpan(sp, err)
	return rec, err
}

func (t *tracingStore) ConditionalDeleteResource(
	ctx context.Context,
	tenantID, resType string,
	condParams SearchParams,
	verifyMatch func(matchedID string) error,
) (*ResourceRecord, error) {
	ctx, sp := t.startOp(ctx, "ConditionalDeleteResource", tenantID, resType)
	rec, err := t.inner.ConditionalDeleteResource(ctx, tenantID, resType, condParams, verifyMatch)
	finishStoreSpan(sp, err)
	return rec, err
}

func (t *tracingStore) ConditionalDeleteResources(
	ctx context.Context,
	tenantID, resType string,
	condParams SearchParams,
	maxCount int,
	verifyMatch func(matchedID string) error,
) ([]ResourceRecord, error) {
	ctx, sp := t.startOp(ctx, "ConditionalDeleteResources", tenantID, resType)
	recs, err := t.inner.ConditionalDeleteResources(ctx, tenantID, resType, condParams, maxCount, verifyMatch)
	finishStoreSpan(sp, err)
	return recs, err
}

func (t *tracingStore) ExecuteTransactionBundle(ctx context.Context, ops []BundleOperation) ([]BundleOpResult, error) {
	var tenant string
	if len(ops) > 0 {
		tenant = ops[0].Input.TenantID
	}
	ctx, sp := t.startOp(ctx, "ExecuteTransactionBundle", tenant, "Bundle")
	sp.SetAttributes(telemetry.Int("fhir.bundle_ops", len(ops)))
	res, err := t.inner.ExecuteTransactionBundle(ctx, ops)
	finishStoreSpan(sp, err)
	return res, err
}

func (t *tracingStore) ReadResource(ctx context.Context, tenantID, resType, resID string) ([]byte, error) {
	ctx, sp := t.startOp(ctx, "ReadResource", tenantID, resType)
	b, err := t.inner.ReadResource(ctx, tenantID, resType, resID)
	finishStoreSpan(sp, err)
	return b, err
}

func (t *tracingStore) ReadResourceWithMeta(ctx context.Context, tenantID, resType, resID string) (*ResourceRecord, error) {
	ctx, sp := t.startOp(ctx, "ReadResourceWithMeta", tenantID, resType)
	rec, err := t.inner.ReadResourceWithMeta(ctx, tenantID, resType, resID)
	finishStoreSpan(sp, err)
	return rec, err
}

func (t *tracingStore) ReadResourceVersion(ctx context.Context, tenantID, resType, resID string, version int) (*ResourceRecord, error) {
	ctx, sp := t.startOp(ctx, "ReadResourceVersion", tenantID, resType)
	rec, err := t.inner.ReadResourceVersion(ctx, tenantID, resType, resID, version)
	finishStoreSpan(sp, err)
	return rec, err
}

func (t *tracingStore) ListResourceHistory(ctx context.Context, tenantID, resType, resID string, limit, offset int) ([]ResourceRecord, int, error) {
	ctx, sp := t.startOp(ctx, "ListResourceHistory", tenantID, resType)
	recs, total, err := t.inner.ListResourceHistory(ctx, tenantID, resType, resID, limit, offset)
	finishStoreSpan(sp, err)
	return recs, total, err
}

func (t *tracingStore) ListHistory(ctx context.Context, tenantID, resType, resID string, since *time.Time, limit, offset int) ([]ResourceRecord, int, error) {
	ctx, sp := t.startOp(ctx, "ListHistory", tenantID, resType)
	recs, total, err := t.inner.ListHistory(ctx, tenantID, resType, resID, since, limit, offset)
	finishStoreSpan(sp, err)
	return recs, total, err
}

func (t *tracingStore) ListHistoryWithParams(ctx context.Context, params HistoryParams) ([]ResourceRecord, int, error) {
	ctx, sp := t.startOp(ctx, "ListHistoryWithParams", params.TenantID, params.ResType)
	recs, total, err := t.inner.ListHistoryWithParams(ctx, params)
	finishStoreSpan(sp, err)
	return recs, total, err
}

func (t *tracingStore) DeleteResource(ctx context.Context, tenantID, resType, resID string) error {
	ctx, sp := t.startOp(ctx, "DeleteResource", tenantID, resType)
	err := t.inner.DeleteResource(ctx, tenantID, resType, resID)
	finishStoreSpan(sp, err)
	return err
}

func (t *tracingStore) DeleteResourceWithMeta(ctx context.Context, tenantID, resType, resID string) (*ResourceRecord, error) {
	ctx, sp := t.startOp(ctx, "DeleteResourceWithMeta", tenantID, resType)
	rec, err := t.inner.DeleteResourceWithMeta(ctx, tenantID, resType, resID)
	finishStoreSpan(sp, err)
	return rec, err
}

func (t *tracingStore) DeleteResourceWithExpectedVersion(ctx context.Context, tenantID, resType, resID string, expectedVersion int) (*ResourceRecord, error) {
	ctx, sp := t.startOp(ctx, "DeleteResourceWithExpectedVersion", tenantID, resType)
	rec, err := t.inner.DeleteResourceWithExpectedVersion(ctx, tenantID, resType, resID, expectedVersion)
	finishStoreSpan(sp, err)
	return rec, err
}

func (t *tracingStore) PurgePatientCompartment(ctx context.Context, tenantID, patientID string, hardExpunge bool) ([]ResourceRecord, error) {
	ctx, sp := t.startOp(ctx, "PurgePatientCompartment", tenantID, "Patient")
	recs, err := t.inner.PurgePatientCompartment(ctx, tenantID, patientID, hardExpunge)
	finishStoreSpan(sp, err)
	return recs, err
}

func (t *tracingStore) ExpungeResources(ctx context.Context, params ExpungeParams) (int, error) {
	ctx, sp := t.startOp(ctx, "ExpungeResources", params.TenantID, params.ResType)
	n, err := t.inner.ExpungeResources(ctx, params)
	finishStoreSpan(sp, err)
	return n, err
}

func (t *tracingStore) Reindex(ctx context.Context, params ReindexParams) (*ReindexResult, error) {
	ctx, sp := t.startOp(ctx, "Reindex", params.TenantID, params.ResType)
	res, err := t.inner.Reindex(ctx, params)
	finishStoreSpan(sp, err)
	return res, err
}

func (t *tracingStore) Search(ctx context.Context, params SearchParams) (*SearchResults, error) {
	ctx, sp := t.startOp(ctx, "Search", params.TenantID, params.ResType)
	res, err := t.inner.Search(ctx, params)
	if res != nil {
		sp.SetAttributes(telemetry.Int("fhir.search_total", res.Total))
	}
	finishStoreSpan(sp, err)
	return res, err
}

func (t *tracingStore) PatientEverything(ctx context.Context, params PatientEverythingParams) (*SearchResults, error) {
	ctx, sp := t.startOp(ctx, "PatientEverything", params.TenantID, "Patient")
	res, err := t.inner.PatientEverything(ctx, params)
	finishStoreSpan(sp, err)
	return res, err
}

func (t *tracingStore) EncounterEverything(ctx context.Context, params EncounterEverythingParams) (*SearchResults, error) {
	ctx, sp := t.startOp(ctx, "EncounterEverything", params.TenantID, "Encounter")
	res, err := t.inner.EncounterEverything(ctx, params)
	finishStoreSpan(sp, err)
	return res, err
}

func (t *tracingStore) GroupEverything(ctx context.Context, params GroupEverythingParams) (*SearchResults, error) {
	ctx, sp := t.startOp(ctx, "GroupEverything", params.TenantID, "Group")
	res, err := t.inner.GroupEverything(ctx, params)
	finishStoreSpan(sp, err)
	return res, err
}

func (t *tracingStore) BelongsToPatientCompartment(ctx context.Context, tenantID, resType, resID, patientID string) (bool, error) {
	ctx, sp := t.startOp(ctx, "BelongsToPatientCompartment", tenantID, resType)
	ok, err := t.inner.BelongsToPatientCompartment(ctx, tenantID, resType, resID, patientID)
	finishStoreSpan(sp, err)
	return ok, err
}

func (t *tracingStore) SearchPatientMatchCandidates(ctx context.Context, params PatientMatchCandidateParams) ([]SearchResult, error) {
	ctx, sp := t.startOp(ctx, "SearchPatientMatchCandidates", params.TenantID, "Patient")
	res, err := t.inner.SearchPatientMatchCandidates(ctx, params)
	finishStoreSpan(sp, err)
	return res, err
}

func (t *tracingStore) ResourceGraph(ctx context.Context, params ResourceGraphParams) (*SearchResults, error) {
	ctx, sp := t.startOp(ctx, "ResourceGraph", params.TenantID, params.ResourceType)
	res, err := t.inner.ResourceGraph(ctx, params)
	finishStoreSpan(sp, err)
	return res, err
}

func (t *tracingStore) AggregateMeta(ctx context.Context, tenantID, resType string) (*AggregatedMeta, error) {
	ctx, sp := t.startOp(ctx, "AggregateMeta", tenantID, resType)
	res, err := t.inner.AggregateMeta(ctx, tenantID, resType)
	finishStoreSpan(sp, err)
	return res, err
}

func (t *tracingStore) ExportResources(ctx context.Context, params ExportParams) ([]SearchResult, error) {
	ctx, sp := t.startOp(ctx, "ExportResources", params.TenantID, "")
	res, err := t.inner.ExportResources(ctx, params)
	finishStoreSpan(sp, err)
	return res, err
}

func (t *tracingStore) StreamExportResources(ctx context.Context, params ExportParams, emit func(SearchResult) error) error {
	ctx, sp := t.startOp(ctx, "StreamExportResources", params.TenantID, "")
	err := t.inner.StreamExportResources(ctx, params, emit)
	finishStoreSpan(sp, err)
	return err
}

func (t *tracingStore) SaveExportJob(ctx context.Context, job ExportJobRecord) error {
	ctx, sp := t.startOp(ctx, "SaveExportJob", job.TenantID, "")
	err := t.inner.SaveExportJob(ctx, job)
	finishStoreSpan(sp, err)
	return err
}

func (t *tracingStore) GetExportJob(ctx context.Context, tenantID, jobID string) (*ExportJobRecord, error) {
	ctx, sp := t.startOp(ctx, "GetExportJob", tenantID, "")
	job, err := t.inner.GetExportJob(ctx, tenantID, jobID)
	finishStoreSpan(sp, err)
	return job, err
}

func (t *tracingStore) DeleteExportJob(ctx context.Context, tenantID, jobID string) (bool, error) {
	ctx, sp := t.startOp(ctx, "DeleteExportJob", tenantID, "")
	ok, err := t.inner.DeleteExportJob(ctx, tenantID, jobID)
	finishStoreSpan(sp, err)
	return ok, err
}

func (t *tracingStore) WriteAuditEvent(ctx context.Context, rec AuditRecord) error {
	ctx, sp := t.startOp(ctx, "WriteAuditEvent", rec.TenantID, "AuditEvent")
	err := t.inner.WriteAuditEvent(ctx, rec)
	finishStoreSpan(sp, err)
	return err
}

func (t *tracingStore) ReadAuditEvent(ctx context.Context, tenantID, auditID string) (*AuditRecord, error) {
	ctx, sp := t.startOp(ctx, "ReadAuditEvent", tenantID, "AuditEvent")
	rec, err := t.inner.ReadAuditEvent(ctx, tenantID, auditID)
	finishStoreSpan(sp, err)
	return rec, err
}

func (t *tracingStore) QueryAuditEvents(ctx context.Context, params AuditQueryParams) ([]AuditRecord, int, error) {
	ctx, sp := t.startOp(ctx, "QueryAuditEvents", params.TenantID, "AuditEvent")
	recs, total, err := t.inner.QueryAuditEvents(ctx, params)
	finishStoreSpan(sp, err)
	return recs, total, err
}

func (t *tracingStore) SaveAuthCode(ctx context.Context, rawCode string, rec OAuthAuthCodeRecord) error {
	ctx, sp := t.startOp(ctx, "SaveAuthCode", rec.TenantID, "")
	err := t.inner.SaveAuthCode(ctx, rawCode, rec)
	finishStoreSpan(sp, err)
	return err
}

func (t *tracingStore) ConsumeAuthCode(ctx context.Context, rawCode string) (OAuthAuthCodeRecord, bool, error) {
	ctx, sp := t.startOp(ctx, "ConsumeAuthCode", "", "")
	rec, ok, err := t.inner.ConsumeAuthCode(ctx, rawCode)
	finishStoreSpan(sp, err)
	return rec, ok, err
}

func (t *tracingStore) SaveRefreshToken(ctx context.Context, rawToken string, rec OAuthRefreshTokenRecord) error {
	ctx, sp := t.startOp(ctx, "SaveRefreshToken", rec.TenantID, "")
	err := t.inner.SaveRefreshToken(ctx, rawToken, rec)
	finishStoreSpan(sp, err)
	return err
}

func (t *tracingStore) PeekRefreshToken(ctx context.Context, rawToken string) (OAuthRefreshTokenRecord, bool, error) {
	ctx, sp := t.startOp(ctx, "PeekRefreshToken", "", "")
	rec, ok, err := t.inner.PeekRefreshToken(ctx, rawToken)
	finishStoreSpan(sp, err)
	return rec, ok, err
}

func (t *tracingStore) ConsumeRefreshToken(ctx context.Context, rawToken string) (OAuthRefreshTokenRecord, bool, error) {
	ctx, sp := t.startOp(ctx, "ConsumeRefreshToken", "", "")
	rec, ok, err := t.inner.ConsumeRefreshToken(ctx, rawToken)
	finishStoreSpan(sp, err)
	return rec, ok, err
}

func (t *tracingStore) RevokeToken(ctx context.Context, rawToken string, ttl time.Duration) error {
	ctx, sp := t.startOp(ctx, "RevokeToken", "", "")
	err := t.inner.RevokeToken(ctx, rawToken, ttl)
	finishStoreSpan(sp, err)
	return err
}

func (t *tracingStore) IsTokenRevoked(ctx context.Context, rawToken string) (bool, error) {
	ctx, sp := t.startOp(ctx, "IsTokenRevoked", "", "")
	revoked, err := t.inner.IsTokenRevoked(ctx, rawToken)
	finishStoreSpan(sp, err)
	return revoked, err
}

func (t *tracingStore) ClaimNonce(ctx context.Context, nonceKey string, ttl time.Duration) (bool, error) {
	ctx, sp := t.startOp(ctx, "ClaimNonce", "", "")
	claimed, err := t.inner.ClaimNonce(ctx, nonceKey, ttl)
	finishStoreSpan(sp, err)
	return claimed, err
}

func (t *tracingStore) GetOrCreateClusterSecret(ctx context.Context, key string, generate func() ([]byte, error)) ([]byte, error) {
	ctx, sp := t.startOp(ctx, "GetOrCreateClusterSecret", "", "")
	b, err := t.inner.GetOrCreateClusterSecret(ctx, key, generate)
	finishStoreSpan(sp, err)
	return b, err
}

func (t *tracingStore) PutClusterState(ctx context.Context, rec ClusterStateRecord) (ClusterStateRecord, error) {
	ctx, sp := t.startOp(ctx, "PutClusterState", rec.TenantID, rec.Namespace)
	out, err := t.inner.PutClusterState(ctx, rec)
	finishStoreSpan(sp, err)
	return out, err
}

func (t *tracingStore) GetClusterState(ctx context.Context, namespace, key string) (*ClusterStateRecord, bool, error) {
	ctx, sp := t.startOp(ctx, "GetClusterState", "", namespace)
	rec, ok, err := t.inner.GetClusterState(ctx, namespace, key)
	finishStoreSpan(sp, err)
	return rec, ok, err
}

func (t *tracingStore) GetClusterStateBySecondary(ctx context.Context, namespace, secondaryKey string) (*ClusterStateRecord, bool, error) {
	ctx, sp := t.startOp(ctx, "GetClusterStateBySecondary", "", namespace)
	rec, ok, err := t.inner.GetClusterStateBySecondary(ctx, namespace, secondaryKey)
	finishStoreSpan(sp, err)
	return rec, ok, err
}

func (t *tracingStore) CompareAndSwapClusterState(ctx context.Context, expectedVersion int64, rec ClusterStateRecord) (ClusterStateRecord, bool, error) {
	ctx, sp := t.startOp(ctx, "CompareAndSwapClusterState", rec.TenantID, rec.Namespace)
	out, ok, err := t.inner.CompareAndSwapClusterState(ctx, expectedVersion, rec)
	finishStoreSpan(sp, err)
	return out, ok, err
}

func (t *tracingStore) ConsumeClusterState(ctx context.Context, namespace, key string) (*ClusterStateRecord, bool, error) {
	ctx, sp := t.startOp(ctx, "ConsumeClusterState", "", namespace)
	rec, ok, err := t.inner.ConsumeClusterState(ctx, namespace, key)
	finishStoreSpan(sp, err)
	return rec, ok, err
}

func (t *tracingStore) DeleteClusterState(ctx context.Context, namespace, key string) (bool, error) {
	ctx, sp := t.startOp(ctx, "DeleteClusterState", "", namespace)
	ok, err := t.inner.DeleteClusterState(ctx, namespace, key)
	finishStoreSpan(sp, err)
	return ok, err
}

func (t *tracingStore) ListClusterStates(ctx context.Context, namespace, tenantID string) ([]ClusterStateRecord, error) {
	ctx, sp := t.startOp(ctx, "ListClusterStates", tenantID, namespace)
	recs, err := t.inner.ListClusterStates(ctx, namespace, tenantID)
	finishStoreSpan(sp, err)
	return recs, err
}

func (a *tracingAnalyticalStore) BackendName() string {
	return a.analytical.BackendName()
}

func (a *tracingAnalyticalStore) EvaluateObservationStats(ctx context.Context, params ObservationStatsParams) (*ObservationStatsResult, error) {
	ctx, sp := a.startOp(ctx, "EvaluateObservationStats", params.TenantID, "Observation")
	res, err := a.analytical.EvaluateObservationStats(ctx, params)
	finishStoreSpan(sp, err)
	return res, err
}

func (a *tracingAnalyticalStore) StreamAnalyticalExport(ctx context.Context, params ExportParams, emit func(SearchResult) error) error {
	ctx, sp := a.startOp(ctx, "StreamAnalyticalExport", params.TenantID, "")
	err := a.analytical.StreamAnalyticalExport(ctx, params, emit)
	finishStoreSpan(sp, err)
	return err
}

func (a *tracingAnalyticalStore) EvaluateCohortCounts(ctx context.Context, tenantID string, resourceTypes []string) (map[string]int, error) {
	ctx, sp := a.startOp(ctx, "EvaluateCohortCounts", tenantID, "")
	res, err := a.analytical.EvaluateCohortCounts(ctx, tenantID, resourceTypes)
	finishStoreSpan(sp, err)
	return res, err
}
