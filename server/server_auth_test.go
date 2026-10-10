package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
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
}
