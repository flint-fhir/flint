package server_test

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestPatchEngines_RFC6902AndMergePatch(t *testing.T) {
	orig := []byte(`{
		"resourceType": "Patient",
		"id": "pat-1",
		"active": true,
		"gender": "male",
		"name": [{"family": "Smith", "given": ["John"]}],
		"telecom": [{"system": "phone", "value": "555-0100"}]
	}`)

	// 1. RFC 6902 JSON Patch: test, replace, add, copy, move, remove
	opsJSON := []byte(`[
		{"op": "test", "path": "/gender", "value": "male"},
		{"op": "replace", "path": "/gender", "value": "other"},
		{"op": "add", "path": "/name/0/given/-", "value": "Edward"},
		{"op": "copy", "from": "/name/0/family", "path": "/name/0/text"},
		{"op": "remove", "path": "/telecom/0"}
	]`)
	patched, status, err := server.ApplyJSONPatch(orig, opsJSON)
	if err != nil || status != http.StatusOK {
		t.Fatalf("ApplyJSONPatch failed (status=%d): %v", status, err)
	}

	var doc map[string]any
	if err := json.Unmarshal(patched, &doc); err != nil {
		t.Fatalf("unmarshal patched doc: %v", err)
	}
	if doc["gender"] != "other" {
		t.Errorf("expected gender=other, got %v", doc["gender"])
	}
	names := doc["name"].([]any)
	name0 := names[0].(map[string]any)
	if name0["text"] != "Smith" {
		t.Errorf("expected copied name[0].text=Smith, got %v", name0["text"])
	}
	givens := name0["given"].([]any)
	if len(givens) != 2 || givens[1] != "Edward" {
		t.Errorf("expected given=[John, Edward], got %v", givens)
	}
	telecoms := doc["telecom"].([]any)
	if len(telecoms) != 0 {
		t.Errorf("expected empty telecom array after remove, got %v", telecoms)
	}

	// 2. RFC 6902 test failure returns 422 Unprocessable Entity
	failOpsJSON := []byte(`[{"op": "test", "path": "/gender", "value": "female"}]`)
	if _, failStatus, err := server.ApplyJSONPatch(orig, failOpsJSON); err == nil || failStatus != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 error on failed test op, got status=%d err=%v", failStatus, err)
	}

	// 3. RFC 7396 JSON Merge Patch: update scalar, add nested object, delete field with null
	mergePatch := []byte(`{
		"active": false,
		"gender": null,
		"birthDate": "1980-01-01"
	}`)
	merged, err := server.ApplyMergePatch(orig, mergePatch)
	if err != nil {
		t.Fatalf("ApplyMergePatch failed: %v", err)
	}
	var mergedDoc map[string]any
	if err := json.Unmarshal(merged, &mergedDoc); err != nil {
		t.Fatalf("unmarshal merged doc: %v", err)
	}
	if mergedDoc["active"] != false {
		t.Errorf("expected active=false, got %v", mergedDoc["active"])
	}
	if _, exists := mergedDoc["gender"]; exists {
		t.Errorf("expected gender to be removed by null in merge patch")
	}
	if mergedDoc["birthDate"] != "1980-01-01" {
		t.Errorf("expected birthDate=1980-01-01, got %v", mergedDoc["birthDate"])
	}
}

func TestRegisterDefaultResourceTypesAndOpenAPISchemas(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(nil, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Check /metadata CapabilityStatement lists all 11 US Core resources + patch + conditionalUpdate/Delete
	resp, err := http.Get(ts.URL + "/fhir/r4/demo/metadata")
	if err != nil {
		t.Fatalf("GET /metadata: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metadata status=%d: %s", resp.StatusCode, body)
	}
	var capStmt map[string]any
	if err := json.Unmarshal(body, &capStmt); err != nil {
		t.Fatalf("unmarshal CapabilityStatement: %v", err)
	}
	restArr := capStmt["rest"].([]any)
	rest0 := restArr[0].(map[string]any)
	resArr := rest0["resource"].([]any)
	foundResources := make(map[string]map[string]any)
	for _, r := range resArr {
		rm := r.(map[string]any)
		foundResources[rm["type"].(string)] = rm
	}

	expectedUSCore := []string{
		"Patient", "Condition", "Encounter", "Observation", "Practitioner",
		"MedicationRequest", "AllergyIntolerance", "Procedure", "DiagnosticReport", "Immunization", "Organization",
	}
	for _, rt := range expectedUSCore {
		rm, ok := foundResources[rt]
		if !ok {
			t.Errorf("expected CapabilityStatement to include US Core resource %q", rt)
			continue
		}
		if rm["conditionalUpdate"] != true {
			t.Errorf("expected %s conditionalUpdate=true, got %v", rt, rm["conditionalUpdate"])
		}
		if rm["conditionalDelete"] != "single" {
			t.Errorf("expected %s conditionalDelete=single, got %v", rt, rm["conditionalDelete"])
		}
	}

	// 2. Check /openapi.json includes all 11 proto-compiled schemas and PATCH operation
	resp, err = http.Get(ts.URL + "/fhir/r4/demo/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	oaBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var oaSpec map[string]any
	if err := json.Unmarshal(oaBody, &oaSpec); err != nil {
		t.Fatalf("unmarshal OpenAPI: %v", err)
	}
	schemas := oaSpec["components"].(map[string]any)["schemas"].(map[string]any)
	for _, rt := range expectedUSCore {
		if _, ok := schemas[rt]; !ok {
			t.Errorf("expected OpenAPI components.schemas to contain proto-compiled schema for %q", rt)
		}
	}
	paths := oaSpec["paths"].(map[string]any)
	patIdItem := paths["/Patient/{id}"].(map[string]any)
	if _, hasPatch := patIdItem["patch"]; !hasPatch {
		t.Errorf("expected OpenAPI /Patient/{id} to expose patch operation")
	}

	// 3. Validate choice-type and ValueSet rules on new US Core resources via /$validate
	validateCases := []struct {
		name         string
		resourceType string
		payload      string
		wantStatus   int
	}{
		{
			name:         "valid MedicationRequest with medicationCodeableConcept",
			resourceType: "MedicationRequest",
			payload:      `{"resourceType":"MedicationRequest","status":"active","intent":"order","subject":{"reference":"Patient/p1"},"medicationCodeableConcept":{"coding":[{"system":"http://www.nlm.nih.gov/research/umls/rxnorm","code":"1049502"}]}}`,
			wantStatus:   http.StatusOK,
		},
		{
			name:         "invalid MedicationRequest missing medication[x]",
			resourceType: "MedicationRequest",
			payload:      `{"resourceType":"MedicationRequest","status":"active","intent":"order","subject":{"reference":"Patient/p1"}}`,
			wantStatus:   http.StatusBadRequest,
		},
		{
			name:         "valid Immunization with occurrenceDateTime",
			resourceType: "Immunization",
			payload:      `{"resourceType":"Immunization","status":"completed","patient":{"reference":"Patient/p1"},"vaccineCode":{"coding":[{"system":"http://hl7.org/fhir/sid/cvx","code":"207"}]},"occurrenceDateTime":"2026-01-15"}`,
			wantStatus:   http.StatusOK,
		},
		{
			name:         "invalid Immunization missing occurrence[x]",
			resourceType: "Immunization",
			payload:      `{"resourceType":"Immunization","status":"completed","patient":{"reference":"Patient/p1"},"vaccineCode":{"coding":[{"system":"http://hl7.org/fhir/sid/cvx","code":"207"}]}}`,
			wantStatus:   http.StatusBadRequest,
		},
		{
			name:         "valid Organization",
			resourceType: "Organization",
			payload:      `{"resourceType":"Organization","active":true,"name":"Flint Medical Center"}`,
			wantStatus:   http.StatusOK,
		},
	}

	for _, tc := range validateCases {
		res, err := http.Post(ts.URL+"/fhir/r4/demo/"+tc.resourceType+"/$validate", "application/fhir+json", strings.NewReader(tc.payload))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		res.Body.Close()
		if res.StatusCode != tc.wantStatus {
			t.Errorf("%s: expected status %d, got %d", tc.name, tc.wantStatus, res.StatusCode)
		}
	}
}

func openTestPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("skipping postgres integration test: %v", err)
	}
	if err := db.PingContext(t.Context()); err != nil {
		db.Close()
		t.Skipf("skipping postgres integration test (postgres unreachable): %v", err)
	}
	return db
}

func TestServer_USCoreResourcesEndToEnd(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-uscore-e2e"
	cleanup := func() {
		_ = postgres.PurgeTenant(ctx, db, tenant)
	}
	cleanup()
	defer cleanup()

	store := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(store, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Seed all 6 new US Core resources + Patient via Transaction Bundle
	bundleJSON := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"resource": {
					"resourceType": "Organization",
					"id": "org-1",
					"active": true,
					"name": "Flint General Hospital",
					"alias": ["FGH Clinic"],
					"identifier": [{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1112223334"}],
					"address": [{"city": "Boston", "state": "MA", "postalCode": "02115"}]
				},
				"request": {"method": "PUT", "url": "Organization/org-1"}
			},
			{
				"resource": {
					"resourceType": "Patient",
					"id": "pat-uscore-1",
					"active": true,
					"gender": "female",
					"name": [{"family": "USCore", "given": ["Jane"]}],
					"managingOrganization": {"reference": "Organization/org-1"}
				},
				"request": {"method": "PUT", "url": "Patient/pat-uscore-1"}
			},
			{
				"resource": {
					"resourceType": "MedicationRequest",
					"id": "medreq-1",
					"status": "active",
					"intent": "order",
					"subject": {"reference": "Patient/pat-uscore-1"},
					"authoredOn": "2026-02-01T10:00:00Z",
					"medicationCodeableConcept": {
						"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "1049502", "display": "Acetaminophen 325 MG"}]
					}
				},
				"request": {"method": "PUT", "url": "MedicationRequest/medreq-1"}
			},
			{
				"resource": {
					"resourceType": "AllergyIntolerance",
					"id": "allergy-1",
					"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}]},
					"verificationStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification", "code": "confirmed"}]},
					"criticality": "high",
					"code": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980", "display": "Penicillin G"}]},
					"patient": {"reference": "Patient/pat-uscore-1"}
				},
				"request": {"method": "PUT", "url": "AllergyIntolerance/allergy-1"}
			},
			{
				"resource": {
					"resourceType": "Procedure",
					"id": "proc-1",
					"status": "completed",
					"code": {"coding": [{"system": "http://snomed.info/sct", "code": "80146002", "display": "Appendectomy"}]},
					"subject": {"reference": "Patient/pat-uscore-1"},
					"performedDateTime": "2026-01-20T14:00:00Z"
				},
				"request": {"method": "PUT", "url": "Procedure/proc-1"}
			},
			{
				"resource": {
					"resourceType": "DiagnosticReport",
					"id": "dr-1",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "LAB"}]}],
					"code": {"coding": [{"system": "http://loinc.org", "code": "58410-2", "display": "CBC panel"}]},
					"subject": {"reference": "Patient/pat-uscore-1"},
					"effectiveDateTime": "2026-02-02T08:00:00Z",
					"issued": "2026-02-02T09:00:00Z",
					"performer": [{"reference": "Organization/org-1"}]
				},
				"request": {"method": "PUT", "url": "DiagnosticReport/dr-1"}
			},
			{
				"resource": {
					"resourceType": "Immunization",
					"id": "imm-1",
					"status": "completed",
					"vaccineCode": {"coding": [{"system": "http://hl7.org/fhir/sid/cvx", "code": "207", "display": "COVID-19, mRNA"}]},
					"patient": {"reference": "Patient/pat-uscore-1"},
					"occurrenceDateTime": "2026-01-10T09:00:00Z"
				},
				"request": {"method": "PUT", "url": "Immunization/imm-1"}
			}
		]
	}`

	resp, err := http.Post(ts.URL+"/fhir/r4/"+tenant, "application/fhir+json", strings.NewReader(bundleJSON))
	if err != nil {
		t.Fatalf("POST bundle: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST bundle status=%d: %s", resp.StatusCode, respBody)
	}

	// Verify US Core search queries across all 6 new resource types
	searchQueries := []struct {
		query     string
		wantTotal float64
	}{
		{"MedicationRequest?patient=Patient/pat-uscore-1&intent=order&code=1049502", 1},
		{"AllergyIntolerance?patient=Patient/pat-uscore-1&clinical-status=active&criticality=high", 1},
		{"AllergyIntolerance?subject=Patient/pat-uscore-1", 1}, // subject <-> patient alias
		{"Procedure?patient=Patient/pat-uscore-1&status=completed&code=80146002", 1},
		{"DiagnosticReport?patient=Patient/pat-uscore-1&code=58410-2&status=final", 1},
		{"Immunization?patient=Patient/pat-uscore-1&vaccine-code=207&status=completed", 1},
		{"Organization?name=FGH&address-city=Boston", 1}, // matches Organization.alias
	}

	for _, sq := range searchQueries {
		res, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/" + sq.query)
		if err != nil {
			t.Fatalf("GET %s: %v", sq.query, err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status=%d: %s", sq.query, res.StatusCode, b)
		}
		var bundle map[string]any
		if err := json.Unmarshal(b, &bundle); err != nil {
			t.Fatalf("unmarshal search bundle for %s: %v", sq.query, err)
		}
		if bundle["total"] != sq.wantTotal {
			t.Errorf("GET %s expected total=%v, got %v", sq.query, sq.wantTotal, bundle["total"])
		}
	}

	// Verify Patient/$everything includes all 5 new clinical compartment resources + referenced Organization
	evResp, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Patient/pat-uscore-1/$everything")
	if err != nil {
		t.Fatalf("GET $everything: %v", err)
	}
	evBody, _ := io.ReadAll(evResp.Body)
	evResp.Body.Close()
	if evResp.StatusCode != http.StatusOK {
		t.Fatalf("GET $everything status=%d: %s", evResp.StatusCode, evBody)
	}
	var evBundle map[string]any
	if err := json.Unmarshal(evBody, &evBundle); err != nil {
		t.Fatalf("unmarshal $everything: %v", err)
	}
	if evBundle["total"] != float64(7) {
		t.Errorf("expected $everything total=7 (Patient + 5 clinical + 1 Organization), got %v", evBundle["total"])
	}
}

func TestServer_PatchAndConditionalCRUD(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-patch-cond"
	cleanup := func() {
		_ = postgres.PurgeTenant(ctx, db, tenant)
	}
	cleanup()
	defer cleanup()

	store := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(store, logger)
	server.RegisterDefaultResourceTypes(srv)

	mockAuth, err := auth.NewMockTokenValidator("https://auth.example.com", "flint-api")
	if err != nil {
		t.Fatalf("new mock validator: %v", err)
	}
	srv.SetTokenValidator(mockAuth)

	sysToken := "sys-admin-token"
	mockAuth.RegisterStatic(sysToken, &authv1.SecurityContext{
		Subject:  "sys-admin",
		TenantId: tenant,
		Scopes:   auth.ParseScopes("system/*.cruds"),
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	doReq := func(method, path, contentType, bodyStr, token string, headers map[string]string) (*http.Response, []byte) {
		var bodyReader io.Reader
		if bodyStr != "" {
			bodyReader = strings.NewReader(bodyStr)
		}
		req, err := http.NewRequest(method, ts.URL+path, bodyReader)
		if err != nil {
			t.Fatalf("new request %s %s: %v", method, path, err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request %s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	// 1. Conditional Update (0 matches -> 201 Created)
	orgBody1 := `{"resourceType":"Organization","active":true,"name":"Acme Health v1","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"9998887776"}]}`
	resp, b := doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Organization?identifier=http://hl7.org/fhir/sid/us-npi|9998887776", "application/fhir+json", orgBody1, sysToken, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("conditional PUT (0 matches) expected 201, got %d: %s", resp.StatusCode, b)
	}
	var createdOrg map[string]any
	json.Unmarshal(b, &createdOrg)
	orgID := createdOrg["id"].(string)
	if resp.Header.Get("ETag") != `W/"1"` {
		t.Errorf("expected ETag W/\"1\", got %q", resp.Header.Get("ETag"))
	}

	// 2. Conditional Update (1 match -> 200 OK, updates same ID in place to version 2)
	orgBody2 := `{"resourceType":"Organization","active":true,"name":"Acme Health v2","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"9998887776"}],"address":[{"city":"Seattle"}]}`
	resp, b = doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Organization?identifier=http://hl7.org/fhir/sid/us-npi|9998887776", "application/fhir+json", orgBody2, sysToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("conditional PUT (1 match) expected 200, got %d: %s", resp.StatusCode, b)
	}
	var updatedOrg map[string]any
	json.Unmarshal(b, &updatedOrg)
	if updatedOrg["id"] != orgID {
		t.Errorf("expected conditional update to preserve id %q, got %v", orgID, updatedOrg["id"])
	}
	if resp.Header.Get("ETag") != `W/"2"` {
		t.Errorf("expected ETag W/\"2\", got %q", resp.Header.Get("ETag"))
	}

	// 3. RFC 6902 JSON Patch on Organization with If-Match W/"2" -> W/"3"
	jsonPatch := `[
		{"op": "test", "path": "/name", "value": "Acme Health v2"},
		{"op": "replace", "path": "/name", "value": "Acme Health Patched"},
		{"op": "add", "path": "/address/0/state", "value": "WA"}
	]`
	resp, b = doReq(http.MethodPatch, "/fhir/r4/"+tenant+"/Organization/"+orgID, "application/json-patch+json", jsonPatch, sysToken, map[string]string{"If-Match": `W/"2"`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH (RFC 6902) expected 200, got %d: %s", resp.StatusCode, b)
	}
	if resp.Header.Get("ETag") != `W/"3"` {
		t.Errorf("expected ETag W/\"3\" after PATCH, got %q", resp.Header.Get("ETag"))
	}

	// Verify re-indexing after PATCH: Organization?address-state=WA should find it
	_, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Organization?address-state=WA", "", "", sysToken, nil)
	var searchBundle map[string]any
	json.Unmarshal(b, &searchBundle)
	if searchBundle["total"] != float64(1) {
		t.Errorf("expected search after PATCH to match address-state=WA, got total=%v", searchBundle["total"])
	}

	// 4. Stale If-Match on PATCH returns 412 Precondition Failed
	resp, _ = doReq(http.MethodPatch, "/fhir/r4/"+tenant+"/Organization/"+orgID, "application/json-patch+json", `[{"op":"replace","path":"/name","value":"Stale"}]`, sysToken, map[string]string{"If-Match": `W/"1"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("expected 412 Precondition Failed on stale If-Match, got %d", resp.StatusCode)
	}

	// 5. Mutating immutable /id via PATCH returns 400 Bad Request
	resp, _ = doReq(http.MethodPatch, "/fhir/r4/"+tenant+"/Organization/"+orgID, "application/json-patch+json", `[{"op":"replace","path":"/id","value":"hacked-id"}]`, sysToken, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 when patching immutable id, got %d", resp.StatusCode)
	}

	// 6. RFC 7396 JSON Merge Patch on Organization -> W/"4"
	mergePatch := `{"name": "Acme Health MergePatched"}`
	resp, b = doReq(http.MethodPatch, "/fhir/r4/"+tenant+"/Organization/"+orgID, "application/merge-patch+json", mergePatch, sysToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH (Merge Patch) expected 200, got %d: %s", resp.StatusCode, b)
	}
	if resp.Header.Get("ETag") != `W/"4"` {
		t.Errorf("expected ETag W/\"4\", got %q", resp.Header.Get("ETag"))
	}

	// 7. Conditional Delete (1 match -> 204 No Content, then 0 matches -> 204 No Content)
	resp, b = doReq(http.MethodDelete, "/fhir/r4/"+tenant+"/Organization?identifier=http://hl7.org/fhir/sid/us-npi|9998887776", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("conditional DELETE (1 match) expected 204, got %d: %s", resp.StatusCode, b)
	}
	// Verify GET /Organization/{id} is now 410 Gone
	resp, _ = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Organization/"+orgID, "", "", sysToken, nil)
	if resp.StatusCode != http.StatusGone {
		t.Errorf("expected GET after conditional DELETE to return 410 Gone, got %d", resp.StatusCode)
	}
	// Repeat conditional DELETE (0 matches) -> idempotent 204 No Content
	resp, _ = doReq(http.MethodDelete, "/fhir/r4/"+tenant+"/Organization?identifier=http://hl7.org/fhir/sid/us-npi|9998887776", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("conditional DELETE (0 matches) expected idempotent 204, got %d", resp.StatusCode)
	}

	// 8. Conditional Update/Delete with >1 matches returns 412 Precondition Failed
	doReq(http.MethodPost, "/fhir/r4/"+tenant+"/Organization", "application/fhir+json", `{"resourceType":"Organization","name":"DupOrg","address":[{"city":"DupCity"}]}`, sysToken, nil)
	doReq(http.MethodPost, "/fhir/r4/"+tenant+"/Organization", "application/fhir+json", `{"resourceType":"Organization","name":"DupOrg","address":[{"city":"DupCity"}]}`, sysToken, nil)

	resp, _ = doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Organization?address-city=DupCity", "application/fhir+json", `{"resourceType":"Organization","name":"DupOrgUpdated"}`, sysToken, nil)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("expected 412 Precondition Failed on conditional PUT with >1 matches, got %d", resp.StatusCode)
	}
	resp, _ = doReq(http.MethodDelete, "/fhir/r4/"+tenant+"/Organization?address-city=DupCity", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("expected 412 Precondition Failed on conditional DELETE with >1 matches, got %d", resp.StatusCode)
	}

	// 9. Verify SMART Patient Compartment Isolation on PATCH
	doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Patient/pat-a", "application/fhir+json", `{"resourceType":"Patient","id":"pat-a","gender":"female"}`, sysToken, nil)
	doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Patient/pat-b", "application/fhir+json", `{"resourceType":"Patient","id":"pat-b","gender":"male"}`, sysToken, nil)
	doReq(http.MethodPut, "/fhir/r4/"+tenant+"/MedicationRequest/mr-b", "application/fhir+json", `{"resourceType":"MedicationRequest","id":"mr-b","status":"active","intent":"order","subject":{"reference":"Patient/pat-b"},"medicationCodeableConcept":{"coding":[{"system":"http://www.nlm.nih.gov/research/umls/rxnorm","code":"1049502"}]}}`, sysToken, nil)

	patAToken := "pat-a-token"
	mockAuth.RegisterStatic(patAToken, &authv1.SecurityContext{
		Subject:   "user-pat-a",
		TenantId:  tenant,
		PatientId: "pat-a",
		Scopes:    auth.ParseScopes("patient/MedicationRequest.u patient/Patient.u"),
	})

	// Patient A trying to PATCH Patient B's MedicationRequest -> 403 Forbidden
	resp, _ = doReq(http.MethodPatch, "/fhir/r4/"+tenant+"/MedicationRequest/mr-b", "application/merge-patch+json", `{"status":"completed"}`, patAToken, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden when Patient A patches Patient B's MedicationRequest, got %d", resp.StatusCode)
	}

	// Patient A trying to PATCH Patient A -> 200 OK
	resp, _ = doReq(http.MethodPatch, "/fhir/r4/"+tenant+"/Patient/pat-a", "application/merge-patch+json", `{"active":true}`, patAToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK when Patient A patches own Patient resource, got %d", resp.StatusCode)
	}
}
