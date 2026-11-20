package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
)

type tenantRouteContextKeyType struct{}

var tenantRouteContextKey = tenantRouteContextKeyType{}

type tenantRouteContext struct {
	flatMode        bool
	originalPath    string
	rawTenant       string
	canonicalTenant string
}

func tenantRouteFromContext(ctx context.Context) (tenantRouteContext, bool) {
	rc, ok := ctx.Value(tenantRouteContextKey).(tenantRouteContext)
	return rc, ok
}

func (s *Server) resolveTenantIdentifier(ctx context.Context, raw string) (string, error) {
	if err := validateTenantSegment(raw); err != nil {
		return "", err
	}
	cp, _ := s.getCompiledPolicy()
	if cp.policy.TenantSlugResolver == nil {
		return raw, nil
	}
	resolved, err := cp.policy.TenantSlugResolver(ctx, raw)
	if err != nil {
		return "", err
	}
	resolved = strings.TrimSpace(resolved)
	if err := validateTenantSegment(resolved); err != nil {
		return "", fmt.Errorf("resolved tenant invalid: %w", err)
	}
	return resolved, nil
}

func validateTenantSegment(tenant string) error {
	if tenant == "" || len(tenant) > 128 {
		return fmt.Errorf("invalid tenant identifier length")
	}
	if strings.ContainsAny(tenant, "/\\?#@ \t\r\n") || strings.Contains(tenant, "..") {
		return fmt.Errorf("tenant identifier %q contains invalid characters", tenant)
	}
	for i := 0; i < len(tenant); i++ {
		if tenant[i] < 0x20 || tenant[i] == 0x7f {
			return fmt.Errorf("tenant identifier contains control characters")
		}
	}
	return nil
}

func isGlobalOperationalFHIRSegment(seg string) bool {
	switch seg {
	case "healthz", "readyz", "metrics", "openapi.json", "$docs", "cds-services",
		"$install-ig", "$load-ig", "$igs", "$subscriptions-ws":
		return true
	default:
		return false
	}
}

func (s *Server) isFlatFHIRSegment(seg string) bool {
	if seg == "" {
		return false
	}
	if strings.HasPrefix(seg, "$") || strings.HasPrefix(seg, "_") {
		return !isGlobalOperationalFHIRSegment(seg)
	}
	if seg == "metadata" || strings.HasPrefix(seg, ".well-known") || seg == "AuditEvent" || seg == "ViewDefinition" {
		return true
	}
	_, isRegisteredType := s.protoRegistry[seg]
	return isRegisteredType
}

func isDiscoveryFHIRPath(parts []string) bool {
	if len(parts) < 3 || parts[0] != "fhir" || parts[1] != "r4" {
		return false
	}
	if len(parts) >= 3 && (parts[2] == "metadata" || strings.HasPrefix(parts[2], ".well-known")) {
		return true
	}
	if len(parts) >= 4 && (parts[3] == "metadata" || strings.HasPrefix(parts[3], ".well-known")) {
		return true
	}
	return false
}

// externalRequestPath returns the outward-facing request path (preserving the flat /fhir/r4/...
// path when a request entered via tenant header routing).
func (s *Server) externalRequestPath(r *http.Request) string {
	if r == nil {
		return ""
	}
	if rc, ok := tenantRouteFromContext(r.Context()); ok && rc.flatMode && rc.originalPath != "" {
		return rc.originalPath
	}
	return r.URL.Path
}

// tenantRoutingMiddleware supports X-Azra-Tenant-Id (or SecurityPolicy.TenantHeaderName)
// header routing over flat /fhir/r4/... endpoints, cross-checks conflicting path vs header
// tenants, and applies optional TenantSlugResolver mapping.
func (s *Server) tenantRoutingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(path, "/")

		if len(parts) < 2 || parts[0] != "fhir" || parts[1] != "r4" {
			next.ServeHTTP(w, r)
			return
		}
		if len(parts) >= 3 && isGlobalOperationalFHIRSegment(parts[2]) {
			next.ServeHTTP(w, r)
			return
		}

		cp, _ := s.getCompiledPolicy()
		headerName := s.effectiveTenantHeaderName()
		rawHeaderTenant := ""
		if headerName != "" {
			rawHeaderTenant = strings.TrimSpace(r.Header.Get(headerName))
		}

		if rawHeaderTenant == "" {
			if cp.policy.RequireTenantHeader && !isDiscoveryFHIRPath(parts) && !isPublicEndpoint(parts) {
				writeOperationOutcome(w, http.StatusBadRequest, "required",
					fmt.Sprintf("missing required %s header", headerName))
				return
			}
			// Even without a header, apply TenantSlugResolver to path-scoped /{tenant}/... if configured.
			if cp.policy.TenantSlugResolver != nil && len(parts) >= 3 && !s.isFlatFHIRSegment(parts[2]) {
				canonical, err := s.resolveTenantIdentifier(r.Context(), parts[2])
				if err != nil {
					writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
					return
				}
				rc := tenantRouteContext{
					flatMode:        false,
					originalPath:    r.URL.Path,
					rawTenant:       parts[2],
					canonicalTenant: canonical,
				}
				reqClone := r.Clone(context.WithValue(r.Context(), tenantRouteContextKey, rc))
				if canonical != parts[2] {
					rewrittenParts := append([]string{"fhir", "r4", canonical}, parts[3:]...)
					reqClone.URL.Path = "/" + strings.Join(rewrittenParts, "/")
					if reqClone.URL.RawPath != "" {
						reqClone.URL.RawPath = ""
					}
				}
				next.ServeHTTP(w, reqClone)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		canonicalHeaderTenant, err := s.resolveTenantIdentifier(r.Context(), rawHeaderTenant)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}

		isFlat := len(parts) == 2 || (len(parts) >= 3 && s.isFlatFHIRSegment(parts[2]))
		if isFlat {
			origPath := r.URL.Path
			var rewrittenParts []string
			if len(parts) == 2 {
				rewrittenParts = []string{"fhir", "r4", canonicalHeaderTenant}
			} else {
				rewrittenParts = append([]string{"fhir", "r4", canonicalHeaderTenant}, parts[2:]...)
			}
			rc := tenantRouteContext{
				flatMode:        true,
				originalPath:    origPath,
				rawTenant:       rawHeaderTenant,
				canonicalTenant: canonicalHeaderTenant,
			}
			reqClone := r.Clone(context.WithValue(r.Context(), tenantRouteContextKey, rc))
			reqClone.URL.Path = "/" + strings.Join(rewrittenParts, "/")
			if reqClone.URL.RawPath != "" {
				reqClone.URL.RawPath = ""
			}
			lw := &flatLocationResponseWriter{
				ResponseWriter: w,
				tenantSegment:  canonicalHeaderTenant,
			}
			next.ServeHTTP(lw, reqClone)
			return
		}

		// Request is path-scoped /fhir/r4/{pathTenant}/... AND also carries the tenant header:
		// cross-check for conflict so header and path cannot disagree silently.
		pathTenant := parts[2]
		canonicalPathTenant, err := s.resolveTenantIdentifier(r.Context(), pathTenant)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		if canonicalPathTenant != canonicalHeaderTenant && pathTenant != rawHeaderTenant {
			writeOperationOutcome(w, http.StatusBadRequest, "conflict",
				fmt.Sprintf("%s header %q conflicts with URL tenant %q", headerName, rawHeaderTenant, pathTenant))
			return
		}

		rc := tenantRouteContext{
			flatMode:        false,
			originalPath:    r.URL.Path,
			rawTenant:       pathTenant,
			canonicalTenant: canonicalPathTenant,
		}
		reqClone := r.Clone(context.WithValue(r.Context(), tenantRouteContextKey, rc))
		if canonicalPathTenant != pathTenant {
			rewrittenParts := append([]string{"fhir", "r4", canonicalPathTenant}, parts[3:]...)
			reqClone.URL.Path = "/" + strings.Join(rewrittenParts, "/")
			if reqClone.URL.RawPath != "" {
				reqClone.URL.RawPath = ""
			}
		}
		next.ServeHTTP(w, reqClone)
	})
}

type flatLocationResponseWriter struct {
	http.ResponseWriter
	tenantSegment string
	wroteHeader   bool
}

func (w *flatLocationResponseWriter) rewriteHeaders() {
	if w.tenantSegment == "" {
		return
	}
	tenantPrefix := "/fhir/r4/" + w.tenantSegment
	for _, hdr := range []string{"Location", "Content-Location"} {
		val := w.ResponseWriter.Header().Get(hdr)
		if val == "" {
			continue
		}
		if val == tenantPrefix {
			w.ResponseWriter.Header().Set(hdr, "/fhir/r4")
		} else if idx := strings.Index(val, tenantPrefix+"/"); idx != -1 {
			rewritten := val[:idx] + "/fhir/r4" + val[idx+len(tenantPrefix):]
			w.ResponseWriter.Header().Set(hdr, rewritten)
		}
	}
}

func (w *flatLocationResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.rewriteHeaders()
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *flatLocationResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.rewriteHeaders()
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *flatLocationResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *flatLocationResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *flatLocationResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}
