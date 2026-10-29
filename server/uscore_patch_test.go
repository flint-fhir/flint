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

func TestHistoryAtListCompartmentsAndDeleteIfMatchWave12(t *testing.T) {
	srv := server.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	// 1. Verify /metadata advertises all 5 FHIR R4 CompartmentDefinitions
	metaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/test-tenant/metadata", nil)
	metaRec := httptest.NewRecorder()
	h.ServeHTTP(metaRec, metaReq)
	if metaRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on /metadata, got %d", metaRec.Code)
	}
	metaBody := metaRec.Body.String()
	for _, compURL := range []string{
		"http://hl7.org/fhir/CompartmentDefinition/patient",
		"http://hl7.org/fhir/CompartmentDefinition/encounter",
		"http://hl7.org/fhir/CompartmentDefinition/practitioner",
		"http://hl7.org/fhir/CompartmentDefinition/relatedPerson",
		"http://hl7.org/fhir/CompartmentDefinition/device",
	} {
		if !strings.Contains(metaBody, compURL) {
			t.Errorf("expected /metadata CapabilityStatement to declare %s", compURL)
		}
	}

	// 2. Verify /openapi.json declares _at, _list, wildcard compartment routes, and If-Match on DELETE
	oaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/test-tenant/openapi.json", nil)
	oaRec := httptest.NewRecorder()
	h.ServeHTTP(oaRec, oaReq)
	if oaRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on /openapi.json, got %d", oaRec.Code)
	}
	oaBody := oaRec.Body.String()
	for _, token := range []string{
		`"/Patient/{id}/*"`,
		`"/Encounter/{id}/*"`,
		`"/Practitioner/{id}/*"`,
		`"/RelatedPerson/{id}/*"`,
		`"/Device/{id}/*"`,
		`"_at"`,
	} {
		if !strings.Contains(oaBody, token) {
			t.Errorf("expected /openapi.json to include %s", token)
		}
	}

	// 3. Verify malformed If-Match on DELETE and invalid _at / _summary on _history return 400 Bad Request
	badDelReq := httptest.NewRequest(http.MethodDelete, "/fhir/r4/test-tenant/Patient/pat-1", nil)
	badDelReq.Header.Set("If-Match", "bogus-etag")
	badDelRec := httptest.NewRecorder()
	h.ServeHTTP(badDelRec, badDelReq)
	if badDelRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request on malformed If-Match header for DELETE, got %d: %s", badDelRec.Code, badDelRec.Body.String())
	}

	badAtReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/test-tenant/Patient/_history?_at=not-a-valid-date", nil)
	badAtRec := httptest.NewRecorder()
	h.ServeHTTP(badAtRec, badAtReq)
	if badAtRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request on invalid _at parameter, got %d: %s", badAtRec.Code, badAtRec.Body.String())
	}

	badSumReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/test-tenant/Patient/_history?_summary=bogus", nil)
	badSumRec := httptest.NewRecorder()
	h.ServeHTTP(badSumRec, badSumReq)
	if badSumRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request on invalid _summary parameter on _history, got %d: %s", badSumRec.Code, badSumRec.Body.String())
	}

	// 4. Postgres-backed integration checks
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	tenant := "test-wave12-e2e"
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

	putRes("Patient", "pat-w12-1", `{
		"resourceType": "Patient",
		"id": "pat-w12-1",
		"name": [{"family": "Compartment", "given": ["Clara"]}],
		"gender": "female",
		"birthDate": "1990-03-15",
		"text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\">Clara Compartment v1</div>"}
	}`)

	// Update Patient/pat-w12-1 to create version 2
	putRes("Patient", "pat-w12-1", `{
		"resourceType": "Patient",
		"id": "pat-w12-1",
		"name": [{"family": "Compartment", "given": ["Clara", "Marie"]}],
		"gender": "female",
		"birthDate": "1990-03-15",
		"text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\">Clara Compartment v2</div>"}
	}`)

	putRes("Practitioner", "prac-w12-1", `{
		"resourceType": "Practitioner",
		"id": "prac-w12-1",
		"name": [{"family": "Pasteur", "given": ["Louis"]}]
	}`)

	putRes("RelatedPerson", "rel-w12-1", `{
		"resourceType": "RelatedPerson",
		"id": "rel-w12-1",
		"patient": {"reference": "Patient/pat-w12-1"},
		"name": [{"family": "Compartment", "given": ["Robert"]}]
	}`)

	putRes("Device", "dev-w12-1", `{
		"resourceType": "Device",
		"id": "dev-w12-1",
		"status": "active",
		"type": {"coding": [{"system": "http://snomed.info/sct", "code": "706689003"}]},
		"patient": {"reference": "Patient/pat-w12-1"}
	}`)

	putRes("Encounter", "enc-w12-1", `{
		"resourceType": "Encounter",
		"id": "enc-w12-1",
		"status": "finished",
		"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
		"subject": {"reference": "Patient/pat-w12-1"},
		"participant": [{"individual": {"reference": "Practitioner/prac-w12-1"}}]
	}`)

	putRes("Observation", "obs-w12-1", `{
		"resourceType": "Observation",
		"id": "obs-w12-1",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
		"subject": {"reference": "Patient/pat-w12-1"},
		"encounter": {"reference": "Encounter/enc-w12-1"},
		"performer": [
			{"reference": "Practitioner/prac-w12-1"},
			{"reference": "RelatedPerson/rel-w12-1"}
		],
		"device": {"reference": "Device/dev-w12-1"},
		"valueQuantity": {"value": 72, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
	}`)

	putRes("MedicationRequest", "medreq-w12-1", `{
		"resourceType": "MedicationRequest",
		"id": "medreq-w12-1",
		"status": "active",
		"intent": "order",
		"subject": {"reference": "Patient/pat-w12-1"},
		"encounter": {"reference": "Encounter/enc-w12-1"},
		"requester": {"reference": "Practitioner/prac-w12-1"},
		"medicationCodeableConcept": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "197361"}]}
	}`)

	putRes("List", "list-w12-1", `{
		"resourceType": "List",
		"id": "list-w12-1",
		"status": "current",
		"mode": "working",
		"entry": [
			{"item": {"reference": "Patient/pat-w12-1"}},
			{"item": {"reference": "Observation/obs-w12-1"}}
		]
	}`)

	// 4a. Verify vread with _summary=true and _elements=gender,birthDate
	vreadSumReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w12-1/_history/1?_summary=true", nil)
	vreadSumRec := httptest.NewRecorder()
	pgH.ServeHTTP(vreadSumRec, vreadSumReq)
	if vreadSumRec.Code != http.StatusOK || !strings.Contains(vreadSumRec.Body.String(), `"SUBSETTED"`) || strings.Contains(vreadSumRec.Body.String(), `"Clara Compartment v1"`) {
		t.Errorf("expected vread ?_summary=true to strip narrative text and add SUBSETTED tag, got %d: %s", vreadSumRec.Code, vreadSumRec.Body.String())
	}

	vreadElemReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w12-1/_history/2?_elements=gender,birthDate", nil)
	vreadElemRec := httptest.NewRecorder()
	pgH.ServeHTTP(vreadElemRec, vreadElemReq)
	if vreadElemRec.Code != http.StatusOK || !strings.Contains(vreadElemRec.Body.String(), `"SUBSETTED"`) || strings.Contains(vreadElemRec.Body.String(), `"Compartment"`) {
		t.Errorf("expected vread ?_elements=gender,birthDate to omit name and add SUBSETTED tag, got %d: %s", vreadElemRec.Code, vreadElemRec.Body.String())
	}

	// 4b. Verify _history pagination (Bundle.link), response.location, _summary=count, _at, and _list
	histPageReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w12-1/_history?_count=1&_offset=0", nil)
	histPageRec := httptest.NewRecorder()
	pgH.ServeHTTP(histPageRec, histPageReq)
	if histPageRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on _history, got %d: %s", histPageRec.Code, histPageRec.Body.String())
	}
	if !strings.Contains(histPageRec.Body.String(), `"Patient/pat-w12-1/_history/2"`) || !strings.Contains(histPageRec.Body.String(), `"relation":"next"`) {
		t.Errorf("expected _history?_count=1 to include response.location Patient/pat-w12-1/_history/2 and next pagination link, got: %s", histPageRec.Body.String())
	}

	histCntReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w12-1/_history?_summary=count", nil)
	histCntRec := httptest.NewRecorder()
	pgH.ServeHTTP(histCntRec, histCntReq)
	if histCntRec.Code != http.StatusOK || strings.Contains(histCntRec.Body.String(), `"entry"`) || !strings.Contains(histCntRec.Body.String(), `"total":2`) {
		t.Errorf("expected _history?_summary=count to return total=2 and omit entry, got %d: %s", histCntRec.Code, histCntRec.Body.String())
	}

	// Point-in-time _at in the future should return only the version current at that instant (version 2, total=1)
	histAtInstReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w12-1/_history?_at=2099-12-31T23:59:59Z", nil)
	histAtInstRec := httptest.NewRecorder()
	pgH.ServeHTTP(histAtInstRec, histAtInstReq)
	if histAtInstRec.Code != http.StatusOK || !strings.Contains(histAtInstRec.Body.String(), `"total":1`) || !strings.Contains(histAtInstRec.Body.String(), `"Patient/pat-w12-1/_history/2"`) {
		t.Errorf("expected point-in-time _at=2099-12-31T23:59:59Z to return only current version 2 (total=1), got %d: %s", histAtInstRec.Code, histAtInstRec.Body.String())
	}

	// Range _at with ge/le comparators should return all versions created within the window (total=2)
	histAtRangeReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w12-1/_history?_at=ge2020-01-01&_at=le2099-12-31", nil)
	histAtRangeRec := httptest.NewRecorder()
	pgH.ServeHTTP(histAtRangeRec, histAtRangeReq)
	if histAtRangeRec.Code != http.StatusOK || !strings.Contains(histAtRangeRec.Body.String(), `"total":2`) {
		t.Errorf("expected range _at=ge2020-01-01&_at=le2099-12-31 to return total=2, got %d: %s", histAtRangeRec.Code, histAtRangeRec.Body.String())
	}

	// System _history filtered by _list=list-w12-1 should include pat-w12-1 (2 versions) + obs-w12-1 (1 version) = 3 versions, and exclude prac-w12-1
	histListReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/_history?_list=list-w12-1", nil)
	histListRec := httptest.NewRecorder()
	pgH.ServeHTTP(histListRec, histListReq)
	if histListRec.Code != http.StatusOK || !strings.Contains(histListRec.Body.String(), `"total":3`) || strings.Contains(histListRec.Body.String(), `"prac-w12-1"`) {
		t.Errorf("expected /_history?_list=list-w12-1 to return total=3 (pat-w12-1 + obs-w12-1) and exclude prac-w12-1, got %d: %s", histListRec.Code, histListRec.Body.String())
	}

	// 4c. Verify all 5 FHIR R4 Compartments (Patient, Encounter, Practitioner, RelatedPerson, Device) + Wildcard /*
	for _, tc := range []struct {
		method string
		path   string
		body   string
		wantID string
	}{
		{http.MethodGet, "/Encounter/enc-w12-1/Observation", "", `"obs-w12-1"`},
		{http.MethodPost, "/Encounter/enc-w12-1/Observation/_search", "code=8867-4", `"obs-w12-1"`},
		{http.MethodGet, "/Practitioner/prac-w12-1/MedicationRequest", "", `"medreq-w12-1"`},
		{http.MethodGet, "/RelatedPerson/rel-w12-1/Observation", "", `"obs-w12-1"`},
		{http.MethodGet, "/Device/dev-w12-1/Observation", "", `"obs-w12-1"`},
		{http.MethodGet, "/Patient/pat-w12-1/*?_type=Observation,MedicationRequest", "", `"medreq-w12-1"`},
		{http.MethodGet, "/Encounter/enc-w12-1/*", "", `"obs-w12-1"`},
		{http.MethodGet, "/Practitioner/prac-w12-1/*", "", `"medreq-w12-1"`},
	} {
		var reqBody io.Reader
		if tc.body != "" {
			reqBody = strings.NewReader(tc.body)
		}
		req := httptest.NewRequest(tc.method, "/fhir/r4/"+tenant+tc.path, reqBody)
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.wantID) {
			t.Errorf("%s %s expected 200 OK containing %s, got %d: %s", tc.method, tc.path, tc.wantID, rec.Code, rec.Body.String())
		}
	}

	// 4d. Verify ETag Optimistic Concurrency Control (If-Match) on DELETE (Instance, Conditional, and Transaction Bundle)
	// Instance DELETE with stale ETag W/"1" on pat-w12-1 (current version is 2) -> 412 Precondition Failed
	delStaleReq := httptest.NewRequest(http.MethodDelete, "/fhir/r4/"+tenant+"/Patient/pat-w12-1", nil)
	delStaleReq.Header.Set("If-Match", `W/"1"`)
	delStaleRec := httptest.NewRecorder()
	pgH.ServeHTTP(delStaleRec, delStaleReq)
	if delStaleRec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 Precondition Failed on DELETE with stale If-Match W/\"1\", got %d: %s", delStaleRec.Code, delStaleRec.Body.String())
	}

	// Conditional DELETE with stale ETag W/"1" on pat-w12-1 -> 412 Precondition Failed
	condDelStaleReq := httptest.NewRequest(http.MethodDelete, "/fhir/r4/"+tenant+"/Patient?_id=pat-w12-1", nil)
	condDelStaleReq.Header.Set("If-Match", `W/"1"`)
	condDelStaleRec := httptest.NewRecorder()
	pgH.ServeHTTP(condDelStaleRec, condDelStaleReq)
	if condDelStaleRec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 Precondition Failed on Conditional DELETE with stale If-Match W/\"1\", got %d: %s", condDelStaleRec.Code, condDelStaleRec.Body.String())
	}

	// Transaction Bundle DELETE entry with stale ifMatch W/"99" -> 412 Precondition Failed
	txDelBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"request": {
					"method": "DELETE",
					"url": "Observation/obs-w12-1",
					"ifMatch": "W/\"99\""
				}
			}
		]
	}`
	txDelReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant, strings.NewReader(txDelBundle))
	txDelReq.Header.Set("Content-Type", "application/fhir+json")
	txDelRec := httptest.NewRecorder()
	pgH.ServeHTTP(txDelRec, txDelReq)
	if txDelRec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 Precondition Failed on transaction Bundle DELETE with stale ifMatch, got %d: %s", txDelRec.Code, txDelRec.Body.String())
	}

	// Instance DELETE with matching ETag W/"1" on Observation/obs-w12-1 -> 204 No Content with ETag W/"2"
	delMatchReq := httptest.NewRequest(http.MethodDelete, "/fhir/r4/"+tenant+"/Observation/obs-w12-1", nil)
	delMatchReq.Header.Set("If-Match", `W/"1"`)
	delMatchRec := httptest.NewRecorder()
	pgH.ServeHTTP(delMatchRec, delMatchReq)
	if delMatchRec.Code != http.StatusNoContent || delMatchRec.Header().Get("ETag") != `W/"2"` {
		t.Fatalf("expected 204 No Content with ETag W/\"2\" on matching If-Match DELETE, got %d (ETag=%q): %s", delMatchRec.Code, delMatchRec.Header().Get("ETag"), delMatchRec.Body.String())
	}
}

func TestFilterUCUMContentBulkUntilAndGroupEverythingWave13(t *testing.T) {
	srv := server.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	// 1. Verify ParseFilterExpr and ValidateSearchQuery for _filter, _content, _text, and quantity ap/sa/eb
	expr, err := postgres.ParseFilterExpr(`(code eq http://loinc.org|8867-4 or code eq http://loinc.org|29463-7) and status ne "cancelled" and not (value-quantity lt 50|http://unitsofmeasure.org|kg)`)
	if err != nil || expr.Kind != "and" {
		t.Fatalf("expected ParseFilterExpr to parse nested boolean expression, got kind=%q err=%v", expr.Kind, err)
	}
	if _, err := postgres.ParseFilterExpr(`code invalidop "val"`); err == nil {
		t.Fatalf("expected ParseFilterExpr to reject unknown operator")
	}
	if _, err := postgres.ParseFilterExpr(`(code eq "123"`); err == nil {
		t.Fatalf("expected ParseFilterExpr to reject unclosed parenthesis")
	}
	if qOp, err := postgres.ParseQuantityOp("ap70|http://unitsofmeasure.org|kg"); err != nil || qOp.Prefix != "ap" || qOp.Value != 70 || qOp.Code != "kg" {
		t.Fatalf("expected ParseQuantityOp to parse ap70|http://unitsofmeasure.org|kg, got %+v, err=%v", qOp, err)
	}

	// 2. Verify /metadata CapabilityStatement and /openapi.json advertise _filter, _content, _text, Group/$everything, and $export _until/_elements
	metaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/demo/metadata", nil)
	metaRec := httptest.NewRecorder()
	h.ServeHTTP(metaRec, metaReq)
	if metaRec.Code != http.StatusOK {
		t.Fatalf("GET /metadata got %d", metaRec.Code)
	}
	metaBody := metaRec.Body.String()
	for _, needle := range []string{`"_filter"`, `"_content"`, `"_text"`, `"http://hl7.org/fhir/OperationDefinition/Group-everything"`} {
		if !strings.Contains(metaBody, needle) {
			t.Errorf("expected CapabilityStatement to contain %s", needle)
		}
	}

	oaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/demo/openapi.json", nil)
	oaRec := httptest.NewRecorder()
	h.ServeHTTP(oaRec, oaReq)
	if oaRec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json got %d", oaRec.Code)
	}
	oaBody := oaRec.Body.String()
	for _, needle := range []string{`"/Group/{id}/$everything"`, `"_filter"`, `"_content"`, `"_text"`, `"_until"`} {
		if !strings.Contains(oaBody, needle) {
			t.Errorf("expected OpenAPI spec to contain %s", needle)
		}
	}

	// 3. End-to-End PostgreSQL Verification
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	tenant := "test-wave13-e2e"
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

	// Seed Patient, narrative Condition, weight & temperature Observations, and a cohort Group
	putRes("Patient", "pat-w13-1", `{
		"resourceType": "Patient",
		"id": "pat-w13-1",
		"active": true,
		"gender": "female",
		"birthDate": "1985-04-12",
		"name": [{"family": "Chalmers", "given": ["Alice"]}],
		"text": {
			"status": "generated",
			"div": "<div xmlns=\"http://www.w3.org/1999/xhtml\"> Longitudinal cardiology cohort patient <b>Alice Chalmers</b> </div>"
		}
	}`)
	putRes("Patient", "pat-w13-2", `{
		"resourceType": "Patient",
		"id": "pat-w13-2",
		"active": true,
		"gender": "male",
		"birthDate": "1978-09-20",
		"name": [{"family": "Baggins", "given": ["Bilbo"]}]
	}`)
	putRes("Condition", "cond-w13-1", `{
		"resourceType": "Condition",
		"id": "cond-w13-1",
		"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
		"verificationStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}]},
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item"}]}],
		"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Essential hypertension"}]},
		"subject": {"reference": "Patient/pat-w13-1"},
		"text": {
			"status": "generated",
			"div": "<div xmlns=\"http://www.w3.org/1999/xhtml\">Refractory nocturnal migraine aura with photophobia</div>"
		}
	}`)
	// Weight Observation stored as 70 kg
	putRes("Observation", "obs-w13-weight", `{
		"resourceType": "Observation",
		"id": "obs-w13-weight",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "29463-7", "display": "Body weight"}]},
		"subject": {"reference": "Patient/pat-w13-1"},
		"effectiveDateTime": "2026-03-01T10:00:00Z",
		"valueQuantity": {"value": 70.0, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"}
	}`)
	// Temperature Observation stored as 37 Cel
	putRes("Observation", "obs-w13-temp", `{
		"resourceType": "Observation",
		"id": "obs-w13-temp",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "8310-5", "display": "Body temperature"}]},
		"subject": {"reference": "Patient/pat-w13-1"},
		"effectiveDateTime": "2026-03-02T10:00:00Z",
		"valueQuantity": {"value": 37.0, "unit": "Cel", "system": "http://unitsofmeasure.org", "code": "Cel"}
	}`)
	putRes("Group", "grp-w13-1", `{
		"resourceType": "Group",
		"id": "grp-w13-1",
		"active": true,
		"type": "person",
		"actual": true,
		"name": "Wave 13 Cohort",
		"member": [
			{"entity": {"reference": "Patient/pat-w13-1"}}
		]
	}`)

	// 3a. Verify _text and _content full-text / narrative search
	for _, tc := range []struct {
		query   string
		wantID  string
		exclude string
	}{
		{"/Condition?_text=photophobia", `"cond-w13-1"`, ""},
		{"/Condition?_content=photophobia", `"cond-w13-1"`, ""},
		{"/Condition?_content=hypertension", `"cond-w13-1"`, ""},
		{"/Patient?_text=cardiology", `"pat-w13-1"`, `"pat-w13-2"`},
	} {
		req := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+tc.query, nil)
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.wantID) {
			t.Errorf("GET %s expected 200 OK containing %s, got %d: %s", tc.query, tc.wantID, rec.Code, rec.Body.String())
		}
		if tc.exclude != "" && strings.Contains(rec.Body.String(), tc.exclude) {
			t.Errorf("GET %s expected to exclude %s, got %s", tc.query, tc.exclude, rec.Body.String())
		}
	}

	// 3b. Verify Canonical UCUM Quantity Conversion & ap / sa / eb prefixes
	for _, tc := range []struct {
		desc   string
		query  string
		wantID string
	}{
		{"kg stored, queried as 70000 g", "/Observation?code=29463-7&value-quantity=70000|http://unitsofmeasure.org|g", `"obs-w13-weight"`},
		{"kg stored, queried as gt150 [lb_av] (70 kg ≈ 154.32 lb)", "/Observation?code=29463-7&value-quantity=gt150|http://unitsofmeasure.org|[lb_av]", `"obs-w13-weight"`},
		{"kg stored, queried with ap72 (±10%)", "/Observation?code=29463-7&value-quantity=ap72|http://unitsofmeasure.org|kg", `"obs-w13-weight"`},
		{"kg stored, queried with sa65", "/Observation?code=29463-7&value-quantity=sa65|http://unitsofmeasure.org|kg", `"obs-w13-weight"`},
		{"kg stored, queried with eb75", "/Observation?code=29463-7&value-quantity=eb75|http://unitsofmeasure.org|kg", `"obs-w13-weight"`},
		{"composite code-value-quantity with UCUM g conversion", "/Observation?code-value-quantity=http://loinc.org|29463-7$70000|http://unitsofmeasure.org|g", `"obs-w13-weight"`},
		{"Cel stored (37 °C), queried as ap98.6 [degF]", "/Observation?code=8310-5&value-quantity=ap98.6|http://unitsofmeasure.org|[degF]", `"obs-w13-temp"`},
		{"Cel stored (37 °C = 310.15 K), queried as gt300 K", "/Observation?code=8310-5&value-quantity=gt300|http://unitsofmeasure.org|K", `"obs-w13-temp"`},
	} {
		req := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+tc.query, nil)
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.wantID) {
			t.Errorf("%s (%s) expected 200 OK containing %s, got %d: %s", tc.desc, tc.query, tc.wantID, rec.Code, rec.Body.String())
		}
	}

	// 3c. Verify FHIR R4 _filter boolean search expressions (and, or, not, parentheses, co, sw, ew, eq, ne, gt, ap, pr, re, chained)
	for _, tc := range []struct {
		desc    string
		resType string
		filter  string
		wantID  string
		exclude string
	}{
		{
			"boolean OR + AND + NOT on Patient",
			"Patient",
			`(family sw "Chal" or family eq "NonExistent") and gender eq "female" and not (gender eq "male")`,
			`"pat-w13-1"`,
			`"pat-w13-2"`,
		},
		{
			"string co / ew andbirthDate ge/le on Patient",
			"Patient",
			`family co "alm" and family ew "mers" and birthdate ge 1980-01-01 and birthdate le 1990-12-31`,
			`"pat-w13-1"`,
			`"pat-w13-2"`,
		},
		{
			"quantity UCUM conversion + chained patient.name inside _filter on Observation",
			"Observation",
			`(code eq http://loinc.org|29463-7 and value-quantity eq 70000|http://unitsofmeasure.org|g) and subject:Patient.family eq "Chalmers" and status pr true`,
			`"obs-w13-weight"`,
			`"obs-w13-temp"`,
		},
	} {
		req := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/"+tc.resType+"?_filter="+url.QueryEscape(tc.filter), nil)
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.wantID) {
			t.Errorf("_filter %s expected 200 OK containing %s, got %d: %s", tc.desc, tc.wantID, rec.Code, rec.Body.String())
		}
		if tc.exclude != "" && strings.Contains(rec.Body.String(), tc.exclude) {
			t.Errorf("_filter %s expected to exclude %s, got %s", tc.desc, tc.exclude, rec.Body.String())
		}
	}

	// 3d. Verify Group/{id}/$everything cohort operation + _until & _elements on $everything and $export
	grpEvReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Group/grp-w13-1/$everything?_until=2099-12-31&_elements=Patient.name,Observation.code", nil)
	grpEvRec := httptest.NewRecorder()
	pgH.ServeHTTP(grpEvRec, grpEvReq)
	if grpEvRec.Code != http.StatusOK {
		t.Fatalf("GET /Group/grp-w13-1/$everything got %d: %s", grpEvRec.Code, grpEvRec.Body.String())
	}
	grpEvBody := grpEvRec.Body.String()
	for _, want := range []string{`"Group/grp-w13-1"`, `"Patient/pat-w13-1"`, `"Observation/obs-w13-weight"`, `"Condition/cond-w13-1"`, `"SUBSETTED"`} {
		if !strings.Contains(grpEvBody, want) {
			t.Errorf("expected Group/grp-w13-1/$everything to contain %s, got %s", want, grpEvBody)
		}
	}
	if strings.Contains(grpEvBody, `"Patient/pat-w13-2"`) {
		t.Errorf("expected Group/grp-w13-1/$everything to exclude non-member pat-w13-2")
	}

	// Group/$everything with _until in the past (2000-01-01) should exclude clinical compartment resources created today
	grpEvPastReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Group/grp-w13-1/$everything?_until=2000-01-01", nil)
	grpEvPastRec := httptest.NewRecorder()
	pgH.ServeHTTP(grpEvPastRec, grpEvPastReq)
	if grpEvPastRec.Code != http.StatusOK || strings.Contains(grpEvPastRec.Body.String(), `"Observation/obs-w13-weight"`) {
		t.Errorf("expected Group/$everything?_until=2000-01-01 to exclude obs-w13-weight, got %d: %s", grpEvPastRec.Code, grpEvPastRec.Body.String())
	}

	// Group/$export with _until=2099-12-31&_elements=Patient.name,Observation.code
	grpExpReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Group/grp-w13-1/$export?_until=2099-12-31&_elements=Patient.name,Observation.code", nil)
	grpExpRec := httptest.NewRecorder()
	pgH.ServeHTTP(grpExpRec, grpExpReq)
	if grpExpRec.Code != http.StatusOK {
		t.Fatalf("GET /Group/grp-w13-1/$export got %d: %s", grpExpRec.Code, grpExpRec.Body.String())
	}
	grpExpBody := grpExpRec.Body.String()
	if !strings.Contains(grpExpBody, `"pat-w13-1"`) || !strings.Contains(grpExpBody, `"obs-w13-weight"`) || !strings.Contains(grpExpBody, `"SUBSETTED"`) {
		t.Errorf("expected Group/$export with _until and _elements to include pat-w13-1, obs-w13-weight, and SUBSETTED tag, got %s", grpExpBody)
	}
	if strings.Contains(grpExpBody, `"birthDate"`) {
		t.Errorf("expected Group/$export?_elements=Patient.name,Observation.code to strip Patient.birthDate, got %s", grpExpBody)
	}

	// Group/$export with _until=2000-01-01 should yield 0 NDJSON lines
	grpExpPastReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Group/grp-w13-1/$export?_until=2000-01-01", nil)
	grpExpPastRec := httptest.NewRecorder()
	pgH.ServeHTTP(grpExpPastRec, grpExpPastReq)
	if grpExpPastRec.Code != http.StatusOK || strings.TrimSpace(grpExpPastRec.Body.String()) != "" {
		t.Errorf("expected empty NDJSON on Group/$export?_until=2000-01-01, got %d: %q", grpExpPastRec.Code, grpExpPastRec.Body.String())
	}

	// Invalid _until earlier than _since must return 400 Bad Request
	badUntilReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/$export?_since=2026-06-01&_until=2026-01-01", nil)
	badUntilRec := httptest.NewRecorder()
	pgH.ServeHTTP(badUntilRec, badUntilReq)
	if badUntilRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request when _until < _since, got %d: %s", badUntilRec.Code, badUntilRec.Body.String())
	}
}

func TestCustomSearchParamReindexPlanDefApplyAndEverythingDatesWave14(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-wave14-e2e"
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

	// 1. Seed a Patient with US Core race, ethnicity, birthsex extensions AND a custom extension (vip-tier) BEFORE the custom SearchParameter is registered
	putRes("Patient", "pat-w14-1", `{
		"resourceType": "Patient",
		"id": "pat-w14-1",
		"active": true,
		"gender": "female",
		"birthDate": "1980-06-15",
		"name": [{"family": "Kowalski", "given": ["Elena"]}],
		"maritalStatus": {
			"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v3-MaritalStatus", "code": "M", "display": "Married"}]
		},
		"extension": [
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
				"extension": [
					{"url": "ombCategory", "valueCoding": {"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2106-3", "display": "White"}},
					{"url": "text", "valueString": "White"}
				]
			},
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
				"extension": [
					{"url": "ombCategory", "valueCoding": {"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2186-5", "display": "Not Hispanic or Latino"}},
					{"url": "text", "valueString": "Not Hispanic or Latino"}
				]
			},
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex",
				"valueCode": "F"
			},
			{
				"url": "http://hospital.example.org/StructureDefinition/vip-tier",
				"valueCode": "platinum"
			}
		]
	}`)

	// 1a. Built-in US Core extension search parameters (race, ethnicity, birthsex) work out-of-the-box
	usCoreReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient?race=2106-3&ethnicity=2186-5&birthsex=F", nil)
	usCoreRec := httptest.NewRecorder()
	pgH.ServeHTTP(usCoreRec, usCoreReq)
	if usCoreRec.Code != http.StatusOK || !strings.Contains(usCoreRec.Body.String(), `"pat-w14-1"`) {
		t.Fatalf("expected US Core extension search (?race=2106-3&ethnicity=2186-5&birthsex=F) to match pat-w14-1, got %d: %s", usCoreRec.Code, usCoreRec.Body.String())
	}

	// 2. Register runtime custom SearchParameters (vip-tier extension + marital-status field)
	putRes("SearchParameter", "sp-vip-tier", `{
		"resourceType": "SearchParameter",
		"id": "sp-vip-tier",
		"url": "http://hospital.example.org/SearchParameter/Patient-vip-tier",
		"name": "PatientVIPTier",
		"status": "active",
		"description": "Search Patients by hospital VIP tier extension",
		"code": "vip-tier",
		"base": ["Patient"],
		"type": "token",
		"expression": "Patient.extension.where(url='http://hospital.example.org/StructureDefinition/vip-tier')"
	}`)
	putRes("SearchParameter", "sp-marital-status", `{
		"resourceType": "SearchParameter",
		"id": "sp-marital-status",
		"url": "http://hl7.org/fhir/SearchParameter/Patient-marital-status",
		"name": "PatientMaritalStatus",
		"status": "active",
		"description": "Search Patients by maritalStatus",
		"code": "marital-status",
		"base": ["Patient"],
		"type": "token",
		"expression": "Patient.maritalStatus"
	}`)

	// 2a. Trigger $reindex on Patient so pre-existing pat-w14-1 is indexed under the newly registered SearchParameters
	reidxReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$reindex", nil)
	reidxRec := httptest.NewRecorder()
	pgH.ServeHTTP(reidxRec, reidxReq)
	if reidxRec.Code != http.StatusOK || !strings.Contains(reidxRec.Body.String(), `"Reindexed 1 resource(s)"`) {
		t.Fatalf("POST /Patient/$reindex expected 200 OK with Reindexed 1 resource(s), got %d: %s", reidxRec.Code, reidxRec.Body.String())
	}

	// 2b. Search using the newly reindexed custom SearchParameters (and inside _filter)
	vipReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient?vip-tier=platinum&marital-status=M", nil)
	vipRec := httptest.NewRecorder()
	pgH.ServeHTTP(vipRec, vipReq)
	if vipRec.Code != http.StatusOK || !strings.Contains(vipRec.Body.String(), `"pat-w14-1"`) {
		t.Fatalf("GET /Patient?vip-tier=platinum&marital-status=M expected pat-w14-1 after $reindex, got %d: %s", vipRec.Code, vipRec.Body.String())
	}

	// 2c. Verify CapabilityStatement (/metadata) and OpenAPI (/openapi.json) advertise custom SearchParameters and $reindex / $apply
	metaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/metadata", nil)
	metaRec := httptest.NewRecorder()
	pgH.ServeHTTP(metaRec, metaReq)
	if metaRec.Code != http.StatusOK || !strings.Contains(metaRec.Body.String(), `"vip-tier"`) || !strings.Contains(metaRec.Body.String(), `"reindex"`) || !strings.Contains(metaRec.Body.String(), `"apply"`) {
		t.Errorf("expected /metadata to advertise vip-tier, reindex, and apply, got %d", metaRec.Code)
	}
	oaReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/openapi.json", nil)
	oaRec := httptest.NewRecorder()
	pgH.ServeHTTP(oaRec, oaReq)
	if oaRec.Code != http.StatusOK || !strings.Contains(oaRec.Body.String(), `"vip-tier"`) || !strings.Contains(oaRec.Body.String(), `"/PlanDefinition/$apply"`) || !strings.Contains(oaRec.Body.String(), `"/$reindex"`) {
		t.Errorf("expected /openapi.json to advertise vip-tier, /PlanDefinition/$apply, and /$reindex, got %d", oaRec.Code)
	}

	// 3. Seed clinical Condition, dated Observations, Encounter, Group, and MedicationRequest with inline contained Medication (#med-inline-1)
	putRes("Condition", "cond-w14-htn", `{
		"resourceType": "Condition",
		"id": "cond-w14-htn",
		"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
		"verificationStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}]},
		"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Essential hypertension"}]},
		"subject": {"reference": "Patient/pat-w14-1"},
		"onsetDateTime": "2026-03-10"
	}`)
	putRes("Encounter", "enc-w14-1", `{
		"resourceType": "Encounter",
		"id": "enc-w14-1",
		"status": "finished",
		"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
		"subject": {"reference": "Patient/pat-w14-1"},
		"period": {"start": "2026-03-15T09:00:00Z", "end": "2026-03-15T10:00:00Z"}
	}`)
	putRes("Observation", "obs-w14-jan", `{
		"resourceType": "Observation",
		"id": "obs-w14-jan",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]},
		"subject": {"reference": "Patient/pat-w14-1"},
		"encounter": {"reference": "Encounter/enc-w14-1"},
		"effectiveDateTime": "2026-01-10T08:00:00Z",
		"valueQuantity": {"value": 68.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
	}`)
	putRes("Observation", "obs-w14-mar", `{
		"resourceType": "Observation",
		"id": "obs-w14-mar",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure panel"}]},
		"subject": {"reference": "Patient/pat-w14-1"},
		"encounter": {"reference": "Encounter/enc-w14-1"},
		"effectiveDateTime": "2026-03-15T09:30:00Z",
		"valueQuantity": {"value": 148.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
	}`)
	putRes("MedicationRequest", "medreq-w14-contained", `{
		"resourceType": "MedicationRequest",
		"id": "medreq-w14-contained",
		"status": "active",
		"intent": "order",
		"contained": [
			{
				"resourceType": "Medication",
				"id": "med-inline-1",
				"code": {
					"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "314076", "display": "Lisinopril 10 MG Oral Tablet"}],
					"text": "Lisinopril 10 MG Oral Tablet"
				}
			}
		],
		"medicationReference": {"reference": "#med-inline-1"},
		"subject": {"reference": "Patient/pat-w14-1"},
		"authoredOn": "2026-03-15T09:45:00Z"
	}`)

	// 3a. Verify contained resource round-trip and chained search (?medication.code=314076 and ?_content=Lisinopril)
	readMedReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/MedicationRequest/medreq-w14-contained", nil)
	readMedRec := httptest.NewRecorder()
	pgH.ServeHTTP(readMedRec, readMedReq)
	if readMedRec.Code != http.StatusOK || !strings.Contains(readMedRec.Body.String(), `"#med-inline-1"`) || !strings.Contains(readMedRec.Body.String(), `"resourceType":"Medication"`) {
		t.Fatalf("GET /MedicationRequest/medreq-w14-contained expected round-tripped contained Medication and #med-inline-1 reference, got %d: %s", readMedRec.Code, readMedRec.Body.String())
	}

	chainContainedReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/MedicationRequest?medication.code=314076&_content=Lisinopril", nil)
	chainContainedRec := httptest.NewRecorder()
	pgH.ServeHTTP(chainContainedRec, chainContainedReq)
	if chainContainedRec.Code != http.StatusOK || !strings.Contains(chainContainedRec.Body.String(), `"medreq-w14-contained"`) {
		t.Fatalf("GET /MedicationRequest?medication.code=314076&_content=Lisinopril expected medreq-w14-contained via inline #med-inline-1 index, got %d: %s", chainContainedRec.Code, chainContainedRec.Body.String())
	}

	// 4. Verify $everything clinical start & end date filtering on Patient, Encounter, and Group
	patEvMarReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w14-1/$everything?start=2026-03-01&end=2026-03-31", nil)
	patEvMarRec := httptest.NewRecorder()
	pgH.ServeHTTP(patEvMarRec, patEvMarReq)
	if patEvMarRec.Code != http.StatusOK {
		t.Fatalf("GET /Patient/pat-w14-1/$everything?start=2026-03-01&end=2026-03-31 got %d: %s", patEvMarRec.Code, patEvMarRec.Body.String())
	}
	patEvMarBody := patEvMarRec.Body.String()
	if !strings.Contains(patEvMarBody, `"Patient/pat-w14-1"`) || !strings.Contains(patEvMarBody, `"Observation/obs-w14-mar"`) || !strings.Contains(patEvMarBody, `"Condition/cond-w14-htn"`) {
		t.Errorf("expected March $everything window to include Patient/pat-w14-1, obs-w14-mar, and cond-w14-htn, got %s", patEvMarBody)
	}
	if strings.Contains(patEvMarBody, `"Observation/obs-w14-jan"`) {
		t.Errorf("expected March $everything window to exclude January obs-w14-jan, got %s", patEvMarBody)
	}

	encEvJanReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Encounter/enc-w14-1/$everything?start=2026-01-01&end=2026-01-31", nil)
	encEvJanRec := httptest.NewRecorder()
	pgH.ServeHTTP(encEvJanRec, encEvJanReq)
	if encEvJanRec.Code != http.StatusOK || !strings.Contains(encEvJanRec.Body.String(), `"Observation/obs-w14-jan"`) || strings.Contains(encEvJanRec.Body.String(), `"Observation/obs-w14-mar"`) {
		t.Errorf("expected January Encounter/$everything window to include obs-w14-jan and exclude obs-w14-mar, got %d: %s", encEvJanRec.Code, encEvJanRec.Body.String())
	}

	badEvDateReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w14-1/$everything?start=2026-04-01&end=2026-03-01", nil)
	badEvDateRec := httptest.NewRecorder()
	pgH.ServeHTTP(badEvDateRec, badEvDateReq)
	if badEvDateRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request when $everything end < start, got %d", badEvDateRec.Code)
	}

	// 5. Verify PlanDefinition/$apply and ActivityDefinition/$apply (built-in and custom stored definitions)
	planApplyReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/PlanDefinition/plandef-hypertension-control/$apply?subject=Patient/pat-w14-1&encounter=Encounter/enc-w14-1", nil)
	planApplyRec := httptest.NewRecorder()
	pgH.ServeHTTP(planApplyRec, planApplyReq)
	if planApplyRec.Code != http.StatusOK {
		t.Fatalf("GET /PlanDefinition/plandef-hypertension-control/$apply got %d: %s", planApplyRec.Code, planApplyRec.Body.String())
	}
	planApplyBody := planApplyRec.Body.String()
	for _, want := range []string{`"resourceType":"CarePlan"`, `"RequestGroup"`, `"ServiceRequest"`, `"MedicationRequest"`, `"Patient/pat-w14-1"`, `"314076"`} {
		if !strings.Contains(planApplyBody, want) {
			t.Errorf("expected PlanDefinition/$apply CarePlan to contain %s, got %s", want, planApplyBody)
		}
	}

	// POST /PlanDefinition/$apply with return=bundle
	postPlanParams := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "planDefinition", "valueCanonical": "http://flint.fhir.org/PlanDefinition/plandef-hypertension-control"},
			{"name": "subject", "valueString": "Patient/pat-w14-1"},
			{"name": "return", "valueCode": "bundle"}
		]
	}`
	postPlanReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/PlanDefinition/$apply", strings.NewReader(postPlanParams))
	postPlanReq.Header.Set("Content-Type", "application/fhir+json")
	postPlanRec := httptest.NewRecorder()
	pgH.ServeHTTP(postPlanRec, postPlanReq)
	if postPlanRec.Code != http.StatusOK || !strings.Contains(postPlanRec.Body.String(), `"resourceType":"Bundle"`) || !strings.Contains(postPlanRec.Body.String(), `"CarePlan"`) {
		t.Fatalf("POST /PlanDefinition/$apply (return=bundle) expected 200 OK Bundle with CarePlan, got %d: %s", postPlanRec.Code, postPlanRec.Body.String())
	}

	// ActivityDefinition/$apply
	actApplyReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/ActivityDefinition/activitydef-hba1c-lab-order/$apply?subject=Patient/pat-w14-1&encounter=Encounter/enc-w14-1", nil)
	actApplyRec := httptest.NewRecorder()
	pgH.ServeHTTP(actApplyRec, actApplyReq)
	if actApplyRec.Code != http.StatusOK || !strings.Contains(actApplyRec.Body.String(), `"resourceType":"ServiceRequest"`) || !strings.Contains(actApplyRec.Body.String(), `"4548-4"`) {
		t.Fatalf("GET /ActivityDefinition/activitydef-hba1c-lab-order/$apply expected ServiceRequest with LOINC 4548-4, got %d: %s", actApplyRec.Code, actApplyRec.Body.String())
	}
}

func TestDataRequirementsFHIRPathClosureAndSubscriptionEventsWave15(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave15-test"
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
			t.Fatalf("PUT %s/%s expected 200/201, got %d: %s", resType, id, rec.Code, rec.Body.String())
		}
	}

	// 1. Terminology $closure maintenance (init -> add parent & child -> version=0 full resynchronization)
	closureInit := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "name", "valueString": "htn-closure-table"}
		]
	}`
	clReq1 := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/$closure", strings.NewReader(closureInit))
	clReq1.Header.Set("Content-Type", "application/fhir+json")
	clRec1 := httptest.NewRecorder()
	pgH.ServeHTTP(clRec1, clReq1)
	if clRec1.Code != http.StatusOK || !strings.Contains(clRec1.Body.String(), `"version":"1"`) {
		t.Fatalf("POST /$closure init expected 200 OK with version 1, got %d: %s", clRec1.Code, clRec1.Body.String())
	}

	closureAdd := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "name", "valueString": "htn-closure-table"},
			{"name": "concept", "valueCoding": {"system": "http://snomed.info/sct", "code": "38341003"}},
			{"name": "concept", "valueCoding": {"system": "http://snomed.info/sct", "code": "59621000"}}
		]
	}`
	clReq2 := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/$closure", strings.NewReader(closureAdd))
	clReq2.Header.Set("Content-Type", "application/fhir+json")
	clRec2 := httptest.NewRecorder()
	pgH.ServeHTTP(clRec2, clReq2)
	if clRec2.Code != http.StatusOK || !strings.Contains(clRec2.Body.String(), `"subsumes"`) || !strings.Contains(clRec2.Body.String(), `"59621000"`) {
		t.Fatalf("POST /$closure add concepts expected subsumes relation between 38341003 and 59621000, got %d: %s", clRec2.Code, clRec2.Body.String())
	}

	closureResync := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "name", "valueString": "htn-closure-table"},
			{"name": "version", "valueString": "0"}
		]
	}`
	clReq3 := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/$closure", strings.NewReader(closureResync))
	clReq3.Header.Set("Content-Type", "application/fhir+json")
	clRec3 := httptest.NewRecorder()
	pgH.ServeHTTP(clRec3, clReq3)
	if clRec3.Code != http.StatusOK || !strings.Contains(clRec3.Body.String(), `"subsumes"`) {
		t.Fatalf("POST /$closure version=0 resync expected full ConceptMap with subsumes, got %d: %s", clRec3.Code, clRec3.Body.String())
	}

	// 2. Clinical Reasoning $data-requirements on Measure, PlanDefinition, and Library
	drReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Measure/cms165-bp-control/$data-requirements?periodStart=2026-01-01&periodEnd=2026-12-31", nil)
	drRec := httptest.NewRecorder()
	pgH.ServeHTTP(drRec, drReq)
	if drRec.Code != http.StatusOK {
		t.Fatalf("GET /Measure/cms165-bp-control/$data-requirements got %d: %s", drRec.Code, drRec.Body.String())
	}
	for _, want := range []string{`"resourceType":"Library"`, `"module-definition"`, `"dataRequirement"`, `"85354-9"`, `"38341003"`, `"2026-01-01"`} {
		if !strings.Contains(drRec.Body.String(), want) {
			t.Errorf("expected $data-requirements Library to contain %s, got %s", want, drRec.Body.String())
		}
	}

	pdDrReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/PlanDefinition/plandef-hypertension-control/$data-requirements", nil)
	pdDrRec := httptest.NewRecorder()
	pgH.ServeHTTP(pdDrRec, pdDrReq)
	if pdDrRec.Code != http.StatusOK || !strings.Contains(pdDrRec.Body.String(), `"MedicationRequest"`) {
		t.Fatalf("GET /PlanDefinition/plandef-hypertension-control/$data-requirements expected MedicationRequest dataRequirement, got %d: %s", pdDrRec.Code, pdDrRec.Body.String())
	}

	// 3. Seed Patient and Blood Pressure Observation and evaluate $fhirpath (stored instance + inline POST)
	putRes("Patient", "pat-w15-1", `{
		"resourceType": "Patient",
		"id": "pat-w15-1",
		"active": true,
		"name": [{"use": "official", "family": "Rivera", "given": ["Elena", "Maria"]}],
		"gender": "female",
		"birthDate": "1979-08-22"
	}`)
	putRes("Observation", "obs-w15-bp", `{
		"resourceType": "Observation",
		"id": "obs-w15-bp",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure panel"}]},
		"subject": {"reference": "Patient/pat-w15-1"},
		"effectiveDateTime": "2026-04-10T10:00:00Z",
		"component": [
			{
				"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic blood pressure"}]},
				"valueQuantity": {"value": 146.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
			},
			{
				"code": {"coding": [{"system": "http://loinc.org", "code": "8462-4", "display": "Diastolic blood pressure"}]},
				"valueQuantity": {"value": 92.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
			}
		]
	}`)

	fpReq1 := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w15-1/$fhirpath?expression=Patient.name.where(use='official').given.first()", nil)
	fpRec1 := httptest.NewRecorder()
	pgH.ServeHTTP(fpRec1, fpReq1)
	if fpRec1.Code != http.StatusOK || !strings.Contains(fpRec1.Body.String(), `"valueString":"Elena"`) {
		t.Fatalf("GET /Patient/pat-w15-1/$fhirpath expected valueString:Elena, got %d: %s", fpRec1.Code, fpRec1.Body.String())
	}

	fpReq2 := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation/obs-w15-bp/$fhirpath?expression=Observation.component.where(code.coding.code='8480-6').valueQuantity.value>=140", nil)
	fpRec2 := httptest.NewRecorder()
	pgH.ServeHTTP(fpRec2, fpReq2)
	if fpRec2.Code != http.StatusOK || !strings.Contains(fpRec2.Body.String(), `"valueBoolean":true`) {
		t.Fatalf("GET /Observation/obs-w15-bp/$fhirpath comparison >=140 expected valueBoolean:true, got %d: %s", fpRec2.Code, fpRec2.Body.String())
	}

	// 4. Subscription/{id}/$events notification replay (HL7 Subscriptions R5 Backport)
	putRes("Subscription", "sub-w15-vitals", `{
		"resourceType": "Subscription",
		"id": "sub-w15-vitals",
		"status": "active",
		"reason": "Replay vital-signs observations",
		"criteria": "Observation?category=vital-signs",
		"channel": {
			"type": "rest-hook",
			"endpoint": "https://hooks.hospital.example.org/fhir/notify",
			"payload": "application/fhir+json"
		}
	}`)
	// Trigger two matching vital-signs Observations
	putRes("Observation", "obs-w15-hr-1", `{
		"resourceType": "Observation",
		"id": "obs-w15-hr-1",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]},
		"subject": {"reference": "Patient/pat-w15-1"},
		"effectiveDateTime": "2026-04-10T10:05:00Z",
		"valueQuantity": {"value": 74.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
	}`)
	putRes("Observation", "obs-w15-hr-2", `{
		"resourceType": "Observation",
		"id": "obs-w15-hr-2",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]},
		"subject": {"reference": "Patient/pat-w15-1"},
		"effectiveDateTime": "2026-04-10T10:10:00Z",
		"valueQuantity": {"value": 78.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
	}`)

	evReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Subscription/sub-w15-vitals/$events?eventsSinceNumber=1&eventsUntilNumber=2&content=full-resource", nil)
	evRec := httptest.NewRecorder()
	pgH.ServeHTTP(evRec, evReq)
	if evRec.Code != http.StatusOK {
		t.Fatalf("GET /Subscription/sub-w15-vitals/$events got %d: %s", evRec.Code, evRec.Body.String())
	}
	evBody := evRec.Body.String()
	for _, want := range []string{`"type":"history"`, `"event-notification"`, `"notification-event"`, `"Observation/obs-w15-hr-1"`, `"Observation/obs-w15-hr-2"`} {
		if !strings.Contains(evBody, want) {
			t.Errorf("expected Subscription/$events Bundle to contain %s, got %s", want, evBody)
		}
	}

	// Verify content=empty omits resource entries and only includes the SubscriptionStatus Parameters entry
	evEmptyReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Subscription/sub-w15-vitals/$events?eventsSinceNumber=2&eventsUntilNumber=2&content=empty", nil)
	evEmptyRec := httptest.NewRecorder()
	pgH.ServeHTTP(evEmptyRec, evEmptyReq)
	if evEmptyRec.Code != http.StatusOK || strings.Contains(evEmptyRec.Body.String(), `"Observation/obs-w15-hr-1"`) || !strings.Contains(evEmptyRec.Body.String(), `"events-in-notification"`) {
		t.Errorf("expected Subscription/$events?content=empty to return only SubscriptionStatus Parameters for event 2, got %d: %s", evEmptyRec.Code, evEmptyRec.Body.String())
	}
}

func TestCollectDataSubmitDataGroupAddRemoveAndBinaryNegotiationWave16(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave16-test"
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
			t.Fatalf("PUT %s/%s expected 200/201, got %d: %s", resType, id, rec.Code, rec.Body.String())
		}
	}

	// 1. Seed Patient & Condition, then run Measure/$collect-data and feed its output directly into Measure/$submit-data
	putRes("Patient", "pat-w16-1", `{
		"resourceType": "Patient",
		"id": "pat-w16-1",
		"active": true,
		"name": [{"use": "official", "family": "Vance", "given": ["Marcus"]}],
		"gender": "male",
		"birthDate": "1974-11-03"
	}`)
	putRes("Condition", "cond-w16-htn", `{
		"resourceType": "Condition",
		"id": "cond-w16-htn",
		"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
		"verificationStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}]},
		"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}]},
		"subject": {"reference": "Patient/pat-w16-1"}
	}`)

	cdReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Measure/cms165-bp-control/$collect-data?subject=Patient/pat-w16-1&periodStart=2026-01-01&periodEnd=2026-12-31", nil)
	cdRec := httptest.NewRecorder()
	pgH.ServeHTTP(cdRec, cdReq)
	if cdRec.Code != http.StatusOK {
		t.Fatalf("GET /Measure/cms165-bp-control/$collect-data got %d: %s", cdRec.Code, cdRec.Body.String())
	}
	cdBody := cdRec.Body.String()
	for _, want := range []string{`"resourceType":"Parameters"`, `"name":"measureReport"`, `"type":"data-collection"`, `"Patient/pat-w16-1"`, `"Condition/cond-w16-htn"`} {
		if !strings.Contains(cdBody, want) {
			t.Errorf("expected Measure/$collect-data Parameters to contain %s, got %s", want, cdBody)
		}
	}

	// Submit collected Parameters payload to POST /Measure/cms165-bp-control/$submit-data in a receiving tenant
	sdReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Measure/cms165-bp-control/$submit-data", strings.NewReader(cdBody))
	sdReq.Header.Set("Content-Type", "application/fhir+json")
	sdRec := httptest.NewRecorder()
	pgH.ServeHTTP(sdRec, sdReq)
	if sdRec.Code != http.StatusOK || !strings.Contains(sdRec.Body.String(), `"type":"transaction-response"`) || !strings.Contains(sdRec.Body.String(), `"MeasureReport/collect-data-cms165-bp-control-pat-w16-1"`) {
		t.Fatalf("POST /Measure/cms165-bp-control/$submit-data expected transaction-response Bundle with persisted MeasureReport, got %d: %s", sdRec.Code, sdRec.Body.String())
	}

	// 2. Group/{id}/$add and Group/{id}/$remove dynamic cohort operations
	putRes("Patient", "pat-w16-2", `{
		"resourceType": "Patient",
		"id": "pat-w16-2",
		"active": true,
		"name": [{"use": "official", "family": "Chen", "given": ["Sophia"]}],
		"gender": "female",
		"birthDate": "1988-05-19"
	}`)
	putRes("Group", "grp-w16-htn", `{
		"resourceType": "Group",
		"id": "grp-w16-htn",
		"active": true,
		"type": "person",
		"actual": true,
		"name": "Hypertension Cohort",
		"member": [
			{"entity": {"reference": "Patient/pat-w16-1"}}
		]
	}`)

	addBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "member", "valueReference": {"reference": "Patient/pat-w16-2"}}
		]
	}`
	addReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Group/grp-w16-htn/$add", strings.NewReader(addBody))
	addReq.Header.Set("Content-Type", "application/fhir+json")
	addRec := httptest.NewRecorder()
	pgH.ServeHTTP(addRec, addReq)
	if addRec.Code != http.StatusOK || !strings.Contains(addRec.Body.String(), `"Patient/pat-w16-2"`) || !strings.Contains(addRec.Body.String(), `"quantity":2`) {
		t.Fatalf("POST /Group/grp-w16-htn/$add expected both members and quantity:2, got %d: %s", addRec.Code, addRec.Body.String())
	}

	rmBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "member", "valueReference": {"reference": "Patient/pat-w16-1"}}
		]
	}`
	rmReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Group/grp-w16-htn/$remove", strings.NewReader(rmBody))
	rmReq.Header.Set("Content-Type", "application/fhir+json")
	rmRec := httptest.NewRecorder()
	pgH.ServeHTTP(rmRec, rmReq)
	if rmRec.Code != http.StatusOK || strings.Contains(rmRec.Body.String(), `"Patient/pat-w16-1"`) || !strings.Contains(rmRec.Body.String(), `"Patient/pat-w16-2"`) || !strings.Contains(rmRec.Body.String(), `"quantity":1`) {
		t.Fatalf("POST /Group/grp-w16-htn/$remove expected only pat-w16-2 and quantity:1, got %d: %s", rmRec.Code, rmRec.Body.String())
	}

	// 3. Native Binary media-type streaming on GET /Binary/{id}
	// "Hello FHIR Binary!" in base64 is "SGVsbG8gRkhJUiBCaW5hcnkh"
	putRes("Binary", "bin-w16-note", `{
		"resourceType": "Binary",
		"id": "bin-w16-note",
		"contentType": "text/plain",
		"securityContext": {"reference": "DocumentReference/doc-w16-1"},
		"data": "SGVsbG8gRkhJUiBCaW5hcnkh"
	}`)

	// 3a. Request with Accept: text/plain -> streams raw decoded bytes and X-Security-Context header
	binRawReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Binary/bin-w16-note", nil)
	binRawReq.Header.Set("Accept", "text/plain")
	binRawRec := httptest.NewRecorder()
	pgH.ServeHTTP(binRawRec, binRawReq)
	if binRawRec.Code != http.StatusOK || binRawRec.Header().Get("Content-Type") != "text/plain" || binRawRec.Header().Get("X-Security-Context") != "DocumentReference/doc-w16-1" || binRawRec.Body.String() != "Hello FHIR Binary!" {
		t.Fatalf("GET /Binary/bin-w16-note (Accept: text/plain) expected raw 'Hello FHIR Binary!' and X-Security-Context header, got status=%d ct=%q sec=%q body=%q",
			binRawRec.Code, binRawRec.Header().Get("Content-Type"), binRawRec.Header().Get("X-Security-Context"), binRawRec.Body.String())
	}

	// 3b. Request with Accept: application/fhir+json -> returns standard FHIR JSON Binary resource
	binJSONReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Binary/bin-w16-note", nil)
	binJSONReq.Header.Set("Accept", "application/fhir+json")
	binJSONRec := httptest.NewRecorder()
	pgH.ServeHTTP(binJSONRec, binJSONReq)
	if binJSONRec.Code != http.StatusOK || !strings.Contains(binJSONRec.Body.String(), `"resourceType":"Binary"`) || !strings.Contains(binJSONRec.Body.String(), `"SGVsbG8gRkhJUiBCaW5hcnkh"`) {
		t.Fatalf("GET /Binary/bin-w16-note (Accept: application/fhir+json) expected FHIR JSON Binary resource, got %d: %s", binJSONRec.Code, binJSONRec.Body.String())
	}
}

func TestObservationLastNStatsAndPatientMergeWave17(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave17-test"
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
			t.Fatalf("PUT %s/%s expected 200/201, got %d: %s", resType, id, rec.Code, rec.Body.String())
		}
	}

	// 1. Seed Patient + longitudinal vital-signs Observations across multiple codes & timestamps
	putRes("Patient", "pat-w17-target", `{
		"resourceType": "Patient",
		"id": "pat-w17-target",
		"active": true,
		"name": [{"use": "official", "family": "Sterling", "given": ["Elena"]}],
		"gender": "female",
		"birthDate": "1980-03-22"
	}`)
	putRes("Observation", "obs-hr-old", `{
		"resourceType": "Observation",
		"id": "obs-hr-old",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]},
		"subject": {"reference": "Patient/pat-w17-target"},
		"effectiveDateTime": "2026-01-10T08:00:00Z",
		"valueQuantity": {"value": 60.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
	}`)
	putRes("Observation", "obs-hr-mid", `{
		"resourceType": "Observation",
		"id": "obs-hr-mid",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]},
		"subject": {"reference": "Patient/pat-w17-target"},
		"effectiveDateTime": "2026-02-10T08:00:00Z",
		"valueQuantity": {"value": 70.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
	}`)
	putRes("Observation", "obs-hr-latest", `{
		"resourceType": "Observation",
		"id": "obs-hr-latest",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]},
		"subject": {"reference": "Patient/pat-w17-target"},
		"effectiveDateTime": "2026-03-10T08:00:00Z",
		"valueQuantity": {"value": 80.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
	}`)
	putRes("Observation", "obs-wt-latest", `{
		"resourceType": "Observation",
		"id": "obs-wt-latest",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "29463-7", "display": "Body weight"}]},
		"subject": {"reference": "Patient/pat-w17-target"},
		"effectiveDateTime": "2026-03-11T09:00:00Z",
		"valueQuantity": {"value": 64.5, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"}
	}`)

	// 1a. Verify GET /Observation/$lastn?patient=Patient/pat-w17-target&category=vital-signs&max=1 returns 1 latest per distinct code (obs-hr-latest and obs-wt-latest, not obs-hr-old/mid)
	lnReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation/$lastn?patient=Patient/pat-w17-target&category=vital-signs&max=1", nil)
	lnRec := httptest.NewRecorder()
	pgH.ServeHTTP(lnRec, lnReq)
	if lnRec.Code != http.StatusOK {
		t.Fatalf("GET /Observation/$lastn got %d: %s", lnRec.Code, lnRec.Body.String())
	}
	lnBody := lnRec.Body.String()
	if !strings.Contains(lnBody, `"obs-hr-latest"`) || !strings.Contains(lnBody, `"obs-wt-latest"`) || strings.Contains(lnBody, `"obs-hr-old"`) || strings.Contains(lnBody, `"obs-hr-mid"`) {
		t.Fatalf("expected Observation/$lastn max=1 to contain obs-hr-latest and obs-wt-latest only, got %s", lnBody)
	}

	// 2. Verify GET /Observation/$stats?subject=Patient/pat-w17-target&code=8867-4&statistic=average&statistic=min&statistic=max&statistic=median&statistic=count&statistic=sum
	stReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation/$stats?subject=Patient/pat-w17-target&code=8867-4&statistic=average&statistic=min&statistic=max&statistic=median&statistic=count&statistic=sum", nil)
	stRec := httptest.NewRecorder()
	pgH.ServeHTTP(stRec, stReq)
	if stRec.Code != http.StatusOK {
		t.Fatalf("GET /Observation/$stats got %d: %s", stRec.Code, stRec.Body.String())
	}
	stBody := stRec.Body.String()
	for _, want := range []string{`"resourceType":"Parameters"`, `"name":"statistic"`, `"code":"average"`, `"value":70`, `"code":"min"`, `"value":60`, `"code":"max"`, `"value":80`, `"code":"median"`, `"code":"sum"`, `"value":210`} {
		if !strings.Contains(stBody, want) {
			t.Errorf("expected Observation/$stats response to contain %s, got %s", want, stBody)
		}
	}

	// 3. Verify POST /Patient/$merge re-links duplicate Patient compartment resources and updates survivorship links
	putRes("Patient", "pat-w17-dup", `{
		"resourceType": "Patient",
		"id": "pat-w17-dup",
		"active": true,
		"name": [{"use": "official", "family": "Sterling", "given": ["Elena"]}],
		"gender": "female",
		"birthDate": "1980-03-22"
	}`)
	putRes("Condition", "cond-w17-dup", `{
		"resourceType": "Condition",
		"id": "cond-w17-dup",
		"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
		"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}]},
		"subject": {"reference": "Patient/pat-w17-dup"}
	}`)

	mergePayload := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "source-patient", "valueReference": {"reference": "Patient/pat-w17-dup"}},
			{"name": "target-patient", "valueReference": {"reference": "Patient/pat-w17-target"}}
		]
	}`
	mReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$merge", strings.NewReader(mergePayload))
	mReq.Header.Set("Content-Type", "application/fhir+json")
	mRec := httptest.NewRecorder()
	pgH.ServeHTTP(mRec, mReq)
	if mRec.Code != http.StatusOK {
		t.Fatalf("POST /Patient/$merge got %d: %s", mRec.Code, mRec.Body.String())
	}
	mBody := mRec.Body.String()
	for _, want := range []string{`"resourceType":"Parameters"`, `"name":"outcome"`, `"name":"result"`, `"name":"task"`, `"replaces"`, `"Patient/pat-w17-dup"`} {
		if !strings.Contains(mBody, want) {
			t.Errorf("expected Patient/$merge response to contain %s, got %s", want, mBody)
		}
	}

	// Verify source patient is now active:false with replaced-by link to Patient/pat-w17-target
	srcGetReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w17-dup", nil)
	srcGetRec := httptest.NewRecorder()
	pgH.ServeHTTP(srcGetRec, srcGetReq)
	if srcGetRec.Code != http.StatusOK || !strings.Contains(srcGetRec.Body.String(), `"active":false`) || !strings.Contains(srcGetRec.Body.String(), `"replaced-by"`) || !strings.Contains(srcGetRec.Body.String(), `"Patient/pat-w17-target"`) {
		t.Fatalf("expected source Patient/pat-w17-dup to be inactive with replaced-by link to Patient/pat-w17-target, got %d: %s", srcGetRec.Code, srcGetRec.Body.String())
	}

	// Verify Condition/cond-w17-dup was re-linked to Patient/pat-w17-target and is searchable under patient=Patient/pat-w17-target
	condGetReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Condition?patient=Patient/pat-w17-target", nil)
	condGetRec := httptest.NewRecorder()
	pgH.ServeHTTP(condGetRec, condGetReq)
	if condGetRec.Code != http.StatusOK || !strings.Contains(condGetRec.Body.String(), `"cond-w17-dup"`) {
		t.Fatalf("expected Condition?patient=Patient/pat-w17-target to return re-linked cond-w17-dup, got %d: %s", condGetRec.Code, condGetRec.Body.String())
	}
}

func TestAppointmentSchedulingCapabilityImplementsAndSnapshotWave18(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave18-test"
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
			t.Fatalf("PUT %s/%s expected 200/201, got %d: %s", resType, id, rec.Code, rec.Body.String())
		}
	}

	// 1. Seed Practitioner, Patient, Schedule, and free Slot, then run Appointment/$find -> $hold -> $book
	putRes("Practitioner", "prac-w18-1", `{
		"resourceType": "Practitioner",
		"id": "prac-w18-1",
		"active": true,
		"name": [{"family": "Cardio", "given": ["Clara"]}]
	}`)
	putRes("Patient", "pat-w18-1", `{
		"resourceType": "Patient",
		"id": "pat-w18-1",
		"active": true,
		"name": [{"family": "Walker", "given": ["Liam"]}],
		"gender": "male",
		"birthDate": "1979-08-14"
	}`)
	putRes("Schedule", "sched-w18-1", `{
		"resourceType": "Schedule",
		"id": "sched-w18-1",
		"active": true,
		"specialty": [{"coding": [{"system": "http://snomed.info/sct", "code": "394579002", "display": "Cardiology"}]}],
		"actor": [{"reference": "Practitioner/prac-w18-1"}]
	}`)
	putRes("Slot", "slot-w18-1", `{
		"resourceType": "Slot",
		"id": "slot-w18-1",
		"schedule": {"reference": "Schedule/sched-w18-1"},
		"status": "free",
		"start": "2026-05-01T09:00:00Z",
		"end": "2026-05-01T09:30:00Z"
	}`)

	// 1a. Appointment/$find
	findReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Appointment/$find?start=2026-05-01T00:00:00Z&end=2026-05-02T00:00:00Z&specialty=394579002&practitioner=Practitioner/prac-w18-1", nil)
	findRec := httptest.NewRecorder()
	pgH.ServeHTTP(findRec, findReq)
	if findRec.Code != http.StatusOK || !strings.Contains(findRec.Body.String(), `"proposed-slot-w18-1"`) || !strings.Contains(findRec.Body.String(), `"Slot/slot-w18-1"`) {
		t.Fatalf("GET /Appointment/$find expected proposed-slot-w18-1 and Slot/slot-w18-1, got %d: %s", findRec.Code, findRec.Body.String())
	}

	// 1b. Appointment/$hold -> transitions Slot/slot-w18-1 to busy-tentative and Appointment to pending
	holdBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "appt-id", "valueString": "proposed-slot-w18-1"},
			{"name": "patient-id", "valueString": "Patient/pat-w18-1"}
		]
	}`
	holdReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Appointment/$hold", strings.NewReader(holdBody))
	holdReq.Header.Set("Content-Type", "application/fhir+json")
	holdRec := httptest.NewRecorder()
	pgH.ServeHTTP(holdRec, holdReq)
	if holdRec.Code != http.StatusOK || !strings.Contains(holdRec.Body.String(), `"status":"pending"`) || !strings.Contains(holdRec.Body.String(), `"busy-tentative"`) {
		t.Fatalf("POST /Appointment/$hold expected pending Appointment and busy-tentative Slot, got %d: %s", holdRec.Code, holdRec.Body.String())
	}

	// 1c. Appointment/$book -> transitions Slot/slot-w18-1 from busy-tentative to busy and Appointment to booked
	bookBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "appt-id", "valueString": "appt-slot-w18-1"},
			{"name": "patient-id", "valueString": "Patient/pat-w18-1"},
			{"name": "comment", "valueString": "Annual cardiology follow-up"}
		]
	}`
	bookReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Appointment/$book", strings.NewReader(bookBody))
	bookReq.Header.Set("Content-Type", "application/fhir+json")
	bookRec := httptest.NewRecorder()
	pgH.ServeHTTP(bookRec, bookReq)
	if bookRec.Code != http.StatusOK || !strings.Contains(bookRec.Body.String(), `"status":"booked"`) || !strings.Contains(bookRec.Body.String(), `"Annual cardiology follow-up"`) {
		t.Fatalf("POST /Appointment/$book expected booked Appointment, got %d: %s", bookRec.Code, bookRec.Body.String())
	}

	// 1d. Re-booking the already-busy Slot/slot-w18-1 must return 422 Unprocessable Entity
	conflictReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Appointment/$book", strings.NewReader(bookBody))
	conflictReq.Header.Set("Content-Type", "application/fhir+json")
	conflictRec := httptest.NewRecorder()
	pgH.ServeHTTP(conflictRec, conflictReq)
	if conflictRec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity when booking already-busy Slot, got %d: %s", conflictRec.Code, conflictRec.Body.String())
	}

	// 2. CapabilityStatement/$subset and CapabilityStatement/$implements
	subReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/CapabilityStatement/$subset?resource=Patient,Appointment", nil)
	subRec := httptest.NewRecorder()
	pgH.ServeHTTP(subRec, subReq)
	if subRec.Code != http.StatusOK || !strings.Contains(subRec.Body.String(), `"SUBSETTED"`) || strings.Contains(subRec.Body.String(), `"type":"Immunization"`) {
		t.Fatalf("GET /CapabilityStatement/$subset?resource=Patient,Appointment expected SUBSETTED CapabilityStatement without Immunization, got %d: %s", subRec.Code, subRec.Body.String())
	}

	implBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{
				"name": "resource",
				"resource": {
					"resourceType": "CapabilityStatement",
					"status": "active",
					"kind": "requirements",
					"fhirVersion": "4.0.1",
					"rest": [
						{
							"mode": "server",
							"resource": [
								{
									"type": "Patient",
									"interaction": [{"code": "read"}, {"code": "search-type"}],
									"searchParam": [{"name": "identifier", "type": "token"}, {"name": "family", "type": "string"}]
								}
							]
						}
					]
				}
			}
		]
	}`
	implReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/CapabilityStatement/$implements", strings.NewReader(implBody))
	implReq.Header.Set("Content-Type", "application/fhir+json")
	implRec := httptest.NewRecorder()
	pgH.ServeHTTP(implRec, implReq)
	if implRec.Code != http.StatusOK || !strings.Contains(implRec.Body.String(), `"valueBoolean":true`) {
		t.Fatalf("POST /CapabilityStatement/$implements expected valueBoolean:true, got %d: %s", implRec.Code, implRec.Body.String())
	}

	// 3. StructureDefinition/$snapshot (both via GET url and POST custom differential)
	snapPostBody := `{
		"resourceType": "StructureDefinition",
		"id": "custom-cardio-obs",
		"url": "http://example.org/fhir/StructureDefinition/custom-cardio-obs",
		"name": "CustomCardioObservation",
		"status": "active",
		"kind": "resource",
		"abstract": false,
		"type": "Observation",
		"baseDefinition": "http://hl7.org/fhir/StructureDefinition/Observation",
		"derivation": "constraint",
		"differential": {
			"element": [
				{
					"id": "Observation.effectiveDateTime",
					"path": "Observation.effectiveDateTime",
					"min": 1,
					"max": "1",
					"mustSupport": true
				}
			]
		}
	}`
	snapReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/StructureDefinition/$snapshot", strings.NewReader(snapPostBody))
	snapReq.Header.Set("Content-Type", "application/fhir+json")
	snapRec := httptest.NewRecorder()
	pgH.ServeHTTP(snapRec, snapReq)
	if snapRec.Code != http.StatusOK || !strings.Contains(snapRec.Body.String(), `"snapshot"`) || !strings.Contains(snapRec.Body.String(), `"Observation.status"`) || !strings.Contains(snapRec.Body.String(), `"Observation.effectiveDateTime"`) {
		t.Fatalf("POST /StructureDefinition/$snapshot expected synthesized snapshot containing Observation.status and differential override Observation.effectiveDateTime, got %d: %s", snapRec.Code, snapRec.Body.String())
	}
}

func TestProcessMessageStructureMapTransformAndPreferredIDWave19(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave19-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	// 1. POST /$process-message with a FHIR message Bundle (MessageHeader + Patient + Observation)
	msgBundle := `{
		"resourceType": "Bundle",
		"id": "msg-bundle-w19-1",
		"type": "message",
		"entry": [
			{
				"fullUrl": "MessageHeader/mh-w19-1",
				"resource": {
					"resourceType": "MessageHeader",
					"id": "mh-w19-1",
					"eventCoding": {
						"system": "http://terminology.hl7.org/CodeSystem/v2-0003",
						"code": "ORU_R01",
						"display": "Unsolicited observational report"
					},
					"source": {
						"name": "Lab Instrument Gateway",
						"endpoint": "https://lab.example.org/fhir"
					},
					"focus": [
						{"reference": "Patient/pat-w19-1"},
						{"reference": "Observation/obs-w19-1"}
					]
				}
			},
			{
				"fullUrl": "Patient/pat-w19-1",
				"resource": {
					"resourceType": "Patient",
					"id": "pat-w19-1",
					"active": true,
					"name": [{"family": "Mercer", "given": ["Diana"]}],
					"gender": "female",
					"birthDate": "1991-12-04"
				}
			},
			{
				"fullUrl": "Observation/obs-w19-1",
				"resource": {
					"resourceType": "Observation",
					"id": "obs-w19-1",
					"status": "final",
					"code": {"coding": [{"system": "http://loinc.org", "code": "4548-4", "display": "Hemoglobin A1c"}]},
					"subject": {"reference": "Patient/pat-w19-1"},
					"valueQuantity": {"value": 5.8, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"}
				}
			}
		]
	}`
	pmReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/$process-message", strings.NewReader(msgBundle))
	pmReq.Header.Set("Content-Type", "application/fhir+json")
	pmRec := httptest.NewRecorder()
	pgH.ServeHTTP(pmRec, pmReq)
	if pmRec.Code != http.StatusOK {
		t.Fatalf("POST /$process-message got %d: %s", pmRec.Code, pmRec.Body.String())
	}
	pmBody := pmRec.Body.String()
	for _, want := range []string{`"type":"message"`, `"ack-mh-w19-1"`, `"identifier":"mh-w19-1"`, `"code":"ok"`, `"Patient/pat-w19-1"`, `"Observation/obs-w19-1"`} {
		if !strings.Contains(pmBody, want) {
			t.Errorf("expected $process-message acknowledgment Bundle to contain %s, got %s", want, pmBody)
		}
	}

	// Verify persisted Observation/obs-w19-1 is readable in the store
	obsGetReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation/obs-w19-1", nil)
	obsGetRec := httptest.NewRecorder()
	pgH.ServeHTTP(obsGetRec, obsGetReq)
	if obsGetRec.Code != http.StatusOK || !strings.Contains(obsGetRec.Body.String(), `"4548-4"`) {
		t.Fatalf("expected persisted Observation/obs-w19-1 after $process-message, got %d: %s", obsGetRec.Code, obsGetRec.Body.String())
	}

	// 2. POST /StructureMap/$transform with inline StructureMap rules mapping Patient -> RelatedPerson
	transformBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{
				"name": "source",
				"resource": {
					"resourceType": "StructureMap",
					"id": "smap-pat-to-rel",
					"url": "http://example.org/fhir/StructureMap/pat-to-rel",
					"name": "PatientToRelatedPerson",
					"status": "active",
					"structure": [
						{"url": "http://hl7.org/fhir/StructureDefinition/Patient", "mode": "source", "alias": "Patient"},
						{"url": "http://hl7.org/fhir/StructureDefinition/RelatedPerson", "mode": "target", "alias": "RelatedPerson"}
					],
					"group": [
						{
							"name": "main",
							"typeMode": "none",
							"input": [
								{"name": "src", "type": "Patient", "mode": "source"},
								{"name": "tgt", "type": "RelatedPerson", "mode": "target"}
							],
							"rule": [
								{
									"name": "copyName",
									"source": [{"context": "src", "element": "name"}],
									"target": [{"context": "tgt", "element": "name", "transform": "copy"}]
								},
								{
									"name": "copyGender",
									"source": [{"context": "src", "element": "gender"}],
									"target": [{"context": "tgt", "element": "gender", "transform": "copy"}]
								},
								{
									"name": "copyBirthDate",
									"source": [{"context": "src", "element": "birthDate"}],
									"target": [{"context": "tgt", "element": "birthDate", "transform": "copy"}]
								}
							]
						}
					]
				}
			},
			{
				"name": "content",
				"resource": {
					"resourceType": "Patient",
					"id": "pat-w19-1",
					"name": [{"family": "Mercer", "given": ["Diana"]}],
					"gender": "female",
					"birthDate": "1991-12-04"
				}
			}
		]
	}`
	trReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/StructureMap/$transform", strings.NewReader(transformBody))
	trReq.Header.Set("Content-Type", "application/fhir+json")
	trRec := httptest.NewRecorder()
	pgH.ServeHTTP(trRec, trReq)
	if trRec.Code != http.StatusOK || !strings.Contains(trRec.Body.String(), `"resourceType":"RelatedPerson"`) || !strings.Contains(trRec.Body.String(), `"Mercer"`) || !strings.Contains(trRec.Body.String(), `"1991-12-04"`) {
		t.Fatalf("POST /StructureMap/$transform expected RelatedPerson with Mercer and 1991-12-04, got %d: %s", trRec.Code, trRec.Body.String())
	}

	// 3. GET /NamingSystem/$preferred-id (both OID->URI and URI->OID)
	nsReq1 := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/NamingSystem/$preferred-id?id=2.16.840.1.113883.6.96&type=uri", nil)
	nsRec1 := httptest.NewRecorder()
	pgH.ServeHTTP(nsRec1, nsReq1)
	if nsRec1.Code != http.StatusOK || !strings.Contains(nsRec1.Body.String(), `"http://snomed.info/sct"`) {
		t.Fatalf("GET /NamingSystem/$preferred-id OID->URI expected http://snomed.info/sct, got %d: %s", nsRec1.Code, nsRec1.Body.String())
	}

	nsReq2 := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/NamingSystem/$preferred-id?id=http://loinc.org&type=oid", nil)
	nsRec2 := httptest.NewRecorder()
	pgH.ServeHTTP(nsRec2, nsReq2)
	if nsRec2.Code != http.StatusOK || !strings.Contains(nsRec2.Body.String(), `"2.16.840.1.113883.6.1"`) {
		t.Fatalf("GET /NamingSystem/$preferred-id URI->OID expected 2.16.840.1.113883.6.1, got %d: %s", nsRec2.Code, nsRec2.Body.String())
	}
}

func TestClaimInquireQuestionnairePackageAndFindMatchesWave20(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave20-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	// 1. Submit a PAS Claim via POST /Claim/$submit, then query via GET /Claim/{id}/$inquire and POST /Claim/$inquire
	claimJSON := `{
		"resourceType": "Claim",
		"id": "claim-w20-1",
		"status": "active",
		"type": {
			"coding": [{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "professional"}]
		},
		"use": "preauthorization",
		"patient": {"reference": "Patient/pat-w20-1"},
		"created": "2026-10-27",
		"provider": {"reference": "Practitioner/prac-w20-1"},
		"priority": {"coding": [{"code": "normal"}]},
		"insurance": [{"sequence": 1, "focal": true, "coverage": {"reference": "Coverage/cov-w20-1"}}]
	}`
	subReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Claim/$submit", strings.NewReader(claimJSON))
	subReq.Header.Set("Content-Type", "application/fhir+json")
	subRec := httptest.NewRecorder()
	pgH.ServeHTTP(subRec, subReq)
	if subRec.Code != http.StatusOK && subRec.Code != http.StatusCreated {
		t.Fatalf("POST /Claim/$submit expected 200/201, got %d: %s", subRec.Code, subRec.Body.String())
	}

	inqReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Claim/claim-w20-1/$inquire", nil)
	inqRec := httptest.NewRecorder()
	pgH.ServeHTTP(inqRec, inqReq)
	if inqRec.Code != http.StatusOK || !strings.Contains(inqRec.Body.String(), `"ClaimResponse"`) || !strings.Contains(inqRec.Body.String(), `"complete"`) {
		t.Fatalf("GET /Claim/claim-w20-1/$inquire expected 200 OK with adjudicated ClaimResponse, got %d: %s", inqRec.Code, inqRec.Body.String())
	}

	inqPostBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "patient", "valueString": "Patient/pat-w20-1"}
		]
	}`
	inqPostReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Claim/$inquire", strings.NewReader(inqPostBody))
	inqPostReq.Header.Set("Content-Type", "application/fhir+json")
	inqPostRec := httptest.NewRecorder()
	pgH.ServeHTTP(inqPostRec, inqPostReq)
	if inqPostRec.Code != http.StatusOK || !strings.Contains(inqPostRec.Body.String(), `"ClaimResponse"`) {
		t.Fatalf("POST /Claim/$inquire expected 200 OK with ClaimResponse, got %d: %s", inqPostRec.Code, inqPostRec.Body.String())
	}

	// 2. Create a DTR Questionnaire referencing a CQF Library and an answerValueSet, then invoke GET /Questionnaire/{id}/$package
	qJSON := `{
		"resourceType": "Questionnaire",
		"id": "q-dtr-w20",
		"url": "http://flint.fhir.org/Questionnaire/q-dtr-w20",
		"status": "active",
		"title": "DTR Home Oxygen Therapy Prior Authorization",
		"extension": [
			{
				"url": "http://hl7.org/fhir/StructureDefinition/cqf-library",
				"valueCanonical": "http://flint.fhir.org/Library/HomeOxygenRuleLib"
			}
		],
		"item": [
			{
				"linkId": "1",
				"text": "Primary Diagnosis",
				"type": "choice",
				"answerValueSet": "http://hl7.org/fhir/ValueSet/condition-clinical"
			}
		]
	}`
	putQReq := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/Questionnaire/q-dtr-w20", strings.NewReader(qJSON))
	putQReq.Header.Set("Content-Type", "application/fhir+json")
	putQRec := httptest.NewRecorder()
	pgH.ServeHTTP(putQRec, putQReq)
	if putQRec.Code != http.StatusOK && putQRec.Code != http.StatusCreated {
		t.Fatalf("PUT /Questionnaire/q-dtr-w20 expected 200/201, got %d: %s", putQRec.Code, putQRec.Body.String())
	}

	pkgReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Questionnaire/q-dtr-w20/$package", nil)
	pkgRec := httptest.NewRecorder()
	pgH.ServeHTTP(pkgRec, pkgReq)
	if pkgRec.Code != http.StatusOK ||
		!strings.Contains(pkgRec.Body.String(), `"Questionnaire/q-dtr-w20"`) ||
		!strings.Contains(pkgRec.Body.String(), `"HomeOxygenRuleLib"`) ||
		!strings.Contains(pkgRec.Body.String(), `"http://hl7.org/fhir/ValueSet/condition-clinical"`) {
		t.Fatalf("GET /Questionnaire/q-dtr-w20/$package expected Bundle with Questionnaire, Library, and ValueSet, got %d: %s", pkgRec.Code, pkgRec.Body.String())
	}

	// 3. POST /CodeSystem/$find-matches with structured property filters
	fmBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "system", "valueUri": "http://loinc.org"},
			{
				"name": "property",
				"part": [
					{"name": "code", "valueCode": "parent"},
					{"name": "value", "valueCode": "85354-9"}
				]
			}
		]
	}`
	fmReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/CodeSystem/$find-matches", strings.NewReader(fmBody))
	fmReq.Header.Set("Content-Type", "application/fhir+json")
	fmRec := httptest.NewRecorder()
	pgH.ServeHTTP(fmRec, fmReq)
	if fmRec.Code != http.StatusOK || !strings.Contains(fmRec.Body.String(), `"8480-6"`) || !strings.Contains(fmRec.Body.String(), `"8462-4"`) {
		t.Fatalf("POST /CodeSystem/$find-matches expected LOINC children 8480-6 and 8462-4 of 85354-9, got %d: %s", fmRec.Code, fmRec.Body.String())
	}
}

func TestResourceConvertListFindAndChargeItemApplyWave21(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave21-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	// 1. POST /$convert (both canonical FHIR R4 JSON and google.fhir.r4.core ProtoJSON)
	convInput := `{
		"resourceType": "Patient",
		"id": "pat-w21-1",
		"active": false,
		"name": [{"family": "Vance", "given": ["Eli"]}],
		"gender": "male",
		"birthDate": "1955-04-12"
	}`
	convReq1 := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/$convert", strings.NewReader(convInput))
	convReq1.Header.Set("Content-Type", "application/fhir+json")
	convRec1 := httptest.NewRecorder()
	pgH.ServeHTTP(convRec1, convReq1)
	if convRec1.Code != http.StatusOK || !strings.Contains(convRec1.Body.String(), `"Vance"`) || !strings.Contains(convRec1.Body.String(), `"active":false`) {
		t.Fatalf("POST /$convert expected canonical Patient JSON with active:false, got %d: %s", convRec1.Code, convRec1.Body.String())
	}

	convReq2 := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$convert?format=proto+json", strings.NewReader(convInput))
	convReq2.Header.Set("Content-Type", "application/fhir+json")
	convRec2 := httptest.NewRecorder()
	pgH.ServeHTTP(convRec2, convReq2)
	if convRec2.Code != http.StatusOK || !strings.Contains(convRec2.Header().Get("Content-Type"), "application/fhir+proto+json") {
		t.Fatalf("POST /Patient/$convert?format=proto+json expected application/fhir+proto+json, got %d (%s): %s", convRec2.Code, convRec2.Header().Get("Content-Type"), convRec2.Body.String())
	}

	// 2. Seed a Condition for Patient/pat-w21-1 and invoke GET /List/$find?patient=pat-w21-1&name=current-problems
	condJSON := `{
		"resourceType": "Condition",
		"id": "cond-w21-1",
		"clinicalStatus": {
			"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]
		},
		"code": {
			"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}]
		},
		"subject": {"reference": "Patient/pat-w21-1"}
	}`
	putCondReq := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/Condition/cond-w21-1", strings.NewReader(condJSON))
	putCondReq.Header.Set("Content-Type", "application/fhir+json")
	putCondRec := httptest.NewRecorder()
	pgH.ServeHTTP(putCondRec, putCondReq)
	if putCondRec.Code != http.StatusOK && putCondRec.Code != http.StatusCreated {
		t.Fatalf("PUT /Condition/cond-w21-1 expected 200/201, got %d: %s", putCondRec.Code, putCondRec.Body.String())
	}

	lfReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/List/$find?patient=pat-w21-1&name=current-problems", nil)
	lfRec := httptest.NewRecorder()
	pgH.ServeHTTP(lfRec, lfReq)
	if lfRec.Code != http.StatusOK || !strings.Contains(lfRec.Body.String(), `"Condition/cond-w21-1"`) || !strings.Contains(lfRec.Body.String(), `"11450-4"`) {
		t.Fatalf("GET /List/$find expected synthesized Current Problems List with Condition/cond-w21-1, got %d: %s", lfRec.Code, lfRec.Body.String())
	}

	// 3. POST /ChargeItem/$apply with ChargeItemDefinition pricing rules (base 200.00 - 10% discount + 5% tax = 189.00)
	ciApplyBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{
				"name": "chargeItem",
				"resource": {
					"resourceType": "ChargeItem",
					"id": "ci-w21-1",
					"status": "planned",
					"code": {"coding": [{"system": "http://www.ama-assn.org/go/cpt", "code": "99214", "display": "Office visit"}]},
					"subject": {"reference": "Patient/pat-w21-1"},
					"quantity": {"value": 1}
				}
			},
			{
				"name": "chargeItemDefinition",
				"resource": {
					"resourceType": "ChargeItemDefinition",
					"id": "cidef-w21-1",
					"url": "http://flint.fhir.org/ChargeItemDefinition/cidef-w21-1",
					"status": "active",
					"propertyGroup": [
						{
							"priceComponent": [
								{"type": "base", "amount": {"value": 200.0, "currency": "USD"}},
								{"type": "discount", "factor": 0.10},
								{"type": "tax", "factor": 0.05}
							]
						}
					]
				}
			}
		]
	}`
	ciReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/ChargeItem/$apply", strings.NewReader(ciApplyBody))
	ciReq.Header.Set("Content-Type", "application/fhir+json")
	ciRec := httptest.NewRecorder()
	pgH.ServeHTTP(ciRec, ciReq)
	if ciRec.Code != http.StatusOK || !strings.Contains(ciRec.Body.String(), `"value":189`) || !strings.Contains(ciRec.Body.String(), `"billable"`) {
		t.Fatalf("POST /ChargeItem/$apply expected priceOverride=189 USD and status=billable, got %d: %s", ciRec.Code, ciRec.Body.String())
	}
}

func TestCoverageEligibilityDataReqAndGraphDefWave22(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave22-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	// Seed Patient, Organization, Coverage, and Condition
	patJSON := `{
		"resourceType": "Patient",
		"id": "pat-w22-1",
		"active": true,
		"name": [{"family": "Eligibility", "given": ["Grace"]}],
		"gender": "female",
		"birthDate": "1980-08-20"
	}`
	putPatReq := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/Patient/pat-w22-1", strings.NewReader(patJSON))
	putPatReq.Header.Set("Content-Type", "application/fhir+json")
	putPatRec := httptest.NewRecorder()
	pgH.ServeHTTP(putPatRec, putPatReq)
	if putPatRec.Code != http.StatusOK && putPatRec.Code != http.StatusCreated {
		t.Fatalf("PUT /Patient/pat-w22-1 expected 200/201, got %d: %s", putPatRec.Code, putPatRec.Body.String())
	}

	covJSON := `{
		"resourceType": "Coverage",
		"id": "cov-w22-1",
		"status": "active",
		"beneficiary": {"reference": "Patient/pat-w22-1"},
		"payor": [{"reference": "Organization/org-w22-1"}]
	}`
	putCovReq := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/Coverage/cov-w22-1", strings.NewReader(covJSON))
	putCovReq.Header.Set("Content-Type", "application/fhir+json")
	putCovRec := httptest.NewRecorder()
	pgH.ServeHTTP(putCovRec, putCovReq)
	if putCovRec.Code != http.StatusOK && putCovRec.Code != http.StatusCreated {
		t.Fatalf("PUT /Coverage/cov-w22-1 expected 200/201, got %d: %s", putCovRec.Code, putCovRec.Body.String())
	}

	condJSON := `{
		"resourceType": "Condition",
		"id": "cond-w22-1",
		"clinicalStatus": {
			"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]
		},
		"code": {
			"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}]
		},
		"subject": {"reference": "Patient/pat-w22-1"}
	}`
	putCondReq := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/Condition/cond-w22-1", strings.NewReader(condJSON))
	putCondReq.Header.Set("Content-Type", "application/fhir+json")
	putCondRec := httptest.NewRecorder()
	pgH.ServeHTTP(putCondRec, putCondReq)
	if putCondRec.Code != http.StatusOK && putCondRec.Code != http.StatusCreated {
		t.Fatalf("PUT /Condition/cond-w22-1 expected 200/201, got %d: %s", putCondRec.Code, putCondRec.Body.String())
	}

	// 1. POST /CoverageEligibilityRequest/$submit
	cerBody := `{
		"resourceType": "CoverageEligibilityRequest",
		"id": "cer-w22-1",
		"status": "active",
		"purpose": ["benefits", "validation"],
		"patient": {"reference": "Patient/pat-w22-1"},
		"created": "2026-10-27T12:00:00Z",
		"insurer": {"reference": "Organization/org-w22-1"},
		"insurance": [
			{"coverage": {"reference": "Coverage/cov-w22-1"}}
		]
	}`
	cerReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/CoverageEligibilityRequest/$submit", strings.NewReader(cerBody))
	cerReq.Header.Set("Content-Type", "application/fhir+json")
	cerRec := httptest.NewRecorder()
	pgH.ServeHTTP(cerRec, cerReq)
	if cerRec.Code != http.StatusOK ||
		!strings.Contains(cerRec.Body.String(), `"CoverageEligibilityResponse"`) ||
		!strings.Contains(cerRec.Body.String(), `"inforce":true`) ||
		!strings.Contains(cerRec.Body.String(), `"deductible"`) {
		t.Fatalf("POST /CoverageEligibilityRequest/$submit expected adjudicated CoverageEligibilityResponse with inforce:true and deductible, got %d: %s", cerRec.Code, cerRec.Body.String())
	}

	// Also verify instance-level POST /CoverageEligibilityRequest/cer-w22-1/$submit
	cerInstReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/CoverageEligibilityRequest/cer-w22-1/$submit", nil)
	cerInstRec := httptest.NewRecorder()
	pgH.ServeHTTP(cerInstRec, cerInstReq)
	if cerInstRec.Code != http.StatusOK || !strings.Contains(cerInstRec.Body.String(), `"CoverageEligibilityRequest/cer-w22-1"`) {
		t.Fatalf("POST /CoverageEligibilityRequest/cer-w22-1/$submit expected CoverageEligibilityResponse referencing cer-w22-1, got %d: %s", cerInstRec.Code, cerInstRec.Body.String())
	}

	// 2. GET /ActivityDefinition/$data-requirements & GET /Questionnaire/{id}/$data-requirements
	adReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/ActivityDefinition/activitydef-bp-home-monitoring/$data-requirements", nil)
	adRec := httptest.NewRecorder()
	pgH.ServeHTTP(adRec, adReq)
	if adRec.Code != http.StatusOK || !strings.Contains(adRec.Body.String(), `"module-definition"`) || !strings.Contains(adRec.Body.String(), `"ServiceRequest"`) {
		t.Fatalf("GET /ActivityDefinition/activitydef-bp-home-monitoring/$data-requirements expected module-definition Library with ServiceRequest dataRequirement, got %d: %s", adRec.Code, adRec.Body.String())
	}

	qJSON := `{
		"resourceType": "Questionnaire",
		"id": "q-w22-1",
		"url": "http://flint.fhir.org/Questionnaire/q-w22-1",
		"name": "CardiologyIntakeQuestionnaire",
		"status": "active",
		"extension": [
			{
				"url": "http://hl7.org/fhir/StructureDefinition/cqf-library",
				"valueCanonical": "http://flint.fhir.org/Library/CardiologyLogic"
			}
		],
		"item": [
			{
				"linkId": "1",
				"text": "Systolic Blood Pressure",
				"type": "quantity",
				"code": [{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic blood pressure"}]
			},
			{
				"linkId": "2",
				"text": "Smoking Status",
				"type": "choice",
				"answerValueSet": "http://hl7.org/fhir/us/core/ValueSet/us-core-smoking-status-observation-codes"
			}
		]
	}`
	putQReq := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/Questionnaire/q-w22-1", strings.NewReader(qJSON))
	putQReq.Header.Set("Content-Type", "application/fhir+json")
	putQRec := httptest.NewRecorder()
	pgH.ServeHTTP(putQRec, putQReq)
	if putQRec.Code != http.StatusOK && putQRec.Code != http.StatusCreated {
		t.Fatalf("PUT /Questionnaire/q-w22-1 expected 200/201, got %d: %s", putQRec.Code, putQRec.Body.String())
	}

	qDrReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Questionnaire/q-w22-1/$data-requirements", nil)
	qDrRec := httptest.NewRecorder()
	pgH.ServeHTTP(qDrRec, qDrReq)
	if qDrRec.Code != http.StatusOK ||
		!strings.Contains(qDrRec.Body.String(), "CardiologyLogic") ||
		!strings.Contains(qDrRec.Body.String(), "us-core-smoking-status-observation-codes") ||
		!strings.Contains(qDrRec.Body.String(), `"8480-6"`) {
		t.Fatalf("GET /Questionnaire/q-w22-1/$data-requirements expected Library with CardiologyLogic, smoking status ValueSet, and 8480-6 Observation dataRequirement, got %d: %s", qDrRec.Code, qDrRec.Body.String())
	}

	// 3. GET /GraphDefinition/{id}/$graph?start=Patient/pat-w22-1 and POST /GraphDefinition/$graph
	gdReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/GraphDefinition/us-core-patient-care-graph/$graph?start=Patient/pat-w22-1", nil)
	gdRec := httptest.NewRecorder()
	pgH.ServeHTTP(gdRec, gdReq)
	if gdRec.Code != http.StatusOK ||
		!strings.Contains(gdRec.Body.String(), `"Patient/pat-w22-1"`) ||
		!strings.Contains(gdRec.Body.String(), `"Condition/cond-w22-1"`) {
		t.Fatalf("GET /GraphDefinition/us-core-patient-care-graph/$graph?start=Patient/pat-w22-1 expected Bundle with Patient/pat-w22-1 and Condition/cond-w22-1, got %d: %s", gdRec.Code, gdRec.Body.String())
	}

	gdPostBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "graph", "valueUri": "http://hl7.org/fhir/us/core/graphdefinition/us-core-patient-care-graph"},
			{"name": "start", "valueString": "Patient/pat-w22-1"}
		]
	}`
	gdPostReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/GraphDefinition/$graph", strings.NewReader(gdPostBody))
	gdPostReq.Header.Set("Content-Type", "application/fhir+json")
	gdPostRec := httptest.NewRecorder()
	pgH.ServeHTTP(gdPostRec, gdPostReq)
	if gdPostRec.Code != http.StatusOK || !strings.Contains(gdPostRec.Body.String(), `"Condition/cond-w22-1"`) {
		t.Fatalf("POST /GraphDefinition/$graph expected Bundle with Condition/cond-w22-1, got %d: %s", gdPostRec.Code, gdPostRec.Body.String())
	}
}

func TestCDexSubmitAttachmentLibraryEvaluateAndEncounterEverythingWave23(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave23-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	// Seed Patient, Encounter, Condition, Blood Pressure Observation, and CDex Task
	patJSON := `{
		"resourceType": "Patient",
		"id": "pat-w23-1",
		"active": true,
		"name": [{"family": "Hopper", "given": ["Grace"]}],
		"gender": "female",
		"birthDate": "1975-12-09"
	}`
	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w23-1", patJSON},
		{"/Encounter/enc-w23-1", `{
			"resourceType": "Encounter",
			"id": "enc-w23-1",
			"status": "finished",
			"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
			"subject": {"reference": "Patient/pat-w23-1"}
		}`},
		{"/Condition/cond-w23-1", `{
			"resourceType": "Condition",
			"id": "cond-w23-1",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}]},
			"subject": {"reference": "Patient/pat-w23-1"},
			"encounter": {"reference": "Encounter/enc-w23-1"}
		}`},
		{"/Observation/obs-w23-1", `{
			"resourceType": "Observation",
			"id": "obs-w23-1",
			"status": "final",
			"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
			"subject": {"reference": "Patient/pat-w23-1"},
			"encounter": {"reference": "Encounter/enc-w23-1"},
			"component": [
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
					"valueQuantity": {"value": 124.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				},
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8462-4"}]},
					"valueQuantity": {"value": 78.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				}
			]
		}`},
		{"/Task/task-cdex-w23", `{
			"resourceType": "Task",
			"id": "task-cdex-w23",
			"status": "in-progress",
			"intent": "order",
			"identifier": [{"system": "http://payer.example.org/cdex-tracking", "value": "TRACK-W23-001"}],
			"for": {"reference": "Patient/pat-w23-1"}
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. POST /$submit-attachment (CDex attachment submission + Task completion)
	cdexBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "TrackingId", "valueString": "TRACK-W23-001"},
			{"name": "AttachTo", "valueCode": "claim"},
			{"name": "MemberId", "valueString": "pat-w23-1"},
			{
				"name": "Attachment",
				"part": [
					{"name": "LineItem", "valueString": "1"},
					{
						"name": "Code",
						"valueCodeableConcept": {
							"coding": [{"system": "http://loinc.org", "code": "11506-3", "display": "Progress note"}]
						}
					},
					{
						"name": "Content",
						"resource": {
							"resourceType": "DocumentReference",
							"id": "doc-cdex-w23-1",
							"status": "current",
							"content": [
								{
									"attachment": {
										"contentType": "text/plain",
										"url": "https://flint.example.org/cdex/doc-cdex-w23-1.txt"
									}
								}
							]
						}
					}
				]
			}
		]
	}`
	cdexReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/$submit-attachment", strings.NewReader(cdexBody))
	cdexReq.Header.Set("Content-Type", "application/fhir+json")
	cdexRec := httptest.NewRecorder()
	pgH.ServeHTTP(cdexRec, cdexReq)
	if cdexRec.Code != http.StatusOK || !strings.Contains(cdexRec.Body.String(), `"DocumentReference/doc-cdex-w23-1"`) {
		t.Fatalf("POST /$submit-attachment expected OperationOutcome with DocumentReference/doc-cdex-w23-1, got %d: %s", cdexRec.Code, cdexRec.Body.String())
	}

	// Verify Task/task-cdex-w23 transitioned to completed with output reference
	getTaskReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Task/task-cdex-w23", nil)
	getTaskRec := httptest.NewRecorder()
	pgH.ServeHTTP(getTaskRec, getTaskReq)
	if getTaskRec.Code != http.StatusOK ||
		!strings.Contains(getTaskRec.Body.String(), `"status":"completed"`) ||
		!strings.Contains(getTaskRec.Body.String(), `"DocumentReference/doc-cdex-w23-1"`) {
		t.Fatalf("GET /Task/task-cdex-w23 expected status:completed and DocumentReference/doc-cdex-w23-1 output, got %d: %s", getTaskRec.Code, getTaskRec.Body.String())
	}

	// 2. GET /Library/CMS165-Logic/$evaluate?subject=Patient/pat-w23-1
	evalReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Library/CMS165-Logic/$evaluate?subject=Patient/pat-w23-1", nil)
	evalRec := httptest.NewRecorder()
	pgH.ServeHTTP(evalRec, evalReq)
	if evalRec.Code != http.StatusOK ||
		!strings.Contains(evalRec.Body.String(), `"InNumerator"`) ||
		!strings.Contains(evalRec.Body.String(), `"LatestSystolicBP"`) ||
		!strings.Contains(evalRec.Body.String(), `124`) {
		t.Fatalf("GET /Library/CMS165-Logic/$evaluate expected InNumerator and LatestSystolicBP=124, got %d: %s", evalRec.Code, evalRec.Body.String())
	}

	// 3. GET /Encounter/$everything?id=enc-w23-1
	encEvReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Encounter/$everything?id=enc-w23-1", nil)
	encEvRec := httptest.NewRecorder()
	pgH.ServeHTTP(encEvRec, encEvReq)
	if encEvRec.Code != http.StatusOK ||
		!strings.Contains(encEvRec.Body.String(), `"Encounter/enc-w23-1"`) ||
		!strings.Contains(encEvRec.Body.String(), `"Observation/obs-w23-1"`) {
		t.Fatalf("GET /Encounter/$everything?id=enc-w23-1 expected Bundle with Encounter/enc-w23-1 and Observation/obs-w23-1, got %d: %s", encEvRec.Code, encEvRec.Body.String())
	}
}

func TestSHCHealthCardsPCDEAndResearchStudyEligibilityWave24(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave24-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w24-1", `{
			"resourceType": "Patient",
			"id": "pat-w24-1",
			"active": true,
			"gender": "female",
			"birthDate": "1985-04-12",
			"name": [{"family": "Wonderland", "given": ["Alice"]}]
		}`},
		{"/Immunization/imm-w24-1", `{
			"resourceType": "Immunization",
			"id": "imm-w24-1",
			"status": "completed",
			"vaccineCode": {"coding": [{"system": "http://hl7.org/fhir/sid/cvx", "code": "208", "display": "SARS-COV-2 (COVID-19) vaccine, mRNA, spike protein, LNP, preservative free, 30 mcg/0.3mL dose"}]},
			"patient": {"reference": "Patient/pat-w24-1"},
			"occurrenceDateTime": "2026-01-10",
			"lotNumber": "EL9262"
		}`},
		{"/Coverage/cov-w24-1", `{
			"resourceType": "Coverage",
			"id": "cov-w24-1",
			"status": "active",
			"beneficiary": {"reference": "Patient/pat-w24-1"},
			"payor": [{"display": "Acme Health Plan"}]
		}`},
		{"/CarePlan/cp-w24-1", `{
			"resourceType": "CarePlan",
			"id": "cp-w24-1",
			"status": "active",
			"intent": "plan",
			"title": "Hypertension Continuity CarePlan",
			"subject": {"reference": "Patient/pat-w24-1"}
		}`},
		{"/Condition/cond-w24-1", `{
			"resourceType": "Condition",
			"id": "cond-w24-1",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}]},
			"subject": {"reference": "Patient/pat-w24-1"}
		}`},
		{"/Group/grp-trial-w24", `{
			"resourceType": "Group",
			"id": "grp-trial-w24",
			"type": "person",
			"actual": false,
			"characteristic": [
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "46098-0", "display": "Sex"}]},
					"valueCodeableConcept": {"coding": [{"system": "http://hl7.org/fhir/administrative-gender", "code": "female"}]},
					"exclude": false
				},
				{
					"code": {"coding": [{"system": "http://snomed.info/sct", "code": "64572001", "display": "Condition"}]},
					"valueCodeableConcept": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}]},
					"exclude": false
				}
			]
		}`},
		{"/ResearchStudy/study-w24-1", `{
			"resourceType": "ResearchStudy",
			"id": "study-w24-1",
			"title": "Phase III Resistant Hypertension Clinical Trial",
			"status": "active",
			"enrollment": [{"reference": "Group/grp-trial-w24"}]
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. POST /Patient/pat-w24-1/$health-cards-issue
	shcBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "credentialType", "valueUri": "https://smarthealth.cards#immunization"},
			{"name": "credentialType", "valueUri": "https://smarthealth.cards#covid19"}
		]
	}`
	shcReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/pat-w24-1/$health-cards-issue", strings.NewReader(shcBody))
	shcReq.Header.Set("Content-Type", "application/fhir+json")
	shcRec := httptest.NewRecorder()
	pgH.ServeHTTP(shcRec, shcReq)
	if shcRec.Code != http.StatusOK ||
		!strings.Contains(shcRec.Body.String(), `"verifiableCredential"`) ||
		!strings.Contains(shcRec.Body.String(), `"resource:0"`) ||
		!strings.Contains(shcRec.Body.String(), `"EL9262"`) {
		t.Fatalf("POST /Patient/pat-w24-1/$health-cards-issue expected verifiableCredential and SHC Bundle with EL9262, got %d: %s", shcRec.Code, shcRec.Body.String())
	}

	// 2. GET /Patient/pat-w24-1/$pcde
	pcdeReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w24-1/$pcde", nil)
	pcdeRec := httptest.NewRecorder()
	pgH.ServeHTTP(pcdeRec, pcdeReq)
	if pcdeRec.Code != http.StatusOK ||
		!strings.Contains(pcdeRec.Body.String(), `"type":"document"`) ||
		!strings.Contains(pcdeRec.Body.String(), `"11503-0"`) ||
		!strings.Contains(pcdeRec.Body.String(), `"CarePlan/cp-w24-1"`) ||
		!strings.Contains(pcdeRec.Body.String(), `"Coverage/cov-w24-1"`) {
		t.Fatalf("GET /Patient/pat-w24-1/$pcde expected document Bundle with LOINC 11503-0, CarePlan/cp-w24-1, and Coverage/cov-w24-1, got %d: %s", pcdeRec.Code, pcdeRec.Body.String())
	}

	// 3. GET /ResearchStudy/study-w24-1/$evaluate-eligibility?patient=Patient/pat-w24-1
	eligReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/ResearchStudy/study-w24-1/$evaluate-eligibility?patient=Patient/pat-w24-1", nil)
	eligRec := httptest.NewRecorder()
	pgH.ServeHTTP(eligRec, eligReq)
	if eligRec.Code != http.StatusOK ||
		!strings.Contains(eligRec.Body.String(), `"valueBoolean":true`) ||
		!strings.Contains(eligRec.Body.String(), `"ResearchSubject"`) ||
		!strings.Contains(eligRec.Body.String(), `"candidate"`) {
		t.Fatalf("GET /ResearchStudy/study-w24-1/$evaluate-eligibility expected eligible=true and candidate ResearchSubject, got %d: %s", eligRec.Code, eligRec.Body.String())
	}
}

func TestClaimPredetermineMeasureRiskAdjustmentAndScheduleFindWave25(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave25-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w25-1", `{
			"resourceType": "Patient",
			"id": "pat-w25-1",
			"active": true,
			"gender": "female",
			"birthDate": "1972-08-19",
			"name": [{"family": "Lovelace", "given": ["Ada"]}]
		}`},
		{"/Condition/cond-w25-htn", `{
			"resourceType": "Condition",
			"id": "cond-w25-htn",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}]},
			"subject": {"reference": "Patient/pat-w25-1"}
		}`},
		{"/Observation/obs-w25-a1c", `{
			"resourceType": "Observation",
			"id": "obs-w25-a1c",
			"status": "final",
			"code": {"coding": [{"system": "http://loinc.org", "code": "4548-4", "display": "Hemoglobin A1c/Hemoglobin.total in Blood"}]},
			"subject": {"reference": "Patient/pat-w25-1"},
			"valueQuantity": {"value": 7.8, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"}
		}`},
		{"/Schedule/sched-w25-1", `{
			"resourceType": "Schedule",
			"id": "sched-w25-1",
			"active": true,
			"specialty": [{"coding": [{"system": "http://snomed.info/sct", "code": "394579002", "display": "Cardiology"}]}],
			"actor": [{"reference": "Practitioner/prac-w25-1", "display": "Dr. Sarah Cardiology"}],
			"planningHorizon": {"start": "2026-06-01T09:00:00Z", "end": "2026-06-01T17:00:00Z"}
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. POST /Claim/$predetermine
	predetBody := `{
		"resourceType": "Claim",
		"id": "claim-w25-1",
		"status": "active",
		"use": "predetermination",
		"patient": {"reference": "Patient/pat-w25-1"},
		"insurer": {"reference": "Organization/org-w25-1"},
		"provider": {"reference": "Practitioner/prac-w25-1"},
		"item": [
			{
				"sequence": 1,
				"productOrService": {"coding": [{"system": "http://www.ama-assn.org/go/cpt", "code": "93306"}]},
				"net": {"value": 500.0, "currency": "USD"}
			}
		]
	}`
	predetReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Claim/$predetermine", strings.NewReader(predetBody))
	predetReq.Header.Set("Content-Type", "application/fhir+json")
	predetRec := httptest.NewRecorder()
	pgH.ServeHTTP(predetRec, predetReq)
	if predetRec.Code != http.StatusOK ||
		!strings.Contains(predetRec.Body.String(), `"resourceType":"ClaimResponse"`) ||
		!strings.Contains(predetRec.Body.String(), `"use":"predetermination"`) ||
		!strings.Contains(predetRec.Body.String(), `"PREDET-CLAIM-W25-1"`) ||
		!strings.Contains(predetRec.Body.String(), `400`) {
		t.Fatalf("POST /Claim/$predetermine expected predetermination ClaimResponse with 400 benefit and PREDET-CLAIM-W25-1, got %d: %s", predetRec.Code, predetRec.Body.String())
	}

	// 2. GET /Measure/$evaluate-risk-condition-category?subject=Patient/pat-w25-1
	raReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Measure/$evaluate-risk-condition-category?subject=Patient/pat-w25-1", nil)
	raRec := httptest.NewRecorder()
	pgH.ServeHTTP(raRec, raReq)
	if raRec.Code != http.StatusOK ||
		!strings.Contains(raRec.Body.String(), `"CMS-HCC 85 (confirmed)"`) ||
		!strings.Contains(raRec.Body.String(), `"CMS-HCC 19 (suspect)"`) {
		t.Fatalf("GET /Measure/$evaluate-risk-condition-category expected CMS-HCC 85 (confirmed) and CMS-HCC 19 (suspect), got %d: %s", raRec.Code, raRec.Body.String())
	}

	// 3. GET /Schedule/$find?specialty=394579002&actor=Practitioner/prac-w25-1
	schedReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Schedule/$find?specialty=394579002&actor=Practitioner/prac-w25-1", nil)
	schedRec := httptest.NewRecorder()
	pgH.ServeHTTP(schedRec, schedReq)
	if schedRec.Code != http.StatusOK ||
		!strings.Contains(schedRec.Body.String(), `"Schedule/sched-w25-1"`) ||
		!strings.Contains(schedRec.Body.String(), `"Slot/slot-open-sched-w25-1"`) {
		t.Fatalf("GET /Schedule/$find expected Schedule/sched-w25-1 and synthesized open Slot, got %d: %s", schedRec.Code, schedRec.Body.String())
	}
}

func TestOrderCRDAdverseEventEvaluateAndCarePlanEvaluateWave26(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave26-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w26-1", `{
			"resourceType": "Patient",
			"id": "pat-w26-1",
			"active": true,
			"gender": "female",
			"birthDate": "1980-03-14",
			"name": [{"family": "Curie", "given": ["Marie"]}]
		}`},
		{"/Coverage/cov-w26-1", `{
			"resourceType": "Coverage",
			"id": "cov-w26-1",
			"status": "active",
			"beneficiary": {"reference": "Patient/pat-w26-1"},
			"payor": [{"display": "National Health Plan"}]
		}`},
		{"/AllergyIntolerance/allergy-w26-1", `{
			"resourceType": "AllergyIntolerance",
			"id": "allergy-w26-1",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980", "display": "Penicillin G"}]},
			"patient": {"reference": "Patient/pat-w26-1"}
		}`},
		{"/MedicationRequest/medreq-w26-1", `{
			"resourceType": "MedicationRequest",
			"id": "medreq-w26-1",
			"status": "active",
			"intent": "order",
			"subject": {"reference": "Patient/pat-w26-1"},
			"medicationCodeableConcept": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980", "display": "Penicillin G"}]}
		}`},
		{"/AdverseEvent/ae-w26-1", `{
			"resourceType": "AdverseEvent",
			"id": "ae-w26-1",
			"actuality": "actual",
			"subject": {"reference": "Patient/pat-w26-1"},
			"severity": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/adverse-event-severity", "code": "severe"}]},
			"suspectEntity": [
				{"instance": {"reference": "MedicationRequest/medreq-w26-1"}}
			]
		}`},
		{"/Goal/goal-w26-1", `{
			"resourceType": "Goal",
			"id": "goal-w26-1",
			"lifecycleStatus": "active",
			"description": {"text": "Maintain blood pressure below 130/80 mmHg"},
			"subject": {"reference": "Patient/pat-w26-1"}
		}`},
		{"/CarePlan/cp-w26-1", `{
			"resourceType": "CarePlan",
			"id": "cp-w26-1",
			"status": "active",
			"intent": "plan",
			"subject": {"reference": "Patient/pat-w26-1"},
			"goal": [{"reference": "Goal/goal-w26-1"}]
		}`},
		{"/Observation/obs-w26-bp", `{
			"resourceType": "Observation",
			"id": "obs-w26-bp",
			"status": "final",
			"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
			"subject": {"reference": "Patient/pat-w26-1"},
			"component": [
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
					"valueQuantity": {"value": 122.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				},
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8462-4"}]},
					"valueQuantity": {"value": 76.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				}
			]
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. POST /MedicationRequest/medreq-w26-1/$crd
	crdReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/MedicationRequest/medreq-w26-1/$crd", nil)
	crdRec := httptest.NewRecorder()
	pgH.ServeHTTP(crdRec, crdReq)
	if crdRec.Code != http.StatusOK ||
		!strings.Contains(crdRec.Body.String(), "ext-coverage-information") ||
		!strings.Contains(crdRec.Body.String(), "dtr-specialty-rx-prior-auth") ||
		!strings.Contains(crdRec.Body.String(), `"Coverage/cov-w26-1"`) {
		t.Fatalf("POST /MedicationRequest/medreq-w26-1/$crd expected ext-coverage-information and Coverage/cov-w26-1, got %d: %s", crdRec.Code, crdRec.Body.String())
	}

	// 2. GET /AdverseEvent/ae-w26-1/$evaluate
	aeReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/AdverseEvent/ae-w26-1/$evaluate", nil)
	aeRec := httptest.NewRecorder()
	pgH.ServeHTTP(aeRec, aeReq)
	if aeRec.Code != http.StatusOK ||
		!strings.Contains(aeRec.Body.String(), `"valueCode":"certain"`) ||
		!strings.Contains(aeRec.Body.String(), `"DetectedIssue"`) ||
		!strings.Contains(aeRec.Body.String(), `"MedicationRequest/medreq-w26-1"`) {
		t.Fatalf("GET /AdverseEvent/ae-w26-1/$evaluate expected causalityAssessment=certain and DetectedIssue implicating MedicationRequest/medreq-w26-1, got %d: %s", aeRec.Code, aeRec.Body.String())
	}

	// 3. GET /CarePlan/cp-w26-1/$evaluate
	cpReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/CarePlan/cp-w26-1/$evaluate", nil)
	cpRec := httptest.NewRecorder()
	pgH.ServeHTTP(cpRec, cpReq)
	if cpRec.Code != http.StatusOK ||
		!strings.Contains(cpRec.Body.String(), `"valueCode":"achieved"`) ||
		!strings.Contains(cpRec.Body.String(), `"Goal/goal-w26-1"`) ||
		!strings.Contains(cpRec.Body.String(), `"Observation/obs-w26-bp"`) {
		t.Fatalf("GET /CarePlan/cp-w26-1/$evaluate expected achievementStatus=achieved, Goal/goal-w26-1, and Observation/obs-w26-bp, got %d: %s", cpRec.Code, cpRec.Body.String())
	}
}

func TestImmDSForecastPCTGFESubmitAndRiskAssessmentEvaluateWave27(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave27-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w27-1", `{
			"resourceType": "Patient",
			"id": "pat-w27-1",
			"active": true,
			"gender": "female",
			"birthDate": "1968-11-05",
			"name": [{"family": "Pasteur", "given": ["Louise"]}]
		}`},
		{"/Immunization/imm-w27-1", `{
			"resourceType": "Immunization",
			"id": "imm-w27-1",
			"status": "completed",
			"vaccineCode": {"coding": [{"system": "http://hl7.org/fhir/sid/cvx", "code": "208", "display": "SARS-COV-2 (COVID-19) vaccine, mRNA"}]},
			"patient": {"reference": "Patient/pat-w27-1"},
			"occurrenceDateTime": "2026-01-10"
		}`},
		{"/Condition/cond-w27-1", `{
			"resourceType": "Condition",
			"id": "cond-w27-1",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}]},
			"subject": {"reference": "Patient/pat-w27-1"}
		}`},
		{"/Coverage/cov-w27-1", `{
			"resourceType": "Coverage",
			"id": "cov-w27-1",
			"status": "active",
			"beneficiary": {"reference": "Patient/pat-w27-1"},
			"payor": [{"display": "Shield Health Plan"}]
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. GET /Patient/pat-w27-1/$immds-forecast
	immReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w27-1/$immds-forecast", nil)
	immRec := httptest.NewRecorder()
	pgH.ServeHTTP(immRec, immReq)
	if immRec.Code != http.StatusOK ||
		!strings.Contains(immRec.Body.String(), "ImmunizationEvaluation") ||
		!strings.Contains(immRec.Body.String(), "ImmunizationRecommendation") ||
		!strings.Contains(immRec.Body.String(), `"code":"complete"`) ||
		!strings.Contains(immRec.Body.String(), `"code":"due"`) {
		t.Fatalf("GET /Patient/pat-w27-1/$immds-forecast expected ImmunizationEvaluation (complete) and ImmunizationRecommendation (due), got %d: %s", immRec.Code, immRec.Body.String())
	}

	// 2. POST /Claim/$gfe-submit
	gfeBody := `{
		"resourceType": "Claim",
		"id": "gfe-w27-1",
		"status": "active",
		"use": "predetermination",
		"patient": {"reference": "Patient/pat-w27-1"},
		"item": [
			{
				"sequence": 1,
				"productOrService": {"coding": [{"system": "http://www.ama-assn.org/go/cpt", "code": "93306"}]},
				"net": {"value": 600.0, "currency": "USD"}
			}
		]
	}`
	gfeReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Claim/$gfe-submit", strings.NewReader(gfeBody))
	gfeReq.Header.Set("Content-Type", "application/fhir+json")
	gfeRec := httptest.NewRecorder()
	pgH.ServeHTTP(gfeRec, gfeReq)
	if gfeRec.Code != http.StatusOK ||
		!strings.Contains(gfeRec.Body.String(), "ExplanationOfBenefit") ||
		!strings.Contains(gfeRec.Body.String(), "davinci-pct-aeob") ||
		!strings.Contains(gfeRec.Body.String(), `480`) {
		t.Fatalf("POST /Claim/$gfe-submit expected AEOB Bundle with 480 benefit amount, got %d: %s", gfeRec.Code, gfeRec.Body.String())
	}

	// 3. GET /RiskAssessment/$evaluate?subject=Patient/pat-w27-1
	raReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/RiskAssessment/$evaluate?subject=Patient/pat-w27-1", nil)
	raRec := httptest.NewRecorder()
	pgH.ServeHTTP(raRec, raReq)
	if raRec.Code != http.StatusOK ||
		!strings.Contains(raRec.Body.String(), `"resourceType":"RiskAssessment"`) ||
		!strings.Contains(raRec.Body.String(), `"probabilityDecimal":0.14`) ||
		!strings.Contains(raRec.Body.String(), `"Condition/cond-w27-1"`) {
		t.Fatalf("GET /RiskAssessment/$evaluate expected RiskAssessment with probabilityDecimal=0.14 and Condition/cond-w27-1, got %d: %s", raRec.Code, raRec.Body.String())
	}
}

func TestCompositionDocumentATRGroupAndDrugInteractionsWave28(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave28-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w28-1", `{
			"resourceType": "Patient",
			"id": "pat-w28-1",
			"active": true,
			"gender": "female",
			"birthDate": "1979-03-22",
			"name": [{"family": "Fleming", "given": ["Alexander"]}]
		}`},
		{"/Condition/cond-w28-1", `{
			"resourceType": "Condition",
			"id": "cond-w28-1",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://snomed.info/sct", "code": "195967001", "display": "Asthma"}]},
			"subject": {"reference": "Patient/pat-w28-1"}
		}`},
		{"/Composition/comp-w28-1", `{
			"resourceType": "Composition",
			"id": "comp-w28-1",
			"status": "final",
			"type": {"coding": [{"system": "http://loinc.org", "code": "11506-3", "display": "Progress note"}]},
			"subject": {"reference": "Patient/pat-w28-1"},
			"date": "2026-02-01T09:00:00Z",
			"title": "Outpatient Pulmonary Consult",
			"section": [
				{
					"title": "Active Problems",
					"entry": [{"reference": "Condition/cond-w28-1"}]
				}
			]
		}`},
		{"/Group/grp-atr-w28", `{
			"resourceType": "Group",
			"id": "grp-atr-w28",
			"type": "person",
			"actual": true,
			"name": "Da Vinci ATR Value-Based Care Roster"
		}`},
		{"/AllergyIntolerance/allergy-w28-1", `{
			"resourceType": "AllergyIntolerance",
			"id": "allergy-w28-1",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980", "display": "Penicillin G"}]},
			"patient": {"reference": "Patient/pat-w28-1"}
		}`},
		{"/MedicationRequest/medreq-w28-1", `{
			"resourceType": "MedicationRequest",
			"id": "medreq-w28-1",
			"status": "active",
			"intent": "order",
			"medicationCodeableConcept": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "723", "display": "Amoxicillin 500 MG Oral Capsule"}]},
			"subject": {"reference": "Patient/pat-w28-1"}
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. GET /Composition/$document?id=comp-w28-1&persist=true
	docReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Composition/$document?id=comp-w28-1&persist=true", nil)
	docRec := httptest.NewRecorder()
	pgH.ServeHTTP(docRec, docReq)
	if docRec.Code != http.StatusOK ||
		!strings.Contains(docRec.Body.String(), `"type":"document"`) ||
		!strings.Contains(docRec.Body.String(), `"Condition/cond-w28-1"`) ||
		docRec.Header().Get("Location") == "" {
		t.Fatalf("GET /Composition/$document?id=comp-w28-1&persist=true expected persisted document Bundle with Condition/cond-w28-1, got %d: %s", docRec.Code, docRec.Body.String())
	}

	// 2. Da Vinci ATR Group/{id}/$member-add, $confirm-attribution-list, and $member-remove
	addBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "memberId", "valueReference": {"reference": "Patient/pat-w28-1"}},
			{"name": "attributionPeriod", "valuePeriod": {"start": "2026-01-01", "end": "2026-12-31"}}
		]
	}`
	addReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Group/grp-atr-w28/$member-add", strings.NewReader(addBody))
	addReq.Header.Set("Content-Type", "application/fhir+json")
	addRec := httptest.NewRecorder()
	pgH.ServeHTTP(addRec, addReq)
	if addRec.Code != http.StatusOK ||
		!strings.Contains(addRec.Body.String(), "atr-group") ||
		!strings.Contains(addRec.Body.String(), `"Patient/pat-w28-1"`) {
		t.Fatalf("POST /Group/grp-atr-w28/$member-add expected atr-group with Patient/pat-w28-1, got %d: %s", addRec.Code, addRec.Body.String())
	}

	confReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Group/grp-atr-w28/$confirm-attribution-list", nil)
	confRec := httptest.NewRecorder()
	pgH.ServeHTTP(confRec, confReq)
	if confRec.Code != http.StatusOK ||
		!strings.Contains(confRec.Body.String(), `"valueCode":"confirmed"`) ||
		!strings.Contains(confRec.Body.String(), `"valueInteger":1`) {
		t.Fatalf("GET /Group/grp-atr-w28/$confirm-attribution-list expected confirmed Parameters with activeMemberCount=1, got %d: %s", confRec.Code, confRec.Body.String())
	}

	remReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Group/grp-atr-w28/$member-remove", strings.NewReader(addBody))
	remReq.Header.Set("Content-Type", "application/fhir+json")
	remRec := httptest.NewRecorder()
	pgH.ServeHTTP(remRec, remReq)
	if remRec.Code != http.StatusOK || !strings.Contains(remRec.Body.String(), `"inactive":true`) {
		t.Fatalf("POST /Group/grp-atr-w28/$member-remove expected inactive=true, got %d: %s", remRec.Code, remRec.Body.String())
	}

	// 3. Da Vinci US Drug Formulary MedicationKnowledge/$lookup & MedicationRequest/$check-interactions
	mkReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/MedicationKnowledge/$lookup?code=723", nil)
	mkRec := httptest.NewRecorder()
	pgH.ServeHTTP(mkRec, mkReq)
	if mkRec.Code != http.StatusOK ||
		!strings.Contains(mkRec.Body.String(), "usdf-FormularyDrug") ||
		!strings.Contains(mkRec.Body.String(), "Amoxicillin 500 MG Oral Capsule") {
		t.Fatalf("GET /MedicationKnowledge/$lookup?code=723 expected usdf-FormularyDrug MedicationKnowledge, got %d: %s", mkRec.Code, mkRec.Body.String())
	}

	intReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/MedicationRequest/medreq-w28-1/$check-interactions", nil)
	intRec := httptest.NewRecorder()
	pgH.ServeHTTP(intRec, intReq)
	if intRec.Code != http.StatusOK ||
		!strings.Contains(intRec.Body.String(), `"valueCode":"interaction-detected"`) ||
		!strings.Contains(intRec.Body.String(), `"code":"DALG"`) ||
		!strings.Contains(intRec.Body.String(), `"AllergyIntolerance/allergy-w28-1"`) {
		t.Fatalf("GET /MedicationRequest/medreq-w28-1/$check-interactions expected interaction-detected with DALG DetectedIssue, got %d: %s", intRec.Code, intRec.Body.String())
	}
}

func TestDaVinciNotificationsIHEPixAndLabEvaluateWave29(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave29-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w29-1", `{
			"resourceType": "Patient",
			"id": "pat-w29-1",
			"active": true,
			"gender": "male",
			"birthDate": "1964-07-12",
			"identifier": [
				{"system": "http://hospital.example.org/mrn", "value": "MRN-W29-001"},
				{"system": "http://hl7.org/fhir/sid/us-ssn", "value": "999-29-0001"}
			],
			"name": [{"family": "Virchow", "given": ["Rudolf"]}]
		}`},
		{"/ServiceRequest/sr-w29-1", `{
			"resourceType": "ServiceRequest",
			"id": "sr-w29-1",
			"status": "active",
			"intent": "order",
			"code": {"coding": [{"system": "http://loinc.org", "code": "2823-3", "display": "Potassium [Moles/volume] in Serum or Plasma"}]},
			"subject": {"reference": "Patient/pat-w29-1"}
		}`},
		{"/Observation/obs-k-crit-w29", `{
			"resourceType": "Observation",
			"id": "obs-k-crit-w29",
			"status": "final",
			"code": {"coding": [{"system": "http://loinc.org", "code": "2823-3", "display": "Potassium [Moles/volume] in Serum or Plasma"}]},
			"subject": {"reference": "Patient/pat-w29-1"},
			"valueQuantity": {"value": 6.4, "unit": "mmol/L", "system": "http://unitsofmeasure.org", "code": "mmol/L"},
			"interpretation": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v3-ObservationInterpretation", "code": "HH", "display": "Critical high"}]}]
		}`},
		{"/DiagnosticReport/dr-w29-1", `{
			"resourceType": "DiagnosticReport",
			"id": "dr-w29-1",
			"status": "final",
			"code": {"coding": [{"system": "http://loinc.org", "code": "24323-8", "display": "Comprehensive metabolic 2000 panel"}]},
			"subject": {"reference": "Patient/pat-w29-1"},
			"result": [{"reference": "Observation/obs-k-crit-w29"}]
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. POST /$process-notification (Da Vinci Unsolicited Notification Bundle)
	notifBody := `{
		"resourceType": "Bundle",
		"id": "notif-admit-w29",
		"type": "history",
		"entry": [
			{
				"fullUrl": "MessageHeader/mh-w29-1",
				"resource": {
					"resourceType": "MessageHeader",
					"id": "mh-w29-1",
					"eventCoding": {
						"system": "http://hl7.org/fhir/us/davinci-notifications/CodeSystem/notification-event",
						"code": "notification-admit",
						"display": "Notification Admit"
					},
					"source": {"endpoint": "http://ehr.example.org/fhir"},
					"focus": [{"reference": "Encounter/enc-admit-w29"}]
				}
			},
			{
				"fullUrl": "Encounter/enc-admit-w29",
				"resource": {
					"resourceType": "Encounter",
					"id": "enc-admit-w29",
					"status": "in-progress",
					"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "IMP", "display": "inpatient encounter"},
					"subject": {"reference": "Patient/pat-w29-1"}
				}
			}
		]
	}`
	notifReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/$process-notification", strings.NewReader(notifBody))
	notifReq.Header.Set("Content-Type", "application/fhir+json")
	notifRec := httptest.NewRecorder()
	pgH.ServeHTTP(notifRec, notifReq)
	if notifRec.Code != http.StatusOK ||
		!strings.Contains(notifRec.Body.String(), `"type":"message"`) ||
		!strings.Contains(notifRec.Body.String(), `"code":"ok"`) ||
		!strings.Contains(notifRec.Body.String(), `"Encounter/enc-admit-w29"`) {
		t.Fatalf("POST /$process-notification expected message Bundle with code=ok and persisted Encounter/enc-admit-w29, got %d: %s", notifRec.Code, notifRec.Body.String())
	}

	// 2. GET /Patient/$ihe-pix?sourceIdentifier=http://hospital.example.org/mrn|MRN-W29-001&targetSystem=http://hl7.org/fhir/sid/us-ssn
	pixReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/$ihe-pix?sourceIdentifier=http://hospital.example.org/mrn|MRN-W29-001&targetSystem=http://hl7.org/fhir/sid/us-ssn", nil)
	pixRec := httptest.NewRecorder()
	pgH.ServeHTTP(pixRec, pixReq)
	if pixRec.Code != http.StatusOK ||
		!strings.Contains(pixRec.Body.String(), `"Patient/pat-w29-1"`) ||
		!strings.Contains(pixRec.Body.String(), `"999-29-0001"`) {
		t.Fatalf("GET /Patient/$ihe-pix expected Parameters with Patient/pat-w29-1 and SSN 999-29-0001, got %d: %s", pixRec.Code, pixRec.Body.String())
	}

	// 3. POST /ServiceRequest/sr-w29-1/$collect-specimen
	specReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/ServiceRequest/sr-w29-1/$collect-specimen", nil)
	specRec := httptest.NewRecorder()
	pgH.ServeHTTP(specRec, specReq)
	if specRec.Code != http.StatusOK ||
		!strings.Contains(specRec.Body.String(), `"resourceType":"Specimen"`) ||
		!strings.Contains(specRec.Body.String(), `"ACC-SR-W29-1"`) {
		t.Fatalf("POST /ServiceRequest/sr-w29-1/$collect-specimen expected Specimen with ACC-SR-W29-1, got %d: %s", specRec.Code, specRec.Body.String())
	}

	// 4. GET /DiagnosticReport/dr-w29-1/$evaluate
	drEvalReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/DiagnosticReport/dr-w29-1/$evaluate", nil)
	drEvalRec := httptest.NewRecorder()
	pgH.ServeHTTP(drEvalRec, drEvalReq)
	if drEvalRec.Code != http.StatusOK ||
		!strings.Contains(drEvalRec.Body.String(), `"valueCode":"critical-alert"`) ||
		!strings.Contains(drEvalRec.Body.String(), `"code":"LAB"`) ||
		!strings.Contains(drEvalRec.Body.String(), `"Observation/obs-k-crit-w29"`) {
		t.Fatalf("GET /DiagnosticReport/dr-w29-1/$evaluate expected critical-alert Parameters with LAB DetectedIssue, got %d: %s", drEvalRec.Code, drEvalRec.Body.String())
	}
}

func TestDaVinciPDexBulkMatchADTAndDispenseWave30(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave30-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w30-1", `{
			"resourceType": "Patient",
			"id": "pat-w30-1",
			"active": true,
			"gender": "female",
			"birthDate": "1982-09-14",
			"name": [{"family": "Nightingale", "given": ["Florence"]}]
		}`},
		{"/Condition/cond-w30-1", `{
			"resourceType": "Condition",
			"id": "cond-w30-1",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://snomed.info/sct", "code": "44054006", "display": "Type 2 diabetes mellitus"}]},
			"subject": {"reference": "Patient/pat-w30-1"}
		}`},
		{"/MedicationRequest/medreq-w30-1", `{
			"resourceType": "MedicationRequest",
			"id": "medreq-w30-1",
			"status": "active",
			"intent": "order",
			"medicationCodeableConcept": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "860975", "display": "Metformin hydrochloride 500 MG Oral Tablet"}]},
			"subject": {"reference": "Patient/pat-w30-1"}
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. POST /Patient/$bulk-member-match & GET /Group/pdex-matched-group-1/$davinci-data-export
	bmmBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{
				"name": "MemberBundle",
				"part": [
					{
						"name": "MemberPatient",
						"resource": {
							"resourceType": "Patient",
							"name": [{"family": "Nightingale", "given": ["Florence"]}],
							"birthDate": "1982-09-14",
							"gender": "female"
						}
					}
				]
			}
		]
	}`
	bmmReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$bulk-member-match", strings.NewReader(bmmBody))
	bmmReq.Header.Set("Content-Type", "application/fhir+json")
	bmmRec := httptest.NewRecorder()
	pgH.ServeHTTP(bmmRec, bmmReq)
	if bmmRec.Code != http.StatusOK ||
		!strings.Contains(bmmRec.Body.String(), `"Group/pdex-matched-group-1"`) ||
		!strings.Contains(bmmRec.Body.String(), `"Patient/pat-w30-1"`) {
		t.Fatalf("POST /Patient/$bulk-member-match expected Group/pdex-matched-group-1 with Patient/pat-w30-1, got %d: %s", bmmRec.Code, bmmRec.Body.String())
	}

	expReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Group/pdex-matched-group-1/$davinci-data-export", nil)
	expRec := httptest.NewRecorder()
	pgH.ServeHTTP(expRec, expReq)
	if expRec.Code != http.StatusOK ||
		!strings.Contains(expRec.Body.String(), `"type":"collection"`) ||
		!strings.Contains(expRec.Body.String(), `"Condition/cond-w30-1"`) {
		t.Fatalf("GET /Group/pdex-matched-group-1/$davinci-data-export expected collection Bundle with Condition/cond-w30-1, got %d: %s", expRec.Code, expRec.Body.String())
	}

	// 2. Inpatient ADT: POST /Encounter/$admit -> /Encounter/{id}/$transfer -> /Encounter/{id}/$discharge
	admitBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "subject", "valueReference": {"reference": "Patient/pat-w30-1"}},
			{"name": "location", "valueReference": {"reference": "Location/loc-ed-1"}}
		]
	}`
	admitReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Encounter/$admit", strings.NewReader(admitBody))
	admitReq.Header.Set("Content-Type", "application/fhir+json")
	admitRec := httptest.NewRecorder()
	pgH.ServeHTTP(admitRec, admitReq)
	if admitRec.Code != http.StatusOK ||
		!strings.Contains(admitRec.Body.String(), `"status":"in-progress"`) ||
		!strings.Contains(admitRec.Body.String(), `"Location/loc-ed-1"`) {
		t.Fatalf("POST /Encounter/$admit expected in-progress Encounter at Location/loc-ed-1, got %d: %s", admitRec.Code, admitRec.Body.String())
	}

	xferBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "location", "valueReference": {"reference": "Location/loc-icu-2"}}
		]
	}`
	xferReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Encounter/enc-admit-pat-w30-1/$transfer", strings.NewReader(xferBody))
	xferReq.Header.Set("Content-Type", "application/fhir+json")
	xferRec := httptest.NewRecorder()
	pgH.ServeHTTP(xferRec, xferReq)
	if xferRec.Code != http.StatusOK ||
		!strings.Contains(xferRec.Body.String(), `"Location/loc-icu-2"`) ||
		!strings.Contains(xferRec.Body.String(), `"status":"completed"`) {
		t.Fatalf("POST /Encounter/enc-admit-pat-w30-1/$transfer expected Location/loc-icu-2 and prior location completed, got %d: %s", xferRec.Code, xferRec.Body.String())
	}

	dischReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Encounter/enc-admit-pat-w30-1/$discharge", nil)
	dischRec := httptest.NewRecorder()
	pgH.ServeHTTP(dischRec, dischReq)
	if dischRec.Code != http.StatusOK ||
		!strings.Contains(dischRec.Body.String(), `"status":"finished"`) ||
		!strings.Contains(dischRec.Body.String(), `"18842-5"`) {
		t.Fatalf("POST /Encounter/enc-admit-pat-w30-1/$discharge expected finished Encounter and LOINC 18842-5 Discharge summary Composition, got %d: %s", dischRec.Code, dischRec.Body.String())
	}

	// 3. POST /MedicationRequest/medreq-w30-1/$dispense
	dispReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/MedicationRequest/medreq-w30-1/$dispense", nil)
	dispRec := httptest.NewRecorder()
	pgH.ServeHTTP(dispRec, dispReq)
	if dispRec.Code != http.StatusOK ||
		!strings.Contains(dispRec.Body.String(), `"resourceType":"MedicationDispense"`) ||
		!strings.Contains(dispRec.Body.String(), `"MedicationRequest/medreq-w30-1"`) {
		t.Fatalf("POST /MedicationRequest/medreq-w30-1/$dispense expected MedicationDispense referencing MedicationRequest/medreq-w30-1, got %d: %s", dispRec.Code, dispRec.Body.String())
	}
}

func TestPlanNetGravitySDOHAndConsentEvaluateWave31(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave31-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w31-1", `{
			"resourceType": "Patient",
			"id": "pat-w31-1",
			"active": true,
			"gender": "female",
			"birthDate": "1974-05-19",
			"name": [{"family": "Barton", "given": ["Clara"]}]
		}`},
		{"/Practitioner/prac-w31-1", `{
			"resourceType": "Practitioner",
			"id": "prac-w31-1",
			"active": true,
			"name": [{"family": "Osler", "given": ["William"]}]
		}`},
		{"/PractitionerRole/pr-w31-1", `{
			"resourceType": "PractitionerRole",
			"id": "pr-w31-1",
			"active": true,
			"practitioner": {"reference": "Practitioner/prac-w31-1"},
			"specialty": [{"coding": [{"system": "http://nucc.org/provider-taxonomy", "code": "207RC0000X", "display": "Cardiovascular Disease"}]}]
		}`},
		{"/Consent/con-deny-w31", `{
			"resourceType": "Consent",
			"id": "con-deny-w31",
			"status": "active",
			"scope": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/consentscope", "code": "patient-privacy"}]},
			"category": [{"coding": [{"system": "http://loinc.org", "code": "59284-0"}]}],
			"patient": {"reference": "Patient/pat-w31-1"},
			"provision": {"type": "deny"}
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. Da Vinci Plan-Net Provider Directory: PractitionerRole/$find-providers & HealthcareService/$find-services
	fpReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/PractitionerRole/$find-providers?specialty=207RC0000X", nil)
	fpRec := httptest.NewRecorder()
	pgH.ServeHTTP(fpRec, fpReq)
	if fpRec.Code != http.StatusOK ||
		!strings.Contains(fpRec.Body.String(), "plannet-PractitionerRole") ||
		!strings.Contains(fpRec.Body.String(), `"Practitioner/prac-w31-1"`) {
		t.Fatalf("GET /PractitionerRole/$find-providers expected plannet-PractitionerRole and included Practitioner/prac-w31-1, got %d: %s", fpRec.Code, fpRec.Body.String())
	}

	fsReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/HealthcareService/$find-services?specialty=207RC0000X", nil)
	fsRec := httptest.NewRecorder()
	pgH.ServeHTTP(fsRec, fsReq)
	if fsRec.Code != http.StatusOK || !strings.Contains(fsRec.Body.String(), "plannet-HealthcareService") {
		t.Fatalf("GET /HealthcareService/$find-services expected plannet-HealthcareService, got %d: %s", fsRec.Code, fsRec.Body.String())
	}

	// 2. Gravity SDOH Referral & Summary: POST /ServiceRequest/$sdoh-referral & GET /Patient/pat-w31-1/$sdoh-summary
	sdohRefBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "patient", "valueReference": {"reference": "Patient/pat-w31-1"}},
			{"name": "domain", "valueCode": "food-insecurity"}
		]
	}`
	sdohReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/ServiceRequest/$sdoh-referral", strings.NewReader(sdohRefBody))
	sdohReq.Header.Set("Content-Type", "application/fhir+json")
	sdohRec := httptest.NewRecorder()
	pgH.ServeHTTP(sdohRec, sdohReq)
	if sdohRec.Code != http.StatusOK ||
		!strings.Contains(sdohRec.Body.String(), "SDOHCC-ServiceRequest") ||
		!strings.Contains(sdohRec.Body.String(), "SDOHCC-TaskForReferralManagement") {
		t.Fatalf("POST /ServiceRequest/$sdoh-referral expected SDOHCC-ServiceRequest and SDOHCC-TaskForReferralManagement, got %d: %s", sdohRec.Code, sdohRec.Body.String())
	}

	sumReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w31-1/$sdoh-summary", nil)
	sumRec := httptest.NewRecorder()
	pgH.ServeHTTP(sumRec, sumReq)
	if sumRec.Code != http.StatusOK ||
		!strings.Contains(sumRec.Body.String(), "SDOHCC-Bundle") ||
		!strings.Contains(sumRec.Body.String(), `"ServiceRequest/sdoh-sr-pat-w31-1"`) {
		t.Fatalf("GET /Patient/pat-w31-1/$sdoh-summary expected SDOHCC-Bundle containing ServiceRequest/sdoh-sr-pat-w31-1, got %d: %s", sumRec.Code, sumRec.Body.String())
	}

	// 3. Consent/$evaluate with active deny provision
	conReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Consent/$evaluate?patient=Patient/pat-w31-1", nil)
	conRec := httptest.NewRecorder()
	pgH.ServeHTTP(conRec, conReq)
	if conRec.Code != http.StatusOK ||
		!strings.Contains(conRec.Body.String(), `"valueCode":"deny"`) ||
		!strings.Contains(conRec.Body.String(), `"code":"R"`) ||
		!strings.Contains(conRec.Body.String(), `"Consent/con-deny-w31"`) {
		t.Fatalf("GET /Consent/$evaluate expected decision=deny and securityLabel=R for Consent/con-deny-w31, got %d: %s", conRec.Code, conRec.Body.String())
	}
}

func TestCarinBBeCRAndVRDRDeceaseWave32(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave32-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w32-1", `{
			"resourceType": "Patient",
			"id": "pat-w32-1",
			"active": true,
			"gender": "male",
			"birthDate": "1958-11-03",
			"name": [{"family": "Snow", "given": ["John"]}]
		}`},
		{"/Coverage/cov-w32-1", `{
			"resourceType": "Coverage",
			"id": "cov-w32-1",
			"status": "active",
			"beneficiary": {"reference": "Patient/pat-w32-1"},
			"payor": [{"display": "CARIN Blue Button Plan"}]
		}`},
		{"/Encounter/enc-w32-1", `{
			"resourceType": "Encounter",
			"id": "enc-w32-1",
			"status": "finished",
			"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "IMP"},
			"subject": {"reference": "Patient/pat-w32-1"}
		}`},
		{"/Condition/cond-w32-1", `{
			"resourceType": "Condition",
			"id": "cond-w32-1",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://snomed.info/sct", "code": "840539006", "display": "Disease caused by 2019-nCoV"}]},
			"subject": {"reference": "Patient/pat-w32-1"}
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. CARIN Blue Button: GET /Patient/pat-w32-1/$carin-bb & GET /ExplanationOfBenefit/$carin-bb-summary
	cbbReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w32-1/$carin-bb", nil)
	cbbRec := httptest.NewRecorder()
	pgH.ServeHTTP(cbbRec, cbbReq)
	if cbbRec.Code != http.StatusOK ||
		!strings.Contains(cbbRec.Body.String(), "C4BB-Bundle") ||
		!strings.Contains(cbbRec.Body.String(), "C4BB-Coverage") ||
		!strings.Contains(cbbRec.Body.String(), "C4BB-ExplanationOfBenefit") {
		t.Fatalf("GET /Patient/pat-w32-1/$carin-bb expected C4BB-Bundle with C4BB-Coverage and C4BB-ExplanationOfBenefit, got %d: %s", cbbRec.Code, cbbRec.Body.String())
	}

	cbbSumReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/ExplanationOfBenefit/$carin-bb-summary?patient=Patient/pat-w32-1", nil)
	cbbSumRec := httptest.NewRecorder()
	pgH.ServeHTTP(cbbSumRec, cbbSumReq)
	if cbbSumRec.Code != http.StatusOK ||
		!strings.Contains(cbbSumRec.Body.String(), `"totalBenefitPaid"`) ||
		!strings.Contains(cbbSumRec.Body.String(), `"totalMemberLiability"`) {
		t.Fatalf("GET /ExplanationOfBenefit/$carin-bb-summary expected totalBenefitPaid and totalMemberLiability, got %d: %s", cbbSumRec.Code, cbbSumRec.Body.String())
	}

	// 2. HL7 Public Health eCR: POST /Encounter/enc-w32-1/$ecr-report & GET /PlanDefinition/$ersd-lookup
	ecrReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Encounter/enc-w32-1/$ecr-report", nil)
	ecrRec := httptest.NewRecorder()
	pgH.ServeHTTP(ecrRec, ecrReq)
	if ecrRec.Code != http.StatusOK ||
		!strings.Contains(ecrRec.Body.String(), `"55751-2"`) ||
		!strings.Contains(ecrRec.Body.String(), `"88085-6"`) ||
		!strings.Contains(ecrRec.Body.String(), `"Condition/cond-w32-1"`) {
		t.Fatalf("POST /Encounter/enc-w32-1/$ecr-report expected eICR Bundle (55751-2), RR Communication (88085-6), and Condition/cond-w32-1, got %d: %s", ecrRec.Code, ecrRec.Body.String())
	}

	ersdReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/PlanDefinition/$ersd-lookup", nil)
	ersdRec := httptest.NewRecorder()
	pgH.ServeHTTP(ersdRec, ersdReq)
	if ersdRec.Code != http.StatusOK || !strings.Contains(ersdRec.Body.String(), "ersd-rctc-plan-1") {
		t.Fatalf("GET /PlanDefinition/$ersd-lookup expected ersd-rctc-plan-1, got %d: %s", ersdRec.Code, ersdRec.Body.String())
	}

	// 3. HL7 VRDR Death Reporting: POST /Patient/pat-w32-1/$decease
	decBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "deceasedDateTime", "valueDateTime": "2026-03-01T14:30:00Z"},
			{"name": "causeCode", "valueCode": "410429000"}
		]
	}`
	decReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/pat-w32-1/$decease", strings.NewReader(decBody))
	decReq.Header.Set("Content-Type", "application/fhir+json")
	decRec := httptest.NewRecorder()
	pgH.ServeHTTP(decRec, decReq)
	if decRec.Code != http.StatusOK ||
		!strings.Contains(decRec.Body.String(), `"deceasedDateTime":"2026-03-01T14:30:00Z"`) ||
		!strings.Contains(decRec.Body.String(), `"64297-5"`) ||
		!strings.Contains(decRec.Body.String(), `"69453-9"`) {
		t.Fatalf("POST /Patient/pat-w32-1/$decease expected deceasedDateTime, VRDR Death Certificate (64297-5), and Cause of Death (69453-9), got %d: %s", decRec.Code, decRec.Body.String())
	}
}

func TestMCODEFormularyAndIHEMHDWave33(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave33-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w33-1", `{
			"resourceType": "Patient",
			"id": "pat-w33-1",
			"active": true,
			"gender": "female",
			"birthDate": "1967-04-12",
			"name": [{"family": "Curie", "given": ["Marie"]}]
		}`},
		{"/Condition/cond-w33-1", `{
			"resourceType": "Condition",
			"id": "cond-w33-1",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}]},
			"subject": {"reference": "Patient/pat-w33-1"}
		}`},
		{"/Coverage/cov-w33-1", `{
			"resourceType": "Coverage",
			"id": "cov-w33-1",
			"status": "active",
			"beneficiary": {"reference": "Patient/pat-w33-1"},
			"payor": [{"display": "USDF Gold Plan"}]
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. HL7 mCODE Oncology: POST /Condition/cond-w33-1/$stage-cancer & GET /Patient/pat-w33-1/$mcode-summary
	stageBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "tCategory", "valueString": "cT3"},
			{"name": "nCategory", "valueString": "cN2"},
			{"name": "mCategory", "valueString": "cM0"}
		]
	}`
	stageReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Condition/cond-w33-1/$stage-cancer", strings.NewReader(stageBody))
	stageReq.Header.Set("Content-Type", "application/fhir+json")
	stageRec := httptest.NewRecorder()
	pgH.ServeHTTP(stageRec, stageReq)
	if stageRec.Code != http.StatusOK ||
		!strings.Contains(stageRec.Body.String(), `"Stage IIIB"`) ||
		!strings.Contains(stageRec.Body.String(), `"21908-9"`) ||
		!strings.Contains(stageRec.Body.String(), "mcode-tnm-clinical-stage-group") {
		t.Fatalf("POST /Condition/cond-w33-1/$stage-cancer expected Stage IIIB and mcode-tnm-clinical-stage-group (21908-9), got %d: %s", stageRec.Code, stageRec.Body.String())
	}

	mcodeReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w33-1/$mcode-summary", nil)
	mcodeRec := httptest.NewRecorder()
	pgH.ServeHTTP(mcodeRec, mcodeReq)
	if mcodeRec.Code != http.StatusOK ||
		!strings.Contains(mcodeRec.Body.String(), "mcode-patient-bundle") ||
		!strings.Contains(mcodeRec.Body.String(), "mcode-primary-cancer-condition") ||
		!strings.Contains(mcodeRec.Body.String(), `"Stage IIIB"`) {
		t.Fatalf("GET /Patient/pat-w33-1/$mcode-summary expected mcode-patient-bundle with Stage IIIB, got %d: %s", mcodeRec.Code, mcodeRec.Body.String())
	}

	// 2. HL7 Da Vinci PDex US Drug Formulary: GET /MedicationKnowledge/$formulary-lookup & GET /Coverage/cov-w33-1/$formulary-coverage
	flReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/MedicationKnowledge/$formulary-lookup?code=1049502&planId=usdf-platinum-2026", nil)
	flRec := httptest.NewRecorder()
	pgH.ServeHTTP(flRec, flReq)
	if flRec.Code != http.StatusOK ||
		!strings.Contains(flRec.Body.String(), `"valueCode":"5-specialty"`) ||
		!strings.Contains(flRec.Body.String(), "usdf-FormularyDrug") {
		t.Fatalf("GET /MedicationKnowledge/$formulary-lookup expected 5-specialty and usdf-FormularyDrug, got %d: %s", flRec.Code, flRec.Body.String())
	}

	fcReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Coverage/cov-w33-1/$formulary-coverage?code=860975", nil)
	fcRec := httptest.NewRecorder()
	pgH.ServeHTTP(fcRec, fcReq)
	if fcRec.Code != http.StatusOK ||
		!strings.Contains(fcRec.Body.String(), `"valueCode":"3-preferred-brand"`) ||
		!strings.Contains(fcRec.Body.String(), `"valueBoolean":true`) {
		t.Fatalf("GET /Coverage/cov-w33-1/$formulary-coverage expected 3-preferred-brand and covered=true, got %d: %s", fcRec.Code, fcRec.Body.String())
	}

	// 3. IHE MHD v4.2.1: POST /Bundle/$iti-65 (Provide Document Bundle) & GET /DocumentReference/$iti-67 (Find Document References)
	iti65Body := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"resource": {
					"resourceType": "List",
					"id": "mhd-sub-w33-1",
					"status": "current",
					"mode": "working",
					"title": "IHE MHD SubmissionSet"
				}
			},
			{
				"resource": {
					"resourceType": "DocumentReference",
					"id": "mhd-doc-w33-1",
					"status": "current",
					"subject": {"reference": "Patient/pat-w33-1"},
					"content": [{"attachment": {"contentType": "application/pdf", "url": "Binary/mhd-bin-w33-1"}}]
				}
			}
		]
	}`
	iti65Req := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Bundle/$iti-65", strings.NewReader(iti65Body))
	iti65Req.Header.Set("Content-Type", "application/fhir+json")
	iti65Rec := httptest.NewRecorder()
	pgH.ServeHTTP(iti65Rec, iti65Req)
	if iti65Rec.Code != http.StatusOK ||
		!strings.Contains(iti65Rec.Body.String(), "IHE.MHD.Minimal.ProvideBundleResponse") ||
		!strings.Contains(iti65Rec.Body.String(), "DocumentReference/mhd-doc-w33-1") {
		t.Fatalf("POST /Bundle/$iti-65 expected ProvideBundleResponse with DocumentReference/mhd-doc-w33-1, got %d: %s", iti65Rec.Code, iti65Rec.Body.String())
	}

	iti67Req := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/DocumentReference/$iti-67?patient=Patient/pat-w33-1&status=current", nil)
	iti67Rec := httptest.NewRecorder()
	pgH.ServeHTTP(iti67Rec, iti67Req)
	if iti67Rec.Code != http.StatusOK ||
		!strings.Contains(iti67Rec.Body.String(), "IHE.MHD.FindDocumentReferencesComprehensiveResponseMessage") ||
		!strings.Contains(iti67Rec.Body.String(), `"mhd-doc-w33-1"`) {
		t.Fatalf("GET /DocumentReference/$iti-67 expected FindDocumentReferencesComprehensiveResponseMessage with mhd-doc-w33-1, got %d: %s", iti67Rec.Code, iti67Rec.Body.String())
	}
}

func TestClinicalGenomicsPACIOAndNHSNWave34(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave34-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w34-1", `{
			"resourceType": "Patient",
			"id": "pat-w34-1",
			"active": true,
			"gender": "female",
			"birthDate": "1951-08-20",
			"name": [{"family": "Franklin", "given": ["Rosalind"]}]
		}`},
		{"/QuestionnaireResponse/qr-w34-1", `{
			"resourceType": "QuestionnaireResponse",
			"id": "qr-w34-1",
			"status": "completed",
			"subject": {"reference": "Patient/pat-w34-1"}
		}`},
		{"/Encounter/enc-w34-1", `{
			"resourceType": "Encounter",
			"id": "enc-w34-1",
			"status": "in-progress",
			"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "IMP"},
			"subject": {"reference": "Patient/pat-w34-1"}
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. HL7 Clinical Genomics v3.0.0: GET /Patient/pat-w34-1/$find-subject-variants & $find-subject-tx-implications
	cgVarReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w34-1/$find-subject-variants?genes=BRCA1", nil)
	cgVarRec := httptest.NewRecorder()
	pgH.ServeHTTP(cgVarRec, cgVarReq)
	if cgVarRec.Code != http.StatusOK ||
		!strings.Contains(cgVarRec.Body.String(), `"69548-6"`) ||
		!strings.Contains(cgVarRec.Body.String(), `"BRCA1"`) ||
		!strings.Contains(cgVarRec.Body.String(), `"LA6668-3"`) {
		t.Fatalf("GET /Patient/pat-w34-1/$find-subject-variants expected variant 69548-6 with BRCA1 and LA6668-3, got %d: %s", cgVarRec.Code, cgVarRec.Body.String())
	}

	cgTxReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w34-1/$find-subject-tx-implications?treatments=Olaparib", nil)
	cgTxRec := httptest.NewRecorder()
	pgH.ServeHTTP(cgTxRec, cgTxReq)
	if cgTxRec.Code != http.StatusOK ||
		!strings.Contains(cgTxRec.Body.String(), `"51963-7"`) ||
		!strings.Contains(cgTxRec.Body.String(), `"Olaparib"`) ||
		!strings.Contains(cgTxRec.Body.String(), "therapeutic-implication") {
		t.Fatalf("GET /Patient/pat-w34-1/$find-subject-tx-implications expected 51963-7 with Olaparib, got %d: %s", cgTxRec.Code, cgTxRec.Body.String())
	}

	// 2. HL7 PACIO Post-Acute Care v2.0.0: POST /QuestionnaireResponse/qr-w34-1/$pacio-assess & GET /Patient/pat-w34-1/$pacio-summary
	pacioAssBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "bimsScore", "valueInteger": 15},
			{"name": "functionalCode", "valueCode": "06"}
		]
	}`
	pacioAssReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/QuestionnaireResponse/qr-w34-1/$pacio-assess", strings.NewReader(pacioAssBody))
	pacioAssReq.Header.Set("Content-Type", "application/fhir+json")
	pacioAssRec := httptest.NewRecorder()
	pgH.ServeHTTP(pacioAssRec, pacioAssReq)
	if pacioAssRec.Code != http.StatusOK ||
		!strings.Contains(pacioAssRec.Body.String(), `"83254-3"`) ||
		!strings.Contains(pacioAssRec.Body.String(), `"52491-8"`) ||
		!strings.Contains(pacioAssRec.Body.String(), `"valueInteger":15`) {
		t.Fatalf("POST /QuestionnaireResponse/qr-w34-1/$pacio-assess expected Section GG (83254-3) and BIMS 15 (52491-8), got %d: %s", pacioAssRec.Code, pacioAssRec.Body.String())
	}

	pacioSumReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w34-1/$pacio-summary", nil)
	pacioSumRec := httptest.NewRecorder()
	pgH.ServeHTTP(pacioSumRec, pacioSumReq)
	if pacioSumRec.Code != http.StatusOK ||
		!strings.Contains(pacioSumRec.Body.String(), "pacio-fs-bundle") ||
		!strings.Contains(pacioSumRec.Body.String(), `"83254-3"`) ||
		!strings.Contains(pacioSumRec.Body.String(), `"52491-8"`) {
		t.Fatalf("GET /Patient/pat-w34-1/$pacio-summary expected pacio-fs-bundle with 83254-3 and 52491-8, got %d: %s", pacioSumRec.Code, pacioSumRec.Body.String())
	}

	// 3. CDC NHSN HAI / dQM v1.1.0: POST /Encounter/enc-w34-1/$nhsn-hai-report & GET /Measure/$nhsn-acute-care
	nhsnRepReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Encounter/enc-w34-1/$nhsn-hai-report", nil)
	nhsnRepRec := httptest.NewRecorder()
	pgH.ServeHTTP(nhsnRepRec, nhsnRepReq)
	if nhsnRepRec.Code != http.StatusOK ||
		!strings.Contains(nhsnRepRec.Body.String(), `"haiDetected"`) ||
		!strings.Contains(nhsnRepRec.Body.String(), "nhsn-hai-aur-measure") {
		t.Fatalf("POST /Encounter/enc-w34-1/$nhsn-hai-report expected haiDetected and nhsn-hai-aur-measure MeasureReport, got %d: %s", nhsnRepRec.Code, nhsnRepRec.Body.String())
	}

	nhsnMeasReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Measure/$nhsn-acute-care", nil)
	nhsnMeasRec := httptest.NewRecorder()
	pgH.ServeHTTP(nhsnMeasRec, nhsnMeasReq)
	if nhsnMeasRec.Code != http.StatusOK || !strings.Contains(nhsnMeasRec.Body.String(), "nhsn-hai-aur-measure") {
		t.Fatalf("GET /Measure/$nhsn-acute-care expected nhsn-hai-aur-measure, got %d: %s", nhsnMeasRec.Code, nhsnMeasRec.Body.String())
	}
}

func TestVulcanBFDRAndIHEPDQmWave35(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave35-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w35-1", `{
			"resourceType": "Patient",
			"id": "pat-w35-1",
			"active": true,
			"gender": "female",
			"birthDate": "2026-10-28",
			"name": [{"family": "Apgar", "given": ["Virginia"]}]
		}`},
		{"/ResearchStudy/study-w35-1", `{
			"resourceType": "ResearchStudy",
			"id": "study-w35-1",
			"status": "active",
			"title": "Vulcan Oncology RWD Cohort Trial"
		}`},
		{"/DocumentReference/doc-w35-1", `{
			"resourceType": "DocumentReference",
			"id": "doc-w35-1",
			"status": "current",
			"type": {"coding": [{"system": "http://loinc.org", "code": "34117-2"}]},
			"subject": {"reference": "Patient/pat-w35-1"},
			"content": [
				{
					"attachment": {
						"contentType": "text/plain",
						"url": "Binary/bin-w35-1"
					}
				}
			]
		}`},
		{"/Binary/bin-w35-1", `{
			"resourceType": "Binary",
			"id": "bin-w35-1",
			"contentType": "text/plain",
			"data": "SGVsbG8gSUhFIE1IRCBJVEktNjg="
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. HL7 Vulcan RWD & Schedule of Activities v1.0.0: GET /ResearchStudy/study-w35-1/$rwd-cohort & POST /ResearchSubject/sub-w35-1/$enroll
	rwdReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/ResearchStudy/study-w35-1/$rwd-cohort", nil)
	rwdRec := httptest.NewRecorder()
	pgH.ServeHTTP(rwdRec, rwdReq)
	if rwdRec.Code != http.StatusOK ||
		!strings.Contains(rwdRec.Body.String(), `"cohortGroup"`) ||
		!strings.Contains(rwdRec.Body.String(), "vulcan-rwd-cohort-study-w35-1") {
		t.Fatalf("GET /ResearchStudy/study-w35-1/$rwd-cohort expected cohortGroup vulcan-rwd-cohort-study-w35-1, got %d: %s", rwdRec.Code, rwdRec.Body.String())
	}

	enrollBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "patient", "valueString": "Patient/pat-w35-1"},
			{"name": "study", "valueString": "ResearchStudy/study-w35-1"}
		]
	}`
	enrollReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/ResearchSubject/sub-w35-1/$enroll", strings.NewReader(enrollBody))
	enrollReq.Header.Set("Content-Type", "application/fhir+json")
	enrollRec := httptest.NewRecorder()
	pgH.ServeHTTP(enrollRec, enrollReq)
	if enrollRec.Code != http.StatusOK ||
		!strings.Contains(enrollRec.Body.String(), `"on-study"`) ||
		!strings.Contains(enrollRec.Body.String(), "SoACarePlan") {
		t.Fatalf("POST /ResearchSubject/sub-w35-1/$enroll expected on-study ResearchSubject and SoACarePlan, got %d: %s", enrollRec.Code, enrollRec.Body.String())
	}

	// 2. HL7 Vital Records BFDR v1.1.0: POST /Patient/pat-w35-1/$birth-report & GET /Composition/$bfdr-summary
	birthBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "birthWeightGrams", "valueDecimal": 3450},
			{"name": "apgar5Min", "valueInteger": 10}
		]
	}`
	birthReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/pat-w35-1/$birth-report", strings.NewReader(birthBody))
	birthReq.Header.Set("Content-Type", "application/fhir+json")
	birthRec := httptest.NewRecorder()
	pgH.ServeHTTP(birthRec, birthReq)
	if birthRec.Code != http.StatusOK ||
		!strings.Contains(birthRec.Body.String(), `"8339-4"`) ||
		!strings.Contains(birthRec.Body.String(), `"9274-2"`) ||
		!strings.Contains(birthRec.Body.String(), `"68998-4"`) {
		t.Fatalf("POST /Patient/pat-w35-1/$birth-report expected birth weight (8339-4), Apgar (9274-2), and BFDR Composition (68998-4), got %d: %s", birthRec.Code, birthRec.Body.String())
	}

	bfdrSumReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Composition/$bfdr-summary?patient=Patient/pat-w35-1", nil)
	bfdrSumRec := httptest.NewRecorder()
	pgH.ServeHTTP(bfdrSumRec, bfdrSumReq)
	if bfdrSumRec.Code != http.StatusOK ||
		!strings.Contains(bfdrSumRec.Body.String(), "Bundle-document-live-birth-report") ||
		!strings.Contains(bfdrSumRec.Body.String(), `"68998-4"`) {
		t.Fatalf("GET /Composition/$bfdr-summary expected Bundle-document-live-birth-report with 68998-4, got %d: %s", bfdrSumRec.Code, bfdrSumRec.Body.String())
	}

	// 3. IHE PDQm v3.0.0 & MHD ITI-68: GET /Patient/$ihe-pdqm & GET /DocumentReference/doc-w35-1/$ihe-iti-68
	pdqmReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/$ihe-pdqm?family=Apgar", nil)
	pdqmRec := httptest.NewRecorder()
	pgH.ServeHTTP(pdqmRec, pdqmReq)
	if pdqmRec.Code != http.StatusOK ||
		!strings.Contains(pdqmRec.Body.String(), "IHE.PDQm.QueryPatientResourceResponseMessage") ||
		!strings.Contains(pdqmRec.Body.String(), "IHE.PDQm.Patient") {
		t.Fatalf("GET /Patient/$ihe-pdqm expected IHE.PDQm.QueryPatientResourceResponseMessage and IHE.PDQm.Patient, got %d: %s", pdqmRec.Code, pdqmRec.Body.String())
	}

	iti68Req := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/DocumentReference/doc-w35-1/$ihe-iti-68", nil)
	iti68Rec := httptest.NewRecorder()
	pgH.ServeHTTP(iti68Rec, iti68Req)
	if iti68Rec.Code != http.StatusOK ||
		!strings.Contains(iti68Rec.Body.String(), `"documentReference"`) ||
		!strings.Contains(iti68Rec.Body.String(), "SGVsbG8gSUhFIE1IRCBJVEktNjg=") {
		t.Fatalf("GET /DocumentReference/doc-w35-1/$ihe-iti-68 expected documentReference and Binary payload, got %d: %s", iti68Rec.Code, iti68Rec.Body.String())
	}
}

func TestFASTNDHPhysicalActivityAndVulcanEPIWave36(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave36-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w36-1", `{
			"resourceType": "Patient",
			"id": "pat-w36-1",
			"active": true,
			"gender": "female",
			"birthDate": "1980-05-14",
			"name": [{"family": "Bannister", "given": ["Roger"]}]
		}`},
		{"/Organization/org-w36-1", `{
			"resourceType": "Organization",
			"id": "org-w36-1",
			"active": true,
			"name": "National Directory Health System",
			"identifier": [{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1992837465"}]
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. HL7 FAST National Healthcare Directory v1.0.0: GET /Endpoint/$fast-endpoint-query & POST /Organization/org-w36-1/$fast-verify
	epReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Endpoint/$fast-endpoint-query?connectionType=hl7-fhir-rest", nil)
	epRec := httptest.NewRecorder()
	pgH.ServeHTTP(epRec, epReq)
	if epRec.Code != http.StatusOK ||
		!strings.Contains(epRec.Body.String(), "ndh-Endpoint") ||
		!strings.Contains(epRec.Body.String(), "hl7-fhir-rest") {
		t.Fatalf("GET /Endpoint/$fast-endpoint-query expected ndh-Endpoint Bundle with hl7-fhir-rest, got %d: %s", epRec.Code, epRec.Body.String())
	}

	verReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Organization/org-w36-1/$fast-verify", nil)
	verRec := httptest.NewRecorder()
	pgH.ServeHTTP(verRec, verReq)
	if verRec.Code != http.StatusOK ||
		!strings.Contains(verRec.Body.String(), "ndh-VerificationResult") ||
		!strings.Contains(verRec.Body.String(), `"attested"`) {
		t.Fatalf("POST /Organization/org-w36-1/$fast-verify expected ndh-VerificationResult with status=attested, got %d: %s", verRec.Code, verRec.Body.String())
	}

	// 2. HL7 Physical Activity v1.0.0: GET /Patient/pat-w36-1/$physical-activity-summary & POST /ServiceRequest/$physical-activity-referral
	paSumReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w36-1/$physical-activity-summary", nil)
	paSumRec := httptest.NewRecorder()
	pgH.ServeHTTP(paSumRec, paSumReq)
	if paSumRec.Code != http.StatusOK ||
		!strings.Contains(paSumRec.Body.String(), `"89555-7"`) ||
		!strings.Contains(paSumRec.Body.String(), `"68516-4"`) ||
		!strings.Contains(paSumRec.Body.String(), `"82290-8"`) {
		t.Fatalf("GET /Patient/pat-w36-1/$physical-activity-summary expected EVS LOINC 89555-7, 68516-4, and 82290-8, got %d: %s", paSumRec.Code, paSumRec.Body.String())
	}

	paRefBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "patient", "valueString": "Patient/pat-w36-1"}
		]
	}`
	paRefReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/ServiceRequest/$physical-activity-referral", strings.NewReader(paRefBody))
	paRefReq.Header.Set("Content-Type", "application/fhir+json")
	paRefRec := httptest.NewRecorder()
	pgH.ServeHTTP(paRefRec, paRefReq)
	if paRefRec.Code != http.StatusOK ||
		!strings.Contains(paRefRec.Body.String(), `"390893007"`) ||
		!strings.Contains(paRefRec.Body.String(), "pa-task-for-referral-management") {
		t.Fatalf("POST /ServiceRequest/$physical-activity-referral expected SNOMED 390893007 and pa-task-for-referral-management, got %d: %s", paRefRec.Code, paRefRec.Body.String())
	}

	// 3. HL7 Vulcan ePI & Regulatory Authorization v1.0.0: GET /MedicinalProduct/$epi-document & POST /MedicinalProductAuthorization/$evaluate-authorization
	epiDocReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/MedicinalProduct/$epi-document?name=Olaparib", nil)
	epiDocRec := httptest.NewRecorder()
	pgH.ServeHTTP(epiDocRec, epiDocReq)
	if epiDocRec.Code != http.StatusOK ||
		!strings.Contains(epiDocRec.Body.String(), "Bundle-uv-epi") ||
		!strings.Contains(epiDocRec.Body.String(), `"69719-3"`) {
		t.Fatalf("GET /MedicinalProduct/$epi-document expected Bundle-uv-epi with LOINC 69719-3, got %d: %s", epiDocRec.Code, epiDocRec.Body.String())
	}

	epiAuthBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "subject", "valueString": "MedicinalProduct/epi-mp-olaparib-150"},
			{"name": "jurisdiction", "valueCode": "US"}
		]
	}`
	epiAuthReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/MedicinalProductAuthorization/$evaluate-authorization", strings.NewReader(epiAuthBody))
	epiAuthReq.Header.Set("Content-Type", "application/fhir+json")
	epiAuthRec := httptest.NewRecorder()
	pgH.ServeHTTP(epiAuthRec, epiAuthReq)
	if epiAuthRec.Code != http.StatusOK ||
		!strings.Contains(epiAuthRec.Body.String(), "RegulatedAuthorization-uv-epi") ||
		!strings.Contains(epiAuthRec.Body.String(), `"valueBoolean":true`) {
		t.Fatalf("POST /MedicinalProductAuthorization/$evaluate-authorization expected RegulatedAuthorization-uv-epi and authorized=true, got %d: %s", epiAuthRec.Code, epiAuthRec.Body.String())
	}
}

func TestELRPACIOADIAndDaVinciRAWave37(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave37-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	for _, item := range []struct {
		path string
		body string
	}{
		{"/Patient/pat-w37-1", `{
			"resourceType": "Patient",
			"id": "pat-w37-1",
			"active": true,
			"gender": "female",
			"birthDate": "1962-11-03",
			"name": [{"family": "Saunders", "given": ["Cicely"]}]
		}`},
		{"/Observation/obs-w37-lab-1", `{
			"resourceType": "Observation",
			"id": "obs-w37-lab-1",
			"status": "final",
			"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}]}],
			"code": {"coding": [{"system": "http://loinc.org", "code": "94500-6", "display": "SARS-CoV-2 (COVID-19) RNA [Presence] in Respiratory specimen by NAA with probe detection"}]},
			"subject": {"reference": "Patient/pat-w37-1"},
			"effectiveDateTime": "2026-06-01T08:30:00Z",
			"valueCodeableConcept": {"coding": [{"system": "http://snomed.info/sct", "code": "260373001", "display": "Detected"}]}
		}`},
		{"/DiagnosticReport/dr-w37-1", `{
			"resourceType": "DiagnosticReport",
			"id": "dr-w37-1",
			"status": "final",
			"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "LAB"}]}],
			"code": {"coding": [{"system": "http://loinc.org", "code": "94500-6", "display": "SARS-CoV-2 (COVID-19) RNA panel"}]},
			"subject": {"reference": "Patient/pat-w37-1"},
			"effectiveDateTime": "2026-06-01T08:30:00Z",
			"issued": "2026-06-01T09:00:00Z",
			"result": [{"reference": "Observation/obs-w37-lab-1"}]
		}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		pgH.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s expected 200/201, got %d: %s", item.path, rec.Code, rec.Body.String())
		}
	}

	// 1. HL7 Public Health ELR v1.0.0: POST /DiagnosticReport/dr-w37-1/$elr-submit & GET /Observation/$reportable-labs
	elrSubReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/DiagnosticReport/dr-w37-1/$elr-submit", nil)
	elrSubRec := httptest.NewRecorder()
	pgH.ServeHTTP(elrSubRec, elrSubReq)
	if elrSubRec.Code != http.StatusOK ||
		!strings.Contains(elrSubRec.Body.String(), "elr-MessageHeader") ||
		!strings.Contains(elrSubRec.Body.String(), `"11502-2"`) {
		t.Fatalf("POST /DiagnosticReport/dr-w37-1/$elr-submit expected elr-MessageHeader with LOINC 11502-2, got %d: %s", elrSubRec.Code, elrSubRec.Body.String())
	}

	repLabsReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Observation/$reportable-labs?patient=Patient/pat-w37-1", nil)
	repLabsRec := httptest.NewRecorder()
	pgH.ServeHTTP(repLabsRec, repLabsReq)
	if repLabsRec.Code != http.StatusOK ||
		!strings.Contains(repLabsRec.Body.String(), `"94500-6"`) ||
		!strings.Contains(repLabsRec.Body.String(), `"260373001"`) {
		t.Fatalf("GET /Observation/$reportable-labs expected SARS-CoV-2 LOINC 94500-6 and SNOMED 260373001, got %d: %s", repLabsRec.Code, repLabsRec.Body.String())
	}

	// 2. HL7 PACIO Advance Directives Interoperability (ADI v1.0.0): POST /Patient/pat-w37-1/$adi-record & GET /Composition/$adi-document
	adiRecBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "preference", "valueString": "Comfort-focused care at home"}
		]
	}`
	adiRecReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/pat-w37-1/$adi-record", strings.NewReader(adiRecBody))
	adiRecReq.Header.Set("Content-Type", "application/fhir+json")
	adiRecRec := httptest.NewRecorder()
	pgH.ServeHTTP(adiRecRec, adiRecReq)
	if adiRecRec.Code != http.StatusOK ||
		!strings.Contains(adiRecRec.Body.String(), "PADI-Consent") ||
		!strings.Contains(adiRecRec.Body.String(), `"75773-2"`) ||
		!strings.Contains(adiRecRec.Body.String(), `"42348-3"`) {
		t.Fatalf("POST /Patient/pat-w37-1/$adi-record expected PADI-Consent, LOINC 75773-2, and LOINC 42348-3, got %d: %s", adiRecRec.Code, adiRecRec.Body.String())
	}

	adiDocReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Composition/$adi-document?patient=Patient/pat-w37-1", nil)
	adiDocRec := httptest.NewRecorder()
	pgH.ServeHTTP(adiDocRec, adiDocReq)
	if adiDocRec.Code != http.StatusOK ||
		!strings.Contains(adiDocRec.Body.String(), "PADI-AdvanceDirectiveBundle") ||
		!strings.Contains(adiDocRec.Body.String(), `"42348-3"`) {
		t.Fatalf("GET /Composition/$adi-document expected PADI-AdvanceDirectiveBundle with LOINC 42348-3, got %d: %s", adiDocRec.Code, adiDocRec.Body.String())
	}

	// 3. HL7 Da Vinci Risk Adjustment v2.0.0: GET /Patient/pat-w37-1/$ra-coding-gaps & POST /MeasureReport/ra-mr-pat-w37-1/$ra-remediate
	raGapsReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w37-1/$ra-coding-gaps", nil)
	raGapsRec := httptest.NewRecorder()
	pgH.ServeHTTP(raGapsRec, raGapsReq)
	if raGapsRec.Code != http.StatusOK ||
		!strings.Contains(raGapsRec.Body.String(), "ra-measurereport") ||
		!strings.Contains(raGapsRec.Body.String(), "CMS-HCC 18") {
		t.Fatalf("GET /Patient/pat-w37-1/$ra-coding-gaps expected ra-measurereport and CMS-HCC 18, got %d: %s", raGapsRec.Code, raGapsRec.Body.String())
	}

	raRemBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "patient", "valueString": "Patient/pat-w37-1"},
			{"name": "action", "valueCode": "confirm"},
			{"name": "icd10Code", "valueCode": "E11.22"}
		]
	}`
	raRemReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/MeasureReport/ra-mr-pat-w37-1/$ra-remediate", strings.NewReader(raRemBody))
	raRemReq.Header.Set("Content-Type", "application/fhir+json")
	raRemRec := httptest.NewRecorder()
	pgH.ServeHTTP(raRemRec, raRemReq)
	if raRemRec.Code != http.StatusOK ||
		!strings.Contains(raRemRec.Body.String(), `"confirmedCondition"`) ||
		!strings.Contains(raRemRec.Body.String(), `"E11.22"`) {
		t.Fatalf("POST /MeasureReport/ra-mr-pat-w37-1/$ra-remediate expected confirmedCondition and ICD-10 E11.22, got %d: %s", raRemRec.Code, raRemRec.Body.String())
	}
}

func TestEICRPDexAndODHWave38(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave38-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	patBody := `{
		"resourceType": "Patient",
		"id": "pat-w38-1",
		"active": true,
		"gender": "female",
		"birthDate": "1978-08-22",
		"name": [{"family": "Ramazzini", "given": ["Bernardino"]}]
	}`
	putReq := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/Patient/pat-w38-1", strings.NewReader(patBody))
	putReq.Header.Set("Content-Type", "application/fhir+json")
	putRec := httptest.NewRecorder()
	pgH.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK && putRec.Code != http.StatusCreated {
		t.Fatalf("PUT /Patient/pat-w38-1 expected 200/201, got %d: %s", putRec.Code, putRec.Body.String())
	}

	// 1. HL7 Public Health eCR v2.1.1: GET /Composition/$eicr-document & POST /Communication/$rr-process
	eicrReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Composition/$eicr-document?patient=Patient/pat-w38-1", nil)
	eicrRec := httptest.NewRecorder()
	pgH.ServeHTTP(eicrRec, eicrReq)
	if eicrRec.Code != http.StatusOK ||
		!strings.Contains(eicrRec.Body.String(), "eicr-document-bundle") ||
		!strings.Contains(eicrRec.Body.String(), `"55751-2"`) {
		t.Fatalf("GET /Composition/$eicr-document expected eicr-document-bundle and LOINC 55751-2, got %d: %s", eicrRec.Code, eicrRec.Body.String())
	}

	rrBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "patient", "valueString": "Patient/pat-w38-1"},
			{"name": "conditionCode", "valueCode": "840539006"}
		]
	}`
	rrReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Communication/$rr-process", strings.NewReader(rrBody))
	rrReq.Header.Set("Content-Type", "application/fhir+json")
	rrRec := httptest.NewRecorder()
	pgH.ServeHTTP(rrRec, rrReq)
	if rrRec.Code != http.StatusOK ||
		!strings.Contains(rrRec.Body.String(), "rr-communication") ||
		!strings.Contains(rrRec.Body.String(), `"88085-6"`) ||
		!strings.Contains(rrRec.Body.String(), `"reportable-condition-confirmed"`) {
		t.Fatalf("POST /Communication/$rr-process expected rr-communication (88085-6) and reportable-condition-confirmed, got %d: %s", rrRec.Code, rrRec.Body.String())
	}

	// 2. HL7 Da Vinci PDex v2.0.0 & USDF v2.0.1: POST /Patient/pat-w38-1/$pdex-WriteData & GET /MedicationKnowledge/$formulary-alternatives
	pdexBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "sourcePayer", "valueString": "Organization/payer-acme-1"}
		]
	}`
	pdexReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/pat-w38-1/$pdex-WriteData", strings.NewReader(pdexBody))
	pdexReq.Header.Set("Content-Type", "application/fhir+json")
	pdexRec := httptest.NewRecorder()
	pgH.ServeHTTP(pdexRec, pdexReq)
	if pdexRec.Code != http.StatusOK ||
		!strings.Contains(pdexRec.Body.String(), "pdex-provenance") ||
		!strings.Contains(pdexRec.Body.String(), `"Transmitter"`) {
		t.Fatalf("POST /Patient/pat-w38-1/$pdex-WriteData expected pdex-provenance with Transmitter agent, got %d: %s", pdexRec.Code, pdexRec.Body.String())
	}

	altReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/MedicationKnowledge/$formulary-alternatives?code=860975", nil)
	altRec := httptest.NewRecorder()
	pgH.ServeHTTP(altRec, altReq)
	if altRec.Code != http.StatusOK ||
		!strings.Contains(altRec.Body.String(), "usdf-FormularyDrug") ||
		!strings.Contains(altRec.Body.String(), `"860975"`) {
		t.Fatalf("GET /MedicationKnowledge/$formulary-alternatives expected usdf-FormularyDrug with RxNorm 860975, got %d: %s", altRec.Code, altRec.Body.String())
	}

	// 3. HL7 ODH v1.3.0: POST /Patient/pat-w38-1/$record-occupation & GET /Patient/pat-w38-1/$odh-summary
	occBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "occupation", "valueString": "Flight Nurse (SOC 29-1141)"}
		]
	}`
	occReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/pat-w38-1/$record-occupation", strings.NewReader(occBody))
	occReq.Header.Set("Content-Type", "application/fhir+json")
	occRec := httptest.NewRecorder()
	pgH.ServeHTTP(occRec, occReq)
	if occRec.Code != http.StatusOK ||
		!strings.Contains(occRec.Body.String(), "odh-PastOrPresentJob") ||
		!strings.Contains(occRec.Body.String(), `"11341-5"`) ||
		!strings.Contains(occRec.Body.String(), `"74165-2"`) ||
		!strings.Contains(occRec.Body.String(), `"87511-2"`) {
		t.Fatalf("POST /Patient/pat-w38-1/$record-occupation expected ODH LOINC 11341-5, 74165-2, and 87511-2, got %d: %s", occRec.Code, occRec.Body.String())
	}

	odhSumReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w38-1/$odh-summary", nil)
	odhSumRec := httptest.NewRecorder()
	pgH.ServeHTTP(odhSumRec, odhSumReq)
	if odhSumRec.Code != http.StatusOK ||
		!strings.Contains(odhSumRec.Body.String(), "odh-PastOrPresentJob") ||
		!strings.Contains(odhSumRec.Body.String(), "odh-CombatZonePeriod") {
		t.Fatalf("GET /Patient/pat-w38-1/$odh-summary expected odh-PastOrPresentJob and odh-CombatZonePeriod, got %d: %s", odhSumRec.Code, odhSumRec.Body.String())
	}
}

func TestCCRRVBPRAndPACIOCSWave39(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave39-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	patBody := `{
		"resourceType": "Patient",
		"id": "pat-w39-1",
		"active": true,
		"gender": "female",
		"birthDate": "1955-03-19",
		"name": [{"family": "Folstein", "given": ["Marshal"]}]
	}`
	putReq := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/Patient/pat-w39-1", strings.NewReader(patBody))
	putReq.Header.Set("Content-Type", "application/fhir+json")
	putRec := httptest.NewRecorder()
	pgH.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK && putRec.Code != http.StatusCreated {
		t.Fatalf("PUT /Patient/pat-w39-1 expected 200/201, got %d: %s", putRec.Code, putRec.Body.String())
	}

	// 1. HL7 CCRR v1.0.0 & MedMorph KAR v1.0.0: GET /Patient/pat-w39-1/$cancer-registry-report & GET /PlanDefinition/$medmorph-kar
	ccrrReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w39-1/$cancer-registry-report", nil)
	ccrrRec := httptest.NewRecorder()
	pgH.ServeHTTP(ccrrRec, ccrrReq)
	if ccrrRec.Code != http.StatusOK ||
		!strings.Contains(ccrrRec.Body.String(), "ccrr-content-bundle") ||
		!strings.Contains(ccrrRec.Body.String(), `"72134-0"`) {
		t.Fatalf("GET /Patient/pat-w39-1/$cancer-registry-report expected ccrr-content-bundle and LOINC 72134-0, got %d: %s", ccrrRec.Code, ccrrRec.Body.String())
	}

	karReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/PlanDefinition/$medmorph-kar", nil)
	karRec := httptest.NewRecorder()
	pgH.ServeHTTP(karRec, karReq)
	if karRec.Code != http.StatusOK ||
		!strings.Contains(karRec.Body.String(), "us-ph-specification-bundle") ||
		!strings.Contains(karRec.Body.String(), "us-ph-plandefinition") {
		t.Fatalf("GET /PlanDefinition/$medmorph-kar expected us-ph-specification-bundle and us-ph-plandefinition, got %d: %s", karRec.Code, karRec.Body.String())
	}

	// 2. HL7 Da Vinci VBPR v1.0.0: GET /MeasureReport/$vbpr-performance & POST /MeasureReport/vbp-mr-2026-q1/$vbpr-attest
	vbprReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/MeasureReport/$vbpr-performance", nil)
	vbprRec := httptest.NewRecorder()
	pgH.ServeHTTP(vbprRec, vbprReq)
	if vbprRec.Code != http.StatusOK ||
		!strings.Contains(vbprRec.Body.String(), "vbp-measurereport") ||
		!strings.Contains(vbprRec.Body.String(), `"pay-for-performance"`) {
		t.Fatalf("GET /MeasureReport/$vbpr-performance expected vbp-measurereport with pay-for-performance, got %d: %s", vbprRec.Code, vbprRec.Body.String())
	}

	attReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/MeasureReport/vbp-mr-2026-q1/$vbpr-attest", nil)
	attRec := httptest.NewRecorder()
	pgH.ServeHTTP(attRec, attReq)
	if attRec.Code != http.StatusOK ||
		!strings.Contains(attRec.Body.String(), `"attested"`) ||
		!strings.Contains(attRec.Body.String(), `"provenance"`) {
		t.Fatalf("POST /MeasureReport/vbp-mr-2026-q1/$vbpr-attest expected attested status and provenance, got %d: %s", attRec.Code, attRec.Body.String())
	}

	// 3. HL7 PACIO Cognitive Status v1.0.0: POST /Patient/pat-w39-1/$pacio-cognitive-assess & GET /Patient/pat-w39-1/$pacio-cognitive-summary
	csBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "bimsScore", "valueInteger": 14}
		]
	}`
	csReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/pat-w39-1/$pacio-cognitive-assess", strings.NewReader(csBody))
	csReq.Header.Set("Content-Type", "application/fhir+json")
	csRec := httptest.NewRecorder()
	pgH.ServeHTTP(csRec, csReq)
	if csRec.Code != http.StatusOK ||
		!strings.Contains(csRec.Body.String(), `"cognitively-intact"`) ||
		!strings.Contains(csRec.Body.String(), `"52491-8"`) {
		t.Fatalf("POST /Patient/pat-w39-1/$pacio-cognitive-assess expected cognitively-intact and BIMS LOINC 52491-8, got %d: %s", csRec.Code, csRec.Body.String())
	}

	csSumReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/pat-w39-1/$pacio-cognitive-summary", nil)
	csSumRec := httptest.NewRecorder()
	pgH.ServeHTTP(csSumRec, csSumReq)
	if csSumRec.Code != http.StatusOK ||
		!strings.Contains(csSumRec.Body.String(), "pacio-cs-Observation") ||
		!strings.Contains(csSumRec.Body.String(), "pacio-cs-QuestionnaireResponse") {
		t.Fatalf("GET /Patient/pat-w39-1/$pacio-cognitive-summary expected pacio-cs-Observation and pacio-cs-QuestionnaireResponse, got %d: %s", csSumRec.Code, csSumRec.Body.String())
	}
}

func TestCarinRTPBCFASTIdentityAndPACIORTWave40(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "wave40-test"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	pgStore := postgres.New(db)
	pgSrv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(pgSrv)
	pgH := pgSrv.Handler()

	patBody := `{
		"resourceType": "Patient",
		"id": "pat-w40-1",
		"active": true,
		"gender": "female",
		"birthDate": "1968-11-04",
		"name": [{"family": "Banting", "given": ["Frederick"]}]
	}`
	putReq := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/Patient/pat-w40-1", strings.NewReader(patBody))
	putReq.Header.Set("Content-Type", "application/fhir+json")
	putRec := httptest.NewRecorder()
	pgH.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK && putRec.Code != http.StatusCreated {
		t.Fatalf("PUT /Patient/pat-w40-1 expected 200/201, got %d: %s", putRec.Code, putRec.Body.String())
	}

	// 1. HL7 CARIN RTPBC v2.0.0: POST /Claim/$rtpbc-submit & GET /ClaimResponse/$rtpbc-coverage
	rtpbcBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "patient", "valueString": "Patient/pat-w40-1"},
			{"name": "medicationCode", "valueCode": "860975"},
			{"name": "copay", "valueDecimal": 12.50}
		]
	}`
	rtpbcReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Claim/$rtpbc-submit", strings.NewReader(rtpbcBody))
	rtpbcReq.Header.Set("Content-Type", "application/fhir+json")
	rtpbcRec := httptest.NewRecorder()
	pgH.ServeHTTP(rtpbcRec, rtpbcReq)
	if rtpbcRec.Code != http.StatusOK ||
		!strings.Contains(rtpbcRec.Body.String(), "rtpbc-response-claimresponse") ||
		!strings.Contains(rtpbcRec.Body.String(), `"patient-pay"`) ||
		!strings.Contains(rtpbcRec.Body.String(), `"formularyAlternative"`) {
		t.Fatalf("POST /Claim/$rtpbc-submit expected rtpbc-response-claimresponse, patient-pay, and formularyAlternative, got %d: %s", rtpbcRec.Code, rtpbcRec.Body.String())
	}

	covReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/ClaimResponse/$rtpbc-coverage?patient=Patient/pat-w40-1", nil)
	covRec := httptest.NewRecorder()
	pgH.ServeHTTP(covRec, covReq)
	if covRec.Code != http.StatusOK ||
		!strings.Contains(covRec.Body.String(), "rtpbc-response-claimresponse") {
		t.Fatalf("GET /ClaimResponse/$rtpbc-coverage expected rtpbc-response-claimresponse Bundle, got %d: %s", covRec.Code, covRec.Body.String())
	}

	// 2. HL7 FAST Digital Identity v2.0.0: POST /Patient/$idi-match & POST /Person/person-w40-1/$verify-identity
	idiReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$idi-match", nil)
	idiRec := httptest.NewRecorder()
	pgH.ServeHTTP(idiRec, idiReq)
	if idiRec.Code != http.StatusOK ||
		!strings.Contains(idiRec.Body.String(), `"score":0.99`) ||
		!strings.Contains(idiRec.Body.String(), "pat-w40-1") {
		t.Fatalf("POST /Patient/$idi-match expected score 0.99 and pat-w40-1, got %d: %s", idiRec.Code, idiRec.Body.String())
	}

	verReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Person/person-w40-1/$verify-identity", nil)
	verRec := httptest.NewRecorder()
	pgH.ServeHTTP(verRec, verReq)
	if verRec.Code != http.StatusOK ||
		!strings.Contains(verRec.Body.String(), `"IAL2-AAL2"`) ||
		!strings.Contains(verRec.Body.String(), `"level3"`) ||
		!strings.Contains(verRec.Body.String(), `"VerificationResult"`) {
		t.Fatalf("POST /Person/person-w40-1/$verify-identity expected IAL2-AAL2, level3, and VerificationResult, got %d: %s", verRec.Code, verRec.Body.String())
	}

	// 3. HL7 PACIO Re-Assessment Timepoints v1.0.0: POST /Encounter/enc-w40-1/$pacio-timepoint & GET /Encounter/enc-w40-1/$pacio-timepoints
	tpReq := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/Encounter/enc-w40-1/$pacio-timepoint", nil)
	tpRec := httptest.NewRecorder()
	pgH.ServeHTTP(tpRec, tpReq)
	if tpRec.Code != http.StatusOK ||
		!strings.Contains(tpRec.Body.String(), "pacio-rt-reassessment-timepoint-encounter") ||
		!strings.Contains(tpRec.Body.String(), "pacio-rt-clinical-impression") {
		t.Fatalf("POST /Encounter/enc-w40-1/$pacio-timepoint expected pacio-rt-reassessment-timepoint-encounter and pacio-rt-clinical-impression, got %d: %s", tpRec.Code, tpRec.Body.String())
	}

	tpsReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/Encounter/enc-w40-1/$pacio-timepoints", nil)
	tpsRec := httptest.NewRecorder()
	pgH.ServeHTTP(tpsRec, tpsReq)
	if tpsRec.Code != http.StatusOK ||
		!strings.Contains(tpsRec.Body.String(), "pacio-rt-reassessment-timepoint-encounter") ||
		!strings.Contains(tpsRec.Body.String(), "pacio-rt-clinical-impression") {
		t.Fatalf("GET /Encounter/enc-w40-1/$pacio-timepoints expected pacio-rt-reassessment-timepoint-encounter and pacio-rt-clinical-impression, got %d: %s", tpsRec.Code, tpsRec.Body.String())
	}
}
