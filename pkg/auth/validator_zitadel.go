package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

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

func decodeZitadelMetaString(s string) string {
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil && len(decoded) > 0 && utf8.Valid(decoded) {
		return string(decoded)
	}
	return s
}

// ValidateToken validates the Zitadel token and maps Zitadel orgs, roles, and metadata
// into a normalized Flint SecurityContext.
func (z *ZitadelValidator) ValidateToken(ctx context.Context, bearerToken string) (*authv1.SecurityContext, error) {
	secCtx, err := z.oidcValidator.ValidateToken(ctx, bearerToken)
	if err != nil {
		return nil, err
	}

	secCtx.AuthScheme = "zitadel"
	activeOrgID := strings.TrimSpace(secCtx.Claims[ZitadelClaimOrgID])

	// 1. Map Zitadel Organization to Flint Tenant Schema
	if z.cfg.TenantFromOrgDomain {
		if orgDomain, ok := secCtx.Claims[ZitadelClaimOrgDomain]; ok && orgDomain != "" {
			secCtx.TenantId = normalizeZitadelOrgDomain(orgDomain)
		}
	} else if activeOrgID != "" && secCtx.TenantId == "" {
		secCtx.TenantId = activeOrgID
	}

	// 2. Map Zitadel User Metadata (e.g. patient_id, encounter_id — plain or base64-encoded)
	if metaRaw, ok := secCtx.Claims[ZitadelClaimUserMeta]; ok && metaRaw != "" {
		var meta map[string]any
		if err := json.Unmarshal([]byte(metaRaw), &meta); err == nil {
			if pat, ok := meta["patient_id"].(string); ok && pat != "" {
				secCtx.PatientId = decodeZitadelMetaString(pat)
			}
			if enc, ok := meta["encounter_id"].(string); ok && enc != "" {
				secCtx.EncounterId = decodeZitadelMetaString(enc)
			}
		}
	}

	// 3. Map Zitadel Project Roles per organization to isolated TenantGrants & active-tenant SMART scopes.
	// Roles held in one Zitadel org are NEVER unioned into another org's active scopes.
	baseScopes := cloneScopes(secCtx.Scopes)
	rolesRaw := strings.TrimSpace(secCtx.Claims[ZitadelClaimRoles])
	if rolesRaw == "" {
		// Support project-qualified claim urn:zitadel:iam:org:project:<id>:roles when generic claim is absent.
		for k, v := range secCtx.Claims {
			if strings.HasPrefix(k, "urn:zitadel:iam:org:project:") && strings.HasSuffix(k, ":roles") && v != "" {
				rolesRaw = v
				break
			}
		}
	}
	if rolesRaw != "" {
		// Zitadel roles claim format: {"roleName": {"orgID": "orgDomainOrMeta"}}
		var rolesMap map[string]any
		if err := json.Unmarshal([]byte(rolesRaw), &rolesMap); err == nil {
			rolesByOrg := make(map[string][]string)
			domainAliasToOrg := make(map[string]string)
			hasDomainHint := make(map[string]bool)

			for roleName, orgsVal := range rolesMap {
				orgEntries, ok := orgsVal.(map[string]any)
				if !ok {
					continue
				}
				for orgID, orgMeta := range orgEntries {
					rolesByOrg[orgID] = append(rolesByOrg[orgID], roleName)
					if metaStr, ok := orgMeta.(string); ok && strings.Contains(metaStr, ".") {
						hasDomainHint[orgID] = true
						domainAliasToOrg[metaStr] = orgID
						domainAliasToOrg[normalizeZitadelOrgDomain(metaStr)] = orgID
					}
				}
			}

			if len(rolesByOrg) > 0 {
				secCtx.TenantGrants = make(map[string]*authv1.TenantGrant, len(rolesByOrg))
				for orgID, roles := range rolesByOrg {
					sort.Strings(roles)
					secCtx.TenantGrants[orgID] = &authv1.TenantGrant{
						Roles:  roles,
						Scopes: mergeSmartScopes(baseScopes, mapRolesToScopes(roles, z.cfg.RoleToScopes)),
					}
				}
				for alias, orgID := range domainAliasToOrg {
					if g, ok := secCtx.TenantGrants[orgID]; ok {
						if _, exists := secCtx.TenantGrants[alias]; !exists {
							secCtx.TenantGrants[alias] = &authv1.TenantGrant{
								Roles:  slices.Clone(g.Roles),
								Scopes: cloneScopes(g.Scopes),
							}
						}
					}
				}

				var activeGrant *authv1.TenantGrant
				switch {
				case activeOrgID != "":
					activeGrant = secCtx.TenantGrants[activeOrgID]
				case secCtx.TenantId != "":
					if g, ok := secCtx.TenantGrants[secCtx.TenantId]; ok {
						activeGrant = g
					} else if len(rolesByOrg) == 1 {
						// Single-org token missing org:id where the org entry carries an opaque label rather than a conflicting domain.
						for orgID := range rolesByOrg {
							if !hasDomainHint[orgID] {
								activeGrant = secCtx.TenantGrants[orgID]
							}
						}
					}
				}

				if activeGrant != nil {
					if secCtx.TenantId != "" {
						secCtx.TenantGrants[secCtx.TenantId] = &authv1.TenantGrant{
							Roles:  slices.Clone(activeGrant.Roles),
							Scopes: cloneScopes(activeGrant.Scopes),
						}
					}
					secCtx.Roles = slices.Clone(activeGrant.Roles)
					secCtx.Scopes = cloneScopes(activeGrant.Scopes)
				}
			}
		}
	}

	return secCtx, nil
}

func normalizeZitadelOrgDomain(orgDomain string) string {
	// e.g. "hospital-a.azra.dev" -> "hospital_a"
	tenant := strings.Split(orgDomain, ".")[0]
	return strings.ReplaceAll(tenant, "-", "_")
}
