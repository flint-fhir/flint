package server

import (
	"encoding/json"
	"net/http"
)

// SMARTConfig holds the SMART App Launch configuration.
// See: https://hl7.org/fhir/smart-app-launch/conformance.html
type SMARTConfig struct {
	// AuthorizationEndpoint is the OAuth2 authorize URL (from IdP)
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	// TokenEndpoint is the OAuth2 token URL (from IdP)
	TokenEndpoint string `json:"token_endpoint"`
	// Issuer is the FHIR server base URL
	Issuer string `json:"issuer"`
}

// SetSMARTConfig configures the SMART App Launch endpoints.
// In production, these point to your IdP (Keycloak/Zitadel).
func (s *Server) SetSMARTConfig(cfg SMARTConfig) {
	s.smartConfig = &cfg
}

// handleSMARTConfig serves /.well-known/smart-configuration.
// This is the discovery document that SMART apps use to find auth endpoints.
func (s *Server) handleSMARTConfig(w http.ResponseWriter, r *http.Request) {
	if s.smartConfig == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			"SMART configuration not set")
		return
	}

	config := map[string]any{
		"issuer":                 s.smartConfig.Issuer,
		"authorization_endpoint": s.smartConfig.AuthorizationEndpoint,
		"token_endpoint":         s.smartConfig.TokenEndpoint,
		"token_endpoint_auth_methods_supported": []string{
			"client_secret_basic",
			"client_secret_post",
			"private_key_jwt",
		},
		"grant_types_supported": []string{
			"authorization_code",
			"client_credentials",
		},
		"scopes_supported": []string{
			// Patient-level scopes
			"patient/Patient.read",
			"patient/Patient.write",
			"patient/Observation.read",
			"patient/Condition.read",
			"patient/Encounter.read",
			"patient/MedicationRequest.read",
			"patient/AllergyIntolerance.read",
			"patient/Procedure.read",
			"patient/DiagnosticReport.read",
			"patient/Immunization.read",
			// User-level scopes
			"user/Patient.read",
			"user/Patient.write",
			"user/*.read",
			"user/*.write",
			// System-level scopes
			"system/Patient.read",
			"system/*.read",
			// Launch
			"launch",
			"launch/patient",
			// OpenID Connect
			"openid",
			"fhirUser",
			"profile",
			"offline_access",
		},
		"response_types_supported": []string{"code"},
		"code_challenge_methods_supported": []string{
			"S256", // PKCE required by SMART 2.0
		},
		"capabilities": []string{
			"launch-ehr",
			"launch-standalone",
			"client-public",
			"client-confidential-symmetric",
			"client-confidential-asymmetric",
			"context-ehr-patient",
			"context-standalone-patient",
			"permission-patient",
			"permission-user",
			"permission-v2",
			"sso-openid-connect",
		},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(config)
}
