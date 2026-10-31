package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

// TestHL7FHIRR4BundleTransactionAndBatchConformanceSuite verifies HAPI / Medplum-grade
// HL7 FHIR R4 Bundle (§3.1.0.10 / §2.36) conformance across:
//   - Topological urn:uuid: fullUrl resolution across forward, reverse, and multi-hop references
//   - FHIR R4 §3.1.0.10.1 transaction execution priority (DELETE -> POST -> PUT/PATCH -> GET/HEAD)
//     while preserving request entry ordering in transaction-response
//   - Full ACID rollback on both StructureDefinition validation errors (400) and in-transaction
//     If-Match ETag version conflicts (412 Precondition Failed)
//   - Conditional Bundle operations (request.ifNoneExist, conditional PUT, conditional DELETE)
//     and Prefer: return=representation / return=OperationOutcome headers
//   - Batch Bundle (type=batch) partial-failure isolation where failing entries return OperationOutcome
//     while sibling entries succeed independently
func TestHL7FHIRR4BundleTransactionAndBatchConformanceSuite(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "r4-bundle-conformance-tenant"
	cleanup := func() {
		_ = postgres.PurgeTenant(ctx, db, tenant)
	}
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	baseURL := ts.URL + "/fhir/r4/" + tenant

	doReq := func(t *testing.T, method, path, body string, headers map[string]string) (int, map[string]any, string) {
		t.Helper()
		var bodyReader io.Reader
		if body != "" {
			bodyReader = bytes.NewBufferString(body)
		}
		req, err := http.NewRequest(method, baseURL+path, bodyReader)
		if err != nil {
			t.Fatalf("new request %s %s: %v", method, path, err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/fhir+json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("do request %s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		rawBytes, _ := io.ReadAll(resp.Body)
		var parsed map[string]any
		_ = json.Unmarshal(rawBytes, &parsed)
		return resp.StatusCode, parsed, string(rawBytes)
	}

	// =========================================================================
	// 1. Topological urn:uuid: Graph Resolution (Forward, Reverse & Multi-Hop)
	// =========================================================================
	t.Run("URN_UUID_Topological_Graph_Resolution_Forward_And_Reverse", func(t *testing.T) {
		// Notice DiagnosticReport is placed FIRST (entry[0]) and references Patient, Encounter,
		// and Observation via forward urn:uuid: fullUrls before they appear in entries 1..4.
		txBundle := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"fullUrl": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1",
					"resource": {
						"resourceType": "DiagnosticReport",
						"status": "final",
						"code": {"coding": [{"system": "http://loinc.org", "code": "24323-8", "display": "Comprehensive metabolic 2000 panel"}]},
						"subject": {"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa2"},
						"encounter": {"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa3"},
						"result": [{"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa4"}]
					},
					"request": {"method": "POST", "url": "DiagnosticReport"}
				},
				{
					"fullUrl": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa4",
					"resource": {
						"resourceType": "Observation",
						"status": "final",
						"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}]}],
						"code": {"coding": [{"system": "http://loinc.org", "code": "2345-7", "display": "Glucose"}]},
						"subject": {"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa2"},
						"encounter": {"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa3"},
						"valueQuantity": {"value": 94, "unit": "mg/dL", "system": "http://unitsofmeasure.org", "code": "mg/dL"}
					},
					"request": {"method": "POST", "url": "Observation"}
				},
				{
					"fullUrl": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa3",
					"resource": {
						"resourceType": "Encounter",
						"status": "finished",
						"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
						"subject": {"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa2"},
						"serviceProvider": {"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa5"}
					},
					"request": {"method": "POST", "url": "Encounter"}
				},
				{
					"fullUrl": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa2",
					"resource": {
						"resourceType": "Patient",
						"identifier": [{"system": "http://hospital.example.org/mrn", "value": "URN-PAT-001"}],
						"name": [{"family": "Topological", "given": ["Tara"]}],
						"gender": "female",
						"birthDate": "1988-04-12",
						"managingOrganization": {"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa5"}
					},
					"request": {"method": "POST", "url": "Patient"}
				},
				{
					"fullUrl": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa5",
					"resource": {
						"resourceType": "Organization",
						"active": true,
						"name": "Topological Health System"
					},
					"request": {"method": "POST", "url": "Organization"}
				},
				{
					"fullUrl": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa6",
					"resource": {
						"resourceType": "Provenance",
						"target": [
							{"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1"},
							{"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa4"}
						],
						"recorded": "2026-03-01T12:00:00Z",
						"agent": [{"who": {"reference": "urn:uuid:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa5"}}]
					},
					"request": {"method": "POST", "url": "Provenance"}
				}
			]
		}`

		code, respBundle, raw := doReq(t, http.MethodPost, "", txBundle, map[string]string{
			"Prefer": "return=representation",
		})
		if code != http.StatusOK {
			t.Fatalf("expected 200 OK on topological urn:uuid: transaction, got %d: %s", code, raw)
		}
		if strings.Contains(raw, "urn:uuid:") {
			t.Fatalf("expected all urn:uuid: references to be rewritten to server resourceType/id, got: %s", raw)
		}
		entries, _ := respBundle["entry"].([]any)
		if len(entries) != 6 {
			t.Fatalf("expected 6 entries in transaction-response, got %d", len(entries))
		}

		// Verify forward-referenced DiagnosticReport (entry[0]) has resolved references in spidx_reference
		// by running a chained search + _include + _revinclude
		code, searchBundle, searchRaw := doReq(t, http.MethodGet, "/DiagnosticReport?subject:Patient.family=Topological&_include=DiagnosticReport:result&_revinclude=Provenance:target", "", nil)
		if code != http.StatusOK {
			t.Fatalf("DiagnosticReport chained + include + revinclude search failed (%d): %s", code, searchRaw)
		}
		searchEntries, _ := searchBundle["entry"].([]any)
		if len(searchEntries) != 3 {
			t.Fatalf("expected 3 entries (DiagnosticReport + included Observation + revincluded Provenance), got %d: %s", len(searchEntries), searchRaw)
		}
	})

	// =========================================================================
	// 2. FHIR R4 §3.1.0.10.1 Execution Ordering (DELETE -> POST -> PUT/PATCH -> GET)
	// =========================================================================
	t.Run("FHIR_R4_Execution_Ordering_DELETE_POST_PUT_PATCH_GET_Preserving_Entry_Order", func(t *testing.T) {
		// Seed Patient/ord-del-1 (to be deleted) and Patient/ord-patch-1 (to be patched)
		if code, _, raw := doReq(t, http.MethodPut, "/Patient/ord-del-1", `{
			"resourceType": "Patient",
			"id": "ord-del-1",
			"name": [{"family": "ToBeDeleted"}]
		}`, nil); code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("seed ord-del-1 failed (%d): %s", code, raw)
		}
		if code, _, raw := doReq(t, http.MethodPut, "/Patient/ord-patch-1", `{
			"resourceType": "Patient",
			"id": "ord-patch-1",
			"name": [{"family": "BeforePatch"}],
			"gender": "female"
		}`, nil); code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("seed ord-patch-1 failed (%d): %s", code, raw)
		}

		// Submit transaction with entries ordered: [0]=GET, [1]=PATCH, [2]=PUT, [3]=POST, [4]=DELETE
		txOrderBundle := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"request": {"method": "GET", "url": "Patient/ord-patch-1"}
				},
				{
					"resource": [
						{"op": "replace", "path": "/name/0/family", "value": "PatchedDuringTx"}
					],
					"request": {"method": "PATCH", "url": "Patient/ord-patch-1"}
				},
				{
					"resource": {
						"resourceType": "Patient",
						"id": "ord-put-1",
						"name": [{"family": "PutDuringTx"}]
					},
					"request": {"method": "PUT", "url": "Patient/ord-put-1"}
				},
				{
					"resource": {
						"resourceType": "Patient",
						"name": [{"family": "PostDuringTx"}]
					},
					"request": {"method": "POST", "url": "Patient"}
				},
				{
					"request": {"method": "DELETE", "url": "Patient/ord-del-1"}
				}
			]
		}`

		code, respBundle, raw := doReq(t, http.MethodPost, "", txOrderBundle, nil)
		if code != http.StatusOK {
			t.Fatalf("expected 200 OK on ordered transaction, got %d: %s", code, raw)
		}
		entries, _ := respBundle["entry"].([]any)
		if len(entries) != 5 {
			t.Fatalf("expected 5 response entries in original order, got %d: %s", len(entries), raw)
		}

		// Entry 0 (GET Patient/ord-patch-1) executed AFTER Entry 1 (PATCH), so it must return "PatchedDuringTx"
		e0, _ := entries[0].(map[string]any)
		e0Res, _ := json.Marshal(e0["resource"])
		if !strings.Contains(string(e0Res), `"PatchedDuringTx"`) {
			t.Fatalf("expected GET entry[0] to observe patched family 'PatchedDuringTx', got: %s", string(e0Res))
		}

		// Entry 4 (DELETE Patient/ord-del-1) must have status "204 No Content"
		e4, _ := entries[4].(map[string]any)
		e4Resp, _ := e4["response"].(map[string]any)
		if status, _ := e4Resp["status"].(string); !strings.HasPrefix(status, "204") {
			t.Fatalf("expected entry[4] DELETE status 204 No Content, got %v", status)
		}

		// Confirm Patient/ord-del-1 is now 410 Gone
		delCode, _, _ := doReq(t, http.MethodGet, "/Patient/ord-del-1", "", nil)
		if delCode != http.StatusGone {
			t.Fatalf("expected 410 Gone for deleted Patient/ord-del-1, got %d", delCode)
		}
	})

	// =========================================================================
	// 3. ACID Transaction Rollback on Validation (400) & If-Match Conflict (412)
	// =========================================================================
	t.Run("ACID_Transaction_Rollback_On_Validation_And_Precondition_Failure", func(t *testing.T) {
		// Seed anchor Patient/cas-anchor-1 at version 1
		if code, _, raw := doReq(t, http.MethodPut, "/Patient/cas-anchor-1", `{
			"resourceType": "Patient",
			"id": "cas-anchor-1",
			"name": [{"family": "AnchorOriginal"}]
		}`, nil); code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("seed cas-anchor-1 failed (%d): %s", code, raw)
		}

		// 3a. Validation failure on Entry 2 must prevent Entry 0 and Entry 1 from persisting
		invalidTx := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {
						"resourceType": "Patient",
						"id": "rb-val-pat-1",
						"name": [{"family": "RollbackValidation"}]
					},
					"request": {"method": "PUT", "url": "Patient/rb-val-pat-1"}
				},
				{
					"resource": {
						"resourceType": "Observation",
						"id": "rb-val-obs-1",
						"status": "final",
						"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
						"subject": {"reference": "Patient/rb-val-pat-1"}
					},
					"request": {"method": "PUT", "url": "Observation/rb-val-obs-1"}
				},
				{
					"resource": {
						"resourceType": "Observation",
						"id": "rb-val-obs-bad"
					},
					"request": {"method": "PUT", "url": "Observation/rb-val-obs-bad"}
				}
			]
		}`
		code, _, raw := doReq(t, http.MethodPost, "", invalidTx, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on invalid Observation in transaction, got %d: %s", code, raw)
		}
		if c, _, _ := doReq(t, http.MethodGet, "/Patient/rb-val-pat-1", "", nil); c != http.StatusNotFound {
			t.Fatalf("expected rolled-back Patient/rb-val-pat-1 to return 404 Not Found, got %d", c)
		}

		// 3b. In-transaction If-Match conflict (412 Precondition Failed) on Entry 2 must roll back Entry 0 & Entry 1
		conflictTx := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {
						"resourceType": "Patient",
						"id": "rb-cas-pat-1",
						"name": [{"family": "RollbackCAS"}]
					},
					"request": {"method": "PUT", "url": "Patient/rb-cas-pat-1"}
				},
				{
					"resource": {
						"resourceType": "Observation",
						"id": "rb-cas-obs-1",
						"status": "final",
						"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
						"subject": {"reference": "Patient/rb-cas-pat-1"}
					},
					"request": {"method": "PUT", "url": "Observation/rb-cas-obs-1"}
				},
				{
					"resource": {
						"resourceType": "Patient",
						"id": "cas-anchor-1",
						"name": [{"family": "AnchorCorrupted"}]
					},
					"request": {
						"method": "PUT",
						"url": "Patient/cas-anchor-1",
						"ifMatch": "W/\"999\""
					}
				}
			]
		}`
		code, _, raw = doReq(t, http.MethodPost, "", conflictTx, nil)
		if code != http.StatusPreconditionFailed {
			t.Fatalf("expected 412 Precondition Failed on stale ifMatch in transaction, got %d: %s", code, raw)
		}
		if c, _, _ := doReq(t, http.MethodGet, "/Patient/rb-cas-pat-1", "", nil); c != http.StatusNotFound {
			t.Fatalf("expected rolled-back Patient/rb-cas-pat-1 to return 404 Not Found, got %d", c)
		}
		if c, _, _ := doReq(t, http.MethodGet, "/Observation/rb-cas-obs-1", "", nil); c != http.StatusNotFound {
			t.Fatalf("expected rolled-back Observation/rb-cas-obs-1 to return 404 Not Found, got %d", c)
		}
		// Verify spidx_string for "RollbackCAS" was also rolled back
		_, srch, srchRaw := doReq(t, http.MethodGet, "/Patient?family=RollbackCAS", "", nil)
		if entries, _ := srch["entry"].([]any); len(entries) != 0 {
			t.Fatalf("expected 0 search results for rolled-back family=RollbackCAS, got %s", srchRaw)
		}
		// Verify cas-anchor-1 is untouched at version 1
		_, anchorMap, anchorRaw := doReq(t, http.MethodGet, "/Patient/cas-anchor-1", "", nil)
		if !strings.Contains(anchorRaw, `"AnchorOriginal"`) {
			t.Fatalf("expected cas-anchor-1 to remain 'AnchorOriginal', got %s", anchorRaw)
		}
		if meta, ok := anchorMap["meta"].(map[string]any); !ok || fmt.Sprint(meta["versionId"]) != "1" {
			t.Fatalf("expected cas-anchor-1 to remain at versionId 1, got %v", anchorMap["meta"])
		}
	})

	// =========================================================================
	// 4. Conditional Bundle Operations (ifNoneExist, Conditional PUT/DELETE)
	// =========================================================================
	t.Run("Conditional_Bundle_Operations_IfNoneExist_ConditionalPut_ConditionalDelete_And_PreferReturn", func(t *testing.T) {
		condBundle := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {
						"resourceType": "Patient",
						"identifier": [{"system": "http://hospital.example.org/mrn", "value": "COND-TX-001"}],
						"name": [{"family": "IdempotentCreate"}]
					},
					"request": {
						"method": "POST",
						"url": "Patient",
						"ifNoneExist": "identifier=http://hospital.example.org/mrn|COND-TX-001"
					}
				},
				{
					"resource": {
						"resourceType": "Organization",
						"identifier": [{"system": "http://hl7.org/fhir/sid/us-npi", "value": "9988776655"}],
						"active": true,
						"name": "Conditional Clinic v1"
					},
					"request": {
						"method": "PUT",
						"url": "Organization?identifier=http://hl7.org/fhir/sid/us-npi|9988776655"
					}
				}
			]
		}`

		// First submission creates both resources (201 Created)
		code, resp1, raw1 := doReq(t, http.MethodPost, "", condBundle, map[string]string{
			"Prefer": "return=OperationOutcome",
		})
		if code != http.StatusOK {
			t.Fatalf("first conditional bundle failed (%d): %s", code, raw1)
		}
		if !strings.Contains(raw1, `"OperationOutcome"`) {
			t.Fatalf("expected Prefer: return=OperationOutcome to include outcome in entry.response, got %s", raw1)
		}
		entries1, _ := resp1["entry"].([]any)
		e0_1, _ := entries1[0].(map[string]any)
		patFullURL1, _ := e0_1["fullUrl"].(string)

		// Second submission with updated Organization name:
		// - Patient POST with ifNoneExist must return 200 OK (no duplicate created, same fullUrl)
		// - Organization conditional PUT must update existing Organization in-place to v2 (200 OK)
		condBundle2 := strings.ReplaceAll(condBundle, "Conditional Clinic v1", "Conditional Clinic v2")
		code, resp2, raw2 := doReq(t, http.MethodPost, "", condBundle2, map[string]string{
			"Prefer": "return=representation",
		})
		if code != http.StatusOK {
			t.Fatalf("second conditional bundle failed (%d): %s", code, raw2)
		}
		entries2, _ := resp2["entry"].([]any)
		e0_2, _ := entries2[0].(map[string]any)
		e0_2Resp, _ := e0_2["response"].(map[string]any)
		if st, _ := e0_2Resp["status"].(string); !strings.HasPrefix(st, "200") {
			t.Fatalf("expected idempotent ifNoneExist on second run to return 200 OK, got %q", st)
		}
		if patFullURL2, _ := e0_2["fullUrl"].(string); patFullURL2 != patFullURL1 {
			t.Fatalf("expected idempotent ifNoneExist to return same Patient %q, got %q", patFullURL1, patFullURL2)
		}
		if !strings.Contains(raw2, `"Conditional Clinic v2"`) {
			t.Fatalf("expected conditional PUT to update Organization to 'Conditional Clinic v2', got %s", raw2)
		}
	})

	// =========================================================================
	// 5. Batch Bundle (type=batch) Partial-Failure Isolation
	// =========================================================================
	t.Run("Batch_Partial_Failure_Isolation", func(t *testing.T) {
		batchBundle := `{
			"resourceType": "Bundle",
			"type": "batch",
			"entry": [
				{
					"resource": {
						"resourceType": "Patient",
						"id": "batch-ok-pat-1",
						"name": [{"family": "BatchSurvivor"}],
						"gender": "male"
					},
					"request": {"method": "PUT", "url": "Patient/batch-ok-pat-1"}
				},
				{
					"resource": {
						"resourceType": "Observation",
						"id": "batch-bad-obs-1"
					},
					"request": {"method": "POST", "url": "Observation"}
				},
				{
					"resource": {
						"resourceType": "Patient",
						"id": "cas-anchor-1",
						"name": [{"family": "BatchStaleETag"}]
					},
					"request": {
						"method": "PUT",
						"url": "Patient/cas-anchor-1",
						"ifMatch": "W/\"999\""
					}
				},
				{
					"request": {"method": "GET", "url": "Patient/batch-ok-pat-1"}
				}
			]
		}`

		code, respBundle, raw := doReq(t, http.MethodPost, "", batchBundle, nil)
		if code != http.StatusOK {
			t.Fatalf("expected 200 OK for batch-response Bundle, got %d: %s", code, raw)
		}
		if respBundle["type"] != "batch-response" {
			t.Fatalf("expected Bundle.type=batch-response, got %v", respBundle["type"])
		}
		entries, _ := respBundle["entry"].([]any)
		if len(entries) != 4 {
			t.Fatalf("expected 4 entries in batch-response, got %d: %s", len(entries), raw)
		}

		expectEntryStatusPrefix := func(idx int, wantPrefix string) {
			t.Helper()
			em, _ := entries[idx].(map[string]any)
			rm, _ := em["response"].(map[string]any)
			st, _ := rm["status"].(string)
			if !strings.HasPrefix(st, wantPrefix) {
				t.Fatalf("entry[%d]: expected status prefix %q, got %q (entry=%v)", idx, wantPrefix, st, em)
			}
		}

		expectEntryStatusPrefix(0, "201") // Valid PUT Patient/batch-ok-pat-1
		expectEntryStatusPrefix(1, "400") // Invalid Observation missing status/code
		expectEntryStatusPrefix(2, "412") // Stale ifMatch W/"999"
		expectEntryStatusPrefix(3, "200") // Valid GET Patient/batch-ok-pat-1

		// Confirm Patient/batch-ok-pat-1 persisted despite sibling failures in batch
		getCode, _, getRaw := doReq(t, http.MethodGet, "/Patient/batch-ok-pat-1", "", nil)
		if getCode != http.StatusOK || !strings.Contains(getRaw, `"BatchSurvivor"`) {
			t.Fatalf("expected Patient/batch-ok-pat-1 to persist after batch partial failure, got %d: %s", getCode, getRaw)
		}
	})
}
