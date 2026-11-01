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
}
