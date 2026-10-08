package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
	"github.com/flint-fhir/flint/pkg/auth"
)

func TestParseScopes(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantCount  int
		assertions func(t *testing.T, scopes []*authv1.SmartScope)
	}{
		{
			name:      "SMART v1 read and write",
			raw:       "patient/Patient.read patient/Observation.write",
			wantCount: 2,
			assertions: func(t *testing.T, scopes []*authv1.SmartScope) {
				s1 := scopes[0]
				assert.Equal(t, authv1.ScopeContext_SCOPE_CONTEXT_PATIENT, s1.Context)
				assert.Equal(t, "Patient", s1.ResourceType)
				assert.True(t, auth.ScopeMatches(s1, "Patient", auth.ActionRead))
				assert.True(t, auth.ScopeMatches(s1, "Patient", auth.ActionSearch))
				assert.False(t, auth.ScopeMatches(s1, "Patient", auth.ActionCreate))

				s2 := scopes[1]
				assert.Equal(t, "Observation", s2.ResourceType)
				assert.True(t, auth.ScopeMatches(s2, "Observation", auth.ActionCreate))
				assert.True(t, auth.ScopeMatches(s2, "Observation", auth.ActionUpdate))
				assert.True(t, auth.ScopeMatches(s2, "Observation", auth.ActionDelete))
				assert.False(t, auth.ScopeMatches(s2, "Observation", auth.ActionRead))
			},
		},
		{
			name:      "SMART v2 cruds letters",
			raw:       "patient/Observation.rs user/*.cruds system/*.r",
			wantCount: 3,
			assertions: func(t *testing.T, scopes []*authv1.SmartScope) {
				s1 := scopes[0]
				assert.Equal(t, "Observation", s1.ResourceType)
				assert.True(t, auth.ScopeMatches(s1, "Observation", auth.ActionRead))
				assert.True(t, auth.ScopeMatches(s1, "Observation", auth.ActionSearch))
				assert.False(t, auth.ScopeMatches(s1, "Observation", auth.ActionCreate))

				s2 := scopes[1]
				assert.Equal(t, "*", s2.ResourceType)
				assert.True(t, auth.ScopeMatches(s2, "Encounter", auth.ActionCreate))
				assert.True(t, auth.ScopeMatches(s2, "Patient", auth.ActionDelete))

				s3 := scopes[2]
				assert.Equal(t, authv1.ScopeContext_SCOPE_CONTEXT_SYSTEM, s3.Context)
				assert.True(t, auth.ScopeMatches(s3, "Condition", auth.ActionRead))
				assert.False(t, auth.ScopeMatches(s3, "Condition", auth.ActionSearch))
			},
		},
		{
			name:      "SMART v2 query parameters stripped",
			raw:       "patient/Observation.rs?category=vital-signs",
			wantCount: 1,
			assertions: func(t *testing.T, scopes []*authv1.SmartScope) {
				s1 := scopes[0]
				assert.Equal(t, "Observation", s1.ResourceType)
				assert.True(t, auth.ScopeMatches(s1, "Observation", auth.ActionRead))
				assert.True(t, auth.ScopeMatches(s1, "Observation", auth.ActionSearch))
			},
		},
		{
			name:      "Wildcard asterisk",
			raw:       "user/*.*",
			wantCount: 1,
			assertions: func(t *testing.T, scopes []*authv1.SmartScope) {
				s1 := scopes[0]
				assert.True(t, auth.ScopeMatches(s1, "AnyResource", auth.ActionCreate))
				assert.True(t, auth.ScopeMatches(s1, "AnyResource", auth.ActionRead))
				assert.True(t, auth.ScopeMatches(s1, "AnyResource", auth.ActionUpdate))
				assert.True(t, auth.ScopeMatches(s1, "AnyResource", auth.ActionDelete))
				assert.True(t, auth.ScopeMatches(s1, "AnyResource", auth.ActionSearch))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scopes := auth.ParseScopes(tt.raw)
			require.Len(t, scopes, tt.wantCount)
			tt.assertions(t, scopes)
		})
	}
}

func TestSecurityContext_Allows(t *testing.T) {
	sc := &authv1.SecurityContext{
		Subject:   "user-1",
		PatientId: "pat-123",
		Scopes:    auth.ParseScopes("patient/Patient.r patient/Observation.rs"),
	}

	assert.True(t, auth.Allows(sc, "Patient", auth.ActionRead))
	assert.False(t, auth.Allows(sc, "Patient", auth.ActionSearch))
	assert.False(t, auth.Allows(sc, "Patient", auth.ActionCreate))

	assert.True(t, auth.Allows(sc, "Observation", auth.ActionRead))
	assert.True(t, auth.Allows(sc, "Observation", auth.ActionSearch))
	assert.False(t, auth.Allows(sc, "Observation", auth.ActionCreate))

	assert.False(t, auth.Allows(sc, "Encounter", auth.ActionRead))

	assert.True(t, auth.IsPatientRestricted(sc))
}

func TestOIDCValidator_EndToEnd(t *testing.T) {
	issuer := "https://auth.example.com"
	audience := "flint-api"

	mock, err := auth.NewMockTokenValidator(issuer, audience)
	require.NoError(t, err)

	// Configure OIDCValidator with mock's static JWKS
	oidcVal, err := auth.NewOIDCValidator(auth.OIDCConfig{
		Issuer:     issuer,
		Audience:   audience,
		StaticJWKS: mock.JWKS(),
	})
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("Valid Token", func(t *testing.T) {
		token, err := mock.IssueToken("sub-123", "patient/Patient.r patient/Observation.rs", "pat-456", time.Hour, map[string]any{
			"tenant_id": "tenant_alpha",
		})
		require.NoError(t, err)

		secCtx, err := oidcVal.ValidateToken(ctx, token)
		require.NoError(t, err)
		assert.Equal(t, "sub-123", secCtx.Subject)
		assert.Equal(t, "pat-456", secCtx.PatientId)
		assert.Equal(t, "tenant_alpha", secCtx.TenantId)
		assert.True(t, auth.Allows(secCtx, "Patient", auth.ActionRead))
		assert.True(t, auth.Allows(secCtx, "Observation", auth.ActionSearch))
	})

	t.Run("Expired Token", func(t *testing.T) {
		token, err := mock.IssueToken("sub-expired", "system/*.r", "", -time.Minute, nil)
		require.NoError(t, err)

		_, err = oidcVal.ValidateToken(ctx, token)
		assert.ErrorIs(t, err, auth.ErrTokenExpired)
	})

	t.Run("Tampered Signature", func(t *testing.T) {
		token, err := mock.IssueToken("sub-tamper", "system/*.r", "", time.Hour, nil)
		require.NoError(t, err)

		tampered := token[:len(token)-4] + "AAAA"
		_, err = oidcVal.ValidateToken(ctx, tampered)
		assert.ErrorIs(t, err, auth.ErrInvalidSignature)
	})
}

func TestZitadelValidator(t *testing.T) {
	issuer := "https://zitadel.azra.dev"
	audience := "flint-api"

	mock, err := auth.NewMockTokenValidator(issuer, audience)
	require.NoError(t, err)

	zitadelVal, err := auth.NewZitadelValidator(auth.ZitadelConfig{
		OIDCConfig: auth.OIDCConfig{
			Issuer:     issuer,
			Audience:   audience,
			StaticJWKS: mock.JWKS(),
		},
		TenantFromOrgDomain: true,
		RoleToScopes: map[string]string{
			"clinical-reader": "patient/*.rs user/*.rs",
			"admin":           "system/*.cruds",
		},
	})
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("Zitadel Org Domain, User Meta, and Roles", func(t *testing.T) {
		token, err := mock.IssueToken("zitadel-user-1", "openid", "", time.Hour, map[string]any{
			auth.ZitadelClaimOrgDomain: "st-jude-hospital.azra.dev",
			auth.ZitadelClaimUserMeta:  `{"patient_id":"pat-999"}`,
			auth.ZitadelClaimRoles:     `{"clinical-reader":{"org-1":true}}`,
		})
		require.NoError(t, err)

		secCtx, err := zitadelVal.ValidateToken(ctx, token)
		require.NoError(t, err)

		// Verified Org Domain mapped to Tenant schema
		assert.Equal(t, "st_jude_hospital", secCtx.TenantId)
		// Verified user meta mapped to patient_id
		assert.Equal(t, "pat-999", secCtx.PatientId)
		// Verified project role mapped to SMART scopes
		assert.True(t, auth.Allows(secCtx, "Patient", auth.ActionRead))
		assert.True(t, auth.Allows(secCtx, "Observation", auth.ActionSearch))
	})
}

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	_, ok := auth.FromContext(ctx)
	assert.False(t, ok)

	sc := &authv1.SecurityContext{Subject: "user-1"}
	ctx = auth.WithSecurityContext(ctx, sc)

	retrieved, ok := auth.FromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, "user-1", retrieved.Subject)
}
