package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func fixedBigIntBytes(v *big.Int, size int) []byte {
	out := make([]byte, size)
	b := v.Bytes()
	copy(out[size-len(b):], b)
	return out
}

func signRSAJWT(t *testing.T, alg, kid string, priv *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	hdrJSON, err := json.Marshal(map[string]any{"alg": alg, "typ": "JWT", "kid": kid})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	signingInput := b64url(hdrJSON) + "." + b64url(payJSON)

	var hashAlg crypto.Hash
	switch alg[len(alg)-3:] {
	case "256":
		hashAlg = crypto.SHA256
	case "384":
		hashAlg = crypto.SHA384
	case "512":
		hashAlg = crypto.SHA512
	default:
		t.Fatalf("unsupported alg %s", alg)
	}
	digest, err := digestForHash(hashAlg, []byte(signingInput))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	var sig []byte
	if alg[:2] == "PS" {
		sig, err = rsa.SignPSS(rand.Reader, priv, hashAlg, digest, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	} else {
		sig, err = rsa.SignPKCS1v15(rand.Reader, priv, hashAlg, digest)
	}
	if err != nil {
		t.Fatalf("sign %s: %v", alg, err)
	}
	return signingInput + "." + b64url(sig)
}

func signECDSAJWT(t *testing.T, alg, kid string, priv *ecdsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	hdrJSON, err := json.Marshal(map[string]any{"alg": alg, "typ": "JWT", "kid": kid})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	signingInput := b64url(hdrJSON) + "." + b64url(payJSON)

	var (
		digest   []byte
		coordLen int
	)
	switch alg {
	case "ES256":
		h := sha256.Sum256([]byte(signingInput))
		digest, coordLen = h[:], 32
	case "ES384":
		h := sha512.Sum384([]byte(signingInput))
		digest, coordLen = h[:], 48
	case "ES512":
		h := sha512.Sum512([]byte(signingInput))
		digest, coordLen = h[:], 66
	default:
		t.Fatalf("unsupported ec alg %s", alg)
	}

	r, s, err := ecdsa.Sign(rand.Reader, priv, digest)
	if err != nil {
		t.Fatalf("ecdsa sign %s: %v", alg, err)
	}
	sig := append(fixedBigIntBytes(r, coordLen), fixedBigIntBytes(s, coordLen)...)
	return signingInput + "." + b64url(sig)
}

func rsaPublicJWK(kid string, pub *rsa.PublicKey) JWK {
	return JWK{
		Kty: "RSA",
		Kid: kid,
		Use: "sig",
		N:   b64url(pub.N.Bytes()),
		E:   b64url(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func ecPublicJWK(kid, crv string, coordLen int, pub *ecdsa.PublicKey) JWK {
	raw, _ := pub.Bytes()
	// Uncompressed ANSI X9.62 format: 0x04 || X(coordLen) || Y(coordLen)
	return JWK{
		Kty: "EC",
		Kid: kid,
		Use: "sig",
		Crv: crv,
		X:   b64url(raw[1 : 1+coordLen]),
		Y:   b64url(raw[1+coordLen : 1+2*coordLen]),
	}
}

func TestJWTSignatureAllAlgorithms(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	ec256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate p256: %v", err)
	}
	ec384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate p384: %v", err)
	}
	ec521, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		t.Fatalf("generate p521: %v", err)
	}

	jwks := JWKSet{
		Keys: []JWK{
			rsaPublicJWK("rsa1", &rsaKey.PublicKey),
			ecPublicJWK("ec256", "P-256", 32, &ec256.PublicKey),
			ecPublicJWK("ec384", "P-384", 48, &ec384.PublicKey),
			ecPublicJWK("ec521", "P-521", 66, &ec521.PublicKey),
		},
	}

	now := time.Now()
	baseClaims := map[string]any{
		"iss": "https://idp.azra.example",
		"sub": "user-123",
		"aud": "flint-fhir",
		"exp": now.Add(5 * time.Minute).Unix(),
		"iat": now.Unix(),
	}

	validator, err := NewOIDCValidator(OIDCConfig{
		Issuer:            "https://idp.azra.example",
		Audience:          "flint-fhir",
		StaticJWKS:        &jwks,
		SigningAlgorithms: []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"},
	})
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}

	for _, alg := range []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"} {
		tok := signRSAJWT(t, alg, "rsa1", rsaKey, baseClaims)
		sc, err := validator.ValidateToken(context.Background(), tok)
		if err != nil {
			t.Fatalf("ValidateToken(%s) failed: %v", alg, err)
		}
		if sc.GetIssuer() != "https://idp.azra.example" || sc.GetSubject() != "user-123" {
			t.Fatalf("unexpected SecurityContext for %s: %+v", alg, sc)
		}
	}

	ecCases := []struct {
		alg string
		kid string
		key *ecdsa.PrivateKey
	}{
		{"ES256", "ec256", ec256},
		{"ES384", "ec384", ec384},
		{"ES512", "ec521", ec521},
	}
	for _, tc := range ecCases {
		tok := signECDSAJWT(t, tc.alg, tc.kid, tc.key, baseClaims)
		sc, err := validator.ValidateToken(context.Background(), tok)
		if err != nil {
			t.Fatalf("ValidateToken(%s) failed: %v", tc.alg, err)
		}
		if sc.GetSubject() != "user-123" {
			t.Fatalf("unexpected subject for %s: %q", tc.alg, sc.GetSubject())
		}
	}

	// Ensure default OIDCConfig rejects RS512 unless configured
	defaultVal, err := NewOIDCValidator(OIDCConfig{
		Issuer:     "https://idp.azra.example",
		Audience:   "flint-fhir",
		StaticJWKS: &jwks,
	})
	if err != nil {
		t.Fatalf("default validator: %v", err)
	}
	if _, err := defaultVal.ValidateToken(context.Background(), signRSAJWT(t, "RS512", "rsa1", rsaKey, baseClaims)); !errors.Is(err, ErrUnsupportedAlg) {
		t.Fatalf("expected ErrUnsupportedAlg for unconfigured RS512, got %v", err)
	}
}

func TestZitadelPerTenantRoleIsolation(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	jwks := JWKSet{Keys: []JWK{rsaPublicJWK("z1", &rsaKey.PublicKey)}}

	val, err := NewZitadelValidator(ZitadelConfig{
		OIDCConfig: OIDCConfig{
			Issuer:     "https://auth.azra.example",
			Audience:   "flint",
			StaticJWKS: &jwks,
		},
		TenantFromOrgDomain: true,
		RoleToScopes: map[string]string{
			"viewer": "user/Patient.rs",
			"admin":  "system/*.*",
		},
	})
	if err != nil {
		t.Fatalf("NewZitadelValidator: %v", err)
	}

	now := time.Now()
	// Alice is viewer in hospital-a (org_a), admin ONLY in hospital-b (org_b)
	claims := map[string]any{
		"iss":                 "https://auth.azra.example",
		"sub":                 "alice",
		"aud":                 "flint",
		"exp":                 now.Add(10 * time.Minute).Unix(),
		ZitadelClaimOrgID:     "org_a",
		ZitadelClaimOrgDomain: "hospital-a.azra.dev",
		ZitadelClaimRoles: map[string]any{
			"viewer": map[string]any{"org_a": "hospital-a.azra.dev"},
			"admin":  map[string]any{"org_b": "hospital-b.azra.dev"},
		},
	}
	tok := signRSAJWT(t, "RS256", "z1", rsaKey, claims)

	sc, err := val.ValidateToken(context.Background(), tok)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if sc.GetTenantId() != "hospital_a" {
		t.Fatalf("expected active tenant hospital_a, got %q", sc.GetTenantId())
	}
	if !Allows(sc, "Patient", ActionRead) {
		t.Fatalf("expected reader scope in hospital_a")
	}
	if Allows(sc, "Patient", ActionDelete) {
		t.Fatalf("SECURITY BUG: admin role from org_b leaked into hospital_a scopes!")
	}

	// Switching via ResolveTenantContext to hospital_b grants ONLY hospital_b's admin role
	scB, err := ResolveTenantContext(sc, "hospital_b")
	if err != nil {
		t.Fatalf("ResolveTenantContext(hospital_b): %v", err)
	}
	if !Allows(scB, "Patient", ActionDelete) {
		t.Fatalf("expected admin delete permission inside hospital_b")
	}
	if scB.GetOperatorCrossTenant() {
		t.Fatalf("direct tenant grant should keep OperatorCrossTenant=false")
	}

	// Unlisted tenant must fail closed
	if _, err := ResolveTenantContext(sc, "hospital_c"); !errors.Is(err, ErrTenantForbidden) {
		t.Fatalf("expected ErrTenantForbidden for hospital_c, got %v", err)
	}
}

func TestTrustDocValidatorMultiIssuerShapesAndSecurity(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ec key: %v", err)
	}
	workloadKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("workload rsa key: %v", err)
	}

	// Serve EC issuer JWKS dynamically over HTTPS test server (AllowPlaintextHTTPJWKS=true for httptest)
	jwksCalls := 0
	rotatedKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	serveRotated := false
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwksCalls++
		keys := []JWK{ecPublicJWK("ec-1", "P-256", 32, &ecKey.PublicKey)}
		if serveRotated {
			keys = append(keys, rsaPublicJWK("rot-2", &rotatedKey.PublicKey))
		}
		_ = json.NewEncoder(w).Encode(JWKSet{Keys: keys})
	}))
	defer jwksSrv.Close()

	inlineRSAJWKS, _ := json.Marshal(JWKSet{Keys: []JWK{rsaPublicJWK("z-1", &rsaKey.PublicKey)}})
	inlineWorkloadJWKS, _ := json.Marshal(JWKSet{Keys: []JWK{rsaPublicJWK("w-1", &workloadKey.PublicKey)}})

	rawDoc := map[string]any{
		"issuers": []any{
			map[string]any{
				"issuer":            "https://zitadel.azra.example",
				"audiences":         []string{"azra-fhir", "flint"},
				"jwks":              json.RawMessage(inlineRSAJWKS),
				"signingAlgorithms": []string{"RS256", "PS256"},
				"claims": map[string]any{
					"shape":             "grantMap",
					"grantsClaim":       "urn:zitadel:iam:org:project:roles",
					"homeTenantClaim":   "home_tenant",
					"activeTenantClaim": "active_tenant",
					"operatorRole":      "azra-operator",
					"platformTenant":    "azra-platform",
					"actorClaim":        "act",
				},
			},
			map[string]any{
				"issuer":            "https://keycloak.azra.example/realms/main",
				"audiences":         []string{"azra-fhir"},
				"jwksUrl":           jwksSrv.URL,
				"signingAlgorithms": []string{"ES256", "RS256"},
				"claims": map[string]any{
					"shape":                 "list",
					"tenantsClaim":          "tenants",
					"rolesClaim":            "roles",
					"activeTenantClaim":     "active_tenant",
					"activeTenantSlugClaim": "active_tenant_slug",
				},
			},
			map[string]any{
				"issuer":            "https://workload.azra.internal",
				"audiences":         []string{"azra-fhir"},
				"jwks":              json.RawMessage(inlineWorkloadJWKS),
				"signingAlgorithms": []string{"RS256"},
				"claims": map[string]any{
					"shape":      "none",
					"actorClaim": "act",
				},
			},
		},
	}
	docBytes, _ := json.Marshal(rawDoc)
	doc, err := ParseTrustDoc(docBytes)
	if err != nil {
		t.Fatalf("ParseTrustDoc: %v", err)
	}

	val, err := NewTrustDocValidator(TrustDocConfig{
		Document: doc,
		Principals: []TrustPrincipal{
			{
				ID:               "ingest-worker",
				Issuer:           "https://workload.azra.internal",
				Subject:          "spiffe://azra/ingest",
				Roles:            []string{"ingest"},
				Scopes:           "system/Patient.crus system/Observation.crus",
				AllowCrossTenant: true,
			},
		},
		RoleToScopes: map[string]string{
			"clinician":     "user/Patient.rs user/Observation.rs",
			"tenant-admin":  "system/*.*",
			"azra-operator": "system/Patient.rs system/AuditEvent.rs",
		},
		MinRefreshInterval:     -1,
		AllowPlaintextHTTPJWKS: true,
	})
	if err != nil {
		t.Fatalf("NewTrustDocValidator: %v", err)
	}

	now := time.Now()

	// 1. GrantMap issuer with PS256 + RFC 8693 act + multi-tenant grants
	grantTok := signRSAJWT(t, "PS256", "z-1", rsaKey, map[string]any{
		"iss": "https://zitadel.azra.example",
		"sub": "svc-gateway",
		"aud": []string{"azra-fhir"},
		"exp": now.Add(5 * time.Minute).Unix(),
		"act": map[string]any{
			"sub": "dr-jane",
			"iss": "https://zitadel.azra.example",
		},
		"active_tenant": "tenant-alpha",
		"urn:zitadel:iam:org:project:roles": map[string]any{
			"clinician":    map[string]any{"tenant-alpha": true},
			"tenant-admin": map[string]any{"tenant-beta": true},
		},
	})
	sc, err := val.ValidateToken(context.Background(), grantTok)
	if err != nil {
		t.Fatalf("ValidateToken(grantMap): %v", err)
	}
	if sc.GetTenantId() != "tenant-alpha" || sc.GetActorSubject() != "dr-jane" || sc.GetActorIssuer() != "https://zitadel.azra.example" {
		t.Fatalf("unexpected SecurityContext: %+v", sc)
	}
	if !Allows(sc, "Patient", ActionRead) || Allows(sc, "Patient", ActionDelete) {
		t.Fatalf("tenant-alpha should allow Read only, not Delete from tenant-beta")
	}

	// Switch context via ResolveTenantContext to tenant-beta vs denied tenant-gamma
	scBeta, err := ResolveTenantContext(sc, "tenant-beta")
	if err != nil {
		t.Fatalf("ResolveTenantContext(tenant-beta): %v", err)
	}
	if !Allows(scBeta, "Patient", ActionDelete) {
		t.Fatalf("expected tenant-admin delete in tenant-beta")
	}
	if _, err := ResolveTenantContext(sc, "tenant-gamma"); !errors.Is(err, ErrTenantForbidden) {
		t.Fatalf("expected ErrTenantForbidden for tenant-gamma, got %v", err)
	}

	// 2. Operator role in platform tenant permits cross-tenant resolution using operator scopes ONLY
	opTok := signRSAJWT(t, "RS256", "z-1", rsaKey, map[string]any{
		"iss":           "https://zitadel.azra.example",
		"sub":           "op-bob",
		"aud":           "flint",
		"exp":           now.Add(5 * time.Minute).Unix(),
		"active_tenant": "tenant-alpha",
		"urn:zitadel:iam:org:project:roles": map[string]any{
			"clinician":     map[string]any{"tenant-alpha": true},
			"azra-operator": map[string]any{"azra-platform": true},
		},
	})
	scOp, err := val.ValidateToken(context.Background(), opTok)
	if err != nil {
		t.Fatalf("ValidateToken(operator): %v", err)
	}
	if !scOp.GetOperatorCrossTenant() {
		t.Fatalf("expected OperatorCrossTenant=true")
	}
	scCross, err := ResolveTenantContext(scOp, "tenant-gamma")
	if err != nil {
		t.Fatalf("expected cross-tenant resolution for operator, got %v", err)
	}
	if !Allows(scCross, "AuditEvent", ActionRead) || Allows(scCross, "Observation", ActionRead) {
		t.Fatalf("cross-tenant operator should use azra-platform operator scopes, not tenant-alpha clinician scopes")
	}

	// 3. List shape issuer via dynamic ES256 JWKS + active_tenant_slug
	listTok := signECDSAJWT(t, "ES256", "ec-1", ecKey, map[string]any{
		"iss":                "https://keycloak.azra.example/realms/main",
		"sub":                "dr-sara",
		"aud":                "azra-fhir",
		"exp":                now.Add(5 * time.Minute).Unix(),
		"tenants":            []string{"t-uuid-1", "t-uuid-2"},
		"roles":              []string{"clinician"},
		"active_tenant":      "t-uuid-1",
		"active_tenant_slug": "mercy-general",
	})
	scList, err := val.ValidateToken(context.Background(), listTok)
	if err != nil {
		t.Fatalf("ValidateToken(list): %v", err)
	}
	if scList.GetTenantId() != "mercy-general" || !Allows(scList, "Observation", ActionSearch) {
		t.Fatalf("unexpected list claim context: tenant=%q allows=%v", scList.GetTenantId(), Allows(scList, "Observation", ActionSearch))
	}

	// Verify JWKS rotation refresh on new kid
	serveRotated = true
	rotTok := signRSAJWT(t, "RS256", "rot-2", rotatedKey, map[string]any{
		"iss":                "https://keycloak.azra.example/realms/main",
		"sub":                "dr-sara",
		"aud":                "azra-fhir",
		"exp":                now.Add(5 * time.Minute).Unix(),
		"tenants":            []string{"t-uuid-1"},
		"active_tenant":      "t-uuid-1",
		"active_tenant_slug": "mercy-general",
	})
	if _, err := val.ValidateToken(context.Background(), rotTok); err != nil {
		t.Fatalf("expected rotated JWKS key validation to succeed: %v", err)
	}
	if jwksCalls < 2 {
		t.Fatalf("expected JWKS endpoint refresh on kid miss, got %d calls", jwksCalls)
	}

	// 4. Shape none + workload Principal allowlist + RFC 8693 actor claim
	allowedWorkloadTok := signRSAJWT(t, "RS256", "w-1", workloadKey, map[string]any{
		"iss": "https://workload.azra.internal",
		"sub": "spiffe://azra/ingest",
		"aud": "azra-fhir",
		"exp": now.Add(5 * time.Minute).Unix(),
		"act": map[string]any{"sub": "human-alice", "iss": "https://zitadel.azra.example"},
	})
	scWork, err := val.ValidateToken(context.Background(), allowedWorkloadTok)
	if err != nil {
		t.Fatalf("ValidateToken(workload allowed): %v", err)
	}
	if scWork.GetPrincipalId() != "ingest-worker" || !scWork.GetOperatorCrossTenant() || scWork.GetActorSubject() != "human-alice" {
		t.Fatalf("unexpected workload SecurityContext: %+v", scWork)
	}
	if !Allows(scWork, "Observation", ActionCreate) {
		t.Fatalf("expected workload principal scopes to grant Observation.create")
	}

	// Unlisted workload caller MUST be rejected
	rogueWorkloadTok := signRSAJWT(t, "RS256", "w-1", workloadKey, map[string]any{
		"iss": "https://workload.azra.internal",
		"sub": "spiffe://azra/rogue-pod",
		"aud": "azra-fhir",
		"exp": now.Add(5 * time.Minute).Unix(),
	})
	if _, err := val.ValidateToken(context.Background(), rogueWorkloadTok); !errors.Is(err, ErrPrincipalNotAllowed) {
		t.Fatalf("expected ErrPrincipalNotAllowed for unlisted workload, got %v", err)
	}

	// Malformed actor claim must fail closed
	badActTok := signRSAJWT(t, "RS256", "w-1", workloadKey, map[string]any{
		"iss": "https://workload.azra.internal",
		"sub": "spiffe://azra/ingest",
		"aud": "azra-fhir",
		"exp": now.Add(5 * time.Minute).Unix(),
		"act": "not-an-object",
	})
	if _, err := val.ValidateToken(context.Background(), badActTok); !errors.Is(err, ErrClaimsShape) {
		t.Fatalf("expected ErrClaimsShape for malformed act claim, got %v", err)
	}

	// Unknown issuer must fail closed
	unknownIssTok := signRSAJWT(t, "RS256", "w-1", workloadKey, map[string]any{
		"iss": "https://evil.attacker.example",
		"sub": "spiffe://azra/ingest",
		"aud": "azra-fhir",
		"exp": now.Add(5 * time.Minute).Unix(),
	})
	if _, err := val.ValidateToken(context.Background(), unknownIssTok); !errors.Is(err, ErrInvalidIssuer) {
		t.Fatalf("expected ErrInvalidIssuer for unknown issuer, got %v", err)
	}
}

func TestTrustDocValidationSecurityControls(t *testing.T) {
	// Reject private key parameter in inline JWKS
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	privJWK := map[string]any{
		"keys": []any{
			map[string]any{
				"kty": "RSA",
				"kid": "k1",
				"n":   b64url(rsaKey.N.Bytes()),
				"e":   b64url(big.NewInt(int64(rsaKey.E)).Bytes()),
				"d":   b64url(rsaKey.D.Bytes()),
			},
		},
	}
	privBytes, _ := json.Marshal(privJWK)
	if _, err := NewTrustDocValidator(TrustDocConfig{
		Document: TrustDocument{
			Issuers: []TrustIssuer{{
				Issuer:    "https://idp.example",
				Audiences: []string{"aud"},
				JWKS:      privBytes,
				Claims:    TrustClaims{Shape: TrustClaimShapeNone},
			}},
		},
	}); !errors.Is(err, ErrTrustDocInvalid) {
		t.Fatalf("expected private JWK rejection, got %v", err)
	}

	// Reject plaintext HTTP jwksUrl when AllowPlaintextHTTPJWKS=false
	if _, err := NewTrustDocValidator(TrustDocConfig{
		Document: TrustDocument{
			Issuers: []TrustIssuer{{
				Issuer:    "https://idp.example",
				Audiences: []string{"aud"},
				JWKSURL:   "http://idp.example/.well-known/jwks.json",
				Claims:    TrustClaims{Shape: TrustClaimShapeNone},
			}},
		},
	}); !errors.Is(err, ErrTrustDocInvalid) {
		t.Fatalf("expected plaintext http jwksUrl rejection, got %v", err)
	}

	// ParseRoleToScopesMap coverage
	m, err := ParseRoleToScopesMap(`clinician=user/Patient.rs user/Observation.rs;admin=system/*.*`)
	if err != nil {
		t.Fatalf("ParseRoleToScopesMap: %v", err)
	}
	if m["clinician"] != "user/Patient.rs user/Observation.rs" || m["admin"] != "system/*.*" {
		t.Fatalf("unexpected parsed map: %+v", m)
	}
}
