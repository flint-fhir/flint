# Graph Report - flint  (2026-10-21)

## Corpus Check
- 148 files · ~356,673 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 1135 nodes · 2952 edges · 70 communities (54 shown, 15 thin omitted)
- Extraction: 95% EXTRACTED · 5% INFERRED · 0% AMBIGUOUS · INFERRED: 151 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `e3a7c399`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- context.Context
- New
- spike4_search_index/search_index_test.go
- FHIRString
- net/http.Request
- ParseScopes
- ExtractPatientIndexes
- MiniPatient
- cleanID
- SmartConfiguration
- spike_test.go
- FHIRDate
- Extension
- mini_fhir.pb.go
- extract.go
- ingest.go
- OIDCValidator
- SearchParams
- Producer
- Flint — High-Performance FHIR R4 Server on a Data Lakehouse
- query_duckdb.py
- query_iceberg.py
- github.com/flint-fhir/flint
- rules/graphify.md
- workflows/graphify.md
- flint-analytics
- json_codec.go
- file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP
- audit/audit.go
- extract_test.go
- Outcome
- SecurityContext
- testing.T
- Server
- README.md
- google.golang.org/protobuf/reflect/protoreflect.Message
- time.Time
- Contributing to Flint
- sdc_document.go
- Contributor Covenant Code of Conduct
- Quickstart
- patch.go
- Server
- time.Duration
- roundtrip.go
- API Reference
- main
- .authMiddleware
- SMART on FHIR v2 & Identity Architecture
- automq_test.go
- .resolveValueSetConcepts
- TestSchema_NoJSONBAndAtlasSumSync
- HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness
- SearchIndexes
- ExtractDiagnosticReport
- ExtractImmunization
- ExtractMedicationRequestIndexes
- ExtractOrganizationIndexes
- ExtractProcedure
- ExtractLocationIndexes
- operations.go
- DefaultIndexExtractors
- ExtractObservation
- .handlePatientMemberMatch
- NewMockTokenValidator
- Installation & Quickstart (Standalone FHIR R4 Server)
- ExtractEncounterIndexes
- Optional Add-Ons (Streaming CDC, Temporal Workflows & Clinical Lakehouse)
- .RegisterResourceType

## God Nodes (most connected - your core abstractions)
1. `Server` - 62 edges
2. `SearchIndexes` - 45 edges
3. `writeOperationOutcome()` - 44 edges
4. `cleanID()` - 37 edges
5. `FromContext()` - 33 edges
6. `IsPatientRestricted()` - 31 edges
7. `Store` - 28 edges
8. `FHIRString` - 28 edges
9. `SecurityContext` - 27 edges
10. `New()` - 25 edges

## Surprising Connections (you probably didn't know these)
- `main()` --calls--> `NewProducer()`  [EXTRACTED]
  cmd/flintd/main.go → automq/producer.go
- `main()` --calls--> `NewProducer()`  [EXTRACTED]
  cmd/worker/main.go → automq/producer.go
- `main()` --calls--> `Run()`  [EXTRACTED]
  cmd/certify/main.go → pkg/certify/certify.go
- `main()` --calls--> `NewOIDCValidator()`  [EXTRACTED]
  cmd/flintd/main.go → pkg/auth/validator_oidc.go
- `main()` --calls--> `NewZitadelValidator()`  [EXTRACTED]
  cmd/flintd/main.go → pkg/auth/validator_zitadel.go

## Import Cycles
- None detected.

## Communities (70 total, 15 thin omitted)

### Community 0 - "context.Context"
Cohesion: 0.07
Nodes (37): MemoryRecorder, StoreRecorder, ExtractConditionIndexes(), context.Context, database/sql.Tx, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto.Condition, sync.Map, BundleOperation (+29 more)

### Community 1 - "New"
Cohesion: 0.20
Nodes (24): main(), TestSeedRun_EndToEndIntegration(), database/sql.DB, TestWritePostgresBatch_WithExtractors(), NewStoreRecorder(), TestServer_BundlePatchAndConditionalCRUD(), TestServer_CertifyEndpointAndRunner(), TestServer_CriticalSecurityAndSearchHardening() (+16 more)

### Community 2 - "spike4_search_index/search_index_test.go"
Cohesion: 0.27
Nodes (15): github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto.Patient, SearchIndexes, SpidxDate, SpidxReference, SpidxString, SpidxToken, buildTestPatient(), TestProtoProduceConsume() (+7 more)

### Community 3 - "FHIRString"
Cohesion: 0.09
Nodes (4): google.golang.org/protobuf/runtime/protoimpl.SizeCache, Coding, FHIRString, HumanName

### Community 4 - "net/http.Request"
Cohesion: 0.12
Nodes (14): net/http.Request, net/http.ResponseWriter, extractBearerHeader(), Server, Server, isValidNDJSONFormat(), requestBaseURL(), exportJob (+6 more)

### Community 5 - "ParseScopes"
Cohesion: 0.19
Nodes (13): ParseScopes(), ParseSingleScope(), ScopeMatches(), TestJWKPublicKey_BoundsValidation(), TestJWTVerifyClaims_Adversarial(), TestOIDCValidator_HTTPJWKSAndHardening(), TestParseScopes(), TestSecurityContext_Allows() (+5 more)

### Community 6 - "ExtractPatientIndexes"
Cohesion: 0.22
Nodes (6): ExtractPatientIndexes(), ExtractPractitionerIndexes(), TestExtractConditionIndexes(), TestExtractPatientIndexes(), TestExtractPractitionerIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto.Practitioner

### Community 7 - "MiniPatient"
Cohesion: 0.11
Nodes (5): google.golang.org/protobuf/runtime/protoimpl.MessageState, google.golang.org/protobuf/runtime/protoimpl.UnknownFields, FHIRBoolean, Identifier, MiniPatient

### Community 8 - "cleanID"
Cohesion: 0.20
Nodes (21): github.com/flint-fhir/flint/pkg/auth.SecurityContext, google.golang.org/protobuf/types/dynamicpb.Message, Allows(), IsPatientRestricted(), NormalizeFHIRJSONToProtoJSON(), Server, cleanID(), extractPatientIDFromIndexes() (+13 more)

### Community 10 - "spike_test.go"
Cohesion: 0.50
Nodes (8): protoNameToFHIR(), ProtoToFHIRJSON(), TestProtojsonMarshalOutput(), TestRoundTripFidelity(), unwrap(), unwrapHumanNames(), unwrapIdentifiers(), unwrapStringArray()

### Community 13 - "mini_fhir.pb.go"
Cohesion: 0.25
Nodes (5): Extension_ValueBoolean, Extension_ValueDateTime, Extension_ValueString, file_spike_spike1_proto_mini_fhir_proto_init(), init()

### Community 14 - "extract.go"
Cohesion: 0.42
Nodes (8): ExtractCondition(), ExtractLocation(), ExtractMedicationRequest(), ExtractOrganization(), ExtractPractitioner(), ExtractSpecimen(), isEnumTokenParam(), normalizeSearchIndexes()

### Community 15 - "ingest.go"
Cohesion: 0.38
Nodes (6): go.temporal.io/sdk/workflow.Context, IngestFHIRBundle(), IngestInput, publishToAutoMQInput, writeMedplumBatchInput, writePostgresBatchInput

### Community 16 - "OIDCValidator"
Cohesion: 0.08
Nodes (29): Audience, JWK, JWKSet, JWTHeader, JWTPayload, OIDCConfig, OIDCValidator, ParsedToken (+21 more)

### Community 17 - "SearchParams"
Cohesion: 0.09
Nodes (46): ChainedParam, CompositeTokenQuantityParam, DateOp, ExportParams, HasParam, IncludeParam, PatientEverythingParams, QuantityOp (+38 more)

### Community 18 - "Producer"
Cohesion: 0.06
Nodes (40): Activities, PublishToAutoMQInput, WriteMedplumBatchInput, WritePostgresBatchInput, Config, MemoryPublisher, Message, Producer (+32 more)

### Community 19 - "Flint — High-Performance FHIR R4 Server on a Data Lakehouse"
Cohesion: 0.24
Nodes (13): Architecture, Clinical Dimensional Lakehouse (`analytics/` — SQLMesh on DuckDB / Iceberg), Community & Contributing, Copy-Pasteable API Cookbook (`curl`), Flint — High-Performance FHIR R4 Server on a Data Lakehouse, Flint — High-Performance Open Source FHIR R4 Server, License, Project Structure (+5 more)

### Community 27 - "json_codec.go"
Cohesion: 0.15
Nodes (27): google.golang.org/protobuf/reflect/protoreflect.FieldDescriptor, google.golang.org/protobuf/reflect/protoreflect.FieldDescriptors, google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor, denormalizeMessageMap(), DenormalizeProtoJSONToFHIRJSON(), denormalizeReferenceMap(), denormalizeSingleValue(), denormalizeUntypedMap() (+19 more)

### Community 29 - "audit/audit.go"
Cohesion: 0.29
Nodes (9): ClassifyHTTPInteraction(), ClassifyHTTPOutcome(), NewMemoryRecorder(), TestProperty_ClassifyNeverPanicsOnArbitraryFuzz(), TestProperty_InteractionClassificationAndFHIRSerialization(), TestProperty_OutcomeClassificationInvariance(), TestClassifyHTTPInteraction(), TestMemoryRecorderAndToFHIRResource() (+1 more)

### Community 30 - "extract_test.go"
Cohesion: 0.22
Nodes (10): ExtractPatient(), TestExtractCondition(), TestExtractLocation(), TestExtractMedicationRequest(), TestExtractOrganization(), TestExtractPatient(), TestExtractPatient_FallbackResID(), TestExtractPatient_InvalidBytes() (+2 more)

### Community 31 - "Outcome"
Cohesion: 0.11
Nodes (19): NewOutcome(), allowedFieldsFromDescriptor(), buildFieldMap(), DefaultSchemas(), extractStringOrWrapped(), Validator, isEmptyValue(), isValidCodeInSet() (+11 more)

### Community 32 - "SecurityContext"
Cohesion: 0.05
Nodes (13): file_proto_flint_auth_v1_auth_proto_init(), file_proto_flint_auth_v1_auth_proto_rawDescGZIP(), init(), crypto/rsa.PrivateKey, crypto/rsa.PublicKey, google.golang.org/protobuf/reflect/protoreflect.EnumDescriptor, google.golang.org/protobuf/reflect/protoreflect.EnumNumber, google.golang.org/protobuf/reflect/protoreflect.EnumType (+5 more)

### Community 33 - "testing.T"
Cohesion: 0.12
Nodes (29): TestCohortTransactionBundle_ValidatesAllResources(), TestSeedRun_UnitHTTP(), testing.T, TestActivities_SliceBoundsAndNilStoreValidation(), TestPublishToAutoMQ_NilAndMemoryPublisher(), TestPublishToAutoMQ_NilProducer(), TestIngestFHIRBundle_AutoMQFailure_Fails(), TestIngestFHIRBundle_HappyPath() (+21 more)

### Community 34 - "Server"
Cohesion: 0.15
Nodes (15): contextKey, net/url.Values, Recorder, FromContext(), TokenValidator, ScopeQueryConstraints(), splitSystemCode(), TestContextHelpers() (+7 more)

### Community 35 - "README.md"
Cohesion: 0.22
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
Cohesion: 0.18
Nodes (14): github.com/flint-fhir/flint/store/postgres.SearchIndexes, appendProvenancePatientRef(), buildDefaultProvenanceAgent(), defaultClinicalIntakeQuestionnaire(), docrefTypeMatchesSummary(), extractFirstCodingMap(), formatPatientDisplayName(), Server (+6 more)

### Community 40 - "Contributor Covenant Code of Conduct"
Cohesion: 0.17
Nodes (12): 1. Correction, 2. Warning, 3. Temporary Ban, 4. Permanent Ban, Attribution, Contributor Covenant Code of Conduct, Enforcement, Enforcement Guidelines (+4 more)

### Community 41 - "Quickstart"
Cohesion: 0.33
Nodes (6): 1. Development Environment (Hermetic Nix Shell), 2. Launch Local Dev Stack (k3d), 3. Build, Run, Seed & Certify, 4. Run Benchmarks & Tests, 5. Query Analytical Iceberg Table with DuckDB, Quickstart

### Community 42 - "patch.go"
Cohesion: 0.18
Nodes (15): encoding/json.RawMessage, bundleErrorResponse(), bundleEntry, bundleEntryRequest, bundleRequest, jsonPatchOp, ApplyJSONPatch(), ApplyMergePatch() (+7 more)

### Community 43 - "Server"
Cohesion: 0.19
Nodes (6): authCodeEntry, refreshTokenEntry, Server, isScopeSubset(), writeOAuthError(), SMARTConfig

### Community 44 - "time.Duration"
Cohesion: 0.23
Nodes (15): generateBundlePayloads(), main(), printReport(), ratio(), runHTTPLoad(), runStoreLoad(), runTemporalLoad(), sync/atomic.Int64 (+7 more)

### Community 45 - "roundtrip.go"
Cohesion: 0.36
Nodes (9): FHIRToProtoJSON(), ProtoJSONToFHIR(), RoundTrip(), TestBirthDateWithExtension(), TestFullPatientRoundTrip(), TestGenderEnumMapping(), TestWrapUnwrapPrimitive(), UnwrapPrimitive() (+1 more)

### Community 46 - "API Reference"
Cohesion: 0.40
Nodes (5): Advanced FHIR Search Engine, API Reference, Batch / Transaction Bundles, FHIR Conformance & Validation Engine (`pkg/validation`), Resource Interactions

### Community 47 - "main"
Cohesion: 0.22
Nodes (10): main(), Option, Store, ProvisionTenantSchema(), QuoteIdent(), SchemaFromSlug(), TestQuoteIdent(), TestSchemaFromSlug() (+2 more)

### Community 48 - ".authMiddleware"
Cohesion: 0.17
Nodes (10): net/http.Handler, auditActorHolder, auditActorKeyType, auditResponseWriter, extractClientIP(), isMeasureOp(), isPublicEndpoint(), isQuestionnaireExtractOp() (+2 more)

### Community 49 - "SMART on FHIR v2 & Identity Architecture"
Cohesion: 0.50
Nodes (4): Decoupled `TokenValidator` Interface (`pkg/auth`), Environment Configuration, Security Guarantees, SMART on FHIR v2 & Identity Architecture

### Community 50 - "automq_test.go"
Cohesion: 0.38
Nodes (6): IcebergColumn, ProtoToExpectedIceberg(), TestAutoMQTableTopicConfig(), TestExpectedIcebergSchema(), TestFlattenVsNested(), TestWrapperProtoDesign()

### Community 51 - ".resolveValueSetConcepts"
Cohesion: 0.32
Nodes (7): builtInCodeSystemDef, conceptMapEntry, extractEntriesFromConceptMapJSON(), extractTerminologyParams(), firstNonEmpty(), Server, terminologyConcept

### Community 53 - "HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness"
Cohesion: 0.50
Nodes (3): 1. Quick Automated Certification (No Docker Required), 2. Running Official HL7 Inferno UI (`localhost:4567`), HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness

### Community 54 - "SearchIndexes"
Cohesion: 0.25
Nodes (7): ExtractAllergyIntoleranceIndexes(), SearchIndexes, ExtractSpecimenIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/allergy_intolerance_go_proto.AllergyIntolerance, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/specimen_go_proto.Specimen, ExtractAllergyIntolerance(), TestExtractAllergyIntolerance()

### Community 55 - "ExtractDiagnosticReport"
Cohesion: 0.40
Nodes (4): ExtractDiagnosticReportIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/diagnostic_report_go_proto.DiagnosticReport, ExtractDiagnosticReport(), TestExtractDiagnosticReport()

### Community 56 - "ExtractImmunization"
Cohesion: 0.40
Nodes (4): ExtractImmunizationIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/immunization_go_proto.Immunization, ExtractImmunization(), TestExtractImmunization()

### Community 59 - "ExtractProcedure"
Cohesion: 0.40
Nodes (4): ExtractProcedureIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/procedure_go_proto.Procedure, ExtractProcedure(), TestExtractProcedure()

### Community 61 - "operations.go"
Cohesion: 0.27
Nodes (12): codingKey, applyMetaAdd(), applyMetaDelete(), codingDedupeKey(), extractPatientDemographics(), matchGradeForScore(), mergeCodingSlices(), normalizeTelecomValue() (+4 more)

### Community 62 - "DefaultIndexExtractors"
Cohesion: 0.26
Nodes (10): envOrDefault(), firstEnvOrDefault(), main(), AllR4ResourceDescriptors(), AllR4ResourceTypes(), USCoreResourceTypes(), DefaultIndexExtractors(), jsonToProtoBytes() (+2 more)

### Community 63 - "ExtractObservation"
Cohesion: 0.25
Nodes (7): ExtractObservationIndexes(), TestExtractObservationIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto.Quantity, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto.Observation, appendQuantityIndexes(), ExtractObservation(), TestExtractObservation()

### Community 64 - ".handlePatientMemberMatch"
Cohesion: 0.32
Nodes (4): extractClaimFromBundle(), extractCoverageBeneficiaryPatientID(), Server, isVitalSignObservation()

### Community 65 - "NewMockTokenValidator"
Cohesion: 0.29
Nodes (5): TestOIDCValidator_EndToEnd(), TestZitadelValidator(), NewMockTokenValidator(), TestServer_OpenAPIAndDocs(), TestServer_AuthMiddleware()

### Community 66 - "Installation & Quickstart (Standalone FHIR R4 Server)"
Cohesion: 0.29
Nodes (7): Installation & Quickstart (Standalone FHIR R4 Server), Option 1: Docker Compose (Fastest — Postgres + FHIR Server + Demo Cohort), Option 2: Container Image (`ghcr.io/flint-fhir/flint`), Option 3: Single Binary (`go install` or GitHub Releases), Option 4: Contributor Development Environment (Nix + `k3d` Kubernetes), Run Benchmarks & Tests, Runtime Configuration (`flintd`)

### Community 67 - "ExtractEncounterIndexes"
Cohesion: 0.33
Nodes (5): ExtractEncounterIndexes(), TestExtractEncounterIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto.Encounter, ExtractEncounter(), TestExtractEncounter()

### Community 68 - "Optional Add-Ons (Streaming CDC, Temporal Workflows & Clinical Lakehouse)"
Cohesion: 0.50
Nodes (4): 1. Streaming CDC Add-On (`pkg/cdc` + `automq/`), 2. Temporal Ingestion Worker Add-On (`ingest/`, `cmd/worker/`), 3. Clinical Dimensional Lakehouse Add-On (`analytics/` — SQLMesh on DuckDB / Iceberg), Optional Add-Ons (Streaming CDC, Temporal Workflows & Clinical Lakehouse)

## Knowledge Gaps
- **58 isolated node(s):** `flint-analytics`, `github.com/flint-fhir/flint`, `writeMedplumBatchInput`, `publishToAutoMQInput`, `contextKey` (+53 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 190 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **15 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `SecurityContext` connect `SecurityContext` to `Server`, `FHIRString`, `ParseScopes`, `MiniPatient`, `cleanID`, `OIDCValidator`?**
  _High betweenness centrality (0.121) - this node is a cross-community bridge._
- **Why does `Server` connect `Server` to `SecurityContext`, `New`, `context.Context`, `net/http.Request`, `time.Time`, `.RegisterResourceType`, `cleanID`, `Server`, `time.Duration`, `Producer`, `json_codec.go`, `Outcome`?**
  _High betweenness centrality (0.099) - this node is a cross-community bridge._
- **Why does `SearchIndexes` connect `SearchIndexes` to `context.Context`, `ExtractEncounterIndexes`, `google.golang.org/protobuf/reflect/protoreflect.Message`, `ExtractPatientIndexes`, `cleanID`, `extract.go`, `ExtractDiagnosticReport`, `ExtractImmunization`, `ExtractMedicationRequestIndexes`, `ExtractOrganizationIndexes`, `ExtractProcedure`, `ExtractLocationIndexes`, `extract_test.go`, `ExtractObservation`?**
  _High betweenness centrality (0.085) - this node is a cross-community bridge._
- **What connects `flint-analytics`, `github.com/flint-fhir/flint`, `writeMedplumBatchInput` to the rest of the system?**
  _58 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `context.Context` be split into smaller, more focused modules?**
  _Cohesion score 0.07292707292707293 - nodes in this community are weakly interconnected._
- **Should `FHIRString` be split into smaller, more focused modules?**
  _Cohesion score 0.09259259259259259 - nodes in this community are weakly interconnected._
- **Should `net/http.Request` be split into smaller, more focused modules?**
  _Cohesion score 0.12473572938689217 - nodes in this community are weakly interconnected._