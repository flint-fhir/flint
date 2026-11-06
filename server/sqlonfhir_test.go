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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	bcrpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/bundle_and_contained_resource_go_proto"
	cspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/capability_statement_go_proto"
	oopb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/operation_outcome_go_proto"
	parampb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/parameters_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	vspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/value_set_go_proto"

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

	// 11. Execute POST /ViewDefinition/$run verifying in-memory FHIRPath evaluation of .tail(), .single(), .combine(), .subsetOf(), .supersetOf(), and .split()
	evalSliceSplitVD := `{
		"resourceType": "ViewDefinition",
		"resource": "Patient",
		"where": [
			{"path": "id = 'sof-pat-2' and name.family.subsetOf(name.family) and name.given.supersetOf(name.where(use = 'nickname').given)"}
		],
		"select": [{
			"column": [
				{"name": "id", "path": "id"},
				{"name": "tail_given", "path": "name.given.tail()", "collection": true},
				{"name": "single_gender", "path": "gender.single()"},
				{"name": "combined_names", "path": "name.given.combine(name.family)", "collection": true},
				{"name": "is_subset", "path": "name.where(use = 'nickname').given.subsetOf(name.given)"},
				{"name": "is_superset", "path": "name.given.supersetOf(name.where(use = 'nickname').given)"},
				{"name": "birth_parts", "path": "birthDate.split('-')", "collection": true}
			]
		}]
	}`
	respEvalSS, err := client.Post(ts.URL+"/fhir/r4/sof/ViewDefinition/$run", "application/fhir+json", bytes.NewBufferString(evalSliceSplitVD))
	require.NoError(t, err)
	evalSSBytes, _ := io.ReadAll(respEvalSS.Body)
	respEvalSS.Body.Close()
	require.Equal(t, http.StatusOK, respEvalSS.StatusCode)
	var evalSSRows []map[string]any
	require.NoError(t, json.Unmarshal(evalSSBytes, &evalSSRows))
	require.Len(t, evalSSRows, 1)
	assert.Equal(t, "sof-pat-2", evalSSRows[0]["id"])
	assert.Equal(t, []any{"Lee", "Marc"}, evalSSRows[0]["tail_given"])
	assert.Equal(t, "male", evalSSRows[0]["single_gender"])
	assert.Equal(t, []any{"Marcus", "Lee", "Marc", "Vance", "Vance"}, evalSSRows[0]["combined_names"])
	assert.Equal(t, true, evalSSRows[0]["is_subset"])
	assert.Equal(t, true, evalSSRows[0]["is_superset"])
	assert.Equal(t, []any{"1978", "11", "22"}, evalSSRows[0]["birth_parts"])
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

	// 14. Binary Proto3 Transaction Bundle (POST /fhir/r4/proto with Content-Type & Accept: application/x-protobuf)
	txBundleJSON := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"fullUrl": "Patient/tx-proto-pat-1",
				"resource": {
					"resourceType": "Patient",
					"id": "tx-proto-pat-1",
					"active": true,
					"gender": "female",
					"birthDate": "1975-08-20",
					"name": [{"family": "ProtoBundleTx", "given": ["Grace"]}],
					"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-PROTO-TX-1"}]
				},
				"request": {
					"method": "PUT",
					"url": "Patient/tx-proto-tx-1"
				}
			}
		]
	}`
	txBundleJSON = strings.ReplaceAll(txBundleJSON, "Patient/tx-proto-tx-1", "Patient/tx-proto-pat-1")
	reqTxConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Bundle/$convert", strings.NewReader(txBundleJSON))
	require.NoError(t, err)
	reqTxConv.Header.Set("Content-Type", "application/fhir+json")
	reqTxConv.Header.Set("Accept", "application/x-protobuf")
	respTxConv, err := client.Do(reqTxConv)
	require.NoError(t, err)
	txProtoBytes, _ := io.ReadAll(respTxConv.Body)
	respTxConv.Body.Close()
	require.Equal(t, http.StatusOK, respTxConv.StatusCode)
	require.NotEmpty(t, txProtoBytes)

	reqProtoTx, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto", bytes.NewReader(txProtoBytes))
	require.NoError(t, err)
	reqProtoTx.Header.Set("Content-Type", "application/x-protobuf")
	reqProtoTx.Header.Set("Accept", "application/x-protobuf")
	reqProtoTx.Header.Set("Prefer", "return=representation")
	respProtoTx, err := client.Do(reqProtoTx)
	require.NoError(t, err)
	txRespProtoBytes, _ := io.ReadAll(respProtoTx.Body)
	respProtoTx.Body.Close()
	require.Equal(t, http.StatusOK, respProtoTx.StatusCode)
	assert.Equal(t, "application/x-protobuf", respProtoTx.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respProtoTx.Header.Get("X-Flint-Wire-Format"))

	var txRespBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(txRespProtoBytes, &txRespBundle))
	require.Len(t, txRespBundle.GetEntry(), 1)
	assert.Contains(t, txRespBundle.GetEntry()[0].GetResponse().GetStatus().GetValue(), "201")
	assert.Equal(t, "tx-proto-pat-1", txRespBundle.GetEntry()[0].GetResource().GetPatient().GetId().GetValue())
	assert.Equal(t, "ProtoBundleTx", txRespBundle.GetEntry()[0].GetResource().GetPatient().GetName()[0].GetFamily().GetValue())

	// 15. Type-level and System-level _history with Accept: application/x-protobuf
	reqTypeHist, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient/_history", nil)
	require.NoError(t, err)
	reqTypeHist.Header.Set("Accept", "application/x-protobuf")
	respTypeHist, err := client.Do(reqTypeHist)
	require.NoError(t, err)
	typeHistBytes, _ := io.ReadAll(respTypeHist.Body)
	respTypeHist.Body.Close()
	require.Equal(t, http.StatusOK, respTypeHist.StatusCode)
	assert.Equal(t, "application/x-protobuf", respTypeHist.Header.Get("Content-Type"))
	var typeHistBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(typeHistBytes, &typeHistBundle))
	assert.GreaterOrEqual(t, int(typeHistBundle.GetTotal().GetValue()), 3)

	reqSysHist, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/_history", nil)
	require.NoError(t, err)
	reqSysHist.Header.Set("Accept", "application/x-protobuf")
	respSysHist, err := client.Do(reqSysHist)
	require.NoError(t, err)
	sysHistBytes, _ := io.ReadAll(respSysHist.Body)
	respSysHist.Body.Close()
	require.Equal(t, http.StatusOK, respSysHist.StatusCode)
	var sysHistBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(sysHistBytes, &sysHistBundle))
	assert.GreaterOrEqual(t, int(sysHistBundle.GetTotal().GetValue()), 3)

	// 16. ViewDefinition/$sql with forEach (inner unnest) and forEachOrNull (outer unnest)
	forEachVD := `{
		"resourceType": "ViewDefinition",
		"name": "patient_identifiers_unnest",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"}
				],
				"select": [
					{
						"forEach": "identifier",
						"column": [
							{"name": "id_system", "path": "system"},
							{"name": "id_value", "path": "value"}
						]
					}
				]
			}
		]
	}`
	reqFEDoris, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect=doris&_format=application/sql", strings.NewReader(forEachVD))
	require.NoError(t, err)
	reqFEDoris.Header.Set("Content-Type", "application/fhir+json")
	respFEDoris, err := client.Do(reqFEDoris)
	require.NoError(t, err)
	feDorisBytes, _ := io.ReadAll(respFEDoris.Body)
	respFEDoris.Body.Close()
	require.Equal(t, http.StatusOK, respFEDoris.StatusCode)
	assert.Contains(t, string(feDorisBytes), "LATERAL VIEW EXPLODE(r.code_tokens) fe_tbl AS fe_tok")
	assert.Contains(t, string(feDorisBytes), "SPLIT_PART(fe_tok, '|', 2) AS id_value")

	reqFEPg, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect=postgres&_format=application/sql", strings.NewReader(forEachVD))
	require.NoError(t, err)
	reqFEPg.Header.Set("Content-Type", "application/fhir+json")
	respFEPg, err := client.Do(reqFEPg)
	require.NoError(t, err)
	fePgBytes, _ := io.ReadAll(respFEPg.Body)
	respFEPg.Body.Close()
	require.Equal(t, http.StatusOK, respFEPg.StatusCode)
	assert.Contains(t, string(fePgBytes), "INNER JOIN spidx_token fe ON fe.tenant_id = r.tenant_id")
	assert.Contains(t, string(fePgBytes), "fe.sp_system AS id_system, fe.sp_value AS id_value")

	forEachOrNullVD := strings.ReplaceAll(forEachVD, `"forEach": "identifier"`, `"forEachOrNull": "name"`)
	forEachOrNullVD = strings.ReplaceAll(forEachOrNullVD, `"path": "value"`, `"path": "family"`)
	reqFEOuterDoris, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect=doris&_format=application/sql", strings.NewReader(forEachOrNullVD))
	require.NoError(t, err)
	reqFEOuterDoris.Header.Set("Content-Type", "application/fhir+json")
	respFEOuterDoris, err := client.Do(reqFEOuterDoris)
	require.NoError(t, err)
	feOuterDorisBytes, _ := io.ReadAll(respFEOuterDoris.Body)
	respFEOuterDoris.Body.Close()
	require.Equal(t, http.StatusOK, respFEOuterDoris.StatusCode)
	assert.Contains(t, string(feOuterDorisBytes), "LATERAL VIEW OUTER EXPLODE(r.string_tokens) fe_tbl AS fe_tok")

	reqFEOuterPg, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect=postgres&_format=application/sql", strings.NewReader(forEachOrNullVD))
	require.NoError(t, err)
	reqFEOuterPg.Header.Set("Content-Type", "application/fhir+json")
	respFEOuterPg, err := client.Do(reqFEOuterPg)
	require.NoError(t, err)
	feOuterPgBytes, _ := io.ReadAll(respFEOuterPg.Body)
	respFEOuterPg.Body.Close()
	require.Equal(t, http.StatusOK, respFEOuterPg.StatusCode)
	assert.Contains(t, string(feOuterPgBytes), "LEFT JOIN spidx_string fe ON fe.tenant_id = r.tenant_id")

	// 17. Binary Proto3 Bulk Data $export (_outputFormat=application/x-protobuf) & $import (Content-Type: application/x-protobuf) Round-Trip
	reqProtoExpKick, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/$export?_outputFormat=application/x-protobuf&_type=Patient", nil)
	require.NoError(t, err)
	reqProtoExpKick.Header.Set("Prefer", "respond-async")
	respProtoExpKick, err := client.Do(reqProtoExpKick)
	require.NoError(t, err)
	respProtoExpKick.Body.Close()
	require.Equal(t, http.StatusAccepted, respProtoExpKick.StatusCode)
	pollURL := respProtoExpKick.Header.Get("Content-Location")
	require.NotEmpty(t, pollURL)

	var exportManifest struct {
		Output []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"output"`
	}
	for i := 0; i < 40; i++ {
		respPoll, pErr := client.Get(pollURL)
		require.NoError(t, pErr)
		if respPoll.StatusCode == http.StatusOK {
			_ = json.NewDecoder(respPoll.Body).Decode(&exportManifest)
			respPoll.Body.Close()
			break
		}
		respPoll.Body.Close()
		time.Sleep(15 * time.Millisecond)
	}
	require.NotEmpty(t, exportManifest.Output)
	assert.True(t, strings.HasSuffix(exportManifest.Output[0].URL, ".pb"))

	respProtoExpFile, err := client.Get(exportManifest.Output[0].URL)
	require.NoError(t, err)
	expProtoBytes, _ := io.ReadAll(respProtoExpFile.Body)
	respProtoExpFile.Body.Close()
	require.Equal(t, http.StatusOK, respProtoExpFile.StatusCode)
	assert.Equal(t, "application/x-protobuf", respProtoExpFile.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respProtoExpFile.Header.Get("X-Flint-Wire-Format"))

	var expBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(expProtoBytes, &expBundle))
	assert.GreaterOrEqual(t, len(expBundle.GetEntry()), 2)

	// Re-import the exported binary Proto3 COLLECTION Bundle into a fresh tenant "proto-imp"
	reqProtoImp, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto-imp/$import", bytes.NewReader(expProtoBytes))
	require.NoError(t, err)
	reqProtoImp.Header.Set("Content-Type", "application/x-protobuf")
	respProtoImp, err := client.Do(reqProtoImp)
	require.NoError(t, err)
	impRespBytes, _ := io.ReadAll(respProtoImp.Body)
	respProtoImp.Body.Close()
	require.Equal(t, http.StatusOK, respProtoImp.StatusCode)
	assert.Contains(t, string(impRespBytes), "Successfully imported")

	// Verify imported Patient exists in "proto-imp" via Proto3 GET
	reqImpCheck, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto-imp/Patient/tx-proto-pat-1", nil)
	require.NoError(t, err)
	reqImpCheck.Header.Set("Accept", "application/x-protobuf")
	respImpCheck, err := client.Do(reqImpCheck)
	require.NoError(t, err)
	impCheckBytes, _ := io.ReadAll(respImpCheck.Body)
	respImpCheck.Body.Close()
	require.Equal(t, http.StatusOK, respImpCheck.StatusCode)
	var impPatient patpb.Patient
	require.NoError(t, proto.Unmarshal(impCheckBytes, &impPatient))
	assert.Equal(t, "tx-proto-pat-1", impPatient.GetId().GetValue())

	// 18. ViewDefinition/$sql with collection: true array aggregation pushdown across Doris, PostgreSQL, and SQLite
	collectionVD := `{
		"resourceType": "ViewDefinition",
		"name": "patient_collections",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "given_names", "path": "name.given", "collection": true},
					{"name": "identifiers", "path": "identifier.value", "collection": true}
				]
			}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains string
	}{
		{dialect: "doris", wantContains: "ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)"},
		{dialect: "postgres", wantContains: "COALESCE(ARRAY_AGG(s.sp_value), ARRAY[]::text[])"},
		{dialect: "sqlite", wantContains: "json_group_array(s.sp_value)"},
	} {
		reqColl, cErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(collectionVD))
		require.NoError(t, cErr)
		reqColl.Header.Set("Content-Type", "application/fhir+json")
		respColl, cErr := client.Do(reqColl)
		require.NoError(t, cErr)
		collBytes, _ := io.ReadAll(respColl.Body)
		respColl.Body.Close()
		require.Equal(t, http.StatusOK, respColl.StatusCode)
		assert.Contains(t, string(collBytes), tc.wantContains)
	}

	// 19. Binary Proto3 Patient/{id}/$everything GET & POST and SQL-on-FHIR v2 .join() / .endsWith() / .contains() $sql pushdown
	reqEvGet, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient/tx-proto-pat-1/$everything?_count=10&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqEvGet.Header.Set("Accept", "application/x-protobuf")
	respEvGet, err := client.Do(reqEvGet)
	require.NoError(t, err)
	evGetBytes, _ := io.ReadAll(respEvGet.Body)
	respEvGet.Body.Close()
	require.Equal(t, http.StatusOK, respEvGet.StatusCode)
	assert.Equal(t, "application/x-protobuf", respEvGet.Header.Get("Content-Type"))
	var evGetBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(evGetBytes, &evGetBundle))
	assert.GreaterOrEqual(t, len(evGetBundle.GetEntry()), 1)

	evParamsJSON := `{"resourceType":"Parameters","parameter":[{"name":"_count","valueInteger":5},{"name":"_type","valueString":"Patient,Observation"}]}`
	reqEvParamsConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(evParamsJSON))
	require.NoError(t, err)
	reqEvParamsConv.Header.Set("Content-Type", "application/fhir+json")
	reqEvParamsConv.Header.Set("Accept", "application/x-protobuf")
	respEvParamsConv, err := client.Do(reqEvParamsConv)
	require.NoError(t, err)
	evParamsProtoBytes, _ := io.ReadAll(respEvParamsConv.Body)
	respEvParamsConv.Body.Close()
	require.Equal(t, http.StatusOK, respEvParamsConv.StatusCode)

	reqEvPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/tx-proto-pat-1/$everything", bytes.NewReader(evParamsProtoBytes))
	require.NoError(t, err)
	reqEvPost.Header.Set("Content-Type", "application/x-protobuf")
	reqEvPost.Header.Set("Accept", "application/x-protobuf")
	respEvPost, err := client.Do(reqEvPost)
	require.NoError(t, err)
	evPostBytes, _ := io.ReadAll(respEvPost.Body)
	respEvPost.Body.Close()
	require.Equal(t, http.StatusOK, respEvPost.StatusCode)
	assert.Equal(t, "application/x-protobuf", respEvPost.Header.Get("Content-Type"))
	var evPostBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(evPostBytes, &evPostBundle))
	assert.GreaterOrEqual(t, len(evPostBundle.GetEntry()), 1)

	joinAndStringVD := `{
		"resourceType": "ViewDefinition",
		"name": "patient_join_and_strings",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "full_given", "path": "name.given.join(' ')"},
					{"name": "id_joined", "path": "identifier.value.join(',')"}
				]
			}
		],
		"where": [
			{"path": "name.family.endsWith('Hopper') and name.given.contains('rac')"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"ARRAY_JOIN(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)), ' ')",
				"ARRAY_JOIN(ARRAY_MAP(x -> SPLIT_PART(x, '|', 2), ARRAY_FILTER(x -> x LIKE 'identifier=|%', r.code_tokens)), ',')",
				"LIKE 'family=%Hopper'",
				"LIKE 'given=%rac%'",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"COALESCE(ARRAY_TO_STRING(ARRAY_AGG(s.sp_value), ' '), '')",
				"COALESCE(ARRAY_TO_STRING(ARRAY_AGG(t.sp_value), ','), '')",
				"s.sp_value LIKE '%Hopper'",
				"s.sp_value LIKE '%rac%'",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"COALESCE(group_concat(s.sp_value, ' '), '')",
				"COALESCE(group_concat(t.sp_value, ','), '')",
				"s.sp_value LIKE '%Hopper'",
				"s.sp_value LIKE '%rac%'",
			},
		},
	} {
		reqJoin, jErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(joinAndStringVD))
		require.NoError(t, jErr)
		reqJoin.Header.Set("Content-Type", "application/fhir+json")
		respJoin, jErr := client.Do(reqJoin)
		require.NoError(t, jErr)
		joinBytes, _ := io.ReadAll(respJoin.Body)
		respJoin.Body.Close()
		require.Equal(t, http.StatusOK, respJoin.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(joinBytes), want)
		}
	}

	// 20. Binary Proto3 $validate (POST resource, POST Parameters, GET instance) & SQL-on-FHIR v2 .count(), .lower(), .upper(), .length() $sql pushdown
	valPatProtoBytes, err := proto.Marshal(&patpb.Patient{
		Id:     &dtpb.Id{Value: "val-proto-pat-1"},
		Active: &dtpb.Boolean{Value: true},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "Hopper"},
				Given:  []*dtpb.String{{Value: "Grace"}},
			},
		},
	})
	require.NoError(t, err)

	reqValPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/$validate", bytes.NewReader(valPatProtoBytes))
	require.NoError(t, err)
	reqValPost.Header.Set("Content-Type", "application/x-protobuf")
	reqValPost.Header.Set("Accept", "application/x-protobuf")
	respValPost, err := client.Do(reqValPost)
	require.NoError(t, err)
	valPostBytes, _ := io.ReadAll(respValPost.Body)
	respValPost.Body.Close()
	require.Equal(t, http.StatusOK, respValPost.StatusCode)
	assert.Equal(t, "application/x-protobuf", respValPost.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respValPost.Header.Get("X-Flint-Wire-Format"))
	var ooPost oopb.OperationOutcome
	require.NoError(t, proto.Unmarshal(valPostBytes, &ooPost))
	require.GreaterOrEqual(t, len(ooPost.GetIssue()), 1)
	assert.Contains(t, ooPost.GetIssue()[0].GetDiagnostics().GetValue(), "All validation checks passed with zero issues")

	reqValGet, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient/tx-proto-pat-1/$validate?_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqValGet.Header.Set("Accept", "application/x-protobuf")
	respValGet, err := client.Do(reqValGet)
	require.NoError(t, err)
	valGetBytes, _ := io.ReadAll(respValGet.Body)
	respValGet.Body.Close()
	require.Equal(t, http.StatusOK, respValGet.StatusCode)
	assert.Equal(t, "application/x-protobuf", respValGet.Header.Get("Content-Type"))
	var ooGet oopb.OperationOutcome
	require.NoError(t, proto.Unmarshal(valGetBytes, &ooGet))
	require.GreaterOrEqual(t, len(ooGet.GetIssue()), 1)
	assert.Contains(t, ooGet.GetIssue()[0].GetDiagnostics().GetValue(), "All validation checks passed with zero issues")

	countCaseLenVD := `{
		"resourceType": "ViewDefinition",
		"name": "patient_count_case_length",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "given_count", "path": "name.given.count()"},
					{"name": "family_lower", "path": "name.family.lower()"},
					{"name": "family_upper", "path": "name.family.upper()"},
					{"name": "family_len", "path": "name.family.length()"}
				]
			}
		],
		"where": [
			{"path": "name.given.count() >= 1 and name.family.lower() = 'hopper' and name.family.length() >= 4"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"ARRAY_SIZE(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)))",
				"LOWER(element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE 'family=%', r.string_tokens)), 1))",
				"UPPER(element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE 'family=%', r.string_tokens)), 1))",
				"CHAR_LENGTH(element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE 'family=%', r.string_tokens)), 1))",
				">= 1",
				"= 'hopper'",
				">= 4",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"(SELECT COUNT(*) FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'given')",
				"LOWER((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1))",
				"UPPER((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1))",
				"LENGTH((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1))",
				">= 1",
				"= 'hopper'",
				">= 4",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"(SELECT COUNT(*) FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'given')",
				"LOWER((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1))",
				"UPPER((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1))",
				"LENGTH((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1))",
			},
		},
	} {
		reqCnt, cErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(countCaseLenVD))
		require.NoError(t, cErr)
		reqCnt.Header.Set("Content-Type", "application/fhir+json")
		respCnt, cErr := client.Do(reqCnt)
		require.NoError(t, cErr)
		cntBytes, _ := io.ReadAll(respCnt.Body)
		respCnt.Body.Close()
		require.Equal(t, http.StatusOK, respCnt.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(cntBytes), want)
		}
	}

	// 21. Binary Proto3 Terminology Operations ($expand, $validate-code, $lookup, $translate) & SQL-on-FHIR v2 .last(), .skip(N), .take(N) & coalesce() $sql pushdown
	reqExpProto, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/ValueSet/$expand?url=http://hl7.org/fhir/ValueSet/administrative-gender&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqExpProto.Header.Set("Accept", "application/x-protobuf")
	respExpProto, err := client.Do(reqExpProto)
	require.NoError(t, err)
	vsExpProtoBytes, _ := io.ReadAll(respExpProto.Body)
	respExpProto.Body.Close()
	require.Equal(t, http.StatusOK, respExpProto.StatusCode)
	assert.Equal(t, "application/x-protobuf", respExpProto.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respExpProto.Header.Get("X-Flint-Wire-Format"))
	var vsExpanded vspb.ValueSet
	require.NoError(t, proto.Unmarshal(vsExpProtoBytes, &vsExpanded))
	assert.GreaterOrEqual(t, len(vsExpanded.GetExpansion().GetContains()), 4)

	vcParamsJSON := `{"resourceType":"Parameters","parameter":[{"name":"url","valueUri":"http://hl7.org/fhir/ValueSet/administrative-gender"},{"name":"code","valueCode":"female"}]}`
	reqVCConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(vcParamsJSON))
	require.NoError(t, err)
	reqVCConv.Header.Set("Content-Type", "application/fhir+json")
	reqVCConv.Header.Set("Accept", "application/x-protobuf")
	respVCConv, err := client.Do(reqVCConv)
	require.NoError(t, err)
	vcParamsProtoBytes, _ := io.ReadAll(respVCConv.Body)
	respVCConv.Body.Close()
	require.Equal(t, http.StatusOK, respVCConv.StatusCode)

	reqVCProto, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ValueSet/$validate-code", bytes.NewReader(vcParamsProtoBytes))
	require.NoError(t, err)
	reqVCProto.Header.Set("Content-Type", "application/x-protobuf")
	reqVCProto.Header.Set("Accept", "application/x-protobuf")
	respVCProto, err := client.Do(reqVCProto)
	require.NoError(t, err)
	vcOutProtoBytes, _ := io.ReadAll(respVCProto.Body)
	respVCProto.Body.Close()
	require.Equal(t, http.StatusOK, respVCProto.StatusCode)
	assert.Equal(t, "application/x-protobuf", respVCProto.Header.Get("Content-Type"))
	var vcOutParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(vcOutProtoBytes, &vcOutParams))
	assert.GreaterOrEqual(t, len(vcOutParams.GetParameter()), 2)

	sliceCoalVD := `{
		"resourceType": "ViewDefinition",
		"name": "patient_slice_coalesce",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "last_given", "path": "name.given.last()"},
					{"name": "second_given", "path": "name.given.skip(1).first()"},
					{"name": "first_two_given", "path": "name.given.take(2).join(' ')"},
					{"name": "family_or_unk", "path": "coalesce(name.family, 'Unknown')"}
				]
			}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"element_at(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)), -1)",
				"element_at(ARRAY_SLICE(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)), 2), 1)",
				"ARRAY_JOIN(ARRAY_SLICE(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)), 1, 2), ' ')",
				"COALESCE(element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE 'family=%', r.string_tokens)), 1), 'Unknown')",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"ORDER BY s.sp_value DESC LIMIT 1",
				"LIMIT 1 OFFSET 1",
				"COALESCE(ARRAY_TO_STRING(ARRAY_AGG(sub.sp_value), ' '), '')",
				"LIMIT 2) sub",
				"COALESCE((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1), 'Unknown')",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"ORDER BY s.sp_value DESC LIMIT 1",
				"LIMIT 1 OFFSET 1",
				"COALESCE(group_concat(sub.sp_value, ' '), '')",
				"LIMIT 2) sub",
				"COALESCE((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1), 'Unknown')",
			},
		},
	} {
		reqSl, sErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(sliceCoalVD))
		require.NoError(t, sErr)
		reqSl.Header.Set("Content-Type", "application/fhir+json")
		respSl, sErr := client.Do(reqSl)
		require.NoError(t, sErr)
		slBytes, _ := io.ReadAll(respSl.Body)
		respSl.Body.Close()
		require.Equal(t, http.StatusOK, respSl.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(slBytes), want)
		}
	}

	// 22. Binary Proto3 $match, $meta / $meta-add & $graph Operations & SQL-on-FHIR v2 iif(), .replace(), .substring() & .trim() $sql pushdown
	reqMatchProto, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/$match", bytes.NewReader(patchedWire))
	require.NoError(t, err)
	reqMatchProto.Header.Set("Content-Type", "application/x-protobuf")
	reqMatchProto.Header.Set("Accept", "application/x-protobuf")
	respMatchProto, err := client.Do(reqMatchProto)
	require.NoError(t, err)
	matchBundleProtoBytes, _ := io.ReadAll(respMatchProto.Body)
	respMatchProto.Body.Close()
	require.Equal(t, http.StatusOK, respMatchProto.StatusCode)
	assert.Equal(t, "application/x-protobuf", respMatchProto.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respMatchProto.Header.Get("X-Flint-Wire-Format"))
	var matchBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(matchBundleProtoBytes, &matchBundle))
	assert.GreaterOrEqual(t, len(matchBundle.GetEntry()), 1)

	metaAddJSON := `{"resourceType":"Parameters","parameter":[{"name":"meta","valueMeta":{"tag":[{"system":"http://flint.fhir.org/tag","code":"proto-vip"}]}}]}`
	reqMetaAddConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(metaAddJSON))
	require.NoError(t, err)
	reqMetaAddConv.Header.Set("Content-Type", "application/fhir+json")
	reqMetaAddConv.Header.Set("Accept", "application/x-protobuf")
	respMetaAddConv, err := client.Do(reqMetaAddConv)
	require.NoError(t, err)
	metaAddProtoBytes, _ := io.ReadAll(respMetaAddConv.Body)
	respMetaAddConv.Body.Close()
	require.Equal(t, http.StatusOK, respMetaAddConv.StatusCode)

	reqMetaAddProto, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/"+patID+"/$meta-add", bytes.NewReader(metaAddProtoBytes))
	require.NoError(t, err)
	reqMetaAddProto.Header.Set("Content-Type", "application/x-protobuf")
	reqMetaAddProto.Header.Set("Accept", "application/x-protobuf")
	respMetaAddProto, err := client.Do(reqMetaAddProto)
	require.NoError(t, err)
	metaAddOutProtoBytes, _ := io.ReadAll(respMetaAddProto.Body)
	respMetaAddProto.Body.Close()
	require.Equal(t, http.StatusOK, respMetaAddProto.StatusCode)
	assert.Equal(t, "application/x-protobuf", respMetaAddProto.Header.Get("Content-Type"))
	var metaAddOutParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(metaAddOutProtoBytes, &metaAddOutParams))
	assert.Len(t, metaAddOutParams.GetParameter(), 1)

	reqMetaGetProto, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient/"+patID+"/$meta?_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqMetaGetProto.Header.Set("Accept", "application/x-protobuf")
	respMetaGetProto, err := client.Do(reqMetaGetProto)
	require.NoError(t, err)
	metaGetOutProtoBytes, _ := io.ReadAll(respMetaGetProto.Body)
	respMetaGetProto.Body.Close()
	require.Equal(t, http.StatusOK, respMetaGetProto.StatusCode)
	assert.Equal(t, "application/x-protobuf", respMetaGetProto.Header.Get("Content-Type"))
	var metaGetOutParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(metaGetOutProtoBytes, &metaGetOutParams))
	assert.Len(t, metaGetOutParams.GetParameter(), 1)

	reqGraphProto, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient/"+patID+"/$graph?graph=us-core-patient-careteam&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqGraphProto.Header.Set("Accept", "application/x-protobuf")
	respGraphProto, err := client.Do(reqGraphProto)
	require.NoError(t, err)
	graphBundleProtoBytes, _ := io.ReadAll(respGraphProto.Body)
	respGraphProto.Body.Close()
	require.Equal(t, http.StatusOK, respGraphProto.StatusCode)
	assert.Equal(t, "application/x-protobuf", respGraphProto.Header.Get("Content-Type"))
	var graphBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(graphBundleProtoBytes, &graphBundle))
	assert.GreaterOrEqual(t, len(graphBundle.GetEntry()), 1)

	iifStrVD := `{
		"resourceType": "ViewDefinition",
		"name": "patient_iif_str",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "status_label", "path": "iif(active = 'true', 'Active', 'Inactive')"},
					{"name": "family_mod", "path": "name.family.replace('Proto', 'Native')"},
					{"name": "family_sub", "path": "name.family.substring(0, 5)"},
					{"name": "family_trim", "path": "name.family.trim()"}
				]
			}
		],
		"where": [
			{"path": "name.family.trim() = 'ProtoUpdated'"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"CASE WHEN array_contains(r.code_tokens, 'active=|true') THEN 'Active' ELSE 'Inactive' END",
				"REPLACE(element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE 'family=%', r.string_tokens)), 1), 'Proto', 'Native')",
				"SUBSTR(element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE 'family=%', r.string_tokens)), 1), 1, 5)",
				"TRIM(element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE 'family=%', r.string_tokens)), 1))",
				"TRIM(element_at(array_map(x -> split_part(x, '=', 2), array_filter(x -> x LIKE 'family=%', r.string_tokens)), 1)) = 'ProtoUpdated'",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"CASE WHEN EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'active' AND t.sp_value = 'true') THEN 'Active' ELSE 'Inactive' END",
				"REPLACE((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1), 'Proto', 'Native')",
				"SUBSTR((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1), 1, 5)",
				"TRIM((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1))",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"CASE WHEN EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'active' AND t.sp_value = 'true') THEN 'Active' ELSE 'Inactive' END",
				"REPLACE((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1), 'Proto', 'Native')",
				"SUBSTR((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1), 1, 5)",
				"TRIM((SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' LIMIT 1))",
			},
		},
	} {
		reqIif, iErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(iifStrVD))
		require.NoError(t, iErr)
		reqIif.Header.Set("Content-Type", "application/fhir+json")
		respIif, iErr := client.Do(reqIif)
		require.NoError(t, iErr)
		iifBytes, _ := io.ReadAll(respIif.Body)
		respIif.Body.Close()
		require.Equal(t, http.StatusOK, respIif.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(iifBytes), want)
		}
	}

	// 23. Binary Proto3 /metadata, $summary, $document, $docref, $lastn & $stats Wire Transport & SQL-on-FHIR v2 Type-Cast (.toInteger(), .toDecimal(), .toString()) & .indexOf() $sql Pushdown
	reqMetaCap, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/metadata?_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqMetaCap.Header.Set("Accept", "application/x-protobuf")
	respMetaCap, err := client.Do(reqMetaCap)
	require.NoError(t, err)
	metaCapBytes, _ := io.ReadAll(respMetaCap.Body)
	respMetaCap.Body.Close()
	require.Equal(t, http.StatusOK, respMetaCap.StatusCode)
	assert.Equal(t, "application/x-protobuf", respMetaCap.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respMetaCap.Header.Get("X-Flint-Wire-Format"))
	var capStmt cspb.CapabilityStatement
	require.NoError(t, proto.Unmarshal(metaCapBytes, &capStmt))
	assert.NotEmpty(t, capStmt.GetRest())

	reqCapSub, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/CapabilityStatement/$subset?resource=Patient&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqCapSub.Header.Set("Accept", "application/x-protobuf")
	respCapSub, err := client.Do(reqCapSub)
	require.NoError(t, err)
	capSubBytes, _ := io.ReadAll(respCapSub.Body)
	respCapSub.Body.Close()
	require.Equal(t, http.StatusOK, respCapSub.StatusCode)
	var capSub cspb.CapabilityStatement
	require.NoError(t, proto.Unmarshal(capSubBytes, &capSub))
	assert.Len(t, capSub.GetRest()[0].GetResource(), 1)

	reqIPSProto, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient/"+patID+"/$summary?_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqIPSProto.Header.Set("Accept", "application/x-protobuf")
	respIPSProto, err := client.Do(reqIPSProto)
	require.NoError(t, err)
	ipsBytes, _ := io.ReadAll(respIPSProto.Body)
	respIPSProto.Body.Close()
	require.Equal(t, http.StatusOK, respIPSProto.StatusCode)
	assert.Equal(t, "application/x-protobuf", respIPSProto.Header.Get("Content-Type"))
	var ipsBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(ipsBytes, &ipsBundle))
	assert.GreaterOrEqual(t, len(ipsBundle.GetEntry()), 2)

	ipsPostParamsJSON := `{"resourceType":"Parameters","parameter":[{"name":"patient","valueString":"` + patID + `"}]}`
	reqIPSConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(ipsPostParamsJSON))
	require.NoError(t, err)
	reqIPSConv.Header.Set("Content-Type", "application/fhir+json")
	reqIPSConv.Header.Set("Accept", "application/x-protobuf")
	respIPSConv, err := client.Do(reqIPSConv)
	require.NoError(t, err)
	ipsParamsProto, _ := io.ReadAll(respIPSConv.Body)
	respIPSConv.Body.Close()
	require.Equal(t, http.StatusOK, respIPSConv.StatusCode)

	reqIPSTypePost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/$summary", bytes.NewReader(ipsParamsProto))
	require.NoError(t, err)
	reqIPSTypePost.Header.Set("Content-Type", "application/x-protobuf")
	reqIPSTypePost.Header.Set("Accept", "application/x-protobuf")
	respIPSTypePost, err := client.Do(reqIPSTypePost)
	require.NoError(t, err)
	ipsTypeBytes, _ := io.ReadAll(respIPSTypePost.Body)
	respIPSTypePost.Body.Close()
	require.Equal(t, http.StatusOK, respIPSTypePost.StatusCode)
	var ipsTypeBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(ipsTypeBytes, &ipsTypeBundle))
	assert.GreaterOrEqual(t, len(ipsTypeBundle.GetEntry()), 2)

	reqDocrefProto, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/DocumentReference/$docref?patient="+patID+"&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqDocrefProto.Header.Set("Accept", "application/x-protobuf")
	respDocrefProto, err := client.Do(reqDocrefProto)
	require.NoError(t, err)
	docrefBytes, _ := io.ReadAll(respDocrefProto.Body)
	respDocrefProto.Body.Close()
	require.Equal(t, http.StatusOK, respDocrefProto.StatusCode)
	var docrefBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(docrefBytes, &docrefBundle))
	assert.GreaterOrEqual(t, len(docrefBundle.GetEntry()), 1)

	obsSeedJSON := `{"resourceType":"Observation","id":"obs-proto-w78","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/` + patID + `"},"effectiveDateTime":"2026-01-15T10:00:00Z","valueQuantity":{"value":76.0,"unit":"/min","system":"http://unitsofmeasure.org","code":"/min"}}`
	reqObsPut, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/proto/Observation/obs-proto-w78", strings.NewReader(obsSeedJSON))
	require.NoError(t, err)
	reqObsPut.Header.Set("Content-Type", "application/fhir+json")
	respObsPut, err := client.Do(reqObsPut)
	require.NoError(t, err)
	respObsPut.Body.Close()
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, respObsPut.StatusCode)

	reqLastnProto, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Observation/$lastn?patient=Patient/"+patID+"&max=1&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqLastnProto.Header.Set("Accept", "application/x-protobuf")
	respLastnProto, err := client.Do(reqLastnProto)
	require.NoError(t, err)
	lastnBytes, _ := io.ReadAll(respLastnProto.Body)
	respLastnProto.Body.Close()
	require.Equal(t, http.StatusOK, respLastnProto.StatusCode)
	var lastnBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(lastnBytes, &lastnBundle))
	assert.GreaterOrEqual(t, len(lastnBundle.GetEntry()), 1)

	reqStatsProto, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Observation/$stats?subject=Patient/"+patID+"&code=8867-4&statistic=average&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqStatsProto.Header.Set("Accept", "application/x-protobuf")
	respStatsProto, err := client.Do(reqStatsProto)
	require.NoError(t, err)
	statsBytes, _ := io.ReadAll(respStatsProto.Body)
	respStatsProto.Body.Close()
	require.Equal(t, http.StatusOK, respStatsProto.StatusCode)
	var statsParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(statsBytes, &statsParams))
	assert.NotEmpty(t, statsParams.GetParameter())

	castIdxVD := `{
		"resourceType": "ViewDefinition",
		"name": "obs_cast_indexof",
		"status": "active",
		"resource": "Observation",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "val_int", "path": "valueQuantity.value.toInteger()"},
					{"name": "val_dec", "path": "valueQuantity.value.toDecimal()"},
					{"name": "val_str", "path": "valueQuantity.value.toString()"},
					{"name": "code_idx", "path": "code.coding.code.indexOf('88')"}
				]
			}
		],
		"where": [
			{"path": "valueQuantity.value.toInteger() >= 60 and code.coding.code.indexOf('88') >= 0"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"CAST(r.value_quantity AS BIGINT)",
				"CAST(r.value_quantity AS DOUBLE)",
				"CAST(r.value_quantity AS STRING)",
				"(LOCATE('88', r.code) - 1)",
				"CAST(r.value_quantity AS BIGINT) >= 60",
				"(LOCATE('88', r.code) - 1) >= 0",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"CAST((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1) AS BIGINT)",
				"CAST((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1) AS DOUBLE PRECISION)",
				"CAST((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1) AS TEXT)",
				"(STRPOS((SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'code' LIMIT 1), '88') - 1)",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"CAST((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1) AS INTEGER)",
				"CAST((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1) AS REAL)",
				"CAST((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1) AS TEXT)",
				"(INSTR((SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'code' LIMIT 1), '88') - 1)",
			},
		},
	} {
		reqCast, cErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(castIdxVD))
		require.NoError(t, cErr)
		reqCast.Header.Set("Content-Type", "application/fhir+json")
		respCast, cErr := client.Do(reqCast)
		require.NoError(t, cErr)
		castBytes, _ := io.ReadAll(respCast.Body)
		respCast.Body.Close()
		require.Equal(t, http.StatusOK, respCast.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(castBytes), want)
		}
	}

	// 24. Binary Proto3 eCQM ($evaluate-measure, $care-gaps, $collect-data, $submit-data) & SDC ($populate, $package, $extract) Wire Transport + SQL-on-FHIR v2 Math & Regex $sql Pushdown
	evalParamsJSON := `{"resourceType":"Parameters","parameter":[{"name":"measure","valueString":"CMS165"},{"name":"subject","valueString":"Patient/` + patID + `"}]}`
	reqEvalConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(evalParamsJSON))
	require.NoError(t, err)
	reqEvalConv.Header.Set("Content-Type", "application/fhir+json")
	reqEvalConv.Header.Set("Accept", "application/x-protobuf")
	respEvalConv, err := client.Do(reqEvalConv)
	require.NoError(t, err)
	evalParamsProto, _ := io.ReadAll(respEvalConv.Body)
	respEvalConv.Body.Close()
	require.Equal(t, http.StatusOK, respEvalConv.StatusCode)

	reqEvalPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Measure/$evaluate-measure", bytes.NewReader(evalParamsProto))
	require.NoError(t, err)
	reqEvalPost.Header.Set("Content-Type", "application/x-protobuf")
	reqEvalPost.Header.Set("Accept", "application/x-protobuf")
	respEvalPost, err := client.Do(reqEvalPost)
	require.NoError(t, err)
	evalPostBytes, _ := io.ReadAll(respEvalPost.Body)
	respEvalPost.Body.Close()
	require.Equal(t, http.StatusOK, respEvalPost.StatusCode)
	assert.Equal(t, "application/x-protobuf", respEvalPost.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respEvalPost.Header.Get("X-Flint-Wire-Format"))
	assert.NotEmpty(t, evalPostBytes)

	reqCGPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Measure/$care-gaps", bytes.NewReader(evalParamsProto))
	require.NoError(t, err)
	reqCGPost.Header.Set("Content-Type", "application/x-protobuf")
	reqCGPost.Header.Set("Accept", "application/x-protobuf")
	respCGPost, err := client.Do(reqCGPost)
	require.NoError(t, err)
	cgPostBytes, _ := io.ReadAll(respCGPost.Body)
	respCGPost.Body.Close()
	require.Equal(t, http.StatusOK, respCGPost.StatusCode)
	var cgBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(cgPostBytes, &cgBundle))
	assert.NotEmpty(t, cgBundle.GetEntry())

	reqCollPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Measure/$collect-data", bytes.NewReader(evalParamsProto))
	require.NoError(t, err)
	reqCollPost.Header.Set("Content-Type", "application/x-protobuf")
	reqCollPost.Header.Set("Accept", "application/x-protobuf")
	respCollPost, err := client.Do(reqCollPost)
	require.NoError(t, err)
	collPostBytes, _ := io.ReadAll(respCollPost.Body)
	respCollPost.Body.Close()
	require.Equal(t, http.StatusOK, respCollPost.StatusCode)
	var collParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(collPostBytes, &collParams))
	assert.NotEmpty(t, collParams.GetParameter())

	reqSubPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Measure/$submit-data", bytes.NewReader(collPostBytes))
	require.NoError(t, err)
	reqSubPost.Header.Set("Content-Type", "application/x-protobuf")
	reqSubPost.Header.Set("Accept", "application/x-protobuf")
	respSubPost, err := client.Do(reqSubPost)
	require.NoError(t, err)
	subPostBytes, _ := io.ReadAll(respSubPost.Body)
	respSubPost.Body.Close()
	require.Equal(t, http.StatusOK, respSubPost.StatusCode)
	var subBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(subPostBytes, &subBundle))
	assert.NotEmpty(t, subBundle.GetEntry())

	reqPopGet, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Questionnaire/phq-9/$populate?subject=Patient/"+patID+"&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqPopGet.Header.Set("Accept", "application/x-protobuf")
	respPopGet, err := client.Do(reqPopGet)
	require.NoError(t, err)
	popGetBytes, _ := io.ReadAll(respPopGet.Body)
	respPopGet.Body.Close()
	require.Equal(t, http.StatusOK, respPopGet.StatusCode)
	assert.Equal(t, "application/x-protobuf", respPopGet.Header.Get("Content-Type"))
	assert.NotEmpty(t, popGetBytes)

	qPkgJSON := `{"resourceType":"Questionnaire","id":"q-pkg-w79","url":"http://flint.fhir.org/Questionnaire/q-pkg-w79","status":"active"}`
	reqQPkgConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Questionnaire/$convert", strings.NewReader(qPkgJSON))
	require.NoError(t, err)
	reqQPkgConv.Header.Set("Content-Type", "application/fhir+json")
	reqQPkgConv.Header.Set("Accept", "application/x-protobuf")
	respQPkgConv, err := client.Do(reqQPkgConv)
	require.NoError(t, err)
	qPkgInProto, _ := io.ReadAll(respQPkgConv.Body)
	respQPkgConv.Body.Close()
	require.Equal(t, http.StatusOK, respQPkgConv.StatusCode)

	reqQPkgPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Questionnaire/$package", bytes.NewReader(qPkgInProto))
	require.NoError(t, err)
	reqQPkgPost.Header.Set("Content-Type", "application/x-protobuf")
	reqQPkgPost.Header.Set("Accept", "application/x-protobuf")
	respQPkgPost, err := client.Do(reqQPkgPost)
	require.NoError(t, err)
	qPkgOutBytes, _ := io.ReadAll(respQPkgPost.Body)
	respQPkgPost.Body.Close()
	require.Equal(t, http.StatusOK, respQPkgPost.StatusCode)
	var qPkgBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(qPkgOutBytes, &qPkgBundle))
	assert.NotEmpty(t, qPkgBundle.GetEntry())

	reqExtPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/QuestionnaireResponse/$extract", bytes.NewReader(popGetBytes))
	require.NoError(t, err)
	reqExtPost.Header.Set("Content-Type", "application/x-protobuf")
	reqExtPost.Header.Set("Accept", "application/x-protobuf")
	respExtPost, err := client.Do(reqExtPost)
	require.NoError(t, err)
	extPostBytes, _ := io.ReadAll(respExtPost.Body)
	respExtPost.Body.Close()
	require.Equal(t, http.StatusOK, respExtPost.StatusCode)
	var extBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(extPostBytes, &extBundle))
	assert.NotEmpty(t, extBundle.GetId().GetValue())

	mathRegexVD := `{
		"resourceType": "ViewDefinition",
		"name": "obs_math_regex",
		"status": "active",
		"resource": "Observation",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "val_abs", "path": "valueQuantity.value.abs()"},
					{"name": "val_ceil", "path": "valueQuantity.value.ceiling()"},
					{"name": "val_floor", "path": "valueQuantity.value.floor()"},
					{"name": "val_round0", "path": "valueQuantity.value.round()"},
					{"name": "val_round2", "path": "valueQuantity.value.round(2)"},
					{"name": "val_sqrt", "path": "valueQuantity.value.sqrt()"},
					{"name": "code_clean", "path": "code.coding.code.replaceMatches('[^0-9]', '')"}
				]
			}
		],
		"where": [
			{"path": "valueQuantity.value.abs() >= 60 and code.coding.code.matches('^88.*')"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"ABS(r.value_quantity)",
				"CEIL(r.value_quantity)",
				"FLOOR(r.value_quantity)",
				"ROUND(r.value_quantity)",
				"ROUND(r.value_quantity, 2)",
				"SQRT(r.value_quantity)",
				"REGEXP_REPLACE(r.code, '[^0-9]', '')",
				"ABS(r.value_quantity) >= 60",
				"REGEXP(r.code, '^88.*')",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"ABS((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1))",
				"CEIL((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1))",
				"FLOOR((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1))",
				"ROUND((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1))",
				"ROUND((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1), 2)",
				"SQRT((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1))",
				"REGEXP_REPLACE((SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'code' LIMIT 1), '[^0-9]', '', 'g')",
				"(SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'code' LIMIT 1) ~ '^88.*'",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"ABS((SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' LIMIT 1))",
				"REGEXP_REPLACE((SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'code' LIMIT 1), '[^0-9]', '')",
				"(SELECT t.sp_value FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'code' LIMIT 1) REGEXP '^88.*'",
			},
		},
	} {
		reqMR, mErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(mathRegexVD))
		require.NoError(t, mErr)
		reqMR.Header.Set("Content-Type", "application/fhir+json")
		respMR, mErr := client.Do(reqMR)
		require.NoError(t, mErr)
		mrBytes, _ := io.ReadAll(respMR.Body)
		respMR.Body.Close()
		require.Equal(t, http.StatusOK, respMR.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(mrBytes), want)
		}
	}

	// 25. Binary Proto3 Clinical Reasoning ($apply, $data-requirements, $fhirpath), Cohort/Identity (Group/$add, Group/$remove, Patient/$merge, $reindex), ViewDefinition/$sql Wire Transport + SQL-on-FHIR v2 Arithmetic, Concat (&) & Temporal $sql Pushdown
	applyParamsJSON := `{"resourceType":"Parameters","parameter":[{"name":"planDefinition","valueString":"plandef-hypertension-control"},{"name":"subject","valueString":"Patient/` + patID + `"}]}`
	reqApplyConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(applyParamsJSON))
	require.NoError(t, err)
	reqApplyConv.Header.Set("Content-Type", "application/fhir+json")
	reqApplyConv.Header.Set("Accept", "application/x-protobuf")
	respApplyConv, err := client.Do(reqApplyConv)
	require.NoError(t, err)
	applyParamsProto, _ := io.ReadAll(respApplyConv.Body)
	respApplyConv.Body.Close()
	require.Equal(t, http.StatusOK, respApplyConv.StatusCode)

	reqPlanApplyPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/PlanDefinition/$apply", bytes.NewReader(applyParamsProto))
	require.NoError(t, err)
	reqPlanApplyPost.Header.Set("Content-Type", "application/x-protobuf")
	reqPlanApplyPost.Header.Set("Accept", "application/x-protobuf")
	respPlanApplyPost, err := client.Do(reqPlanApplyPost)
	require.NoError(t, err)
	planApplyBytes, _ := io.ReadAll(respPlanApplyPost.Body)
	respPlanApplyPost.Body.Close()
	require.Equal(t, http.StatusOK, respPlanApplyPost.StatusCode)
	assert.Equal(t, "application/x-protobuf", respPlanApplyPost.Header.Get("Content-Type"))
	assert.Equal(t, "google.fhir.r4.proto3", respPlanApplyPost.Header.Get("X-Flint-Wire-Format"))
	assert.NotEmpty(t, planApplyBytes)

	reqActApplyPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ActivityDefinition/activitydef-bp-home-monitoring/$apply", bytes.NewReader(applyParamsProto))
	require.NoError(t, err)
	reqActApplyPost.Header.Set("Content-Type", "application/x-protobuf")
	reqActApplyPost.Header.Set("Accept", "application/x-protobuf")
	respActApplyPost, err := client.Do(reqActApplyPost)
	require.NoError(t, err)
	actApplyBytes, _ := io.ReadAll(respActApplyPost.Body)
	respActApplyPost.Body.Close()
	require.Equal(t, http.StatusOK, respActApplyPost.StatusCode)
	assert.Equal(t, "application/x-protobuf", respActApplyPost.Header.Get("Content-Type"))
	assert.NotEmpty(t, actApplyBytes)

	drParamsJSON := `{"resourceType":"Parameters","parameter":[{"name":"periodStart","valueDate":"2026-01-01"},{"name":"periodEnd","valueDate":"2026-12-31"}]}`
	reqDRConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(drParamsJSON))
	require.NoError(t, err)
	reqDRConv.Header.Set("Content-Type", "application/fhir+json")
	reqDRConv.Header.Set("Accept", "application/x-protobuf")
	respDRConv, err := client.Do(reqDRConv)
	require.NoError(t, err)
	drParamsProto, _ := io.ReadAll(respDRConv.Body)
	respDRConv.Body.Close()
	require.Equal(t, http.StatusOK, respDRConv.StatusCode)

	reqDRPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Measure/cms165-bp-control/$data-requirements", bytes.NewReader(drParamsProto))
	require.NoError(t, err)
	reqDRPost.Header.Set("Content-Type", "application/x-protobuf")
	reqDRPost.Header.Set("Accept", "application/x-protobuf")
	respDRPost, err := client.Do(reqDRPost)
	require.NoError(t, err)
	drPostBytes, _ := io.ReadAll(respDRPost.Body)
	respDRPost.Body.Close()
	require.Equal(t, http.StatusOK, respDRPost.StatusCode)
	assert.Equal(t, "application/x-protobuf", respDRPost.Header.Get("Content-Type"))
	assert.NotEmpty(t, drPostBytes)

	fpParamsJSON := `{"resourceType":"Parameters","parameter":[{"name":"expression","valueString":"Patient.name.family"}]}`
	reqFPConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(fpParamsJSON))
	require.NoError(t, err)
	reqFPConv.Header.Set("Content-Type", "application/fhir+json")
	reqFPConv.Header.Set("Accept", "application/x-protobuf")
	respFPConv, err := client.Do(reqFPConv)
	require.NoError(t, err)
	fpParamsProto, _ := io.ReadAll(respFPConv.Body)
	respFPConv.Body.Close()
	require.Equal(t, http.StatusOK, respFPConv.StatusCode)

	reqFPPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/"+patID+"/$fhirpath", bytes.NewReader(fpParamsProto))
	require.NoError(t, err)
	reqFPPost.Header.Set("Content-Type", "application/x-protobuf")
	reqFPPost.Header.Set("Accept", "application/x-protobuf")
	respFPPost, err := client.Do(reqFPPost)
	require.NoError(t, err)
	fpPostBytes, _ := io.ReadAll(respFPPost.Body)
	respFPPost.Body.Close()
	require.Equal(t, http.StatusOK, respFPPost.StatusCode)
	var fpOutParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(fpPostBytes, &fpOutParams))
	assert.NotEmpty(t, fpOutParams.GetParameter())

	grpSeedJSON := `{"resourceType":"Group","id":"grp-w80","type":"person","actual":true,"name":"Wave 80 Cohort"}`
	reqGrpPut, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/proto/Group/grp-w80", strings.NewReader(grpSeedJSON))
	require.NoError(t, err)
	reqGrpPut.Header.Set("Content-Type", "application/fhir+json")
	respGrpPut, err := client.Do(reqGrpPut)
	require.NoError(t, err)
	respGrpPut.Body.Close()
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, respGrpPut.StatusCode)

	grpMutJSON := `{"resourceType":"Parameters","parameter":[{"name":"member","valueReference":{"reference":"Patient/` + patID + `"}}]}`
	reqGrpConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(grpMutJSON))
	require.NoError(t, err)
	reqGrpConv.Header.Set("Content-Type", "application/fhir+json")
	reqGrpConv.Header.Set("Accept", "application/x-protobuf")
	respGrpConv, err := client.Do(reqGrpConv)
	require.NoError(t, err)
	grpMutProto, _ := io.ReadAll(respGrpConv.Body)
	respGrpConv.Body.Close()
	require.Equal(t, http.StatusOK, respGrpConv.StatusCode)

	reqGrpAdd, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Group/grp-w80/$add", bytes.NewReader(grpMutProto))
	require.NoError(t, err)
	reqGrpAdd.Header.Set("Content-Type", "application/x-protobuf")
	reqGrpAdd.Header.Set("Accept", "application/x-protobuf")
	respGrpAdd, err := client.Do(reqGrpAdd)
	require.NoError(t, err)
	grpAddBytes, _ := io.ReadAll(respGrpAdd.Body)
	respGrpAdd.Body.Close()
	require.Equal(t, http.StatusOK, respGrpAdd.StatusCode)
	assert.Equal(t, "application/x-protobuf", respGrpAdd.Header.Get("Content-Type"))
	assert.NotEmpty(t, grpAddBytes)

	reqGrpRem, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Group/grp-w80/$remove", bytes.NewReader(grpMutProto))
	require.NoError(t, err)
	reqGrpRem.Header.Set("Content-Type", "application/x-protobuf")
	reqGrpRem.Header.Set("Accept", "application/x-protobuf")
	respGrpRem, err := client.Do(reqGrpRem)
	require.NoError(t, err)
	grpRemBytes, _ := io.ReadAll(respGrpRem.Body)
	respGrpRem.Body.Close()
	require.Equal(t, http.StatusOK, respGrpRem.StatusCode)
	assert.NotEmpty(t, grpRemBytes)

	patDupSeed := `{"resourceType":"Patient","id":"pat-dup-w80","active":true,"name":[{"family":"ProtoDuplicate","given":["Bob"]}]}`
	reqPatDupPut, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/proto/Patient/pat-dup-w80", strings.NewReader(patDupSeed))
	require.NoError(t, err)
	reqPatDupPut.Header.Set("Content-Type", "application/fhir+json")
	respPatDupPut, err := client.Do(reqPatDupPut)
	require.NoError(t, err)
	respPatDupPut.Body.Close()
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, respPatDupPut.StatusCode)

	mergeParamsJSON := `{"resourceType":"Parameters","parameter":[{"name":"source-patient","valueReference":{"reference":"Patient/pat-dup-w80"}},{"name":"target-patient","valueReference":{"reference":"Patient/` + patID + `"}},{"name":"preview","valueBoolean":true}]}`
	reqMergeConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(mergeParamsJSON))
	require.NoError(t, err)
	reqMergeConv.Header.Set("Content-Type", "application/fhir+json")
	reqMergeConv.Header.Set("Accept", "application/x-protobuf")
	respMergeConv, err := client.Do(reqMergeConv)
	require.NoError(t, err)
	mergeParamsProto, _ := io.ReadAll(respMergeConv.Body)
	respMergeConv.Body.Close()
	require.Equal(t, http.StatusOK, respMergeConv.StatusCode)

	reqMergePost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/$merge", bytes.NewReader(mergeParamsProto))
	require.NoError(t, err)
	reqMergePost.Header.Set("Content-Type", "application/x-protobuf")
	reqMergePost.Header.Set("Accept", "application/x-protobuf")
	respMergePost, err := client.Do(reqMergePost)
	require.NoError(t, err)
	mergeOutBytes, _ := io.ReadAll(respMergePost.Body)
	respMergePost.Body.Close()
	require.Equal(t, http.StatusOK, respMergePost.StatusCode)
	var mergeOutParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(mergeOutBytes, &mergeOutParams))
	assert.NotEmpty(t, mergeOutParams.GetParameter())

	reqReidxPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/"+patID+"/$reindex", nil)
	require.NoError(t, err)
	reqReidxPost.Header.Set("Accept", "application/x-protobuf")
	respReidxPost, err := client.Do(reqReidxPost)
	require.NoError(t, err)
	reidxBytes, _ := io.ReadAll(respReidxPost.Body)
	respReidxPost.Body.Close()
	require.Equal(t, http.StatusOK, respReidxPost.StatusCode)
	var reidxOO oopb.OperationOutcome
	require.NoError(t, proto.Unmarshal(reidxBytes, &reidxOO))
	assert.NotEmpty(t, reidxOO.GetIssue())

	reqVDSQLProto, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/ViewDefinition/observation-vitals/$sql?dialect=doris&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqVDSQLProto.Header.Set("Accept", "application/x-protobuf")
	respVDSQLProto, err := client.Do(reqVDSQLProto)
	require.NoError(t, err)
	vdSQLProtoBytes, _ := io.ReadAll(respVDSQLProto.Body)
	respVDSQLProto.Body.Close()
	require.Equal(t, http.StatusOK, respVDSQLProto.StatusCode)
	var vdSQLParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(vdSQLProtoBytes, &vdSQLParams))
	assert.NotEmpty(t, vdSQLParams.GetParameter())

	arithTemporalVD := `{
		"resourceType": "ViewDefinition",
		"name": "obs_arith_concat_temporal",
		"status": "active",
		"resource": "Observation",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "code_label", "path": "code.coding.system & '|' & code.coding.code"},
					{"name": "val_scaled", "path": "valueQuantity.value * 2 + 10"},
					{"name": "val_sub", "path": "valueQuantity.value - 5"},
					{"name": "val_div_float", "path": "valueQuantity.value / 2"},
					{"name": "val_div_int", "path": "valueQuantity.value div 10"},
					{"name": "val_mod", "path": "valueQuantity.value mod 5"},
					{"name": "eff_date", "path": "effectiveDateTime.toDate()"},
					{"name": "eff_dt", "path": "effectiveDateTime.toDateTime()"},
					{"name": "cur_date", "path": "today()"},
					{"name": "cur_ts", "path": "now()"}
				]
			}
		],
		"where": [
			{"path": "valueQuantity.value + 5 >= 65 and effectiveDateTime.toDate() <= today()"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"CONCAT(r.code_system, '|', r.code)",
				"((r.value_quantity * 2) + 10)",
				"(r.value_quantity - 5)",
				"(r.value_quantity / 2)",
				"CAST((r.value_quantity / 10) AS BIGINT)",
				"MOD(r.value_quantity, 5)",
				"CAST(r.effective_start AS DATE)",
				"CAST(r.effective_start AS DATETIME)",
				"CURRENT_DATE()",
				"NOW()",
				"(r.value_quantity + 5) >= 65",
				"CAST(r.effective_start AS DATE) <= CURRENT_DATE()",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"|| '|' ||",
				" * 2) + 10)",
				" - 5)",
				" / 2)",
				"CAST(TRUNC((",
				"MOD(CAST(",
				"AS DATE)",
				"AS TIMESTAMPTZ)",
				"CURRENT_DATE",
				"NOW()",
				" + 5) >= 65",
				" <= CURRENT_DATE",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"|| '|' ||",
				" * 2) + 10)",
				"CAST((",
				" % CAST(5 AS INTEGER))",
				"DATE((SELECT d.sp_low",
				"DATETIME((SELECT d.sp_low",
				"CURRENT_DATE",
				"CURRENT_TIMESTAMP",
			},
		},
	} {
		reqAT, aErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(arithTemporalVD))
		require.NoError(t, aErr)
		reqAT.Header.Set("Content-Type", "application/fhir+json")
		respAT, aErr := client.Do(reqAT)
		require.NoError(t, aErr)
		atBytes, _ := io.ReadAll(respAT.Body)
		respAT.Body.Close()
		require.Equal(t, http.StatusOK, respAT.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(atBytes), want)
		}
	}

	// 25. Binary Proto3 Da Vinci Payer (Claim/$submit, CoverageEligibilityRequest/$submit) & Conformance (StructureDefinition/$snapshot, NamingSystem/$preferred-id)
	clmJSON := `{"resourceType":"Claim","id":"proto-claim-1","status":"active","type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"professional"}]},"use":"preauthorization","patient":{"reference":"Patient/proto-pat-1"}}`
	reqClmConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Claim/$convert", strings.NewReader(clmJSON))
	require.NoError(t, err)
	reqClmConv.Header.Set("Content-Type", "application/fhir+json")
	reqClmConv.Header.Set("Accept", "application/x-protobuf")
	respClmConv, err := client.Do(reqClmConv)
	require.NoError(t, err)
	clmProtoBytes, _ := io.ReadAll(respClmConv.Body)
	respClmConv.Body.Close()
	require.Equal(t, http.StatusOK, respClmConv.StatusCode)

	reqClmSub, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Claim/$submit", bytes.NewReader(clmProtoBytes))
	require.NoError(t, err)
	reqClmSub.Header.Set("Content-Type", "application/x-protobuf")
	reqClmSub.Header.Set("Accept", "application/x-protobuf")
	respClmSub, err := client.Do(reqClmSub)
	require.NoError(t, err)
	clmRespProto, _ := io.ReadAll(respClmSub.Body)
	respClmSub.Body.Close()
	require.Equal(t, http.StatusOK, respClmSub.StatusCode)
	assert.Contains(t, respClmSub.Header.Get("Content-Type"), "application/x-protobuf")
	assert.Equal(t, "google.fhir.r4.proto3", respClmSub.Header.Get("X-Flint-Wire-Format"))
	assert.NotEmpty(t, clmRespProto)

	cerJSON := `{"resourceType":"CoverageEligibilityRequest","id":"proto-cer-1","status":"active","purpose":["benefits"],"patient":{"reference":"Patient/proto-pat-1"}}`
	reqCERConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/CoverageEligibilityRequest/$convert", strings.NewReader(cerJSON))
	require.NoError(t, err)
	reqCERConv.Header.Set("Content-Type", "application/fhir+json")
	reqCERConv.Header.Set("Accept", "application/x-protobuf")
	respCERConv, err := client.Do(reqCERConv)
	require.NoError(t, err)
	cerProtoBytes, _ := io.ReadAll(respCERConv.Body)
	respCERConv.Body.Close()
	require.Equal(t, http.StatusOK, respCERConv.StatusCode)

	reqCERSub, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/CoverageEligibilityRequest/$submit", bytes.NewReader(cerProtoBytes))
	require.NoError(t, err)
	reqCERSub.Header.Set("Content-Type", "application/x-protobuf")
	reqCERSub.Header.Set("Accept", "application/x-protobuf")
	respCERSub, err := client.Do(reqCERSub)
	require.NoError(t, err)
	cerRespProto, _ := io.ReadAll(respCERSub.Body)
	respCERSub.Body.Close()
	require.Equal(t, http.StatusOK, respCERSub.StatusCode)
	assert.Contains(t, respCERSub.Header.Get("Content-Type"), "application/x-protobuf")
	assert.Equal(t, "google.fhir.r4.proto3", respCERSub.Header.Get("X-Flint-Wire-Format"))
	assert.NotEmpty(t, cerRespProto)

	reqSnap, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/StructureDefinition/$snapshot?url=http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqSnap.Header.Set("Accept", "application/x-protobuf")
	respSnap, err := client.Do(reqSnap)
	require.NoError(t, err)
	snapProtoBytes, _ := io.ReadAll(respSnap.Body)
	respSnap.Body.Close()
	require.Equal(t, http.StatusOK, respSnap.StatusCode)
	assert.Contains(t, respSnap.Header.Get("Content-Type"), "application/x-protobuf")
	assert.Equal(t, "google.fhir.r4.proto3", respSnap.Header.Get("X-Flint-Wire-Format"))
	assert.NotEmpty(t, snapProtoBytes)

	reqPref, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/NamingSystem/$preferred-id?id=2.16.840.1.113883.6.1&type=uri&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqPref.Header.Set("Accept", "application/x-protobuf")
	respPref, err := client.Do(reqPref)
	require.NoError(t, err)
	prefProtoBytes, _ := io.ReadAll(respPref.Body)
	respPref.Body.Close()
	require.Equal(t, http.StatusOK, respPref.StatusCode)
	assert.Contains(t, respPref.Header.Get("Content-Type"), "application/x-protobuf")
	assert.Equal(t, "google.fhir.r4.proto3", respPref.Header.Get("X-Flint-Wire-Format"))
	var prefParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(prefProtoBytes, &prefParams))
	assert.NotEmpty(t, prefParams.GetParameter())

	// 26. SQL-on-FHIR v2 Collection Set/Deduplication & Transcendental Math $sql Pushdown
	setMathVD := `{
		"resourceType": "ViewDefinition",
		"name": "obs_set_math",
		"status": "active",
		"resource": "Observation",
		"select": [{
			"column": [
				{"name": "id", "path": "id"},
				{"name": "distinct_given", "path": "name.given.distinct()", "collection": true},
				{"name": "distinct_joined", "path": "name.given.distinct().join(', ')"},
				{"name": "distinct_cnt", "path": "name.given.distinct().count()"},
				{"name": "is_dist_col", "path": "name.given.isDistinct()"},
				{"name": "union_names", "path": "name.given.union(name.family)", "collection": true},
				{"name": "intersect_names", "path": "name.given.intersect(name.family)", "collection": true},
				{"name": "exclude_names", "path": "name.given.exclude(name.family)", "collection": true},
				{"name": "val_exp", "path": "valueQuantity.value.exp()"},
				{"name": "val_ln", "path": "valueQuantity.value.ln()"},
				{"name": "val_log10", "path": "valueQuantity.value.log(10)"},
				{"name": "val_pow2", "path": "valueQuantity.value.power(2)"},
				{"name": "val_trunc", "path": "valueQuantity.value.truncate()"}
			]
		}],
		"where": [
			{"path": "name.given.isDistinct() and valueQuantity.value.truncate() >= 60"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"ARRAY_DISTINCT(",
				"ARRAY_JOIN(ARRAY_DISTINCT(",
				"ARRAY_SIZE(ARRAY_DISTINCT(",
				"ARRAY_UNION(",
				"ARRAY_INTERSECT(",
				"ARRAY_EXCEPT(",
				"EXP(r.value_quantity)",
				"LN(r.value_quantity)",
				"LOG(10, r.value_quantity)",
				"POW(r.value_quantity, 2)",
				"TRUNCATE(r.value_quantity, 0)",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"ARRAY_AGG(DISTINCT s.sp_value)",
				"ARRAY_TO_STRING(ARRAY_AGG(DISTINCT s.sp_value), ', ')",
				"COUNT(DISTINCT s.sp_value)",
				" UNION SELECT ",
				" INTERSECT SELECT ",
				" EXCEPT SELECT ",
				"EXP(",
				"LN(",
				"LOG(10, ",
				"POWER(",
				"TRUNC(",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"SELECT DISTINCT s.sp_value",
				" UNION SELECT ",
				" INTERSECT SELECT ",
				" EXCEPT SELECT ",
				"COUNT(DISTINCT s.sp_value)",
				"EXP(",
				"LN(",
				"LOG(10, ",
				"POW(",
				"CAST(",
			},
		},
	} {
		reqSM, smErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(setMathVD))
		require.NoError(t, smErr)
		reqSM.Header.Set("Content-Type", "application/fhir+json")
		respSM, smErr := client.Do(reqSM)
		require.NoError(t, smErr)
		smBytes, _ := io.ReadAll(respSM.Body)
		respSM.Body.Close()
		require.Equal(t, http.StatusOK, respSM.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(smBytes), want)
		}
	}

	// 27. Binary Proto3 Subscription/$status (GET & POST), Subscription/$events (GET & POST),
	// AuditEvent (GET search & $verify-integrity), /$diff (GET & POST), /$import, /$import-poll DELETE,
	// Patient/$expunge, Patient/$purge, and TestScript/$evaluate
	subSeed := `{"resourceType":"Subscription","id":"proto-sub-1","status":"requested","reason":"Proto test","criteria":"Observation?code=8867-4","channel":{"type":"rest-hook","endpoint":"` + ts.URL + `/fhir/r4/proto/$validate","payload":"application/x-protobuf"}}`
	reqSubPut, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/proto/Subscription/proto-sub-1", strings.NewReader(subSeed))
	require.NoError(t, err)
	reqSubPut.Header.Set("Content-Type", "application/fhir+json")
	respSubPut, err := client.Do(reqSubPut)
	require.NoError(t, err)
	respSubPut.Body.Close()
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, respSubPut.StatusCode)

	reqSubSt, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Subscription/proto-sub-1/$status?_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqSubSt.Header.Set("Accept", "application/x-protobuf")
	respSubSt, err := client.Do(reqSubSt)
	require.NoError(t, err)
	subStBytes, _ := io.ReadAll(respSubSt.Body)
	respSubSt.Body.Close()
	require.Equal(t, http.StatusOK, respSubSt.StatusCode)
	assert.Contains(t, respSubSt.Header.Get("Content-Type"), "application/x-protobuf")
	assert.Equal(t, "google.fhir.r4.proto3", respSubSt.Header.Get("X-Flint-Wire-Format"))
	var subStParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(subStBytes, &subStParams))
	assert.NotEmpty(t, subStParams.GetParameter())

	// POST /Subscription/$status with binary Proto3 Parameters body
	subStInJSON := `{"resourceType":"Parameters","parameter":[{"name":"id","valueString":"proto-sub-1"}]}`
	reqSubStConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(subStInJSON))
	require.NoError(t, err)
	reqSubStConv.Header.Set("Content-Type", "application/fhir+json")
	reqSubStConv.Header.Set("Accept", "application/x-protobuf")
	respSubStConv, err := client.Do(reqSubStConv)
	require.NoError(t, err)
	subStInProto, _ := io.ReadAll(respSubStConv.Body)
	respSubStConv.Body.Close()
	require.Equal(t, http.StatusOK, respSubStConv.StatusCode)

	reqSubStPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Subscription/$status", bytes.NewReader(subStInProto))
	require.NoError(t, err)
	reqSubStPost.Header.Set("Content-Type", "application/x-protobuf")
	reqSubStPost.Header.Set("Accept", "application/x-protobuf")
	respSubStPost, err := client.Do(reqSubStPost)
	require.NoError(t, err)
	subStPostBytes, _ := io.ReadAll(respSubStPost.Body)
	respSubStPost.Body.Close()
	require.Equal(t, http.StatusOK, respSubStPost.StatusCode)
	var subStPostParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(subStPostBytes, &subStPostParams))
	assert.NotEmpty(t, subStPostParams.GetParameter())

	reqSubEv, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Subscription/proto-sub-1/$events?_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqSubEv.Header.Set("Accept", "application/x-protobuf")
	respSubEv, err := client.Do(reqSubEv)
	require.NoError(t, err)
	subEvBytes, _ := io.ReadAll(respSubEv.Body)
	respSubEv.Body.Close()
	require.Equal(t, http.StatusOK, respSubEv.StatusCode)
	assert.Contains(t, respSubEv.Header.Get("Content-Type"), "application/x-protobuf")
	var subEvBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(subEvBytes, &subEvBundle))
	assert.NotEmpty(t, subEvBundle.GetEntry())

	// POST /Subscription/proto-sub-1/$events with binary Proto3 Parameters body
	subEvInJSON := `{"resourceType":"Parameters","parameter":[{"name":"eventsSinceNumber","valueString":"0"},{"name":"content","valueCode":"full-resource"}]}`
	reqSubEvConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(subEvInJSON))
	require.NoError(t, err)
	reqSubEvConv.Header.Set("Content-Type", "application/fhir+json")
	reqSubEvConv.Header.Set("Accept", "application/x-protobuf")
	respSubEvConv, err := client.Do(reqSubEvConv)
	require.NoError(t, err)
	subEvInProto, _ := io.ReadAll(respSubEvConv.Body)
	respSubEvConv.Body.Close()
	require.Equal(t, http.StatusOK, respSubEvConv.StatusCode)

	reqSubEvPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Subscription/proto-sub-1/$events", bytes.NewReader(subEvInProto))
	require.NoError(t, err)
	reqSubEvPost.Header.Set("Content-Type", "application/x-protobuf")
	reqSubEvPost.Header.Set("Accept", "application/x-protobuf")
	respSubEvPost, err := client.Do(reqSubEvPost)
	require.NoError(t, err)
	subEvPostBytes, _ := io.ReadAll(respSubEvPost.Body)
	respSubEvPost.Body.Close()
	require.Equal(t, http.StatusOK, respSubEvPost.StatusCode)
	var subEvPostBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(subEvPostBytes, &subEvPostBundle))
	assert.NotEmpty(t, subEvPostBundle.GetEntry())

	// GET /AuditEvent search & GET /AuditEvent/$verify-integrity in binary Proto3
	reqAudSearch, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/AuditEvent?_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqAudSearch.Header.Set("Accept", "application/x-protobuf")
	respAudSearch, err := client.Do(reqAudSearch)
	require.NoError(t, err)
	audSearchBytes, _ := io.ReadAll(respAudSearch.Body)
	respAudSearch.Body.Close()
	require.Equal(t, http.StatusOK, respAudSearch.StatusCode)
	assert.Contains(t, respAudSearch.Header.Get("Content-Type"), "application/x-protobuf")
	assert.Equal(t, "google.fhir.r4.proto3", respAudSearch.Header.Get("X-Flint-Wire-Format"))
	var audSearchBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(audSearchBytes, &audSearchBundle))

	reqAudVer, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/AuditEvent/$verify-integrity?_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqAudVer.Header.Set("Accept", "application/x-protobuf")
	respAudVer, err := client.Do(reqAudVer)
	require.NoError(t, err)
	audVerBytes, _ := io.ReadAll(respAudVer.Body)
	respAudVer.Body.Close()
	require.Equal(t, http.StatusOK, respAudVer.StatusCode)
	assert.Contains(t, respAudVer.Header.Get("Content-Type"), "application/x-protobuf")
	var audVerParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(audVerBytes, &audVerParams))
	assert.NotEmpty(t, audVerParams.GetParameter())

	// POST & GET /$diff in binary Proto3
	diffInJSON := `{"resourceType":"Parameters","parameter":[{"name":"from","resource":{"resourceType":"Patient","id":"diff-p1","active":true,"gender":"female"}},{"name":"to","resource":{"resourceType":"Patient","id":"diff-p1","active":true,"gender":"other"}}]}`
	reqDiffConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(diffInJSON))
	require.NoError(t, err)
	reqDiffConv.Header.Set("Content-Type", "application/fhir+json")
	reqDiffConv.Header.Set("Accept", "application/x-protobuf")
	respDiffConv, err := client.Do(reqDiffConv)
	require.NoError(t, err)
	diffInProto, _ := io.ReadAll(respDiffConv.Body)
	respDiffConv.Body.Close()
	require.Equal(t, http.StatusOK, respDiffConv.StatusCode)

	reqDiff, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/$diff", bytes.NewReader(diffInProto))
	require.NoError(t, err)
	reqDiff.Header.Set("Content-Type", "application/x-protobuf")
	reqDiff.Header.Set("Accept", "application/x-protobuf")
	respDiff, err := client.Do(reqDiff)
	require.NoError(t, err)
	diffOutBytes, _ := io.ReadAll(respDiff.Body)
	respDiff.Body.Close()
	require.Equal(t, http.StatusOK, respDiff.StatusCode)
	assert.Contains(t, respDiff.Header.Get("Content-Type"), "application/x-protobuf")
	var diffOutParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(diffOutBytes, &diffOutParams))
	assert.NotEmpty(t, diffOutParams.GetParameter())

	reqInstDiff, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient/"+patID+"/$diff?fromVersion=1&toVersion=2&_format=application/x-protobuf", nil)
	require.NoError(t, err)
	reqInstDiff.Header.Set("Accept", "application/x-protobuf")
	respInstDiff, err := client.Do(reqInstDiff)
	require.NoError(t, err)
	instDiffBytes, _ := io.ReadAll(respInstDiff.Body)
	respInstDiff.Body.Close()
	require.Equal(t, http.StatusOK, respInstDiff.StatusCode)
	var instDiffParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(instDiffBytes, &instDiffParams))
	assert.NotEmpty(t, instDiffParams.GetParameter())

	// POST /$import (synchronous & asynchronous cancel) in binary Proto3
	impParamsJSON := `{"resourceType":"Parameters","parameter":[{"name":"inputFormat","valueCode":"application/fhir+ndjson"},{"name":"input","part":[{"name":"type","valueCode":"Patient"},{"name":"content","valueString":"{\"resourceType\":\"Patient\",\"id\":\"imp-proto-p1\",\"active\":true,\"gender\":\"female\",\"birthDate\":\"1990-01-01\",\"identifier\":[{\"system\":\"http://hospital.smarthealthit.org\",\"value\":\"imp-proto-p1\"}],\"name\":[{\"family\":\"ImportedProto\",\"given\":[\"Alice\"]}]}"}]}]}`
	reqImpConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(impParamsJSON))
	require.NoError(t, err)
	reqImpConv.Header.Set("Content-Type", "application/fhir+json")
	reqImpConv.Header.Set("Accept", "application/x-protobuf")
	respImpConv, err := client.Do(reqImpConv)
	require.NoError(t, err)
	impParamsProto, _ := io.ReadAll(respImpConv.Body)
	respImpConv.Body.Close()
	require.Equal(t, http.StatusOK, respImpConv.StatusCode)

	reqImpSync, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/$import", bytes.NewReader(impParamsProto))
	require.NoError(t, err)
	reqImpSync.Header.Set("Content-Type", "application/x-protobuf")
	reqImpSync.Header.Set("Accept", "application/x-protobuf")
	respImpSync, err := client.Do(reqImpSync)
	require.NoError(t, err)
	impSyncBytes, _ := io.ReadAll(respImpSync.Body)
	respImpSync.Body.Close()
	require.Equal(t, http.StatusOK, respImpSync.StatusCode)
	assert.Contains(t, respImpSync.Header.Get("Content-Type"), "application/x-protobuf")
	var impSyncOO oopb.OperationOutcome
	require.NoError(t, proto.Unmarshal(impSyncBytes, &impSyncOO))
	require.NotEmpty(t, impSyncOO.GetIssue())
	assert.Contains(t, impSyncOO.GetIssue()[0].GetDiagnostics().GetValue(), "Successfully imported 1 FHIR resource(s) via $import")

	reqImpAsync, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/$import", bytes.NewReader(impParamsProto))
	require.NoError(t, err)
	reqImpAsync.Header.Set("Content-Type", "application/x-protobuf")
	reqImpAsync.Header.Set("Prefer", "respond-async")
	respImpAsync, err := client.Do(reqImpAsync)
	require.NoError(t, err)
	respImpAsync.Body.Close()
	require.Equal(t, http.StatusAccepted, respImpAsync.StatusCode)
	impPollLoc := respImpAsync.Header.Get("Content-Location")
	require.NotEmpty(t, impPollLoc)

	reqImpCancel, err := http.NewRequest(http.MethodDelete, impPollLoc, nil)
	require.NoError(t, err)
	reqImpCancel.Header.Set("Accept", "application/x-protobuf")
	respImpCancel, err := client.Do(reqImpCancel)
	require.NoError(t, err)
	impCancelBytes, _ := io.ReadAll(respImpCancel.Body)
	respImpCancel.Body.Close()
	require.Equal(t, http.StatusAccepted, respImpCancel.StatusCode)
	var impCancelOO oopb.OperationOutcome
	require.NoError(t, proto.Unmarshal(impCancelBytes, &impCancelOO))
	assert.NotEmpty(t, impCancelOO.GetIssue())

	// POST /Patient/{id}/$expunge in binary Proto3
	expInJSON := `{"resourceType":"Parameters","parameter":[{"name":"expungePreviousVersions","valueBoolean":true},{"name":"limit","valueInteger":10}]}`
	reqExpConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(expInJSON))
	require.NoError(t, err)
	reqExpConv.Header.Set("Content-Type", "application/fhir+json")
	reqExpConv.Header.Set("Accept", "application/x-protobuf")
	respExpConv, err := client.Do(reqExpConv)
	require.NoError(t, err)
	expInProto, _ := io.ReadAll(respExpConv.Body)
	respExpConv.Body.Close()
	require.Equal(t, http.StatusOK, respExpConv.StatusCode)

	reqExpPost, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/imp-proto-p1/$expunge", bytes.NewReader(expInProto))
	require.NoError(t, err)
	reqExpPost.Header.Set("Content-Type", "application/x-protobuf")
	reqExpPost.Header.Set("Accept", "application/x-protobuf")
	respExpPost, err := client.Do(reqExpPost)
	require.NoError(t, err)
	expOutBytes, _ := io.ReadAll(respExpPost.Body)
	respExpPost.Body.Close()
	require.Equal(t, http.StatusOK, respExpPost.StatusCode)
	var expOutOO oopb.OperationOutcome
	require.NoError(t, proto.Unmarshal(expOutBytes, &expOutOO))
	assert.NotEmpty(t, expOutOO.GetIssue())

	// POST /TestScript/$evaluate in binary Proto3
	tsEvalJSON := `{"resourceType":"TestScript","id":"ts-proto-eval","url":"http://flint.fhir/TestScript/proto-eval","name":"ProtoEval","status":"active","test":[{"name":"ReadPatient","action":[{"operation":{"type":{"code":"read"},"resource":"Patient","params":"/` + patID + `"}},{"assert":{"response":"okay"}}]}]}`
	reqTSConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/TestScript/$convert", strings.NewReader(tsEvalJSON))
	require.NoError(t, err)
	reqTSConv.Header.Set("Content-Type", "application/fhir+json")
	reqTSConv.Header.Set("Accept", "application/x-protobuf")
	respTSConv, err := client.Do(reqTSConv)
	require.NoError(t, err)
	tsEvalProto, _ := io.ReadAll(respTSConv.Body)
	respTSConv.Body.Close()
	require.Equal(t, http.StatusOK, respTSConv.StatusCode)

	reqTSEval, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/TestScript/$evaluate", bytes.NewReader(tsEvalProto))
	require.NoError(t, err)
	reqTSEval.Header.Set("Content-Type", "application/x-protobuf")
	reqTSEval.Header.Set("Accept", "application/x-protobuf")
	respTSEval, err := client.Do(reqTSEval)
	require.NoError(t, err)
	tsEvalOutBytes, _ := io.ReadAll(respTSEval.Body)
	respTSEval.Body.Close()
	require.Equal(t, http.StatusOK, respTSEval.StatusCode)
	assert.Contains(t, respTSEval.Header.Get("Content-Type"), "application/x-protobuf")
	assert.Equal(t, "google.fhir.r4.proto3", respTSEval.Header.Get("X-Flint-Wire-Format"))
	assert.NotEmpty(t, tsEvalOutBytes)

	// 28. SQL-on-FHIR v2 Collection Slicing (.tail, .single, .combine), Set Predicates (.subsetOf, .supersetOf) & .split() $sql Pushdown
	sliceSplitVD := `{
		"resourceType": "ViewDefinition",
		"name": "obs_slice_split",
		"status": "active",
		"resource": "Observation",
		"select": [{
			"column": [
				{"name": "id", "path": "id"},
				{"name": "tail_given", "path": "name.given.tail()", "collection": true},
				{"name": "tail_joined", "path": "name.given.tail().join(', ')"},
				{"name": "tail_first", "path": "name.given.tail().first()"},
				{"name": "tail_cnt", "path": "name.given.tail().count()"},
				{"name": "tail_exists", "path": "name.given.tail().exists()"},
				{"name": "tail_empty", "path": "name.given.tail().empty()"},
				{"name": "single_family", "path": "name.family.single()"},
				{"name": "combine_names", "path": "name.given.combine(name.family)", "collection": true},
				{"name": "combine_joined", "path": "name.given.combine(name.family).join(' ')"},
				{"name": "combine_cnt", "path": "name.given.combine(name.family).count()"},
				{"name": "subset_col", "path": "name.family.subsetOf(name.given)"},
				{"name": "superset_col", "path": "name.given.supersetOf(name.family)"},
				{"name": "split_code", "path": "code.coding.code.split('-')", "collection": true},
				{"name": "split_scalar", "path": "code.coding.code.split('-')"}
			]
		}],
		"where": [
			{"path": "name.family.subsetOf(name.given) and name.given.supersetOf(name.family) and name.given.tail().exists() and name.given.tail().empty()"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"ARRAY_SLICE(",
				"ARRAY_JOIN(ARRAY_SLICE(",
				"GREATEST(ARRAY_SIZE(",
				"ARRAY_SIZE(",
				" > 1)",
				" <= 1)",
				"element_at(",
				"ARRAY_CONCAT(",
				"ARRAY_JOIN(ARRAY_CONCAT(",
				"ARRAY_SIZE(ARRAY_EXCEPT(",
				"SPLIT_BY_STRING(",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"OFFSET 1) sub)",
				"LIMIT 1 OFFSET 1)",
				"GREATEST((SELECT COUNT(*) FROM (",
				" UNION ALL SELECT ",
				"ARRAY_TO_STRING(ARRAY(",
				"NOT EXISTS (",
				" EXCEPT SELECT ",
				"STRING_TO_ARRAY(",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"LIMIT -1 OFFSET 1) sub)",
				"LIMIT 1 OFFSET 1)",
				"MAX((SELECT COUNT(*) FROM (",
				" UNION ALL SELECT ",
				"group_concat(u.sp_value, ' ')",
				"NOT EXISTS (",
				" EXCEPT SELECT ",
				"json_array(",
			},
		},
	} {
		reqSS, ssErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(sliceSplitVD))
		require.NoError(t, ssErr)
		reqSS.Header.Set("Content-Type", "application/fhir+json")
		respSS, ssErr := client.Do(reqSS)
		require.NoError(t, ssErr)
		ssBytes, _ := io.ReadAll(respSS.Body)
		respSS.Body.Close()
		require.Equal(t, http.StatusOK, respSS.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(ssBytes), want)
		}
	}

	// 29. Binary Proto3 /$sync-analytics, /$process-message, Appointment/$find,
	// CodeSystem/$find-matches, Patient/$member-match, _elements projection & Prefer headers
	reqSyncP, syncPErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/$sync-analytics", nil)
	require.NoError(t, syncPErr)
	reqSyncP.Header.Set("Accept", "application/x-protobuf")
	respSyncP, syncPErr := client.Do(reqSyncP)
	require.NoError(t, syncPErr)
	syncPBytes, _ := io.ReadAll(respSyncP.Body)
	respSyncP.Body.Close()
	require.Equal(t, http.StatusOK, respSyncP.StatusCode)
	var syncPOut parampb.Parameters
	require.NoError(t, proto.Unmarshal(syncPBytes, &syncPOut))
	assert.NotEmpty(t, syncPOut.GetParameter())

	msgInJSON := `{"resourceType":"Bundle","type":"message","entry":[{"resource":{"resourceType":"MessageHeader","id":"mh-proto-83","eventCoding":{"system":"http://example.org/events","code":"admin-notify"},"source":{"endpoint":"http://example.org/src"}}}]}`
	reqMsgConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Bundle/$convert", strings.NewReader(msgInJSON))
	require.NoError(t, err)
	reqMsgConv.Header.Set("Content-Type", "application/fhir+json")
	reqMsgConv.Header.Set("Accept", "application/x-protobuf")
	respMsgConv, err := client.Do(reqMsgConv)
	require.NoError(t, err)
	msgInProto, _ := io.ReadAll(respMsgConv.Body)
	respMsgConv.Body.Close()
	require.Equal(t, http.StatusOK, respMsgConv.StatusCode)

	reqMsgP, msgPErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/$process-message", bytes.NewReader(msgInProto))
	require.NoError(t, msgPErr)
	reqMsgP.Header.Set("Content-Type", "application/x-protobuf")
	reqMsgP.Header.Set("Accept", "application/x-protobuf")
	respMsgP, msgPErr := client.Do(reqMsgP)
	require.NoError(t, msgPErr)
	msgOutBytes, _ := io.ReadAll(respMsgP.Body)
	respMsgP.Body.Close()
	require.Equal(t, http.StatusOK, respMsgP.StatusCode)
	var msgOutBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(msgOutBytes, &msgOutBundle))
	assert.NotEmpty(t, msgOutBundle.GetEntry())

	reqFindP, findPErr := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Appointment/$find?start=2025-01-01T00:00:00Z&end=2026-12-31T23:59:59Z", nil)
	require.NoError(t, findPErr)
	reqFindP.Header.Set("Accept", "application/x-protobuf")
	respFindP, findPErr := client.Do(reqFindP)
	require.NoError(t, findPErr)
	findPBytes, _ := io.ReadAll(respFindP.Body)
	respFindP.Body.Close()
	require.Equal(t, http.StatusOK, respFindP.StatusCode)
	var findOutBundle bcrpb.Bundle
	require.NoError(t, proto.Unmarshal(findPBytes, &findOutBundle))
	assert.NotNil(t, findOutBundle.GetType())

	fmInJSON := `{"resourceType":"Parameters","parameter":[{"name":"system","valueUri":"http://loinc.org"}]}`
	reqFmConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(fmInJSON))
	require.NoError(t, err)
	reqFmConv.Header.Set("Content-Type", "application/fhir+json")
	reqFmConv.Header.Set("Accept", "application/x-protobuf")
	respFmConv, err := client.Do(reqFmConv)
	require.NoError(t, err)
	fmInProto, _ := io.ReadAll(respFmConv.Body)
	respFmConv.Body.Close()
	require.Equal(t, http.StatusOK, respFmConv.StatusCode)

	reqFmP, fmPErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/CodeSystem/$find-matches", bytes.NewReader(fmInProto))
	require.NoError(t, fmPErr)
	reqFmP.Header.Set("Content-Type", "application/x-protobuf")
	reqFmP.Header.Set("Accept", "application/x-protobuf")
	respFmP, fmPErr := client.Do(reqFmP)
	require.NoError(t, fmPErr)
	fmOutBytes, _ := io.ReadAll(respFmP.Body)
	respFmP.Body.Close()
	require.Equal(t, http.StatusOK, respFmP.StatusCode)
	var fmOutParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(fmOutBytes, &fmOutParams))
	assert.NotEmpty(t, fmOutBytes)

	mmInJSON := `{"resourceType":"Parameters","parameter":[{"name":"MemberPatient","resource":{"resourceType":"Patient","identifier":[{"system":"http://hospital.example.org/mrn","value":"MRN-PROTO-TX-1"}],"name":[{"family":"ProtoBundleTx","given":["Grace"]}],"gender":"female","birthDate":"1975-08-20"}},{"name":"CoverageToMatch","resource":{"resourceType":"Coverage","status":"active","subscriberId":"MRN-PROTO-TX-1","beneficiary":{"reference":"Patient/tx-proto-pat-1"},"payor":[{"display":"Example Payor"}]}}]}`
	reqMmConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(mmInJSON))
	require.NoError(t, err)
	reqMmConv.Header.Set("Content-Type", "application/fhir+json")
	reqMmConv.Header.Set("Accept", "application/x-protobuf")
	respMmConv, err := client.Do(reqMmConv)
	require.NoError(t, err)
	mmInProto, _ := io.ReadAll(respMmConv.Body)
	respMmConv.Body.Close()
	require.Equal(t, http.StatusOK, respMmConv.StatusCode)

	reqMmP, mmPErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Patient/$member-match", bytes.NewReader(mmInProto))
	require.NoError(t, mmPErr)
	reqMmP.Header.Set("Content-Type", "application/x-protobuf")
	reqMmP.Header.Set("Accept", "application/x-protobuf")
	respMmP, mmPErr := client.Do(reqMmP)
	require.NoError(t, mmPErr)
	mmOutBytes, _ := io.ReadAll(respMmP.Body)
	respMmP.Body.Close()
	require.Equal(t, http.StatusOK, respMmP.StatusCode)
	var mmOutParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(mmOutBytes, &mmOutParams))
	assert.NotEmpty(t, mmOutParams.GetParameter())

	// _elements projection with SUBSETTED tag in binary Proto3
	reqElP, elPErr := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient/"+patID+"?_elements=id,gender", nil)
	require.NoError(t, elPErr)
	reqElP.Header.Set("Accept", "application/x-protobuf")
	respElP, elPErr := client.Do(reqElP)
	require.NoError(t, elPErr)
	elPBytes, _ := io.ReadAll(respElP.Body)
	respElP.Body.Close()
	require.Equal(t, http.StatusOK, respElP.StatusCode)
	var elPat patpb.Patient
	require.NoError(t, proto.Unmarshal(elPBytes, &elPat))
	assert.Empty(t, elPat.GetName())
	require.NotNil(t, elPat.GetMeta())
	require.NotEmpty(t, elPat.GetMeta().GetTag())

	// Prefer: handling=strict on unknown search parameter in binary Proto3
	reqPrefP, prefPErr := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/proto/Patient?unknown_proto_search_param=1", nil)
	require.NoError(t, prefPErr)
	reqPrefP.Header.Set("Accept", "application/x-protobuf")
	reqPrefP.Header.Set("Prefer", "handling=strict")
	respPrefP, prefPErr := client.Do(reqPrefP)
	require.NoError(t, prefPErr)
	prefPBytes, _ := io.ReadAll(respPrefP.Body)
	respPrefP.Body.Close()
	require.Equal(t, http.StatusBadRequest, respPrefP.StatusCode)
	var prefOO oopb.OperationOutcome
	require.NoError(t, proto.Unmarshal(prefPBytes, &prefOO))
	assert.NotEmpty(t, prefOO.GetIssue())

	// 30. Zero-JSON SQL transpiler pushdown for .all(), .exists(criteria),
	// .anyTrue(), .allTrue(), .anyFalse(), .allFalse(), and extension('url')
	quantExtVD := `{
		"resourceType": "ViewDefinition",
		"name": "patient_quantifiers_ext",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "all_proto_fam", "path": "name.all(family = 'ProtoUpdated')"},
					{"name": "ex_proto_fam", "path": "name.exists(family = 'ProtoUpdated')"},
					{"name": "any_active", "path": "active.anyTrue()"},
					{"name": "all_active", "path": "active.allTrue()"},
					{"name": "any_inactive", "path": "active.anyFalse()"},
					{"name": "all_inactive", "path": "active.allFalse()"},
					{"name": "birthsex_ext", "path": "extension('http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex').valueCode"}
				]
			}
		],
		"where": [
			{"path": "active.anyTrue()"},
			{"path": "name.all(family = 'ProtoUpdated')"},
			{"path": "extension('http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex').exists()"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"NOT array_exists(x -> x LIKE 'family=%' AND SPLIT_PART(x, '=', 2) <> 'ProtoUpdated', r.string_tokens)",
				"array_exists(x -> x = 'family=ProtoUpdated', r.string_tokens)",
				"array_contains(r.code_tokens, 'active=|true')",
				"NOT array_contains(r.code_tokens, 'active=|false')",
				"array_contains(r.code_tokens, 'active=|false')",
				"NOT array_contains(r.code_tokens, 'active=|true')",
				"ext:us-core-birthsex=|%",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"NOT EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' AND s.sp_value <> 'ProtoUpdated')",
				"EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' AND s.sp_value = 'ProtoUpdated')",
				"EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'active' AND t.sp_value = 'true')",
				"NOT EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'active' AND t.sp_value = 'false')",
				"t.sp_system = 'http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex'",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"NOT EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' AND s.sp_value <> 'ProtoUpdated')",
				"EXISTS (SELECT 1 FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' AND s.sp_value = 'ProtoUpdated')",
				"EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'active' AND t.sp_value = 'true')",
				"NOT EXISTS (SELECT 1 FROM spidx_token t WHERE t.tenant_id = r.tenant_id AND t.res_type = r.res_type AND t.res_id = r.res_id AND t.sp_name = 'active' AND t.sp_value = 'false')",
				"t.sp_system = 'http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex'",
			},
		},
	} {
		reqQE, qeErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(quantExtVD))
		require.NoError(t, qeErr)
		reqQE.Header.Set("Content-Type", "application/fhir+json")
		respQE, qeErr := client.Do(reqQE)
		require.NoError(t, qeErr)
		qeBytes, _ := io.ReadAll(respQE.Body)
		respQE.Body.Close()
		require.Equal(t, http.StatusOK, respQE.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(qeBytes), want)
		}
	}

	// 31. Binary Proto3 PATCH (Parameters FHIRPath Patch & Merge Patch), DELETE (Prefer: return=OperationOutcome),
	// Terminology ($validate-code, $subsumes), Conformance ($subset, $implements), & Polymorphic/Aggregate $sql Pushdown
	patchParamsJSON := `{"resourceType":"Parameters","parameter":[{"name":"operation","part":[{"name":"type","valueCode":"replace"},{"name":"path","valueString":"Patient.name[0].family"},{"name":"value","valueString":"WireProtoFHIRPatched"}]}]}`
	reqPatchConv, pConvErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Parameters/$convert", strings.NewReader(patchParamsJSON))
	require.NoError(t, pConvErr)
	reqPatchConv.Header.Set("Content-Type", "application/fhir+json")
	reqPatchConv.Header.Set("Accept", "application/x-protobuf")
	respPatchConv, pConvErr := client.Do(reqPatchConv)
	require.NoError(t, pConvErr)
	patchProtoBytes, _ := io.ReadAll(respPatchConv.Body)
	respPatchConv.Body.Close()
	require.Equal(t, http.StatusOK, respPatchConv.StatusCode)

	reqPatchP, patchPErr := http.NewRequest(http.MethodPatch, ts.URL+"/fhir/r4/proto/Patient/"+patID, bytes.NewReader(patchProtoBytes))
	require.NoError(t, patchPErr)
	reqPatchP.Header.Set("Content-Type", "application/x-protobuf")
	reqPatchP.Header.Set("Accept", "application/x-protobuf")
	respPatchP, patchPErr := client.Do(reqPatchP)
	require.NoError(t, patchPErr)
	patchedPBytes, _ := io.ReadAll(respPatchP.Body)
	respPatchP.Body.Close()
	require.Equal(t, http.StatusOK, respPatchP.StatusCode)
	var patchedPatProto84 patpb.Patient
	require.NoError(t, proto.Unmarshal(patchedPBytes, &patchedPatProto84))
	assert.Equal(t, "WireProtoFHIRPatched", patchedPatProto84.GetName()[0].GetFamily().GetValue())

	// Binary Proto3 Merge Patch (Patient proto directly)
	mergePatProto := &patpb.Patient{
		Active: &dtpb.Boolean{Value: true},
	}
	mergePatBytes, mErr := proto.Marshal(mergePatProto)
	require.NoError(t, mErr)
	reqMergeP, mergePErr := http.NewRequest(http.MethodPatch, ts.URL+"/fhir/r4/proto/Patient/"+patID, bytes.NewReader(mergePatBytes))
	require.NoError(t, mergePErr)
	reqMergeP.Header.Set("Content-Type", "application/x-protobuf")
	reqMergeP.Header.Set("Accept", "application/x-protobuf")
	respMergeP, mergePErr := client.Do(reqMergeP)
	require.NoError(t, mergePErr)
	mergedPBytes, _ := io.ReadAll(respMergeP.Body)
	respMergeP.Body.Close()
	require.Equal(t, http.StatusOK, respMergeP.StatusCode)
	var mergedPat patpb.Patient
	require.NoError(t, proto.Unmarshal(mergedPBytes, &mergedPat))
	assert.True(t, mergedPat.GetActive().GetValue())

	// Binary Proto3 DELETE with Prefer: return=OperationOutcome
	delPatProto := &patpb.Patient{
		Id:     &dtpb.Id{Value: "proto-del-oo-1"},
		Active: &dtpb.Boolean{Value: true},
	}
	delPatBytes, _ := proto.Marshal(delPatProto)
	reqPutDelP, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/proto/Patient/proto-del-oo-1", bytes.NewReader(delPatBytes))
	reqPutDelP.Header.Set("Content-Type", "application/x-protobuf")
	respPutDelP, errPutDelP := client.Do(reqPutDelP)
	require.NoError(t, errPutDelP)
	respPutDelP.Body.Close()

	reqDelOOP, delOOPErr := http.NewRequest(http.MethodDelete, ts.URL+"/fhir/r4/proto/Patient/proto-del-oo-1", nil)
	require.NoError(t, delOOPErr)
	reqDelOOP.Header.Set("Accept", "application/x-protobuf")
	reqDelOOP.Header.Set("Prefer", "return=OperationOutcome")
	respDelOOP, delOOPErr := client.Do(reqDelOOP)
	require.NoError(t, delOOPErr)
	delOOPBytes, _ := io.ReadAll(respDelOOP.Body)
	respDelOOP.Body.Close()
	require.Equal(t, http.StatusOK, respDelOOP.StatusCode)
	var delOO oopb.OperationOutcome
	require.NoError(t, proto.Unmarshal(delOOPBytes, &delOO))
	assert.NotEmpty(t, delOO.GetIssue())

	// Polymorphic .ofType(Type), .as(Type), .is(Type), collection numeric aggregations (.sum, .avg, .min, .max),
	// and scalar boolean string predicates in select column projections
	polyAggVD := `{
		"resourceType": "ViewDefinition",
		"name": "obs_poly_agg",
		"status": "active",
		"resource": "Observation",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "val_qty", "path": "value.ofType(Quantity).value"},
					{"name": "val_unit", "path": "value.as(Quantity).unit"},
					{"name": "eff_dt", "path": "effective.ofType(DateTime)"},
					{"name": "is_qty", "path": "value.is(Quantity)"},
					{"name": "sum_val", "path": "component.value.ofType(Quantity).value.sum()"},
					{"name": "avg_val", "path": "component.value.ofType(Quantity).value.avg()"},
					{"name": "min_val", "path": "component.value.ofType(Quantity).value.min()"},
					{"name": "max_val", "path": "component.value.ofType(Quantity).value.max()"},
					{"name": "starts_hr", "path": "code.coding.code.startsWith('88')"}
				]
			}
		],
		"where": [
			{"path": "value.is(Quantity) and value.ofType(Quantity).value >= 60 and component.value.ofType(Quantity).value.max() >= 60"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"r.value_quantity AS val_qty",
				"r.value_unit_code AS val_unit",
				"r.effective_start AS eff_dt",
				"r.value_quantity IS NOT NULL AS is_qty",
				"ARRAY_SUM(",
				"ARRAY_AVG(",
				"ARRAY_MIN(",
				"ARRAY_MAX(",
				"r.code LIKE '88%' AS starts_hr",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"SELECT SUM(q.sp_value) FROM spidx_quantity q",
				"SELECT AVG(q.sp_value) FROM spidx_quantity q",
				"SELECT MIN(q.sp_value) FROM spidx_quantity q",
				"SELECT MAX(q.sp_value) FROM spidx_quantity q",
				"LIKE '88%'",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"SELECT SUM(q.sp_value) FROM spidx_quantity q",
				"SELECT AVG(q.sp_value) FROM spidx_quantity q",
				"SELECT MIN(q.sp_value) FROM spidx_quantity q",
				"SELECT MAX(q.sp_value) FROM spidx_quantity q",
				"LIKE '88%'",
			},
		},
	} {
		reqPA, paErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(polyAggVD))
		require.NoError(t, paErr)
		reqPA.Header.Set("Content-Type", "application/fhir+json")
		respPA, paErr := client.Do(reqPA)
		require.NoError(t, paErr)
		paBytes, _ := io.ReadAll(respPA.Body)
		respPA.Body.Close()
		require.Equal(t, http.StatusOK, respPA.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(paBytes), want)
		}
	}

	// Collection .where(system = '...')/.select(...) and type conversion (.toBoolean, .convertsTo*) in ViewDefinition/$sql
	whereSelectConvVD := `{
		"resourceType": "ViewDefinition",
		"name": "pat_where_select_conv",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "mrn", "path": "identifier.where(system = 'http://hospital.example.org/mrn').value.first()"},
					{"name": "all_mrns", "path": "identifier.where(system = 'http://hospital.example.org/mrn').value", "collection": true},
					{"name": "mrn_count", "path": "identifier.where(system = 'http://hospital.example.org/mrn').count()"},
					{"name": "official_family", "path": "name.where(use = 'official').select(family).first()"},
					{"name": "given_csv", "path": "name.select(given).join(',')"},
					{"name": "is_active", "path": "active.toBoolean()"},
					{"name": "has_dob", "path": "birthDate.convertsToDate()"},
					{"name": "has_int", "path": "gender.convertsToInteger()"}
				]
			}
		],
		"where": [
			{"path": "identifier.where(system = 'http://hospital.example.org/mrn').value = 'MRN-100' and identifier.where(system = 'http://hospital.example.org/mrn').exists() and birthDate.convertsToDate()"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"identifier=http://hospital.example.org/mrn|%",
				"identifier=http://hospital.example.org/mrn|MRN-100",
				"AS BOOLEAN) AS is_active",
				"AS DATE) IS NOT NULL) AS has_dob",
				"AS BIGINT) IS NOT NULL) AS has_int",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"t.sp_name = 'identifier' AND t.sp_system = 'http://hospital.example.org/mrn'",
				"t.sp_value = 'MRN-100'",
				"AS BOOLEAN) AS is_active",
				"IS NOT NULL) AS has_dob",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"t.sp_name = 'identifier' AND t.sp_system = 'http://hospital.example.org/mrn'",
				"t.sp_value = 'MRN-100'",
				"IN ('true', '1')) AS is_active",
				"IS NOT NULL) AS has_dob",
			},
		},
	} {
		reqWSC, wscErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(whereSelectConvVD))
		require.NoError(t, wscErr)
		reqWSC.Header.Set("Content-Type", "application/fhir+json")
		respWSC, wscErr := client.Do(reqWSC)
		require.NoError(t, wscErr)
		wscBytes, _ := io.ReadAll(respWSC.Body)
		respWSC.Body.Close()
		require.Equal(t, http.StatusOK, respWSC.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(wscBytes), want)
		}
	}

	// Equivalence (~, !~), boolean logic (xor, implies), membership (in) & .toChars() in ViewDefinition/$sql
	equivInXorVD := `{
		"resourceType": "ViewDefinition",
		"name": "obs_equiv_in_xor",
		"status": "active",
		"resource": "Observation",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id"},
					{"name": "code_chars", "path": "code.coding.code.first().toChars()", "collection": true},
					{"name": "is_final_or_amended", "path": "status in ('final' | 'amended')"},
					{"name": "code_equiv", "path": "code.coding.code.first() ~ '8867-4'"},
					{"name": "code_not_equiv", "path": "code.coding.code.first() !~ '9999-9'"},
					{"name": "exclusive_flag", "path": "(status = 'final') xor (status = 'cancelled')"},
					{"name": "implies_flag", "path": "(status = 'final') implies valueQuantity.exists()"}
				]
			}
		],
		"where": [
			{"path": "status in ('final' | 'amended') and code.coding.code.first() ~ '8867-4' and ((status = 'final') implies valueQuantity.exists()) and ((status = 'final') xor (status = 'cancelled'))"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"SPLIT_BY_STRING(r.code, '') AS code_chars",
				"IN ('final', 'amended')",
				"LOWER(TRIM(r.code)) = LOWER(TRIM('8867-4'))",
				"LOWER(TRIM(r.code)) <> LOWER(TRIM('9999-9'))",
				"<>",
				"(NOT (",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"REGEXP_SPLIT_TO_ARRAY(",
				"IN ('final', 'amended')",
				"LOWER(TRIM(",
				"<> LOWER(TRIM('9999-9'))",
				"(NOT (",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"json_array(",
				"IN ('final', 'amended')",
				"LOWER(TRIM(",
				"(NOT (",
			},
		},
	} {
		reqEX, exErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(equivInXorVD))
		require.NoError(t, exErr)
		reqEX.Header.Set("Content-Type", "application/fhir+json")
		respEX, exErr := client.Do(reqEX)
		require.NoError(t, exErr)
		exBytes, _ := io.ReadAll(respEX.Body)
		respEX.Body.Close()
		require.Equal(t, http.StatusOK, respEX.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(exBytes), want)
		}
	}

	// 32. Wave 87: Binary Proto3 SDOH ($sdoh-referral, $sdoh-summary), Consent/$evaluate, CARIN-BB ($carin-bb, $carin-bb-summary),
	// Public Health eCR ($ecr-report, $ersd-lookup), VRDR ($decease), mCODE Oncology ($stage-cancer, $mcode-summary),
	// Da Vinci Drug Formulary ($formulary-lookup, $formulary-coverage) & SQL-on-FHIR v2 [n] subscript, .skip/.take, contains, .toTime/.convertsToTime & .trace
	sdohSRRaw := `{"resourceType":"ServiceRequest","id":"sr-sdoh-proto-1","status":"active","intent":"order","category":[{"coding":[{"system":"http://hl7.org/fhir/us/sdoh-clinicalcare/CodeSystem/SDOHCC-CodeSystemTemporaryCodes","code":"food-insecurity"}]}],"code":{"coding":[{"system":"http://snomed.info/sct","code":"467691000124102"}]},"subject":{"reference":"Patient/` + patID + `"}}`
	reqSDOHConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ServiceRequest/$convert", strings.NewReader(sdohSRRaw))
	require.NoError(t, err)
	reqSDOHConv.Header.Set("Content-Type", "application/fhir+json")
	reqSDOHConv.Header.Set("Accept", "application/x-protobuf")
	respSDOHConv, err := client.Do(reqSDOHConv)
	require.NoError(t, err)
	bSDOHInProto, _ := io.ReadAll(respSDOHConv.Body)
	respSDOHConv.Body.Close()
	require.Equal(t, http.StatusOK, respSDOHConv.StatusCode)

	reqSDOHRef, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ServiceRequest/$sdoh-referral", bytes.NewReader(bSDOHInProto))
	require.NoError(t, err)
	reqSDOHRef.Header.Set("Content-Type", "application/x-protobuf")
	reqSDOHRef.Header.Set("Accept", "application/x-protobuf")
	respSDOHRef, err := client.Do(reqSDOHRef)
	require.NoError(t, err)
	bSDOHRef, _ := io.ReadAll(respSDOHRef.Body)
	respSDOHRef.Body.Close()
	require.Equal(t, http.StatusOK, respSDOHRef.StatusCode)
	var sdohParams parampb.Parameters
	require.NoError(t, proto.Unmarshal(bSDOHRef, &sdohParams))
	assert.NotEmpty(t, sdohParams.GetParameter())

	// Seed an Encounter and Condition for eCR and mCODE
	reqSeedEnc, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/proto/Encounter/enc-w87-1", strings.NewReader(`{"resourceType":"Encounter","id":"enc-w87-1","status":"finished","class":{"system":"http://terminology.hl7.org/CodeSystem/v3-ActCode","code":"AMB"},"subject":{"reference":"Patient/`+patID+`"}}`))
	reqSeedEnc.Header.Set("Content-Type", "application/fhir+json")
	respSeedEnc, err := client.Do(reqSeedEnc)
	require.NoError(t, err)
	respSeedEnc.Body.Close()

	reqSeedCond, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/proto/Condition/cond-w87-1", strings.NewReader(`{"resourceType":"Condition","id":"cond-w87-1","clinicalStatus":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/condition-clinical","code":"active"}]},"code":{"coding":[{"system":"http://snomed.info/sct","code":"363346000","display":"Malignant neoplastic disease"}]},"subject":{"reference":"Patient/`+patID+`"}}`))
	reqSeedCond.Header.Set("Content-Type", "application/fhir+json")
	respSeedCond, err := client.Do(reqSeedCond)
	require.NoError(t, err)
	respSeedCond.Body.Close()

	w87ParamsProto := &parampb.Parameters{
		Parameter: []*parampb.Parameters_Parameter{
			{
				Name:  &dtpb.String{Value: "patient"},
				Value: &parampb.Parameters_Parameter_ValueX{Choice: &parampb.Parameters_Parameter_ValueX_Reference{Reference: &dtpb.Reference{Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "Patient/" + patID}}}}},
			},
			{
				Name:  &dtpb.String{Value: "encounter"},
				Value: &parampb.Parameters_Parameter_ValueX{Choice: &parampb.Parameters_Parameter_ValueX_Reference{Reference: &dtpb.Reference{Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "Encounter/enc-w87-1"}}}}},
			},
			{
				Name:  &dtpb.String{Value: "condition"},
				Value: &parampb.Parameters_Parameter_ValueX{Choice: &parampb.Parameters_Parameter_ValueX_Reference{Reference: &dtpb.Reference{Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "Condition/cond-w87-1"}}}}},
			},
			{
				Name:  &dtpb.String{Value: "purpose"},
				Value: &parampb.Parameters_Parameter_ValueX{Choice: &parampb.Parameters_Parameter_ValueX_StringValue{StringValue: &dtpb.String{Value: "TREAT"}}},
			},
			{
				Name:  &dtpb.String{Value: "tStage"},
				Value: &parampb.Parameters_Parameter_ValueX{Choice: &parampb.Parameters_Parameter_ValueX_StringValue{StringValue: &dtpb.String{Value: "T2"}}},
			},
			{
				Name:  &dtpb.String{Value: "nStage"},
				Value: &parampb.Parameters_Parameter_ValueX{Choice: &parampb.Parameters_Parameter_ValueX_StringValue{StringValue: &dtpb.String{Value: "N1"}}},
			},
			{
				Name:  &dtpb.String{Value: "mStage"},
				Value: &parampb.Parameters_Parameter_ValueX{Choice: &parampb.Parameters_Parameter_ValueX_StringValue{StringValue: &dtpb.String{Value: "M0"}}},
			},
			{
				Name:  &dtpb.String{Value: "stageGroup"},
				Value: &parampb.Parameters_Parameter_ValueX{Choice: &parampb.Parameters_Parameter_ValueX_StringValue{StringValue: &dtpb.String{Value: "Stage IIB"}}},
			},
			{
				Name:  &dtpb.String{Value: "code"},
				Value: &parampb.Parameters_Parameter_ValueX{Choice: &parampb.Parameters_Parameter_ValueX_StringValue{StringValue: &dtpb.String{Value: "1049502"}}},
			},
			{
				Name:  &dtpb.String{Value: "planId"},
				Value: &parampb.Parameters_Parameter_ValueX{Choice: &parampb.Parameters_Parameter_ValueX_StringValue{StringValue: &dtpb.String{Value: "plan-gold-1"}}},
			},
		},
	}
	w87ParamsBytes, err := proto.Marshal(w87ParamsProto)
	require.NoError(t, err)

	for _, ep := range []string{
		"/fhir/r4/proto/Patient/$sdoh-summary",
		"/fhir/r4/proto/Consent/$evaluate",
		"/fhir/r4/proto/Patient/$carin-bb",
		"/fhir/r4/proto/ExplanationOfBenefit/$carin-bb-summary",
		"/fhir/r4/proto/Encounter/$ecr-report",
		"/fhir/r4/proto/Condition/$stage-cancer",
		"/fhir/r4/proto/Patient/$mcode-summary",
		"/fhir/r4/proto/MedicationKnowledge/$formulary-lookup",
		"/fhir/r4/proto/Coverage/$formulary-coverage",
		"/fhir/r4/proto/Patient/$decease",
	} {
		reqOp, opErr := http.NewRequest(http.MethodPost, ts.URL+ep, bytes.NewReader(w87ParamsBytes))
		require.NoError(t, opErr)
		reqOp.Header.Set("Content-Type", "application/x-protobuf")
		reqOp.Header.Set("Accept", "application/x-protobuf")
		respOp, opErr := client.Do(reqOp)
		require.NoError(t, opErr)
		opBytes, _ := io.ReadAll(respOp.Body)
		respOp.Body.Close()
		require.Equalf(t, http.StatusOK, respOp.StatusCode, "POST %s failed", ep)
		assert.Contains(t, respOp.Header.Get("Content-Type"), "application/x-protobuf")
		assert.Equal(t, "google.fhir.r4.proto3", respOp.Header.Get("X-Flint-Wire-Format"))
		assert.NotEmpty(t, opBytes)
	}

	subContainsTimeVD := `{
		"resourceType": "ViewDefinition",
		"name": "pat_sub_contains_time",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id", "path": "id.trace('id-dbg')"},
					{"name": "first_given", "path": "name.given[0]"},
					{"name": "second_given", "path": "name.given[1]"},
					{"name": "tail_given_csv", "path": "name.given.skip(1).join(',')"},
					{"name": "tail_given_arr", "path": "name.given.skip(1)", "collection": true},
					{"name": "top2_given_arr", "path": "name.given.take(2)", "collection": true},
					{"name": "is_target_gender", "path": "('female' | 'other') contains gender"},
					{"name": "birth_time", "path": "birthDate.toTime()"},
					{"name": "has_birth_time", "path": "birthDate.convertsToTime()"}
				]
			}
		],
		"where": [
			{"path": "('female' | 'other') contains gender.trace('gender-dbg') and name.given[1] = 'Marie' and birthDate.convertsToTime()"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"element_at(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)), 2) AS second_given",
				"IN ('female', 'other')",
				"AS TIME)",
				"= 'Marie'",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"LIMIT 1 OFFSET 1) AS second_given",
				"OFFSET 1) sub) AS tail_given_csv",
				"OFFSET 1) sub) AS tail_given_arr",
				"LIMIT 2) sub) AS top2_given_arr",
				"IN ('female', 'other')",
				"AS TIME)",
				"= 'Marie'",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"LIMIT 1 OFFSET 1) AS second_given",
				"LIMIT -1 OFFSET 1) sub) AS tail_given_csv",
				"LIMIT -1 OFFSET 1) sub) AS tail_given_arr",
				"LIMIT 2) sub) AS top2_given_arr",
				"IN ('female', 'other')",
				"TIME(",
				"= 'Marie'",
			},
		},
	} {
		reqSCT, sctErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(subContainsTimeVD))
		require.NoError(t, sctErr)
		reqSCT.Header.Set("Content-Type", "application/fhir+json")
		respSCT, sctErr := client.Do(reqSCT)
		require.NoError(t, sctErr)
		sctBytes, _ := io.ReadAll(respSCT.Body)
		respSCT.Body.Close()
		require.Equal(t, http.StatusOK, respSCT.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(sctBytes), want)
		}
	}

	// 33. Wave 88: Binary Proto3 IHE MHD ($iti-65, $iti-67), Clinical Genomics ($find-subject-variants, $find-subject-tx-implications), PACIO ($pacio-summary, $pacio-assess), CDC NHSN HAI ($nhsn-hai-report, $nhsn-acute-care), Vulcan Research ($rwd-cohort, $enroll), Vital Records BFDR ($birth-report, $bfdr-summary), IHE PDQm/ITI-68 ($ihe-pdqm, $ihe-iti-68), FAST NDH ($fast-endpoint-query, $fast-verify) & SQL-on-FHIR v2 | / .not() / .encode / .decode $sql Pushdown
	mhdBundleJSON := `{"resourceType":"Bundle","type":"transaction","entry":[{"resource":{"resourceType":"DocumentReference","id":"w88-mhd-doc-1","status":"current","subject":{"reference":"Patient/` + patID + `"}}}]}`
	reqMHDConv, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/Bundle/$convert", strings.NewReader(mhdBundleJSON))
	require.NoError(t, err)
	reqMHDConv.Header.Set("Content-Type", "application/fhir+json")
	reqMHDConv.Header.Set("Accept", "application/x-protobuf")
	respMHDConv, err := client.Do(reqMHDConv)
	require.NoError(t, err)
	mhdBundleProtoBytes, _ := io.ReadAll(respMHDConv.Body)
	respMHDConv.Body.Close()
	require.Equal(t, http.StatusOK, respMHDConv.StatusCode)
	require.NotEmpty(t, mhdBundleProtoBytes)

	reqITI65, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/$iti-65", bytes.NewReader(mhdBundleProtoBytes))
	require.NoError(t, err)
	reqITI65.Header.Set("Content-Type", "application/x-protobuf")
	reqITI65.Header.Set("Accept", "application/x-protobuf")
	respITI65, err := client.Do(reqITI65)
	require.NoError(t, err)
	iti65OutBytes, _ := io.ReadAll(respITI65.Body)
	respITI65.Body.Close()
	require.Equal(t, http.StatusOK, respITI65.StatusCode)
	assert.Contains(t, respITI65.Header.Get("Content-Type"), "application/x-protobuf")
	require.NotEmpty(t, iti65OutBytes)

	w88ParamsPb := &parampb.Parameters{
		Parameter: []*parampb.Parameters_Parameter{
			{
				Name: &dtpb.String{Value: "patient"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "Patient/" + patID}},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "encounter"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "Encounter/enc-w87-1"}},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "documentReference"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "DocumentReference/w88-mhd-doc-1"}},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "organization"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "Organization/w88-org-1"}},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "study"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "ResearchStudy/w88-study-1"}},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "genes"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "BRCA1"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "treatments"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "Olaparib"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "family"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "Proto"},
					},
				},
			},
		},
	}
	w88ParamsBytes, err := proto.Marshal(w88ParamsPb)
	require.NoError(t, err)

	for _, ep := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/fhir/r4/proto/DocumentReference/$iti-67"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$find-subject-variants"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$find-subject-tx-implications"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$pacio-summary"},
		{http.MethodPost, "/fhir/r4/proto/QuestionnaireResponse/$pacio-assess"},
		{http.MethodPost, "/fhir/r4/proto/Encounter/$nhsn-hai-report"},
		{http.MethodGet, "/fhir/r4/proto/Measure/$nhsn-acute-care?_format=application/x-protobuf"},
		{http.MethodPost, "/fhir/r4/proto/ResearchStudy/$rwd-cohort"},
		{http.MethodPost, "/fhir/r4/proto/ResearchSubject/$enroll"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$birth-report"},
		{http.MethodPost, "/fhir/r4/proto/Composition/$bfdr-summary"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$ihe-pdqm"},
		{http.MethodPost, "/fhir/r4/proto/DocumentReference/$ihe-iti-68"},
		{http.MethodPost, "/fhir/r4/proto/Endpoint/$fast-endpoint-query"},
		{http.MethodPost, "/fhir/r4/proto/Organization/$fast-verify"},
	} {
		var bodyReader io.Reader
		if ep.method == http.MethodPost {
			bodyReader = bytes.NewReader(w88ParamsBytes)
		}
		reqOp, opErr := http.NewRequest(ep.method, ts.URL+ep.path, bodyReader)
		require.NoError(t, opErr)
		if ep.method == http.MethodPost {
			reqOp.Header.Set("Content-Type", "application/x-protobuf")
		}
		reqOp.Header.Set("Accept", "application/x-protobuf")
		respOp, opErr := client.Do(reqOp)
		require.NoError(t, opErr)
		opBytes, _ := io.ReadAll(respOp.Body)
		respOp.Body.Close()
		require.Equal(t, http.StatusOK, respOp.StatusCode, "endpoint %s failed: %s", ep.path, string(opBytes))
		assert.Contains(t, respOp.Header.Get("Content-Type"), "application/x-protobuf", "endpoint %s Content-Type", ep.path)
		assert.Equal(t, "google.fhir.r4.proto3", respOp.Header.Get("X-Flint-Wire-Format"), "endpoint %s X-Flint-Wire-Format", ep.path)
		require.NotEmpty(t, opBytes, "endpoint %s empty body", ep.path)
	}

	unionNotEncodeVD := `{
		"resourceType": "ViewDefinition",
		"name": "patient_union_not_encode_v1",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "id_b64", "path": "id.encode('base64')"},
					{"name": "id_roundtrip", "path": "id.encode('base64').decode('base64')"},
					{"name": "id_hex", "path": "id.encode('hex')"},
					{"name": "id_hex_rt", "path": "id.encode('hex').decode('hex')"},
					{"name": "all_names", "path": "name.family | name.given", "collection": true},
					{"name": "all_names_cnt", "path": "(name.family | name.given).count()"},
					{"name": "all_names_csv", "path": "(name.family | name.given).join(',')"},
					{"name": "is_inactive", "path": "active.not()"},
					{"name": "not_other_gender", "path": "not(gender = 'other')"}
				]
			}
		],
		"where": [
			{"path": "active.not().not() and id.encode('hex') != ''"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"TO_BASE64(r.res_id) AS id_b64",
				"FROM_BASE64(TO_BASE64(r.res_id)) AS id_roundtrip",
				"HEX(r.res_id) AS id_hex",
				"UNHEX(HEX(r.res_id)) AS id_hex_rt",
				"ARRAY_UNION(",
				"ARRAY_SIZE(ARRAY_UNION(",
				"ARRAY_JOIN(ARRAY_UNION(",
				"NOT (array_contains(r.code_tokens, 'active=|true')) AS is_inactive",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"ENCODE(CAST(r.res_id AS BYTEA), 'base64') AS id_b64",
				"CONVERT_FROM(DECODE(ENCODE(CAST(r.res_id AS BYTEA), 'base64'), 'base64'), 'UTF8') AS id_roundtrip",
				"ENCODE(CAST(r.res_id AS BYTEA), 'hex') AS id_hex",
				"CONVERT_FROM(DECODE(ENCODE(CAST(r.res_id AS BYTEA), 'hex'), 'hex'), 'UTF8') AS id_hex_rt",
				"UNION SELECT s.sp_value FROM spidx_string",
				"NOT (EXISTS (SELECT 1 FROM spidx_token t",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"HEX(r.res_id) AS id_b64",
				"CAST(UNHEX(HEX(r.res_id)) AS TEXT) AS id_roundtrip",
				"UNION SELECT s.sp_value FROM spidx_string",
				"NOT (EXISTS (SELECT 1 FROM spidx_token t",
			},
		},
	} {
		reqUNE, uneErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(unionNotEncodeVD))
		require.NoError(t, uneErr)
		reqUNE.Header.Set("Content-Type", "application/fhir+json")
		respUNE, uneErr := client.Do(reqUNE)
		require.NoError(t, uneErr)
		uneBytes, _ := io.ReadAll(respUNE.Body)
		respUNE.Body.Close()
		require.Equal(t, http.StatusOK, respUNE.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(uneBytes), want)
		}
	}

	// 34. Wave 89: Binary Proto3 Physical Activity ($physical-activity-summary, $physical-activity-referral), Vulcan ePI ($epi-document, $evaluate-authorization), Public Health ELR ($elr-submit, $reportable-labs), PACIO ADI ($adi-record, $adi-document), Da Vinci Risk Adjustment ($ra-coding-gaps, $ra-remediate), Public Health eICR/RR ($eicr-document, $rr-process), Da Vinci PDex/Formulary ($pdex-WriteData, $formulary-alternatives), HL7 ODH ($record-occupation, $odh-summary), Public Health CCRR/MedMorph ($cancer-registry-report, $medmorph-kar), Da Vinci VBPR ($vbpr-performance, $vbpr-attest), PACIO Cognitive Status ($pacio-cognitive-assess, $pacio-cognitive-summary) & SQL-on-FHIR v2 .hasValue() / .aggregate() / .repeat() / .children() / .descendants() / .lowBoundary() / .highBoundary() $sql Pushdown
	w89ParamsPb := &parampb.Parameters{
		Parameter: []*parampb.Parameters_Parameter{
			{
				Name: &dtpb.String{Value: "patient"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "Patient/" + patID}},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "subject"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "Patient/" + patID}},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "encounter"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "Encounter/enc-w87-1"}},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "measureReport"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "MeasureReport/mr-w89-1"}},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "organization"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{Uri: &dtpb.String{Value: "Organization/w88-org-1"}},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "medicinalProduct"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "mp-w89-1"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "authorization"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "mpa-w89-1"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "diagnosticReport"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "dr-w89-1"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "medicationKnowledge"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "mk-w89-1"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "planDefinition"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "pd-w89-1"},
					},
				},
			},
		},
	}
	w89ParamsBytes, err := proto.Marshal(w89ParamsPb)
	require.NoError(t, err)

	for _, ep := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/fhir/r4/proto/Patient/$physical-activity-summary"},
		{http.MethodPost, "/fhir/r4/proto/ServiceRequest/$physical-activity-referral"},
		{http.MethodPost, "/fhir/r4/proto/MedicinalProduct/$epi-document"},
		{http.MethodPost, "/fhir/r4/proto/MedicinalProductAuthorization/$evaluate-authorization"},
		{http.MethodPost, "/fhir/r4/proto/DiagnosticReport/$elr-submit"},
		{http.MethodPost, "/fhir/r4/proto/Observation/$reportable-labs"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$adi-record"},
		{http.MethodPost, "/fhir/r4/proto/Composition/$adi-document"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$ra-coding-gaps"},
		{http.MethodPost, "/fhir/r4/proto/MeasureReport/$ra-remediate"},
		{http.MethodPost, "/fhir/r4/proto/Composition/$eicr-document"},
		{http.MethodPost, "/fhir/r4/proto/Communication/$rr-process"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$pdex-WriteData"},
		{http.MethodPost, "/fhir/r4/proto/MedicationKnowledge/$formulary-alternatives"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$record-occupation"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$odh-summary"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$cancer-registry-report"},
		{http.MethodPost, "/fhir/r4/proto/PlanDefinition/$medmorph-kar"},
		{http.MethodPost, "/fhir/r4/proto/MeasureReport/$vbpr-performance"},
		{http.MethodPost, "/fhir/r4/proto/MeasureReport/$vbpr-attest"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$pacio-cognitive-assess"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$pacio-cognitive-summary"},
	} {
		reqOp, opErr := http.NewRequest(ep.method, ts.URL+ep.path, bytes.NewReader(w89ParamsBytes))
		require.NoError(t, opErr)
		reqOp.Header.Set("Content-Type", "application/x-protobuf")
		reqOp.Header.Set("Accept", "application/x-protobuf")
		respOp, opErr := client.Do(reqOp)
		require.NoError(t, opErr)
		opBytes, _ := io.ReadAll(respOp.Body)
		respOp.Body.Close()
		require.Equal(t, http.StatusOK, respOp.StatusCode, "endpoint %s failed: %s", ep.path, string(opBytes))
		assert.Contains(t, respOp.Header.Get("Content-Type"), "application/x-protobuf", "endpoint %s Content-Type", ep.path)
		assert.Equal(t, "google.fhir.r4.proto3", respOp.Header.Get("X-Flint-Wire-Format"), "endpoint %s X-Flint-Wire-Format", ep.path)
		require.NotEmpty(t, opBytes, "endpoint %s empty body", ep.path)
	}

	hasValueAggBoundaryVD := `{
		"resourceType": "ViewDefinition",
		"name": "patient_hasvalue_agg_boundary_v1",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "has_family", "path": "name.family.first().hasValue()"},
					{"name": "given_len_sum", "path": "name.given.aggregate($total + $this.length(), 0)"},
					{"name": "given_agg_cnt", "path": "name.given.aggregate($total + 1, 0)"},
					{"name": "repeat_given_cnt", "path": "name.repeat(given).count()"},
					{"name": "child_id_has_val", "path": "children().id.hasValue()"},
					{"name": "desc_family", "path": "descendants().name.family.first()"},
					{"name": "birth_low", "path": "birthDate.lowBoundary()"},
					{"name": "birth_high", "path": "birthDate.highBoundary()"}
				]
			}
		],
		"where": [
			{"path": "name.family.first().hasValue() and birthDate.lowBoundary() >= '1900-01-01' and name.given.aggregate($total + 1, 0) >= 1"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"IS NOT NULL AND CAST(",
				"AS STRING) != '') AS has_family",
				"COALESCE(ARRAY_SUM(ARRAY_MAP(x -> CHAR_LENGTH(x),",
				"AS given_len_sum",
				"ARRAY_SIZE(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens))) AS given_agg_cnt",
				"ARRAY_SIZE(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens))) AS repeat_given_cnt",
				"(r.res_id IS NOT NULL AND CAST(r.res_id AS STRING) != '') AS child_id_has_val",
				"CAST(r.effective_start AS DATE) AS birth_low",
				"LAST_DAY(CAST(r.effective_start AS DATE)) AS birth_high",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"IS NOT NULL AND CAST(",
				"AS TEXT) != '') AS has_family",
				"COALESCE((SELECT SUM(CHAR_LENGTH(sub.sp_value)) FROM (",
				"(SELECT COUNT(*) FROM spidx_string s",
				"(r.res_id IS NOT NULL AND CAST(r.res_id AS TEXT) != '') AS child_id_has_val",
				"CAST((SELECT d.sp_low FROM spidx_date d",
				"DATE_TRUNC('month'",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"IS NOT NULL AND CAST(",
				"AS TEXT) != '') AS has_family",
				"COALESCE((SELECT SUM(LENGTH(sub.sp_value)) FROM (",
				"(SELECT COUNT(*) FROM spidx_string s",
				"(r.res_id IS NOT NULL AND CAST(r.res_id AS TEXT) != '') AS child_id_has_val",
				"DATE((SELECT d.sp_low FROM spidx_date d",
				"'start of month', '+1 month', '-1 day') AS birth_high",
			},
		},
	} {
		reqHAB, habErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(hasValueAggBoundaryVD))
		require.NoError(t, habErr)
		reqHAB.Header.Set("Content-Type", "application/fhir+json")
		respHAB, habErr := client.Do(reqHAB)
		require.NoError(t, habErr)
		habBytes, _ := io.ReadAll(respHAB.Body)
		respHAB.Body.Close()
		require.Equal(t, http.StatusOK, respHAB.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(habBytes), want)
		}
	}

	// 35. Wave 90: Binary Proto3 CARIN RTPBC, FAST Digital Identity, PACIO RT/PFE, Public Health VRDR/BFDR/IIS, Da Vinci CDex, CodeX/mCODE Radiotherapy & .precision(), .escape()/.unescape() & .comparable() $sql Pushdown
	w90ParamsPb := &parampb.Parameters{
		Parameter: []*parampb.Parameters_Parameter{
			{
				Name: &dtpb.String{Value: "patient"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Patient/" + patID},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "subject"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Patient/" + patID},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "person"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Person/pers-w90-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "encounter"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Encounter/enc-w90-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "mother"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Patient/" + patID},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "signer"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Practitioner/prac-w90-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "code"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "1049502"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "causeOfDeathCode"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "I21.9"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "query"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "Condition?patient=Patient/" + patID},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "icfCode"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "d450"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "goal"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "Walk 150 feet independently"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "TrackingId"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "cdex-track-w90-1"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "questionnaire"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "http://hl7.org/fhir/us/davinci-cdex/Questionnaire/cdex-questionnaire-example1"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "cvx"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "208"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "bundleId"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "cdex-signed-bundle-w90-1"},
					},
				},
			},
		},
	}
	w90ParamsBytes, err := proto.Marshal(w90ParamsPb)
	require.NoError(t, err)

	for _, ep := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/fhir/r4/proto/Claim/$rtpbc-submit"},
		{http.MethodPost, "/fhir/r4/proto/ClaimResponse/$rtpbc-coverage"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$idi-match"},
		{http.MethodPost, "/fhir/r4/proto/Person/$verify-identity"},
		{http.MethodPost, "/fhir/r4/proto/Encounter/$pacio-timepoint"},
		{http.MethodPost, "/fhir/r4/proto/Encounter/$pacio-timepoints"},
		{http.MethodPost, "/fhir/r4/proto/Composition/$vrdr-document"},
		{http.MethodPost, "/fhir/r4/proto/Bundle/$vrdr-submit"},
		{http.MethodPost, "/fhir/r4/proto/Task/$cdex-request"},
		{http.MethodPost, "/fhir/r4/proto/Task/$cdex-poll"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$pacio-pfe-record"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$pacio-pfe-summary"},
		{http.MethodPost, "/fhir/r4/proto/Composition/$bfdr-document"},
		{http.MethodPost, "/fhir/r4/proto/Bundle/$bfdr-submit"},
		{http.MethodPost, "/fhir/r4/proto/Communication/$cdex-submit-attachment"},
		{http.MethodPost, "/fhir/r4/proto/Task/$cdex-questionnaire-request"},
		{http.MethodPost, "/fhir/r4/proto/Procedure/$mcode-radiotherapy-record"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$mcode-radiotherapy-summary"},
		{http.MethodPost, "/fhir/r4/proto/Immunization/$iis-submit"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$iis-history"},
		{http.MethodPost, "/fhir/r4/proto/Bundle/$cdex-sign"},
		{http.MethodPost, "/fhir/r4/proto/Bundle/$cdex-verify-signature"},
	} {
		reqOp, opErr := http.NewRequest(ep.method, ts.URL+ep.path, bytes.NewReader(w90ParamsBytes))
		require.NoError(t, opErr)
		reqOp.Header.Set("Content-Type", "application/x-protobuf")
		reqOp.Header.Set("Accept", "application/x-protobuf")
		respOp, opErr := client.Do(reqOp)
		require.NoError(t, opErr)
		opBytes, _ := io.ReadAll(respOp.Body)
		respOp.Body.Close()
		require.Equal(t, http.StatusOK, respOp.StatusCode, "endpoint %s failed: %s", ep.path, string(opBytes))
		assert.Contains(t, respOp.Header.Get("Content-Type"), "application/x-protobuf", "endpoint %s Content-Type", ep.path)
		assert.Equal(t, "google.fhir.r4.proto3", respOp.Header.Get("X-Flint-Wire-Format"), "endpoint %s X-Flint-Wire-Format", ep.path)
		require.NotEmpty(t, opBytes, "endpoint %s empty body", ep.path)
	}

	precEscCompVD := `{
		"resourceType": "ViewDefinition",
		"name": "obs_precision_escape_comparable_v1",
		"status": "active",
		"resource": "Observation",
		"select": [
			{
				"column": [
					{"name": "val_prec", "path": "valueQuantity.value.precision()"},
					{"name": "code_esc_html", "path": "code.coding.code.first().escape('html')"},
					{"name": "code_unesc_html", "path": "code.coding.code.first().unescape('html')"},
					{"name": "code_esc_json", "path": "code.coding.code.first().escape('json')"},
					{"name": "qty_comp", "path": "valueQuantity.comparable(valueQuantity)"}
				]
			}
		],
		"where": [
			{"path": "valueQuantity.value.precision() >= 1 and valueQuantity.comparable(valueQuantity) and code.coding.code.first().escape('html') = '8867-4'"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"CHAR_LENGTH(CAST(r.value_quantity AS STRING)) AS val_prec",
				"REPLACE(REPLACE(REPLACE(r.code, '&', '&amp;'), '<', '&lt;'), '>', '&gt;') AS code_esc_html",
				"REPLACE(REPLACE(REPLACE(r.code, '&gt;', '>'), '&lt;', '<'), '&amp;', '&') AS code_unesc_html",
				"AS code_esc_json",
				"IS NOT NULL) AS qty_comp",
				"CHAR_LENGTH(CAST(r.value_quantity AS STRING)) >= 1",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"CHAR_LENGTH(CAST((SELECT q.sp_value FROM spidx_quantity q",
				"AS TEXT)) AS val_prec",
				"REPLACE(REPLACE(REPLACE((SELECT t.sp_value FROM spidx_token t",
				"'&', '&amp;'), '<', '&lt;'), '>', '&gt;') AS code_esc_html",
				"'&gt;', '>'), '&lt;', '<'), '&amp;', '&') AS code_unesc_html",
				"IS NOT NULL) AS qty_comp",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"LENGTH(CAST((SELECT q.sp_value FROM spidx_quantity q",
				"AS TEXT)) AS val_prec",
				"REPLACE(REPLACE(REPLACE((SELECT t.sp_value FROM spidx_token t",
				"'&', '&amp;'), '<', '&lt;'), '>', '&gt;') AS code_esc_html",
				"'&gt;', '>'), '&lt;', '<'), '&amp;', '&') AS code_unesc_html",
				"IS NOT NULL) AS qty_comp",
			},
		},
	} {
		reqPEC, pecErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(precEscCompVD))
		require.NoError(t, pecErr)
		reqPEC.Header.Set("Content-Type", "application/fhir+json")
		respPEC, pecErr := client.Do(reqPEC)
		require.NoError(t, pecErr)
		pecBytes, _ := io.ReadAll(respPEC.Body)
		respPEC.Body.Close()
		require.Equal(t, http.StatusOK, respPEC.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(pecBytes), want)
		}
	}

	// 36. Wave 91: Binary Proto3 CodeX/mCODE Genomics & Trial Matching, Public Health NBS/eCR/ELR/CCRR, Da Vinci PCT, Vulcan ICSR/SOA/eSource, Gravity SDOH, CARIN BB/RTPBC, PACIO TOC/ADI, CDC NHSN HAI/AUR, FAST NDH & .toQuantity()/.convertsToQuantity(), .matchesFull(), timeOfDay(), .trace()/.check()/.defineVariable() $sql Pushdown
	w91ParamsPb := &parampb.Parameters{
		Parameter: []*parampb.Parameters_Parameter{
			{
				Name: &dtpb.String{Value: "patient"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Patient/" + patID},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "subject"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Patient/" + patID},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "study"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "ResearchStudy/study-w91-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "healthcareService"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "HealthcareService/ndh-hs-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "network"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Organization/plannet-network-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "organizationAffiliation"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "OrganizationAffiliation/ndh-orgaff-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "gene"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "BRCA1"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "result"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "Normal newborn dried blood spot and hearing screening"},
					},
				},
			},
		},
	}
	w91ParamsBytes, err := proto.Marshal(w91ParamsPb)
	require.NoError(t, err)

	for _, ep := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/fhir/r4/proto/DiagnosticReport/$mcode-genomics-report"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$mcode-genomics-summary"},
		{http.MethodPost, "/fhir/r4/proto/DiagnosticReport/$nbs-submit"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$nbs-summary"},
		{http.MethodPost, "/fhir/r4/proto/ExplanationOfBenefit/$pct-aeob-summary"},
		{http.MethodPost, "/fhir/r4/proto/Task/$pct-gfe-coordination"},
		{http.MethodPost, "/fhir/r4/proto/AdverseEvent/$vulcan-icsr-report"},
		{http.MethodPost, "/fhir/r4/proto/ResearchSubject/$vulcan-soa-timeline"},
		{http.MethodPost, "/fhir/r4/proto/Bundle/$ecr-submit"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$ecr-summary"},
		{http.MethodPost, "/fhir/r4/proto/QuestionnaireResponse/$sdoh-assess"},
		{http.MethodPost, "/fhir/r4/proto/Task/$sdoh-fulfill"},
		{http.MethodPost, "/fhir/r4/proto/ResearchSubject/$vulcan-esource-extract"},
		{http.MethodPost, "/fhir/r4/proto/ResearchStudy/$vulcan-summary"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$elr-summary"},
		{http.MethodPost, "/fhir/r4/proto/Bundle/$cancer-registry-submit"},
		{http.MethodPost, "/fhir/r4/proto/ExplanationOfBenefit/$carin-bb-claims"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$rtpbc-summary"},
		{http.MethodPost, "/fhir/r4/proto/Composition/$pacio-toc-bundle"},
		{http.MethodPost, "/fhir/r4/proto/Consent/$adi-verify"},
		{http.MethodPost, "/fhir/r4/proto/MeasureReport/$nhsn-aur-submit"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$nhsn-hai-summary"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$mcode-trial-match"},
		{http.MethodPost, "/fhir/r4/proto/Observation/$mcode-disease-status"},
		{http.MethodPost, "/fhir/r4/proto/OrganizationAffiliation/$fast-network-query"},
		{http.MethodPost, "/fhir/r4/proto/HealthcareService/$fast-attest"},
	} {
		reqOp, opErr := http.NewRequest(ep.method, ts.URL+ep.path, bytes.NewReader(w91ParamsBytes))
		require.NoError(t, opErr)
		reqOp.Header.Set("Content-Type", "application/x-protobuf")
		reqOp.Header.Set("Accept", "application/x-protobuf")
		respOp, opErr := client.Do(reqOp)
		require.NoError(t, opErr)
		opBytes, _ := io.ReadAll(respOp.Body)
		respOp.Body.Close()
		require.Equal(t, http.StatusOK, respOp.StatusCode, "endpoint %s failed: %s", ep.path, string(opBytes))
		assert.Contains(t, respOp.Header.Get("Content-Type"), "application/x-protobuf", "endpoint %s Content-Type", ep.path)
		assert.Equal(t, "google.fhir.r4.proto3", respOp.Header.Get("X-Flint-Wire-Format"), "endpoint %s X-Flint-Wire-Format", ep.path)
		require.NotEmpty(t, opBytes, "endpoint %s empty body", ep.path)
	}

	toQtyMatchesVD := `{
		"resourceType": "ViewDefinition",
		"name": "obs_toquantity_matchesfull_timeofday_v1",
		"status": "active",
		"resource": "Observation",
		"select": [
			{
				"column": [
					{"name": "qty_val", "path": "valueQuantity.toQuantity('bpm').trace('qty').check($this > 0, 'positive')"},
					{"name": "can_qty", "path": "valueQuantity.convertsToQuantity('bpm').defineVariable('v', code)"},
					{"name": "full_match", "path": "code.coding.code.first().matchesFull('8867-4')"},
					{"name": "tod_val", "path": "timeOfDay()"}
				]
			}
		],
		"where": [
			{"path": "valueQuantity.toQuantity('bpm') >= 60 and valueQuantity.convertsToQuantity('bpm') and code.coding.code.first().matchesFull('8867-4') and effectiveDateTime.toTime() <= timeOfDay()"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"CAST(r.value_quantity AS DOUBLE) AS qty_val",
				"(CAST(r.value_quantity AS DOUBLE) IS NOT NULL) AS can_qty",
				"REGEXP(r.code, '^8867-4$') AS full_match",
				"CURTIME() AS tod_val",
				"CAST(r.value_quantity AS DOUBLE) >= 60",
				"CAST(r.effective_start AS TIME) <= CURTIME()",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"CAST((SELECT q.sp_value FROM spidx_quantity q",
				"AS DOUBLE PRECISION) AS qty_val",
				"IS NOT NULL) AS can_qty",
				"~ '^8867-4$' AS full_match",
				"CURRENT_TIME AS tod_val",
				"<= CURRENT_TIME",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"CAST((SELECT q.sp_value FROM spidx_quantity q",
				"AS REAL) AS qty_val",
				"IS NOT NULL) AS can_qty",
				"REGEXP '^8867-4$' AS full_match",
				"CURRENT_TIME AS tod_val",
				"<= CURRENT_TIME",
			},
		},
	} {
		reqTQM, tqmErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(toQtyMatchesVD))
		require.NoError(t, tqmErr)
		reqTQM.Header.Set("Content-Type", "application/fhir+json")
		respTQM, tqmErr := client.Do(reqTQM)
		require.NoError(t, tqmErr)
		tqmBytes, _ := io.ReadAll(respTQM.Body)
		respTQM.Body.Close()
		require.Equal(t, http.StatusOK, respTQM.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(tqmBytes), want)
		}
	}

	// 37. Wave 92: Binary Proto3 Da Vinci RA/CDex, CDC NHSN dQM/ACH, Da Vinci DTR/PAS/Burden Reduction, CodeX mCODE/RTTD, FAST NDH/UDAP, Da Vinci ATR, PACIO SPLASCH, Vulcan RWD, Public Health SANER, Gravity SDOH, Da Vinci PDex & .sort()/.sortDesc() + Chained String Predicates $sql Pushdown
	w92ParamsPb := &parampb.Parameters{
		Parameter: []*parampb.Parameters_Parameter{
			{
				Name: &dtpb.String{Value: "patient"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Patient/" + patID},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "subject"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Patient/" + patID},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "group"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Group/atr-group-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "organization"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Organization/ndh-org-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "location"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Location/saner-loc-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "study"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "ResearchStudy/vulcan-rwd-study-1"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "measure"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Reference{
						Reference: &dtpb.Reference{
							Reference: &dtpb.Reference_Uri{
								Uri: &dtpb.String{Value: "Measure/nhsn-ach-hypoglycemia-cdi"},
							},
						},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "codingGapType"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Code{
						Code: &dtpb.Code{Value: "suspected"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "purpose_of_use"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_Code{
						Code: &dtpb.Code{Value: "TREAT"},
					},
				},
			},
			{
				Name: &dtpb.String{Value: "client_name"},
				Value: &parampb.Parameters_Parameter_ValueX{
					Choice: &parampb.Parameters_Parameter_ValueX_StringValue{
						StringValue: &dtpb.String{Value: "Flint Proto3 UDAP Client"},
					},
				},
			},
		},
	}
	w92ParamsBytes, err := proto.Marshal(w92ParamsPb)
	require.NoError(t, err)

	for _, ep := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/fhir/r4/proto/MeasureReport/$ra-evaluate-risk"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$ra-coding-gap-report"},
		{http.MethodPost, "/fhir/r4/proto/Task/$cdex-submit-attachment"},
		{http.MethodPost, "/fhir/r4/proto/Measure/$nhsn-dqm-evaluate"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$nhsn-ach-summary"},
		{http.MethodPost, "/fhir/r4/proto/Questionnaire/$dtr-questionnaire-package"},
		{http.MethodPost, "/fhir/r4/proto/Claim/$pas-inquire"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$burden-reduction-summary"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$mcode-trial-summary"},
		{http.MethodPost, "/fhir/r4/proto/VerificationResult/$ndh-verify"},
		{http.MethodPost, "/fhir/r4/proto/Organization/$ndh-directory-summary"},
		{http.MethodPost, "/fhir/r4/proto/Group/$confirm-attribution-list"},
		{http.MethodPost, "/fhir/r4/proto/Group/$atr-summary"},
		{http.MethodPost, "/fhir/r4/proto/Observation/$pacio-splasch-record"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$pacio-splasch-summary"},
		{http.MethodPost, "/fhir/r4/proto/ResearchStudy/$vulcan-rwd-cohort"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$vulcan-rwd-summary"},
		{http.MethodPost, "/fhir/r4/proto/MeasureReport/$saner-report-capacity"},
		{http.MethodPost, "/fhir/r4/proto/Location/$saner-capacity-summary"},
		{http.MethodPost, "/fhir/r4/proto/ServiceRequest/$codex-rttd-plan"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$codex-rttd-summary"},
		{http.MethodPost, "/fhir/r4/proto/Consent/$sdoh-consent-record"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$sdoh-care-bundle"},
		{http.MethodPost, "/fhir/r4/proto/Organization/$udap-register"},
		{http.MethodPost, "/fhir/r4/proto/Organization/$udap-trust-summary"},
		{http.MethodPost, "/fhir/r4/proto/Group/$pdex-provider-member-match"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$pdex-provider-everything"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$pdex-payer-exchange"},
		{http.MethodPost, "/fhir/r4/proto/Patient/$pdex-payer-summary"},
	} {
		reqOp, opErr := http.NewRequest(ep.method, ts.URL+ep.path, bytes.NewReader(w92ParamsBytes))
		require.NoError(t, opErr)
		reqOp.Header.Set("Content-Type", "application/x-protobuf")
		reqOp.Header.Set("Accept", "application/x-protobuf")
		respOp, opErr := client.Do(reqOp)
		require.NoError(t, opErr)
		opBytes, _ := io.ReadAll(respOp.Body)
		respOp.Body.Close()
		require.Equal(t, http.StatusOK, respOp.StatusCode, "endpoint %s failed: %s", ep.path, string(opBytes))
		assert.Contains(t, respOp.Header.Get("Content-Type"), "application/x-protobuf", "endpoint %s Content-Type", ep.path)
		assert.Equal(t, "google.fhir.r4.proto3", respOp.Header.Get("X-Flint-Wire-Format"), "endpoint %s X-Flint-Wire-Format", ep.path)
		require.NotEmpty(t, opBytes, "endpoint %s empty body", ep.path)
	}

	sortChainVD := `{
		"resourceType": "ViewDefinition",
		"name": "pat_sort_sortdesc_chained_predicates_v1",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "sorted_given", "path": "name.given.sort().join(',')"},
					{"name": "reverse_given", "path": "name.given.sortDesc().join(',')"},
					{"name": "first_sorted", "path": "name.given.sort().first()"},
					{"name": "first_rev", "path": "name.given.sortDesc().first()"}
				]
			}
		],
		"where": [
			{"path": "name.family.first().lower().startsWith('cert') and name.family.first().upper().endsWith('TIFY') and coalesce(name.family.first(), 'none').contains('ti') and name.given.sort().first() = 'Alice'"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"ARRAY_JOIN(ARRAY_SORT(",
				"ARRAY_JOIN(ARRAY_REVERSE_SORT(",
				"element_at(ARRAY_SORT(",
				"element_at(ARRAY_REVERSE_SORT(",
				"LIKE 'cert%'",
				"LIKE '%TIFY'",
				"LIKE '%ti%'",
				"= 'Alice'",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"ORDER BY sp_value ASC) sub) AS sorted_given",
				"ORDER BY sp_value DESC) sub) AS reverse_given",
				"ORDER BY s.sp_value ASC LIMIT 1) AS first_sorted",
				"ORDER BY s.sp_value DESC LIMIT 1) AS first_rev",
				"LIKE 'cert%'",
				"LIKE '%TIFY'",
				"LIKE '%ti%'",
				"= 'Alice'",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"ORDER BY sp_value ASC) sub) AS sorted_given",
				"ORDER BY sp_value DESC) sub) AS reverse_given",
				"ORDER BY s.sp_value ASC LIMIT 1) AS first_sorted",
				"ORDER BY s.sp_value DESC LIMIT 1) AS first_rev",
				"LIKE 'cert%'",
				"LIKE '%TIFY'",
				"LIKE '%ti%'",
				"= 'Alice'",
			},
		},
	} {
		reqSC, scErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(sortChainVD))
		require.NoError(t, scErr)
		reqSC.Header.Set("Content-Type", "application/fhir+json")
		respSC, scErr := client.Do(reqSC)
		require.NoError(t, scErr)
		scBytes, _ := io.ReadAll(respSC.Body)
		respSC.Body.Close()
		require.Equal(t, http.StatusOK, respSC.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(scBytes), want)
		}
	}

	// 48. Wave 93: Chained .distinct().sort()/.sortDesc() & .split().first()/.last()/.count() $sql Pushdown
	distSortSplitVD := `{
		"resourceType": "ViewDefinition",
		"name": "pat_distinct_sort_split_v1",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "dist_sorted_arr", "path": "name.given.distinct().sort()", "collection": true},
					{"name": "dist_sorted_join", "path": "name.given.distinct().sort().join(',')"},
					{"name": "dist_rev_join", "path": "name.given.distinct().sortDesc().join(',')"},
					{"name": "dist_first", "path": "name.given.distinct().sort().first()"},
					{"name": "dist_last", "path": "name.given.distinct().sort().last()"},
					{"name": "id_prefix", "path": "id.split('-').first()"},
					{"name": "id_suffix", "path": "id.split('-').last()"},
					{"name": "id_parts", "path": "id.split('-').count()"}
				]
			}
		],
		"where": [
			{"path": "name.given.distinct().sort().first() = 'Alice' and id.split('-').first() = 'proto' and id.split('-').count() = 3"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"ARRAY_SORT(ARRAY_DISTINCT(",
				"ARRAY_JOIN(ARRAY_SORT(ARRAY_DISTINCT(",
				"ARRAY_JOIN(ARRAY_REVERSE_SORT(ARRAY_DISTINCT(",
				"element_at(ARRAY_SORT(ARRAY_DISTINCT(",
				"element_at(SPLIT_BY_STRING(r.res_id, '-'), 1)",
				"element_at(SPLIT_BY_STRING(r.res_id, '-'), -1)",
				"ARRAY_SIZE(SPLIT_BY_STRING(r.res_id, '-'))",
				"= 'Alice'",
				"= 'proto'",
				"= 3",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"SELECT DISTINCT s.sp_value",
				"ORDER BY sp_value ASC) sub) AS dist_sorted_join",
				"ORDER BY sp_value DESC) sub) AS dist_rev_join",
				"ORDER BY s.sp_value ASC LIMIT 1) AS dist_first",
				"ORDER BY s.sp_value DESC LIMIT 1) AS dist_last",
				"SPLIT_PART(r.res_id, '-', 1) AS id_prefix",
				"[ARRAY_UPPER(STRING_TO_ARRAY(r.res_id, '-'), 1)] AS id_suffix",
				"COALESCE(ARRAY_LENGTH(STRING_TO_ARRAY(r.res_id, '-'), 1), 0) AS id_parts",
				"= 'Alice'",
				"= 'proto'",
				"= 3",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"SELECT DISTINCT s.sp_value",
				"ORDER BY sp_value ASC) sub) AS dist_sorted_join",
				"ORDER BY sp_value DESC) sub) AS dist_rev_join",
				"ORDER BY s.sp_value ASC LIMIT 1) AS dist_first",
				"ORDER BY s.sp_value DESC LIMIT 1) AS dist_last",
				"INSTR(r.res_id, '-')",
				"RTRIM(r.res_id, REPLACE(r.res_id, '-', ''))",
				"LENGTH(r.res_id) - LENGTH(REPLACE(r.res_id, '-', ''))",
				"= 'Alice'",
				"= 'proto'",
				"= 3",
			},
		},
	} {
		reqDSS, dssErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(distSortSplitVD))
		require.NoError(t, dssErr)
		reqDSS.Header.Set("Content-Type", "application/fhir+json")
		respDSS, dssErr := client.Do(reqDSS)
		require.NoError(t, dssErr)
		dssBytes, _ := io.ReadAll(respDSS.Body)
		respDSS.Body.Close()
		require.Equal(t, http.StatusOK, respDSS.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(dssBytes), want)
		}
	}

	// 49. Wave 94: Chained .skip().take() & .sort().take()/.skip() $sql Pushdown
	sliceSortVD := `{
		"resourceType": "ViewDefinition",
		"name": "pat_slice_sort_v1",
		"status": "active",
		"resource": "Patient",
		"select": [
			{
				"column": [
					{"name": "sliced_arr", "path": "name.given.skip(1).take(2)", "collection": true},
					{"name": "sliced_join", "path": "name.given.skip(1).take(2).join(',')"},
					{"name": "sorted_top2", "path": "name.given.sort().take(2).join(',')"},
					{"name": "sorted_second", "path": "name.given.sort().skip(1).first()"},
					{"name": "sliced_count", "path": "name.given.skip(1).take(2).count()"}
				]
			}
		],
		"where": [
			{"path": "name.given.sort().skip(1).first() = 'Bob' and name.given.skip(1).take(2).count() = 2"}
		]
	}`
	for _, tc := range []struct {
		dialect      string
		wantContains []string
	}{
		{
			dialect: "doris",
			wantContains: []string{
				"ARRAY_SLICE(ARRAY_SLICE(",
				"ARRAY_JOIN(ARRAY_SLICE(ARRAY_SLICE(",
				"ARRAY_JOIN(ARRAY_SLICE(ARRAY_SORT(",
				"element_at(ARRAY_SLICE(ARRAY_SORT(",
				"ARRAY_SIZE(ARRAY_SLICE(ARRAY_SLICE(",
				"= 'Bob'",
				"= 2",
			},
		},
		{
			dialect: "postgres",
			wantContains: []string{
				"LIMIT 2 OFFSET 1) sub) AS sliced_arr",
				"LIMIT 2 OFFSET 1) sub) AS sliced_join",
				"ORDER BY sp_value ASC LIMIT 2) sub) AS sorted_top2",
				"ORDER BY s.sp_value ASC LIMIT 1 OFFSET 1) AS sorted_second",
				"SELECT COUNT(*) FROM (",
				"= 'Bob'",
				"= 2",
			},
		},
		{
			dialect: "sqlite",
			wantContains: []string{
				"LIMIT 2 OFFSET 1) sub) AS sliced_arr",
				"LIMIT 2 OFFSET 1) sub) AS sliced_join",
				"ORDER BY sp_value ASC LIMIT 2) sub) AS sorted_top2",
				"ORDER BY s.sp_value ASC LIMIT 1 OFFSET 1) AS sorted_second",
				"SELECT COUNT(*) FROM (",
				"= 'Bob'",
				"= 2",
			},
		},
	} {
		reqSS, ssErr := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/proto/ViewDefinition/$sql?dialect="+tc.dialect+"&_format=application/sql", strings.NewReader(sliceSortVD))
		require.NoError(t, ssErr)
		reqSS.Header.Set("Content-Type", "application/fhir+json")
		respSS, ssErr := client.Do(reqSS)
		require.NoError(t, ssErr)
		ssBytes, _ := io.ReadAll(respSS.Body)
		respSS.Body.Close()
		require.Equal(t, http.StatusOK, respSS.StatusCode)
		for _, want := range tc.wantContains {
			assert.Contains(t, string(ssBytes), want)
		}
	}
}
