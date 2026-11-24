package server_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/sqlite"
)

func b64Test(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func signTestRS256(t *testing.T, kid string, priv *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	hdrJSON, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	require.NoError(t, err)
	payJSON, err := json.Marshal(claims)
	require.NoError(t, err)
	signingInput := b64Test(hdrJSON) + "." + b64Test(payJSON)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	require.NoError(t, err)
	return signingInput + "." + b64Test(sig)
}

func testRSAPublicJWK(kid string, pub *rsa.PublicKey) auth.JWK {
	return auth.JWK{
		Kty: "RSA",
		Kid: kid,
		Use: "sig",
		N:   b64Test(pub.N.Bytes()),
		E:   b64Test(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func newHardeningSQLiteServer(t *testing.T, dbPath string, policy server.SecurityPolicy, validator auth.TokenValidator) (*server.Server, store.Store, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "hardening.db")
	}
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, sqlite.RunMigrations(ctx, db, sqlite.JournalWAL))
	st := sqlite.New(db, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(dbPath))
	require.NoError(t, st.LoadFromDB(ctx))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	t.Cleanup(func() { _ = srv.Close() })
	server.RegisterDefaultResourceTypes(srv)
	if validator != nil {
		srv.SetTokenValidator(validator)
	}
	srv.SetSecurityPolicy(policy)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, st, ts
}

func doHardeningReq(t *testing.T, method, targetURL string, headers map[string]string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		switch b := body.(type) {
		case []byte:
			reader = bytes.NewReader(b)
		case string:
			reader = strings.NewReader(b)
		default:
			raw, err := json.Marshal(b)
			require.NoError(t, err)
			reader = bytes.NewReader(raw)
		}
	}
	req, err := http.NewRequest(method, targetURL, reader)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/fhir+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/fhir+json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	respBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	return resp, respBytes
}

func TestSecurityHardening_FailClosedAuthAndCORS(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	noAuthSrv := server.New(nil, logger)
	defer func() { _ = noAuthSrv.Close() }()
	server.RegisterDefaultResourceTypes(noAuthSrv)
	noAuthSrv.SetSecurityPolicy(server.SecurityPolicy{RequireAuth: true})
	require.ErrorContains(t, noAuthSrv.ValidateSecurityPolicy(), "RequireAuth")

	noAuthTS := httptest.NewServer(noAuthSrv.Handler())
	defer noAuthTS.Close()
	resp, _ := doHardeningReq(t, http.MethodGet, noAuthTS.URL+"/fhir/r4/t1/Patient", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	jwksJSON, _ := json.Marshal(auth.JWKSet{Keys: []auth.JWK{testRSAPublicJWK("k1", &rsaKey.PublicKey)}})
	val, err := auth.NewTrustDocValidator(auth.TrustDocConfig{
		Document: auth.TrustDocument{
			Issuers: []auth.TrustIssuer{{
				Issuer:            "https://idp.azra.example",
				Audiences:         []string{"azra-fhir"},
				JWKS:              jwksJSON,
				SigningAlgorithms: []string{"RS256"},
				Claims:            auth.TrustClaims{Shape: auth.TrustClaimShapeNone},
			}},
		},
	})
	require.NoError(t, err)

	_, _, ts := newHardeningSQLiteServer(t, "", server.SecurityPolicy{
		RequireAuth:                     true,
		RequireTenantClaim:              true,
		DisableUnauthenticatedEndpoints: true,
		AllowedOrigins:                  []string{"https://app.azra.health"},
	}, val)

	// Unauthenticated bulk/uds/health-cards endpoints fail closed with 401
	for _, path := range []string{
		"/fhir/r4/t1/$bulk-publish",
		"/fhir/r4/t1/$uds-plus-manifest",
		"/fhir/r4/t1/Patient/$health-cards-issue",
	} {
		method := http.MethodGet
		if strings.Contains(path, "$health-cards-issue") {
			method = http.MethodPost
		}
		r, _ := doHardeningReq(t, method, ts.URL+path, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, r.StatusCode, "endpoint %s should require auth", path)
	}

	// Token with shape=none (no tenant claim) rejected with 403 when RequireTenantClaim=true
	now := time.Now()
	tok := signTestRS256(t, "k1", rsaKey, map[string]any{
		"iss": "https://idp.azra.example",
		"sub": "u-no-tenant",
		"aud": "azra-fhir",
		"exp": now.Add(5 * time.Minute).Unix(),
	})
	r, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/t1/Patient", map[string]string{
		"Authorization": "Bearer " + tok,
	}, nil)
	assert.Equal(t, http.StatusForbidden, r.StatusCode)

	// Restricted CORS: allowed origin succeeds, disallowed origin rejected on preflight & omits ACAO
	preflightOK, _ := doHardeningReq(t, http.MethodOptions, ts.URL+"/fhir/r4/t1/metadata", map[string]string{
		"Origin": "https://app.azra.health",
	}, nil)
	assert.Equal(t, http.StatusNoContent, preflightOK.StatusCode)
	assert.Equal(t, "https://app.azra.health", preflightOK.Header.Get("Access-Control-Allow-Origin"))
	assert.Contains(t, preflightOK.Header.Get("Access-Control-Allow-Headers"), "X-Azra-Tenant-Id")
	assert.Contains(t, preflightOK.Header.Get("Access-Control-Allow-Headers"), "Idempotency-Key")

	preflightDeny, _ := doHardeningReq(t, http.MethodOptions, ts.URL+"/fhir/r4/t1/metadata", map[string]string{
		"Origin": "https://evil.example",
	}, nil)
	assert.Equal(t, http.StatusForbidden, preflightDeny.StatusCode)
	assert.Empty(t, preflightDeny.Header.Get("Access-Control-Allow-Origin"))

	getDeny, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/t1/metadata", map[string]string{
		"Origin": "https://evil.example",
	}, nil)
	assert.Equal(t, http.StatusOK, getDeny.StatusCode)
	assert.Empty(t, getDeny.Header.Get("Access-Control-Allow-Origin"))
}

func TestSecurityHardening_TrustDocMultiIssuerIsolationAndAudit(t *testing.T) {
	userKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	workKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	userJWKS, _ := json.Marshal(auth.JWKSet{Keys: []auth.JWK{testRSAPublicJWK("u1", &userKey.PublicKey)}})
	workJWKS, _ := json.Marshal(auth.JWKSet{Keys: []auth.JWK{testRSAPublicJWK("w1", &workKey.PublicKey)}})

	val, err := auth.NewTrustDocValidator(auth.TrustDocConfig{
		Document: auth.TrustDocument{
			Issuers: []auth.TrustIssuer{
				{
					Issuer:            "https://zitadel.azra.example",
					Audiences:         []string{"azra-fhir"},
					JWKS:              userJWKS,
					SigningAlgorithms: []string{"RS256"},
					Claims: auth.TrustClaims{
						Shape:             auth.TrustClaimShapeGrantMap,
						GrantsClaim:       "urn:zitadel:iam:org:project:roles",
						ActiveTenantClaim: "active_tenant",
						OperatorRole:      "azra-operator",
						PlatformTenant:    "azra-platform",
						ActorClaim:        "act",
					},
				},
				{
					Issuer:            "https://workload.azra.internal",
					Audiences:         []string{"azra-fhir"},
					JWKS:              workJWKS,
					SigningAlgorithms: []string{"RS256"},
					Claims: auth.TrustClaims{
						Shape:      auth.TrustClaimShapeNone,
						ActorClaim: "act",
					},
				},
			},
		},
		Principals: []auth.TrustPrincipal{
			{
				ID:               "fhir-gateway",
				Issuer:           "https://workload.azra.internal",
				Subject:          "spiffe://azra/gateway",
				Scopes:           "system/Patient.crus",
				AllowCrossTenant: true,
			},
		},
		RoleToScopes: map[string]string{
			"viewer":        "user/Patient.rs",
			"tenant-admin":  "system/*.*",
			"azra-operator": "system/Patient.crs",
		},
	})
	require.NoError(t, err)

	_, st, ts := newHardeningSQLiteServer(t, "", server.SecurityPolicy{
		RequireAuth:        true,
		RequireTenantClaim: true,
	}, val)

	now := time.Now()
	// Alice is viewer in hosp-a, tenant-admin ONLY in hosp-b
	aliceTok := signTestRS256(t, "u1", userKey, map[string]any{
		"iss": "https://zitadel.azra.example",
		"sub": "alice",
		"aud": "azra-fhir",
		"exp": now.Add(5 * time.Minute).Unix(),
		"urn:zitadel:iam:org:project:roles": map[string]any{
			"viewer":       map[string]any{"hosp-a": true},
			"tenant-admin": map[string]any{"hosp-b": true},
		},
	})

	// Allowed read in hosp-a
	r, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/Patient", map[string]string{
		"Authorization":                "Bearer " + aliceTok,
		server.DefaultTenantHeaderName: "hosp-a",
	}, nil)
	assert.Equal(t, http.StatusOK, r.StatusCode)

	// Forbidden write in hosp-a (tenant-admin in hosp-b MUST NOT escalate hosp-a!)
	r, _ = doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/Patient", map[string]string{
		"Authorization":                "Bearer " + aliceTok,
		server.DefaultTenantHeaderName: "hosp-a",
	}, map[string]any{"resourceType": "Patient"})
	assert.Equal(t, http.StatusForbidden, r.StatusCode)

	// Allowed write in hosp-b
	r, _ = doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/Patient", map[string]string{
		"Authorization":                "Bearer " + aliceTok,
		server.DefaultTenantHeaderName: "hosp-b",
	}, map[string]any{"resourceType": "Patient"})
	assert.Equal(t, http.StatusCreated, r.StatusCode)

	// Forbidden in ungranted hosp-c
	r, _ = doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/Patient", map[string]string{
		"Authorization":                "Bearer " + aliceTok,
		server.DefaultTenantHeaderName: "hosp-c",
	}, nil)
	assert.Equal(t, http.StatusForbidden, r.StatusCode)

	// Operator acts cross-tenant in hosp-c with X-Request-Id
	opTok := signTestRS256(t, "u1", userKey, map[string]any{
		"iss": "https://zitadel.azra.example",
		"sub": "operator-bob",
		"aud": "azra-fhir",
		"exp": now.Add(5 * time.Minute).Unix(),
		"urn:zitadel:iam:org:project:roles": map[string]any{
			"azra-operator": map[string]any{"azra-platform": true},
		},
	})
	r, _ = doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/Patient", map[string]string{
		"Authorization":                "Bearer " + opTok,
		server.DefaultTenantHeaderName: "hosp-c",
		"X-Request-Id":                 "req-op-99",
	}, nil)
	assert.Equal(t, http.StatusOK, r.StatusCode)

	auds, _, err := st.QueryAuditEvents(context.Background(), store.AuditQueryParams{
		TenantID:     "hosp-c",
		AgentSubject: "operator-bob",
	})
	require.NoError(t, err)
	require.NotEmpty(t, auds)
	assert.True(t, auds[0].OperatorCrossTenant)
	assert.Equal(t, "https://zitadel.azra.example", auds[0].AgentIssuer)
	assert.Equal(t, "req-op-99", auds[0].RequestID)

	// Delegated workload token carrying RFC 8693 act claim writes full provenance
	workTok := signTestRS256(t, "w1", workKey, map[string]any{
		"iss": "https://workload.azra.internal",
		"sub": "spiffe://azra/gateway",
		"aud": "azra-fhir",
		"exp": now.Add(5 * time.Minute).Unix(),
		"act": map[string]any{
			"sub": "dr-jane",
			"iss": "https://zitadel.azra.example",
		},
	})
	r, _ = doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/Patient", map[string]string{
		"Authorization":                "Bearer " + workTok,
		server.DefaultTenantHeaderName: "hosp-d",
		"X-Request-Id":                 "req-act-1",
	}, map[string]any{"resourceType": "Patient"})
	assert.Equal(t, http.StatusCreated, r.StatusCode)

	workAuds, _, err := st.QueryAuditEvents(context.Background(), store.AuditQueryParams{
		TenantID:     "hosp-d",
		AgentSubject: "spiffe://azra/gateway",
	})
	require.NoError(t, err)
	require.NotEmpty(t, workAuds)
	assert.Equal(t, "https://workload.azra.internal", workAuds[0].AgentIssuer)
	assert.Equal(t, "dr-jane", workAuds[0].ActorSubject)
	assert.Equal(t, "https://zitadel.azra.example", workAuds[0].ActorIssuer)
	assert.Equal(t, "req-act-1", workAuds[0].RequestID)
}

func TestSecurityHardening_TenantHeaderRoutingAndSlugResolver(t *testing.T) {
	uuidToSlug := map[string]string{
		"00000000-0000-0000-0000-0000000000aa": "hospital-alpha",
	}
	_, st, ts := newHardeningSQLiteServer(t, "", server.SecurityPolicy{
		RequireTenantHeader: true,
		TenantSlugResolver: func(_ context.Context, raw string) (string, error) {
			if slug, ok := uuidToSlug[raw]; ok {
				return slug, nil
			}
			return raw, nil
		},
	}, nil)

	// Operational healthz & FHIR metadata bypass RequireTenantHeader
	r, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	assert.Equal(t, http.StatusOK, r.StatusCode)
	r, _ = doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/metadata", map[string]string{
		server.DefaultTenantHeaderName: "00000000-0000-0000-0000-0000000000aa",
	}, nil)
	assert.Equal(t, http.StatusOK, r.StatusCode)

	// Missing required tenant header -> 400
	r, _ = doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/Patient", nil, nil)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode)

	// Conflicting path vs header tenant -> 400
	r, _ = doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/hospital-alpha/Patient", map[string]string{
		server.DefaultTenantHeaderName: "hospital-beta",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode)

	// Flat create preserves flat Location header and resolves UUID -> hospital-alpha
	r, createdBytes := doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/Patient", map[string]string{
		server.DefaultTenantHeaderName: "00000000-0000-0000-0000-0000000000aa",
	}, map[string]any{
		"resourceType": "Patient",
		"name":         []map[string]any{{"family": "Ada"}},
	})
	require.Equal(t, http.StatusCreated, r.StatusCode)
	loc := r.Header.Get("Location")
	assert.True(t, strings.HasPrefix(loc, "/fhir/r4/Patient/"), "expected flat Location header, got %q", loc)
	var createdPat map[string]any
	require.NoError(t, json.Unmarshal(createdBytes, &createdPat))
	patID := createdPat["id"].(string)

	// Stored under canonical resolved tenant hospital-alpha
	rec, err := st.ReadResourceWithMeta(context.Background(), "hospital-alpha", "Patient", patID)
	require.NoError(t, err)
	assert.NotNil(t, rec)

	// Flat search preserves flat Bundle.link URLs
	r, searchBytes := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/Patient?family=Ada", map[string]string{
		server.DefaultTenantHeaderName: "00000000-0000-0000-0000-0000000000aa",
	}, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	var searchBundle map[string]any
	require.NoError(t, json.Unmarshal(searchBytes, &searchBundle))
	assert.Equal(t, float64(1), searchBundle["total"])
	links := searchBundle["link"].([]any)
	selfLink := links[0].(map[string]any)["url"].(string)
	assert.Contains(t, selfLink, "/fhir/r4/Patient?family=Ada")
	assert.NotContains(t, selfLink, "/hospital-alpha/")
}

func TestSecurityHardening_IdempotencyMultiReplicaAndConflict(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cluster-idem.db")
	_, stA, tsA := newHardeningSQLiteServer(t, dbPath, server.SecurityPolicy{}, nil)
	_, _, tsB := newHardeningSQLiteServer(t, dbPath, server.SecurityPolicy{}, nil)

	patBody := map[string]any{
		"resourceType": "Patient",
		"name":         []map[string]any{{"family": "Idempotent"}},
	}

	// 1. POST Create on Node A with Idempotency-Key
	respA, bodyA := doHardeningReq(t, http.MethodPost, tsA.URL+"/fhir/r4/Patient", map[string]string{
		server.DefaultTenantHeaderName: "tenant-a",
		"Idempotency-Key":              "key-pat-1",
	}, patBody)
	require.Equal(t, http.StatusCreated, respA.StatusCode)
	assert.Empty(t, respA.Header.Get("X-Idempotency-Replayed"))
	locA := respA.Header.Get("Location")

	// 2. Replay identical request to Node B with X-Idempotency-Key
	respB, bodyB := doHardeningReq(t, http.MethodPost, tsB.URL+"/fhir/r4/Patient", map[string]string{
		server.DefaultTenantHeaderName: "tenant-a",
		"X-Idempotency-Key":            "key-pat-1",
	}, patBody)
	require.Equal(t, http.StatusCreated, respB.StatusCode)
	assert.Equal(t, "true", respB.Header.Get("X-Idempotency-Replayed"))
	assert.Equal(t, locA, respB.Header.Get("Location"))
	assert.Equal(t, bodyA, bodyB)

	// Confirm only 1 Patient was created
	sr, err := stA.Search(context.Background(), store.SearchParams{TenantID: "tenant-a", ResType: "Patient"})
	require.NoError(t, err)
	assert.Equal(t, 1, sr.Total)

	// 3. Reusing same key in tenant-a with different payload -> 409 Conflict
	respConflict, _ := doHardeningReq(t, http.MethodPost, tsB.URL+"/fhir/r4/Patient", map[string]string{
		server.DefaultTenantHeaderName: "tenant-a",
		"Idempotency-Key":              "key-pat-1",
	}, map[string]any{
		"resourceType": "Patient",
		"name":         []map[string]any{{"family": "Different"}},
	})
	assert.Equal(t, http.StatusConflict, respConflict.StatusCode)

	// 4. Reusing same key in tenant-b is isolated and succeeds
	respTenantB, _ := doHardeningReq(t, http.MethodPost, tsB.URL+"/fhir/r4/Patient", map[string]string{
		server.DefaultTenantHeaderName: "tenant-b",
		"Idempotency-Key":              "key-pat-1",
	}, map[string]any{
		"resourceType": "Patient",
		"name":         []map[string]any{{"family": "Different"}},
	})
	assert.Equal(t, http.StatusCreated, respTenantB.StatusCode)

	// 5. Flat POST /fhir/r4 transaction Bundle idempotency
	bundleBody := map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry": []map[string]any{{
			"fullUrl": "urn:uuid:11111111-1111-1111-1111-111111111111",
			"resource": map[string]any{
				"resourceType": "Patient",
				"name":         []map[string]any{{"family": "BundlePat"}},
			},
			"request": map[string]any{"method": "POST", "url": "Patient"},
		}},
	}
	b1, bBytes1 := doHardeningReq(t, http.MethodPost, tsA.URL+"/fhir/r4", map[string]string{
		server.DefaultTenantHeaderName: "tenant-a",
		"Idempotency-Key":              "key-tx-1",
	}, bundleBody)
	require.Equal(t, http.StatusOK, b1.StatusCode)

	b2, bBytes2 := doHardeningReq(t, http.MethodPost, tsB.URL+"/fhir/r4", map[string]string{
		server.DefaultTenantHeaderName: "tenant-a",
		"Idempotency-Key":              "key-tx-1",
	}, bundleBody)
	require.Equal(t, http.StatusOK, b2.StatusCode)
	assert.Equal(t, "true", b2.Header.Get("X-Idempotency-Replayed"))
	assert.Equal(t, bBytes1, bBytes2)
}

func TestSecurityHardening_RefLockLocalRefsLimitsDeniedAndRateLimit(t *testing.T) {
	srv, _, ts := newHardeningSQLiteServer(t, "", server.SecurityPolicy{
		LockReferentialIntegrity:   true,
		EnforceLocalReferencesOnly: true,
		MaxBundleBytes:             4096,
		MaxBundleEntries:           2,
		DeniedResourceTypes:        []string{"MedicationKnowledge"},
		RateLimitPerSecond:         2,
		RateLimitBurst:             2,
	}, nil)
	srv.SetReferentialIntegrity(server.ReferentialIntegrityStrict)

	// Denied resource type returns 403 directly and inside Bundle (use tenant-denied so rate limiter bucket stays separate)
	r, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/tenant-denied/MedicationKnowledge", nil, nil)
	assert.Equal(t, http.StatusForbidden, r.StatusCode)

	r, _ = doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/tenant-denied", nil, map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry": []map[string]any{{
			"resource": map[string]any{"resourceType": "MedicationKnowledge", "status": "active"},
			"request":  map[string]any{"method": "POST", "url": "MedicationKnowledge"},
		}},
	})
	assert.Equal(t, http.StatusForbidden, r.StatusCode)

	// LockReferentialIntegrity ignores X-Flint-Referential-Integrity: lenient override
	r, _ = doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/tenant-ref/Observation", map[string]string{
		"X-Flint-Referential-Integrity": "lenient",
	}, map[string]any{
		"resourceType": "Observation",
		"status":       "final",
		"code":         map[string]any{"text": "HR"},
		"subject":      map[string]any{"reference": "Patient/missing-pat"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, r.StatusCode)

	// EnforceLocalReferencesOnly rejects external https:// and unresolved urn: references -> 422
	r, _ = doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/tenant-ref2/Observation", nil, map[string]any{
		"resourceType": "Observation",
		"status":       "final",
		"code":         map[string]any{"text": "HR"},
		"subject":      map[string]any{"reference": "https://external.attacker.example/fhir/Patient/1"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, r.StatusCode)

	// Intra-bundle urn:uuid: resolution rewrites before local-ref check so transaction Bundle succeeds!
	r, _ = doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/tenant-tx", nil, map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry": []map[string]any{
			{
				"fullUrl":  "urn:uuid:22222222-2222-2222-2222-222222222222",
				"resource": map[string]any{"resourceType": "Patient", "name": []map[string]any{{"family": "Tx"}}},
				"request":  map[string]any{"method": "POST", "url": "Patient"},
			},
			{
				"resource": map[string]any{
					"resourceType": "Observation",
					"status":       "final",
					"code":         map[string]any{"text": "HR"},
					"subject":      map[string]any{"reference": "urn:uuid:22222222-2222-2222-2222-222222222222"},
				},
				"request": map[string]any{"method": "POST", "url": "Observation"},
			},
		},
	})
	assert.Equal(t, http.StatusOK, r.StatusCode)

	// MaxBundleEntries (> 2 entries) -> 413 Request Entity Too Large
	r, _ = doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/tenant-limits", nil, map[string]any{
		"resourceType": "Bundle",
		"type":         "batch",
		"entry": []map[string]any{
			{"request": map[string]any{"method": "GET", "url": "Patient"}},
			{"request": map[string]any{"method": "GET", "url": "Patient"}},
			{"request": map[string]any{"method": "GET", "url": "Patient"}},
		},
	})
	assert.Equal(t, http.StatusRequestEntityTooLarge, r.StatusCode)

	// MaxBundleBytes (> 4096 bytes) -> 413 Request Entity Too Large
	hugeComment := strings.Repeat("x", 5000)
	r, _ = doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/tenant-big", nil, map[string]any{
		"resourceType": "Bundle",
		"type":         "batch",
		"entry": []map[string]any{{
			"resource": map[string]any{"resourceType": "Patient", "id": "p-big", "text": map[string]any{"status": "generated", "div": "<div>" + hugeComment + "</div>"}},
			"request":  map[string]any{"method": "PUT", "url": "Patient/p-big"},
		}},
	})
	assert.Equal(t, http.StatusRequestEntityTooLarge, r.StatusCode)

	// Per-tenant RateLimitPerSecond=2, Burst=2: 3rd request in tenant-rl -> 429; tenant-other still 200
	r1, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/tenant-rl/Patient", nil, nil)
	r2, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/tenant-rl/Patient", nil, nil)
	r3, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/tenant-rl/Patient", nil, nil)
	rOther, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/tenant-other/Patient", nil, nil)
	assert.Equal(t, http.StatusOK, r1.StatusCode)
	assert.Equal(t, http.StatusOK, r2.StatusCode)
	assert.Equal(t, http.StatusTooManyRequests, r3.StatusCode)
	assert.NotEmpty(t, r3.Header.Get("Retry-After"))
	assert.Equal(t, http.StatusOK, rOther.StatusCode)
}

func TestSecurityHardening_OpenBaoWorkloadSubjectClaimAndTenantHeader(t *testing.T) {
	workKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tlsJWKS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(auth.JWKSet{Keys: []auth.JWK{testRSAPublicJWK("bao-1", &workKey.PublicKey)}})
	}))
	defer tlsJWKS.Close()

	caPath := filepath.Join(t.TempDir(), "openbao-ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsJWKS.Certificate().Raw})
	require.NoError(t, os.WriteFile(caPath, caPEM, 0o600))

	openbaoIssuer := "https://openbao.openbao-system:8200/v1/identity/oidc"
	val, err := auth.NewTrustDocValidator(auth.TrustDocConfig{
		Document: auth.TrustDocument{
			Issuers: []auth.TrustIssuer{{
				Issuer:            openbaoIssuer,
				Audiences:         []string{"azra-fhir"},
				JWKSURL:           tlsJWKS.URL,
				CACertFile:        caPath,
				SigningAlgorithms: []string{"RS256"},
				Claims: auth.TrustClaims{
					Shape:        auth.TrustClaimShapeNone,
					SubjectClaim: "workload",
				},
				Principals: []auth.TrustPrincipal{{
					ID:               "fhir-ingest-worker",
					Subject:          "fhir-ingest-worker",
					Scopes:           "system/*.*",
					AllowCrossTenant: true,
				}},
			}},
		},
	})
	require.NoError(t, err)

	_, st, ts := newHardeningSQLiteServer(t, "", server.SecurityPolicy{
		RequireAuth:                     true,
		RequireTenantClaim:              true,
		RequireTenantHeader:             true,
		TenantHeaderName:                server.DefaultTenantHeaderName,
		DisableUnauthenticatedEndpoints: true,
	}, val)

	now := time.Now()
	entityUUID := "a1b2c3d4-0000-4000-8000-000000000001"
	workerTok := signTestRS256(t, "bao-1", workKey, map[string]any{
		"iss":         openbaoIssuer,
		"sub":         entityUUID,
		"workload":    "fhir-ingest-worker",
		"permissions": []string{"automation:write"},
		"aud":         "azra-fhir",
		"exp":         now.Add(5 * time.Minute).Unix(),
	})

	// Missing X-Azra-Tenant-Id is rejected even with valid cross-tenant workload token
	rNoTenant, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/Patient", map[string]string{
		"Authorization": "Bearer " + workerTok,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, rNoTenant.StatusCode)

	// Unlisted workload caller is rejected with 401
	rogueTok := signTestRS256(t, "bao-1", workKey, map[string]any{
		"iss":      openbaoIssuer,
		"sub":      entityUUID,
		"workload": "rogue-worker",
		"aud":      "azra-fhir",
		"exp":      now.Add(5 * time.Minute).Unix(),
	})
	rRogue, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/Patient", map[string]string{
		"Authorization":                "Bearer " + rogueTok,
		server.DefaultTenantHeaderName: "acme-clinic",
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, rRogue.StatusCode)

	// Authorized worker creates and reads a Patient in X-Azra-Tenant-Id: acme-clinic
	rCreate, createdBody := doHardeningReq(t, http.MethodPost, ts.URL+"/fhir/r4/Patient", map[string]string{
		"Authorization":                "Bearer " + workerTok,
		server.DefaultTenantHeaderName: "acme-clinic",
	}, map[string]any{
		"resourceType": "Patient",
		"name":         []map[string]any{{"family": "OpenBaoWorker"}},
	})
	require.Equal(t, http.StatusCreated, rCreate.StatusCode)

	var createdPat map[string]any
	require.NoError(t, json.Unmarshal(createdBody, &createdPat))
	patID, _ := createdPat["id"].(string)
	require.NotEmpty(t, patID)

	rGet, _ := doHardeningReq(t, http.MethodGet, ts.URL+"/fhir/r4/Patient/"+patID, map[string]string{
		"Authorization":                "Bearer " + workerTok,
		server.DefaultTenantHeaderName: "acme-clinic",
	}, nil)
	require.Equal(t, http.StatusOK, rGet.StatusCode)

	auds, _, err := st.QueryAuditEvents(context.Background(), store.AuditQueryParams{
		TenantID:     "acme-clinic",
		AgentSubject: entityUUID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, auds)
	assert.True(t, auds[0].OperatorCrossTenant)
	assert.Equal(t, openbaoIssuer, auds[0].AgentIssuer)
}
