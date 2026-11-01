package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	bcrpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/bundle_and_contained_resource_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestSQLOnFHIRv2_ViewDefinitionCRUD_Run_Export_And_SyncAnalytics(t *testing.T) {
	db := openMemPostgres(t)
	defer db.Close()

	pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
	dorisReplica := doris.New(nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Seed a pre-existing Patient directly in pgStore BEFORE attaching Doris analytical replica
	prePatProto, err := proto.Marshal(&patpb.Patient{
		Id: &dtpb.Id{Value: "sof-pre-pat"},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "PreExisting"},
				Given:  []*dtpb.String{{Value: "Grace"}},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, pgStore.WriteResource(context.Background(), store.ResourceInput{
		TenantID:      "sof",
		ResType:       "Patient",
		ResID:         "sof-pre-pat",
		ResourceProto: prePatProto,
	}))

	srv := server.New(pgStore, logger)
	srv.SetAnalyticalStore(dorisReplica)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	// 1. Trigger POST /$sync-analytics to backfill pre-existing Postgres resources into Doris
	reqSync, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/sof/$sync-analytics", nil)
	require.NoError(t, err)
	respSync, err := client.Do(reqSync)
	require.NoError(t, err)
	syncBody, _ := io.ReadAll(respSync.Body)
	respSync.Body.Close()
	assert.Equal(t, http.StatusOK, respSync.StatusCode)
	assert.Equal(t, "doris", respSync.Header.Get("X-Flint-Analytical-Backend"))
	assert.Contains(t, string(syncBody), `"syncedResources"`)
	assert.Contains(t, string(syncBody), `"valueInteger":1`)

	// 2. Seed additional Patient & Observations via FHIR REST API (auto-replicated to Doris)
	pat2 := `{
		"resourceType": "Patient",
		"id": "sof-pat-2",
		"active": true,
		"gender": "male",
		"birthDate": "1978-11-22",
		"name": [
			{"use": "official", "family": "Vance", "given": ["Marcus", "Lee"]},
			{"use": "nickname", "family": "Vance", "given": ["Marc"]}
		],
		"telecom": [
			{"system": "phone", "value": "555-0101"},
			{"system": "email", "value": "marcus@example.org"}
		]
	}`
	reqP2, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/sof/Patient/sof-pat-2", bytes.NewBufferString(pat2))
	reqP2.Header.Set("Content-Type", "application/fhir+json")
	respP2, err := client.Do(reqP2)
	require.NoError(t, err)
	respP2.Body.Close()
	assert.Equal(t, http.StatusCreated, respP2.StatusCode)

	obs1 := `{
		"resourceType": "Observation",
		"id": "sof-obs-hr",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]},
		"subject": {"reference": "Patient/sof-pat-2"},
		"effectiveDateTime": "2026-03-01T10:00:00Z",
		"valueQuantity": {"value": 84.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
	}`
	reqO1, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/sof/Observation/sof-obs-hr", bytes.NewBufferString(obs1))
	reqO1.Header.Set("Content-Type", "application/fhir+json")
	respO1, err := client.Do(reqO1)
	require.NoError(t, err)
	respO1.Body.Close()
	assert.Equal(t, http.StatusCreated, respO1.StatusCode)

	// 3. Built-in ViewDefinition preset: GET /ViewDefinition/patient-demographics/$run (JSON)
	respDem, err := client.Get(ts.URL + "/fhir/r4/sof/ViewDefinition/patient-demographics/$run")
	require.NoError(t, err)
	demBytes, _ := io.ReadAll(respDem.Body)
	respDem.Body.Close()
	assert.Equal(t, http.StatusOK, respDem.StatusCode)
	assert.Equal(t, "doris", respDem.Header.Get("X-Flint-Analytical-Backend"))
	var demRows []map[string]any
	require.NoError(t, json.Unmarshal(demBytes, &demRows))
	assert.Len(t, demRows, 2)

	// 4. Built-in ViewDefinition preset: GET /ViewDefinition/observation-vitals/$run?_format=text/csv
	respCSV, err := client.Get(ts.URL + "/fhir/r4/sof/ViewDefinition/observation-vitals/$run?_format=text/csv")
	require.NoError(t, err)
	csvBytes, _ := io.ReadAll(respCSV.Body)
	respCSV.Body.Close()
	assert.Equal(t, http.StatusOK, respCSV.StatusCode)
	assert.Contains(t, respCSV.Header.Get("Content-Type"), "text/csv")
	csvText := string(csvBytes)
	assert.Contains(t, csvText, "id,patient_id,status,code,system,value,unit,effective")
	assert.Contains(t, csvText, "sof-obs-hr,sof-pat-2,final,8867-4,http://loinc.org,84,/min,2026-03-01T10:00:00Z")

	// 5. Custom ViewDefinition CRUD (PUT, GET, list Bundle, DELETE)
	customVD := `{
		"resourceType": "ViewDefinition",
		"id": "custom-patient-telecom",
		"name": "custom_patient_telecom",
		"status": "active",
		"resource": "Patient",
		"constant": [
			{"name": "target_family", "valueString": "Vance"}
		],
		"where": [
			{"path": "name.where(family = %target_family).exists()"}
		],
		"select": [
			{
				"column": [
					{"name": "patient_id", "path": "getResourceKey()"},
					{"name": "all_given", "path": "name.given", "collection": true}
				]
			},
			{
				"forEachOrNull": "telecom",
				"column": [
					{"name": "contact_system", "path": "system"},
					{"name": "contact_value", "path": "value"}
				]
			}
		]
	}`
	reqPutVD, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/sof/ViewDefinition/custom-patient-telecom", bytes.NewBufferString(customVD))
	reqPutVD.Header.Set("Content-Type", "application/fhir+json")
	respPutVD, err := client.Do(reqPutVD)
	require.NoError(t, err)
	respPutVD.Body.Close()
	assert.Equal(t, http.StatusCreated, respPutVD.StatusCode)

	// GET /ViewDefinition/custom-patient-telecom
	respGetVD, err := client.Get(ts.URL + "/fhir/r4/sof/ViewDefinition/custom-patient-telecom")
	require.NoError(t, err)
	respGetVD.Body.Close()
	assert.Equal(t, http.StatusOK, respGetVD.StatusCode)

	// GET /ViewDefinition (searchset Bundle)
	respListVD, err := client.Get(ts.URL + "/fhir/r4/sof/ViewDefinition")
	require.NoError(t, err)
	listVDBytes, _ := io.ReadAll(respListVD.Body)
	respListVD.Body.Close()
	assert.Equal(t, http.StatusOK, respListVD.StatusCode)
	assert.Contains(t, string(listVDBytes), `"custom-patient-telecom"`)

	// Execute GET /ViewDefinition/custom-patient-telecom/$export (NDJSON)
	respExpVD, err := client.Get(ts.URL + "/fhir/r4/sof/ViewDefinition/custom-patient-telecom/$export")
	require.NoError(t, err)
	expVDBytes, _ := io.ReadAll(respExpVD.Body)
	respExpVD.Body.Close()
	assert.Equal(t, http.StatusOK, respExpVD.StatusCode)
	assert.Contains(t, respExpVD.Header.Get("Content-Type"), "application/ndjson")
	lines := strings.Split(strings.TrimSpace(string(expVDBytes)), "\n")
	assert.Len(t, lines, 2) // 2 telecom entries on sof-pat-2
	assert.Contains(t, lines[0], `"patient_id":"sof-pat-2"`)
	assert.Contains(t, lines[0], `"555-0101"`)

	// 6. POST /ViewDefinition/$run with Parameters payload + unionAll + forEach
	paramsPayload := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "_format", "valueCode": "application/json"},
			{"name": "_limit", "valueInteger": 5},
			{
				"name": "viewResource",
				"resource": {
					"resourceType": "ViewDefinition",
					"resource": "Patient",
					"select": [
						{
							"column": [
								{"name": "id", "path": "getResourceKey()"}
							],
							"unionAll": [
								{
									"forEach": "name.where(use = 'official')",
									"column": [
										{"name": "name_type", "path": "'official'"},
										{"name": "family", "path": "family"}
									]
								},
								{
									"forEach": "name.where(use = 'nickname')",
									"column": [
										{"name": "name_type", "path": "'nickname'"},
										{"name": "family", "path": "family"}
									]
								}
							]
						}
					]
				}
			}
		]
	}`
	respUnion, err := client.Post(ts.URL+"/fhir/r4/sof/ViewDefinition/$run", "application/fhir+json", bytes.NewBufferString(paramsPayload))
	require.NoError(t, err)
	unionBytes, _ := io.ReadAll(respUnion.Body)
	respUnion.Body.Close()
	assert.Equal(t, http.StatusOK, respUnion.StatusCode)
	var unionRows []map[string]any
	require.NoError(t, json.Unmarshal(unionBytes, &unionRows))
	assert.Len(t, unionRows, 2)

	// 7. Single-valued column violation returns 422 Unprocessable Entity when collection: false yields >1 item
	badCardVD := `{
		"resourceType": "ViewDefinition",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "given_names_not_collection", "path": "name.given", "collection": false}
				]
			}
		]
	}`
	respBadCard, err := client.Post(ts.URL+"/fhir/r4/sof/ViewDefinition/$run", "application/fhir+json", bytes.NewBufferString(badCardVD))
	require.NoError(t, err)
	respBadCard.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, respBadCard.StatusCode)

	// 8. Delete custom ViewDefinition
	reqDelVD, _ := http.NewRequest(http.MethodDelete, ts.URL+"/fhir/r4/sof/ViewDefinition/custom-patient-telecom", nil)
	respDelVD, err := client.Do(reqDelVD)
	require.NoError(t, err)
	respDelVD.Body.Close()
	assert.Equal(t, http.StatusNoContent, respDelVD.StatusCode)

	// 9. Verify Zero-JSON headers on ViewDefinition/$run and test ViewDefinition/$sql transpiler across Doris, Postgres, and SQLite dialects
	assert.Equal(t, "zero-json-proto3", respDem.Header.Get("X-Flint-Storage-Model"))
	assert.Contains(t, respDem.Header.Get("X-Flint-Generated-SQL"), "fhir_patient")

	// 9a. Doris dialect (Parameters JSON) for observation-vitals -> queries zero-blob fhir_observation
	respSQLDoris, err := client.Get(ts.URL + "/fhir/r4/sof/ViewDefinition/observation-vitals/$sql?dialect=doris&_limit=25")
	require.NoError(t, err)
	sqlDorisBytes, _ := io.ReadAll(respSQLDoris.Body)
	respSQLDoris.Body.Close()
	assert.Equal(t, http.StatusOK, respSQLDoris.StatusCode)
	assert.Contains(t, string(sqlDorisBytes), "fhir_observation")
	assert.Contains(t, string(sqlDorisBytes), "zero-json-proto3-doris-mow")
	assert.NotContains(t, string(sqlDorisBytes), "json_extract")
	assert.NotContains(t, string(sqlDorisBytes), "jsonb")

	// 9b. PostgreSQL & SQLite dialects (raw application/sql output) -> queries fhir_resource + spidx_* covering indexes
	for _, dialect := range []string{"postgres", "sqlite"} {
		respSQL, err := client.Get(ts.URL + "/fhir/r4/sof/ViewDefinition/patient-demographics/$sql?dialect=" + dialect + "&_format=application/sql")
		require.NoError(t, err)
		rawSQL, _ := io.ReadAll(respSQL.Body)
		respSQL.Body.Close()
		assert.Equal(t, http.StatusOK, respSQL.StatusCode)
		assert.Contains(t, respSQL.Header.Get("Content-Type"), "application/sql")
		assert.Contains(t, string(rawSQL), "FROM fhir_resource r")
		assert.Contains(t, string(rawSQL), "spidx_string")
		assert.Contains(t, string(rawSQL), "spidx_token")
		assert.Contains(t, string(rawSQL), "spidx_date")
		assert.NotContains(t, string(rawSQL), "jsonb")
		assert.NotContains(t, string(rawSQL), "json_extract")
	}

	// 9c. POST /ViewDefinition/$sql with inline ViewDefinition and where clause
	inlineSQLVD := `{
		"resourceType": "ViewDefinition",
		"resource": "Observation",
		"where": [
			{"path": "code.coding.exists(code = '8867-4')"},
			{"path": "status = 'final'"}
		],
		"select": [
			{
				"column": [
					{"name": "id", "path": "getResourceKey()"},
					{"name": "patient_id", "path": "subject.getReferenceKey(Patient)"},
					{"name": "value_quantity", "path": "valueQuantity.value", "type": "decimal"}
				]
			}
		]
	}`
	for _, dialect := range []string{"doris", "postgres"} {
		respInlineSQL, err := client.Post(ts.URL+"/fhir/r4/sof/ViewDefinition/$sql?dialect="+dialect+"&_format=sql", "application/fhir+json", bytes.NewBufferString(inlineSQLVD))
		require.NoError(t, err)
		inlineSQLBytes, _ := io.ReadAll(respInlineSQL.Body)
		respInlineSQL.Body.Close()
		assert.Equal(t, http.StatusOK, respInlineSQL.StatusCode)
		assert.Contains(t, string(inlineSQLBytes), "8867-4")
	}

	// 10. Execute GET /ViewDefinition/patient-demographics/$run?_format=text/tab-separated-values
	respTSV, err := client.Get(ts.URL + "/fhir/r4/sof/ViewDefinition/patient-demographics/$run?_format=text/tab-separated-values")
	require.NoError(t, err)
	tsvBytes, _ := io.ReadAll(respTSV.Body)
	respTSV.Body.Close()
	assert.Equal(t, http.StatusOK, respTSV.StatusCode)
	assert.Contains(t, respTSV.Header.Get("Content-Type"), "text/tab-separated-values")
	assert.Equal(t, "zero-json-proto3", respTSV.Header.Get("X-Flint-Storage-Model"))
	tsvStr := string(tsvBytes)
	assert.Contains(t, tsvStr, "id\tfamily\tgiven\tgender\tbirth_date")
	assert.Contains(t, tsvStr, "sof-pat-2\tVance\tMarcus\tmale\t1978-11-22\ttrue")
}

func TestProto3WireHTTPTransport_CreateUpdateReadVReadAndSearch(t *testing.T) {
	db := openMemPostgres(t)
	defer db.Close()

	pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(pgStore, logger)
	server.RegisterDefaultResourceTypes(srv)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := ts.Client()

	// 1. POST /fhir/r4/proto/Patient with binary google/fhir R4 Protobuf wire payload (Content-Type: application/x-protobuf)
	createPat := &patpb.Patient{
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "WireProto"},
				Given:  []*dtpb.String{{Value: "Alan"}},
			},
		},
	}
	createWire, err := proto.Marshal(createPat)
	require.NoError(t, err)

	reqCreate, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient", bytes.NewReader(createWire))
	require.NoError(t, err)
	reqCreate.Header.Set("Content-Type", "application/x-protobuf")
	respCreate, err := client.Do(reqCreate)
	require.NoError(t, err)
	createdWire, _ := io.ReadAll(respCreate.Body)
	respCreate.Body.Close()
	require.Equal(t, http.StatusCreated, respCreate.StatusCode)
	assert.Equal(t, "application/x-protobuf", respCreate.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respCreate.Header.Get("X-Flint-Wire-Format"))
	assert.Equal(t, "zero-json-proto3", respCreate.Header.Get("X-Flint-Storage-Model"))

	var createdPat patpb.Patient
	require.NoError(t, proto.Unmarshal(createdWire, &createdPat))
	patID := createdPat.GetId().GetValue()
	require.NotEmpty(t, patID)
	assert.Equal(t, "1", createdPat.GetMeta().GetVersionId().GetValue())
	require.Len(t, createdPat.GetName(), 1)
	assert.Equal(t, "WireProto", createdPat.GetName()[0].GetFamily().GetValue())

	// 2. PUT /fhir/r4/proto/Patient/{id} with Content-Type: application/fhir+protobuf
	updatePat := &patpb.Patient{
		Id: &dtpb.Id{Value: patID},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "WireProtoUpdated"},
				Given:  []*dtpb.String{{Value: "Alan"}, {Value: "Mathison"}},
			},
		},
	}
	updateWire, err := proto.Marshal(updatePat)
	require.NoError(t, err)

	reqUpdate, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/proto/Patient/"+patID, bytes.NewReader(updateWire))
	require.NoError(t, err)
	reqUpdate.Header.Set("Content-Type", "application/fhir+protobuf")
	respUpdate, err := client.Do(reqUpdate)
	require.NoError(t, err)
	updatedWire, _ := io.ReadAll(respUpdate.Body)
	respUpdate.Body.Close()
	require.Equal(t, http.StatusOK, respUpdate.StatusCode)
	assert.Equal(t, "application/fhir+protobuf", respUpdate.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respUpdate.Header.Get("X-Flint-Wire-Format"))

	var updatedPat patpb.Patient
	require.NoError(t, proto.Unmarshal(updatedWire, &updatedPat))
	assert.Equal(t, "2", updatedPat.GetMeta().GetVersionId().GetValue())
	assert.Equal(t, "WireProtoUpdated", updatedPat.GetName()[0].GetFamily().GetValue())

	// 3. GET /fhir/r4/proto/Patient/{id} with Accept: application/x-protobuf
	reqRead, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient/"+patID, nil)
	require.NoError(t, err)
	reqRead.Header.Set("Accept", "application/x-protobuf")
	respRead, err := client.Do(reqRead)
	require.NoError(t, err)
	readWire, _ := io.ReadAll(respRead.Body)
	respRead.Body.Close()
	require.Equal(t, http.StatusOK, respRead.StatusCode)
	assert.Equal(t, "application/x-protobuf", respRead.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respRead.Header.Get("X-Flint-Wire-Format"))

	var readPat patpb.Patient
	require.NoError(t, proto.Unmarshal(readWire, &readPat))
	assert.Equal(t, patID, readPat.GetId().GetValue())
	assert.Equal(t, "2", readPat.GetMeta().GetVersionId().GetValue())
	assert.Equal(t, "WireProtoUpdated", readPat.GetName()[0].GetFamily().GetValue())

	// 4. GET /fhir/r4/proto/Patient/{id}/_history/1?_format=application/x-protobuf (vread binary Proto3)
	respVRead, err := client.Get(ts.URL + "/fhir/r4/proto/Patient/" + patID + "/_history/1?_format=application/x-protobuf")
	require.NoError(t, err)
	vreadWire, _ := io.ReadAll(respVRead.Body)
	respVRead.Body.Close()
	require.Equal(t, http.StatusOK, respVRead.StatusCode)
	assert.Equal(t, "application/x-protobuf", respVRead.Header.Get("Content-Type"))

	var vreadPat patpb.Patient
	require.NoError(t, proto.Unmarshal(vreadWire, &vreadPat))
	assert.Equal(t, "1", vreadPat.GetMeta().GetVersionId().GetValue())
	assert.Equal(t, "WireProto", vreadPat.GetName()[0].GetFamily().GetValue())

	// 5. Verify search index extraction worked directly on the binary Proto3 write (GET /Patient?family=WireProtoUpdated)
	respSearch, err := client.Get(ts.URL + "/fhir/r4/proto/Patient?family=WireProtoUpdated")
	require.NoError(t, err)
	searchBytes, _ := io.ReadAll(respSearch.Body)
	respSearch.Body.Close()
	require.Equal(t, http.StatusOK, respSearch.StatusCode)
	assert.Contains(t, string(searchBytes), patID)
	assert.Contains(t, string(searchBytes), "WireProtoUpdated")

	// 6. POST /fhir/r4/proto/Patient/$convert with JSON -> binary Proto3 wire bytes (Accept: application/x-protobuf)
	jsonInput := `{"resourceType":"Patient","id":"conv-proto-1","name":[{"family":"Lovelace","given":["Ada"]}],"birthDate":"1815-12-10"}`
	reqConvToProto, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/$convert", strings.NewReader(jsonInput))
	require.NoError(t, err)
	reqConvToProto.Header.Set("Content-Type", "application/fhir+json")
	reqConvToProto.Header.Set("Accept", "application/x-protobuf")
	respConvToProto, err := client.Do(reqConvToProto)
	require.NoError(t, err)
	convProtoBytes, _ := io.ReadAll(respConvToProto.Body)
	respConvToProto.Body.Close()
	require.Equal(t, http.StatusOK, respConvToProto.StatusCode)
	assert.Equal(t, "application/x-protobuf", respConvToProto.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respConvToProto.Header.Get("X-Flint-Wire-Format"))
	assert.Equal(t, "zero-json-proto3", respConvToProto.Header.Get("X-Flint-Storage-Model"))

	var convPat patpb.Patient
	require.NoError(t, proto.Unmarshal(convProtoBytes, &convPat))
	assert.Equal(t, "conv-proto-1", convPat.GetId().GetValue())
	require.Len(t, convPat.GetName(), 1)
	assert.Equal(t, "Lovelace", convPat.GetName()[0].GetFamily().GetValue())

	// 7. POST /fhir/r4/proto/Patient/$convert with binary Proto3 wire bytes -> canonical FHIR R4 JSON
	reqConvToJSON, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/$convert", bytes.NewReader(convProtoBytes))
	require.NoError(t, err)
	reqConvToJSON.Header.Set("Content-Type", "application/x-protobuf")
	reqConvToJSON.Header.Set("Accept", "application/fhir+json")
	respConvToJSON, err := client.Do(reqConvToJSON)
	require.NoError(t, err)
	convJSONBytes, _ := io.ReadAll(respConvToJSON.Body)
	respConvToJSON.Body.Close()
	require.Equal(t, http.StatusOK, respConvToJSON.StatusCode)
	assert.Contains(t, respConvToJSON.Header.Get("Content-Type"), "application/fhir+json")
	assert.Equal(t, "zero-json-proto3", respConvToJSON.Header.Get("X-Flint-Storage-Model"))
	assert.Contains(t, string(convJSONBytes), `"Lovelace"`)
	assert.Contains(t, string(convJSONBytes), `"1815-12-10"`)

	// 8. POST /fhir/r4/proto/ViewDefinition/$sql with constants + compound AND/OR where predicates + numeric >= & !=
	multiPredVD := `{
		"resourceType": "ViewDefinition",
		"name": "bp_threshold_view",
		"status": "active",
		"resource": "Observation",
		"constant": [
			{"name": "minVal", "valueDecimal": 120},
			{"name": "errStatus", "valueCode": "entered-in-error"}
		],
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "patient_id", "path": "subject.getReferenceKey('Patient')"},
					{"name": "value", "path": "valueQuantity.value"}
				]
			}
		],
		"where": [
			{"path": "valueQuantity.value >= %minVal and status != %errStatus"}
		]
	}`
	reqDorisSQL, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect=doris&_format=application/sql", strings.NewReader(multiPredVD))
	require.NoError(t, err)
	reqDorisSQL.Header.Set("Content-Type", "application/fhir+json")
	respDorisSQL, err := client.Do(reqDorisSQL)
	require.NoError(t, err)
	dorisSQLBytes, _ := io.ReadAll(respDorisSQL.Body)
	respDorisSQL.Body.Close()
	require.Equal(t, http.StatusOK, respDorisSQL.StatusCode)
	dorisSQLStr := string(dorisSQLBytes)
	assert.Contains(t, dorisSQLStr, "r.value_quantity >= 120")
	assert.Contains(t, dorisSQLStr, "NOT array_contains(r.code_tokens, 'status=|entered-in-error')")

	reqPgSQL, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect=postgres&_format=application/sql", strings.NewReader(multiPredVD))
	require.NoError(t, err)
	reqPgSQL.Header.Set("Content-Type", "application/fhir+json")
	respPgSQL, err := client.Do(reqPgSQL)
	require.NoError(t, err)
	pgSQLBytes, _ := io.ReadAll(respPgSQL.Body)
	respPgSQL.Body.Close()
	require.Equal(t, http.StatusOK, respPgSQL.StatusCode)
	pgSQLStr := string(pgSQLBytes)
	assert.Contains(t, pgSQLStr, "q.sp_value >= 120")
	assert.Contains(t, pgSQLStr, "NOT EXISTS (SELECT 1 FROM spidx_token t")

	// 9. GET /fhir/r4/proto/Patient?family=WireProtoUpdated with Accept: application/x-protobuf -> binary Proto3 Bundle
	reqProtoSearch, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient?family=WireProtoUpdated", nil)
	require.NoError(t, err)
	reqProtoSearch.Header.Set("Accept", "application/x-protobuf")
	respProtoSearch, err := client.Do(reqProtoSearch)
	require.NoError(t, err)
	protoSearchBytes, _ := io.ReadAll(respProtoSearch.Body)
	respProtoSearch.Body.Close()
	require.Equal(t, http.StatusOK, respProtoSearch.StatusCode)
	assert.Equal(t, "application/x-protobuf", respProtoSearch.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respProtoSearch.Header.Get("X-Flint-Wire-Format"))
	assert.Equal(t, "zero-json-proto3", respProtoSearch.Header.Get("X-Flint-Storage-Model"))

	var searchBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(protoSearchBytes, &searchBundle))
	assert.Equal(t, uint32(1), searchBundle.GetTotal().GetValue())
	require.Len(t, searchBundle.GetEntry(), 1)
	matchedPat := searchBundle.GetEntry()[0].GetResource().GetPatient()
	require.NotNil(t, matchedPat)
	assert.Equal(t, patID, matchedPat.GetId().GetValue())
	assert.Equal(t, "WireProtoUpdated", matchedPat.GetName()[0].GetFamily().GetValue())

	// 10. POST /fhir/r4/proto/Bundle/$convert with binary Proto3 Bundle -> FHIR R4 JSON Bundle
	reqBundleConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Bundle/$convert", bytes.NewReader(protoSearchBytes))
	require.NoError(t, err)
	reqBundleConv.Header.Set("Content-Type", "application/x-protobuf")
	reqBundleConv.Header.Set("Accept", "application/fhir+json")
	respBundleConv, err := client.Do(reqBundleConv)
	require.NoError(t, err)
	bundleConvJSON, _ := io.ReadAll(respBundleConv.Body)
	respBundleConv.Body.Close()
	require.Equal(t, http.StatusOK, respBundleConv.StatusCode)
	assert.Contains(t, string(bundleConvJSON), `"resourceType":"Bundle"`)
	assert.Contains(t, string(bundleConvJSON), `"WireProtoUpdated"`)

	// 11. GET /fhir/r4/proto/Patient/{id}/_history with Accept: application/fhir+protobuf -> binary Proto3 History Bundle
	reqProtoHist, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient/"+patID+"/_history", nil)
	require.NoError(t, err)
	reqProtoHist.Header.Set("Accept", "application/fhir+protobuf")
	respProtoHist, err := client.Do(reqProtoHist)
	require.NoError(t, err)
	protoHistBytes, _ := io.ReadAll(respProtoHist.Body)
	respProtoHist.Body.Close()
	require.Equal(t, http.StatusOK, respProtoHist.StatusCode)
	assert.Equal(t, "application/fhir+protobuf", respProtoHist.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respProtoHist.Header.Get("X-Flint-Wire-Format"))

	var histBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(protoHistBytes, &histBundle))
	assert.Equal(t, uint32(2), histBundle.GetTotal().GetValue())
	require.Len(t, histBundle.GetEntry(), 2)
	assert.Equal(t, patID, histBundle.GetEntry()[0].GetResource().GetPatient().GetId().GetValue())
	assert.Contains(t, histBundle.GetEntry()[0].GetResponse().GetStatus().GetValue(), "200")

	// 12. PATCH /fhir/r4/proto/Patient/{id} with Accept: application/x-protobuf -> binary Proto3 Patient response
	patchPayload := `[{"op":"replace","path":"/name/0/family","value":"WireProtoPatched"}]`
	reqProtoPatch, err := http.NewRequest(http.MethodPatch, ts.URL+"/fhir/r4/proto/Patient/"+patID, strings.NewReader(patchPayload))
	require.NoError(t, err)
	reqProtoPatch.Header.Set("Content-Type", "application/json-patch+json")
	reqProtoPatch.Header.Set("Accept", "application/x-protobuf")
	respProtoPatch, err := client.Do(reqProtoPatch)
	require.NoError(t, err)
	patchedWire, _ := io.ReadAll(respProtoPatch.Body)
	respProtoPatch.Body.Close()
	require.Equal(t, http.StatusOK, respProtoPatch.StatusCode)
	assert.Equal(t, "application/x-protobuf", respProtoPatch.Header.Get("Content-Type"))

	var patchedPat patpb.Patient
	require.NoError(t, proto.Unmarshal(patchedWire, &patchedPat))
	assert.Equal(t, "3", patchedPat.GetMeta().GetVersionId().GetValue())
	assert.Equal(t, "WireProtoPatched", patchedPat.GetName()[0].GetFamily().GetValue())

	// 13. ViewDefinition/$sql with unionAll + predicate functions (.exists(), .empty(), not(...), .startsWith(), .contains())
	unionPredVD := `{
		"resourceType": "ViewDefinition",
		"name": "patient_union_pred_view",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"}
				],
				"unionAll": [
					{
						"column": [
							{"name": "name_part", "path": "name.family.first()"},
							{"name": "part_type", "path": "'family'"}
						]
					},
					{
						"column": [
							{"name": "name_part", "path": "name.given.first()"},
							{"name": "part_type", "path": "'given'"}
						]
					}
				]
			}
		],
		"where": [
			{"path": "birthDate.exists() and deceasedDateTime.empty() and not(active = false) and name.family.startsWith('Wire') and name.given.contains('Al')"}
		]
	}`
	reqUnionDoris, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect=doris&_format=application/sql", strings.NewReader(unionPredVD))
	require.NoError(t, err)
	reqUnionDoris.Header.Set("Content-Type", "application/fhir+json")
	respUnionDoris, err := client.Do(reqUnionDoris)
	require.NoError(t, err)
	unionDorisBytes, _ := io.ReadAll(respUnionDoris.Body)
	respUnionDoris.Body.Close()
	require.Equal(t, http.StatusOK, respUnionDoris.StatusCode)
	unionDorisSQL := string(unionDorisBytes)
	assert.Contains(t, unionDorisSQL, "UNION ALL")
	assert.Contains(t, unionDorisSQL, "'family' AS part_type")
	assert.Contains(t, unionDorisSQL, "'given' AS part_type")
	assert.Contains(t, unionDorisSQL, "IS NOT NULL")
	assert.Contains(t, unionDorisSQL, "NOT (")
	assert.Contains(t, unionDorisSQL, "LIKE 'family=Wire%'")
	assert.Contains(t, unionDorisSQL, "LIKE 'given=%Al%'")

	reqUnionPg, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect=postgres&_format=application/sql", strings.NewReader(unionPredVD))
	require.NoError(t, err)
	reqUnionPg.Header.Set("Content-Type", "application/fhir+json")
	respUnionPg, err := client.Do(reqUnionPg)
	require.NoError(t, err)
	unionPgBytes, _ := io.ReadAll(respUnionPg.Body)
	respUnionPg.Body.Close()
	require.Equal(t, http.StatusOK, respUnionPg.StatusCode)
	unionPgSQL := string(unionPgBytes)
	assert.Contains(t, unionPgSQL, "UNION ALL")
	assert.Contains(t, unionPgSQL, "'family' AS part_type")
	assert.Contains(t, unionPgSQL, "'given' AS part_type")
	assert.Contains(t, unionPgSQL, "spidx_date")
	assert.Contains(t, unionPgSQL, "LIKE 'Wire%'")
	assert.Contains(t, unionPgSQL, "LIKE '%Al%'")
}
