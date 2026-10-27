package server_test

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestServer_OpenAPIAndDocs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(nil, logger)
	srv.RegisterResourceType("Patient", &patpb.Patient{})
	srv.RegisterResourceType("Condition", &condpb.Condition{})
	srv.RegisterResourceType("Encounter", &encpb.Encounter{})
	srv.RegisterResourceType("Observation", &obspb.Observation{})
	srv.RegisterResourceType("Practitioner", &pracpb.Practitioner{})

	// Enable SMART auth to verify that /openapi.json and /$docs remain publicly accessible discovery endpoints
	mockAuth, err := auth.NewMockTokenValidator("https://auth.example.com", "flint-api")
	if err != nil {
		t.Fatalf("new mock validator: %v", err)
	}
	srv.SetTokenValidator(mockAuth)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Verify GET /fhir/r4/demo/openapi.json and GET /fhir/r4/openapi.json
	for _, path := range []string{"/fhir/r4/demo/openapi.json", "/fhir/r4/openapi.json"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s expected 200 OK, got %d: %s", path, resp.StatusCode, body)
		}

		var spec map[string]any
		if err := json.Unmarshal(body, &spec); err != nil {
			t.Fatalf("unmarshal OpenAPI spec from %s: %v", path, err)
		}
		if spec["openapi"] != "3.1.0" {
			t.Errorf("expected openapi=3.1.0, got %v", spec["openapi"])
		}

		components, ok := spec["components"].(map[string]any)
		if !ok {
			t.Fatalf("missing components in OpenAPI spec")
		}
		schemas, ok := components["schemas"].(map[string]any)
		if !ok {
			t.Fatalf("missing components.schemas in OpenAPI spec")
		}

		// Verify proto-compiled Patient schema
		patSchema, ok := schemas["Patient"].(map[string]any)
		if !ok {
			t.Fatalf("missing Patient schema in components.schemas")
		}
		if patSchema["x-protobuf-full-name"] != "google.fhir.r4.core.Patient" {
			t.Errorf("expected Patient x-protobuf-full-name=google.fhir.r4.core.Patient, got %v", patSchema["x-protobuf-full-name"])
		}
		patProps, _ := patSchema["properties"].(map[string]any)
		for _, field := range []string{"id", "meta", "active", "name", "gender", "birthDate", "deceasedBoolean", "deceasedDateTime"} {
			if _, exists := patProps[field]; !exists {
				t.Errorf("expected proto-compiled Patient schema to contain property %q", field)
			}
		}

		// Verify proto-compiled Observation schema
		obsSchema, ok := schemas["Observation"].(map[string]any)
		if !ok {
			t.Fatalf("missing Observation schema in components.schemas")
		}
		if obsSchema["x-protobuf-full-name"] != "google.fhir.r4.core.Observation" {
			t.Errorf("expected Observation x-protobuf-full-name=google.fhir.r4.core.Observation, got %v", obsSchema["x-protobuf-full-name"])
		}
		obsProps, _ := obsSchema["properties"].(map[string]any)
		for _, field := range []string{"status", "code", "subject", "effectiveDateTime", "valueQuantity", "valueCodeableConcept", "component"} {
			if _, exists := obsProps[field]; !exists {
				t.Errorf("expected proto-compiled Observation schema to contain property %q", field)
			}
		}

		// Verify paths include CRUD, $validate, $everything, and Bulk Data $export
		pathsMap, ok := spec["paths"].(map[string]any)
		if !ok {
			t.Fatalf("missing paths in OpenAPI spec")
		}
		for _, requiredPath := range []string{
			"/metadata",
			"/openapi.json",
			"/$docs",
			"/$export",
			"/Patient/$export",
			"/Patient/{id}/$export",
			"/$export-poll/{jobId}",
			"/$export-file/{jobId}/{fileName}",
			"/Patient/{id}/$everything",
			"/DocumentReference/$docref",
			"/Patient/{id}/$provenance",
			"/Patient",
			"/Patient/{id}",
			"/Observation",
			"/AuditEvent",
		} {
			if _, exists := pathsMap[requiredPath]; !exists {
				t.Errorf("expected OpenAPI paths to contain %q", requiredPath)
			}
		}
	}

	// 2. Verify GET /fhir/r4/demo/$docs and GET /fhir/r4/$docs
	for _, path := range []string{"/fhir/r4/demo/$docs", "/fhir/r4/$docs"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		htmlBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s expected 200 OK, got %d", path, resp.StatusCode)
		}
		htmlStr := string(htmlBytes)
		for _, expectedSubstr := range []string{
			"Flint FHIR R4",
			"FHIR R4 / US Core / ONC (g)(10) Conformance Matrix",
			"google/fhir",
			"$export",
			"openapi.json",
			"smart-token-minter",
			"request-workbench",
			"recipe-runner",
			"DocumentReference/$docref",
			"$provenance",
		} {
			if !strings.Contains(htmlStr, expectedSubstr) {
				t.Errorf("expected $docs HTML from %s to contain %q", path, expectedSubstr)
			}
		}
	}

	// 3. Verify ConformanceMatrix helper
	matrix := server.ConformanceMatrix()
	if len(matrix) < 7 {
		t.Errorf("expected at least 7 rows in ConformanceMatrix, got %d", len(matrix))
	}
}

func TestServer_BulkExport(t *testing.T) {
	dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("FLINT_TEST_POSTGRES_DSN not set")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-export"
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

	// Seed cohort via transaction Bundle:
	// - exp-prac-1 (Practitioner)
	// - exp-pat-1 (Patient) + exp-enc-1 (Encounter) + exp-cond-1 (Condition) + exp-obs-1 (Observation referencing exp-prac-1)
	// - exp-pat-2 (Patient) + exp-obs-2 (Observation)
	seedBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"resource": {"resourceType": "Practitioner", "id": "exp-prac-1", "active": true, "name": [{"family": "ExportDoc"}]},
				"request": {"method": "PUT", "url": "Practitioner/exp-prac-1"}
			},
			{
				"resource": {"resourceType": "Patient", "id": "exp-pat-1", "active": true, "gender": "female", "name": [{"family": "ExportOne"}]},
				"request": {"method": "PUT", "url": "Patient/exp-pat-1"}
			},
			{
				"resource": {
					"resourceType": "Encounter",
					"id": "exp-enc-1",
					"status": "finished",
					"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
					"subject": {"reference": "Patient/exp-pat-1"}
				},
				"request": {"method": "PUT", "url": "Encounter/exp-enc-1"}
			},
			{
				"resource": {
					"resourceType": "Condition",
					"id": "exp-cond-1",
					"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
					"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003"}]},
					"subject": {"reference": "Patient/exp-pat-1"}
				},
				"request": {"method": "PUT", "url": "Condition/exp-cond-1"}
			},
			{
				"resource": {
					"resourceType": "Observation",
					"id": "exp-obs-1",
					"status": "final",
					"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
					"subject": {"reference": "Patient/exp-pat-1"},
					"performer": [{"reference": "Practitioner/exp-prac-1"}],
					"valueQuantity": {"value": 72, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
				},
				"request": {"method": "PUT", "url": "Observation/exp-obs-1"}
			},
			{
				"resource": {"resourceType": "Patient", "id": "exp-pat-2", "active": true, "gender": "male", "name": [{"family": "ExportTwo"}]},
				"request": {"method": "PUT", "url": "Patient/exp-pat-2"}
			},
			{
				"resource": {
					"resourceType": "Observation",
					"id": "exp-obs-2",
					"status": "final",
					"code": {"coding": [{"system": "http://loinc.org", "code": "29463-7"}]},
					"subject": {"reference": "Patient/exp-pat-2"},
					"valueQuantity": {"value": 80, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"}
				},
				"request": {"method": "PUT", "url": "Observation/exp-obs-2"}
			}
		]
	}`
	respSeed, err := http.Post(ts.URL+"/fhir/r4/"+tenant, "application/fhir+json", strings.NewReader(seedBundle))
	if err != nil {
		t.Fatalf("seed bundle: %v", err)
	}
	seedBytes, _ := io.ReadAll(respSeed.Body)
	respSeed.Body.Close()
	if respSeed.StatusCode != http.StatusOK {
		t.Fatalf("seed bundle failed (%d): %s", respSeed.StatusCode, seedBytes)
	}

	parseNDJSON := func(t *testing.T, r io.Reader) []map[string]any {
		t.Helper()
		var out []map[string]any
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				t.Fatalf("invalid NDJSON line %q: %v", line, err)
			}
			if obj["resourceType"] == "" || obj["id"] == "" {
				t.Fatalf("NDJSON resource missing resourceType or id: %v", obj)
			}
			out = append(out, obj)
		}
		if err := scanner.Err(); err != nil {
			t.Fatalf("scan NDJSON: %v", err)
		}
		return out
	}

	t.Run("Direct NDJSON streaming: System $export, Patient/$export, and Single-Patient Patient/{id}/$export", func(t *testing.T) {
		// 1. System-level $export returns all 7 resources
		respSys, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/$export")
		if err != nil {
			t.Fatalf("GET /$export: %v", err)
		}
		if !strings.Contains(respSys.Header.Get("Content-Type"), "application/fhir+ndjson") {
			t.Errorf("expected Content-Type application/fhir+ndjson, got %q", respSys.Header.Get("Content-Type"))
		}
		sysItems := parseNDJSON(t, respSys.Body)
		respSys.Body.Close()
		if len(sysItems) != 7 {
			t.Fatalf("expected 7 resources in system $export, got %d", len(sysItems))
		}

		// 2. All-Patients group $export returns all 7 compartment + referenced Practitioner resources
		respGroup, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Patient/$export")
		if err != nil {
			t.Fatalf("GET /Patient/$export: %v", err)
		}
		groupItems := parseNDJSON(t, respGroup.Body)
		respGroup.Body.Close()
		if len(groupItems) != 7 {
			t.Fatalf("expected 7 resources in Patient/$export, got %d", len(groupItems))
		}

		// 3. Single-Patient compartment Patient/exp-pat-1/$export returns ONLY exp-pat-1's 5 resources
		respPat1, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Patient/exp-pat-1/$export")
		if err != nil {
			t.Fatalf("GET /Patient/exp-pat-1/$export: %v", err)
		}
		pat1Items := parseNDJSON(t, respPat1.Body)
		respPat1.Body.Close()
		if len(pat1Items) != 5 {
			t.Fatalf("expected 5 resources in Patient/exp-pat-1/$export, got %d", len(pat1Items))
		}
		for _, item := range pat1Items {
			id := item["id"].(string)
			if id == "exp-pat-2" || id == "exp-obs-2" {
				t.Errorf("Patient/exp-pat-1/$export leaked exp-pat-2 resource: %s", id)
			}
		}

		// 4. Filter by _type=Patient,Observation on Patient/exp-pat-1/$export -> 2 resources
		respTyped, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Patient/exp-pat-1/$export?_type=Patient,Observation")
		if err != nil {
			t.Fatalf("GET _type=Patient,Observation: %v", err)
		}
		typedItems := parseNDJSON(t, respTyped.Body)
		respTyped.Body.Close()
		if len(typedItems) != 2 {
			t.Fatalf("expected 2 resources with _type=Patient,Observation, got %d", len(typedItems))
		}

		// 5. Filter by future _since -> 0 resources
		respFuture, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/$export?_since=2099-01-01T00:00:00Z")
		if err != nil {
			t.Fatalf("GET future _since: %v", err)
		}
		futureItems := parseNDJSON(t, respFuture.Body)
		respFuture.Body.Close()
		if len(futureItems) != 0 {
			t.Fatalf("expected 0 resources for future _since, got %d", len(futureItems))
		}
	})

	t.Run("Async Bulk Data Kick-Off (Prefer: respond-async), Status Polling, NDJSON File Download, and Cancellation", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/Patient/exp-pat-1/$export", nil)
		req.Header.Set("Prefer", "respond-async")
		req.Header.Set("Accept", "application/fhir+json")

		respKickoff, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("async kickoff: %v", err)
		}
		respKickoff.Body.Close()
		if respKickoff.StatusCode != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted for Prefer: respond-async, got %d", respKickoff.StatusCode)
		}

		pollURL := respKickoff.Header.Get("Content-Location")
		if pollURL == "" || !strings.Contains(pollURL, "/$export-poll/") {
			t.Fatalf("expected Content-Location header containing /$export-poll/, got %q", pollURL)
		}

		// Poll job status
		respPoll, err := http.Get(pollURL)
		if err != nil {
			t.Fatalf("GET pollURL: %v", err)
		}
		pollBytes, _ := io.ReadAll(respPoll.Body)
		respPoll.Body.Close()
		if respPoll.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK from poll endpoint, got %d: %s", respPoll.StatusCode, pollBytes)
		}

		var manifest map[string]any
		if err := json.Unmarshal(pollBytes, &manifest); err != nil {
			t.Fatalf("unmarshal manifest: %v", err)
		}
		outputs, ok := manifest["output"].([]any)
		if !ok || len(outputs) != 5 {
			t.Fatalf("expected 5 output file entries in manifest (Condition, Encounter, Observation, Patient, Practitioner), got %v", manifest["output"])
		}

		// Download each NDJSON file in manifest["output"] and verify resource count
		totalDownloaded := 0
		for _, outEntry := range outputs {
			entryMap := outEntry.(map[string]any)
			fileURL := entryMap["url"].(string)
			expectedCount := int(entryMap["count"].(float64))

			respFile, err := http.Get(fileURL)
			if err != nil {
				t.Fatalf("GET %s: %v", fileURL, err)
			}
			if !strings.Contains(respFile.Header.Get("Content-Type"), "application/fhir+ndjson") {
				t.Errorf("expected application/fhir+ndjson on file download, got %q", respFile.Header.Get("Content-Type"))
			}
			items := parseNDJSON(t, respFile.Body)
			respFile.Body.Close()
			if len(items) != expectedCount {
				t.Errorf("expected %d items in %s, got %d", expectedCount, fileURL, len(items))
			}
			totalDownloaded += len(items)
		}
		if totalDownloaded != 5 {
			t.Errorf("expected 5 total downloaded resources across NDJSON files, got %d", totalDownloaded)
		}

		// Cancel/Delete export job -> 202 Accepted, subsequent poll -> 404 Not Found
		reqDel, _ := http.NewRequest(http.MethodDelete, pollURL, nil)
		respDel, err := http.DefaultClient.Do(reqDel)
		if err != nil {
			t.Fatalf("DELETE pollURL: %v", err)
		}
		respDel.Body.Close()
		if respDel.StatusCode != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted on DELETE pollURL, got %d", respDel.StatusCode)
		}

		respPollAfterDel, err := http.Get(pollURL)
		if err != nil {
			t.Fatalf("GET pollURL after DELETE: %v", err)
		}
		respPollAfterDel.Body.Close()
		if respPollAfterDel.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found after job deletion, got %d", respPollAfterDel.StatusCode)
		}
	})

	t.Run("Validation of invalid $export parameters and missing patients", func(t *testing.T) {
		for _, tc := range []struct {
			path       string
			wantStatus int
		}{
			{"/$export?_outputFormat=text/csv", http.StatusBadRequest},
			{"/$export?_since=invalid-date", http.StatusBadRequest},
			{"/$export?_type=UnknownResource", http.StatusBadRequest},
			{"/Patient/non-existent-id/$export", http.StatusNotFound},
		} {
			resp, err := http.Get(ts.URL + "/fhir/r4/" + tenant + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("GET %s expected %d, got %d", tc.path, tc.wantStatus, resp.StatusCode)
			}
		}
	})

	t.Run("SMART on FHIR v2 Security & Compartment Isolation for $export", func(t *testing.T) {
		mockAuth, err := auth.NewMockTokenValidator("https://auth.example.com", "flint-api")
		if err != nil {
			t.Fatalf("new mock validator: %v", err)
		}
		mockAuth.RegisterStatic("pat1-token", &authv1.SecurityContext{
			Subject:   "patient-user-1",
			TenantId:  tenant,
			PatientId: "exp-pat-1",
			Scopes:    auth.ParseScopes("patient/Patient.rs patient/Observation.rs"),
		})
		mockAuth.RegisterStatic("pat2-token", &authv1.SecurityContext{
			Subject:   "patient-user-2",
			TenantId:  tenant,
			PatientId: "exp-pat-2",
			Scopes:    auth.ParseScopes("patient/*.rs"),
		})
		mockAuth.RegisterStatic("system-token", &authv1.SecurityContext{
			Subject:  "backend-service-1",
			TenantId: tenant,
			Scopes:   auth.ParseScopes("system/*.rs"),
		})

		srv.SetTokenValidator(mockAuth)
		defer srv.SetTokenValidator(nil)

		doAuthGet := func(urlStr, token, prefer string) *http.Response {
			req, _ := http.NewRequest(http.MethodGet, urlStr, nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			if prefer != "" {
				req.Header.Set("Prefer", prefer)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", urlStr, err)
			}
			return resp
		}

		// 1. Unauthenticated $export returns 401 Unauthorized
		r401 := doAuthGet(ts.URL+"/fhir/r4/"+tenant+"/$export", "", "")
		r401.Body.Close()
		if r401.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized without token, got %d", r401.StatusCode)
		}

		// 2. Patient-scoped token cannot invoke System $export or All-Patients Patient/$export -> 403 Forbidden
		rSys403 := doAuthGet(ts.URL+"/fhir/r4/"+tenant+"/$export", "pat1-token", "")
		rSys403.Body.Close()
		if rSys403.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for patient token on system $export, got %d", rSys403.StatusCode)
		}

		rGrp403 := doAuthGet(ts.URL+"/fhir/r4/"+tenant+"/Patient/$export", "pat1-token", "")
		rGrp403.Body.Close()
		if rGrp403.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for patient token on group Patient/$export, got %d", rGrp403.StatusCode)
		}

		// 3. Patient-1 token cannot export Patient-2's compartment -> 403 Forbidden
		rCross403 := doAuthGet(ts.URL+"/fhir/r4/"+tenant+"/Patient/exp-pat-2/$export", "pat1-token", "")
		rCross403.Body.Close()
		if rCross403.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for cross-patient $export, got %d", rCross403.StatusCode)
		}

		// 4. Patient-1 token CAN export Patient/exp-pat-1/$export, and only receives Patient + Observation (authorized by scopes!)
		rPat1OK := doAuthGet(ts.URL+"/fhir/r4/"+tenant+"/Patient/exp-pat-1/$export", "pat1-token", "")
		pat1ScopedItems := parseNDJSON(t, rPat1OK.Body)
		rPat1OK.Body.Close()
		if rPat1OK.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for pat1-token on Patient/exp-pat-1/$export, got %d", rPat1OK.StatusCode)
		}
		if len(pat1ScopedItems) != 2 {
			t.Fatalf("expected 2 resources (Patient + Observation) for pat1-token scopes, got %d", len(pat1ScopedItems))
		}

		// 5. System token CAN run async system $export, poll, and download; pat2-token is blocked (403) from polling/downloading system job
		rSysKick := doAuthGet(ts.URL+"/fhir/r4/"+tenant+"/$export", "system-token", "respond-async")
		rSysKick.Body.Close()
		if rSysKick.StatusCode != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted for system-token async $export, got %d", rSysKick.StatusCode)
		}
		sysPollURL := rSysKick.Header.Get("Content-Location")

		// pat2-token attempting to poll the system export job must get 403 Forbidden
		rHijack := doAuthGet(sysPollURL, "pat2-token", "")
		rHijack.Body.Close()
		if rHijack.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden when pat2-token polls system export job, got %d", rHijack.StatusCode)
		}

		// system-token polls successfully
		rSysPoll := doAuthGet(sysPollURL, "system-token", "")
		rSysPoll.Body.Close()
		if rSysPoll.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK when system-token polls system export job, got %d", rSysPoll.StatusCode)
		}
	})

	t.Run("ONC (g)(10) Group/{id}/$export cohort scoping, _typeFilter, patient param, and POST Parameters kick-off", func(t *testing.T) {
		// Create Group/cohort-1 with active exp-pat-1 and inactive exp-pat-2
		groupJSON := `{
			"resourceType": "Group",
			"id": "cohort-1",
			"type": "person",
			"actual": true,
			"member": [
				{"entity": {"reference": "Patient/exp-pat-1"}},
				{"entity": {"reference": "Patient/exp-pat-2"}, "inactive": true}
			]
		}`
		reqGrp, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Group/cohort-1", strings.NewReader(groupJSON))
		reqGrp.Header.Set("Content-Type", "application/fhir+json")
		respGrp, err := http.DefaultClient.Do(reqGrp)
		if err != nil {
			t.Fatalf("PUT Group/cohort-1: %v", err)
		}
		respGrp.Body.Close()
		if respGrp.StatusCode != http.StatusCreated && respGrp.StatusCode != http.StatusOK {
			t.Fatalf("expected 201/200 on PUT Group/cohort-1, got %d", respGrp.StatusCode)
		}

		// 1. GET /Group/cohort-1/$export exports ONLY exp-pat-1's compartment (5 resources: exp-pat-1, exp-enc-1, exp-cond-1, exp-obs-1, exp-prac-1)
		respCohort, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Group/cohort-1/$export")
		if err != nil {
			t.Fatalf("GET /Group/cohort-1/$export: %v", err)
		}
		cohortItems := parseNDJSON(t, respCohort.Body)
		respCohort.Body.Close()
		if len(cohortItems) != 5 {
			t.Fatalf("expected 5 resources in Group/cohort-1/$export (excluding inactive exp-pat-2), got %d", len(cohortItems))
		}
		for _, item := range cohortItems {
			if item["id"] == "exp-pat-2" || item["id"] == "exp-obs-2" {
				t.Errorf("inactive group member resource %s/%s leaked into Group/cohort-1/$export", item["resourceType"], item["id"])
			}
		}

		// 2. Non-existent Group returns 404 Not Found, while built-in Group/all still works
		respMissingGrp, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Group/does-not-exist/$export")
		if err != nil {
			t.Fatalf("GET /Group/does-not-exist/$export: %v", err)
		}
		respMissingGrp.Body.Close()
		if respMissingGrp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 Not Found for missing Group/does-not-exist/$export, got %d", respMissingGrp.StatusCode)
		}

		// 3. _typeFilter filters exported Observations by FHIR search criteria (code=8867-4)
		respTF, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Patient/$export?_type=Observation&_typeFilter=Observation%3Fcode%3D8867-4")
		if err != nil {
			t.Fatalf("GET /Patient/$export with _typeFilter: %v", err)
		}
		tfItems := parseNDJSON(t, respTF.Body)
		respTF.Body.Close()
		if len(tfItems) != 1 || tfItems[0]["id"] != "exp-obs-1" {
			t.Fatalf("expected _typeFilter=Observation?code=8867-4 to return only exp-obs-1, got %+v", tfItems)
		}

		// 4. POST /Patient/$export with Parameters resource specifying patient=Patient/exp-pat-2 and _type=Patient,Observation
		postParams := `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "patient", "valueReference": {"reference": "Patient/exp-pat-2"}},
				{"name": "_type", "valueString": "Patient,Observation"}
			]
		}`
		respPostExp, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/Patient/$export", "application/fhir+json", strings.NewReader(postParams))
		if err != nil {
			t.Fatalf("POST /Patient/$export: %v", err)
		}
		postExpItems := parseNDJSON(t, respPostExp.Body)
		respPostExp.Body.Close()
		if len(postExpItems) != 2 {
			t.Fatalf("expected 2 resources (exp-pat-2 and exp-obs-2) from POST /Patient/$export with patient filter, got %d", len(postExpItems))
		}
	})

	t.Run("FHIR R4 Subscription advanced criteria, $status operation, and US Core $validate", func(t *testing.T) {
		// Create an active Subscription with quantity + token criteria and query $status
		subJSON := `{
			"resourceType": "Subscription",
			"id": "sub-hr-high",
			"status": "requested",
			"reason": "Alert on tachycardic heart rate",
			"criteria": "Observation?code=http://loinc.org|8867-4&value-quantity=gt100",
			"channel": {
				"type": "rest-hook",
				"endpoint": "` + ts.URL + `/fhir/r4/` + tenant + `/$validate",
				"payload": "application/fhir+json"
			}
		}`
		reqSub, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Subscription/sub-hr-high", strings.NewReader(subJSON))
		reqSub.Header.Set("Content-Type", "application/fhir+json")
		respSub, err := http.DefaultClient.Do(reqSub)
		if err != nil {
			t.Fatalf("PUT Subscription/sub-hr-high: %v", err)
		}
		respSub.Body.Close()
		if respSub.StatusCode != http.StatusCreated && respSub.StatusCode != http.StatusOK {
			t.Fatalf("expected 201/200 on PUT Subscription/sub-hr-high, got %d", respSub.StatusCode)
		}

		// Query GET /Subscription/sub-hr-high/$status
		respSt, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Subscription/sub-hr-high/$status")
		if err != nil {
			t.Fatalf("GET /Subscription/sub-hr-high/$status: %v", err)
		}
		stBytes, _ := io.ReadAll(respSt.Body)
		respSt.Body.Close()
		if respSt.StatusCode != http.StatusOK || !strings.Contains(string(stBytes), `"Parameters"`) || !strings.Contains(string(stBytes), `"query-status"`) {
			t.Fatalf("expected 200 OK Parameters with query-status from Subscription/$status, got %d: %s", respSt.StatusCode, string(stBytes))
		}

		// Type-level GET /Subscription/$status returns a Bundle of Parameters
		respAllSt, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Subscription/$status")
		if err != nil {
			t.Fatalf("GET /Subscription/$status: %v", err)
		}
		allStBytes, _ := io.ReadAll(respAllSt.Body)
		respAllSt.Body.Close()
		if respAllSt.StatusCode != http.StatusOK || !strings.Contains(string(allStBytes), `"Bundle"`) {
			t.Fatalf("expected 200 OK Bundle from GET /Subscription/$status, got %d: %s", respAllSt.StatusCode, string(allStBytes))
		}

		// Instance-level GET /Patient/exp-pat-1/$validate?profile=http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient
		// exp-pat-1 has no identifier, so validating against us-core-patient returns 400 Bad Request OperationOutcome
		respInstVal, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Patient/exp-pat-1/$validate?profile=http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient")
		if err != nil {
			t.Fatalf("GET /Patient/exp-pat-1/$validate: %v", err)
		}
		respInstVal.Body.Close()
		if respInstVal.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 Bad Request when exp-pat-1 (missing identifier) is validated against us-core-patient, got %d", respInstVal.StatusCode)
		}

		// Base FHIR R4 instance GET /Patient/exp-pat-1/$validate (without US Core profile) passes with 200 OK
		respBaseVal, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Patient/exp-pat-1/$validate")
		if err != nil {
			t.Fatalf("GET /Patient/exp-pat-1/$validate base: %v", err)
		}
		respBaseVal.Body.Close()
		if respBaseVal.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK on base instance GET /Patient/exp-pat-1/$validate, got %d", respBaseVal.StatusCode)
		}
	})
}

func TestServer_ValidateUSCoreProfilesAndSubscriptionCriteria(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(nil, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Parameters wrapper with profile=us-core-patient and mode=update fails when Patient is missing id and gender
	paramsBody := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "profile", "valueCanonical": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
			{"name": "mode", "valueCode": "update"},
			{
				"name": "resource",
				"resource": {
					"resourceType": "Patient",
					"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-1"}],
					"name": [{"family": "Doe", "given": ["John"]}]
				}
			}
		]
	}`
	respFail, err := http.Post(ts.URL+"/fhir/r4/demo/Patient/$validate", "application/fhir+json", strings.NewReader(paramsBody))
	if err != nil {
		t.Fatalf("POST /Patient/$validate: %v", err)
	}
	failBytes, _ := io.ReadAll(respFail.Body)
	respFail.Body.Close()
	if respFail.StatusCode != http.StatusBadRequest || !strings.Contains(string(failBytes), "Patient.id") || !strings.Contains(string(failBytes), "Patient.gender") {
		t.Fatalf("expected 400 Bad Request with Patient.id and Patient.gender issues, got %d: %s", respFail.StatusCode, string(failBytes))
	}

	// 2. Valid US Core Patient with id and gender passes $validate?profile=...&mode=update
	validBody := `{
		"resourceType": "Patient",
		"id": "pat-valid-1",
		"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-1"}],
		"name": [{"family": "Doe", "given": ["John"]}],
		"gender": "male"
	}`
	respOK, err := http.Post(
		ts.URL+"/fhir/r4/demo/Patient/$validate?profile=http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient&mode=update",
		"application/fhir+json",
		strings.NewReader(validBody),
	)
	if err != nil {
		t.Fatalf("POST /Patient/$validate valid: %v", err)
	}
	respOK.Body.Close()
	if respOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for valid US Core Patient, got %d", respOK.StatusCode)
	}

	// 3. Subscription with unknown target resourceType in criteria fails $validate
	badSub := `{
		"resourceType": "Subscription",
		"status": "requested",
		"reason": "Invalid criteria test",
		"criteria": "NonExistentResource?code=123",
		"channel": {
			"type": "rest-hook",
			"endpoint": "https://hooks.example.org/fhir",
			"payload": "application/fhir+json"
		}
	}`
	respBadSub, err := http.Post(ts.URL+"/fhir/r4/demo/Subscription/$validate", "application/fhir+json", strings.NewReader(badSub))
	if err != nil {
		t.Fatalf("POST /Subscription/$validate: %v", err)
	}
	respBadSub.Body.Close()
	if respBadSub.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for Subscription with unsupported criteria resourceType, got %d", respBadSub.StatusCode)
	}
}
