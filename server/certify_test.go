package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/certify"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestServer_CertifyEndpointAndRunner(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-certify-suite"
	cleanup := func() {
		for _, table := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri", "fhir_audit_event", "fhir_resource_history", "fhir_resource"} {
			_, _ = db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = $1", tenant)
		}
	}
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)
	srv.SetAuditRecorder(audit.NewStoreRecorder(st))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Run pkg/certify.Run directly and verify 100% pass rate & scorecard
	rep, err := certify.Run(ctx, certify.Config{
		BaseURL:    ts.URL,
		Tenant:     tenant,
		HTTPClient: ts.Client(),
	})
	if err != nil {
		t.Fatalf("certify.Run failed: %v", err)
	}
	if rep.Result != "pass" || rep.Failed != 0 || rep.Score != 100.0 {
		t.Fatalf("expected 100%% certification pass, got result=%s score=%.1f failed=%d:\n%s",
			rep.Result, rep.Score, rep.Failed, rep.FormatScorecard())
	}
	if rep.TotalChecks < 20 {
		t.Errorf("expected >= 20 certification checks, got %d", rep.TotalChecks)
	}
	scorecard := rep.FormatScorecard()
	if !strings.Contains(scorecard, "OVERALL RESULT: PASS (100.0%") && !strings.Contains(scorecard, "Overall Result: PASS (100.0%") {
		t.Errorf("unexpected scorecard output:\n%s", scorecard)
	}

	// 2. Verify POST /fhir/r4/{tenant}/$certify returns a valid FHIR R4 TestReport
	cleanup()
	resp, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/$certify", "application/fhir+json", nil)
	if err != nil {
		t.Fatalf("POST /$certify: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /$certify status=%d: %s", resp.StatusCode, body)
	}
	var testReport map[string]any
	if err := json.Unmarshal(body, &testReport); err != nil {
		t.Fatalf("unmarshal TestReport: %v", err)
	}
	if testReport["resourceType"] != "TestReport" {
		t.Errorf("expected resourceType=TestReport, got %v", testReport["resourceType"])
	}
	if testReport["result"] != "pass" {
		t.Errorf("expected TestReport.result=pass, got %v", testReport["result"])
	}
	if testReport["score"] != 100.0 {
		t.Errorf("expected TestReport.score=100.0, got %v", testReport["score"])
	}
}

func TestServer_BundlePatchAndConditionalCRUD(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-bundle-patch-cond"
	cleanup := func() {
		for _, table := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri", "fhir_audit_event", "fhir_resource_history", "fhir_resource"} {
			_, _ = db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = $1", tenant)
		}
	}
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Step 1: Seed initial Organization via PUT
	seedBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"resource": {
					"resourceType": "Organization",
					"id": "bnd-org-1",
					"active": true,
					"name": "Bundle Org v1",
					"identifier": [{"system": "http://hl7.org/fhir/sid/us-npi", "value": "4445556667"}]
				},
				"request": {"method": "PUT", "url": "Organization/bnd-org-1"}
			}
		]
	}`
	resp, err := http.Post(ts.URL+"/fhir/r4/"+tenant, "application/fhir+json", strings.NewReader(seedBundle))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("seed bundle failed: %v", err)
	}
	resp.Body.Close()

	// Step 2: Transaction Bundle containing RFC 6902 PATCH + Conditional PUT (create-on-0-match) + Conditional PUT (update-on-1-match)
	patchAndCondBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"resource": [
					{"op": "test", "path": "/name", "value": "Bundle Org v1"},
					{"op": "replace", "path": "/name", "value": "Bundle Org Patched"}
				],
				"request": {"method": "PATCH", "url": "Organization/bnd-org-1", "ifMatch": "W/\"1\""}
			},
			{
				"resource": {
					"resourceType": "Organization",
					"active": true,
					"name": "Conditionally Created In Bundle",
					"identifier": [{"system": "http://hl7.org/fhir/sid/us-npi", "value": "8889990001"}]
				},
				"request": {"method": "PUT", "url": "Organization?identifier=http://hl7.org/fhir/sid/us-npi|8889990001"}
			}
		]
	}`
	resp, err = http.Post(ts.URL+"/fhir/r4/"+tenant, "application/fhir+json", strings.NewReader(patchAndCondBundle))
	if err != nil {
		t.Fatalf("POST patchAndCondBundle: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for patchAndCondBundle, got %d: %s", resp.StatusCode, b)
	}

	// Verify Organization/bnd-org-1 was patched to "Bundle Org Patched" (ETag W/"2")
	getResp, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Organization/bnd-org-1")
	if err != nil {
		t.Fatalf("GET bnd-org-1: %v", err)
	}
	orgBytes, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	var orgMap map[string]any
	_ = json.Unmarshal(orgBytes, &orgMap)
	if orgMap["name"] != "Bundle Org Patched" {
		t.Errorf("expected name=Bundle Org Patched, got %v", orgMap["name"])
	}
	if getResp.Header.Get("ETag") != `W/"2"` {
		t.Errorf("expected ETag W/\"2\", got %q", getResp.Header.Get("ETag"))
	}
}
