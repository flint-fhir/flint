package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/shlink"
	"github.com/flint-fhir/flint/store"
)

type serverSHLinkStore struct {
	srv *Server
}

func (st *serverSHLinkStore) CreateLink(id, tenant string, rawJSON []byte) error {
	if st == nil || st.srv == nil || st.srv.store == nil {
		return nil
	}
	_, ok, err := st.srv.store.CompareAndSwapClusterState(context.Background(), 0, store.ClusterStateRecord{
		Namespace: "shlink",
		Key:       id,
		TenantID:  tenant,
		Value:     rawJSON,
	})
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("shlink: duplicate link ID")
	}
	return nil
}

func (st *serverSHLinkStore) GetLink(id string) ([]byte, int64, bool, error) {
	if st == nil || st.srv == nil || st.srv.store == nil {
		return nil, 0, false, nil
	}
	rec, ok, err := st.srv.store.GetClusterState(context.Background(), "shlink", id)
	if err != nil || !ok || rec == nil {
		return nil, 0, false, err
	}
	return rec.Value, rec.Version, true, nil
}

func (st *serverSHLinkStore) CompareAndSwapLink(id, tenant string, expectedVersion int64, rawJSON []byte) (bool, error) {
	if st == nil || st.srv == nil || st.srv.store == nil {
		return false, nil
	}
	_, ok, err := st.srv.store.CompareAndSwapClusterState(context.Background(), expectedVersion, store.ClusterStateRecord{
		Namespace: "shlink",
		Key:       id,
		TenantID:  tenant,
		Value:     rawJSON,
	})
	return ok, err
}

// SHLinkManager returns the server's SMART Health Links manager.
func (s *Server) SHLinkManager() *shlink.Manager {
	if s.shlinks == nil {
		m := shlink.NewManager()
		if s.store != nil {
			m.SetBackendStore(&serverSHLinkStore{srv: s})
		}
		s.shlinks = m
	}
	return s.shlinks
}

type shlinkIssueOptions struct {
	PatientID        string
	Label            string
	Passcode         string
	ExpiresInSeconds int64
	ExpUnix          int64
	MaxViews         int
	IncludeIPS       bool
	IncludeSHC       bool
	LongTerm         bool
	ViewerURL        string
}

func parseSHLinkIssueOptions(r *http.Request) shlinkIssueOptions {
	opts := shlinkIssueOptions{
		PatientID:        cleanID(strings.TrimSpace(r.URL.Query().Get("patient"))),
		Label:            strings.TrimSpace(r.URL.Query().Get("label")),
		Passcode:         strings.TrimSpace(r.URL.Query().Get("passcode")),
		ExpiresInSeconds: 86400, // default 24h expiration
		IncludeIPS:       true,
		IncludeSHC:       true,
		ViewerURL:        strings.TrimSpace(r.URL.Query().Get("viewerUrl")),
	}
	if v := strings.TrimSpace(r.URL.Query().Get("maxViews")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.MaxViews = n
		}
	}
	if r.Body == nil {
		return opts
	}
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil || len(strings.TrimSpace(string(rawBody))) == 0 {
		return opts
	}
	var doc map[string]any
	if json.Unmarshal(rawBody, &doc) != nil {
		return opts
	}

	if doc["resourceType"] == "Parameters" {
		rawParams, _ := doc["parameter"].([]any)
		for _, item := range rawParams {
			pm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := pm["name"].(string)
			switch name {
			case "patient":
				if sVal := firstStringValue(pm, "valueString", "valueId"); sVal != "" {
					opts.PatientID = cleanID(sVal)
				} else if refMap, ok := pm["valueReference"].(map[string]any); ok {
					if refStr, _ := refMap["reference"].(string); refStr != "" {
						opts.PatientID = cleanID(refStr)
					}
				}
			case "label":
				opts.Label = firstStringValue(pm, "valueString")
			case "passcode":
				opts.Passcode = firstStringValue(pm, "valueString")
			case "viewerUrl":
				opts.ViewerURL = firstStringValue(pm, "valueUrl", "valueUri", "valueString")
			case "expiresInSeconds":
				if n, ok := int64FromMap(pm, "valueInteger"); ok {
					opts.ExpiresInSeconds = n
				}
			case "exp":
				if n, ok := int64FromMap(pm, "valueInteger"); ok {
					opts.ExpUnix = n
				}
			case "maxViews":
				if n, ok := int64FromMap(pm, "valueInteger"); ok && n > 0 {
					opts.MaxViews = int(n)
				}
			case "includeIPS":
				if b, ok := pm["valueBoolean"].(bool); ok {
					opts.IncludeIPS = b
				}
			case "includeSHC":
				if b, ok := pm["valueBoolean"].(bool); ok {
					opts.IncludeSHC = b
				}
			case "longTerm":
				if b, ok := pm["valueBoolean"].(bool); ok {
					opts.LongTerm = b
				}
			}
		}
		return opts
	}

	if sVal, ok := doc["patient"].(string); ok && sVal != "" {
		opts.PatientID = cleanID(sVal)
	}
	if sVal, ok := doc["label"].(string); ok && sVal != "" {
		opts.Label = strings.TrimSpace(sVal)
	}
	if sVal, ok := doc["passcode"].(string); ok && sVal != "" {
		opts.Passcode = strings.TrimSpace(sVal)
	}
	if sVal, ok := doc["viewerUrl"].(string); ok && sVal != "" {
		opts.ViewerURL = strings.TrimSpace(sVal)
	}
	if n, ok := int64FromMap(doc, "expiresInSeconds"); ok {
		opts.ExpiresInSeconds = n
	}
	if n, ok := int64FromMap(doc, "exp"); ok {
		opts.ExpUnix = n
	}
	if n, ok := int64FromMap(doc, "maxViews"); ok && n > 0 {
		opts.MaxViews = int(n)
	}
	if b, ok := doc["includeIPS"].(bool); ok {
		opts.IncludeIPS = b
	}
	if b, ok := doc["includeSHC"].(bool); ok {
		opts.IncludeSHC = b
	}
	if b, ok := doc["longTerm"].(bool); ok {
		opts.LongTerm = b
	}
	return opts
}

func firstStringValue(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func int64FromMap(m map[string]any, key string) (int64, bool) {
	v, exists := m[key]
	if !exists || v == nil {
		return 0, false
	}
	switch tv := v.(type) {
	case float64:
		return int64(tv), true
	case int64:
		return tv, true
	case int:
		return int64(tv), true
	case json.Number:
		if n, err := tv.Int64(); err == nil {
			return n, true
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(tv), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

func (s *Server) recordSHLinkAudit(
	r *http.Request,
	tenant string,
	action string,
	subtype string,
	status int,
	linkID string,
	patientID string,
	actorOverride string,
) {
	if s.auditRecorder == nil {
		return
	}
	if tenant == "" {
		tenant = "default"
	}
	subject := "anonymous"
	if actorOverride != "" {
		subject = actorOverride
	}
	if secCtx, ok := auth.FromContext(r.Context()); ok && secCtx.GetSubject() != "" {
		subject = secCtx.GetSubject()
	}
	outcome := audit.OutcomeSuccess
	if status >= 400 {
		if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests {
			outcome = audit.OutcomeSeriousFailure
		} else {
			outcome = audit.OutcomeMinorFailure
		}
	}
	rec := store.AuditRecord{
		TenantID:     tenant,
		AuditID:      uuid.NewString(),
		Recorded:     time.Now().UTC(),
		Action:       action,
		SubtypeCode:  subtype,
		Outcome:      outcome,
		OutcomeDesc:  fmt.Sprintf("%d %s", status, http.StatusText(status)),
		HTTPMethod:   r.Method,
		HTTPStatus:   status,
		RequestURI:   r.URL.RequestURI(),
		AgentSubject: subject,
		AgentPatient: cleanID(patientID),
		ClientIP:     extractClientIP(r),
		EntityType:   "SmartHealthLink",
		EntityID:     linkID,
	}
	_ = s.auditRecorder.Record(context.WithoutCancel(r.Context()), rec)
}

// handlePatientSHLinkIssue serves POST /fhir/r4/{tenant}/Patient/{id}/$shlink
// (and type-level POST /fhir/r4/{tenant}/Patient/$shlink with a patient parameter).
func (s *Server) handlePatientSHLinkIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "$shlink requires POST")
		return
	}
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = "default"
	}
	opts := parseSHLinkIssueOptions(r)
	patID := cleanID(strings.TrimSpace(r.PathValue("id")))
	if patID == "" {
		patID = opts.PatientID
	}
	if patID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Patient ID is required to issue a SMART Health Link")
		return
	}

	if secCtx, hasSec := auth.FromContext(r.Context()); hasSec {
		if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient SMART scope to issue Patient/$shlink")
			return
		}
		if auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) != patID {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot issue $shlink for Patient/%s", secCtx.GetPatientId(), patID))
			return
		}
	}

	setAuditPatientID(r.Context(), patID)

	if !opts.IncludeSHC && !opts.IncludeIPS {
		opts.IncludeIPS = true
	}

	var artifacts []shlink.ArtifactInput
	if opts.IncludeSHC {
		jwsToken, _, shcStatus, shcErrMsg := s.buildSignedSmartHealthCard(r, tenant, patID, nil)
		if shcStatus != http.StatusOK {
			writeOperationOutcome(w, shcStatus, "not-found", shcErrMsg)
			return
		}
		shcContainer, _ := json.Marshal(map[string]any{
			"verifiableCredential": []string{jwsToken},
		})
		artifacts = append(artifacts, shlink.ArtifactInput{
			ContentType: "application/smart-health-card",
			Plaintext:   shcContainer,
			Deflate:     false,
		})
	}
	if opts.IncludeIPS {
		ipsBundle, ipsStatus, ipsCode, ipsMsg := s.assembleIPSSummaryBundle(r, tenant, patID)
		if ipsStatus != http.StatusOK {
			writeOperationOutcome(w, ipsStatus, ipsCode, ipsMsg)
			return
		}
		ipsJSON, _ := json.Marshal(ipsBundle)
		artifacts = append(artifacts, shlink.ArtifactInput{
			ContentType: "application/fhir+json",
			Plaintext:   ipsJSON,
			Deflate:     true,
		})
	}

	var expiresAt time.Time
	now := s.SHLinkManager().Now()
	if opts.ExpUnix > 0 {
		expiresAt = time.Unix(opts.ExpUnix, 0).UTC()
	} else if opts.ExpiresInSeconds > 0 {
		expiresAt = now.Add(time.Duration(opts.ExpiresInSeconds) * time.Second)
	}

	label := opts.Label
	if label == "" {
		label = "Flint Patient Summary (" + patID + ")"
	}

	issued, err := s.SHLinkManager().CreateLink(shlink.CreateRequest{
		Tenant:     tenant,
		PatientID:  patID,
		BaseOrigin: requestBaseOrigin(r),
		ViewerURL:  opts.ViewerURL,
		Label:      label,
		Passcode:   opts.Passcode,
		ExpiresAt:  expiresAt,
		MaxViews:   opts.MaxViews,
		LongTerm:   opts.LongTerm,
		Artifacts:  artifacts,
	})
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to create SMART Health Link: "+err.Error())
		return
	}

	s.recordSHLinkAudit(r, tenant, audit.ActionCreate, "shlink-issue", http.StatusOK, issued.ID, patID, "")

	paramsList := []map[string]any{
		{"name": "shlink", "valueUri": issued.SHLinkURI},
		{"name": "linkId", "valueString": issued.ID},
		{"name": "manifestUrl", "valueUrl": issued.ManifestURL},
		{"name": "label", "valueString": issued.Label},
		{"name": "qrCodeSvg", "valueString": issued.QRCodeSVG},
	}
	if issued.Flag != "" {
		paramsList = append(paramsList, map[string]any{"name": "flag", "valueString": issued.Flag})
	}
	if issued.Exp > 0 {
		paramsList = append(paramsList, map[string]any{"name": "exp", "valueInteger": issued.Exp})
	}

	out := map[string]any{
		"resourceType": "Parameters",
		"parameter":    paramsList,
	}
	s.writeOperationResponse(w, r, http.StatusOK, "Parameters", out)
}

// handlePatientSHLinkRevoke serves POST /fhir/r4/{tenant}/Patient/{id}/$shlink-revoke.
func (s *Server) handlePatientSHLinkRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "$shlink-revoke requires POST")
		return
	}
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = "default"
	}
	patID := cleanID(strings.TrimSpace(r.PathValue("id")))
	if secCtx, hasSec := auth.FromContext(r.Context()); hasSec {
		if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionUpdate) && !auth.Allows(secCtx, "Patient", auth.ActionDelete) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient SMART scope to revoke Patient/$shlink")
			return
		}
		if auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) != patID {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot revoke $shlink for Patient/%s", secCtx.GetPatientId(), patID))
			return
		}
	}

	linkID := strings.TrimSpace(r.URL.Query().Get("linkId"))
	if linkID == "" && r.Body != nil {
		rawBody, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var doc map[string]any
		if json.Unmarshal(rawBody, &doc) == nil {
			if sVal, ok := doc["linkId"].(string); ok {
				linkID = strings.TrimSpace(sVal)
			} else if rawParams, ok := doc["parameter"].([]any); ok {
				for _, item := range rawParams {
					if pm, ok := item.(map[string]any); ok && pm["name"] == "linkId" {
						linkID = firstStringValue(pm, "valueString", "valueId")
					}
				}
			}
		}
	}
	if linkID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "linkId parameter is required")
		return
	}

	err := s.SHLinkManager().Revoke(tenant, patID, linkID)
	switch {
	case errors.Is(err, shlink.ErrNotFound):
		s.recordSHLinkAudit(r, tenant, audit.ActionDelete, "shlink-revoke", http.StatusNotFound, linkID, patID, "")
		writeOperationOutcome(w, http.StatusNotFound, "not-found", "SMART Health Link not found")
		return
	case errors.Is(err, shlink.ErrForbiddenPatient):
		s.recordSHLinkAudit(r, tenant, audit.ActionDelete, "shlink-revoke", http.StatusForbidden, linkID, patID, "")
		writeOperationOutcome(w, http.StatusForbidden, "forbidden", "SMART Health Link belongs to a different patient")
		return
	case err != nil:
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", err.Error())
		return
	}

	s.recordSHLinkAudit(r, tenant, audit.ActionDelete, "shlink-revoke", http.StatusOK, linkID, patID, "")
	s.writeOperationResponse(w, r, http.StatusOK, "OperationOutcome", map[string]any{
		"resourceType": "OperationOutcome",
		"issue": []map[string]any{
			{"severity": "information", "code": "informational", "diagnostics": "SMART Health Link revoked"},
		},
	})
}

func setSHLinkPublicHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Cache-Control", "no-store")
}

// handleSHLinkManifest serves POST /shlink/{id}/manifest per the HL7 SMART Health Links spec.
func (s *Server) handleSHLinkManifest(w http.ResponseWriter, r *http.Request) {
	setSHLinkPublicHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	linkID := strings.TrimSpace(r.PathValue("id"))
	var mReq shlink.ManifestRequest
	if r.Body != nil {
		rawBody, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || json.Unmarshal(rawBody, &mReq) != nil {
			tenant, patID := s.SHLinkManager().LinkOwner(linkID)
			s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-manifest", http.StatusBadRequest, linkID, patID, "")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_request", "message": "malformed JSON manifest request"})
			return
		}
	}

	manifest, tenant, patID, err := s.SHLinkManager().ResolveManifest(linkID, requestBaseOrigin(r), mReq)
	recipientActor := strings.TrimSpace(mReq.Recipient)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		var pe *shlink.PasscodeError
		switch {
		case errors.Is(err, shlink.ErrRecipientRequired):
			s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-manifest", http.StatusBadRequest, linkID, patID, recipientActor)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "recipient_required", "message": err.Error()})
		case errors.Is(err, shlink.ErrNotFound):
			s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-manifest", http.StatusNotFound, linkID, patID, recipientActor)
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not_found", "message": err.Error()})
		case errors.As(err, &pe):
			s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-manifest", http.StatusUnauthorized, linkID, patID, recipientActor)
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":             "invalid_passcode",
				"message":           err.Error(),
				"remainingAttempts": pe.RemainingAttempts,
			})
		case errors.Is(err, shlink.ErrPasscodeLockedOut):
			s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-manifest", http.StatusTooManyRequests, linkID, patID, recipientActor)
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "passcode_locked_out", "message": err.Error(), "remainingAttempts": 0})
		case errors.Is(err, shlink.ErrExpired), errors.Is(err, shlink.ErrRevoked), errors.Is(err, shlink.ErrMaxViewsReached):
			s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-manifest", http.StatusGone, linkID, patID, recipientActor)
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "inactive_link", "message": err.Error()})
		default:
			s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-manifest", http.StatusInternalServerError, linkID, patID, recipientActor)
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "server_error", "message": err.Error()})
		}
		return
	}

	s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-manifest", http.StatusOK, linkID, patID, recipientActor)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(manifest)
}

// handleSHLinkContent serves GET /shlink/{id}/content/{item}, returning the pre-encrypted
// compact JWE ("alg":"dir", "enc":"A256GCM") with Content-Type: application/jose.
func (s *Server) handleSHLinkContent(w http.ResponseWriter, r *http.Request) {
	setSHLinkPublicHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	linkID := strings.TrimSpace(r.PathValue("id"))
	itemID := strings.TrimSpace(r.PathValue("item"))
	compactJWE, tenant, patID, err := s.SHLinkManager().ReadContent(linkID, itemID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case errors.Is(err, shlink.ErrNotFound):
			s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-content", http.StatusNotFound, linkID, patID, "")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not_found", "message": err.Error()})
		case errors.Is(err, shlink.ErrExpired), errors.Is(err, shlink.ErrRevoked):
			s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-content", http.StatusGone, linkID, patID, "")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "inactive_link", "message": err.Error()})
		default:
			s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-content", http.StatusInternalServerError, linkID, patID, "")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "server_error", "message": err.Error()})
		}
		return
	}

	s.recordSHLinkAudit(r, tenant, audit.ActionRead, "shlink-content", http.StatusOK, linkID, patID, "")
	w.Header().Set("Content-Type", "application/jose")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(compactJWE))
}
