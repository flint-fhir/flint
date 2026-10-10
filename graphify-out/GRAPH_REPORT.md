# Graph Report - flint  (2026-10-10)

## Corpus Check
- 92 files · ~245,186 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 681 nodes · 1344 edges · 39 communities (26 shown, 12 thin omitted)
- Extraction: 98% EXTRACTED · 2% INFERRED · 0% AMBIGUOUS · INFERRED: 31 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `d1d6bec9`
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
- flint-analytics
- roundtrip.go
- file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP
- .auditMiddleware
- testing.T
- Outcome
- ExtractEncounterIndexes
- ExtractPatientIndexes
- Contributor Covenant Code of Conduct
- automq_test.go
- NewEngine
- bundle.go
- TestSchema_NoJSONBAndAtlasSumSync

## God Nodes (most connected - your core abstractions)
1. `Server` - 36 edges
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
- `main()` --calls--> `NewProducer()`  [EXTRACTED]
  cmd/worker/main.go → automq/producer.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/flintd/main.go → server/server.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/flintd/main.go → store/postgres/store.go
- `Allows()` --references--> `Action`  [EXTRACTED]
  pkg/auth/auth.go → gen/go/flint/auth/v1/auth.pb.go

## Import Cycles
- None detected.

## Communities (39 total, 12 thin omitted)

### Community 0 - "context.Context"
Cohesion: 0.09
Nodes (31): Activities, PublishToAutoMQInput, WriteMedplumBatchInput, WritePostgresBatchInput, MemoryRecorder, StoreRecorder, context.Context, database/sql.Tx (+23 more)

### Community 1 - "extract_test.go"
Cohesion: 0.23
Nodes (13): ExtractCondition(), ExtractEncounter(), ExtractObservation(), ExtractPatient(), ExtractPractitioner(), SearchIndexes, TestExtractCondition(), TestExtractEncounter() (+5 more)

### Community 2 - "loadtest/main.go"
Cohesion: 0.14
Nodes (22): generateBundlePayloads(), main(), printReport(), ratio(), runHTTPLoad(), runStoreLoad(), runTemporalLoad(), envOrDefault() (+14 more)

### Community 3 - "FHIRString"
Cohesion: 0.13
Nodes (4): google.golang.org/protobuf/runtime/protoimpl.MessageState, Coding, FHIRString, HumanName

### Community 4 - "Server"
Cohesion: 0.12
Nodes (21): google.golang.org/protobuf/proto.Message, google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor, google.golang.org/protobuf/types/dynamicpb.Message, net/http.Request, net/http.ResponseWriter, Recorder, ToFHIRResource(), TokenValidator (+13 more)

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
Cohesion: 0.12
Nodes (27): net/url.Values, ChainedParam, DateOp, IncludeParam, PatientEverythingParams, QuantityOp, SearchParams, SearchResult (+19 more)

### Community 18 - "Producer"
Cohesion: 0.14
Nodes (18): Config, Message, Producer, NewProducer(), topicSet(), runDelete(), envOrDefault(), headerMap() (+10 more)

### Community 19 - "Flint — High-Performance FHIR R4 Server on a Data Lakehouse"
Cohesion: 0.08
Nodes (25): 1. Development Environment (Hermetic Nix Shell), 2. Launch Local Dev Stack (k3d), 3. Build & Run Flint, 4. Run Benchmarks & Tests, 5. Query Analytical Iceberg Table with DuckDB, Advanced FHIR Search Engine, API Reference, Architecture (+17 more)

### Community 27 - "roundtrip.go"
Cohesion: 0.36
Nodes (9): FHIRToProtoJSON(), ProtoJSONToFHIR(), RoundTrip(), TestBirthDateWithExtension(), TestFullPatientRoundTrip(), TestGenderEnumMapping(), TestWrapUnwrapPrimitive(), UnwrapPrimitive() (+1 more)

### Community 29 - ".auditMiddleware"
Cohesion: 0.13
Nodes (14): net/http.Handler, ClassifyHTTPInteraction(), ClassifyHTTPOutcome(), TestProperty_ClassifyNeverPanicsOnArbitraryFuzz(), TestProperty_InteractionClassificationAndFHIRSerialization(), TestProperty_OutcomeClassificationInvariance(), TestClassifyHTTPInteraction(), auditActorHolder (+6 more)

### Community 30 - "testing.T"
Cohesion: 0.14
Nodes (11): TestProducer_BuildRecord(), testing.T, TestIngestFHIRBundle_AutoMQFailure_Fails(), TestIngestFHIRBundle_HappyPath(), TestIngestFHIRBundle_MedplumFailure_ContinuesAnyway(), TestIngestFHIRBundle_PostgresFailure_Fails(), TestOutcome_ToOperationOutcomeMap(), TestServer_AuthMiddleware() (+3 more)

### Community 31 - "Outcome"
Cohesion: 0.17
Nodes (11): NewOutcome(), buildFieldMap(), DefaultSchemas(), extractStringOrWrapped(), isEmptyValue(), Engine, Issue, IssueCode (+3 more)

### Community 32 - "ExtractEncounterIndexes"
Cohesion: 0.12
Nodes (13): ExtractEncounterIndexes(), SearchIndexes, ExtractObservationIndexes(), SearchIndexes, ExtractPractitionerIndexes(), SearchIndexes, TestExtractConditionIndexes(), TestExtractEncounterIndexes() (+5 more)

### Community 34 - "ExtractPatientIndexes"
Cohesion: 0.29
Nodes (6): ExtractPatientIndexes(), SearchIndexes, TestExtractPatientIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto.Patient, buildTestPatient(), TestProtoProduceConsume()

### Community 35 - "Contributor Covenant Code of Conduct"
Cohesion: 0.07
Nodes (27): 1. Correction, 2. Warning, 3. Temporary Ban, 4. Permanent Ban, Attribution, Contributor Covenant Code of Conduct, Enforcement, Enforcement Guidelines (+19 more)

### Community 36 - "automq_test.go"
Cohesion: 0.38
Nodes (6): IcebergColumn, ProtoToExpectedIceberg(), TestAutoMQTableTopicConfig(), TestExpectedIcebergSchema(), TestFlattenVsNested(), TestWrapperProtoDesign()

### Community 39 - "NewEngine"
Cohesion: 0.28
Nodes (13): TestProperty_InvalidGenderRejected(), TestProperty_ObservationRequiredFields(), TestProperty_ValidatorNeverPanics(), TestProperty_ValidPatientInvariance(), DefaultOptions(), NewEngine(), TestValidationEngine_PrimitiveFormats(), TestValidationEngine_RequiredElements() (+5 more)

### Community 42 - "bundle.go"
Cohesion: 0.47
Nodes (5): encoding/json.RawMessage, bundleErrorResponse(), bundleEntry, bundleEntryRequest, bundleRequest

## Knowledge Gaps
- **55 isolated node(s):** `flint-analytics`, `SpidxToken`, `SpidxString`, `SpidxReference`, `github.com/flint-fhir/flint` (+50 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 178 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **12 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `SecurityContext` connect `SecurityContext` to `FHIRString`, `SmartScope`, `Extension`, `google.golang.org/protobuf/reflect/protoreflect.Message`, `Identifier`?**
  _High betweenness centrality (0.213) - this node is a cross-community bridge._
- **Why does `SmartScope` connect `SmartScope` to `SecurityContext`, `Extension`, `FHIRString`, `Identifier`?**
  _High betweenness centrality (0.106) - this node is a cross-community bridge._
- **Why does `Server` connect `Server` to `context.Context`, `Producer`, `.auditMiddleware`, `NewEngine`?**
  _High betweenness centrality (0.065) - this node is a cross-community bridge._
- **What connects `flint-analytics`, `SpidxToken`, `SpidxString` to the rest of the system?**
  _55 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `context.Context` be split into smaller, more focused modules?**
  _Cohesion score 0.08636363636363636 - nodes in this community are weakly interconnected._
- **Should `loadtest/main.go` be split into smaller, more focused modules?**
  _Cohesion score 0.13756613756613756 - nodes in this community are weakly interconnected._
- **Should `FHIRString` be split into smaller, more focused modules?**
  _Cohesion score 0.12554112554112554 - nodes in this community are weakly interconnected._