package server_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/sqlite"
	"github.com/google/uuid"
)

func newSMARTSTU2TestServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	st := sqlite.New(nil, sqlite.WithDSN(":memory:"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seedResource := func(path, body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build seed request %s: %v", path, err)
		}
		req.Header.Set("Content-Type", "application/fhir+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("seed %s HTTP %d: %s", path, resp.StatusCode, string(raw))
		}
	}

	seedResource("/fhir/r4/demo/Patient/pat-alice-001", `{
		"resourceType": "Patient",
		"id": "pat-alice-001",
		"active": true,
		"gender": "female",
		"name": [{"family": "Wonderland", "given": ["Alice"]}]
	}`)
	seedResource("/fhir/r4/demo/Encounter/enc-alice-001", `{
		"resourceType": "Encounter",
		"id": "enc-alice-001",
		"status": "finished",
		"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
		"subject": {"reference": "Patient/pat-alice-001"}
	}`)
	seedResource("/fhir/r4/demo/Practitioner/prac-cardiology-1", `{
		"resourceType": "Practitioner",
		"id": "prac-cardiology-1",
		"active": true,
		"name": [{"family": "Vance", "given": ["Eleanor"]}]
	}`)

	srv.EnableDevTokenEnforcement()
	return srv, ts
}

func pkceS256Pair() (verifier, challenge string) {
	verifier = "smart-stu2-verifier-" + uuid.NewString() + "-entropy-padding-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func signES384ClientAssertion(t *testing.T, priv *ecdsa.PrivateKey, kid, clientID, aud string) string {
	t.Helper()
	hdrJSON, err := json.Marshal(map[string]string{
		"alg": "ES384",
		"kid": kid,
		"typ": "JWT",
	})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	now := time.Now().Unix()
	payJSON, err := json.Marshal(map[string]any{
		"iss": clientID,
		"sub": clientID,
		"aud": aud,
		"iat": now,
		"exp": now + 180,
		"jti": uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hdrJSON) + "." + base64.RawURLEncoding.EncodeToString(payJSON)
	digest := sha512.Sum384([]byte(signingInput))
	rInt, sInt, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("ecdsa sign: %v", err)
	}
	const coordLen = 48
	rawSig := make([]byte, coordLen*2)
	rBytes := rInt.Bytes()
	sBytes := sInt.Bytes()
	copy(rawSig[coordLen-len(rBytes):coordLen], rBytes)
	copy(rawSig[coordLen*2-len(sBytes):], sBytes)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(rawSig)
}

func TestSMARTSTU2_StandaloneLaunch_OIDC_Refresh_And_Introspection(t *testing.T) {
	_, ts := newSMARTSTU2TestServer(t)
	fhirBase := ts.URL + "/fhir/r4/demo"

	// 1. Verify SMART discovery and OIDC discovery match tenant-qualified issuer.
	smartResp, err := http.Get(fhirBase + "/.well-known/smart-configuration")
	if err != nil {
		t.Fatalf("GET smart-configuration: %v", err)
	}
	defer smartResp.Body.Close()
	if smartResp.StatusCode != http.StatusOK {
		t.Fatalf("smart-configuration status=%d", smartResp.StatusCode)
	}
	var smartDoc map[string]any
	if err := json.NewDecoder(smartResp.Body).Decode(&smartDoc); err != nil {
		t.Fatalf("decode smart-configuration: %v", err)
	}
	if smartDoc["issuer"] != fhirBase {
		t.Fatalf("expected issuer %q, got %v", fhirBase, smartDoc["issuer"])
	}

	oidcResp, err := http.Get(fhirBase + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("GET openid-configuration: %v", err)
	}
	defer oidcResp.Body.Close()
	var oidcDoc map[string]any
	if err := json.NewDecoder(oidcResp.Body).Decode(&oidcDoc); err != nil {
		t.Fatalf("decode openid-configuration: %v", err)
	}
	if oidcDoc["issuer"] != fhirBase {
		t.Fatalf("expected OIDC issuer %q, got %v", fhirBase, oidcDoc["issuer"])
	}

	jwksResp, err := http.Get(oidcDoc["jwks_uri"].(string))
	if err != nil {
		t.Fatalf("GET jwks_uri: %v", err)
	}
	defer jwksResp.Body.Close()
	var jwks auth.JWKSet
	if err := json.NewDecoder(jwksResp.Body).Decode(&jwks); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	if len(jwks.Keys) == 0 || jwks.Keys[0].Kty != "RSA" {
		t.Fatalf("expected RSA signing key in JWKS, got %+v", jwks)
	}

	// 2. Standalone Launch GET /oauth2/authorize with PKCE S256 & aud.
	verifier, challenge := pkceS256Pair()
	redirectURI := "http://localhost:4567/custom/smart_stu2/redirect"
	clientID := "standalone-public-app"
	requestedScope := "launch/patient openid fhirUser offline_access patient/*.rs"

	authParams := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {requestedScope},
		"state":                 {"state-xyz-123"},
		"nonce":                 {"nonce-abc-456"},
		"aud":                   {fhirBase},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	authHTTPResp, err := noRedirectClient().Get(ts.URL + "/oauth2/authorize?" + authParams.Encode())
	if err != nil {
		t.Fatalf("GET /oauth2/authorize: %v", err)
	}
	defer authHTTPResp.Body.Close()
	if authHTTPResp.StatusCode != http.StatusFound {
		raw, _ := io.ReadAll(authHTTPResp.Body)
		t.Fatalf("expected 302 redirect, got %d: %s", authHTTPResp.StatusCode, string(raw))
	}
	cbURL, err := url.Parse(authHTTPResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse callback location: %v", err)
	}
	if cbURL.Query().Get("state") != "state-xyz-123" {
		t.Fatalf("state mismatch: %s", cbURL.Query().Get("state"))
	}
	code := cbURL.Query().Get("code")
	if code == "" {
		t.Fatal("missing authorization code in redirect")
	}

	// 3. Exchange authorization_code at POST /oauth2/token.
	tokForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	tokResp, err := http.PostForm(ts.URL+"/oauth2/token", tokForm)
	if err != nil {
		t.Fatalf("POST /oauth2/token: %v", err)
	}
	defer tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(tokResp.Body)
		t.Fatalf("token status=%d: %s", tokResp.StatusCode, string(raw))
	}
	if !strings.Contains(strings.ToLower(tokResp.Header.Get("Cache-Control")), "no-store") {
		t.Fatalf("missing Cache-Control: no-store header: %q", tokResp.Header.Get("Cache-Control"))
	}
	if !strings.Contains(strings.ToLower(tokResp.Header.Get("Pragma")), "no-cache") {
		t.Fatalf("missing Pragma: no-cache header: %q", tokResp.Header.Get("Pragma"))
	}

	var tokBody map[string]any
	if err := json.NewDecoder(tokResp.Body).Decode(&tokBody); err != nil {
		t.Fatalf("decode token body: %v", err)
	}
	accessToken, _ := tokBody["access_token"].(string)
	idToken, _ := tokBody["id_token"].(string)
	refreshToken, _ := tokBody["refresh_token"].(string)
	if accessToken == "" || idToken == "" || refreshToken == "" {
		t.Fatalf("expected access_token, id_token, and refresh_token: %+v", tokBody)
	}
	if tokBody["patient"] != "pat-alice-001" {
		t.Fatalf("expected patient pat-alice-001, got %v", tokBody["patient"])
	}

	// 4. Verify RS256 OIDC id_token claims and fhirUser resource retrieval.
	parsedID, err := auth.ParseJWT(idToken)
	if err != nil {
		t.Fatalf("parse id_token: %v", err)
	}
	pubKey, err := jwks.Keys[0].PublicKey()
	if err != nil {
		t.Fatalf("jwk public key: %v", err)
	}
	if err := parsedID.VerifySignature(pubKey); err != nil {
		t.Fatalf("verify id_token signature: %v", err)
	}
	if parsedID.Payload.Issuer != fhirBase {
		t.Fatalf("id_token iss=%q, want %q", parsedID.Payload.Issuer, fhirBase)
	}
	if !parsedID.Payload.Audience.Contains(clientID) {
		t.Fatalf("id_token aud=%v does not contain %q", parsedID.Payload.Audience, clientID)
	}
	fhirUser, _ := parsedID.Payload.Extra["fhirUser"].(string)
	if fhirUser != "Patient/pat-alice-001" {
		t.Fatalf("id_token fhirUser=%q, want Patient/pat-alice-001", fhirUser)
	}
	if parsedID.Payload.Extra["nonce"] != "nonce-abc-456" {
		t.Fatalf("id_token nonce=%v, want nonce-abc-456", parsedID.Payload.Extra["nonce"])
	}

	fhirReq, _ := http.NewRequest(http.MethodGet, fhirBase+"/"+fhirUser, nil)
	fhirReq.Header.Set("Authorization", "Bearer "+accessToken)
	fhirReadResp, err := http.DefaultClient.Do(fhirReq)
	if err != nil {
		t.Fatalf("GET %s: %v", fhirUser, err)
	}
	defer fhirReadResp.Body.Close()
	if fhirReadResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(fhirReadResp.Body)
		t.Fatalf("GET %s status=%d: %s", fhirUser, fhirReadResp.StatusCode, string(raw))
	}

	// 5. Verify RFC 7662 Token Introspection for active and invalid/revoked tokens.
	introForm := url.Values{"token": {accessToken}}
	introReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/oauth2/introspect", strings.NewReader(introForm.Encode()))
	introReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	introReq.Header.Set("Authorization", "Bearer "+accessToken)
	introResp, err := http.DefaultClient.Do(introReq)
	if err != nil {
		t.Fatalf("POST /oauth2/introspect: %v", err)
	}
	defer introResp.Body.Close()
	if introResp.StatusCode != http.StatusOK {
		t.Fatalf("introspect status=%d", introResp.StatusCode)
	}
	var introDoc map[string]any
	if err := json.NewDecoder(introResp.Body).Decode(&introDoc); err != nil {
		t.Fatalf("decode introspect response: %v", err)
	}
	if introDoc["active"] != true {
		t.Fatalf("expected active=true, got %+v", introDoc)
	}
	if introDoc["client_id"] != clientID {
		t.Fatalf("expected client_id=%q, got %v", clientID, introDoc["client_id"])
	}
	if introDoc["scope"] != requestedScope {
		t.Fatalf("expected exact scope %q, got %v", requestedScope, introDoc["scope"])
	}
	if introDoc["iss"] != fhirBase || introDoc["sub"] != parsedID.Payload.Subject || introDoc["fhirUser"] != fhirUser {
		t.Fatalf("introspection OIDC claim mismatch: %+v", introDoc)
	}
	if introDoc["patient"] != "pat-alice-001" || introDoc["encounter"] != "enc-alice-001" {
		t.Fatalf("introspection launch context mismatch: %+v", introDoc)
	}

	invalidIntroResp, err := http.PostForm(ts.URL+"/oauth2/introspect", url.Values{"token": {"invalid_token_value"}})
	if err != nil {
		t.Fatalf("POST /oauth2/introspect invalid: %v", err)
	}
	defer invalidIntroResp.Body.Close()
	var invalidDoc map[string]any
	if err := json.NewDecoder(invalidIntroResp.Body).Decode(&invalidDoc); err != nil {
		t.Fatalf("decode invalid introspect: %v", err)
	}
	if len(invalidDoc) != 1 || invalidDoc["active"] != false {
		t.Fatalf("expected exact 1-key {\"active\": false}, got %+v", invalidDoc)
	}

	// 6. Refresh token rotation without scopes and with narrowed scopes.
	refResp1, err := http.PostForm(ts.URL+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	})
	if err != nil {
		t.Fatalf("refresh 1: %v", err)
	}
	defer refResp1.Body.Close()
	if refResp1.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(refResp1.Body)
		t.Fatalf("refresh 1 status=%d: %s", refResp1.StatusCode, string(raw))
	}
	var refDoc1 map[string]any
	_ = json.NewDecoder(refResp1.Body).Decode(&refDoc1)
	rt2, _ := refDoc1["refresh_token"].(string)
	rotIDToken, _ := refDoc1["id_token"].(string)
	if rt2 == "" || rt2 == refreshToken {
		t.Fatalf("expected rotated refresh_token, got %q", rt2)
	}
	if rotIDToken == "" || rotIDToken == refDoc1["access_token"] {
		t.Fatal("expected distinct OIDC id_token on refresh")
	}
	parsedRotID, err := auth.ParseJWT(rotIDToken)
	if err != nil {
		t.Fatalf("parse rotated id_token: %v", err)
	}
	if parsedRotID.Payload.Issuer != fhirBase || !parsedRotID.Payload.Audience.Contains(clientID) {
		t.Fatalf("rotated id_token iss/aud mismatch: iss=%q aud=%v", parsedRotID.Payload.Issuer, parsedRotID.Payload.Audience)
	}

	narrowScope := "openid fhirUser patient/*.rs"
	refResp2, err := http.PostForm(ts.URL+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt2},
		"client_id":     {clientID},
		"scope":         {narrowScope},
	})
	if err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	defer refResp2.Body.Close()
	if refResp2.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(refResp2.Body)
		t.Fatalf("refresh 2 status=%d: %s", refResp2.StatusCode, string(raw))
	}
	var refDoc2 map[string]any
	_ = json.NewDecoder(refResp2.Body).Decode(&refDoc2)
	if refDoc2["scope"] != narrowScope {
		t.Fatalf("expected narrowed scope %q, got %v", narrowScope, refDoc2["scope"])
	}
	rt3, _ := refDoc2["refresh_token"].(string)

	// Overscoped refresh request must return 400 invalid_scope.
	badScopeResp, err := http.PostForm(ts.URL+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt3},
		"client_id":     {clientID},
		"scope":         {"system/*.cruds"},
	})
	if err != nil {
		t.Fatalf("overscoped refresh: %v", err)
	}
	defer badScopeResp.Body.Close()
	if badScopeResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on overscoped refresh, got %d", badScopeResp.StatusCode)
	}

	// Revoke access token and verify introspection returns {"active": false}.
	revResp, err := http.PostForm(ts.URL+"/oauth2/revoke", url.Values{"token": {accessToken}})
	if err != nil {
		t.Fatalf("POST /oauth2/revoke: %v", err)
	}
	revResp.Body.Close()
	revIntroResp, err := http.PostForm(ts.URL+"/oauth2/introspect", url.Values{"token": {accessToken}})
	if err != nil {
		t.Fatalf("POST /oauth2/introspect after revoke: %v", err)
	}
	defer revIntroResp.Body.Close()
	var revIntroDoc map[string]any
	_ = json.NewDecoder(revIntroResp.Body).Decode(&revIntroDoc)
	if len(revIntroDoc) != 1 || revIntroDoc["active"] != false {
		t.Fatalf("expected revoked token to return {\"active\": false}, got %+v", revIntroDoc)
	}
}

func TestSMARTSTU2_EHRLaunch_ConfidentialSymmetric_AuthorizePost(t *testing.T) {
	_, ts := newSMARTSTU2TestServer(t)
	fhirBase := ts.URL + "/fhir/r4/demo"

	// 1. Register confidential symmetric client via POST /oauth2/register.
	clientID := "ehr-symmetric-client"
	clientSecret := "s3cr3t-ehr-key-98765"
	regPayload, _ := json.Marshal(map[string]any{
		"client_id":                  clientID,
		"client_secret":              clientSecret,
		"client_name":                "EHR Confidential Symmetric App",
		"token_endpoint_auth_method": "client_secret_basic",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"redirect_uris":              []string{"http://localhost:4567/custom/smart_stu2/redirect"},
	})
	regResp, err := http.Post(ts.URL+"/oauth2/register", "application/json", bytes.NewReader(regPayload))
	if err != nil {
		t.Fatalf("POST /oauth2/register: %v", err)
	}
	defer regResp.Body.Close()
	if regResp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(regResp.Body)
		t.Fatalf("register status=%d: %s", regResp.StatusCode, string(raw))
	}

	// 2. Mint opaque EHR launch context via POST /oauth2/launch.
	launchPayload, _ := json.Marshal(map[string]string{
		"patient":   "pat-alice-001",
		"encounter": "enc-alice-001",
		"tenant":    "demo",
	})
	launchResp, err := http.Post(ts.URL+"/oauth2/launch", "application/json", bytes.NewReader(launchPayload))
	if err != nil {
		t.Fatalf("POST /oauth2/launch: %v", err)
	}
	defer launchResp.Body.Close()
	if launchResp.StatusCode != http.StatusCreated {
		t.Fatalf("launch status=%d", launchResp.StatusCode)
	}
	var launchDoc map[string]string
	_ = json.NewDecoder(launchResp.Body).Decode(&launchDoc)
	launchToken := launchDoc["launch"]
	if !strings.HasPrefix(launchToken, "flint-launch-") {
		t.Fatalf("unexpected launch token: %q", launchToken)
	}

	// 3. Submit POST /oauth2/authorize (authorize-post capability) with Practitioner user/* scopes.
	verifier, challenge := pkceS256Pair()
	redirectURI := "http://localhost:4567/custom/smart_stu2/redirect"
	authForm := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"launch":                {launchToken},
		"scope":                 {"launch launch/encounter openid fhirUser offline_access user/*.cruds"},
		"state":                 {"ehr-state-001"},
		"aud":                   {fhirBase},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	authResp, err := noRedirectClient().PostForm(ts.URL+"/oauth2/authorize", authForm)
	if err != nil {
		t.Fatalf("POST /oauth2/authorize: %v", err)
	}
	defer authResp.Body.Close()
	if authResp.StatusCode != http.StatusFound {
		raw, _ := io.ReadAll(authResp.Body)
		t.Fatalf("expected 302 from POST /oauth2/authorize, got %d: %s", authResp.StatusCode, string(raw))
	}
	cbURL, _ := url.Parse(authResp.Header.Get("Location"))
	code := cbURL.Query().Get("code")

	// 4. Reject unauthenticated or wrong-secret token exchange for confidential client (before code consumption via wrong secret).
	wrongReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/oauth2/token", strings.NewReader(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}.Encode()))
	wrongReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wrongReq.SetBasicAuth(clientID, "wrong-secret")
	wrongResp, err := http.DefaultClient.Do(wrongReq)
	if err != nil {
		t.Fatalf("wrong secret request: %v", err)
	}
	wrongResp.Body.Close()
	if wrongResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on wrong client_secret, got %d", wrongResp.StatusCode)
	}

	// 5. Exchange code with valid HTTP Basic credentials.
	tokReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/oauth2/token", strings.NewReader(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokReq.SetBasicAuth(clientID, clientSecret)
	tokResp, err := http.DefaultClient.Do(tokReq)
	if err != nil {
		t.Fatalf("Basic token exchange: %v", err)
	}
	defer tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(tokResp.Body)
		t.Fatalf("Basic token exchange status=%d: %s", tokResp.StatusCode, string(raw))
	}
	var tokDoc map[string]any
	_ = json.NewDecoder(tokResp.Body).Decode(&tokDoc)
	if tokDoc["patient"] != "pat-alice-001" || tokDoc["encounter"] != "enc-alice-001" {
		t.Fatalf("expected resolved EHR patient/encounter context, got %+v", tokDoc)
	}

	parsedID, err := auth.ParseJWT(tokDoc["id_token"].(string))
	if err != nil {
		t.Fatalf("parse EHR id_token: %v", err)
	}
	if parsedID.Payload.Extra["fhirUser"] != "Practitioner/prac-cardiology-1" {
		t.Fatalf("expected Practitioner/prac-cardiology-1 fhirUser, got %v", parsedID.Payload.Extra["fhirUser"])
	}

	// Verify Practitioner resource is readable with the issued user/*.cruds access token.
	pracReq, _ := http.NewRequest(http.MethodGet, fhirBase+"/Practitioner/prac-cardiology-1", nil)
	pracReq.Header.Set("Authorization", "Bearer "+tokDoc["access_token"].(string))
	pracResp, err := http.DefaultClient.Do(pracReq)
	if err != nil {
		t.Fatalf("GET Practitioner: %v", err)
	}
	defer pracResp.Body.Close()
	if pracResp.StatusCode != http.StatusOK {
		t.Fatalf("GET Practitioner status=%d", pracResp.StatusCode)
	}

	// 6. Refresh using client_secret_post.
	rt := tokDoc["refresh_token"].(string)
	postRefreshResp, err := http.PostForm(ts.URL+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if err != nil {
		t.Fatalf("client_secret_post refresh: %v", err)
	}
	defer postRefreshResp.Body.Close()
	if postRefreshResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(postRefreshResp.Body)
		t.Fatalf("client_secret_post refresh status=%d: %s", postRefreshResp.StatusCode, string(raw))
	}
}

func TestSMARTSTU2_ConfidentialAsymmetric_PrivateKeyJWT_AuthCodeAndRefresh(t *testing.T) {
	_, ts := newSMARTSTU2TestServer(t)
	fhirBase := ts.URL + "/fhir/r4/demo"

	ecPriv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate P-384 key: %v", err)
	}
	uncompressed, err := ecPriv.PublicKey.Bytes()
	if err != nil {
		t.Fatalf("encode P-384 public key: %v", err)
	}
	const coordLen = 48
	if len(uncompressed) != 1+2*coordLen || uncompressed[0] != 0x04 {
		t.Fatalf("unexpected P-384 public key encoding length=%d", len(uncompressed))
	}
	xBytes := uncompressed[1 : 1+coordLen]
	yBytes := uncompressed[1+coordLen:]

	kid := "es384-smart-key-1"
	clientID := "asymmetric-smart-app"
	regBody, _ := json.Marshal(map[string]any{
		"client_id":                  clientID,
		"token_endpoint_auth_method": "private_key_jwt",
		"jwks": map[string]any{
			"keys": []map[string]string{
				{
					"kty": "EC",
					"crv": "P-384",
					"use": "sig",
					"alg": "ES384",
					"kid": kid,
					"x":   base64.RawURLEncoding.EncodeToString(xBytes),
					"y":   base64.RawURLEncoding.EncodeToString(yBytes),
				},
			},
		},
	})
	regResp, err := http.Post(ts.URL+"/oauth2/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatalf("register asymmetric client: %v", err)
	}
	regResp.Body.Close()
	if regResp.StatusCode != http.StatusCreated {
		t.Fatalf("register asymmetric status=%d", regResp.StatusCode)
	}

	verifier, challenge := pkceS256Pair()
	redirectURI := "http://localhost:4567/custom/smart_stu2/redirect"
	authResp, err := noRedirectClient().Get(ts.URL + "/oauth2/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"launch/patient openid fhirUser offline_access patient/*.rs"},
		"state":                 {"asym-state"},
		"aud":                   {fhirBase},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode())
	if err != nil {
		t.Fatalf("authorize asymmetric: %v", err)
	}
	authResp.Body.Close()
	cbURL, _ := url.Parse(authResp.Header.Get("Location"))
	code := cbURL.Query().Get("code")

	assertion1 := signES384ClientAssertion(t, ecPriv, kid, clientID, ts.URL+"/oauth2/token")
	tokResp, err := http.PostForm(ts.URL+"/oauth2/token", url.Values{
		"grant_type":            {"authorization_code"},
		"code":                  {code},
		"redirect_uri":          {redirectURI},
		"code_verifier":         {verifier},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion1},
	})
	if err != nil {
		t.Fatalf("token exchange private_key_jwt: %v", err)
	}
	defer tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(tokResp.Body)
		t.Fatalf("asymmetric token exchange status=%d: %s", tokResp.StatusCode, string(raw))
	}
	var tokDoc map[string]any
	_ = json.NewDecoder(tokResp.Body).Decode(&tokDoc)
	rt := tokDoc["refresh_token"].(string)

	// Replaying the same client_assertion jti must fail!
	replayResp, err := http.PostForm(ts.URL+"/oauth2/token", url.Values{
		"grant_type":            {"refresh_token"},
		"refresh_token":         {rt},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion1},
	})
	if err != nil {
		t.Fatalf("replay request: %v", err)
	}
	replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on replayed client_assertion jti, got %d", replayResp.StatusCode)
	}

	// Fresh ES384 client_assertion on refresh_token grant succeeds!
	assertion2 := signES384ClientAssertion(t, ecPriv, kid, clientID, ts.URL+"/oauth2/token")
	refResp, err := http.PostForm(ts.URL+"/oauth2/token", url.Values{
		"grant_type":            {"refresh_token"},
		"refresh_token":         {rt},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion2},
	})
	if err != nil {
		t.Fatalf("asymmetric refresh: %v", err)
	}
	defer refResp.Body.Close()
	if refResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(refResp.Body)
		t.Fatalf("asymmetric refresh status=%d: %s", refResp.StatusCode, string(raw))
	}
}

func TestSMARTSTU2_NegativePKCEAndAudEnforcement(t *testing.T) {
	srv, ts := newSMARTSTU2TestServer(t)
	fhirBase := ts.URL + "/fhir/r4/demo"
	redirectURI := "http://localhost:4567/cb"
	_, challenge := pkceS256Pair()

	cases := []struct {
		name   string
		params url.Values
		strict bool
	}{
		{
			name: "missing PKCE when aud provided",
			params: url.Values{
				"response_type": {"code"},
				"client_id":     {"c1"},
				"redirect_uri":  {redirectURI},
				"aud":           {fhirBase},
			},
		},
		{
			name:   "missing PKCE when SetRequirePKCE(true)",
			strict: true,
			params: url.Values{
				"response_type": {"code"},
				"client_id":     {"c1"},
				"redirect_uri":  {redirectURI},
			},
		},
		{
			name: "plain code_challenge_method rejected",
			params: url.Values{
				"response_type":         {"code"},
				"client_id":             {"c1"},
				"redirect_uri":          {redirectURI},
				"aud":                   {fhirBase},
				"code_challenge":        {challenge},
				"code_challenge_method": {"plain"},
			},
		},
		{
			name: "missing code_challenge_method rejected",
			params: url.Values{
				"response_type":  {"code"},
				"client_id":      {"c1"},
				"redirect_uri":   {redirectURI},
				"code_challenge": {challenge},
			},
		},
		{
			name: "invalid foreign aud rejected",
			params: url.Values{
				"response_type":         {"code"},
				"client_id":             {"c1"},
				"redirect_uri":          {redirectURI},
				"aud":                   {"https://untrusted.example.org/other-api"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv.SetRequirePKCE(tc.strict)
			defer srv.SetRequirePKCE(false)

			resp, err := noRedirectClient().Get(ts.URL + "/oauth2/authorize?" + tc.params.Encode())
			if err != nil {
				t.Fatalf("GET /oauth2/authorize: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400 Bad Request for %s, got %d", tc.name, resp.StatusCode)
			}
		})
	}
}
