package auth

import (
	"context"
	"strings"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
)

// Re-export protobuf types and enums for convenience.
type (
	SecurityContext    = authv1.SecurityContext
	SmartScope         = authv1.SmartScope
	Action             = authv1.Action
	ScopeContext       = authv1.ScopeContext
	SmartConfiguration = authv1.SmartConfiguration
)

const (
	ActionUnspecified = authv1.Action_ACTION_UNSPECIFIED
	ActionCreate      = authv1.Action_ACTION_CREATE // 'c'
	ActionRead        = authv1.Action_ACTION_READ   // 'r'
	ActionUpdate      = authv1.Action_ACTION_UPDATE // 'u'
	ActionDelete      = authv1.Action_ACTION_DELETE // 'd'
	ActionSearch      = authv1.Action_ACTION_SEARCH // 's'

	ScopeContextUnspecified = authv1.ScopeContext_SCOPE_CONTEXT_UNSPECIFIED
	ScopeContextPatient     = authv1.ScopeContext_SCOPE_CONTEXT_PATIENT
	ScopeContextUser        = authv1.ScopeContext_SCOPE_CONTEXT_USER
	ScopeContextSystem      = authv1.ScopeContext_SCOPE_CONTEXT_SYSTEM
)

// TokenValidator verifies an incoming bearer token and extracts its clinical claims.
// Implementations can be standard OIDC (JWKS), Zitadel, or test mocks.
type TokenValidator interface {
	ValidateToken(ctx context.Context, bearerToken string) (*authv1.SecurityContext, error)
}

type contextKey struct{}

var securityContextKey = contextKey{}

// WithSecurityContext returns a new context with the SecurityContext attached.
func WithSecurityContext(ctx context.Context, sc *authv1.SecurityContext) context.Context {
	return context.WithValue(ctx, securityContextKey, sc)
}

// FromContext extracts the SecurityContext from context, if present.
func FromContext(ctx context.Context) (*authv1.SecurityContext, bool) {
	sc, ok := ctx.Value(securityContextKey).(*authv1.SecurityContext)
	return sc, ok && sc != nil
}

// Allows checks whether the SecurityContext authorizes the given action on a resource type.
func Allows(sc *authv1.SecurityContext, resType string, action authv1.Action) bool {
	if sc == nil {
		return false
	}
	for _, s := range sc.GetScopes() {
		if ScopeMatches(s, resType, action) {
			return true
		}
	}
	return false
}

// ScopeMatches checks if a single SmartScope allows the target resource type and action.
func ScopeMatches(s *authv1.SmartScope, resType string, action authv1.Action) bool {
	if s == nil {
		return false
	}
	if s.GetResourceType() != "*" && !strings.EqualFold(s.GetResourceType(), resType) {
		return false
	}
	for _, a := range s.GetActions() {
		if a == action {
			return true
		}
	}
	return false
}

// IsPatientRestricted returns true if the token is scoped to a specific patient compartment.
func IsPatientRestricted(sc *authv1.SecurityContext) bool {
	return sc != nil && sc.GetPatientId() != ""
}

// ParseScopes parses a space-delimited string of OAuth2/SMART scopes into proto SmartScopes.
func ParseScopes(scopeStr string) []*authv1.SmartScope {
	parts := strings.Fields(scopeStr)
	res := make([]*authv1.SmartScope, 0, len(parts))
	for _, p := range parts {
		if scope, ok := ParseSingleScope(p); ok {
			res = append(res, scope)
		}
	}
	return res
}

// ParseSingleScope parses an individual SMART on FHIR scope into a proto SmartScope.
// Supports both SMART v1 (read, write, *) and SMART v2 (cruds letters).
func ParseSingleScope(raw string) (*authv1.SmartScope, bool) {
	// Strip parameter query filters if present (e.g. patient/Observation.rs?category=vital-signs)
	clean := raw
	if idx := strings.IndexByte(raw, '?'); idx != -1 {
		clean = raw[:idx]
	}

	slashIdx := strings.IndexByte(clean, '/')
	if slashIdx == -1 {
		return nil, false
	}

	ctxStr := clean[:slashIdx]
	var sCtx authv1.ScopeContext
	switch ctxStr {
	case "patient":
		sCtx = authv1.ScopeContext_SCOPE_CONTEXT_PATIENT
	case "user":
		sCtx = authv1.ScopeContext_SCOPE_CONTEXT_USER
	case "system":
		sCtx = authv1.ScopeContext_SCOPE_CONTEXT_SYSTEM
	default:
		return nil, false
	}

	remainder := clean[slashIdx+1:]
	dotIdx := strings.IndexByte(remainder, '.')
	if dotIdx == -1 {
		return nil, false
	}

	resType := remainder[:dotIdx]
	actionStr := remainder[dotIdx+1:]

	actionSet := make(map[authv1.Action]bool)

	// SMART v1 format
	switch actionStr {
	case "read":
		actionSet[ActionRead] = true
		actionSet[ActionSearch] = true
	case "write":
		actionSet[ActionCreate] = true
		actionSet[ActionUpdate] = true
		actionSet[ActionDelete] = true
	case "*":
		actionSet[ActionCreate] = true
		actionSet[ActionRead] = true
		actionSet[ActionUpdate] = true
		actionSet[ActionDelete] = true
		actionSet[ActionSearch] = true
	default:
		// SMART v2 format: individual 'c', 'r', 'u', 'd', 's' letters
		for _, ch := range actionStr {
			switch ch {
			case 'c':
				actionSet[ActionCreate] = true
			case 'r':
				actionSet[ActionRead] = true
			case 'u':
				actionSet[ActionUpdate] = true
			case 'd':
				actionSet[ActionDelete] = true
			case 's':
				actionSet[ActionSearch] = true
			}
		}
	}

	if len(actionSet) == 0 {
		return nil, false
	}

	actions := make([]authv1.Action, 0, len(actionSet))
	for a := range actionSet {
		actions = append(actions, a)
	}

	return &authv1.SmartScope{
		Raw:          raw,
		Context:      sCtx,
		ResourceType: resType,
		Actions:      actions,
	}, true
}
