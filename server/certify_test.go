package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	// 5. Track 1: _sort, _has, _summary (true/data/text/count), _elements, _total=none, Bundle.link (self/next/previous),
	// Compartment Search GET /Patient/{id}/Observation, and Type/System _history
	// 5a. _has with insufficient scope -> 403 Forbidden (downscoped refreshed token only has patient/Patient.rs, missing Observation scope)
	patOnlyToken, _ := refreshedMap["access_token"].(string)
	reqHasForbidden, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/Patient?_has:Observation:patient:code=8867-4", nil)
	reqHasForbidden.Header.Set("Authorization", "Bearer "+patOnlyToken)
	respHasForbidden, err := http.DefaultClient.Do(reqHasForbidden)
	if err != nil {
		t.Fatalf("GET _has with patOnlyToken: %v", err)
	}
	respHasForbidden.Body.Close()
	if respHasForbidden.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on _has:Observation without Observation scope, got %d", respHasForbidden.StatusCode)
	}

	// 5b. Compartment search GET /Patient/hard-pat-2/Observation with token restricted to hard-pat-1 -> 403 Forbidden
	if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/hard-pat-2/Observation", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on cross-patient compartment search /Patient/hard-pat-2/Observation, got %d", r.StatusCode)
	}

	// 5c. Compartment search GET /Patient/hard-pat-1/Observation with pagination (_count=1&_offset=1) -> Bundle.link has self and previous
	respPage2, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Patient/hard-pat-1/Observation?_sort=-date&_count=1&_offset=1&_summary=true")
	if err != nil || respPage2.StatusCode != http.StatusOK {
		t.Fatalf("GET /Patient/hard-pat-1/Observation page 2 failed: %v", err)
	}
	bPage2, _ := io.ReadAll(respPage2.Body)
	respPage2.Body.Close()
	if !strings.Contains(string(bPage2), `"previous"`) || !strings.Contains(string(bPage2), `"SUBSETTED"`) {
		t.Errorf("expected previous link and SUBSETTED tag on page 2 summary response, got %s", string(bPage2))
	}

	// 5d. Read with _summary=data, _summary=text, and _elements=gender,name
	for _, qSuffix := range []string{"?_summary=data", "?_summary=text", "?_elements=gender,name"} {
		respR, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Patient/hard-pat-1" + qSuffix)
		if err != nil || respR.StatusCode != http.StatusOK {
			t.Fatalf("GET /Patient/hard-pat-1%s failed: %v", qSuffix, err)
		}
		bR, _ := io.ReadAll(respR.Body)
		respR.Body.Close()
		if !strings.Contains(string(bR), `"SUBSETTED"`) {
			t.Errorf("expected SUBSETTED tag on GET /Patient/hard-pat-1%s, got %s", qSuffix, string(bR))
		}
	}

	// 5e. System-level _history GET /fhir/r4/{tenant}/_history?_since=2020-01-01
	respSysHist, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/_history?_since=2020-01-01&_count=5")
	if err != nil || respSysHist.StatusCode != http.StatusOK {
		t.Fatalf("GET /_history failed: %v", err)
	}
	bSysHist, _ := io.ReadAll(respSysHist.Body)
	respSysHist.Body.Close()
	if !strings.Contains(string(bSysHist), `"history"`) {
		t.Errorf("expected history Bundle from GET /_history, got %s", string(bSysHist))
	}

	// 6. Track 2: Terminology Services (stored CodeSystem & ValueSet + POST Parameters $expand / $validate-code / $lookup)
	// and Subscription rest-hook real-time HTTP webhook delivery
	customCS := `{
		"resourceType": "CodeSystem",
		"id": "cs-custom-oncology",
		"url": "http://flint.health/fhir/CodeSystem/oncology-stage",
		"name": "FlintOncologyStage",
		"status": "active",
		"content": "complete",
		"concept": [
			{"code": "STAGE-I", "display": "Stage I Carcinoma"},
			{"code": "STAGE-II", "display": "Stage II Carcinoma"}
		]
	}`
	respCS, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/CodeSystem", "application/fhir+json", strings.NewReader(customCS))
	if err != nil || respCS.StatusCode != http.StatusCreated {
		t.Fatalf("POST /CodeSystem failed: %v / %d", err, respCS.StatusCode)
	}
	respCS.Body.Close()

	customVS := `{
		"resourceType": "ValueSet",
		"id": "vs-custom-oncology",
		"url": "http://flint.health/fhir/ValueSet/oncology-stage",
		"name": "FlintOncologyStageVS",
		"status": "active",
		"compose": {
			"include": [
				{"system": "http://flint.health/fhir/CodeSystem/oncology-stage"}
			]
		}
	}`
	respVS, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/ValueSet", "application/fhir+json", strings.NewReader(customVS))
	if err != nil || respVS.StatusCode != http.StatusCreated {
		t.Fatalf("POST /ValueSet failed: %v / %d", err, respVS.StatusCode)
	}
	respVS.Body.Close()

	// Instance-level POST /ValueSet/vs-custom-oncology/$expand with Parameters body
	expandParams := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "filter", "valueString": "Stage II"}
		]
	}`
	respExpCustom, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/ValueSet/vs-custom-oncology/$expand", "application/fhir+json", strings.NewReader(expandParams))
	if err != nil || respExpCustom.StatusCode != http.StatusOK {
		t.Fatalf("POST /ValueSet/vs-custom-oncology/$expand failed: %v / %d", err, respExpCustom.StatusCode)
	}
	bExpCustom, _ := io.ReadAll(respExpCustom.Body)
	respExpCustom.Body.Close()
	if !strings.Contains(string(bExpCustom), `"STAGE-II"`) || strings.Contains(string(bExpCustom), `"STAGE-I"`) {
		t.Errorf("expected filtered expansion to contain only STAGE-II, got %s", string(bExpCustom))
	}

	// Instance-level POST /ValueSet/vs-custom-oncology/$validate-code with coding Parameter
	vcParams := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "coding", "valueCoding": {"system": "http://flint.health/fhir/CodeSystem/oncology-stage", "code": "STAGE-I"}}
		]
	}`
	respVCCustom, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/ValueSet/vs-custom-oncology/$validate-code", "application/fhir+json", strings.NewReader(vcParams))
	if err != nil || respVCCustom.StatusCode != http.StatusOK {
		t.Fatalf("POST /ValueSet/vs-custom-oncology/$validate-code failed: %v / %d", err, respVCCustom.StatusCode)
	}
	bVCCustom, _ := io.ReadAll(respVCCustom.Body)
	respVCCustom.Body.Close()
	if !strings.Contains(string(bVCCustom), `"valueBoolean":true`) || !strings.Contains(string(bVCCustom), `"Stage I Carcinoma"`) {
		t.Errorf("expected valueBoolean:true and Stage I Carcinoma, got %s", string(bVCCustom))
	}

	// Instance-level GET /CodeSystem/cs-custom-oncology/$lookup?code=STAGE-II
	respLkpCustom, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/CodeSystem/cs-custom-oncology/$lookup?code=STAGE-II")
	if err != nil || respLkpCustom.StatusCode != http.StatusOK {
		t.Fatalf("GET /CodeSystem/cs-custom-oncology/$lookup failed: %v / %d", err, respLkpCustom.StatusCode)
	}
	bLkpCustom, _ := io.ReadAll(respLkpCustom.Body)
	respLkpCustom.Body.Close()
	if !strings.Contains(string(bLkpCustom), `"Stage II Carcinoma"`) {
		t.Errorf("expected Stage II Carcinoma from stored CodeSystem/$lookup, got %s", string(bLkpCustom))
	}

	// Subscription rest-hook webhook delivery test
	webhookCh := make(chan string, 2)
	webhookHeaderCh := make(chan string, 2)
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wb, _ := io.ReadAll(r.Body)
		webhookHeaderCh <- r.Header.Get("X-Flint-Webhook-Secret")
		webhookCh <- string(wb)
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	subCreate := fmt.Sprintf(`{
		"resourceType": "Subscription",
		"id": "sub-webhook-1",
		"status": "requested",
		"reason": "Notify webhook on new heart rate Observation",
		"criteria": "Observation?code=8867-4&patient=Patient/hard-pat-1",
		"channel": {
			"type": "rest-hook",
			"endpoint": %q,
			"payload": "application/fhir+json",
			"header": ["X-Flint-Webhook-Secret: s3cr3t-token"]
		}
	}`, webhookSrv.URL)
	respSub, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/Subscription", "application/fhir+json", strings.NewReader(subCreate))
	if err != nil || respSub.StatusCode != http.StatusCreated {
		t.Fatalf("POST /Subscription failed: %v / %d", err, respSub.StatusCode)
	}
	respSub.Body.Close()

	// Create matching Observation -> should trigger webhook delivery
	matchingObs := `{
		"resourceType": "Observation",
		"id": "obs-sub-trigger-1",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
		"subject": {"reference": "Patient/hard-pat-1"}
	}`
	respTrig, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/Observation", "application/fhir+json", strings.NewReader(matchingObs))
	if err != nil || respTrig.StatusCode != http.StatusCreated {
		t.Fatalf("POST matching Observation failed: %v / %d", err, respTrig.StatusCode)
	}
	respTrig.Body.Close()

	select {
	case deliveredBody := <-webhookCh:
		gotSecret := <-webhookHeaderCh
		if !strings.Contains(deliveredBody, `"obs-sub-trigger-1"`) || gotSecret != "s3cr3t-token" {
			t.Errorf("unexpected webhook payload or header: secret=%q body=%s", gotSecret, deliveredBody)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("timed out waiting for Subscription rest-hook webhook notification")
	}

	// 7. Track 3: Operational Probes (/healthz, /readyz, /metrics) & OpenAPI 3.1.0 Spec Synchronization
	for _, probeURL := range []string{
		ts.URL + "/healthz",
		ts.URL + "/readyz",
		ts.URL + "/fhir/r4/" + tenant + "/healthz",
		ts.URL + "/fhir/r4/" + tenant + "/readyz",
	} {
		respProbe, err := http.Get(probeURL)
		if err != nil || respProbe.StatusCode != http.StatusOK {
			t.Fatalf("GET %s failed: %v / %d", probeURL, err, respProbe.StatusCode)
		}
		respProbe.Body.Close()
	}

	respMetrics, err := http.Get(ts.URL + "/metrics")
	if err != nil || respMetrics.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics failed: %v / %d", err, respMetrics.StatusCode)
	}
	bMetrics, _ := io.ReadAll(respMetrics.Body)
	respMetrics.Body.Close()
	metricsText := string(bMetrics)
	for _, wantMetric := range []string{
		"flint_up 1",
		"flint_registered_resource_types ",
		"flint_http_requests_total{",
		"flint_subscriptions_delivered_total 1",
	} {
		if !strings.Contains(metricsText, wantMetric) {
			t.Errorf("expected /metrics to contain %q, got:\n%s", wantMetric, metricsText)
		}
	}

	respOpenAPI, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/openapi.json")
	if err != nil || respOpenAPI.StatusCode != http.StatusOK {
		t.Fatalf("GET /openapi.json failed: %v / %d", err, respOpenAPI.StatusCode)
	}
	var openAPISpec map[string]any
	if err := json.NewDecoder(respOpenAPI.Body).Decode(&openAPISpec); err != nil {
		t.Fatalf("decode openapi.json: %v", err)
	}
	respOpenAPI.Body.Close()

	conformanceEntries, _ := openAPISpec["x-flint-conformance"].([]any)
	if len(conformanceEntries) != 12 {
		t.Errorf("expected 12 x-flint-conformance entries in OpenAPI 3.1.0 spec, got %d", len(conformanceEntries))
	}
	specPaths, _ := openAPISpec["paths"].(map[string]any)
	for _, requiredPath := range []string{
		"/healthz",
		"/readyz",
		"/metrics",
		"/_history",
		"/$meta",
		"/$certify",
		"/Measure/$evaluate-measure",
		"/Measure/{id}/$evaluate-measure",
		"/cds-services",
		"/cds-services/{serviceId}",
		"/Patient/$match",
		"/Patient/$meta",
		"/Patient/{id}/$meta",
		"/Patient/{id}/$meta-add",
		"/Patient/{id}/$meta-delete",
		"/ValueSet/$expand",
		"/ValueSet/{id}/$expand",
		"/ValueSet/$validate-code",
		"/CodeSystem/$lookup",
		"/Patient/{id}/Observation",
		"/Observation/_search",
		"/Observation/_history",
	} {
		if _, ok := specPaths[requiredPath]; !ok {
			t.Errorf("expected OpenAPI 3.1.0 spec paths to include %q", requiredPath)
		}
	}

	// 8. Track 4: System/Type $meta & private_key_jwt negative validation checks
	for _, metaURL := range []string{
		ts.URL + "/fhir/r4/" + tenant + "/$meta",
		ts.URL + "/fhir/r4/" + tenant + "/Patient/$meta",
		ts.URL + "/fhir/r4/" + tenant + "/Patient/hard-pat-1/$meta",
	} {
		respMeta, err := http.Get(metaURL)
		if err != nil || respMeta.StatusCode != http.StatusOK {
			t.Fatalf("GET %s failed: %v / %d", metaURL, err, respMeta.StatusCode)
		}
		respMeta.Body.Close()
	}

	// Reject malformed client_assertion_type or invalid client_assertion JWT on /oauth2/token
	respBadAssert, err := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=client_credentials&client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer&client_assertion=not.a.jwt"))
	if err != nil {
		t.Fatalf("POST /oauth2/token invalid client_assertion: %v", err)
	}
	respBadAssert.Body.Close()
	if respBadAssert.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for malformed client_assertion, got %d", respBadAssert.StatusCode)
	}

	// 9. Track 5: Clinical Quality Measures (Measure/$evaluate-measure) & CDS Hooks v2.0 (patient-view care gap)
	cqmParams := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "measure", "valueUri": "http://hl7.org/fhir/us/cqfmeasures/Measure/CMS122"},
			{"name": "periodStart", "valueDate": "2025-01-01"},
			{"name": "periodEnd", "valueDate": "2027-01-01"},
			{"name": "reportType", "valueCode": "summary"}
		]
	}`
	respCQM, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/Measure/$evaluate-measure", "application/fhir+json", strings.NewReader(cqmParams))
	if err != nil || respCQM.StatusCode != http.StatusOK {
		t.Fatalf("POST /Measure/$evaluate-measure failed: %v / %d", err, respCQM.StatusCode)
	}
	var cqmReport map[string]any
	_ = json.NewDecoder(respCQM.Body).Decode(&cqmReport)
	respCQM.Body.Close()
	if cqmReport["resourceType"] != "MeasureReport" || cqmReport["type"] != "summary" {
		t.Errorf("expected summary MeasureReport, got %v", cqmReport)
	}

	// CDS Hooks patient-view care gap on patient with hypertension in inline prefetch
	cdsViewReq := `{
		"hook": "patient-view",
		"hookInstance": "test-htn-view-1",
		"fhirServer": "` + ts.URL + `/fhir/r4/` + tenant + `",
		"context": {"userId": "Practitioner/prac-1", "patientId": "hard-pat-1"},
		"prefetch": {
			"conditions": {
				"resourceType": "Bundle",
				"type": "searchset",
				"entry": [
					{
						"resource": {
							"resourceType": "Condition",
							"id": "cond-htn-inline",
							"code": {"coding": [{"system": "http://snomed.info/sct", "code": "59621000", "display": "Essential hypertension"}]},
							"subject": {"reference": "Patient/hard-pat-1"}
						}
					}
				]
			}
		}
	}`
	respCDSView, err := http.Post(ts.URL+"/cds-services/flint-hypertension-care-gap", "application/json", strings.NewReader(cdsViewReq))
	if err != nil || respCDSView.StatusCode != http.StatusOK {
		t.Fatalf("POST /cds-services/flint-hypertension-care-gap failed: %v / %d", err, respCDSView.StatusCode)
	}
	var cdsViewOut map[string]any
	_ = json.NewDecoder(respCDSView.Body).Decode(&cdsViewOut)
	respCDSView.Body.Close()
	viewCards, _ := cdsViewOut["cards"].([]any)
	if len(viewCards) != 1 {
		t.Errorf("expected 1 warning card from flint-hypertension-care-gap, got %d", len(viewCards))
	}
}
