package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
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

func openTrack8SQLiteStore(t *testing.T) *sqlite.Store {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)
	if err := sqlite.RunMigrations(context.Background(), sqlDB, sqlite.JournalMemory); err != nil {
		t.Fatalf("sqlite.RunMigrations: %v", err)
	}
	return sqlite.New(sqlDB, sqlite.WithJournalMode(sqlite.JournalMemory), sqlite.WithDSN(":memory:"))
}

func mintTestUDAPChain(t *testing.T, sanURI string) (rootPEM []byte, leafX5C []string, leafKey *rsa.PrivateKey) {
	t.Helper()
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate UDAP root key: %v", err)
	}
	now := time.Now().UTC()
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1001),
		Subject:               pkix.Name{CommonName: "Test UDAP Client Root CA", Country: []string{"US"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("create UDAP root cert: %v", err)
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatalf("parse UDAP root cert: %v", err)
	}
	rootPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})

	leafKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate UDAP leaf key: %v", err)
	}
	sanParsed, err := url.Parse(sanURI)
	if err != nil {
		t.Fatalf("parse SAN URI %q: %v", sanURI, err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2002),
		Subject:               pkix.Name{CommonName: "Test UDAP Client Leaf", Country: []string{"US"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{sanParsed},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, rootCert, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("create UDAP leaf cert: %v", err)
	}
	return rootPEM, []string{base64.StdEncoding.EncodeToString(leafDER)}, leafKey
}

func TestTrack8UDAPSecurityConformance(t *testing.T) {
	st := openTrack8SQLiteStore(t)

	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	srv.EnableDevTokenEnforcement()
	srv.SetRequireRegisteredClientJWK(true)
	srv.SetRequirePKCE(true)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		_ = srv.Close()
		ts.Close()
	})

	// 1. UDAP Discovery + Signed Metadata + Server Root CA + CRL verification
	discResp, err := http.Get(ts.URL + "/fhir/r4/demo/.well-known/udap")
	if err != nil {
		t.Fatalf("GET /.well-known/udap: %v", err)
	}
	defer discResp.Body.Close()
	if discResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /.well-known/udap, got %d", discResp.StatusCode)
	}
	var discDoc map[string]any
	if err := json.NewDecoder(discResp.Body).Decode(&discDoc); err != nil {
		t.Fatalf("decode UDAP discovery: %v", err)
	}
	signedMeta, _ := discDoc["signed_metadata"].(string)
	if signedMeta == "" {
		t.Fatalf("missing signed_metadata in UDAP discovery document")
	}
	parsedMetaJWT, err := auth.ParseJWT(signedMeta)
	if err != nil || len(parsedMetaJWT.Header.X5c) == 0 {
		t.Fatalf("expected valid signed_metadata JWT with x5c, got err=%v x5c=%v", err, parsedMetaJWT)
	}
	leafDER, err := base64.StdEncoding.DecodeString(parsedMetaJWT.Header.X5c[0])
	if err != nil {
		t.Fatalf("decode server x5c[0]: %v", err)
	}
	srvLeaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse server leaf cert: %v", err)
	}
	caResp, err := http.Get(ts.URL + "/oauth2/udap-ca.pem")
	if err != nil || caResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /oauth2/udap-ca.pem failed: status=%v err=%v", caResp, err)
	}
	caPEMBytes, _ := io.ReadAll(caResp.Body)
	caResp.Body.Close()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEMBytes) {
		t.Fatalf("failed to append server Root CA PEM")
	}
	if _, err := srvLeaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Fatalf("server signed_metadata x5c failed Root CA verification: %v", err)
	}
	if err := parsedMetaJWT.VerifySignature(srvLeaf.PublicKey); err != nil {
		t.Fatalf("server signed_metadata signature verification failed: %v", err)
	}
	crlResp, err := http.Get(ts.URL + "/oauth2/udap.crl")
	if err != nil || crlResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /oauth2/udap.crl failed: status=%v err=%v", crlResp, err)
	}
	crlDER, _ := io.ReadAll(crlResp.Body)
	crlResp.Body.Close()
	if _, err := x509.ParseRevocationList(crlDER); err != nil {
		t.Fatalf("parse server CRL failed: %v", err)
	}

	// 2. UDAP Dynamic Client Registration (Client Credentials + Authorization Code + Negative Checks)
	ccISS := "https://client.example.org/udap-cc"
	rootPEM, ccX5C, ccKey := mintTestUDAPChain(t, ccISS)

	// Before registering trust anchor -> must reject with 400 invalid_software_statement
	now := time.Now().Unix()
	untrustedSS, err := auth.SignRS256WithX5C(map[string]any{
		"iss":                        ccISS,
		"sub":                        ccISS,
		"aud":                        ts.URL + "/oauth2/register",
		"exp":                        now + 300,
		"iat":                        now,
		"jti":                        uuid.NewString(),
		"client_name":                "UDAP CC Test App",
		"grant_types":                []string{"client_credentials"},
		"token_endpoint_auth_method": "private_key_jwt",
		"scope":                      "system/*.rs",
	}, ccKey, ccX5C)
	if err != nil {
		t.Fatalf("sign untrusted software_statement: %v", err)
	}
	untrustedBody, _ := json.Marshal(map[string]any{"udap": "1", "software_statement": untrustedSS})
	untrustedResp, err := http.Post(ts.URL+"/oauth2/register", "application/json", bytes.NewReader(untrustedBody))
	if err != nil {
		t.Fatalf("POST untrusted DCR: %v", err)
	}
	untrustedResp.Body.Close()
	if untrustedResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for untrusted UDAP CA, got %d", untrustedResp.StatusCode)
	}

	// Register trust anchor via POST /oauth2/udap-trust-anchor
	taBody, _ := json.Marshal(map[string]string{"cert_pem": string(rootPEM)})
	taResp, err := http.Post(ts.URL+"/oauth2/udap-trust-anchor", "application/json", bytes.NewReader(taBody))
	if err != nil || taResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /oauth2/udap-trust-anchor failed: status=%v err=%v", taResp, err)
	}
	taResp.Body.Close()

	// Register CC client -> 201 Created, then re-register -> 200 OK
	validCCSS, _ := auth.SignRS256WithX5C(map[string]any{
		"iss":                        ccISS,
		"sub":                        ccISS,
		"aud":                        ts.URL + "/oauth2/register",
		"exp":                        now + 300,
		"iat":                        now,
		"jti":                        uuid.NewString(),
		"client_name":                "UDAP CC Test App",
		"grant_types":                []string{"client_credentials"},
		"token_endpoint_auth_method": "private_key_jwt",
		"scope":                      "system/*.rs",
	}, ccKey, ccX5C)
	regReqBody, _ := json.Marshal(map[string]any{"udap": "1", "software_statement": validCCSS})
	regResp1, err := http.Post(ts.URL+"/oauth2/register", "application/json", bytes.NewReader(regReqBody))
	if err != nil {
		t.Fatalf("POST UDAP DCR 1: %v", err)
	}
	if regResp1.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(regResp1.Body)
		regResp1.Body.Close()
		t.Fatalf("expected 201 Created on initial UDAP DCR, got %d: %s", regResp1.StatusCode, string(b))
	}
	var regDoc1 map[string]any
	_ = json.NewDecoder(regResp1.Body).Decode(&regDoc1)
	regResp1.Body.Close()
	ccClientID, _ := regDoc1["client_id"].(string)
	if ccClientID == "" {
		t.Fatalf("expected non-empty client_id from UDAP DCR")
	}

	updateCCSS, _ := auth.SignRS256WithX5C(map[string]any{
		"iss":                        ccISS,
		"sub":                        ccISS,
		"aud":                        ts.URL + "/oauth2/register",
		"exp":                        now + 300,
		"iat":                        now,
		"jti":                        uuid.NewString(),
		"client_name":                "UDAP CC Test App Updated",
		"grant_types":                []string{"client_credentials"},
		"token_endpoint_auth_method": "private_key_jwt",
		"scope":                      "system/*.rs",
	}, ccKey, ccX5C)
	updBody, _ := json.Marshal(map[string]any{"udap": "1", "software_statement": updateCCSS})
	regResp2, err := http.Post(ts.URL+"/oauth2/register", "application/json", bytes.NewReader(updBody))
	if err != nil || regResp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on UDAP DCR update, got status=%v err=%v", regResp2, err)
	}
	regResp2.Body.Close()

	// 3. UDAP B2B Client Credentials Token Exchange + Replay Protection
	b2bAssertionJTI := uuid.NewString()
	b2bAssertion, err := auth.SignRS256WithX5C(map[string]any{
		"iss": ccClientID,
		"sub": ccClientID,
		"aud": ts.URL + "/oauth2/token",
		"exp": now + 300,
		"iat": now,
		"jti": b2bAssertionJTI,
		"extensions": map[string]any{
			"hl7-b2b": map[string]any{
				"version":           "1",
				"organization_id":   "https://org.example.org",
				"organization_name": "Example Org",
				"purpose_of_use":    []string{"TREAT"},
			},
		},
	}, ccKey, ccX5C)
	if err != nil {
		t.Fatalf("sign UDAP B2B assertion: %v", err)
	}
	tokForm := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {b2bAssertion},
		"scope":                 {"system/*.rs"},
		"udap":                  {"1"},
	}
	tokResp, err := http.PostForm(ts.URL+"/oauth2/token", tokForm)
	if err != nil {
		t.Fatalf("POST /oauth2/token UDAP CC: %v", err)
	}
	if tokResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(tokResp.Body)
		tokResp.Body.Close()
		t.Fatalf("expected 200 OK from UDAP CC token exchange, got %d: %s", tokResp.StatusCode, string(b))
	}
	tokResp.Body.Close()

	// Replaying identical jti must fail
	replayResp, err := http.PostForm(ts.URL+"/oauth2/token", tokForm)
	if err != nil {
		t.Fatalf("POST replay UDAP CC: %v", err)
	}
	replayResp.Body.Close()
	if replayResp.StatusCode == http.StatusOK {
		t.Fatalf("expected replayed UDAP client_assertion jti to be rejected")
	}

	// 4. UDAP Authorization Code Flow WITHOUT PKCE (even when strict PKCE is enabled for SMART)
	acISS := "https://client.example.org/udap-ac"
	acRootPEM, acX5C, acKey := mintTestUDAPChain(t, acISS)
	if err := srv.RegisterUDAPTrustAnchorPEM(acRootPEM); err != nil {
		t.Fatalf("RegisterUDAPTrustAnchorPEM: %v", err)
	}
	redirectURI := "https://client.example.org/cb"
	acSS, _ := auth.SignRS256WithX5C(map[string]any{
		"iss":                        acISS,
		"sub":                        acISS,
		"aud":                        ts.URL + "/oauth2/register",
		"exp":                        now + 300,
		"iat":                        now,
		"jti":                        uuid.NewString(),
		"client_name":                "UDAP AC Test App",
		"redirect_uris":              []string{redirectURI},
		"response_types":             []string{"code"},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_method": "private_key_jwt",
		"scope":                      "openid fhirUser patient/Patient.rs",
	}, acKey, acX5C)
	acRegBody, _ := json.Marshal(map[string]any{"udap": "1", "software_statement": acSS})
	acRegResp, err := http.Post(ts.URL+"/oauth2/register", "application/json", bytes.NewReader(acRegBody))
	if err != nil || acRegResp.StatusCode != http.StatusCreated {
		t.Fatalf("UDAP AC DCR failed: status=%v err=%v", acRegResp, err)
	}
	var acRegDoc map[string]any
	_ = json.NewDecoder(acRegResp.Body).Decode(&acRegDoc)
	acRegResp.Body.Close()
	acClientID := acRegDoc["client_id"].(string)

	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	authURL := ts.URL + "/oauth2/authorize?" + url.Values{
		"response_type": {"code"},
		"client_id":     {acClientID},
		"redirect_uri":  {redirectURI},
		"state":         {"xyz-state"},
		"scope":         {"openid fhirUser patient/Patient.rs"},
		"aud":           {ts.URL + "/fhir/r4/demo"},
	}.Encode()
	authzResp, err := noRedirectClient.Get(authURL)
	if err != nil {
		t.Fatalf("GET /oauth2/authorize UDAP AC: %v", err)
	}
	authzResp.Body.Close()
	if authzResp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 redirect for UDAP AC without PKCE, got %d", authzResp.StatusCode)
	}
	cbLoc, err := url.Parse(authzResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse callback Location: %v", err)
	}
	authCode := cbLoc.Query().Get("code")
	if authCode == "" {
		t.Fatalf("missing authorization code in redirect Location")
	}

	acAssertion, _ := auth.SignRS256WithX5C(map[string]any{
		"iss": acClientID,
		"sub": acClientID,
		"aud": ts.URL + "/oauth2/token",
		"exp": now + 300,
		"iat": now,
		"jti": uuid.NewString(),
	}, acKey, acX5C)
	acTokResp, err := http.PostForm(ts.URL+"/oauth2/token", url.Values{
		"grant_type":            {"authorization_code"},
		"code":                  {authCode},
		"redirect_uri":          {redirectURI},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {acAssertion},
		"udap":                  {"1"},
	})
	if err != nil {
		t.Fatalf("POST /oauth2/token UDAP AC: %v", err)
	}
	defer acTokResp.Body.Close()
	if acTokResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(acTokResp.Body)
		t.Fatalf("expected 200 OK on UDAP AC token exchange, got %d: %s", acTokResp.StatusCode, string(b))
	}
}

func TestTrack8CRDAndDTRConformance(t *testing.T) {
	st := openTrack8SQLiteStore(t)

	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)

	// Register RS384 CDS Hooks client key to verify RS384 JWS support
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	srv.RegisterCDSClientJWKS("https://ehr.example.org", auth.JWKSet{
		Keys: []auth.JWK{
			{
				Kty: "RSA",
				Kid: "crd-rs384-1",
				Alg: "RS384",
				Use: "sig",
				N:   base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()),
				E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes()),
			},
		},
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		_ = srv.Close()
		ts.Close()
	})

	// 1. CRD Discovery check
	discResp, err := http.Get(ts.URL + "/cds-services")
	if err != nil || discResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /cds-services failed: status=%v err=%v", discResp, err)
	}
	var discMap map[string]any
	_ = json.NewDecoder(discResp.Body).Decode(&discMap)
	discResp.Body.Close()
	services, _ := discMap["services"].([]any)
	foundCRD := 0
	for _, s := range services {
		sm, _ := s.(map[string]any)
		if strings.HasPrefix(sm["id"].(string), "crd-") {
			foundCRD++
		}
	}
	if foundCRD != 6 {
		t.Fatalf("expected all 6 CRD services in /cds-services, got %d", foundCRD)
	}

	// 2. Invoke crd-order-sign signed with RS384 CDS Client JWS + validate Logical Model
	now := time.Now().Unix()
	targetURL := ts.URL + "/cds-services/crd-order-sign"
	cdsJWT, err := auth.SignRS384(map[string]any{
		"iss": "https://ehr.example.org",
		"aud": targetURL,
		"exp": now + 300,
		"iat": now,
		"jti": uuid.NewString(),
	}, rsaKey, "crd-rs384-1")
	if err != nil {
		t.Fatalf("SignRS384: %v", err)
	}

	hookReqBody, _ := json.Marshal(map[string]any{
		"hook":         "order-sign",
		"hookInstance": uuid.NewString(),
		"fhirServer":   ts.URL + "/fhir/r4/demo",
		"context": map[string]any{
			"userId":    "Practitioner/prac-1",
			"patientId": "pat-alice-001",
			"draftOrders": map[string]any{
				"resourceType": "Bundle",
				"type":         "collection",
				"entry": []any{
					map[string]any{
						"resource": map[string]any{
							"resourceType": "ServiceRequest",
							"id":           "sr-1",
							"status":       "draft",
							"intent":       "order",
							"code": map[string]any{
								"coding": []any{map[string]any{"system": "http://www.ama-assn.org/go/cpt", "code": "70551"}},
							},
							"subject": map[string]any{"reference": "Patient/pat-alice-001"},
						},
					},
				},
			},
		},
	})
	hReq, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(hookReqBody))
	hReq.Header.Set("Content-Type", "application/json")
	hReq.Header.Set("Authorization", "Bearer "+cdsJWT)
	hResp, err := http.DefaultClient.Do(hReq)
	if err != nil {
		t.Fatalf("POST crd-order-sign: %v", err)
	}
	respBytes, _ := io.ReadAll(hResp.Body)
	hResp.Body.Close()
	if hResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from crd-order-sign with RS384 JWT, got %d: %s", hResp.StatusCode, string(respBytes))
	}

	// Validate CRD Logical Model via POST /fhir/r4/demo/$validate (no resourceType!)
	valURL := ts.URL + "/fhir/r4/demo/$validate?profile=" + url.QueryEscape("http://hl7.org/fhir/us/davinci-crd/StructureDefinition/CRDHooksResponse-coverageInformation|2.2.1")
	valResp, err := http.Post(valURL, "application/fhir+json", bytes.NewReader(respBytes))
	if err != nil || valResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(valResp.Body)
		t.Fatalf("expected 200 OK validating CRDHooksResponse-coverageInformation Logical Model, got status=%v body=%s", valResp.StatusCode, string(b))
	}
	valResp.Body.Close()

	// Verify coverage-info: false suppresses systemActions
	disabledBody, _ := json.Marshal(map[string]any{
		"hook":         "order-sign",
		"hookInstance": uuid.NewString(),
		"extension": map[string]any{
			"davinci-crd.configuration": map[string]any{"coverage-info": false},
		},
		"context": map[string]any{
			"userId":    "Practitioner/prac-1",
			"patientId": "pat-alice-001",
			"draftOrders": map[string]any{
				"resourceType": "Bundle",
				"type":         "collection",
				"entry":        []any{},
			},
		},
	})
	disResp, err := http.Post(targetURL, "application/json", bytes.NewReader(disabledBody))
	if err != nil || disResp.StatusCode != http.StatusOK {
		t.Fatalf("POST crd-order-sign disabled coverage-info failed: %v", err)
	}
	var disMap map[string]any
	_ = json.NewDecoder(disResp.Body).Decode(&disMap)
	disResp.Body.Close()
	if actions, _ := disMap["systemActions"].([]any); len(actions) != 0 {
		t.Fatalf("expected 0 systemActions when coverage-info=false, got %d", len(actions))
	}

	// 3. DTR CapabilityStatement + $questionnaire-package + ValueSet/$expand + $next-question + $log-questionnaire-errors
	metaResp, err := http.Get(ts.URL + "/fhir/r4/demo/metadata")
	if err != nil || metaResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metadata failed: %v", err)
	}
	metaBytes, _ := io.ReadAll(metaResp.Body)
	metaResp.Body.Close()
	for _, needle := range []string{"dtr-payer-service", "questionnaire-package", "next-question", "log-questionnaire-errors"} {
		if !bytes.Contains(metaBytes, []byte(needle)) {
			t.Fatalf("CapabilityStatement missing DTR capability %q", needle)
		}
	}

	// Adaptive $questionnaire-package via context parameter
	qpReqBody, _ := json.Marshal(map[string]any{
		"resourceType": "Parameters",
		"parameter": []any{
			map[string]any{
				"name": "coverage",
				"resource": map[string]any{
					"resourceType": "Coverage",
					"id":           "cov-1",
					"status":       "active",
					"beneficiary":  map[string]any{"reference": "Patient/pat-alice-001"},
					"payor":        []any{map[string]any{"reference": "Organization/org-1"}},
				},
			},
			map[string]any{
				"name":        "context",
				"valueString": "CRD-ASSERT-ADAPT-OXYGEN-220",
			},
		},
	})
	qpResp, err := http.Post(ts.URL+"/fhir/r4/demo/Questionnaire/$questionnaire-package", "application/fhir+json", bytes.NewReader(qpReqBody))
	if err != nil || qpResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(qpResp.Body)
		t.Fatalf("POST $questionnaire-package failed: status=%v body=%s", qpResp.StatusCode, string(b))
	}
	var qpOut map[string]any
	_ = json.NewDecoder(qpResp.Body).Decode(&qpOut)
	qpResp.Body.Close()

	// Extract initial adaptive QuestionnaireResponse and step through $next-question to completion
	var adaptQR map[string]any
	for _, p := range qpOut["parameter"].([]any) {
		pm := p.(map[string]any)
		if pm["name"] == "packagebundle" {
			bnd := pm["resource"].(map[string]any)
			for _, e := range bnd["entry"].([]any) {
				res := e.(map[string]any)["resource"].(map[string]any)
				if res["resourceType"] == "QuestionnaireResponse" {
					adaptQR = res
				}
			}
		}
	}
	if adaptQR == nil {
		t.Fatalf("expected adaptive QuestionnaireResponse in packagebundle")
	}

	for step, wantStatus := range []string{"in-progress", "in-progress", "completed"} {
		nqBytes, _ := json.Marshal(adaptQR)
		nqResp, err := http.Post(ts.URL+"/fhir/r4/demo/Questionnaire/$next-question", "application/fhir+json", bytes.NewReader(nqBytes))
		if err != nil || nqResp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(nqResp.Body)
			t.Fatalf("$next-question step %d failed: status=%v body=%s", step+1, nqResp.StatusCode, string(b))
		}
		adaptQR = nil
		_ = json.NewDecoder(nqResp.Body).Decode(&adaptQR)
		nqResp.Body.Close()
		if got := adaptQR["status"]; got != wantStatus {
			t.Fatalf("$next-question step %d: expected status %q, got %v", step+1, wantStatus, got)
		}
	}

	// Expand persisted unexpanded DTR ValueSet
	expBody, _ := json.Marshal(map[string]any{
		"resourceType": "Parameters",
		"parameter": []any{
			map[string]any{"name": "url", "valueUri": "http://flint.health/fhir/ValueSet/dtr-large-vs|2.2.0"},
		},
	})
	expResp, err := http.Post(ts.URL+"/fhir/r4/demo/ValueSet/$expand", "application/fhir+json", bytes.NewReader(expBody))
	if err != nil || expResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(expResp.Body)
		t.Fatalf("POST ValueSet/$expand for DTR ValueSet failed: status=%v body=%s", expResp.StatusCode, string(b))
	}
	expResp.Body.Close()

	// $log-questionnaire-errors
	logErrBody, _ := json.Marshal(map[string]any{
		"resourceType": "Parameters",
		"parameter": []any{
			map[string]any{"name": "questionnaire", "valueCanonical": "http://flint.health/fhir/Questionnaire/dtr-home-oxygen|2.2.0"},
			map[string]any{
				"name": "operationOutcome",
				"resource": map[string]any{
					"resourceType": "OperationOutcome",
					"issue": []any{
						map[string]any{"severity": "error", "code": "processing", "diagnostics": "Test expression error"},
					},
				},
			},
		},
	})
	logResp, err := http.Post(ts.URL+"/fhir/r4/demo/Questionnaire/$log-questionnaire-errors", "application/fhir+json", bytes.NewReader(logErrBody))
	if err != nil || logResp.StatusCode != http.StatusOK {
		t.Fatalf("POST $log-questionnaire-errors failed: status=%v err=%v", logResp, err)
	}
	logResp.Body.Close()
}
