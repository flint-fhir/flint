# Graph Report - flint  (2026-10-09)

## Corpus Check
- 82 files · ~240,361 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 671 nodes · 1313 edges · 44 communities (32 shown, 11 thin omitted)
- Extraction: 98% EXTRACTED · 2% INFERRED · 0% AMBIGUOUS · INFERRED: 31 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `f6a4bb9d`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- context.Context
- extract_test.go
- loadtest/main.go
- FHIRString
- Server
- MiniPatient
- spike4_search_index/search_index_test.go
- FHIRBoolean
- condition_search_index.go
- SmartScope
- spike_test.go
- Extension
- google.golang.org/protobuf/reflect/protoreflect.Message
- mini_fhir.pb.go
- Identifier
- ingest.go
- SecurityContext
- search_parse.go
- Producer
- Flint — High-Performance FHIR R4 Server on a Data Lakehouse
- query_duckdb.py
- query_iceberg.py
- github.com/flint-fhir/flint
- rules/graphify.md
- workflows/graphify.md
- Contributor Covenant Code of Conduct
- roundtrip.go
- file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP
- audit/audit.go
- testing.T
- Outcome
- ExtractEncounterIndexes
- Contributing to Flint
- ExtractPatientIndexes
- README.md
- automq_test.go
- Quickstart
- Enforcement Guidelines
- NewEngine
- SMART on FHIR v2 & Identity Architecture
- API Reference
- bundle.go
- TestSchema_NoJSONBAndAtlasSumSync

## God Nodes (most connected - your core abstractions)
1. `Server` - 34 edges
2. `FHIRString` - 28 edges
3. `SecurityContext` - 26 edges
4. `MiniPatient` - 23 edges
5. `Extension` - 22 edges
6. `SmartScope` - 20 edges
7. `FHIRBoolean` - 19 edges
8. `SmartConfiguration` - 18 edges
9. `FHIRDate` - 17 edges
10. `Store` - 17 edges

## Surprising Connections (you probably didn't know these)
- `Activities` --references--> `Producer`  [EXTRACTED]
  ingest/activity/activities.go → automq/producer.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/flintd/main.go → server/server.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/flintd/main.go → store/postgres/store.go
- `runStoreLoad()` --calls--> `DefaultIndexExtractors()`  [EXTRACTED]
  cmd/loadtest/main.go → store/postgres/extract.go
- `main()` --calls--> `DefaultIndexExtractors()`  [EXTRACTED]
  cmd/worker/main.go → store/postgres/extract.go

## Import Cycles
- None detected.

## Communities (44 total, 11 thin omitted)

### Community 0 - "context.Context"
Cohesion: 0.09
Nodes (29): Activities, PublishToAutoMQInput, WriteMedplumBatchInput, WritePostgresBatchInput, MemoryRecorder, StoreRecorder, context.Context, database/sql.Tx (+21 more)

### Community 1 - "extract_test.go"
Cohesion: 0.20
Nodes (15): DefaultIndexExtractors(), ExtractCondition(), ExtractEncounter(), ExtractObservation(), ExtractPatient(), ExtractPractitioner(), SearchIndexes, TestDefaultIndexExtractors() (+7 more)

### Community 2 - "loadtest/main.go"
Cohesion: 0.21
Nodes (16): generateBundlePayloads(), main(), printReport(), ratio(), runHTTPLoad(), runStoreLoad(), runTemporalLoad(), database/sql.DB (+8 more)

### Community 3 - "FHIRString"
Cohesion: 0.13
Nodes (4): google.golang.org/protobuf/runtime/protoimpl.MessageState, Coding, FHIRString, HumanName

### Community 4 - "Server"
Cohesion: 0.12
Nodes (20): google.golang.org/protobuf/proto.Message, google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor, google.golang.org/protobuf/types/dynamicpb.Message, net/http.Request, net/http.ResponseWriter, Recorder, TokenValidator, Validator (+12 more)

### Community 6 - "spike4_search_index/search_index_test.go"
Cohesion: 0.41
Nodes (12): SearchIndexes, SpidxDate, SpidxReference, SpidxString, SpidxToken, assertStringValue(), ExtractPatientIndexes(), findDates() (+4 more)

### Community 8 - "condition_search_index.go"
Cohesion: 0.09
Nodes (23): mockString, ExtractConditionIndexes(), SpidxDate, SpidxReference, SpidxString, SpidxToken, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto.Condition, time.Time (+15 more)

### Community 9 - "SmartScope"
Cohesion: 0.05
Nodes (10): file_proto_flint_auth_v1_auth_proto_init(), file_proto_flint_auth_v1_auth_proto_rawDescGZIP(), init(), google.golang.org/protobuf/reflect/protoreflect.EnumDescriptor, google.golang.org/protobuf/reflect/protoreflect.EnumNumber, google.golang.org/protobuf/reflect/protoreflect.EnumType, Action, ScopeContext (+2 more)

### Community 10 - "spike_test.go"
Cohesion: 0.50
Nodes (8): protoNameToFHIR(), ProtoToFHIRJSON(), TestProtojsonMarshalOutput(), TestRoundTripFidelity(), unwrap(), unwrapHumanNames(), unwrapIdentifiers(), unwrapStringArray()

### Community 11 - "Extension"
Cohesion: 0.13
Nodes (4): google.golang.org/protobuf/runtime/protoimpl.UnknownFields, Extension, FHIRDate, isExtension_Value

### Community 13 - "mini_fhir.pb.go"
Cohesion: 0.20
Nodes (6): Extension_ValueBoolean, Extension_ValueDate, Extension_ValueDateTime, Extension_ValueString, file_spike_spike1_proto_mini_fhir_proto_init(), init()

### Community 15 - "ingest.go"
Cohesion: 0.38
Nodes (6): go.temporal.io/sdk/workflow.Context, IngestFHIRBundle(), IngestInput, publishToAutoMQInput, writeMedplumBatchInput, writePostgresBatchInput

### Community 16 - "SecurityContext"
Cohesion: 0.05
Nodes (41): Audience, contextKey, JWK, JWKSet, JWTHeader, JWTPayload, MockTokenValidator, OIDCConfig (+33 more)

### Community 17 - "search_parse.go"
Cohesion: 0.14
Nodes (22): ChainedParam, DateOp, IncludeParam, QuantityOp, SearchParams, SearchResult, SearchResults, buildDateCondition() (+14 more)

### Community 18 - "Producer"
Cohesion: 0.12
Nodes (20): Config, Message, Producer, NewProducer(), topicSet(), runDelete(), envOrDefault(), headerMap() (+12 more)

### Community 19 - "Flint — High-Performance FHIR R4 Server on a Data Lakehouse"
Cohesion: 0.22
Nodes (9): Architecture, Community & Contributing, Flint — High-Performance FHIR R4 Server on a Data Lakehouse, License, Project Structure, Property-Based Testing (via Hegel), Supported FHIR Resources (Big 5), Verified Benchmarks (+1 more)

### Community 26 - "Contributor Covenant Code of Conduct"
Cohesion: 0.29
Nodes (7): Attribution, Contributor Covenant Code of Conduct, Enforcement, Enforcement Responsibilities, Our Pledge, Our Standards, Scope

### Community 27 - "roundtrip.go"
Cohesion: 0.36
Nodes (9): FHIRToProtoJSON(), ProtoJSONToFHIR(), RoundTrip(), TestBirthDateWithExtension(), TestFullPatientRoundTrip(), TestGenderEnumMapping(), TestWrapUnwrapPrimitive(), UnwrapPrimitive() (+1 more)

### Community 29 - "audit/audit.go"
Cohesion: 0.12
Nodes (17): net/http.Handler, ClassifyHTTPInteraction(), ClassifyHTTPOutcome(), NewMemoryRecorder(), TestProperty_ClassifyNeverPanicsOnArbitraryFuzz(), TestProperty_InteractionClassificationAndFHIRSerialization(), TestProperty_OutcomeClassificationInvariance(), TestClassifyHTTPInteraction() (+9 more)

### Community 30 - "testing.T"
Cohesion: 0.14
Nodes (12): TestProducer_BuildRecord(), testing.T, TestPublishToAutoMQ_NilProducer(), TestWritePostgresBatch_WithExtractors(), TestIngestFHIRBundle_AutoMQFailure_Fails(), TestIngestFHIRBundle_HappyPath(), TestIngestFHIRBundle_MedplumFailure_ContinuesAnyway(), TestIngestFHIRBundle_PostgresFailure_Fails() (+4 more)

### Community 31 - "Outcome"
Cohesion: 0.16
Nodes (12): NewOutcome(), buildFieldMap(), DefaultSchemas(), extractStringOrWrapped(), isEmptyValue(), Engine, Issue, IssueCode (+4 more)

### Community 32 - "ExtractEncounterIndexes"
Cohesion: 0.12
Nodes (13): ExtractEncounterIndexes(), SearchIndexes, ExtractObservationIndexes(), SearchIndexes, ExtractPractitionerIndexes(), SearchIndexes, TestExtractConditionIndexes(), TestExtractEncounterIndexes() (+5 more)

### Community 33 - "Contributing to Flint"
Cohesion: 0.22
Nodes (9): Code Generation (`proto2type` & `buf`), Code of Conduct, Contributing to Flint, Development Environment Setup, Git Workflow & Conventional Commits, Local Development Stack, Prerequisites, Quick Start with Nix & Direnv (+1 more)

### Community 34 - "ExtractPatientIndexes"
Cohesion: 0.29
Nodes (6): ExtractPatientIndexes(), SearchIndexes, TestExtractPatientIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto.Patient, buildTestPatient(), TestProtoProduceConsume()

### Community 35 - "README.md"
Cohesion: 0.22
Nodes (6): Healthcare & Security Architecture Principles, Information to Include, Reporting a Vulnerability, Response Timeline, Security Policy, Supported Versions

### Community 36 - "automq_test.go"
Cohesion: 0.38
Nodes (6): IcebergColumn, ProtoToExpectedIceberg(), TestAutoMQTableTopicConfig(), TestExpectedIcebergSchema(), TestFlattenVsNested(), TestWrapperProtoDesign()

### Community 37 - "Quickstart"
Cohesion: 0.33
Nodes (6): 1. Development Environment (Hermetic Nix Shell), 2. Launch Local Dev Stack (k3d), 3. Build & Run Flint, 4. Run Benchmarks & Tests, 5. Query Analytical Iceberg Table with DuckDB, Quickstart

### Community 38 - "Enforcement Guidelines"
Cohesion: 0.40
Nodes (5): 1. Correction, 2. Warning, 3. Temporary Ban, 4. Permanent Ban, Enforcement Guidelines

### Community 39 - "NewEngine"
Cohesion: 0.26
Nodes (13): TestProperty_InvalidGenderRejected(), TestProperty_ObservationRequiredFields(), TestProperty_ValidatorNeverPanics(), TestProperty_ValidPatientInvariance(), DefaultOptions(), NewEngine(), TestValidationEngine_PrimitiveFormats(), TestValidationEngine_RequiredElements() (+5 more)

### Community 40 - "SMART on FHIR v2 & Identity Architecture"
Cohesion: 0.50
Nodes (4): Decoupled `TokenValidator` Interface (`pkg/auth`), Environment Configuration, Security Guarantees, SMART on FHIR v2 & Identity Architecture

### Community 41 - "API Reference"
Cohesion: 0.40
Nodes (5): Advanced FHIR Search Engine, API Reference, Batch / Transaction Bundles, FHIR Conformance & Validation Engine (`pkg/validation`), Resource Interactions

### Community 42 - "bundle.go"
Cohesion: 0.47
Nodes (5): encoding/json.RawMessage, bundleErrorResponse(), bundleEntry, bundleEntryRequest, bundleRequest

## Knowledge Gaps
- **53 isolated node(s):** `SpidxToken`, `SpidxString`, `SpidxReference`, `github.com/flint-fhir/flint`, `writeMedplumBatchInput` (+48 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 175 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **11 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `SecurityContext` connect `SecurityContext` to `FHIRString`, `SmartScope`, `Extension`, `google.golang.org/protobuf/reflect/protoreflect.Message`, `Identifier`?**
  _High betweenness centrality (0.215) - this node is a cross-community bridge._
- **Why does `SmartScope` connect `SmartScope` to `SecurityContext`, `Extension`, `FHIRString`, `Identifier`?**
  _High betweenness centrality (0.108) - this node is a cross-community bridge._
- **Why does `Server` connect `Server` to `context.Context`, `Producer`, `audit/audit.go`, `NewEngine`?**
  _High betweenness centrality (0.065) - this node is a cross-community bridge._
- **What connects `SpidxToken`, `SpidxString`, `SpidxReference` to the rest of the system?**
  _53 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `context.Context` be split into smaller, more focused modules?**
  _Cohesion score 0.09216255442670537 - nodes in this community are weakly interconnected._
- **Should `FHIRString` be split into smaller, more focused modules?**
  _Cohesion score 0.12554112554112554 - nodes in this community are weakly interconnected._
- **Should `Server` be split into smaller, more focused modules?**
  _Cohesion score 0.1207897793263647 - nodes in this community are weakly interconnected._