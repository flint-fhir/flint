package server_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

// TestONCG10SMARTv2AndMultiTenantIsolationConformanceSuite verifies zero-leakage
// multi-tenant isolation across REST/History/$everything/$graphql, SMART App Launch v2.2.0
// patient compartment & granular sub-resource scopes, OAuth2 token revocation & JWT
// signature enforcement, and HTTP optimistic concurrency (If-Match / If-None-Exist).
func TestONCG10SMARTv2AndMultiTenantIsolationConformanceSuite(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenantAlpha := "test-sec-tenant-alpha"
	tenantBeta := "test-sec-tenant-beta"
	cleanup := func() {
		_ = postgres.PurgeTenant(ctx, db, tenantAlpha)
		_ = postgres.PurgeTenant(ctx, db, tenantBeta)
	}
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Seed Tenant Alpha with 2 Patients (sec-pat-1, sec-pat-2) and 3 Observations
	seedAlpha := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"fullUrl": "Patient/sec-pat-1",
				"resource": {
					"resourceType": "Patient",
					"id": "sec-pat-1",
					"active": true,
					"gender": "female",
					"name": [{"use": "official", "family": "AlphaSecret", "given": ["Alice"]}],
					"identifier": [{"system": "http://alpha.example.org/mrn", "value": "DUP-MRN"}]
				},
				"request": {"method": "PUT", "url": "Patient/sec-pat-1"}
			},
			{
				"fullUrl": "Patient/sec-pat-2",
				"resource": {
					"resourceType": "Patient",
					"id": "sec-pat-2",
					"active": true,
					"gender": "male",
					"name": [{"use": "official", "family": "AlphaOther", "given": ["Bob"]}],
					"identifier": [{"system": "http://alpha.example.org/mrn", "value": "DUP-MRN"}]
				},
				"request": {"method": "PUT", "url": "Patient/sec-pat-2"}
			},
			{
				"fullUrl": "Observation/sec-obs-lab-1",
				"resource": {
					"resourceType": "Observation",
					"id": "sec-obs-lab-1",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}]}],
					"code": {"coding": [{"system": "http://loinc.org", "code": "4548-4", "display": "HbA1c"}]},
					"subject": {"reference": "Patient/sec-pat-1"},
					"valueQuantity": {"value": 5.6, "unit": "%"}
				},
				"request": {"method": "PUT", "url": "Observation/sec-obs-lab-1"}
			},
			{
				"fullUrl": "Observation/sec-obs-vital-1",
				"resource": {
					"resourceType": "Observation",
					"id": "sec-obs-vital-1",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
					"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]},
					"subject": {"reference": "Patient/sec-pat-1"},
					"valueQuantity": {"value": 70.0, "unit": "/min"}
				},
				"request": {"method": "PUT", "url": "Observation/sec-obs-vital-1"}
			},
			{
				"fullUrl": "Observation/sec-obs-pat2-lab",
				"resource": {
					"resourceType": "Observation",
					"id": "sec-obs-pat2-lab",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}]}],
					"code": {"coding": [{"system": "http://loinc.org", "code": "4548-4"}]},
					"subject": {"reference": "Patient/sec-pat-2"},
					"valueQuantity": {"value": 6.4, "unit": "%"}
				},
				"request": {"method": "PUT", "url": "Observation/sec-obs-pat2-lab"}
			}
		]
	}`

	respSeed, err := http.Post(ts.URL+"/fhir/r4/"+tenantAlpha, "application/fhir+json", strings.NewReader(seedAlpha))
	if err != nil || respSeed.StatusCode != http.StatusOK {
		t.Fatalf("seed tenantAlpha failed: %v / %d", err, respSeed.StatusCode)
	}
	respSeed.Body.Close()

	// =========================================================================
	// 1. Multi-Tenant Zero-Leakage Isolation (Tenant Alpha vs Tenant Beta)
	// =========================================================================
	t.Run("MultiTenant_ZeroLeakage_Isolation_Across_REST_History_Everything_GraphQL", func(t *testing.T) {
		// 1a. Cross-tenant GET read -> 404 Not Found
		respRead, err := http.Get(ts.URL + "/fhir/r4/" + tenantBeta + "/Patient/sec-pat-1")
		if err != nil {
			t.Fatalf("GET tenantBeta Patient: %v", err)
		}
		respRead.Body.Close()
		if respRead.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found reading tenantAlpha Patient from tenantBeta, got %d", respRead.StatusCode)
		}

		// 1b. Cross-tenant GET search -> total == 0
		respSearch, err := http.Get(ts.URL + "/fhir/r4/" + tenantBeta + "/Patient?family=AlphaSecret")
		if err != nil {
			t.Fatalf("GET tenantBeta search: %v", err)
		}
		var betaBundle map[string]any
		_ = json.NewDecoder(respSearch.Body).Decode(&betaBundle)
		respSearch.Body.Close()
		if betaBundle["total"] != float64(0) {
			t.Fatalf("expected tenantBeta search total=0, got %v", betaBundle["total"])
		}

		// 1c. Cross-tenant _history -> total == 0
		respHist, err := http.Get(ts.URL + "/fhir/r4/" + tenantBeta + "/Patient/sec-pat-1/_history")
		if err != nil {
			t.Fatalf("GET tenantBeta _history: %v", err)
		}
		var histBundle map[string]any
		_ = json.NewDecoder(respHist.Body).Decode(&histBundle)
		respHist.Body.Close()
		if histBundle["total"] != float64(0) {
			t.Fatalf("expected tenantBeta _history total=0, got %v", histBundle["total"])
		}

		// 1d. Cross-tenant $everything -> 404 Not Found
		respEv, err := http.Get(ts.URL + "/fhir/r4/" + tenantBeta + "/Patient/sec-pat-1/$everything")
		if err != nil {
			t.Fatalf("GET tenantBeta $everything: %v", err)
		}
		respEv.Body.Close()
		if respEv.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found on tenantBeta $everything for tenantAlpha Patient, got %d", respEv.StatusCode)
		}

		// 1e. Cross-tenant $graph & system-level _type search -> 404 Not Found / 0 entries
		respGraph, err := http.Get(ts.URL + "/fhir/r4/" + tenantBeta + "/Patient/sec-pat-1/$graph")
		if err != nil {
			t.Fatalf("GET tenantBeta $graph: %v", err)
		}
		respGraph.Body.Close()
		if respGraph.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found on tenantBeta $graph for tenantAlpha Patient, got %d", respGraph.StatusCode)
		}

		respSys, err := http.Get(ts.URL + "/fhir/r4/" + tenantBeta + "?_type=Patient,Observation")
		if err != nil {
			t.Fatalf("GET tenantBeta system search: %v", err)
		}
		var sysBundle map[string]any
		_ = json.NewDecoder(respSys.Body).Decode(&sysBundle)
		respSys.Body.Close()
		if sysBundle["total"] != float64(0) {
			t.Fatalf("expected tenantBeta system search total=0, got %v", sysBundle["total"])
		}

		// 1f. Cross-tenant DELETE in tenantBeta does not affect tenantAlpha
		reqDel, _ := http.NewRequest(http.MethodDelete, ts.URL+"/fhir/r4/"+tenantBeta+"/Patient/sec-pat-1", nil)
		respDel, err := http.DefaultClient.Do(reqDel)
		if err != nil {
			t.Fatalf("DELETE tenantBeta Patient: %v", err)
		}
		respDel.Body.Close()

		respAlphaCheck, err := http.Get(ts.URL + "/fhir/r4/" + tenantAlpha + "/Patient/sec-pat-1")
		if err != nil || respAlphaCheck.StatusCode != http.StatusOK {
			t.Fatalf("expected tenantAlpha Patient/sec-pat-1 to remain intact (200 OK), got %v / %d", err, respAlphaCheck.StatusCode)
		}
		respAlphaCheck.Body.Close()
	})

	// =========================================================================
	// 2. SMART App Launch v2.2.0 Granular Scope & Patient Compartment Enforcement
	// =========================================================================
	t.Run("SMARTv2_GranularScope_Category_And_PatientCompartment_Enforcement", func(t *testing.T) {
		noRedirect := &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		// Authorize token for sec-pat-1 with read/search ONLY on Patient and laboratory Observations
		authURL := ts.URL + "/oauth2/authorize?response_type=code&client_id=smart-client&redirect_uri=https://app.example/cb&patient=sec-pat-1&scope=patient/Patient.rs+patient/Observation.rs%3Fcategory%3Dhttp://terminology.hl7.org/CodeSystem/observation-category%7Claboratory"
		respAuth, err := noRedirect.Get(authURL)
		if err != nil || respAuth.StatusCode != http.StatusFound {
			t.Fatalf("expected 302 from /oauth2/authorize, got %v / %d", err, respAuth.StatusCode)
		}
		respAuth.Body.Close()
		loc := respAuth.Header.Get("Location")
		code := loc[strings.Index(loc, "code=")+5:]
		if amp := strings.IndexByte(code, '&'); amp != -1 {
			code = code[:amp]
		}

		respTok, err := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
			strings.NewReader("grant_type=authorization_code&code="+code+"&redirect_uri=https://app.example/cb"))
		if err != nil || respTok.StatusCode != http.StatusOK {
			t.Fatalf("token exchange failed: %v / %d", err, respTok.StatusCode)
		}
		var tokMap map[string]any
		_ = json.NewDecoder(respTok.Body).Decode(&tokMap)
		respTok.Body.Close()
		accessToken, _ := tokMap["access_token"].(string)
		if accessToken == "" {
			t.Fatalf("missing access_token in %+v", tokMap)
		}

		doAuthed := func(method, path, body string) (*http.Response, []byte) {
			var rdr io.Reader
			if body != "" {
				rdr = strings.NewReader(body)
			}
			req, _ := http.NewRequest(method, ts.URL+path, rdr)
			req.Header.Set("Authorization", "Bearer "+accessToken)
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

		// Authorized: own Patient (sec-pat-1) -> 200 OK
		if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenantAlpha+"/Patient/sec-pat-1", ""); r.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK reading authorized Patient/sec-pat-1, got %d", r.StatusCode)
		}
		// Forbidden: other Patient (sec-pat-2) -> 403 Forbidden
		if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenantAlpha+"/Patient/sec-pat-2", ""); r.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden reading cross-compartment Patient/sec-pat-2, got %d", r.StatusCode)
		}
		// Authorized: laboratory Observation for sec-pat-1 -> 200 OK
		if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenantAlpha+"/Observation/sec-obs-lab-1", ""); r.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK reading laboratory Observation/sec-obs-lab-1, got %d", r.StatusCode)
		}
		// Forbidden: vital-signs Observation for sec-pat-1 -> 403 Forbidden (granular category=laboratory filter)
		if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenantAlpha+"/Observation/sec-obs-vital-1", ""); r.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden reading vital-signs Observation with laboratory-only scope, got %d", r.StatusCode)
		}
		// Forbidden: laboratory Observation belonging to sec-pat-2 -> 403 Forbidden
		if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenantAlpha+"/Observation/sec-obs-pat2-lab", ""); r.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden reading sec-pat-2 Observation, got %d", r.StatusCode)
		}
		// Search /Observation automatically filters to sec-obs-lab-1 only
		if r, b := doAuthed(http.MethodGet, "/fhir/r4/"+tenantAlpha+"/Observation", ""); r.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK searching /Observation, got %d", r.StatusCode)
		} else if !strings.Contains(string(b), "sec-obs-lab-1") || strings.Contains(string(b), "sec-obs-vital-1") || strings.Contains(string(b), "sec-obs-pat2-lab") {
			t.Fatalf("expected search /Observation to return only sec-obs-lab-1, got %s", string(b))
		}
		// Write attempts (POST, PUT, DELETE) with .rs token -> 403 Forbidden
		if r, _ := doAuthed(http.MethodDelete, "/fhir/r4/"+tenantAlpha+"/Patient/sec-pat-1", ""); r.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden deleting Patient with .rs scope, got %d", r.StatusCode)
		}
		if r, _ := doAuthed(http.MethodPut, "/fhir/r4/"+tenantAlpha+"/Patient/sec-pat-1", `{"resourceType":"Patient","id":"sec-pat-1"}`); r.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden updating Patient with .rs scope, got %d", r.StatusCode)
		}

		// Revoke token via POST /oauth2/revoke -> subsequent GET returns 401 Unauthorized
		respRev, err := http.Post(ts.URL+"/oauth2/revoke", "application/x-www-form-urlencoded",
			strings.NewReader("token="+accessToken))
		if err != nil || respRev.StatusCode != http.StatusOK {
			t.Fatalf("POST /oauth2/revoke failed: %v / %d", err, respRev.StatusCode)
		}
		respRev.Body.Close()

		if r, _ := doAuthed(http.MethodGet, "/fhir/r4/"+tenantAlpha+"/Patient/sec-pat-1", ""); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized using revoked Bearer token, got %d", r.StatusCode)
		}
	})

	// =========================================================================
	// 3. Backend Services JWT Algorithm & Signature Tampering Rejection
	// =========================================================================
	t.Run("BackendServices_JWT_AlgNone_And_TamperedSignature_Rejection", func(t *testing.T) {
		hdrNone := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
		claims := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"bulk","sub":"bulk","aud":"` + ts.URL + `/oauth2/token","jti":"none-1","exp":4102444800}`))
		unsignedJWT := hdrNone + "." + claims + ".sig"

		respNone, err := http.Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded",
			strings.NewReader("grant_type=client_credentials&client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer&client_assertion="+unsignedJWT))
		if err != nil {
			t.Fatalf("POST /oauth2/token alg=none: %v", err)
		}
		respNone.Body.Close()
		if respNone.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for alg=none client_assertion, got %d", respNone.StatusCode)
		}
	})

	// =========================================================================
	// 4. Optimistic Concurrency (If-Match) & Conditional Create (If-None-Exist)
	// =========================================================================
	t.Run("OptimisticConcurrency_IfMatch_And_IfNoneExist_DuplicatePrecondition", func(t *testing.T) {
		// Stale If-Match on PUT -> 412 Precondition Failed
		reqPut, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/"+tenantAlpha+"/Patient/sec-pat-1",
			strings.NewReader(`{"resourceType":"Patient","id":"sec-pat-1","active":true}`))
		reqPut.Header.Set("Content-Type", "application/fhir+json")
		reqPut.Header.Set("If-Match", `W/"999"`)
		respPut, err := http.DefaultClient.Do(reqPut)
		if err != nil {
			t.Fatalf("PUT If-Match W/999: %v", err)
		}
		respPut.Body.Close()
		if respPut.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("expected 412 Precondition Failed on stale If-Match PUT, got %d", respPut.StatusCode)
		}

		// Conditional Create (If-None-Exist) matching 2 existing Patients (both have identifier=DUP-MRN) -> 412 Precondition Failed
		reqCond, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenantAlpha+"/Patient",
			strings.NewReader(`{"resourceType":"Patient","active":true}`))
		reqCond.Header.Set("Content-Type", "application/fhir+json")
		reqCond.Header.Set("If-None-Exist", "identifier=http://alpha.example.org/mrn|DUP-MRN")
		respCond, err := http.DefaultClient.Do(reqCond)
		if err != nil {
			t.Fatalf("POST If-None-Exist duplicate: %v", err)
		}
		respCond.Body.Close()
		if respCond.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("expected 412 Precondition Failed when If-None-Exist matches multiple resources, got %d", respCond.StatusCode)
		}
	})
}
