# Graph Report - flint  (2026-10-07)

## Corpus Check
- 48 files · ~211,767 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 349 nodes · 608 edges · 26 communities (18 shown, 7 thin omitted)
- Extraction: 97% EXTRACTED · 3% INFERRED · 0% AMBIGUOUS · INFERRED: 17 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `c08d3b06`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- context.Context
- testing.T
- Producer
- FHIRString
- Server
- file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP
- spike4_search_index/search_index_test.go
- MiniPatient
- helpers.go
- spike_test.go
- Extension
- google.golang.org/protobuf/reflect/protoreflect.Message
- mini_fhir.pb.go
- ingest.go
- bundle.go
- .Search
- NewProducer
- Flint — Open Source FHIR Server on a Data Lake
- query_duckdb.py
- query_iceberg.py
- github.com/flint-fhir/flint
- rules/graphify.md
- workflows/graphify.md
- postgres/search_index_test.go
- condition_search_index.go

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
- `main()` --calls--> `DefaultIndexExtractors()`  [EXTRACTED]
  cmd/worker/main.go → store/postgres/extract.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/worker/main.go → store/postgres/store.go
- `bulkInsertQuantities()` --references--> `SpidxQuantity`  [EXTRACTED]
  store/postgres/store.go → gen/go/store/postgres/condition_search_index.go
- `bulkInsertURIs()` --references--> `SpidxURI`  [EXTRACTED]
  store/postgres/store.go → gen/go/store/postgres/condition_search_index.go
- `ExtractCondition()` --calls--> `ExtractConditionIndexes()`  [EXTRACTED]
  store/postgres/extract.go → gen/go/store/postgres/condition_search_index.go

## Import Cycles
- None detected.

## Communities (26 total, 7 thin omitted)

### Community 0 - "context.Context"
Cohesion: 0.16
Nodes (22): main(), context.Context, database/sql.DB, database/sql.Tx, ResourceInput, New(), TestServer_Integration(), bulkInsertDates() (+14 more)

### Community 1 - "testing.T"
Cohesion: 0.07
Nodes (40): testing.T, TestPublishToAutoMQ_NilProducer(), TestWritePostgresBatch_WithExtractors(), TestIngestFHIRBundle_AutoMQFailure_Fails(), TestIngestFHIRBundle_HappyPath(), TestIngestFHIRBundle_MedplumFailure_ContinuesAnyway(), TestIngestFHIRBundle_PostgresFailure_Fails(), IcebergColumn (+32 more)

### Community 2 - "Producer"
Cohesion: 0.20
Nodes (10): Activities, PublishToAutoMQInput, WriteMedplumBatchInput, WritePostgresBatchInput, Config, Message, Producer, topicSet() (+2 more)

### Community 3 - "FHIRString"
Cohesion: 0.11
Nodes (7): google.golang.org/protobuf/runtime/protoimpl.MessageState, google.golang.org/protobuf/runtime/protoimpl.SizeCache, google.golang.org/protobuf/runtime/protoimpl.UnknownFields, Coding, FHIRString, HumanName, Identifier

### Community 4 - "Server"
Cohesion: 0.16
Nodes (12): google.golang.org/protobuf/proto.Message, google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor, net/http.Handler, net/http.Request, net/http.ResponseWriter, Server, generateID(), Server (+4 more)

### Community 5 - "file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP"
Cohesion: 0.11
Nodes (5): google.golang.org/protobuf/reflect/protoreflect.EnumDescriptor, google.golang.org/protobuf/reflect/protoreflect.EnumNumber, google.golang.org/protobuf/reflect/protoreflect.EnumType, GenderCode, file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP()

### Community 6 - "spike4_search_index/search_index_test.go"
Cohesion: 0.41
Nodes (12): SearchIndexes, SpidxDate, SpidxReference, SpidxString, SpidxToken, assertStringValue(), ExtractPatientIndexes(), findDates() (+4 more)

### Community 8 - "helpers.go"
Cohesion: 0.16
Nodes (11): mockString, time.Time, FHIRDateToTime(), NormalizeString(), StringValue(), StringValues(), TestFHIRDateToTime(), TestNormalizeString() (+3 more)

### Community 10 - "spike_test.go"
Cohesion: 0.50
Nodes (8): protoNameToFHIR(), ProtoToFHIRJSON(), TestProtojsonMarshalOutput(), TestRoundTripFidelity(), unwrap(), unwrapHumanNames(), unwrapIdentifiers(), unwrapStringArray()

### Community 11 - "Extension"
Cohesion: 0.12
Nodes (3): Extension, FHIRDate, isExtension_Value

### Community 13 - "mini_fhir.pb.go"
Cohesion: 0.20
Nodes (6): Extension_ValueBoolean, Extension_ValueDate, Extension_ValueDateTime, Extension_ValueString, file_spike_spike1_proto_mini_fhir_proto_init(), init()

### Community 15 - "ingest.go"
Cohesion: 0.38
Nodes (6): go.temporal.io/sdk/workflow.Context, IngestFHIRBundle(), IngestInput, publishToAutoMQInput, writeMedplumBatchInput, writePostgresBatchInput

### Community 16 - "bundle.go"
Cohesion: 0.47
Nodes (5): encoding/json.RawMessage, bundleErrorResponse(), bundleEntry, bundleEntryRequest, bundleRequest

### Community 17 - ".Search"
Cohesion: 0.47
Nodes (4): DateOp, SearchParams, SearchResult, Store

### Community 18 - "NewProducer"
Cohesion: 0.21
Nodes (10): NewProducer(), runDelete(), envOrDefault(), headerMap(), main(), runPipeline(), runUpdate(), envOrDefault() (+2 more)

### Community 19 - "Flint — Open Source FHIR Server on a Data Lake"
Cohesion: 0.29
Nodes (6): Architecture, Flint — Open Source FHIR Server on a Data Lake, Key Design Decisions, License, Project Structure, Quick Start

### Community 26 - "postgres/search_index_test.go"
Cohesion: 0.09
Nodes (18): ExtractEncounterIndexes(), SearchIndexes, ExtractObservationIndexes(), SearchIndexes, ExtractPatientIndexes(), SearchIndexes, ExtractPractitionerIndexes(), SearchIndexes (+10 more)

### Community 27 - "condition_search_index.go"
Cohesion: 0.18
Nodes (13): ExtractConditionIndexes(), SpidxDate, SpidxReference, SpidxString, SpidxToken, TestExtractConditionIndexes(), github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto.Condition, SearchIndexes (+5 more)

## Knowledge Gaps
- **15 isolated node(s):** `SpidxToken`, `SpidxString`, `SpidxReference`, `github.com/flint-fhir/flint`, `writeMedplumBatchInput` (+10 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 101 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **7 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `Server` connect `Server` to `context.Context`, `Producer`?**
  _High betweenness centrality (0.089) - this node is a cross-community bridge._
- **Why does `TestServer_Integration()` connect `context.Context` to `testing.T`?**
  _High betweenness centrality (0.083) - this node is a cross-community bridge._
- **Why does `New()` connect `context.Context` to `testing.T`, `NewProducer`?**
  _High betweenness centrality (0.071) - this node is a cross-community bridge._
- **What connects `SpidxToken`, `SpidxString`, `SpidxReference` to the rest of the system?**
  _15 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `testing.T` be split into smaller, more focused modules?**
  _Cohesion score 0.07346938775510205 - nodes in this community are weakly interconnected._
- **Should `FHIRString` be split into smaller, more focused modules?**
  _Cohesion score 0.10574712643678161 - nodes in this community are weakly interconnected._
- **Should `file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP` be split into smaller, more focused modules?**
  _Cohesion score 0.11052631578947368 - nodes in this community are weakly interconnected._