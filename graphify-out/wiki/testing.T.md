# testing.T

> 28 nodes · cohesion 0.12

## Key Concepts

- **testing.T** (24 connections)
- **spike_test.go** (8 connections) — `spike/spike1_proto/spike_test.go`
- **ProtoToFHIRJSON()** (7 connections) — `spike/spike1_proto/spike_test.go`
- **automq_test.go** (6 connections) — `spike/spike3b_automq_table_topic/automq_test.go`
- **unwrap()** (5 connections) — `spike/spike1_proto/spike_test.go`
- **ingest_test.go** (4 connections) — `ingest/workflow/ingest_test.go`
- **unwrapHumanNames()** (4 connections) — `spike/spike1_proto/spike_test.go`
- **TestProtojsonMarshalOutput()** (3 connections) — `spike/spike1_proto/spike_test.go`
- **TestRoundTripFidelity()** (3 connections) — `spike/spike1_proto/spike_test.go`
- **unwrapIdentifiers()** (3 connections) — `spike/spike1_proto/spike_test.go`
- **unwrapStringArray()** (3 connections) — `spike/spike1_proto/spike_test.go`
- **ProtoToExpectedIceberg()** (3 connections) — `spike/spike3b_automq_table_topic/automq_test.go`
- **TestExpectedIcebergSchema()** (3 connections) — `spike/spike3b_automq_table_topic/automq_test.go`
- **TestStore_Integration()** (3 connections) — `store/postgres/store_test.go`
- **TestIngestFHIRBundle_AutoMQFailure_Fails()** (2 connections) — `ingest/workflow/ingest_test.go`
- **TestIngestFHIRBundle_HappyPath()** (2 connections) — `ingest/workflow/ingest_test.go`
- **TestIngestFHIRBundle_MedplumFailure_ContinuesAnyway()** (2 connections) — `ingest/workflow/ingest_test.go`
- **TestIngestFHIRBundle_PostgresFailure_Fails()** (2 connections) — `ingest/workflow/ingest_test.go`
- **IcebergColumn** (2 connections) — `spike/spike3b_automq_table_topic/automq_test.go`
- **protoNameToFHIR()** (2 connections) — `spike/spike1_proto/spike_test.go`
- **TestRealGoogleFHIRPatientMarshal()** (2 connections) — `spike/spike1_real/real_patient_test.go`
- **TestHardestExpressions()** (2 connections) — `spike/spike2_fhirpath/fhirpath_test.go`
- **TestAutoMQTableTopicConfig()** (2 connections) — `spike/spike3b_automq_table_topic/automq_test.go`
- **TestFlattenVsNested()** (2 connections) — `spike/spike3b_automq_table_topic/automq_test.go`
- **TestWrapperProtoDesign()** (2 connections) — `spike/spike3b_automq_table_topic/automq_test.go`
- *... and 3 more nodes in this community*

## Relationships

- [roundtrip.go](roundtrip.go.md) (4 shared connections)
- [helpers.go](helpers.go.md) (3 shared connections)
- [search_index_test.go](search_index_test.go.md) (3 shared connections)
- [store.go](store.go.md) (2 shared connections)

## Source Files

- `ingest/workflow/ingest_test.go`
- `spike/spike1_proto/spike_test.go`
- `spike/spike1_real/real_patient_test.go`
- `spike/spike2_fhirpath/fhirpath_test.go`
- `spike/spike3b_automq_table_topic/automq_test.go`
- `store/postgres/store_test.go`

## Audit Trail

- EXTRACTED: 58 (100%)
- INFERRED: 0 (0%)
- AMBIGUOUS: 0 (0%)

---

*Part of the graphify knowledge wiki. See [index](index.md) to navigate.*