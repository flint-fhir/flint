package auth_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
	"github.com/flint-fhir/flint/pkg/auth"
)

func TestParseScopes(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantCount  int
		assertions func(t *testing.T, scopes []*authv1.SmartScope)
	}{
		{
			name:      "SMART v1 read and write",
			raw:       "patient/Patient.read patient/Observation.write",
			wantCount: 2,
			assertions: func(t *testing.T, scopes []*authv1.SmartScope) {
				s1 := scopes[0]
				assert.Equal(t, authv1.ScopeContext_SCOPE_CONTEXT_PATIENT, s1.Context)
				assert.Equal(t, "Patient", s1.ResourceType)
				assert.True(t, auth.ScopeMatches(s1, "Patient", auth.ActionRead))
				assert.True(t, auth.ScopeMatches(s1, "Patient", auth.ActionSearch))
				assert.False(t, auth.ScopeMatches(s1, "Patient", auth.ActionCreate))

				s2 := scopes[1]
				assert.Equal(t, "Observation", s2.ResourceType)
				assert.True(t, auth.ScopeMatches(s2, "Observation", auth.ActionCreate))
				assert.True(t, auth.ScopeMatches(s2, "Observation", auth.ActionUpdate))
				assert.True(t, auth.ScopeMatches(s2, "Observation", auth.ActionDelete))
				assert.False(t, auth.ScopeMatches(s2, "Observation", auth.ActionRead))
			},
		},
		{
			name:      "SMART v2 cruds letters",
			raw:       "patient/Observation.rs user/*.cruds system/*.r",
			wantCount: 3,
			assertions: func(t *testing.T, scopes []*authv1.SmartScope) {
				s1 := scopes[0]
				assert.Equal(t, "Observation", s1.ResourceType)
				assert.True(t, auth.ScopeMatches(s1, "Observation", auth.ActionRead))
				assert.True(t, auth.ScopeMatches(s1, "Observation", auth.ActionSearch))
				assert.False(t, auth.ScopeMatches(s1, "Observation", auth.ActionCreate))

				s2 := scopes[1]
				assert.Equal(t, "*", s2.ResourceType)
				assert.True(t, auth.ScopeMatches(s2, "Encounter", auth.ActionCreate))
				assert.True(t, auth.ScopeMatches(s2, "Patient", auth.ActionDelete))

				s3 := scopes[2]
				assert.Equal(t, authv1.ScopeContext_SCOPE_CONTEXT_SYSTEM, s3.Context)
				assert.True(t, auth.ScopeMatches(s3, "Condition", auth.ActionRead))
				assert.False(t, auth.ScopeMatches(s3, "Condition", auth.ActionSearch))
			},
		},
		{
			name:      "SMART v2 query parameters stripped",
			raw:       "patient/Observation.rs?category=vital-signs",
			wantCount: 1,
			assertions: func(t *testing.T, scopes []*authv1.SmartScope) {
				s1 := scopes[0]
				assert.Equal(t, "Observation", s1.ResourceType)
				assert.True(t, auth.ScopeMatches(s1, "Observation", auth.ActionRead))
				assert.True(t, auth.ScopeMatches(s1, "Observation", auth.ActionSearch))
			},
		},
		{
			name:      "Wildcard asterisk",
			raw:       "user/*.*",
			wantCount: 1,
			assertions: func(t *testing.T, scopes []*authv1.SmartScope) {
				s1 := scopes[0]
				assert.True(t, auth.ScopeMatches(s1, "AnyResource", auth.ActionCreate))
				assert.True(t, auth.ScopeMatches(s1, "AnyResource", auth.ActionRead))
				assert.True(t, auth.ScopeMatches(s1, "AnyResource", auth.ActionUpdate))
				assert.True(t, auth.ScopeMatches(s1, "AnyResource", auth.ActionDelete))
				assert.True(t, auth.ScopeMatches(s1, "AnyResource", auth.ActionSearch))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scopes := auth.ParseScopes(tt.raw)
			require.Len(t, scopes, tt.wantCount)
			tt.assertions(t, scopes)
		})
	}
}

func TestSecurityContext_Allows(t *testing.T) {
	sc := &authv1.SecurityContext{
		Subject:   "user-1",
		PatientId: "pat-123",
		Scopes:    auth.ParseScopes("patient/Patient.r patient/Observation.rs"),
	}

	assert.True(t, auth.Allows(sc, "Patient", auth.ActionRead))
	assert.False(t, auth.Allows(sc, "Patient", auth.ActionSearch))
	assert.False(t, auth.Allows(sc, "Patient", auth.ActionCreate))

	assert.True(t, auth.Allows(sc, "Observation", auth.ActionRead))
	assert.True(t, auth.Allows(sc, "Observation", auth.ActionSearch))
	assert.False(t, auth.Allows(sc, "Observation", auth.ActionCreate))

	assert.False(t, auth.Allows(sc, "Encounter", auth.ActionRead))

	assert.True(t, auth.IsPatientRestricted(sc))

	// A token with patient/* scopes is still considered patient-restricted even if patient_id claim is missing
	scMissingPatientID := &authv1.SecurityContext{
		Subject:   "user-no-patient-claim",
		PatientId: "",
		Scopes:    auth.ParseScopes("patient/Patient.rs patient/Observation.rs"),
	}
	assert.True(t, auth.IsPatientRestricted(scMissingPatientID))

	// A token with only user/* or system/* scopes and no patient_id is NOT patient-restricted
	scSystem := &authv1.SecurityContext{
		Subject:   "sys-worker",
		PatientId: "",
		Scopes:    auth.ParseScopes("user/Patient.rs system/Observation.cruds"),
	}
	assert.False(t, auth.IsPatientRestricted(scSystem))
	assert.False(t, auth.IsPatientRestricted(nil))
}

func TestOIDCValidator_EndToEnd(t *testing.T) {
	issuer := "https://auth.example.com"
	audience := "flint-api"

	mock, err := auth.NewMockTokenValidator(issuer, audience)
	require.NoError(t, err)

	// Configure OIDCValidator with mock's static JWKS
	oidcVal, err := auth.NewOIDCValidator(auth.OIDCConfig{
		Issuer:     issuer,
		Audience:   audience,
		StaticJWKS: mock.JWKS(),
	})
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("Valid Token", func(t *testing.T) {
		token, err := mock.IssueToken("sub-123", "patient/Patient.r patient/Observation.rs", "pat-456", time.Hour, map[string]any{
			"tenant_id": "tenant_alpha",
		})
		require.NoError(t, err)

		secCtx, err := oidcVal.ValidateToken(ctx, token)
		require.NoError(t, err)
		assert.Equal(t, "sub-123", secCtx.Subject)
		assert.Equal(t, "pat-456", secCtx.PatientId)
		assert.Equal(t, "tenant_alpha", secCtx.TenantId)
		assert.True(t, auth.Allows(secCtx, "Patient", auth.ActionRead))
		assert.True(t, auth.Allows(secCtx, "Observation", auth.ActionSearch))
	})

	t.Run("Expired Token", func(t *testing.T) {
		token, err := mock.IssueToken("sub-expired", "system/*.r", "", -time.Minute, nil)
		require.NoError(t, err)

		_, err = oidcVal.ValidateToken(ctx, token)
		assert.ErrorIs(t, err, auth.ErrTokenExpired)
	})

	t.Run("Tampered Signature", func(t *testing.T) {
		token, err := mock.IssueToken("sub-tamper", "system/*.r", "", time.Hour, nil)
		require.NoError(t, err)

		tampered := token[:len(token)-4] + "AAAA"
		_, err = oidcVal.ValidateToken(ctx, tampered)
		assert.ErrorIs(t, err, auth.ErrInvalidSignature)
	})
}

func TestZitadelValidator(t *testing.T) {
	issuer := "https://zitadel.azra.dev"
	audience := "flint-api"

	mock, err := auth.NewMockTokenValidator(issuer, audience)
	require.NoError(t, err)

	zitadelVal, err := auth.NewZitadelValidator(auth.ZitadelConfig{
		OIDCConfig: auth.OIDCConfig{
			Issuer:     issuer,
			Audience:   audience,
			StaticJWKS: mock.JWKS(),
		},
		TenantFromOrgDomain: true,
		RoleToScopes: map[string]string{
			"clinical-reader": "patient/*.rs user/*.rs",
			"admin":           "system/*.cruds",
		},
	})
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("Zitadel Org Domain, User Meta, and Roles", func(t *testing.T) {
		token, err := mock.IssueToken("zitadel-user-1", "openid", "", time.Hour, map[string]any{
			auth.ZitadelClaimOrgDomain: "st-jude-hospital.azra.dev",
			auth.ZitadelClaimUserMeta:  `{"patient_id":"pat-999"}`,
			auth.ZitadelClaimRoles:     `{"clinical-reader":{"org-1":true}}`,
		})
		require.NoError(t, err)

		secCtx, err := zitadelVal.ValidateToken(ctx, token)
		require.NoError(t, err)

		// Verified Org Domain mapped to Tenant schema
		assert.Equal(t, "st_jude_hospital", secCtx.TenantId)
		// Verified user meta mapped to patient_id
		assert.Equal(t, "pat-999", secCtx.PatientId)
		// Verified project role mapped to SMART scopes
		assert.True(t, auth.Allows(secCtx, "Patient", auth.ActionRead))
		assert.True(t, auth.Allows(secCtx, "Observation", auth.ActionSearch))
	})

	t.Run("Zitadel Nested JSON Object Claims and Base64 Metadata", func(t *testing.T) {
		token, err := mock.IssueToken("zitadel-user-2", "openid", "", time.Hour, map[string]any{
			auth.ZitadelClaimOrgDomain: "mercy-health.azra.dev",
			auth.ZitadelClaimUserMeta: map[string]any{
				"patient_id": base64.StdEncoding.EncodeToString([]byte("pat-b64-777")),
			},
			auth.ZitadelClaimRoles: map[string]any{
				"admin": map[string]any{"org-mercy": "mercy-health.azra.dev"},
			},
		})
		require.NoError(t, err)

		secCtx, err := zitadelVal.ValidateToken(ctx, token)
		require.NoError(t, err)
		assert.Equal(t, "mercy_health", secCtx.TenantId)
		assert.Equal(t, "pat-b64-777", secCtx.PatientId)
		assert.True(t, auth.Allows(secCtx, "Patient", auth.ActionCreate))
		assert.True(t, auth.Allows(secCtx, "Observation", auth.ActionDelete))
	})
}

func TestJWTVerifyClaims_Adversarial(t *testing.T) {
	now := time.Unix(1700000000, 0)

	t.Run("Rejects Missing or Zero Expiration Claim", func(t *testing.T) {
		tok := &auth.ParsedToken{
			Payload: auth.JWTPayload{
				Issuer:    "https://issuer.example",
				Subject:   "user-1",
				Audience:  auth.Audience{"flint-api"},
				ExpiresAt: 0,
			},
		}
		err := tok.VerifyClaims("https://issuer.example", "flint-api", now)
		assert.ErrorIs(t, err, auth.ErrTokenExpired)
	})

	t.Run("Rejects Missing Audience When ExpectedAudience Is Configured", func(t *testing.T) {
		tok := &auth.ParsedToken{
			Payload: auth.JWTPayload{
				Issuer:    "https://issuer.example",
				Subject:   "user-1",
				Audience:  nil,
				ExpiresAt: now.Add(time.Hour).Unix(),
			},
		}
		err := tok.VerifyClaims("https://issuer.example", "flint-api", now)
		assert.ErrorIs(t, err, auth.ErrInvalidAudience)
	})

	t.Run("Rejects Wrong Audience and Accepts Array Audience", func(t *testing.T) {
		tokWrong := &auth.ParsedToken{
			Payload: auth.JWTPayload{
				Issuer:    "https://issuer.example",
				Subject:   "user-1",
				Audience:  auth.Audience{"other-service", "billing-api"},
				ExpiresAt: now.Add(time.Hour).Unix(),
			},
		}
		assert.ErrorIs(t, tokWrong.VerifyClaims("https://issuer.example", "flint-api", now), auth.ErrInvalidAudience)

		tokRight := &auth.ParsedToken{
			Payload: auth.JWTPayload{
				Issuer:    "https://issuer.example",
				Subject:   "user-1",
				Audience:  auth.Audience{"other-service", "flint-api"},
				ExpiresAt: now.Add(time.Hour).Unix(),
			},
		}
		assert.NoError(t, tokRight.VerifyClaims("https://issuer.example", "flint-api", now))
	})
}

func TestJWKPublicKey_BoundsValidation(t *testing.T) {
	t.Run("Oversized EC Coordinates Return Error Instead of Panicking", func(t *testing.T) {
		oversized := base64.RawURLEncoding.EncodeToString(make([]byte, 64))
		jwk := auth.JWK{
			Kty: "EC",
			Crv: "P-256",
			X:   oversized,
			Y:   oversized,
		}
		_, err := jwk.PublicKey()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid EC x coordinate length")
	})

	t.Run("Leading Zero 33-Byte P-256 Coordinate Succeeds", func(t *testing.T) {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		uncompressed, err := priv.PublicKey.Bytes()
		require.NoError(t, err)
		require.Len(t, uncompressed, 65)
		xBytes := append([]byte{0x00}, uncompressed[1:33]...)
		yBytes := uncompressed[33:65]
		jwk := auth.JWK{
			Kty: "EC",
			Crv: "P-256",
			X:   base64.RawURLEncoding.EncodeToString(xBytes),
			Y:   base64.RawURLEncoding.EncodeToString(yBytes),
		}
		pub, err := jwk.PublicKey()
		require.NoError(t, err)
		require.NotNil(t, pub)
	})

	t.Run("Oversized or Non-Positive RSA Exponent Rejected", func(t *testing.T) {
		jwkHugeE := auth.JWK{
			Kty: "RSA",
			N:   base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3, 4}),
			E:   base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
		}
		_, err := jwkHugeE.PublicKey()
		require.Error(t, err)

		jwkSmallE := auth.JWK{
			Kty: "RSA",
			N:   base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3, 4}),
			E:   base64.RawURLEncoding.EncodeToString([]byte{1}),
		}
		_, err = jwkSmallE.PublicKey()
		require.Error(t, err)
	})
}

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	_, ok := auth.FromContext(ctx)
	assert.False(t, ok)

	sc := &authv1.SecurityContext{Subject: "user-1"}
	ctx = auth.WithSecurityContext(ctx, sc)

	retrieved, ok := auth.FromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, "user-1", retrieved.Subject)
}

func TestOIDCValidator_HTTPJWKSAndHardening(t *testing.T) {
	ctx := context.Background()
	audience := "flint-api"

	// Generate an RSA 2048 keypair to test RS256 signing and JWK Set discovery over HTTP
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	rsaJWK := auth.JWK{
		Kty: "RSA",
		Use: "sig",
		Alg: "RS256",
		Kid: "rsa-key-1",
		N:   base64.RawURLEncoding.EncodeToString(rsaPriv.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaPriv.E)).Bytes()),
	}
	encJWK := auth.JWK{
		Kty: "RSA",
		Use: "enc",
		Alg: "RSA-OAEP",
		Kid: "enc-key-only",
		N:   rsaJWK.N,
		E:   rsaJWK.E,
	}

	var jwksFetchCount atomic.Int32
	var activeJWKS atomic.Value
	activeJWKS.Store(auth.JWKSet{Keys: []auth.JWK{encJWK, rsaJWK}})

	var serverURL string
	idpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"issuer":   serverURL,
				"jwks_uri": serverURL + "/oauth/v2/keys",
			})
		case "/oauth/v2/keys":
			jwksFetchCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(activeJWKS.Load().(auth.JWKSet))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer idpServer.Close()
	serverURL = idpServer.URL

	validator, err := auth.NewOIDCValidator(auth.OIDCConfig{
		Issuer:             serverURL,
		Audience:           audience,
		JWKSURL:            serverURL + "/oauth/v2/keys",
		HTTPClient:         idpServer.Client(),
		CacheTTL:           10 * time.Minute,
		MinRefreshInterval: 500 * time.Millisecond,
	})
	require.NoError(t, err)

	signRS256 := func(kid, alg string, exp time.Time) string {
		hdr, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": kid})
		payload, _ := json.Marshal(map[string]any{
			"iss":   serverURL,
			"sub":   "rsa-practitioner-1",
			"aud":   audience,
			"exp":   exp.Unix(),
			"iat":   time.Now().Unix(),
			"scope": "user/Patient.rs user/Observation.cruds",
		})
		unsigned := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(payload)
		digest := sha256.Sum256([]byte(unsigned))
		sig, err := rsa.SignPKCS1v15(rand.Reader, rsaPriv, crypto.SHA256, digest[:])
		require.NoError(t, err)
		return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
	}

	t.Run("Live OIDC Discovery and RS256 Verification", func(t *testing.T) {
		tok := signRS256("rsa-key-1", "RS256", time.Now().Add(time.Hour))
		secCtx, err := validator.ValidateToken(ctx, tok)
		require.NoError(t, err)
		assert.Equal(t, "rsa-practitioner-1", secCtx.GetSubject())
		assert.Equal(t, int32(1), jwksFetchCount.Load())
	})

	t.Run("Rejects Non-Signing Key (use=enc)", func(t *testing.T) {
		tok := signRS256("enc-key-only", "RS256", time.Now().Add(time.Hour))
		_, err := validator.ValidateToken(ctx, tok)
		require.ErrorIs(t, err, auth.ErrKeyNotFound)
	})

	t.Run("Rejects Unsupported Algorithms Before Triggering Any JWKS HTTP Refresh", func(t *testing.T) {
		beforeCount := jwksFetchCount.Load()
		tokHS256 := signRS256("unknown-kid-999", "HS256", time.Now().Add(time.Hour))
		_, err := validator.ValidateToken(ctx, tokHS256)
		require.ErrorIs(t, err, auth.ErrUnsupportedAlg)
		assert.Equal(t, beforeCount, jwksFetchCount.Load(), "unsupported alg must be rejected before JWKS fetch")

		tokNone := signRS256("unknown-kid-999", "none", time.Now().Add(time.Hour))
		_, err = validator.ValidateToken(ctx, tokNone)
		require.ErrorIs(t, err, auth.ErrUnsupportedAlg)
		assert.Equal(t, beforeCount, jwksFetchCount.Load())
	})

	t.Run("MinRefreshInterval Cooldown Prevents JWKS Hammering on Unknown kid", func(t *testing.T) {
		// Wait for cooldown to expire once, then send 5 tokens with random kids in a burst
		time.Sleep(550 * time.Millisecond)
		beforeCount := jwksFetchCount.Load()

		for i := range 5 {
			tok := signRS256("forged-kid-"+string(rune('A'+i)), "RS256", time.Now().Add(time.Hour))
			_, err := validator.ValidateToken(ctx, tok)
			require.ErrorIs(t, err, auth.ErrKeyNotFound)
		}

		// Only the first miss in the 500ms window should trigger a JWKS HTTP fetch
		assert.Equal(t, beforeCount+1, jwksFetchCount.Load(), "expected MinRefreshInterval to rate-limit JWKS refresh on kid misses")
	})

	t.Run("Key Rotation Succeeds After Cooldown", func(t *testing.T) {
		time.Sleep(550 * time.Millisecond)
		rotatedJWK := rsaJWK
		rotatedJWK.Kid = "rsa-key-rotated-2"
		activeJWKS.Store(auth.JWKSet{Keys: []auth.JWK{rotatedJWK}})

		tokRotated := signRS256("rsa-key-rotated-2", "RS256", time.Now().Add(time.Hour))
		secCtx, err := validator.ValidateToken(ctx, tokRotated)
		require.NoError(t, err)
		assert.Equal(t, "rsa-practitioner-1", secCtx.GetSubject())
	})
}

func TestGranularScopeConstraintsAndMockAndAlgorithmsWave55(t *testing.T) {
	// 1. ScopeQueryConstraints & TokenValueMatchesConstraint
	vals, ok := auth.ScopeQueryConstraints(nil, "Observation", auth.ActionRead)
	assert.False(t, ok)
	assert.Nil(t, vals)

	scConstrained := &authv1.SecurityContext{
		Scopes: auth.ParseScopes("patient/Observation.rs?category=http://terminology.hl7.org/CodeSystem/observation-category|laboratory patient/Observation.rs?category=vital-signs"),
	}
	vals, ok = auth.ScopeQueryConstraints(scConstrained, "Observation", auth.ActionRead)
	assert.True(t, ok)
	assert.Equal(t, "http://terminology.hl7.org/CodeSystem/observation-category|laboratory,vital-signs", vals.Get("category"))

	// Unconstrained scope overrides constrained scope for the same resource type
	scMixed := &authv1.SecurityContext{
		Scopes: auth.ParseScopes("patient/Observation.rs?category=vital-signs patient/Observation.rs"),
	}
	_, ok = auth.ScopeQueryConstraints(scMixed, "Observation", auth.ActionRead)
	assert.False(t, ok)

	assert.True(t, auth.TokenValueMatchesConstraint("vital-signs", "http://terminology.hl7.org/CodeSystem/observation-category|laboratory,vital-signs"))
	assert.True(t, auth.TokenValueMatchesConstraint("http://terminology.hl7.org/CodeSystem/observation-category|laboratory", "laboratory"))
	assert.False(t, auth.TokenValueMatchesConstraint("social-history", "laboratory,vital-signs"))

	// 2. MockTokenValidator static & custom hooks
	mock, err := auth.NewMockTokenValidator("https://mock.issuer", "mock-aud")
	require.NoError(t, err)
	mock.RegisterStatic("static-tok-1", &authv1.SecurityContext{Subject: "static-sub"})
	scStatic, err := mock.ValidateToken(t.Context(), "static-tok-1")
	require.NoError(t, err)
	assert.Equal(t, "static-sub", scStatic.Subject)

	mock.CustomValidate = func(_ context.Context, tok string) (*authv1.SecurityContext, error) {
		return &authv1.SecurityContext{Subject: "custom-" + tok}, nil
	}
	scCustom, err := mock.ValidateToken(t.Context(), "abc")
	require.NoError(t, err)
	assert.Equal(t, "custom-abc", scCustom.Subject)

	// 3. VerifySignature for RS384, ES256, ES384, HS256 & JWKSet.FindKey with empty kid
	ec256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ec384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	pub384Bytes, err := ec384.PublicKey.Bytes()
	require.NoError(t, err)
	jwk384 := auth.JWK{
		Kty: "EC",
		Crv: "P-384",
		Kid: "ec384-1",
		Use: "sig",
		X:   base64.RawURLEncoding.EncodeToString(pub384Bytes[1:49]),
		Y:   base64.RawURLEncoding.EncodeToString(pub384Bytes[49:97]),
	}
	parsedPub384, err := jwk384.PublicKey()
	require.NoError(t, err)

	set := auth.JWKSet{Keys: []auth.JWK{jwk384}}
	foundKey, err := set.FindKey("")
	require.NoError(t, err)
	assert.Equal(t, "ec384-1", foundKey.Kid)

	// Sign ES256, ES384, and HS256 tokens and verify
	signedData := []byte("eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiIxIn0")
	h256 := sha256.Sum256(signedData)
	r256, s256, err := ecdsa.Sign(rand.Reader, ec256, h256[:])
	require.NoError(t, err)
	sig256 := make([]byte, 64)
	r256.FillBytes(sig256[:32])
	s256.FillBytes(sig256[32:])
	tokES256 := &auth.ParsedToken{
		Header:       auth.JWTHeader{Alg: "ES256"},
		SignedData:   signedData,
		RawSignature: sig256,
	}
	require.NoError(t, tokES256.VerifySignature(&ec256.PublicKey))
	require.ErrorIs(t, tokES256.VerifySignature([]byte("wrong-type")), auth.ErrUnsupportedKeyType)

	tokES384 := &auth.ParsedToken{
		Header:       auth.JWTHeader{Alg: "ES384"},
		SignedData:   signedData,
		RawSignature: make([]byte, 10), // wrong length -> ErrInvalidSignature
	}
	require.ErrorIs(t, tokES384.VerifySignature(parsedPub384), auth.ErrInvalidSignature)

	// Zitadel fallback to OrgID and encounter_id metadata
	mockZ, err := auth.NewMockTokenValidator("https://zitadel.example", "aud")
	require.NoError(t, err)
	zVal, err := auth.NewZitadelValidator(auth.ZitadelConfig{
		OIDCConfig: auth.OIDCConfig{
			Issuer:     "https://zitadel.example",
			Audience:   "aud",
			StaticJWKS: mockZ.JWKS(),
		},
		TenantFromOrgDomain: false,
	})
	require.NoError(t, err)
	zTok, err := mockZ.IssueToken("u1", "openid", "", time.Hour, map[string]any{
		auth.ZitadelClaimOrgID:    "org_987",
		auth.ZitadelClaimUserMeta: `{"encounter_id":"enc-555"}`,
	})
	require.NoError(t, err)
	zCtx, err := zVal.ValidateToken(t.Context(), zTok)
	require.NoError(t, err)
	assert.Equal(t, "org_987", zCtx.TenantId)
	assert.Equal(t, "enc-555", zCtx.EncounterId)
}
