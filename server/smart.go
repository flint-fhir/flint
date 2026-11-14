package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/store"
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
	// RevocationEndpoint is the OAuth2 RFC 7009 token revocation URL
	RevocationEndpoint string `json:"revocation_endpoint,omitempty"`
	// RegistrationEndpoint is the OAuth2 RFC 7591 dynamic client registration URL
	RegistrationEndpoint string `json:"registration_endpoint,omitempty"`
	// JWKSURI is the JSON Web Key Set URL
	JWKSURI string `json:"jwks_uri,omitempty"`
	// Issuer is the FHIR server base URL
	Issuer string `json:"issuer"`
}

const maxOAuthEntries = 4096

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
	Nonce               string
	ExpiresAt           time.Time
}

// refreshTokenEntry stores an issued OAuth2 refresh token and its original grant context.
type refreshTokenEntry struct {
	Subject     string
	ClientID    string
	PatientID   string
	EncounterID string
	Tenant      string
	Nonce       string
	Scope       string
	ExpiresAt   time.Time
}

// launchContextEntry stores an ephemeral EHR launch session context (patient, encounter, tenant).
type launchContextEntry struct {
	PatientID   string
	EncounterID string
	Tenant      string
	ExpiresAt   time.Time
}

// RegisterOAuthClientJWK registers a trusted public JWK for a SMART client_id.
func (s *Server) RegisterOAuthClientJWK(clientID string, jwk auth.JWK) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.clientJWKs == nil {
		s.clientJWKs = make(map[string]auth.JWK)
	}
	s.clientJWKs[strings.TrimSpace(clientID)] = jwk
}

// RegisterOAuthClientJWKSet registers a trusted RFC 7517 JWKSet (containing one or more
// RSA/EC public signing keys) for a SMART client_id.
func (s *Server) RegisterOAuthClientJWKSet(clientID string, jwks auth.JWKSet) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.clientJWKSets == nil {
		s.clientJWKSets = make(map[string]auth.JWKSet)
	}
	s.clientJWKSets[strings.TrimSpace(clientID)] = jwks
}

// RegisterOAuthClientSecret registers a shared secret for a confidential symmetric OAuth2 client.
func (s *Server) RegisterOAuthClientSecret(clientID, clientSecret string) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.clientSecrets == nil {
		s.clientSecrets = make(map[string]string)
	}
	s.clientSecrets[strings.TrimSpace(clientID)] = clientSecret
}

// SetRequireRegisteredClientJWK configures whether private_key_jwt assertions
// must match a key pre-registered via RegisterOAuthClientJWK / RegisterOAuthClientJWKSet.
func (s *Server) SetRequireRegisteredClientJWK(require bool) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	s.requireRegisteredClientJWK = require
}

// SetRequirePKCE configures whether /oauth2/authorize requires a PKCE S256 code_challenge
// on all authorization requests (even when the SMART aud parameter is omitted).
func (s *Server) SetRequirePKCE(require bool) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	s.requirePKCE = require
}

func (s *Server) isPKCERequired() bool {
	s.oauthMu.Lock()
	req := s.requirePKCE
	s.oauthMu.Unlock()
	if req {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("FLINT_SMART_REQUIRE_PKCE")), "true")
}

// RegisterLaunchContext registers an EHR launch token mapped to a specific patient, encounter, and tenant.
func (s *Server) RegisterLaunchContext(launchID, patientID, encounterID, tenant string) {
	id := strings.TrimSpace(launchID)
	if id == "" {
		return
	}
	s.storeLaunchContext(id, launchContextEntry{
		PatientID:   strings.TrimSpace(patientID),
		EncounterID: strings.TrimSpace(encounterID),
		Tenant:      strings.TrimSpace(tenant),
		ExpiresAt:   time.Now().Add(15 * time.Minute),
	})
}

func (s *Server) storeLaunchContext(launchID string, entry launchContextEntry) {
	now := time.Now()
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.launchContexts == nil {
		s.launchContexts = make(map[string]launchContextEntry)
	}
	for k, v := range s.launchContexts {
		if now.After(v.ExpiresAt) {
			delete(s.launchContexts, k)
		}
	}
	if len(s.launchContexts) >= maxOAuthEntries {
		var oldestKey string
		var oldestExp time.Time
		for k, v := range s.launchContexts {
			if oldestKey == "" || v.ExpiresAt.Before(oldestExp) {
				oldestKey = k
				oldestExp = v.ExpiresAt
			}
		}
		if oldestKey != "" {
			delete(s.launchContexts, oldestKey)
		}
	}
	s.launchContexts[launchID] = entry
}

func (s *Server) resolveLaunchContext(launchID string) (launchContextEntry, bool) {
	if launchID == "" {
		return launchContextEntry{}, false
	}
	now := time.Now()
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.launchContexts == nil {
		return launchContextEntry{}, false
	}
	entry, ok := s.launchContexts[launchID]
	if !ok {
		return launchContextEntry{}, false
	}
	if now.After(entry.ExpiresAt) {
		delete(s.launchContexts, launchID)
		return launchContextEntry{}, false
	}
	return entry, true
}

// handleOAuthLaunch implements POST /oauth2/launch for minting EHR launch context tokens
// used by SMART App Launch STU2 EHR launch sequences.
func (s *Server) handleOAuthLaunch(w http.ResponseWriter, r *http.Request) {
	var reqBody struct {
		Launch    string `json:"launch"`
		Patient   string `json:"patient"`
		Encounter string `json:"encounter"`
		Tenant    string `json:"tenant"`
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "json") {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&reqBody); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed JSON launch context request")
			return
		}
	} else {
		_ = r.ParseForm()
		reqBody.Launch = r.FormValue("launch")
		reqBody.Patient = r.FormValue("patient")
		reqBody.Encounter = r.FormValue("encounter")
		reqBody.Tenant = r.FormValue("tenant")
	}

	launchID := strings.TrimSpace(reqBody.Launch)
	if launchID == "" {
		launchID = "flint-launch-" + uuid.NewString()
	}
	patientID := cleanID(strings.TrimSpace(reqBody.Patient))
	if patientID == "" {
		patientID = "pat-alice-001"
	}
	encounterID := cleanID(strings.TrimSpace(reqBody.Encounter))
	if encounterID == "" {
		encounterID = "enc-alice-001"
	}
	tenant := strings.TrimSpace(reqBody.Tenant)

	s.RegisterLaunchContext(launchID, patientID, encounterID, tenant)

	resp := map[string]any{
		"launch":    launchID,
		"patient":   patientID,
		"encounter": encounterID,
	}
	if tenant != "" {
		resp["tenant"] = tenant
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) storeAuthCode(code string, entry authCodeEntry) {
	if s.store != nil {
		_ = s.store.SaveAuthCode(context.Background(), code, store.OAuthAuthCodeRecord{
			ClientID:            entry.ClientID,
			RedirectURI:         entry.RedirectURI,
			Scope:               entry.Scope,
			CodeChallenge:       entry.CodeChallenge,
			CodeChallengeMethod: entry.CodeChallengeMethod,
			PatientID:           entry.PatientID,
			EncounterID:         entry.EncounterID,
			TenantID:            entry.Tenant,
			Nonce:               entry.Nonce,
			ExpiresAt:           entry.ExpiresAt,
		})
	}
	now := time.Now()
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.oauthCodes == nil {
		s.oauthCodes = make(map[string]authCodeEntry)
	}
	for k, v := range s.oauthCodes {
		if now.After(v.ExpiresAt) {
			delete(s.oauthCodes, k)
		}
	}
	if len(s.oauthCodes) >= maxOAuthEntries {
		var oldestKey string
		var oldestExp time.Time
		for k, v := range s.oauthCodes {
			if oldestKey == "" || v.ExpiresAt.Before(oldestExp) {
				oldestKey = k
				oldestExp = v.ExpiresAt
			}
		}
		if oldestKey != "" {
			delete(s.oauthCodes, oldestKey)
		}
	}
	s.oauthCodes[code] = entry
}

func (s *Server) consumeAuthCode(code string) (authCodeEntry, bool) {
	if s.store != nil {
		if rec, ok, err := s.store.ConsumeAuthCode(context.Background(), code); err == nil && ok {
			s.oauthMu.Lock()
			if s.oauthCodes != nil {
				delete(s.oauthCodes, code)
			}
			s.oauthMu.Unlock()
			return authCodeEntry{
				ClientID:            rec.ClientID,
				RedirectURI:         rec.RedirectURI,
				Scope:               rec.Scope,
				CodeChallenge:       rec.CodeChallenge,
				CodeChallengeMethod: rec.CodeChallengeMethod,
				PatientID:           rec.PatientID,
				EncounterID:         rec.EncounterID,
				Tenant:              rec.TenantID,
				Nonce:               rec.Nonce,
				ExpiresAt:           rec.ExpiresAt,
			}, true
		}
	}
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.oauthCodes == nil {
		return authCodeEntry{}, false
	}
	entry, ok := s.oauthCodes[code]
	if ok {
		delete(s.oauthCodes, code)
	}
	return entry, ok
}

func (s *Server) storeRefreshToken(token string, entry refreshTokenEntry) {
	if s.store != nil {
		_ = s.store.SaveRefreshToken(context.Background(), token, store.OAuthRefreshTokenRecord{
			Subject:     entry.Subject,
			PatientID:   entry.PatientID,
			EncounterID: entry.EncounterID,
			Scope:       entry.Scope,
			ExpiresAt:   entry.ExpiresAt,
		})
	}
	now := time.Now()
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.refreshTokens == nil {
		s.refreshTokens = make(map[string]refreshTokenEntry)
	}
	for k, v := range s.refreshTokens {
		if now.After(v.ExpiresAt) {
			delete(s.refreshTokens, k)
		}
	}
	if len(s.refreshTokens) >= maxOAuthEntries {
		var oldestKey string
		var oldestExp time.Time
		for k, v := range s.refreshTokens {
			if oldestKey == "" || v.ExpiresAt.Before(oldestExp) {
				oldestKey = k
				oldestExp = v.ExpiresAt
			}
		}
		if oldestKey != "" {
			delete(s.refreshTokens, oldestKey)
		}
	}
	s.refreshTokens[token] = entry
}

func (s *Server) consumeRefreshToken(token string) (refreshTokenEntry, bool) {
	if s.store != nil {
		if rec, ok, err := s.store.ConsumeRefreshToken(context.Background(), token); err == nil && ok {
			var memEntry refreshTokenEntry
			s.oauthMu.Lock()
			if s.refreshTokens != nil {
				memEntry = s.refreshTokens[token]
				delete(s.refreshTokens, token)
			}
			s.oauthMu.Unlock()
			return refreshTokenEntry{
				Subject:     rec.Subject,
				ClientID:    memEntry.ClientID,
				PatientID:   rec.PatientID,
				EncounterID: rec.EncounterID,
				Tenant:      memEntry.Tenant,
				Nonce:       memEntry.Nonce,
				Scope:       rec.Scope,
				ExpiresAt:   rec.ExpiresAt,
			}, true
		}
	}
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.refreshTokens == nil {
		return refreshTokenEntry{}, false
	}
	entry, ok := s.refreshTokens[token]
	if ok {
		delete(s.refreshTokens, token)
	}
	return entry, ok
}

func isScopeSubset(requested, granted string) bool {
	allowed := make(map[string]bool)
	for tok := range strings.FieldsSeq(granted) {
		allowed[tok] = true
	}
	for req := range strings.FieldsSeq(requested) {
		if !allowed[req] {
			return false
		}
	}
	return true
}

// SetSMARTConfig configures the SMART App Launch endpoints.
// In production, these point to your IdP (Keycloak/Zitadel).
func (s *Server) SetSMARTConfig(cfg SMARTConfig) {
	s.smartConfig = &cfg
}

func (s *Server) resolveSMARTConfig(r *http.Request) SMARTConfig {
	return s.resolveSMARTConfigForTenant(r, r.PathValue("tenant"))
}

func (s *Server) resolveSMARTConfigForTenant(r *http.Request, explicitTenant string) SMARTConfig {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost:8080"
	}
	tenant := strings.TrimSpace(explicitTenant)
	if tenant == "" {
		tenant = strings.TrimSpace(r.PathValue("tenant"))
	}
	if tenant == "" {
		tenant = "default"
	}
	baseOrigin := scheme + "://" + host
	defaultCfg := SMARTConfig{
		Issuer:                baseOrigin + "/fhir/r4/" + tenant,
		AuthorizationEndpoint: baseOrigin + "/oauth2/authorize",
		TokenEndpoint:         baseOrigin + "/oauth2/token",
		IntrospectionEndpoint: baseOrigin + "/oauth2/introspect",
		RevocationEndpoint:    baseOrigin + "/oauth2/revoke",
		RegistrationEndpoint:  baseOrigin + "/oauth2/register",
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
	if out.RevocationEndpoint == "" {
		out.RevocationEndpoint = defaultCfg.RevocationEndpoint
	}
	if out.RegistrationEndpoint == "" {
		out.RegistrationEndpoint = defaultCfg.RegistrationEndpoint
	}
	if out.JWKSURI == "" {
		out.JWKSURI = defaultCfg.JWKSURI
	}
	return out
}

func (s *Server) extractTenantFromAud(rawAud string) (string, bool) {
	trimmed := strings.TrimSpace(rawAud)
	if trimmed == "" {
		return "", true
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	if s.smartConfig != nil && s.smartConfig.Issuer != "" &&
		strings.EqualFold(strings.TrimRight(trimmed, "/"), strings.TrimRight(s.smartConfig.Issuer, "/")) {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i := 0; i+2 < len(parts); i++ {
			if parts[i] == "fhir" && parts[i+1] == "r4" && parts[i+2] != "" {
				return parts[i+2], true
			}
		}
		return "default", true
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "fhir" && parts[i+1] == "r4" {
			if i+2 < len(parts) && parts[i+2] != "" {
				return parts[i+2], true
			}
			return "default", true
		}
	}
	return "", false
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
		"revocation_endpoint":    cfg.RevocationEndpoint,
		"registration_endpoint":  cfg.RegistrationEndpoint,
		"token_endpoint_auth_methods_supported": []string{
			"client_secret_basic",
			"client_secret_post",
			"private_key_jwt",
		},
		"token_endpoint_auth_signing_alg_values_supported": []string{
			"RS256",
			"RS384",
			"ES256",
			"ES384",
		},
		"grant_types_supported": []string{
			"authorization_code",
			"client_credentials",
			"refresh_token",
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
	_ = json.NewEncoder(w).Encode(config)
}

// handleOIDCConfig serves /.well-known/openid-configuration for SMART sso-openid-connect conformance.
func (s *Server) handleOIDCConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.resolveSMARTConfig(r)
	doc := map[string]any{
		"issuer":                                           cfg.Issuer,
		"authorization_endpoint":                           cfg.AuthorizationEndpoint,
		"token_endpoint":                                   cfg.TokenEndpoint,
		"introspection_endpoint":                           cfg.IntrospectionEndpoint,
		"jwks_uri":                                         cfg.JWKSURI,
		"response_types_supported":                         []string{"code"},
		"grant_types_supported":                            []string{"authorization_code", "client_credentials", "refresh_token"},
		"subject_types_supported":                          []string{"public"},
		"id_token_signing_alg_values_supported":            []string{"RS256"},
		"scopes_supported":                                 []string{"openid", "profile", "fhirUser", "launch", "launch/patient", "offline_access"},
		"token_endpoint_auth_methods_supported":            []string{"client_secret_basic", "client_secret_post", "private_key_jwt"},
		"token_endpoint_auth_signing_alg_values_supported": []string{"RS256", "RS384", "ES256", "ES384"},
		"claims_supported":                                 []string{"sub", "iss", "aud", "exp", "iat", "fhirUser", "patient", "encounter"},
		"code_challenge_methods_supported":                 []string{"S256"},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(doc)
}

// handleJWKS serves /.well-known/jwks.json containing the server's public signing keys.
func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if s.devAuth != nil {
		_ = json.NewEncoder(w).Encode(s.devAuth.JWKS())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
}

// handleOAuthAuthorize implements GET/POST /oauth2/authorize for SMART App Launch v2.0.0 with PKCE S256.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	responseType := strings.TrimSpace(r.FormValue("response_type"))
	clientID := strings.TrimSpace(r.FormValue("client_id"))
	redirectURI := strings.TrimSpace(r.FormValue("redirect_uri"))
	scope := strings.TrimSpace(r.FormValue("scope"))
	state := r.FormValue("state")
	nonce := r.FormValue("nonce")
	rawAud := strings.TrimSpace(r.FormValue("aud"))
	codeChallenge := strings.TrimSpace(r.FormValue("code_challenge"))
	codeChallengeMethod := strings.TrimSpace(r.FormValue("code_challenge_method"))
	patientID := cleanID(strings.TrimSpace(r.FormValue("patient")))
	encounterID := cleanID(strings.TrimSpace(r.FormValue("encounter")))
	launchParam := strings.TrimSpace(r.FormValue("launch"))

	if redirectURI == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing redirect_uri")
		return
	}
	u, err := url.Parse(redirectURI)
	if err != nil || u.Scheme == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed redirect_uri")
		return
	}
	if responseType != "" && responseType != "code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_response_type", "only response_type=code is supported")
		return
	}

	tenant, audOK := s.extractTenantFromAud(rawAud)
	if !audOK {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid SMART aud parameter")
		return
	}
	if tenant == "" {
		tenant = strings.TrimSpace(r.PathValue("tenant"))
	}

	// SMART App Launch STU2 requires PKCE S256; reject plain or missing code_challenge_method whenever
	// code_challenge is supplied, and require PKCE whenever aud is supplied or strict PKCE is enabled.
	if codeChallenge != "" && !strings.EqualFold(codeChallengeMethod, "S256") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "SMART v2 requires code_challenge_method=S256")
		return
	}
	if codeChallenge == "" && (s.isPKCERequired() || rawAud != "") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "SMART v2 requires PKCE code_challenge with code_challenge_method=S256")
		return
	}

	if launchParam != "" {
		if lc, ok := s.resolveLaunchContext(launchParam); ok {
			if patientID == "" && lc.PatientID != "" {
				patientID = lc.PatientID
			}
			if encounterID == "" && lc.EncounterID != "" {
				encounterID = lc.EncounterID
			}
			if tenant == "" && lc.Tenant != "" {
				tenant = lc.Tenant
			}
		} else if patientID == "" && s.store != nil {
			lookupTenant := tenant
			if lookupTenant == "" {
				lookupTenant = "default"
			}
			if _, err := s.store.ReadResourceWithMeta(r.Context(), lookupTenant, "Patient", launchParam); err == nil {
				patientID = launchParam
			}
		}
	}
	if patientID == "" {
		patientID = "pat-alice-001"
	}
	if encounterID == "" {
		encounterID = "enc-alice-001"
	}

	code := "flint-code-" + uuid.NewString()
	s.storeAuthCode(code, authCodeEntry{
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		Scope:               scope,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		PatientID:           patientID,
		EncounterID:         encounterID,
		Tenant:              tenant,
		Nonce:               nonce,
		ExpiresAt:           time.Now().Add(5 * time.Minute),
	})

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

// authenticateOAuthClient authenticates a SMART OAuth2 client at /oauth2/token,
// /oauth2/introspect, or /oauth2/revoke supporting private_key_jwt, client_secret_basic,
// client_secret_post, and public clients.
func (s *Server) authenticateOAuthClient(w http.ResponseWriter, r *http.Request) (string, bool) {
	assertionType := strings.TrimSpace(r.FormValue("client_assertion_type"))
	assertion := strings.TrimSpace(r.FormValue("client_assertion"))
	if assertionType != "" || assertion != "" {
		if assertionType != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" || assertion == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion_type must be urn:ietf:params:oauth:client-assertion-type:jwt-bearer with a non-empty client_assertion")
			return "", false
		}
		parsed, err := auth.ParseJWT(assertion)
		if err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "malformed client_assertion JWT: "+err.Error())
			return "", false
		}
		switch parsed.Header.Alg {
		case "RS256", "RS384", "ES256", "ES384":
		default:
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "unsupported client_assertion signing algorithm: "+parsed.Header.Alg)
			return "", false
		}
		iss := strings.TrimSpace(parsed.Payload.Issuer)
		sub := strings.TrimSpace(parsed.Payload.Subject)
		if iss == "" || sub == "" || iss != sub {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion iss and sub must be non-empty and identical")
			return "", false
		}
		if formClientID := strings.TrimSpace(r.FormValue("client_id")); formClientID != "" && formClientID != iss {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_id does not match client_assertion iss")
			return "", false
		}
		cfg := s.resolveSMARTConfig(r)
		audValid := false
		for _, aud := range parsed.Payload.Audience {
			trimmedAud := strings.TrimRight(strings.TrimSpace(aud), "/")
			if trimmedAud != "" && (trimmedAud == strings.TrimRight(cfg.TokenEndpoint, "/") ||
				trimmedAud == strings.TrimRight(cfg.IntrospectionEndpoint, "/") ||
				trimmedAud == strings.TrimRight(cfg.RevocationEndpoint, "/") ||
				trimmedAud == strings.TrimRight(cfg.Issuer, "/") ||
				strings.HasSuffix(trimmedAud, "/oauth2/token") ||
				strings.HasSuffix(trimmedAud, "/oauth2/introspect") ||
				strings.HasSuffix(trimmedAud, "/oauth2/revoke")) {
				audValid = true
				break
			}
		}
		if !audValid {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion aud must identify the OAuth2 endpoint")
			return "", false
		}
		jti := strings.TrimSpace(parsed.Payload.JTI)
		if jti == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion missing required jti claim")
			return "", false
		}
		nowUnix := time.Now().Unix()
		if parsed.Payload.ExpiresAt <= nowUnix {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion has expired")
			return "", false
		}
		if parsed.Payload.ExpiresAt-nowUnix > 300 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion exp must not exceed 5 minutes in the future")
			return "", false
		}
		if parsed.Payload.NotBefore != 0 && parsed.Payload.NotBefore > nowUnix+60 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion nbf is in the future")
			return "", false
		}
		if len(parsed.RawSignature) < 64 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "invalid client_assertion signature")
			return "", false
		}

		s.oauthMu.Lock()
		regJWKSet, hasRegJWKSet := s.clientJWKSets[iss]
		regJWK, hasRegJWK := s.clientJWKs[iss]
		requireReg := s.requireRegisteredClientJWK
		s.oauthMu.Unlock()

		if hasRegJWKSet {
			verified := false
			if jwk, err := regJWKSet.FindKey(parsed.Header.Kid); err == nil {
				if pubKey, pkErr := jwk.PublicKey(); pkErr == nil && parsed.VerifySignature(pubKey) == nil {
					verified = true
				}
			} else if parsed.Header.Kid == "" {
				for i := range regJWKSet.Keys {
					if pubKey, pkErr := regJWKSet.Keys[i].PublicKey(); pkErr == nil && parsed.VerifySignature(pubKey) == nil {
						verified = true
						break
					}
				}
			}
			if !verified {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion signature verification failed against registered client JWKS")
				return "", false
			}
		} else if hasRegJWK {
			pubKey, err := regJWK.PublicKey()
			if err != nil {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client", "invalid registered JWK for client: "+err.Error())
				return "", false
			}
			if err := parsed.VerifySignature(pubKey); err != nil {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion signature verification failed against registered client JWK")
				return "", false
			}
		} else if requireReg {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_id has no registered public key JWK")
			return "", false
		} else if parsed.Header.Jwk != nil {
			pubKey, err := parsed.Header.Jwk.PublicKey()
			if err != nil {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client", "invalid JWK in client_assertion header: "+err.Error())
				return "", false
			}
			if err := parsed.VerifySignature(pubKey); err != nil {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion signature verification failed")
				return "", false
			}
		} else if s.devAuth != nil {
			if jwk, err := s.devAuth.JWKS().FindKey(parsed.Header.Kid); err == nil {
				if pubKey, err := jwk.PublicKey(); err == nil {
					if err := parsed.VerifySignature(pubKey); err != nil {
						writeOAuthError(w, http.StatusBadRequest, "invalid_client", "client_assertion signature verification failed")
						return "", false
					}
				}
			}
		}

		jtiKey := "jti:" + jti
		if s.isTokenRevoked(jtiKey) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_assertion jti has already been used (replay detected)")
			return "", false
		}
		s.revokeTokenInternal(r.Context(), jtiKey, 10*time.Minute)
		return iss, true
	}

	if basicUser, basicPass, hasBasic := r.BasicAuth(); hasBasic {
		clientID := strings.TrimSpace(basicUser)
		if clientID == "" || basicPass == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="Flint OAuth2"`)
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid HTTP Basic client credentials")
			return "", false
		}
		if formClientID := strings.TrimSpace(r.FormValue("client_id")); formClientID != "" && formClientID != clientID {
			w.Header().Set("WWW-Authenticate", `Basic realm="Flint OAuth2"`)
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "Basic auth client_id does not match form client_id")
			return "", false
		}
		s.oauthMu.Lock()
		regSecret, hasSecret := s.clientSecrets[clientID]
		s.oauthMu.Unlock()
		if hasSecret && subtle.ConstantTimeCompare([]byte(basicPass), []byte(regSecret)) != 1 {
			if decPass, err := url.QueryUnescape(basicPass); err != nil || subtle.ConstantTimeCompare([]byte(decPass), []byte(regSecret)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="Flint OAuth2"`)
				writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client_secret")
				return "", false
			}
		}
		return clientID, true
	}

	formClientID := strings.TrimSpace(r.FormValue("client_id"))
	formSecret, hasFormSecret := r.Form["client_secret"]
	if hasFormSecret {
		secretVal := ""
		if len(formSecret) > 0 {
			secretVal = formSecret[0]
		}
		if formClientID == "" || secretVal == "" {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client_secret_post requires non-empty client_id and client_secret")
			return "", false
		}
		s.oauthMu.Lock()
		regSecret, hasSecret := s.clientSecrets[formClientID]
		s.oauthMu.Unlock()
		if hasSecret && subtle.ConstantTimeCompare([]byte(secretVal), []byte(regSecret)) != 1 {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client_secret")
			return "", false
		}
		return formClientID, true
	}

	if formClientID != "" {
		s.oauthMu.Lock()
		regSecret, hasSecret := s.clientSecrets[formClientID]
		s.oauthMu.Unlock()
		if hasSecret && regSecret != "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="Flint OAuth2"`)
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "confidential client requires client_secret authentication")
			return "", false
		}
	}

	return formClientID, true
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
		reqClientID, ok := s.authenticateOAuthClient(w, r)
		if !ok {
			return
		}

		code := r.FormValue("code")
		codeVerifier := r.FormValue("code_verifier")

		entry, ok := s.consumeAuthCode(code)
		if !ok || time.Now().After(entry.ExpiresAt) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
			return
		}

		// RFC 6749 §4.1.3: enforce redirect_uri and client_id binding when provided
		if reqRedirectURI := strings.TrimSpace(r.FormValue("redirect_uri")); reqRedirectURI != "" && entry.RedirectURI != "" && reqRedirectURI != entry.RedirectURI {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match authorization request")
			return
		}
		if entry.ClientID != "" {
			s.oauthMu.Lock()
			regSecret, hasSecret := s.clientSecrets[entry.ClientID]
			s.oauthMu.Unlock()
			if hasSecret && regSecret != "" && reqClientID == "" {
				w.Header().Set("WWW-Authenticate", `Basic realm="Flint OAuth2"`)
				writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "confidential client requires client_secret authentication")
				return
			}
		}
		if reqClientID != "" && entry.ClientID != "" && reqClientID != entry.ClientID {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id does not match authorization request")
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
		sub := "Practitioner/prac-cardiology-1"
		if strings.Contains(scope, "patient/") {
			sub = "Patient/" + entry.PatientID
		}

		audClientID := entry.ClientID
		if audClientID == "" {
			audClientID = reqClientID
		}
		if audClientID == "" {
			audClientID = "flint-smart-client"
		}

		cfg := s.resolveSMARTConfigForTenant(r, entry.Tenant)
		extra := map[string]any{
			"iss":       cfg.Issuer,
			"client_id": audClientID,
			"fhirUser":  sub,
			"encounter": entry.EncounterID,
			"jti":       uuid.NewString(),
		}
		if entry.Tenant != "" {
			extra["tenant_id"] = entry.Tenant
		}
		accessToken, err := s.devAuth.IssueToken(sub, scope, entry.PatientID, time.Hour, extra)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}

		idTokenExtra := map[string]any{
			"iss":      cfg.Issuer,
			"aud":      []string{audClientID},
			"fhirUser": sub,
			"profile":  sub,
			"jti":      uuid.NewString(),
		}
		if entry.Nonce != "" {
			idTokenExtra["nonce"] = entry.Nonce
		}
		idToken, err := s.devAuth.IssueToken(sub, scope, entry.PatientID, time.Hour, idTokenExtra)
		if err != nil {
			idToken = accessToken
		}

		refreshToken := "flint-rt-" + uuid.NewString()
		s.storeRefreshToken(refreshToken, refreshTokenEntry{
			Subject:     sub,
			ClientID:    audClientID,
			PatientID:   entry.PatientID,
			EncounterID: entry.EncounterID,
			Tenant:      entry.Tenant,
			Nonce:       entry.Nonce,
			Scope:       scope,
			ExpiresAt:   time.Now().Add(24 * time.Hour),
		})

		resp := map[string]any{
			"access_token":        accessToken,
			"token_type":          "Bearer",
			"expires_in":          3600,
			"scope":               scope,
			"patient":             entry.PatientID,
			"encounter":           entry.EncounterID,
			"need_patient_banner": true,
			"smart_style_url":     "https://hl7.org/fhir/smart-app-launch/smart-styles.json",
			"id_token":            idToken,
			"refresh_token":       refreshToken,
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)

	case "client_credentials":
		reqClientID, ok := s.authenticateOAuthClient(w, r)
		if !ok {
			return
		}
		subject := reqClientID
		if subject == "" {
			subject = "system-bulk-client"
		}

		scope := r.FormValue("scope")
		if scope == "" {
			scope = "system/*.rs"
		}
		patientID := strings.TrimSpace(r.FormValue("patient"))
		cfg := s.resolveSMARTConfig(r)
		accessToken, err := s.devAuth.IssueToken(subject, scope, patientID, time.Hour, map[string]any{
			"iss":       cfg.Issuer,
			"client_id": subject,
			"jti":       uuid.NewString(),
		})
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
		if patientID != "" {
			resp["patient"] = patientID
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)

	case "refresh_token":
		reqClientID, ok := s.authenticateOAuthClient(w, r)
		if !ok {
			return
		}
		rawRT := strings.TrimSpace(r.FormValue("refresh_token"))
		if rawRT == "" || s.isTokenRevoked(rawRT) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "missing or revoked refresh_token")
			return
		}
		rtEntry, ok := s.consumeRefreshToken(rawRT)
		if !ok || time.Now().After(rtEntry.ExpiresAt) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh_token")
			return
		}
		if rtEntry.ClientID != "" {
			s.oauthMu.Lock()
			regSecret, hasSecret := s.clientSecrets[rtEntry.ClientID]
			s.oauthMu.Unlock()
			if hasSecret && regSecret != "" && reqClientID == "" {
				w.Header().Set("WWW-Authenticate", `Basic realm="Flint OAuth2"`)
				writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "confidential client requires client_secret authentication")
				return
			}
		}
		if reqClientID != "" && rtEntry.ClientID != "" && reqClientID != rtEntry.ClientID {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id does not match refresh_token grant")
			return
		}

		reqScope := strings.TrimSpace(r.FormValue("scope"))
		effectiveScope := rtEntry.Scope
		if reqScope != "" {
			if !isScopeSubset(reqScope, rtEntry.Scope) {
				writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "requested scope exceeds original grant")
				return
			}
			effectiveScope = reqScope
		}

		audClientID := rtEntry.ClientID
		if audClientID == "" {
			audClientID = reqClientID
		}
		if audClientID == "" {
			audClientID = "flint-smart-client"
		}

		cfg := s.resolveSMARTConfigForTenant(r, rtEntry.Tenant)
		extra := map[string]any{
			"iss":       cfg.Issuer,
			"client_id": audClientID,
			"fhirUser":  rtEntry.Subject,
			"encounter": rtEntry.EncounterID,
			"jti":       uuid.NewString(),
		}
		if rtEntry.Tenant != "" {
			extra["tenant_id"] = rtEntry.Tenant
		}
		accessToken, err := s.devAuth.IssueToken(rtEntry.Subject, effectiveScope, rtEntry.PatientID, time.Hour, extra)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}

		idTokenExtra := map[string]any{
			"iss":      cfg.Issuer,
			"aud":      []string{audClientID},
			"fhirUser": rtEntry.Subject,
			"profile":  rtEntry.Subject,
			"jti":      uuid.NewString(),
		}
		if rtEntry.Nonce != "" {
			idTokenExtra["nonce"] = rtEntry.Nonce
		}
		idToken, err := s.devAuth.IssueToken(rtEntry.Subject, effectiveScope, rtEntry.PatientID, time.Hour, idTokenExtra)
		if err != nil {
			idToken = accessToken
		}

		nextRefreshToken := "flint-rt-" + uuid.NewString()
		s.storeRefreshToken(nextRefreshToken, refreshTokenEntry{
			Subject:     rtEntry.Subject,
			ClientID:    audClientID,
			PatientID:   rtEntry.PatientID,
			EncounterID: rtEntry.EncounterID,
			Tenant:      rtEntry.Tenant,
			Nonce:       rtEntry.Nonce,
			Scope:       rtEntry.Scope,
			ExpiresAt:   time.Now().Add(24 * time.Hour),
		})
		resp := map[string]any{
			"access_token":  accessToken,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         effectiveScope,
			"patient":       rtEntry.PatientID,
			"encounter":     rtEntry.EncounterID,
			"id_token":      idToken,
			"refresh_token": nextRefreshToken,
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)

	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "supported grant_types: authorization_code, client_credentials, refresh_token")
	}
}

// handleOAuthIntrospect implements POST /oauth2/introspect (RFC 7662 + SMART App Launch STU2).
func (s *Server) handleOAuthIntrospect(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	// Validate resource server / client credentials if supplied (Basic, client_secret_post, private_key_jwt, or Bearer).
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		rsToken := strings.TrimSpace(authHeader[7:])
		validator := s.tokenValidator
		if validator == nil {
			validator = s.devAuth
		}
		if rsToken == "" || s.isTokenRevoked(rsToken) || validator == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="Flint OAuth2"`)
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid bearer token for introspection")
			return
		}
		if _, err := validator.ValidateToken(r.Context(), rsToken); err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="Flint OAuth2"`)
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid bearer token for introspection")
			return
		}
	} else if authHeader != "" || r.FormValue("client_assertion") != "" || r.FormValue("client_assertion_type") != "" || len(r.Form["client_secret"]) > 0 {
		if _, ok := s.authenticateOAuthClient(w, r); !ok {
			return
		}
	}

	rawToken := strings.TrimSpace(r.FormValue("token"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if rawToken == "" || s.isTokenRevoked(rawToken) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
		return
	}

	validator := s.tokenValidator
	if validator == nil {
		validator = s.devAuth
	}
	if validator == nil {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
		return
	}

	secCtx, err := validator.ValidateToken(r.Context(), rawToken)
	if err != nil || secCtx == nil {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
		return
	}

	resp := map[string]any{
		"active":     true,
		"sub":        secCtx.GetSubject(),
		"token_type": "Bearer",
	}

	if parsed, parseErr := auth.ParseJWT(rawToken); parseErr == nil && parsed != nil {
		if parsed.Payload.Scope != "" {
			resp["scope"] = parsed.Payload.Scope
		}
		if parsed.Payload.ExpiresAt > 0 {
			resp["exp"] = parsed.Payload.ExpiresAt
		}
		if parsed.Payload.IssuedAt > 0 {
			resp["iat"] = parsed.Payload.IssuedAt
		}
		if parsed.Payload.Issuer != "" {
			resp["iss"] = parsed.Payload.Issuer
		}
		if parsed.Payload.Patient != "" {
			resp["patient"] = parsed.Payload.Patient
		}
		if parsed.Payload.Encounter != "" {
			resp["encounter"] = parsed.Payload.Encounter
		}
		if cid, ok := parsed.Payload.Extra["client_id"].(string); ok && cid != "" {
			resp["client_id"] = cid
		} else if parsed.Payload.Subject != "" {
			resp["client_id"] = parsed.Payload.Subject
		}
		if fu, ok := parsed.Payload.Extra["fhirUser"].(string); ok && fu != "" {
			resp["fhirUser"] = fu
		}
	} else {
		var rawScopes []string
		for _, sc := range secCtx.GetScopes() {
			rawScopes = append(rawScopes, sc.GetRaw())
		}
		resp["scope"] = strings.Join(rawScopes, " ")
		if secCtx.GetPatientId() != "" {
			resp["patient"] = secCtx.GetPatientId()
		}
		if secCtx.GetEncounterId() != "" {
			resp["encounter"] = secCtx.GetEncounterId()
		}
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleOAuthRevoke implements POST /oauth2/revoke (RFC 7009 OAuth 2.0 Token Revocation).
func (s *Server) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.Header.Get("Authorization") != "" || r.FormValue("client_assertion") != "" || r.FormValue("client_assertion_type") != "" || len(r.Form["client_secret"]) > 0 {
		if _, ok := s.authenticateOAuthClient(w, r); !ok {
			return
		}
	}
	rawToken := strings.TrimSpace(r.FormValue("token"))
	if rawToken != "" {
		s.revokeTokenInternal(r.Context(), rawToken, 24*time.Hour)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) revokeTokenInternal(ctx context.Context, rawToken string, ttl time.Duration) {
	if s.store != nil {
		_ = s.store.RevokeToken(ctx, rawToken, ttl)
	}
	now := time.Now()
	s.oauthMu.Lock()
	if s.revokedTokens == nil {
		s.revokedTokens = make(map[string]time.Time)
	}
	if s.refreshTokens != nil {
		delete(s.refreshTokens, rawToken)
	}
	for k, ts := range s.revokedTokens {
		if now.Sub(ts) > 24*time.Hour {
			delete(s.revokedTokens, k)
		}
	}
	if len(s.revokedTokens) >= maxOAuthEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, ts := range s.revokedTokens {
			if oldestKey == "" || ts.Before(oldestTime) {
				oldestKey = k
				oldestTime = ts
			}
		}
		if oldestKey != "" {
			delete(s.revokedTokens, oldestKey)
		}
	}
	s.revokedTokens[rawToken] = now
	s.oauthMu.Unlock()
}

func (s *Server) isTokenRevoked(rawToken string) bool {
	if rawToken == "" {
		return false
	}
	s.oauthMu.Lock()
	_, revoked := s.revokedTokens[rawToken]
	s.oauthMu.Unlock()
	if revoked {
		return true
	}
	if s.store != nil {
		if dbRevoked, err := s.store.IsTokenRevoked(context.Background(), rawToken); err == nil && dbRevoked {
			return true
		}
	}
	return false
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

// handleOAuthRegister implements RFC 7591 OAuth 2.0 Dynamic Client Registration
// (POST /oauth2/register) for registering SMART public, symmetric, and asymmetric clients.
func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientID                string      `json:"client_id"`
		ClientSecret            string      `json:"client_secret"`
		ClientName              string      `json:"client_name"`
		TokenEndpointAuthMethod string      `json:"token_endpoint_auth_method"`
		GrantTypes              []string    `json:"grant_types"`
		RedirectURIs            []string    `json:"redirect_uris"`
		Scope                   string      `json:"scope"`
		JWKS                    auth.JWKSet `json:"jwks"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "malformed JSON client registration request")
		return
	}
	clientID := strings.TrimSpace(req.ClientID)
	if clientID == "" {
		clientID = "flint-client-" + uuid.NewString()
	}
	if len(req.JWKS.Keys) > 0 {
		for i := range req.JWKS.Keys {
			if _, err := req.JWKS.Keys[i].PublicKey(); err != nil {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JWK in jwks.keys: "+err.Error())
				return
			}
		}
		s.RegisterOAuthClientJWKSet(clientID, req.JWKS)
	}
	authMethod := strings.TrimSpace(req.TokenEndpointAuthMethod)
	if authMethod == "" {
		if req.ClientSecret != "" {
			authMethod = "client_secret_basic"
		} else if len(req.JWKS.Keys) > 0 {
			authMethod = "private_key_jwt"
		} else {
			authMethod = "none"
		}
	}
	clientSecret := req.ClientSecret
	if (authMethod == "client_secret_basic" || authMethod == "client_secret_post") && clientSecret == "" {
		clientSecret = "flint-secret-" + uuid.NewString()
	}
	if clientSecret != "" {
		s.RegisterOAuthClientSecret(clientID, clientSecret)
	}
	grantTypes := req.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"client_credentials", "authorization_code", "refresh_token"}
	}
	resp := map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                req.ClientName,
		"token_endpoint_auth_method": authMethod,
		"grant_types":                grantTypes,
		"scope":                      req.Scope,
	}
	if len(req.JWKS.Keys) > 0 {
		resp["jwks"] = req.JWKS
	}
	if len(req.RedirectURIs) > 0 {
		resp["redirect_uris"] = req.RedirectURIs
	}
	if clientSecret != "" {
		resp["client_secret"] = clientSecret
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}
