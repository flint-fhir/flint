package server_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/certify"
	"github.com/flint-fhir/flint/pkg/shlink"
	"github.com/flint-fhir/flint/pkg/telemetry"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

type clusterRig struct {
	nodeA *server.Server
	nodeB *server.Server
	tsA   *httptest.Server
	tsB   *httptest.Server
	lb    *httptest.Server
	hitsA atomic.Uint64
	hitsB atomic.Uint64
}

func newClusterRig(t *testing.T, storeA, storeB store.Store) *clusterRig {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	nodeA := server.New(storeA, logger)
	server.RegisterDefaultResourceTypes(nodeA)
	t.Cleanup(func() { _ = nodeA.Close() })

	nodeB := server.New(storeB, logger)
	server.RegisterDefaultResourceTypes(nodeB)
	t.Cleanup(func() { _ = nodeB.Close() })

	tsA := httptest.NewServer(nodeA.Handler())
	t.Cleanup(tsA.Close)
	tsB := httptest.NewServer(nodeB.Handler())
	t.Cleanup(tsB.Close)

	urlA, err := url.Parse(tsA.URL)
	require.NoError(t, err)
	urlB, err := url.Parse(tsB.URL)
	require.NoError(t, err)

	proxyA := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(urlA)
			pr.Out.Host = pr.In.Host
		},
	}
	proxyB := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(urlB)
			pr.Out.Host = pr.In.Host
		},
	}

	rig := &clusterRig{
		nodeA: nodeA,
		nodeB: nodeB,
		tsA:   tsA,
		tsB:   tsB,
	}
	var seq atomic.Uint64
	rig.lb = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seq.Add(1)%2 == 1 {
			rig.hitsA.Add(1)
			w.Header().Set("X-Cluster-Node", "node-a")
			proxyA.ServeHTTP(w, r)
			return
		}
		rig.hitsB.Add(1)
		w.Header().Set("X-Cluster-Node", "node-b")
		proxyB.ServeHTTP(w, r)
	}))
	t.Cleanup(rig.lb.Close)
	return rig
}

func newSQLiteClusterStores(t *testing.T) (store.Store, store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cluster-wal.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	require.NoError(t, sqlite.RunMigrations(t.Context(), db, sqlite.JournalWAL))

	stA := sqlite.New(db, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(dbPath))
	stB := sqlite.New(db, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(dbPath))
	return stA, stB
}

func mintUDAPTestChain(t *testing.T, clientURI string) (caPEM []byte, leafPriv *rsa.PrivateKey, x5c []string) {
	t.Helper()
	caPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	now := time.Now().UTC()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1001),
		Subject:               pkix.Name{CommonName: "Cluster Test UDAP CA", Organization: []string{"Flint Test"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caPriv.PublicKey, caPriv)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leafPriv, err = rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	parsedURI, err := url.Parse(clientURI)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1002),
		Subject:               pkix.Name{CommonName: "Cluster UDAP B2B App", Organization: []string{"Flint Test"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{parsedURI},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafPriv.PublicKey, caPriv)
	require.NoError(t, err)
	x5c = []string{
		base64.StdEncoding.EncodeToString(leafDER),
		base64.StdEncoding.EncodeToString(caDER),
	}
	return caPEM, leafPriv, x5c
}

func runCrossReplicaContinuitySuite(t *testing.T, rig *clusterRig, tenant string) {
	t.Helper()
	baseA := rig.tsA.URL
	baseB := rig.tsB.URL

	// 1. Verify automatic RS256/ES256 JWKS and UDAP Root CA convergence between Node A and Node B
	respJWKSA, err := http.Get(baseA + "/.well-known/jwks.json")
	require.NoError(t, err)
	jwksABytes, _ := io.ReadAll(respJWKSA.Body)
	respJWKSA.Body.Close()

	respJWKSB, err := http.Get(baseB + "/.well-known/jwks.json")
	require.NoError(t, err)
	jwksBBytes, _ := io.ReadAll(respJWKSB.Body)
	respJWKSB.Body.Close()
	assert.JSONEq(t, string(jwksABytes), string(jwksBBytes), "Node A and Node B must expose identical signing JWKS")

	respCAA, err := http.Get(baseA + "/oauth2/udap-ca.pem")
	require.NoError(t, err)
	caABytes, _ := io.ReadAll(respCAA.Body)
	respCAA.Body.Close()

	respCAB, err := http.Get(baseB + "/oauth2/udap-ca.pem")
	require.NoError(t, err)
	caBBytes, _ := io.ReadAll(respCAB.Body)
	respCAB.Body.Close()
	assert.Equal(t, string(caABytes), string(caBBytes), "Node A and Node B must share identical UDAP Root CA PEM")

	// Seed a Patient on Node A and read it on Node B
	putPatReq, _ := http.NewRequest(http.MethodPut, baseA+"/fhir/r4/"+tenant+"/Patient/cl-pat-1",
		strings.NewReader(`{"resourceType":"Patient","id":"cl-pat-1","active":true,"gender":"female","birthDate":"1990-05-20","name":[{"use":"official","family":"Cluster","given":["Ada"]}]}`))
	putPatReq.Header.Set("Content-Type", "application/fhir+json")
	putPatResp, err := http.DefaultClient.Do(putPatReq)
	require.NoError(t, err)
	putPatResp.Body.Close()
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, putPatResp.StatusCode)

	// 2. Cross-node OAuth2 DCR (Node A) -> EHR Launch (Node B) -> Authorize (Node A) -> PKCE Token (Node B) -> Refresh Rotation (Node A) -> Replay Reject (Node B) -> Cascade Revoke (Node A -> Node B)
	rig.nodeA.EnableDevTokenEnforcement()
	rig.nodeB.EnableDevTokenEnforcement()
	defer func() {
		rig.nodeA.SetTokenValidator(nil)
		rig.nodeB.SetTokenValidator(nil)
	}()

	dcrBody := `{"client_name":"Cross Node EHR App","token_endpoint_auth_method":"client_secret_basic","grant_types":["authorization_code","refresh_token"],"redirect_uris":["https://ehr.cluster.example/cb"],"scope":"launch patient/Patient.rs patient/Patient.cruds user/*.cruds offline_access"}`
	respDCR, err := http.Post(baseA+"/oauth2/register", "application/json", strings.NewReader(dcrBody))
	require.NoError(t, err)
	var dcrDoc map[string]any
	require.NoError(t, json.NewDecoder(respDCR.Body).Decode(&dcrDoc))
	respDCR.Body.Close()
	require.Equal(t, http.StatusCreated, respDCR.StatusCode)
	cliID := dcrDoc["client_id"].(string)
	cliSec := dcrDoc["client_secret"].(string)
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(cliID+":"+cliSec))

	respLaunch, err := http.Post(baseB+"/oauth2/launch", "application/json",
		strings.NewReader(fmt.Sprintf(`{"patient":"cl-pat-1","tenant":%q}`, tenant)))
	require.NoError(t, err)
	var launchDoc map[string]any
	require.NoError(t, json.NewDecoder(respLaunch.Body).Decode(&launchDoc))
	respLaunch.Body.Close()
	require.Equal(t, http.StatusCreated, respLaunch.StatusCode)
	launchID := launchDoc["launch"].(string)

	verifier := "cross-replica-pkce-verifier-0123456789abcdef012345"
	vSum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(vSum[:])

	noRedir := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	authURL := fmt.Sprintf("%s/oauth2/authorize?response_type=code&client_id=%s&redirect_uri=%s&launch=%s&scope=%s&state=s1&code_challenge=%s&code_challenge_method=S256",
		baseA,
		url.QueryEscape(cliID),
		url.QueryEscape("https://ehr.cluster.example/cb"),
		url.QueryEscape(launchID),
		url.QueryEscape("launch patient/Patient.rs user/*.cruds offline_access"),
		url.QueryEscape(challenge),
	)
	respAuth, err := noRedir.Get(authURL)
	require.NoError(t, err)
	loc := respAuth.Header.Get("Location")
	respAuth.Body.Close()
	require.Equal(t, http.StatusFound, respAuth.StatusCode)
	parsedLoc, err := url.Parse(loc)
	require.NoError(t, err)
	authCode := parsedLoc.Query().Get("code")
	require.NotEmpty(t, authCode)

	// Exchange code on Node B
	tokForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {"https://ehr.cluster.example/cb"},
		"code_verifier": {verifier},
	}
	reqTokB, _ := http.NewRequest(http.MethodPost, baseB+"/oauth2/token", strings.NewReader(tokForm.Encode()))
	reqTokB.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqTokB.Header.Set("Authorization", basicAuth)
	respTokB, err := http.DefaultClient.Do(reqTokB)
	require.NoError(t, err)
	var tokDocB map[string]any
	require.NoError(t, json.NewDecoder(respTokB.Body).Decode(&tokDocB))
	respTokB.Body.Close()
	require.Equal(t, http.StatusOK, respTokB.StatusCode)
	assert.Equal(t, "cl-pat-1", tokDocB["patient"])
	rt1 := tokDocB["refresh_token"].(string)

	// Rotate refresh_token on Node A
	refForm := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt1},
	}
	reqRefA, _ := http.NewRequest(http.MethodPost, baseA+"/oauth2/token", strings.NewReader(refForm.Encode()))
	reqRefA.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqRefA.Header.Set("Authorization", basicAuth)
	respRefA, err := http.DefaultClient.Do(reqRefA)
	require.NoError(t, err)
	var refDocA map[string]any
	require.NoError(t, json.NewDecoder(respRefA.Body).Decode(&refDocA))
	respRefA.Body.Close()
	require.Equal(t, http.StatusOK, respRefA.StatusCode)
	at2 := refDocA["access_token"].(string)
	rt2 := refDocA["refresh_token"].(string)

	// Replay old refresh_token on Node B -> must fail 400 invalid_grant (zero RAM cache bypass)
	reqReplayB, _ := http.NewRequest(http.MethodPost, baseB+"/oauth2/token", strings.NewReader(refForm.Encode()))
	reqReplayB.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqReplayB.Header.Set("Authorization", basicAuth)
	respReplayB, err := http.DefaultClient.Do(reqReplayB)
	require.NoError(t, err)
	respReplayB.Body.Close()
	assert.Equal(t, http.StatusBadRequest, respReplayB.StatusCode)

	// Use rotated access_token (minted by Node A) against Node B
	reqReadB, _ := http.NewRequest(http.MethodGet, baseB+"/fhir/r4/"+tenant+"/Patient/cl-pat-1", nil)
	reqReadB.Header.Set("Authorization", "Bearer "+at2)
	respReadB, err := http.DefaultClient.Do(reqReadB)
	require.NoError(t, err)
	respReadB.Body.Close()
	assert.Equal(t, http.StatusOK, respReadB.StatusCode)

	// Keep a valid system token for MCP/SHLink tests before revoking rt2
	sysTokForm := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {"system/*.cruds"},
	}
	// Mint a patient/user token for MCP & SHLink before revoking grant
	mcpToken := at2

	// 3. Cross-node MCP Session: Initialize on Node A -> tools/call on Node B -> DELETE on Node A
	mcpInitBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"cluster-agent","version":"1.0"}}}`
	reqMCPInit, _ := http.NewRequest(http.MethodPost, baseA+"/fhir/r4/"+tenant+"/$mcp", strings.NewReader(mcpInitBody))
	reqMCPInit.Header.Set("Content-Type", "application/json")
	reqMCPInit.Header.Set("Authorization", "Bearer "+mcpToken)
	respMCPInit, err := http.DefaultClient.Do(reqMCPInit)
	require.NoError(t, err)
	mcpSessID := respMCPInit.Header.Get("Mcp-Session-Id")
	respMCPInit.Body.Close()
	require.Equal(t, http.StatusOK, respMCPInit.StatusCode)
	require.NotEmpty(t, mcpSessID)

	mcpCallBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fhir_read","arguments":{"resourceType":"Patient","id":"cl-pat-1"}}}`
	reqMCPCallB, _ := http.NewRequest(http.MethodPost, baseB+"/fhir/r4/"+tenant+"/$mcp", strings.NewReader(mcpCallBody))
	reqMCPCallB.Header.Set("Content-Type", "application/json")
	reqMCPCallB.Header.Set("Authorization", "Bearer "+mcpToken)
	reqMCPCallB.Header.Set("Mcp-Session-Id", mcpSessID)
	respMCPCallB, err := http.DefaultClient.Do(reqMCPCallB)
	require.NoError(t, err)
	mcpCallBytes, _ := io.ReadAll(respMCPCallB.Body)
	respMCPCallB.Body.Close()
	require.Equal(t, http.StatusOK, respMCPCallB.StatusCode)
	assert.Contains(t, string(mcpCallBytes), "Cluster")

	reqMCPDelA, _ := http.NewRequest(http.MethodDelete, baseA+"/fhir/r4/"+tenant+"/$mcp", nil)
	reqMCPDelA.Header.Set("Authorization", "Bearer "+mcpToken)
	reqMCPDelA.Header.Set("Mcp-Session-Id", mcpSessID)
	respMCPDelA, err := http.DefaultClient.Do(reqMCPDelA)
	require.NoError(t, err)
	respMCPDelA.Body.Close()
	assert.Contains(t, []int{http.StatusOK, http.StatusNoContent}, respMCPDelA.StatusCode)

	// 4. Cross-node SMART Health Link: Issue on Node A (maxViews=1, passcode) -> Resolve & ReadContent on Node B -> Exhausted on Node A -> Revoke on Node A -> 410 on Node B
	shlReqBody := `{"resourceType":"Parameters","parameter":[{"name":"passcode","valueString":"246810"},{"name":"maxViews","valueInteger":1},{"name":"label","valueString":"Cluster IPS"}]}`
	reqSHLIssueA, _ := http.NewRequest(http.MethodPost, baseA+"/fhir/r4/"+tenant+"/Patient/cl-pat-1/$shlink", strings.NewReader(shlReqBody))
	reqSHLIssueA.Header.Set("Content-Type", "application/fhir+json")
	reqSHLIssueA.Header.Set("Authorization", "Bearer "+mcpToken)
	respSHLIssueA, err := http.DefaultClient.Do(reqSHLIssueA)
	require.NoError(t, err)
	var shlParams map[string]any
	require.NoError(t, json.NewDecoder(respSHLIssueA.Body).Decode(&shlParams))
	respSHLIssueA.Body.Close()
	require.Equal(t, http.StatusOK, respSHLIssueA.StatusCode)

	var shlURI, shlID string
	for _, p := range shlParams["parameter"].([]any) {
		pm := p.(map[string]any)
		if pm["name"] == "shlink" {
			shlURI, _ = pm["valueUri"].(string)
		}
		if pm["name"] == "linkId" {
			shlID, _ = pm["valueString"].(string)
		}
	}
	require.NotEmpty(t, shlID)
	decSHL, err := shlink.DecodeSHLinkURI(shlURI)
	require.NoError(t, err)
	symKey, err := shlink.DecodeAES256Key(decSHL.Key)
	require.NoError(t, err)

	// Wrong passcode on Node B -> 401
	respBadPassB, err := http.Post(baseB+"/shlink/"+shlID+"/manifest", "application/json",
		strings.NewReader(`{"recipient":"Dr. NodeB","passcode":"000000"}`))
	require.NoError(t, err)
	respBadPassB.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, respBadPassB.StatusCode)

	// Correct passcode on Node B -> 200 (consumes 1/1 view)
	respManB, err := http.Post(baseB+"/shlink/"+shlID+"/manifest", "application/json",
		strings.NewReader(`{"recipient":"Dr. NodeB","passcode":"246810"}`))
	require.NoError(t, err)
	respManB.Body.Close()
	require.Equal(t, http.StatusOK, respManB.StatusCode)

	// Second manifest view on Node A -> 410 Gone (maxViews=1 enforced across cluster)
	respManExhaustedA, err := http.Post(baseA+"/shlink/"+shlID+"/manifest", "application/json",
		strings.NewReader(`{"recipient":"Dr. NodeA","passcode":"246810"}`))
	require.NoError(t, err)
	respManExhaustedA.Body.Close()
	assert.Equal(t, http.StatusGone, respManExhaustedA.StatusCode)

	// Fetch encrypted JWE item on Node B and decrypt
	respContentB, err := http.Get(baseB + "/shlink/" + shlID + "/content/item-0")
	require.NoError(t, err)
	jweBytes, _ := io.ReadAll(respContentB.Body)
	respContentB.Body.Close()
	require.Equal(t, http.StatusOK, respContentB.StatusCode)
	plainIPS, _, err := shlink.DecryptCompactJWE(symKey, string(jweBytes))
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(plainIPS), "verifiableCredential") || strings.Contains(string(plainIPS), "60591-5"),
		"expected decrypted SHLink payload to contain SMART Health Card or IPS Bundle, got %s", string(plainIPS))

	// Revoke on Node A -> content on Node B returns 410 Gone
	reqSHLRevA, _ := http.NewRequest(http.MethodPost, baseA+"/fhir/r4/"+tenant+"/Patient/cl-pat-1/$shlink-revoke",
		strings.NewReader(fmt.Sprintf(`{"linkId":%q}`, shlID)))
	reqSHLRevA.Header.Set("Content-Type", "application/json")
	reqSHLRevA.Header.Set("Authorization", "Bearer "+mcpToken)
	respSHLRevA, err := http.DefaultClient.Do(reqSHLRevA)
	require.NoError(t, err)
	respSHLRevA.Body.Close()
	require.Equal(t, http.StatusOK, respSHLRevA.StatusCode)

	respContentAfterRevB, err := http.Get(baseB + "/shlink/" + shlID + "/content/item-0")
	require.NoError(t, err)
	respContentAfterRevB.Body.Close()
	assert.Equal(t, http.StatusGone, respContentAfterRevB.StatusCode)

	// Now revoke rt2 on Node A -> at2 must immediately return 401 on Node B via grant_id cascade revocation!
	revForm := url.Values{"token": {rt2}}
	reqRevA, _ := http.NewRequest(http.MethodPost, baseA+"/oauth2/revoke", strings.NewReader(revForm.Encode()))
	reqRevA.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqRevA.Header.Set("Authorization", basicAuth)
	respRevA, err := http.DefaultClient.Do(reqRevA)
	require.NoError(t, err)
	respRevA.Body.Close()
	require.Equal(t, http.StatusOK, respRevA.StatusCode)

	reqAfterRevB, _ := http.NewRequest(http.MethodGet, baseB+"/fhir/r4/"+tenant+"/Patient/cl-pat-1", nil)
	reqAfterRevB.Header.Set("Authorization", "Bearer "+at2)
	respAfterRevB, err := http.DefaultClient.Do(reqAfterRevB)
	require.NoError(t, err)
	respAfterRevB.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, respAfterRevB.StatusCode)

	// Disable dev token enforcement for subsequent unauthenticated IG operations
	rig.nodeA.SetTokenValidator(nil)
	rig.nodeB.SetTokenValidator(nil)
	_ = sysTokForm

	// 5. Cross-node UDAP Trust Anchor (Node A) -> UDAP DCR (Node B) -> Replay JTI Reject (Node A) -> UDAP B2B Token (Node A) -> Replay JTI Reject (Node B)
	udapClientURI := "https://b2b.cluster-udap.example/app"
	caPEM, leafPriv, x5c := mintUDAPTestChain(t, udapClientURI)
	respAnchorA, err := http.Post(baseA+"/oauth2/udap-trust-anchor", "application/json",
		strings.NewReader(fmt.Sprintf(`{"pem":%q}`, string(caPEM))))
	require.NoError(t, err)
	respAnchorA.Body.Close()
	require.Equal(t, http.StatusCreated, respAnchorA.StatusCode)

	nowUnix := time.Now().UTC().Unix()
	ssJWT, err := auth.SignRS256WithX5C(map[string]any{
		"iss":                        udapClientURI,
		"sub":                        udapClientURI,
		"aud":                        baseB + "/oauth2/register",
		"iat":                        nowUnix,
		"exp":                        nowUnix + 180,
		"jti":                        "udap-dcr-jti-cluster-1",
		"client_name":                "UDAP Cluster B2B",
		"grant_types":                []string{"client_credentials"},
		"token_endpoint_auth_method": "private_key_jwt",
		"scope":                      "system/*.rs",
	}, leafPriv, x5c)
	require.NoError(t, err)

	udapRegPayload := fmt.Sprintf(`{"udap":"1","software_statement":%q}`, ssJWT)
	respUDAPRegB, err := http.Post(baseB+"/oauth2/register", "application/json", strings.NewReader(udapRegPayload))
	require.NoError(t, err)
	var udapRegDoc map[string]any
	require.NoError(t, json.NewDecoder(respUDAPRegB.Body).Decode(&udapRegDoc))
	respUDAPRegB.Body.Close()
	require.Equal(t, http.StatusCreated, respUDAPRegB.StatusCode)
	udapCliID := udapRegDoc["client_id"].(string)
	require.NotEmpty(t, udapCliID)

	// Replaying the same software_statement JTI on Node A (with matching aud) must fail; test JTI claim directly on B2B token across nodes
	b2bAssert, err := auth.SignRS256WithX5C(map[string]any{
		"iss": udapCliID,
		"sub": udapCliID,
		"aud": baseA + "/oauth2/token",
		"iat": nowUnix,
		"exp": nowUnix + 180,
		"jti": "udap-b2b-jti-cluster-1",
	}, leafPriv, x5c)
	require.NoError(t, err)
	udapTokForm := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {b2bAssert},
		"scope":                 {"system/*.rs"},
		"udap":                  {"1"},
	}
	respUDAPTokA, err := http.Post(baseA+"/oauth2/token", "application/x-www-form-urlencoded", strings.NewReader(udapTokForm.Encode()))
	require.NoError(t, err)
	respUDAPTokA.Body.Close()
	require.Equal(t, http.StatusOK, respUDAPTokA.StatusCode)

	// Replay identical JTI on Node B (signing with baseB aud but same jti) -> rejected 400!
	b2bReplayAssert, err := auth.SignRS256WithX5C(map[string]any{
		"iss": udapCliID,
		"sub": udapCliID,
		"aud": baseB + "/oauth2/token",
		"iat": nowUnix,
		"exp": nowUnix + 180,
		"jti": "udap-b2b-jti-cluster-1",
	}, leafPriv, x5c)
	require.NoError(t, err)
	udapReplayForm := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {b2bReplayAssert},
		"scope":                 {"system/*.rs"},
		"udap":                  {"1"},
	}
	respUDAPReplayB, err := http.Post(baseB+"/oauth2/token", "application/x-www-form-urlencoded", strings.NewReader(udapReplayForm.Encode()))
	require.NoError(t, err)
	respUDAPReplayB.Body.Close()
	assert.Equal(t, http.StatusBadRequest, respUDAPReplayB.StatusCode)

	// 6. Cross-node CDS Hooks 2.0 Client JWKS (registered on Node A) + JTI Replay Rejection across Node B -> Node A
	cdsEC, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	cdsIss := "https://ehr-cds.cluster.example"
	rig.nodeA.RegisterCDSClientJWKS(cdsIss, auth.JWKSet{
		Keys: []auth.JWK{ecPublicKeyToJWK(t, &cdsEC.PublicKey, "cds-k1", "ES256")},
	})
	rig.nodeA.SetCDSHooksRequireJWT(true)
	rig.nodeB.SetCDSHooksRequireJWT(true)
	defer func() {
		rig.nodeA.SetCDSHooksRequireJWT(false)
		rig.nodeB.SetCDSHooksRequireJWT(false)
	}()

	cdsJWSB := signCDSJWT(t,
		map[string]any{"alg": "ES256", "kid": "cds-k1", "typ": "JWT"},
		map[string]any{
			"iss": cdsIss,
			"aud": baseB + "/fhir/r4/" + tenant + "/cds-services",
			"iat": nowUnix,
			"exp": nowUnix + 120,
			"jti": "cds-cluster-jti-99",
		},
		cdsEC,
	)
	reqCDSB, _ := http.NewRequest(http.MethodGet, baseB+"/fhir/r4/"+tenant+"/cds-services", nil)
	reqCDSB.Header.Set("Authorization", "Bearer "+cdsJWSB)
	respCDSB, err := http.DefaultClient.Do(reqCDSB)
	require.NoError(t, err)
	respCDSB.Body.Close()
	require.Equal(t, http.StatusOK, respCDSB.StatusCode)

	// Replay same JTI ("cds-cluster-jti-99") against Node A -> rejected 401!
	cdsJWSA := signCDSJWT(t,
		map[string]any{"alg": "ES256", "kid": "cds-k1", "typ": "JWT"},
		map[string]any{
			"iss": cdsIss,
			"aud": baseA + "/fhir/r4/" + tenant + "/cds-services",
			"iat": nowUnix,
			"exp": nowUnix + 120,
			"jti": "cds-cluster-jti-99",
		},
		cdsEC,
	)
	reqCDSA, _ := http.NewRequest(http.MethodGet, baseA+"/fhir/r4/"+tenant+"/cds-services", nil)
	reqCDSA.Header.Set("Authorization", "Bearer "+cdsJWSA)
	respCDSA, err := http.DefaultClient.Do(reqCDSA)
	require.NoError(t, err)
	respCDSA.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, respCDSA.StatusCode)

	// 7. Cross-node Async Bulk $export (kickoff on Node A -> poll & download on Node B -> delete on Node A -> 404 on Node B)
	reqExpA, _ := http.NewRequest(http.MethodGet, baseA+"/fhir/r4/"+tenant+"/$export?_type=Patient", nil)
	reqExpA.Header.Set("Prefer", "respond-async")
	respExpA, err := http.DefaultClient.Do(reqExpA)
	require.NoError(t, err)
	contentLoc := respExpA.Header.Get("Content-Location")
	respExpA.Body.Close()
	require.Equal(t, http.StatusAccepted, respExpA.StatusCode)
	parsedPoll, err := url.Parse(contentLoc)
	require.NoError(t, err)

	var manifestDoc map[string]any
	require.Eventually(t, func() bool {
		rPoll, pErr := http.Get(baseB + parsedPoll.Path)
		if pErr != nil {
			return false
		}
		defer rPoll.Body.Close()
		if rPoll.StatusCode == http.StatusOK {
			_ = json.NewDecoder(rPoll.Body).Decode(&manifestDoc)
			return true
		}
		return false
	}, 5*time.Second, 25*time.Millisecond)

	outputs := manifestDoc["output"].([]any)
	require.NotEmpty(t, outputs)
	fileURLStr := outputs[0].(map[string]any)["url"].(string)
	parsedFile, err := url.Parse(fileURLStr)
	require.NoError(t, err)
	respFileB, err := http.Get(baseB + parsedFile.Path)
	require.NoError(t, err)
	ndjsonBytes, _ := io.ReadAll(respFileB.Body)
	respFileB.Body.Close()
	require.Equal(t, http.StatusOK, respFileB.StatusCode)
	assert.Contains(t, string(ndjsonBytes), "cl-pat-1")

	reqDelExpA, _ := http.NewRequest(http.MethodDelete, baseA+parsedPoll.Path, nil)
	respDelExpA, err := http.DefaultClient.Do(reqDelExpA)
	require.NoError(t, err)
	respDelExpA.Body.Close()

	respPollAfterDelB, err := http.Get(baseB + parsedPoll.Path)
	require.NoError(t, err)
	respPollAfterDelB.Body.Close()
	assert.Equal(t, http.StatusNotFound, respPollAfterDelB.StatusCode)

	// 8. Cross-node Async Bulk $import (kickoff on Node A -> poll on Node B)
	reqImpA, _ := http.NewRequest(http.MethodPost, baseA+"/fhir/r4/"+tenant+"/$import",
		strings.NewReader(`{"resourceType":"Patient","id":"cl-imp-pat-1","name":[{"family":"AsyncImported"}]}`+"\n"))
	reqImpA.Header.Set("Content-Type", "application/fhir+ndjson")
	reqImpA.Header.Set("Prefer", "respond-async")
	respImpA, err := http.DefaultClient.Do(reqImpA)
	require.NoError(t, err)
	impLoc := respImpA.Header.Get("Content-Location")
	respImpA.Body.Close()
	require.Equal(t, http.StatusAccepted, respImpA.StatusCode)
	parsedImpLoc, err := url.Parse(impLoc)
	require.NoError(t, err)

	respImpPollB, err := http.Get(baseB + parsedImpLoc.Path)
	require.NoError(t, err)
	impPollBytes, _ := io.ReadAll(respImpPollB.Body)
	respImpPollB.Body.Close()
	require.Equal(t, http.StatusOK, respImpPollB.StatusCode)
	assert.Contains(t, string(impPollBytes), `"importedCount"`)

	// 9. Cross-node WebSocket Subscription/$bind-ws (mint wsbt-... on Node A -> bind on Node B -> replay rejected on Node A)
	subJSON := `{"resourceType":"Subscription","id":"cl-ws-sub-1","status":"requested","reason":"Cluster WS","criteria":"Observation?patient=Patient/cl-pat-1","channel":{"type":"websocket","payload":"application/fhir+json"}}`
	reqSubA, _ := http.NewRequest(http.MethodPut, baseA+"/fhir/r4/"+tenant+"/Subscription/cl-ws-sub-1", strings.NewReader(subJSON))
	reqSubA.Header.Set("Content-Type", "application/fhir+json")
	respSubA, err := http.DefaultClient.Do(reqSubA)
	require.NoError(t, err)
	respSubA.Body.Close()
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, respSubA.StatusCode)

	respBindA, err := http.Get(baseA + "/fhir/r4/" + tenant + "/Subscription/cl-ws-sub-1/$bind-ws")
	require.NoError(t, err)
	var bindParams map[string]any
	require.NoError(t, json.NewDecoder(respBindA.Body).Decode(&bindParams))
	respBindA.Body.Close()
	require.Equal(t, http.StatusOK, respBindA.StatusCode)
	var wsToken string
	for _, p := range bindParams["parameter"].([]any) {
		pm := p.(map[string]any)
		if pm["name"] == "token" {
			wsToken, _ = pm["valueString"].(string)
		}
	}
	require.True(t, strings.HasPrefix(wsToken, "wsbt-"))

	wsCliB, statusWSB, _ := dialTestWS(t, baseB, "/fhir/r4/"+tenant+"/$subscriptions-ws", nil)
	require.Equal(t, http.StatusSwitchingProtocols, statusWSB)
	wsCliB.sendText("bind-with-token: " + wsToken)
	hsBundleB := wsCliB.readJSONBundle(3 * time.Second)
	assert.Equal(t, "history", hsBundleB["type"])

	// Replaying consumed wsbt-... token on Node A must fail
	wsCliA, statusWSA, _ := dialTestWS(t, baseA, "/fhir/r4/"+tenant+"/$subscriptions-ws", nil)
	require.Equal(t, http.StatusSwitchingProtocols, statusWSA)
	wsCliA.sendText("bind-with-token: " + wsToken)
	_, errFrameA, err := wsCliA.readFrame(3 * time.Second)
	require.NoError(t, err)
	assert.Contains(t, strings.ToLower(string(errFrameA)), "invalid or unknown websocket binding token")

	// 10. Cross-node SQL-on-FHIR v2 ViewDefinition CRUD (PUT on Node A -> $run on Node B -> DELETE on Node A -> 404 on Node B)
	vdBody := `{"resourceType":"ViewDefinition","id":"cl-vd-pat","name":"cl_vd_pat","status":"active","resource":"Patient","select":[{"column":[{"name":"id","path":"id"},{"name":"family","path":"name.family.first()"}]}]}`
	reqVDA, _ := http.NewRequest(http.MethodPut, baseA+"/fhir/r4/"+tenant+"/ViewDefinition/cl-vd-pat", strings.NewReader(vdBody))
	reqVDA.Header.Set("Content-Type", "application/fhir+json")
	respVDA, err := http.DefaultClient.Do(reqVDA)
	require.NoError(t, err)
	respVDA.Body.Close()
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, respVDA.StatusCode)

	respVDRunB, err := http.Get(baseB + "/fhir/r4/" + tenant + "/ViewDefinition/cl-vd-pat/$run")
	require.NoError(t, err)
	vdRunBytes, _ := io.ReadAll(respVDRunB.Body)
	respVDRunB.Body.Close()
	require.Equal(t, http.StatusOK, respVDRunB.StatusCode)
	assert.Contains(t, string(vdRunBytes), "Cluster")

	reqVDDelA, _ := http.NewRequest(http.MethodDelete, baseA+"/fhir/r4/"+tenant+"/ViewDefinition/cl-vd-pat", nil)
	respVDDelA, err := http.DefaultClient.Do(reqVDDelA)
	require.NoError(t, err)
	respVDDelA.Body.Close()
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, respVDDelA.StatusCode)

	respVDGoneB, err := http.Get(baseB + "/fhir/r4/" + tenant + "/ViewDefinition/cl-vd-pat")
	require.NoError(t, err)
	respVDGoneB.Body.Close()
	assert.Equal(t, http.StatusNotFound, respVDGoneB.StatusCode)
}

func TestCluster_StatelessCrossReplicaContinuity_SQLiteAndPostgres(t *testing.T) {
	t.Run("SQLiteWAL", func(t *testing.T) {
		stA, stB := newSQLiteClusterStores(t)
		rig := newClusterRig(t, stA, stB)
		runCrossReplicaContinuitySuite(t, rig, "cluster-sqlite")
	})

	t.Run("PostgreSQL", func(t *testing.T) {
		db := openTestPostgres(t)
		defer db.Close()
		require.NoError(t, postgres.RunMigrations(t.Context(), db))
		tenant := "cluster-postgres"
		_ = postgres.PurgeTenant(t.Context(), db, tenant)
		defer func() { _ = postgres.PurgeTenant(t.Context(), db, tenant) }()

		rig := newClusterRig(t, postgres.New(db), postgres.New(db))
		runCrossReplicaContinuitySuite(t, rig, tenant)
	})
}

func TestCluster_RoundRobinLoadBalancerFull144CheckCertify(t *testing.T) {
	t.Run("SQLiteWAL_RoundRobin144Checks", func(t *testing.T) {
		stA, stB := newSQLiteClusterStores(t)
		rig := newClusterRig(t, stA, stB)

		rep, err := certify.Run(t.Context(), certify.Config{
			BaseURL:    rig.lb.URL,
			Tenant:     "certify-rr-sqlite",
			HTTPClient: rig.lb.Client(),
		})
		require.NoError(t, err)
		require.Equal(t, "pass", rep.Result, "scorecard:\n%s", rep.FormatScorecard())
		assert.Equal(t, 144, rep.TotalChecks)
		assert.Equal(t, 0, rep.Failed)
		assert.Equal(t, 100.0, rep.Score)
		assert.Greater(t, rig.hitsA.Load(), uint64(100), "Node A must serve alternating load-balanced requests")
		assert.Greater(t, rig.hitsB.Load(), uint64(100), "Node B must serve alternating load-balanced requests")
	})

	t.Run("PostgreSQL_RoundRobin144Checks", func(t *testing.T) {
		db := openTestPostgres(t)
		defer db.Close()
		require.NoError(t, postgres.RunMigrations(t.Context(), db))
		tenant := "certify-rr-pg"
		_ = postgres.PurgeTenant(t.Context(), db, tenant)
		defer func() { _ = postgres.PurgeTenant(t.Context(), db, tenant) }()

		rig := newClusterRig(t, postgres.New(db), postgres.New(db))
		rep, err := certify.Run(t.Context(), certify.Config{
			BaseURL:    rig.lb.URL,
			Tenant:     tenant,
			HTTPClient: rig.lb.Client(),
		})
		require.NoError(t, err)
		require.Equal(t, "pass", rep.Result, "scorecard:\n%s", rep.FormatScorecard())
		assert.Equal(t, 144, rep.TotalChecks)
		assert.Equal(t, 0, rep.Failed)
		assert.Equal(t, 100.0, rep.Score)
		assert.Greater(t, rig.hitsA.Load(), uint64(100))
		assert.Greater(t, rig.hitsB.Load(), uint64(100))
	})
}

func TestServer_OpenTelemetryEndToEndTracingSlogAndAuditCorrelation(t *testing.T) {
	var (
		colMu        sync.Mutex
		otlpPayloads [][]byte
		spans        []telemetry.SpanData
	)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		colMu.Lock()
		otlpPayloads = append(otlpPayloads, raw)
		colMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	var logBuf bytes.Buffer
	var logMu sync.Mutex
	syncWriter := writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logBuf.Write(p)
	})
	logger := slog.New(telemetry.WrapSlogHandler(slog.NewJSONHandler(syncWriter, &slog.HandlerOptions{Level: slog.LevelInfo})))

	primaryDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer primaryDB.Close()
	primaryDB.SetMaxOpenConns(1)
	require.NoError(t, sqlite.RunMigrations(t.Context(), primaryDB, sqlite.JournalMemory))
	primaryStore := sqlite.New(primaryDB, sqlite.WithJournalMode(sqlite.JournalMemory), sqlite.WithDSN(":memory:"))

	replicaDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer replicaDB.Close()
	replicaDB.SetMaxOpenConns(1)
	require.NoError(t, sqlite.RunMigrations(t.Context(), replicaDB, sqlite.JournalMemory))
	replicaStore := sqlite.New(replicaDB, sqlite.WithJournalMode(sqlite.JournalMemory), sqlite.WithDSN(":memory:"))

	srv := server.New(primaryStore, logger)
	defer func() { _ = srv.Close() }()
	srv.SetAnalyticalStore(replicaStore)
	server.RegisterDefaultResourceTypes(srv)

	tr, err := telemetry.NewTracer(telemetry.Config{
		Endpoint:       collector.URL,
		Protocol:       "http/protobuf",
		ServiceName:    "flint-otel-e2e",
		ServiceVersion: "0.7.0",
		BatchSize:      2,
		FlushInterval:  20 * time.Millisecond,
		Exporter: func(_ context.Context, batch []telemetry.SpanData) error {
			colMu.Lock()
			spans = append(spans, batch...)
			colMu.Unlock()
			return nil
		},
	})
	require.NoError(t, err)
	srv.SetTracer(tr)
	require.Equal(t, tr, srv.Tracer())

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wantTraceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	wantParentSpanID := "00f067aa0ba902b7"
	reqPut, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/otel-demo/Patient/otel-pat-1",
		strings.NewReader(`{"resourceType":"Patient","id":"otel-pat-1","active":true,"name":[{"family":"Telemetry"}]}`))
	reqPut.Header.Set("Content-Type", "application/fhir+json")
	reqPut.Header.Set("Traceparent", "00-"+wantTraceID+"-"+wantParentSpanID+"-01")
	reqPut.Header.Set("Tracestate", "ehr=epic")

	respPut, err := http.DefaultClient.Do(reqPut)
	require.NoError(t, err)
	respPut.Body.Close()
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, respPut.StatusCode)
	assert.Equal(t, wantTraceID, respPut.Header.Get("X-Trace-Id"))

	// Emit a trace-correlated slog entry inside an active span context to verify end-to-end log enrichment
	sc, ok := telemetry.ParseTraceParent("00-" + wantTraceID + "-" + wantParentSpanID + "-01")
	require.True(t, ok)
	logger.InfoContext(telemetry.ContextWithSpanContext(t.Context(), sc), "completed fhir request")

	// Verify AuditEvent contains trace-context extension AND tamper-evident hash chain remains valid
	respAud, err := http.Get(ts.URL + "/fhir/r4/otel-demo/AuditEvent?entity=Patient/otel-pat-1")
	require.NoError(t, err)
	audBytes, _ := io.ReadAll(respAud.Body)
	respAud.Body.Close()
	require.Equal(t, http.StatusOK, respAud.StatusCode)
	assert.Contains(t, string(audBytes), "http://flint-fhir.org/fhir/StructureDefinition/trace-context")
	assert.Contains(t, string(audBytes), wantTraceID)

	respIntegrity, err := http.Get(ts.URL + "/fhir/r4/otel-demo/AuditEvent/$verify-integrity")
	require.NoError(t, err)
	intBytes, _ := io.ReadAll(respIntegrity.Body)
	respIntegrity.Body.Close()
	require.Equal(t, http.StatusOK, respIntegrity.StatusCode)
	assert.Contains(t, string(intBytes), `"valueBoolean":true`)

	// Flush tracer and verify parent->child span hierarchy + OTLP collector payload + Prometheus metrics
	require.NoError(t, tr.Shutdown(t.Context()))

	colMu.Lock()
	gotPayloads := len(otlpPayloads)
	gotSpans := append([]telemetry.SpanData(nil), spans...)
	colMu.Unlock()
	assert.GreaterOrEqual(t, gotPayloads, 1, "expected OTLP/HTTP collector to receive protobuf batches")

	var serverSpan *telemetry.SpanData
	var storeSpan *telemetry.SpanData
	for i := range gotSpans {
		s := &gotSpans[i]
		if hex.EncodeToString(s.TraceID[:]) != wantTraceID {
			continue
		}
		if s.Kind == telemetry.SpanKindServer && strings.Contains(s.Name, "PUT /fhir/r4/{tenant}/Patient/{id}") {
			serverSpan = s
		}
		if s.Name == "store.WriteResourceWithMeta" || s.Name == "store.WriteResource" {
			storeSpan = s
		}
	}
	require.NotNil(t, serverSpan, "expected SpanKindServer span for PUT Patient")
	assert.Equal(t, wantParentSpanID, hex.EncodeToString(serverSpan.ParentSpanID[:]))
	require.NotNil(t, storeSpan, "expected child store.WriteResource span")
	assert.Equal(t, serverSpan.SpanID, storeSpan.ParentSpanID, "store.WriteResource must be a child of the HTTP server span")

	logMu.Lock()
	logsStr := logBuf.String()
	logMu.Unlock()
	assert.Contains(t, logsStr, `"trace_id":"`+wantTraceID+`"`)

	respMetrics, err := http.Get(ts.URL + "/metrics")
	require.NoError(t, err)
	metricsBytes, _ := io.ReadAll(respMetrics.Body)
	respMetrics.Body.Close()
	metricsStr := string(metricsBytes)
	assert.Contains(t, metricsStr, "flint_otlp_spans_exported_total ")
	assert.NotContains(t, metricsStr, "flint_otlp_spans_exported_total 0\n")
	assert.Contains(t, metricsStr, "flint_hybrid_replicated_writes_total ")
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
