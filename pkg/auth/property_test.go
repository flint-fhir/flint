package auth_test

import (
	"strings"
	"testing"

	"hegel.dev/go/hegel"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
	"github.com/flint-fhir/flint/pkg/auth"
)

// Property-based tests for SMART on FHIR scope grammar and invariants using Hegel.
//
// WHY PROPERTY-BASED TESTS: Table tests only check the examples humans think of.
// For security and authorization parsers, vulnerabilities hide in combinations nobody thought of.
// Hegel generates inputs across the documented grammar and its boundaries, shrinking any failures
// to the minimal counterexample.

const (
	smartV1Grammar = `(patient|user|system)/([A-Z][a-zA-Z0-9]*|\*)\.(read|write|\*)`
	smartV2Grammar = `(patient|user|system)/([A-Z][a-zA-Z0-9]*|\*)\.[cruds]{1,5}`
)

func TestProperty_SMARTV1GrammarAcceptance(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.FromRegex(smartV1Grammar, true))

		scope, ok := auth.ParseSingleScope(raw)
		if !ok {
			ht.Fatalf("ParseSingleScope(%q) = false, but it matches the SMART v1 grammar", raw)
		}

		if scope == nil {
			ht.Fatalf("ParseSingleScope(%q) returned nil scope with ok=true", raw)
		}

		parts := strings.Split(raw, "/")
		expectedCtx := parts[0]
		rest := strings.Split(parts[1], ".")
		expectedRes := rest[0]
		expectedAction := rest[1]

		switch expectedCtx {
		case "patient":
			if scope.Context != authv1.ScopeContext_SCOPE_CONTEXT_PATIENT {
				ht.Fatalf("expected ScopeContext_PATIENT, got %v", scope.Context)
			}
		case "user":
			if scope.Context != authv1.ScopeContext_SCOPE_CONTEXT_USER {
				ht.Fatalf("expected ScopeContext_USER, got %v", scope.Context)
			}
		case "system":
			if scope.Context != authv1.ScopeContext_SCOPE_CONTEXT_SYSTEM {
				ht.Fatalf("expected ScopeContext_SYSTEM, got %v", scope.Context)
			}
		}

		if scope.ResourceType != expectedRes {
			ht.Fatalf("expected resource %q, got %q", expectedRes, scope.ResourceType)
		}

		switch expectedAction {
		case "read":
			if !auth.ScopeMatches(scope, expectedRes, auth.ActionRead) || !auth.ScopeMatches(scope, expectedRes, auth.ActionSearch) {
				ht.Fatalf("scope %q with 'read' must permit read and search", raw)
			}
			if auth.ScopeMatches(scope, expectedRes, auth.ActionCreate) {
				ht.Fatalf("scope %q with 'read' must NOT permit create", raw)
			}
		case "write":
			if !auth.ScopeMatches(scope, expectedRes, auth.ActionCreate) || !auth.ScopeMatches(scope, expectedRes, auth.ActionUpdate) {
				ht.Fatalf("scope %q with 'write' must permit create and update", raw)
			}
			if auth.ScopeMatches(scope, expectedRes, auth.ActionRead) {
				ht.Fatalf("scope %q with 'write' must NOT permit read", raw)
			}
		}
	})
}

func TestProperty_SMARTV2GrammarAcceptance(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.FromRegex(smartV2Grammar, true))

		scope, ok := auth.ParseSingleScope(raw)
		if !ok {
			ht.Fatalf("ParseSingleScope(%q) = false, but it matches the SMART v2 grammar", raw)
		}

		parts := strings.Split(raw, "/")
		rest := strings.Split(parts[1], ".")
		expectedRes := rest[0]
		actionLetters := rest[1]

		for _, ch := range actionLetters {
			var act authv1.Action
			switch ch {
			case 'c':
				act = auth.ActionCreate
			case 'r':
				act = auth.ActionRead
			case 'u':
				act = auth.ActionUpdate
			case 'd':
				act = auth.ActionDelete
			case 's':
				act = auth.ActionSearch
			}
			if !auth.ScopeMatches(scope, expectedRes, act) {
				ht.Fatalf("scope %q contains %c but ScopeMatches returned false", raw, ch)
			}
		}
	})
}

func TestProperty_QueryParameterFilterInvariance(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		base := hegel.Draw(ht, hegel.FromRegex(smartV2Grammar, true))
		filterKey := hegel.Draw(ht, hegel.FromRegex(`[a-z]{3,10}`, true))
		filterVal := hegel.Draw(ht, hegel.FromRegex(`[a-z0-9-]{3,15}`, true))

		rawWithFilter := base + "?" + filterKey + "=" + filterVal

		baseScope, ok1 := auth.ParseSingleScope(base)
		filterScope, ok2 := auth.ParseSingleScope(rawWithFilter)

		if !ok1 || !ok2 {
			ht.Fatalf("expected both to parse: base ok=%v, filtered ok=%v", ok1, ok2)
		}

		if baseScope.Context != filterScope.Context {
			ht.Fatalf("context mismatch: %v vs %v", baseScope.Context, filterScope.Context)
		}
		if baseScope.ResourceType != filterScope.ResourceType {
			ht.Fatalf("resource mismatch: %v vs %v", baseScope.ResourceType, filterScope.ResourceType)
		}
		if len(baseScope.Actions) != len(filterScope.Actions) {
			ht.Fatalf("action count mismatch: %d vs %d", len(baseScope.Actions), len(filterScope.Actions))
		}
	})
}

func TestProperty_WildcardResourceMatchesAnyResourceType(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		actionLetter := hegel.Draw(ht, hegel.FromRegex(`[cruds]`, true))
		wildcardRaw := "system/*." + actionLetter

		scope, ok := auth.ParseSingleScope(wildcardRaw)
		if !ok {
			ht.Fatalf("failed to parse wildcard scope: %s", wildcardRaw)
		}

		randomResource := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-zA-Z0-9]{2,20}`, true))

		var act authv1.Action
		switch actionLetter {
		case "c":
			act = auth.ActionCreate
		case "r":
			act = auth.ActionRead
		case "u":
			act = auth.ActionUpdate
		case "d":
			act = auth.ActionDelete
		case "s":
			act = auth.ActionSearch
		}

		if !auth.ScopeMatches(scope, randomResource, act) {
			ht.Fatalf("wildcard scope %q failed to match random resource %q", wildcardRaw, randomResource)
		}
	})
}

func TestProperty_ParseScopesNeverPanicsOnArbitraryInput(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		arbitraryInput := hegel.Draw(ht, hegel.FromRegex(`.*`, true))
		// Must not panic or hang
		_ = auth.ParseScopes(arbitraryInput)
	})
}
