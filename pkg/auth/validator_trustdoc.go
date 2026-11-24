package auth

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
)

const (
	// TrustClaimShapeGrantMap extracts per-tenant roles from a nested role->tenant map (e.g. Zitadel).
	TrustClaimShapeGrantMap = "grantMap"
	// TrustClaimShapeList extracts tenant IDs from a string/array claim with flat role claims.
	TrustClaimShapeList = "list"
	// TrustClaimShapeNone verifies issuer & signature without extracting tenant claims (e.g. workload tokens).
	TrustClaimShapeNone = "none"

	maxTrustDocBytes = 1 << 20
	minTrustRSABits  = 2048
)

var (
	// ErrTrustDocInvalid indicates malformed or incomplete trust document configuration.
	ErrTrustDocInvalid = errors.New("invalid identity trust document")
	// ErrClaimsShape indicates token claims violate the configured trust claim shape.
	ErrClaimsShape = errors.New("token claims do not match configured shape")
	// ErrPrincipalNotAllowed indicates a verified caller is not on the issuer's workload principal allowlist.
	ErrPrincipalNotAllowed = errors.New("caller principal not in allowlist")
)

// TrustDocument mirrors the multi-issuer platform identity trust schema (e.g. PLATFORM_IDENTITY_TRUST_FILE).
type TrustDocument struct {
	Issuers      []TrustIssuer     `json:"issuers"`
	Principals   []TrustPrincipal  `json:"principals,omitempty"`
	RoleToScopes map[string]string `json:"roleToScopes,omitempty"`
}

// TrustIssuer configures verification and claim extraction rules for one trusted OIDC/JWT issuer.
type TrustIssuer struct {
	Issuer            string            `json:"issuer"`
	Audiences         []string          `json:"audiences"`
	JWKSURL           string            `json:"jwksUrl"`
	CACertFile        string            `json:"caCertFile,omitempty"`
	JWKS              json.RawMessage   `json:"jwks"`
	SigningAlgorithms []string          `json:"signingAlgorithms"`
	Claims            TrustClaims       `json:"claims"`
	RoleToScopes      map[string]string `json:"roleToScopes,omitempty"`
	Principals        []TrustPrincipal  `json:"principals,omitempty"`
}

// TrustClaims defines how tenant memberships, roles, and delegated actor identity are read from JWT claims.
type TrustClaims struct {
	Shape                 string `json:"shape"`
	SubjectClaim          string `json:"subjectClaim,omitempty"`
	GrantsClaim           string `json:"grantsClaim"`
	HomeTenantClaim       string `json:"homeTenantClaim"`
	TenantsClaim          string `json:"tenantsClaim"`
	ActiveTenantClaim     string `json:"activeTenantClaim"`
	OperatorRole          string `json:"operatorRole"`
	PlatformTenant        string `json:"platformTenant"`
	ActorClaim            string `json:"actorClaim"`
	RolesClaim            string `json:"rolesClaim"`
	ActiveTenantSlugClaim string `json:"activeTenantSlugClaim"`
}

// TrustPrincipal authorizes a specific (issuer, subject) workload/service caller.
type TrustPrincipal struct {
	ID               string   `json:"id,omitempty"`
	Issuer           string   `json:"issuer"`
	Subject          string   `json:"subject"`
	Roles            []string `json:"roles,omitempty"`
	Scopes           string   `json:"scopes,omitempty"`
	AllowCrossTenant bool     `json:"allowCrossTenant,omitempty"`
}

// TrustDocConfig configures a generic multi-issuer TrustDocValidator.
type TrustDocConfig struct {
	Document               TrustDocument
	Principals             []TrustPrincipal
	RoleToScopes           map[string]string
	HTTPClient             *http.Client
	CacheTTL               time.Duration
	MinRefreshInterval     time.Duration
	AllowPlaintextHTTPJWKS bool
}

type trustIssuerRuntime struct {
	cfg               TrustIssuer
	oidc              *OIDCValidator
	roleToScopes      map[string]string
	enforcePrincipals bool
	principalsBySub   map[string]TrustPrincipal
}

// TrustDocValidator validates bearer tokens across multiple trusted issuers declared in a TrustDocument.
type TrustDocValidator struct {
	issuers map[string]*trustIssuerRuntime
}

// LoadTrustDocFile reads and parses a JSON trust document from disk (max 1 MiB).
func LoadTrustDocFile(path string) (TrustDocument, error) {
	if strings.TrimSpace(path) == "" {
		return TrustDocument{}, fmt.Errorf("%w: trust file path is empty", ErrTrustDocInvalid)
	}
	info, err := os.Stat(path)
	if err != nil {
		return TrustDocument{}, fmt.Errorf("stat trust file: %w", err)
	}
	if info.Size() > maxTrustDocBytes {
		return TrustDocument{}, fmt.Errorf("%w: file exceeds %d bytes", ErrTrustDocInvalid, maxTrustDocBytes)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- trusted configuration file path supplied by operator
	if err != nil {
		return TrustDocument{}, fmt.Errorf("read trust file: %w", err)
	}
	return ParseTrustDoc(raw)
}

// ParseTrustDoc decodes and validates a raw JSON trust document.
func ParseTrustDoc(raw []byte) (TrustDocument, error) {
	if len(raw) > maxTrustDocBytes {
		return TrustDocument{}, fmt.Errorf("%w: document exceeds %d bytes", ErrTrustDocInvalid, maxTrustDocBytes)
	}
	var doc TrustDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return TrustDocument{}, fmt.Errorf("%w: %w", ErrTrustDocInvalid, err)
	}
	return doc, nil
}

// NewTrustDocValidator builds a generic multi-issuer TokenValidator from a TrustDocument.
func NewTrustDocValidator(cfg TrustDocConfig) (*TrustDocValidator, error) {
	if len(cfg.Document.Issuers) == 0 {
		return nil, fmt.Errorf("%w: at least one issuer is required", ErrTrustDocInvalid)
	}

	issuers := make(map[string]*trustIssuerRuntime, len(cfg.Document.Issuers))
	for i, iss := range cfg.Document.Issuers {
		if err := validateHTTPURL(iss.Issuer); err != nil {
			return nil, fmt.Errorf("%w: issuer[%d].issuer: %w", ErrTrustDocInvalid, i, err)
		}
		if _, dup := issuers[iss.Issuer]; dup {
			return nil, fmt.Errorf("%w: duplicate issuer %q", ErrTrustDocInvalid, iss.Issuer)
		}
		if len(iss.Audiences) == 0 {
			return nil, fmt.Errorf("%w: issuer %q requires at least one audience", ErrTrustDocInvalid, iss.Issuer)
		}
		for _, aud := range iss.Audiences {
			if strings.TrimSpace(aud) == "" {
				return nil, fmt.Errorf("%w: issuer %q contains empty audience", ErrTrustDocInvalid, iss.Issuer)
			}
		}
		if err := iss.Claims.validate(); err != nil {
			return nil, fmt.Errorf("%w: issuer %q claims: %w", ErrTrustDocInvalid, iss.Issuer, err)
		}

		var staticJWKS *JWKSet
		hasInlineJWKS := len(iss.JWKS) > 0 && string(iss.JWKS) != "null"
		if hasInlineJWKS && iss.JWKSURL != "" {
			return nil, fmt.Errorf("%w: issuer %q cannot set both jwks and jwksUrl", ErrTrustDocInvalid, iss.Issuer)
		}
		if hasInlineJWKS {
			parsedSet, err := parseValidatedPublicJWKS(iss.JWKS)
			if err != nil {
				return nil, fmt.Errorf("%w: issuer %q jwks: %w", ErrTrustDocInvalid, iss.Issuer, err)
			}
			staticJWKS = parsedSet
		} else {
			if err := validateHTTPURL(iss.JWKSURL); err != nil {
				return nil, fmt.Errorf("%w: issuer %q jwksUrl: %w", ErrTrustDocInvalid, iss.Issuer, err)
			}
			if strings.HasPrefix(iss.JWKSURL, "http://") && !cfg.AllowPlaintextHTTPJWKS {
				return nil, fmt.Errorf("%w: issuer %q uses plaintext http jwksUrl without AllowPlaintextHTTPJWKS", ErrTrustDocInvalid, iss.Issuer)
			}
		}

		issuerHTTPClient := cfg.HTTPClient
		if iss.CACertFile != "" {
			caCertFile := strings.TrimSpace(iss.CACertFile)
			if caCertFile == "" {
				return nil, fmt.Errorf("%w: issuer %q caCertFile must not be whitespace-only", ErrTrustDocInvalid, iss.Issuer)
			}
			if hasInlineJWKS {
				return nil, fmt.Errorf("%w: issuer %q caCertFile requires jwksUrl and cannot be combined with inline jwks", ErrTrustDocInvalid, iss.Issuer)
			}
			if strings.HasPrefix(iss.JWKSURL, "http://") {
				return nil, fmt.Errorf("%w: issuer %q caCertFile requires an https jwksUrl", ErrTrustDocInvalid, iss.Issuer)
			}
			roots, err := loadIssuerCACertPool(caCertFile)
			if err != nil {
				return nil, err
			}
			issuerHTTPClient, err = httpClientWithRootCAs(cfg.HTTPClient, roots)
			if err != nil {
				return nil, err
			}
		}

		oidcVal, err := NewOIDCValidator(OIDCConfig{
			Issuer:             iss.Issuer,
			Audiences:          iss.Audiences,
			JWKSURL:            iss.JWKSURL,
			StaticJWKS:         staticJWKS,
			HTTPClient:         issuerHTTPClient,
			CacheTTL:           cfg.CacheTTL,
			MinRefreshInterval: cfg.MinRefreshInterval,
			SigningAlgorithms:  iss.SigningAlgorithms,
			MinRSABits:         minTrustRSABits,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: issuer %q: %w", ErrTrustDocInvalid, iss.Issuer, err)
		}

		mergedRoleToScopes := mergeStringMaps(cfg.RoleToScopes, cfg.Document.RoleToScopes, iss.RoleToScopes)

		rt := &trustIssuerRuntime{
			cfg:             iss,
			oidc:            oidcVal,
			roleToScopes:    mergedRoleToScopes,
			principalsBySub: make(map[string]TrustPrincipal),
		}
		for _, p := range iss.Principals {
			if p.Issuer == "" {
				p.Issuer = iss.Issuer
			}
			if err := addTrustPrincipal(rt, iss.Issuer, p); err != nil {
				return nil, err
			}
		}
		issuers[iss.Issuer] = rt
	}

	registerGlobalPrincipal := func(p TrustPrincipal) error {
		if strings.TrimSpace(p.Issuer) == "" || strings.TrimSpace(p.Subject) == "" {
			return fmt.Errorf("%w: principal must set both issuer and subject", ErrTrustDocInvalid)
		}
		rt, ok := issuers[p.Issuer]
		if !ok {
			return fmt.Errorf("%w: principal issuer %q is not listed in trust document", ErrTrustDocInvalid, p.Issuer)
		}
		return addTrustPrincipal(rt, p.Issuer, p)
	}
	for _, p := range cfg.Document.Principals {
		if err := registerGlobalPrincipal(p); err != nil {
			return nil, err
		}
	}
	for _, p := range cfg.Principals {
		if err := registerGlobalPrincipal(p); err != nil {
			return nil, err
		}
	}

	return &TrustDocValidator{issuers: issuers}, nil
}

func addTrustPrincipal(rt *trustIssuerRuntime, expectedIssuer string, p TrustPrincipal) error {
	if strings.TrimSpace(p.Issuer) != expectedIssuer {
		return fmt.Errorf("%w: principal issuer %q does not match %q", ErrTrustDocInvalid, p.Issuer, expectedIssuer)
	}
	if strings.TrimSpace(p.Subject) == "" {
		return fmt.Errorf("%w: principal for issuer %q requires a subject", ErrTrustDocInvalid, expectedIssuer)
	}
	rt.enforcePrincipals = true
	rt.principalsBySub[p.Subject] = p
	return nil
}

// ValidateToken verifies the JWT against its matching trusted issuer and maps claims into SecurityContext.
func (v *TrustDocValidator) ValidateToken(ctx context.Context, bearerToken string) (*authv1.SecurityContext, error) {
	unverified, err := ParseJWT(bearerToken)
	if err != nil {
		return nil, err
	}
	if unverified.Payload.Issuer == "" {
		return nil, fmt.Errorf("%w: missing iss claim", ErrInvalidIssuer)
	}
	rt, ok := v.issuers[unverified.Payload.Issuer]
	if !ok {
		return nil, fmt.Errorf("%w: untrusted issuer %q", ErrInvalidIssuer, unverified.Payload.Issuer)
	}

	token, secCtx, err := rt.oidc.validateJWT(ctx, bearerToken)
	if err != nil {
		return nil, err
	}
	if token.Payload.Subject == "" {
		return nil, fmt.Errorf("%w: sub claim is required", ErrClaimsShape)
	}

	secCtx.Issuer = token.Payload.Issuer
	secCtx.AuthScheme = "trustdoc"
	secCtx.TenantId = ""

	if rt.cfg.Claims.ActorClaim != "" {
		actorSub, actorIss, err := extractActorClaim(token.Payload.Extra, rt.cfg.Claims.ActorClaim)
		if err != nil {
			return nil, err
		}
		secCtx.ActorSubject = actorSub
		secCtx.ActorIssuer = actorIss
	}

	principalSubject := token.Payload.Subject
	if subjectClaim := strings.TrimSpace(rt.cfg.Claims.SubjectClaim); subjectClaim != "" && subjectClaim != "sub" {
		raw, ok := token.Payload.Extra[subjectClaim]
		if !ok {
			return nil, fmt.Errorf("%w: %s claim is required", ErrClaimsShape, subjectClaim)
		}
		strVal, ok := raw.(string)
		trimmed := strings.TrimSpace(strVal)
		if !ok || trimmed == "" {
			return nil, fmt.Errorf("%w: %s claim is required", ErrClaimsShape, subjectClaim)
		}
		principalSubject = trimmed
		secCtx.PrincipalId = principalSubject
	}

	baseScopes := cloneScopes(secCtx.Scopes)
	var principalRule TrustPrincipal
	hasPrincipalRule := false
	if rt.enforcePrincipals {
		matched, allowed := rt.principalsBySub[principalSubject]
		if !allowed {
			return nil, fmt.Errorf("%w: %s (%s)", ErrPrincipalNotAllowed, principalSubject, token.Payload.Issuer)
		}
		principalRule = matched
		hasPrincipalRule = true
		if matched.ID != "" {
			secCtx.PrincipalId = matched.ID
		} else {
			secCtx.PrincipalId = principalSubject
		}
		if matched.AllowCrossTenant {
			secCtx.OperatorCrossTenant = true
		}
		baseScopes = mergeSmartScopes(
			baseScopes,
			ParseScopes(matched.Scopes),
			mapRolesToScopes(matched.Roles, rt.roleToScopes),
		)
		secCtx.Roles = slices.Clone(matched.Roles)
	}

	switch rt.cfg.Claims.Shape {
	case TrustClaimShapeGrantMap:
		if err := applyGrantMapClaims(token.Payload.Extra, rt.cfg.Claims, rt.roleToScopes, baseScopes, secCtx); err != nil {
			return nil, err
		}
	case TrustClaimShapeList:
		if err := applyListClaims(token.Payload.Extra, rt.cfg.Claims, rt.roleToScopes, baseScopes, secCtx); err != nil {
			return nil, err
		}
	case TrustClaimShapeNone:
		secCtx.Scopes = baseScopes
	}

	if hasPrincipalRule && len(principalRule.Roles) > 0 && len(secCtx.Roles) == 0 {
		secCtx.Roles = slices.Clone(principalRule.Roles)
	}

	return secCtx, nil
}

func (c TrustClaims) validate() error {
	switch c.Shape {
	case TrustClaimShapeGrantMap:
		if c.GrantsClaim == "" {
			return fmt.Errorf("grantsClaim is required for shape %q", TrustClaimShapeGrantMap)
		}
		if c.TenantsClaim != "" || c.RolesClaim != "" || c.ActiveTenantSlugClaim != "" {
			return fmt.Errorf("tenantsClaim, rolesClaim, and activeTenantSlugClaim are invalid for shape %q", TrustClaimShapeGrantMap)
		}
	case TrustClaimShapeList:
		if c.TenantsClaim == "" {
			return fmt.Errorf("tenantsClaim is required for shape %q", TrustClaimShapeList)
		}
		if c.GrantsClaim != "" || c.HomeTenantClaim != "" || c.OperatorRole != "" || c.PlatformTenant != "" {
			return fmt.Errorf("grantsClaim, homeTenantClaim, operatorRole, and platformTenant are invalid for shape %q", TrustClaimShapeList)
		}
	case TrustClaimShapeNone:
		if c.GrantsClaim != "" || c.HomeTenantClaim != "" || c.TenantsClaim != "" ||
			c.ActiveTenantClaim != "" || c.OperatorRole != "" || c.PlatformTenant != "" ||
			c.RolesClaim != "" || c.ActiveTenantSlugClaim != "" {
			return fmt.Errorf("tenant claims are invalid for shape %q", TrustClaimShapeNone)
		}
	case "":
		return fmt.Errorf("shape is required (one of grantMap, list, none)")
	default:
		return fmt.Errorf("unsupported claim shape %q", c.Shape)
	}
	if c.SubjectClaim != "" && strings.TrimSpace(c.SubjectClaim) == "" {
		return errors.New("subjectClaim must not be whitespace-only")
	}
	return nil
}

func loadIssuerCACertPool(path string) (*x509.CertPool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: stat caCertFile %q: %w", ErrTrustDocInvalid, path, err)
	}
	if info.Size() > maxTrustDocBytes {
		return nil, fmt.Errorf("%w: caCertFile %q exceeds %d bytes", ErrTrustDocInvalid, path, maxTrustDocBytes)
	}
	pemBytes, err := os.ReadFile(path) // #nosec G304 -- trusted CA certificate path supplied by operator in identity trust document
	if err != nil {
		return nil, fmt.Errorf("%w: read caCertFile %q: %w", ErrTrustDocInvalid, path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("%w: caCertFile %q contains no valid PEM certificates", ErrTrustDocInvalid, path)
	}
	return pool, nil
}

func httpClientWithRootCAs(base *http.Client, roots *x509.CertPool) (*http.Client, error) {
	var client http.Client
	if base != nil {
		client = *base
	} else {
		client = http.Client{Timeout: 10 * time.Second}
	}
	var tr *http.Transport
	if client.Transport != nil {
		orig, ok := client.Transport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("%w: caCertFile requires *http.Transport", ErrTrustDocInvalid)
		}
		tr = orig.Clone()
	} else {
		def, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("%w: caCertFile requires *http.Transport", ErrTrustDocInvalid)
		}
		tr = def.Clone()
	}
	var tlsCfg *tls.Config
	if tr.TLSClientConfig != nil {
		tlsCfg = tr.TLSClientConfig.Clone()
	} else {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	tlsCfg.RootCAs = roots
	tr.TLSClientConfig = tlsCfg
	client.Transport = tr
	return &client, nil
}

func parseValidatedPublicJWKS(raw json.RawMessage) (*JWKSet, error) {
	var rawObjects struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(raw, &rawObjects); err != nil {
		return nil, fmt.Errorf("invalid JWKS JSON: %w", err)
	}
	for i, obj := range rawObjects.Keys {
		for _, privField := range []string{"d", "p", "q", "dp", "dq", "qi", "k", "oth"} {
			if _, present := obj[privField]; present {
				return nil, fmt.Errorf("key #%d contains private key parameter %q", i, privField)
			}
		}
	}

	var set JWKSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, errors.New("JWKS contains no keys")
	}
	for i := range set.Keys {
		k := &set.Keys[i]
		if k.Use != "" && k.Use != "sig" {
			return nil, fmt.Errorf("key #%d has unsupported use %q", i, k.Use)
		}
		pub, err := k.PublicKey()
		if err != nil {
			return nil, fmt.Errorf("key #%d: %w", i, err)
		}
		if rsaPub, ok := pub.(*rsa.PublicKey); ok && rsaPub.N.BitLen() < minTrustRSABits {
			return nil, fmt.Errorf("key #%d RSA key is %d bits (minimum %d)", i, rsaPub.N.BitLen(), minTrustRSABits)
		}
	}
	return &set, nil
}

func validateHTTPURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("must be an absolute http or https URL")
	}
	return nil
}

func extractActorClaim(extra map[string]any, claimName string) (subject, issuer string, err error) {
	raw, present := extra[claimName]
	if !present {
		return "", "", nil
	}
	act, ok := raw.(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("%w: claim %q must be an object", ErrClaimsShape, claimName)
	}
	sub, _ := act["sub"].(string)
	if sub == "" {
		return "", "", fmt.Errorf("%w: claim %q requires non-empty sub", ErrClaimsShape, claimName)
	}
	iss, _ := act["iss"].(string)
	return sub, iss, nil
}

func applyGrantMapClaims(
	extra map[string]any,
	cfg TrustClaims,
	roleToScopes map[string]string,
	baseScopes []*authv1.SmartScope,
	secCtx *authv1.SecurityContext,
) error {
	rolesByTenant := make(map[string][]string)
	allowed := make([]string, 0, 4)

	if raw, present := extra[cfg.GrantsClaim]; present {
		grantsMap, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: grants claim %q must be a map", ErrClaimsShape, cfg.GrantsClaim)
		}
		for roleName, byTenantRaw := range grantsMap {
			byTenant, ok := byTenantRaw.(map[string]any)
			if !ok {
				continue
			}
			for tenantID := range byTenant {
				if tenantID == "" {
					continue
				}
				if _, seen := rolesByTenant[tenantID]; !seen {
					allowed = append(allowed, tenantID)
				}
				rolesByTenant[tenantID] = append(rolesByTenant[tenantID], roleName)
			}
		}
	}

	if len(allowed) == 0 && cfg.HomeTenantClaim != "" {
		if home, ok := extra[cfg.HomeTenantClaim].(string); ok && home != "" {
			allowed = append(allowed, home)
			rolesByTenant[home] = nil
		}
	}

	sort.Strings(allowed)
	secCtx.TenantGrants = make(map[string]*authv1.TenantGrant, len(allowed))
	for _, tenantID := range allowed {
		roles := rolesByTenant[tenantID]
		sort.Strings(roles)
		secCtx.TenantGrants[tenantID] = &authv1.TenantGrant{
			Roles:  roles,
			Scopes: mergeSmartScopes(baseScopes, mapRolesToScopes(roles, roleToScopes)),
		}
	}

	active, err := optionalStringClaim(extra, cfg.ActiveTenantClaim)
	if err != nil {
		return err
	}
	resolved := active
	if resolved == "" && len(allowed) == 1 {
		resolved = allowed[0]
	}

	if cfg.OperatorRole != "" && cfg.PlatformTenant != "" {
		secCtx.PlatformTenant = cfg.PlatformTenant
		if platformGrant, ok := secCtx.TenantGrants[cfg.PlatformTenant]; ok && slices.Contains(platformGrant.Roles, cfg.OperatorRole) {
			secCtx.OperatorCrossTenant = true
		}
	}

	if resolved != "" {
		secCtx.TenantId = resolved
		if grant, ok := secCtx.TenantGrants[resolved]; ok {
			secCtx.Roles = slices.Clone(grant.Roles)
			secCtx.Scopes = cloneScopes(grant.Scopes)
		} else if secCtx.OperatorCrossTenant {
			if platformGrant, ok := secCtx.TenantGrants[cfg.PlatformTenant]; ok {
				secCtx.Roles = slices.Clone(platformGrant.Roles)
				secCtx.Scopes = cloneScopes(platformGrant.Scopes)
			}
		}
	} else {
		secCtx.Scopes = baseScopes
		if secCtx.OperatorCrossTenant {
			if platformGrant, ok := secCtx.TenantGrants[cfg.PlatformTenant]; ok {
				secCtx.Roles = slices.Clone(platformGrant.Roles)
				secCtx.Scopes = cloneScopes(platformGrant.Scopes)
			}
		}
	}

	return nil
}

func applyListClaims(
	extra map[string]any,
	cfg TrustClaims,
	roleToScopes map[string]string,
	baseScopes []*authv1.SmartScope,
	secCtx *authv1.SecurityContext,
) error {
	allowed, err := stringListClaim(extra, cfg.TenantsClaim, true)
	if err != nil {
		return err
	}
	roles, err := stringListClaim(extra, cfg.RolesClaim, false)
	if err != nil {
		return err
	}
	active, err := optionalStringClaim(extra, cfg.ActiveTenantClaim)
	if err != nil {
		return err
	}
	activeSlug, err := optionalStringClaim(extra, cfg.ActiveTenantSlugClaim)
	if err != nil {
		return err
	}

	mergedScopes := mergeSmartScopes(baseScopes, mapRolesToScopes(roles, roleToScopes))
	secCtx.TenantGrants = make(map[string]*authv1.TenantGrant, len(allowed)+1)
	for _, tenantID := range allowed {
		secCtx.TenantGrants[tenantID] = &authv1.TenantGrant{
			Roles:  slices.Clone(roles),
			Scopes: cloneScopes(mergedScopes),
		}
	}
	if activeSlug != "" {
		if active == "" || slices.Contains(allowed, active) {
			secCtx.TenantGrants[activeSlug] = &authv1.TenantGrant{
				Roles:  slices.Clone(roles),
				Scopes: cloneScopes(mergedScopes),
			}
		}
	}

	resolved := activeSlug
	if resolved == "" {
		resolved = active
	}
	if resolved == "" && len(allowed) == 1 {
		resolved = allowed[0]
	}

	secCtx.TenantId = resolved
	secCtx.Roles = roles
	secCtx.Scopes = mergedScopes
	return nil
}

func optionalStringClaim(extra map[string]any, claimName string) (string, error) {
	if claimName == "" {
		return "", nil
	}
	raw, present := extra[claimName]
	if !present || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%w: claim %q must be a string", ErrClaimsShape, claimName)
	}
	return s, nil
}

func stringListClaim(extra map[string]any, claimName string, requireValidShape bool) ([]string, error) {
	if claimName == "" {
		return nil, nil
	}
	raw, present := extra[claimName]
	if !present || raw == nil {
		return nil, nil
	}
	seen := make(map[string]struct{})
	var out []string
	switch v := raw.(type) {
	case string:
		if v != "" {
			out = append(out, v)
		}
	case []any:
		for _, elem := range v {
			s, ok := elem.(string)
			if !ok {
				if requireValidShape {
					return nil, fmt.Errorf("%w: claim %q array elements must be strings", ErrClaimsShape, claimName)
				}
				return nil, nil
			}
			if s == "" {
				continue
			}
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	default:
		if requireValidShape {
			return nil, fmt.Errorf("%w: claim %q must be a string or array of strings", ErrClaimsShape, claimName)
		}
		return nil, nil
	}
	sort.Strings(out)
	return out, nil
}

func mergeStringMaps(maps ...map[string]string) map[string]string {
	out := make(map[string]string)
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
