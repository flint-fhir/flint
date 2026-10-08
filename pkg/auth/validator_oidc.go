package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
)

// OIDCConfig configures the standard OIDC / JWKS token validator.
type OIDCConfig struct {
	Issuer     string
	Audience   string
	JWKSURL    string
	HTTPClient *http.Client
	CacheTTL   time.Duration
	StaticJWKS *JWKSet // Optional: pre-configured keys for tests or isolated deployments
}

// OIDCValidator implements TokenValidator using standard JWKS and OIDC conventions.
// It is compatible with Zitadel, Keycloak, Okta, Azure AD, Auth0, and any compliant IdP.
type OIDCValidator struct {
	cfg        OIDCConfig
	httpClient *http.Client

	mu         sync.RWMutex
	cachedJWKS *JWKSet
	cacheExp   time.Time
}

// NewOIDCValidator creates a new OIDC / JWKS token validator.
func NewOIDCValidator(cfg OIDCConfig) (*OIDCValidator, error) {
	if cfg.Issuer == "" && cfg.StaticJWKS == nil {
		return nil, fmt.Errorf("OIDCConfig: Issuer or StaticJWKS is required")
	}
	if cfg.JWKSURL == "" && cfg.Issuer != "" {
		cfg.JWKSURL = fmt.Sprintf("%s/.well-known/jwks.json", cfg.Issuer)
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = time.Hour
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	return &OIDCValidator{
		cfg:        cfg,
		httpClient: client,
		cachedJWKS: cfg.StaticJWKS,
		cacheExp:   time.Now().Add(cfg.CacheTTL),
	}, nil
}

// ValidateToken validates a bearer token against the IdP's JWKS and OIDC rules.
func (v *OIDCValidator) ValidateToken(ctx context.Context, bearerToken string) (*authv1.SecurityContext, error) {
	token, err := ParseJWT(bearerToken)
	if err != nil {
		return nil, err
	}

	// 1. Verify standard claims (iss, aud, exp, nbf)
	if err := token.VerifyClaims(v.cfg.Issuer, v.cfg.Audience, time.Now()); err != nil {
		return nil, err
	}

	// 2. Fetch or retrieve cached JWKS
	jwks, err := v.getJWKS(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve JWKS: %w", err)
	}

	// 3. Locate signing key
	key, err := jwks.FindKey(token.Header.Kid)
	if err != nil {
		// Key might have rotated; refresh cache and retry once
		if v.cfg.StaticJWKS == nil {
			if refreshed, refErr := v.fetchJWKS(ctx); refErr == nil {
				v.mu.Lock()
				v.cachedJWKS = refreshed
				v.cacheExp = time.Now().Add(v.cfg.CacheTTL)
				v.mu.Unlock()
				key, err = refreshed.FindKey(token.Header.Kid)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("find signing key %q: %w", token.Header.Kid, err)
		}
	}

	// 4. Verify signature
	pubKey, err := key.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	if err := token.VerifySignature(pubKey); err != nil {
		return nil, err
	}

	// 5. Build proto SecurityContext
	scopes := ParseScopes(token.Payload.Scope)

	claimsMap := make(map[string]string)
	for k, val := range token.Payload.Extra {
		claimsMap[k] = fmt.Sprintf("%v", val)
	}

	patientID := token.Payload.Patient
	if patientID == "" {
		if pVal, ok := token.Payload.Extra["patient_id"].(string); ok {
			patientID = pVal
		}
	}

	encounterID := token.Payload.Encounter
	if encounterID == "" {
		if eVal, ok := token.Payload.Extra["encounter_id"].(string); ok {
			encounterID = eVal
		}
	}

	tenantID := ""
	if tVal, ok := token.Payload.Extra["tenant_id"].(string); ok {
		tenantID = tVal
	} else if tVal, ok := token.Payload.Extra["tenant"].(string); ok {
		tenantID = tVal
	}

	return &authv1.SecurityContext{
		Subject:     token.Payload.Subject,
		TenantId:    tenantID,
		PatientId:   patientID,
		EncounterId: encounterID,
		Scopes:      scopes,
		Claims:      claimsMap,
		RawToken:    bearerToken,
	}, nil
}

func (v *OIDCValidator) getJWKS(ctx context.Context) (*JWKSet, error) {
	v.mu.RLock()
	if v.cachedJWKS != nil && time.Now().Before(v.cacheExp) {
		set := v.cachedJWKS
		v.mu.RUnlock()
		return set, nil
	}
	v.mu.RUnlock()

	if v.cfg.StaticJWKS != nil {
		return v.cfg.StaticJWKS, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	// Double-check under write lock
	if v.cachedJWKS != nil && time.Now().Before(v.cacheExp) {
		return v.cachedJWKS, nil
	}

	set, err := v.fetchJWKS(ctx)
	if err != nil {
		return nil, err
	}

	v.cachedJWKS = set
	v.cacheExp = time.Now().Add(v.cfg.CacheTTL)
	return set, nil
}

func (v *OIDCValidator) fetchJWKS(ctx context.Context) (*JWKSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status from JWKS: %d", resp.StatusCode)
	}

	var set JWKSet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, fmt.Errorf("failed to decode JWKS response: %w", err)
	}

	return &set, nil
}
