package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/google/uuid"
)

type auditActorKeyType struct{}

var auditActorKey = auditActorKeyType{}

// auditActorHolder captures authenticated actor metadata inside authMiddleware
// even if authMiddleware subsequently aborts the request with 403 Forbidden.
type auditActorHolder struct {
	actorSubject string
	patientID    string
}

// authMiddleware enforces SMART on FHIR bearer token authentication,
// scope checking, and patient compartment isolation.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(path, "/")

		// Allow public endpoints (discovery, metadata, OAuth2 authorize/token/introspect, JWKS)
		if isPublicEndpoint(parts) {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		secCtx, hasExistingSecCtx := auth.FromContext(r.Context())
		if !hasExistingSecCtx || authHeader != "" {
			validator := s.tokenValidator
			if validator == nil {
				// If no external token validator is configured and no Authorization header is supplied,
				// server operates in unauthenticated dev mode. When an Authorization header IS supplied
				// (e.g. SMART App Launch / HL7 Inferno conformance tests), validate and enforce scopes via devAuth.
				if authHeader == "" {
					next.ServeHTTP(w, r)
					return
				}
				validator = s.devAuth
			}

			// Extract Bearer token
			if authHeader == "" || !strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
				writeOperationOutcome(w, http.StatusUnauthorized, "login", "missing or malformed Authorization header")
				return
			}
			rawToken := strings.TrimSpace(authHeader[7:])

			if s.isTokenRevoked(rawToken) {
				writeOperationOutcome(w, http.StatusUnauthorized, "login", "token has been revoked")
				return
			}

			// Validate token using configured TokenValidator (OIDC/Zitadel/Mock)
			var err error
			secCtx, err = validator.ValidateToken(r.Context(), rawToken)
			if err != nil {
				s.logger.Warn("token validation failed", "error", err, "path", r.URL.Path)
				writeOperationOutcome(w, http.StatusUnauthorized, "login", err.Error())
				return
			}
		}

		if holder, ok := r.Context().Value(auditActorKey).(*auditActorHolder); ok && holder != nil {
			holder.actorSubject = secCtx.GetSubject()
			holder.patientID = cleanID(secCtx.GetPatientId())
		}

		// Ensure path is /fhir/r4/{tenant}/...
		if len(parts) < 3 || parts[0] != "fhir" || parts[1] != "r4" {
			ctx := auth.WithSecurityContext(r.Context(), secCtx)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		reqTenant := parts[2]

		// Enforce tenant boundary if token is bound to a specific tenant
		if secCtx.GetTenantId() != "" && secCtx.GetTenantId() != reqTenant {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("token tenant %s cannot access tenant %s", secCtx.GetTenantId(), reqTenant))
			return
		}

		isValidateReq := (len(parts) >= 4 && parts[3] == "$validate") ||
			(len(parts) >= 5 && parts[4] == "$validate") ||
			(len(parts) >= 6 && parts[5] == "$validate")
		if auth.IsPatientRestricted(secCtx) && !isValidateReq && cleanID(secCtx.GetPatientId()) == "" {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "patient-scoped token is missing required patient_id claim")
			return
		}

		// Check SMART scopes based on request shape
		if len(parts) == 3 && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if len(secCtx.GetScopes()) == 0 {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for system search")
				return
			}
		}
		if len(parts) >= 4 {
			resType := parts[3]
			isBulkExportOp := resType == "$export" || resType == "$export-poll" || resType == "$export-file" ||
				resType == "$import" || resType == "$import-poll" || resType == "$certify" ||
				(strings.EqualFold(resType, "Patient") && len(parts) == 5 && parts[4] == "$export") ||
				(strings.EqualFold(resType, "Patient") && len(parts) == 6 && parts[5] == "$export") ||
				(strings.EqualFold(resType, "Group") && len(parts) == 6 && parts[5] == "$export")

			if !isBulkExportOp {
				switch r.Method {
				case http.MethodGet, http.MethodHead:
					if isTerminologyOp(parts) {
						if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for terminology operation on %s", resType))
							return
						}
					} else if isMeasureOp(parts) {
						if !auth.Allows(secCtx, "Measure", auth.ActionRead) && !auth.Allows(secCtx, "Measure", auth.ActionSearch) &&
							!auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Measure/$evaluate-measure")
							return
						}
					} else if isPlanApplyOp(parts) {
						// Detailed SMART scope and patient compartment checks are enforced in handlePlanDefinitionApply / handleActivityDefinitionApply
						// alongside baseline scope and Patient/{id} compartment isolation here.
						if len(secCtx.GetScopes()) == 0 {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for operation")
							return
						}
						if len(parts) == 6 && strings.EqualFold(resType, "Patient") {
							id := parts[4]
							if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) &&
								!auth.Allows(secCtx, "Patient", auth.ActionCreate) && !auth.Allows(secCtx, "Patient", auth.ActionUpdate) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("insufficient scope for Patient/%s", parts[5]))
								return
							}
							if auth.IsPatientRestricted(secCtx) && cleanID(id) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access %s for patient %s",
										secCtx.GetPatientId(), parts[5], id))
								return
							}
						}
					} else if isQuestionnairePopulateOp(parts) {
						hasQScope := auth.Allows(secCtx, "Questionnaire", auth.ActionRead) ||
							auth.Allows(secCtx, "Questionnaire", auth.ActionSearch) ||
							auth.Allows(secCtx, "QuestionnaireResponse", auth.ActionCreate)
						hasPatScope := auth.Allows(secCtx, "Patient", auth.ActionRead) ||
							auth.Allows(secCtx, "Patient", auth.ActionSearch)
						if !hasQScope || !hasPatScope {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Questionnaire/$populate (requires Questionnaire and Patient read/search scope)")
							return
						}
					} else if len(parts) == 5 && strings.EqualFold(resType, "DocumentReference") && parts[4] == "$docref" {
						hasDocScope := auth.Allows(secCtx, "DocumentReference", auth.ActionRead) || auth.Allows(secCtx, "DocumentReference", auth.ActionSearch)
						hasPatScope := auth.Allows(secCtx, "Patient", auth.ActionRead) || auth.Allows(secCtx, "Patient", auth.ActionSearch)
						if !hasDocScope || !hasPatScope {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for DocumentReference/$docref (requires DocumentReference and Patient read/search scope)")
							return
						}
					} else if len(parts) == 6 && strings.EqualFold(resType, "Composition") && parts[5] == "$document" {
						if !auth.Allows(secCtx, "Composition", auth.ActionRead) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Composition/$document")
							return
						}
						if auth.IsPatientRestricted(secCtx) && s.store != nil {
							exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, "Composition", parts[4], secCtx.GetPatientId())
							if err != nil {
								writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
								return
							}
							if exists && !allowed {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access Composition/%s outside patient compartment",
										secCtx.GetPatientId(), parts[4]))
								return
							}
						}
					} else if len(parts) == 6 && parts[5] == "$provenance" {
						id := parts[4]
						if !auth.Allows(secCtx, "Provenance", auth.ActionRead) && !auth.Allows(secCtx, "Provenance", auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for $provenance (requires Provenance read or search scope)")
							return
						}
						if auth.IsPatientRestricted(secCtx) {
							if strings.EqualFold(resType, "Patient") && cleanID(id) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access patient %s",
										secCtx.GetPatientId(), id))
								return
							}
							if !strings.EqualFold(resType, "Patient") && postgres.IsPatientCompartmentResourceType(resType) && s.store != nil {
								exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, resType, id, secCtx.GetPatientId())
								if err != nil {
									writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
									return
								}
								if exists && !allowed {
									writeOperationOutcome(w, http.StatusForbidden, "forbidden",
										fmt.Sprintf("patient-scoped token (%s) cannot access $provenance for %s/%s outside patient compartment",
											secCtx.GetPatientId(), resType, id))
									return
								}
							}
						}
					} else if resType == "_history" || resType == "$meta" {
						// System-level history or $meta: GET /{tenant}/_history or GET /{tenant}/$meta
						if len(secCtx.GetScopes()) == 0 {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for system operation "+resType)
							return
						}
					} else if len(parts) == 5 && (parts[4] == "_history" || parts[4] == "$meta" || (strings.EqualFold(resType, "Subscription") && (parts[4] == "$status" || parts[4] == "$events"))) {
						// Type-level history, $meta, Subscription/$status, or Subscription/$events
						if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for %s on %s", parts[4], resType))
							return
						}
						if auth.IsPatientRestricted(secCtx) && !strings.EqualFold(resType, "Patient") && !postgres.IsPatientCompartmentResourceType(resType) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("patient-scoped token (%s) cannot read %s of non-compartment resource %s",
									secCtx.GetPatientId(), parts[4], resType))
							return
						}
					} else if len(parts) == 6 && postgres.IsCompartmentType(resType) && !strings.HasPrefix(parts[5], "$") && parts[5] != "_history" {
						// Compartment search: GET /{tenant}/{compartmentType}/{compartmentId}/{targetType}
						compType := postgres.CanonicalCompartmentType(resType)
						compID := parts[4]
						targetType := parts[5]
						if auth.IsPatientRestricted(secCtx) {
							if strings.EqualFold(compType, "Patient") && cleanID(compID) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access patient %s",
										secCtx.GetPatientId(), compID))
								return
							}
							if !strings.EqualFold(compType, "Patient") && postgres.IsPatientCompartmentResourceType(compType) && s.store != nil {
								exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, compType, compID, secCtx.GetPatientId())
								if err != nil {
									writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
									return
								}
								if exists && !allowed {
									writeOperationOutcome(w, http.StatusForbidden, "forbidden",
										fmt.Sprintf("patient-scoped token (%s) cannot access %s/%s outside patient compartment",
											secCtx.GetPatientId(), compType, compID))
									return
								}
							}
						}
						if targetType == "*" {
							if len(secCtx.GetScopes()) == 0 {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for wildcard compartment search")
								return
							}
						} else if !s.enforceSearchScopeAndCompartment(w, r, secCtx, targetType) {
							return
						}
					} else if len(parts) == 6 && strings.EqualFold(resType, "Group") && parts[5] == "$everything" {
						if !auth.Allows(secCtx, "Group", auth.ActionRead) && !auth.Allows(secCtx, "Group", auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Group/$everything")
							return
						}
						if auth.IsPatientRestricted(secCtx) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"patient-scoped token cannot invoke multi-patient Group/$everything; use /Patient/{id}/$everything")
							return
						}
					} else if len(parts) >= 5 {
						// Read / VRead / Instance History / $everything / $meta / Patient/{id}/$export: GET /{tenant}/{resourceType}/{id}...
						id := parts[4]
						if !auth.Allows(secCtx, resType, auth.ActionRead) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for read on %s", resType))
							return
						}

						// Patient compartment enforcement on point reads, vread, _history, $everything, $meta, and $export
						if auth.IsPatientRestricted(secCtx) {
							if strings.EqualFold(resType, "Patient") && cleanID(id) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access patient %s",
										secCtx.GetPatientId(), id))
								return
							}
							if !strings.EqualFold(resType, "Patient") && postgres.IsPatientCompartmentResourceType(resType) && s.store != nil {
								_, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, resType, id, secCtx.GetPatientId())
								if err != nil {
									s.logger.Error("patient compartment check failed", "error", err, "type", resType, "id", id)
									writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
									return
								}
								if !allowed {
									writeOperationOutcome(w, http.StatusForbidden, "forbidden",
										fmt.Sprintf("patient-scoped token (%s) cannot access %s/%s outside patient compartment",
											secCtx.GetPatientId(), resType, id))
									return
								}
							}
						}
					} else {
						// Search: GET /{tenant}/{resourceType}
						if !s.enforceSearchScopeAndCompartment(w, r, secCtx, resType) {
							return
						}
					}

				case http.MethodPost:
					if isTerminologyOp(parts) {
						if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for terminology operation on %s", resType))
							return
						}
					} else if isMeasureOp(parts) {
						if !auth.Allows(secCtx, "Measure", auth.ActionRead) && !auth.Allows(secCtx, "Measure", auth.ActionSearch) &&
							!auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Measure/$evaluate-measure")
							return
						}
					} else if isPlanApplyOp(parts) {
						// Detailed SMART scope and patient compartment checks are enforced in handlePlanDefinitionApply / handleActivityDefinitionApply
						// alongside baseline scope and Patient/{id} compartment isolation here.
						if len(secCtx.GetScopes()) == 0 {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for operation")
							return
						}
						if len(parts) == 6 && strings.EqualFold(resType, "Patient") {
							id := parts[4]
							if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) &&
								!auth.Allows(secCtx, "Patient", auth.ActionCreate) && !auth.Allows(secCtx, "Patient", auth.ActionUpdate) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("insufficient scope for Patient/%s", parts[5]))
								return
							}
							if auth.IsPatientRestricted(secCtx) && cleanID(id) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access %s for patient %s",
										secCtx.GetPatientId(), parts[5], id))
								return
							}
						}
					} else if isQuestionnairePopulateOp(parts) {
						hasQScope := auth.Allows(secCtx, "Questionnaire", auth.ActionRead) ||
							auth.Allows(secCtx, "Questionnaire", auth.ActionSearch) ||
							auth.Allows(secCtx, "QuestionnaireResponse", auth.ActionCreate)
						hasPatScope := auth.Allows(secCtx, "Patient", auth.ActionRead) ||
							auth.Allows(secCtx, "Patient", auth.ActionSearch)
						if !hasQScope || !hasPatScope {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Questionnaire/$populate (requires Questionnaire and Patient read/search scope)")
							return
						}
					} else if isQuestionnaireExtractOp(parts) {
						if !auth.Allows(secCtx, "QuestionnaireResponse", auth.ActionRead) && !auth.Allows(secCtx, "QuestionnaireResponse", auth.ActionCreate) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for QuestionnaireResponse/$extract")
							return
						}
						if len(parts) == 6 && auth.IsPatientRestricted(secCtx) && s.store != nil {
							exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, "QuestionnaireResponse", parts[4], secCtx.GetPatientId())
							if err != nil {
								writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
								return
							}
							if exists && !allowed {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access QuestionnaireResponse/%s outside patient compartment",
										secCtx.GetPatientId(), parts[4]))
								return
							}
						}
					} else if len(parts) == 5 && strings.EqualFold(resType, "DocumentReference") && parts[4] == "$docref" {
						hasDocScope := auth.Allows(secCtx, "DocumentReference", auth.ActionRead) || auth.Allows(secCtx, "DocumentReference", auth.ActionSearch)
						hasPatScope := auth.Allows(secCtx, "Patient", auth.ActionRead) || auth.Allows(secCtx, "Patient", auth.ActionSearch)
						if !hasDocScope || !hasPatScope {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for DocumentReference/$docref (requires DocumentReference and Patient read/search scope)")
							return
						}
					} else if len(parts) == 6 && strings.EqualFold(resType, "Composition") && parts[5] == "$document" {
						if !auth.Allows(secCtx, "Composition", auth.ActionRead) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Composition/$document")
							return
						}
						if auth.IsPatientRestricted(secCtx) && s.store != nil {
							exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, "Composition", parts[4], secCtx.GetPatientId())
							if err != nil {
								writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
								return
							}
							if exists && !allowed {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access Composition/%s outside patient compartment",
										secCtx.GetPatientId(), parts[4]))
								return
							}
						}
					} else if len(parts) == 5 && resType == "cds-services" {
						if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for CDS Hooks service invocation")
							return
						}
					} else if len(parts) == 5 && strings.EqualFold(resType, "Patient") && (parts[4] == "$match" || parts[4] == "$member-match") {
						if !auth.Allows(secCtx, "Patient", auth.ActionSearch) && !auth.Allows(secCtx, "Patient", auth.ActionRead) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Patient/"+parts[4])
							return
						}
					} else if len(parts) == 5 && strings.EqualFold(resType, "Claim") && parts[4] == "$submit" {
						if !auth.Allows(secCtx, "Claim", auth.ActionCreate) && !auth.Allows(secCtx, "Claim", auth.ActionUpdate) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Claim/$submit (requires Claim create or update scope)")
							return
						}
					} else if len(parts) == 6 && strings.EqualFold(resType, "Patient") && (parts[5] == "$summary" || parts[5] == "$everything") {
						id := parts[4]
						if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for Patient/%s", parts[5]))
							return
						}
						if auth.IsPatientRestricted(secCtx) && cleanID(id) != cleanID(secCtx.GetPatientId()) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("patient-scoped token (%s) cannot access %s for patient %s",
									secCtx.GetPatientId(), parts[5], id))
							return
						}
					} else if len(parts) == 6 && strings.EqualFold(resType, "Encounter") && parts[5] == "$everything" {
						id := parts[4]
						if !auth.Allows(secCtx, "Encounter", auth.ActionRead) && !auth.Allows(secCtx, "Encounter", auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Encounter/$everything")
							return
						}
						if auth.IsPatientRestricted(secCtx) && s.store != nil {
							exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, "Encounter", id, secCtx.GetPatientId())
							if err != nil {
								writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
								return
							}
							if exists && !allowed {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access Encounter/%s outside patient compartment",
										secCtx.GetPatientId(), id))
								return
							}
						}
					} else if len(parts) == 6 && strings.EqualFold(resType, "Group") && parts[5] == "$everything" {
						if !auth.Allows(secCtx, "Group", auth.ActionRead) && !auth.Allows(secCtx, "Group", auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Group/$everything")
							return
						}
						if auth.IsPatientRestricted(secCtx) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"patient-scoped token cannot invoke multi-patient Group/$everything; use /Patient/{id}/$everything")
							return
						}
					} else if len(parts) == 6 && (parts[5] == "$meta-add" || parts[5] == "$meta-delete") {
						id := parts[4]
						if !auth.Allows(secCtx, resType, auth.ActionUpdate) && !auth.Allows(secCtx, resType, auth.ActionCreate) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for %s on %s", parts[5], resType))
							return
						}
						if auth.IsPatientRestricted(secCtx) {
							if strings.EqualFold(resType, "Patient") && cleanID(id) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot mutate meta of patient %s",
										secCtx.GetPatientId(), id))
								return
							}
							if !strings.EqualFold(resType, "Patient") && postgres.IsPatientCompartmentResourceType(resType) && s.store != nil {
								exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, resType, id, secCtx.GetPatientId())
								if err != nil {
									writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
									return
								}
								if exists && !allowed {
									writeOperationOutcome(w, http.StatusForbidden, "forbidden",
										fmt.Sprintf("patient-scoped token (%s) cannot mutate meta of %s/%s outside patient compartment",
											secCtx.GetPatientId(), resType, id))
									return
								}
							}
						}
					} else if len(parts) == 4 && resType == "_search" {
						// System-level POST search: POST /{tenant}/_search
						if len(secCtx.GetScopes()) == 0 {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for system search")
							return
						}
					} else if len(parts) == 6 && parts[5] == "$graph" {
						id := parts[4]
						if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for $graph on %s", resType))
							return
						}
						if auth.IsPatientRestricted(secCtx) {
							if strings.EqualFold(resType, "Patient") && cleanID(id) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access $graph for patient %s",
										secCtx.GetPatientId(), id))
								return
							}
							if !strings.EqualFold(resType, "Patient") && postgres.IsPatientCompartmentResourceType(resType) && s.store != nil {
								exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, resType, id, secCtx.GetPatientId())
								if err != nil {
									writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
									return
								}
								if exists && !allowed {
									writeOperationOutcome(w, http.StatusForbidden, "forbidden",
										fmt.Sprintf("patient-scoped token (%s) cannot access $graph for %s/%s outside patient compartment",
											secCtx.GetPatientId(), resType, id))
									return
								}
							}
						}
					} else if len(parts) == 7 && postgres.IsCompartmentType(resType) && !strings.HasPrefix(parts[5], "$") && parts[5] != "_history" && parts[6] == "_search" {
						// POST-based compartment search: POST /{tenant}/{compartmentType}/{compartmentId}/{targetType}/_search
						compType := postgres.CanonicalCompartmentType(resType)
						compID := parts[4]
						targetType := parts[5]
						if auth.IsPatientRestricted(secCtx) {
							if strings.EqualFold(compType, "Patient") && cleanID(compID) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot access patient %s",
										secCtx.GetPatientId(), compID))
								return
							}
							if !strings.EqualFold(compType, "Patient") && postgres.IsPatientCompartmentResourceType(compType) && s.store != nil {
								exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, compType, compID, secCtx.GetPatientId())
								if err != nil {
									writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
									return
								}
								if exists && !allowed {
									writeOperationOutcome(w, http.StatusForbidden, "forbidden",
										fmt.Sprintf("patient-scoped token (%s) cannot access %s/%s outside patient compartment",
											secCtx.GetPatientId(), compType, compID))
									return
								}
							}
						}
						if targetType == "*" {
							if len(secCtx.GetScopes()) == 0 {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for wildcard compartment search")
								return
							}
						} else if !s.enforceSearchScopeAndCompartment(w, r, secCtx, targetType) {
							return
						}
					} else if len(parts) >= 5 && parts[4] == "_search" {
						// POST-based search: POST /{tenant}/{resourceType}/_search
						if !s.enforceSearchScopeAndCompartment(w, r, secCtx, resType) {
							return
						}
					} else if len(parts) >= 5 && (parts[4] == "$validate" || (len(parts) == 6 && parts[5] == "$validate")) {
						// Validate operation: POST /{tenant}/{resourceType}/$validate or /{tenant}/{resourceType}/{id}/$validate
						if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionCreate) && !auth.Allows(secCtx, resType, auth.ActionUpdate) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for validate on %s", resType))
							return
						}
					} else if strings.EqualFold(resType, "Subscription") && ((len(parts) == 5 && (parts[4] == "$status" || parts[4] == "$events")) || (len(parts) == 6 && (parts[5] == "$status" || parts[5] == "$events"))) {
						if !auth.Allows(secCtx, "Subscription", auth.ActionRead) && !auth.Allows(secCtx, "Subscription", auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Subscription operation")
							return
						}
					} else if len(parts) == 6 && strings.EqualFold(resType, "Patient") && parts[5] == "$purge" {
						id := parts[4]
						if !auth.Allows(secCtx, "Patient", auth.ActionDelete) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"insufficient scope for Patient/$purge (requires Patient delete scope)")
							return
						}
						if auth.IsPatientRestricted(secCtx) && cleanID(id) != cleanID(secCtx.GetPatientId()) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("patient-scoped token (%s) cannot purge patient %s",
									secCtx.GetPatientId(), id))
							return
						}
					} else if resType == "$expunge" || (len(parts) == 5 && parts[4] == "$expunge") || (len(parts) == 6 && parts[5] == "$expunge") {
						if auth.IsPatientRestricted(secCtx) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								"patient-scoped tokens cannot invoke $expunge")
							return
						}
						targetType := resType
						if resType == "$expunge" {
							targetType = "*"
						}
						if !auth.Allows(secCtx, targetType, auth.ActionDelete) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for $expunge on %s (requires delete scope)", targetType))
							return
						}
					} else if resType == "$reindex" || (len(parts) == 5 && parts[4] == "$reindex") || (len(parts) == 6 && parts[5] == "$reindex") {
						// Detailed SMART scope checks are enforced in handleReindex.
					} else if resType == "$diff" || (len(parts) == 5 && parts[4] == "$diff") || (len(parts) == 6 && parts[5] == "$diff") {
						if resType == "$diff" {
							if len(secCtx.GetScopes()) == 0 {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope for $diff")
								return
							}
						} else {
							if !auth.Allows(secCtx, resType, auth.ActionRead) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("insufficient scope for $diff on %s", resType))
								return
							}
							if len(parts) == 6 && auth.IsPatientRestricted(secCtx) {
								id := parts[4]
								if strings.EqualFold(resType, "Patient") && cleanID(id) != cleanID(secCtx.GetPatientId()) {
									writeOperationOutcome(w, http.StatusForbidden, "forbidden",
										fmt.Sprintf("patient-scoped token (%s) cannot access $diff for patient %s",
											secCtx.GetPatientId(), id))
									return
								}
								if !strings.EqualFold(resType, "Patient") && postgres.IsPatientCompartmentResourceType(resType) && s.store != nil {
									exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, resType, id, secCtx.GetPatientId())
									if err != nil {
										writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
										return
									}
									if exists && !allowed {
										writeOperationOutcome(w, http.StatusForbidden, "forbidden",
											fmt.Sprintf("patient-scoped token (%s) cannot access $diff for %s/%s outside patient compartment",
												secCtx.GetPatientId(), resType, id))
										return
									}
								}
							}
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
						if auth.IsPatientRestricted(secCtx) && !strings.EqualFold(resType, "Patient") && !postgres.IsPatientCompartmentResourceType(resType) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("patient-scoped token (%s) cannot create non-compartment resource %s",
									secCtx.GetPatientId(), resType))
							return
						}
					}

				case http.MethodPut, http.MethodPatch:
					if !auth.Allows(secCtx, resType, auth.ActionUpdate) {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("insufficient scope for update on %s", resType))
						return
					}
					if auth.IsPatientRestricted(secCtx) {
						if !strings.EqualFold(resType, "Patient") && !postgres.IsPatientCompartmentResourceType(resType) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("patient-scoped token (%s) cannot update non-compartment resource %s",
									secCtx.GetPatientId(), resType))
							return
						}
						if len(parts) >= 5 {
							id := parts[4]
							if strings.EqualFold(resType, "Patient") && cleanID(id) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot update patient %s",
										secCtx.GetPatientId(), id))
								return
							}
							if !strings.EqualFold(resType, "Patient") && postgres.IsPatientCompartmentResourceType(resType) && s.store != nil {
								exists, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, resType, id, secCtx.GetPatientId())
								if err != nil {
									s.logger.Error("patient compartment check on PUT/PATCH failed", "error", err, "type", resType, "id", id)
									writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
									return
								}
								if exists && !allowed {
									writeOperationOutcome(w, http.StatusForbidden, "forbidden",
										fmt.Sprintf("patient-scoped token (%s) cannot update %s/%s belonging to another patient",
											secCtx.GetPatientId(), resType, id))
									return
								}
							}
						}
					}

				case http.MethodDelete:
					if !auth.Allows(secCtx, resType, auth.ActionDelete) {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("insufficient scope for delete on %s", resType))
						return
					}
					if auth.IsPatientRestricted(secCtx) {
						if !strings.EqualFold(resType, "Patient") && !postgres.IsPatientCompartmentResourceType(resType) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("patient-scoped token (%s) cannot delete non-compartment resource %s",
									secCtx.GetPatientId(), resType))
							return
						}
						if len(parts) >= 5 {
							id := parts[4]
							if strings.EqualFold(resType, "Patient") && cleanID(id) != cleanID(secCtx.GetPatientId()) {
								writeOperationOutcome(w, http.StatusForbidden, "forbidden",
									fmt.Sprintf("patient-scoped token (%s) cannot delete patient %s",
										secCtx.GetPatientId(), id))
								return
							}
							if !strings.EqualFold(resType, "Patient") && postgres.IsPatientCompartmentResourceType(resType) && s.store != nil {
								_, allowed, err := s.checkExistingResourceCompartment(r.Context(), reqTenant, resType, id, secCtx.GetPatientId())
								if err != nil {
									s.logger.Error("patient compartment check on DELETE failed", "error", err, "type", resType, "id", id)
									writeOperationOutcome(w, http.StatusInternalServerError, "exception", "internal security check error")
									return
								}
								if !allowed {
									writeOperationOutcome(w, http.StatusForbidden, "forbidden",
										fmt.Sprintf("patient-scoped token (%s) cannot delete %s/%s outside patient compartment",
											secCtx.GetPatientId(), resType, id))
									return
								}
							}
						}
					}
				}
			}
		}

		// Attach verified SecurityContext to request
		ctx := auth.WithSecurityContext(r.Context(), secCtx)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) enforceSearchScopeAndCompartment(w http.ResponseWriter, r *http.Request, secCtx *auth.SecurityContext, resType string) bool {
	if !auth.Allows(secCtx, resType, auth.ActionSearch) {
		writeOperationOutcome(w, http.StatusForbidden, "forbidden",
			fmt.Sprintf("insufficient scope for search on %s", resType))
		return false
	}

	if auth.IsPatientRestricted(secCtx) {
		allowedPatientID := cleanID(secCtx.GetPatientId())
		q := mergedSearchQuery(r)
		if strings.EqualFold(resType, "Patient") {
			for _, rawVal := range q["_id"] {
				for part := range strings.SplitSeq(rawVal, ",") {
					if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("patient-scoped token (%s) cannot search patient %s",
								secCtx.GetPatientId(), trimmed))
						return false
					}
				}
			}
		}

		// For clinical resources, reject any mismatched patient/subject/beneficiary filters across all query values
		for _, key := range []string{"patient", "patient._id"} {
			for _, rawVal := range q[key] {
				for part := range strings.SplitSeq(rawVal, ",") {
					if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("cannot query records of patient %s with token restricted to %s",
								trimmed, secCtx.GetPatientId()))
						return false
					}
				}
			}
		}
		for _, key := range []string{"subject", "subject._id"} {
			for _, rawVal := range q[key] {
				for part := range strings.SplitSeq(rawVal, ",") {
					if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							fmt.Sprintf("cannot query records of subject %s with token restricted to %s",
								trimmed, secCtx.GetPatientId()))
						return false
					}
				}
			}
		}
	}
	return true
}

func isPublicEndpoint(parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	if parts[0] == "oauth2" || parts[0] == "healthz" || parts[0] == "readyz" || parts[0] == "metrics" {
		return true
	}
	if len(parts) == 1 && parts[0] == "cds-services" {
		return true
	}
	for _, p := range parts {
		if strings.HasPrefix(p, ".well-known") {
			return true
		}
	}
	if len(parts) < 3 {
		return false
	}
	if len(parts) == 3 && (parts[2] == "openapi.json" || parts[2] == "$docs" || parts[2] == "cds-services" || parts[2] == "healthz" || parts[2] == "readyz" || parts[2] == "metrics") {
		return true
	}
	if len(parts) == 4 && parts[3] == "cds-services" {
		return true
	}
	if len(parts) >= 4 && (parts[3] == "metadata" || parts[3] == "openapi.json" || parts[3] == "$docs" || parts[3] == "healthz" || parts[3] == "readyz" || parts[3] == "metrics") {
		return true
	}
	return false
}

func cleanID(id string) string {
	if idx := strings.LastIndex(id, "/"); idx != -1 {
		return id[idx+1:]
	}
	return id
}

// checkExistingResourceCompartment verifies whether an existing resource (active or in history)
// belongs to the restricted patient compartment.
// Returns (exists, allowed, err).
func (s *Server) checkExistingResourceCompartment(ctx context.Context, tenant, resType, resID, patientID string) (bool, bool, error) {
	patientID = cleanID(patientID)
	if strings.EqualFold(resType, "Patient") {
		return true, cleanID(resID) == patientID, nil
	}
	if !postgres.IsPatientCompartmentResourceType(resType) {
		return true, true, nil
	}
	if s.store == nil {
		return false, false, nil
	}
	belongs, err := s.store.BelongsToPatientCompartment(ctx, tenant, resType, resID, patientID)
	if err != nil {
		return false, false, err
	}
	if belongs {
		return true, true, nil
	}
	rec, err := s.store.ReadResourceWithMeta(ctx, tenant, resType, resID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil && !errors.Is(err, postgres.ErrResourceDeleted) {
		return false, false, err
	}
	if rec != nil && !rec.IsDeleted {
		return true, false, nil
	}
	history, _, err := s.store.ListResourceHistory(ctx, tenant, resType, resID, 10, 0)
	if err != nil {
		return true, false, err
	}
	if extractor, hasExtractor := s.extractors[resType]; hasExtractor {
		for _, h := range history {
			if len(h.ResourceProto) > 0 {
				if idx, err := extractor(tenant, resID, h.ResourceProto); err == nil {
					if postgres.IndexesBelongToPatientCompartment(resType, resID, idx, patientID) {
						return true, true, nil
					}
					return true, false, nil
				}
			}
		}
	}
	return true, false, nil
}

type auditResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (w *auditResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.statusCode = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// auditMiddleware records an immutable HL7 FHIR R4 / US Core AuditEvent for every
// clinical interaction and security rejection (401/403).
func (s *Server) auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auditRecorder == nil {
			next.ServeHTTP(w, r)
			return
		}

		path := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(path, "/")

		// Only audit FHIR tenant endpoints, skipping public discovery endpoints
		if len(parts) < 3 || parts[0] != "fhir" || parts[1] != "r4" || isPublicEndpoint(parts) {
			next.ServeHTTP(w, r)
			return
		}

		holder := &auditActorHolder{}
		ctx := context.WithValue(r.Context(), auditActorKey, holder)
		arw := &auditResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(arw, r.WithContext(ctx))

		tenantID := parts[2]
		actionCode, subtypeCode, entityType, entityID, entityVersion := audit.ClassifyHTTPInteraction(
			r.Method,
			parts,
			arw.statusCode,
		)

		// Skip recording successful GET/HEAD reads/searches on AuditEvent itself to prevent
		// self-referential audit log inflation while still auditing 401/403 attempts.
		if entityType == "AuditEvent" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && arw.statusCode < 400 {
			return
		}

		// On successful POST Create (201/200) or Conditional PUT/DELETE, extract resource ID from Location header:
		// Location: /fhir/r4/{tenant}/{resourceType}/{id}
		if entityID == "" && (r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete) && arw.statusCode < 400 {
			if loc := arw.Header().Get("Location"); loc != "" {
				locParts := strings.Split(strings.Trim(loc, "/"), "/")
				if len(locParts) >= 5 && locParts[0] == "fhir" && locParts[1] == "r4" {
					entityID = locParts[4]
				}
			}
		}

		if entityVersion == "" {
			if etag := arw.Header().Get("ETag"); etag != "" {
				if v, err := postgres.ParseETagVersion(etag); err == nil && v > 0 {
					entityVersion = strconv.Itoa(v)
				}
			}
		}

		actorSubject := holder.actorSubject
		if actorSubject == "" {
			actorSubject = "anonymous"
		}

		patientID := holder.patientID
		if patientID == "" && strings.EqualFold(entityType, "Patient") && entityID != "" {
			patientID = cleanID(entityID)
		}
		if patientID == "" {
			q := r.URL.Query()
			if p := q.Get("patient"); p != "" {
				patientID = cleanID(p)
			} else if sub := q.Get("subject"); sub != "" {
				patientID = cleanID(sub)
			}
		}
		if patientID == "" && postgres.IsPatientCompartmentResourceType(entityType) && entityID != "" {
			patientID = s.resolveClinicalResourcePatientID(r.Context(), tenantID, entityType, entityID)
		}

		outcomeCode, outcomeDesc := audit.ClassifyHTTPOutcome(arw.statusCode)

		rec := postgres.AuditRecord{
			TenantID:      tenantID,
			AuditID:       uuid.NewString(),
			Recorded:      time.Now().UTC(),
			Action:        actionCode,
			SubtypeCode:   subtypeCode,
			Outcome:       outcomeCode,
			OutcomeDesc:   outcomeDesc,
			HTTPMethod:    r.Method,
			HTTPStatus:    arw.statusCode,
			RequestURI:    r.URL.RequestURI(),
			AgentSubject:  actorSubject,
			AgentPatient:  patientID,
			ClientIP:      extractClientIP(r),
			EntityType:    entityType,
			EntityID:      entityID,
			EntityVersion: entityVersion,
		}

		if err := s.auditRecorder.Record(r.Context(), rec); err != nil {
			s.logger.Warn("failed to record audit event", "error", err, "path", r.URL.Path)
		}
	})
}

func setAuditPatientID(ctx context.Context, patientID string) {
	if patientID == "" {
		return
	}
	if holder, ok := ctx.Value(auditActorKey).(*auditActorHolder); ok && holder != nil && holder.patientID == "" {
		holder.patientID = cleanID(patientID)
	}
}

func extractPatientIDFromIndexes(resType, resID string, idx *postgres.SearchIndexes) string {
	if strings.EqualFold(resType, "Patient") {
		return cleanID(resID)
	}
	if idx == nil {
		return ""
	}
	for _, ref := range idx.References {
		if (ref.SpName == "subject" || ref.SpName == "patient") && strings.EqualFold(ref.TargetType, "Patient") && ref.TargetID != "" {
			return cleanID(ref.TargetID)
		}
	}
	return ""
}

func (s *Server) resolveClinicalResourcePatientID(ctx context.Context, tenant, resType, resID string) string {
	if s.store == nil || !postgres.IsPatientCompartmentResourceType(resType) {
		return ""
	}
	extractor, hasExtractor := s.extractors[resType]
	if !hasExtractor {
		return ""
	}
	rec, err := s.store.ReadResourceWithMeta(ctx, tenant, resType, resID)
	if err == nil && rec != nil && len(rec.ResourceProto) > 0 {
		if idx, err := extractor(tenant, resID, rec.ResourceProto); err == nil {
			if pid := extractPatientIDFromIndexes(resType, resID, idx); pid != "" {
				return pid
			}
		}
	}
	history, _, err := s.store.ListResourceHistory(ctx, tenant, resType, resID, 5, 0)
	if err == nil {
		for _, h := range history {
			if len(h.ResourceProto) > 0 {
				if idx, err := extractor(tenant, resID, h.ResourceProto); err == nil {
					if pid := extractPatientIDFromIndexes(resType, resID, idx); pid != "" {
						return pid
					}
				}
			}
		}
	}
	return ""
}

func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func isTerminologyOp(parts []string) bool {
	if len(parts) == 5 {
		return (strings.EqualFold(parts[3], "ValueSet") && (parts[4] == "$expand" || parts[4] == "$validate-code")) ||
			(strings.EqualFold(parts[3], "CodeSystem") && (parts[4] == "$lookup" || parts[4] == "$validate-code" || parts[4] == "$subsumes" || parts[4] == "$find-matches")) ||
			(strings.EqualFold(parts[3], "ConceptMap") && parts[4] == "$translate")
	}
	if len(parts) == 6 {
		return (strings.EqualFold(parts[3], "ValueSet") && (parts[5] == "$expand" || parts[5] == "$validate-code")) ||
			(strings.EqualFold(parts[3], "CodeSystem") && (parts[5] == "$lookup" || parts[5] == "$validate-code" || parts[5] == "$subsumes" || parts[5] == "$find-matches")) ||
			(strings.EqualFold(parts[3], "ConceptMap") && parts[5] == "$translate")
	}
	return false
}

func isMeasureOp(parts []string) bool {
	if len(parts) == 5 {
		return strings.EqualFold(parts[3], "Measure") && (parts[4] == "$evaluate-measure" || parts[4] == "$care-gaps")
	}
	if len(parts) == 6 {
		return strings.EqualFold(parts[3], "Measure") && (parts[5] == "$evaluate-measure" || parts[5] == "$care-gaps")
	}
	return false
}

func isQuestionnairePopulateOp(parts []string) bool {
	if len(parts) == 5 {
		return strings.EqualFold(parts[3], "Questionnaire") && parts[4] == "$populate"
	}
	if len(parts) == 6 {
		return strings.EqualFold(parts[3], "Questionnaire") && parts[5] == "$populate"
	}
	return false
}

func isQuestionnaireExtractOp(parts []string) bool {
	if len(parts) == 5 {
		return strings.EqualFold(parts[3], "QuestionnaireResponse") && parts[4] == "$extract"
	}
	if len(parts) == 6 {
		return strings.EqualFold(parts[3], "QuestionnaireResponse") && parts[5] == "$extract"
	}
	return false
}

func isPlanApplyOp(parts []string) bool {
	if len(parts) == 4 {
		return parts[3] == "$closure" || parts[3] == "$data-requirements" || parts[3] == "$fhirpath" || parts[3] == "$process-message" || parts[3] == "$process-notification" || parts[3] == "$davinci-data-export" || parts[3] == "$convert" || parts[3] == "$submit-attachment" || parts[3] == "$iti-65"
	}
	if len(parts) == 5 {
		if parts[4] == "$convert" {
			return true
		}
		if strings.EqualFold(parts[3], "Bundle") && (parts[4] == "$iti-65" || parts[4] == "$vrdr-submit" || parts[4] == "$bfdr-submit" || parts[4] == "$cdex-sign" || parts[4] == "$cdex-verify-signature" || parts[4] == "$ecr-submit" || parts[4] == "$cancer-registry-submit") {
			return true
		}
		if strings.EqualFold(parts[3], "Immunization") && parts[4] == "$iis-submit" {
			return true
		}
		if strings.EqualFold(parts[3], "DocumentReference") && parts[4] == "$iti-67" {
			return true
		}
		if strings.EqualFold(parts[3], "Condition") && parts[4] == "$stage-cancer" {
			return true
		}
		if strings.EqualFold(parts[3], "Procedure") && parts[4] == "$mcode-radiotherapy-record" {
			return true
		}
		if strings.EqualFold(parts[3], "Coverage") && parts[4] == "$formulary-coverage" {
			return true
		}
		if strings.EqualFold(parts[3], "Task") && (parts[4] == "$submit-attachment" || parts[4] == "$cdex-request" || parts[4] == "$cdex-poll" || parts[4] == "$cdex-questionnaire-request" || parts[4] == "$pct-gfe-coordination" || parts[4] == "$sdoh-fulfill" || parts[4] == "$cdex-submit-attachment") {
			return true
		}
		if strings.EqualFold(parts[3], "Library") && parts[4] == "$evaluate" {
			return true
		}
		if (strings.EqualFold(parts[3], "Patient") || strings.EqualFold(parts[3], "Encounter") || strings.EqualFold(parts[3], "Group")) && parts[4] == "$everything" {
			return true
		}
		if strings.EqualFold(parts[3], "Encounter") && (parts[4] == "$admit" || parts[4] == "$ecr-report" || parts[4] == "$nhsn-hai-report" || parts[4] == "$pacio-timepoint" || parts[4] == "$pacio-timepoints") {
			return true
		}
		if strings.EqualFold(parts[3], "QuestionnaireResponse") && (parts[4] == "$pacio-assess" || parts[4] == "$sdoh-assess") {
			return true
		}
		if (strings.EqualFold(parts[3], "PlanDefinition") || strings.EqualFold(parts[3], "ActivityDefinition") || strings.EqualFold(parts[3], "ChargeItem")) && parts[4] == "$apply" {
			return true
		}
		if strings.EqualFold(parts[3], "PlanDefinition") && (parts[4] == "$ersd-lookup" || parts[4] == "$medmorph-kar") {
			return true
		}
		if strings.EqualFold(parts[3], "ExplanationOfBenefit") && (parts[4] == "$carin-bb-summary" || parts[4] == "$pct-aeob-summary" || parts[4] == "$carin-bb-claims") {
			return true
		}
		if (strings.EqualFold(parts[3], "Library") || strings.EqualFold(parts[3], "Measure") || strings.EqualFold(parts[3], "PlanDefinition") || strings.EqualFold(parts[3], "ActivityDefinition") || strings.EqualFold(parts[3], "Questionnaire")) && parts[4] == "$data-requirements" {
			return true
		}
		if strings.EqualFold(parts[3], "CoverageEligibilityRequest") && parts[4] == "$submit" {
			return true
		}
		if strings.EqualFold(parts[3], "GraphDefinition") && parts[4] == "$graph" {
			return true
		}
		if strings.EqualFold(parts[3], "ConceptMap") && parts[4] == "$closure" {
			return true
		}
		if strings.EqualFold(parts[3], "Measure") && (parts[4] == "$collect-data" || parts[4] == "$submit-data" || parts[4] == "$evaluate-risk-condition-category" || parts[4] == "$nhsn-acute-care" || parts[4] == "$nhsn-dqm-evaluate") {
			return true
		}
		if strings.EqualFold(parts[3], "Observation") && (parts[4] == "$lastn" || parts[4] == "$stats" || parts[4] == "$reportable-labs" || parts[4] == "$mcode-disease-status" || parts[4] == "$pacio-splasch-record") {
			return true
		}
		if strings.EqualFold(parts[3], "DiagnosticReport") && (parts[4] == "$elr-submit" || parts[4] == "$mcode-genomics-report" || parts[4] == "$nbs-submit") {
			return true
		}
		if strings.EqualFold(parts[3], "MeasureReport") && (parts[4] == "$ra-remediate" || parts[4] == "$vbpr-performance" || parts[4] == "$vbpr-attest" || parts[4] == "$nhsn-aur-submit" || parts[4] == "$ra-evaluate-risk") {
			return true
		}
		if strings.EqualFold(parts[3], "Communication") && (parts[4] == "$rr-process" || parts[4] == "$cdex-submit-attachment") {
			return true
		}
		if strings.EqualFold(parts[3], "Patient") && (parts[4] == "$merge" || parts[4] == "$health-cards-issue" || parts[4] == "$pcde" || parts[4] == "$immds-forecast" || parts[4] == "$gfe" || parts[4] == "$ihe-pix" || parts[4] == "$bulk-member-match" || parts[4] == "$sdoh-summary" || parts[4] == "$carin-bb" || parts[4] == "$decease" || parts[4] == "$mcode-summary" || parts[4] == "$find-subject-variants" || parts[4] == "$find-subject-tx-implications" || parts[4] == "$pacio-summary" || parts[4] == "$birth-report" || parts[4] == "$ihe-pdqm" || parts[4] == "$physical-activity-summary" || parts[4] == "$adi-record" || parts[4] == "$ra-coding-gaps" || parts[4] == "$pdex-WriteData" || parts[4] == "$record-occupation" || parts[4] == "$odh-summary" || parts[4] == "$cancer-registry-report" || parts[4] == "$pacio-cognitive-assess" || parts[4] == "$pacio-cognitive-summary" || parts[4] == "$idi-match" || parts[4] == "$pacio-pfe-record" || parts[4] == "$pacio-pfe-summary" || parts[4] == "$mcode-radiotherapy-summary" || parts[4] == "$iis-history" || parts[4] == "$mcode-genomics-summary" || parts[4] == "$nbs-summary" || parts[4] == "$ecr-summary" || parts[4] == "$elr-summary" || parts[4] == "$rtpbc-summary" || parts[4] == "$nhsn-hai-summary" || parts[4] == "$mcode-trial-match" || parts[4] == "$ra-coding-gap-report" || parts[4] == "$nhsn-ach-summary" || parts[4] == "$burden-reduction-summary" || parts[4] == "$mcode-trial-summary" || parts[4] == "$pacio-splasch-summary" || parts[4] == "$vulcan-rwd-summary") {
			return true
		}
		if strings.EqualFold(parts[3], "Group") && (parts[4] == "$confirm-attribution-list" || parts[4] == "$atr-summary") {
			return true
		}
		if strings.EqualFold(parts[3], "ClaimResponse") && parts[4] == "$rtpbc-coverage" {
			return true
		}
		if strings.EqualFold(parts[3], "Person") && parts[4] == "$verify-identity" {
			return true
		}
		if strings.EqualFold(parts[3], "Endpoint") && parts[4] == "$fast-endpoint-query" {
			return true
		}
		if strings.EqualFold(parts[3], "Organization") && (parts[4] == "$fast-verify" || parts[4] == "$ndh-directory-summary") {
			return true
		}
		if strings.EqualFold(parts[3], "VerificationResult") && parts[4] == "$ndh-verify" {
			return true
		}
		if strings.EqualFold(parts[3], "OrganizationAffiliation") && parts[4] == "$fast-network-query" {
			return true
		}
		if (strings.EqualFold(parts[3], "MedicinalProduct") || strings.EqualFold(parts[3], "MedicinalProductDefinition")) && parts[4] == "$epi-document" {
			return true
		}
		if (strings.EqualFold(parts[3], "MedicinalProductAuthorization") || strings.EqualFold(parts[3], "RegulatedAuthorization")) && parts[4] == "$evaluate-authorization" {
			return true
		}
		if (strings.EqualFold(parts[3], "PractitionerRole") && parts[4] == "$find-providers") || (strings.EqualFold(parts[3], "HealthcareService") && (parts[4] == "$find-services" || parts[4] == "$fast-attest")) {
			return true
		}
		if strings.EqualFold(parts[3], "ResearchStudy") && (parts[4] == "$evaluate-eligibility" || parts[4] == "$rwd-cohort" || parts[4] == "$vulcan-summary" || parts[4] == "$vulcan-rwd-cohort") {
			return true
		}
		if strings.EqualFold(parts[3], "ResearchSubject") && (parts[4] == "$enroll" || parts[4] == "$vulcan-soa-timeline" || parts[4] == "$vulcan-esource-extract") {
			return true
		}
		if strings.EqualFold(parts[3], "AdverseEvent") && parts[4] == "$vulcan-icsr-report" {
			return true
		}
		if strings.EqualFold(parts[3], "Schedule") && parts[4] == "$find" {
			return true
		}
		if (strings.EqualFold(parts[3], "ServiceRequest") || strings.EqualFold(parts[3], "MedicationRequest")) && parts[4] == "$crd" {
			return true
		}
		if strings.EqualFold(parts[3], "ServiceRequest") && (parts[4] == "$collect-specimen" || parts[4] == "$sdoh-referral" || parts[4] == "$physical-activity-referral") {
			return true
		}
		if (strings.EqualFold(parts[3], "AdverseEvent") || strings.EqualFold(parts[3], "CarePlan") || strings.EqualFold(parts[3], "RiskAssessment") || strings.EqualFold(parts[3], "DiagnosticReport") || strings.EqualFold(parts[3], "Consent")) && parts[4] == "$evaluate" {
			return true
		}
		if strings.EqualFold(parts[3], "Consent") && parts[4] == "$adi-verify" {
			return true
		}
		if strings.EqualFold(parts[3], "Appointment") && (parts[4] == "$find" || parts[4] == "$hold" || parts[4] == "$book") {
			return true
		}
		if (strings.EqualFold(parts[3], "CapabilityStatement") || strings.EqualFold(parts[3], "metadata")) && (parts[4] == "$subset" || parts[4] == "$implements") {
			return true
		}
		if strings.EqualFold(parts[3], "StructureDefinition") && parts[4] == "$snapshot" {
			return true
		}
		if strings.EqualFold(parts[3], "MessageHeader") && (parts[4] == "$process-message" || parts[4] == "$process-notification") {
			return true
		}
		if strings.EqualFold(parts[3], "StructureMap") && parts[4] == "$transform" {
			return true
		}
		if strings.EqualFold(parts[3], "NamingSystem") && parts[4] == "$preferred-id" {
			return true
		}
		if strings.EqualFold(parts[3], "Claim") && (parts[4] == "$inquire" || parts[4] == "$predetermine" || parts[4] == "$gfe-submit" || parts[4] == "$rtpbc-submit" || parts[4] == "$pas-inquire") {
			return true
		}
		if strings.EqualFold(parts[3], "Questionnaire") && (parts[4] == "$package" || parts[4] == "$dtr-questionnaire-package") {
			return true
		}
		if strings.EqualFold(parts[3], "List") && parts[4] == "$find" {
			return true
		}
		if strings.EqualFold(parts[3], "Composition") && (parts[4] == "$document" || parts[4] == "$bfdr-summary" || parts[4] == "$adi-document" || parts[4] == "$eicr-document" || parts[4] == "$vrdr-document" || parts[4] == "$bfdr-document" || parts[4] == "$pacio-toc-bundle") {
			return true
		}
		if strings.EqualFold(parts[3], "DocumentReference") && parts[4] == "$ihe-iti-68" {
			return true
		}
		if strings.EqualFold(parts[3], "MedicationKnowledge") && (parts[4] == "$lookup" || parts[4] == "$formulary-lookup" || parts[4] == "$formulary-alternatives") {
			return true
		}
		if strings.EqualFold(parts[3], "MedicationRequest") && (parts[4] == "$check-interactions" || parts[4] == "$dispense") {
			return true
		}
		return false
	}
	if len(parts) == 6 {
		if strings.EqualFold(parts[3], "Condition") && parts[5] == "$stage-cancer" {
			return true
		}
		if strings.EqualFold(parts[3], "Immunization") && parts[5] == "$iis-submit" {
			return true
		}
		if strings.EqualFold(parts[3], "Bundle") && (parts[5] == "$cdex-sign" || parts[5] == "$cdex-verify-signature" || parts[5] == "$ecr-submit" || parts[5] == "$cancer-registry-submit") {
			return true
		}
		if strings.EqualFold(parts[3], "Procedure") && parts[5] == "$mcode-radiotherapy-record" {
			return true
		}
		if strings.EqualFold(parts[3], "Coverage") && parts[5] == "$formulary-coverage" {
			return true
		}
		if (strings.EqualFold(parts[3], "PlanDefinition") || strings.EqualFold(parts[3], "ActivityDefinition") || strings.EqualFold(parts[3], "ChargeItem")) && parts[5] == "$apply" {
			return true
		}
		if (strings.EqualFold(parts[3], "Library") || strings.EqualFold(parts[3], "Measure") || strings.EqualFold(parts[3], "PlanDefinition") || strings.EqualFold(parts[3], "ActivityDefinition") || strings.EqualFold(parts[3], "Questionnaire")) && parts[5] == "$data-requirements" {
			return true
		}
		if strings.EqualFold(parts[3], "CoverageEligibilityRequest") && parts[5] == "$submit" {
			return true
		}
		if strings.EqualFold(parts[3], "GraphDefinition") && parts[5] == "$graph" {
			return true
		}
		if strings.EqualFold(parts[3], "Task") && (parts[5] == "$submit-attachment" || parts[5] == "$cdex-request" || parts[5] == "$cdex-poll" || parts[5] == "$cdex-questionnaire-request" || parts[5] == "$pct-gfe-coordination" || parts[5] == "$sdoh-fulfill" || parts[5] == "$cdex-submit-attachment") {
			return true
		}
		if strings.EqualFold(parts[3], "Library") && parts[5] == "$evaluate" {
			return true
		}
		if strings.EqualFold(parts[3], "Patient") && (parts[5] == "$health-cards-issue" || parts[5] == "$pcde" || parts[5] == "$immds-forecast" || parts[5] == "$gfe" || parts[5] == "$ihe-pix" || parts[5] == "$sdoh-summary" || parts[5] == "$carin-bb" || parts[5] == "$decease" || parts[5] == "$mcode-summary" || parts[5] == "$find-subject-variants" || parts[5] == "$find-subject-tx-implications" || parts[5] == "$pacio-summary" || parts[5] == "$birth-report" || parts[5] == "$physical-activity-summary" || parts[5] == "$adi-record" || parts[5] == "$ra-coding-gaps" || parts[5] == "$pdex-WriteData" || parts[5] == "$record-occupation" || parts[5] == "$odh-summary" || parts[5] == "$cancer-registry-report" || parts[5] == "$pacio-cognitive-assess" || parts[5] == "$pacio-cognitive-summary" || parts[5] == "$pacio-pfe-record" || parts[5] == "$pacio-pfe-summary" || parts[5] == "$mcode-radiotherapy-summary" || parts[5] == "$iis-history" || parts[5] == "$mcode-genomics-summary" || parts[5] == "$nbs-summary" || parts[5] == "$ecr-summary" || parts[5] == "$elr-summary" || parts[5] == "$rtpbc-summary" || parts[5] == "$nhsn-hai-summary" || parts[5] == "$mcode-trial-match" || parts[5] == "$ra-coding-gap-report" || parts[5] == "$nhsn-ach-summary" || parts[5] == "$burden-reduction-summary" || parts[5] == "$mcode-trial-summary" || parts[5] == "$pacio-splasch-summary" || parts[5] == "$vulcan-rwd-summary") {
			return true
		}
		if strings.EqualFold(parts[3], "Observation") && (parts[5] == "$mcode-disease-status" || parts[5] == "$pacio-splasch-record") {
			return true
		}
		if strings.EqualFold(parts[3], "OrganizationAffiliation") && parts[5] == "$fast-network-query" {
			return true
		}
		if strings.EqualFold(parts[3], "HealthcareService") && parts[5] == "$fast-attest" {
			return true
		}
		if strings.EqualFold(parts[3], "PlanDefinition") && parts[5] == "$medmorph-kar" {
			return true
		}
		if strings.EqualFold(parts[3], "Composition") && (parts[5] == "$eicr-document" || parts[5] == "$vrdr-document" || parts[5] == "$bfdr-document" || parts[5] == "$pacio-toc-bundle") {
			return true
		}
		if strings.EqualFold(parts[3], "Consent") && parts[5] == "$adi-verify" {
			return true
		}
		if strings.EqualFold(parts[3], "Communication") && (parts[5] == "$rr-process" || parts[5] == "$cdex-submit-attachment") {
			return true
		}
		if strings.EqualFold(parts[3], "MedicationKnowledge") && parts[5] == "$formulary-alternatives" {
			return true
		}
		if strings.EqualFold(parts[3], "DiagnosticReport") && (parts[5] == "$elr-submit" || parts[5] == "$mcode-genomics-report" || parts[5] == "$nbs-submit") {
			return true
		}
		if strings.EqualFold(parts[3], "ExplanationOfBenefit") && (parts[5] == "$carin-bb-summary" || parts[5] == "$pct-aeob-summary" || parts[5] == "$carin-bb-claims") {
			return true
		}
		if strings.EqualFold(parts[3], "AdverseEvent") && parts[5] == "$vulcan-icsr-report" {
			return true
		}
		if strings.EqualFold(parts[3], "MeasureReport") && (parts[5] == "$ra-remediate" || parts[5] == "$vbpr-performance" || parts[5] == "$vbpr-attest" || parts[5] == "$nhsn-aur-submit" || parts[5] == "$ra-evaluate-risk") {
			return true
		}
		if strings.EqualFold(parts[3], "Organization") && (parts[5] == "$fast-verify" || parts[5] == "$ndh-directory-summary") {
			return true
		}
		if strings.EqualFold(parts[3], "VerificationResult") && parts[5] == "$ndh-verify" {
			return true
		}
		if (strings.EqualFold(parts[3], "MedicinalProduct") || strings.EqualFold(parts[3], "MedicinalProductDefinition")) && parts[5] == "$epi-document" {
			return true
		}
		if (strings.EqualFold(parts[3], "MedicinalProductAuthorization") || strings.EqualFold(parts[3], "RegulatedAuthorization")) && parts[5] == "$evaluate-authorization" {
			return true
		}
		if strings.EqualFold(parts[3], "QuestionnaireResponse") && (parts[5] == "$pacio-assess" || parts[5] == "$sdoh-assess") {
			return true
		}
		if strings.EqualFold(parts[3], "Encounter") && (parts[5] == "$transfer" || parts[5] == "$discharge" || parts[5] == "$ecr-report" || parts[5] == "$nhsn-hai-report" || parts[5] == "$pacio-timepoint" || parts[5] == "$pacio-timepoints") {
			return true
		}
		if strings.EqualFold(parts[3], "ClaimResponse") && parts[5] == "$rtpbc-coverage" {
			return true
		}
		if strings.EqualFold(parts[3], "Person") && parts[5] == "$verify-identity" {
			return true
		}
		if strings.EqualFold(parts[3], "ResearchStudy") && (parts[5] == "$evaluate-eligibility" || parts[5] == "$rwd-cohort" || parts[5] == "$vulcan-summary" || parts[5] == "$vulcan-rwd-cohort") {
			return true
		}
		if strings.EqualFold(parts[3], "ResearchSubject") && (parts[5] == "$enroll" || parts[5] == "$vulcan-soa-timeline" || parts[5] == "$vulcan-esource-extract") {
			return true
		}
		if strings.EqualFold(parts[3], "DocumentReference") && parts[5] == "$ihe-iti-68" {
			return true
		}
		if strings.EqualFold(parts[3], "Measure") && (parts[5] == "$collect-data" || parts[5] == "$submit-data" || parts[5] == "$evaluate-risk-condition-category" || parts[5] == "$nhsn-dqm-evaluate") {
			return true
		}
		if strings.EqualFold(parts[3], "Schedule") && parts[5] == "$find" {
			return true
		}
		if (strings.EqualFold(parts[3], "ServiceRequest") || strings.EqualFold(parts[3], "MedicationRequest")) && parts[5] == "$crd" {
			return true
		}
		if strings.EqualFold(parts[3], "ServiceRequest") && (parts[5] == "$collect-specimen" || parts[5] == "$sdoh-referral") {
			return true
		}
		if (strings.EqualFold(parts[3], "AdverseEvent") || strings.EqualFold(parts[3], "CarePlan") || strings.EqualFold(parts[3], "RiskAssessment") || strings.EqualFold(parts[3], "DiagnosticReport") || strings.EqualFold(parts[3], "Consent")) && parts[5] == "$evaluate" {
			return true
		}
		if strings.EqualFold(parts[3], "Group") && (parts[5] == "$add" || parts[5] == "$remove" || parts[5] == "$member-add" || parts[5] == "$member-remove" || parts[5] == "$confirm-attribution-list" || parts[5] == "$davinci-data-export" || parts[5] == "$atr-summary") {
			return true
		}
		if strings.EqualFold(parts[3], "MedicationKnowledge") && parts[5] == "$lookup" {
			return true
		}
		if strings.EqualFold(parts[3], "MedicationRequest") && (parts[5] == "$check-interactions" || parts[5] == "$dispense") {
			return true
		}
		if strings.EqualFold(parts[3], "Appointment") && (parts[5] == "$hold" || parts[5] == "$book") {
			return true
		}
		if strings.EqualFold(parts[3], "StructureDefinition") && parts[5] == "$snapshot" {
			return true
		}
		if strings.EqualFold(parts[3], "StructureMap") && parts[5] == "$transform" {
			return true
		}
		if strings.EqualFold(parts[3], "Claim") && (parts[5] == "$inquire" || parts[5] == "$predetermine" || parts[5] == "$rtpbc-submit" || parts[5] == "$pas-inquire") {
			return true
		}
		if strings.EqualFold(parts[3], "Questionnaire") && (parts[5] == "$package" || parts[5] == "$dtr-questionnaire-package") {
			return true
		}
		if parts[5] == "$fhirpath" {
			return true
		}
		return false
	}
	return false
}
