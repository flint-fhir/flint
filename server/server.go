// Package server implements the Flint FHIR R4 REST API.
//
// This serves FHIR resources from the Postgres operational store.
// Resources are stored as proto blobs and converted to JSON on read.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	allpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/allergy_intolerance_go_proto"
	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	diagpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/diagnostic_report_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	immpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/immunization_go_proto"
	locpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/location_go_proto"
	medreqpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/medication_request_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	orgpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/organization_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"
	procpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/procedure_go_proto"
	specpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/specimen_go_proto"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
	automq "github.com/flint-fhir/flint/pkg/cdc"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/postgres"
)

var idSeq atomic.Uint64

// Server is the Flint FHIR REST API server.
type Server struct {
	primaryStore    store.Store
	store           store.Store
	analyticalStore store.AnalyticalStore
	logger          *slog.Logger
	// protoRegistry maps FHIR resource type names to proto message descriptors.
	// Used to unmarshal proto bytes back to proto messages for JSON conversion.
	protoRegistry              map[string]protoreflect.MessageDescriptor
	extractors                 map[string]postgres.IndexExtractorFunc
	smartConfig                *SMARTConfig
	tokenValidator             auth.TokenValidator
	devAuth                    *auth.MockTokenValidator
	oauthMu                    sync.Mutex
	oauthCodes                 map[string]authCodeEntry
	refreshTokens              map[string]refreshTokenEntry
	revokedTokens              map[string]time.Time
	validator                  validation.Validator
	auditRecorder              audit.Recorder
	cdcPublisher               automq.Publisher
	exportMu                   sync.RWMutex
	exportJobs                 map[string]*exportJob
	importMu                   sync.RWMutex
	importJobs                 map[string]*importJob
	viewDefsMu                 sync.RWMutex
	viewDefs                   map[string]map[string]sqlOnFHIRViewDefinition
	subSem                     chan struct{}
	subStatsMu                 sync.RWMutex
	subStats                   map[string]*subscriptionDeliveryStats
	disallowPrivateWebhooks    bool
	clientJWKs                 map[string]auth.JWK
	requireRegisteredClientJWK bool
	routerOnce                 sync.Once
	internalRouter             http.Handler
	metrics                    *serverMetrics
}

// New creates a new FHIR server backed by any pluggable store.Store implementation
// (such as *postgres.Store, *doris.Store, or *sqlite.Store). If st also implements
// store.AnalyticalStore, it is automatically registered as the server's analytical engine.
func New(st store.Store, logger *slog.Logger) *Server {
	if store.IsNil(st) {
		st = nil
	}
	var auditRec audit.Recorder
	var analytical store.AnalyticalStore
	if st != nil {
		auditRec = audit.NewStoreRecorder(st)
		if as, ok := st.(store.AnalyticalStore); ok && !store.IsNil(as) {
			analytical = as
		}
	} else {
		auditRec = audit.NewMemoryRecorder()
	}
	devAuth, _ := auth.NewMockTokenValidator("", "")
	valEngine := validation.NewEngine(validation.DefaultOptions())
	valEngine.SetFHIRPathEvaluator(evalFHIRPath)
	return &Server{
		primaryStore:    st,
		store:           st,
		analyticalStore: analytical,
		logger:          logger,
		protoRegistry:   make(map[string]protoreflect.MessageDescriptor),
		extractors:      postgres.DefaultIndexExtractors(),
		devAuth:         devAuth,
		oauthCodes:      make(map[string]authCodeEntry),
		refreshTokens:   make(map[string]refreshTokenEntry),
		revokedTokens:   make(map[string]time.Time),
		clientJWKs:      make(map[string]auth.JWK),
		validator:       valEngine,
		auditRecorder:   auditRec,
		exportJobs:      make(map[string]*exportJob),
		subSem:          make(chan struct{}, 32),
		metrics:         newServerMetrics(),
	}
}

// Store returns the configured operational store.Store.
func (s *Server) Store() store.Store {
	return s.store
}

// SetAnalyticalStore configures an optional columnar AnalyticalStore (such as *doris.Store
// or *sqlite.Store) for hybrid OLTP + OLAP deployments (--store=postgres --analytics-store=doris).
// When as also implements store.Store and is distinct from the primary operational store,
// s.store is automatically wrapped in a *store.HybridStore so all committed mutations
// replicate to the analytical store in real time.
func (s *Server) SetAnalyticalStore(as store.AnalyticalStore) {
	if store.IsNil(as) {
		s.analyticalStore = nil
		s.store = s.primaryStore
		if s.primaryStore != nil {
			s.auditRecorder = audit.NewStoreRecorder(s.primaryStore)
		}
		return
	}
	if asStore, ok := as.(store.Store); ok && !store.IsNil(asStore) && s.primaryStore != nil && any(asStore) != any(s.primaryStore) {
		hyb := store.NewHybridStore(s.primaryStore, asStore)
		s.store = hyb
		s.analyticalStore = hyb
		s.auditRecorder = audit.NewStoreRecorder(hyb)
		return
	}
	s.store = s.primaryStore
	s.analyticalStore = as
	if s.primaryStore != nil {
		s.auditRecorder = audit.NewStoreRecorder(s.primaryStore)
	}
}

// AnalyticalStore returns the configured AnalyticalStore, or nil if none is active.
func (s *Server) AnalyticalStore() store.AnalyticalStore {
	return s.analyticalStore
}

// RegisterDefaultResourceTypes registers all ~146 official HL7 FHIR R4 resource types
// compiled in google/fhir R4 ContainedResource (including all 26 US Core v6.1.0 & v7.0.0 types).
func RegisterDefaultResourceTypes(srv *Server) {
	for resType, md := range fhirutil.AllR4ResourceDescriptors() {
		srv.protoRegistry[resType] = md
	}
	srv.RegisterResourceType("Patient", &patpb.Patient{})
	srv.RegisterResourceType("Condition", &condpb.Condition{})
	srv.RegisterResourceType("Encounter", &encpb.Encounter{})
	srv.RegisterResourceType("Observation", &obspb.Observation{})
	srv.RegisterResourceType("Practitioner", &pracpb.Practitioner{})
	srv.RegisterResourceType("MedicationRequest", &medreqpb.MedicationRequest{})
	srv.RegisterResourceType("AllergyIntolerance", &allpb.AllergyIntolerance{})
	srv.RegisterResourceType("Procedure", &procpb.Procedure{})
	srv.RegisterResourceType("DiagnosticReport", &diagpb.DiagnosticReport{})
	srv.RegisterResourceType("Immunization", &immpb.Immunization{})
	srv.RegisterResourceType("Organization", &orgpb.Organization{})
	srv.RegisterResourceType("Specimen", &specpb.Specimen{})
	srv.RegisterResourceType("Location", &locpb.Location{})
}

// SetCDCPublisher configures the optional AutoMQ / Kafka CDC publisher for lakehouse streaming.
func (s *Server) SetCDCPublisher(p automq.Publisher) {
	s.cdcPublisher = p
}

// CDCPublisher returns the configured AutoMQ / Kafka CDC publisher, or nil if disabled.
func (s *Server) CDCPublisher() automq.Publisher {
	return s.cdcPublisher
}

func (s *Server) publishCDC(ctx context.Context, msgs ...automq.Message) {
	if s.cdcPublisher == nil || len(msgs) == 0 {
		return
	}
	if err := s.cdcPublisher.Publish(ctx, msgs); err != nil {
		if s.metrics != nil {
			s.metrics.cdcPublishErrorsTotal.Add(1)
		}
		if s.logger != nil {
			s.logger.Warn("failed to publish CDC event to AutoMQ", "error", err, "count", len(msgs))
		}
		return
	}
	if s.metrics != nil {
		s.metrics.cdcPublishedTotal.Add(uint64(len(msgs)))
	}
}

// SetAuditRecorder configures the HIPAA / ONC security audit event recorder.
func (s *Server) SetAuditRecorder(r audit.Recorder) {
	s.auditRecorder = r
}

// AuditRecorder returns the active security audit event recorder.
func (s *Server) AuditRecorder() audit.Recorder {
	return s.auditRecorder
}

// SetValidator configures the FHIR StructureDefinition and ValueSet validator.
// Passing nil disables validation.
func (s *Server) SetValidator(v validation.Validator) {
	if eng, ok := v.(*validation.Engine); ok && eng != nil {
		eng.SetFHIRPathEvaluator(evalFHIRPath)
	}
	s.validator = v
}

// SetTokenValidator configures the SMART on FHIR bearer token validator.
// When set, all non-public FHIR endpoints require valid tokens and matching scopes.
func (s *Server) SetTokenValidator(v auth.TokenValidator) {
	s.tokenValidator = v
}

// RegisterResourceType registers a proto message type for a FHIR resource type.
// This allows the server to unmarshal proto bytes and convert to JSON.
func (s *Server) RegisterResourceType(resType string, msg proto.Message) {
	s.protoRegistry[resType] = msg.ProtoReflect().Descriptor()
}

// RegisterIndexExtractor registers a search index extractor function for a resource type.
func (s *Server) RegisterIndexExtractor(resType string, fn postgres.IndexExtractorFunc) {
	s.extractors[resType] = fn
}

// Handler returns the HTTP handler for the FHIR API.
func (s *Server) Handler() http.Handler {
	return s.metricsMiddleware(s.auditMiddleware(s.authMiddleware(s.fhirRouter())))
}

func (s *Server) fhirRouter() http.Handler {
	s.routerOnce.Do(func() {
		s.internalRouter = s.buildFHIRRouter()
	})
	return s.internalRouter
}

func (s *Server) buildFHIRRouter() http.Handler {
	mux := http.NewServeMux()

	// Operational Liveness, Readiness & Prometheus Metrics endpoints
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /fhir/r4/healthz", s.handleHealthz)
	mux.HandleFunc("GET /fhir/r4/readyz", s.handleReadyz)
	mux.HandleFunc("GET /fhir/r4/metrics", s.handleMetrics)
	mux.HandleFunc("GET /fhir/r4/{tenant}/healthz", s.handleHealthz)
	mux.HandleFunc("GET /fhir/r4/{tenant}/readyz", s.handleReadyz)
	mux.HandleFunc("GET /fhir/r4/{tenant}/metrics", s.handleMetrics)

	// Built-in SMART on FHIR v2.0.0 OAuth2 & OIDC Discovery endpoints
	mux.HandleFunc("GET /oauth2/authorize", s.handleOAuthAuthorize)
	mux.HandleFunc("POST /oauth2/authorize", s.handleOAuthAuthorize)
	mux.HandleFunc("POST /oauth2/token", s.handleOAuthToken)
	mux.HandleFunc("POST /oauth2/introspect", s.handleOAuthIntrospect)
	mux.HandleFunc("POST /oauth2/revoke", s.handleOAuthRevoke)
	mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleOIDCConfig)
	mux.HandleFunc("GET /.well-known/udap", s.handleUDAPDiscoveryWave54)

	// Discovery & OpenAPI 3.1 / Interactive Explorer / CDS Hooks v2.0 endpoints
	mux.HandleFunc("GET /fhir/r4/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /fhir/r4/$docs", s.handleDocsUI)
	mux.HandleFunc("GET /fhir/r4/{tenant}/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$docs", s.handleDocsUI)
	mux.HandleFunc("GET /cds-services", s.handleCDSServicesDiscovery)
	mux.HandleFunc("POST /cds-services/{serviceId}", s.handleCDSServiceInvoke)
	mux.HandleFunc("GET /cds-services/{serviceId}/$dsi-attributes", s.handleCDSServiceDSIAttributes)
	mux.HandleFunc("POST /cds-services/{serviceId}/$dsi-attributes", s.handleCDSServiceDSIAttributes)
	mux.HandleFunc("POST /cds-services/{serviceId}/feedback", s.handleCDSServiceFeedback)
	mux.HandleFunc("GET /fhir/r4/cds-services", s.handleCDSServicesDiscovery)
	mux.HandleFunc("GET /fhir/r4/{tenant}/cds-services", s.handleCDSServicesDiscovery)

	// FHIR Bulk Data Access ($export — ONC (g)(10)) & Automated Conformance Certification ($certify)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$export", s.handleSystemExport)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$export", s.handleSystemExport)
	mux.HandleFunc("GET /fhir/r4/{tenant}/Patient/$export", s.handlePatientGroupExport)
	mux.HandleFunc("POST /fhir/r4/{tenant}/Patient/$export", s.handlePatientGroupExport)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}/$export", s.handleSinglePatientExport)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/{id}/$export", s.handleSinglePatientExport)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$export-poll/{jobId}", s.handleExportPoll)
	mux.HandleFunc("DELETE /fhir/r4/{tenant}/$export-poll/{jobId}", s.handleExportCancel)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$import", s.handleBulkImport)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$import-poll/{jobId}", s.handleImportPoll)
	mux.HandleFunc("DELETE /fhir/r4/{tenant}/$import-poll/{jobId}", s.handleImportCancel)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$expunge", s.handleExpunge)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/$expunge", s.handleExpunge)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$reindex", s.handleReindex)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/$reindex", s.handleReindex)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$diff", s.handleDiff)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/$diff", s.handleDiff)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$closure", s.handleClosure)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$data-requirements", s.handleDataRequirements)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$data-requirements", s.handleDataRequirements)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$fhirpath", s.handleFHIRPath)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$fhirpath", s.handleFHIRPath)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$process-message", s.handleProcessMessage)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$process-notification", s.handleProcessNotification)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$davinci-data-export", s.handleDaVinciDataExport)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$davinci-data-export", s.handleDaVinciDataExport)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$submit-attachment", s.handleCDexSubmitAttachment)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$convert", s.handleResourceConvert)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/$convert", s.handleResourceConvert)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$certify", s.handleCertify)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$certify", s.handleCertify)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$sync-analytics", s.handleSyncAnalytics)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$sync-analytics", s.handleSyncAnalytics)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$run", s.handleViewDefinitionRun)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$run", s.handleViewDefinitionRun)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$sql", s.handleViewDefinitionSQL)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$sql", s.handleViewDefinitionSQL)

	// HL7 SQL-on-FHIR v2 ViewDefinition CRUD, $run, $export, and Zero-JSON $sql endpoints
	mux.HandleFunc("GET /fhir/r4/{tenant}/ViewDefinition/$run", s.handleViewDefinitionRun)
	mux.HandleFunc("POST /fhir/r4/{tenant}/ViewDefinition/$run", s.handleViewDefinitionRun)
	mux.HandleFunc("GET /fhir/r4/{tenant}/ViewDefinition/{id}/$run", s.handleViewDefinitionRun)
	mux.HandleFunc("POST /fhir/r4/{tenant}/ViewDefinition/{id}/$run", s.handleViewDefinitionRun)
	mux.HandleFunc("GET /fhir/r4/{tenant}/ViewDefinition/$export", s.handleViewDefinitionExport)
	mux.HandleFunc("POST /fhir/r4/{tenant}/ViewDefinition/$export", s.handleViewDefinitionExport)
	mux.HandleFunc("GET /fhir/r4/{tenant}/ViewDefinition/{id}/$export", s.handleViewDefinitionExport)
	mux.HandleFunc("POST /fhir/r4/{tenant}/ViewDefinition/{id}/$export", s.handleViewDefinitionExport)
	mux.HandleFunc("GET /fhir/r4/{tenant}/ViewDefinition/$sql", s.handleViewDefinitionSQL)
	mux.HandleFunc("POST /fhir/r4/{tenant}/ViewDefinition/$sql", s.handleViewDefinitionSQL)
	mux.HandleFunc("GET /fhir/r4/{tenant}/ViewDefinition/{id}/$sql", s.handleViewDefinitionSQL)
	mux.HandleFunc("POST /fhir/r4/{tenant}/ViewDefinition/{id}/$sql", s.handleViewDefinitionSQL)
	mux.HandleFunc("GET /fhir/r4/{tenant}/ViewDefinition/{id}", s.handleViewDefinitionCRUD)
	mux.HandleFunc("PUT /fhir/r4/{tenant}/ViewDefinition/{id}", s.handleViewDefinitionCRUD)
	mux.HandleFunc("DELETE /fhir/r4/{tenant}/ViewDefinition/{id}", s.handleViewDefinitionCRUD)
	mux.HandleFunc("GET /fhir/r4/{tenant}/ViewDefinition", s.handleViewDefinitionCRUD)
	mux.HandleFunc("POST /fhir/r4/{tenant}/ViewDefinition", s.handleViewDefinitionCRUD)

	// FHIR REST endpoints
	mux.HandleFunc("GET /fhir/r4/{tenant}/AuditEvent/{id}", s.handleReadAuditEvent)
	mux.HandleFunc("GET /fhir/r4/{tenant}/AuditEvent", s.handleSearchAuditEvents)
	mux.HandleFunc("POST /fhir/r4/{tenant}/AuditEvent/_search", s.handleSearchAuditEvents)
	mux.HandleFunc("POST /fhir/r4/{tenant}/Patient/$match", s.handlePatientMatch)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$meta", s.handleMeta)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}/$meta", s.handleMeta)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/{id}/$meta-add", s.handleMetaAdd)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/{id}/$meta-delete", s.handleMetaDelete)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}/$everything", s.handlePatientEverything)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}/_history/{vid}", s.handleVRead)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}/_history", s.handleInstanceHistory)
	mux.HandleFunc("GET /fhir/r4/{tenant}/_history", s.handleInstanceHistory)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/_search", s.handleSearch)
	mux.HandleFunc("POST /fhir/r4/{tenant}/_search", s.handleSystemSearch)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}/{id}", s.handleRead)
	mux.HandleFunc("PUT /fhir/r4/{tenant}/{resourceType}/{id}", s.handleUpdate)
	mux.HandleFunc("PATCH /fhir/r4/{tenant}/{resourceType}/{id}", s.handlePatch)
	mux.HandleFunc("DELETE /fhir/r4/{tenant}/{resourceType}/{id}", s.handleDelete)
	mux.HandleFunc("GET /fhir/r4/{tenant}/{resourceType}", s.handleSearch)
	mux.HandleFunc("GET /fhir/r4/{tenant}", s.handleSystemSearch)
	mux.HandleFunc("PUT /fhir/r4/{tenant}/{resourceType}", s.handleConditionalUpdate)
	mux.HandleFunc("PATCH /fhir/r4/{tenant}/{resourceType}", s.handleConditionalPatch)
	mux.HandleFunc("DELETE /fhir/r4/{tenant}/{resourceType}", s.handleConditionalDelete)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/$validate", s.handleValidate)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$validate", s.handleValidate)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}", s.handleCreate)
	mux.HandleFunc("POST /fhir/r4/{tenant}", s.handleBundle)
	mux.HandleFunc("GET /fhir/r4/{tenant}/metadata", s.handleMetadata)
	mux.HandleFunc("GET /fhir/r4/{tenant}/.well-known/smart-configuration", s.handleSMARTConfig)
	mux.HandleFunc("GET /fhir/r4/{tenant}/.well-known/openid-configuration", s.handleOIDCConfig)
	mux.HandleFunc("GET /fhir/r4/{tenant}/.well-known/jwks.json", s.handleJWKS)
	mux.HandleFunc("GET /fhir/r4/{tenant}/.well-known/udap", s.handleUDAPDiscoveryWave54)

	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isGetOrHead := r.Method == http.MethodGet || r.Method == http.MethodHead
		if isGetOrHead || r.Method == http.MethodPost {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 4 && parts[0] == "fhir" && parts[1] == "r4" && parts[2] == "cds-services" && r.Method == http.MethodPost {
				r.SetPathValue("serviceId", parts[3])
				s.handleCDSServiceInvoke(w, r)
				return
			}
			if len(parts) == 4 && parts[0] == "fhir" && parts[1] == "r4" && parts[3] == "$iti-65" && r.Method == http.MethodPost {
				r.SetPathValue("tenant", parts[2])
				r.SetPathValue("op", "$iti-65")
				s.handleIHEMHDExchange(w, r)
				return
			}
			if len(parts) == 5 && parts[0] == "fhir" && parts[1] == "r4" {
				if isGetOrHead && parts[4] == "_history" && !strings.HasPrefix(parts[3], "$") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					s.handleInstanceHistory(w, r)
					return
				}
				if isGetOrHead && parts[4] == "$meta" && !strings.HasPrefix(parts[3], "$") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					s.handleMeta(w, r)
					return
				}
				if r.Method == http.MethodPost && parts[3] == "cds-services" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("serviceId", parts[4])
					s.handleCDSServiceInvoke(w, r)
					return
				}
				if strings.EqualFold(parts[3], "AuditEvent") && parts[4] == "$verify-integrity" {
					r.SetPathValue("tenant", parts[2])
					s.handleAuditVerifyIntegrity(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$member-match" {
					r.SetPathValue("tenant", parts[2])
					s.handlePatientMemberMatch(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$merge" {
					r.SetPathValue("tenant", parts[2])
					s.handlePatientMerge(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Observation") && parts[4] == "$lastn" {
					r.SetPathValue("tenant", parts[2])
					s.handleObservationLastN(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Observation") && parts[4] == "$stats" {
					r.SetPathValue("tenant", parts[2])
					s.handleObservationStats(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ViewDefinition") && parts[4] == "$run" {
					r.SetPathValue("tenant", parts[2])
					s.handleViewDefinitionRun(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ViewDefinition") && parts[4] == "$export" {
					r.SetPathValue("tenant", parts[2])
					s.handleViewDefinitionExport(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ViewDefinition") && parts[4] == "$sql" {
					r.SetPathValue("tenant", parts[2])
					s.handleViewDefinitionSQL(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Claim") && parts[4] == "$submit" {
					r.SetPathValue("tenant", parts[2])
					s.handleClaimSubmit(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Claim") && parts[4] == "$inquire" {
					r.SetPathValue("tenant", parts[2])
					s.handleClaimInquire(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[4] == "$evaluate-measure" {
					r.SetPathValue("tenant", parts[2])
					s.handleEvaluateMeasure(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[4] == "$care-gaps" {
					r.SetPathValue("tenant", parts[2])
					s.handleCareGaps(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[4] == "$collect-data" {
					r.SetPathValue("tenant", parts[2])
					s.handleMeasureCollectData(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Measure") && parts[4] == "$submit-data" {
					r.SetPathValue("tenant", parts[2])
					s.handleMeasureSubmitData(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ValueSet") && parts[4] == "$expand" {
					r.SetPathValue("tenant", parts[2])
					s.handleValueSetExpand(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ValueSet") && parts[4] == "$validate-code" {
					r.SetPathValue("tenant", parts[2])
					s.handleValueSetValidateCode(w, r)
					return
				}
				if strings.EqualFold(parts[3], "CodeSystem") && parts[4] == "$lookup" {
					r.SetPathValue("tenant", parts[2])
					s.handleCodeSystemLookup(w, r)
					return
				}
				if strings.EqualFold(parts[3], "CodeSystem") && parts[4] == "$validate-code" {
					r.SetPathValue("tenant", parts[2])
					s.handleCodeSystemValidateCode(w, r)
					return
				}
				if strings.EqualFold(parts[3], "CodeSystem") && parts[4] == "$subsumes" {
					r.SetPathValue("tenant", parts[2])
					s.handleCodeSystemSubsumes(w, r)
					return
				}
				if strings.EqualFold(parts[3], "CodeSystem") && parts[4] == "$find-matches" {
					r.SetPathValue("tenant", parts[2])
					s.handleCodeSystemFindMatches(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ConceptMap") && parts[4] == "$translate" {
					r.SetPathValue("tenant", parts[2])
					s.handleConceptMapTranslate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "DocumentReference") && parts[4] == "$docref" {
					r.SetPathValue("tenant", parts[2])
					s.handleDocumentReferenceDocref(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Questionnaire") && parts[4] == "$populate" {
					r.SetPathValue("tenant", parts[2])
					s.handleQuestionnairePopulate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Questionnaire") && parts[4] == "$package" {
					r.SetPathValue("tenant", parts[2])
					s.handleQuestionnairePackage(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "QuestionnaireResponse") && parts[4] == "$extract" {
					r.SetPathValue("tenant", parts[2])
					s.handleQuestionnaireResponseExtract(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Subscription") && parts[4] == "$status" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", "Subscription")
					s.handleSubscriptionStatus(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Subscription") && parts[4] == "$events" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", "Subscription")
					s.handleSubscriptionEvents(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "ConceptMap") && parts[4] == "$closure" {
					r.SetPathValue("tenant", parts[2])
					s.handleClosure(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "Library") || strings.EqualFold(parts[3], "Measure") || strings.EqualFold(parts[3], "PlanDefinition") || strings.EqualFold(parts[3], "ActivityDefinition") || strings.EqualFold(parts[3], "Questionnaire")) && parts[4] == "$data-requirements" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					s.handleDataRequirements(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "CoverageEligibilityRequest") && parts[4] == "$submit" {
					r.SetPathValue("tenant", parts[2])
					s.handleCoverageEligibilitySubmit(w, r)
					return
				}
				if strings.EqualFold(parts[3], "GraphDefinition") && parts[4] == "$graph" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", "GraphDefinition")
					s.handleResourceGraph(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Task") && parts[4] == "$submit-attachment" {
					r.SetPathValue("tenant", parts[2])
					s.handleCDexSubmitAttachment(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Library") && parts[4] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					s.handleLibraryEvaluate(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$health-cards-issue" {
					r.SetPathValue("tenant", parts[2])
					s.handlePatientHealthCardsIssue(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$pcde" {
					r.SetPathValue("tenant", parts[2])
					s.handlePatientPCDE(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ResearchStudy") && parts[4] == "$evaluate-eligibility" {
					r.SetPathValue("tenant", parts[2])
					s.handleResearchStudyEvaluateEligibility(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Claim") && parts[4] == "$predetermine" {
					r.SetPathValue("tenant", parts[2])
					s.handleClaimPredetermine(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[4] == "$evaluate-risk-condition-category" {
					r.SetPathValue("tenant", parts[2])
					s.handleMeasureEvaluateRiskConditionCategory(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Schedule") && parts[4] == "$find" {
					r.SetPathValue("tenant", parts[2])
					s.handleScheduleFind(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "ServiceRequest") || strings.EqualFold(parts[3], "MedicationRequest")) && parts[4] == "$crd" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					s.handleOrderCRD(w, r)
					return
				}
				if strings.EqualFold(parts[3], "AdverseEvent") && parts[4] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					s.handleAdverseEventEvaluate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "CarePlan") && parts[4] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					s.handleCarePlanEvaluate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$immds-forecast" {
					r.SetPathValue("tenant", parts[2])
					s.handlePatientImmDSForecast(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "Claim") && parts[4] == "$gfe-submit") || (strings.EqualFold(parts[3], "Patient") && parts[4] == "$gfe") {
					r.SetPathValue("tenant", parts[2])
					s.handleGFESubmit(w, r)
					return
				}
				if strings.EqualFold(parts[3], "RiskAssessment") && parts[4] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					s.handleRiskAssessmentEvaluate(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "Patient") || strings.EqualFold(parts[3], "Encounter") || strings.EqualFold(parts[3], "Group")) && parts[4] == "$everything" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					s.handlePatientEverything(w, r)
					return
				}
				if strings.EqualFold(parts[3], "PlanDefinition") && parts[4] == "$apply" {
					r.SetPathValue("tenant", parts[2])
					s.handlePlanDefinitionApply(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ActivityDefinition") && parts[4] == "$apply" {
					r.SetPathValue("tenant", parts[2])
					s.handleActivityDefinitionApply(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Appointment") && parts[4] == "$find" {
					r.SetPathValue("tenant", parts[2])
					s.handleAppointmentFind(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Appointment") && parts[4] == "$hold" {
					r.SetPathValue("tenant", parts[2])
					s.handleAppointmentHold(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Appointment") && parts[4] == "$book" {
					r.SetPathValue("tenant", parts[2])
					s.handleAppointmentBook(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "CapabilityStatement") || strings.EqualFold(parts[3], "metadata")) && parts[4] == "$subset" {
					r.SetPathValue("tenant", parts[2])
					s.handleCapabilityStatementSubset(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "CapabilityStatement") || strings.EqualFold(parts[3], "metadata")) && parts[4] == "$implements" {
					r.SetPathValue("tenant", parts[2])
					s.handleCapabilityStatementImplements(w, r)
					return
				}
				if strings.EqualFold(parts[3], "StructureDefinition") && parts[4] == "$snapshot" {
					r.SetPathValue("tenant", parts[2])
					s.handleStructureDefinitionSnapshot(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "MessageHeader") && parts[4] == "$process-message" {
					r.SetPathValue("tenant", parts[2])
					s.handleProcessMessage(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "StructureMap") && parts[4] == "$transform" {
					r.SetPathValue("tenant", parts[2])
					s.handleStructureMapTransform(w, r)
					return
				}
				if strings.EqualFold(parts[3], "NamingSystem") && parts[4] == "$preferred-id" {
					r.SetPathValue("tenant", parts[2])
					s.handleNamingSystemPreferredID(w, r)
					return
				}
				if strings.EqualFold(parts[3], "List") && parts[4] == "$find" {
					r.SetPathValue("tenant", parts[2])
					s.handleListFind(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "ChargeItem") && parts[4] == "$apply" {
					r.SetPathValue("tenant", parts[2])
					s.handleChargeItemApply(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[4] == "$document" {
					r.SetPathValue("tenant", parts[2])
					s.handleCompositionDocumentType(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MedicationKnowledge") && parts[4] == "$lookup" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", "MedicationKnowledge")
					s.handleMedicationKnowledgeAndInteractions(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MedicationRequest") && parts[4] == "$check-interactions" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", "MedicationRequest")
					s.handleMedicationKnowledgeAndInteractions(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "MessageHeader") && parts[4] == "$process-notification" {
					r.SetPathValue("tenant", parts[2])
					s.handleProcessNotification(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$ihe-pix" {
					r.SetPathValue("tenant", parts[2])
					s.handlePatientIHEPix(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ServiceRequest") && parts[4] == "$collect-specimen" {
					r.SetPathValue("tenant", parts[2])
					s.handleServiceRequestCollectSpecimen(w, r)
					return
				}
				if strings.EqualFold(parts[3], "DiagnosticReport") && parts[4] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					s.handleDiagnosticReportEvaluate(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$bulk-member-match" {
					r.SetPathValue("tenant", parts[2])
					s.handlePatientBulkMemberMatch(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Encounter") && parts[4] == "$admit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$admit")
					s.handleEncounterADTOps(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MedicationRequest") && parts[4] == "$dispense" {
					r.SetPathValue("tenant", parts[2])
					s.handleMedicationRequestDispense(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "PractitionerRole") && parts[4] == "$find-providers") || (strings.EqualFold(parts[3], "HealthcareService") && parts[4] == "$find-services") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					s.handlePlanNetDirectory(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ServiceRequest") && parts[4] == "$sdoh-referral" {
					r.SetPathValue("tenant", parts[2])
					s.handleGravitySDOHReferral(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$sdoh-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handlePatientSDOHSummary(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Consent") && parts[4] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					s.handleConsentEvaluate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$carin-bb" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$carin-bb")
					s.handleCarinBlueButton(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ExplanationOfBenefit") && parts[4] == "$carin-bb-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$carin-bb-summary")
					s.handleCarinBlueButton(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Encounter") && parts[4] == "$ecr-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$ecr-report")
					s.handlePublicHealthECR(w, r)
					return
				}
				if strings.EqualFold(parts[3], "PlanDefinition") && parts[4] == "$ersd-lookup" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$ersd-lookup")
					s.handlePublicHealthECR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$decease" {
					r.SetPathValue("tenant", parts[2])
					s.handlePatientVRDRDecease(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$mcode-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$mcode-summary")
					s.handleMCODEOncology(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Condition") && parts[4] == "$stage-cancer" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$stage-cancer")
					s.handleMCODEOncology(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MedicationKnowledge") && parts[4] == "$formulary-lookup" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$formulary-lookup")
					s.handleDaVinciDrugFormulary(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Coverage") && parts[4] == "$formulary-coverage" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$formulary-coverage")
					s.handleDaVinciDrugFormulary(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Bundle") && parts[4] == "$iti-65" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$iti-65")
					s.handleIHEMHDExchange(w, r)
					return
				}
				if strings.EqualFold(parts[3], "DocumentReference") && parts[4] == "$iti-67" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$iti-67")
					s.handleIHEMHDExchange(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && (parts[4] == "$find-subject-variants" || parts[4] == "$find-subject-tx-implications") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", parts[4])
					s.handleClinicalGenomics(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$pacio-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pacio-summary")
					s.handlePACIOPostAcute(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "QuestionnaireResponse") && parts[4] == "$pacio-assess" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pacio-assess")
					s.handlePACIOPostAcute(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Encounter") && parts[4] == "$nhsn-hai-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$nhsn-hai-report")
					s.handleNHSNHAISurveillance(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[4] == "$nhsn-acute-care" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$nhsn-acute-care")
					s.handleNHSNHAISurveillance(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ResearchStudy") && parts[4] == "$rwd-cohort" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$rwd-cohort")
					s.handleVulcanClinicalResearch(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "ResearchSubject") && parts[4] == "$enroll" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$enroll")
					s.handleVulcanClinicalResearch(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$birth-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$birth-report")
					s.handleVitalRecordsBFDR(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[4] == "$bfdr-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$bfdr-summary")
					s.handleVitalRecordsBFDR(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$ihe-pdqm" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$ihe-pdqm")
					s.handleIHEPDQmAndITI68(w, r)
					return
				}
				if strings.EqualFold(parts[3], "DocumentReference") && parts[4] == "$ihe-iti-68" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$ihe-iti-68")
					s.handleIHEPDQmAndITI68(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Endpoint") && parts[4] == "$fast-endpoint-query" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$fast-endpoint-query")
					s.handleFASTNationalDirectory(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Organization") && parts[4] == "$fast-verify" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$fast-verify")
					s.handleFASTNationalDirectory(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$physical-activity-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$physical-activity-summary")
					s.handlePhysicalActivityIG(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "ServiceRequest") && parts[4] == "$physical-activity-referral" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$physical-activity-referral")
					s.handlePhysicalActivityIG(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "MedicinalProduct") || strings.EqualFold(parts[3], "MedicinalProductDefinition")) && parts[4] == "$epi-document" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$epi-document")
					s.handleVulcanEPIAndRegulatory(w, r)
					return
				}
				if r.Method == http.MethodPost && (strings.EqualFold(parts[3], "MedicinalProductAuthorization") || strings.EqualFold(parts[3], "RegulatedAuthorization")) && parts[4] == "$evaluate-authorization" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$evaluate-authorization")
					s.handleVulcanEPIAndRegulatory(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "DiagnosticReport") && parts[4] == "$elr-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$elr-submit")
					s.handlePublicHealthELR(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Observation") && parts[4] == "$reportable-labs" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$reportable-labs")
					s.handlePublicHealthELR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$adi-record" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$adi-record")
					s.handlePACIOAdvanceDirectives(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[4] == "$adi-document" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$adi-document")
					s.handlePACIOAdvanceDirectives(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$ra-coding-gaps" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$ra-coding-gaps")
					s.handleDaVinciRiskAdjustmentWave37(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "MeasureReport") && parts[4] == "$ra-remediate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$ra-remediate")
					s.handleDaVinciRiskAdjustmentWave37(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[4] == "$eicr-document" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$eicr-document")
					s.handlePublicHealthEICRAndRR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Communication") && parts[4] == "$rr-process" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$rr-process")
					s.handlePublicHealthEICRAndRR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$pdex-WriteData" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pdex-WriteData")
					s.handleDaVinciPDexAndFormularyWave38(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MedicationKnowledge") && parts[4] == "$formulary-alternatives" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$formulary-alternatives")
					s.handleDaVinciPDexAndFormularyWave38(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$record-occupation" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$record-occupation")
					s.handleODHOccupationalHealth(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$odh-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$odh-summary")
					s.handleODHOccupationalHealth(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$cancer-registry-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$cancer-registry-report")
					s.handlePublicHealthCCRRAndMedMorph(w, r)
					return
				}
				if strings.EqualFold(parts[3], "PlanDefinition") && parts[4] == "$medmorph-kar" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$medmorph-kar")
					s.handlePublicHealthCCRRAndMedMorph(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MeasureReport") && parts[4] == "$vbpr-performance" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$vbpr-performance")
					s.handleDaVinciVBPR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "MeasureReport") && parts[4] == "$vbpr-attest" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$vbpr-attest")
					s.handleDaVinciVBPR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$pacio-cognitive-assess" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pacio-cognitive-assess")
					s.handlePACIOCognitiveStatus(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$pacio-cognitive-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pacio-cognitive-summary")
					s.handlePACIOCognitiveStatus(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Claim") && parts[4] == "$rtpbc-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$rtpbc-submit")
					s.handleCarinRTPBC(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ClaimResponse") && parts[4] == "$rtpbc-coverage" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$rtpbc-coverage")
					s.handleCarinRTPBC(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$idi-match" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$idi-match")
					s.handleFASTDigitalIdentity(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Person") && parts[4] == "$verify-identity" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$verify-identity")
					s.handleFASTDigitalIdentity(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Encounter") && parts[4] == "$pacio-timepoint" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pacio-timepoint")
					s.handlePACIOReassessmentTimepoints(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Encounter") && parts[4] == "$pacio-timepoints" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pacio-timepoints")
					s.handlePACIOReassessmentTimepoints(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[4] == "$vrdr-document" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$vrdr-document")
					s.handlePublicHealthVRDRWave41(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Bundle") && parts[4] == "$vrdr-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$vrdr-submit")
					s.handlePublicHealthVRDRWave41(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Task") && parts[4] == "$cdex-request" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$cdex-request")
					s.handleDaVinciCDexTaskRequestAndPoll(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Task") && parts[4] == "$cdex-poll" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$cdex-poll")
					s.handleDaVinciCDexTaskRequestAndPoll(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$pacio-pfe-record" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pacio-pfe-record")
					s.handlePACIOPFE(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$pacio-pfe-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pacio-pfe-summary")
					s.handlePACIOPFE(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[4] == "$bfdr-document" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$bfdr-document")
					s.handlePublicHealthBFDRWave42(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Bundle") && parts[4] == "$bfdr-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$bfdr-submit")
					s.handlePublicHealthBFDRWave42(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Communication") && parts[4] == "$cdex-submit-attachment" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$cdex-submit-attachment")
					s.handleDaVinciCDexWave42(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Task") && parts[4] == "$cdex-questionnaire-request" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$cdex-questionnaire-request")
					s.handleDaVinciCDexWave42(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Procedure") && parts[4] == "$mcode-radiotherapy-record" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$mcode-radiotherapy-record")
					s.handleMCODERadiotherapyWave42(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$mcode-radiotherapy-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$mcode-radiotherapy-summary")
					s.handleMCODERadiotherapyWave42(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Immunization") && parts[4] == "$iis-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$iis-submit")
					s.handlePublicHealthIISWave43(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$iis-history" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$iis-history")
					s.handlePublicHealthIISWave43(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Bundle") && (parts[4] == "$cdex-sign" || parts[4] == "$cdex-verify-signature") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", parts[4])
					s.handleDaVinciCDexSignatureWave43(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "DiagnosticReport") && parts[4] == "$mcode-genomics-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$mcode-genomics-report")
					s.handleMCODEGenomicsWave43(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$mcode-genomics-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$mcode-genomics-summary")
					s.handleMCODEGenomicsWave43(w, r)
					return
				}
				if strings.EqualFold(parts[3], "DiagnosticReport") && parts[4] == "$nbs-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$nbs-submit")
					s.handlePublicHealthNBSWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$nbs-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$nbs-summary")
					s.handlePublicHealthNBSWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ExplanationOfBenefit") && parts[4] == "$pct-aeob-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pct-aeob-summary")
					s.handleDaVinciPCTWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Task") && parts[4] == "$pct-gfe-coordination" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$pct-gfe-coordination")
					s.handleDaVinciPCTWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "AdverseEvent") && parts[4] == "$vulcan-icsr-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$vulcan-icsr-report")
					s.handleVulcanICSRAndSOAWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ResearchSubject") && parts[4] == "$vulcan-soa-timeline" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("op", "$vulcan-soa-timeline")
					s.handleVulcanICSRAndSOAWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Bundle") && parts[4] == "$ecr-submit" {
					r.SetPathValue("tenant", parts[2])
					s.handlePublicHealthECRWave45(w, r, "ecr-submit")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$ecr-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handlePublicHealthECRWave45(w, r, "ecr-summary")
					return
				}
				if strings.EqualFold(parts[3], "QuestionnaireResponse") && parts[4] == "$sdoh-assess" {
					r.SetPathValue("tenant", parts[2])
					s.handleGravitySDOHWave45(w, r, "sdoh-assess")
					return
				}
				if strings.EqualFold(parts[3], "Task") && parts[4] == "$sdoh-fulfill" {
					r.SetPathValue("tenant", parts[2])
					s.handleGravitySDOHWave45(w, r, "sdoh-fulfill")
					return
				}
				if strings.EqualFold(parts[3], "ResearchSubject") && parts[4] == "$vulcan-esource-extract" {
					r.SetPathValue("tenant", parts[2])
					s.handleVulcanESourceWave45(w, r, "vulcan-esource-extract")
					return
				}
				if strings.EqualFold(parts[3], "ResearchStudy") && parts[4] == "$vulcan-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleVulcanESourceWave45(w, r, "vulcan-summary")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$elr-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handlePublicHealthELRAndCCRRWave46(w, r, "elr-summary")
					return
				}
				if strings.EqualFold(parts[3], "Bundle") && parts[4] == "$cancer-registry-submit" {
					r.SetPathValue("tenant", parts[2])
					s.handlePublicHealthELRAndCCRRWave46(w, r, "cancer-registry-submit")
					return
				}
				if strings.EqualFold(parts[3], "ExplanationOfBenefit") && parts[4] == "$carin-bb-claims" {
					r.SetPathValue("tenant", parts[2])
					s.handleCARINBBAndRTPBCWave46(w, r, "carin-bb-claims")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$rtpbc-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleCARINBBAndRTPBCWave46(w, r, "rtpbc-summary")
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[4] == "$pacio-toc-bundle" {
					r.SetPathValue("tenant", parts[2])
					s.handlePACIOTOCAndADIWave46(w, r, "pacio-toc-bundle")
					return
				}
				if strings.EqualFold(parts[3], "Consent") && parts[4] == "$adi-verify" {
					r.SetPathValue("tenant", parts[2])
					s.handlePACIOTOCAndADIWave46(w, r, "adi-verify")
					return
				}
				if strings.EqualFold(parts[3], "MeasureReport") && parts[4] == "$nhsn-aur-submit" {
					r.SetPathValue("tenant", parts[2])
					s.handleNHSNHAIAURWave47(w, r, "nhsn-aur-submit")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$nhsn-hai-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleNHSNHAIAURWave47(w, r, "nhsn-hai-summary")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$mcode-trial-match" {
					r.SetPathValue("tenant", parts[2])
					s.handleCodeXMCODEWave47(w, r, "mcode-trial-match")
					return
				}
				if strings.EqualFold(parts[3], "Observation") && parts[4] == "$mcode-disease-status" {
					r.SetPathValue("tenant", parts[2])
					s.handleCodeXMCODEWave47(w, r, "mcode-disease-status")
					return
				}
				if strings.EqualFold(parts[3], "OrganizationAffiliation") && parts[4] == "$fast-network-query" {
					r.SetPathValue("tenant", parts[2])
					s.handleFASTNDHNetworkWave47(w, r, "fast-network-query")
					return
				}
				if strings.EqualFold(parts[3], "HealthcareService") && parts[4] == "$fast-attest" {
					r.SetPathValue("tenant", parts[2])
					s.handleFASTNDHNetworkWave47(w, r, "fast-attest")
					return
				}
				if strings.EqualFold(parts[3], "MeasureReport") && parts[4] == "$ra-evaluate-risk" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciRAAndCDexWave49(w, r, "ra-evaluate-risk")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$ra-coding-gap-report" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciRAAndCDexWave49(w, r, "ra-coding-gap-report")
					return
				}
				if strings.EqualFold(parts[3], "Task") && parts[4] == "$cdex-submit-attachment" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciRAAndCDexWave49(w, r, "cdex-submit-attachment")
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[4] == "$nhsn-dqm-evaluate" {
					r.SetPathValue("tenant", parts[2])
					s.handleNHSNDQMWave49(w, r, "nhsn-dqm-evaluate")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$nhsn-ach-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleNHSNDQMWave49(w, r, "nhsn-ach-summary")
					return
				}
				if strings.EqualFold(parts[3], "Questionnaire") && parts[4] == "$dtr-questionnaire-package" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciBurdenReductionWave50(w, r, "dtr-questionnaire-package")
					return
				}
				if strings.EqualFold(parts[3], "Claim") && parts[4] == "$pas-inquire" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciBurdenReductionWave50(w, r, "pas-inquire")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$burden-reduction-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciBurdenReductionWave50(w, r, "burden-reduction-summary")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$mcode-trial-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleCodeXMCODEWave47(w, r, "mcode-trial-summary")
					return
				}
				if strings.EqualFold(parts[3], "VerificationResult") && parts[4] == "$ndh-verify" {
					r.SetPathValue("tenant", parts[2])
					s.handleFASTNDHVerifyAndDirectoryWave50(w, r, "ndh-verify")
					return
				}
				if strings.EqualFold(parts[3], "Organization") && parts[4] == "$ndh-directory-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleFASTNDHVerifyAndDirectoryWave50(w, r, "ndh-directory-summary")
					return
				}
				if strings.EqualFold(parts[3], "Group") && parts[4] == "$confirm-attribution-list" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciATRWave51(w, r, "confirm-attribution-list")
					return
				}
				if strings.EqualFold(parts[3], "Group") && parts[4] == "$atr-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciATRWave51(w, r, "atr-summary")
					return
				}
				if strings.EqualFold(parts[3], "Observation") && parts[4] == "$pacio-splasch-record" {
					r.SetPathValue("tenant", parts[2])
					s.handlePACIOSPLASCHWave51(w, r, "pacio-splasch-record")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$pacio-splasch-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handlePACIOSPLASCHWave51(w, r, "pacio-splasch-summary")
					return
				}
				if strings.EqualFold(parts[3], "ResearchStudy") && parts[4] == "$vulcan-rwd-cohort" {
					r.SetPathValue("tenant", parts[2])
					s.handleVulcanRWDWave51(w, r, "vulcan-rwd-cohort")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$vulcan-rwd-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleVulcanRWDWave51(w, r, "vulcan-rwd-summary")
					return
				}
				if strings.EqualFold(parts[3], "MeasureReport") && parts[4] == "$saner-report-capacity" {
					r.SetPathValue("tenant", parts[2])
					s.handlePublicHealthSANERWave52(w, r, "saner-report-capacity")
					return
				}
				if strings.EqualFold(parts[3], "Location") && parts[4] == "$saner-capacity-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handlePublicHealthSANERWave52(w, r, "saner-capacity-summary")
					return
				}
				if strings.EqualFold(parts[3], "ServiceRequest") && parts[4] == "$codex-rttd-plan" {
					r.SetPathValue("tenant", parts[2])
					s.handleCodeXRTTDWave52(w, r, "codex-rttd-plan")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$codex-rttd-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleCodeXRTTDWave52(w, r, "codex-rttd-summary")
					return
				}
				if strings.EqualFold(parts[3], "Consent") && parts[4] == "$sdoh-consent-record" {
					r.SetPathValue("tenant", parts[2])
					s.handleGravitySDOHConsentAndCareWave52(w, r, "sdoh-consent-record")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$sdoh-care-bundle" {
					r.SetPathValue("tenant", parts[2])
					s.handleGravitySDOHConsentAndCareWave52(w, r, "sdoh-care-bundle")
					return
				}
				if strings.EqualFold(parts[3], "Organization") && parts[4] == "$udap-register" {
					r.SetPathValue("tenant", parts[2])
					s.handleUDAPSecurityWave54(w, r, "udap-register")
					return
				}
				if strings.EqualFold(parts[3], "Organization") && parts[4] == "$udap-trust-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleUDAPSecurityWave54(w, r, "udap-trust-summary")
					return
				}
				if strings.EqualFold(parts[3], "Group") && parts[4] == "$pdex-provider-member-match" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciPDexProviderAccessWave54(w, r, "pdex-provider-member-match")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$pdex-provider-everything" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciPDexProviderAccessWave54(w, r, "pdex-provider-everything")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$pdex-payer-exchange" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciPDexPayerExchangeWave54(w, r, "pdex-payer-exchange")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[4] == "$pdex-payer-summary" {
					r.SetPathValue("tenant", parts[2])
					s.handleDaVinciPDexPayerExchangeWave54(w, r, "pdex-payer-summary")
					return
				}
				if strings.EqualFold(parts[3], "TestScript") && parts[4] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					s.handleTestScriptEvaluate(w, r)
					return
				}
				if r.Method == http.MethodPost && parts[4] == "$convert" && !strings.HasPrefix(parts[3], "$") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					s.handleResourceConvert(w, r)
					return
				}
			}
			if len(parts) == 6 && parts[0] == "fhir" && parts[1] == "r4" {
				if strings.EqualFold(parts[3], "TestScript") && parts[5] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleTestScriptEvaluate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Organization") && parts[5] == "$udap-register" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleUDAPSecurityWave54(w, r, "udap-register")
					return
				}
				if strings.EqualFold(parts[3], "Organization") && parts[5] == "$udap-trust-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleUDAPSecurityWave54(w, r, "udap-trust-summary")
					return
				}
				if strings.EqualFold(parts[3], "Group") && parts[5] == "$pdex-provider-member-match" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciPDexProviderAccessWave54(w, r, "pdex-provider-member-match")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$pdex-provider-everything" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciPDexProviderAccessWave54(w, r, "pdex-provider-everything")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$pdex-payer-exchange" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciPDexPayerExchangeWave54(w, r, "pdex-payer-exchange")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$pdex-payer-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciPDexPayerExchangeWave54(w, r, "pdex-payer-summary")
					return
				}
				if strings.EqualFold(parts[3], "MeasureReport") && parts[5] == "$saner-report-capacity" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePublicHealthSANERWave52(w, r, "saner-report-capacity")
					return
				}
				if strings.EqualFold(parts[3], "Location") && parts[5] == "$saner-capacity-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePublicHealthSANERWave52(w, r, "saner-capacity-summary")
					return
				}
				if strings.EqualFold(parts[3], "ServiceRequest") && parts[5] == "$codex-rttd-plan" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCodeXRTTDWave52(w, r, "codex-rttd-plan")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$codex-rttd-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCodeXRTTDWave52(w, r, "codex-rttd-summary")
					return
				}
				if strings.EqualFold(parts[3], "Consent") && parts[5] == "$sdoh-consent-record" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleGravitySDOHConsentAndCareWave52(w, r, "sdoh-consent-record")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$sdoh-care-bundle" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleGravitySDOHConsentAndCareWave52(w, r, "sdoh-care-bundle")
					return
				}
				if strings.EqualFold(parts[3], "Group") && parts[5] == "$confirm-attribution-list" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$confirm-attribution-list")
					s.handleDaVinciATRWave51(w, r, "confirm-attribution-list")
					return
				}
				if strings.EqualFold(parts[3], "Group") && parts[5] == "$atr-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciATRWave51(w, r, "atr-summary")
					return
				}
				if strings.EqualFold(parts[3], "Observation") && parts[5] == "$pacio-splasch-record" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePACIOSPLASCHWave51(w, r, "pacio-splasch-record")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$pacio-splasch-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePACIOSPLASCHWave51(w, r, "pacio-splasch-summary")
					return
				}
				if strings.EqualFold(parts[3], "ResearchStudy") && parts[5] == "$vulcan-rwd-cohort" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleVulcanRWDWave51(w, r, "vulcan-rwd-cohort")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$vulcan-rwd-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleVulcanRWDWave51(w, r, "vulcan-rwd-summary")
					return
				}
				if strings.EqualFold(parts[3], "Questionnaire") && parts[5] == "$dtr-questionnaire-package" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciBurdenReductionWave50(w, r, "dtr-questionnaire-package")
					return
				}
				if strings.EqualFold(parts[3], "Claim") && parts[5] == "$pas-inquire" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciBurdenReductionWave50(w, r, "pas-inquire")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$burden-reduction-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciBurdenReductionWave50(w, r, "burden-reduction-summary")
					return
				}
				if strings.EqualFold(parts[3], "ViewDefinition") && parts[5] == "$run" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleViewDefinitionRun(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ViewDefinition") && parts[5] == "$export" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleViewDefinitionExport(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ViewDefinition") && parts[5] == "$sql" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleViewDefinitionSQL(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$mcode-trial-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCodeXMCODEWave47(w, r, "mcode-trial-summary")
					return
				}
				if strings.EqualFold(parts[3], "VerificationResult") && parts[5] == "$ndh-verify" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleFASTNDHVerifyAndDirectoryWave50(w, r, "ndh-verify")
					return
				}
				if strings.EqualFold(parts[3], "Organization") && parts[5] == "$ndh-directory-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleFASTNDHVerifyAndDirectoryWave50(w, r, "ndh-directory-summary")
					return
				}
				if strings.EqualFold(parts[3], "MeasureReport") && parts[5] == "$ra-evaluate-risk" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciRAAndCDexWave49(w, r, "ra-evaluate-risk")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$ra-coding-gap-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciRAAndCDexWave49(w, r, "ra-coding-gap-report")
					return
				}
				if strings.EqualFold(parts[3], "Task") && parts[5] == "$cdex-submit-attachment" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciRAAndCDexWave49(w, r, "cdex-submit-attachment")
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[5] == "$nhsn-dqm-evaluate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleNHSNDQMWave49(w, r, "nhsn-dqm-evaluate")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$nhsn-ach-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleNHSNDQMWave49(w, r, "nhsn-ach-summary")
					return
				}
				if strings.EqualFold(parts[3], "ServiceRequest") && parts[5] == "$sdoh-referral" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleGravitySDOHReferral(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MeasureReport") && parts[5] == "$nhsn-aur-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleNHSNHAIAURWave47(w, r, "nhsn-aur-submit")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$nhsn-hai-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleNHSNHAIAURWave47(w, r, "nhsn-hai-summary")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$mcode-trial-match" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCodeXMCODEWave47(w, r, "mcode-trial-match")
					return
				}
				if strings.EqualFold(parts[3], "Observation") && parts[5] == "$mcode-disease-status" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCodeXMCODEWave47(w, r, "mcode-disease-status")
					return
				}
				if strings.EqualFold(parts[3], "OrganizationAffiliation") && parts[5] == "$fast-network-query" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleFASTNDHNetworkWave47(w, r, "fast-network-query")
					return
				}
				if strings.EqualFold(parts[3], "HealthcareService") && parts[5] == "$fast-attest" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleFASTNDHNetworkWave47(w, r, "fast-attest")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$elr-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePublicHealthELRAndCCRRWave46(w, r, "elr-summary")
					return
				}
				if strings.EqualFold(parts[3], "Bundle") && parts[5] == "$cancer-registry-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePublicHealthELRAndCCRRWave46(w, r, "cancer-registry-submit")
					return
				}
				if strings.EqualFold(parts[3], "ExplanationOfBenefit") && parts[5] == "$carin-bb-claims" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCARINBBAndRTPBCWave46(w, r, "carin-bb-claims")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$rtpbc-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCARINBBAndRTPBCWave46(w, r, "rtpbc-summary")
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[5] == "$pacio-toc-bundle" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePACIOTOCAndADIWave46(w, r, "pacio-toc-bundle")
					return
				}
				if strings.EqualFold(parts[3], "Consent") && parts[5] == "$adi-verify" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePACIOTOCAndADIWave46(w, r, "adi-verify")
					return
				}
				if strings.EqualFold(parts[3], "Bundle") && parts[5] == "$ecr-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePublicHealthECRWave45(w, r, "ecr-submit")
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$ecr-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePublicHealthECRWave45(w, r, "ecr-summary")
					return
				}
				if strings.EqualFold(parts[3], "QuestionnaireResponse") && parts[5] == "$sdoh-assess" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleGravitySDOHWave45(w, r, "sdoh-assess")
					return
				}
				if strings.EqualFold(parts[3], "Task") && parts[5] == "$sdoh-fulfill" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleGravitySDOHWave45(w, r, "sdoh-fulfill")
					return
				}
				if strings.EqualFold(parts[3], "ResearchSubject") && parts[5] == "$vulcan-esource-extract" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleVulcanESourceWave45(w, r, "vulcan-esource-extract")
					return
				}
				if strings.EqualFold(parts[3], "ResearchStudy") && parts[5] == "$vulcan-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleVulcanESourceWave45(w, r, "vulcan-summary")
					return
				}
				if strings.EqualFold(parts[3], "DiagnosticReport") && parts[5] == "$nbs-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$nbs-submit")
					s.handlePublicHealthNBSWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$nbs-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$nbs-summary")
					s.handlePublicHealthNBSWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ExplanationOfBenefit") && parts[5] == "$pct-aeob-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pct-aeob-summary")
					s.handleDaVinciPCTWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Task") && parts[5] == "$pct-gfe-coordination" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pct-gfe-coordination")
					s.handleDaVinciPCTWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "AdverseEvent") && parts[5] == "$vulcan-icsr-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$vulcan-icsr-report")
					s.handleVulcanICSRAndSOAWave44(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ResearchSubject") && parts[5] == "$vulcan-soa-timeline" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$vulcan-soa-timeline")
					s.handleVulcanICSRAndSOAWave44(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Immunization") && parts[5] == "$iis-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$iis-submit")
					s.handlePublicHealthIISWave43(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$iis-history" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$iis-history")
					s.handlePublicHealthIISWave43(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Bundle") && (parts[5] == "$cdex-sign" || parts[5] == "$cdex-verify-signature") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", parts[5])
					s.handleDaVinciCDexSignatureWave43(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "DiagnosticReport") && parts[5] == "$mcode-genomics-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$mcode-genomics-report")
					s.handleMCODEGenomicsWave43(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$mcode-genomics-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$mcode-genomics-summary")
					s.handleMCODEGenomicsWave43(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[5] == "$bfdr-document" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$bfdr-document")
					s.handlePublicHealthBFDRWave42(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Communication") && parts[5] == "$cdex-submit-attachment" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$cdex-submit-attachment")
					s.handleDaVinciCDexWave42(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Task") && parts[5] == "$cdex-questionnaire-request" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$cdex-questionnaire-request")
					s.handleDaVinciCDexWave42(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Procedure") && parts[5] == "$mcode-radiotherapy-record" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$mcode-radiotherapy-record")
					s.handleMCODERadiotherapyWave42(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$mcode-radiotherapy-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$mcode-radiotherapy-summary")
					s.handleMCODERadiotherapyWave42(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[5] == "$vrdr-document" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$vrdr-document")
					s.handlePublicHealthVRDRWave41(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Task") && parts[5] == "$cdex-request" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$cdex-request")
					s.handleDaVinciCDexTaskRequestAndPoll(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Task") && parts[5] == "$cdex-poll" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$cdex-poll")
					s.handleDaVinciCDexTaskRequestAndPoll(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[5] == "$pacio-pfe-record" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pacio-pfe-record")
					s.handlePACIOPFE(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$pacio-pfe-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pacio-pfe-summary")
					s.handlePACIOPFE(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Claim") && parts[5] == "$rtpbc-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$rtpbc-submit")
					s.handleCarinRTPBC(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ClaimResponse") && parts[5] == "$rtpbc-coverage" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$rtpbc-coverage")
					s.handleCarinRTPBC(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Person") && parts[5] == "$verify-identity" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$verify-identity")
					s.handleFASTDigitalIdentity(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Encounter") && parts[5] == "$pacio-timepoint" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pacio-timepoint")
					s.handlePACIOReassessmentTimepoints(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Encounter") && parts[5] == "$pacio-timepoints" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pacio-timepoints")
					s.handlePACIOReassessmentTimepoints(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$cancer-registry-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$cancer-registry-report")
					s.handlePublicHealthCCRRAndMedMorph(w, r)
					return
				}
				if strings.EqualFold(parts[3], "PlanDefinition") && parts[5] == "$medmorph-kar" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$medmorph-kar")
					s.handlePublicHealthCCRRAndMedMorph(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MeasureReport") && parts[5] == "$vbpr-performance" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$vbpr-performance")
					s.handleDaVinciVBPR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "MeasureReport") && parts[5] == "$vbpr-attest" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$vbpr-attest")
					s.handleDaVinciVBPR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[5] == "$pacio-cognitive-assess" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pacio-cognitive-assess")
					s.handlePACIOCognitiveStatus(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$pacio-cognitive-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pacio-cognitive-summary")
					s.handlePACIOCognitiveStatus(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[5] == "$eicr-document" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$eicr-document")
					s.handlePublicHealthEICRAndRR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Communication") && parts[5] == "$rr-process" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$rr-process")
					s.handlePublicHealthEICRAndRR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[5] == "$pdex-WriteData" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pdex-WriteData")
					s.handleDaVinciPDexAndFormularyWave38(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MedicationKnowledge") && parts[5] == "$formulary-alternatives" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$formulary-alternatives")
					s.handleDaVinciPDexAndFormularyWave38(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[5] == "$record-occupation" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$record-occupation")
					s.handleODHOccupationalHealth(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$odh-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$odh-summary")
					s.handleODHOccupationalHealth(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "DiagnosticReport") && parts[5] == "$elr-submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$elr-submit")
					s.handlePublicHealthELR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[5] == "$adi-record" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$adi-record")
					s.handlePACIOAdvanceDirectives(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$ra-coding-gaps" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$ra-coding-gaps")
					s.handleDaVinciRiskAdjustmentWave37(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "MeasureReport") && parts[5] == "$ra-remediate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$ra-remediate")
					s.handleDaVinciRiskAdjustmentWave37(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Organization") && parts[5] == "$fast-verify" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$fast-verify")
					s.handleFASTNationalDirectory(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$physical-activity-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$physical-activity-summary")
					s.handlePhysicalActivityIG(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "MedicinalProduct") || strings.EqualFold(parts[3], "MedicinalProductDefinition")) && parts[5] == "$epi-document" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$epi-document")
					s.handleVulcanEPIAndRegulatory(w, r)
					return
				}
				if r.Method == http.MethodPost && (strings.EqualFold(parts[3], "MedicinalProductAuthorization") || strings.EqualFold(parts[3], "RegulatedAuthorization")) && parts[5] == "$evaluate-authorization" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$evaluate-authorization")
					s.handleVulcanEPIAndRegulatory(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ResearchStudy") && parts[5] == "$rwd-cohort" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$rwd-cohort")
					s.handleVulcanClinicalResearch(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "ResearchSubject") && parts[5] == "$enroll" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$enroll")
					s.handleVulcanClinicalResearch(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[5] == "$birth-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$birth-report")
					s.handleVitalRecordsBFDR(w, r)
					return
				}
				if strings.EqualFold(parts[3], "DocumentReference") && parts[5] == "$ihe-iti-68" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$ihe-iti-68")
					s.handleIHEPDQmAndITI68(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && (parts[5] == "$find-subject-variants" || parts[5] == "$find-subject-tx-implications") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", parts[5])
					s.handleClinicalGenomics(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$pacio-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pacio-summary")
					s.handlePACIOPostAcute(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "QuestionnaireResponse") && parts[5] == "$pacio-assess" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$pacio-assess")
					s.handlePACIOPostAcute(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Encounter") && parts[5] == "$nhsn-hai-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$nhsn-hai-report")
					s.handleNHSNHAISurveillance(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$mcode-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$mcode-summary")
					s.handleMCODEOncology(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Condition") && parts[5] == "$stage-cancer" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$stage-cancer")
					s.handleMCODEOncology(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Coverage") && parts[5] == "$formulary-coverage" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$formulary-coverage")
					s.handleDaVinciDrugFormulary(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$carin-bb" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$carin-bb")
					s.handleCarinBlueButton(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Encounter") && parts[5] == "$ecr-report" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", "$ecr-report")
					s.handlePublicHealthECR(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[5] == "$decease" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePatientVRDRDecease(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$sdoh-summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePatientSDOHSummary(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Consent") && parts[5] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleConsentEvaluate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Group") && parts[5] == "$davinci-data-export" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDaVinciDataExport(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Encounter") && (parts[5] == "$transfer" || parts[5] == "$discharge") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", parts[5])
					s.handleEncounterADTOps(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MedicationRequest") && parts[5] == "$dispense" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleMedicationRequestDispense(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$ihe-pix" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePatientIHEPix(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ServiceRequest") && parts[5] == "$collect-specimen" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleServiceRequestCollectSpecimen(w, r)
					return
				}
				if strings.EqualFold(parts[3], "DiagnosticReport") && parts[5] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleDiagnosticReportEvaluate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Group") && (parts[5] == "$member-add" || parts[5] == "$member-remove" || parts[5] == "$confirm-attribution-list") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					r.SetPathValue("op", parts[5])
					s.handleGroupATRMember(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MedicationKnowledge") && parts[5] == "$lookup" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", "MedicationKnowledge")
					r.SetPathValue("id", parts[4])
					s.handleMedicationKnowledgeAndInteractions(w, r)
					return
				}
				if strings.EqualFold(parts[3], "MedicationRequest") && parts[5] == "$check-interactions" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", "MedicationRequest")
					r.SetPathValue("id", parts[4])
					s.handleMedicationKnowledgeAndInteractions(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "ChargeItem") && parts[5] == "$apply" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleChargeItemApply(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "StructureMap") && parts[5] == "$transform" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleStructureMapTransform(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Appointment") && parts[5] == "$hold" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleAppointmentHold(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Appointment") && parts[5] == "$book" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleAppointmentBook(w, r)
					return
				}
				if strings.EqualFold(parts[3], "StructureDefinition") && parts[5] == "$snapshot" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleStructureDefinitionSnapshot(w, r)
					return
				}
				if parts[5] == "$everything" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handlePatientEverything(w, r)
					return
				}
				if r.Method == http.MethodPost && parts[5] == "$reindex" && !strings.HasPrefix(parts[3], "$") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handleReindex(w, r)
					return
				}
				if strings.EqualFold(parts[3], "PlanDefinition") && parts[5] == "$apply" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePlanDefinitionApply(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ActivityDefinition") && parts[5] == "$apply" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleActivityDefinitionApply(w, r)
					return
				}
				if parts[5] == "$validate" && !strings.HasPrefix(parts[3], "$") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handleValidate(w, r)
					return
				}
				if parts[5] == "$diff" && !strings.HasPrefix(parts[3], "$") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handleDiff(w, r)
					return
				}
				if parts[5] == "$graph" && !strings.HasPrefix(parts[3], "$") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handleResourceGraph(w, r)
					return
				}
				if r.Method == http.MethodPost && parts[5] == "$expunge" && !strings.HasPrefix(parts[3], "$") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handleExpunge(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[5] == "$purge" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", "Patient")
					r.SetPathValue("id", parts[4])
					s.handlePatientPurge(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Subscription") && parts[5] == "$status" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", "Subscription")
					r.SetPathValue("id", parts[4])
					s.handleSubscriptionStatus(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Subscription") && parts[5] == "$events" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", "Subscription")
					r.SetPathValue("id", parts[4])
					s.handleSubscriptionEvents(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "Library") || strings.EqualFold(parts[3], "Measure") || strings.EqualFold(parts[3], "PlanDefinition") || strings.EqualFold(parts[3], "ActivityDefinition") || strings.EqualFold(parts[3], "Questionnaire")) && parts[5] == "$data-requirements" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handleDataRequirements(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "CoverageEligibilityRequest") && parts[5] == "$submit" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCoverageEligibilitySubmit(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Task") && parts[5] == "$submit-attachment" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCDexSubmitAttachment(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Library") && parts[5] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleLibraryEvaluate(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[5] == "$health-cards-issue" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePatientHealthCardsIssue(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$pcde" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePatientPCDE(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ResearchStudy") && parts[5] == "$evaluate-eligibility" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleResearchStudyEvaluateEligibility(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Claim") && parts[5] == "$predetermine" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleClaimPredetermine(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[5] == "$evaluate-risk-condition-category" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleMeasureEvaluateRiskConditionCategory(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Schedule") && parts[5] == "$find" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleScheduleFind(w, r)
					return
				}
				if (strings.EqualFold(parts[3], "ServiceRequest") || strings.EqualFold(parts[3], "MedicationRequest")) && parts[5] == "$crd" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handleOrderCRD(w, r)
					return
				}
				if strings.EqualFold(parts[3], "AdverseEvent") && parts[5] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleAdverseEventEvaluate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "CarePlan") && parts[5] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCarePlanEvaluate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$immds-forecast" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePatientImmDSForecast(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$gfe" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleGFESubmit(w, r)
					return
				}
				if strings.EqualFold(parts[3], "RiskAssessment") && parts[5] == "$evaluate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleRiskAssessmentEvaluate(w, r)
					return
				}
				if parts[5] == "$fhirpath" && !strings.HasPrefix(parts[3], "$") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handleFHIRPath(w, r)
					return
				}
				if r.Method == http.MethodGet && parts[5] == "$provenance" && !strings.HasPrefix(parts[3], "$") {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handleResourceProvenance(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Patient") && parts[5] == "$summary" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handlePatientSummary(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Composition") && parts[5] == "$document" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCompositionDocument(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Questionnaire") && parts[5] == "$populate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleQuestionnairePopulate(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Questionnaire") && parts[5] == "$package" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleQuestionnairePackage(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Claim") && parts[5] == "$inquire" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleClaimInquire(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "QuestionnaireResponse") && parts[5] == "$extract" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleQuestionnaireResponseExtract(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[5] == "$evaluate-measure" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleEvaluateMeasure(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[5] == "$collect-data" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleMeasureCollectData(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Measure") && parts[5] == "$submit-data" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleMeasureSubmitData(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Group") && parts[5] == "$add" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleGroupAdd(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Group") && parts[5] == "$remove" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleGroupRemove(w, r)
					return
				}
				if strings.EqualFold(parts[3], "Measure") && parts[5] == "$care-gaps" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCareGaps(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ValueSet") && parts[5] == "$expand" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleValueSetExpand(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ValueSet") && parts[5] == "$validate-code" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleValueSetValidateCode(w, r)
					return
				}
				if strings.EqualFold(parts[3], "CodeSystem") && parts[5] == "$lookup" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCodeSystemLookup(w, r)
					return
				}
				if strings.EqualFold(parts[3], "CodeSystem") && parts[5] == "$validate-code" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCodeSystemValidateCode(w, r)
					return
				}
				if strings.EqualFold(parts[3], "CodeSystem") && parts[5] == "$subsumes" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCodeSystemSubsumes(w, r)
					return
				}
				if strings.EqualFold(parts[3], "CodeSystem") && parts[5] == "$find-matches" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleCodeSystemFindMatches(w, r)
					return
				}
				if strings.EqualFold(parts[3], "ConceptMap") && parts[5] == "$translate" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("id", parts[4])
					s.handleConceptMapTranslate(w, r)
					return
				}
				if isGetOrHead && parts[3] == "$export-file" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("jobId", parts[4])
					r.SetPathValue("fileName", parts[5])
					s.handleExportFile(w, r)
					return
				}
				if parts[3] == "cds-services" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("serviceId", parts[4])
					switch parts[5] {
					case "$dsi-attributes":
						s.handleCDSServiceDSIAttributes(w, r)
						return
					case "feedback":
						if r.Method == http.MethodPost {
							s.handleCDSServiceFeedback(w, r)
							return
						}
					}
				}
				if isGetOrHead && postgres.IsCompartmentType(parts[3]) && !strings.HasPrefix(parts[5], "$") && parts[5] != "_history" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("compartmentType", postgres.CanonicalCompartmentType(parts[3]))
					r.SetPathValue("compartmentId", parts[4])
					r.SetPathValue("patientId", parts[4])
					r.SetPathValue("resourceType", parts[5])
					s.handleCompartmentSearch(w, r)
					return
				}
			}
			if len(parts) == 7 && parts[0] == "fhir" && parts[1] == "r4" {
				if r.Method == http.MethodPost && postgres.IsCompartmentType(parts[3]) && !strings.HasPrefix(parts[5], "$") && parts[5] != "_history" && parts[6] == "_search" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("compartmentType", postgres.CanonicalCompartmentType(parts[3]))
					r.SetPathValue("compartmentId", parts[4])
					r.SetPathValue("patientId", parts[4])
					r.SetPathValue("resourceType", parts[5])
					s.handleCompartmentSearch(w, r)
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})

	return router
}

// handleRead handles GET /{resourceType}/{id} — read a single resource.
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("%s/%s not found", resType, resID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		if rec != nil && rec.ResVersion > 0 {
			w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
		}
		writeOperationOutcome(w, http.StatusGone, "deleted",
			fmt.Sprintf("%s/%s has been deleted", resType, resID))
		return
	}
	if err != nil {
		s.logger.Error("read resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal error")
		return
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if constraints, constrained := auth.ScopeQueryConstraints(secCtx, resType, auth.ActionRead); constrained {
			if !s.resourceSatisfiesScopeConstraints(tenant, resType, resID, rec.ResourceProto, constraints) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient granular SMART v2 scope to read %s/%s", resType, resID))
				return
			}
		}
	}

	// Convert proto bytes to JSON
	jsonBytes, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		s.logger.Error("proto to json", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "serialization error")
		return
	}

	summary := strings.TrimSpace(r.URL.Query().Get("_summary"))
	var elements []string
	for _, rawEl := range r.URL.Query()["_elements"] {
		for el := range strings.SplitSeq(rawEl, ",") {
			if trimmed := strings.TrimSpace(el); trimmed != "" {
				elements = append(elements, trimmed)
			}
		}
	}
	if (summary != "" && summary != "false") || len(elements) > 0 {
		var resourceMap map[string]any
		if err := json.Unmarshal(jsonBytes, &resourceMap); err == nil {
			projected := applySummaryOrElements(resourceMap, summary, elements)
			if b, err := json.Marshal(projected); err == nil {
				jsonBytes = b
			}
		}
	} else if strings.EqualFold(resType, "Binary") {
		formatParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("_format")))
		acceptHdr := strings.ToLower(strings.TrimSpace(r.Header.Get("Accept")))
		wantsFHIRJSON := strings.Contains(formatParam, "fhir") || strings.Contains(formatParam, "json") ||
			strings.Contains(acceptHdr, "application/fhir+json") || strings.Contains(acceptHdr, "application/json")
		if !wantsFHIRJSON && (formatParam != "" || acceptHdr != "") {
			var binMap map[string]any
			if json.Unmarshal(jsonBytes, &binMap) == nil {
				ct, _ := binMap["contentType"].(string)
				dataB64, _ := binMap["data"].(string)
				ctTrim := strings.TrimSpace(ct)
				ctLower := strings.ToLower(ctTrim)
				if ctTrim != "" && dataB64 != "" &&
					(formatParam == ctLower || strings.Contains(acceptHdr, ctLower) || acceptHdr == "*/*") {
					if rawBytes, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(dataB64)); decErr == nil {
						if secRefMap, ok := binMap["securityContext"].(map[string]any); ok {
							if secRef, _ := secRefMap["reference"].(string); strings.TrimSpace(secRef) != "" {
								w.Header().Set("X-Security-Context", strings.TrimSpace(secRef))
							}
						}
						w.Header().Set("Content-Type", ctTrim)
						w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
						w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
						if checkConditionalReadNotModified(w, r, rec.ResVersion, rec.LastUpdated) {
							return
						}
						w.WriteHeader(http.StatusOK)
						if r.Method != http.MethodHead {
							w.Write(rawBytes)
						}
						return
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	if checkConditionalReadNotModified(w, r, rec.ResVersion, rec.LastUpdated) {
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(jsonBytes)
	}
}

// checkConditionalReadNotModified evaluates FHIR R4 §3.1.0.1.1 Conditional Read headers
// (If-None-Match and If-Modified-Since) and writes 304 Not Modified when satisfied.
func checkConditionalReadNotModified(w http.ResponseWriter, r *http.Request, version int, lastUpdated time.Time) bool {
	if r == nil {
		return false
	}
	if inmVals := r.Header.Values("If-None-Match"); len(inmVals) > 0 {
		for _, rawHeader := range inmVals {
			for cand := range strings.SplitSeq(rawHeader, ",") {
				cand = strings.TrimSpace(cand)
				if cand == "" {
					continue
				}
				if cand == "*" {
					w.WriteHeader(http.StatusNotModified)
					return true
				}
				if v, err := postgres.ParseETagVersion(cand); err == nil && v == version {
					w.WriteHeader(http.StatusNotModified)
					return true
				}
			}
		}
		return false
	}

	if ims := strings.TrimSpace(r.Header.Get("If-Modified-Since")); ims != "" && !lastUpdated.IsZero() {
		imsTime, ok := parseConditionalReadTime(ims)
		if ok {
			resUTC := lastUpdated.UTC()
			imsUTC := imsTime.UTC()
			if imsUTC.Nanosecond() == 0 {
				resUTC = resUTC.Truncate(time.Second)
			}
			if !resUTC.After(imsUTC) {
				w.WriteHeader(http.StatusNotModified)
				return true
			}
		}
	}
	return false
}

func parseConditionalReadTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := http.ParseTime(raw); err == nil {
		return t, true
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, time.RFC1123Z, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// handleVRead handles GET /{resourceType}/{id}/_history/{vid} — read a specific historical version.
func (s *Server) handleVRead(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")
	vidStr := r.PathValue("vid")

	version, err := strconv.Atoi(vidStr)
	if err != nil || version <= 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid",
			fmt.Sprintf("invalid version id %q", vidStr))
		return
	}

	rec, err := s.store.ReadResourceVersion(r.Context(), tenant, resType, resID, version)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("%s/%s/_history/%d not found", resType, resID, version))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		if rec != nil && rec.ResVersion > 0 {
			w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
		}
		writeOperationOutcome(w, http.StatusGone, "deleted",
			fmt.Sprintf("%s/%s/_history/%d was a deletion", resType, resID, version))
		return
	}
	if err != nil {
		s.logger.Error("vread resource", "error", err, "type", resType, "id", resID, "version", version)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal error")
		return
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if constraints, constrained := auth.ScopeQueryConstraints(secCtx, resType, auth.ActionRead); constrained {
			if !s.resourceSatisfiesScopeConstraints(tenant, resType, resID, rec.ResourceProto, constraints) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient granular SMART v2 scope to vread %s/%s/_history/%d", resType, resID, version))
				return
			}
		}
	}

	jsonBytes, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		s.logger.Error("proto to json in vread", "error", err, "type", resType, "id", resID, "version", version)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "serialization error")
		return
	}

	summary := strings.TrimSpace(r.URL.Query().Get("_summary"))
	var elements []string
	for _, rawEl := range r.URL.Query()["_elements"] {
		for el := range strings.SplitSeq(rawEl, ",") {
			if trimmed := strings.TrimSpace(el); trimmed != "" {
				elements = append(elements, trimmed)
			}
		}
	}
	if (summary != "" && summary != "false") || len(elements) > 0 {
		var resourceMap map[string]any
		if err := json.Unmarshal(jsonBytes, &resourceMap); err == nil {
			projected := applySummaryOrElements(resourceMap, summary, elements)
			if b, err := json.Marshal(projected); err == nil {
				jsonBytes = b
			}
		}
	} else if strings.EqualFold(resType, "Binary") {
		formatParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("_format")))
		acceptHdr := strings.ToLower(strings.TrimSpace(r.Header.Get("Accept")))
		wantsFHIRJSON := strings.Contains(formatParam, "fhir") || strings.Contains(formatParam, "json") ||
			strings.Contains(acceptHdr, "application/fhir+json") || strings.Contains(acceptHdr, "application/json")
		if !wantsFHIRJSON && (formatParam != "" || acceptHdr != "") {
			var binMap map[string]any
			if json.Unmarshal(jsonBytes, &binMap) == nil {
				ct, _ := binMap["contentType"].(string)
				dataB64, _ := binMap["data"].(string)
				ctTrim := strings.TrimSpace(ct)
				ctLower := strings.ToLower(ctTrim)
				if ctTrim != "" && dataB64 != "" &&
					(formatParam == ctLower || strings.Contains(acceptHdr, ctLower) || acceptHdr == "*/*") {
					if rawBytes, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(dataB64)); decErr == nil {
						if secRefMap, ok := binMap["securityContext"].(map[string]any); ok {
							if secRef, _ := secRefMap["reference"].(string); strings.TrimSpace(secRef) != "" {
								w.Header().Set("X-Security-Context", strings.TrimSpace(secRef))
							}
						}
						w.Header().Set("Content-Type", ctTrim)
						w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
						w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
						if checkConditionalReadNotModified(w, r, rec.ResVersion, rec.LastUpdated) {
							return
						}
						w.WriteHeader(http.StatusOK)
						if r.Method != http.MethodHead {
							w.Write(rawBytes)
						}
						return
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	if checkConditionalReadNotModified(w, r, rec.ResVersion, rec.LastUpdated) {
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(jsonBytes)
	}
}

// handleInstanceHistory handles GET /{resourceType}/{id}/_history, GET /{resourceType}/_history,
// and GET /_history — list resource versions as a history Bundle.
func (s *Server) handleInstanceHistory(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")
	q := r.URL.Query()

	count := 100
	offset := 0
	var since *time.Time
	var atOps []postgres.DateOp
	var listIDs []string
	summary := strings.TrimSpace(q.Get("_summary"))
	switch summary {
	case "", "true", "text", "data", "count", "false":
	default:
		writeOperationOutcome(w, http.StatusBadRequest, "invalid",
			fmt.Sprintf("invalid _summary parameter %q: expected true, text, data, count, or false", summary))
		return
	}
	var elements []string
	for _, rawEl := range q["_elements"] {
		for el := range strings.SplitSeq(rawEl, ",") {
			if trimmed := strings.TrimSpace(el); trimmed != "" {
				elements = append(elements, trimmed)
			}
		}
	}

	if v := strings.TrimSpace(q.Get("_count")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _count parameter %q: must be a positive integer", v))
			return
		}
		count = n
	}
	if v := strings.TrimSpace(q.Get("_offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _offset parameter %q: must be a non-negative integer", v))
			return
		}
		offset = n
	}
	if sinceRaw := strings.TrimSpace(q.Get("_since")); sinceRaw != "" {
		dOp, err := postgres.ParseDateOp(sinceRaw)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _since parameter %q: %v", sinceRaw, err))
			return
		}
		t := dOp.Value
		since = &t
	}
	for _, rawAt := range q["_at"] {
		for part := range strings.SplitSeq(rawAt, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			dOp, err := postgres.ParseDateOp(trimmed)
			if err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("invalid _at parameter %q: %v", trimmed, err))
				return
			}
			atOps = append(atOps, dOp)
		}
	}
	for _, rawList := range q["_list"] {
		if trimmed := strings.TrimSpace(rawList); trimmed != "" {
			listIDs = append(listIDs, trimmed)
		}
	}

	if len(listIDs) > 0 {
		if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
			if !auth.Allows(secCtx, "List", auth.ActionRead) && !auth.Allows(secCtx, "List", auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"insufficient scope for _list history filter (requires List read/search scope)")
				return
			}
		}
	}

	records, total, err := s.store.ListHistoryWithParams(r.Context(), postgres.HistoryParams{
		TenantID: tenant,
		ResType:  resType,
		ResID:    resID,
		Since:    since,
		At:       atOps,
		List:     listIDs,
		Limit:    count,
		Offset:   offset,
	})
	if err != nil {
		s.logger.Error("list resource history", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to query resource history")
		return
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if resID != "" {
			if constraints, constrained := auth.ScopeQueryConstraints(secCtx, resType, auth.ActionRead); constrained {
				for _, rec := range records {
					if !rec.IsDeleted && len(rec.ResourceProto) > 0 && !s.resourceSatisfiesScopeConstraints(tenant, resType, resID, rec.ResourceProto, constraints) {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("insufficient granular SMART v2 scope to read history of %s/%s", resType, resID))
						return
					}
				}
			}
		} else {
			patientRestricted := auth.IsPatientRestricted(secCtx)
			allowedPatientID := cleanID(secCtx.GetPatientId())
			filtered := make([]postgres.ResourceRecord, 0, len(records))
			for _, rec := range records {
				rt := rec.ResType
				if rt == "" {
					rt = resType
				}
				if !s.resourceAllowedByGranularScope(secCtx, tenant, rt, rec.ResID, rec.ResourceProto) {
					continue
				}
				if patientRestricted {
					if strings.EqualFold(rt, "Patient") {
						if cleanID(rec.ResID) != allowedPatientID {
							continue
						}
					} else if postgres.IsPatientCompartmentResourceType(rt) {
						if extractor, ok := s.extractors[rt]; ok && len(rec.ResourceProto) > 0 {
							idx, err := extractor(tenant, rec.ResID, rec.ResourceProto)
							if err != nil || !postgres.IndexesBelongToPatientCompartment(rt, rec.ResID, idx, allowedPatientID) {
								continue
							}
						} else if s.store != nil {
							belongs, err := s.store.BelongsToPatientCompartment(r.Context(), tenant, rt, rec.ResID, allowedPatientID)
							if err != nil || !belongs {
								continue
							}
						}
					} else {
						continue
					}
				}
				filtered = append(filtered, rec)
			}
			removed := len(records) - len(filtered)
			records = filtered
			if removed > 0 {
				total = max(len(records), total-removed)
			}
		}
	}

	baseURL := requestBaseURL(r) + r.URL.Path
	makeURL := func(vals url.Values) string {
		enc := vals.Encode()
		if enc == "" {
			return baseURL
		}
		return baseURL + "?" + enc
	}
	links := []map[string]string{
		{
			"relation": "self",
			"url":      makeURL(q),
		},
	}
	if summary != "count" {
		if len(records) > 0 && offset+len(records) < total {
			nextQ := cloneURLValues(q)
			nextQ.Set("_count", strconv.Itoa(count))
			nextQ.Set("_offset", strconv.Itoa(offset+count))
			links = append(links, map[string]string{
				"relation": "next",
				"url":      makeURL(nextQ),
			})
		}
		if offset > 0 {
			prevQ := cloneURLValues(q)
			prevQ.Set("_count", strconv.Itoa(count))
			prevQ.Set("_offset", strconv.Itoa(max(0, offset-count)))
			links = append(links, map[string]string{
				"relation": "previous",
				"url":      makeURL(prevQ),
			})
		}
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "history",
		"total":        total,
		"link":         links,
	}

	if summary != "count" {
		needsProjection := (summary != "" && summary != "false") || len(elements) > 0
		entries := make([]any, 0, len(records))
		for _, rec := range records {
			rt := rec.ResType
			if rt == "" {
				rt = resType
			}
			rid := rec.ResID
			if rid == "" {
				rid = resID
			}
			method := "PUT"
			status := "200 OK"
			if rec.IsDeleted {
				method = "DELETE"
				status = "204 No Content"
			} else if rec.ResVersion == 1 {
				method = "POST"
				status = "201 Created"
			}

			entry := map[string]any{
				"fullUrl": fmt.Sprintf("%s/%s", rt, rid),
				"request": map[string]any{
					"method": method,
					"url":    fmt.Sprintf("%s/%s", rt, rid),
				},
				"response": map[string]any{
					"status":       status,
					"location":     fmt.Sprintf("%s/%s/_history/%d", rt, rid, rec.ResVersion),
					"etag":         postgres.FormatETag(rec.ResVersion),
					"lastModified": rec.LastUpdated.UTC().Format(time.RFC3339),
				},
			}

			if !rec.IsDeleted && len(rec.ResourceProto) > 0 {
				if jsonBytes, err := s.protoToJSONWithMeta(rt, rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
					var resourceMap map[string]any
					if err := json.Unmarshal(jsonBytes, &resourceMap); err == nil {
						if needsProjection {
							resourceMap = applySummaryOrElements(resourceMap, summary, elements)
						}
						entry["resource"] = resourceMap
					}
				}
			}

			entries = append(entries, entry)
		}
		bundle["entry"] = entries
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r == nil || r.Method != http.MethodHead {
		json.NewEncoder(w).Encode(bundle)
	}
}

// handleCreate handles POST /{resourceType} — create a FHIR resource.
// Accepts FHIR JSON (standard flat R4 JSON or google/fhir protojson), converts to proto, stores in Postgres.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")

	md, ok := s.protoRegistry[resType]
	if !ok {
		writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
			fmt.Sprintf("resource type %s not registered", resType))
		return
	}

	provSpec, err := parseXProvenanceHeader(r)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	// Read request body
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB max
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read body")
		return
	}

	resolvedBody, refStatus, refErr := s.resolveConditionalReferencesInJSON(r.Context(), tenant, body, nil)
	if refErr != nil {
		writeOperationOutcome(w, refStatus, conditionalRefOutcomeCode(refStatus), refErr.Error())
		return
	}
	body = resolvedBody

	// Validate against FHIR R4 StructureDefinitions and ValueSets if validator configured
	if s.validator != nil {
		outcome := s.validator.ValidateJSON(resType, body)
		if !outcome.IsValid() {
			outcome.WriteHTTP(w, http.StatusBadRequest)
			return
		}
	}

	if err := s.validateSubscriptionResourceJSON(resType, body); err != nil {
		writeOperationOutcome(w, http.StatusUnprocessableEntity, "security", err.Error())
		return
	}
	body = normalizeSubscriptionOnWrite(resType, body)

	// Normalize standard flat HL7 FHIR R4 JSON to google/fhir protojson shape
	normalizedBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, body)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("invalid FHIR JSON: %v", err))
		return
	}

	// Unmarshal JSON → proto
	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(normalizedBody, msg); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("invalid FHIR JSON: %v", err))
		return
	}

	// Extract resource ID from the proto
	resID := extractProtoResourceID(msg)
	if resID == "" {
		// Generate a unique ID if none provided
		resID = generateID()
		setProtoResourceID(msg, resID)
	}

	// Marshal proto → bytes for storage
	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception",
			"failed to encode resource")
		return
	}

	s.syncSearchParameterOnWrite(resType, resID, body)

	// Extract search indexes if available
	var idx *postgres.SearchIndexes
	if extractor, ok := s.extractors[resType]; ok {
		var extractErr error
		idx, extractErr = extractor(tenant, resID, protoBytes)
		if extractErr != nil {
			s.logger.Warn("index extraction failed, storing without indexes",
				"type", resType, "id", resID, "error", extractErr)
		}
	}
	if strings.EqualFold(resType, "Provenance") {
		s.enrichProvenancePatientIndex(r.Context(), tenant, resID, idx)
	}
	setAuditPatientID(r.Context(), extractPatientIDFromIndexes(resType, resID, idx))

	// Enforce patient compartment isolation on incoming resource payload
	if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
		if strings.EqualFold(resType, "Patient") || postgres.IsPatientCompartmentResourceType(resType) {
			if !postgres.IndexesBelongToPatientCompartment(resType, resID, idx, cleanID(secCtx.GetPatientId())) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot create %s outside patient compartment",
						secCtx.GetPatientId(), resType))
				return
			}
		}
	}

	input := postgres.ResourceInput{
		TenantID:      tenant,
		ResType:       resType,
		ResID:         resID,
		ResourceProto: protoBytes,
		SearchIndexes: idx,
		CreateOnly:    true,
	}

	// Evaluate Conditional Create (If-None-Exist header) atomically per FHIR R4 3.1.0.5.1
	if ifNoneExist := strings.TrimSpace(r.Header.Get("If-None-Exist")); ifNoneExist != "" && s.store != nil {
		condParams, err := postgres.ParseIfNoneExist(tenant, resType, ifNoneExist)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
			if err := validateAndScopeIfNoneExistForPatient(&condParams, resType, cleanID(secCtx.GetPatientId()), secCtx.GetPatientId()); err != nil {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden", err.Error())
				return
			}
		}

		rec, created, err := s.store.ConditionalCreateResource(r.Context(), input, condParams)
		if errors.Is(err, postgres.ErrPreconditionFailed) {
			writeOperationOutcome(w, http.StatusPreconditionFailed, "duplicate", err.Error())
			return
		}
		if errors.Is(err, postgres.ErrResourceAlreadyExists) {
			writeOperationOutcome(w, http.StatusConflict, "duplicate", err.Error())
			return
		}
		if err != nil {
			s.logger.Error("conditional create", "error", err, "type", resType, "ifNoneExist", ifNoneExist)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "conditional create failed")
			return
		}
		if created {
			s.publishCDC(r.Context(), automq.Message{
				TenantID:  tenant,
				ResType:   resType,
				ResID:     rec.ResID,
				ProtoData: rec.ResourceProto,
				Op:        "U",
			})
			s.dispatchSubscriptions(r.Context(), tenant, resType, rec.ResID, rec.ResourceProto, rec.ResVersion, rec.LastUpdated, idx)
			s.recordAutoProvenance(r, w, tenant, resType, rec.ResID, rec.ResVersion, idx, provSpec)
		}
		jsonBytes, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
		if err != nil {
			s.logger.Error("conditional create proto to json", "error", err, "type", resType, "id", rec.ResID)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "serialization error")
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, rec.ResID))
		w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
		w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writePreferResourceResponse(w, r, status, resType, rec.ResID, rec.ResVersion, jsonBytes)
		return
	}

	// Store in Postgres (CreateOnly = true ensures POST does not overwrite existing active resources)
	rec, err := s.store.WriteResourceWithMeta(r.Context(), input)
	if errors.Is(err, postgres.ErrResourceAlreadyExists) {
		writeOperationOutcome(w, http.StatusConflict, "duplicate", err.Error())
		return
	}
	if err != nil {
		s.logger.Error("create resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to store resource")
		return
	}

	s.publishCDC(r.Context(), automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     resID,
		ProtoData: rec.ResourceProto,
		Op:        "U",
	})
	s.dispatchSubscriptions(r.Context(), tenant, resType, resID, rec.ResourceProto, rec.ResVersion, rec.LastUpdated, idx)
	s.recordAutoProvenance(r, w, tenant, resType, resID, rec.ResVersion, idx, provSpec)

	jsonBytes, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		jsonBytes = body
	}

	// Return the serialized resource (or Prefer representation/minimal/OperationOutcome) with 201 Created, Location, ETag, and Last-Modified
	w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, resID))
	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	writePreferResourceResponse(w, r, http.StatusCreated, resType, resID, rec.ResVersion, jsonBytes)
}

func validateAndScopeIfNoneExistForPatient(condParams *postgres.SearchParams, resType, allowedPatientID, rawPatientID string) error {
	if allowedPatientID == "" {
		return fmt.Errorf("patient-scoped token is missing required patient_id claim")
	}
	if strings.EqualFold(resType, "Patient") {
		if condParams.ID != "" {
			for part := range strings.SplitSeq(condParams.ID, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
					return fmt.Errorf("patient-scoped token (%s) cannot evaluate If-None-Exist for patient %s", rawPatientID, trimmed)
				}
			}
		}
		condParams.ID = allowedPatientID
		return nil
	}
	if postgres.IsPatientCompartmentResourceType(resType) {
		if condParams.References == nil {
			condParams.References = make(map[string]string)
		}
		if pRef := condParams.References["patient"]; pRef != "" {
			for part := range strings.SplitSeq(pRef, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
					return fmt.Errorf("patient-scoped token (%s) cannot evaluate If-None-Exist for patient %s", rawPatientID, trimmed)
				}
			}
		}
		if sRef := condParams.References["subject"]; sRef != "" {
			for part := range strings.SplitSeq(sRef, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
					return fmt.Errorf("patient-scoped token (%s) cannot evaluate If-None-Exist for subject %s", rawPatientID, trimmed)
				}
			}
		}
		for _, ch := range condParams.Chained {
			if (ch.RefParam == "patient" || ch.RefParam == "subject") && ch.TargetParam == "_id" {
				for part := range strings.SplitSeq(ch.Value, ",") {
					if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
						return fmt.Errorf("patient-scoped token (%s) cannot evaluate If-None-Exist for %s._id=%s", rawPatientID, ch.RefParam, trimmed)
					}
				}
			}
		}
		condParams.References["patient"] = "Patient/" + allowedPatientID
		if condParams.References["subject"] != "" {
			condParams.References["subject"] = "Patient/" + allowedPatientID
		}
		return nil
	}
	return fmt.Errorf("patient-scoped token (%s) cannot evaluate If-None-Exist on non-compartment resource %s", rawPatientID, resType)
}

// handleUpdate handles PUT /{resourceType}/{id} — update or create-on-update a FHIR resource.
// Supports optimistic concurrency control via the If-Match header (returning 412 Precondition Failed on conflict).
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	md, ok := s.protoRegistry[resType]
	if !ok {
		writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
			fmt.Sprintf("resource type %s not registered", resType))
		return
	}

	provSpec, err := parseXProvenanceHeader(r)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	expectedVersion := 0
	if ifMatch := strings.TrimSpace(r.Header.Get("If-Match")); ifMatch != "" {
		v, err := postgres.ParseETagVersion(ifMatch)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid If-Match header %q", ifMatch))
			return
		}
		expectedVersion = v
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB max
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read body")
		return
	}

	resolvedBody, refStatus, refErr := s.resolveConditionalReferencesInJSON(r.Context(), tenant, body, nil)
	if refErr != nil {
		writeOperationOutcome(w, refStatus, conditionalRefOutcomeCode(refStatus), refErr.Error())
		return
	}
	body = resolvedBody

	if s.validator != nil {
		outcome := s.validator.ValidateJSON(resType, body)
		if !outcome.IsValid() {
			outcome.WriteHTTP(w, http.StatusBadRequest)
			return
		}
	}

	if err := s.validateSubscriptionResourceJSON(resType, body); err != nil {
		writeOperationOutcome(w, http.StatusUnprocessableEntity, "security", err.Error())
		return
	}
	body = normalizeSubscriptionOnWrite(resType, body)

	normalizedBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, body)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("invalid FHIR JSON: %v", err))
		return
	}

	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(normalizedBody, msg); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure",
			fmt.Sprintf("invalid FHIR JSON: %v", err))
		return
	}

	bodyID := extractProtoResourceID(msg)
	if bodyID != "" && bodyID != resID {
		writeOperationOutcome(w, http.StatusBadRequest, "invariant",
			fmt.Sprintf("resource id %q in body does not match URL id %q", bodyID, resID))
		return
	}
	if bodyID == "" {
		setProtoResourceID(msg, resID)
	}

	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to encode resource")
		return
	}

	s.syncSearchParameterOnWrite(resType, resID, body)

	var idx *postgres.SearchIndexes
	if extractor, ok := s.extractors[resType]; ok {
		var extractErr error
		idx, extractErr = extractor(tenant, resID, protoBytes)
		if extractErr != nil {
			s.logger.Warn("index extraction failed on update, storing without indexes",
				"type", resType, "id", resID, "error", extractErr)
		}
	}
	if strings.EqualFold(resType, "Provenance") {
		s.enrichProvenancePatientIndex(r.Context(), tenant, resID, idx)
	}
	setAuditPatientID(r.Context(), extractPatientIDFromIndexes(resType, resID, idx))

	// Enforce patient compartment isolation on incoming resource payload
	if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
		if strings.EqualFold(resType, "Patient") || postgres.IsPatientCompartmentResourceType(resType) {
			if !postgres.IndexesBelongToPatientCompartment(resType, resID, idx, cleanID(secCtx.GetPatientId())) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot update %s/%s outside patient compartment",
						secCtx.GetPatientId(), resType, resID))
				return
			}
		}
	}

	rec, err := s.store.WriteResourceWithMeta(r.Context(), postgres.ResourceInput{
		TenantID:        tenant,
		ResType:         resType,
		ResID:           resID,
		ResourceProto:   protoBytes,
		SearchIndexes:   idx,
		ExpectedVersion: expectedVersion,
	})
	if errors.Is(err, postgres.ErrVersionConflict) {
		writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", err.Error())
		return
	}
	if err != nil {
		s.logger.Error("update resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to update resource")
		return
	}

	s.publishCDC(r.Context(), automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     resID,
		ProtoData: protoBytes,
		Op:        "U",
	})
	s.dispatchSubscriptions(r.Context(), tenant, resType, resID, protoBytes, rec.ResVersion, rec.LastUpdated, idx)
	s.recordAutoProvenance(r, w, tenant, resType, resID, rec.ResVersion, idx, provSpec)

	jsonBytes, err := s.protoToJSONWithMeta(resType, protoBytes, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		jsonBytes = body
	}

	w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
	w.Header().Set("Last-Modified", rec.LastUpdated.UTC().Format(http.TimeFormat))
	status := http.StatusOK
	if rec.Created {
		w.Header().Set("Location", fmt.Sprintf("/fhir/r4/%s/%s/%s", tenant, resType, resID))
		status = http.StatusCreated
	}
	writePreferResourceResponse(w, r, status, resType, resID, rec.ResVersion, jsonBytes)
}

// handleDelete handles DELETE /{resourceType}/{id} — soft-delete a FHIR resource.
// Supports optimistic concurrency control via the If-Match header (returning 412 Precondition Failed on conflict).
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	expectedVersion := 0
	if ifMatch := strings.TrimSpace(r.Header.Get("If-Match")); ifMatch != "" {
		v, err := postgres.ParseETagVersion(ifMatch)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid If-Match header %q", ifMatch))
			return
		}
		expectedVersion = v
	}

	rec, err := s.store.DeleteResourceWithExpectedVersion(r.Context(), tenant, resType, resID, expectedVersion)
	if errors.Is(err, postgres.ErrVersionConflict) {
		writeOperationOutcome(w, http.StatusPreconditionFailed, "conflict", err.Error())
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.logger.Error("delete resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to delete resource")
		return
	}
	if rec != nil && rec.ResVersion > 0 {
		s.syncSearchParameterOnDelete(resType, resID)
		w.Header().Set("ETag", postgres.FormatETag(rec.ResVersion))
		s.publishCDC(r.Context(), automq.Message{
			TenantID: tenant,
			ResType:  resType,
			ResID:    resID,
			Op:       "D",
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

func extractProtoResourceID(msg *dynamicpb.Message) string {
	idField := msg.Descriptor().Fields().ByName("id")
	if idField == nil {
		return ""
	}
	idMsg := msg.Get(idField).Message()
	if !idMsg.IsValid() {
		return ""
	}
	valField := idMsg.Descriptor().Fields().ByName("value")
	if valField == nil {
		return ""
	}
	return idMsg.Get(valField).String()
}

func setProtoResourceID(msg *dynamicpb.Message, resID string) {
	idField := msg.Descriptor().Fields().ByName("id")
	if idField == nil {
		return
	}
	idMsg := msg.Mutable(idField).Message()
	if valField := idMsg.Descriptor().Fields().ByName("value"); valField != nil {
		idMsg.Set(valField, protoreflect.ValueOfString(resID))
	}
}

// handleValidate handles POST /{tenant}/{resourceType}/$validate, POST /{tenant}/$validate,
// and GET/POST /{tenant}/{resourceType}/{id}/$validate.
// Validates the target resource against FHIR R4 StructureDefinitions, ValueSets,
// optional $validate mode (create/update/delete), and US Core v6.1.0/v7.0.0 profiles
// without persisting to the database, returning an OperationOutcome.
func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := strings.TrimSpace(r.PathValue("resourceType"))
	resID := strings.TrimSpace(r.PathValue("id"))
	q := r.URL.Query()

	var profiles []string
	for _, p := range q["profile"] {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			profiles = append(profiles, trimmed)
		}
	}
	mode := strings.TrimSpace(q.Get("mode"))

	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB max
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read body")
			return
		}
	}

	targetJSON := bytes.TrimSpace(body)
	if len(targetJSON) > 0 {
		var rootMap map[string]any
		if err := json.Unmarshal(targetJSON, &rootMap); err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("malformed JSON: %v", err))
			return
		}

		// Support FHIR Parameters resource wrapper (e.g. { resourceType: "Parameters", parameter: [{ name: "resource", resource: ... }, { name: "profile", valueUri: ... }, { name: "mode", valueCode: ... }] })
		if rootType, _ := rootMap["resourceType"].(string); rootType == "Parameters" {
			foundResourceParam := false
			if params, ok := rootMap["parameter"].([]any); ok {
				for _, p := range params {
					pMap, ok := p.(map[string]any)
					if !ok {
						continue
					}
					pName, _ := pMap["name"].(string)
					switch pName {
					case "resource":
						if resObj, ok := pMap["resource"].(map[string]any); ok {
							if marshaled, err := json.Marshal(resObj); err == nil {
								targetJSON = marshaled
								foundResourceParam = true
							}
						}
					case "profile":
						for _, k := range []string{"valueUri", "valueCanonical", "valueString", "valueCode"} {
							if v, ok := pMap[k].(string); ok && strings.TrimSpace(v) != "" {
								profiles = append(profiles, strings.TrimSpace(v))
								break
							}
						}
					case "mode":
						if mode == "" {
							for _, k := range []string{"valueCode", "valueString"} {
								if v, ok := pMap[k].(string); ok && strings.TrimSpace(v) != "" {
									mode = strings.TrimSpace(v)
									break
								}
							}
						}
					}
				}
			}
			if !foundResourceParam && resID != "" {
				targetJSON = nil
			}
		}
	}

	// Instance-level $validate (GET/POST /{tenant}/{resourceType}/{id}/$validate without an inline resource body):
	// load the stored resource from Postgres and validate it against the requested profile/mode.
	if len(targetJSON) == 0 && resID != "" && resType != "" {
		if s.store == nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
			return
		}
		rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
		if errors.Is(err, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found",
				fmt.Sprintf("%s/%s not found", resType, resID))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted",
				fmt.Sprintf("%s/%s has been deleted", resType, resID))
			return
		}
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read resource for $validate")
			return
		}
		jb, err := s.protoToJSONWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to serialize resource for $validate")
			return
		}
		targetJSON = jb
	}

	if len(targetJSON) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "empty request body for $validate")
		return
	}

	if s.validator == nil {
		outcome := validation.NewOutcome()
		outcome.WriteHTTP(w, http.StatusOK)
		return
	}

	var outcome *validation.Outcome
	if pv, ok := s.validator.(validation.ProfileValidator); ok {
		outcome = pv.ValidateJSONWithProfiles(resType, targetJSON, profiles, mode)
	} else {
		outcome = s.validator.ValidateJSON(resType, targetJSON)
	}

	if resID != "" {
		var resMap map[string]any
		if json.Unmarshal(targetJSON, &resMap) == nil {
			if bodyID, ok := resMap["id"].(string); ok && strings.TrimSpace(bodyID) != "" && bodyID != resID {
				outcome.AddError(validation.CodeInvariant,
					fmt.Sprintf("Resource id %q does not match instance endpoint id %q", bodyID, resID),
					resType+".id")
			}
		}
	}

	effectiveType := resType
	if effectiveType == "" {
		var resMap map[string]any
		if json.Unmarshal(targetJSON, &resMap) == nil {
			effectiveType, _ = resMap["resourceType"].(string)
		}
	}
	if effectiveType == "Subscription" {
		if err := s.validateSubscriptionResourceJSON("Subscription", targetJSON); err != nil {
			outcome.AddError(validation.CodeValue, err.Error(), "Subscription")
		}
	}

	outcome.WriteHTTP(w, http.StatusOK)
}

// generateID creates a collision-free unique ID for resources and audit events.
func generateID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), idSeq.Add(1))
}

// handleReadAuditEvent handles GET /{tenant}/AuditEvent/{id}.
func (s *Server) handleReadAuditEvent(w http.ResponseWriter, r *http.Request) {
	if s.auditRecorder == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", "AuditEvent recorder not configured")
		return
	}
	tenant := r.PathValue("tenant")
	auditID := r.PathValue("id")

	rec, err := s.auditRecorder.Read(r.Context(), tenant, auditID)
	if err != nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("AuditEvent/%s not found", auditID))
		return
	}

	if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
		if cleanID(rec.AgentPatient) != cleanID(secCtx.GetPatientId()) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot read AuditEvent/%s outside patient compartment",
					secCtx.GetPatientId(), auditID))
			return
		}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(audit.ToFHIRResource(*rec))
}

// handleSearchAuditEvents handles GET /{tenant}/AuditEvent?{params}.
func (s *Server) handleSearchAuditEvents(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	q := r.URL.Query()

	params := postgres.AuditQueryParams{
		TenantID:     tenant,
		Action:       q.Get("action"),
		SubtypeCode:  q.Get("subtype"),
		Outcome:      q.Get("outcome"),
		AgentSubject: q.Get("agent"),
		AgentPatient: q.Get("patient"),
		EntityType:   q.Get("entity-type"),
		EntityID:     q.Get("entity-id"),
	}
	if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
		allowedPatientID := cleanID(secCtx.GetPatientId())
		if params.AgentPatient != "" && cleanID(params.AgentPatient) != allowedPatientID {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot query AuditEvents for patient %s",
					secCtx.GetPatientId(), params.AgentPatient))
			return
		}
		params.AgentPatient = allowedPatientID
	}
	if entityRef := q.Get("entity"); entityRef != "" {
		if parts := strings.SplitN(entityRef, "/", 2); len(parts) == 2 {
			params.EntityType = parts[0]
			params.EntityID = parts[1]
		} else {
			params.EntityID = entityRef
		}
	}
	if v := strings.TrimSpace(q.Get("_count")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _count parameter %q: must be a positive integer", v))
			return
		}
		params.Count = n
	}
	if v := strings.TrimSpace(q.Get("_offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _offset parameter %q: must be a non-negative integer", v))
			return
		}
		params.Offset = n
	}

	var records []postgres.AuditRecord
	var total int
	if s.auditRecorder != nil {
		var err error
		records, total, err = s.auditRecorder.Query(r.Context(), params)
		if err != nil {
			s.logger.Error("query audit events", "error", err, "tenant", tenant)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to query AuditEvents")
			return
		}
	}

	entries := make([]any, 0, len(records))
	for _, rec := range records {
		entries = append(entries, map[string]any{
			"fullUrl":  fmt.Sprintf("AuditEvent/%s", rec.AuditID),
			"resource": audit.ToFHIRResource(rec),
			"search": map[string]any{
				"mode": "match",
			},
		})
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
		"total":        total,
		"entry":        entries,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r == nil || r.Method != http.MethodHead {
		json.NewEncoder(w).Encode(bundle)
	}
}

// handleAuditVerifyIntegrity handles GET/POST /fhir/r4/{tenant}/AuditEvent/$verify-integrity
// per HIPAA 45 CFR §164.312(b) and ONC §170.315(d)(2)/(d)(3) tamper-evident audit log verification.
func (s *Server) handleAuditVerifyIntegrity(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	var records []postgres.AuditRecord
	var total int
	if s.auditRecorder != nil {
		var err error
		records, total, err = s.auditRecorder.Query(r.Context(), postgres.AuditQueryParams{
			TenantID: tenant,
			Count:    1000,
		})
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to query AuditEvents for integrity verification")
			return
		}
	}

	chainHead, verifiedCount, chainEntries, valid := audit.VerifyAuditChain(records)
	params := []map[string]any{
		{
			"name":         "verified",
			"valueBoolean": valid,
		},
		{
			"name":        "algorithm",
			"valueString": "SHA-256-HASH-CHAIN",
		},
		{
			"name":        "regulatoryStandard",
			"valueString": "HIPAA 45 CFR §164.312(b) / ONC 45 CFR §170.315(d)(2)-(d)(3)",
		},
		{
			"name":         "totalRecords",
			"valueInteger": total,
		},
		{
			"name":         "verifiedCount",
			"valueInteger": verifiedCount,
		},
		{
			"name":        "chainHeadDigest",
			"valueString": chainHead,
		},
	}
	if len(chainEntries) > 0 {
		latest := chainEntries[len(chainEntries)-1]
		if latestID, ok := latest["auditId"].(string); ok && latestID != "" {
			params = append(params, map[string]any{
				"name":        "latestAuditId",
				"valueString": latestID,
			})
		}
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "Parameters",
		"parameter":    params,
	})
}

// handlePatientEverything handles GET/POST /{tenant}/{resourceType}/{id}/$everything — assemble full Patient, Encounter, or Group compartment Bundle.
func (s *Server) handlePatientEverything(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := strings.TrimSpace(r.PathValue("resourceType"))
	if resType == "" {
		resType = "Patient"
	}
	if !strings.EqualFold(resType, "Patient") && !strings.EqualFold(resType, "Encounter") && !strings.EqualFold(resType, "Group") {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("$everything is only supported on Patient, Encounter, and Group (got %s)", resType))
		return
	}
	resID := strings.TrimSpace(r.PathValue("id"))
	q := mergedSearchQuery(r)
	if resID == "" {
		for _, k := range []string{"id", "_id", "encounter", "patient", "subject", "group"} {
			if v := strings.TrimSpace(q.Get(k)); v != "" {
				resID = cleanID(v)
				break
			}
		}
	}

	typeRaw := strings.TrimSpace(q.Get("_type"))
	sinceRaw := strings.TrimSpace(q.Get("_since"))
	untilRaw := strings.TrimSpace(q.Get("_until"))
	startRaw := strings.TrimSpace(q.Get("start"))
	endRaw := strings.TrimSpace(q.Get("end"))
	countRaw := strings.TrimSpace(q.Get("_count"))
	offsetRaw := strings.TrimSpace(q.Get("_offset"))
	var elements []string
	for _, el := range q["_elements"] {
		for part := range strings.SplitSeq(el, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				elements = append(elements, trimmed)
			}
		}
	}
	var typeFilters []string
	for _, tf := range q["_typeFilter"] {
		for part := range strings.SplitSeq(tf, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				typeFilters = append(typeFilters, trimmed)
			}
		}
	}

	if r.Method == http.MethodPost && r.ContentLength != 0 {
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		if !strings.Contains(ct, "application/x-www-form-urlencoded") {
			body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
			if err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read request body")
				return
			}
			if strings.TrimSpace(string(body)) != "" {
				var raw map[string]any
				if err := json.Unmarshal(body, &raw); err != nil {
					writeOperationOutcome(w, http.StatusBadRequest, "invalid",
						fmt.Sprintf("invalid JSON in $everything request: %v", err))
					return
				}
				if rt, _ := raw["resourceType"].(string); rt != "" && rt != "Parameters" {
					writeOperationOutcome(w, http.StatusBadRequest, "invalid",
						fmt.Sprintf("$everything POST body must be a Parameters resource (got %q)", rt))
					return
				}
				if paramsArr, ok := raw["parameter"].([]any); ok {
					for _, p := range paramsArr {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						val := ""
						for _, k := range []string{"valueString", "valueCode", "valueUri", "valueInstant", "valueDateTime", "valueDate"} {
							if sVal, ok := pm[k].(string); ok && strings.TrimSpace(sVal) != "" {
								val = strings.TrimSpace(sVal)
								break
							}
						}
						if val == "" {
							for _, k := range []string{"valueInteger", "valuePositiveInt", "valueUnsignedInt"} {
								if fVal, ok := pm[k].(float64); ok {
									val = strconv.Itoa(int(fVal))
									break
								}
							}
						}
						switch name {
						case "id", "_id", "encounter", "patient", "subject", "group":
							if val == "" {
								if sVal, ok := pm["valueId"].(string); ok && strings.TrimSpace(sVal) != "" {
									val = strings.TrimSpace(sVal)
								} else if refMap, ok := pm["valueReference"].(map[string]any); ok {
									if refStr, _ := refMap["reference"].(string); strings.TrimSpace(refStr) != "" {
										val = strings.TrimSpace(refStr)
									}
								}
							}
							if val != "" && resID == "" {
								resID = cleanID(val)
							}
						case "_type":
							if val != "" && typeRaw == "" {
								typeRaw = val
							}
						case "_since":
							if val != "" && sinceRaw == "" {
								sinceRaw = val
							}
						case "_until":
							if val != "" && untilRaw == "" {
								untilRaw = val
							}
						case "start":
							if val != "" && startRaw == "" {
								startRaw = val
							}
						case "end":
							if val != "" && endRaw == "" {
								endRaw = val
							}
						case "_count":
							if val != "" && countRaw == "" {
								countRaw = val
							}
						case "_offset":
							if val != "" && offsetRaw == "" {
								offsetRaw = val
							}
						case "_elements":
							for part := range strings.SplitSeq(val, ",") {
								if trimmed := strings.TrimSpace(part); trimmed != "" {
									elements = append(elements, trimmed)
								}
							}
						case "_typeFilter":
							for part := range strings.SplitSeq(val, ",") {
								if trimmed := strings.TrimSpace(part); trimmed != "" {
									typeFilters = append(typeFilters, trimmed)
								}
							}
						}
					}
				}
			}
		}
	}

	var parsedTypeFilters []exportTypeFilter
	for _, tf := range typeFilters {
		parts := strings.SplitN(tf, "?", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _typeFilter %q: must be formatted as ResourceType?param=value", tf))
			return
		}
		rt := strings.TrimSpace(parts[0])
		if _, ok := s.protoRegistry[rt]; !ok {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("unsupported resource type %q in _typeFilter %q", rt, tf))
			return
		}
		qVals, err := url.ParseQuery(strings.TrimSpace(parts[1]))
		if err != nil || len(qVals) == 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid query in _typeFilter %q", tf))
			return
		}
		if err := postgres.ValidateSearchQuery(qVals); err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _typeFilter %q: %v", tf, err))
			return
		}
		sp := postgres.BuildSearchParamsFromQuery(tenant, rt, qVals)
		sp.Count = 10000
		parsedTypeFilters = append(parsedTypeFilters, exportTypeFilter{
			resType: rt,
			params:  sp,
		})
	}

	var resourceTypes []string
	if typeRaw != "" {
		for part := range strings.SplitSeq(typeRaw, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				if _, ok := s.protoRegistry[trimmed]; !ok {
					writeOperationOutcome(w, http.StatusBadRequest, "invalid",
						fmt.Sprintf("unknown or unsupported resource type %q in _type parameter", trimmed))
					return
				}
				resourceTypes = append(resourceTypes, trimmed)
			}
		}
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if resID == "" && hasSecCtx && secCtx != nil && strings.EqualFold(resType, "Patient") && secCtx.GetPatientId() != "" {
		resID = cleanID(secCtx.GetPatientId())
	}
	if resID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			fmt.Sprintf("%s/$everything requires a resource {id} in the path or an 'id' parameter", resType))
		return
	}
	if hasSecCtx {
		if strings.EqualFold(resType, "Group") {
			if auth.IsPatientRestricted(secCtx) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"patient-scoped token cannot invoke multi-patient Group/$everything; use /Patient/{id}/$everything")
				return
			}
			if !auth.Allows(secCtx, "Group", auth.ActionRead) && !auth.Allows(secCtx, "Group", auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"insufficient scope for Group/$everything")
				return
			}
		} else {
			if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for %s/$everything", resType))
				return
			}
			if auth.IsPatientRestricted(secCtx) && strings.EqualFold(resType, "Patient") && cleanID(secCtx.GetPatientId()) != cleanID(resID) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot invoke Patient/$everything for Patient/%s",
						secCtx.GetPatientId(), resID))
				return
			}
		}
		for _, rt := range resourceTypes {
			if !auth.Allows(secCtx, rt, auth.ActionRead) && !auth.Allows(secCtx, rt, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for $everything _type %s", rt))
				return
			}
		}
	}

	for _, el := range elements {
		if idx := strings.IndexByte(el, '.'); idx != -1 {
			rt := strings.TrimSpace(el[:idx])
			field := strings.TrimSpace(el[idx+1:])
			if rt == "" || field == "" || strings.Contains(field, ".") {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("invalid _elements token %q", el))
				return
			}
			if _, ok := s.protoRegistry[rt]; !ok {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("unsupported resource type %q in _elements token %q", rt, el))
				return
			}
		}
	}

	var sinceTime *time.Time
	if sinceRaw != "" {
		dOp, err := postgres.ParseDateOp(sinceRaw)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _since parameter %q: %v", sinceRaw, err))
			return
		}
		t := dOp.Value
		sinceTime = &t
	}

	var untilTime *time.Time
	if untilRaw != "" {
		dOp, err := postgres.ParseDateOp(untilRaw)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _until parameter %q: %v", untilRaw, err))
			return
		}
		t := dOp.Value
		if dOp.High.After(dOp.Value) {
			t = dOp.High.Add(-time.Microsecond)
		}
		untilTime = &t
	}
	if sinceTime != nil && untilTime != nil && untilTime.Before(*sinceTime) {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid",
			fmt.Sprintf("_until (%s) must not be earlier than _since (%s)", untilRaw, sinceRaw))
		return
	}

	var startTime *time.Time
	if startRaw != "" {
		dOp, err := postgres.ParseDateOp(startRaw)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid start parameter %q: %v", startRaw, err))
			return
		}
		t := dOp.Value
		startTime = &t
	}

	var endTime *time.Time
	if endRaw != "" {
		dOp, err := postgres.ParseDateOp(endRaw)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid end parameter %q: %v", endRaw, err))
			return
		}
		t := dOp.Value
		if dOp.High.After(dOp.Value) {
			t = dOp.High.Add(-time.Microsecond)
		}
		endTime = &t
	}
	if startTime != nil && endTime != nil && endTime.Before(*startTime) {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid",
			fmt.Sprintf("end (%s) must not be earlier than start (%s)", endRaw, startRaw))
		return
	}

	count := 100
	if countRaw != "" {
		n, err := strconv.Atoi(countRaw)
		if err != nil || n <= 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _count parameter %q: must be a positive integer", countRaw))
			return
		}
		count = n
	}
	offset := 0
	if offsetRaw != "" {
		n, err := strconv.Atoi(offsetRaw)
		if err != nil || n < 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _offset parameter %q: must be a non-negative integer", offsetRaw))
			return
		}
		offset = n
	}

	var sr *postgres.SearchResults
	var err error
	targetLabel := "Patient"
	if strings.EqualFold(resType, "Group") {
		targetLabel = "Group"
		groupPatIDs, isBuiltinAll, gErr := s.resolveExportGroupCohort(r.Context(), tenant, resID, 0, make(map[string]bool))
		if errors.Is(gErr, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found",
				fmt.Sprintf("Group/%s not found", resID))
			return
		}
		if errors.Is(gErr, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted",
				fmt.Sprintf("Group/%s has been deleted", resID))
			return
		}
		if gErr != nil {
			s.logger.Error("Group/$everything cohort resolution failed", "error", gErr, "group", resID)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to assemble Group/$everything")
			return
		}
		sr, err = s.store.GroupEverything(r.Context(), postgres.GroupEverythingParams{
			TenantID:      tenant,
			GroupID:       resID,
			PatientIDs:    groupPatIDs,
			AllPatients:   isBuiltinAll,
			EmptyCohort:   !isBuiltinAll && len(groupPatIDs) == 0,
			ResourceTypes: resourceTypes,
			Since:         sinceTime,
			Until:         untilTime,
			Start:         startTime,
			End:           endTime,
			Count:         count,
			Offset:        offset,
		})
	} else if strings.EqualFold(resType, "Encounter") {
		targetLabel = "Encounter"
		sr, err = s.store.EncounterEverything(r.Context(), postgres.EncounterEverythingParams{
			TenantID:      tenant,
			EncounterID:   resID,
			ResourceTypes: resourceTypes,
			Since:         sinceTime,
			Until:         untilTime,
			Start:         startTime,
			End:           endTime,
			Count:         count,
			Offset:        offset,
		})
	} else {
		sr, err = s.store.PatientEverything(r.Context(), postgres.PatientEverythingParams{
			TenantID:      tenant,
			PatientID:     resID,
			ResourceTypes: resourceTypes,
			Since:         sinceTime,
			Until:         untilTime,
			Start:         startTime,
			End:           endTime,
			Count:         count,
			Offset:        offset,
		})
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("%s/%s not found", targetLabel, resID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted",
			fmt.Sprintf("%s/%s has been deleted", targetLabel, resID))
		return
	}
	if err != nil {
		s.logger.Error("$everything failed", "error", err, "type", targetLabel, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception",
			fmt.Sprintf("failed to assemble %s/$everything", targetLabel))
		return
	}

	if len(parsedTypeFilters) > 0 {
		typeFilterAllowed, tfErr := s.resolveExportTypeFilterAllowedIDs(r.Context(), parsedTypeFilters)
		if tfErr != nil {
			s.logger.Error("$everything _typeFilter evaluation failed", "error", tfErr, "tenant", tenant)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to evaluate _typeFilter criteria")
			return
		}
		removed := 0
		if len(sr.Matches) > 0 {
			filteredMatches := make([]postgres.SearchResult, 0, len(sr.Matches))
			for _, m := range sr.Matches {
				if matchesExportTypeFilter(typeFilterAllowed, m.ResType, m.ResID) {
					filteredMatches = append(filteredMatches, m)
				} else {
					removed++
				}
			}
			sr.Matches = filteredMatches
		}
		if len(sr.Includes) > 0 {
			filteredIncludes := make([]postgres.SearchResult, 0, len(sr.Includes))
			for _, inc := range sr.Includes {
				if matchesExportTypeFilter(typeFilterAllowed, inc.ResType, inc.ResID) {
					filteredIncludes = append(filteredIncludes, inc)
				} else {
					removed++
				}
			}
			sr.Includes = filteredIncludes
		}
		if removed > 0 {
			sr.Total = max(len(sr.Matches)+len(sr.Includes), sr.Total-removed)
		}
	}

	if hasSecCtx && len(sr.Includes) > 0 {
		beforeInc := len(sr.Includes)
		sr.Includes = s.filterAuthorizedIncludes(r, tenant, secCtx, sr.Includes)
		if removed := beforeInc - len(sr.Includes); removed > 0 {
			sr.Total = max(len(sr.Matches)+len(sr.Includes), sr.Total-removed)
		}
	}

	s.writeSearchSetBundleWithOpts(w, r, sr, postgres.SearchParams{
		Count:    count,
		Offset:   offset,
		ResType:  "$everything",
		Elements: elements,
	})
}

// mergedSearchQuery returns URL query parameters combined with application/x-www-form-urlencoded
// body parameters when r.Method == POST (FHIR R4 §3.1.1.3 POST _search).
func mergedSearchQuery(r *http.Request) url.Values {
	q := make(url.Values)
	for k, vals := range r.URL.Query() {
		q[k] = append(q[k], vals...)
	}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		for k, vals := range r.PostForm {
			q[k] = append(q[k], vals...)
		}
	}
	return q
}

func (s *Server) resourceSatisfiesScopeConstraints(tenant, resType, resID string, protoBytes []byte, constraints url.Values) bool {
	if len(constraints) == 0 {
		return true
	}
	extractor, ok := s.extractors[resType]
	if !ok || len(protoBytes) == 0 {
		return true
	}
	idx, err := extractor(tenant, resID, protoBytes)
	if err != nil || idx == nil {
		return false
	}
	for paramKey, allowedVals := range constraints {
		if len(allowedVals) == 0 {
			continue
		}
		allowedCSV := allowedVals[0]
		matched := false
		for _, tok := range idx.Tokens {
			if !strings.EqualFold(tok.SpName, paramKey) {
				continue
			}
			cand := tok.SpValue
			if tok.SpSystem != "" {
				cand = tok.SpSystem + "|" + tok.SpValue
			}
			if auth.TokenValueMatchesConstraint(cand, allowedCSV) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// handleCompartmentSearch handles GET/HEAD /{tenant}/{compartmentType}/{compartmentId}/{resourceType}
// and POST /{tenant}/{compartmentType}/{compartmentId}/{resourceType}/_search
// across all 5 FHIR R4 compartments (Patient, Encounter, Practitioner, RelatedPerson, Device),
// including wildcard multi-type compartment search (/{compartmentType}/{compartmentId}/*).
func (s *Server) handleCompartmentSearch(w http.ResponseWriter, r *http.Request) {
	compType := postgres.CanonicalCompartmentType(r.PathValue("compartmentType"))
	if compType == "" {
		compType = "Patient"
	}
	compID := cleanID(r.PathValue("compartmentId"))
	if compID == "" {
		compID = cleanID(r.PathValue("patientId"))
	}
	resType := r.PathValue("resourceType")

	if resType == "AuditEvent" && compType == "Patient" {
		q := r.URL.Query()
		q.Set("patient", compID)
		r.URL.RawQuery = q.Encode()
		s.handleSearchAuditEvents(w, r)
		return
	}

	if resType == "*" {
		r.SetPathValue("compartmentType", compType)
		r.SetPathValue("compartmentId", compID)
		s.handleSystemSearch(w, r)
		return
	}

	if _, ok := s.protoRegistry[resType]; !ok {
		writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
			fmt.Sprintf("resource type %s not registered", resType))
		return
	}

	if compType == "Patient" {
		q := r.URL.Query()
		if strings.EqualFold(resType, "Patient") {
			q.Set("_id", compID)
		} else {
			q.Set("patient", "Patient/"+compID)
		}
		r.URL.RawQuery = q.Encode()
		s.handleSearch(w, r)
		return
	}

	r.SetPathValue("compartmentType", compType)
	r.SetPathValue("compartmentId", compID)
	s.handleSearch(w, r)
}

// handleSearch handles GET /{resourceType}?{params} and POST /{resourceType}/_search — search resources.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")

	q := mergedSearchQuery(r)
	ignoredParams, ok := evaluateSearchPreferHandling(w, r, q)
	if !ok {
		return
	}

	if err := postgres.ValidateSearchQuery(q); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	params := postgres.BuildSearchParamsFromQuery(tenant, resType, q)
	if compType := postgres.CanonicalCompartmentType(r.PathValue("compartmentType")); compType != "" && compType != "Patient" {
		params.CompartmentType = compType
		params.CompartmentID = cleanID(r.PathValue("compartmentId"))
	}
	if err := s.expandTerminologySearchParams(r.Context(), tenant, &params); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx {
		// Enforce SMART scopes on explicit _include target types, _revinclude source types, and _has source types
		for _, inc := range params.Includes {
			if inc.TargetType != "" && inc.TargetType != "*" && !auth.Allows(secCtx, inc.TargetType, auth.ActionRead) && !auth.Allows(secCtx, inc.TargetType, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for _include target %s", inc.TargetType))
				return
			}
		}
		for _, rev := range params.RevIncludes {
			if rev.SourceType != "" && rev.SourceType != "*" && !auth.Allows(secCtx, rev.SourceType, auth.ActionRead) && !auth.Allows(secCtx, rev.SourceType, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for _revinclude source %s", rev.SourceType))
				return
			}
		}
		for _, hp := range params.Has {
			for cur := &hp; cur != nil; cur = cur.Child {
				if cur.SourceType != "" && !auth.Allows(secCtx, cur.SourceType, auth.ActionRead) && !auth.Allows(secCtx, cur.SourceType, auth.ActionSearch) {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("insufficient scope for _has source %s", cur.SourceType))
					return
				}
			}
		}
		for _, ch := range params.Chained {
			hops := ch.Hops
			if len(hops) == 0 && ch.TargetType != "" {
				hops = []postgres.ChainedHop{{RefParam: ch.RefParam, TargetType: ch.TargetType}}
			}
			for _, hop := range hops {
				if hop.TargetType != "" && !auth.Allows(secCtx, hop.TargetType, auth.ActionRead) && !auth.Allows(secCtx, hop.TargetType, auth.ActionSearch) {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("insufficient scope for chained search target %s", hop.TargetType))
					return
				}
			}
		}
		for _, f := range params.Filters {
			if unauthType := checkFilterExprChainedScopes(secCtx, f); unauthType != "" {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for chained search target %s", unauthType))
				return
			}
		}
		if len(params.List) > 0 && !auth.Allows(secCtx, "List", auth.ActionRead) && !auth.Allows(secCtx, "List", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"insufficient scope for _list cohort search (requires List read/search scope)")
			return
		}

		// Enforce SMART v2 granular sub-resource scope constraints (e.g. patient/Observation.rs?category=laboratory)
		if constraints, constrained := auth.ScopeQueryConstraints(secCtx, resType, auth.ActionSearch); constrained {
			if params.Tokens == nil {
				params.Tokens = make(map[string]string)
			}
			if params.TokenFilters == nil {
				params.TokenFilters = make(map[string][]string)
			}
			for paramKey, allowedVals := range constraints {
				if len(allowedVals) == 0 {
					continue
				}
				allowedCSV := allowedVals[0]
				if reqVals, exists := q[paramKey]; exists && len(reqVals) > 0 {
					for _, rawReq := range reqVals {
						for reqPart := range strings.SplitSeq(rawReq, ",") {
							if trimmed := strings.TrimSpace(reqPart); trimmed != "" && !auth.TokenValueMatchesConstraint(trimmed, allowedCSV) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("insufficient granular SMART v2 scope for %s=%s (allowed: %s)", paramKey, trimmed, allowedCSV))
								return
							}
						}
					}
				} else {
					params.Tokens[paramKey] = allowedCSV
					params.TokenFilters[paramKey] = append(params.TokenFilters[paramKey], allowedCSV)
				}
			}
		}

		// Enforce patient compartment scoping on searches when using a patient-restricted SMART token
		if auth.IsPatientRestricted(secCtx) {
			allowedPatientID := cleanID(secCtx.GetPatientId())
			if strings.EqualFold(resType, "Patient") {
				if params.ID != "" {
					for part := range strings.SplitSeq(params.ID, ",") {
						if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("patient-scoped token (%s) cannot search patient %s",
									secCtx.GetPatientId(), trimmed))
							return
						}
					}
				}
				params.ID = allowedPatientID
			} else if postgres.IsPatientCompartmentResourceType(resType) {
				if params.References == nil {
					params.References = make(map[string]string)
				}
				if params.ReferenceFilters == nil {
					params.ReferenceFilters = make(map[string][]string)
				}
				params.References["patient"] = "Patient/" + allowedPatientID
				params.ReferenceFilters["patient"] = []string{"Patient/" + allowedPatientID}
				if params.References["subject"] != "" {
					params.References["subject"] = "Patient/" + allowedPatientID
					params.ReferenceFilters["subject"] = []string{"Patient/" + allowedPatientID}
				}
			}
		}
	}

	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}

	sr, err := s.store.Search(r.Context(), params)
	if err != nil {
		s.logger.Error("search", "error", err, "type", resType)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "search error")
		return
	}

	if hasSecCtx && len(sr.Includes) > 0 {
		sr.Includes = s.filterAuthorizedIncludes(r, tenant, secCtx, sr.Includes)
	}

	s.writeSearchSetBundleWithOpts(w, r, sr, params, ignoredParams...)
}

// handleSystemSearch handles GET /fhir/r4/{tenant}?_type=Patient,Observation&... and POST /fhir/r4/{tenant}/_search
// per FHIR R4 §3.1.1.4.2 (system-level multi-type search).
func (s *Server) handleSystemSearch(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	q := mergedSearchQuery(r)
	ignoredParams, ok := evaluateSearchPreferHandling(w, r, q)
	if !ok {
		return
	}

	if err := postgres.ValidateSearchQuery(q); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	params := postgres.BuildSearchParamsFromQuery(tenant, "", q)
	if compType := postgres.CanonicalCompartmentType(r.PathValue("compartmentType")); compType != "" {
		params.CompartmentType = compType
		params.CompartmentID = cleanID(r.PathValue("compartmentId"))
	}
	if len(params.ResTypes) > 0 {
		seen := make(map[string]bool, len(params.ResTypes))
		deduped := make([]string, 0, len(params.ResTypes))
		for _, rt := range params.ResTypes {
			if _, ok := s.protoRegistry[rt]; !ok && rt != "AuditEvent" {
				writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
					fmt.Sprintf("unknown or unsupported resource type %q in _type parameter", rt))
				return
			}
			if !seen[rt] {
				seen[rt] = true
				deduped = append(deduped, rt)
			}
		}
		params.ResTypes = deduped
	}

	if err := s.expandTerminologySearchParams(r.Context(), tenant, &params); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx {
		if len(params.ResTypes) > 0 {
			for _, rt := range params.ResTypes {
				if !auth.Allows(secCtx, rt, auth.ActionSearch) && !auth.Allows(secCtx, rt, auth.ActionRead) {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("insufficient scope for system search on %s", rt))
					return
				}
				if auth.IsPatientRestricted(secCtx) && !strings.EqualFold(rt, "Patient") && !postgres.IsPatientCompartmentResourceType(rt) {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("patient-scoped token (%s) cannot search non-compartment resource %s",
							secCtx.GetPatientId(), rt))
					return
				}
			}
		} else if !auth.Allows(secCtx, "*", auth.ActionSearch) && !auth.Allows(secCtx, "*", auth.ActionRead) {
			var allowedTypes []string
			for _, rt := range fhirutil.AllR4ResourceTypes() {
				if _, ok := s.protoRegistry[rt]; !ok {
					continue
				}
				if auth.IsPatientRestricted(secCtx) && !strings.EqualFold(rt, "Patient") && !postgres.IsPatientCompartmentResourceType(rt) {
					continue
				}
				if auth.Allows(secCtx, rt, auth.ActionSearch) || auth.Allows(secCtx, rt, auth.ActionRead) {
					allowedTypes = append(allowedTypes, rt)
				}
			}
			if len(allowedTypes) == 0 {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for system search")
				return
			}
			params.ResTypes = allowedTypes
		}

		for _, inc := range params.Includes {
			if inc.TargetType != "" && inc.TargetType != "*" && !auth.Allows(secCtx, inc.TargetType, auth.ActionRead) && !auth.Allows(secCtx, inc.TargetType, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for _include target %s", inc.TargetType))
				return
			}
		}
		for _, rev := range params.RevIncludes {
			if rev.SourceType != "" && rev.SourceType != "*" && !auth.Allows(secCtx, rev.SourceType, auth.ActionRead) && !auth.Allows(secCtx, rev.SourceType, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for _revinclude source %s", rev.SourceType))
				return
			}
		}
		for _, hp := range params.Has {
			for cur := &hp; cur != nil; cur = cur.Child {
				if cur.SourceType != "" && !auth.Allows(secCtx, cur.SourceType, auth.ActionRead) && !auth.Allows(secCtx, cur.SourceType, auth.ActionSearch) {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("insufficient scope for _has source %s", cur.SourceType))
					return
				}
			}
		}
		for _, ch := range params.Chained {
			for _, hop := range ch.Hops {
				if hop.TargetType != "" && !auth.Allows(secCtx, hop.TargetType, auth.ActionRead) && !auth.Allows(secCtx, hop.TargetType, auth.ActionSearch) {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("insufficient scope for chained search target %s", hop.TargetType))
					return
				}
			}
		}
		for _, f := range params.Filters {
			if unauthType := checkFilterExprChainedScopes(secCtx, f); unauthType != "" {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for chained search target %s", unauthType))
				return
			}
		}
		if len(params.List) > 0 && !auth.Allows(secCtx, "List", auth.ActionRead) && !auth.Allows(secCtx, "List", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"insufficient scope for _list cohort search (requires List read/search scope)")
			return
		}

		if auth.IsPatientRestricted(secCtx) {
			allowedPatientID := cleanID(secCtx.GetPatientId())
			for _, refKey := range []string{"patient", "subject"} {
				for _, rawRef := range params.ReferenceFilters[refKey] {
					for part := range strings.SplitSeq(rawRef, ",") {
						if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("patient-scoped token (%s) cannot search %s %s",
									secCtx.GetPatientId(), refKey, trimmed))
							return
						}
					}
				}
			}
		}
	}

	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}

	sr, err := s.store.Search(r.Context(), params)
	if err != nil {
		s.logger.Error("system search", "error", err, "types", params.ResTypes)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "search error")
		return
	}

	if hasSecCtx {
		if len(sr.Matches) > 0 {
			before := len(sr.Matches)
			sr.Matches = s.filterAuthorizedIncludes(r, tenant, secCtx, sr.Matches)
			if removed := before - len(sr.Matches); removed > 0 {
				sr.Total = max(len(sr.Matches), sr.Total-removed)
			}
		}
		if len(sr.Includes) > 0 {
			sr.Includes = s.filterAuthorizedIncludes(r, tenant, secCtx, sr.Includes)
		}
	}

	s.writeSearchSetBundleWithOpts(w, r, sr, params, ignoredParams...)
}

func evaluateSearchPreferHandling(w http.ResponseWriter, r *http.Request, q url.Values) ([]string, bool) {
	if r == nil {
		return nil, true
	}
	var handling string
	for _, rawPrefer := range r.Header.Values("Prefer") {
		for part := range strings.SplitSeq(rawPrefer, ",") {
			for sub := range strings.SplitSeq(part, ";") {
				kv := strings.SplitN(strings.TrimSpace(sub), "=", 2)
				if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "handling") {
					handling = strings.ToLower(strings.Trim(strings.TrimSpace(kv[1]), `"`))
				}
			}
		}
	}
	if handling != "strict" && handling != "lenient" {
		return nil, true
	}
	var unknownKeys []string
	for k := range q {
		if !postgres.IsKnownSearchParam(k) {
			unknownKeys = append(unknownKeys, k)
		}
	}
	sort.Strings(unknownKeys)
	if handling == "strict" {
		w.Header().Set("Preference-Applied", "handling=strict")
		if len(unknownKeys) > 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
				fmt.Sprintf("unknown or unsupported search parameter(s): %s", strings.Join(unknownKeys, ", ")))
			return nil, false
		}
		return nil, true
	}
	w.Header().Set("Preference-Applied", "handling=lenient")
	for _, k := range unknownKeys {
		delete(q, k)
	}
	return unknownKeys, true
}

func (s *Server) resourceAllowedByGranularScope(secCtx *auth.SecurityContext, tenant, resType, resID string, protoBytes []byte) bool {
	if secCtx == nil {
		return true
	}
	if auth.Allows(secCtx, resType, auth.ActionRead) {
		constraints, constrained := auth.ScopeQueryConstraints(secCtx, resType, auth.ActionRead)
		if !constrained || s.resourceSatisfiesScopeConstraints(tenant, resType, resID, protoBytes, constraints) {
			return true
		}
	}
	if auth.Allows(secCtx, resType, auth.ActionSearch) {
		constraints, constrained := auth.ScopeQueryConstraints(secCtx, resType, auth.ActionSearch)
		if !constrained || s.resourceSatisfiesScopeConstraints(tenant, resType, resID, protoBytes, constraints) {
			return true
		}
	}
	return false
}

func (s *Server) filterAuthorizedIncludes(r *http.Request, tenant string, secCtx *auth.SecurityContext, includes []postgres.SearchResult) []postgres.SearchResult {
	if secCtx == nil || len(includes) == 0 {
		return includes
	}
	patientRestricted := auth.IsPatientRestricted(secCtx)
	allowedPatientID := cleanID(secCtx.GetPatientId())
	filtered := make([]postgres.SearchResult, 0, len(includes))

	for _, inc := range includes {
		if !s.resourceAllowedByGranularScope(secCtx, tenant, inc.ResType, inc.ResID, inc.ResourceProto) {
			continue
		}
		if patientRestricted {
			if strings.EqualFold(inc.ResType, "Patient") {
				if cleanID(inc.ResID) != allowedPatientID {
					continue
				}
			} else if postgres.IsPatientCompartmentResourceType(inc.ResType) {
				if extractor, ok := s.extractors[inc.ResType]; ok && len(inc.ResourceProto) > 0 {
					idx, err := extractor(tenant, inc.ResID, inc.ResourceProto)
					if err != nil || !postgres.IndexesBelongToPatientCompartment(inc.ResType, inc.ResID, idx, allowedPatientID) {
						if inc.ResType == "Provenance" && s.store != nil {
							belongs, bErr := s.store.BelongsToPatientCompartment(r.Context(), tenant, inc.ResType, inc.ResID, allowedPatientID)
							if bErr != nil || !belongs {
								continue
							}
						} else {
							continue
						}
					}
				} else if s.store != nil {
					belongs, err := s.store.BelongsToPatientCompartment(r.Context(), tenant, inc.ResType, inc.ResID, allowedPatientID)
					if err != nil || !belongs {
						continue
					}
				}
			}
		}
		filtered = append(filtered, inc)
	}
	return filtered
}

func (s *Server) writeSearchSetBundleWithOpts(w http.ResponseWriter, r *http.Request, sr *postgres.SearchResults, params postgres.SearchParams, ignoredParams ...string) {
	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
	}

	if params.TotalMode != "none" || params.Summary == "count" {
		bundle["total"] = sr.Total
	}

	if r != nil {
		bundle["link"] = s.buildSearchBundleLinks(r, sr, params)
	}

	if params.Summary != "count" {
		entries := make([]any, 0, len(sr.Matches)+len(sr.Includes)+1)
		needsProjection := (params.Summary != "" && params.Summary != "false") || len(params.Elements) > 0

		// 1. Primary search matches
		for _, res := range sr.Matches {
			jsonBytes, err := s.protoToJSONWithMeta(res.ResType, res.ResourceProto, res.ResVersion, res.LastUpdated)
			if err != nil {
				s.logger.Error("proto to json in search", "error", err, "type", res.ResType, "id", res.ResID)
				continue
			}

			var resource map[string]any
			if err := json.Unmarshal(jsonBytes, &resource); err != nil {
				continue
			}
			if needsProjection {
				resource = applySummaryOrElements(resource, params.Summary, params.Elements)
			}

			entries = append(entries, map[string]any{
				"fullUrl":  fmt.Sprintf("%s/%s", res.ResType, res.ResID),
				"resource": resource,
				"search": map[string]any{
					"mode": "match",
				},
			})
		}

		// 2. Included resources (_include, _revinclude, and $everything compartment resources)
		for _, inc := range sr.Includes {
			jsonBytes, err := s.protoToJSONWithMeta(inc.ResType, inc.ResourceProto, inc.ResVersion, inc.LastUpdated)
			if err != nil {
				s.logger.Error("proto to json for included resource", "error", err, "type", inc.ResType, "id", inc.ResID)
				continue
			}

			var resource map[string]any
			if err := json.Unmarshal(jsonBytes, &resource); err != nil {
				continue
			}
			if needsProjection && params.ResType == "$everything" {
				resource = applySummaryOrElements(resource, params.Summary, params.Elements)
			}

			entries = append(entries, map[string]any{
				"fullUrl":  fmt.Sprintf("%s/%s", inc.ResType, inc.ResID),
				"resource": resource,
				"search": map[string]any{
					"mode": "include",
				},
			})
		}

		// 3. Optional search outcome warning entry (e.g. Prefer: handling=lenient ignored parameters)
		if len(ignoredParams) > 0 {
			entries = append(entries, map[string]any{
				"resource": map[string]any{
					"resourceType": "OperationOutcome",
					"issue": []any{
						map[string]any{
							"severity":    "warning",
							"code":        "not-supported",
							"diagnostics": fmt.Sprintf("ignored unsupported search parameter(s): %s", strings.Join(ignoredParams, ", ")),
						},
					},
				},
				"search": map[string]any{
					"mode": "outcome",
				},
			})
		}

		bundle["entry"] = entries
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r == nil || r.Method != http.MethodHead {
		json.NewEncoder(w).Encode(bundle)
	}
}

func (s *Server) buildSearchBundleLinks(r *http.Request, sr *postgres.SearchResults, params postgres.SearchParams) []map[string]string {
	basePath := strings.TrimSuffix(r.URL.Path, "/_search")
	baseURL := requestBaseURL(r) + basePath
	q := mergedSearchQuery(r)

	makeURL := func(vals url.Values) string {
		enc := vals.Encode()
		if enc == "" {
			return baseURL
		}
		return baseURL + "?" + enc
	}

	links := []map[string]string{
		{
			"relation": "self",
			"url":      makeURL(q),
		},
	}

	if params.Summary == "count" {
		return links
	}

	count := params.Count
	if count <= 0 {
		count = 20
	}
	offset := max(params.Offset, 0)

	pageItems := len(sr.Matches)
	if params.ResType == "$everything" {
		pageItems = len(sr.Matches) + len(sr.Includes)
	}

	if pageItems > 0 && (params.TotalMode == "none" && pageItems == count || offset+pageItems < sr.Total) {
		nextQ := cloneURLValues(q)
		nextQ.Set("_count", strconv.Itoa(count))
		nextQ.Set("_offset", strconv.Itoa(offset+count))
		links = append(links, map[string]string{
			"relation": "next",
			"url":      makeURL(nextQ),
		})
	}

	if offset > 0 {
		prevQ := cloneURLValues(q)
		prevQ.Set("_count", strconv.Itoa(count))
		prevQ.Set("_offset", strconv.Itoa(max(0, offset-count)))
		links = append(links, map[string]string{
			"relation": "previous",
			"url":      makeURL(prevQ),
		})
	}

	return links
}

func cloneURLValues(in url.Values) url.Values {
	out := make(url.Values, len(in))
	for k, v := range in {
		out[k] = append([]string(nil), v...)
	}
	return out
}

var fhirSummaryElements = map[string]bool{
	"resourceType":              true,
	"id":                        true,
	"meta":                      true,
	"identifier":                true,
	"active":                    true,
	"name":                      true,
	"telecom":                   true,
	"gender":                    true,
	"birthDate":                 true,
	"deceasedBoolean":           true,
	"deceasedDateTime":          true,
	"address":                   true,
	"managingOrganization":      true,
	"link":                      true,
	"status":                    true,
	"category":                  true,
	"code":                      true,
	"subject":                   true,
	"patient":                   true,
	"encounter":                 true,
	"effectiveDateTime":         true,
	"effectivePeriod":           true,
	"effectiveInstant":          true,
	"issued":                    true,
	"performer":                 true,
	"valueQuantity":             true,
	"valueCodeableConcept":      true,
	"valueString":               true,
	"valueBoolean":              true,
	"valueInteger":              true,
	"clinicalStatus":            true,
	"verificationStatus":        true,
	"onsetDateTime":             true,
	"recordedDate":              true,
	"class":                     true,
	"type":                      true,
	"period":                    true,
	"serviceProvider":           true,
	"medicationCodeableConcept": true,
	"medicationReference":       true,
	"authoredOn":                true,
	"requester":                 true,
	"vaccineCode":               true,
	"occurrenceDateTime":        true,
}

func checkFilterExprChainedScopes(secCtx *auth.SecurityContext, expr postgres.FilterExpr) string {
	switch expr.Kind {
	case "and", "or", "not":
		for _, child := range expr.Children {
			if t := checkFilterExprChainedScopes(secCtx, child); t != "" {
				return t
			}
		}
	case "param":
		if strings.Contains(expr.Param, ".") {
			if ch, ok := postgres.ParseChainedParam(expr.Param, expr.Value); ok {
				for _, hop := range ch.Hops {
					if hop.TargetType != "" && !auth.Allows(secCtx, hop.TargetType, auth.ActionRead) && !auth.Allows(secCtx, hop.TargetType, auth.ActionSearch) {
						return hop.TargetType
					}
				}
			}
		}
	}
	return ""
}

func applySummaryOrElements(resource map[string]any, summary string, elements []string) map[string]any {
	if resource == nil {
		return nil
	}
	out := make(map[string]any, len(resource))

	switch {
	case len(elements) > 0:
		resType, _ := resource["resourceType"].(string)
		keep := map[string]bool{
			"resourceType": true,
			"id":           true,
			"meta":         true,
		}
		var prefixes []string
		hasMatchedForType := false
		hasUnqualified := false
		for _, el := range elements {
			if idx := strings.IndexByte(el, '.'); idx != -1 {
				rt := strings.TrimSpace(el[:idx])
				field := strings.TrimSpace(el[idx+1:])
				if strings.EqualFold(rt, resType) && field != "" {
					keep[field] = true
					prefixes = append(prefixes, field)
					hasMatchedForType = true
				}
			} else {
				keep[el] = true
				prefixes = append(prefixes, el)
				hasUnqualified = true
			}
		}
		if !hasUnqualified && !hasMatchedForType {
			return resource
		}
		for k, v := range resource {
			if keep[k] {
				out[k] = v
				continue
			}
			for _, p := range prefixes {
				if strings.HasPrefix(k, p) && len(k) > len(p) && k[len(p)] >= 'A' && k[len(p)] <= 'Z' {
					out[k] = v
					break
				}
			}
		}
		addSubsettedTag(out)
		return out

	case summary == "text":
		for _, k := range []string{"resourceType", "id", "meta", "text", "status"} {
			if v, ok := resource[k]; ok {
				out[k] = v
			}
		}
		addSubsettedTag(out)
		return out

	case summary == "data":
		for k, v := range resource {
			if k == "text" {
				continue
			}
			out[k] = v
		}
		addSubsettedTag(out)
		return out

	case summary == "true":
		for k, v := range resource {
			if fhirSummaryElements[k] {
				out[k] = v
			}
		}
		addSubsettedTag(out)
		return out

	default:
		return resource
	}
}

func addSubsettedTag(resource map[string]any) {
	meta, _ := resource["meta"].(map[string]any)
	if meta == nil {
		meta = make(map[string]any)
		resource["meta"] = meta
	}
	tags, _ := meta["tag"].([]any)
	for _, t := range tags {
		if tm, ok := t.(map[string]any); ok && tm["code"] == "SUBSETTED" {
			return
		}
	}
	tags = append(tags, map[string]any{
		"system":  "http://terminology.hl7.org/CodeSystem/v3-ObservationValue",
		"code":    "SUBSETTED",
		"display": "Resource encoded in summary mode",
	})
	meta["tag"] = tags
}

func usCoreSupportedProfiles(rt string) []string {
	switch rt {
	case "Patient":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"}
	case "Encounter":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-encounter"}
	case "Condition":
		return []string{
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-condition-problems-health-concerns",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-condition-encounter-diagnosis",
		}
	case "Observation":
		return []string{
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-vital-signs",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-lab",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-blood-pressure",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-average-blood-pressure",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-heart-rate",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-respiratory-rate",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-body-temperature",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-body-weight",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-body-height",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-bmi",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-pulse-oximetry",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-smokingstatus",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-care-experience-preference",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-treatment-intervention-preference",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-simple-observation",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-screening-assessment",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-clinical-result",
		}
	case "Practitioner":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitioner"}
	case "MedicationRequest":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medicationrequest"}
	case "AllergyIntolerance":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-allergyintolerance"}
	case "Procedure":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-procedure"}
	case "DiagnosticReport":
		return []string{
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-diagnosticreport-lab",
			"http://hl7.org/fhir/us/core/StructureDefinition/us-core-diagnosticreport-note",
		}
	case "Immunization":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-immunization"}
	case "Organization":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-organization"}
	case "Specimen":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-specimen"}
	case "Location":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-location"}
	case "DocumentReference":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-documentreference"}
	case "Provenance":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-provenance"}
	case "CarePlan":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-careplan"}
	case "CareTeam":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-careteam"}
	case "Goal":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-goal"}
	case "ServiceRequest":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-servicerequest"}
	case "Coverage":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-coverage"}
	case "QuestionnaireResponse":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-questionnaireresponse"}
	case "MedicationDispense":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medicationdispense"}
	case "Medication":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medication"}
	case "Device":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-implantable-device"}
	case "PractitionerRole":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitionerrole"}
	case "RelatedPerson":
		return []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-relatedperson"}
	default:
		return nil
	}
}

// handleMetadata returns the FHIR CapabilityStatement.
func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	cap := s.buildCapabilityStatement(r)
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(cap)
}

func (s *Server) buildCapabilityStatement(r *http.Request) map[string]any {
	s.syncTenantCustomSearchParams(r.Context(), r.PathValue("tenant"))

	interactions := []map[string]string{
		{"code": "read"},
		{"code": "vread"},
		{"code": "update"},
		{"code": "patch"},
		{"code": "delete"},
		{"code": "history-instance"},
		{"code": "history-type"},
		{"code": "create"},
		{"code": "search-type"},
	}

	defaultSearchParams := []map[string]string{
		{"name": "_id", "type": "token"},
		{"name": "_lastUpdated", "type": "date"},
		{"name": "_profile", "type": "uri"},
		{"name": "_tag", "type": "token"},
		{"name": "_security", "type": "token"},
		{"name": "_source", "type": "uri"},
		{"name": "_filter", "type": "special"},
		{"name": "_content", "type": "string"},
		{"name": "_text", "type": "string"},
		{"name": "identifier", "type": "token"},
		{"name": "status", "type": "token"},
		{"name": "code", "type": "token"},
		{"name": "patient", "type": "reference"},
		{"name": "subject", "type": "reference"},
		{"name": "date", "type": "date"},
	}

	spDefs := resourceSearchParamDefs()
	usCoreOrder := fhirutil.USCoreResourceTypes()
	seenTypes := make(map[string]bool, len(s.protoRegistry)+2)
	resEntries := make([]map[string]any, 0, len(s.protoRegistry)+2)
	for _, rt := range usCoreOrder {
		seenTypes[rt] = true
		sp := spDefs[rt]
		if len(sp) == 0 {
			sp = defaultSearchParams
		}
		sp = mergeCustomSearchParams(rt, sp)
		entry := map[string]any{
			"type":              rt,
			"profile":           "http://hl7.org/fhir/StructureDefinition/" + rt,
			"supportedProfile":  usCoreSupportedProfiles(rt),
			"versioning":        "versioned-update",
			"readHistory":       true,
			"updateCreate":      true,
			"conditionalCreate": true,
			"conditionalRead":   "full-support",
			"conditionalUpdate": true,
			"conditionalPatch":  true,
			"conditionalDelete": "single",
			"interaction":       interactions,
			"searchParam":       sp,
		}
		switch rt {
		case "Patient":
			entry["operation"] = []map[string]string{
				{
					"name":       "everything",
					"definition": "http://hl7.org/fhir/OperationDefinition/Patient-everything",
				},
				{
					"name":       "summary",
					"definition": "http://hl7.org/fhir/uv/ips/OperationDefinition/summary",
				},
				{
					"name":       "match",
					"definition": "http://hl7.org/fhir/OperationDefinition/Patient-match",
				},
				{
					"name":       "member-match",
					"definition": "http://hl7.org/fhir/us/davinci-hrex/OperationDefinition/member-match",
				},
				{
					"name":       "export",
					"definition": "http://hl7.org/fhir/uv/bulkdata/OperationDefinition/patient-export",
				},
				{
					"name":       "purge",
					"definition": "http://hl7.org/fhir/OperationDefinition/Patient-purge",
				},
				{
					"name":       "merge",
					"definition": "http://hl7.org/fhir/OperationDefinition/Patient-merge",
				},
				{
					"name":       "health-cards-issue",
					"definition": "http://hl7.org/fhir/uv/shc-vaccination/OperationDefinition/Patient-i-health-cards-issue",
				},
				{
					"name":       "pcde",
					"definition": "http://hl7.org/fhir/us/davinci-pcde/OperationDefinition/PCDE",
				},
				{
					"name":       "immds-forecast",
					"definition": "http://hl7.org/fhir/us/immds/OperationDefinition/immds-forecast",
				},
				{
					"name":       "gfe",
					"definition": "http://hl7.org/fhir/us/davinci-pct/OperationDefinition/gfe-submit",
				},
				{
					"name":       "ihe-pix",
					"definition": "https://profiles.ihe.net/ITI/PIXm/OperationDefinition/IHE.PIXm.pix",
				},
				{
					"name":       "bulk-member-match",
					"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/BulkMemberMatch",
				},
				{
					"name":       "sdoh-summary",
					"definition": "http://hl7.org/fhir/us/sdoh-clinicalcare/OperationDefinition/sdoh-summary",
				},
				{
					"name":       "carin-bb",
					"definition": "http://hl7.org/fhir/us/carin-bb/OperationDefinition/carin-bb",
				},
				{
					"name":       "decease",
					"definition": "http://hl7.org/fhir/us/vrdr/OperationDefinition/Patient-decease",
				},
				{
					"name":       "mcode-summary",
					"definition": "http://hl7.org/fhir/us/mcode/OperationDefinition/Patient-mcode-summary",
				},
				{
					"name":       "find-subject-variants",
					"definition": "http://hl7.org/fhir/uv/genomics-reporting/OperationDefinition/find-subject-variants",
				},
				{
					"name":       "find-subject-tx-implications",
					"definition": "http://hl7.org/fhir/uv/genomics-reporting/OperationDefinition/find-subject-tx-implications",
				},
				{
					"name":       "pacio-summary",
					"definition": "http://hl7.org/fhir/us/pacio-fs/OperationDefinition/pacio-summary",
				},
				{
					"name":       "birth-report",
					"definition": "http://hl7.org/fhir/us/bfdr/OperationDefinition/Patient-birth-report",
				},
				{
					"name":       "ihe-pdqm",
					"definition": "https://profiles.ihe.net/ITI/PDQm/OperationDefinition/IHE.PDQm.match",
				},
				{
					"name":       "physical-activity-summary",
					"definition": "http://hl7.org/fhir/us/physical-activity/OperationDefinition/Patient-physical-activity-summary",
				},
				{
					"name":       "adi-record",
					"definition": "http://hl7.org/fhir/us/pacio-adi/OperationDefinition/Patient-adi-record",
				},
				{
					"name":       "ra-coding-gaps",
					"definition": "http://hl7.org/fhir/us/davinci-ra/OperationDefinition/Patient-coding-gaps",
				},
				{
					"name":       "pdex-WriteData",
					"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/Patient-pdex-WriteData",
				},
				{
					"name":       "record-occupation",
					"definition": "http://hl7.org/fhir/us/odh/OperationDefinition/Patient-record-occupation",
				},
				{
					"name":       "odh-summary",
					"definition": "http://hl7.org/fhir/us/odh/OperationDefinition/Patient-odh-summary",
				},
				{
					"name":       "cancer-registry-report",
					"definition": "http://hl7.org/fhir/us/central-cancer-registry-reporting/OperationDefinition/Patient-cancer-registry-report",
				},
				{
					"name":       "pacio-cognitive-assess",
					"definition": "http://hl7.org/fhir/us/pacio-cs/OperationDefinition/Patient-pacio-cognitive-assess",
				},
				{
					"name":       "pacio-cognitive-summary",
					"definition": "http://hl7.org/fhir/us/pacio-cs/OperationDefinition/Patient-pacio-cognitive-summary",
				},
				{
					"name":       "idi-match",
					"definition": "http://hl7.org/fhir/us/identity-matching/OperationDefinition/Patient-idi-match",
				},
				{
					"name":       "pacio-pfe-record",
					"definition": "http://hl7.org/fhir/us/pacio-pfe/OperationDefinition/Patient-pacio-pfe-record",
				},
				{
					"name":       "pacio-pfe-summary",
					"definition": "http://hl7.org/fhir/us/pacio-pfe/OperationDefinition/Patient-pacio-pfe-summary",
				},
				{
					"name":       "mcode-radiotherapy-summary",
					"definition": "http://hl7.org/fhir/us/mcode/OperationDefinition/Patient-mcode-radiotherapy-summary",
				},
				{
					"name":       "iis-history",
					"definition": "http://hl7.org/fhir/us/immds/OperationDefinition/Patient-iis-history",
				},
				{
					"name":       "mcode-genomics-summary",
					"definition": "http://hl7.org/fhir/us/mcode/OperationDefinition/Patient-mcode-genomics-summary",
				},
				{
					"name":       "nbs-summary",
					"definition": "http://hl7.org/fhir/us/newborn-screening/OperationDefinition/Patient-nbs-summary",
				},
				{
					"name":       "ecr-summary",
					"definition": "http://hl7.org/fhir/us/ecr/OperationDefinition/Patient-ecr-summary",
				},
				{
					"name":       "elr-summary",
					"definition": "http://hl7.org/fhir/us/elr/OperationDefinition/Patient-elr-summary",
				},
				{
					"name":       "rtpbc-summary",
					"definition": "http://hl7.org/fhir/us/carin-rtpbc/OperationDefinition/Patient-rtpbc-summary",
				},
				{
					"name":       "nhsn-hai-summary",
					"definition": "http://hl7.org/fhir/us/nhsn-dqm/OperationDefinition/Patient-nhsn-hai-summary",
				},
				{
					"name":       "mcode-trial-match",
					"definition": "http://hl7.org/fhir/us/codex-clinical-trial-matching/OperationDefinition/Patient-mcode-trial-match",
				},
				{
					"name":       "ra-coding-gap-report",
					"definition": "http://hl7.org/fhir/us/davinci-ra/OperationDefinition/Patient-ra-coding-gap-report",
				},
				{
					"name":       "nhsn-ach-summary",
					"definition": "http://hl7.org/fhir/us/nhsn-dqm/OperationDefinition/Patient-nhsn-ach-summary",
				},
				{
					"name":       "burden-reduction-summary",
					"definition": "http://hl7.org/fhir/us/davinci-crd/OperationDefinition/Patient-burden-reduction-summary",
				},
				{
					"name":       "mcode-trial-summary",
					"definition": "http://hl7.org/fhir/us/codex-clinical-trial-matching/OperationDefinition/Patient-mcode-trial-summary",
				},
				{
					"name":       "pacio-splasch-summary",
					"definition": "http://hl7.org/fhir/us/pacio-splasch/OperationDefinition/Patient-pacio-splasch-summary",
				},
				{
					"name":       "vulcan-rwd-summary",
					"definition": "http://hl7.org/fhir/uv/vulcan-rwd/OperationDefinition/Patient-vulcan-rwd-summary",
				},
				{
					"name":       "codex-rttd-summary",
					"definition": "http://hl7.org/fhir/us/codex-radiation-therapy/OperationDefinition/Patient-codex-rttd-summary",
				},
				{
					"name":       "sdoh-care-bundle",
					"definition": "http://hl7.org/fhir/us/sdoh-clinicalcare/OperationDefinition/Patient-sdoh-care-bundle",
				},
				{
					"name":       "pdex-provider-everything",
					"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/Patient-pdex-provider-everything",
				},
				{
					"name":       "pdex-payer-exchange",
					"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/Patient-pdex-payer-exchange",
				},
				{
					"name":       "pdex-payer-summary",
					"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/Patient-pdex-payer-summary",
				},
			}
		case "Location":
			entry["operation"] = []map[string]string{
				{
					"name":       "saner-capacity-summary",
					"definition": "http://hl7.org/fhir/uv/saner/OperationDefinition/Location-saner-capacity-summary",
				},
			}
		case "Immunization":
			entry["operation"] = []map[string]string{
				{
					"name":       "iis-submit",
					"definition": "http://hl7.org/fhir/us/immds/OperationDefinition/Immunization-iis-submit",
				},
			}
		case "Procedure":
			entry["operation"] = []map[string]string{
				{
					"name":       "mcode-radiotherapy-record",
					"definition": "http://hl7.org/fhir/us/mcode/OperationDefinition/Procedure-mcode-radiotherapy-record",
				},
			}
		case "Organization":
			entry["operation"] = []map[string]string{
				{
					"name":       "fast-verify",
					"definition": "http://hl7.org/fhir/us/ndh/OperationDefinition/Organization-fast-verify",
				},
				{
					"name":       "ndh-directory-summary",
					"definition": "http://hl7.org/fhir/us/ndh/OperationDefinition/Organization-ndh-directory-summary",
				},
				{
					"name":       "udap-register",
					"definition": "http://hl7.org/fhir/us/udap-security/OperationDefinition/Organization-udap-register",
				},
				{
					"name":       "udap-trust-summary",
					"definition": "http://hl7.org/fhir/us/udap-security/OperationDefinition/Organization-udap-trust-summary",
				},
			}
		case "Condition":
			entry["operation"] = []map[string]string{
				{
					"name":       "stage-cancer",
					"definition": "http://hl7.org/fhir/us/mcode/OperationDefinition/Condition-stage-cancer",
				},
			}
		case "Coverage":
			entry["operation"] = []map[string]string{
				{
					"name":       "formulary-coverage",
					"definition": "http://hl7.org/fhir/us/davinci-drug-formulary/OperationDefinition/Coverage-formulary-coverage",
				},
			}
		case "PractitionerRole":
			entry["operation"] = []map[string]string{
				{
					"name":       "find-providers",
					"definition": "http://hl7.org/fhir/us/davinci-pdex-plan-net/OperationDefinition/find-providers",
				},
			}
		case "DiagnosticReport":
			entry["operation"] = []map[string]string{
				{
					"name":       "evaluate",
					"definition": "http://flint.fhir.org/OperationDefinition/DiagnosticReport-evaluate",
				},
				{
					"name":       "elr-submit",
					"definition": "http://hl7.org/fhir/us/elr/OperationDefinition/DiagnosticReport-elr-submit",
				},
				{
					"name":       "mcode-genomics-report",
					"definition": "http://hl7.org/fhir/us/mcode/OperationDefinition/DiagnosticReport-mcode-genomics-report",
				},
				{
					"name":       "nbs-submit",
					"definition": "http://hl7.org/fhir/us/newborn-screening/OperationDefinition/DiagnosticReport-nbs-submit",
				},
			}
		case "Observation":
			entry["operation"] = []map[string]string{
				{
					"name":       "lastn",
					"definition": "http://hl7.org/fhir/OperationDefinition/Observation-lastn",
				},
				{
					"name":       "stats",
					"definition": "http://hl7.org/fhir/OperationDefinition/Observation-stats",
				},
				{
					"name":       "reportable-labs",
					"definition": "http://hl7.org/fhir/us/elr/OperationDefinition/Observation-reportable-labs",
				},
				{
					"name":       "mcode-disease-status",
					"definition": "http://hl7.org/fhir/us/mcode/OperationDefinition/Observation-mcode-disease-status",
				},
				{
					"name":       "pacio-splasch-record",
					"definition": "http://hl7.org/fhir/us/pacio-splasch/OperationDefinition/Observation-pacio-splasch-record",
				},
			}
		case "Encounter":
			entry["operation"] = []map[string]string{
				{
					"name":       "everything",
					"definition": "http://hl7.org/fhir/OperationDefinition/Encounter-everything",
				},
				{
					"name":       "admit",
					"definition": "http://flint.fhir.org/OperationDefinition/Encounter-admit",
				},
				{
					"name":       "transfer",
					"definition": "http://flint.fhir.org/OperationDefinition/Encounter-transfer",
				},
				{
					"name":       "discharge",
					"definition": "http://flint.fhir.org/OperationDefinition/Encounter-discharge",
				},
				{
					"name":       "ecr-report",
					"definition": "http://hl7.org/fhir/us/ecr/OperationDefinition/Encounter-ecr-report",
				},
				{
					"name":       "nhsn-hai-report",
					"definition": "http://hl7.org/fhir/us/nhsn-dqm/OperationDefinition/Encounter-nhsn-hai-report",
				},
				{
					"name":       "pacio-timepoint",
					"definition": "http://hl7.org/fhir/us/pacio-rt/OperationDefinition/Encounter-pacio-timepoint",
				},
				{
					"name":       "pacio-timepoints",
					"definition": "http://hl7.org/fhir/us/pacio-rt/OperationDefinition/Encounter-pacio-timepoints",
				},
			}
		case "Composition":
			entry["operation"] = []map[string]string{
				{
					"name":       "document",
					"definition": "http://hl7.org/fhir/OperationDefinition/Composition-document",
				},
				{
					"name":       "bfdr-summary",
					"definition": "http://hl7.org/fhir/us/bfdr/OperationDefinition/Composition-bfdr-summary",
				},
				{
					"name":       "adi-document",
					"definition": "http://hl7.org/fhir/us/pacio-adi/OperationDefinition/Composition-adi-document",
				},
				{
					"name":       "eicr-document",
					"definition": "http://hl7.org/fhir/us/ecr/OperationDefinition/Composition-eicr-document",
				},
				{
					"name":       "vrdr-document",
					"definition": "http://hl7.org/fhir/us/vrdr/OperationDefinition/Composition-vrdr-document",
				},
				{
					"name":       "bfdr-document",
					"definition": "http://hl7.org/fhir/us/bfdr/OperationDefinition/Composition-bfdr-document",
				},
				{
					"name":       "pacio-toc-bundle",
					"definition": "http://hl7.org/fhir/us/pacio-toc/OperationDefinition/Composition-pacio-toc-bundle",
				},
			}
		case "DocumentReference":
			entry["operation"] = []map[string]string{
				{
					"name":       "docref",
					"definition": "http://hl7.org/fhir/us/core/OperationDefinition/docref",
				},
				{
					"name":       "iti-67",
					"definition": "https://profiles.ihe.net/ITI/MHD/OperationDefinition/IHE.MHD.FindDocumentReferences",
				},
				{
					"name":       "ihe-iti-68",
					"definition": "https://profiles.ihe.net/ITI/MHD/OperationDefinition/IHE.MHD.RetrieveDocument",
				},
			}
		case "QuestionnaireResponse":
			entry["operation"] = []map[string]string{
				{
					"name":       "extract",
					"definition": "http://hl7.org/fhir/uv/sdc/OperationDefinition/QuestionnaireResponse-extract",
				},
				{
					"name":       "pacio-assess",
					"definition": "http://hl7.org/fhir/us/pacio-fs/OperationDefinition/pacio-assess",
				},
				{
					"name":       "sdoh-assess",
					"definition": "http://hl7.org/fhir/us/sdoh-clinicalcare/OperationDefinition/QuestionnaireResponse-sdoh-assess",
				},
			}
		case "ServiceRequest":
			entry["operation"] = []map[string]string{
				{
					"name":       "crd",
					"definition": "http://hl7.org/fhir/us/davinci-crd/OperationDefinition/crd",
				},
				{
					"name":       "collect-specimen",
					"definition": "http://flint.fhir.org/OperationDefinition/ServiceRequest-collect-specimen",
				},
				{
					"name":       "sdoh-referral",
					"definition": "http://hl7.org/fhir/us/sdoh-clinicalcare/OperationDefinition/sdoh-referral",
				},
				{
					"name":       "physical-activity-referral",
					"definition": "http://hl7.org/fhir/us/physical-activity/OperationDefinition/ServiceRequest-physical-activity-referral",
				},
				{
					"name":       "codex-rttd-plan",
					"definition": "http://hl7.org/fhir/us/codex-radiation-therapy/OperationDefinition/ServiceRequest-codex-rttd-plan",
				},
			}
		case "MedicationRequest":
			entry["operation"] = []map[string]string{
				{
					"name":       "crd",
					"definition": "http://hl7.org/fhir/us/davinci-crd/OperationDefinition/crd",
				},
				{
					"name":       "check-interactions",
					"definition": "http://flint.fhir.org/OperationDefinition/MedicationRequest-check-interactions",
				},
				{
					"name":       "dispense",
					"definition": "http://flint.fhir.org/OperationDefinition/MedicationRequest-dispense",
				},
			}
		case "CarePlan":
			entry["operation"] = []map[string]string{
				{
					"name":       "evaluate",
					"definition": "http://flint.fhir.org/OperationDefinition/CarePlan-evaluate",
				},
			}
		}
		resEntries = append(resEntries, entry)
	}
	seenTypes["Group"] = true
	resEntries = append(resEntries, map[string]any{
		"type":              "Group",
		"profile":           "http://hl7.org/fhir/StructureDefinition/Group",
		"versioning":        "versioned-update",
		"readHistory":       true,
		"updateCreate":      true,
		"conditionalCreate": true,
		"conditionalRead":   "full-support",
		"conditionalUpdate": true,
		"conditionalPatch":  true,
		"conditionalDelete": "single",
		"interaction":       interactions,
		"operation": []map[string]string{
			{
				"name":       "export",
				"definition": "http://hl7.org/fhir/uv/bulkdata/OperationDefinition/group-export",
			},
			{
				"name":       "everything",
				"definition": "http://hl7.org/fhir/OperationDefinition/Group-everything",
			},
			{
				"name":       "add",
				"definition": "http://hl7.org/fhir/OperationDefinition/Group-add",
			},
			{
				"name":       "remove",
				"definition": "http://hl7.org/fhir/OperationDefinition/Group-remove",
			},
			{
				"name":       "member-add",
				"definition": "http://hl7.org/fhir/us/davinci-atr/OperationDefinition/member-add",
			},
			{
				"name":       "member-remove",
				"definition": "http://hl7.org/fhir/us/davinci-atr/OperationDefinition/member-remove",
			},
			{
				"name":       "confirm-attribution-list",
				"definition": "http://hl7.org/fhir/us/davinci-atr/OperationDefinition/confirm-attribution-list",
			},
			{
				"name":       "atr-summary",
				"definition": "http://hl7.org/fhir/us/davinci-atr/OperationDefinition/Group-atr-summary",
			},
			{
				"name":       "davinci-data-export",
				"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/DavinciDataExport",
			},
			{
				"name":       "pdex-provider-member-match",
				"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/Group-pdex-provider-member-match",
			},
		},
	})
	seenTypes["AuditEvent"] = true
	resEntries = append(resEntries, map[string]any{
		"type":        "AuditEvent",
		"profile":     "http://hl7.org/fhir/StructureDefinition/AuditEvent",
		"interaction": []map[string]string{{"code": "read"}, {"code": "search-type"}},
		"searchParam": []map[string]string{
			{"name": "action", "type": "token"},
			{"name": "subtype", "type": "token"},
			{"name": "outcome", "type": "token"},
			{"name": "agent", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "entity", "type": "reference"},
			{"name": "entity-type", "type": "token"},
			{"name": "entity-id", "type": "token"},
		},
		"operation": []map[string]string{
			{
				"name":       "verify-integrity",
				"definition": "http://flint.fhir.org/OperationDefinition/AuditEvent-verify-integrity",
			},
		},
	})

	for _, rt := range fhirutil.AllR4ResourceTypes() {
		if seenTypes[rt] {
			continue
		}
		if _, registered := s.protoRegistry[rt]; !registered {
			continue
		}
		sp := spDefs[rt]
		if len(sp) == 0 {
			sp = defaultSearchParams
		}
		sp = mergeCustomSearchParams(rt, sp)
		entry := map[string]any{
			"type":              rt,
			"profile":           "http://hl7.org/fhir/StructureDefinition/" + rt,
			"versioning":        "versioned-update",
			"readHistory":       true,
			"updateCreate":      true,
			"conditionalCreate": true,
			"conditionalRead":   "full-support",
			"conditionalUpdate": true,
			"conditionalPatch":  true,
			"conditionalDelete": "single",
			"interaction":       interactions,
			"searchParam":       sp,
		}
		switch rt {
		case "Claim":
			entry["operation"] = []map[string]string{
				{
					"name":       "submit",
					"definition": "http://hl7.org/fhir/OperationDefinition/Claim-submit",
				},
				{
					"name":       "inquire",
					"definition": "http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-inquiry",
				},
				{
					"name":       "predetermine",
					"definition": "http://hl7.org/fhir/OperationDefinition/Claim-predetermine",
				},
				{
					"name":       "gfe-submit",
					"definition": "http://hl7.org/fhir/us/davinci-pct/OperationDefinition/gfe-submit",
				},
				{
					"name":       "rtpbc-submit",
					"definition": "http://hl7.org/fhir/us/carin-rtpbc/OperationDefinition/Claim-rtpbc-submit",
				},
				{
					"name":       "pas-inquire",
					"definition": "http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-pas-inquire",
				},
			}
		case "ClaimResponse":
			entry["operation"] = []map[string]string{
				{
					"name":       "rtpbc-coverage",
					"definition": "http://hl7.org/fhir/us/carin-rtpbc/OperationDefinition/ClaimResponse-rtpbc-coverage",
				},
			}
		case "Person":
			entry["operation"] = []map[string]string{
				{
					"name":       "verify-identity",
					"definition": "http://hl7.org/fhir/us/identity-matching/OperationDefinition/Person-verify-identity",
				},
			}
		case "Composition":
			entry["operation"] = []map[string]string{
				{
					"name":       "document",
					"definition": "http://hl7.org/fhir/OperationDefinition/Composition-document",
				},
				{
					"name":       "bfdr-summary",
					"definition": "http://hl7.org/fhir/us/bfdr/OperationDefinition/Composition-bfdr-summary",
				},
				{
					"name":       "adi-document",
					"definition": "http://hl7.org/fhir/us/pacio-adi/OperationDefinition/Composition-adi-document",
				},
				{
					"name":       "eicr-document",
					"definition": "http://hl7.org/fhir/us/ecr/OperationDefinition/Composition-eicr-document",
				},
				{
					"name":       "vrdr-document",
					"definition": "http://hl7.org/fhir/us/vrdr/OperationDefinition/Composition-vrdr-document",
				},
				{
					"name":       "bfdr-document",
					"definition": "http://hl7.org/fhir/us/bfdr/OperationDefinition/Composition-bfdr-document",
				},
				{
					"name":       "pacio-toc-bundle",
					"definition": "http://hl7.org/fhir/us/pacio-toc/OperationDefinition/Composition-pacio-toc-bundle",
				},
			}
		case "MeasureReport":
			entry["operation"] = []map[string]string{
				{
					"name":       "ra-remediate",
					"definition": "http://hl7.org/fhir/us/davinci-ra/OperationDefinition/MeasureReport-ra-remediate",
				},
				{
					"name":       "vbpr-performance",
					"definition": "http://hl7.org/fhir/us/davinci-vbpr/OperationDefinition/MeasureReport-vbpr-performance",
				},
				{
					"name":       "vbpr-attest",
					"definition": "http://hl7.org/fhir/us/davinci-vbpr/OperationDefinition/MeasureReport-vbpr-attest",
				},
				{
					"name":       "nhsn-aur-submit",
					"definition": "http://hl7.org/fhir/us/nhsn-dqm/OperationDefinition/MeasureReport-nhsn-aur-submit",
				},
				{
					"name":       "ra-evaluate-risk",
					"definition": "http://hl7.org/fhir/us/davinci-ra/OperationDefinition/MeasureReport-ra-evaluate-risk",
				},
				{
					"name":       "saner-report-capacity",
					"definition": "http://hl7.org/fhir/uv/saner/OperationDefinition/MeasureReport-saner-report-capacity",
				},
			}
		case "DocumentReference":
			entry["operation"] = []map[string]string{
				{
					"name":       "docref",
					"definition": "http://hl7.org/fhir/us/core/OperationDefinition/docref",
				},
				{
					"name":       "iti-67",
					"definition": "https://profiles.ihe.net/ITI/MHD/OperationDefinition/IHE.MHD.FindDocumentReferences",
				},
				{
					"name":       "ihe-iti-68",
					"definition": "https://profiles.ihe.net/ITI/MHD/OperationDefinition/IHE.MHD.RetrieveDocument",
				},
			}
		case "Questionnaire":
			entry["operation"] = []map[string]string{
				{
					"name":       "populate",
					"definition": "http://hl7.org/fhir/uv/sdc/OperationDefinition/Questionnaire-populate",
				},
				{
					"name":       "package",
					"definition": "http://hl7.org/fhir/us/davinci-dtr/OperationDefinition/questionnaire-package",
				},
				{
					"name":       "dtr-questionnaire-package",
					"definition": "http://hl7.org/fhir/us/davinci-dtr/OperationDefinition/Questionnaire-dtr-questionnaire-package",
				},
				{
					"name":       "data-requirements",
					"definition": "http://hl7.org/fhir/OperationDefinition/Questionnaire-data-requirements",
				},
			}
		case "Measure":
			entry["operation"] = []map[string]string{
				{
					"name":       "evaluate-measure",
					"definition": "http://hl7.org/fhir/OperationDefinition/Measure-evaluate-measure",
				},
				{
					"name":       "care-gaps",
					"definition": "http://hl7.org/fhir/us/davinci-deqm/OperationDefinition/care-gaps",
				},
				{
					"name":       "data-requirements",
					"definition": "http://hl7.org/fhir/OperationDefinition/Measure-data-requirements",
				},
				{
					"name":       "collect-data",
					"definition": "http://hl7.org/fhir/OperationDefinition/Measure-collect-data",
				},
				{
					"name":       "submit-data",
					"definition": "http://hl7.org/fhir/OperationDefinition/Measure-submit-data",
				},
				{
					"name":       "evaluate-risk-condition-category",
					"definition": "http://hl7.org/fhir/us/davinci-ra/OperationDefinition/evaluate-risk-condition-category",
				},
				{
					"name":       "nhsn-acute-care",
					"definition": "http://hl7.org/fhir/us/nhsn-dqm/OperationDefinition/Measure-nhsn-acute-care",
				},
				{
					"name":       "nhsn-dqm-evaluate",
					"definition": "http://hl7.org/fhir/us/nhsn-dqm/OperationDefinition/Measure-nhsn-dqm-evaluate",
				},
			}
		case "Library":
			entry["operation"] = []map[string]string{
				{
					"name":       "data-requirements",
					"definition": "http://hl7.org/fhir/OperationDefinition/Library-data-requirements",
				},
				{
					"name":       "evaluate",
					"definition": "http://hl7.org/fhir/uv/cpg/OperationDefinition/cpg-library-evaluate",
				},
			}
		case "Encounter":
			entry["operation"] = []map[string]string{
				{
					"name":       "everything",
					"definition": "http://hl7.org/fhir/OperationDefinition/Encounter-everything",
				},
				{
					"name":       "pacio-timepoint",
					"definition": "http://hl7.org/fhir/us/pacio-rt/OperationDefinition/Encounter-pacio-timepoint",
				},
				{
					"name":       "pacio-timepoints",
					"definition": "http://hl7.org/fhir/us/pacio-rt/OperationDefinition/Encounter-pacio-timepoints",
				},
			}
		case "Task":
			entry["operation"] = []map[string]string{
				{
					"name":       "submit-attachment",
					"definition": "http://hl7.org/fhir/us/davinci-cdex/OperationDefinition/submit-attachment",
				},
				{
					"name":       "cdex-request",
					"definition": "http://hl7.org/fhir/us/davinci-cdex/OperationDefinition/Task-cdex-request",
				},
				{
					"name":       "cdex-poll",
					"definition": "http://hl7.org/fhir/us/davinci-cdex/OperationDefinition/Task-cdex-poll",
				},
				{
					"name":       "cdex-questionnaire-request",
					"definition": "http://hl7.org/fhir/us/davinci-cdex/OperationDefinition/Task-cdex-questionnaire-request",
				},
				{
					"name":       "pct-gfe-coordination",
					"definition": "http://hl7.org/fhir/us/davinci-pct/OperationDefinition/Task-pct-gfe-coordination",
				},
				{
					"name":       "sdoh-fulfill",
					"definition": "http://hl7.org/fhir/us/sdoh-clinicalcare/OperationDefinition/Task-sdoh-fulfill",
				},
				{
					"name":       "cdex-submit-attachment",
					"definition": "http://hl7.org/fhir/us/davinci-cdex/OperationDefinition/Task-cdex-submit-attachment",
				},
			}
		case "ValueSet":
			entry["operation"] = []map[string]string{
				{
					"name":       "expand",
					"definition": "http://hl7.org/fhir/OperationDefinition/ValueSet-expand",
				},
				{
					"name":       "validate-code",
					"definition": "http://hl7.org/fhir/OperationDefinition/ValueSet-validate-code",
				},
			}
		case "CodeSystem":
			entry["operation"] = []map[string]string{
				{
					"name":       "lookup",
					"definition": "http://hl7.org/fhir/OperationDefinition/CodeSystem-lookup",
				},
				{
					"name":       "validate-code",
					"definition": "http://hl7.org/fhir/OperationDefinition/CodeSystem-validate-code",
				},
				{
					"name":       "subsumes",
					"definition": "http://hl7.org/fhir/OperationDefinition/CodeSystem-subsumes",
				},
				{
					"name":       "find-matches",
					"definition": "http://hl7.org/fhir/OperationDefinition/CodeSystem-find-matches",
				},
			}
		case "ConceptMap":
			entry["operation"] = []map[string]string{
				{
					"name":       "translate",
					"definition": "http://hl7.org/fhir/OperationDefinition/ConceptMap-translate",
				},
				{
					"name":       "closure",
					"definition": "http://hl7.org/fhir/OperationDefinition/ConceptMap-closure",
				},
			}
		case "Subscription":
			entry["operation"] = []map[string]string{
				{
					"name":       "status",
					"definition": "http://hl7.org/fhir/uv/subscriptions-backport/OperationDefinition/backport-subscription-status",
				},
				{
					"name":       "events",
					"definition": "http://hl7.org/fhir/uv/subscriptions-backport/OperationDefinition/backport-subscription-events",
				},
			}
		case "PlanDefinition":
			entry["operation"] = []map[string]string{
				{
					"name":       "apply",
					"definition": "http://hl7.org/fhir/OperationDefinition/PlanDefinition-apply",
				},
				{
					"name":       "data-requirements",
					"definition": "http://hl7.org/fhir/OperationDefinition/PlanDefinition-data-requirements",
				},
				{
					"name":       "ersd-lookup",
					"definition": "http://hl7.org/fhir/us/ecr/OperationDefinition/ersd-lookup",
				},
				{
					"name":       "medmorph-kar",
					"definition": "http://hl7.org/fhir/us/medmorph/OperationDefinition/PlanDefinition-medmorph-kar",
				},
			}
		case "ActivityDefinition":
			entry["operation"] = []map[string]string{
				{
					"name":       "apply",
					"definition": "http://hl7.org/fhir/OperationDefinition/ActivityDefinition-apply",
				},
				{
					"name":       "data-requirements",
					"definition": "http://hl7.org/fhir/OperationDefinition/ActivityDefinition-data-requirements",
				},
			}
		case "CoverageEligibilityRequest":
			entry["operation"] = []map[string]string{
				{
					"name":       "submit",
					"definition": "http://hl7.org/fhir/OperationDefinition/CoverageEligibilityRequest-submit",
				},
			}
		case "GraphDefinition":
			entry["operation"] = []map[string]string{
				{
					"name":       "graph",
					"definition": "http://hl7.org/fhir/OperationDefinition/Resource-graph",
				},
			}
		case "Appointment":
			entry["operation"] = []map[string]string{
				{
					"name":       "find",
					"definition": "http://fhir.org/guides/argonaut/scheduling/OperationDefinition/appointment-find",
				},
				{
					"name":       "hold",
					"definition": "http://fhir.org/guides/argonaut/scheduling/OperationDefinition/appointment-hold",
				},
				{
					"name":       "book",
					"definition": "http://fhir.org/guides/argonaut/scheduling/OperationDefinition/appointment-book",
				},
			}
		case "CapabilityStatement":
			entry["operation"] = []map[string]string{
				{
					"name":       "subset",
					"definition": "http://hl7.org/fhir/OperationDefinition/CapabilityStatement-subset",
				},
				{
					"name":       "implements",
					"definition": "http://hl7.org/fhir/OperationDefinition/CapabilityStatement-implements",
				},
			}
		case "StructureDefinition":
			entry["operation"] = []map[string]string{
				{
					"name":       "snapshot",
					"definition": "http://hl7.org/fhir/OperationDefinition/StructureDefinition-snapshot",
				},
			}
		case "MessageHeader":
			entry["operation"] = []map[string]string{
				{
					"name":       "process-message",
					"definition": "http://hl7.org/fhir/OperationDefinition/MessageHeader-process-message",
				},
				{
					"name":       "process-notification",
					"definition": "http://hl7.org/fhir/us/davinci-notifications/OperationDefinition/process-notification",
				},
			}
		case "StructureMap":
			entry["operation"] = []map[string]string{
				{
					"name":       "transform",
					"definition": "http://hl7.org/fhir/OperationDefinition/StructureMap-transform",
				},
			}
		case "NamingSystem":
			entry["operation"] = []map[string]string{
				{
					"name":       "preferred-id",
					"definition": "http://hl7.org/fhir/OperationDefinition/NamingSystem-preferred-id",
				},
			}
		case "List":
			entry["operation"] = []map[string]string{
				{
					"name":       "find",
					"definition": "http://hl7.org/fhir/OperationDefinition/List-find",
				},
			}
		case "ChargeItem":
			entry["operation"] = []map[string]string{
				{
					"name":       "apply",
					"definition": "http://hl7.org/fhir/OperationDefinition/ChargeItem-apply",
				},
			}
		case "ResearchStudy":
			entry["operation"] = []map[string]string{
				{
					"name":       "evaluate-eligibility",
					"definition": "http://flint.fhir.org/OperationDefinition/ResearchStudy-evaluate-eligibility",
				},
				{
					"name":       "rwd-cohort",
					"definition": "http://hl7.org/fhir/uv/vulcan-rwd/OperationDefinition/ResearchStudy-rwd-cohort",
				},
				{
					"name":       "vulcan-rwd-cohort",
					"definition": "http://hl7.org/fhir/uv/vulcan-rwd/OperationDefinition/ResearchStudy-vulcan-rwd-cohort",
				},
				{
					"name":       "vulcan-summary",
					"definition": "http://hl7.org/fhir/uv/vulcan-rwd/OperationDefinition/ResearchStudy-vulcan-summary",
				},
			}
		case "ResearchSubject":
			entry["operation"] = []map[string]string{
				{
					"name":       "enroll",
					"definition": "http://hl7.org/fhir/uv/vulcan-schedule/OperationDefinition/ResearchSubject-enroll",
				},
				{
					"name":       "vulcan-soa-timeline",
					"definition": "http://hl7.org/fhir/uv/vulcan-schedule/OperationDefinition/ResearchSubject-vulcan-soa-timeline",
				},
				{
					"name":       "vulcan-esource-extract",
					"definition": "http://hl7.org/fhir/uv/vulcan-esource/OperationDefinition/ResearchSubject-vulcan-esource-extract",
				},
			}
		case "Endpoint":
			entry["operation"] = []map[string]string{
				{
					"name":       "fast-endpoint-query",
					"definition": "http://hl7.org/fhir/us/ndh/OperationDefinition/Endpoint-fast-endpoint-query",
				},
			}
		case "MedicinalProduct":
			entry["operation"] = []map[string]string{
				{
					"name":       "epi-document",
					"definition": "http://hl7.org/fhir/uv/emedicinal-product-info/OperationDefinition/MedicinalProduct-epi-document",
				},
			}
		case "MedicinalProductAuthorization":
			entry["operation"] = []map[string]string{
				{
					"name":       "evaluate-authorization",
					"definition": "http://hl7.org/fhir/uv/emedicinal-product-info/OperationDefinition/MedicinalProductAuthorization-evaluate-authorization",
				},
			}
		case "Schedule":
			entry["operation"] = []map[string]string{
				{
					"name":       "find",
					"definition": "http://flint.fhir.org/OperationDefinition/Schedule-find",
				},
			}
		case "AdverseEvent":
			entry["operation"] = []map[string]string{
				{
					"name":       "evaluate",
					"definition": "http://flint.fhir.org/OperationDefinition/AdverseEvent-evaluate",
				},
				{
					"name":       "vulcan-icsr-report",
					"definition": "http://hl7.org/fhir/uv/vulcan-ae/OperationDefinition/AdverseEvent-vulcan-icsr-report",
				},
			}
		case "RiskAssessment":
			entry["operation"] = []map[string]string{
				{
					"name":       "evaluate",
					"definition": "http://flint.fhir.org/OperationDefinition/RiskAssessment-evaluate",
				},
			}
		case "MedicationKnowledge":
			entry["operation"] = []map[string]string{
				{
					"name":       "lookup",
					"definition": "http://hl7.org/fhir/us/davinci-drug-formulary/OperationDefinition/MedicationKnowledge-lookup",
				},
				{
					"name":       "formulary-lookup",
					"definition": "http://hl7.org/fhir/us/davinci-drug-formulary/OperationDefinition/MedicationKnowledge-formulary-lookup",
				},
				{
					"name":       "formulary-alternatives",
					"definition": "http://hl7.org/fhir/us/davinci-drug-formulary/OperationDefinition/MedicationKnowledge-formulary-alternatives",
				},
			}
		case "Communication":
			entry["operation"] = []map[string]string{
				{
					"name":       "rr-process",
					"definition": "http://hl7.org/fhir/us/ecr/OperationDefinition/Communication-rr-process",
				},
				{
					"name":       "cdex-submit-attachment",
					"definition": "http://hl7.org/fhir/us/davinci-cdex/OperationDefinition/Communication-cdex-submit-attachment",
				},
			}
		case "Bundle":
			entry["operation"] = []map[string]string{
				{
					"name":       "iti-65",
					"definition": "https://profiles.ihe.net/ITI/MHD/OperationDefinition/IHE.MHD.ProvideBundle",
				},
				{
					"name":       "vrdr-submit",
					"definition": "http://hl7.org/fhir/us/vrdr/OperationDefinition/Bundle-vrdr-submit",
				},
				{
					"name":       "bfdr-submit",
					"definition": "http://hl7.org/fhir/us/bfdr/OperationDefinition/Bundle-bfdr-submit",
				},
				{
					"name":       "cdex-sign",
					"definition": "http://hl7.org/fhir/us/davinci-cdex/OperationDefinition/Bundle-cdex-sign",
				},
				{
					"name":       "cdex-verify-signature",
					"definition": "http://hl7.org/fhir/us/davinci-cdex/OperationDefinition/Bundle-cdex-verify-signature",
				},
				{
					"name":       "ecr-submit",
					"definition": "http://hl7.org/fhir/us/ecr/OperationDefinition/Bundle-ecr-submit",
				},
				{
					"name":       "cancer-registry-submit",
					"definition": "http://hl7.org/fhir/us/central-cancer-registry-reporting/OperationDefinition/Bundle-cancer-registry-submit",
				},
			}
		case "HealthcareService":
			entry["operation"] = []map[string]string{
				{
					"name":       "find-services",
					"definition": "http://hl7.org/fhir/us/davinci-pdex-plan-net/OperationDefinition/find-services",
				},
				{
					"name":       "fast-attest",
					"definition": "http://hl7.org/fhir/us/ndh/OperationDefinition/HealthcareService-fast-attest",
				},
			}
		case "OrganizationAffiliation":
			entry["operation"] = []map[string]string{
				{
					"name":       "fast-network-query",
					"definition": "http://hl7.org/fhir/us/ndh/OperationDefinition/OrganizationAffiliation-fast-network-query",
				},
			}
		case "VerificationResult":
			entry["operation"] = []map[string]string{
				{
					"name":       "ndh-verify",
					"definition": "http://hl7.org/fhir/us/ndh/OperationDefinition/VerificationResult-ndh-verify",
				},
			}
		case "Consent":
			entry["operation"] = []map[string]string{
				{
					"name":       "evaluate",
					"definition": "http://flint.fhir.org/OperationDefinition/Consent-evaluate",
				},
				{
					"name":       "adi-verify",
					"definition": "http://hl7.org/fhir/us/pacio-adi/OperationDefinition/Consent-adi-verify",
				},
				{
					"name":       "sdoh-consent-record",
					"definition": "http://hl7.org/fhir/us/sdoh-clinicalcare/OperationDefinition/Consent-sdoh-consent-record",
				},
			}
		case "ExplanationOfBenefit":
			entry["operation"] = []map[string]string{
				{
					"name":       "carin-bb-summary",
					"definition": "http://hl7.org/fhir/us/carin-bb/OperationDefinition/carin-bb-summary",
				},
				{
					"name":       "pct-aeob-summary",
					"definition": "http://hl7.org/fhir/us/davinci-pct/OperationDefinition/ExplanationOfBenefit-pct-aeob-summary",
				},
				{
					"name":       "carin-bb-claims",
					"definition": "http://hl7.org/fhir/us/carin-bb/OperationDefinition/ExplanationOfBenefit-carin-bb-claims",
				},
			}
		case "TestScript":
			entry["operation"] = []map[string]string{
				{
					"name":       "evaluate",
					"definition": "http://hl7.org/fhir/OperationDefinition/TestScript-evaluate",
				},
			}
		}
		resEntries = append(resEntries, entry)
	}
	resEntries = append(resEntries, map[string]any{
		"type":              "ViewDefinition",
		"profile":           "http://hl7.org/fhir/uv/sql-on-fhir/StructureDefinition/ViewDefinition",
		"versioning":        "versioned-update",
		"readHistory":       false,
		"updateCreate":      true,
		"conditionalCreate": false,
		"conditionalRead":   "not-supported",
		"conditionalUpdate": false,
		"conditionalDelete": "not-supported",
		"interaction": []map[string]string{
			{"code": "read"},
			{"code": "create"},
			{"code": "update"},
			{"code": "delete"},
			{"code": "search-type"},
		},
		"operation": []map[string]string{
			{
				"name":       "run",
				"definition": "http://hl7.org/fhir/uv/sql-on-fhir/OperationDefinition/ViewDefinition-run",
			},
			{
				"name":       "export",
				"definition": "http://hl7.org/fhir/uv/sql-on-fhir/OperationDefinition/ViewDefinition-export",
			},
			{
				"name":       "sql",
				"definition": "http://flint.fhir.org/OperationDefinition/ViewDefinition-sql",
			},
		},
	})

	smartCfg := s.resolveSMARTConfig(r)

	cap := map[string]any{
		"resourceType": "CapabilityStatement",
		"status":       "active",
		"kind":         "instance",
		"fhirVersion":  "4.0.1",
		"instantiates": []string{
			"http://hl7.org/fhir/us/core/CapabilityStatement/us-core-server",
			"http://hl7.org/fhir/uv/bulkdata/CapabilityStatement/bulk-data",
		},
		"format":      []string{"json", "application/fhir+json", "application/fhir+ndjson"},
		"patchFormat": []string{"application/json-patch+json", "application/merge-patch+json", "application/fhir+json"},
		"software": map[string]any{
			"name":    "Flint FHIR Server",
			"version": "0.1.0",
		},
		"rest": []map[string]any{
			{
				"mode": "server",
				"security": map[string]any{
					"cors": true,
					"service": []map[string]any{
						{
							"coding": []map[string]string{
								{
									"system":  "http://terminology.hl7.org/CodeSystem/restful-security-service",
									"code":    "SMART-on-FHIR",
									"display": "SMART-on-FHIR",
								},
							},
							"text": "OAuth2 using SMART-on-FHIR v2.0.0 profile (see http://hl7.org/fhir/smart-app-launch)",
						},
					},
					"extension": []map[string]any{
						{
							"url": "http://fhir-registry.smarthealthit.org/StructureDefinition/oauth-uris",
							"extension": []map[string]string{
								{"url": "authorize", "valueUri": smartCfg.AuthorizationEndpoint},
								{"url": "token", "valueUri": smartCfg.TokenEndpoint},
								{"url": "introspect", "valueUri": smartCfg.IntrospectionEndpoint},
							},
						},
					},
				},
				"resource": resEntries,
				"interaction": []map[string]string{
					{"code": "transaction"},
					{"code": "batch"},
					{"code": "search-system"},
					{"code": "history-system"},
				},
				"compartment": []string{
					"http://hl7.org/fhir/CompartmentDefinition/patient",
					"http://hl7.org/fhir/CompartmentDefinition/encounter",
					"http://hl7.org/fhir/CompartmentDefinition/practitioner",
					"http://hl7.org/fhir/CompartmentDefinition/relatedPerson",
					"http://hl7.org/fhir/CompartmentDefinition/device",
				},
				"operation": []map[string]any{
					{
						"name":       "validate",
						"definition": "http://hl7.org/fhir/OperationDefinition/Resource-validate",
					},
					{
						"name":       "graph",
						"definition": "http://hl7.org/fhir/OperationDefinition/Resource-graph",
					},
					{
						"name":       "everything",
						"definition": "http://hl7.org/fhir/OperationDefinition/Patient-everything",
					},
					{
						"name":       "summary",
						"definition": "http://hl7.org/fhir/uv/ips/OperationDefinition/summary",
					},
					{
						"name":       "docref",
						"definition": "http://hl7.org/fhir/us/core/OperationDefinition/docref",
					},
					{
						"name":       "provenance",
						"definition": "http://flint.fhir.org/OperationDefinition/Resource-provenance",
					},
					{
						"name":       "document",
						"definition": "http://hl7.org/fhir/OperationDefinition/Composition-document",
					},
					{
						"name":       "match",
						"definition": "http://hl7.org/fhir/OperationDefinition/Patient-match",
					},
					{
						"name":       "member-match",
						"definition": "http://hl7.org/fhir/us/davinci-hrex/OperationDefinition/member-match",
					},
					{
						"name":       "submit",
						"definition": "http://hl7.org/fhir/OperationDefinition/Claim-submit",
					},
					{
						"name":       "populate",
						"definition": "http://hl7.org/fhir/uv/sdc/OperationDefinition/Questionnaire-populate",
					},
					{
						"name":       "extract",
						"definition": "http://hl7.org/fhir/uv/sdc/OperationDefinition/QuestionnaireResponse-extract",
					},
					{
						"name":       "evaluate-measure",
						"definition": "http://hl7.org/fhir/OperationDefinition/Measure-evaluate-measure",
					},
					{
						"name":       "care-gaps",
						"definition": "http://hl7.org/fhir/us/davinci-deqm/OperationDefinition/care-gaps",
					},
					{
						"name":       "meta",
						"definition": "http://hl7.org/fhir/OperationDefinition/Resource-meta",
					},
					{
						"name":       "meta-add",
						"definition": "http://hl7.org/fhir/OperationDefinition/Resource-meta-add",
					},
					{
						"name":       "meta-delete",
						"definition": "http://hl7.org/fhir/OperationDefinition/Resource-meta-delete",
					},
					{
						"name":       "export",
						"definition": "http://hl7.org/fhir/uv/bulkdata/OperationDefinition/export",
					},
					{
						"name":       "import",
						"definition": "http://hl7.org/fhir/uv/bulkdata/OperationDefinition/import",
					},
					{
						"name":       "purge",
						"definition": "http://hl7.org/fhir/OperationDefinition/Patient-purge",
					},
					{
						"name":       "expunge",
						"definition": "http://hl7.org/fhir/OperationDefinition/Resource-expunge",
					},
					{
						"name":       "reindex",
						"definition": "http://flint.fhir.org/OperationDefinition/Resource-reindex",
					},
					{
						"name":       "apply",
						"definition": "http://hl7.org/fhir/OperationDefinition/PlanDefinition-apply",
					},
					{
						"name":       "diff",
						"definition": "http://hl7.org/fhir/OperationDefinition/Resource-diff",
					},
					{
						"name":       "expand",
						"definition": "http://hl7.org/fhir/OperationDefinition/ValueSet-expand",
					},
					{
						"name":       "validate-code",
						"definition": "http://hl7.org/fhir/OperationDefinition/ValueSet-validate-code",
					},
					{
						"name":       "lookup",
						"definition": "http://hl7.org/fhir/OperationDefinition/CodeSystem-lookup",
					},
					{
						"name":       "subsumes",
						"definition": "http://hl7.org/fhir/OperationDefinition/CodeSystem-subsumes",
					},
					{
						"name":       "translate",
						"definition": "http://hl7.org/fhir/OperationDefinition/ConceptMap-translate",
					},
					{
						"name":       "closure",
						"definition": "http://hl7.org/fhir/OperationDefinition/ConceptMap-closure",
					},
					{
						"name":       "data-requirements",
						"definition": "http://hl7.org/fhir/OperationDefinition/Library-data-requirements",
					},
					{
						"name":       "fhirpath",
						"definition": "http://flint.fhir.org/OperationDefinition/Resource-fhirpath",
					},
					{
						"name":       "events",
						"definition": "http://hl7.org/fhir/uv/subscriptions-backport/OperationDefinition/backport-subscription-events",
					},
					{
						"name":       "collect-data",
						"definition": "http://hl7.org/fhir/OperationDefinition/Measure-collect-data",
					},
					{
						"name":       "submit-data",
						"definition": "http://hl7.org/fhir/OperationDefinition/Measure-submit-data",
					},
					{
						"name":       "add",
						"definition": "http://hl7.org/fhir/OperationDefinition/Group-add",
					},
					{
						"name":       "remove",
						"definition": "http://hl7.org/fhir/OperationDefinition/Group-remove",
					},
					{
						"name":       "lastn",
						"definition": "http://hl7.org/fhir/OperationDefinition/Observation-lastn",
					},
					{
						"name":       "stats",
						"definition": "http://hl7.org/fhir/OperationDefinition/Observation-stats",
					},
					{
						"name":       "merge",
						"definition": "http://hl7.org/fhir/OperationDefinition/Patient-merge",
					},
					{
						"name":       "find",
						"definition": "http://fhir.org/guides/argonaut/scheduling/OperationDefinition/appointment-find",
					},
					{
						"name":       "hold",
						"definition": "http://fhir.org/guides/argonaut/scheduling/OperationDefinition/appointment-hold",
					},
					{
						"name":       "book",
						"definition": "http://fhir.org/guides/argonaut/scheduling/OperationDefinition/appointment-book",
					},
					{
						"name":       "subset",
						"definition": "http://hl7.org/fhir/OperationDefinition/CapabilityStatement-subset",
					},
					{
						"name":       "implements",
						"definition": "http://hl7.org/fhir/OperationDefinition/CapabilityStatement-implements",
					},
					{
						"name":       "snapshot",
						"definition": "http://hl7.org/fhir/OperationDefinition/StructureDefinition-snapshot",
					},
					{
						"name":       "process-message",
						"definition": "http://hl7.org/fhir/OperationDefinition/MessageHeader-process-message",
					},
					{
						"name":       "transform",
						"definition": "http://hl7.org/fhir/OperationDefinition/StructureMap-transform",
					},
					{
						"name":       "preferred-id",
						"definition": "http://hl7.org/fhir/OperationDefinition/NamingSystem-preferred-id",
					},
					{
						"name":       "inquire",
						"definition": "http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-inquiry",
					},
					{
						"name":       "package",
						"definition": "http://hl7.org/fhir/us/davinci-dtr/OperationDefinition/questionnaire-package",
					},
					{
						"name":       "find-matches",
						"definition": "http://hl7.org/fhir/OperationDefinition/CodeSystem-find-matches",
					},
					{
						"name":       "convert",
						"definition": "http://hl7.org/fhir/OperationDefinition/Resource-convert",
					},
					{
						"name":       "submit-attachment",
						"definition": "http://hl7.org/fhir/us/davinci-cdex/OperationDefinition/submit-attachment",
					},
					{
						"name":       "evaluate",
						"definition": "http://hl7.org/fhir/uv/cpg/OperationDefinition/cpg-library-evaluate",
					},
					{
						"name":       "health-cards-issue",
						"definition": "http://hl7.org/fhir/uv/shc-vaccination/OperationDefinition/Patient-i-health-cards-issue",
					},
					{
						"name":       "pcde",
						"definition": "http://hl7.org/fhir/us/davinci-pcde/OperationDefinition/PCDE",
					},
					{
						"name":       "evaluate-eligibility",
						"definition": "http://flint.fhir.org/OperationDefinition/ResearchStudy-evaluate-eligibility",
					},
					{
						"name":       "predetermine",
						"definition": "http://hl7.org/fhir/OperationDefinition/Claim-predetermine",
					},
					{
						"name":       "evaluate-risk-condition-category",
						"definition": "http://hl7.org/fhir/us/davinci-ra/OperationDefinition/evaluate-risk-condition-category",
					},
					{
						"name":       "crd",
						"definition": "http://hl7.org/fhir/us/davinci-crd/OperationDefinition/crd",
					},
					{
						"name":       "immds-forecast",
						"definition": "http://hl7.org/fhir/us/immds/OperationDefinition/immds-forecast",
					},
					{
						"name":       "gfe-submit",
						"definition": "http://hl7.org/fhir/us/davinci-pct/OperationDefinition/gfe-submit",
					},
					{
						"name":       "member-add",
						"definition": "http://hl7.org/fhir/us/davinci-atr/OperationDefinition/member-add",
					},
					{
						"name":       "member-remove",
						"definition": "http://hl7.org/fhir/us/davinci-atr/OperationDefinition/member-remove",
					},
					{
						"name":       "confirm-attribution-list",
						"definition": "http://hl7.org/fhir/us/davinci-atr/OperationDefinition/confirm-attribution-list",
					},
					{
						"name":       "check-interactions",
						"definition": "http://flint.fhir.org/OperationDefinition/MedicationRequest-check-interactions",
					},
					{
						"name":       "process-notification",
						"definition": "http://hl7.org/fhir/us/davinci-notifications/OperationDefinition/process-notification",
					},
					{
						"name":       "ihe-pix",
						"definition": "https://profiles.ihe.net/ITI/PIXm/OperationDefinition/IHE.PIXm.pix",
					},
					{
						"name":       "collect-specimen",
						"definition": "http://flint.fhir.org/OperationDefinition/ServiceRequest-collect-specimen",
					},
					{
						"name":       "bulk-member-match",
						"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/BulkMemberMatch",
					},
					{
						"name":       "davinci-data-export",
						"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/DavinciDataExport",
					},
					{
						"name":       "admit",
						"definition": "http://flint.fhir.org/OperationDefinition/Encounter-admit",
					},
					{
						"name":       "transfer",
						"definition": "http://flint.fhir.org/OperationDefinition/Encounter-transfer",
					},
					{
						"name":       "discharge",
						"definition": "http://flint.fhir.org/OperationDefinition/Encounter-discharge",
					},
					{
						"name":       "dispense",
						"definition": "http://flint.fhir.org/OperationDefinition/MedicationRequest-dispense",
					},
					{
						"name":       "find-providers",
						"definition": "http://hl7.org/fhir/us/davinci-pdex-plan-net/OperationDefinition/find-providers",
					},
					{
						"name":       "find-services",
						"definition": "http://hl7.org/fhir/us/davinci-pdex-plan-net/OperationDefinition/find-services",
					},
					{
						"name":       "sdoh-referral",
						"definition": "http://hl7.org/fhir/us/sdoh-clinicalcare/OperationDefinition/sdoh-referral",
					},
					{
						"name":       "sdoh-summary",
						"definition": "http://hl7.org/fhir/us/sdoh-clinicalcare/OperationDefinition/sdoh-summary",
					},
					{
						"name":       "carin-bb",
						"definition": "http://hl7.org/fhir/us/carin-bb/OperationDefinition/carin-bb",
					},
					{
						"name":       "carin-bb-summary",
						"definition": "http://hl7.org/fhir/us/carin-bb/OperationDefinition/carin-bb-summary",
					},
					{
						"name":       "ecr-report",
						"definition": "http://hl7.org/fhir/us/ecr/OperationDefinition/Encounter-ecr-report",
					},
					{
						"name":       "ersd-lookup",
						"definition": "http://hl7.org/fhir/us/ecr/OperationDefinition/ersd-lookup",
					},
					{
						"name":       "decease",
						"definition": "http://hl7.org/fhir/us/vrdr/OperationDefinition/Patient-decease",
					},
					{
						"name":       "mcode-summary",
						"definition": "http://hl7.org/fhir/us/mcode/OperationDefinition/Patient-mcode-summary",
					},
					{
						"name":       "stage-cancer",
						"definition": "http://hl7.org/fhir/us/mcode/OperationDefinition/Condition-stage-cancer",
					},
					{
						"name":       "formulary-lookup",
						"definition": "http://hl7.org/fhir/us/davinci-drug-formulary/OperationDefinition/MedicationKnowledge-formulary-lookup",
					},
					{
						"name":       "formulary-coverage",
						"definition": "http://hl7.org/fhir/us/davinci-drug-formulary/OperationDefinition/Coverage-formulary-coverage",
					},
					{
						"name":       "iti-65",
						"definition": "https://profiles.ihe.net/ITI/MHD/OperationDefinition/IHE.MHD.ProvideBundle",
					},
					{
						"name":       "iti-67",
						"definition": "https://profiles.ihe.net/ITI/MHD/OperationDefinition/IHE.MHD.FindDocumentReferences",
					},
					{
						"name":       "find-subject-variants",
						"definition": "http://hl7.org/fhir/uv/genomics-reporting/OperationDefinition/find-subject-variants",
					},
					{
						"name":       "find-subject-tx-implications",
						"definition": "http://hl7.org/fhir/uv/genomics-reporting/OperationDefinition/find-subject-tx-implications",
					},
					{
						"name":       "pacio-summary",
						"definition": "http://hl7.org/fhir/us/pacio-fs/OperationDefinition/pacio-summary",
					},
					{
						"name":       "pacio-assess",
						"definition": "http://hl7.org/fhir/us/pacio-fs/OperationDefinition/pacio-assess",
					},
					{
						"name":       "nhsn-hai-report",
						"definition": "http://hl7.org/fhir/us/nhsn-dqm/OperationDefinition/Encounter-nhsn-hai-report",
					},
					{
						"name":       "nhsn-acute-care",
						"definition": "http://hl7.org/fhir/us/nhsn-dqm/OperationDefinition/Measure-nhsn-acute-care",
					},
					{
						"name":       "ra-evaluate-risk",
						"definition": "http://hl7.org/fhir/us/davinci-ra/OperationDefinition/MeasureReport-ra-evaluate-risk",
					},
					{
						"name":       "ra-coding-gap-report",
						"definition": "http://hl7.org/fhir/us/davinci-ra/OperationDefinition/Patient-ra-coding-gap-report",
					},
					{
						"name":       "cdex-submit-attachment",
						"definition": "http://hl7.org/fhir/us/davinci-cdex/OperationDefinition/Task-cdex-submit-attachment",
					},
					{
						"name":       "nhsn-dqm-evaluate",
						"definition": "http://hl7.org/fhir/us/nhsn-dqm/OperationDefinition/Measure-nhsn-dqm-evaluate",
					},
					{
						"name":       "nhsn-ach-summary",
						"definition": "http://hl7.org/fhir/us/nhsn-dqm/OperationDefinition/Patient-nhsn-ach-summary",
					},
					{
						"name":       "dtr-questionnaire-package",
						"definition": "http://hl7.org/fhir/us/davinci-dtr/OperationDefinition/Questionnaire-dtr-questionnaire-package",
					},
					{
						"name":       "pas-inquire",
						"definition": "http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-pas-inquire",
					},
					{
						"name":       "burden-reduction-summary",
						"definition": "http://hl7.org/fhir/us/davinci-crd/OperationDefinition/Patient-burden-reduction-summary",
					},
					{
						"name":       "mcode-trial-summary",
						"definition": "http://hl7.org/fhir/us/codex-clinical-trial-matching/OperationDefinition/Patient-mcode-trial-summary",
					},
					{
						"name":       "ndh-verify",
						"definition": "http://hl7.org/fhir/us/ndh/OperationDefinition/VerificationResult-ndh-verify",
					},
					{
						"name":       "ndh-directory-summary",
						"definition": "http://hl7.org/fhir/us/ndh/OperationDefinition/Organization-ndh-directory-summary",
					},
					{
						"name":       "udap-register",
						"definition": "http://hl7.org/fhir/us/udap-security/OperationDefinition/Organization-udap-register",
					},
					{
						"name":       "udap-trust-summary",
						"definition": "http://hl7.org/fhir/us/udap-security/OperationDefinition/Organization-udap-trust-summary",
					},
					{
						"name":       "pdex-provider-member-match",
						"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/Group-pdex-provider-member-match",
					},
					{
						"name":       "pdex-provider-everything",
						"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/Patient-pdex-provider-everything",
					},
					{
						"name":       "pdex-payer-exchange",
						"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/Patient-pdex-payer-exchange",
					},
					{
						"name":       "pdex-payer-summary",
						"definition": "http://hl7.org/fhir/us/davinci-pdex/OperationDefinition/Patient-pdex-payer-summary",
					},
					{
						"name":       "run",
						"definition": "http://hl7.org/fhir/uv/sql-on-fhir/OperationDefinition/ViewDefinition-run",
					},
					{
						"name":       "sql",
						"definition": "http://flint.fhir.org/OperationDefinition/ViewDefinition-sql",
					},
					{
						"name":       "sync-analytics",
						"definition": "http://flint.fhir.org/OperationDefinition/System-sync-analytics",
					},
				},
			},
		},
	}

	return cap
}

// protoToFHIRMapWithMeta decodes binary wire Protobuf bytes directly into an in-memory
// FHIR R4 map[string]any using protoreflect without any intermediate JSON serialization.
func (s *Server) protoToFHIRMapWithMeta(resType string, protoBytes []byte, version int, lastUpdated time.Time) (map[string]any, error) {
	md := s.protoRegistry[resType]
	return fhirutil.ProtoBytesToFHIRMap(md, resType, protoBytes, version, lastUpdated)
}

// protoToJSONWithMeta converts proto bytes to standard flat HL7 FHIR R4 JSON using the registered message type.
func (s *Server) protoToJSONWithMeta(resType string, protoBytes []byte, version int, lastUpdated time.Time) ([]byte, error) {
	md, ok := s.protoRegistry[resType]
	if !ok {
		// Fallback: try to use Any wrapper
		var anyMsg anypb.Any
		if err := proto.Unmarshal(protoBytes, &anyMsg); err != nil {
			return nil, fmt.Errorf("no registered type for %s and proto unmarshal failed: %w", resType, err)
		}
		return protojson.Marshal(&anyMsg)
	}

	_ = protodesc.ToFileDescriptorProto // ensure import is used
	return fhirutil.DenormalizeProtoJSONToFHIRJSON(md, resType, protoBytes, version, lastUpdated)
}

// writeOperationOutcome writes a FHIR OperationOutcome error response.
func writeOperationOutcome(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"resourceType": "OperationOutcome",
		"issue": []map[string]any{
			{
				"severity":    "error",
				"code":        code,
				"diagnostics": message,
			},
		},
	})
}

// parsePreferReturn extracts the FHIR Return Preference ("minimal", "representation", or "OperationOutcome")
// from the HTTP Prefer header per FHIR R4 §3.1.0.5.2. Returns "" when unset.
func parsePreferReturn(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, h := range r.Header.Values("Prefer") {
		for part := range strings.SplitSeq(h, ",") {
			for kv := range strings.SplitSeq(part, ";") {
				kv = strings.TrimSpace(kv)
				if strings.HasPrefix(strings.ToLower(kv), "return=") {
					val := strings.Trim(strings.TrimSpace(kv[len("return="):]), `"`)
					switch strings.ToLower(val) {
					case "minimal":
						return "minimal"
					case "representation":
						return "representation"
					case "operationoutcome":
						return "OperationOutcome"
					}
				}
			}
		}
	}
	return ""
}

// writePreferResourceResponse writes a resource write response honoring the HTTP Prefer: return=... header.
func writePreferResourceResponse(w http.ResponseWriter, r *http.Request, status int, resType, resID string, version int, jsonBytes []byte) {
	switch parsePreferReturn(r) {
	case "minimal":
		w.WriteHeader(status)
	case "OperationOutcome":
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resourceType": "OperationOutcome",
			"issue": []map[string]any{
				{
					"severity":    "information",
					"code":        "informational",
					"diagnostics": fmt.Sprintf("%s/%s version %d stored", resType, resID, version),
				},
			},
		})
	default:
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(jsonBytes)
	}
}
