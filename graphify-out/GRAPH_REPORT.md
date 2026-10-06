# Graph Report - flint  (2026-10-06)

## Corpus Check
- cluster-only mode — file stats not available

## Summary
- 289 nodes · 515 edges · 24 communities (14 shown, 9 thin omitted)
- Extraction: 97% EXTRACTED · 3% INFERRED · 0% AMBIGUOUS · INFERRED: 13 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `3d3d853e`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- store.go
- testing.T
- Producer
- FHIRString
- Server
- file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP
- search_index_test.go
- MiniPatient
- helpers.go
- FHIRDate
- roundtrip.go
- Extension
- google.golang.org/protobuf/reflect/protoreflect.Message
- mini_fhir.pb.go
- Identifier
- ingest.go
- bundle.go
- .Search
- e2e/main.go
- test_update.go
- query_duckdb.py
- query_iceberg.py
- github.com/flint-fhir/flint

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
- `Activities` --references--> `Store`  [EXTRACTED]
  ingest/activity/activities.go → store/postgres/store.go
- `Server` --references--> `Store`  [EXTRACTED]
  server/server.go → store/postgres/store.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/worker/main.go → store/postgres/store.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/flintd/main.go → server/server.go
- `main()` --calls--> `New()`  [EXTRACTED]
  cmd/flintd/main.go → store/postgres/store.go

## Import Cycles
- None detected.

## Communities (24 total, 9 thin omitted)

### Community 0 - "store.go"
Cohesion: 0.13
Nodes (28): main(), context.Context, database/sql.DB, database/sql.Tx, ResourceInput, SearchIndexes, SpidxDate, SpidxQuantity (+20 more)

### Community 1 - "testing.T"
Cohesion: 0.12
Nodes (22): testing.T, TestIngestFHIRBundle_AutoMQFailure_Fails(), TestIngestFHIRBundle_HappyPath(), TestIngestFHIRBundle_MedplumFailure_ContinuesAnyway(), TestIngestFHIRBundle_PostgresFailure_Fails(), IcebergColumn, protoNameToFHIR(), ProtoToFHIRJSON() (+14 more)

### Community 2 - "Producer"
Cohesion: 0.13
Nodes (16): Activities, IndexExtractorFunc, PublishToAutoMQInput, WriteMedplumBatchInput, WritePostgresBatchInput, Config, Message, Producer (+8 more)

### Community 3 - "FHIRString"
Cohesion: 0.13
Nodes (5): google.golang.org/protobuf/runtime/protoimpl.MessageState, google.golang.org/protobuf/runtime/protoimpl.SizeCache, Coding, FHIRString, HumanName

### Community 4 - "Server"
Cohesion: 0.18
Nodes (11): google.golang.org/protobuf/proto.Message, google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor, net/http.Handler, net/http.Request, net/http.ResponseWriter, Server, generateID(), Server (+3 more)

### Community 5 - "file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP"
Cohesion: 0.12
Nodes (5): google.golang.org/protobuf/reflect/protoreflect.EnumDescriptor, google.golang.org/protobuf/reflect/protoreflect.EnumNumber, google.golang.org/protobuf/reflect/protoreflect.EnumType, GenderCode, file_spike_spike1_proto_mini_fhir_proto_rawDescGZIP()

### Community 6 - "search_index_test.go"
Cohesion: 0.27
Nodes (15): github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto.Patient, SearchIndexes, SpidxDate, SpidxReference, SpidxString, SpidxToken, buildTestPatient(), TestProtoProduceConsume() (+7 more)

### Community 8 - "helpers.go"
Cohesion: 0.18
Nodes (10): mockString, time.Time, FHIRDateToTime(), NormalizeString(), StringValue(), StringValues(), TestFHIRDateToTime(), TestNormalizeString() (+2 more)

### Community 10 - "roundtrip.go"
Cohesion: 0.36
Nodes (9): FHIRToProtoJSON(), ProtoJSONToFHIR(), RoundTrip(), TestBirthDateWithExtension(), TestFullPatientRoundTrip(), TestGenderEnumMapping(), TestWrapUnwrapPrimitive(), UnwrapPrimitive() (+1 more)

### Community 13 - "mini_fhir.pb.go"
Cohesion: 0.25
Nodes (5): Extension_ValueBoolean, Extension_ValueDateTime, Extension_ValueString, file_spike_spike1_proto_mini_fhir_proto_init(), init()

### Community 15 - "ingest.go"
Cohesion: 0.33
Nodes (6): go.temporal.io/sdk/workflow.Context, IngestFHIRBundle(), IngestInput, publishToAutoMQInput, writeMedplumBatchInput, writePostgresBatchInput

### Community 16 - "bundle.go"
Cohesion: 0.47
Nodes (5): encoding/json.RawMessage, bundleErrorResponse(), bundleEntry, bundleEntryRequest, bundleRequest

### Community 17 - ".Search"
Cohesion: 0.47
Nodes (4): DateOp, SearchParams, SearchResult, Store

### Community 18 - "e2e/main.go"
Cohesion: 0.60
Nodes (4): envOrDefault(), headerMap(), main(), github.com/twmb/franz-go/pkg/kgo.RecordHeader

## Knowledge Gaps
- **6 isolated node(s):** `publishToAutoMQInput`, `writeMedplumBatchInput`, `writePostgresBatchInput`, `Store`, `github.com/flint-fhir/flint` (+1 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 71 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **9 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `TestServer_Integration()` connect `store.go` to `testing.T`?**
  _High betweenness centrality (0.101) - this node is a cross-community bridge._
- **Why does `Server` connect `Server` to `store.go`, `Producer`?**
  _High betweenness centrality (0.092) - this node is a cross-community bridge._
- **Why does `New()` connect `store.go` to `testing.T`, `Producer`?**
  _High betweenness centrality (0.074) - this node is a cross-community bridge._
- **What connects `publishToAutoMQInput`, `writeMedplumBatchInput`, `writePostgresBatchInput` to the rest of the system?**
  _6 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `store.go` be split into smaller, more focused modules?**
  _Cohesion score 0.13277310924369748 - nodes in this community are weakly interconnected._
- **Should `testing.T` be split into smaller, more focused modules?**
  _Cohesion score 0.12169312169312169 - nodes in this community are weakly interconnected._
- **Should `Producer` be split into smaller, more focused modules?**
  _Cohesion score 0.13043478260869565 - nodes in this community are weakly interconnected._