package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
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

	// In-memory ECDSA P-256 key pair for ES256 JWS signing (e.g. SMART Health Cards)
	ecPrivateKey *ecdsa.PrivateKey
	ecPublicKey  *ecdsa.PublicKey
	ecKeyID      string
	ecX          string
	ecY          string

	issuer   string
	audience string
}

// NewMockTokenValidator initializes a mock validator with an ephemeral RSA-2048 and ECDSA P-256 key pair.
func NewMockTokenValidator(issuer, audience string) (*MockTokenValidator, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate mock rsa key: %w", err)
	}
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate mock p256 key: %w", err)
	}
	uncompressed, err := ecPriv.PublicKey.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode mock p256 public key: %w", err)
	}
	ecX := base64.RawURLEncoding.EncodeToString(uncompressed[1:33])
	ecY := base64.RawURLEncoding.EncodeToString(uncompressed[33:65])
	// RFC 7638 JWK SHA-256 thumbprint over lexicographically ordered required EC members: crv, kty, x, y
	thumbInput := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":%q,"y":%q}`, ecX, ecY)
	thumbSum := sha256.Sum256([]byte(thumbInput))
	ecKid := base64.RawURLEncoding.EncodeToString(thumbSum[:])

	return &MockTokenValidator{
		staticContexts: make(map[string]*authv1.SecurityContext),
		privateKey:     priv,
		publicKey:      &priv.PublicKey,
		keyID:          "mock-key-1",
		ecPrivateKey:   ecPriv,
		ecPublicKey:    &ecPriv.PublicKey,
		ecKeyID:        ecKid,
		ecX:            ecX,
		ecY:            ecY,
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

// ECKeyID returns the RFC 7638 JWK SHA-256 thumbprint kid of the P-256 ES256 signing key.
func (m *MockTokenValidator) ECKeyID() string {
	if m == nil {
		return ""
	}
	return m.ecKeyID
}

// SignRawES256 signs an arbitrary JWS signing input ("<b64header>.<b64payload>") with ECDSA P-256 SHA-256
// and returns the unpadded base64url-encoded 64-byte IEEE P1363 R||S signature segment.
func (m *MockTokenValidator) SignRawES256(signingInput string) (string, error) {
	if m == nil || m.ecPrivateKey == nil {
		return "", fmt.Errorf("mock validator missing ECDSA P-256 key")
	}
	sum := sha256.Sum256([]byte(signingInput))
	rInt, sInt, err := ecdsa.Sign(rand.Reader, m.ecPrivateKey, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign ES256: %w", err)
	}
	sig := make([]byte, 64)
	rBytes := rInt.Bytes()
	sBytes := sInt.Bytes()
	copy(sig[32-len(rBytes):32], rBytes)
	copy(sig[64-len(sBytes):64], sBytes)
	return base64.RawURLEncoding.EncodeToString(sig), nil
}

// JWKS returns the mock's RSA (RS256) and EC P-256 (ES256) public keys as a JWKSet.
func (m *MockTokenValidator) JWKS() *JWKSet {
	nBytes := m.publicKey.N.Bytes()
	eBytes := []byte{1, 0, 1} // 65537

	keys := []JWK{
		{
			Kty: "RSA",
			Kid: m.keyID,
			Use: "sig",
			Alg: "RS256",
			N:   base64.RawURLEncoding.EncodeToString(nBytes),
			E:   base64.RawURLEncoding.EncodeToString(eBytes),
		},
	}
	if m.ecPublicKey != nil && m.ecX != "" && m.ecY != "" {
		keys = append(keys, JWK{
			Kty: "EC",
			Kid: m.ecKeyID,
			Use: "sig",
			Alg: "ES256",
			Crv: "P-256",
			X:   m.ecX,
			Y:   m.ecY,
		})
	}

	return &JWKSet{Keys: keys}
}

// ExportKeysPEM exports the validator's RSA-2048 (RS256) and ECDSA P-256 (ES256) private keys as PKCS#8 PEM blocks
// so stateless cluster replicas can share identical token and SMART Health Card signing keys.
func (m *MockTokenValidator) ExportKeysPEM() (rsaPEM []byte, ecPEM []byte, err error) {
	if m == nil || m.privateKey == nil || m.ecPrivateKey == nil {
		return nil, nil, errors.New("mock validator keys not initialized")
	}
	rsaDER, err := x509.MarshalPKCS8PrivateKey(m.privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal rsa pkcs8: %w", err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(m.ecPrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal ecdsa pkcs8: %w", err)
	}
	rsaPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaDER})
	ecPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})
	return rsaPEM, ecPEM, nil
}

// LoadKeysPEM replaces the validator's RSA (when len(rsaPEM) > 0) and/or ECDSA P-256 (when len(ecPEM) > 0)
// signing keys from PEM-encoded PKCS#1 / PKCS#8 / SEC1 private keys, recomputing JWK thumbprints.
func (m *MockTokenValidator) LoadKeysPEM(rsaPEM []byte, ecPEM []byte) error {
	if m == nil {
		return errors.New("nil mock validator")
	}
	if len(rsaPEM) > 0 {
		blk, _ := pem.Decode(rsaPEM)
		if blk == nil {
			return errors.New("invalid RSA private key PEM")
		}
		var rsaPriv *rsa.PrivateKey
		if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
			var ok bool
			rsaPriv, ok = k.(*rsa.PrivateKey)
			if !ok {
				return errors.New("PKCS#8 PEM block is not an RSA private key")
			}
		} else if k1, err1 := x509.ParsePKCS1PrivateKey(blk.Bytes); err1 == nil {
			rsaPriv = k1
		} else {
			return fmt.Errorf("parse RSA private key: %w", err)
		}
		m.privateKey = rsaPriv
		m.publicKey = &rsaPriv.PublicKey
	}
	if len(ecPEM) > 0 {
		blk, _ := pem.Decode(ecPEM)
		if blk == nil {
			return errors.New("invalid ECDSA private key PEM")
		}
		var ecPriv *ecdsa.PrivateKey
		if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
			var ok bool
			ecPriv, ok = k.(*ecdsa.PrivateKey)
			if !ok {
				return errors.New("PKCS#8 PEM block is not an ECDSA private key")
			}
		} else if k1, err1 := x509.ParseECPrivateKey(blk.Bytes); err1 == nil {
			ecPriv = k1
		} else {
			return fmt.Errorf("parse ECDSA private key: %w", err)
		}
		if ecPriv.Curve != elliptic.P256() {
			return errors.New("ECDSA signing key must use P-256 curve")
		}
		uncompressed, err := ecPriv.PublicKey.Bytes()
		if err != nil {
			return fmt.Errorf("encode ECDSA P-256 public key: %w", err)
		}
		ecX := base64.RawURLEncoding.EncodeToString(uncompressed[1:33])
		ecY := base64.RawURLEncoding.EncodeToString(uncompressed[33:65])
		thumbInput := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":%q,"y":%q}`, ecX, ecY)
		thumbSum := sha256.Sum256([]byte(thumbInput))
		m.ecPrivateKey = ecPriv
		m.ecPublicKey = &ecPriv.PublicKey
		m.ecKeyID = base64.RawURLEncoding.EncodeToString(thumbSum[:])
		m.ecX = ecX
		m.ecY = ecY
	}
	return nil
}
