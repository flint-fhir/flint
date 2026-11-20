package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TenantSlugResolver optionally translates an incoming tenant identifier or slug
// (from X-Azra-Tenant-Id or URL path) to the canonical Flint tenant key.
type TenantSlugResolver func(ctx context.Context, tenant string) (string, error)

// SecurityPolicy configures production fail-closed security, header-based tenant routing,
// reference confinement, request/bundle bounds, denied resources, idempotency TTL, and rate limiting.
type SecurityPolicy struct {
	// RequireAuth rejects all unauthenticated requests to protected FHIR endpoints (401)
	// and causes ValidateSecurityPolicy to fail closed if no TokenValidator is configured.
	RequireAuth bool

	// RequireTenantClaim rejects authenticated tokens that carry neither an active tenant_id
	// nor tenant_grants (unless OperatorCrossTenant is true).
	RequireTenantClaim bool

	// DisableUnauthenticatedEndpoints disables anonymous access to $bulk-publish, $uds-plus-*,
	// and SMART Health Cards issuance endpoints when no Authorization header is provided.
	DisableUnauthenticatedEndpoints bool

	// AllowedOrigins restricts CORS Access-Control-Allow-Origin when non-empty.
	// When empty, Flint keeps wildcard "*" CORS behavior for SMART App Launch compatibility.
	AllowedOrigins []string

	// TenantHeaderName names the HTTP header for tenant resolution & flat /fhir/r4/... routing.
	// Empty defaults to DefaultTenantHeaderName ("X-Azra-Tenant-Id"); "-" or "none" disables header routing.
	TenantHeaderName string

	// RequireTenantHeader rejects FHIR requests missing the configured tenant header (400).
	RequireTenantHeader bool

	// LockReferentialIntegrity ignores per-request X-Flint-Referential-Integrity overrides.
	LockReferentialIntegrity bool

	// EnforceLocalReferencesOnly rejects external http(s):// and unresolved urn: References (422) on writes.
	EnforceLocalReferencesOnly bool

	// MaxBundleBytes bounds POST Bundle body size in bytes (default 50 MiB when <= 0).
	MaxBundleBytes int64

	// MaxBundleEntries bounds the number of entries in a transaction/batch Bundle (0 = unlimited).
	MaxBundleEntries int

	// DeniedResourceTypes blocks HTTP and Bundle operations targeting listed FHIR resource types (403).
	DeniedResourceTypes []string

	// RateLimitPerSecond sets per-tenant steady-state request refill rate (0 = disabled).
	RateLimitPerSecond float64

	// RateLimitBurst sets per-tenant token bucket capacity (defaults to ceil(RateLimitPerSecond) when <= 0).
	RateLimitBurst int

	// IdempotencyTTL controls how long persisted idempotency entries remain valid (default 24h when <= 0).
	IdempotencyTTL time.Duration

	// TenantSlugResolver optionally normalizes/maps incoming tenant slugs to canonical tenant IDs.
	TenantSlugResolver TenantSlugResolver
}

// DefaultTenantHeaderName is the default HTTP header carrying the caller's tenant slug/ID.
const DefaultTenantHeaderName = "X-Azra-Tenant-Id"

const (
	defaultMaxBundleBytes = 50 * 1024 * 1024
	defaultIdempotencyTTL = 24 * time.Hour
)

type compiledSecurityPolicy struct {
	policy        SecurityPolicy
	deniedTypes   map[string]struct{}
	allowedOrigin map[string]struct{}
	wildcardCORS  bool
}

func compileSecurityPolicy(p SecurityPolicy) compiledSecurityPolicy {
	denied := make(map[string]struct{}, len(p.DeniedResourceTypes))
	for _, rt := range p.DeniedResourceTypes {
		trimmed := strings.ToLower(strings.TrimSpace(rt))
		if trimmed != "" {
			denied[trimmed] = struct{}{}
		}
	}

	origins := make(map[string]struct{}, len(p.AllowedOrigins))
	wildcard := false
	cleanedOrigins := make([]string, 0, len(p.AllowedOrigins))
	for _, o := range p.AllowedOrigins {
		trimmed := strings.TrimSpace(o)
		if trimmed == "" {
			continue
		}
		if trimmed == "*" {
			wildcard = true
		}
		origins[trimmed] = struct{}{}
		cleanedOrigins = append(cleanedOrigins, trimmed)
	}
	p.AllowedOrigins = cleanedOrigins

	return compiledSecurityPolicy{
		policy:        p,
		deniedTypes:   denied,
		allowedOrigin: origins,
		wildcardCORS:  wildcard,
	}
}

// SetSecurityPolicy atomically updates the server's runtime security policy.
func (s *Server) SetSecurityPolicy(p SecurityPolicy) {
	compiled := compileSecurityPolicy(p)
	s.securityMu.Lock()
	s.compiledPolicy = compiled
	if p.RateLimitPerSecond > 0 {
		s.rateLimiter = newTenantRateLimiter(p.RateLimitPerSecond, p.RateLimitBurst)
	} else {
		s.rateLimiter = nil
	}
	s.securityMu.Unlock()
}

// SecurityPolicy returns a copy of the currently active SecurityPolicy.
func (s *Server) SecurityPolicy() SecurityPolicy {
	s.securityMu.RLock()
	p := s.compiledPolicy.policy
	s.securityMu.RUnlock()
	return p
}

func (s *Server) getCompiledPolicy() (compiledSecurityPolicy, *tenantRateLimiter) {
	s.securityMu.RLock()
	cp := s.compiledPolicy
	rl := s.rateLimiter
	s.securityMu.RUnlock()
	return cp, rl
}

// HasTokenValidator reports whether an authentication TokenValidator is currently configured.
func (s *Server) HasTokenValidator() bool {
	s.securityMu.RLock()
	tv := s.tokenValidator
	s.securityMu.RUnlock()
	return tv != nil
}

// ValidateSecurityPolicy verifies that the active SecurityPolicy can be enforced safely.
func (s *Server) ValidateSecurityPolicy() error {
	cp, _ := s.getCompiledPolicy()
	p := cp.policy
	if p.RequireAuth && !s.HasTokenValidator() {
		return fmt.Errorf("security policy invalid: RequireAuth is enabled but no TokenValidator is configured")
	}
	if p.MaxBundleBytes < 0 {
		return fmt.Errorf("security policy invalid: MaxBundleBytes (%d) must be non-negative", p.MaxBundleBytes)
	}
	if p.MaxBundleEntries < 0 {
		return fmt.Errorf("security policy invalid: MaxBundleEntries (%d) must be non-negative", p.MaxBundleEntries)
	}
	if p.RateLimitPerSecond < 0 {
		return fmt.Errorf("security policy invalid: RateLimitPerSecond (%v) must be non-negative", p.RateLimitPerSecond)
	}
	return nil
}

func (s *Server) effectiveTenantHeaderName() string {
	cp, _ := s.getCompiledPolicy()
	name := strings.TrimSpace(cp.policy.TenantHeaderName)
	if name == "" {
		return DefaultTenantHeaderName
	}
	if name == "-" || strings.EqualFold(name, "none") {
		return ""
	}
	return http.CanonicalHeaderKey(name)
}

func (s *Server) effectiveMaxBundleBytes() int64 {
	cp, _ := s.getCompiledPolicy()
	if cp.policy.MaxBundleBytes > 0 {
		return cp.policy.MaxBundleBytes
	}
	return defaultMaxBundleBytes
}

func (s *Server) effectiveIdempotencyTTL() time.Duration {
	cp, _ := s.getCompiledPolicy()
	if cp.policy.IdempotencyTTL > 0 {
		return cp.policy.IdempotencyTTL
	}
	return defaultIdempotencyTTL
}

func (s *Server) isResourceTypeDenied(resType string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(resType))
	if trimmed == "" {
		return false
	}
	cp, _ := s.getCompiledPolicy()
	_, denied := cp.deniedTypes[trimmed]
	return denied
}

func (s *Server) checkCORSOrigin(origin string) (allow bool, restricted bool, echoOrigin string) {
	cp, _ := s.getCompiledPolicy()
	if len(cp.allowedOrigin) == 0 {
		return true, false, "*"
	}
	if cp.wildcardCORS {
		return true, true, "*"
	}
	if origin == "" {
		return false, true, ""
	}
	if _, ok := cp.allowedOrigin[origin]; ok {
		return true, true, origin
	}
	return false, true, ""
}

// idemMemCache provides in-memory idempotency storage when Server is run without a backing store.
type idemMemCache struct {
	mu      sync.RWMutex
	entries map[string][]byte
}
