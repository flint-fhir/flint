# Graph Report - flint  (2026-10-10)

## Corpus Check
- 94 files · ~255,853 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 735 nodes · 1556 edges · 53 communities (32 shown, 20 thin omitted)
- Extraction: 97% EXTRACTED · 3% INFERRED · 0% AMBIGUOUS · INFERRED: 39 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `bb72904b`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- context.Context
- MiniPatient
- loadtest/main.go
- FHIRString
- Server
- ParseScopes
- spike4_search_index/search_index_test.go
- FHIRBoolean
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
- AuditRecord
- testing.T
- NewEngine
- ExtractEncounterIndexes
- SecurityContext
- ExtractPatientIndexes
- README.md
- helpers.go
- SmartScope
- Contributing to Flint
- ScopeContext
- Contributor Covenant Code of Conduct
- Quickstart
- bundle.go
- Enforcement Guidelines
- file_proto_flint_auth_v1_auth_proto_rawDescGZIP
- roundtrip.go
- API Reference
- google.golang.org/protobuf/reflect/protoreflect.EnumNumber
- google.golang.org/protobuf/reflect/protoreflect.EnumType
- SMART on FHIR v2 & Identity Architecture
- Identifier
- auth.pb.go
- TestSchema_NoJSONBAndAtlasSumSync

## God Nodes (most connected - your core abstractions)
1. `Server` - 37 edges
2. `FHIRString` - 28 edges
3. `SecurityContext` - 26 edges
4. `MiniPatient` - 23 edges
5. `Store` - 23 edges
6. `Extension` - 22 edges
7. `SmartScope` - 20 edges
8. `FHIRBoolean` - 19 edges
9. `SmartConfiguration` - 18 edges
10. `FHIRDate` - 17 edges

## Surprising Connections (you probably didn't know these)
- `Activities` --references--> `Producer`  [EXTRACTED]
  ingest/activity/activities.go → automq/producer.go
- `main()` --calls--> `NewProducer()`  [EXTRACTED]
  cmd/worker/main.go → automq/producer.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/flintd/main.go → server/server.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/flintd/main.go → store/postgres/store.go
- `runStoreLoad()` --calls--> `DefaultIndexExtractors()`  [EXTRACTED]
  cmd/loadtest/main.go → store/postgres/extract.go

## Import Cycles
- None detected.

## Communities (53 total, 20 thin omitted)

### Community 0 - "context.Context"
Cohesion: 0.08
Nodes (39): Activities, PublishToAutoMQInput, WriteMedplumBatchInput, WritePostgresBatchInput, envOrDefault(), main(), context.Context, database/sql.DB (+31 more)

### Community 2 - "loadtest/main.go"
Cohesion: 0.36
Nodes (12): generateBundlePayloads(), main(), printReport(), ratio(), runHTTPLoad(), runStoreLoad(), runTemporalLoad(), sync/atomic.Int64 (+4 more)

### Community 3 - "FHIRString"
Cohesion: 0.12
Nodes (4): google.golang.org/protobuf/runtime/protoimpl.MessageState, Coding, FHIRString, HumanName

### Community 4 - "Server"
Cohesion: 0.08
Nodes (37): contextKey, github.com/flint-fhir/flint/pkg/auth.SecurityContext, google.golang.org/protobuf/proto.Message, google.golang.org/protobuf/types/dynamicpb.Message, net/http.Handler, net/http.Request, net/http.ResponseWriter, Recorder (+29 more)

### Community 5 - "ParseScopes"
Cohesion: 0.39
Nodes (8): ParseScopes(), ParseSingleScope(), ScopeMatches(), TestProperty_ParseScopesNeverPanicsOnArbitraryInput(), TestProperty_QueryParameterFilterInvariance(), TestProperty_SMARTV1GrammarAcceptance(), TestProperty_SMARTV2GrammarAcceptance(), TestProperty_WildcardResourceMatchesAnyResourceType()

### Community 6 - "spike4_search_index/search_index_test.go"
Cohesion: 0.41
Nodes (12): SearchIndexes, SpidxDate, SpidxReference, SpidxString, SpidxToken, assertStringValue(), ExtractPatientIndexes(), findDates() (+4 more)

### Community 8 - "condition_search_index.go"
Cohesion: 0.18
Nodes (13): ExtractConditionIndexes(), SpidxDate, SpidxReference, SpidxString, SpidxToken, github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto.Condition, SearchIndexes, SpidxDate (+5 more)

### Community 10 - "spike_test.go"
Cohesion: 0.50
Nodes (8): protoNameToFHIR(), ProtoToFHIRJSON(), TestProtojsonMarshalOutput(), TestRoundTripFidelity(), unwrap(), unwrapHumanNames(), unwrapIdentifiers(), unwrapStringArray()

### Community 11 - "Extension"
Cohesion: 0.13
Nodes (4): google.golang.org/protobuf/runtime/protoimpl.UnknownFields, Extension, FHIRDate, isExtension_Value

### Community 13 - "mini_fhir.pb.go"
Cohesion: 0.20
Nodes (6): Extension_ValueBoolean, Extension_ValueDate, Extension_ValueDateTime, Extension_ValueString, file_spike_spike1_proto_mini_fhir_proto_init(), init()

### Community 14 - "extract.go"
Cohesion: 0.20
Nodes (18): github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto.Quantity, appendQuantityIndexes(), ExtractCondition(), ExtractEncounter(), ExtractObservation(), ExtractPatient(), ExtractPractitioner(), SearchIndexes (+10 more)

### Community 15 - "ingest.go"
Cohesion: 0.38
Nodes (6): go.temporal.io/sdk/workflow.Context, IngestFHIRBundle(), IngestInput, publishToAutoMQInput, writeMedplumBatchInput, writePostgresBatchInput

### Community 16 - "OIDCValidator"
Cohesion: 0.08
Nodes (22): Audience, JWK, JWKSet, JWTHeader, JWTPayload, MockTokenValidator, OIDCConfig, OIDCValidator (+14 more)

### Community 17 - "time.Time"
Cohesion: 0.11
Nodes (29): net/url.Values, time.Time, ChainedParam, DateOp, IncludeParam, PatientEverythingParams, QuantityOp, queryExecer (+21 more)

### Community 18 - "Producer"
Cohesion: 0.14
Nodes (18): Config, Message, Producer, NewProducer(), topicSet(), runDelete(), envOrDefault(), headerMap() (+10 more)

### Community 19 - "Flint — High-Performance FHIR R4 Server on a Data Lakehouse"
Cohesion: 0.20
Nodes (10): Architecture, Clinical Dimensional Lakehouse (`analytics/` — SQLMesh on DuckDB / Iceberg), Community & Contributing, Flint — High-Performance FHIR R4 Server on a Data Lakehouse, License, Project Structure, Property-Based Testing (via Hegel), Supported FHIR Resources (Big 5) (+2 more)

### Community 27 - "json_codec.go"
Cohesion: 0.17
Nodes (25): google.golang.org/protobuf/reflect/protoreflect.FieldDescriptor, google.golang.org/protobuf/reflect/protoreflect.FieldDescriptors, google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor, denormalizeMessageMap(), DenormalizeProtoJSONToFHIRJSON(), denormalizeReferenceMap(), denormalizeSingleValue(), findFieldDescriptor() (+17 more)

### Community 29 - "AuditRecord"
Cohesion: 0.12
Nodes (16): MemoryRecorder, StoreRecorder, sync.RWMutex, ClassifyHTTPInteraction(), ClassifyHTTPOutcome(), NewMemoryRecorder(), NewStoreRecorder(), TestProperty_ClassifyNeverPanicsOnArbitraryFuzz() (+8 more)

### Community 30 - "testing.T"
Cohesion: 0.11
Nodes (21): TestProducer_BuildRecord(), testing.T, TestActivities_SliceBoundsAndNilStoreValidation(), TestPublishToAutoMQ_NilProducer(), TestIngestFHIRBundle_AutoMQFailure_Fails(), TestIngestFHIRBundle_HappyPath(), TestIngestFHIRBundle_MedplumFailure_ContinuesAnyway(), TestIngestFHIRBundle_PostgresFailure_Fails() (+13 more)

### Community 31 - "NewEngine"
Cohesion: 0.11
Nodes (25): NewOutcome(), TestProperty_InvalidGenderRejected(), TestProperty_ObservationRequiredFields(), TestProperty_ValidatorNeverPanics(), TestProperty_ValidPatientInvariance(), buildFieldMap(), DefaultSchemas(), DefaultOptions() (+17 more)

### Community 32 - "ExtractEncounterIndexes"
Cohesion: 0.12
Nodes (13): ExtractEncounterIndexes(), SearchIndexes, ExtractObservationIndexes(), SearchIndexes, ExtractPractitionerIndexes(), SearchIndexes, TestExtractConditionIndexes(), TestExtractEncounterIndexes() (+5 more)

### Community 34 - "ExtractPatientIndexes"
Cohesion: 0.29
Nodes (6): ExtractPatientIndexes(), SearchIndexes, TestExtractPatientIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto.Patient, buildTestPatient(), TestProtoProduceConsume()

### Community 35 - "README.md"
Cohesion: 0.22
Nodes (6): Healthcare & Security Architecture Principles, Information to Include, Reporting a Vulnerability, Response Timeline, Security Policy, Supported Versions

### Community 36 - "helpers.go"
Cohesion: 0.20
Nodes (9): mockString, FHIRDateToTime(), NormalizeString(), StringValue(), StringValues(), TestFHIRDateToTime(), TestNormalizeString(), TestStringValue() (+1 more)

### Community 38 - "Contributing to Flint"
Cohesion: 0.22
Nodes (9): Code Generation (`proto2type` & `buf`), Code of Conduct, Contributing to Flint, Development Environment Setup, Git Workflow & Conventional Commits, Local Development Stack, Prerequisites, Quick Start with Nix & Direnv (+1 more)

### Community 40 - "Contributor Covenant Code of Conduct"
Cohesion: 0.29
Nodes (7): Attribution, Contributor Covenant Code of Conduct, Enforcement, Enforcement Responsibilities, Our Pledge, Our Standards, Scope

### Community 41 - "Quickstart"
Cohesion: 0.33
Nodes (6): 1. Development Environment (Hermetic Nix Shell), 2. Launch Local Dev Stack (k3d), 3. Build & Run Flint, 4. Run Benchmarks & Tests, 5. Query Analytical Iceberg Table with DuckDB, Quickstart

### Community 42 - "bundle.go"
Cohesion: 0.47
Nodes (5): encoding/json.RawMessage, bundleErrorResponse(), bundleEntry, bundleEntryRequest, bundleRequest

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

## Knowledge Gaps
- **55 isolated node(s):** `flint-analytics`, `SpidxToken`, `SpidxString`, `SpidxReference`, `github.com/flint-fhir/flint` (+50 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 180 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **20 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `SecurityContext` connect `SecurityContext` to `FHIRString`, `Server`, `SmartScope`, `Extension`, `file_proto_flint_auth_v1_auth_proto_rawDescGZIP`, `google.golang.org/protobuf/reflect/protoreflect.Message`, `OIDCValidator`, `Identifier`, `auth.pb.go`?**
  _High betweenness centrality (0.220) - this node is a cross-community bridge._
- **Why does `SmartScope` connect `SmartScope` to `SecurityContext`, `FHIRString`, `ParseScopes`, `ScopeContext`, `Extension`, `file_proto_flint_auth_v1_auth_proto_rawDescGZIP`, `google.golang.org/protobuf/reflect/protoreflect.Message`, `Identifier`, `auth.pb.go`?**
  _High betweenness centrality (0.086) - this node is a cross-community bridge._
- **Why does `Server` connect `Server` to `context.Context`, `Producer`, `json_codec.go`?**
  _High betweenness centrality (0.067) - this node is a cross-community bridge._
- **What connects `flint-analytics`, `SpidxToken`, `SpidxString` to the rest of the system?**
  _55 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `context.Context` be split into smaller, more focused modules?**
  _Cohesion score 0.07884615384615384 - nodes in this community are weakly interconnected._
- **Should `FHIRString` be split into smaller, more focused modules?**
  _Cohesion score 0.11857707509881422 - nodes in this community are weakly interconnected._
- **Should `Server` be split into smaller, more focused modules?**
  _Cohesion score 0.08098891730605286 - nodes in this community are weakly interconnected._