package auth

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
)

// OIDCConfig configures the standard OIDC / JWKS token validator.
type OIDCConfig struct {
	Issuer             string
	Audience           string
	Audiences          []string // Optional accepted audiences (merged with Audience when set)
	JWKSURL            string
	HTTPClient         *http.Client
	CacheTTL           time.Duration
	MinRefreshInterval time.Duration // Minimum cooldown between forced JWKS refreshes on kid miss (default 5s; <0 disables)
	StaticJWKS         *JWKSet       // Optional: pre-configured keys for tests or isolated deployments
	SigningAlgorithms  []string      // Optional allowed asymmetric JWS algs (default: RS256, ES256)
	MinRSABits         int           // Optional minimum RSA public key bit length (e.g. 2048)
}

// OIDCValidator implements TokenValidator using standard JWKS and OIDC conventions.
// It is compatible with Zitadel, Keycloak, Okta, Azure AD, Auth0, and any compliant IdP.
type OIDCValidator struct {
	cfg                OIDCConfig
	httpClient         *http.Client
	minRefreshInterval time.Duration
	allowedAlgs        map[string]struct{}
	acceptedAudiences  []string

	mu          sync.RWMutex
	cachedJWKS  *JWKSet
	cacheExp    time.Time
	lastRefresh time.Time
}

// DefaultSigningAlgorithms lists the asymmetric JWS algorithms accepted when none are specified.
var DefaultSigningAlgorithms = []string{"RS256", "ES256"}

// PermittedAsymmetricAlgorithms enumerates all asymmetric JWS algorithms supported by Flint.
var PermittedAsymmetricAlgorithms = map[string]struct{}{
	"RS256": {}, "RS384": {}, "RS512": {},
	"PS256": {}, "PS384": {}, "PS512": {},
	"ES256": {}, "ES384": {}, "ES512": {},
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
	minRefresh := cfg.MinRefreshInterval
	if minRefresh == 0 {
		minRefresh = 5 * time.Second
	} else if minRefresh < 0 {
		minRefresh = 0
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	algs := cfg.SigningAlgorithms
	if len(algs) == 0 {
		algs = DefaultSigningAlgorithms
	}
	allowedAlgs := make(map[string]struct{}, len(algs))
	for _, alg := range algs {
		if _, ok := PermittedAsymmetricAlgorithms[alg]; !ok {
			return nil, fmt.Errorf("%w: %s is not a permitted asymmetric algorithm", ErrUnsupportedAlg, alg)
		}
		allowedAlgs[alg] = struct{}{}
	}

	audiences := make([]string, 0, 1+len(cfg.Audiences))
	if cfg.Audience != "" {
		audiences = append(audiences, cfg.Audience)
	}
	for _, aud := range cfg.Audiences {
		if aud != "" {
			audiences = append(audiences, aud)
		}
	}

	return &OIDCValidator{
		cfg:                cfg,
		httpClient:         client,
		minRefreshInterval: minRefresh,
		allowedAlgs:        allowedAlgs,
		acceptedAudiences:  audiences,
		cachedJWKS:         cfg.StaticJWKS,
		cacheExp:           time.Now().Add(cfg.CacheTTL),
	}, nil
}

// ValidateToken validates a bearer token against the IdP's JWKS and OIDC rules.
func (v *OIDCValidator) ValidateToken(ctx context.Context, bearerToken string) (*authv1.SecurityContext, error) {
	_, secCtx, err := v.validateJWT(ctx, bearerToken)
	return secCtx, err
}

func (v *OIDCValidator) validateJWT(ctx context.Context, bearerToken string) (*ParsedToken, *authv1.SecurityContext, error) {
	token, err := ParseJWT(bearerToken)
	if err != nil {
		return nil, nil, err
	}

	// 1. Enforce configured asymmetric signing algorithm before JWKS lookup
	if _, ok := v.allowedAlgs[token.Header.Alg]; !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnsupportedAlg, token.Header.Alg)
	}

	// 2. Verify standard claims (iss, aud, exp, nbf)
	if err := token.VerifyClaimsAudiences(v.cfg.Issuer, v.acceptedAudiences, time.Now()); err != nil {
		return nil, nil, err
	}

	// 3. Fetch or retrieve cached JWKS
	jwks, err := v.getJWKS(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to retrieve JWKS: %w", err)
	}

	// 4. Locate signing key (with rate-limited rotation refresh on cache miss)
	key, err := jwks.FindKey(token.Header.Kid)
	if err != nil {
		if v.cfg.StaticJWKS == nil {
			v.mu.Lock()
			if v.cachedJWKS != nil {
				key, err = v.cachedJWKS.FindKey(token.Header.Kid)
			}
			if err != nil && (v.minRefreshInterval == 0 || time.Since(v.lastRefresh) >= v.minRefreshInterval) {
				if refreshed, refErr := v.fetchJWKS(ctx); refErr == nil {
					now := time.Now()
					v.cachedJWKS = refreshed
					v.cacheExp = now.Add(v.cfg.CacheTTL)
					v.lastRefresh = now
					key, err = refreshed.FindKey(token.Header.Kid)
				}
			}
			v.mu.Unlock()
		}
		if err != nil {
			return nil, nil, fmt.Errorf("find signing key %q: %w", token.Header.Kid, err)
		}
	}

	// 5. Verify signature
	pubKey, err := key.PublicKey()
	if err != nil {
		return nil, nil, fmt.Errorf("parse public key: %w", err)
	}
	if v.cfg.MinRSABits > 0 {
		if rsaPub, ok := pubKey.(*rsa.PublicKey); ok && rsaPub.N.BitLen() < v.cfg.MinRSABits {
			return nil, nil, fmt.Errorf("%w: %d-bit RSA key below minimum %d bits", ErrUnsupportedKeyType, rsaPub.N.BitLen(), v.cfg.MinRSABits)
		}
	}
	if err := token.VerifySignature(pubKey); err != nil {
		return nil, nil, err
	}

	// 6. Build proto SecurityContext
	scopes := ParseScopes(token.Payload.Scope)

	claimsMap := make(map[string]string)
	for k, val := range token.Payload.Extra {
		switch valTyped := val.(type) {
		case string:
			claimsMap[k] = valTyped
		case map[string]any, []any:
			if b, err := json.Marshal(valTyped); err == nil {
				claimsMap[k] = string(b)
			} else {
				claimsMap[k] = fmt.Sprintf("%v", valTyped)
			}
		default:
			claimsMap[k] = fmt.Sprintf("%v", valTyped)
		}
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

	return token, &authv1.SecurityContext{
		Subject:     token.Payload.Subject,
		TenantId:    tenantID,
		PatientId:   patientID,
		EncounterId: encounterID,
		Scopes:      scopes,
		Claims:      claimsMap,
		RawToken:    bearerToken,
		Issuer:      token.Payload.Issuer,
		AuthScheme:  "oidc",
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

	now := time.Now()
	v.cachedJWKS = set
	v.cacheExp = now.Add(v.cfg.CacheTTL)
	v.lastRefresh = now
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("failed to decode JWKS response: %w", err)
	}

	return &set, nil
}
