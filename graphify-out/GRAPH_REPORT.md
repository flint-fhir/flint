# Graph Report - flint  (2026-10-07)

## Corpus Check
- 53 files · ~216,517 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 408 nodes · 697 edges · 29 communities (17 shown, 11 thin omitted)
- Extraction: 97% EXTRACTED · 3% INFERRED · 0% AMBIGUOUS · INFERRED: 19 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `442097e9`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- context.Context
- testing.T
- loadtest/main.go
- FHIRString
- Server
- GenderCode
- spike4_search_index/search_index_test.go
- MiniPatient
- condition_search_index.go
- FHIRDate
- spike_test.go
- Extension
- google.golang.org/protobuf/reflect/protoreflect.Message
- mini_fhir.pb.go
- HumanName
- ingest.go
- bundle.go
- .Search
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

## God Nodes (most connected - your core abstractions)
1. `FHIRString` - 28 edges
2. `MiniPatient` - 23 edges
3. `Extension` - 22 edges
4. `FHIRBoolean` - 19 edges
5. `Server` - 18 edges
6. `FHIRDate` - 17 edges
7. `HumanName` - 15 edges
8. `Identifier` - 14 edges
9. `Coding` - 13 edges
10. `insertSearchIndexes()` - 12 edges

## Surprising Connections (you probably didn't know these)
- `Activities` --references--> `Producer`  [EXTRACTED]
  ingest/activity/activities.go → automq/producer.go
- `runStoreLoad()` --calls--> `DefaultIndexExtractors()`  [EXTRACTED]
  cmd/loadtest/main.go → store/postgres/extract.go
- `runStoreLoad()` --calls--> `New()`  [EXTRACTED]
  cmd/loadtest/main.go → store/postgres/store.go
- `main()` --calls--> `DefaultIndexExtractors()`  [EXTRACTED]
  cmd/worker/main.go → store/postgres/extract.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/worker/main.go → store/postgres/store.go

## Import Cycles
- None detected.

## Communities (29 total, 11 thin omitted)

### Community 0 - "context.Context"
Cohesion: 0.10
Nodes (29): Activities, PublishToAutoMQInput, WriteMedplumBatchInput, WritePostgresBatchInput, main(), context.Context, database/sql.DB, database/sql.Tx (+21 more)

### Community 1 - "testing.T"
Cohesion: 0.05
Nodes (48): ExtractEncounterIndexes(), SearchIndexes, ExtractObservationIndexes(), SearchIndexes, ExtractPatientIndexes(), SearchIndexes, ExtractPractitionerIndexes(), SearchIndexes (+40 more)

### Community 2 - "loadtest/main.go"
Cohesion: 0.36
Nodes (12): generateBundlePayloads(), main(), printReport(), ratio(), runHTTPLoad(), runStoreLoad(), runTemporalLoad(), sync/atomic.Int64 (+4 more)

### Community 3 - "FHIRString"
Cohesion: 0.13
Nodes (5): google.golang.org/protobuf/runtime/protoimpl.MessageState, google.golang.org/protobuf/runtime/protoimpl.UnknownFields, Coding, FHIRString, Identifier

### Community 4 - "Server"
Cohesion: 0.16
Nodes (12): google.golang.org/protobuf/proto.Message, google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor, net/http.Handler, net/http.Request, net/http.ResponseWriter, Server, generateID(), Server (+4 more)

### Community 5 - "GenderCode"
Cohesion: 0.18
Nodes (4): google.golang.org/protobuf/reflect/protoreflect.EnumDescriptor, google.golang.org/protobuf/reflect/protoreflect.EnumNumber, google.golang.org/protobuf/reflect/protoreflect.EnumType, GenderCode

### Community 6 - "spike4_search_index/search_index_test.go"
Cohesion: 0.41
Nodes (12): SearchIndexes, SpidxDate, SpidxReference, SpidxString, SpidxToken, assertStringValue(), ExtractPatientIndexes(), findDates() (+4 more)

### Community 8 - "condition_search_index.go"
Cohesion: 0.09
Nodes (22): mockString, ExtractConditionIndexes(), SpidxDate, SpidxReference, SpidxString, SpidxToken, TestExtractConditionIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto.Condition (+14 more)

### Community 10 - "spike_test.go"
Cohesion: 0.50
Nodes (8): protoNameToFHIR(), ProtoToFHIRJSON(), TestProtojsonMarshalOutput(), TestRoundTripFidelity(), unwrap(), unwrapHumanNames(), unwrapIdentifiers(), unwrapStringArray()

### Community 13 - "mini_fhir.pb.go"
Cohesion: 0.25
Nodes (5): Extension_ValueBoolean, Extension_ValueDateTime, Extension_ValueString, file_spike_spike1_proto_mini_fhir_proto_init(), init()

### Community 15 - "ingest.go"
Cohesion: 0.38
Nodes (6): go.temporal.io/sdk/workflow.Context, IngestFHIRBundle(), IngestInput, publishToAutoMQInput, writeMedplumBatchInput, writePostgresBatchInput

### Community 16 - "bundle.go"
Cohesion: 0.47
Nodes (5): encoding/json.RawMessage, bundleErrorResponse(), bundleEntry, bundleEntryRequest, bundleRequest

### Community 17 - ".Search"
Cohesion: 0.47
Nodes (4): DateOp, SearchParams, SearchResult, Store

### Community 18 - "Producer"
Cohesion: 0.13
Nodes (19): Config, Message, Producer, NewProducer(), topicSet(), runDelete(), envOrDefault(), headerMap() (+11 more)

### Community 19 - "Flint — High-Performance FHIR R4 Server on a Data Lakehouse"
Cohesion: 0.06
Nodes (32): Code Generation (`proto2type` & `buf`), Code of Conduct, Contributing to Flint, Development Environment Setup, Git Workflow & Conventional Commits, Local Development Stack, Prerequisites, Quick Start with Nix & Direnv (+24 more)

### Community 26 - "Contributor Covenant Code of Conduct"
Cohesion: 0.17
Nodes (12): 1. Correction, 2. Warning, 3. Temporary Ban, 4. Permanent Ban, Attribution, Contributor Covenant Code of Conduct, Enforcement, Enforcement Guidelines (+4 more)

### Community 27 - "roundtrip.go"
Cohesion: 0.36
Nodes (9): FHIRToProtoJSON(), ProtoJSONToFHIR(), RoundTrip(), TestBirthDateWithExtension(), TestFullPatientRoundTrip(), TestGenderEnumMapping(), TestWrapUnwrapPrimitive(), UnwrapPrimitive() (+1 more)

## Knowledge Gaps
- **45 isolated node(s):** `SpidxToken`, `SpidxString`, `SpidxReference`, `github.com/flint-fhir/flint`, `writeMedplumBatchInput` (+40 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 132 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **11 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `Server` connect `Server` to `context.Context`, `Producer`?**
  _High betweenness centrality (0.071) - this node is a cross-community bridge._
- **Why does `New()` connect `context.Context` to `testing.T`, `loadtest/main.go`, `Producer`?**
  _High betweenness centrality (0.062) - this node is a cross-community bridge._
- **Why does `TestServer_Integration()` connect `context.Context` to `testing.T`?**
  _High betweenness centrality (0.061) - this node is a cross-community bridge._
- **What connects `SpidxToken`, `SpidxString`, `SpidxReference` to the rest of the system?**
  _45 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `context.Context` be split into smaller, more focused modules?**
  _Cohesion score 0.1 - nodes in this community are weakly interconnected._
- **Should `testing.T` be split into smaller, more focused modules?**
  _Cohesion score 0.053005464480874315 - nodes in this community are weakly interconnected._
- **Should `FHIRString` be split into smaller, more focused modules?**
  _Cohesion score 0.12987012987012986 - nodes in this community are weakly interconnected._