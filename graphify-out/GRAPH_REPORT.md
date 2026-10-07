# Graph Report - flint  (2026-10-07)

## Corpus Check
- 45 files · ~209,877 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 338 nodes · 570 edges · 28 communities (17 shown, 10 thin omitted)
- Extraction: 97% EXTRACTED · 3% INFERRED · 0% AMBIGUOUS · INFERRED: 17 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `53ee89ba`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- store.go
- testing.T
- Producer
- FHIRString
- Server
- file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP
- spike4_search_index/search_index_test.go
- MiniPatient
- time.Time
- FHIRDate
- spike_test.go
- Extension
- google.golang.org/protobuf/reflect/protoreflect.Message
- mini_fhir.pb.go
- Identifier
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
5. `FHIRDate` - 17 edges
6. `Server` - 16 edges
7. `HumanName` - 15 edges
8. `Identifier` - 14 edges
9. `Coding` - 13 edges
10. `insertSearchIndexes()` - 12 edges

## Surprising Connections (you probably didn't know these)
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/worker/main.go → store/postgres/store.go
- `Activities` --references--> `Store`  [EXTRACTED]
  ingest/activity/activities.go → store/postgres/store.go
- `Server` --references--> `Store`  [EXTRACTED]
  server/server.go → store/postgres/store.go
- `Activities` --references--> `Producer`  [EXTRACTED]
  ingest/activity/activities.go → automq/producer.go
- `runDelete()` --calls--> `NewProducer()`  [EXTRACTED]
  cmd/e2e/delete.go → automq/producer.go

## Import Cycles
- None detected.

## Communities (28 total, 10 thin omitted)

### Community 0 - "store.go"
Cohesion: 0.12
Nodes (30): main(), context.Context, database/sql.DB, database/sql.Tx, ResourceInput, New(), TestServer_Integration(), bulkInsertDates() (+22 more)

### Community 1 - "testing.T"
Cohesion: 0.11
Nodes (23): testing.T, TestIngestFHIRBundle_AutoMQFailure_Fails(), TestIngestFHIRBundle_HappyPath(), TestIngestFHIRBundle_MedplumFailure_ContinuesAnyway(), TestIngestFHIRBundle_PostgresFailure_Fails(), IcebergColumn, FHIRToProtoJSON(), ProtoJSONToFHIR() (+15 more)

### Community 2 - "Producer"
Cohesion: 0.19
Nodes (11): Activities, IndexExtractorFunc, PublishToAutoMQInput, WriteMedplumBatchInput, WritePostgresBatchInput, Config, Message, Producer (+3 more)

### Community 3 - "FHIRString"
Cohesion: 0.13
Nodes (5): google.golang.org/protobuf/runtime/protoimpl.MessageState, google.golang.org/protobuf/runtime/protoimpl.UnknownFields, Coding, FHIRString, HumanName

### Community 4 - "Server"
Cohesion: 0.18
Nodes (11): google.golang.org/protobuf/proto.Message, google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor, net/http.Handler, net/http.Request, net/http.ResponseWriter, Server, generateID(), Server (+3 more)

### Community 5 - "file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP"
Cohesion: 0.11
Nodes (5): google.golang.org/protobuf/reflect/protoreflect.EnumDescriptor, google.golang.org/protobuf/reflect/protoreflect.EnumNumber, google.golang.org/protobuf/reflect/protoreflect.EnumType, GenderCode, file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP()

### Community 6 - "spike4_search_index/search_index_test.go"
Cohesion: 0.41
Nodes (12): SearchIndexes, SpidxDate, SpidxReference, SpidxString, SpidxToken, assertStringValue(), ExtractPatientIndexes(), findDates() (+4 more)

### Community 8 - "time.Time"
Cohesion: 0.16
Nodes (11): mockString, SpidxDate, time.Time, FHIRDateToTime(), NormalizeString(), StringValue(), StringValues(), TestFHIRDateToTime() (+3 more)

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
Cohesion: 0.13
Nodes (15): ExtractConditionIndexes(), SearchIndexes, SpidxQuantity, SpidxReference, SpidxString, SpidxToken, SpidxURI, SpidxDate (+7 more)

## Knowledge Gaps
- **17 isolated node(s):** `SpidxToken`, `SpidxString`, `SpidxQuantity`, `SpidxReference`, `SpidxURI` (+12 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 107 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **10 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `TestServer_Integration()` connect `store.go` to `testing.T`?**
  _High betweenness centrality (0.117) - this node is a cross-community bridge._
- **Why does `New()` connect `store.go` to `testing.T`, `NewProducer`?**
  _High betweenness centrality (0.094) - this node is a cross-community bridge._
- **Why does `Server` connect `Server` to `store.go`, `Producer`?**
  _High betweenness centrality (0.089) - this node is a cross-community bridge._
- **What connects `SpidxToken`, `SpidxString`, `SpidxQuantity` to the rest of the system?**
  _17 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `store.go` be split into smaller, more focused modules?**
  _Cohesion score 0.11861861861861862 - nodes in this community are weakly interconnected._
- **Should `testing.T` be split into smaller, more focused modules?**
  _Cohesion score 0.11494252873563218 - nodes in this community are weakly interconnected._
- **Should `FHIRString` be split into smaller, more focused modules?**
  _Cohesion score 0.1341991341991342 - nodes in this community are weakly interconnected._