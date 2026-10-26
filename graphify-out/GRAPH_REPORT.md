# Graph Report - flint  (2026-10-26)

## Corpus Check
- 148 files · ~357,824 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 974 nodes · 2607 edges · 55 communities (43 shown, 10 thin omitted)
- Extraction: 94% EXTRACTED · 6% INFERRED · 0% AMBIGUOUS · INFERRED: 144 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `e7cfede6`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- context.Context
- New
- Run
- SmartScope
- net/http.Request
- auth.go
- SearchIndexes
- SecurityContext
- cleanID
- SmartConfiguration
- auth.pb.go
- MockTokenValidator
- Enforcement Guidelines
- ingest.go
- OIDCValidator
- SearchParams
- Producer
- Flint — High-Performance Open Source FHIR R4 Server
- query_duckdb.py
- query_iceberg.py
- github.com/flint-fhir/flint
- rules/graphify.md
- workflows/graphify.md
- flint-analytics
- json_codec.go
- audit/audit.go
- testing.T
- Outcome
- Action
- NewEngine
- Server
- Security Policy
- google.golang.org/protobuf/reflect/protoreflect.Message
- time.Time
- Contributing to Flint
- sdc_document.go
- Contributor Covenant Code of Conduct
- patch.go
- Server
- time.Duration
- API Reference
- main
- .authMiddleware
- SMART on FHIR v2 & Identity Architecture
- .resolveValueSetConcepts
- TestSchema_NoJSONBAndAtlasSumSync
- HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness
- operations.go
- DefaultIndexExtractors
- FromContext
- Installation & Quickstart (Standalone FHIR R4 Server)
- Optional Add-Ons (Streaming CDC, Temporal Workflows & Clinical Lakehouse)
- .RegisterResourceType

## God Nodes (most connected - your core abstractions)
1. `Server` - 60 edges
2. `SearchIndexes` - 45 edges
3. `writeOperationOutcome()` - 44 edges
4. `cleanID()` - 37 edges
5. `FromContext()` - 33 edges
6. `IsPatientRestricted()` - 31 edges
7. `Store` - 28 edges
8. `SecurityContext` - 27 edges
9. `New()` - 25 edges
10. `IsPatientCompartmentResourceType()` - 22 edges

## Surprising Connections (you probably didn't know these)
- `main()` --calls--> `NewProducer()`  [EXTRACTED]
  cmd/flintd/main.go → automq/producer.go
- `main()` --calls--> `Run()`  [EXTRACTED]
  cmd/certify/main.go → pkg/certify/certify.go
- `main()` --calls--> `NewOIDCValidator()`  [EXTRACTED]
  cmd/flintd/main.go → pkg/auth/validator_oidc.go
- `main()` --calls--> `NewZitadelValidator()`  [EXTRACTED]
  cmd/flintd/main.go → pkg/auth/validator_zitadel.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/flintd/main.go → server/server.go

## Import Cycles
- None detected.

## Communities (55 total, 10 thin omitted)

### Community 0 - "context.Context"
Cohesion: 0.08
Nodes (29): MemoryRecorder, StoreRecorder, context.Context, database/sql.Tx, sync.Map, BundleOperation, BundleOperationType, BundleOpResult (+21 more)

### Community 1 - "New"
Cohesion: 0.17
Nodes (26): main(), TestSeedRun_EndToEndIntegration(), database/sql.DB, TestWritePostgresBatch_WithExtractors(), NewStoreRecorder(), NewMockTokenValidator(), TestServer_BundlePatchAndConditionalCRUD(), TestServer_CertifyEndpointAndRunner() (+18 more)

### Community 2 - "Run"
Cohesion: 0.36
Nodes (6): CheckResult, Config, Report, defaultCertifyCohortBundle(), Run(), signES384ClientAssertion()

### Community 4 - "net/http.Request"
Cohesion: 0.14
Nodes (11): net/http.Request, net/http.ResponseWriter, Server, isValidNDJSONFormat(), requestBaseURL(), exportJob, Server, Server (+3 more)

### Community 5 - "auth.go"
Cohesion: 0.19
Nodes (13): contextKey, TokenValidator, ParseScopes(), ParseSingleScope(), ScopeMatches(), splitSystemCode(), TestParseScopes(), TokenValueMatchesConstraint() (+5 more)

### Community 6 - "SearchIndexes"
Cohesion: 0.05
Nodes (54): ExtractAllergyIntoleranceIndexes(), ExtractConditionIndexes(), SearchIndexes, ExtractDiagnosticReportIndexes(), ExtractEncounterIndexes(), ExtractImmunizationIndexes(), ExtractLocationIndexes(), ExtractMedicationRequestIndexes() (+46 more)

### Community 7 - "SecurityContext"
Cohesion: 0.14
Nodes (4): google.golang.org/protobuf/runtime/protoimpl.MessageState, TestContextHelpers(), WithSecurityContext(), SecurityContext

### Community 8 - "cleanID"
Cohesion: 0.27
Nodes (18): google.golang.org/protobuf/types/dynamicpb.Message, NormalizeFHIRJSONToProtoJSON(), Server, cleanID(), extractPatientIDFromIndexes(), setAuditPatientID(), Server, parseXProvenanceHeader() (+10 more)

### Community 10 - "auth.pb.go"
Cohesion: 0.29
Nodes (3): file_proto_flint_auth_v1_auth_proto_init(), file_proto_flint_auth_v1_auth_proto_rawDescGZIP(), init()

### Community 11 - "MockTokenValidator"
Cohesion: 0.29
Nodes (3): crypto/rsa.PrivateKey, crypto/rsa.PublicKey, MockTokenValidator

### Community 12 - "Enforcement Guidelines"
Cohesion: 0.40
Nodes (5): 1. Correction, 2. Warning, 3. Temporary Ban, 4. Permanent Ban, Enforcement Guidelines

### Community 15 - "ingest.go"
Cohesion: 0.38
Nodes (6): go.temporal.io/sdk/workflow.Context, IngestFHIRBundle(), IngestInput, publishToAutoMQInput, writeMedplumBatchInput, writePostgresBatchInput

### Community 16 - "OIDCValidator"
Cohesion: 0.10
Nodes (24): Audience, JWK, JWKSet, JWTHeader, JWTPayload, OIDCConfig, OIDCValidator, ParsedToken (+16 more)

### Community 17 - "SearchParams"
Cohesion: 0.09
Nodes (47): ChainedParam, CompositeTokenQuantityParam, DateOp, ExportParams, HasParam, IncludeParam, PatientEverythingParams, QuantityOp (+39 more)

### Community 18 - "Producer"
Cohesion: 0.07
Nodes (31): Config, Producer, EncodeResourceEnvelopeProto(), NewProducer(), TestMemoryPublisher(), TestNewProducer(), TestProducer_ApicurioSchemaRegistryAndFraming(), TestProducer_BuildRecord() (+23 more)

### Community 19 - "Flint — High-Performance Open Source FHIR R4 Server"
Cohesion: 0.20
Nodes (10): Architecture, Community & Contributing, Copy-Pasteable API Cookbook (`curl`), Flint — High-Performance Open Source FHIR R4 Server, License, Project Structure, Property-Based Testing (via Hegel), Supported FHIR Resources (All 146 FHIR R4 Types & 26 US Core v6.1.0 / v7.0.0 Profiles) & Conformance Matrix (+2 more)

### Community 27 - "json_codec.go"
Cohesion: 0.15
Nodes (27): google.golang.org/protobuf/reflect/protoreflect.FieldDescriptor, google.golang.org/protobuf/reflect/protoreflect.FieldDescriptors, google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor, denormalizeMessageMap(), DenormalizeProtoJSONToFHIRJSON(), denormalizeReferenceMap(), denormalizeSingleValue(), denormalizeUntypedMap() (+19 more)

### Community 29 - "audit/audit.go"
Cohesion: 0.29
Nodes (9): ClassifyHTTPInteraction(), ClassifyHTTPOutcome(), NewMemoryRecorder(), TestProperty_ClassifyNeverPanicsOnArbitraryFuzz(), TestProperty_InteractionClassificationAndFHIRSerialization(), TestProperty_OutcomeClassificationInvariance(), TestClassifyHTTPInteraction(), TestMemoryRecorderAndToFHIRResource() (+1 more)

### Community 30 - "testing.T"
Cohesion: 0.11
Nodes (28): testing.T, TestActivities_SliceBoundsAndNilStoreValidation(), TestPublishToAutoMQ_NilAndMemoryPublisher(), TestIngestFHIRBundle_AutoMQFailure_Fails(), TestIngestFHIRBundle_HappyPath(), TestIngestFHIRBundle_MedplumFailure_ContinuesAnyway(), TestIngestFHIRBundle_PostgresFailure_Fails(), TestJWKPublicKey_BoundsValidation() (+20 more)

### Community 31 - "Outcome"
Cohesion: 0.13
Nodes (16): NewOutcome(), allowedFieldsFromDescriptor(), buildFieldMap(), DefaultSchemas(), extractStringOrWrapped(), Validator, isEmptyValue(), isValidCodeInSet() (+8 more)

### Community 32 - "Action"
Cohesion: 0.14
Nodes (5): google.golang.org/protobuf/reflect/protoreflect.EnumDescriptor, google.golang.org/protobuf/reflect/protoreflect.EnumNumber, google.golang.org/protobuf/reflect/protoreflect.EnumType, Action, ScopeContext

### Community 33 - "NewEngine"
Cohesion: 0.20
Nodes (17): TestCohortTransactionBundle_ValidatesAllResources(), TestSeedRun_UnitHTTP(), TestDenormalizeProtoJSONToFHIRJSON_RoundTripAndValidation(), TestProperty_NormalizeAndDenormalizeRoundTripValidates(), TestRewriteURNReferences(), TestProperty_InvalidGenderRejected(), TestProperty_ObservationRequiredFields(), TestProperty_ValidatorNeverPanics() (+9 more)

### Community 34 - "Server"
Cohesion: 0.17
Nodes (10): Publisher, net/url.Values, Recorder, ScopeQueryConstraints(), addSubsettedTag(), applySummaryOrElements(), cloneURLValues(), Server (+2 more)

### Community 35 - "Security Policy"
Cohesion: 0.29
Nodes (6): Healthcare & Security Architecture Principles, Information to Include, Reporting a Vulnerability, Response Timeline, Security Policy, Supported Versions

### Community 36 - "google.golang.org/protobuf/reflect/protoreflect.Message"
Cohesion: 0.13
Nodes (30): mockString, google.golang.org/protobuf/reflect/protoreflect.Message, google.golang.org/protobuf/reflect/protoreflect.Value, FHIRDateToTime(), NormalizeString(), StringValue(), StringValues(), TestFHIRDateToTime() (+22 more)

### Community 37 - "time.Time"
Cohesion: 0.19
Nodes (25): time.Time, Server, buildMeasurePopulation(), checkMedicationAllergyConflict(), evaluatePatientForMeasure(), extractCodingsFromResource(), extractParamString(), extractPrefetchResources() (+17 more)

### Community 38 - "Contributing to Flint"
Cohesion: 0.22
Nodes (9): Code Generation (`proto2type` & `buf`), Code of Conduct, Contributing to Flint, Development Environment Setup, Git Workflow & Conventional Commits, Local Development Stack, Prerequisites, Quick Start with Nix & Direnv (+1 more)

### Community 39 - "sdc_document.go"
Cohesion: 0.20
Nodes (13): github.com/flint-fhir/flint/store/postgres.SearchIndexes, appendProvenancePatientRef(), buildDefaultProvenanceAgent(), defaultClinicalIntakeQuestionnaire(), docrefTypeMatchesSummary(), extractFirstCodingMap(), formatPatientDisplayName(), Server (+5 more)

### Community 40 - "Contributor Covenant Code of Conduct"
Cohesion: 0.29
Nodes (7): Attribution, Contributor Covenant Code of Conduct, Enforcement, Enforcement Responsibilities, Our Pledge, Our Standards, Scope

### Community 42 - "patch.go"
Cohesion: 0.18
Nodes (16): encoding/json.RawMessage, bundleErrorResponse(), bundleEntry, bundleEntryRequest, bundleRequest, jsonPatchOp, ApplyJSONPatch(), ApplyMergePatch() (+8 more)

### Community 43 - "Server"
Cohesion: 0.20
Nodes (5): authCodeEntry, refreshTokenEntry, Server, isScopeSubset(), SMARTConfig

### Community 44 - "time.Duration"
Cohesion: 0.23
Nodes (15): generateBundlePayloads(), main(), printReport(), ratio(), runHTTPLoad(), runStoreLoad(), runTemporalLoad(), sync/atomic.Int64 (+7 more)

### Community 46 - "API Reference"
Cohesion: 0.40
Nodes (5): Advanced FHIR Search Engine, API Reference, Batch / Transaction Bundles, FHIR Conformance & Validation Engine (`pkg/validation`), Resource Interactions

### Community 47 - "main"
Cohesion: 0.26
Nodes (9): main(), Option, ProvisionTenantSchema(), QuoteIdent(), SchemaFromSlug(), TestQuoteIdent(), TestSchemaFromSlug(), WithAutoProvision() (+1 more)

### Community 48 - ".authMiddleware"
Cohesion: 0.16
Nodes (11): net/http.Handler, auditActorHolder, auditActorKeyType, auditResponseWriter, extractClientIP(), Server, isMeasureOp(), isPublicEndpoint() (+3 more)

### Community 49 - "SMART on FHIR v2 & Identity Architecture"
Cohesion: 0.50
Nodes (4): Decoupled `TokenValidator` Interface (`pkg/auth`), Environment Configuration, Security Guarantees, SMART on FHIR v2 & Identity Architecture

### Community 51 - ".resolveValueSetConcepts"
Cohesion: 0.32
Nodes (7): builtInCodeSystemDef, conceptMapEntry, extractEntriesFromConceptMapJSON(), extractTerminologyParams(), firstNonEmpty(), Server, terminologyConcept

### Community 53 - "HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness"
Cohesion: 0.50
Nodes (3): 1. Quick Automated Certification (No Docker Required), 2. Running Official HL7 Inferno UI (`localhost:4567`), HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness

### Community 61 - "operations.go"
Cohesion: 0.24
Nodes (13): codingKey, applyMetaAdd(), applyMetaDelete(), codingDedupeKey(), extractInputValueMeta(), extractPatientDemographics(), matchGradeForScore(), mergeCodingSlices() (+5 more)

### Community 62 - "DefaultIndexExtractors"
Cohesion: 0.11
Nodes (19): Activities, PublishToAutoMQInput, WriteMedplumBatchInput, WritePostgresBatchInput, github.com/flint-fhir/flint/automq.Publisher, log/slog.Logger, getDefaultExtractors(), isNilPublisher() (+11 more)

### Community 64 - "FromContext"
Cohesion: 0.15
Nodes (11): github.com/flint-fhir/flint/pkg/auth.SecurityContext, Allows(), FromContext(), IsPatientRestricted(), TestSecurityContext_Allows(), extractBearerHeader(), Server, extractClaimFromBundle() (+3 more)

### Community 66 - "Installation & Quickstart (Standalone FHIR R4 Server)"
Cohesion: 0.29
Nodes (7): Installation & Quickstart (Standalone FHIR R4 Server), Option 1: Docker Compose (Fastest — Postgres + FHIR Server + Demo Cohort), Option 2: Container Image (`ghcr.io/flint-fhir/flint`), Option 3: Single Binary (`go install` or GitHub Releases), Option 4: Contributor Development Environment (Nix + `k3d` Kubernetes), Run Benchmarks & Tests, Runtime Configuration (`flintd`)

### Community 68 - "Optional Add-Ons (Streaming CDC, Temporal Workflows & Clinical Lakehouse)"
Cohesion: 0.50
Nodes (4): 1. Streaming CDC Add-On (`pkg/cdc` + `automq/`), 2. Temporal Ingestion Worker Add-On (`ingest/`, `cmd/worker/`), 3. Clinical Dimensional Lakehouse Add-On (`analytics/` — SQLMesh on DuckDB / Iceberg), Optional Add-Ons (Streaming CDC, Temporal Workflows & Clinical Lakehouse)

## Knowledge Gaps
- **59 isolated node(s):** `flint-analytics`, `github.com/flint-fhir/flint`, `writeMedplumBatchInput`, `publishToAutoMQInput`, `contextKey` (+54 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 153 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **10 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `Server` connect `Server` to `FromContext`, `New`, `context.Context`, `net/http.Request`, `time.Time`, `auth.go`, `.RegisterResourceType`, `cleanID`, `MockTokenValidator`, `time.Duration`, `Server`, `Producer`, `json_codec.go`, `DefaultIndexExtractors`, `Outcome`?**
  _High betweenness centrality (0.110) - this node is a cross-community bridge._
- **Why does `SearchIndexes` connect `SearchIndexes` to `context.Context`, `cleanID`, `google.golang.org/protobuf/reflect/protoreflect.Message`?**
  _High betweenness centrality (0.090) - this node is a cross-community bridge._
- **Why does `SecurityContext` connect `SecurityContext` to `FromContext`, `Server`, `SmartScope`, `auth.go`, `SmartConfiguration`, `auth.pb.go`, `MockTokenValidator`, `OIDCValidator`?**
  _High betweenness centrality (0.067) - this node is a cross-community bridge._
- **What connects `flint-analytics`, `github.com/flint-fhir/flint`, `writeMedplumBatchInput` to the rest of the system?**
  _59 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `context.Context` be split into smaller, more focused modules?**
  _Cohesion score 0.07887323943661972 - nodes in this community are weakly interconnected._
- **Should `net/http.Request` be split into smaller, more focused modules?**
  _Cohesion score 0.1422475106685633 - nodes in this community are weakly interconnected._
- **Should `SearchIndexes` be split into smaller, more focused modules?**
  _Cohesion score 0.05115089514066496 - nodes in this community are weakly interconnected._