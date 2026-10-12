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

		if holder, ok := r.Context().Value(auditActorKey).(*auditActorHolder); ok && holder != nil {
			holder.actorSubject = secCtx.GetSubject()
			holder.patientID = cleanID(secCtx.GetPatientId())
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

		isValidateReq := r.Method == http.MethodPost && ((len(parts) >= 4 && parts[3] == "$validate") || (len(parts) >= 5 && parts[4] == "$validate"))
		if auth.IsPatientRestricted(secCtx) && !isValidateReq && cleanID(secCtx.GetPatientId()) == "" {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "patient-scoped token is missing required patient_id claim")
			return
		}

		// Check SMART scopes based on request shape
		if len(parts) >= 4 {
			resType := parts[3]
			isBulkExportOp := resType == "$export" || resType == "$export-poll" || resType == "$export-file" || resType == "$certify" ||
				(strings.EqualFold(resType, "Patient") && len(parts) == 5 && parts[4] == "$export") ||
				(strings.EqualFold(resType, "Group") && len(parts) == 6 && parts[5] == "$export")

			if !isBulkExportOp {
				switch r.Method {
				case http.MethodGet:
					if len(parts) >= 5 {
						// Read / VRead / History / $everything / Patient/{id}/$export: GET /{tenant}/{resourceType}/{id}...
						id := parts[4]
						if !auth.Allows(secCtx, resType, auth.ActionRead) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for read on %s", resType))
							return
						}

						// Patient compartment enforcement on point reads, vread, _history, $everything, and $export
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
						if !auth.Allows(secCtx, resType, auth.ActionSearch) {
							writeOperationOutcome(w, http.StatusForbidden, "forbidden",
								fmt.Sprintf("insufficient scope for search on %s", resType))
							return
						}

						// Patient compartment enforcement in search query parameters
						if auth.IsPatientRestricted(secCtx) {
							allowedPatientID := cleanID(secCtx.GetPatientId())
							q := r.URL.Query()
							if strings.EqualFold(resType, "Patient") {
								for _, rawVal := range q["_id"] {
									for part := range strings.SplitSeq(rawVal, ",") {
										if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
											writeOperationOutcome(w, http.StatusForbidden, "forbidden",
												fmt.Sprintf("patient-scoped token (%s) cannot search patient %s",
													secCtx.GetPatientId(), trimmed))
											return
										}
									}
								}
							}

							// For clinical resources, reject any mismatched patient/subject filters across all query values
							for _, key := range []string{"patient", "patient._id"} {
								for _, rawVal := range q[key] {
									for part := range strings.SplitSeq(rawVal, ",") {
										if trimmed := strings.TrimSpace(part); trimmed != "" && cleanID(trimmed) != allowedPatientID {
											writeOperationOutcome(w, http.StatusForbidden, "forbidden",
												fmt.Sprintf("cannot query records of patient %s with token restricted to %s",
													trimmed, secCtx.GetPatientId()))
											return
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
											return
										}
									}
								}
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

func isPublicEndpoint(parts []string) bool {
	if len(parts) < 3 {
		return false
	}
	if len(parts) == 3 && (parts[2] == "openapi.json" || parts[2] == "$docs") {
		return true
	}
	if len(parts) >= 4 && (parts[3] == "metadata" || parts[3] == "openapi.json" || parts[3] == "$docs") {
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

		// Skip recording successful GET reads/searches on AuditEvent itself to prevent
		// self-referential audit log inflation while still auditing 401/403 attempts.
		if entityType == "AuditEvent" && r.Method == http.MethodGet && arw.statusCode < 400 {
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
