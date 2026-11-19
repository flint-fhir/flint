package server

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/telemetry"
	"github.com/flint-fhir/flint/store"
	"github.com/google/uuid"
)

const (
	maxCDSJKUCacheEntries  = 256
	cdsJKUCacheTTL         = 5 * time.Minute
	cdsJKUMinRefreshWindow = 2 * time.Second
	maxCDSJTIReplayEntries = 16384
	maxCDSJWTClockSkewSec  = 60
	maxCDSJWTLifetimeSec   = 3600
)

type cdsClientIssuerKeyType struct{}

var cdsClientIssuerKey = cdsClientIssuerKeyType{}

type cdsJKUCacheEntry struct {
	jwks      auth.JWKSet
	fetchedAt time.Time
	expiresAt time.Time
}

// CDSClientIssuerFromContext returns the verified CDS Hooks 2.0 client issuer (iss)
// attached to ctx when authenticated via a CDS Client JWS token.
func CDSClientIssuerFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(cdsClientIssuerKey).(string)
	return v, ok && v != ""
}

// SetCDSHooksRequireJWT configures whether all CDS Hooks 2.0 endpoints
// (discovery, service invocation, $dsi-attributes, and feedback) strictly require
// a cryptographically verified RFC 7515 CDS Client Bearer JWT.
func (s *Server) SetCDSHooksRequireJWT(require bool) {
	s.cdsHooksMu.Lock()
	defer s.cdsHooksMu.Unlock()
	s.requireCDSHooksJWT = require
}

// RegisterCDSClientJWKS registers a trusted RFC 7517 JWKSet for a CDS Hooks 2.0 client issuer (iss).
func (s *Server) RegisterCDSClientJWKS(issuer string, jwks auth.JWKSet) {
	iss := strings.TrimSpace(issuer)
	if iss == "" {
		return
	}
	if s.store != nil {
		if b, err := json.Marshal(jwks); err == nil {
			_, _ = s.store.PutClusterState(context.Background(), store.ClusterStateRecord{
				Namespace: "cds_client_jwks",
				Key:       iss,
				Value:     b,
			})
		}
	}
	s.cdsHooksMu.Lock()
	defer s.cdsHooksMu.Unlock()
	if s.cdsClientJWKS == nil {
		s.cdsClientJWKS = make(map[string]auth.JWKSet)
	}
	s.cdsClientJWKS[iss] = jwks
}

func (s *Server) lookupCDSClientJWKS(iss string) (auth.JWKSet, bool) {
	s.cdsHooksMu.RLock()
	set, ok := s.cdsClientJWKS[iss]
	s.cdsHooksMu.RUnlock()
	if ok {
		return set, true
	}
	if s.store != nil {
		if rec, found, err := s.store.GetClusterState(context.Background(), "cds_client_jwks", iss); err == nil && found && rec != nil {
			var parsed auth.JWKSet
			if json.Unmarshal(rec.Value, &parsed) == nil {
				return parsed, true
			}
		}
	}
	return auth.JWKSet{}, false
}

// SetCDSHooksAllowedJKUPrefixes configures the allowlisted HTTPS (or loopback HTTP)
// URL prefixes permitted for dynamic JOSE 'jku' header JWKS resolution on CDS Hooks 2.0 requests.
func (s *Server) SetCDSHooksAllowedJKUPrefixes(prefixes []string) {
	var cleaned []string
	for _, p := range prefixes {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	s.cdsHooksMu.Lock()
	defer s.cdsHooksMu.Unlock()
	s.cdsAllowedJKUPrefixes = cleaned
}

// SetCDSHooksHTTPClient overrides the HTTP client used for allowlisted 'jku' JWKS fetches.
func (s *Server) SetCDSHooksHTTPClient(cli *http.Client) {
	s.cdsHooksMu.Lock()
	defer s.cdsHooksMu.Unlock()
	s.cdsHTTPClient = cli
}

func (s *Server) isCDSHooksJWTRequired() bool {
	s.cdsHooksMu.RLock()
	req := s.requireCDSHooksJWT
	s.cdsHooksMu.RUnlock()
	if req {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("FLINT_CDS_HOOKS_REQUIRE_JWT")), "true")
}

func (s *Server) hasCDSHooksJWTConfig() bool {
	s.cdsHooksMu.RLock()
	has := s.requireCDSHooksJWT || len(s.cdsClientJWKS) > 0 || len(s.cdsAllowedJKUPrefixes) > 0
	s.cdsHooksMu.RUnlock()
	if has {
		return true
	}
	if s.store != nil {
		if recs, err := s.store.ListClusterStates(context.Background(), "cds_client_jwks", ""); err == nil && len(recs) > 0 {
			return true
		}
	}
	return strings.TrimSpace(os.Getenv("FLINT_CDS_HOOKS_REQUIRE_JWT")) != "" ||
		strings.TrimSpace(os.Getenv("FLINT_CDS_HOOKS_JKU_ALLOWLIST")) != "" ||
		strings.TrimSpace(os.Getenv("FLINT_CDS_HOOKS_JWKS_JSON")) != ""
}

func isCDSHooksEndpoint(parts []string) bool {
	if len(parts) >= 1 && parts[0] == "cds-services" {
		return len(parts) <= 3
	}
	if len(parts) >= 3 && parts[0] == "fhir" && parts[1] == "r4" {
		if parts[2] == "cds-services" {
			return len(parts) <= 5
		}
		if len(parts) >= 4 && parts[3] == "cds-services" {
			return len(parts) <= 6
		}
	}
	return false
}

func resolveCDSHooksAuditTenant(r *http.Request, parts []string) string {
	if len(parts) >= 4 && parts[0] == "fhir" && parts[1] == "r4" && parts[2] != "cds-services" && parts[3] == "cds-services" {
		if t := strings.TrimSpace(parts[2]); t != "" && !strings.HasPrefix(t, "$") {
			return t
		}
	}
	if hdr := strings.TrimSpace(r.Header.Get("X-Tenant-ID")); hdr != "" {
		return hdr
	}
	return "default"
}

func sanitizeAuditIssuer(rawToken string) string {
	parsed, err := auth.ParseJWT(rawToken)
	if err != nil || parsed == nil {
		return "anonymous"
	}
	iss := strings.TrimSpace(parsed.Payload.Issuer)
	if iss == "" || len(iss) > 256 {
		return "anonymous"
	}
	return iss
}

func (s *Server) recordCDSHooksAuthFailure(w http.ResponseWriter, r *http.Request, parts []string, actorSubject, reason string) {
	if actorSubject == "" {
		actorSubject = "anonymous"
	}
	desc := "401 Unauthorized: " + reason
	if holder, ok := r.Context().Value(auditActorKey).(*auditActorHolder); ok && holder != nil {
		holder.actorSubject = actorSubject
		holder.outcomeDesc = desc
		holder.subtypeCode = "cds-hooks-auth"
	} else if s.auditRecorder != nil {
		tenantID := resolveCDSHooksAuditTenant(r, parts)
		serviceEntityID := ""
		if len(parts) >= 2 && parts[0] == "cds-services" {
			serviceEntityID = parts[1]
		} else if len(parts) >= 4 && parts[0] == "fhir" && parts[1] == "r4" && parts[2] == "cds-services" {
			serviceEntityID = parts[3]
		}
		rec := store.AuditRecord{
			TenantID:     tenantID,
			AuditID:      uuid.NewString(),
			Recorded:     time.Now().UTC(),
			Action:       audit.ActionExecute,
			SubtypeCode:  "cds-hooks-auth",
			Outcome:      audit.OutcomeMinorFailure,
			OutcomeDesc:  desc,
			HTTPMethod:   r.Method,
			HTTPStatus:   http.StatusUnauthorized,
			RequestURI:   r.URL.RequestURI(),
			AgentSubject: actorSubject,
			ClientIP:     extractClientIP(r),
			EntityType:   "CDSService",
			EntityID:     serviceEntityID,
		}
		if err := s.auditRecorder.Record(r.Context(), rec); err != nil && s.logger != nil {
			s.logger.Warn("failed to record CDS Hooks auth failure audit event", "error", err, "path", r.URL.Path)
		}
	}

	if strings.Contains(strings.ToLower(reason), "missing") {
		w.Header().Set("WWW-Authenticate", "Bearer")
	} else {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	}
	writeOperationOutcome(w, http.StatusUnauthorized, "login", reason)
}

// shouldVerifyAsCDSClientJWT determines whether rawToken on a CDS Hooks endpoint
// should be verified as a CDS Hooks 2.0 Client JWS rather than a SMART OAuth2 access token.
func (s *Server) shouldVerifyAsCDSClientJWT(rawToken string) bool {
	if s.isCDSHooksJWTRequired() {
		return true
	}
	parsed, err := auth.ParseJWT(rawToken)
	if err != nil {
		return s.hasCDSHooksJWTConfig()
	}
	if strings.EqualFold(strings.TrimSpace(parsed.Header.Alg), "none") {
		return true
	}
	if strings.TrimSpace(parsed.Header.Jku) != "" {
		return true
	}
	iss := strings.TrimSpace(parsed.Payload.Issuer)
	if iss != "" {
		s.cdsHooksMu.RLock()
		_, isRegisteredCDSIssuer := s.cdsClientJWKS[iss]
		s.cdsHooksMu.RUnlock()
		if isRegisteredCDSIssuer {
			return true
		}
	}
	// If the token carries SMART scopes and no CDS client JWK/JKU match, allow SMART validation to try.
	if strings.TrimSpace(parsed.Payload.Scope) != "" && !s.isCDSHooksJWTRequired() {
		return false
	}
	return s.hasCDSHooksJWTConfig()
}

// handleCDSHooksAuthMiddleware enforces CDS Hooks 2.0 JWS caller verification
// while preserving SMART OAuth2 bearer coexistence when strict CDS JWT mode is off.
func (s *Server) handleCDSHooksAuthMiddleware(w http.ResponseWriter, r *http.Request, parts []string, next http.Handler) (handled bool) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		if s.isCDSHooksJWTRequired() {
			s.recordCDSHooksAuthFailure(w, r, parts, "anonymous", "missing required CDS Hooks 2.0 Authorization Bearer JWT")
			return true
		}
		if isPublicEndpoint(parts) {
			next.ServeHTTP(w, r)
			return true
		}
		return false
	}

	if len(authHeader) <= 7 || !strings.EqualFold(authHeader[:7], "bearer ") {
		s.recordCDSHooksAuthFailure(w, r, parts, "anonymous", "malformed Authorization header on CDS Hooks endpoint; expected Bearer JWT")
		return true
	}
	rawToken := strings.TrimSpace(authHeader[7:])
	if rawToken == "" {
		s.recordCDSHooksAuthFailure(w, r, parts, "anonymous", "empty Bearer token on CDS Hooks endpoint")
		return true
	}

	if s.shouldVerifyAsCDSClientJWT(rawToken) {
		issuer, err := s.verifyCDSClientJWT(r, rawToken)
		if err != nil {
			s.recordCDSHooksAuthFailure(w, r, parts, sanitizeAuditIssuer(rawToken), err.Error())
			return true
		}
		if holder, ok := r.Context().Value(auditActorKey).(*auditActorHolder); ok && holder != nil {
			holder.actorSubject = issuer
		}
		ctx := context.WithValue(r.Context(), cdsClientIssuerKey, issuer)
		next.ServeHTTP(w, r.WithContext(ctx))
		return true
	}

	// Non-CDS token on a public discovery route (e.g. GET /cds-services): validate SMART token if supplied,
	// rather than silently ignoring an invalid Authorization header.
	if isPublicEndpoint(parts) {
		validator := s.tokenValidator
		if validator == nil {
			validator = s.devAuth
		}
		if s.isTokenRevoked(rawToken) {
			s.recordCDSHooksAuthFailure(w, r, parts, sanitizeAuditIssuer(rawToken), "token has been revoked")
			return true
		}
		secCtx, err := validator.ValidateToken(r.Context(), rawToken)
		if err != nil {
			s.recordCDSHooksAuthFailure(w, r, parts, sanitizeAuditIssuer(rawToken), err.Error())
			return true
		}
		ctx := auth.WithSecurityContext(r.Context(), secCtx)
		next.ServeHTTP(w, r.WithContext(ctx))
		return true
	}

	return false
}

// verifyCDSClientJWT validates an HL7 CDS Hooks 2.0 Client JWT according to
// "Trusting CDS Clients" (RFC 7515 / RFC 7519).
func (s *Server) verifyCDSClientJWT(r *http.Request, rawToken string) (string, error) {
	parsed, err := auth.ParseJWT(rawToken)
	if err != nil {
		return "", fmt.Errorf("invalid CDS Hooks JWT: %w", err)
	}

	if typ := strings.TrimSpace(parsed.Header.Typ); typ != "" && !strings.EqualFold(typ, "JWT") {
		return "", fmt.Errorf("unsupported CDS Hooks JWT typ %q", typ)
	}

	switch parsed.Header.Alg {
	case "ES256", "ES384", "RS256", "RS384":
	default:
		return "", fmt.Errorf("%w: CDS Hooks 2.0 requires ES256, ES384, RS256, or RS384 (got %q)", auth.ErrUnsupportedAlg, parsed.Header.Alg)
	}

	if parsed.Header.Jwk != nil {
		return "", errors.New("untrusted inline JOSE 'jwk' header is not permitted in CDS Hooks 2.0 JWT")
	}

	now := time.Now().UTC()
	iss := strings.TrimSpace(parsed.Payload.Issuer)
	if iss == "" {
		return "", fmt.Errorf("%w: missing required 'iss' claim", auth.ErrInvalidIssuer)
	}

	jti := strings.TrimSpace(parsed.Payload.JTI)
	if jti == "" {
		return "", errors.New("missing required 'jti' nonce claim in CDS Hooks 2.0 JWT")
	}

	if parsed.Payload.ExpiresAt <= 0 || now.Unix() >= parsed.Payload.ExpiresAt {
		return "", auth.ErrTokenExpired
	}

	if parsed.Payload.IssuedAt <= 0 {
		return "", errors.New("missing required 'iat' claim in CDS Hooks 2.0 JWT")
	}
	if parsed.Payload.IssuedAt > now.Unix()+maxCDSJWTClockSkewSec {
		return "", fmt.Errorf("%w: 'iat' is in the future", auth.ErrTokenNotYetValid)
	}
	if now.Unix()-parsed.Payload.IssuedAt > maxCDSJWTLifetimeSec {
		return "", fmt.Errorf("%w: 'iat' exceeds maximum CDS Hooks JWT age", auth.ErrTokenExpired)
	}
	if parsed.Payload.ExpiresAt-parsed.Payload.IssuedAt > maxCDSJWTLifetimeSec {
		return "", errors.New("CDS Hooks 2.0 JWT lifetime (exp - iat) exceeds 3600 seconds")
	}
	if parsed.Payload.NotBefore != 0 && now.Unix()+5 < parsed.Payload.NotBefore {
		return "", auth.ErrTokenNotYetValid
	}

	if !matchesCDSAudience(r, parsed.Payload.Audience) {
		return "", fmt.Errorf("%w: 'aud' does not match CDS service endpoint %q", auth.ErrInvalidAudience, requestBaseURL(r)+r.URL.Path)
	}

	pubKey, err := s.resolveCDSClientSigningKey(r.Context(), parsed, now)
	if err != nil {
		return "", err
	}

	if err := parsed.VerifySignature(pubKey); err != nil {
		return "", err
	}

	if !s.consumeCDSJTIOnce(iss, jti, time.Unix(parsed.Payload.ExpiresAt, 0).Add(maxCDSJWTClockSkewSec*time.Second), now) {
		return "", errors.New("replayed CDS Hooks 2.0 JWT 'jti' detected")
	}

	return iss, nil
}

func matchesCDSAudience(r *http.Request, aud auth.Audience) bool {
	if r == nil || len(aud) == 0 {
		return false
	}
	candidates := []string{
		strings.TrimRight(requestBaseURL(r)+r.URL.Path, "/"),
	}
	if xfh := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); xfh != "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			scheme = "https"
		}
		candidates = append(candidates, strings.TrimRight(scheme+"://"+xfh+r.URL.Path, "/"))
	}
	for _, got := range aud {
		gotNorm := strings.TrimRight(strings.TrimSpace(got), "/")
		if gotNorm == "" {
			continue
		}
		for _, want := range candidates {
			if gotNorm == want {
				return true
			}
		}
	}
	return false
}

func (s *Server) resolveCDSClientSigningKey(ctx context.Context, parsed *auth.ParsedToken, now time.Time) (any, error) {
	iss := strings.TrimSpace(parsed.Payload.Issuer)
	jku := strings.TrimSpace(parsed.Header.Jku)

	var jwks auth.JWKSet
	if jku != "" {
		if !s.isCDSJKUAllowed(jku) {
			return nil, fmt.Errorf("untrusted CDS Hooks 'jku' URL %q is not in allowlist", jku)
		}
		fetched, err := s.fetchCDSJKU(ctx, jku, parsed.Header.Kid, now)
		if err != nil {
			return nil, err
		}
		jwks = fetched
	} else {
		staticSet, ok := s.lookupCDSClientJWKS(iss)
		if !ok {
			return nil, fmt.Errorf("%w: untrusted CDS client issuer %q", auth.ErrInvalidIssuer, iss)
		}
		jwks = staticSet
	}

	jwk, err := jwks.FindKey(parsed.Header.Kid)
	if err != nil {
		return nil, fmt.Errorf("%w (kid=%q)", err, parsed.Header.Kid)
	}
	if jwk.Alg != "" && jwk.Alg != parsed.Header.Alg {
		return nil, fmt.Errorf("%w: JWK alg %q does not match JWT header alg %q", auth.ErrUnsupportedAlg, jwk.Alg, parsed.Header.Alg)
	}
	pubKey, err := jwk.PublicKey()
	if err != nil {
		return nil, err
	}
	if parsed.Header.Alg == "RS256" || parsed.Header.Alg == "RS384" {
		rsaPub, ok := pubKey.(*rsa.PublicKey)
		if !ok || rsaPub.N.BitLen() < 2048 {
			return nil, fmt.Errorf("%w: %s CDS Hooks key must be >= 2048 bits", auth.ErrUnsupportedKeyType, parsed.Header.Alg)
		}
	}
	return pubKey, nil
}

func (s *Server) isCDSJKUAllowed(jkuURL string) bool {
	s.cdsHooksMu.RLock()
	prefixes := append([]string(nil), s.cdsAllowedJKUPrefixes...)
	s.cdsHooksMu.RUnlock()
	if rawEnv := strings.TrimSpace(os.Getenv("FLINT_CDS_HOOKS_JKU_ALLOWLIST")); rawEnv != "" {
		for p := range strings.SplitSeq(rawEnv, ",") {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				prefixes = append(prefixes, trimmed)
			}
		}
	}
	for _, prefix := range prefixes {
		if matchesAllowedJKUPrefix(jkuURL, prefix) {
			return true
		}
	}
	return false
}

func matchesAllowedJKUPrefix(jkuURL, allowedPrefix string) bool {
	u, err := url.Parse(strings.TrimSpace(jkuURL))
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	p, err := url.Parse(strings.TrimSpace(allowedPrefix))
	if err != nil || p.Host == "" || p.User != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != strings.ToLower(p.Scheme) {
		return false
	}
	if scheme != "https" {
		if scheme != "http" || !isLoopbackHost(u.Hostname()) {
			return false
		}
	}
	if !strings.EqualFold(u.Host, p.Host) {
		return false
	}
	pPath := p.Path
	if pPath == "" || pPath == "/" {
		return true
	}
	pTrim := strings.TrimRight(pPath, "/")
	return u.Path == pPath || u.Path == pTrim || strings.HasPrefix(u.Path, pTrim+"/")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) fetchCDSJKU(ctx context.Context, jkuURL, kid string, now time.Time) (auth.JWKSet, error) {
	s.cdsHooksMu.RLock()
	cached, hasCached := s.cdsJKUCache[jkuURL]
	cli := s.cdsHTTPClient
	s.cdsHooksMu.RUnlock()

	if hasCached && now.Before(cached.expiresAt) {
		if _, err := cached.jwks.FindKey(kid); err == nil || now.Sub(cached.fetchedAt) < cdsJKUMinRefreshWindow {
			return cached.jwks, nil
		}
	}

	if cli == nil {
		cli = &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 2 {
					return errors.New("too many JKU redirects")
				}
				if !s.isCDSJKUAllowed(req.URL.String()) {
					return fmt.Errorf("JKU redirect target %q is not allowlisted", req.URL.String())
				}
				return nil
			},
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jkuURL, nil)
	if err != nil {
		return auth.JWKSet{}, fmt.Errorf("invalid JKU URL: %w", err)
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	telemetry.InjectHTTP(ctx, req.Header)

	resp, err := cli.Do(req)
	if err != nil {
		return auth.JWKSet{}, fmt.Errorf("fetch CDS client JKU %q: %w", jkuURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return auth.JWKSet{}, fmt.Errorf("CDS client JKU %q returned HTTP %d", jkuURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return auth.JWKSet{}, fmt.Errorf("read CDS client JKU response: %w", err)
	}

	var set auth.JWKSet
	if err := json.Unmarshal(body, &set); err != nil {
		return auth.JWKSet{}, fmt.Errorf("invalid JWKS JSON from JKU %q: %w", jkuURL, err)
	}
	if len(set.Keys) == 0 {
		return auth.JWKSet{}, fmt.Errorf("empty JWKS returned from JKU %q", jkuURL)
	}

	s.cdsHooksMu.Lock()
	if s.cdsJKUCache == nil {
		s.cdsJKUCache = make(map[string]cdsJKUCacheEntry)
	}
	for k, v := range s.cdsJKUCache {
		if now.After(v.expiresAt) {
			delete(s.cdsJKUCache, k)
		}
	}
	if len(s.cdsJKUCache) >= maxCDSJKUCacheEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range s.cdsJKUCache {
			if oldestKey == "" || v.fetchedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.fetchedAt
			}
		}
		if oldestKey != "" {
			delete(s.cdsJKUCache, oldestKey)
		}
	}
	s.cdsJKUCache[jkuURL] = cdsJKUCacheEntry{
		jwks:      set,
		fetchedAt: now,
		expiresAt: now.Add(cdsJKUCacheTTL),
	}
	s.cdsHooksMu.Unlock()

	return set, nil
}

func (s *Server) consumeCDSJTIOnce(issuer, jti string, expiresAt, now time.Time) bool {
	if s.store != nil {
		ttl := expiresAt.Sub(now)
		if ttl <= 0 {
			ttl = time.Minute
		}
		claimed, err := s.store.ClaimNonce(context.Background(), "cds-jti:"+issuer+":"+jti, ttl)
		return err == nil && claimed
	}
	key := issuer + "\x00" + jti
	s.cdsHooksMu.Lock()
	defer s.cdsHooksMu.Unlock()

	if s.cdsJTIReplay == nil {
		s.cdsJTIReplay = make(map[string]time.Time)
	}
	if existingExp, exists := s.cdsJTIReplay[key]; exists {
		if now.Before(existingExp) {
			return false
		}
		delete(s.cdsJTIReplay, key)
	}
	if len(s.cdsJTIReplay) >= maxCDSJTIReplayEntries {
		for k, exp := range s.cdsJTIReplay {
			if !now.Before(exp) {
				delete(s.cdsJTIReplay, k)
			}
		}
		if len(s.cdsJTIReplay) >= maxCDSJTIReplayEntries {
			var oldestKey string
			var oldestExp time.Time
			for k, exp := range s.cdsJTIReplay {
				if oldestKey == "" || exp.Before(oldestExp) {
					oldestKey = k
					oldestExp = exp
				}
			}
			if oldestKey != "" {
				delete(s.cdsJTIReplay, oldestKey)
			}
		}
	}
	s.cdsJTIReplay[key] = expiresAt
	return true
}
