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
		_ = postgres.PurgeTenant(ctx, db, tenant)
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
	if rep.TotalChecks != 87 {
		t.Errorf("expected 87 certification checks, got %d", rep.TotalChecks)
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
	if len(conformanceEntries) < 12 {
		t.Errorf("expected >= 12 x-flint-conformance entries in OpenAPI 3.1.0 spec, got %d", len(conformanceEntries))
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
		"/Patient/$member-match",
		"/Patient/{id}/$summary",
		"/Composition/{id}/$document",
		"/DocumentReference/$docref",
		"/Questionnaire/$populate",
		"/Questionnaire/{id}/$populate",
		"/QuestionnaireResponse/$extract",
		"/QuestionnaireResponse/{id}/$extract",
		"/Claim/$submit",
		"/Patient/$meta",
		"/Patient/{id}/$meta",
		"/Patient/{id}/$meta-add",
		"/Patient/{id}/$meta-delete",
		"/Patient/{id}/$provenance",
		"/ValueSet/$expand",
		"/ValueSet/{id}/$expand",
		"/ValueSet/$validate-code",
		"/CodeSystem/$lookup",
		"/ConceptMap/$translate",
		"/ConceptMap/{id}/$translate",
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

	// 10. Track 6: IPS Patient/{id}/$summary, Da Vinci $member-match, Claim/$submit, _list Cohort, & Forward Chaining
	// Cross-patient $summary with token restricted to hard-pat-1 -> 403 Forbidden
	if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/hard-pat-2/$summary", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on cross-patient /Patient/hard-pat-2/$summary, got %d", r.StatusCode)
	}
	// Own-patient $summary with granular Vital Signs scope -> 200 OK, strips social-history obs-social-1 and keeps obs-vital-1
	if r, b := doAuthed(http.MethodGet, "/fhir/r4/"+tenant+"/Patient/hard-pat-1/$summary", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /Patient/hard-pat-1/$summary, got %d: %s", r.StatusCode, b)
	} else if strings.Contains(string(b), "obs-social-1") || !strings.Contains(string(b), "obs-vital-1") {
		t.Errorf("expected IPS $summary to include obs-vital-1 and exclude obs-social-1 under granular scope, got: %s", b)
	}

	// Da Vinci $member-match negative case (no matching patient) -> 422 Unprocessable Entity
	mmNoMatch := `{
		"resourceType": "Parameters",
		"parameter": [
			{
				"name": "MemberPatient",
				"resource": {"resourceType": "Patient", "name": [{"family": "NonExistentMember999"}]}
			},
			{
				"name": "CoverageToMatch",
				"resource": {"resourceType": "Coverage", "status": "active", "subscriberId": "NO-SUCH-SUB"}
			}
		]
	}`
	respMMNo, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(mmNoMatch))
	if err != nil {
		t.Fatalf("POST /Patient/$member-match no-match: %v", err)
	}
	respMMNo.Body.Close()
	if respMMNo.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity on unmatched $member-match, got %d", respMMNo.StatusCode)
	}

	// Da Vinci PAS Claim/$submit with Bundle payload -> 200 OK ClaimResponse
	pasBundle := `{
		"resourceType": "Bundle",
		"type": "collection",
		"entry": [
			{
				"resource": {
					"resourceType": "Claim",
					"id": "hard-claim-pas-1",
					"status": "active",
					"type": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "institutional"}]},
					"use": "preauthorization",
					"patient": {"reference": "Patient/hard-pat-1"},
					"created": "2026-01-15",
					"provider": {"reference": "Organization/bnd-org-1"},
					"priority": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/processpriority", "code": "normal"}]},
					"insurance": [{"sequence": 1, "focal": true, "coverage": {"reference": "Coverage/cov-1"}}],
					"item": [{"sequence": 1, "productOrService": {"coding": [{"system": "http://www.ama-assn.org/go/cpt", "code": "99213"}]}, "net": {"value": 150.0, "currency": "USD"}}]
				}
			}
		]
	}`
	respPAS, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/Claim/$submit", "application/fhir+json", strings.NewReader(pasBundle))
	if err != nil || respPAS.StatusCode != http.StatusOK {
		t.Fatalf("POST /Claim/$submit Bundle failed: %v / %d", err, respPAS.StatusCode)
	}
	bPAS, _ := io.ReadAll(respPAS.Body)
	respPAS.Body.Close()
	if !strings.Contains(string(bPAS), `"ClaimResponse"`) || !strings.Contains(string(bPAS), `"PA-HARD-CLAIM-PAS-1"`) {
		t.Errorf("expected ClaimResponse with preAuthRef PA-HARD-CLAIM-PAS-1, got %s", string(bPAS))
	}

	// 11. Track 7: ConceptMap/$translate, Paired Composite Search, Questionnaire/$populate, QuestionnaireResponse/$extract, & Composition/$document
	customCM := `{
		"resourceType": "ConceptMap",
		"id": "cm-custom-1",
		"url": "http://flint.health/fhir/ConceptMap/local-to-loinc",
		"status": "active",
		"group": [
			{
				"source": "http://flint.health/fhir/CodeSystem/local-labs",
				"target": "http://loinc.org",
				"element": [
					{
						"code": "LOC-A1C",
						"display": "Local Hemoglobin A1c",
						"target": [
							{"code": "4548-4", "display": "Hemoglobin A1c/Hemoglobin.total in Blood", "equivalence": "equivalent"}
						]
					}
				]
			}
		]
	}`
	respCMCreate, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/ConceptMap", "application/fhir+json", strings.NewReader(customCM))
	if err != nil || respCMCreate.StatusCode != http.StatusCreated {
		t.Fatalf("POST /ConceptMap failed: %v / %d", err, respCMCreate.StatusCode)
	}
	respCMCreate.Body.Close()

	// Translate via stored ConceptMap/{id}/$translate POST Parameters
	trParams := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "system", "valueUri": "http://flint.health/fhir/CodeSystem/local-labs"},
			{"name": "code", "valueCode": "LOC-A1C"}
		]
	}`
	respTrCustom, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/ConceptMap/cm-custom-1/$translate", "application/fhir+json", strings.NewReader(trParams))
	if err != nil || respTrCustom.StatusCode != http.StatusOK {
		t.Fatalf("POST /ConceptMap/cm-custom-1/$translate failed: %v / %d", err, respTrCustom.StatusCode)
	}
	bTrCustom, _ := io.ReadAll(respTrCustom.Body)
	respTrCustom.Body.Close()
	if !strings.Contains(string(bTrCustom), `"4548-4"`) || !strings.Contains(string(bTrCustom), `"valueBoolean":true`) {
		t.Errorf("expected translated LOINC 4548-4 from stored ConceptMap, got %s", string(bTrCustom))
	}

	// Paired component-code-value-quantity test: Systolic (8480-6) = 125 mmHg, Diastolic (8462-4) = 82 mmHg.
	// Querying systolic <= 85 MUST NOT match (even though diastolic is 82 <= 85), proving per-component pairing!
	bpObs := `{
		"resourceType": "Observation",
		"id": "obs-bp-paired-1",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
		"subject": {"reference": "Patient/hard-pat-1"},
		"component": [
			{
				"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
				"valueQuantity": {"value": 125.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
			},
			{
				"code": {"coding": [{"system": "http://loinc.org", "code": "8462-4"}]},
				"valueQuantity": {"value": 82.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
			}
		]
	}`
	respBPCreate, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/Observation", "application/fhir+json", strings.NewReader(bpObs))
	if err != nil || respBPCreate.StatusCode != http.StatusCreated {
		t.Fatalf("POST BP Observation failed: %v / %d", err, respBPCreate.StatusCode)
	}
	respBPCreate.Body.Close()

	respBPHit, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Observation?component-code-value-quantity=http://loinc.org|8480-6$le130")
	if err != nil || respBPHit.StatusCode != http.StatusOK {
		t.Fatalf("GET component-code-value-quantity hit failed: %v", err)
	}
	bBPHit, _ := io.ReadAll(respBPHit.Body)
	respBPHit.Body.Close()
	if !strings.Contains(string(bBPHit), `"obs-bp-paired-1"`) {
		t.Errorf("expected obs-bp-paired-1 to match systolic <= 130, got %s", string(bBPHit))
	}

	respBPMiss, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Observation?component-code-value-quantity=http://loinc.org|8480-6$le85")
	if err != nil || respBPMiss.StatusCode != http.StatusOK {
		t.Fatalf("GET component-code-value-quantity miss failed: %v", err)
	}
	bBPMiss, _ := io.ReadAll(respBPMiss.Body)
	respBPMiss.Body.Close()
	if strings.Contains(string(bBPMiss), `"obs-bp-paired-1"`) {
		t.Errorf("expected obs-bp-paired-1 NOT to match systolic <= 85 (no cross-component false positive), got %s", string(bBPMiss))
	}

	// 12. Track 8: Adversarial Security & Compartment Isolation for $document, $extract, Claim/$submit, and $member-match
	// Issue a patient-scoped token for hard-pat-1 that includes Composition, Questionnaire, QuestionnaireResponse, Claim, Observation, and Patient scopes
	authURLSec := fmt.Sprintf("%s/oauth2/authorize?response_type=code&client_id=test-app&redirect_uri=https://app.example/cb&scope=%s&state=sec1&patient=hard-pat-1",
		ts.URL,
		"patient/Patient.rs+patient/Observation.crus+patient/Composition.crus+patient/Questionnaire.rs+patient/QuestionnaireResponse.crus+patient/Claim.crus",
	)
	respAuthSec, err := noRedirect.Get(authURLSec)
	if err != nil || respAuthSec.StatusCode != http.StatusFound {
		t.Fatalf("authorize sec token failed: %v", err)
	}
	codeSec := respAuthSec.Header.Get("Location")
	respAuthSec.Body.Close()
	if idx := strings.Index(codeSec, "code="); idx != -1 {
		codeSec = strings.Split(codeSec[idx+5:], "&")[0]
	}
	respTokSec, err := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
		strings.NewReader(fmt.Sprintf("grant_type=authorization_code&code=%s&redirect_uri=https://app.example/cb", codeSec)))
	if err != nil || respTokSec.StatusCode != http.StatusOK {
		t.Fatalf("token exchange sec failed: %v", err)
	}
	var tokSecMap map[string]any
	_ = json.NewDecoder(respTokSec.Body).Decode(&tokSecMap)
	respTokSec.Body.Close()
	secToken, _ := tokSecMap["access_token"].(string)

	doSec := func(method, path, body string) (*http.Response, []byte) {
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+secToken)
		if body != "" {
			req.Header.Set("Content-Type", "application/fhir+json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("doSec %s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	// (a) Cross-patient Composition.section.entry exfiltration attempt:
	// Composition subject is hard-pat-1, but section.entry references obs-pat2-vital (belonging to hard-pat-2).
	// Calling $document with hard-pat-1's token MUST return 403 Forbidden!
	evilComp := `{
		"resourceType": "Composition",
		"id": "comp-cross-pat-evil",
		"status": "final",
		"type": {"coding": [{"system": "http://loinc.org", "code": "11506-3"}]},
		"subject": {"reference": "Patient/hard-pat-1"},
		"date": "2026-01-15T10:35:00Z",
		"author": [{"reference": "Practitioner/prac-1"}],
		"title": "Cross-Patient Exfiltration Attempt",
		"section": [
			{
				"title": "Stolen Vitals",
				"entry": [{"reference": "Observation/obs-pat2-vital"}]
			}
		]
	}`
	if r, b := doSec(http.MethodPut, "/fhir/r4/"+tenant+"/Composition/comp-cross-pat-evil", evilComp); r.StatusCode != http.StatusCreated && r.StatusCode != http.StatusOK {
		t.Fatalf("PUT Composition/comp-cross-pat-evil failed: %d %s", r.StatusCode, b)
	}
	if r, _ := doSec(http.MethodGet, "/fhir/r4/"+tenant+"/Composition/comp-cross-pat-evil/$document", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden when Composition/$document references another patient's Observation, got %d", r.StatusCode)
	}

	// (b) QuestionnaireResponse/$extract?persist=true persists extracted Observation into Postgres
	qrExtractPersist := `{
		"resourceType": "QuestionnaireResponse",
		"id": "qr-sec-persist-1",
		"status": "completed",
		"subject": {"reference": "Patient/hard-pat-1"},
		"authored": "2026-01-15T11:00:00Z",
		"item": [
			{
				"linkId": "heart-rate",
				"text": "Heart rate",
				"answer": [{"valueQuantity": {"value": 68.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}}]
			}
		]
	}`
	if r, b := doSec(http.MethodPost, "/fhir/r4/"+tenant+"/QuestionnaireResponse/$extract?persist=true", qrExtractPersist); r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on QuestionnaireResponse/$extract?persist=true, got %d: %s", r.StatusCode, b)
	} else if !strings.Contains(string(b), `"prov-extract-qr-sec-persist-1"`) {
		t.Errorf("expected QuestionnaireResponse/$extract Bundle to include automatic Provenance resource prov-extract-qr-sec-persist-1, got: %s", b)
	}
	if r, b := doSec(http.MethodGet, "/fhir/r4/"+tenant+"/Observation/ext-obs-qr-sec-persist-1-1", ""); r.StatusCode != http.StatusOK || !strings.Contains(string(b), `"value":68`) {
		t.Errorf("expected persisted Observation/ext-obs-qr-sec-persist-1-1 to be readable after $extract?persist=true, got %d: %s", r.StatusCode, b)
	}

	// (c) Cross-patient Claim/$submit PAS Bundle attempting to overwrite Patient/hard-pat-2 -> 403 Forbidden
	evilPASBundle := `{
		"resourceType": "Bundle",
		"type": "collection",
		"entry": [
			{
				"resource": {
					"resourceType": "Claim",
					"id": "claim-evil-1",
					"status": "active",
					"type": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "institutional"}]},
					"use": "preauthorization",
					"patient": {"reference": "Patient/hard-pat-1"},
					"created": "2026-01-15",
					"provider": {"reference": "Organization/bnd-org-1"},
					"priority": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/processpriority", "code": "normal"}]},
					"insurance": [{"sequence": 1, "focal": true, "coverage": {"reference": "Coverage/cov-1"}}]
				}
			},
			{
				"resource": {
					"resourceType": "Patient",
					"id": "hard-pat-2",
					"name": [{"family": "HijackedVictim"}]
				}
			}
		]
	}`
	if r, _ := doSec(http.MethodPost, "/fhir/r4/"+tenant+"/Claim/$submit", evilPASBundle); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden when PAS Bundle attempts to overwrite Patient/hard-pat-2, got %d", r.StatusCode)
	}

	// (d) Patient/$member-match with patient-restricted token for hard-pat-1 attempting to match hard-pat-2 -> 422 Unprocessable Entity (not found in compartment)
	mmOtherPat := `{
		"resourceType": "Parameters",
		"parameter": [
			{
				"name": "MemberPatient",
				"resource": {"resourceType": "Patient", "name": [{"family": "HardeningTwo"}]}
			},
			{
				"name": "CoverageToMatch",
				"resource": {"resourceType": "Coverage", "status": "active", "subscriberId": "ANY"}
			}
		]
	}`
	if r, _ := doSec(http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$member-match", mmOtherPat); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity when patient-scoped token for hard-pat-1 attempts $member-match on hard-pat-2, got %d", r.StatusCode)
	}

	// 13. Track 9: DocumentReference/$docref, X-Provenance Header & $provenance Lineage, and POST Patient/{id}/$everything
	// (a) Verify Observation/ext-obs-qr-sec-persist-1-1/$provenance returns the persisted Provenance from $extract?persist=true
	respExtProv, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/Observation/ext-obs-qr-sec-persist-1-1/$provenance")
	if err != nil || respExtProv.StatusCode != http.StatusOK {
		t.Fatalf("GET /Observation/ext-obs-qr-sec-persist-1-1/$provenance failed: %v / %d", err, respExtProv.StatusCode)
	}
	bExtProv, _ := io.ReadAll(respExtProv.Body)
	respExtProv.Body.Close()
	if !strings.Contains(string(bExtProv), `"prov-extract-qr-sec-persist-1"`) {
		t.Errorf("expected $provenance on extracted Observation to contain prov-extract-qr-sec-persist-1, got: %s", string(bExtProv))
	}

	// (b) Create a stored DocumentReference with X-Provenance: true and context.period
	docRefCreate := `{
		"resourceType": "DocumentReference",
		"id": "docref-hard-1",
		"status": "current",
		"type": {"coding": [{"system": "http://loinc.org", "code": "34133-9", "display": "Summary of episode note"}]},
		"subject": {"reference": "Patient/hard-pat-1"},
		"date": "2026-01-15T10:00:00Z",
		"description": "Initial Episode Summary",
		"content": [{"attachment": {"contentType": "text/plain", "title": "Episode Summary"}}],
		"context": {"period": {"start": "2026-01-10T00:00:00Z", "end": "2026-01-20T00:00:00Z"}}
	}`
	reqDocCreate, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/DocumentReference/docref-hard-1", strings.NewReader(docRefCreate))
	reqDocCreate.Header.Set("Content-Type", "application/fhir+json")
	reqDocCreate.Header.Set("X-Provenance", "true")
	respDocCreate, err := http.DefaultClient.Do(reqDocCreate)
	if err != nil || (respDocCreate.StatusCode != http.StatusCreated && respDocCreate.StatusCode != http.StatusOK) {
		t.Fatalf("PUT DocumentReference/docref-hard-1 with X-Provenance: true failed: %v / %d", err, respDocCreate.StatusCode)
	}
	respDocCreate.Body.Close()
	if provLoc := respDocCreate.Header.Get("X-Provenance-Location"); !strings.Contains(provLoc, "/Provenance/") {
		t.Errorf("expected X-Provenance-Location header containing /Provenance/, got %q", provLoc)
	}

	// Patch DocumentReference/docref-hard-1 with an inline X-Provenance JSON template -> records a second UPDATE Provenance
	reqDocPatch, _ := http.NewRequest(http.MethodPatch, ts.URL+"/fhir/r4/"+tenant+"/DocumentReference/docref-hard-1",
		strings.NewReader(`[{"op":"replace","path":"/description","value":"Patched Episode Summary"}]`))
	reqDocPatch.Header.Set("Content-Type", "application/json-patch+json")
	reqDocPatch.Header.Set("X-Provenance", `{"agent":[{"who":{"display":"Dr. Patch Lineage"}}]}`)
	respDocPatch, err := http.DefaultClient.Do(reqDocPatch)
	if err != nil || respDocPatch.StatusCode != http.StatusOK {
		t.Fatalf("PATCH DocumentReference/docref-hard-1 with X-Provenance failed: %v / %d", err, respDocPatch.StatusCode)
	}
	respDocPatch.Body.Close()

	// Verify GET /DocumentReference/docref-hard-1/$provenance returns 2 Provenance entries (v1 CREATE + v2 UPDATE)
	respDocProv, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/DocumentReference/docref-hard-1/$provenance")
	if err != nil || respDocProv.StatusCode != http.StatusOK {
		t.Fatalf("GET /DocumentReference/docref-hard-1/$provenance failed: %v / %d", err, respDocProv.StatusCode)
	}
	var docProvBundle map[string]any
	_ = json.NewDecoder(respDocProv.Body).Decode(&docProvBundle)
	respDocProv.Body.Close()
	if tot, _ := docProvBundle["total"].(float64); tot != 2 {
		t.Errorf("expected 2 Provenance records for DocumentReference/docref-hard-1, got %v", docProvBundle["total"])
	}

	// (c) DocumentReference/$docref via GET (on-demand=true + matching stored docref) and POST Parameters (type=34133-9, on-demand=false)
	respDocrefGet, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/DocumentReference/$docref?patient=Patient/hard-pat-1&start=2026-01-01&end=2026-02-01")
	if err != nil || respDocrefGet.StatusCode != http.StatusOK {
		t.Fatalf("GET /DocumentReference/$docref failed: %v / %d", err, respDocrefGet.StatusCode)
	}
	bDocrefGet, _ := io.ReadAll(respDocrefGet.Body)
	respDocrefGet.Body.Close()
	if !strings.Contains(string(bDocrefGet), `"docref-hard-1"`) || !strings.Contains(string(bDocrefGet), `"docref-ips-hard-pat-1"`) {
		t.Errorf("expected $docref GET to include stored docref-hard-1 and on-demand docref-ips-hard-pat-1, got: %s", string(bDocrefGet))
	}

	docrefPostParams := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "patient", "valueReference": {"reference": "Patient/hard-pat-1"}},
			{"name": "type", "valueCodeableConcept": {"coding": [{"system": "http://loinc.org", "code": "34133-9"}]}},
			{"name": "on-demand", "valueBoolean": false}
		]
	}`
	respDocrefPost, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/DocumentReference/$docref", "application/fhir+json", strings.NewReader(docrefPostParams))
	if err != nil || respDocrefPost.StatusCode != http.StatusOK {
		t.Fatalf("POST /DocumentReference/$docref failed: %v / %d", err, respDocrefPost.StatusCode)
	}
	bDocrefPost, _ := io.ReadAll(respDocrefPost.Body)
	respDocrefPost.Body.Close()
	if !strings.Contains(string(bDocrefPost), `"docref-hard-1"`) || strings.Contains(string(bDocrefPost), `"docref-ips-hard-pat-1"`) {
		t.Errorf("expected $docref POST with on-demand=false and type=34133-9 to return only docref-hard-1, got: %s", string(bDocrefPost))
	}

	// (d) POST /Patient/hard-pat-1/$everything with Parameters body (_type=Patient,Observation&_count=2&_offset=0)
	everythingParams := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "_type", "valueString": "Patient,Observation"},
			{"name": "_count", "valueInteger": 2},
			{"name": "_offset", "valueInteger": 0}
		]
	}`
	respEvPost, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/Patient/hard-pat-1/$everything", "application/fhir+json", strings.NewReader(everythingParams))
	if err != nil || respEvPost.StatusCode != http.StatusOK {
		t.Fatalf("POST /Patient/hard-pat-1/$everything failed: %v / %d", err, respEvPost.StatusCode)
	}
	bEvPost, _ := io.ReadAll(respEvPost.Body)
	respEvPost.Body.Close()
	if !strings.Contains(string(bEvPost), `"next"`) || !strings.Contains(string(bEvPost), `"self"`) {
		t.Errorf("expected POST /Patient/hard-pat-1/$everything with _count=2 to include self and next pagination links, got: %s", string(bEvPost))
	}
}

// TestServer_CriticalSecurityUnitSuite runs without requiring FLINT_TEST_POSTGRES_DSN,
// verifying SMART v2 scopes, patient compartment enforcement, and query validation across
// $populate, $extract, $document, Claim/$submit, $member-match, $translate, _list, $docref, $provenance, and $everything.
func TestServer_CriticalSecurityUnitSuite(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(nil, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	noRedirect := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	issuePatientToken := func(patientID, scopeStr string) string {
		authURL := fmt.Sprintf("%s/oauth2/authorize?response_type=code&client_id=unit-app&redirect_uri=https://app.example/cb&scope=%s&state=st1&patient=%s",
			ts.URL, strings.ReplaceAll(scopeStr, " ", "+"), patientID)
		respAuth, err := noRedirect.Get(authURL)
		if err != nil || respAuth.StatusCode != http.StatusFound {
			t.Fatalf("authorize failed: %v", err)
		}
		loc := respAuth.Header.Get("Location")
		respAuth.Body.Close()
		code := ""
		if idx := strings.Index(loc, "code="); idx != -1 {
			code = strings.Split(loc[idx+5:], "&")[0]
		}
		respTok, err := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
			strings.NewReader(fmt.Sprintf("grant_type=authorization_code&code=%s&redirect_uri=https://app.example/cb", code)))
		if err != nil || respTok.StatusCode != http.StatusOK {
			t.Fatalf("token exchange failed: %v", err)
		}
		var m map[string]any
		_ = json.NewDecoder(respTok.Body).Decode(&m)
		respTok.Body.Close()
		tok, _ := m["access_token"].(string)
		return tok
	}

	doWithToken := func(tok, method, path, body string) (*http.Response, []byte) {
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new req: %v", err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/fhir+json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do req: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	// 1. Composition/{id}/$document requires Composition.r scope (Patient.r alone is 403 Forbidden)
	patOnlyTok := issuePatientToken("pat-1", "patient/Patient.rs")
	if r, _ := doWithToken(patOnlyTok, http.MethodGet, "/fhir/r4/acme/Composition/comp-1/$document", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on GET /Composition/comp-1/$document with only Patient.rs scope, got %d", r.StatusCode)
	}
	if r, _ := doWithToken(patOnlyTok, http.MethodPost, "/fhir/r4/acme/Composition/comp-1/$document", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on POST /Composition/comp-1/$document with only Patient.rs scope, got %d", r.StatusCode)
	}

	// 2. Questionnaire/$populate requires BOTH Questionnaire scope AND Patient read/search scope
	qOnlyTok := issuePatientToken("pat-1", "patient/Questionnaire.rs")
	if r, _ := doWithToken(qOnlyTok, http.MethodGet, "/fhir/r4/acme/Questionnaire/phq-9/$populate?subject=Patient/pat-1", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on Questionnaire/$populate without Patient read/search scope, got %d", r.StatusCode)
	}
	if r, _ := doWithToken(patOnlyTok, http.MethodGet, "/fhir/r4/acme/Questionnaire/phq-9/$populate?subject=Patient/pat-1", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on Questionnaire/$populate without Questionnaire scope, got %d", r.StatusCode)
	}

	// 3. QuestionnaireResponse/$extract enforces patient compartment isolation & persist=true scope check
	qrReadTok := issuePatientToken("pat-1", "patient/QuestionnaireResponse.rs")
	qrCrossPat := `{
		"resourceType": "QuestionnaireResponse",
		"id": "qr-other-pat",
		"status": "completed",
		"subject": {"reference": "Patient/pat-2"},
		"item": [{"linkId": "heart-rate", "answer": [{"valueQuantity": {"value": 80.0}}]}]
	}`
	if r, _ := doWithToken(qrReadTok, http.MethodPost, "/fhir/r4/acme/QuestionnaireResponse/$extract", qrCrossPat); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on cross-patient QuestionnaireResponse/$extract, got %d", r.StatusCode)
	}
	qrOwnPat := `{
		"resourceType": "QuestionnaireResponse",
		"id": "qr-own-pat",
		"status": "completed",
		"subject": {"reference": "Patient/pat-1"},
		"item": [{"linkId": "heart-rate", "answer": [{"valueQuantity": {"value": 80.0}}]}]
	}`
	if r, b := doWithToken(qrReadTok, http.MethodPost, "/fhir/r4/acme/QuestionnaireResponse/$extract", qrOwnPat); r.StatusCode != http.StatusOK || !strings.Contains(string(b), `"Observation"`) || !strings.Contains(string(b), `"Provenance"`) {
		t.Errorf("expected 200 OK with Observation and Provenance on own-patient QuestionnaireResponse/$extract (persist=false), got %d: %s", r.StatusCode, b)
	}
	if r, _ := doWithToken(qrReadTok, http.MethodPost, "/fhir/r4/acme/QuestionnaireResponse/$extract?persist=true", qrOwnPat); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on QuestionnaireResponse/$extract?persist=true without Observation.c scope, got %d", r.StatusCode)
	}

	// 4. Claim/$submit requires Claim.c or Claim.u, enforces patient compartment, and checks supporting resource scopes
	claimReadOnlyTok := issuePatientToken("pat-1", "patient/Claim.rs patient/Patient.rs")
	if r, _ := doWithToken(claimReadOnlyTok, http.MethodPost, "/fhir/r4/acme/Claim/$submit", `{"resourceType":"Claim"}`); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on Claim/$submit with read-only Claim.rs scope, got %d", r.StatusCode)
	}
	claimWriteTok := issuePatientToken("pat-1", "patient/Claim.c")
	if r, _ := doWithToken(claimWriteTok, http.MethodPost, "/fhir/r4/acme/Claim/$submit", `{"resourceType":"Claim","patient":{"reference":"Patient/pat-2"}}`); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on cross-patient Claim/$submit, got %d", r.StatusCode)
	}
	pasWithUnauthSupporting := `{
		"resourceType": "Bundle",
		"type": "collection",
		"entry": [
			{"resource": {"resourceType": "Claim", "patient": {"reference": "Patient/pat-1"}}},
			{"resource": {"resourceType": "Practitioner", "id": "prac-inject", "name": [{"family": "Evil"}]}}
		]
	}`
	if r, _ := doWithToken(claimWriteTok, http.MethodPost, "/fhir/r4/acme/Claim/$submit", pasWithUnauthSupporting); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on Claim/$submit Bundle containing unauthorized supporting Practitioner resource, got %d", r.StatusCode)
	}

	// 5. _list cohort search requires List read/search scope and rejects empty _list=,
	if r, _ := doWithToken(patOnlyTok, http.MethodGet, "/fhir/r4/acme/Patient?_list=cohort-1", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on _list search without List read/search scope, got %d", r.StatusCode)
	}
	if r, _ := doWithToken("", http.MethodGet, "/fhir/r4/acme/Patient?_list=,", ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request on empty _list=, query, got %d", r.StatusCode)
	}

	// 6. ConceptMap/$translate enforces ConceptMap.r/s scope and translates built-in SNOMED -> ICD-10-CM
	if r, _ := doWithToken(patOnlyTok, http.MethodGet, "/fhir/r4/acme/ConceptMap/$translate?system=http://snomed.info/sct&code=38341003", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on ConceptMap/$translate without ConceptMap scope, got %d", r.StatusCode)
	}
	cmTok := issuePatientToken("pat-1", "patient/ConceptMap.rs")
	if r, b := doWithToken(cmTok, http.MethodGet, "/fhir/r4/acme/ConceptMap/$translate?system=http://snomed.info/sct&code=38341003", ""); r.StatusCode != http.StatusOK || !strings.Contains(string(b), `"I10"`) {
		t.Errorf("expected 200 OK with ICD-10-CM I10 from ConceptMap/$translate, got %d: %s", r.StatusCode, b)
	}

	// 7. DocumentReference/$docref requires DocumentReference.r/s scope, requires patient parameter, and enforces patient compartment
	if r, _ := doWithToken(patOnlyTok, http.MethodGet, "/fhir/r4/acme/DocumentReference/$docref?patient=Patient/pat-1", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on DocumentReference/$docref without DocumentReference scope, got %d", r.StatusCode)
	}
	docRefTok := issuePatientToken("pat-1", "patient/DocumentReference.rs patient/Patient.rs")
	if r, _ := doWithToken(docRefTok, http.MethodGet, "/fhir/r4/acme/DocumentReference/$docref", ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request on DocumentReference/$docref without patient parameter, got %d", r.StatusCode)
	}
	if r, _ := doWithToken(docRefTok, http.MethodGet, "/fhir/r4/acme/DocumentReference/$docref?patient=Patient/pat-2", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on cross-patient DocumentReference/$docref, got %d", r.StatusCode)
	}

	// 8. {resourceType}/{id}/$provenance requires BOTH target resource read scope AND Provenance read/search scope, plus patient compartment check
	if r, _ := doWithToken(patOnlyTok, http.MethodGet, "/fhir/r4/acme/Patient/pat-1/$provenance", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on /Patient/pat-1/$provenance without Provenance.rs scope, got %d", r.StatusCode)
	}
	provTok := issuePatientToken("pat-1", "patient/Patient.rs patient/Provenance.rs")
	if r, _ := doWithToken(provTok, http.MethodGet, "/fhir/r4/acme/Patient/pat-2/$provenance", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on cross-patient /Patient/pat-2/$provenance, got %d", r.StatusCode)
	}

	// 9. POST Patient/{id}/$everything enforces patient compartment and validates _type parameter
	if r, _ := doWithToken(patOnlyTok, http.MethodPost, "/fhir/r4/acme/Patient/pat-2/$everything", ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on cross-patient POST /Patient/pat-2/$everything, got %d", r.StatusCode)
	}
	if r, _ := doWithToken("", http.MethodGet, "/fhir/r4/acme/Patient/pat-1/$everything?_type=NotAResource", ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request on Patient/$everything with unknown _type=NotAResource, got %d", r.StatusCode)
	}

	// 10. /oauth2/token client_credentials supports optional patient form parameter for $docs SMART v2 Token Minter
	respCCPat, err := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=client_credentials&scope=patient/*.rs&patient=example-patient-1"))
	if err != nil || respCCPat.StatusCode != http.StatusOK {
		t.Fatalf("POST /oauth2/token client_credentials with patient failed: %v / %d", err, respCCPat.StatusCode)
	}
	var ccPatMap map[string]any
	_ = json.NewDecoder(respCCPat.Body).Decode(&ccPatMap)
	respCCPat.Body.Close()
	if ccPatMap["patient"] != "example-patient-1" {
		t.Errorf("expected client_credentials token response to include patient=example-patient-1, got %+v", ccPatMap)
	}
}
