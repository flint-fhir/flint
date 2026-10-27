// Package server implements the Flint FHIR R4 REST API.
//
// This serves FHIR resources from the Postgres operational store.
// Resources are stored as proto blobs and converted to JSON on read.
package server

import (
	"bytes"
	"context"
	"database/sql"
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
	"github.com/flint-fhir/flint/store/postgres"
)

var idSeq atomic.Uint64

// Server is the Flint FHIR REST API server.
type Server struct {
	store  *postgres.Store
	logger *slog.Logger
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

// New creates a new FHIR server.
func New(store *postgres.Store, logger *slog.Logger) *Server {
	var auditRec audit.Recorder
	if store != nil {
		auditRec = audit.NewStoreRecorder(store)
	} else {
		auditRec = audit.NewMemoryRecorder()
	}
	devAuth, _ := auth.NewMockTokenValidator("", "")
	return &Server{
		store:         store,
		logger:        logger,
		protoRegistry: make(map[string]protoreflect.MessageDescriptor),
		extractors:    postgres.DefaultIndexExtractors(),
		devAuth:       devAuth,
		oauthCodes:    make(map[string]authCodeEntry),
		refreshTokens: make(map[string]refreshTokenEntry),
		revokedTokens: make(map[string]time.Time),
		clientJWKs:    make(map[string]auth.JWK),
		validator:     validation.NewEngine(validation.DefaultOptions()),
		auditRecorder: auditRec,
		exportJobs:    make(map[string]*exportJob),
		subSem:        make(chan struct{}, 32),
		metrics:       newServerMetrics(),
	}
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

	// Discovery & OpenAPI 3.1 / Interactive Explorer / CDS Hooks v2.0 endpoints
	mux.HandleFunc("GET /fhir/r4/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /fhir/r4/$docs", s.handleDocsUI)
	mux.HandleFunc("GET /fhir/r4/{tenant}/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$docs", s.handleDocsUI)
	mux.HandleFunc("GET /cds-services", s.handleCDSServicesDiscovery)
	mux.HandleFunc("POST /cds-services/{serviceId}", s.handleCDSServiceInvoke)
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
	mux.HandleFunc("POST /fhir/r4/{tenant}/$diff", s.handleDiff)
	mux.HandleFunc("POST /fhir/r4/{tenant}/{resourceType}/$diff", s.handleDiff)
	mux.HandleFunc("GET /fhir/r4/{tenant}/$certify", s.handleCertify)
	mux.HandleFunc("POST /fhir/r4/{tenant}/$certify", s.handleCertify)

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

	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isGetOrHead := r.Method == http.MethodGet || r.Method == http.MethodHead
		if isGetOrHead || r.Method == http.MethodPost {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 4 && parts[0] == "fhir" && parts[1] == "r4" && parts[2] == "cds-services" && r.Method == http.MethodPost {
				r.SetPathValue("serviceId", parts[3])
				s.handleCDSServiceInvoke(w, r)
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
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Patient") && parts[4] == "$member-match" {
					r.SetPathValue("tenant", parts[2])
					s.handlePatientMemberMatch(w, r)
					return
				}
				if r.Method == http.MethodPost && strings.EqualFold(parts[3], "Claim") && parts[4] == "$submit" {
					r.SetPathValue("tenant", parts[2])
					s.handleClaimSubmit(w, r)
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
			}
			if len(parts) == 6 && parts[0] == "fhir" && parts[1] == "r4" {
				if parts[5] == "$everything" {
					r.SetPathValue("tenant", parts[2])
					r.SetPathValue("resourceType", parts[3])
					r.SetPathValue("id", parts[4])
					s.handlePatientEverything(w, r)
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
				if isGetOrHead && strings.EqualFold(parts[3], "Patient") && !strings.HasPrefix(parts[5], "$") && parts[5] != "_history" {
					r.SetPathValue("tenant", parts[2])
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

	count := 100
	offset := 0
	var since *time.Time
	if v := strings.TrimSpace(r.URL.Query().Get("_count")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _count parameter %q: must be a positive integer", v))
			return
		}
		count = n
	}
	if v := strings.TrimSpace(r.URL.Query().Get("_offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _offset parameter %q: must be a non-negative integer", v))
			return
		}
		offset = n
	}
	if sinceRaw := strings.TrimSpace(r.URL.Query().Get("_since")); sinceRaw != "" {
		dOp, err := postgres.ParseDateOp(sinceRaw)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _since parameter %q: %v", sinceRaw, err))
			return
		}
		t := dOp.Value
		since = &t
	}

	records, total, err := s.store.ListHistory(r.Context(), tenant, resType, resID, since, count, offset)
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
				"etag":         postgres.FormatETag(rec.ResVersion),
				"lastModified": rec.LastUpdated.UTC().Format(time.RFC3339),
			},
		}

		if !rec.IsDeleted && len(rec.ResourceProto) > 0 {
			if jsonBytes, err := s.protoToJSONWithMeta(rt, rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
				var resource any
				if err := json.Unmarshal(jsonBytes, &resource); err == nil {
					entry["resource"] = resource
				}
			}
		}

		entries = append(entries, entry)
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "history",
		"total":        total,
		"entry":        entries,
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
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	rec, err := s.store.DeleteResourceWithMeta(r.Context(), tenant, resType, resID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.logger.Error("delete resource", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to delete resource")
		return
	}
	if rec != nil && rec.ResVersion > 0 {
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

// handlePatientEverything handles GET/POST /{tenant}/{resourceType}/{id}/$everything — assemble full Patient or Encounter compartment Bundle.
func (s *Server) handlePatientEverything(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := strings.TrimSpace(r.PathValue("resourceType"))
	if resType == "" {
		resType = "Patient"
	}
	if !strings.EqualFold(resType, "Patient") && !strings.EqualFold(resType, "Encounter") {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("$everything is only supported on Patient and Encounter (got %s)", resType))
		return
	}
	resID := r.PathValue("id")
	q := mergedSearchQuery(r)

	typeRaw := strings.TrimSpace(q.Get("_type"))
	sinceRaw := strings.TrimSpace(q.Get("_since"))
	countRaw := strings.TrimSpace(q.Get("_count"))
	offsetRaw := strings.TrimSpace(q.Get("_offset"))
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
						case "_type":
							if val != "" && typeRaw == "" {
								typeRaw = val
							}
						case "_since":
							if val != "" && sinceRaw == "" {
								sinceRaw = val
							}
						case "_count":
							if val != "" && countRaw == "" {
								countRaw = val
							}
						case "_offset":
							if val != "" && offsetRaw == "" {
								offsetRaw = val
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
	if hasSecCtx {
		for _, rt := range resourceTypes {
			if !auth.Allows(secCtx, rt, auth.ActionRead) && !auth.Allows(secCtx, rt, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for $everything _type %s", rt))
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
	if strings.EqualFold(resType, "Encounter") {
		targetLabel = "Encounter"
		sr, err = s.store.EncounterEverything(r.Context(), postgres.EncounterEverythingParams{
			TenantID:      tenant,
			EncounterID:   resID,
			ResourceTypes: resourceTypes,
			Since:         sinceTime,
			Count:         count,
			Offset:        offset,
		})
	} else {
		sr, err = s.store.PatientEverything(r.Context(), postgres.PatientEverythingParams{
			TenantID:      tenant,
			PatientID:     resID,
			ResourceTypes: resourceTypes,
			Since:         sinceTime,
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
		Count:   count,
		Offset:  offset,
		ResType: "$everything",
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

// handleCompartmentSearch handles GET /{tenant}/Patient/{patientId}/{resourceType} — compartment search.
func (s *Server) handleCompartmentSearch(w http.ResponseWriter, r *http.Request) {
	patientID := cleanID(r.PathValue("patientId"))
	resType := r.PathValue("resourceType")

	if resType == "AuditEvent" {
		q := r.URL.Query()
		q.Set("patient", patientID)
		r.URL.RawQuery = q.Encode()
		s.handleSearchAuditEvents(w, r)
		return
	}

	if _, ok := s.protoRegistry[resType]; !ok {
		writeOperationOutcome(w, http.StatusBadRequest, "not-supported",
			fmt.Sprintf("resource type %s not registered", resType))
		return
	}

	q := r.URL.Query()
	if strings.EqualFold(resType, "Patient") {
		q.Set("_id", patientID)
	} else {
		q.Set("patient", "Patient/"+patientID)
	}
	r.URL.RawQuery = q.Encode()
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

func applySummaryOrElements(resource map[string]any, summary string, elements []string) map[string]any {
	if resource == nil {
		return nil
	}
	out := make(map[string]any, len(resource))

	switch {
	case len(elements) > 0:
		keep := map[string]bool{
			"resourceType": true,
			"id":           true,
			"meta":         true,
		}
		var prefixes []string
		for _, el := range elements {
			keep[el] = true
			prefixes = append(prefixes, el)
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
			}
		case "Encounter":
			entry["operation"] = []map[string]string{
				{
					"name":       "everything",
					"definition": "http://hl7.org/fhir/OperationDefinition/Encounter-everything",
				},
			}
		case "Composition":
			entry["operation"] = []map[string]string{
				{
					"name":       "document",
					"definition": "http://hl7.org/fhir/OperationDefinition/Composition-document",
				},
			}
		case "DocumentReference":
			entry["operation"] = []map[string]string{
				{
					"name":       "docref",
					"definition": "http://hl7.org/fhir/us/core/OperationDefinition/docref",
				},
			}
		case "QuestionnaireResponse":
			entry["operation"] = []map[string]string{
				{
					"name":       "extract",
					"definition": "http://hl7.org/fhir/uv/sdc/OperationDefinition/QuestionnaireResponse-extract",
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
			}
		case "Composition":
			entry["operation"] = []map[string]string{
				{
					"name":       "document",
					"definition": "http://hl7.org/fhir/OperationDefinition/Composition-document",
				},
			}
		case "DocumentReference":
			entry["operation"] = []map[string]string{
				{
					"name":       "docref",
					"definition": "http://hl7.org/fhir/us/core/OperationDefinition/docref",
				},
			}
		case "Questionnaire":
			entry["operation"] = []map[string]string{
				{
					"name":       "populate",
					"definition": "http://hl7.org/fhir/uv/sdc/OperationDefinition/Questionnaire-populate",
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
			}
		case "ConceptMap":
			entry["operation"] = []map[string]string{
				{
					"name":       "translate",
					"definition": "http://hl7.org/fhir/OperationDefinition/ConceptMap-translate",
				},
			}
		case "Subscription":
			entry["operation"] = []map[string]string{
				{
					"name":       "status",
					"definition": "http://hl7.org/fhir/uv/subscriptions-backport/OperationDefinition/backport-subscription-status",
				},
			}
		}
		resEntries = append(resEntries, entry)
	}

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
				},
			},
		},
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(cap)
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
