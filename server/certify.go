package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/certify"
)

// handleCertify handles GET/POST /fhir/r4/{tenant}/$certify — executes the automated
// FHIR R4 / US Core v6.1.0 / ONC (g)(10) certification test suite against an in-process
// loopback test server in an isolated tenant namespace and returns a FHIR R4 TestReport.
func (s *Server) handleCertify(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	if tenant == "" {
		tenant = "certify"
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if auth.IsPatientRestricted(secCtx) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"patient-scoped token cannot invoke system-level $certify operation")
			return
		}
	}

	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception",
			"$certify requires an active Postgres operational store")
		return
	}

	loopback := httptest.NewServer(s.Handler())
	defer loopback.Close()

	rep, err := certify.Run(r.Context(), certify.Config{
		BaseURL:     loopback.URL,
		Tenant:      tenant,
		BearerToken: extractBearerHeader(r),
		HTTPClient:  loopback.Client(),
	})
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(rep.ToFHIRTestReport())
}

func extractBearerHeader(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && (h[:7] == "Bearer " || h[:7] == "bearer ") {
		return h[7:]
	}
	return ""
}
