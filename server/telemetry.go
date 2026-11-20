package server

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/telemetry"
	"github.com/flint-fhir/flint/store"
)

const (
	clusterSecretOAuthRSAPEM = "flint_oauth_rsa_pem"
	clusterSecretSHCECPEM    = "flint_shc_ec_pem"
	clusterSecretUDAPCAPEM   = "flint_udap_ca_bundle_pem"
)

// bootstrapClusterSigningKeys synchronizes devAuth RS256 (SMART OAuth2 JWT) and ES256
// (SMART Health Cards) signing keys across stateless replicas using explicit environment
// PEMs (FLINT_SIGNING_KEY_PEM / FLINT_SHC_SIGNING_KEY_PEM) or automatic cluster-store
// secret convergence when s.primaryStore != nil.
func (s *Server) bootstrapClusterSigningKeys() {
	if s == nil || s.devAuth == nil {
		return
	}
	envRSA := strings.TrimSpace(os.Getenv("FLINT_SIGNING_KEY_PEM"))
	envEC := strings.TrimSpace(os.Getenv("FLINT_SHC_SIGNING_KEY_PEM"))
	if envRSA != "" || envEC != "" {
		if err := s.devAuth.LoadKeysPEM([]byte(envRSA), []byte(envEC)); err != nil && s.logger != nil {
			s.logger.Warn("failed to load signing key PEM from environment", "error", err)
		}
		if envRSA != "" && envEC != "" {
			return
		}
	}
	if s.primaryStore == nil {
		return
	}
	genRSA, genEC, err := s.devAuth.ExportKeysPEM()
	if err != nil {
		return
	}
	ctx := context.Background()
	var rsaPEM, ecPEM []byte
	if envRSA == "" {
		rsaPEM, _ = s.primaryStore.GetOrCreateClusterSecret(ctx, clusterSecretOAuthRSAPEM, func() ([]byte, error) {
			return genRSA, nil
		})
	}
	if envEC == "" {
		ecPEM, _ = s.primaryStore.GetOrCreateClusterSecret(ctx, clusterSecretSHCECPEM, func() ([]byte, error) {
			return genEC, nil
		})
	}
	if len(rsaPEM) > 0 || len(ecPEM) > 0 {
		_ = s.devAuth.LoadKeysPEM(rsaPEM, ecPEM)
	}
}

func (s *Server) refreshStoreWiring() {
	base := s.baseStore
	if base == nil {
		base = s.primaryStore
	}
	if base == nil {
		s.store = nil
		return
	}
	traced := store.WrapWithTracing(base, s.tracer)
	s.store = traced
	if as, ok := traced.(store.AnalyticalStore); ok && s.analyticalStore != nil {
		if _, wasHyb := base.(*store.HybridStore); wasHyb {
			s.analyticalStore = as
		} else if any(s.analyticalStore) == any(base) {
			s.analyticalStore = as
		}
	}
	s.auditRecorder = audit.NewStoreRecorder(traced)
}

// SetTracer attaches an OpenTelemetry OTLP/HTTP Tracer to the server, instrumenting
// inbound HTTP requests, storage operations, AuditEvents, and outbound calls.
func (s *Server) SetTracer(tr *telemetry.Tracer) {
	if s == nil {
		return
	}
	s.tracer = tr
	s.refreshStoreWiring()
}

// Tracer returns the server's active OpenTelemetry Tracer (or nil if disabled).
func (s *Server) Tracer() *telemetry.Tracer {
	if s == nil {
		return nil
	}
	return s.tracer
}

func (s *Server) hybridStore() *store.HybridStore {
	if s == nil || s.store == nil {
		return nil
	}
	cur := s.store
	for cur != nil {
		if hyb, ok := cur.(*store.HybridStore); ok {
			return hyb
		}
		u, ok := cur.(interface{ Unwrap() store.Store })
		if !ok {
			break
		}
		cur = u.Unwrap()
	}
	return nil
}

func classifyTraceRoute(rawPath string) (route string, tenant string, resType string) {
	trimmed := strings.Trim(rawPath, "/")
	if trimmed == "" {
		return "/", "", ""
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 3 && parts[0] == "fhir" && parts[1] == "r4" {
		if strings.HasPrefix(parts[2], "$") || parts[2] == "healthz" || parts[2] == "readyz" || parts[2] == "metrics" || parts[2] == "cds-services" {
			return "/fhir/r4/" + parts[2], "default", ""
		}
		tenant = parts[2]
		if len(parts) == 3 {
			return "/fhir/r4/{tenant}", tenant, ""
		}
		p3 := parts[3]
		if strings.HasPrefix(p3, "$") || p3 == "metadata" || p3 == ".well-known" || p3 == "cds-services" || p3 == "healthz" || p3 == "readyz" || p3 == "metrics" {
			return "/fhir/r4/{tenant}/" + p3, tenant, ""
		}
		resType = p3
		if len(parts) == 4 {
			return "/fhir/r4/{tenant}/" + resType, tenant, resType
		}
		if strings.HasPrefix(parts[4], "$") || parts[4] == "_search" || parts[4] == "_history" {
			return "/fhir/r4/{tenant}/" + resType + "/" + parts[4], tenant, resType
		}
		if len(parts) == 5 {
			return "/fhir/r4/{tenant}/" + resType + "/{id}", tenant, resType
		}
		return "/fhir/r4/{tenant}/" + resType + "/{id}/" + parts[5], tenant, resType
	}
	if len(parts) >= 2 && parts[0] == "shlink" {
		if len(parts) >= 3 {
			return "/shlink/{id}/" + parts[2], "", "SHLink"
		}
		return "/shlink/{id}", "", "SHLink"
	}
	return "/" + parts[0], "", ""
}

// telemetryMiddleware extracts W3C Trace Context (traceparent / tracestate) headers,
// starts a SpanKindServer span when OpenTelemetry is enabled, and propagates SpanContext
// down the request context for trace-correlated slog and AuditEvent persistence.
func (s *Server) telemetryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := telemetry.ExtractHTTP(r.Context(), r.Header)
		parentSC, hasParent := telemetry.FromContext(ctx)
		if s.tracer == nil || !s.tracer.Enabled() {
			if hasParent {
				w.Header().Set("X-Trace-Id", parentSC.TraceIDHex())
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		route, tenant, resType := classifyTraceRoute(r.URL.Path)
		attrs := []telemetry.Attribute{
			telemetry.String("http.request.method", r.Method),
			telemetry.String("url.path", r.URL.Path),
			telemetry.String("http.route", route),
		}
		if tenant != "" {
			attrs = append(attrs, telemetry.String("fhir.tenant", tenant))
		}
		if resType != "" {
			attrs = append(attrs, telemetry.String("fhir.resource_type", resType))
		}

		spanCtx, sp := s.tracer.Start(ctx, r.Method+" "+route, telemetry.SpanKindServer, attrs...)
		if sc := sp.SpanContext(); sc.IsValid() {
			w.Header().Set("X-Trace-Id", sc.TraceIDHex())
		}
		trw := &auditResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(trw, r.WithContext(spanCtx))

		sp.SetAttributes(telemetry.Int("http.response.status_code", trw.statusCode))
		if trw.statusCode >= 500 {
			sp.SetStatus(telemetry.StatusError, http.StatusText(trw.statusCode))
		} else {
			sp.SetStatus(telemetry.StatusOK, "")
		}
		sp.End()
	})
}
