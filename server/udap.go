package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/google/uuid"
)

type udapServerLeaf struct {
	priv   *rsa.PrivateKey
	x5cB64 []string
}

type udapClientEntry struct {
	ClientID                string
	CertISS                 string
	ClientName              string
	GrantTypes              []string
	ResponseTypes           []string
	RedirectURIs            []string
	TokenEndpointAuthMethod string
	Scope                   string
	Contacts                []string
	LogoURI                 string
}

func (c udapClientEntry) supportsGrant(gt string) bool {
	return slices.Contains(c.GrantTypes, gt)
}

func (s *Server) ensureUDAPRootCA() (*x509.Certificate, *rsa.PrivateKey, []byte, error) {
	s.udapMu.RLock()
	if s.udapRootCert != nil && s.udapRootKey != nil {
		cert, key, pemBytes := s.udapRootCert, s.udapRootKey, s.udapRootPEM
		s.udapMu.RUnlock()
		return cert, key, pemBytes, nil
	}
	s.udapMu.RUnlock()

	s.udapMu.Lock()
	defer s.udapMu.Unlock()
	if s.udapRootCert != nil && s.udapRootKey != nil {
		return s.udapRootCert, s.udapRootKey, s.udapRootPEM, nil
	}

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, nil, nil, err
	}
	ski := sha1.Sum(pubDER)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Country:      []string{"US"},
			Organization: []string{"Flint FHIR Trust Community"},
			CommonName:   "Flint UDAP Trust Anchor Root CA",
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		SubjectKeyId:          ski[:],
		AuthorityKeyId:        ski[:],
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	s.udapRootCert = cert
	s.udapRootKey = priv
	s.udapRootPEM = pemBytes
	s.udapTrustAnchors = append(s.udapTrustAnchors, cert)
	return cert, priv, pemBytes, nil
}

// RegisterUDAPTrustAnchor registers a trusted X.509 CA certificate for HL7 UDAP client registration and authentication.
func (s *Server) RegisterUDAPTrustAnchor(cert *x509.Certificate) {
	if cert == nil {
		return
	}
	_, _, _, _ = s.ensureUDAPRootCA()
	s.udapMu.Lock()
	defer s.udapMu.Unlock()
	for _, existing := range s.udapTrustAnchors {
		if existing.Equal(cert) {
			return
		}
	}
	s.udapTrustAnchors = append(s.udapTrustAnchors, cert)
}

// RegisterUDAPTrustAnchorPEM parses one or more PEM-encoded X.509 certificates and registers them as UDAP trust anchors.
func (s *Server) RegisterUDAPTrustAnchorPEM(pemBytes []byte) error {
	rest := pemBytes
	parsedAny := false
	for len(rest) > 0 {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return err
		}
		s.RegisterUDAPTrustAnchor(cert)
		parsedAny = true
	}
	if !parsedAny {
		return errors.New("no valid CERTIFICATE PEM block found")
	}
	return nil
}

func (s *Server) udapTrustCertPool() *x509.CertPool {
	_, _, _, _ = s.ensureUDAPRootCA()
	s.udapMu.RLock()
	defer s.udapMu.RUnlock()
	pool := x509.NewCertPool()
	for _, c := range s.udapTrustAnchors {
		pool.AddCert(c)
	}
	return pool
}

func (s *Server) getOrCreateUDAPServerLeaf(issuerURL, crlURL string) (udapServerLeaf, error) {
	rootCert, rootKey, _, err := s.ensureUDAPRootCA()
	if err != nil {
		return udapServerLeaf{}, err
	}
	cacheKey := issuerURL + "|" + crlURL
	s.udapMu.RLock()
	if cached, ok := s.udapServerCerts[cacheKey]; ok {
		s.udapMu.RUnlock()
		return cached, nil
	}
	s.udapMu.RUnlock()

	parsedIss, err := url.Parse(issuerURL)
	if err != nil {
		return udapServerLeaf{}, err
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return udapServerLeaf{}, err
	}
	leafPubDER, err := x509.MarshalPKIXPublicKey(&leafKey.PublicKey)
	if err != nil {
		return udapServerLeaf{}, err
	}
	leafSKI := sha1.Sum(leafPubDER)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return udapServerLeaf{}, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Country:      []string{"US"},
			Organization: []string{"Flint FHIR Server"},
			CommonName:   parsedIss.Host,
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.AddDate(2, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		SubjectKeyId:          leafSKI[:],
		AuthorityKeyId:        rootCert.SubjectKeyId,
		URIs:                  []*url.URL{parsedIss},
	}
	if crlURL != "" {
		tmpl.CRLDistributionPoints = []string{crlURL}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, tmpl, rootCert, &leafKey.PublicKey, rootKey)
	if err != nil {
		return udapServerLeaf{}, err
	}
	entry := udapServerLeaf{
		priv: leafKey,
		x5cB64: []string{
			base64.StdEncoding.EncodeToString(leafDER),
			base64.StdEncoding.EncodeToString(rootCert.Raw),
		},
	}
	s.udapMu.Lock()
	if s.udapServerCerts == nil {
		s.udapServerCerts = make(map[string]udapServerLeaf)
	}
	s.udapServerCerts[cacheKey] = entry
	s.udapMu.Unlock()
	return entry, nil
}

// handleUDAPRootCA serves Flint's UDAP Trust Anchor Root CA PEM at GET /oauth2/udap-ca.pem.
func (s *Server) handleUDAPRootCA(w http.ResponseWriter, r *http.Request) {
	_, _, pemBytes, err := s.ensureUDAPRootCA()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(pemBytes)
	}
}

// handleUDAPCRL serves a signed DER X.509 CRL at GET /oauth2/udap.crl for UDAP trust chain verification.
func (s *Server) handleUDAPCRL(w http.ResponseWriter, r *http.Request) {
	rootCert, rootKey, _, err := s.ensureUDAPRootCA()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	now := time.Now().UTC()
	crlTmpl := &x509.RevocationList{
		Number:     big.NewInt(1),
		ThisUpdate: now.Add(-5 * time.Minute),
		NextUpdate: now.Add(7 * 24 * time.Hour),
	}
	crlDER, err := x509.CreateRevocationList(rand.Reader, crlTmpl, rootCert, rootKey)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/pkix-crl")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(crlDER)
	}
}

// handleUDAPTrustAnchorRegister registers a PEM trust anchor at POST /oauth2/udap-trust-anchor.
func (s *Server) handleUDAPTrustAnchorRegister(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	var req struct {
		PEM     string `json:"pem"`
		CertPEM string `json:"cert_pem"`
	}
	pemText := ""
	if err := json.Unmarshal(bodyBytes, &req); err == nil {
		pemText = strings.TrimSpace(req.PEM)
		if pemText == "" {
			pemText = strings.TrimSpace(req.CertPEM)
		}
	} else if strings.Contains(string(bodyBytes), "-----BEGIN CERTIFICATE-----") {
		pemText = strings.TrimSpace(string(bodyBytes))
	}
	if pemText == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "expected JSON object with non-empty 'pem' or 'cert_pem' field")
		return
	}
	if err := s.RegisterUDAPTrustAnchorPEM([]byte(pemText)); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid X.509 PEM certificate: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
}

func (s *Server) buildUDAPDiscoveryMetadata(r *http.Request) (map[string]any, error) {
	baseURL := requestBaseURL(r)
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	rawTenant := strings.TrimSpace(r.PathValue("tenant"))
	issuerURL := baseURL
	if rawTenant != "" {
		issuerURL = baseURL + "/fhir/r4/" + rawTenant
	}

	authEndpoint := baseURL + "/oauth2/authorize"
	tokEndpoint := baseURL + "/oauth2/token"
	regEndpoint := baseURL + "/oauth2/register"
	crlEndpoint := baseURL + "/oauth2/udap.crl"

	leaf, err := s.getOrCreateUDAPServerLeaf(issuerURL, crlEndpoint)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	signedPayload := map[string]any{
		"iss":                    issuerURL,
		"sub":                    issuerURL,
		"iat":                    now.Unix(),
		"exp":                    now.Add(24 * time.Hour).Unix(),
		"jti":                    uuid.NewString(),
		"authorization_endpoint": authEndpoint,
		"token_endpoint":         tokEndpoint,
		"registration_endpoint":  regEndpoint,
	}
	signedMetaJWT, err := auth.SignRS256WithX5C(signedPayload, leaf.priv, leaf.x5cB64)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"udap_versions_supported":                 []string{"1"},
		"udap_profiles_supported":                 []string{"udap_dcr", "udap_authn", "udap_authz", "udap_to"},
		"udap_authorization_extensions_supported": []string{"hl7-b2b", "hl7-b2b-user"},
		"udap_authorization_extensions_required":  []string{},
		"udap_certifications_supported": []string{
			"http://hl7.org/fhir/us/udap-security/ImplementationGuide/hl7.fhir.us.udap-security",
		},
		"udap_certifications_required": []string{},
		"grant_types_supported":        []string{"authorization_code", "refresh_token", "client_credentials"},
		"scopes_supported": []string{
			"openid", "fhirUser", "udap", "launch", "launch/patient",
			"patient/*.rs", "user/*.cruds", "system/*.rs", "system/*.cruds",
		},
		"authorization_endpoint": authEndpoint,
		"token_endpoint":         tokEndpoint,
		"token_endpoint_auth_methods_supported": []string{
			"private_key_jwt",
		},
		"token_endpoint_auth_signing_alg_values_supported": []string{
			"RS256", "RS384", "ES256", "ES384",
		},
		"registration_endpoint": regEndpoint,
		"registration_endpoint_jwt_signing_alg_values_supported": []string{
			"RS256", "RS384", "ES256", "ES384",
		},
		"signed_metadata": signedMetaJWT,
		"x5c":             leaf.x5cB64,
	}, nil
}

func parseX5CChain(x5c []string) ([]*x509.Certificate, error) {
	if len(x5c) == 0 {
		return nil, errors.New("missing required x5c certificate chain in JOSE header")
	}
	certs := make([]*x509.Certificate, 0, len(x5c))
	for i, rawB64 := range x5c {
		der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rawB64))
		if err != nil {
			der, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(rawB64), "="))
			if err != nil {
				return nil, fmt.Errorf("invalid base64 in x5c[%d]: %w", i, err)
			}
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("invalid X.509 DER in x5c[%d]: %w", i, err)
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

var oidExtensionSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}

func certMatchesURISAN(cert *x509.Certificate, wantURI string) bool {
	if cert == nil || wantURI == "" {
		return false
	}
	for _, u := range cert.URIs {
		if u != nil && u.String() == wantURI {
			return true
		}
	}
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(oidExtensionSubjectAltName) {
			continue
		}
		var seq asn1.RawValue
		if _, err := asn1.Unmarshal(ext.Value, &seq); err != nil {
			continue
		}
		rest := seq.Bytes
		for len(rest) > 0 {
			var v asn1.RawValue
			var uErr error
			rest, uErr = asn1.Unmarshal(rest, &v)
			if uErr != nil {
				break
			}
			// GeneralName [6] IA5String = uniformResourceIdentifier
			if v.Tag == 6 && string(v.Bytes) == wantURI {
				return true
			}
		}
	}
	return false
}

func (s *Server) verifyUDAPCertChain(certs []*x509.Certificate) error {
	if len(certs) == 0 {
		return errors.New("empty certificate chain")
	}
	interPool := x509.NewCertPool()
	for _, ic := range certs[1:] {
		interPool.AddCert(ic)
	}
	opts := x509.VerifyOptions{
		Roots:         s.udapTrustCertPool(),
		Intermediates: interPool,
		CurrentTime:   time.Now().UTC(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}
	_, err := certs[0].Verify(opts)
	return err
}

func extractStringSliceClaim(m map[string]any, key string) ([]string, bool) {
	raw, exists := m[key]
	if !exists {
		return nil, false
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, true
}

func (s *Server) lookupUDAPClient(clientID string) (udapClientEntry, bool) {
	if clientID == "" {
		return udapClientEntry{}, false
	}
	s.udapMu.RLock()
	defer s.udapMu.RUnlock()
	c, ok := s.udapClients[clientID]
	return c, ok
}

func (s *Server) handleUDAPClientRegistration(w http.ResponseWriter, r *http.Request, rawMap map[string]any) {
	udapVer, _ := rawMap["udap"].(string)
	if strings.TrimSpace(udapVer) != "1" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "UDAP dynamic client registration requires udap=\"1\"")
		return
	}
	rawSS, _ := rawMap["software_statement"].(string)
	rawSS = strings.TrimSpace(rawSS)
	if rawSS == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "missing required software_statement JWT")
		return
	}

	parsed, err := auth.ParseJWT(rawSS)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", "malformed software_statement JWT: "+err.Error())
		return
	}
	switch parsed.Header.Alg {
	case "RS256", "RS384", "ES256", "ES384":
	default:
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", "unsupported software_statement alg: "+parsed.Header.Alg)
		return
	}

	certs, err := parseX5CChain(parsed.Header.X5c)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", err.Error())
		return
	}
	leafCert := certs[0]
	if err := parsed.VerifySignature(leafCert.PublicKey); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", "software_statement signature verification failed: "+err.Error())
		return
	}
	if err := s.verifyUDAPCertChain(certs); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "unapproved_software_statement", "untrusted X.509 certificate chain in software_statement: "+err.Error())
		return
	}

	iss := strings.TrimSpace(parsed.Payload.Issuer)
	sub := strings.TrimSpace(parsed.Payload.Subject)
	if iss == "" || sub == "" || iss != sub {
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", "software_statement iss and sub must be non-empty and equal")
		return
	}
	if !certMatchesURISAN(leafCert, iss) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", "software_statement iss does not match any URI SAN in x5c leaf certificate")
		return
	}

	cfg := s.resolveSMARTConfig(r)
	audOK := false
	for _, aud := range parsed.Payload.Audience {
		ta := strings.TrimRight(strings.TrimSpace(aud), "/")
		if ta != "" && (ta == strings.TrimRight(cfg.RegistrationEndpoint, "/") || strings.HasSuffix(ta, "/oauth2/register")) {
			audOK = true
			break
		}
	}
	if !audOK {
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", "software_statement aud must match registration_endpoint")
		return
	}

	nowUnix := time.Now().UTC().Unix()
	if parsed.Payload.IssuedAt <= 0 || parsed.Payload.IssuedAt > nowUnix+60 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", "invalid software_statement iat claim")
		return
	}
	if parsed.Payload.ExpiresAt <= nowUnix || parsed.Payload.ExpiresAt <= parsed.Payload.IssuedAt || parsed.Payload.ExpiresAt-parsed.Payload.IssuedAt > 300 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", "software_statement exp must be in the future and at most 5 minutes after iat")
		return
	}
	jti := strings.TrimSpace(parsed.Payload.JTI)
	if jti == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", "missing required jti claim in software_statement")
		return
	}
	jtiKey := "udap-dcr-jti:" + iss + ":" + jti
	if s.isTokenRevoked(jtiKey) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_software_statement", "replayed software_statement jti")
		return
	}
	s.revokeTokenInternal(r.Context(), jtiKey, 10*time.Minute)

	extra := parsed.Payload.Extra
	grantTypes, hasGrantTypes := extractStringSliceClaim(extra, "grant_types")
	if !hasGrantTypes {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "missing required grant_types array in software_statement")
		return
	}

	// UDAP IG §3.4 registration cancellation: empty grant_types [] for an existing iss
	if len(grantTypes) == 0 {
		s.udapMu.Lock()
		existingID, hadExisting := s.udapClientsByISS[iss]
		if hadExisting {
			delete(s.udapClientsByISS, iss)
			delete(s.udapClients, existingID)
		}
		s.udapMu.Unlock()
		if !hadExisting {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "grant_types must not be empty for new client registration")
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":          existingID,
			"software_statement": rawSS,
			"grant_types":        []string{},
		})
		return
	}

	hasAuthCode := slices.Contains(grantTypes, "authorization_code")
	hasClientCreds := slices.Contains(grantTypes, "client_credentials")
	hasRefresh := slices.Contains(grantTypes, "refresh_token")
	for _, gt := range grantTypes {
		if gt != "authorization_code" && gt != "client_credentials" && gt != "refresh_token" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant_type: "+gt)
			return
		}
	}
	if hasAuthCode && hasClientCreds {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "UDAP client cannot register both authorization_code and client_credentials for the same iss")
		return
	}
	if hasRefresh && !hasAuthCode {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "refresh_token requires authorization_code grant_type")
		return
	}

	clientName, _ := extra["client_name"].(string)
	clientName = strings.TrimSpace(clientName)
	authMethod, _ := extra["token_endpoint_auth_method"].(string)
	authMethod = strings.TrimSpace(authMethod)
	scope, _ := extra["scope"].(string)
	scope = strings.TrimSpace(scope)
	if clientName == "" || authMethod != "private_key_jwt" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "client_name is required and token_endpoint_auth_method must be private_key_jwt")
		return
	}

	redirectURIs, hasRedirectURIs := extractStringSliceClaim(extra, "redirect_uris")
	responseTypes, hasResponseTypes := extractStringSliceClaim(extra, "response_types")
	contacts, hasContacts := extractStringSliceClaim(extra, "contacts")
	logoURI, _ := extra["logo_uri"].(string)
	logoURI = strings.TrimSpace(logoURI)

	if hasAuthCode {
		if !hasRedirectURIs || len(redirectURIs) == 0 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "authorization_code registration requires non-empty redirect_uris")
			return
		}
		for _, ru := range redirectURIs {
			u, uErr := url.Parse(ru)
			if uErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid URI in redirect_uris: "+ru)
				return
			}
		}
		if !hasResponseTypes || !slices.Contains(responseTypes, "code") {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "authorization_code registration requires response_types containing \"code\"")
			return
		}
	}
	if hasClientCreds && (hasRedirectURIs || hasResponseTypes) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "client_credentials registration must not include redirect_uris or response_types")
		return
	}

	s.udapMu.Lock()
	if s.udapClients == nil {
		s.udapClients = make(map[string]udapClientEntry)
	}
	if s.udapClientsByISS == nil {
		s.udapClientsByISS = make(map[string]string)
	}
	clientID, isUpdate := s.udapClientsByISS[iss]
	if !isUpdate || clientID == "" {
		clientID = "udap-client-" + uuid.NewString()
		s.udapClientsByISS[iss] = clientID
	}
	entry := udapClientEntry{
		ClientID:                clientID,
		CertISS:                 iss,
		ClientName:              clientName,
		GrantTypes:              grantTypes,
		ResponseTypes:           responseTypes,
		RedirectURIs:            redirectURIs,
		TokenEndpointAuthMethod: authMethod,
		Scope:                   scope,
		Contacts:                contacts,
		LogoURI:                 logoURI,
	}
	s.udapClients[clientID] = entry
	s.udapMu.Unlock()

	resp := map[string]any{
		"client_id":                  clientID,
		"software_statement":         rawSS,
		"client_name":                clientName,
		"grant_types":                grantTypes,
		"token_endpoint_auth_method": authMethod,
		"scope":                      scope,
	}
	if hasRedirectURIs {
		resp["redirect_uris"] = redirectURIs
	}
	if hasResponseTypes {
		resp["response_types"] = responseTypes
	}
	if hasContacts {
		resp["contacts"] = contacts
	}
	if logoURI != "" {
		resp["logo_uri"] = logoURI
	}

	status := http.StatusCreated
	if isUpdate {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) authenticateUDAPClientAssertion(w http.ResponseWriter, r *http.Request, parsed *auth.ParsedToken, iss, jti string) (string, bool) {
	if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Authorization header must not be sent with UDAP private_key_jwt client_assertion")
		return "", false
	}
	if _, hasSec := r.Form["client_secret"]; hasSec {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_secret must not be sent with UDAP private_key_jwt client_assertion")
		return "", false
	}
	udapCli, ok := s.lookupUDAPClient(iss)
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", "unknown UDAP client_id in client_assertion iss")
		return "", false
	}
	certs, err := parseX5CChain(parsed.Header.X5c)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", "invalid x5c in UDAP client_assertion: "+err.Error())
		return "", false
	}
	if err := parsed.VerifySignature(certs[0].PublicKey); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", "UDAP client_assertion signature verification failed: "+err.Error())
		return "", false
	}
	if err := s.verifyUDAPCertChain(certs); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", "UDAP client_assertion certificate chain untrusted: "+err.Error())
		return "", false
	}
	if !certMatchesURISAN(certs[0], udapCli.CertISS) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", "UDAP client_assertion x5c certificate SAN does not match registered client URI")
		return "", false
	}
	if parsed.Payload.IssuedAt <= 0 || parsed.Payload.ExpiresAt-parsed.Payload.IssuedAt > 300 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", "UDAP client_assertion requires valid iat and exp <= iat + 300s")
		return "", false
	}
	if exts, hasExts := parsed.Payload.Extra["extensions"].(map[string]any); hasExts {
		if b2b, hasB2B := exts["hl7-b2b"].(map[string]any); hasB2B {
			ver, _ := b2b["version"].(string)
			orgID, _ := b2b["organization_id"].(string)
			pou, _ := b2b["purpose_of_use"].([]any)
			if ver != "1" || strings.TrimSpace(orgID) == "" || len(pou) == 0 {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client", "invalid hl7-b2b authorization extension object")
				return "", false
			}
		}
	}

	jtiKey := "jti:" + jti
	if s.isTokenRevoked(jtiKey) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_assertion jti has already been used (replay detected)")
		return "", false
	}
	s.revokeTokenInternal(r.Context(), jtiKey, 10*time.Minute)
	return iss, true
}
