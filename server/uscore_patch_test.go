package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/fhirutil"
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

	// 4. Verify Subscription SSRF endpoint validation
	if err := server.ValidateSubscriptionEndpoint("http://169.254.169.254/latest/meta-data/", false); err == nil {
		t.Errorf("expected link-local cloud metadata IP 169.254.169.254 to be blocked")
	}
	if err := server.ValidateSubscriptionEndpoint("http://metadata.google.internal/computeMetadata/v1/", false); err == nil {
		t.Errorf("expected metadata.google.internal to be blocked")
	}
	if err := server.ValidateSubscriptionEndpoint("ftp://example.org/hook", false); err == nil {
		t.Errorf("expected non-http(s) scheme ftp:// to be blocked")
	}
	if err := server.ValidateSubscriptionEndpoint("http://127.0.0.1:8080/hook", false); err != nil {
		t.Errorf("expected loopback webhook to be allowed by default for local testing, got %v", err)
	}
	if err := server.ValidateSubscriptionEndpoint("http://127.0.0.1:8080/hook", true); err == nil {
		t.Errorf("expected loopback webhook to be blocked when disallowPrivate is true")
	}
	if err := server.ValidateSubscriptionEndpoint("http://10.0.0.1/hook", true); err == nil {
		t.Errorf("expected RFC1918 private IP webhook to be blocked when disallowPrivate is true")
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

	// 10. Multi-hop chained search (Observation?patient.organization.name=HopOrg), Encounter/$everything, and Composition/$document 2nd-hop resolution
	doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Organization/org-hop", "application/fhir+json", `{"resourceType":"Organization","id":"org-hop","active":true,"name":"HopOrg Medical"}`, sysToken, nil)
	doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Patient/pat-hop", "application/fhir+json", `{"resourceType":"Patient","id":"pat-hop","gender":"female","managingOrganization":{"reference":"Organization/org-hop"}}`, sysToken, nil)
	doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Encounter/enc-hop", "application/fhir+json", `{"resourceType":"Encounter","id":"enc-hop","status":"finished","class":{"system":"http://terminology.hl7.org/CodeSystem/v3-ActCode","code":"AMB"},"subject":{"reference":"Patient/pat-hop"},"serviceProvider":{"reference":"Organization/org-hop"}}`, sysToken, nil)
	doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Observation/obs-hop", "application/fhir+json", `{"resourceType":"Observation","id":"obs-hop","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/pat-hop"},"encounter":{"reference":"Encounter/enc-hop"}}`, sysToken, nil)
	doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Composition/comp-hop", "application/fhir+json", `{"resourceType":"Composition","id":"comp-hop","status":"final","type":{"coding":[{"system":"http://loinc.org","code":"11502-2"}]},"date":"2026-01-15T10:00:00Z","title":"Summary","subject":{"reference":"Patient/pat-hop"},"author":[{"reference":"Organization/org-hop"}],"section":[{"title":"Vitals","entry":[{"reference":"Observation/obs-hop"}]}]}`, sysToken, nil)

	resp, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Observation?patient.organization.name=HopOrg", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("multi-hop chained search expected 200, got %d: %s", resp.StatusCode, b)
	}
	var hopBundle map[string]any
	_ = json.Unmarshal(b, &hopBundle)
	if hopBundle["total"] != float64(1) {
		t.Errorf("expected multi-hop chained search Observation?patient.organization.name=HopOrg to return total=1, got %v", hopBundle["total"])
	}

	resp, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Encounter/enc-hop/$everything", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Encounter/$everything expected 200, got %d: %s", resp.StatusCode, b)
	}
	var encEvBundle map[string]any
	_ = json.Unmarshal(b, &encEvBundle)
	if total, _ := encEvBundle["total"].(float64); total < 3 {
		t.Errorf("expected Encounter/$everything to include Encounter, Patient, Organization, and Observation (total >= 3), got %v", encEvBundle["total"])
	}

	resp, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Composition/comp-hop/$document", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Composition/$document expected 200, got %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `"Encounter/enc-hop"`) {
		t.Errorf("expected Composition/$document to include 2nd-hop referenced Encounter/enc-hop from Observation/obs-hop, got %s", b)
	}

	// 11. Da Vinci DEQM Measure/$care-gaps, Measure/$evaluate-measure reportType=subject-list, and Coverage?subscriber-id=...
	doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Condition/cond-htn-hop", "application/fhir+json", `{"resourceType":"Condition","id":"cond-htn-hop","clinicalStatus":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/condition-clinical","code":"active"}]},"code":{"coding":[{"system":"http://snomed.info/sct","code":"38341003","display":"Essential hypertension"}]},"subject":{"reference":"Patient/pat-hop"}}`, sysToken, nil)
	doReq(http.MethodPut, "/fhir/r4/"+tenant+"/Coverage/cov-hop", "application/fhir+json", `{"resourceType":"Coverage","id":"cov-hop","status":"active","subscriberId":"SUB-HOP-777","beneficiary":{"reference":"Patient/pat-hop"},"payor":[{"reference":"Organization/org-hop"}]}`, sysToken, nil)

	// Patient/pat-hop has Hypertension + Encounter/enc-hop, but no BP reading -> open-gap on CMS165!
	resp, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Measure/$care-gaps?subject=Patient/pat-hop&status=open-gap", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Measure/$care-gaps expected 200, got %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `"open-gap"`) || !strings.Contains(string(b), `"care-gap-cms165-bp-control-pat-hop"`) {
		t.Errorf("expected Measure/$care-gaps to report open-gap for CMS165 on pat-hop, got %s", b)
	}

	// Measure/$evaluate-measure with reportType=subject-list returns contained List resources and subjectResults
	resp, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Measure/cms165-bp-control/$evaluate-measure?reportType=subject-list", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Measure/$evaluate-measure?reportType=subject-list expected 200, got %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `"subject-list"`) || !strings.Contains(string(b), `"#denominator-list"`) || !strings.Contains(string(b), `"Patient/pat-hop"`) {
		t.Errorf("expected subject-list MeasureReport to include #denominator-list with Patient/pat-hop, got %s", b)
	}

	// Coverage?subscriber-id=SUB-HOP-777 hits spidx_token
	resp, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Coverage?subscriber-id=SUB-HOP-777", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"cov-hop"`) {
		t.Errorf("expected Coverage?subscriber-id=SUB-HOP-777 to match cov-hop, got %d: %s", resp.StatusCode, b)
	}

	// 12. Wave 8: Conditional PATCH + FHIRPath Patch (Parameters) + Multiple Conditional DELETE (_count) + Composition/$document?persist=true + $everything _typeFilter
	fhirPatchParams := `{
		"resourceType": "Parameters",
		"parameter": [
			{
				"name": "operation",
				"part": [
					{"name": "type", "valueCode": "replace"},
					{"name": "path", "valueString": "Organization.name"},
					{"name": "value", "valueString": "HopOrg FHIRPatched"}
				]
			}
		]
	}`
	resp, b = doReq(http.MethodPatch, "/fhir/r4/"+tenant+"/Organization?_id=org-hop", "application/fhir+json", fhirPatchParams, sysToken, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"HopOrg FHIRPatched"`) {
		t.Fatalf("conditional PATCH with FHIRPath Parameters expected 200 OK with HopOrg FHIRPatched, got %d: %s", resp.StatusCode, b)
	}
	// Conditional PATCH with 0 matches -> 404 Not Found
	resp, _ = doReq(http.MethodPatch, "/fhir/r4/"+tenant+"/Organization?_id=nonexistent-org-999", "application/fhir+json", fhirPatchParams, sysToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 Not Found on conditional PATCH with 0 matches, got %d", resp.StatusCode)
	}
	// Conditional PATCH with >1 matches (DupCity) -> 412 Precondition Failed
	resp, _ = doReq(http.MethodPatch, "/fhir/r4/"+tenant+"/Organization?address-city=DupCity", "application/fhir+json", fhirPatchParams, sysToken, nil)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("expected 412 Precondition Failed on conditional PATCH with >1 matches, got %d", resp.StatusCode)
	}

	// Multiple Conditional DELETE with _count=10 and Prefer: return=OperationOutcome on DupCity (2 matches)
	resp, b = doReq(http.MethodDelete, "/fhir/r4/"+tenant+"/Organization?address-city=DupCity&_count=10", "", "", sysToken, map[string]string{"Prefer": "return=OperationOutcome"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "Conditional delete soft-deleted 2 Organization resource(s)") {
		t.Fatalf("expected 200 OK OperationOutcome deleting 2 DupCity Organizations with _count=10, got %d: %s", resp.StatusCode, b)
	}

	// Composition/$document?persist=true persists the assembled Bundle and sets Location + ETag
	resp, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Composition/comp-hop/$document?persist=true", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Composition/$document?persist=true expected 200, got %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(resp.Header.Get("Location"), "/Bundle/doc-comp-hop") || !strings.Contains(resp.Header.Get("Content-Location"), "/Bundle/doc-comp-hop/_history/1") {
		t.Errorf("expected Location and Content-Location headers for persisted document Bundle, got Location=%q Content-Location=%q", resp.Header.Get("Location"), resp.Header.Get("Content-Location"))
	}
	resp, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Bundle/doc-comp-hop", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"type":"document"`) {
		t.Errorf("expected persisted Bundle/doc-comp-hop to be readable via GET /Bundle/doc-comp-hop, got %d: %s", resp.StatusCode, b)
	}

	// Patient/$everything and Encounter/$everything with _typeFilter
	resp, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-hop/$everything?_type=Observation&_typeFilter=Observation%3Fcode%3D8867-4", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"obs-hop"`) {
		t.Errorf("expected Patient/$everything with matching _typeFilter to include obs-hop, got %d: %s", resp.StatusCode, b)
	}
	resp, b = doReq(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-hop/$everything?_type=Observation&_typeFilter=Observation%3Fcode%3Dnonexistent-loinc", "", "", sysToken, nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(b), `"obs-hop"`) {
		t.Errorf("expected Patient/$everything with non-matching _typeFilter to exclude obs-hop, got %d: %s", resp.StatusCode, b)
	}
}

func TestBundleOrderingConditionalRefsPreferAndSMART(t *testing.T) {
	srv := server.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	// 1. Transaction Bundle with intra-bundle conditional reference & Prefer: return=OperationOutcome
	// Entry 0 (PUT Observation) references Patient?identifier=http://hospital.org|mrn-777 which is created by Entry 1 (POST Patient).
	// Because transaction ordering runs POST (priority 2) before PUT (priority 3), Entry 1 is indexed first
	// and Entry 0 resolves the conditional reference to Patient/pat-cond-777!
	txBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"request": {"method": "PUT", "url": "Observation/obs-cond-1"},
				"resource": {
					"resourceType": "Observation",
					"id": "obs-cond-1",
					"status": "final",
					"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
					"subject": {"reference": "Patient?identifier=http://hospital.org|mrn-777"}
				}
			},
			{
				"request": {"method": "POST", "url": "Patient"},
				"resource": {
					"resourceType": "Patient",
					"id": "pat-cond-777",
					"identifier": [{"system": "http://hospital.org", "value": "mrn-777"}],
					"name": [{"family": "CondTarget"}]
				}
			},
			{
				"request": {"method": "GET", "url": "metadata"}
			}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/fhir/r4/default", strings.NewReader(txBundle))
	req.Header.Set("Content-Type", "application/fhir+json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on transaction bundle with conditional reference and GET entry, got %d: %s", rec.Code, rec.Body.String())
	}
	var bundleResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &bundleResp); err != nil {
		t.Fatalf("unmarshal bundle response: %v", err)
	}
	entries, _ := bundleResp["entry"].([]any)
	if len(entries) == 0 {
		t.Fatalf("expected non-empty bundle response entries, got %v", bundleResp)
	}
	firstEntry, _ := entries[0].(map[string]any)
	firstRes, _ := firstEntry["resource"].(map[string]any)
	if firstRes == nil || firstRes["resourceType"] != "CapabilityStatement" {
		t.Fatalf("expected GET metadata entry in transaction bundle to return CapabilityStatement resource, got %v", firstEntry)
	}

	// 2. SMART v2 OAuth2 redirect_uri binding, client_id binding, and OIDC id_token with aud + nonce
	authReq := httptest.NewRequest(http.MethodGet, "/oauth2/authorize?response_type=code&client_id=my-smart-app&redirect_uri=https://app.example.com/cb&scope=openid+fhirUser+patient/Patient.rs&nonce=nonce-xyz-123&state=st-1", nil)
	authRec := httptest.NewRecorder()
	h.ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusFound {
		t.Fatalf("expected 302 from /oauth2/authorize, got %d: %s", authRec.Code, authRec.Body.String())
	}
	locURL, err := http.NewRequest(http.MethodGet, authRec.Header().Get("Location"), nil)
	if err != nil {
		t.Fatalf("parse redirect Location: %v", err)
	}
	code := locURL.URL.Query().Get("code")
	if code == "" {
		t.Fatalf("expected code in redirect Location %q", authRec.Header().Get("Location"))
	}

	// Mismatched redirect_uri must fail with 400 invalid_grant (before consuming a valid exchange? Note: code is single-use, so let's issue a code specifically to test mismatch)
	authReqMismatch := httptest.NewRequest(http.MethodGet, "/oauth2/authorize?response_type=code&client_id=my-smart-app&redirect_uri=https://app.example.com/cb", nil)
	authRecMismatch := httptest.NewRecorder()
	h.ServeHTTP(authRecMismatch, authReqMismatch)
	locMismatch, _ := http.NewRequest(http.MethodGet, authRecMismatch.Header().Get("Location"), nil)
	badCode := locMismatch.URL.Query().Get("code")

	tokMismatchReq := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader("grant_type=authorization_code&code="+badCode+"&redirect_uri=https://evil.example.com/cb"))
	tokMismatchReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokMismatchRec := httptest.NewRecorder()
	h.ServeHTTP(tokMismatchRec, tokMismatchReq)
	if tokMismatchRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on mismatched redirect_uri, got %d: %s", tokMismatchRec.Code, tokMismatchRec.Body.String())
	}

	// Valid token exchange with matching redirect_uri and client_id
	tokReq := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader("grant_type=authorization_code&code="+code+"&client_id=my-smart-app&redirect_uri=https://app.example.com/cb"))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokRec := httptest.NewRecorder()
	h.ServeHTTP(tokRec, tokReq)
	if tokRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /oauth2/token, got %d: %s", tokRec.Code, tokRec.Body.String())
	}
	var tokResp map[string]any
	if err := json.Unmarshal(tokRec.Body.Bytes(), &tokResp); err != nil {
		t.Fatalf("unmarshal token response: %v", err)
	}
	idTokenStr, _ := tokResp["id_token"].(string)
	accessTokenStr, _ := tokResp["access_token"].(string)
	if idTokenStr == "" || idTokenStr == accessTokenStr {
		t.Fatalf("expected distinct OIDC id_token, got id_token=%q access_token=%q", idTokenStr, accessTokenStr)
	}
	parsedIDToken, err := auth.ParseJWT(idTokenStr)
	if err != nil {
		t.Fatalf("ParseJWT(id_token): %v", err)
	}
	if !parsedIDToken.Payload.Audience.Contains("my-smart-app") {
		t.Errorf("expected id_token aud to contain my-smart-app, got %v", parsedIDToken.Payload.Audience)
	}
	if parsedIDToken.Payload.Extra["nonce"] != "nonce-xyz-123" {
		t.Errorf("expected id_token nonce=nonce-xyz-123, got %v", parsedIDToken.Payload.Extra["nonce"])
	}
}

func TestMultiHopChainedScopeAndEncounterEverything(t *testing.T) {
	srv := server.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	mockAuth, err := auth.NewMockTokenValidator("https://auth.example.com", "flint-api")
	if err != nil {
		t.Fatalf("new mock validator: %v", err)
	}
	srv.SetTokenValidator(mockAuth)
	h := srv.Handler()

	// 1. Multi-hop chained search SMART scope check:
	// Observation?patient.organization.name=Acme requires scope on Observation, Patient (hop 1), and Organization (hop 2).
	partialToken := "partial-hop-token"
	mockAuth.RegisterStatic(partialToken, &authv1.SecurityContext{
		Subject:  "prac-1",
		TenantId: "test-tenant",
		Scopes:   auth.ParseScopes("user/Observation.rs user/Patient.rs"),
	})
	req := httptest.NewRequest(http.MethodGet, "/fhir/r4/test-tenant/Observation?patient.organization.name=Acme", nil)
	req.Header.Set("Authorization", "Bearer "+partialToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when missing Organization scope on 2nd hop of patient.organization.name, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. $everything on unsupported resource type (e.g. Observation/{id}/$everything) returns 404 Not Found
	fullToken := "full-sys-token"
	mockAuth.RegisterStatic(fullToken, &authv1.SecurityContext{
		Subject:  "prac-1",
		TenantId: "test-tenant",
		Scopes:   auth.ParseScopes("system/*.rs"),
	})
	badEvReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/test-tenant/Observation/obs-1/$everything", nil)
	badEvReq.Header.Set("Authorization", "Bearer "+fullToken)
	badEvRec := httptest.NewRecorder()
	h.ServeHTTP(badEvRec, badEvReq)
	if badEvRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for Observation/$everything, got %d: %s", badEvRec.Code, badEvRec.Body.String())
	}

	// 3. CapabilityStatement advertises Encounter-everything and Composition-document
	metaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/test-tenant/metadata", nil)
	metaRec := httptest.NewRecorder()
	h.ServeHTTP(metaRec, metaReq)
	if metaRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /metadata, got %d", metaRec.Code)
	}
	bodyStr := metaRec.Body.String()
	if !strings.Contains(bodyStr, "http://hl7.org/fhir/OperationDefinition/Encounter-everything") {
		t.Errorf("expected CapabilityStatement to advertise Encounter-everything operation")
	}
	if !strings.Contains(bodyStr, "http://hl7.org/fhir/OperationDefinition/Composition-document") {
		t.Errorf("expected CapabilityStatement to advertise Composition-document operation")
	}
	if !strings.Contains(bodyStr, "http://hl7.org/fhir/us/davinci-deqm/OperationDefinition/care-gaps") {
		t.Errorf("expected CapabilityStatement to advertise Da Vinci DEQM care-gaps operation")
	}

	// 4. OpenAPI 3.1.0 exposes /Measure/$care-gaps and /Encounter/{id}/$everything
	oaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/test-tenant/openapi.json", nil)
	oaRec := httptest.NewRecorder()
	h.ServeHTTP(oaRec, oaReq)
	if oaRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /openapi.json, got %d", oaRec.Code)
	}
	oaStr := oaRec.Body.String()
	for _, wantPath := range []string{`"/Measure/$care-gaps"`, `"/Measure/{id}/$care-gaps"`, `"/Encounter/{id}/$everything"`} {
		if !strings.Contains(oaStr, wantPath) {
			t.Errorf("expected OpenAPI spec to contain path %s", wantPath)
		}
	}
}

func TestWave5MetaSearchTerminologyAndSDC(t *testing.T) {
	reg := fhirutil.AllR4ResourceDescriptors()
	toProtoBytes := func(md protoreflect.MessageDescriptor, rawJSON []byte) []byte {
		t.Helper()
		norm, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawJSON)
		if err != nil {
			t.Fatalf("NormalizeFHIRJSONToProtoJSON: %v", err)
		}
		msg := dynamicpb.NewMessage(md)
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(norm, msg); err != nil {
			t.Fatalf("protojson.Unmarshal: %v", err)
		}
		b, err := proto.Marshal(msg)
		if err != nil {
			t.Fatalf("proto.Marshal: %v", err)
		}
		return b
	}

	// 1. Verify static (Patient) and dynamic (Coverage & ConceptMap) meta & URI index extraction
	patJSON := []byte(`{
		"resourceType": "Patient",
		"id": "pat-meta-1",
		"meta": {
			"source": "http://ehr.example.org/system-a",
			"profile": ["http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"],
			"tag": [{"system": "http://example.org/tags", "code": "cohort-a"}],
			"security": [{"system": "http://terminology.hl7.org/CodeSystem/v3-Confidentiality", "code": "R"}]
		},
		"name": [{"family": "MetaTest"}]
	}`)
	patProto := toProtoBytes(reg["Patient"], patJSON)
	patIdx, err := postgres.ExtractPatient("t1", "pat-meta-1", patProto)
	if err != nil {
		t.Fatalf("ExtractPatient: %v", err)
	}
	foundProfileURI, foundSourceURI, foundTagToken, foundSecToken := false, false, false, false
	for _, u := range patIdx.URIs {
		if u.SpName == "_profile" && u.SpValue == "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient" {
			foundProfileURI = true
		}
		if u.SpName == "_source" && u.SpValue == "http://ehr.example.org/system-a" {
			foundSourceURI = true
		}
	}
	for _, tk := range patIdx.Tokens {
		if tk.SpName == "_tag" && tk.SpSystem == "http://example.org/tags" && tk.SpValue == "cohort-a" {
			foundTagToken = true
		}
		if tk.SpName == "_security" && tk.SpSystem == "http://terminology.hl7.org/CodeSystem/v3-Confidentiality" && tk.SpValue == "R" {
			foundSecToken = true
		}
	}
	if !foundProfileURI || !foundSourceURI || !foundTagToken || !foundSecToken {
		t.Fatalf("expected _profile, _source, _tag, and _security in static Patient indexes, got profile=%v source=%v tag=%v sec=%v",
			foundProfileURI, foundSourceURI, foundTagToken, foundSecToken)
	}

	// Dynamic extractor on Coverage (meta) and ConceptMap (source-uri / target-uri)
	covJSON := []byte(`{
		"resourceType": "Coverage",
		"id": "cov-meta-1",
		"meta": {
			"source": "http://payer.example.org",
			"profile": ["http://hl7.org/fhir/us/core/StructureDefinition/us-core-coverage"],
			"tag": [{"system": "http://example.org/tags", "code": "plan-gold"}],
			"security": [{"system": "http://terminology.hl7.org/CodeSystem/v3-Confidentiality", "code": "N"}]
		},
		"status": "active",
		"beneficiary": {"reference": "Patient/pat-meta-1"},
		"payor": [{"reference": "Organization/org-1"}]
	}`)
	covProto := toProtoBytes(reg["Coverage"], covJSON)
	covExtract := postgres.NewDynamicResourceExtractor("Coverage", reg["Coverage"])
	covIdx, err := covExtract("t1", "cov-meta-1", covProto)
	if err != nil {
		t.Fatalf("ExtractDynamic(Coverage): %v", err)
	}
	hasCovProfile, hasCovTag := false, false
	for _, u := range covIdx.URIs {
		if u.SpName == "_profile" && u.SpValue == "http://hl7.org/fhir/us/core/StructureDefinition/us-core-coverage" {
			hasCovProfile = true
		}
	}
	for _, tk := range covIdx.Tokens {
		if tk.SpName == "_tag" && tk.SpValue == "plan-gold" {
			hasCovTag = true
		}
	}
	if !hasCovProfile || !hasCovTag {
		t.Fatalf("expected dynamic Coverage extractor to index _profile and _tag, got profile=%v tag=%v", hasCovProfile, hasCovTag)
	}

	cmJSON := []byte(`{
		"resourceType": "ConceptMap",
		"id": "cm-1",
		"url": "http://example.org/fhir/ConceptMap/custom-map",
		"status": "active",
		"sourceUri": "http://snomed.info/sct",
		"targetUri": "http://hl7.org/fhir/sid/icd-10-cm",
		"group": [{
			"source": "http://snomed.info/sct",
			"target": "http://hl7.org/fhir/sid/icd-10-cm"
		}]
	}`)
	cmProto := toProtoBytes(reg["ConceptMap"], cmJSON)
	cmExtract := postgres.NewDynamicResourceExtractor("ConceptMap", reg["ConceptMap"])
	cmIdx, err := cmExtract("t1", "cm-1", cmProto)
	if err != nil {
		t.Fatalf("ExtractDynamic(ConceptMap): %v", err)
	}
	hasSrcURI, hasTgtURI := false, false
	for _, u := range cmIdx.URIs {
		if u.SpName == "source-uri" && u.SpValue == "http://snomed.info/sct" {
			hasSrcURI = true
		}
		if u.SpName == "target-uri" && u.SpValue == "http://hl7.org/fhir/sid/icd-10-cm" {
			hasTgtURI = true
		}
	}
	if !hasSrcURI || !hasTgtURI {
		t.Fatalf("expected ConceptMap dynamic extractor to index source-uri and target-uri, got src=%v tgt=%v", hasSrcURI, hasTgtURI)
	}

	// 2. Verify ValidateSearchQuery & BuildSearchParamsFromQuery for _profile, _tag, _security, _source
	qVals, _ := url.ParseQuery("_profile:below=http://hl7.org/fhir/us/core/StructureDefinition/&_tag=http://example.org/tags|cohort-a&_tag:not=excluded&_security:missing=false&_source=http://ehr.example.org/system-a")
	if err := postgres.ValidateSearchQuery(qVals); err != nil {
		t.Fatalf("ValidateSearchQuery rejected meta search params: %v", err)
	}
	sp := postgres.BuildSearchParamsFromQuery("t1", "Patient", qVals)
	if sp.URIs["_profile:below"] != "http://hl7.org/fhir/us/core/StructureDefinition/" {
		t.Errorf("expected URIs[_profile:below], got %v", sp.URIs)
	}
	if sp.URIs["_source"] != "http://ehr.example.org/system-a" {
		t.Errorf("expected URIs[_source], got %v", sp.URIs)
	}
	if sp.Tokens["_tag"] != "http://example.org/tags|cohort-a" {
		t.Errorf("expected Tokens[_tag], got %v", sp.Tokens)
	}
	if sp.Tokens["_tag:not"] != "excluded" {
		t.Errorf("expected Tokens[_tag:not]=excluded, got %v", sp.Tokens)
	}
	if sp.Missing["_security"] {
		t.Errorf("expected Missing[_security]==false, got %v", sp.Missing)
	}

	// 3. Verify CodeSystem/$validate-code, CodeSystem/$subsumes, ValueSet compose filter/exclude/valueSet, and SDC $extract
	srv := server.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	// 3a. CodeSystem/$validate-code (valid code + display mismatch)
	csVCReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/t1/CodeSystem/$validate-code?url=http://snomed.info/sct&code=38341003", nil)
	csVCRec := httptest.NewRecorder()
	h.ServeHTTP(csVCRec, csVCReq)
	if csVCRec.Code != http.StatusOK || !strings.Contains(csVCRec.Body.String(), `"valueBoolean":true`) {
		t.Fatalf("expected 200 OK with valueBoolean:true from CodeSystem/$validate-code, got %d: %s", csVCRec.Code, csVCRec.Body.String())
	}

	csBadDispReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/t1/CodeSystem/$validate-code?url=http://snomed.info/sct&code=38341003&display=WrongDisplay", nil)
	csBadDispRec := httptest.NewRecorder()
	h.ServeHTTP(csBadDispRec, csBadDispReq)
	if csBadDispRec.Code != http.StatusOK || !strings.Contains(csBadDispRec.Body.String(), `"valueBoolean":false`) {
		t.Fatalf("expected valueBoolean:false on display mismatch in CodeSystem/$validate-code, got %d: %s", csBadDispRec.Code, csBadDispRec.Body.String())
	}

	// 3b. CodeSystem/$subsumes (subsumes, subsumed-by, equivalent, not-subsumed)
	for _, tc := range []struct {
		codeA, codeB, want string
	}{
		{"38341003", "59621000", "subsumes"},
		{"59621000", "38341003", "subsumed-by"},
		{"38341003", "38341003", "equivalent"},
		{"38341003", "44054006", "not-subsumed"},
	} {
		subReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/t1/CodeSystem/$subsumes?system=http://snomed.info/sct&codeA="+tc.codeA+"&codeB="+tc.codeB, nil)
		subRec := httptest.NewRecorder()
		h.ServeHTTP(subRec, subReq)
		if subRec.Code != http.StatusOK || !strings.Contains(subRec.Body.String(), `"valueCode":"`+tc.want+`"`) {
			t.Fatalf("CodeSystem/$subsumes(%s, %s): expected %s, got %d: %s", tc.codeA, tc.codeB, tc.want, subRec.Code, subRec.Body.String())
		}
	}

	// 3c. ValueSet/$expand with compose.include.filter (is-a), compose.include.valueSet, and compose.exclude
	inlineVSBody := []byte(`{
		"resourceType": "Parameters",
		"parameter": [{
			"name": "valueSet",
			"resource": {
				"resourceType": "ValueSet",
				"url": "http://example.org/fhir/ValueSet/diabetes-and-vitals-custom",
				"status": "active",
				"compose": {
					"include": [
						{
							"system": "http://snomed.info/sct",
							"filter": [{"property": "concept", "op": "is-a", "value": "73211009"}]
						},
						{
							"valueSet": ["http://hl7.org/fhir/ValueSet/administrative-gender"]
						}
					],
					"exclude": [
						{
							"system": "http://hl7.org/fhir/administrative-gender",
							"concept": [{"code": "unknown"}]
						}
					]
				}
			}
		}]
	}`)
	expReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/t1/ValueSet/$expand", strings.NewReader(string(inlineVSBody)))
	expReq.Header.Set("Content-Type", "application/fhir+json")
	expRec := httptest.NewRecorder()
	h.ServeHTTP(expRec, expReq)
	if expRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from ValueSet/$expand with filter/valueSet/exclude, got %d: %s", expRec.Code, expRec.Body.String())
	}
	expStr := expRec.Body.String()
	// Must include 73211009 (Diabetes mellitus) and 44054006 (Diabetes mellitus type 2, child of 73211009), plus female, but exclude unknown
	if !strings.Contains(expStr, `"73211009"`) || !strings.Contains(expStr, `"44054006"`) || !strings.Contains(expStr, `"female"`) {
		t.Fatalf("expected expanded ValueSet to contain 73211009, 44054006 (via is-a filter), and female (via imported ValueSet), got: %s", expStr)
	}
	if strings.Contains(expStr, `"code":"unknown"`) {
		t.Fatalf("expected compose.exclude to remove code 'unknown', got: %s", expStr)
	}

	// 3d. QuestionnaireResponse/$extract with MedicationRequest and Procedure
	qrExtractBody := []byte(`{
		"resourceType": "QuestionnaireResponse",
		"id": "qr-ext-wave5",
		"status": "completed",
		"subject": {"reference": "Patient/pat-1"},
		"authored": "2026-02-01T12:00:00Z",
		"item": [
			{
				"linkId": "current-medication",
				"text": "Active Medication Prescription",
				"answer": [{"valueCoding": {"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "314076", "display": "Lisinopril 10 MG Oral Tablet"}}]
			},
			{
				"linkId": "prior-procedure",
				"text": "Past Surgical Procedure",
				"answer": [{"valueCoding": {"system": "http://snomed.info/sct", "code": "80146002", "display": "Appendectomy"}}]
			}
		]
	}`)
	extReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/t1/QuestionnaireResponse/$extract", strings.NewReader(string(qrExtractBody)))
	extReq.Header.Set("Content-Type", "application/fhir+json")
	extRec := httptest.NewRecorder()
	h.ServeHTTP(extRec, extReq)
	if extRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from QuestionnaireResponse/$extract, got %d: %s", extRec.Code, extRec.Body.String())
	}
	extStr := extRec.Body.String()
	if !strings.Contains(extStr, `"MedicationRequest"`) || !strings.Contains(extStr, `"Procedure"`) || !strings.Contains(extStr, `"Provenance"`) {
		t.Fatalf("expected extracted Bundle to include MedicationRequest, Procedure, and Provenance, got: %s", extStr)
	}
}

func TestWave8FHIRPathPatchAndConditionalOps(t *testing.T) {
	// 1. Unit test ApplyFHIRPatch (add, insert, replace, move, delete, complex part, $diff JSON string value)
	origDoc := []byte(`{
		"resourceType": "Patient",
		"id": "pat-fp-1",
		"active": true,
		"gender": "male",
		"name": [
			{"family": "OrigFamily", "given": ["John", "Quincy"]}
		],
		"telecom": [
			{"system": "phone", "value": "555-0100"}
		]
	}`)

	patchParams := []byte(`{
		"resourceType": "Parameters",
		"parameter": [
			{
				"name": "operation",
				"part": [
					{"name": "type", "valueCode": "replace"},
					{"name": "path", "valueString": "Patient.gender"},
					{"name": "value", "valueCode": "other"}
				]
			},
			{
				"name": "operation",
				"part": [
					{"name": "type", "valueCode": "replace"},
					{"name": "path", "valueString": "Patient.name[0].family"},
					{"name": "value", "valueString": "PatchedFamily"}
				]
			},
			{
				"name": "operation",
				"part": [
					{"name": "type", "valueCode": "insert"},
					{"name": "path", "valueString": "Patient.name[0].given"},
					{"name": "index", "valueInteger": 0},
					{"name": "value", "valueString": "Dr."}
				]
			},
			{
				"name": "operation",
				"part": [
					{"name": "type", "valueCode": "move"},
					{"name": "path", "valueString": "Patient.name[0].given"},
					{"name": "source", "valueInteger": 2},
					{"name": "destination", "valueInteger": 1}
				]
			},
			{
				"name": "operation",
				"part": [
					{"name": "type", "valueCode": "add"},
					{"name": "path", "valueString": "Patient"},
					{"name": "name", "valueString": "birthDate"},
					{"name": "value", "valueDate": "1980-06-15"}
				]
			},
			{
				"name": "operation",
				"part": [
					{"name": "type", "valueCode": "add"},
					{"name": "path", "valueString": "Patient.address"},
					{
						"name": "value",
						"part": [
							{"name": "city", "valueString": "Boston"},
							{"name": "state", "valueString": "MA"}
						]
					}
				]
			},
			{
				"name": "operation",
				"part": [
					{"name": "type", "valueCode": "delete"},
					{"name": "path", "valueString": "Patient.telecom"}
				]
			}
		]
	}`)

	patchedBytes, status, err := server.ApplyFHIRPatch(origDoc, patchParams)
	if err != nil || status != http.StatusOK {
		t.Fatalf("ApplyFHIRPatch failed with status=%d err=%v", status, err)
	}
	var patched map[string]any
	if err := json.Unmarshal(patchedBytes, &patched); err != nil {
		t.Fatalf("unmarshal patched patient: %v", err)
	}
	if patched["gender"] != "other" {
		t.Errorf("expected gender=other, got %v", patched["gender"])
	}
	if patched["birthDate"] != "1980-06-15" {
		t.Errorf("expected birthDate=1980-06-15, got %v", patched["birthDate"])
	}
	if _, hasTelecom := patched["telecom"]; hasTelecom {
		t.Errorf("expected telecom to be deleted, got %v", patched["telecom"])
	}
	names, _ := patched["name"].([]any)
	name0, _ := names[0].(map[string]any)
	if name0["family"] != "PatchedFamily" {
		t.Errorf("expected name[0].family=PatchedFamily, got %v", name0["family"])
	}
	given, _ := name0["given"].([]any)
	if len(given) != 3 || given[0] != "Dr." || given[1] != "Quincy" || given[2] != "John" {
		t.Errorf("expected given=[Dr., Quincy, John] after insert+move, got %v", given)
	}
	addrs, _ := patched["address"].([]any)
	if len(addrs) != 1 {
		t.Fatalf("expected 1 address added via complex part, got %v", patched["address"])
	}
	addr0, _ := addrs[0].(map[string]any)
	if addr0["city"] != "Boston" || addr0["state"] != "MA" {
		t.Errorf("expected address Boston, MA, got %v", addr0)
	}

	// 2. Verify ApplyFHIRPatch rejects mutating immutable id or resourceType
	badPatch := []byte(`{
		"resourceType": "Parameters",
		"parameter": [{
			"name": "operation",
			"part": [
				{"name": "type", "valueCode": "replace"},
				{"name": "path", "valueString": "Patient.id"},
				{"name": "value", "valueString": "tampered-id"}
			]
		}]
	}`)
	if _, badStatus, badErr := server.ApplyFHIRPatch(origDoc, badPatch); badErr == nil || badStatus != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request when mutating Patient.id via FHIRPath Patch, got status=%d err=%v", badStatus, badErr)
	}

	// 3. Verify CapabilityStatement and OpenAPI 3.1.0 advertise Wave 8 capabilities
	srv := server.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	metaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/t1/metadata", nil)
	metaRec := httptest.NewRecorder()
	h.ServeHTTP(metaRec, metaReq)
	if metaRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /metadata, got %d", metaRec.Code)
	}
	metaStr := metaRec.Body.String()
	if !strings.Contains(metaStr, `"conditionalPatch":true`) {
		t.Errorf("expected CapabilityStatement to declare conditionalPatch:true")
	}
	if !strings.Contains(metaStr, `"application/fhir+json"`) {
		t.Errorf("expected CapabilityStatement patchFormat to include application/fhir+json")
	}

	oaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/t1/openapi.json", nil)
	oaRec := httptest.NewRecorder()
	h.ServeHTTP(oaRec, oaReq)
	if oaRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /openapi.json, got %d", oaRec.Code)
	}
	var oaDoc map[string]any
	if err := json.Unmarshal(oaRec.Body.Bytes(), &oaDoc); err != nil {
		t.Fatalf("unmarshal openapi.json: %v", err)
	}
	paths, _ := oaDoc["paths"].(map[string]any)
	patPath, _ := paths["/Patient"].(map[string]any)
	if _, hasCondPatch := patPath["patch"]; !hasCondPatch {
		t.Errorf("expected OpenAPI /Patient path to expose conditional patch operation")
	}
	evPath, _ := paths["/Patient/{id}/$everything"].(map[string]any)
	evGet, _ := evPath["get"].(map[string]any)
	evParamsJSON, _ := json.Marshal(evGet["parameters"])
	if !strings.Contains(string(evParamsJSON), `"_typeFilter"`) {
		t.Errorf("expected OpenAPI /Patient/{id}/$everything to advertise _typeFilter parameter, got %s", evParamsJSON)
	}
}

func TestWave9TerminologyModifiersReferenceIdentifierAndConditionalRead(t *testing.T) {
	srv := server.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	// 1. Verify ExpandTerminologySearchParams expands :in, :not-in, :below, and :above
	q := map[string][]string{
		"gender:in":             {"http://hl7.org/fhir/ValueSet/administrative-gender"},
		"status:not-in":         {"http://hl7.org/fhir/ValueSet/administrative-gender"},
		"code:below":            {"http://snomed.info/sct|73211009"},
		"component-code:below":  {"http://loinc.org|85354-9"},
		"clinical-status:above": {"http://terminology.hl7.org/CodeSystem/condition-clinical|recurrence"},
		"subject:Patient.gender:in": {
			"http://hl7.org/fhir/ValueSet/administrative-gender",
		},
	}
	params := postgres.BuildSearchParamsFromQuery("t1", "Observation", q)
	if err := srv.ExpandTerminologySearchParams(context.Background(), "t1", &params); err != nil {
		t.Fatalf("ExpandTerminologySearchParams failed: %v", err)
	}

	// gender:in should expand into TokenFilters["gender"] containing administrative-gender codes
	genderExpanded := strings.Join(params.TokenFilters["gender"], ",")
	for _, expectedCode := range []string{
		"http://hl7.org/fhir/administrative-gender|female",
		"http://hl7.org/fhir/administrative-gender|male",
		"http://hl7.org/fhir/administrative-gender|other",
		"http://hl7.org/fhir/administrative-gender|unknown",
	} {
		if !strings.Contains(genderExpanded, expectedCode) {
			t.Errorf("expected gender:in expansion to include %s, got %s", expectedCode, genderExpanded)
		}
	}
	if _, stillHasIn := params.TokenFilters["gender:in"]; stillHasIn {
		t.Errorf("expected gender:in key to be rewritten to gender")
	}

	// status:not-in should expand into TokenFilters["status:not"]
	statusNotExpanded := strings.Join(params.TokenFilters["status:not"], ",")
	if !strings.Contains(statusNotExpanded, "http://hl7.org/fhir/administrative-gender|female") {
		t.Errorf("expected status:not-in expansion to populate status:not, got %s", statusNotExpanded)
	}

	// code:below=http://snomed.info/sct|73211009 (Diabetes mellitus) should include 73211009 and child 44054006 (Type 2 diabetes mellitus)
	snomedBelow := strings.Join(params.TokenFilters["code"], ",")
	if !strings.Contains(snomedBelow, "http://snomed.info/sct|73211009") || !strings.Contains(snomedBelow, "http://snomed.info/sct|44054006") {
		t.Errorf("expected code:below=http://snomed.info/sct|73211009 to include 73211009 and 44054006, got %s", snomedBelow)
	}

	// component-code:below=http://loinc.org|85354-9 (Blood pressure panel) should include 85354-9, 8480-6 (Systolic), and 8462-4 (Diastolic)
	loincBelow := strings.Join(params.TokenFilters["component-code"], ",")
	if !strings.Contains(loincBelow, "http://loinc.org|85354-9") ||
		!strings.Contains(loincBelow, "http://loinc.org|8480-6") ||
		!strings.Contains(loincBelow, "http://loinc.org|8462-4") {
		t.Errorf("expected component-code:below=http://loinc.org|85354-9 to include 85354-9, 8480-6, 8462-4, got %s", loincBelow)
	}

	// clinical-status:above=recurrence should include recurrence and ancestor active
	clinAbove := strings.Join(params.TokenFilters["clinical-status"], ",")
	if !strings.Contains(clinAbove, "http://terminology.hl7.org/CodeSystem/condition-clinical|recurrence") ||
		!strings.Contains(clinAbove, "http://terminology.hl7.org/CodeSystem/condition-clinical|active") {
		t.Errorf("expected clinical-status:above=recurrence to include recurrence and active, got %s", clinAbove)
	}

	// Chained subject:Patient.gender:in should rewrite TargetParam from gender:in to gender
	if len(params.Chained) != 1 || params.Chained[0].TargetParam != "gender" || !strings.Contains(params.Chained[0].Value, "female") {
		t.Errorf("expected chained subject:Patient.gender:in to expand to TargetParam=gender with female, got %+v", params.Chained)
	}

	// Unknown ValueSet on :in must return an error
	badParams := postgres.BuildSearchParamsFromQuery("t1", "Observation", map[string][]string{
		"code:in": {"http://example.org/fhir/ValueSet/does-not-exist"},
	})
	if err := srv.ExpandTerminologySearchParams(context.Background(), "t1", &badParams); err == nil {
		t.Errorf("expected error for unknown ValueSet in :in modifier")
	}

	// 2. Verify extractors index inline Reference.identifier tokens under <spName>:identifier
	obsWithInlineRefIdent := []byte(`{
		"resourceType": "Observation",
		"id": "obs-inline-ref-1",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
		"subject": {
			"type": "Patient",
			"identifier": {
				"system": "http://hospital.example.org/mrn",
				"value": "MRN-INLINE-999"
			}
		}
	}`)
	var obsIdx *postgres.SearchIndexes
	// Use dynamic extractor via ExtractDynamicResource or ExtractObservation
	md, ok := fhirutil.AllR4ResourceDescriptors()["Observation"]
	if !ok {
		t.Fatalf("Observation descriptor not found")
	}
	normBytes, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, obsWithInlineRefIdent)
	if err != nil {
		t.Fatalf("normalize Observation: %v", err)
	}
	dynMsg := dynamicpb.NewMessage(md)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normBytes, dynMsg); err != nil {
		t.Fatalf("unmarshal Observation proto: %v", err)
	}
	protoBytes, err := proto.Marshal(dynMsg)
	if err != nil {
		t.Fatalf("marshal Observation proto: %v", err)
	}
	obsIdx, err = postgres.ExtractObservation("t1", "obs-inline-ref-1", protoBytes)
	if err != nil {
		t.Fatalf("ExtractObservation failed: %v", err)
	}
	hasSubjectIdentToken := false
	hasPatientIdentToken := false
	for _, tok := range obsIdx.Tokens {
		if tok.SpName == "subject:identifier" && tok.SpSystem == "http://hospital.example.org/mrn" && tok.SpValue == "MRN-INLINE-999" {
			hasSubjectIdentToken = true
		}
		if tok.SpName == "patient:identifier" && tok.SpSystem == "http://hospital.example.org/mrn" && tok.SpValue == "MRN-INLINE-999" {
			hasPatientIdentToken = true
		}
	}
	if !hasSubjectIdentToken || !hasPatientIdentToken {
		t.Errorf("expected ExtractObservation to index subject:identifier and patient:identifier tokens, got %+v", obsIdx.Tokens)
	}

	// 3. Verify CapabilityStatement and OpenAPI 3.1.0 declare conditionalRead: full-support and If-None-Match / If-Modified-Since / 304
	metaReq := httptest.NewRequest(http.MethodHead, "/fhir/r4/t1/metadata", nil)
	metaRec := httptest.NewRecorder()
	h.ServeHTTP(metaRec, metaReq)
	if metaRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from HEAD /metadata, got %d", metaRec.Code)
	}

	metaGetReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/t1/metadata", nil)
	metaGetRec := httptest.NewRecorder()
	h.ServeHTTP(metaGetRec, metaGetReq)
	if !strings.Contains(metaGetRec.Body.String(), `"conditionalRead":"full-support"`) {
		t.Errorf("expected CapabilityStatement to declare conditionalRead:full-support")
	}

	oaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/t1/openapi.json", nil)
	oaRec := httptest.NewRecorder()
	h.ServeHTTP(oaRec, oaReq)
	oaStr := oaRec.Body.String()
	for _, expected := range []string{`"If-None-Match"`, `"If-Modified-Since"`, `"304"`} {
		if !strings.Contains(oaStr, expected) {
			t.Errorf("expected OpenAPI spec to contain %s", expected)
		}
	}

	// 4. End-to-End Postgres verification for Conditional Read, HEAD, Bundle ifNoneMatch/ifModifiedSince,
	//    Terminology Search Modifiers (:in, :not-in, :below, :above), and Reference :identifier
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	tenant := "test-wave9-e2e"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	// Seed a Patient, Condition (Type 2 diabetes mellitus 44054006, recurrence), and two Observations (one with reference, one with inline Reference.identifier)
	seedBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"resource": {
					"resourceType": "Patient",
					"id": "pat-w9-1",
					"active": true,
					"gender": "female",
					"name": [{"family": "WaveNine", "given": ["Ada"]}],
					"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-W9-001"}]
				},
				"request": {"method": "PUT", "url": "Patient/pat-w9-1"}
			},
			{
				"resource": {
					"resourceType": "Condition",
					"id": "cond-w9-1",
					"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "recurrence"}]},
					"verificationStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}]},
					"code": {"coding": [{"system": "http://snomed.info/sct", "code": "44054006"}]},
					"subject": {"reference": "Patient/pat-w9-1"}
				},
				"request": {"method": "PUT", "url": "Condition/cond-w9-1"}
			},
			{
				"resource": {
					"resourceType": "Observation",
					"id": "obs-w9-inline",
					"status": "final",
					"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
					"subject": {
						"type": "Patient",
						"identifier": {"system": "http://hospital.example.org/mrn", "value": "MRN-W9-INLINE"}
					},
					"valueQuantity": {"value": 74.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
				},
				"request": {"method": "PUT", "url": "Observation/obs-w9-inline"}
			}
		]
	}`
	seedReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant, strings.NewReader(seedBundle))
	seedReq.Header.Set("Content-Type", "application/fhir+json")
	seedRec := httptest.NewRecorder()
	pgH.ServeHTTP(seedRec, seedReq)
	if seedRec.Code != http.StatusOK {
		t.Fatalf("seed bundle failed (%d): %s", seedRec.Code, seedRec.Body.String())
	}

	// 4a. Read Patient to capture ETag and Last-Modified, then verify 304 Not Modified on If-None-Match and If-Modified-Since
	readReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w9-1", nil)
	readRec := httptest.NewRecorder()
	pgH.ServeHTTP(readRec, readReq)
	if readRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on GET /Patient/pat-w9-1, got %d", readRec.Code)
	}
	etag := readRec.Header().Get("ETag")
	lastMod := readRec.Header().Get("Last-Modified")

	inmReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w9-1", nil)
	inmReq.Header.Set("If-None-Match", etag)
	inmRec := httptest.NewRecorder()
	pgH.ServeHTTP(inmRec, inmReq)
	if inmRec.Code != http.StatusNotModified || inmRec.Body.Len() != 0 {
		t.Errorf("expected 304 Not Modified with empty body on matching If-None-Match, got %d body=%q", inmRec.Code, inmRec.Body.String())
	}

	imsReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w9-1", nil)
	imsReq.Header.Set("If-Modified-Since", lastMod)
	imsRec := httptest.NewRecorder()
	pgH.ServeHTTP(imsRec, imsReq)
	if imsRec.Code != http.StatusNotModified || imsRec.Body.Len() != 0 {
		t.Errorf("expected 304 Not Modified with empty body on matching If-Modified-Since, got %d body=%q", imsRec.Code, imsRec.Body.String())
	}

	headReq := httptest.NewRequest(http.MethodHead, "/fhir/r4/"+tenant+"/Patient/pat-w9-1", nil)
	headRec := httptest.NewRecorder()
	pgH.ServeHTTP(headRec, headReq)
	if headRec.Code != http.StatusOK || headRec.Header().Get("ETag") != etag || headRec.Body.Len() != 0 {
		t.Errorf("expected 200 OK with ETag and empty body on HEAD /Patient/pat-w9-1, got %d etag=%q len=%d", headRec.Code, headRec.Header().Get("ETag"), headRec.Body.Len())
	}

	// 4b. Verify Batch Bundle with ifNoneMatch (304) and HEAD (200)
	batchCondRead := `{
		"resourceType": "Bundle",
		"type": "batch",
		"entry": [
			{
				"request": {
					"method": "GET",
					"url": "Patient/pat-w9-1",
					"ifNoneMatch": ` + "`" + `"` + strings.ReplaceAll(etag, `"`, `\"`) + `"` + "`" + `
				}
			},
			{
				"request": {
					"method": "HEAD",
					"url": "Patient/pat-w9-1"
				}
			}
		]
	}`
	// Build valid JSON with json.Marshal to avoid quote escaping issues
	batchPayloadBytes, _ := json.Marshal(map[string]any{
		"resourceType": "Bundle",
		"type":         "batch",
		"entry": []any{
			map[string]any{
				"request": map[string]any{
					"method":      "GET",
					"url":         "Patient/pat-w9-1",
					"ifNoneMatch": etag,
				},
			},
			map[string]any{
				"request": map[string]any{
					"method": "HEAD",
					"url":    "Patient/pat-w9-1",
				},
			},
		},
	})
	_ = batchCondRead
	bReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant, strings.NewReader(string(batchPayloadBytes)))
	bReq.Header.Set("Content-Type", "application/fhir+json")
	bRec := httptest.NewRecorder()
	pgH.ServeHTTP(bRec, bReq)
	if bRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from batch bundle, got %d: %s", bRec.Code, bRec.Body.String())
	}
	if !strings.Contains(bRec.Body.String(), `"304 Not Modified"`) {
		t.Errorf("expected batch response entry[0] to have status '304 Not Modified', got: %s", bRec.Body.String())
	}

	// 4c. Verify Condition search with code:below=http://snomed.info/sct|73211009 (parent of 44054006)
	//     and patient:identifier=http://hospital.example.org/mrn|MRN-W9-001
	condSearchReq := httptest.NewRequest(http.MethodGet,
		"/fhir/r4/"+tenant+"/Condition?code:below=http://snomed.info/sct|73211009&clinical-status:below=http://terminology.hl7.org/CodeSystem/condition-clinical|active&patient:identifier=http://hospital.example.org/mrn|MRN-W9-001",
		nil)
	condSearchRec := httptest.NewRecorder()
	pgH.ServeHTTP(condSearchRec, condSearchReq)
	if condSearchRec.Code != http.StatusOK || !strings.Contains(condSearchRec.Body.String(), `"cond-w9-1"`) {
		t.Errorf("expected Condition search with :below and patient:identifier to match cond-w9-1, got %d: %s", condSearchRec.Code, condSearchRec.Body.String())
	}

	// 4d. Verify Observation search with inline logical Reference.identifier (subject:identifier=MRN-W9-INLINE) and code:in
	obsSearchReq := httptest.NewRequest(http.MethodGet,
		"/fhir/r4/"+tenant+"/Observation?subject:identifier=http://hospital.example.org/mrn|MRN-W9-INLINE&code:in=http://hl7.org/fhir/ValueSet/observation-vitalsignresult",
		nil)
	obsSearchRec := httptest.NewRecorder()
	pgH.ServeHTTP(obsSearchRec, obsSearchReq)
	if obsSearchRec.Code != http.StatusOK || !strings.Contains(obsSearchRec.Body.String(), `"obs-w9-inline"`) {
		t.Errorf("expected Observation search with inline subject:identifier and code:in to match obs-w9-inline, got %d: %s", obsSearchRec.Code, obsSearchRec.Body.String())
	}
}

func TestSystemSearchTokenTextAndResourceGraphWave10(t *testing.T) {
	srv := server.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	// 1. Verify CapabilityStatement declares search-system and $graph operation
	metaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/t1/metadata", nil)
	metaRec := httptest.NewRecorder()
	h.ServeHTTP(metaRec, metaReq)
	if metaRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on /metadata, got %d", metaRec.Code)
	}
	if !strings.Contains(metaRec.Body.String(), `"search-system"`) || !strings.Contains(metaRec.Body.String(), `"http://hl7.org/fhir/OperationDefinition/Resource-graph"`) {
		t.Errorf("expected CapabilityStatement to declare search-system and Resource-graph operation")
	}

	// 2. Verify invalid _type in system search returns 400 Bad Request
	badSysReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/t1?_type=Patient,NotAResourceType", nil)
	badSysRec := httptest.NewRecorder()
	h.ServeHTTP(badSysRec, badSysReq)
	if badSysRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for unknown _type in system search, got %d: %s", badSysRec.Code, badSysRec.Body.String())
	}

	// 3. End-to-end Postgres tests when FLINT_TEST_POSTGRES_DSN is configured
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	tenant := "test-wave10-e2e"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	putRes := func(resType, id, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/"+resType+"/"+id, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s/%s failed (%d): %s", resType, id, rec.Code, rec.Body.String())
		}
	}

	putRes("Organization", "org-w10-1", `{
		"resourceType": "Organization",
		"id": "org-w10-1",
		"active": true,
		"name": "Wave10 Heart Institute"
	}`)

	putRes("Patient", "pat-w10-1", `{
		"resourceType": "Patient",
		"id": "pat-w10-1",
		"identifier": [
			{
				"type": {
					"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MR", "display": "Medical Record Number"}],
					"text": "Hospital MRN"
				},
				"system": "http://hospital.example.org/mrn",
				"value": "MRN-W10-001"
			}
		],
		"name": [{"family": "Graphson", "given": ["Grace"]}],
		"gender": "female",
		"birthDate": "1982-06-15",
		"managingOrganization": {"reference": "Organization/org-w10-1"}
	}`)

	putRes("Condition", "cond-w10-1", `{
		"resourceType": "Condition",
		"id": "cond-w10-1",
		"clinicalStatus": {
			"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active", "display": "Active"}]
		},
		"category": [
			{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "encounter-diagnosis", "display": "Encounter Diagnosis"}]}
		],
		"code": {
			"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}],
			"text": "Benign Essential Hypertension"
		},
		"subject": {"reference": "Patient/pat-w10-1"}
	}`)

	putRes("Observation", "obs-w10-1", `{
		"resourceType": "Observation",
		"id": "obs-w10-1",
		"status": "final",
		"category": [
			{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}]}
		],
		"code": {
			"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}],
			"text": "Resting Heart Rate"
		},
		"subject": {"reference": "Patient/pat-w10-1"},
		"valueQuantity": {"value": 68, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
	}`)

	// 3a. Verify Token :text search modifier on Condition, Observation, and chained Patient.identifier:text
	condTxtReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Condition?code:text=Benign+Essential", nil)
	condTxtRec := httptest.NewRecorder()
	pgH.ServeHTTP(condTxtRec, condTxtReq)
	if condTxtRec.Code != http.StatusOK || !strings.Contains(condTxtRec.Body.String(), `"cond-w10-1"`) {
		t.Errorf("expected Condition?code:text=Benign+Essential to match cond-w10-1, got %d: %s", condTxtRec.Code, condTxtRec.Body.String())
	}

	obsTxtReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation?category:text=vital&subject:Patient.identifier:text=Medical+Record", nil)
	obsTxtRec := httptest.NewRecorder()
	pgH.ServeHTTP(obsTxtRec, obsTxtReq)
	if obsTxtRec.Code != http.StatusOK || !strings.Contains(obsTxtRec.Body.String(), `"obs-w10-1"`) {
		t.Errorf("expected Observation?category:text=vital&subject:Patient.identifier:text=Medical+Record to match obs-w10-1, got %d: %s", obsTxtRec.Code, obsTxtRec.Body.String())
	}

	// 3b. Verify System-Level Multi-Type Search (GET /fhir/r4/{tenant}?_type=Condition,Observation&patient=Patient/pat-w10-1 and POST /_search)
	sysGetReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"?_type=Condition,Observation&patient=Patient/pat-w10-1", nil)
	sysGetRec := httptest.NewRecorder()
	pgH.ServeHTTP(sysGetRec, sysGetReq)
	if sysGetRec.Code != http.StatusOK || !strings.Contains(sysGetRec.Body.String(), `"cond-w10-1"`) || !strings.Contains(sysGetRec.Body.String(), `"obs-w10-1"`) {
		t.Errorf("expected system GET search with _type=Condition,Observation to return both cond-w10-1 and obs-w10-1, got %d: %s", sysGetRec.Code, sysGetRec.Body.String())
	}

	sysPostReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/_search", strings.NewReader("_type=Patient,Organization&_id=pat-w10-1,org-w10-1"))
	sysPostReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sysPostRec := httptest.NewRecorder()
	pgH.ServeHTTP(sysPostRec, sysPostReq)
	if sysPostRec.Code != http.StatusOK || !strings.Contains(sysPostRec.Body.String(), `"pat-w10-1"`) || !strings.Contains(sysPostRec.Body.String(), `"org-w10-1"`) {
		t.Errorf("expected system POST /_search with _type=Patient,Organization to return pat-w10-1 and org-w10-1, got %d: %s", sysPostRec.Code, sysPostRec.Body.String())
	}

	// 3c. Verify Resource/$graph (default built-in graph & custom inline GraphDefinition POST)
	graphGetReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w10-1/$graph?graph=us-core-patient-care-graph", nil)
	graphGetRec := httptest.NewRecorder()
	pgH.ServeHTTP(graphGetRec, graphGetReq)
	if graphGetRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on GET /Patient/pat-w10-1/$graph, got %d: %s", graphGetRec.Code, graphGetRec.Body.String())
	}
	for _, expectedID := range []string{`"pat-w10-1"`, `"org-w10-1"`, `"cond-w10-1"`, `"obs-w10-1"`} {
		if !strings.Contains(graphGetRec.Body.String(), expectedID) {
			t.Errorf("expected $graph Bundle to contain %s, got: %s", expectedID, graphGetRec.Body.String())
		}
	}

	// Inline GraphDefinition selecting only Vital Signs Observations for the Patient
	customGraphJSON := `{
		"resourceType": "GraphDefinition",
		"name": "VitalSignsOnlyGraph",
		"status": "active",
		"start": "Patient",
		"link": [
			{
				"target": [
					{
						"type": "Observation",
						"params": "patient={ref}&category:text=vital"
					}
				]
			}
		]
	}`
	graphPostReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/pat-w10-1/$graph", strings.NewReader(customGraphJSON))
	graphPostReq.Header.Set("Content-Type", "application/fhir+json")
	graphPostRec := httptest.NewRecorder()
	pgH.ServeHTTP(graphPostRec, graphPostReq)
	if graphPostRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on POST /Patient/pat-w10-1/$graph, got %d: %s", graphPostRec.Code, graphPostRec.Body.String())
	}
	if !strings.Contains(graphPostRec.Body.String(), `"pat-w10-1"`) || !strings.Contains(graphPostRec.Body.String(), `"obs-w10-1"`) || strings.Contains(graphPostRec.Body.String(), `"cond-w10-1"`) {
		t.Errorf("expected custom GraphDefinition $graph to include pat-w10-1 and obs-w10-1 but exclude cond-w10-1, got: %s", graphPostRec.Body.String())
	}
}

func TestWildcardIncludeOfTypeCompositeAndPreferHandlingWave11(t *testing.T) {
	srv := server.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	// 1. Verify CapabilityStatement advertises Wave 11 Observation composite & value-* search parameters
	metaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/test-tenant/metadata", nil)
	metaRec := httptest.NewRecorder()
	h.ServeHTTP(metaRec, metaReq)
	if metaRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on /metadata, got %d", metaRec.Code)
	}
	metaBody := metaRec.Body.String()
	for _, spName := range []string{
		`"code-value-concept"`,
		`"component-code-value-concept"`,
		`"combo-code-value-concept"`,
		`"code-value-date"`,
		`"code-value-string"`,
		`"value-concept"`,
		`"value-date"`,
		`"value-string"`,
	} {
		if !strings.Contains(metaBody, spName) {
			t.Errorf("expected /metadata CapabilityStatement to declare %s", spName)
		}
	}

	// 2. Verify Prefer: handling=strict rejects unrecognized search parameter with 400 Bad Request
	strictReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/test-tenant/Patient?family=Smith&unknown_bogus_param=123", nil)
	strictReq.Header.Set("Prefer", "handling=strict")
	strictRec := httptest.NewRecorder()
	h.ServeHTTP(strictRec, strictReq)
	if strictRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request under Prefer: handling=strict for unknown search param, got %d: %s", strictRec.Code, strictRec.Body.String())
	}
	if strictRec.Header().Get("Preference-Applied") != "handling=strict" {
		t.Errorf("expected Preference-Applied: handling=strict header, got %q", strictRec.Header().Get("Preference-Applied"))
	}

	// 3. Postgres-backed integration checks
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	tenant := "test-wave11-e2e"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	putRes := func(resType, id, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/"+resType+"/"+id, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s/%s failed (%d): %s", resType, id, rec.Code, rec.Body.String())
		}
	}

	putRes("Organization", "org-w11-1", `{
		"resourceType": "Organization",
		"id": "org-w11-1",
		"active": true,
		"name": "Wave11 Medical Group"
	}`)

	putRes("Patient", "pat-w11-1", `{
		"resourceType": "Patient",
		"id": "pat-w11-1",
		"meta": {
			"profile": ["http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"],
			"tag": [{"system": "http://example.org/tags", "code": "w11-cohort"}],
			"security": [{"system": "http://terminology.hl7.org/CodeSystem/v3-Confidentiality", "code": "N"}]
		},
		"identifier": [
			{
				"type": {
					"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MR"}]
				},
				"system": "http://hospital.example.org/mrn",
				"value": "MRN-W11-777"
			}
		],
		"name": [{"family": "Wildcard", "given": ["Wendy"]}],
		"gender": "female",
		"birthDate": "1985-05-20",
		"managingOrganization": {"reference": "Organization/org-w11-1"}
	}`)

	putRes("Encounter", "enc-w11-1", `{
		"resourceType": "Encounter",
		"id": "enc-w11-1",
		"status": "finished",
		"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
		"subject": {"reference": "Patient/pat-w11-1"},
		"period": {"start": "2026-02-10T09:00:00Z", "end": "2026-02-10T10:00:00Z"}
	}`)

	putRes("Observation", "obs-w11-concept", `{
		"resourceType": "Observation",
		"id": "obs-w11-concept",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "72166-2"}]},
		"subject": {"reference": "Patient/pat-w11-1"},
		"encounter": {"reference": "Encounter/enc-w11-1"},
		"effectiveDateTime": "2026-02-10T09:15:00Z",
		"valueCodeableConcept": {
			"coding": [{"system": "http://snomed.info/sct", "code": "266919005", "display": "Never smoked tobacco"}]
		},
		"component": [
			{
				"code": {"coding": [{"system": "http://loinc.org", "code": "11367-0"}]},
				"valueCodeableConcept": {
					"coding": [{"system": "http://snomed.info/sct", "code": "8517006"}]
				}
			}
		]
	}`)

	putRes("Observation", "obs-w11-str", `{
		"resourceType": "Observation",
		"id": "obs-w11-str",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "95541-9"}]},
		"subject": {"reference": "Patient/pat-w11-1"},
		"effectiveDateTime": "2026-02-10T09:20:00Z",
		"valueString": "Prefers afternoon telehealth"
	}`)

	// 3a. Verify identifier:of-type search (direct and chained)
	ofTypeReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient?identifier:of-type=http://terminology.hl7.org/CodeSystem/v2-0203|MR|MRN-W11-777", nil)
	ofTypeRec := httptest.NewRecorder()
	pgH.ServeHTTP(ofTypeRec, ofTypeReq)
	if ofTypeRec.Code != http.StatusOK || !strings.Contains(ofTypeRec.Body.String(), `"pat-w11-1"`) {
		t.Errorf("expected identifier:of-type search to match pat-w11-1, got %d: %s", ofTypeRec.Code, ofTypeRec.Body.String())
	}

	chainOfTypeReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation?subject:Patient.identifier:of-type=|MR|MRN-W11-777", nil)
	chainOfTypeRec := httptest.NewRecorder()
	pgH.ServeHTTP(chainOfTypeRec, chainOfTypeReq)
	if chainOfTypeRec.Code != http.StatusOK || !strings.Contains(chainOfTypeRec.Body.String(), `"obs-w11-concept"`) {
		t.Errorf("expected chained subject:Patient.identifier:of-type=|MR|MRN-W11-777 to match obs-w11-concept, got %d: %s", chainOfTypeRec.Code, chainOfTypeRec.Body.String())
	}

	// 3b. Verify Wildcard _include=* and _revinclude=*
	incWildReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation?_id=obs-w11-concept&_include=*", nil)
	incWildRec := httptest.NewRecorder()
	pgH.ServeHTTP(incWildRec, incWildReq)
	if incWildRec.Code != http.StatusOK || !strings.Contains(incWildRec.Body.String(), `"pat-w11-1"`) || !strings.Contains(incWildRec.Body.String(), `"enc-w11-1"`) {
		t.Errorf("expected Observation?_id=obs-w11-concept&_include=* to include pat-w11-1 and enc-w11-1, got %d: %s", incWildRec.Code, incWildRec.Body.String())
	}

	revWildReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient?_id=pat-w11-1&_revinclude=*", nil)
	revWildRec := httptest.NewRecorder()
	pgH.ServeHTTP(revWildRec, revWildReq)
	if revWildRec.Code != http.StatusOK || !strings.Contains(revWildRec.Body.String(), `"enc-w11-1"`) || !strings.Contains(revWildRec.Body.String(), `"obs-w11-concept"`) {
		t.Errorf("expected Patient?_id=pat-w11-1&_revinclude=* to include enc-w11-1 and obs-w11-concept, got %d: %s", revWildRec.Code, revWildRec.Body.String())
	}

	// 3c. Verify Multi-Hop Recursive _has Reverse Chaining
	multiHasReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient?_has:Encounter:patient:_has:Observation:encounter:code=72166-2", nil)
	multiHasRec := httptest.NewRecorder()
	pgH.ServeHTTP(multiHasRec, multiHasReq)
	if multiHasRec.Code != http.StatusOK || !strings.Contains(multiHasRec.Body.String(), `"pat-w11-1"`) {
		t.Errorf("expected multi-hop _has Patient?_has:Encounter:patient:_has:Observation:encounter:code=72166-2 to match pat-w11-1, got %d: %s", multiHasRec.Code, multiHasRec.Body.String())
	}

	// 3d. Verify Date ap (approximately) prefix & Composite Observation searches (code-value-concept, component-code-value-concept, code-value-string)
	apDateReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation?date=ap2026-02-12", nil)
	apDateRec := httptest.NewRecorder()
	pgH.ServeHTTP(apDateRec, apDateReq)
	if apDateRec.Code != http.StatusOK || !strings.Contains(apDateRec.Body.String(), `"obs-w11-concept"`) {
		t.Errorf("expected Observation?date=ap2026-02-12 to match obs-w11-concept (2026-02-10), got %d: %s", apDateRec.Code, apDateRec.Body.String())
	}

	cvcReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation?code-value-concept=http://loinc.org|72166-2$http://snomed.info/sct|266919005&component-code-value-concept=http://loinc.org|11367-0$http://snomed.info/sct|8517006", nil)
	cvcRec := httptest.NewRecorder()
	pgH.ServeHTTP(cvcRec, cvcReq)
	if cvcRec.Code != http.StatusOK || !strings.Contains(cvcRec.Body.String(), `"obs-w11-concept"`) {
		t.Errorf("expected code-value-concept & component-code-value-concept to match obs-w11-concept, got %d: %s", cvcRec.Code, cvcRec.Body.String())
	}

	cvsReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation?code-value-string=http://loinc.org|95541-9$Prefers+afternoon", nil)
	cvsRec := httptest.NewRecorder()
	pgH.ServeHTTP(cvsRec, cvsReq)
	if cvsRec.Code != http.StatusOK || !strings.Contains(cvsRec.Body.String(), `"obs-w11-str"`) {
		t.Errorf("expected code-value-string to match obs-w11-str, got %d: %s", cvsRec.Code, cvsRec.Body.String())
	}

	// 3e. Verify System-Level and Type-Level $meta aggregation
	typeMetaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/$meta", nil)
	typeMetaRec := httptest.NewRecorder()
	pgH.ServeHTTP(typeMetaRec, typeMetaReq)
	if typeMetaRec.Code != http.StatusOK || !strings.Contains(typeMetaRec.Body.String(), `"w11-cohort"`) || !strings.Contains(typeMetaRec.Body.String(), `"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"`) {
		t.Errorf("expected GET /Patient/$meta to aggregate w11-cohort tag and us-core-patient profile, got %d: %s", typeMetaRec.Code, typeMetaRec.Body.String())
	}

	sysMetaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/$meta", nil)
	sysMetaRec := httptest.NewRecorder()
	pgH.ServeHTTP(sysMetaRec, sysMetaReq)
	if sysMetaRec.Code != http.StatusOK || !strings.Contains(sysMetaRec.Body.String(), `"w11-cohort"`) {
		t.Errorf("expected GET /$meta to aggregate w11-cohort tag across tenant, got %d: %s", sysMetaRec.Code, sysMetaRec.Body.String())
	}

	// 3f. Verify Prefer: handling=lenient strips unknown params and includes OperationOutcome warning in Bundle
	lenientReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient?family=Wildcard&unknown_extra_param=abc", nil)
	lenientReq.Header.Set("Prefer", "handling=lenient")
	lenientRec := httptest.NewRecorder()
	pgH.ServeHTTP(lenientRec, lenientReq)
	if lenientRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK under Prefer: handling=lenient, got %d: %s", lenientRec.Code, lenientRec.Body.String())
	}
	if lenientRec.Header().Get("Preference-Applied") != "handling=lenient" {
		t.Errorf("expected Preference-Applied: handling=lenient, got %q", lenientRec.Header().Get("Preference-Applied"))
	}
	if !strings.Contains(lenientRec.Body.String(), `"pat-w11-1"`) || !strings.Contains(lenientRec.Body.String(), `"OperationOutcome"`) || !strings.Contains(lenientRec.Body.String(), `"mode":"outcome"`) {
		t.Errorf("expected lenient search Bundle to contain pat-w11-1 and warning OperationOutcome entry with mode=outcome, got: %s", lenientRec.Body.String())
	}
}
