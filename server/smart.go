package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SMARTConfig holds the SMART App Launch configuration.
// See: https://hl7.org/fhir/smart-app-launch/conformance.html
type SMARTConfig struct {
	// AuthorizationEndpoint is the OAuth2 authorize URL (from IdP)
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	// TokenEndpoint is the OAuth2 token URL (from IdP)
	TokenEndpoint string `json:"token_endpoint"`
	// IntrospectionEndpoint is the OAuth2 RFC 7662 token introspection URL
	IntrospectionEndpoint string `json:"introspection_endpoint,omitempty"`
	// JWKSURI is the JSON Web Key Set URL
	JWKSURI string `json:"jwks_uri,omitempty"`
	// Issuer is the FHIR server base URL
	Issuer string `json:"issuer"`
}

// authCodeEntry stores an ephemeral OAuth2 authorization code and PKCE challenge.
type authCodeEntry struct {
	ClientID            string
	RedirectURI         string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	PatientID           string
	EncounterID         string
	Tenant              string
	ExpiresAt           time.Time
}

// SetSMARTConfig configures the SMART App Launch endpoints.
// In production, these point to your IdP (Keycloak/Zitadel).
func (s *Server) SetSMARTConfig(cfg SMARTConfig) {
	s.smartConfig = &cfg
}

func (s *Server) resolveSMARTConfig(r *http.Request) SMARTConfig {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost:8080"
	}
	tenant := r.PathValue("tenant")
	if tenant == "" {
		tenant = "default"
	}
	baseOrigin := scheme + "://" + host
	defaultCfg := SMARTConfig{
		Issuer:                baseOrigin + "/fhir/r4/" + tenant,
		AuthorizationEndpoint: baseOrigin + "/oauth2/authorize",
		TokenEndpoint:         baseOrigin + "/oauth2/token",
		IntrospectionEndpoint: baseOrigin + "/oauth2/introspect",
		JWKSURI:               baseOrigin + "/fhir/r4/" + tenant + "/.well-known/jwks.json",
	}
	if s.smartConfig == nil {
		return defaultCfg
	}
	out := *s.smartConfig
	if out.Issuer == "" {
		out.Issuer = defaultCfg.Issuer
	}
	if out.AuthorizationEndpoint == "" {
		out.AuthorizationEndpoint = defaultCfg.AuthorizationEndpoint
	}
	if out.TokenEndpoint == "" {
		out.TokenEndpoint = defaultCfg.TokenEndpoint
	}
	if out.IntrospectionEndpoint == "" {
		out.IntrospectionEndpoint = defaultCfg.IntrospectionEndpoint
	}
	if out.JWKSURI == "" {
		out.JWKSURI = defaultCfg.JWKSURI
	}
	return out
}

// handleSMARTConfig serves /.well-known/smart-configuration.
// This is the discovery document that SMART apps and ONC (g)(10) test kits use to find auth endpoints.
func (s *Server) handleSMARTConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.resolveSMARTConfig(r)

	config := map[string]any{
		"issuer":                 cfg.Issuer,
		"jwks_uri":               cfg.JWKSURI,
		"authorization_endpoint": cfg.AuthorizationEndpoint,
		"token_endpoint":         cfg.TokenEndpoint,
		"introspection_endpoint": cfg.IntrospectionEndpoint,
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
			// SMART v2 granular scopes
			"patient/Patient.rs",
			"patient/Observation.rs",
			"patient/Condition.rs",
			"patient/Encounter.rs",
			"patient/MedicationRequest.rs",
			"patient/AllergyIntolerance.rs",
			"patient/Procedure.rs",
			"patient/DiagnosticReport.rs",
			"patient/Immunization.rs",
			"patient/Practitioner.rs",
			"patient/Organization.rs",
			"patient/*.rs",
			"user/*.cruds",
			"system/*.rs",
			// SMART v1 scopes
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
			"user/Patient.read",
			"user/Patient.write",
			"user/*.read",
			"user/*.write",
			"system/Patient.read",
			"system/*.read",
			// Launch
			"launch",
			"launch/patient",
			"launch/encounter",
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
			"context-ehr-encounter",
			"context-standalone-patient",
			"permission-offline",
			"permission-patient",
			"permission-user",
			"permission-v1",
			"permission-v2",
			"authorize-post",
			"sso-openid-connect",
		},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(config)
}

// handleOIDCConfig serves /.well-known/openid-configuration for SMART sso-openid-connect conformance.
func (s *Server) handleOIDCConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.resolveSMARTConfig(r)
	doc := map[string]any{
		"issuer":                                cfg.Issuer,
		"authorization_endpoint":                cfg.AuthorizationEndpoint,
		"token_endpoint":                        cfg.TokenEndpoint,
		"introspection_endpoint":                cfg.IntrospectionEndpoint,
		"jwks_uri":                              cfg.JWKSURI,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "client_credentials", "refresh_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "fhirUser", "launch", "launch/patient", "offline_access"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "private_key_jwt"},
		"claims_supported":                      []string{"sub", "iss", "aud", "exp", "iat", "fhirUser", "patient"},
		"code_challenge_methods_supported":      []string{"S256"},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(doc)
}

// handleJWKS serves /.well-known/jwks.json containing the server's public signing keys.
func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if s.devAuth != nil {
		json.NewEncoder(w).Encode(s.devAuth.JWKS())
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
}

// handleOAuthAuthorize implements GET/POST /oauth2/authorize for SMART App Launch v2.0.0 with PKCE S256.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	responseType := r.FormValue("response_type")
	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	scope := r.FormValue("scope")
	state := r.FormValue("state")
	codeChallenge := r.FormValue("code_challenge")
	codeChallengeMethod := r.FormValue("code_challenge_method")
	patientID := r.FormValue("patient")
	if patientID == "" {
		patientID = r.FormValue("launch")
	}
	if patientID == "" {
		patientID = "pat-alice-001"
	}
	encounterID := r.FormValue("encounter")
	if encounterID == "" {
		encounterID = "enc-alice-001"
	}

	if redirectURI == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing redirect_uri")
		return
	}
	if responseType != "" && responseType != "code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	if codeChallenge != "" && codeChallengeMethod != "" && !strings.EqualFold(codeChallengeMethod, "S256") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "SMART v2 requires code_challenge_method=S256")
		return
	}

	code := "flint-code-" + uuid.NewString()
	s.oauthMu.Lock()
	s.oauthCodes[code] = authCodeEntry{
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		Scope:               scope,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		PatientID:           patientID,
		EncounterID:         encounterID,
		ExpiresAt:           time.Now().Add(5 * time.Minute),
	}
	s.oauthMu.Unlock()

	u, err := url.Parse(redirectURI)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed redirect_uri")
		return
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// handleOAuthToken implements POST /oauth2/token for SMART App Launch v2.0.0 and Backend Services.
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if s.devAuth == nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "OAuth signer not initialized")
		return
	}
	_ = r.ParseForm()
	grantType := r.FormValue("grant_type")

	switch grantType {
	case "authorization_code":
		code := r.FormValue("code")
		codeVerifier := r.FormValue("code_verifier")

		s.oauthMu.Lock()
		entry, ok := s.oauthCodes[code]
		if ok {
			delete(s.oauthCodes, code)
		}
		s.oauthMu.Unlock()

		if !ok || time.Now().After(entry.ExpiresAt) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
			return
		}

		// Verify PKCE S256 if code_challenge was provided during authorize
		if entry.CodeChallenge != "" {
			if codeVerifier == "" {
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "missing PKCE code_verifier")
				return
			}
			sum := sha256.Sum256([]byte(codeVerifier))
			computed := base64.RawURLEncoding.EncodeToString(sum[:])
			if computed != entry.CodeChallenge {
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE S256 code_verifier mismatch")
				return
			}
		}

		scope := entry.Scope
		if scope == "" {
			scope = "patient/Patient.rs patient/Observation.rs openid fhirUser"
		}
		sub := "Practitioner/prac-smith-001"
		if strings.Contains(scope, "patient/") {
			sub = "Patient/" + entry.PatientID
		}
		extra := map[string]any{
			"fhirUser":  sub,
			"encounter": entry.EncounterID,
		}
		accessToken, err := s.devAuth.IssueToken(sub, scope, entry.PatientID, time.Hour, extra)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}

		resp := map[string]any{
			"access_token":           accessToken,
			"token_type":             "Bearer",
			"expires_in":             3600,
			"scope":                  scope,
			"patient":                entry.PatientID,
			"encounter":              entry.EncounterID,
			"need_patient_banner":    true,
			"smart_style_url":        "https://hl7.org/fhir/smart-app-launch/smart-styles.json",
			"id_token":               accessToken,
			"refresh_token":          "flint-rt-" + uuid.NewString(),
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)

	case "client_credentials":
		scope := r.FormValue("scope")
		if scope == "" {
			scope = "system/*.rs"
		}
		accessToken, err := s.devAuth.IssueToken("system-bulk-client", scope, "", time.Hour, nil)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		resp := map[string]any{
			"access_token": accessToken,
			"token_type":   "Bearer",
			"expires_in":   3600,
			"scope":        scope,
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)

	case "refresh_token":
		scope := r.FormValue("scope")
		if scope == "" {
			scope = "patient/*.rs openid fhirUser offline_access"
		}
		accessToken, err := s.devAuth.IssueToken("Patient/pat-alice-001", scope, "pat-alice-001", time.Hour, map[string]any{
			"fhirUser": "Patient/pat-alice-001",
		})
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		resp := map[string]any{
			"access_token":  accessToken,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         scope,
			"patient":       "pat-alice-001",
			"id_token":      accessToken,
			"refresh_token": "flint-rt-" + uuid.NewString(),
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)

	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "supported grant_types: authorization_code, client_credentials, refresh_token")
	}
}

// handleOAuthIntrospect implements POST /oauth2/introspect (RFC 7662).
func (s *Server) handleOAuthIntrospect(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rawToken := strings.TrimSpace(r.FormValue("token"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if rawToken == "" {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"active": false})
		return
	}

	validator := s.tokenValidator
	if validator == nil {
		validator = s.devAuth
	}
	if validator == nil {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"active": false})
		return
	}

	secCtx, err := validator.ValidateToken(r.Context(), rawToken)
	if err != nil || secCtx == nil {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"active": false})
		return
	}

	var rawScopes []string
	for _, sc := range secCtx.GetScopes() {
		rawScopes = append(rawScopes, sc.GetRaw())
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"active":     true,
		"sub":        secCtx.GetSubject(),
		"scope":      strings.Join(rawScopes, " "),
		"patient":    secCtx.GetPatientId(),
		"encounter":  secCtx.GetEncounterId(),
		"token_type": "Bearer",
	})
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

