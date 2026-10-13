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

func TestServer_CriticalSecurityAndSearchHardening(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-hardening-suite"
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

	// Seed 2 patients and 3 observations (1 vital-signs for pat-1, 1 social-history for pat-1, 1 vital-signs for pat-2)
	seedBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"resource": {"resourceType": "Patient", "id": "hard-pat-1", "name": [{"family": "HardeningOne"}]},
				"request": {"method": "PUT", "url": "Patient/hard-pat-1"}
			},
			{
				"resource": {"resourceType": "Patient", "id": "hard-pat-2", "name": [{"family": "HardeningTwo"}]},
				"request": {"method": "PUT", "url": "Patient/hard-pat-2"}
			},
			{
				"resource": {
					"resourceType": "Observation",
					"id": "obs-vital-1",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
					"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
					"subject": {"reference": "Patient/hard-pat-1"},
					"effectiveDateTime": "2026-01-15T09:15:00Z"
				},
				"request": {"method": "PUT", "url": "Observation/obs-vital-1"}
			},
			{
				"resource": {
					"resourceType": "Observation",
					"id": "obs-social-1",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history"}]}],
					"code": {"coding": [{"system": "http://loinc.org", "code": "72166-2"}]},
					"subject": {"reference": "Patient/hard-pat-1"},
					"effectiveDateTime": "2026-01-15T14:30:00Z"
				},
				"request": {"method": "PUT", "url": "Observation/obs-social-1"}
			},
			{
				"resource": {
					"resourceType": "Observation",
					"id": "obs-pat2-vital",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
					"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
					"subject": {"reference": "Patient/hard-pat-2"}
				},
				"request": {"method": "PUT", "url": "Observation/obs-pat2-vital"}
			}
		]
	}`
	respSeed, err := http.Post(ts.URL+"/fhir/r4/"+tenant, "application/fhir+json", strings.NewReader(seedBundle))
	if err != nil || respSeed.StatusCode != http.StatusOK {
		t.Fatalf("seed bundle failed: %v", err)
	}
	respSeed.Body.Close()

	// Issue a patient-scoped token for hard-pat-1 with granular Observation.rs?category=vital-signs scope via OAuth2 Authorize + Token
	noRedirect := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	authURL := ts.URL + "/oauth2/authorize?response_type=code&client_id=test-app&redirect_uri=https://app.example/cb&patient=hard-pat-1&scope=patient/Patient.rs+patient/Observation.ru%3Fcategory%3Dvital-signs+patient/Observation.rs%3Fcategory%3Dvital-signs+offline_access"
	respAuth, err := noRedirect.Get(authURL)
	if err != nil || respAuth.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 from /oauth2/authorize, got %v / %d", err, respAuth.StatusCode)
	}
	respAuth.Body.Close()
	loc := respAuth.Header.Get("Location")
	codeIdx := strings.Index(loc, "code=")
	if codeIdx == -1 {
		t.Fatalf("missing code in redirect Location: %s", loc)
	}
	authCode := loc[codeIdx+5:]
	if amp := strings.IndexByte(authCode, '&'); amp != -1 {
		authCode = authCode[:amp]
	}

	respTok, err := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=authorization_code&code="+authCode+"&redirect_uri=https://app.example/cb"))
	if err != nil || respTok.StatusCode != http.StatusOK {
		t.Fatalf("token exchange failed: %v", err)
	}
	var tokMap map[string]any
	_ = json.NewDecoder(respTok.Body).Decode(&tokMap)
	respTok.Body.Close()
	patToken, _ := tokMap["access_token"].(string)
	refreshToken, _ := tokMap["refresh_token"].(string)
	if patToken == "" || refreshToken == "" {
		t.Fatalf("expected access_token and refresh_token, got %+v", tokMap)
	}

	doAuthed := func(method, path, body string) (*http.Response, []byte) {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rdr)
		req.Header.Set("Authorization", "Bearer "+patToken)
		if body != "" {
			req.Header.Set("Content-Type", "application/fhir+json")
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		return r, b
	}

	// 1. Critical Issue 1: Granular scope enforcement on vread, _history, _revinclude, $everything, and $export
	if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenant+"/Observation/obs-vital-1/_history/1", ""); r.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK on vread of vital-signs Observation, got %d", r.StatusCode)
	}
	if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenant+"/Observation/obs-social-1/_history/1", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on vread of social-history Observation with vital-signs scope, got %d", r.StatusCode)
	}
	if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenant+"/Observation/obs-social-1/_history", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on _history of social-history Observation with vital-signs scope, got %d", r.StatusCode)
	}

	// _revinclude must strip obs-social-1 and keep obs-vital-1
	if r, b := doAuthed(http.MethodGet, "/fhir/r4/"+tenant+"/Patient?_id=hard-pat-1&_revinclude=Observation:patient", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on Patient _revinclude, got %d: %s", r.StatusCode, b)
	} else if strings.Contains(string(b), "obs-social-1") || !strings.Contains(string(b), "obs-vital-1") {
		t.Errorf("expected _revinclude to include obs-vital-1 and exclude obs-social-1, got: %s", b)
	}

	// Patient/$everything must strip obs-social-1 and keep obs-vital-1
	if r, b := doAuthed(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/hard-pat-1/$everything", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on $everything, got %d: %s", r.StatusCode, b)
	} else if strings.Contains(string(b), "obs-social-1") || !strings.Contains(string(b), "obs-vital-1") {
		t.Errorf("expected $everything to include obs-vital-1 and exclude obs-social-1, got: %s", b)
	}

	// Patient/{id}/$export must strip obs-social-1 and keep obs-vital-1
	if r, b := doAuthed(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/hard-pat-1/$export?inline=true", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on inline Patient/$export, got %d: %s", r.StatusCode, b)
	} else if strings.Contains(string(b), "obs-social-1") || !strings.Contains(string(b), "obs-vital-1") {
		t.Errorf("expected Patient/$export to include obs-vital-1 and exclude obs-social-1, got: %s", b)
	}

	// 2. Critical Issue 2: Bundle PATCH compartment & granular scope check + Transaction Bundle conditional PUT/DELETE rollback
	crossPatchBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"resource": [{"op": "replace", "path": "/status", "value": "amended"}],
				"request": {"method": "PATCH", "url": "Observation/obs-pat2-vital"}
			}
		]
	}`
	if r, _ := doAuthed(http.MethodPost, "/fhir/r4/"+tenant, crossPatchBundle); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden when patient-1 patches patient-2's Observation in Bundle, got %d", r.StatusCode)
	}

	outOfScopePatchBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"resource": [{"op": "replace", "path": "/status", "value": "amended"}],
				"request": {"method": "PATCH", "url": "Observation/obs-social-1"}
			}
		]
	}`
	if r, _ := doAuthed(http.MethodPost, "/fhir/r4/"+tenant, outOfScopePatchBundle); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden when patching social-history Observation in Bundle with vital-signs scope, got %d", r.StatusCode)
	}

	// Transaction Bundle where entry[0] is a conditional DELETE and entry[1] fails with 412 If-Match conflict:
	// verify entry[0]'s conditional DELETE is rolled back!
	rollbackCondTx := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"request": {"method": "DELETE", "url": "Observation?_id=obs-vital-1"}
			},
			{
				"resource": {"resourceType": "Patient", "id": "hard-pat-1", "name": [{"family": "Conflict"}]},
				"request": {"method": "PUT", "url": "Patient/hard-pat-1", "ifMatch": "W/\"999\""}
			}
		]
	}`
	respRb, err := http.Post(ts.URL+"/fhir/r4/"+tenant, "application/fhir+json", strings.NewReader(rollbackCondTx))
	if err != nil || respRb.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 Precondition Failed on rollbackCondTx, got %v / %d", err, respRb.StatusCode)
	}
	respRb.Body.Close()

	// Verify obs-vital-1 was NOT deleted (200 OK, not 410 Gone) because conditional DELETE ran inside the rolled-back SQL transaction!
	if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenant+"/Observation/obs-vital-1", ""); r.StatusCode != http.StatusOK {
		t.Errorf("expected obs-vital-1 to remain active (200 OK) after transaction rollback of conditional DELETE, got %d", r.StatusCode)
	}

	// 3. Critical Issue 3: Refresh token validation, rotation, and scope escalation prevention
	// Forged refresh token -> 400 invalid_grant
	respBadRT, _ := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=refresh_token&refresh_token=forged-token"))
	respBadRT.Body.Close()
	if respBadRT.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for forged refresh_token, got %d", respBadRT.StatusCode)
	}

	// Valid refresh token with unauthorized scope escalation (system/*.cruds) -> 400 invalid_scope
	respEscRT, _ := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=refresh_token&refresh_token="+refreshToken+"&scope=system/*.cruds"))
	respEscRT.Body.Close()
	if respEscRT.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request on refresh_token scope escalation, got %d", respEscRT.StatusCode)
	}

	// Valid refresh token with valid rotated exchange -> issue new refresh_token and test valid refresh
	respAuth2, _ := noRedirect.Get(authURL)
	respAuth2.Body.Close()
	loc2 := respAuth2.Header.Get("Location")
	authCode2 := loc2[strings.Index(loc2, "code=")+5:]
	if amp := strings.IndexByte(authCode2, '&'); amp != -1 {
		authCode2 = authCode2[:amp]
	}
	respTok2, _ := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=authorization_code&code="+authCode2+"&redirect_uri=https://app.example/cb"))
	var tokMap2 map[string]any
	_ = json.NewDecoder(respTok2.Body).Decode(&tokMap2)
	respTok2.Body.Close()
	validRT, _ := tokMap2["refresh_token"].(string)

	respRefreshOK, err := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=refresh_token&refresh_token="+validRT+"&scope=patient/Patient.rs"))
	if err != nil || respRefreshOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on valid refresh_token exchange, got %v / %d", err, respRefreshOK.StatusCode)
	}
	var refreshedMap map[string]any
	_ = json.NewDecoder(respRefreshOK.Body).Decode(&refreshedMap)
	respRefreshOK.Body.Close()
	if refreshedMap["patient"] != "hard-pat-1" || refreshedMap["scope"] != "patient/Patient.rs" {
		t.Errorf("expected refreshed token bound to hard-pat-1 and downscoped to patient/Patient.rs, got %+v", refreshedMap)
	}

	// Replaying the consumed refresh_token must fail with 400 invalid_grant
	respReplayRT, _ := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=refresh_token&refresh_token="+validRT))
	respReplayRT.Body.Close()
	if respReplayRT.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request when replaying rotated refresh_token, got %d", respReplayRT.StatusCode)
	}

	// 4. Critical Issue 4: Date interval precision on REST GET /Observation?date=2026-01-15
	respDatePrec, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Observation?date=2026-01-15")
	if err != nil || respDatePrec.StatusCode != http.StatusOK {
		t.Fatalf("GET /Observation?date=2026-01-15 failed: %v", err)
	}
	var dateBundle map[string]any
	_ = json.NewDecoder(respDatePrec.Body).Decode(&dateBundle)
	respDatePrec.Body.Close()
	if tot, _ := dateBundle["total"].(float64); tot != 2 {
		t.Errorf("expected 2 Observations matching date=2026-01-15 (09:15Z and 14:30Z), got %v", dateBundle["total"])
	}
}

