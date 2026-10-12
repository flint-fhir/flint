# Graph Report - flint  (2026-10-12)

## Corpus Check
- 127 files · ~301,891 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 903 nodes · 2074 edges · 58 communities (43 shown, 14 thin omitted)
- Extraction: 96% EXTRACTED · 4% INFERRED · 0% AMBIGUOUS · INFERRED: 85 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `e09f478d`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- context.Context
- New
- loadtest/main.go
- FHIRString
- Server
- ParseScopes
- spike4_search_index/search_index_test.go
- MiniPatient
- condition_search_index.go
- SmartConfiguration
- spike_test.go
- Extension
- google.golang.org/protobuf/reflect/protoreflect.Message
- mini_fhir.pb.go
- extract.go
- ingest.go
- OIDCValidator
- time.Time
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
- testing.T
- Outcome
- SecurityContext
- NewEngine
- Action
- README.md
- helpers.go
- ScopeContext
- Contributing to Flint
- auth.pb.go
- Contributor Covenant Code of Conduct
- Quickstart
- patch.go
- Enforcement Guidelines
- HumanName
- roundtrip.go
- API Reference
- SmartScope
- SMART on FHIR v2 & Identity Architecture
- automq_test.go
- TestSchema_NoJSONBAndAtlasSumSync
- HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness
- ExtractAllergyIntoleranceIndexes
- ExtractDiagnosticReportIndexes
- ExtractImmunizationIndexes
- ExtractMedicationRequestIndexes
- ExtractOrganizationIndexes
- ExtractProcedureIndexes

## God Nodes (most connected - your core abstractions)
1. `Server` - 49 edges
2. `FHIRString` - 28 edges
3. `SecurityContext` - 26 edges
4. `writeOperationOutcome()` - 25 edges
5. `Store` - 25 edges
6. `MiniPatient` - 23 edges
7. `New()` - 22 edges
8. `Extension` - 22 edges
9. `SmartScope` - 20 edges
10. `NewEngine()` - 20 edges

## Surprising Connections (you probably didn't know these)
- `Activities` --references--> `Producer`  [EXTRACTED]
  ingest/activity/activities.go → automq/producer.go
- `TestServer_Integration()` --calls--> `NewMemoryPublisher()`  [EXTRACTED]
  server/server_test.go → automq/producer.go
- `main()` --calls--> `NewProducer()`  [EXTRACTED]
  cmd/flintd/main.go → automq/producer.go
- `main()` --calls--> `NewOIDCValidator()`  [EXTRACTED]
  cmd/flintd/main.go → pkg/auth/validator_oidc.go
- `main()` --calls--> `NewZitadelValidator()`  [EXTRACTED]
  cmd/flintd/main.go → pkg/auth/validator_zitadel.go

## Import Cycles
- None detected.

## Communities (58 total, 14 thin omitted)

### Community 0 - "context.Context"
Cohesion: 0.08
Nodes (33): Activities, PublishToAutoMQInput, WriteMedplumBatchInput, WritePostgresBatchInput, MemoryRecorder, StoreRecorder, context.Context, database/sql.Tx (+25 more)

### Community 1 - "New"
Cohesion: 0.11
Nodes (28): CheckResult, Config, Report, main(), main(), TestSeedRun_EndToEndIntegration(), database/sql.DB, google.golang.org/protobuf/proto.Message (+20 more)

### Community 2 - "loadtest/main.go"
Cohesion: 0.36
Nodes (12): generateBundlePayloads(), main(), printReport(), ratio(), runHTTPLoad(), runStoreLoad(), runTemporalLoad(), sync/atomic.Int64 (+4 more)

### Community 3 - "FHIRString"
Cohesion: 0.13
Nodes (6): google.golang.org/protobuf/runtime/protoimpl.MessageState, google.golang.org/protobuf/runtime/protoimpl.SizeCache, google.golang.org/protobuf/runtime/protoimpl.UnknownFields, Coding, FHIRString, Identifier

### Community 4 - "Server"
Cohesion: 0.06
Nodes (49): Publisher, github.com/flint-fhir/flint/pkg/auth.SecurityContext, github.com/flint-fhir/flint/store/postgres.SearchIndexes, google.golang.org/protobuf/types/dynamicpb.Message, net/http.Handler, net/http.Request, net/http.ResponseWriter, Recorder (+41 more)

### Community 5 - "ParseScopes"
Cohesion: 0.27
Nodes (10): contextKey, ParseScopes(), ParseSingleScope(), ScopeMatches(), TestParseScopes(), TestProperty_ParseScopesNeverPanicsOnArbitraryInput(), TestProperty_QueryParameterFilterInvariance(), TestProperty_SMARTV1GrammarAcceptance() (+2 more)

### Community 6 - "spike4_search_index/search_index_test.go"
Cohesion: 0.09
Nodes (30): ExtractEncounterIndexes(), SearchIndexes, ExtractObservationIndexes(), SearchIndexes, ExtractPatientIndexes(), SearchIndexes, ExtractPractitionerIndexes(), SearchIndexes (+22 more)

### Community 8 - "condition_search_index.go"
Cohesion: 0.16
Nodes (14): ExtractConditionIndexes(), SearchIndexes, SpidxDate, SpidxReference, SpidxString, SpidxToken, TestExtractConditionIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto.Condition (+6 more)

### Community 10 - "spike_test.go"
Cohesion: 0.50
Nodes (8): protoNameToFHIR(), ProtoToFHIRJSON(), TestProtojsonMarshalOutput(), TestRoundTripFidelity(), unwrap(), unwrapHumanNames(), unwrapIdentifiers(), unwrapStringArray()

### Community 11 - "Extension"
Cohesion: 0.11
Nodes (4): Extension, Extension_ValueDate, FHIRDate, isExtension_Value

### Community 13 - "mini_fhir.pb.go"
Cohesion: 0.25
Nodes (5): Extension_ValueBoolean, Extension_ValueDateTime, Extension_ValueString, file_spike_spike1_proto_mini_fhir_proto_init(), init()

### Community 14 - "extract.go"
Cohesion: 0.30
Nodes (16): github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto.Quantity, appendQuantityIndexes(), ExtractAllergyIntolerance(), ExtractCondition(), ExtractDiagnosticReport(), ExtractEncounter(), ExtractImmunization(), ExtractMedicationRequest() (+8 more)

### Community 15 - "ingest.go"
Cohesion: 0.38
Nodes (6): go.temporal.io/sdk/workflow.Context, IngestFHIRBundle(), IngestInput, publishToAutoMQInput, writeMedplumBatchInput, writePostgresBatchInput

### Community 16 - "OIDCValidator"
Cohesion: 0.08
Nodes (28): Audience, JWK, JWKSet, JWTHeader, JWTPayload, OIDCConfig, OIDCValidator, ParsedToken (+20 more)

### Community 17 - "time.Time"
Cohesion: 0.09
Nodes (35): net/url.Values, time.Time, ChainedParam, DateOp, ExportParams, IncludeParam, PatientEverythingParams, QuantityOp (+27 more)

### Community 18 - "Producer"
Cohesion: 0.09
Nodes (26): Config, MemoryPublisher, Message, Producer, NewMemoryPublisher(), NewProducer(), TestMemoryPublisher(), TestNewProducer() (+18 more)

### Community 19 - "Flint — High-Performance FHIR R4 Server on a Data Lakehouse"
Cohesion: 0.18
Nodes (11): Architecture, Clinical Dimensional Lakehouse (`analytics/` — SQLMesh on DuckDB / Iceberg), Community & Contributing, Copy-Pasteable API Cookbook (`curl`), Flint — High-Performance FHIR R4 Server on a Data Lakehouse, License, Project Structure, Property-Based Testing (via Hegel) (+3 more)

### Community 27 - "json_codec.go"
Cohesion: 0.16
Nodes (25): google.golang.org/protobuf/reflect/protoreflect.FieldDescriptor, google.golang.org/protobuf/reflect/protoreflect.FieldDescriptors, google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor, denormalizeMessageMap(), denormalizeReferenceMap(), denormalizeSingleValue(), denormalizeUntypedMap(), denormalizeUntypedValue() (+17 more)

### Community 29 - "audit/audit.go"
Cohesion: 0.29
Nodes (9): ClassifyHTTPInteraction(), ClassifyHTTPOutcome(), NewMemoryRecorder(), TestProperty_ClassifyNeverPanicsOnArbitraryFuzz(), TestProperty_InteractionClassificationAndFHIRSerialization(), TestProperty_OutcomeClassificationInvariance(), TestClassifyHTTPInteraction(), TestMemoryRecorderAndToFHIRResource() (+1 more)

### Community 30 - "testing.T"
Cohesion: 0.11
Nodes (25): testing.T, TestActivities_SliceBoundsAndNilStoreValidation(), TestPublishToAutoMQ_NilProducer(), TestIngestFHIRBundle_AutoMQFailure_Fails(), TestIngestFHIRBundle_HappyPath(), TestIngestFHIRBundle_MedplumFailure_ContinuesAnyway(), TestIngestFHIRBundle_PostgresFailure_Fails(), TestNormalizeFHIRJSONToProtoJSON_StandardFlatFHIR() (+17 more)

### Community 31 - "Outcome"
Cohesion: 0.11
Nodes (17): NewOutcome(), buildFieldMap(), DefaultSchemas(), extractStringOrWrapped(), isEmptyValue(), isValidCodeInSet(), TestOutcome_ToOperationOutcomeMap(), ConformanceMatrixEntry (+9 more)

### Community 32 - "SecurityContext"
Cohesion: 0.12
Nodes (6): crypto/rsa.PrivateKey, crypto/rsa.PublicKey, TestContextHelpers(), WithSecurityContext(), MockTokenValidator, SecurityContext

### Community 33 - "NewEngine"
Cohesion: 0.17
Nodes (20): TestCohortTransactionBundle_ValidatesAllResources(), TestSeedRun_UnitHTTP(), DenormalizeProtoJSONToFHIRJSON(), InjectResourceType(), TestDenormalizeProtoJSONToFHIRJSON_RoundTripAndValidation(), TestProperty_NormalizeAndDenormalizeRoundTripValidates(), TestRewriteURNReferences(), TestProperty_InvalidGenderRejected() (+12 more)

### Community 35 - "README.md"
Cohesion: 0.22
Nodes (6): Healthcare & Security Architecture Principles, Information to Include, Reporting a Vulnerability, Response Timeline, Security Policy, Supported Versions

### Community 36 - "helpers.go"
Cohesion: 0.20
Nodes (9): mockString, FHIRDateToTime(), NormalizeString(), StringValue(), StringValues(), TestFHIRDateToTime(), TestNormalizeString(), TestStringValue() (+1 more)

### Community 37 - "ScopeContext"
Cohesion: 0.18
Nodes (3): google.golang.org/protobuf/reflect/protoreflect.EnumNumber, google.golang.org/protobuf/reflect/protoreflect.EnumType, ScopeContext

### Community 38 - "Contributing to Flint"
Cohesion: 0.22
Nodes (9): Code Generation (`proto2type` & `buf`), Code of Conduct, Contributing to Flint, Development Environment Setup, Git Workflow & Conventional Commits, Local Development Stack, Prerequisites, Quick Start with Nix & Direnv (+1 more)

### Community 39 - "auth.pb.go"
Cohesion: 0.29
Nodes (3): file_proto_flint_auth_v1_auth_proto_init(), file_proto_flint_auth_v1_auth_proto_rawDescGZIP(), init()

### Community 40 - "Contributor Covenant Code of Conduct"
Cohesion: 0.29
Nodes (7): Attribution, Contributor Covenant Code of Conduct, Enforcement, Enforcement Responsibilities, Our Pledge, Our Standards, Scope

### Community 41 - "Quickstart"
Cohesion: 0.33
Nodes (6): 1. Development Environment (Hermetic Nix Shell), 2. Launch Local Dev Stack (k3d), 3. Build, Run, Seed & Certify, 4. Run Benchmarks & Tests, 5. Query Analytical Iceberg Table with DuckDB, Quickstart

### Community 42 - "patch.go"
Cohesion: 0.18
Nodes (16): encoding/json.RawMessage, bundleErrorResponse(), bundleEntry, bundleEntryRequest, bundleRequest, jsonPatchOp, ApplyJSONPatch(), ApplyMergePatch() (+8 more)

### Community 43 - "Enforcement Guidelines"
Cohesion: 0.40
Nodes (5): 1. Correction, 2. Warning, 3. Temporary Ban, 4. Permanent Ban, Enforcement Guidelines

### Community 45 - "roundtrip.go"
Cohesion: 0.36
Nodes (9): FHIRToProtoJSON(), ProtoJSONToFHIR(), RoundTrip(), TestBirthDateWithExtension(), TestFullPatientRoundTrip(), TestGenderEnumMapping(), TestWrapUnwrapPrimitive(), UnwrapPrimitive() (+1 more)

### Community 46 - "API Reference"
Cohesion: 0.40
Nodes (5): Advanced FHIR Search Engine, API Reference, Batch / Transaction Bundles, FHIR Conformance & Validation Engine (`pkg/validation`), Resource Interactions

### Community 49 - "SMART on FHIR v2 & Identity Architecture"
Cohesion: 0.50
Nodes (4): Decoupled `TokenValidator` Interface (`pkg/auth`), Environment Configuration, Security Guarantees, SMART on FHIR v2 & Identity Architecture

### Community 50 - "automq_test.go"
Cohesion: 0.38
Nodes (6): IcebergColumn, ProtoToExpectedIceberg(), TestAutoMQTableTopicConfig(), TestExpectedIcebergSchema(), TestFlattenVsNested(), TestWrapperProtoDesign()

### Community 53 - "HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness"
Cohesion: 0.50
Nodes (3): 1. Quick Automated Certification (No Docker Required), 2. Running Official HL7 Inferno UI (`localhost:4567`), HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness

### Community 54 - "ExtractAllergyIntoleranceIndexes"
Cohesion: 0.50
Nodes (3): ExtractAllergyIntoleranceIndexes(), SearchIndexes, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/allergy_intolerance_go_proto.AllergyIntolerance

### Community 55 - "ExtractDiagnosticReportIndexes"
Cohesion: 0.50
Nodes (3): ExtractDiagnosticReportIndexes(), SearchIndexes, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/diagnostic_report_go_proto.DiagnosticReport

### Community 56 - "ExtractImmunizationIndexes"
Cohesion: 0.50
Nodes (3): ExtractImmunizationIndexes(), SearchIndexes, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/immunization_go_proto.Immunization

### Community 57 - "ExtractMedicationRequestIndexes"
Cohesion: 0.50
Nodes (3): ExtractMedicationRequestIndexes(), SearchIndexes, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/medication_request_go_proto.MedicationRequest

### Community 58 - "ExtractOrganizationIndexes"
Cohesion: 0.50
Nodes (3): ExtractOrganizationIndexes(), SearchIndexes, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/organization_go_proto.Organization

### Community 59 - "ExtractProcedureIndexes"
Cohesion: 0.50
Nodes (3): ExtractProcedureIndexes(), SearchIndexes, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/procedure_go_proto.Procedure

## Knowledge Gaps
- **59 isolated node(s):** `flint-analytics`, `SpidxToken`, `SpidxString`, `SpidxReference`, `github.com/flint-fhir/flint` (+54 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 206 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **14 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `SecurityContext` connect `SecurityContext` to `FHIRString`, `Server`, `ParseScopes`, `auth.pb.go`, `google.golang.org/protobuf/reflect/protoreflect.Message`, `SmartScope`, `OIDCValidator`?**
  _High betweenness centrality (0.185) - this node is a cross-community bridge._
- **Why does `Server` connect `Server` to `context.Context`, `SecurityContext`, `loadtest/main.go`, `New`, `Producer`, `json_codec.go`?**
  _High betweenness centrality (0.091) - this node is a cross-community bridge._
- **Why does `SmartScope` connect `SmartScope` to `SecurityContext`, `Action`, `FHIRString`, `ParseScopes`, `ScopeContext`, `auth.pb.go`, `google.golang.org/protobuf/reflect/protoreflect.Message`?**
  _High betweenness centrality (0.078) - this node is a cross-community bridge._
- **What connects `flint-analytics`, `SpidxToken`, `SpidxString` to the rest of the system?**
  _59 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `context.Context` be split into smaller, more focused modules?**
  _Cohesion score 0.08251748251748252 - nodes in this community are weakly interconnected._
- **Should `New` be split into smaller, more focused modules?**
  _Cohesion score 0.11282051282051282 - nodes in this community are weakly interconnected._
- **Should `FHIRString` be split into smaller, more focused modules?**
  _Cohesion score 0.13043478260869565 - nodes in this community are weakly interconnected._