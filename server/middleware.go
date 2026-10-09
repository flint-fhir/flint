package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/flint-fhir/flint/pkg/auth"
)

// authMiddleware enforces SMART on FHIR bearer token authentication,
// scope checking, and patient compartment isolation.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If no token validator configured, server is in unauthenticated dev mode
		if s.tokenValidator == nil {
			next.ServeHTTP(w, r)
			return
		}

		path := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(path, "/")

		// Allow public endpoints
		if isPublicEndpoint(parts) {
			next.ServeHTTP(w, r)
			return
		}

		// Extract Bearer token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			writeOperationOutcome(w, http.StatusUnauthorized, "login", "missing or malformed Authorization header")
			return
		}
		rawToken := strings.TrimSpace(authHeader[7:])

		// Validate token using configured TokenValidator (OIDC/Zitadel/Mock)
		secCtx, err := s.tokenValidator.ValidateToken(r.Context(), rawToken)
		if err != nil {
			s.logger.Warn("token validation failed", "error", err, "path", r.URL.Path)
			writeOperationOutcome(w, http.StatusUnauthorized, "login", err.Error())
			return
		}

		// Ensure path is /fhir/r4/{tenant}/...
		if len(parts) < 3 || parts[0] != "fhir" || parts[1] != "r4" {
			next.ServeHTTP(w, r)
			return
		}

		reqTenant := parts[2]

		// Enforce tenant boundary if token is bound to a specific tenant
		if secCtx.GetTenantId() != "" && secCtx.GetTenantId() != reqTenant {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("token tenant %s cannot access tenant %s", secCtx.GetTenantId(), reqTenant))
			return
		}

		// Check SMART scopes based on request shape
		if len(parts) >= 4 {
			resType := parts[3]

			switch r.Method {
			case http.MethodGet:
				if len(parts) >= 5 {
					// Read: GET /{tenant}/{resourceType}/{id}
					id := parts[4]
					if !auth.Allows(secCtx, resType, auth.ActionRead) {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("insufficient scope for read on %s", resType))
						return
					}

					// Patient compartment enforcement
					if auth.IsPatientRestricted(secCtx) {
						if strings.EqualFold(resType, "Patient") && cleanID(id) != cleanID(secCtx.GetPatientId()) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("patient-scoped token (%s) cannot access patient %s",
									secCtx.GetPatientId(), id))
							return
						}
					}
				} else {
					// Search: GET /{tenant}/{resourceType}
					if !auth.Allows(secCtx, resType, auth.ActionSearch) {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("insufficient scope for search on %s", resType))
						return
					}

					// Patient compartment enforcement in search query parameters
					if auth.IsPatientRestricted(secCtx) {
						q := r.URL.Query()
						if strings.EqualFold(resType, "Patient") {
							if idParam := q.Get("_id"); idParam != "" && cleanID(idParam) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot search patient %s",
										secCtx.GetPatientId(), idParam))
								return
							}
						}

						// For clinical resources, reject mismatched patient/subject filters
						if pParam := q.Get("patient"); pParam != "" && cleanID(pParam) != cleanID(secCtx.GetPatientId()) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("cannot query records of patient %s with token restricted to %s",
									pParam, secCtx.GetPatientId()))
							return
						}
						if sParam := q.Get("subject"); sParam != "" && cleanID(sParam) != cleanID(secCtx.GetPatientId()) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("cannot query records of subject %s with token restricted to %s",
									sParam, secCtx.GetPatientId()))
							return
						}
					}
				}

			case http.MethodPost:
				if len(parts) >= 5 && parts[4] == "$validate" {
					// Validate operation: POST /{tenant}/{resourceType}/$validate
					if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionCreate) {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("insufficient scope for validate on %s", resType))
						return
					}
				} else if resType == "$validate" {
					// System-level validate: POST /{tenant}/$validate
					if !auth.Allows(secCtx, "*", auth.ActionRead) && !auth.Allows(secCtx, "*", auth.ActionCreate) {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							"insufficient scope for validate")
						return
					}
				} else {
					// Create: POST /{tenant}/{resourceType}
					if !auth.Allows(secCtx, resType, auth.ActionCreate) {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("insufficient scope for create on %s", resType))
						return
					}
				}
			}
		} else if r.Method == http.MethodPost && len(parts) == 3 {
			// Bundle: POST /{tenant}
			if !auth.Allows(secCtx, "*", auth.ActionCreate) && !auth.Allows(secCtx, "*", auth.ActionUpdate) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"insufficient scope for transaction bundle processing")
				return
			}
		}

		// Attach verified SecurityContext to request
		ctx := auth.WithSecurityContext(r.Context(), secCtx)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isPublicEndpoint(parts []string) bool {
	if len(parts) < 3 {
		return false
	}
	if len(parts) >= 4 && parts[3] == "metadata" {
		return true
	}
	for _, p := range parts {
		if strings.HasPrefix(p, ".well-known") {
			return true
		}
	}
	return false
}

func cleanID(id string) string {
	if idx := strings.LastIndex(id, "/"); idx != -1 {
		return id[idx+1:]
	}
	return id
}
