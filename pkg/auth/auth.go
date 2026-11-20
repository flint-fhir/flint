package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
	"google.golang.org/protobuf/proto"
)

// Re-export protobuf types and enums for convenience.
type (
	SecurityContext    = authv1.SecurityContext
	SmartScope         = authv1.SmartScope
	TenantGrant        = authv1.TenantGrant
	Action             = authv1.Action
	ScopeContext       = authv1.ScopeContext
	SmartConfiguration = authv1.SmartConfiguration
)

// ErrTenantForbidden indicates that the verified SecurityContext does not hold a grant
// for the requested tenant.
var ErrTenantForbidden = errors.New("token not authorized for requested tenant")

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

// ScopeQueryConstraints inspects all scopes in sc that authorize (resType, action).
// If at least one matching scope is unconstrained (no "?param=val" suffix), it returns (nil, false).
// If all matching scopes carry SMART v2 granular sub-resource query constraints
// (e.g., "patient/Observation.rs?category=http://terminology.hl7.org/CodeSystem/observation-category|laboratory"),
// it returns the merged allowed parameter values (comma-joined per parameter for multipleOr) and true.
func ScopeQueryConstraints(sc *authv1.SecurityContext, resType string, action authv1.Action) (url.Values, bool) {
	if sc == nil {
		return nil, false
	}
	var constrained []url.Values
	for _, s := range sc.GetScopes() {
		if !ScopeMatches(s, resType, action) {
			continue
		}
		raw := s.GetRaw()
		qIdx := strings.IndexByte(raw, '?')
		if qIdx == -1 || qIdx == len(raw)-1 {
			// Unconstrained scope grant covers the entire resource type.
			return nil, false
		}
		parsed, err := url.ParseQuery(raw[qIdx+1:])
		if err != nil || len(parsed) == 0 {
			return nil, false
		}
		constrained = append(constrained, parsed)
	}
	if len(constrained) == 0 {
		return nil, false
	}
	merged := make(url.Values)
	for _, q := range constrained {
		for k, vals := range q {
			for _, v := range vals {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					merged.Add(k, trimmed)
				}
			}
		}
	}
	if len(merged) == 0 {
		return nil, false
	}
	// Collapse multiple allowed values per parameter into a single comma-separated multipleOr string.
	out := make(url.Values, len(merged))
	for k, vals := range merged {
		out[k] = []string{strings.Join(vals, ",")}
	}
	return out, true
}

// TokenValueMatchesConstraint checks whether a requested or extracted token value
// (e.g. "laboratory" or "http://terminology.hl7.org/CodeSystem/observation-category|laboratory")
// is permitted by a comma-separated SMART v2 scope constraint string.
func TokenValueMatchesConstraint(candidate, allowedCSV string) bool {
	candSys, candCode := splitSystemCode(candidate)
	for allowedPart := range strings.SplitSeq(allowedCSV, ",") {
		allowSys, allowCode := splitSystemCode(strings.TrimSpace(allowedPart))
		if allowCode == "" {
			continue
		}
		if strings.EqualFold(candCode, allowCode) {
			if allowSys == "" || candSys == "" || strings.EqualFold(candSys, allowSys) {
				return true
			}
		}
	}
	return false
}

func splitSystemCode(raw string) (string, string) {
	if parts := strings.SplitN(strings.TrimSpace(raw), "|", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", strings.TrimSpace(raw)
}

// IsPatientRestricted returns true if the token is scoped to a specific patient compartment
// (either via an explicit patient_id claim or because it carries patient/* SMART scopes).
func IsPatientRestricted(sc *authv1.SecurityContext) bool {
	if sc == nil {
		return false
	}
	if strings.TrimSpace(sc.GetPatientId()) != "" {
		return true
	}
	for _, s := range sc.GetScopes() {
		if s.GetContext() == ScopeContextPatient {
			return true
		}
	}
	return false
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

// ResolveTenantContext binds sc to requestedTenant using isolated per-tenant grants when present.
// It clones sc on change and never unions roles or scopes across tenants.
func ResolveTenantContext(sc *authv1.SecurityContext, requestedTenant string) (*authv1.SecurityContext, error) {
	if sc == nil {
		return nil, nil
	}
	target := strings.TrimSpace(requestedTenant)
	grants := sc.GetTenantGrants()

	if len(grants) > 0 {
		if target == "" {
			target = strings.TrimSpace(sc.GetTenantId())
		}
		if target == "" && len(grants) == 1 {
			for k := range grants {
				target = k
			}
		}
		if target == "" {
			if sc.GetOperatorCrossTenant() {
				return sc, nil
			}
			return nil, fmt.Errorf("%w: no active tenant specified", ErrTenantForbidden)
		}

		if grant, ok := grants[target]; ok {
			cloned, _ := proto.Clone(sc).(*authv1.SecurityContext)
			cloned.TenantId = target
			cloned.OperatorCrossTenant = false
			cloned.Roles = slices.Clone(grant.GetRoles())
			cloned.Scopes = cloneScopes(grant.GetScopes())
			return cloned, nil
		}

		if sc.GetOperatorCrossTenant() {
			cloned, _ := proto.Clone(sc).(*authv1.SecurityContext)
			cloned.TenantId = target
			if pt := sc.GetPlatformTenant(); pt != "" {
				if pGrant, ok := grants[pt]; ok {
					cloned.Roles = slices.Clone(pGrant.GetRoles())
					cloned.Scopes = cloneScopes(pGrant.GetScopes())
				}
			}
			return cloned, nil
		}

		return nil, fmt.Errorf("%w: token grants do not include tenant %s", ErrTenantForbidden, target)
	}

	if target != "" && sc.GetTenantId() != "" && sc.GetTenantId() != target && !sc.GetOperatorCrossTenant() {
		return nil, fmt.Errorf("%w: token tenant %s cannot access tenant %s", ErrTenantForbidden, sc.GetTenantId(), target)
	}

	if target != "" && sc.GetTenantId() == "" && sc.GetOperatorCrossTenant() {
		cloned, _ := proto.Clone(sc).(*authv1.SecurityContext)
		cloned.TenantId = target
		return cloned, nil
	}

	return sc, nil
}

// ParseRoleToScopesMap parses a JSON object (e.g. `{"clinician":"user/*.rs"}`)
// or semicolon/comma-delimited `role=scope1 scope2` list into a role -> scopes mapping.
func ParseRoleToScopesMap(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
			return nil, fmt.Errorf("parse role-to-scopes JSON: %w", err)
		}
		return m, nil
	}

	sep := ";"
	if !strings.Contains(trimmed, ";") && strings.Contains(trimmed, ",") {
		sep = ","
	}
	out := make(map[string]string)
	for _, entry := range strings.Split(trimmed, sep) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		role, scopeStr, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("invalid role-to-scopes entry %q (expected role=scopes)", entry)
		}
		role = strings.TrimSpace(role)
		scopeStr = strings.TrimSpace(scopeStr)
		if role == "" {
			return nil, fmt.Errorf("empty role name in entry %q", entry)
		}
		out[role] = scopeStr
	}
	return out, nil
}

func mapRolesToScopes(roles []string, roleToScopes map[string]string) []*authv1.SmartScope {
	if len(roles) == 0 || len(roleToScopes) == 0 {
		return nil
	}
	var out []*authv1.SmartScope
	for _, role := range roles {
		if scopeStr, ok := roleToScopes[role]; ok && scopeStr != "" {
			out = append(out, ParseScopes(scopeStr)...)
		}
	}
	return out
}

func mergeSmartScopes(groups ...[]*authv1.SmartScope) []*authv1.SmartScope {
	seen := make(map[string]struct{})
	var out []*authv1.SmartScope
	for _, grp := range groups {
		for _, s := range grp {
			if s == nil {
				continue
			}
			key := s.GetRaw()
			if key != "" {
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
			}
			if cloned, ok := proto.Clone(s).(*authv1.SmartScope); ok {
				out = append(out, cloned)
			}
		}
	}
	return out
}

func cloneScopes(scopes []*authv1.SmartScope) []*authv1.SmartScope {
	return mergeSmartScopes(scopes)
}
