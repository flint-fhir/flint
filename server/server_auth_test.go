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
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestServer_AuthMiddleware(t *testing.T) {
	issuer := "https://auth.azra.dev"
	audience := "flint-api"

	mockVal, err := auth.NewMockTokenValidator(issuer, audience)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(nil, logger) // store is nil for auth-layer rejection tests
	srv.SetSMARTConfig(server.SMARTConfig{
		Issuer:                issuer,
		AuthorizationEndpoint: issuer + "/oauth/authorize",
		TokenEndpoint:         issuer + "/oauth/token",
	})
	srv.SetTokenValidator(mockVal)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	t.Run("Public Endpoints Allow Unauthenticated", func(t *testing.T) {
		// 1. CapabilityStatement /metadata
		resp, err := http.Get(ts.URL + "/fhir/r4/default/metadata")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// 2. SMART Configuration
		resp2, err := http.Get(ts.URL + "/fhir/r4/default/.well-known/smart-configuration")
		require.NoError(t, err)
		defer resp2.Body.Close()
		assert.Equal(t, http.StatusOK, resp2.StatusCode)
	})

	t.Run("Protected Endpoint Rejects Missing Token", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/default/Patient/pat-1")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

		var outcome map[string]any
		err = json.NewDecoder(resp.Body).Decode(&outcome)
		require.NoError(t, err)
		assert.Equal(t, "OperationOutcome", outcome["resourceType"])
	})

	t.Run("Protected Endpoint Rejects Malformed Header", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/default/Patient/pat-1", nil)
		req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("Protected Endpoint Rejects Expired Token", func(t *testing.T) {
		expiredToken, err := mockVal.IssueToken("user-1", "patient/Patient.r", "pat-1", -time.Minute, nil)
		require.NoError(t, err)

		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/default/Patient/pat-1", nil)
		req.Header.Set("Authorization", "Bearer "+expiredToken)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("Protected Endpoint Rejects Insufficient Scope", func(t *testing.T) {
		// Token only has Observation scope, trying to read Patient
		token, err := mockVal.IssueToken("user-1", "patient/Observation.rs", "pat-1", time.Hour, nil)
		require.NoError(t, err)

		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/default/Patient/pat-1", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)

		var outcome map[string]any
		json.NewDecoder(resp.Body).Decode(&outcome)
		issues := outcome["issue"].([]any)
		diag := issues[0].(map[string]any)["diagnostics"].(string)
		assert.Contains(t, diag, "insufficient scope for read on Patient")
	})

	t.Run("Patient Compartment Isolation Blocks Other Patients", func(t *testing.T) {
		// Scoped to patient pat-100
		token, err := mockVal.IssueToken("user-patient", "patient/Patient.r", "pat-100", time.Hour, nil)
		require.NoError(t, err)

		// Attempt to read patient pat-999
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/default/Patient/pat-999", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)

		var outcome map[string]any
		json.NewDecoder(resp.Body).Decode(&outcome)
		issues := outcome["issue"].([]any)
		diag := issues[0].(map[string]any)["diagnostics"].(string)
		assert.Contains(t, diag, "patient-scoped token (pat-100) cannot access patient pat-999")
	})

	t.Run("Patient Compartment Isolation Blocks Other Patient Search Filter", func(t *testing.T) {
		// Scoped to patient pat-100
		token, err := mockVal.IssueToken("user-patient", "patient/Observation.rs", "pat-100", time.Hour, nil)
		require.NoError(t, err)

		// Attempt to search observations for patient pat-200
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/default/Observation?patient=pat-200", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("Tenant Isolation Blocks Access to Other Tenants", func(t *testing.T) {
		token, err := mockVal.IssueToken("user-1", "system/*.r", "", time.Hour, map[string]any{
			"tenant_id": "hospital_alpha",
		})
		require.NoError(t, err)

		// Attempt to access hospital_beta
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/hospital_beta/Patient/pat-1", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("Validate Endpoint Enforces Scope Checks", func(t *testing.T) {
		validPatient := `{"resourceType":"Patient","name":[{"family":"Doe"}]}`

		// 1. Unauthenticated request to $validate is rejected with 401
		reqNoAuth, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/default/Patient/$validate", strings.NewReader(validPatient))
		reqNoAuth.Header.Set("Content-Type", "application/fhir+json")
		respNoAuth, err := http.DefaultClient.Do(reqNoAuth)
		require.NoError(t, err)
		defer respNoAuth.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, respNoAuth.StatusCode)

		// 2. Token with patient/*.r allows $validate
		readToken, err := mockVal.IssueToken("doc-1", "patient/*.r", "", time.Hour, nil)
		require.NoError(t, err)
		reqAuth, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/default/Patient/$validate", strings.NewReader(validPatient))
		reqAuth.Header.Set("Content-Type", "application/fhir+json")
		reqAuth.Header.Set("Authorization", "Bearer "+readToken)
		respAuth, err := http.DefaultClient.Do(reqAuth)
		require.NoError(t, err)
		defer respAuth.Body.Close()
		assert.Equal(t, http.StatusOK, respAuth.StatusCode)

		// 3. Token with scope only for Encounter (not Patient) is rejected with 403
		encToken, err := mockVal.IssueToken("doc-2", "patient/Encounter.rs", "", time.Hour, nil)
		require.NoError(t, err)
		reqMismatched, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/default/Patient/$validate", strings.NewReader(validPatient))
		reqMismatched.Header.Set("Content-Type", "application/fhir+json")
		reqMismatched.Header.Set("Authorization", "Bearer "+encToken)
		respMismatched, err := http.DefaultClient.Do(reqMismatched)
		require.NoError(t, err)
		defer respMismatched.Body.Close()
		assert.Equal(t, http.StatusForbidden, respMismatched.StatusCode)
	})

	t.Run("PUT and DELETE Enforce Update/Delete Scopes and Patient Compartment", func(t *testing.T) {
		readOnlyToken, err := mockVal.IssueToken("user-ro", "patient/Patient.rs", "pat-100", time.Hour, nil)
		require.NoError(t, err)

		// PUT with read-only token -> 403 Forbidden
		reqPut, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/default/Patient/pat-100", strings.NewReader(`{"resourceType":"Patient"}`))
		reqPut.Header.Set("Authorization", "Bearer "+readOnlyToken)
		respPut, err := http.DefaultClient.Do(reqPut)
		require.NoError(t, err)
		defer respPut.Body.Close()
		assert.Equal(t, http.StatusForbidden, respPut.StatusCode)

		// DELETE with read-only token -> 403 Forbidden
		reqDel, _ := http.NewRequest(http.MethodDelete, ts.URL+"/fhir/r4/default/Patient/pat-100", nil)
		reqDel.Header.Set("Authorization", "Bearer "+readOnlyToken)
		respDel, err := http.DefaultClient.Do(reqDel)
		require.NoError(t, err)
		defer respDel.Body.Close()
		assert.Equal(t, http.StatusForbidden, respDel.StatusCode)

		// Patient-scoped token with u/d scopes blocked from updating/deleting another patient
		udToken, err := mockVal.IssueToken("user-ud", "patient/Patient.ud", "pat-100", time.Hour, nil)
		require.NoError(t, err)

		reqPutOther, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/default/Patient/pat-999", strings.NewReader(`{"resourceType":"Patient"}`))
		reqPutOther.Header.Set("Authorization", "Bearer "+udToken)
		respPutOther, err := http.DefaultClient.Do(reqPutOther)
		require.NoError(t, err)
		defer respPutOther.Body.Close()
		assert.Equal(t, http.StatusForbidden, respPutOther.StatusCode)

		reqDelOther, _ := http.NewRequest(http.MethodDelete, ts.URL+"/fhir/r4/default/Patient/pat-999", nil)
		reqDelOther.Header.Set("Authorization", "Bearer "+udToken)
		respDelOther, err := http.DefaultClient.Do(reqDelOther)
		require.NoError(t, err)
		defer respDelOther.Body.Close()
		assert.Equal(t, http.StatusForbidden, respDelOther.StatusCode)
	})

	t.Run("Security Rejections (401 and 403) Are Recorded in AuditEvent Log With Actor Identity", func(t *testing.T) {
		rec := srv.AuditRecorder()
		require.NotNil(t, rec)

		// Verify 403 DELETE attempt by user-ud on Patient/pat-999 captured actor identity and compartment
		records, total, err := rec.Query(t.Context(), postgres.AuditQueryParams{
			TenantID:     "default",
			Action:       "D",
			Outcome:      "4",
			AgentSubject: "user-ud",
			EntityType:   "Patient",
			EntityID:     "pat-999",
		})
		require.NoError(t, err)
		require.Equal(t, 1, total)
		require.Len(t, records, 1)
		assert.Equal(t, http.StatusForbidden, records[0].HTTPStatus)
		assert.Equal(t, "pat-100", records[0].AgentPatient)
		assert.Equal(t, "delete", records[0].SubtypeCode)

		// Verify authorized auditor with system/AuditEvent.rs can query AuditEvent via FHIR REST
		auditorToken, err := mockVal.IssueToken("sec-auditor", "system/AuditEvent.rs", "", time.Hour, nil)
		require.NoError(t, err)

		reqSearch, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/default/AuditEvent?outcome=4&agent=user-ud", nil)
		reqSearch.Header.Set("Authorization", "Bearer "+auditorToken)
		respSearch, err := http.DefaultClient.Do(reqSearch)
		require.NoError(t, err)
		defer respSearch.Body.Close()
		assert.Equal(t, http.StatusOK, respSearch.StatusCode)

		var bundle map[string]any
		require.NoError(t, json.NewDecoder(respSearch.Body).Decode(&bundle))
		assert.Equal(t, "Bundle", bundle["resourceType"])
		assert.GreaterOrEqual(t, int(bundle["total"].(float64)), 2) // PUT + DELETE 403s by user-ud
	})

	t.Run("Patient/$everything Enforces Patient Compartment and Records Audit Subtype everything", func(t *testing.T) {
		patToken, err := mockVal.IssueToken("user-ev", "patient/Patient.r", "pat-100", time.Hour, nil)
		require.NoError(t, err)

		// Attempt to fetch $everything for pat-999 with token restricted to pat-100 -> 403 Forbidden
		reqEv, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/default/Patient/pat-999/$everything", nil)
		reqEv.Header.Set("Authorization", "Bearer "+patToken)
		respEv, err := http.DefaultClient.Do(reqEv)
		require.NoError(t, err)
		defer respEv.Body.Close()
		assert.Equal(t, http.StatusForbidden, respEv.StatusCode)

		records, total, err := srv.AuditRecorder().Query(t.Context(), postgres.AuditQueryParams{
			TenantID:     "default",
			Action:       "E",
			SubtypeCode:  "everything",
			Outcome:      "4",
			AgentSubject: "user-ev",
			EntityType:   "Patient",
			EntityID:     "pat-999",
		})
		require.NoError(t, err)
		require.Equal(t, 1, total)
		assert.Equal(t, "pat-100", records[0].AgentPatient)
	})
}

func TestServer_PatientCompartmentIsolation_Integration(t *testing.T) {
	dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("FLINT_TEST_POSTGRES_DSN not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, postgres.RunMigrations(t.Context(), db))

	store := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(store, logger)
	srv.RegisterResourceType("Patient", &patpb.Patient{})
	srv.RegisterResourceType("Observation", &obspb.Observation{})
	srv.RegisterResourceType("Practitioner", &pracpb.Practitioner{})

	mockVal, err := auth.NewMockTokenValidator("https://auth.azra.dev", "flint-api")
	require.NoError(t, err)
	srv.SetTokenValidator(mockVal)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := t.Context()
	defer func() {
		_ = postgres.PurgeTenant(ctx, db, "test-compartment")
	}()

	// System token to seed 2 patients (pat-100 and pat-200), a shared Practitioner (prac-shared), and their Observations (obs-100 and obs-200)
	sysToken, err := mockVal.IssueToken("sys-admin", "system/*.cruds", "", time.Hour, nil)
	require.NoError(t, err)

	seedReq := func(method, path, body string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+sysToken)
		req.Header.Set("Content-Type", "application/fhir+json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Less(t, resp.StatusCode, 300)
	}

	seedReq(http.MethodPut, "/fhir/r4/test-compartment/Practitioner/prac-shared", `{"resourceType":"Practitioner","id":"prac-shared","name":[{"family":"House"}]}`)
	seedReq(http.MethodPut, "/fhir/r4/test-compartment/Patient/pat-100", `{"resourceType":"Patient","id":"pat-100","name":[{"family":"Hundred"}]}`)
	seedReq(http.MethodPut, "/fhir/r4/test-compartment/Patient/pat-200", `{"resourceType":"Patient","id":"pat-200","name":[{"family":"TwoHundred"}]}`)
	seedReq(http.MethodPut, "/fhir/r4/test-compartment/Observation/obs-100", `{"resourceType":"Observation","id":"obs-100","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/pat-100"},"performer":[{"reference":"Practitioner/prac-shared"}]}`)
	seedReq(http.MethodPut, "/fhir/r4/test-compartment/Observation/obs-200", `{"resourceType":"Observation","id":"obs-200","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/pat-200"},"performer":[{"reference":"Practitioner/prac-shared"}]}`)

	// Issue a patient-scoped token restricted to pat-100 with full cruds on Patient, Observation, and Practitioner
	pat100Token, err := mockVal.IssueToken("pat-100-user", "patient/Patient.cruds patient/Observation.cruds patient/Practitioner.rs", "pat-100", time.Hour, nil)
	require.NoError(t, err)

	doPat100 := func(method, path, body string) *http.Response {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, r)
		req.Header.Set("Authorization", "Bearer "+pat100Token)
		req.Header.Set("Content-Type", "application/fhir+json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}

	t.Run("Clinical Point Read, VRead, and History Block Cross-Patient Access", func(t *testing.T) {
		// Own observation -> 200 OK
		respOwn := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Observation/obs-100", "")
		defer respOwn.Body.Close()
		assert.Equal(t, http.StatusOK, respOwn.StatusCode)

		// Other patient's observation -> 403 Forbidden
		respOther := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Observation/obs-200", "")
		defer respOther.Body.Close()
		assert.Equal(t, http.StatusForbidden, respOther.StatusCode)

		// Other patient's observation _history and vread -> 403 Forbidden
		respHist := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Observation/obs-200/_history", "")
		defer respHist.Body.Close()
		assert.Equal(t, http.StatusForbidden, respHist.StatusCode)

		respVRead := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Observation/obs-200/_history/1", "")
		defer respVRead.Body.Close()
		assert.Equal(t, http.StatusForbidden, respVRead.StatusCode)
	})

	t.Run("Unfiltered Clinical and Patient Searches Auto-Scope to Patient Compartment", func(t *testing.T) {
		// Unfiltered GET /Observation?code=8867-4 without patient= parameter must ONLY return obs-100
		respObs := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Observation?code=8867-4", "")
		defer respObs.Body.Close()
		require.Equal(t, http.StatusOK, respObs.StatusCode)

		var obsBundle map[string]any
		require.NoError(t, json.NewDecoder(respObs.Body).Decode(&obsBundle))
		assert.Equal(t, 1, int(obsBundle["total"].(float64)))
		entries := obsBundle["entry"].([]any)
		require.Len(t, entries, 1)
		assert.Equal(t, "Observation/obs-100", entries[0].(map[string]any)["fullUrl"])

		// Unfiltered GET /Patient without _id= parameter must ONLY return pat-100
		respPat := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Patient", "")
		defer respPat.Body.Close()
		require.Equal(t, http.StatusOK, respPat.StatusCode)

		var patBundle map[string]any
		require.NoError(t, json.NewDecoder(respPat.Body).Decode(&patBundle))
		assert.Equal(t, 1, int(patBundle["total"].(float64)))
	})

	t.Run("RevInclude and Include Enforce Patient Compartment and SMART Scopes", func(t *testing.T) {
		// 1. Searching shared Practitioner with _revinclude=Observation:performer must ONLY include obs-100, never obs-200
		respRev := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Practitioner?family=House&_revinclude=Observation:performer", "")
		defer respRev.Body.Close()
		require.Equal(t, http.StatusOK, respRev.StatusCode)

		var revBundle map[string]any
		require.NoError(t, json.NewDecoder(respRev.Body).Decode(&revBundle))
		entries := revBundle["entry"].([]any)
		require.Len(t, entries, 2, "expected 1 Practitioner match + 1 authorized Observation include (obs-100 only)")
		var fullURLs []string
		for _, e := range entries {
			fullURLs = append(fullURLs, e.(map[string]any)["fullUrl"].(string))
		}
		assert.Contains(t, fullURLs, "Practitioner/prac-shared")
		assert.Contains(t, fullURLs, "Observation/obs-100")
		assert.NotContains(t, fullURLs, "Observation/obs-200")

		// 2. Token with only Practitioner scope (no Observation scope) cannot _revinclude=Observation:performer -> 403 Forbidden
		pracOnlyToken, err := mockVal.IssueToken("prac-only", "user/Practitioner.rs", "", time.Hour, nil)
		require.NoError(t, err)
		reqPracOnly, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/test-compartment/Practitioner?family=House&_revinclude=Observation:performer", nil)
		reqPracOnly.Header.Set("Authorization", "Bearer "+pracOnlyToken)
		respPracOnly, err := http.DefaultClient.Do(reqPracOnly)
		require.NoError(t, err)
		defer respPracOnly.Body.Close()
		assert.Equal(t, http.StatusForbidden, respPracOnly.StatusCode)

		// 3. Token with only Observation scope (no Patient scope) filters out Patient on untyped _include=Observation:subject
		obsOnlyToken, err := mockVal.IssueToken("obs-only", "patient/Observation.rs", "pat-100", time.Hour, nil)
		require.NoError(t, err)
		reqObsOnly, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/test-compartment/Observation?_include=Observation:subject", nil)
		reqObsOnly.Header.Set("Authorization", "Bearer "+obsOnlyToken)
		respObsOnly, err := http.DefaultClient.Do(reqObsOnly)
		require.NoError(t, err)
		defer respObsOnly.Body.Close()
		require.Equal(t, http.StatusOK, respObsOnly.StatusCode)
		var obsOnlyBundle map[string]any
		require.NoError(t, json.NewDecoder(respObsOnly.Body).Decode(&obsOnlyBundle))
		obsOnlyEntries := obsOnlyBundle["entry"].([]any)
		require.Len(t, obsOnlyEntries, 1, "expected Patient include to be stripped when caller lacks Patient read/search scope")
	})

	t.Run("Multi-Value Query Parameter Smuggling and Chained Search Bypass Are Blocked", func(t *testing.T) {
		// 1. Multi-value ?patient=pat-100&patient=pat-200 -> 403 Forbidden
		respMultiPat := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Observation?patient=pat-100&patient=pat-200", "")
		defer respMultiPat.Body.Close()
		assert.Equal(t, http.StatusForbidden, respMultiPat.StatusCode)

		// 2. Multi-value ?subject=pat-100&subject=pat-200 -> 403 Forbidden
		respMultiSub := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Observation?subject=pat-100&subject=pat-200", "")
		defer respMultiSub.Body.Close()
		assert.Equal(t, http.StatusForbidden, respMultiSub.StatusCode)

		// 3. Comma-separated ?patient=pat-100,pat-200 -> 403 Forbidden
		respCommaPat := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Observation?patient=pat-100,pat-200", "")
		defer respCommaPat.Body.Close()
		assert.Equal(t, http.StatusForbidden, respCommaPat.StatusCode)

		// 4. Multi-value ?_id=pat-100&_id=pat-200 on Patient -> 403 Forbidden
		respMultiID := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Patient?_id=pat-100&_id=pat-200", "")
		defer respMultiID.Body.Close()
		assert.Equal(t, http.StatusForbidden, respMultiID.StatusCode)

		// 5. Chained search ?patient.family=TwoHundred with pat-100 token must return 0 matches
		respChain := doPat100(http.MethodGet, "/fhir/r4/test-compartment/Observation?patient.family=TwoHundred", "")
		defer respChain.Body.Close()
		require.Equal(t, http.StatusOK, respChain.StatusCode)
		var chainBundle map[string]any
		require.NoError(t, json.NewDecoder(respChain.Body).Decode(&chainBundle))
		assert.Equal(t, 0, int(chainBundle["total"].(float64)))
	})

	t.Run("Clinical POST, PUT, DELETE, and Bundle Block Cross-Patient Writes", func(t *testing.T) {
		// 1. POST Observation referencing pat-200 -> 403 Forbidden
		respPostOther := doPat100(http.MethodPost, "/fhir/r4/test-compartment/Observation",
			`{"resourceType":"Observation","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/pat-200"}}`)
		defer respPostOther.Body.Close()
		assert.Equal(t, http.StatusForbidden, respPostOther.StatusCode)

		// 2. PUT overwriting obs-200 -> 403 Forbidden
		respPutOther := doPat100(http.MethodPut, "/fhir/r4/test-compartment/Observation/obs-200",
			`{"resourceType":"Observation","id":"obs-200","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/pat-100"}}`)
		defer respPutOther.Body.Close()
		assert.Equal(t, http.StatusForbidden, respPutOther.StatusCode)

		// 3. DELETE obs-200 -> 403 Forbidden
		respDelOther := doPat100(http.MethodDelete, "/fhir/r4/test-compartment/Observation/obs-200", "")
		defer respDelOther.Body.Close()
		assert.Equal(t, http.StatusForbidden, respDelOther.StatusCode)

		// 4. Transaction Bundle attempting to DELETE obs-200 -> 403 Forbidden
		respBundleOther := doPat100(http.MethodPost, "/fhir/r4/test-compartment", `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"request": {"method": "DELETE", "url": "Observation/obs-200"}
				}
			]
		}`)
		defer respBundleOther.Body.Close()
		assert.Equal(t, http.StatusForbidden, respBundleOther.StatusCode)

		// 5. POST /Observation with If-None-Exist: subject=Patient/pat-200 -> 403 Forbidden
		reqIfNone, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/test-compartment/Observation",
			strings.NewReader(`{"resourceType":"Observation","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/pat-100"}}`))
		reqIfNone.Header.Set("Authorization", "Bearer "+pat100Token)
		reqIfNone.Header.Set("Content-Type", "application/fhir+json")
		reqIfNone.Header.Set("If-None-Exist", "subject=Patient/pat-200&code=http://loinc.org|8867-4")
		respIfNone, err := http.DefaultClient.Do(reqIfNone)
		require.NoError(t, err)
		defer respIfNone.Body.Close()
		assert.Equal(t, http.StatusForbidden, respIfNone.StatusCode, "patient-scoped token must not probe another patient's compartment via If-None-Exist subject=Patient/pat-200")

		// 6. Transaction Bundle with ifNoneExist: "subject=Patient/pat-200&code=8867-4" -> 403 Forbidden
		respBundleIfNone := doPat100(http.MethodPost, "/fhir/r4/test-compartment", `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {
						"resourceType": "Observation",
						"status": "final",
						"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
						"subject": {"reference": "Patient/pat-100"}
					},
					"request": {
						"method": "POST",
						"url": "Observation",
						"ifNoneExist": "subject=Patient/pat-200&code=http://loinc.org|8867-4"
					}
				}
			]
		}`)
		defer respBundleIfNone.Body.Close()
		assert.Equal(t, http.StatusForbidden, respBundleIfNone.StatusCode, "bundle entry with cross-patient ifNoneExist subject must be rejected with 403")
	})

	t.Run("Clinician Reads and Writes of Clinical Resources Populate AuditEvent Patient Attribution", func(t *testing.T) {
		clinicianToken, err := mockVal.IssueToken("dr-house", "user/*.*", "", time.Hour, nil)
		require.NoError(t, err)

		// 1. Clinician reads Observation/obs-100 (no patient ID in URL or token)
		reqRead, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/test-compartment/Observation/obs-100", nil)
		reqRead.Header.Set("Authorization", "Bearer "+clinicianToken)
		respRead, err := http.DefaultClient.Do(reqRead)
		require.NoError(t, err)
		respRead.Body.Close()
		require.Equal(t, http.StatusOK, respRead.StatusCode)

		// 2. Clinician creates a new Observation for Patient/pat-100
		reqCreate, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/test-compartment/Observation",
			strings.NewReader(`{"resourceType":"Observation","id":"obs-clinician-audit-1","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/pat-100"}}`))
		reqCreate.Header.Set("Authorization", "Bearer "+clinicianToken)
		reqCreate.Header.Set("Content-Type", "application/fhir+json")
		respCreate, err := http.DefaultClient.Do(reqCreate)
		require.NoError(t, err)
		respCreate.Body.Close()
		require.Equal(t, http.StatusCreated, respCreate.StatusCode)

		// 3. Patient pat-100 queries GET /AuditEvent and sees BOTH clinician interactions in their Accounting of Disclosures
		patAuditToken, err := mockVal.IssueToken("pat-100-user", "patient/AuditEvent.rs", "pat-100", time.Hour, nil)
		require.NoError(t, err)
		reqAudit, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/test-compartment/AuditEvent?agent=dr-house", nil)
		reqAudit.Header.Set("Authorization", "Bearer "+patAuditToken)
		respAudit, err := http.DefaultClient.Do(reqAudit)
		require.NoError(t, err)
		defer respAudit.Body.Close()
		require.Equal(t, http.StatusOK, respAudit.StatusCode)
		var auditBundle map[string]any
		require.NoError(t, json.NewDecoder(respAudit.Body).Decode(&auditBundle))
		assert.GreaterOrEqual(t, int(auditBundle["total"].(float64)), 2, "expected clinician read and create on pat-100's Observation to be attributed to pat-100 in AuditEvent log")
	})

	t.Run("Patient-Scoped Token Missing patient_id Claim Fails Closed With 403 Forbidden", func(t *testing.T) {
		noClaimToken, err := mockVal.IssueToken("broken-launch-user", "patient/Patient.cruds patient/Observation.cruds", "", time.Hour, nil)
		require.NoError(t, err)

		for _, tc := range []struct {
			method string
			path   string
			body   string
		}{
			{http.MethodGet, "/fhir/r4/test-compartment/Patient/pat-100", ""},
			{http.MethodGet, "/fhir/r4/test-compartment/Observation", ""},
			{http.MethodPost, "/fhir/r4/test-compartment/Observation", `{"resourceType":"Observation","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/pat-100"}}`},
			{http.MethodPost, "/fhir/r4/test-compartment", `{"resourceType":"Bundle","type":"transaction","entry":[{"request":{"method":"DELETE","url":"Observation/obs-100"}}]}`},
		} {
			var r io.Reader
			if tc.body != "" {
				r = strings.NewReader(tc.body)
			}
			req, _ := http.NewRequest(tc.method, ts.URL+tc.path, r)
			req.Header.Set("Authorization", "Bearer "+noClaimToken)
			req.Header.Set("Content-Type", "application/fhir+json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusForbidden, resp.StatusCode, "expected 403 Forbidden for %s %s with patient/* scope and empty patient_id", tc.method, tc.path)
		}
	})

	t.Run("Patient-Scoped Token Cannot Mutate Shared Non-Compartment Practitioner Resource", func(t *testing.T) {
		patWithPracWrite, err := mockVal.IssueToken("pat-100-user", "patient/Patient.cruds patient/Practitioner.cruds", "pat-100", time.Hour, nil)
		require.NoError(t, err)

		for _, tc := range []struct {
			method string
			path   string
			body   string
		}{
			{http.MethodPost, "/fhir/r4/test-compartment/Practitioner", `{"resourceType":"Practitioner","name":[{"family":"FakeDoc"}]}`},
			{http.MethodPut, "/fhir/r4/test-compartment/Practitioner/prac-shared", `{"resourceType":"Practitioner","id":"prac-shared","name":[{"family":"Hijacked"}]}`},
			{http.MethodDelete, "/fhir/r4/test-compartment/Practitioner/prac-shared", ""},
			{http.MethodPost, "/fhir/r4/test-compartment", `{"resourceType":"Bundle","type":"transaction","entry":[{"request":{"method":"DELETE","url":"Practitioner/prac-shared"}}]}`},
		} {
			var r io.Reader
			if tc.body != "" {
				r = strings.NewReader(tc.body)
			}
			req, _ := http.NewRequest(tc.method, ts.URL+tc.path, r)
			req.Header.Set("Authorization", "Bearer "+patWithPracWrite)
			req.Header.Set("Content-Type", "application/fhir+json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusForbidden, resp.StatusCode, "patient-scoped token must not %s %s", tc.method, tc.path)
		}
	})

	t.Run("Patient/$everything Enforces Per-Resource SMART Scopes and Pagination Validation", func(t *testing.T) {
		// Token with ONLY Patient.r (no Observation or Practitioner scope)
		patOnlyToken, err := mockVal.IssueToken("pat-100-only", "patient/Patient.r", "pat-100", time.Hour, nil)
		require.NoError(t, err)

		// 1. Explicit _type=Observation without Observation scope -> 403 Forbidden
		reqType, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/test-compartment/Patient/pat-100/$everything?_type=Observation", nil)
		reqType.Header.Set("Authorization", "Bearer "+patOnlyToken)
		respType, err := http.DefaultClient.Do(reqType)
		require.NoError(t, err)
		respType.Body.Close()
		assert.Equal(t, http.StatusForbidden, respType.StatusCode)

		// 2. Unfiltered $everything with only Patient.r strips unauthorized Observation and Practitioner includes
		reqAll, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/test-compartment/Patient/pat-100/$everything", nil)
		reqAll.Header.Set("Authorization", "Bearer "+patOnlyToken)
		respAll, err := http.DefaultClient.Do(reqAll)
		require.NoError(t, err)
		defer respAll.Body.Close()
		require.Equal(t, http.StatusOK, respAll.StatusCode)
		var evBundle map[string]any
		require.NoError(t, json.NewDecoder(respAll.Body).Decode(&evBundle))
		evEntries := evBundle["entry"].([]any)
		require.Len(t, evEntries, 1, "expected only Patient/pat-100 when token lacks Observation and Practitioner scopes")
		assert.Equal(t, "Patient/pat-100", evEntries[0].(map[string]any)["fullUrl"])

		// 3. Malformed _count or _offset on $everything returns 400 Bad Request
		for _, q := range []string{"_count=-1", "_count=abc", "_offset=-5"} {
			reqBad, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/test-compartment/Patient/pat-100/$everything?"+q, nil)
			reqBad.Header.Set("Authorization", "Bearer "+patOnlyToken)
			respBad, err := http.DefaultClient.Do(reqBad)
			require.NoError(t, err)
			respBad.Body.Close()
			assert.Equal(t, http.StatusBadRequest, respBad.StatusCode)
		}
	})
}
