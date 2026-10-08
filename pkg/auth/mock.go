package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
)

// MockTokenValidator is an in-memory TokenValidator for unit tests and local testing.
type MockTokenValidator struct {
	// CustomValidate is an optional hook to override validation behavior.
	CustomValidate func(ctx context.Context, bearerToken string) (*authv1.SecurityContext, error)

	// Pre-registered tokens mapped to security contexts
	staticContexts map[string]*authv1.SecurityContext

	// In-memory RSA key pair for issuing real cryptographically signed test tokens
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	keyID      string
	issuer     string
	audience   string
}

// NewMockTokenValidator initializes a mock validator with an ephemeral RSA-2048 key pair.
func NewMockTokenValidator(issuer, audience string) (*MockTokenValidator, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate mock rsa key: %w", err)
	}

	return &MockTokenValidator{
		staticContexts: make(map[string]*authv1.SecurityContext),
		privateKey:     priv,
		publicKey:      &priv.PublicKey,
		keyID:          "mock-key-1",
		issuer:         issuer,
		audience:       audience,
	}, nil
}

// RegisterStatic registers a static bearer token string that maps directly to a SecurityContext.
func (m *MockTokenValidator) RegisterStatic(token string, sc *authv1.SecurityContext) {
	m.staticContexts[token] = sc
}

// ValidateToken fulfills the TokenValidator interface.
func (m *MockTokenValidator) ValidateToken(ctx context.Context, bearerToken string) (*authv1.SecurityContext, error) {
	if m.CustomValidate != nil {
		return m.CustomValidate(ctx, bearerToken)
	}

	if sc, ok := m.staticContexts[bearerToken]; ok {
		return sc, nil
	}

	// If no static match, try parsing as a signed JWT issued by this mock
	parsed, err := ParseJWT(bearerToken)
	if err != nil {
		return nil, err
	}

	if err := parsed.VerifyClaims(m.issuer, m.audience, time.Now()); err != nil {
		return nil, err
	}

	if err := parsed.VerifySignature(m.publicKey); err != nil {
		return nil, err
	}

	scopes := ParseScopes(parsed.Payload.Scope)
	claimsMap := make(map[string]string)
	for k, v := range parsed.Payload.Extra {
		claimsMap[k] = fmt.Sprintf("%v", v)
	}

	tenantID := ""
	if tVal, ok := parsed.Payload.Extra["tenant_id"].(string); ok {
		tenantID = tVal
	} else if tVal, ok := parsed.Payload.Extra["tenant"].(string); ok {
		tenantID = tVal
	}

	return &authv1.SecurityContext{
		Subject:     parsed.Payload.Subject,
		TenantId:    tenantID,
		PatientId:   parsed.Payload.Patient,
		EncounterId: parsed.Payload.Encounter,
		Scopes:      scopes,
		Claims:      claimsMap,
		RawToken:    bearerToken,
	}, nil
}

// IssueToken mints a cryptographically valid, signed RS256 JWT using the mock's private key.
func (m *MockTokenValidator) IssueToken(sub, scopes, patientID string, ttl time.Duration, extra map[string]any) (string, error) {
	now := time.Now()
	header := JWTHeader{
		Alg: "RS256",
		Kid: m.keyID,
		Typ: "JWT",
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}

	payloadMap := map[string]any{
		"iss":   m.issuer,
		"sub":   sub,
		"aud":   []string{m.audience},
		"iat":   now.Unix(),
		"exp":   now.Add(ttl).Unix(),
		"scope": scopes,
	}
	if patientID != "" {
		payloadMap["patient"] = patientID
	}
	for k, v := range extra {
		payloadMap[k] = v
	}

	payloadJSON, err := json.Marshal(payloadMap)
	if err != nil {
		return "", err
	}

	hB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	pB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signedData := hB64 + "." + pB64

	hasher := sha256.New()
	hasher.Write([]byte(signedData))
	hashed := hasher.Sum(nil)

	sig, err := rsa.SignPKCS1v15(rand.Reader, m.privateKey, crypto.SHA256, hashed)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}

	sB64 := base64.RawURLEncoding.EncodeToString(sig)
	return signedData + "." + sB64, nil
}

// JWKS returns the mock's public key as a JWKSet.
func (m *MockTokenValidator) JWKS() *JWKSet {
	nBytes := m.publicKey.N.Bytes()
	eBytes := []byte{1, 0, 1} // 65537

	return &JWKSet{
		Keys: []JWK{
			{
				Kty: "RSA",
				Kid: m.keyID,
				Use: "sig",
				Alg: "RS256",
				N:   base64.RawURLEncoding.EncodeToString(nBytes),
				E:   base64.RawURLEncoding.EncodeToString(eBytes),
			},
		},
	}
}
