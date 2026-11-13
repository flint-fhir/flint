package postgres

import "github.com/flint-fhir/flint/store"

// Compile-time assertion that *postgres.Store satisfies store.Store.
var _ store.Store = (*Store)(nil)

// Re-exported neutral domain constants from package store.
const (
	MaxSearchCount = store.MaxSearchCount
	BundleOpWrite  = store.BundleOpWrite
	BundleOpDelete = store.BundleOpDelete
)

// Re-exported storage error sentinels from package store.
var (
	ErrVersionConflict       = store.ErrVersionConflict
	ErrResourceDeleted       = store.ErrResourceDeleted
	ErrInvalidETag           = store.ErrInvalidETag
	ErrResourceAlreadyExists = store.ErrResourceAlreadyExists
	ErrPreconditionFailed    = store.ErrPreconditionFailed
	ErrNoTenant              = store.ErrNoTenant
	ErrInvalidTenantSlug     = store.ErrInvalidTenantSlug
	ErrCrossTenantBatch      = store.ErrCrossTenantBatch
)

// Re-exported neutral domain types from package store.
type (
	SearchIndexes               = store.SearchIndexes
	SpidxString                 = store.SpidxString
	SpidxToken                  = store.SpidxToken
	SpidxDate                   = store.SpidxDate
	SpidxReference              = store.SpidxReference
	SpidxQuantity               = store.SpidxQuantity
	SpidxURI                    = store.SpidxURI
	IndexExtractorFunc          = store.IndexExtractorFunc
	ResourceInput               = store.ResourceInput
	ResourceRecord              = store.ResourceRecord
	BundleOperationType         = store.BundleOperationType
	BundleOperation             = store.BundleOperation
	BundleOpResult              = store.BundleOpResult
	HistoryParams               = store.HistoryParams
	ReindexParams               = store.ReindexParams
	ReindexResult               = store.ReindexResult
	SearchResult                = store.SearchResult
	SearchResults               = store.SearchResults
	SearchParams                = store.SearchParams
	DateOp                      = store.DateOp
	QuantityOp                  = store.QuantityOp
	IncludeParam                = store.IncludeParam
	ChainedHop                  = store.ChainedHop
	ChainedParam                = store.ChainedParam
	HasParam                    = store.HasParam
	SortSpec                    = store.SortSpec
	CompositeTokenQuantityParam = store.CompositeTokenQuantityParam
	CompositeTokenTokenParam    = store.CompositeTokenTokenParam
	CompositeTokenDateParam     = store.CompositeTokenDateParam
	CompositeTokenStringParam   = store.CompositeTokenStringParam
	FilterExpr                  = store.FilterExpr
	CustomSearchParamDef        = store.CustomSearchParamDef
	PatientEverythingParams     = store.PatientEverythingParams
	EncounterEverythingParams   = store.EncounterEverythingParams
	GroupEverythingParams       = store.GroupEverythingParams
	PatientMatchCandidateParams = store.PatientMatchCandidateParams
	GraphLink                   = store.GraphLink
	GraphTarget                 = store.GraphTarget
	ResourceGraphParams         = store.ResourceGraphParams
	MetaCodingPair              = store.MetaCodingPair
	AggregatedMeta              = store.AggregatedMeta
	ExportParams                = store.ExportParams
	ExpungeParams               = store.ExpungeParams
	AuditRecord                 = store.AuditRecord
	AuditQueryParams            = store.AuditQueryParams
	OAuthAuthCodeRecord         = store.OAuthAuthCodeRecord
	OAuthRefreshTokenRecord     = store.OAuthRefreshTokenRecord
	ExportJobRecord             = store.ExportJobRecord
)

// Re-exported pure domain helpers, search parsers, and index extractors from package store.
var (
	FormatETag                        = store.FormatETag
	ParseETagVersion                  = store.ParseETagVersion
	CanonicalCompartmentType          = store.CanonicalCompartmentType
	IsCompartmentType                 = store.IsCompartmentType
	IsPatientCompartmentResourceType  = store.IsPatientCompartmentResourceType
	IndexesBelongToPatientCompartment = store.IndexesBelongToPatientCompartment
	DefaultGraphLinksForResource      = store.DefaultGraphLinksForResource
	ParseDateOp                       = store.ParseDateOp
	ParseQuantityOp                   = store.ParseQuantityOp
	ParseInclude                      = store.ParseInclude
	ParseChainedParam                 = store.ParseChainedParam
	ParseHasParam                     = store.ParseHasParam
	ParseSortSpecs                    = store.ParseSortSpecs
	ParseCompositeTokenQuantity       = store.ParseCompositeTokenQuantity
	ParseCompositeTokenToken          = store.ParseCompositeTokenToken
	ParseCompositeTokenDate           = store.ParseCompositeTokenDate
	ParseCompositeTokenString         = store.ParseCompositeTokenString
	RegisterCustomSearchParam         = store.RegisterCustomSearchParam
	UnregisterCustomSearchParam       = store.UnregisterCustomSearchParam
	CustomSearchParamsForType         = store.CustomSearchParamsForType
	ParseFilterExpr                   = store.ParseFilterExpr
	IsKnownSearchParam                = store.IsKnownSearchParam
	ValidateSearchQuery               = store.ValidateSearchQuery
	BuildSearchParamsFromQuery        = store.BuildSearchParamsFromQuery
	ParseIfNoneExist                  = store.ParseIfNoneExist
	HasSearchCriteria                 = store.HasSearchCriteria
	DefaultIndexExtractors            = store.DefaultIndexExtractors
	NewDynamicResourceExtractor       = store.NewDynamicResourceExtractor
	ExtractPatient                    = store.ExtractPatient
	ExtractCondition                  = store.ExtractCondition
	ExtractEncounter                  = store.ExtractEncounter
	ExtractObservation                = store.ExtractObservation
	ExtractPractitioner               = store.ExtractPractitioner
	ExtractMedicationRequest          = store.ExtractMedicationRequest
	ExtractAllergyIntolerance         = store.ExtractAllergyIntolerance
	ExtractProcedure                  = store.ExtractProcedure
	ExtractDiagnosticReport           = store.ExtractDiagnosticReport
	ExtractImmunization               = store.ExtractImmunization
	ExtractOrganization               = store.ExtractOrganization
	ExtractSpecimen                   = store.ExtractSpecimen
	ExtractLocation                   = store.ExtractLocation
)
