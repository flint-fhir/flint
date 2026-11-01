// Package store defines the pluggable persistence and analytical storage interfaces
// for the Flint FHIR R4 server.
//
// Flint supports two storage backends and a hybrid operational/analytical topology:
//   - "postgres" (store/postgres): Transactional OLTP store using proto blobs + HAPI-style spidx_* B-tree tables.
//   - "doris"    (store/doris):    Zero-JSON Apache Doris Unique Key Merge-on-Write (MoW) columnar + inverted-index store.
//   - Hybrid mode:                 PostgreSQL for sub-millisecond OLTP writes paired with Apache Doris as AnalyticalStore
//     for Bulk $export, Observation/$stats, and population cohort analytics.
package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/flint-fhir/flint/store/postgres"
)

// BackendType identifies a storage backend engine.
type BackendType string

const (
	// BackendPostgres is the default PostgreSQL operational store.
	BackendPostgres BackendType = "postgres"
	// BackendDoris is the Zero-JSON Apache Doris Unique Key Merge-on-Write (MoW) store.
	BackendDoris BackendType = "doris"
	// BackendSQLite is the embedded zero-dependency SQLite WAL operational & analytical store.
	BackendSQLite BackendType = "sqlite"
)

// ParseBackendType parses a configuration string (e.g. from --store or FLINT_STORE_BACKEND)
// into a BackendType. Empty string defaults to BackendPostgres.
func ParseBackendType(raw string) (BackendType, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "postgres", "postgresql", "pg":
		return BackendPostgres, nil
	case "doris", "apache-doris":
		return BackendDoris, nil
	case "sqlite", "sqlite3":
		return BackendSQLite, nil
	default:
		return "", fmt.Errorf("invalid store backend %q (valid backends: %q, %q, %q)", raw, BackendPostgres, BackendDoris, BackendSQLite)
	}
}

// ParseOptionalBackendType parses an optional analytics store backend string (e.g. from --analytics-store
// or FLINT_ANALYTICS_BACKEND). Returns ("", nil) when empty or "none".
func ParseOptionalBackendType(raw string) (BackendType, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "none", "off", "disabled", "false":
		return "", nil
	case "doris", "apache-doris":
		return BackendDoris, nil
	case "postgres", "postgresql", "pg":
		return BackendPostgres, nil
	case "sqlite", "sqlite3":
		return BackendSQLite, nil
	default:
		return "", fmt.Errorf("invalid analytics backend %q (valid backends: %q, %q, %q, or \"none\")", raw, BackendDoris, BackendPostgres, BackendSQLite)
	}
}

// MaxSearchCount is the maximum number of resources returned in a single search page.
const (
	MaxSearchCount = postgres.MaxSearchCount
	BundleOpWrite  = postgres.BundleOpWrite
	BundleOpDelete = postgres.BundleOpDelete
)

// Re-exported storage error sentinels.
var (
	ErrVersionConflict       = postgres.ErrVersionConflict
	ErrResourceDeleted       = postgres.ErrResourceDeleted
	ErrInvalidETag           = postgres.ErrInvalidETag
	ErrResourceAlreadyExists = postgres.ErrResourceAlreadyExists
	ErrPreconditionFailed    = postgres.ErrPreconditionFailed
	ErrCrossTenantBatch      = postgres.ErrCrossTenantBatch
	ErrNoTenant              = postgres.ErrNoTenant
	ParseDateOp              = postgres.ParseDateOp
	ParseQuantityOp          = postgres.ParseQuantityOp
	ParseChainedParam        = postgres.ParseChainedParam
)

// Re-exported domain types from store/postgres so callers and backend implementations
// share the exact same zero-copy structs without conversion overhead.
type (
	SearchIndexes               = postgres.SearchIndexes
	SpidxString                 = postgres.SpidxString
	SpidxToken                  = postgres.SpidxToken
	SpidxDate                   = postgres.SpidxDate
	SpidxReference              = postgres.SpidxReference
	SpidxQuantity               = postgres.SpidxQuantity
	SpidxURI                    = postgres.SpidxURI
	IndexExtractorFunc          = postgres.IndexExtractorFunc
	ResourceInput               = postgres.ResourceInput
	ResourceRecord              = postgres.ResourceRecord
	BundleOperationType         = postgres.BundleOperationType
	BundleOperation             = postgres.BundleOperation
	BundleOpResult              = postgres.BundleOpResult
	HistoryParams               = postgres.HistoryParams
	ReindexParams               = postgres.ReindexParams
	ReindexResult               = postgres.ReindexResult
	SearchParams                = postgres.SearchParams
	DateOp                      = postgres.DateOp
	QuantityOp                  = postgres.QuantityOp
	IncludeParam                = postgres.IncludeParam
	ChainedHop                  = postgres.ChainedHop
	ChainedParam                = postgres.ChainedParam
	HasParam                    = postgres.HasParam
	SortSpec                    = postgres.SortSpec
	CompositeTokenQuantityParam = postgres.CompositeTokenQuantityParam
	CompositeTokenTokenParam    = postgres.CompositeTokenTokenParam
	CompositeTokenDateParam     = postgres.CompositeTokenDateParam
	CompositeTokenStringParam   = postgres.CompositeTokenStringParam
	FilterExpr                  = postgres.FilterExpr
	SearchResult                = postgres.SearchResult
	SearchResults               = postgres.SearchResults
	PatientEverythingParams     = postgres.PatientEverythingParams
	EncounterEverythingParams   = postgres.EncounterEverythingParams
	GroupEverythingParams       = postgres.GroupEverythingParams
	PatientMatchCandidateParams = postgres.PatientMatchCandidateParams
	GraphLink                   = postgres.GraphLink
	GraphTarget                 = postgres.GraphTarget
	ResourceGraphParams         = postgres.ResourceGraphParams
	MetaCodingPair              = postgres.MetaCodingPair
	AggregatedMeta              = postgres.AggregatedMeta
	ExportParams                = postgres.ExportParams
	ExpungeParams               = postgres.ExpungeParams
	AuditRecord                 = postgres.AuditRecord
	AuditQueryParams            = postgres.AuditQueryParams
	OAuthAuthCodeRecord         = postgres.OAuthAuthCodeRecord
	OAuthRefreshTokenRecord     = postgres.OAuthRefreshTokenRecord
	ExportJobRecord             = postgres.ExportJobRecord
)

// Store defines the operational persistence contract for the Flint FHIR R4 server.
// Both *postgres.Store and *doris.Store implement this interface.
type Store interface {
	Ping(ctx context.Context) error

	// CRUD & Batch / Transaction Bundles
	WriteResource(ctx context.Context, input ResourceInput) error
	WriteResourceWithMeta(ctx context.Context, input ResourceInput) (*ResourceRecord, error)
	WriteBatch(ctx context.Context, inputs []ResourceInput) error
	ConditionalCreateResource(ctx context.Context, input ResourceInput, condParams SearchParams) (*ResourceRecord, bool, error)
	ConditionalUpdateResource(
		ctx context.Context,
		tenantID, resType string,
		condParams SearchParams,
		buildInput func(matchedID string, isExisting bool) (ResourceInput, error),
	) (*ResourceRecord, error)
	ConditionalDeleteResource(
		ctx context.Context,
		tenantID, resType string,
		condParams SearchParams,
		verifyMatch func(matchedID string) error,
	) (*ResourceRecord, error)
	ConditionalDeleteResources(
		ctx context.Context,
		tenantID, resType string,
		condParams SearchParams,
		maxCount int,
		verifyMatch func(matchedID string) error,
	) ([]ResourceRecord, error)
	ExecuteTransactionBundle(ctx context.Context, ops []BundleOperation) ([]BundleOpResult, error)

	// Read & History
	ReadResource(ctx context.Context, tenantID, resType, resID string) ([]byte, error)
	ReadResourceWithMeta(ctx context.Context, tenantID, resType, resID string) (*ResourceRecord, error)
	ReadResourceVersion(ctx context.Context, tenantID, resType, resID string, version int) (*ResourceRecord, error)
	ListResourceHistory(ctx context.Context, tenantID, resType, resID string, limit, offset int) ([]ResourceRecord, int, error)
	ListHistory(ctx context.Context, tenantID, resType, resID string, since *time.Time, limit, offset int) ([]ResourceRecord, int, error)
	ListHistoryWithParams(ctx context.Context, params HistoryParams) ([]ResourceRecord, int, error)

	// Delete, Purge & Expunge
	DeleteResource(ctx context.Context, tenantID, resType, resID string) error
	DeleteResourceWithMeta(ctx context.Context, tenantID, resType, resID string) (*ResourceRecord, error)
	DeleteResourceWithExpectedVersion(ctx context.Context, tenantID, resType, resID string, expectedVersion int) (*ResourceRecord, error)
	PurgePatientCompartment(ctx context.Context, tenantID, patientID string, hardExpunge bool) ([]ResourceRecord, error)
	ExpungeResources(ctx context.Context, params ExpungeParams) (int, error)
	Reindex(ctx context.Context, params ReindexParams) (*ReindexResult, error)

	// Search, Everything, Compartments, MPI, Graph & Meta
	Search(ctx context.Context, params SearchParams) (*SearchResults, error)
	PatientEverything(ctx context.Context, params PatientEverythingParams) (*SearchResults, error)
	EncounterEverything(ctx context.Context, params EncounterEverythingParams) (*SearchResults, error)
	GroupEverything(ctx context.Context, params GroupEverythingParams) (*SearchResults, error)
	BelongsToPatientCompartment(ctx context.Context, tenantID, resType, resID, patientID string) (bool, error)
	SearchPatientMatchCandidates(ctx context.Context, params PatientMatchCandidateParams) ([]SearchResult, error)
	ResourceGraph(ctx context.Context, params ResourceGraphParams) (*SearchResults, error)
	AggregateMeta(ctx context.Context, tenantID, resType string) (*AggregatedMeta, error)

	// Bulk Data ($export)
	ExportResources(ctx context.Context, params ExportParams) ([]SearchResult, error)
	StreamExportResources(ctx context.Context, params ExportParams, emit func(SearchResult) error) error
	SaveExportJob(ctx context.Context, job ExportJobRecord) error
	GetExportJob(ctx context.Context, tenantID, jobID string) (*ExportJobRecord, error)
	DeleteExportJob(ctx context.Context, tenantID, jobID string) (bool, error)

	// HIPAA / ONC AuditEvent Persistence
	WriteAuditEvent(ctx context.Context, rec AuditRecord) error
	ReadAuditEvent(ctx context.Context, tenantID, auditID string) (*AuditRecord, error)
	QueryAuditEvents(ctx context.Context, params AuditQueryParams) ([]AuditRecord, int, error)

	// SMART on FHIR OAuth2 Grant & Revocation Persistence
	SaveAuthCode(ctx context.Context, rawCode string, rec OAuthAuthCodeRecord) error
	ConsumeAuthCode(ctx context.Context, rawCode string) (OAuthAuthCodeRecord, bool, error)
	SaveRefreshToken(ctx context.Context, rawToken string, rec OAuthRefreshTokenRecord) error
	ConsumeRefreshToken(ctx context.Context, rawToken string) (OAuthRefreshTokenRecord, bool, error)
	RevokeToken(ctx context.Context, rawToken string, ttl time.Duration) error
	IsTokenRevoked(ctx context.Context, rawToken string) (bool, error)
}

// ObservationStatsParams specifies query criteria for columnar-accelerated Observation/$stats.
type ObservationStatsParams struct {
	TenantID  string
	PatientID string
	Code      string
	System    string
	Start     *time.Time
	End       *time.Time
}

// ObservationStatsMetrics holds aggregated statistical metrics for a single Observation code/unit.
type ObservationStatsMetrics struct {
	Code       string  `json:"code"`
	System     string  `json:"system,omitempty"`
	Unit       string  `json:"unit,omitempty"`
	Count      int     `json:"count"`
	TotalCount int     `json:"totalCount"`
	Average    float64 `json:"average"`
	Min        float64 `json:"min"`
	Max        float64 `json:"max"`
	Sum        float64 `json:"sum"`
	Median     float64 `json:"median"`
	StdDev     float64 `json:"stdDev"`
	Variance   float64 `json:"variance"`
}

// ObservationStatsResult holds the columnar-computed results of Observation/$stats.
type ObservationStatsResult struct {
	Series []ObservationStatsMetrics `json:"series"`
}

// AnalyticalStore is an optional capability interface for columnar/OLAP storage backends
// (such as Apache Doris). When a Store implements AnalyticalStore — or when a separate
// AnalyticalStore is wired into the Server alongside Postgres — Flint routes bulk export,
// cohort counts, and statistical aggregations to the analytical engine.
type AnalyticalStore interface {
	BackendName() string
	EvaluateObservationStats(ctx context.Context, params ObservationStatsParams) (*ObservationStatsResult, error)
	StreamAnalyticalExport(ctx context.Context, params ExportParams, emit func(SearchResult) error) error
	EvaluateCohortCounts(ctx context.Context, tenantID string, resourceTypes []string) (map[string]int, error)
}

// IsNil safely reports whether a Store or AnalyticalStore interface is nil or wraps a typed-nil pointer.
func IsNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

var _ Store = (*postgres.Store)(nil)
