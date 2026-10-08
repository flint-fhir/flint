package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
)

const (
	ZitadelClaimOrgID     = "urn:zitadel:iam:org:id"
	ZitadelClaimOrgDomain = "urn:zitadel:iam:org:domain"
	ZitadelClaimProjectID = "urn:zitadel:iam:org:project:id"
	ZitadelClaimRoles     = "urn:zitadel:iam:org:project:roles"
	ZitadelClaimUserMeta  = "urn:zitadel:iam:user:metadata"
)

// ZitadelConfig configures the Zitadel-specific token validator.
type ZitadelConfig struct {
	OIDCConfig
	// RoleToScopes maps Zitadel project roles to SMART on FHIR scopes.
	// For example: "clinical-reader" -> "patient/*.rs user/*.rs"
	//              "system-admin"    -> "system/*.cruds"
	RoleToScopes map[string]string

	// TenantFromOrgDomain when true uses the Zitadel org domain prefix as the Flint tenant schema.
	TenantFromOrgDomain bool
}

// ZitadelValidator wraps OIDCValidator to handle Zitadel-specific claims and project roles,
// fulfilling the TokenValidator interface.
type ZitadelValidator struct {
	oidcValidator *OIDCValidator
	cfg           ZitadelConfig
}

// NewZitadelValidator creates a new TokenValidator configured for Zitadel identity provider.
func NewZitadelValidator(cfg ZitadelConfig) (*ZitadelValidator, error) {
	oidcVal, err := NewOIDCValidator(cfg.OIDCConfig)
	if err != nil {
		return nil, fmt.Errorf("new zitadel validator: %w", err)
	}

	return &ZitadelValidator{
		oidcValidator: oidcVal,
		cfg:           cfg,
	}, nil
}

// ValidateToken validates the Zitadel token and maps Zitadel orgs, roles, and metadata
// into a normalized Flint SecurityContext.
func (z *ZitadelValidator) ValidateToken(ctx context.Context, bearerToken string) (*authv1.SecurityContext, error) {
	secCtx, err := z.oidcValidator.ValidateToken(ctx, bearerToken)
	if err != nil {
		return nil, err
	}

	// 1. Map Zitadel Organization to Flint Tenant Schema
	if z.cfg.TenantFromOrgDomain {
		if orgDomain, ok := secCtx.Claims[ZitadelClaimOrgDomain]; ok && orgDomain != "" {
			// e.g. "hospital-a.azra.dev" -> "hospital_a"
			tenant := strings.Split(orgDomain, ".")[0]
			tenant = strings.ReplaceAll(tenant, "-", "_")
			secCtx.TenantId = tenant
		}
	} else if orgID, ok := secCtx.Claims[ZitadelClaimOrgID]; ok && orgID != "" && secCtx.TenantId == "" {
		secCtx.TenantId = orgID
	}

	// 2. Map Zitadel User Metadata (e.g. patient_id, encounter_id)
	if metaRaw, ok := secCtx.Claims[ZitadelClaimUserMeta]; ok && metaRaw != "" {
		var meta map[string]any
		if err := json.Unmarshal([]byte(metaRaw), &meta); err == nil {
			if pat, ok := meta["patient_id"].(string); ok && pat != "" {
				secCtx.PatientId = pat
			}
			if enc, ok := meta["encounter_id"].(string); ok && enc != "" {
				secCtx.EncounterId = enc
			}
		}
	}

	// 3. Map Zitadel Project Roles to SMART scopes
	if len(z.cfg.RoleToScopes) > 0 {
		if rolesRaw, ok := secCtx.Claims[ZitadelClaimRoles]; ok && rolesRaw != "" {
			// Zitadel roles claim format: {"roleName": {"orgID": "..."}}
			var rolesMap map[string]any
			if err := json.Unmarshal([]byte(rolesRaw), &rolesMap); err == nil {
				for roleName := range rolesMap {
					if scopeStr, ok := z.cfg.RoleToScopes[roleName]; ok {
						extraScopes := ParseScopes(scopeStr)
						secCtx.Scopes = append(secCtx.Scopes, extraScopes...)
					}
				}
			}
		}
	}

	return secCtx, nil
}
