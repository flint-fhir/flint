package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

// TestHL7TestScriptR4ConformanceExecutionEngine verifies that Flint natively executes
// declarative HL7 FHIR R4 / AEGIS Touchstone TestScript resources (setup, test[],
// teardown, fixtures, dynamic variables, CRUD/search/history/$operation actions,
// and HTTP/FHIRPath assertions) and produces standard HL7 FHIR R4 TestReport resources.
func TestHL7TestScriptR4ConformanceExecutionEngine(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "testscript-r4-conformance"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	// 1. Execute a comprehensive Touchstone-style US Core Patient + Vital Signs TestScript
	t.Run("Touchstone_USCore_Patient_And_Observation_Lifecycle", func(t *testing.T) {
		testScriptJSON := []byte(`{
			"resourceType": "TestScript",
			"id": "ts-touchstone-uscore-patient",
			"url": "http://flint.fhir.org/TestScript/ts-touchstone-uscore-patient",
			"name": "TouchstoneUSCorePatientLifecycle",
			"status": "active",
			"contained": [
				{
					"resourceType": "Patient",
					"id": "fixture-pat-create",
					"active": true,
					"gender": "female",
					"birthDate": "1988-11-23",
					"identifier": [
						{"system": "http://hospital.smarthealthit.org", "value": "TS-MRN-1001"}
					],
					"name": [
						{"use": "official", "family": "Touchstone", "given": [" Clara "]}
					]
				},
				{
					"resourceType": "Patient",
					"id": "fixture-pat-update",
					"active": true,
					"gender": "female",
					"birthDate": "1988-11-23",
					"identifier": [
						{"system": "http://hospital.smarthealthit.org", "value": "TS-MRN-1001"}
					],
					"name": [
						{"use": "official", "family": "Touchstone-Updated", "given": ["Clara", "Marie"]}
					],
					"telecom": [
						{"system": "phone", "value": "555-0144", "rank": 1}
					]
				}
			],
			"fixture": [
				{
					"id": "pat-create-payload",
					"autocreate": false,
					"autodelete": false,
					"resource": {"reference": "#fixture-pat-create"}
				},
				{
					"id": "pat-update-payload",
					"autocreate": false,
					"autodelete": false,
					"resource": {"reference": "#fixture-pat-update"}
				}
			],
			"variable": [
				{
					"name": "createdPatientId",
					"expression": "Patient.id",
					"sourceId": "create-patient-response"
				},
				{
					"name": "patientFamily",
					"defaultValue": "Touchstone-Updated"
				}
			],
			"setup": {
				"action": [
					{
						"operation": {
							"type": {"system": "http://terminology.hl7.org/CodeSystem/testscript-operation-codes", "code": "capabilities"},
							"responseId": "cap-response"
						}
					},
					{
						"assert": {
							"response": "okay",
							"resource": "CapabilityStatement",
							"contentType": "application/fhir+json",
							"expression": "CapabilityStatement.fhirVersion",
							"operator": "equals",
							"value": "4.0.1"
						}
					}
				]
			},
			"test": [
				{
					"name": "01-CreateAndReadPatient",
					"description": "Create a Patient via POST and verify location header, ETag, minimumId, and FHIRPath expressions.",
					"action": [
						{
							"operation": {
								"type": {"code": "create"},
								"resource": "Patient",
								"sourceId": "pat-create-payload",
								"responseId": "create-patient-response"
							}
						},
						{
							"assert": {
								"response": "created",
								"responseCode": "201",
								"resource": "Patient",
								"headerField": "ETag",
								"operator": "contains",
								"value": "W/\"1\""
							}
						},
						{
							"assert": {
								"minimumId": "pat-create-payload"
							}
						},
						{
							"operation": {
								"type": {"code": "read"},
								"resource": "Patient",
								"targetId": "${createdPatientId}",
								"responseId": "read-patient-response"
							}
						},
						{
							"assert": {
								"response": "okay",
								"resource": "Patient",
								"expression": "Patient.name.where(use = 'official').family",
								"compareToSourceId": "create-patient-response",
								"compareToSourceExpression": "Patient.name.where(use = 'official').family"
							}
						}
					]
				},
				{
					"name": "02-UpdateVReadHistoryAndSearch",
					"description": "Update the Patient to v2, verify _history, vread v1, and search by family.",
					"action": [
						{
							"operation": {
								"type": {"code": "update"},
								"resource": "Patient",
								"targetId": "${createdPatientId}",
								"sourceId": "pat-update-payload",
								"responseId": "update-patient-response"
							}
						},
						{
							"assert": {
								"responseCode": "200,201",
								"expression": "Patient.meta.versionId",
								"operator": "equals",
								"value": "2"
							}
						},
						{
							"assert": {
								"expression": "Patient.name.given.count()",
								"operator": "greaterThan",
								"value": "1"
							}
						},
						{
							"assert": {
								"expression": "Patient.gender",
								"operator": "in",
								"value": "female,other"
							}
						},
						{
							"assert": {
								"expression": "Patient.gender",
								"operator": "notIn",
								"value": "male,unknown"
							}
						},
						{
							"assert": {
								"expression": "Patient.photo",
								"operator": "empty"
							}
						},
						{
							"assert": {
								"expression": "Patient.identifier[0].value",
								"operator": "notEmpty"
							}
						},
						{
							"operation": {
								"type": {"code": "vread"},
								"resource": "Patient",
								"targetId": "${createdPatientId}",
								"params": "1"
							}
						},
						{
							"assert": {
								"response": "okay",
								"expression": "Patient.name[0].family",
								"operator": "equals",
								"value": "Touchstone"
							}
						},
						{
							"operation": {
								"type": {"code": "history"},
								"resource": "Patient",
								"targetId": "${createdPatientId}"
							}
						},
						{
							"assert": {
								"response": "okay",
								"resource": "Bundle",
								"expression": "Bundle.total",
								"operator": "equals",
								"value": "2"
							}
						},
						{
							"operation": {
								"type": {"code": "search"},
								"resource": "Patient",
								"params": "family=${patientFamily}"
							}
						},
						{
							"assert": {
								"response": "okay",
								"resource": "Bundle",
								"expression": "Bundle.entry.exists()"
							}
						}
					]
				}
			],
			"teardown": {
				"action": [
					{
						"operation": {
							"type": {"code": "delete"},
							"resource": "Patient",
							"targetId": "${createdPatientId}"
						}
					}
				]
			}
		}`)

		req := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/TestScript/$evaluate", bytes.NewReader(testScriptJSON))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /TestScript/$evaluate returned %d: %s", rec.Code, rec.Body.String())
		}

		var report map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatalf("unmarshal TestReport: %v", err)
		}
		if rt, _ := report["resourceType"].(string); rt != "TestReport" {
			t.Fatalf("expected resourceType=TestReport, got %q", rt)
		}
		if res, _ := report["result"].(string); res != "pass" {
			t.Fatalf("expected TestReport result=pass, got %q:\n%s", res, rec.Body.String())
		}
		if score, _ := report["score"].(float64); score != 100.0 {
			t.Fatalf("expected TestReport score=100.0, got %.1f:\n%s", score, rec.Body.String())
		}
	})

	// 2. Stored TestScript/{id}/$evaluate + Parameters wrapper with external fixture
	t.Run("Stored_TestScript_Instance_Evaluate_And_Parameters_Fixture", func(t *testing.T) {
		storedScript := []byte(`{
			"resourceType": "TestScript",
			"id": "ts-stored-1",
			"url": "http://flint.fhir.org/TestScript/ts-stored-1",
			"name": "StoredTestScriptCheck",
			"status": "active",
			"test": [
				{
					"name": "CreateAndValidateExternalFixture",
					"action": [
						{
							"operation": {
								"type": {"code": "create"},
								"resource": "Organization",
								"sourceId": "ext-org",
								"responseId": "created-org"
							}
						},
						{
							"assert": {
								"response": "created",
								"expression": "Organization.name",
								"operator": "contains",
								"value": "Touchstone"
							}
						},
						{
							"operation": {
								"type": {"code": "$fhirpath"},
								"resource": "Organization",
								"targetId": "created-org",
								"params": "expression=Organization.name.upper()"
							}
						},
						{
							"assert": {
								"response": "okay",
								"resource": "Parameters"
							}
						}
					]
				}
			],
			"teardown": {
				"action": [
					{
						"operation": {
							"type": {"code": "delete"},
							"resource": "Organization",
							"targetId": "created-org"
						}
					}
				]
			}
		}`)

		reqPut := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/TestScript/ts-stored-1", bytes.NewReader(storedScript))
		reqPut.Header.Set("Content-Type", "application/fhir+json")
		recPut := httptest.NewRecorder()
		h.ServeHTTP(recPut, reqPut)
		if recPut.Code != http.StatusOK && recPut.Code != http.StatusCreated {
			t.Fatalf("PUT /TestScript/ts-stored-1 returned %d: %s", recPut.Code, recPut.Body.String())
		}

		paramsBody := []byte(`{
			"resourceType": "Parameters",
			"parameter": [
				{
					"name": "fixture",
					"part": [
						{"name": "id", "valueString": "ext-org"},
						{
							"name": "resource",
							"resource": {
								"resourceType": "Organization",
								"active": true,
								"name": "Touchstone Conformance Lab"
							}
						}
					]
				}
			]
		}`)
		reqEval := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/TestScript/ts-stored-1/$evaluate", bytes.NewReader(paramsBody))
		reqEval.Header.Set("Content-Type", "application/fhir+json")
		recEval := httptest.NewRecorder()
		h.ServeHTTP(recEval, reqEval)
		if recEval.Code != http.StatusOK {
			t.Fatalf("POST /TestScript/ts-stored-1/$evaluate returned %d: %s", recEval.Code, recEval.Body.String())
		}
		var rep map[string]any
		_ = json.Unmarshal(recEval.Body.Bytes(), &rep)
		if rep["result"] != "pass" {
			t.Fatalf("expected stored TestScript evaluation to pass, got %s", recEval.Body.String())
		}
	})

	// 3. WarningOnly vs Fail assertions and error handling (400 / 404)
	t.Run("WarningOnly_And_Failure_Reporting", func(t *testing.T) {
		failScript := []byte(`{
			"resourceType": "TestScript",
			"name": "FailAndWarningCheck",
			"status": "active",
			"test": [
				{
					"name": "CheckWarningAndFailure",
					"action": [
						{
							"operation": {
								"type": {"code": "read"},
								"resource": "Patient",
								"targetId": "non-existent-patient-999"
							}
						},
						{
							"assert": {
								"response": "notFound",
								"resource": "OperationOutcome"
							}
						},
						{
							"assert": {
								"response": "okay",
								"warningOnly": true
							}
						},
						{
							"assert": {
								"responseCode": "200"
							}
						}
					]
				}
			]
		}`)
		req := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/TestScript/$evaluate", bytes.NewReader(failScript))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK with failing TestReport, got %d: %s", rec.Code, rec.Body.String())
		}
		var rep map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &rep)
		if rep["result"] != "fail" {
			t.Fatalf("expected result=fail when non-warning assertion fails, got %v", rep["result"])
		}

		// 404 on missing stored TestScript
		req404 := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/TestScript/does-not-exist/$evaluate", nil)
		rec404 := httptest.NewRecorder()
		h.ServeHTTP(rec404, req404)
		if rec404.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for missing TestScript, got %d", rec404.Code)
		}

		// 400 on missing script parameter
		req400 := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/TestScript/$evaluate", nil)
		rec400 := httptest.NewRecorder()
		h.ServeHTTP(rec400, req400)
		if rec400.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for TestScript/$evaluate without script, got %d", rec400.Code)
		}
	})
}
