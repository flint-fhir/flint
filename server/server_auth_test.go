package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
}
