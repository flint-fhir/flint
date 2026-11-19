package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/mcp"
	"github.com/flint-fhir/flint/store"
)

// ValidateBearerToken validates a raw SMART/OIDC Bearer token string against the server's
// revocation list and configured TokenValidator (falling back to built-in devAuth).
func (s *Server) ValidateBearerToken(ctx context.Context, rawToken string) (*auth.SecurityContext, error) {
	trimmed := strings.TrimSpace(rawToken)
	if strings.HasPrefix(strings.ToLower(trimmed), "bearer ") {
		trimmed = strings.TrimSpace(trimmed[7:])
	}
	if trimmed == "" {
		return nil, errors.New("empty bearer token")
	}
	if s.isTokenRevoked(trimmed) {
		return nil, errors.New("token has been revoked")
	}
	validator := s.tokenValidator
	if validator == nil {
		validator = s.devAuth
	}
	if validator == nil {
		return nil, errors.New("no token validator available")
	}
	return validator.ValidateToken(ctx, trimmed)
}

// IssueDevToken mints a signed RS256 SMART JWT using Flint's built-in development authority.
func (s *Server) IssueDevToken(sub, scopes, tenantID, patientID string, ttl time.Duration) (string, error) {
	if s.devAuth == nil {
		return "", errors.New("built-in dev token issuer unavailable")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	extra := map[string]any{}
	if strings.TrimSpace(tenantID) != "" {
		extra["tenant_id"] = strings.TrimSpace(tenantID)
	}
	return s.devAuth.IssueToken(sub, scopes, cleanID(patientID), ttl, extra)
}

// MCPServer returns the SMART-authorized Model Context Protocol server bound to s.
func (s *Server) MCPServer() *mcp.Server {
	s.mcpOnce.Do(func() {
		s.mcpServer = mcp.NewServer("flint-fhir-mcp", Version, &serverMCPBackend{srv: s})
	})
	return s.mcpServer
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 3 && parts[0] == "fhir" && parts[1] == "r4" {
			tenant = parts[2]
		}
	}
	if tenant == "" {
		tenant = "default"
	}

	if secCtx, ok := auth.FromContext(r.Context()); ok {
		if secCtx.GetTenantId() != "" && secCtx.GetTenantId() != tenant {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("token restricted to tenant %q, cannot access MCP tenant %q", secCtx.GetTenantId(), tenant))
			return
		}
		if len(secCtx.GetScopes()) == 0 {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient SMART scope for MCP endpoint")
			return
		}
	}
	s.MCPServer().ServeHTTP(w, r, tenant)
}

type serverMCPBackend struct {
	srv *Server
}

func (b *serverMCPBackend) checkTenantAndAuth(ctx context.Context, tenant string) (*auth.SecurityContext, bool, error) {
	secCtx, hasSec := auth.FromContext(ctx)
	if !hasSec {
		if b.srv.tokenValidator != nil {
			return nil, false, mcp.ErrUnauthorized
		}
		return nil, false, nil
	}
	if secCtx.GetTenantId() != "" && secCtx.GetTenantId() != tenant {
		return secCtx, true, fmt.Errorf("%w: token restricted to tenant %q", mcp.ErrForbidden, secCtx.GetTenantId())
	}
	if auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) == "" {
		return secCtx, true, fmt.Errorf("%w: patient-scoped token missing patient context", mcp.ErrForbidden)
	}
	return secCtx, true, nil
}

func (b *serverMCPBackend) recordMCPAudit(
	ctx context.Context,
	tenant string,
	action string,
	subtype string,
	httpMethod string,
	reqURI string,
	status int,
	entityType string,
	entityID string,
	patientID string,
	secCtx *auth.SecurityContext,
) {
	if b.srv.auditRecorder == nil {
		return
	}
	subject := "anonymous"
	agentPatient := cleanID(patientID)
	if secCtx != nil {
		if secCtx.GetSubject() != "" {
			subject = secCtx.GetSubject()
		}
		if agentPatient == "" {
			agentPatient = cleanID(secCtx.GetPatientId())
		}
	}
	outcome := audit.OutcomeSuccess
	if status >= 400 && status < 500 {
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			outcome = audit.OutcomeSeriousFailure
		} else {
			outcome = audit.OutcomeMinorFailure
		}
	} else if status >= 500 {
		outcome = audit.OutcomeSeriousFailure
	}
	rec := store.AuditRecord{
		TenantID:     tenant,
		AuditID:      uuid.NewString(),
		Recorded:     time.Now().UTC(),
		Action:       action,
		SubtypeCode:  subtype,
		Outcome:      outcome,
		OutcomeDesc:  fmt.Sprintf("%d %s", status, http.StatusText(status)),
		HTTPMethod:   httpMethod,
		HTTPStatus:   status,
		RequestURI:   reqURI,
		AgentSubject: subject,
		AgentPatient: agentPatient,
		ClientIP:     "mcp",
		EntityType:   entityType,
		EntityID:     entityID,
	}
	_ = b.srv.auditRecorder.Record(context.WithoutCancel(ctx), rec)
}

func (b *serverMCPBackend) dispatchInternal(
	ctx context.Context,
	method string,
	targetPath string,
	contentType string,
	body []byte,
	headers map[string]string,
) (*httptest.ResponseRecorder, error) {
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://flint.internal"+targetPath, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/fhir+json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	// Run through authMiddleware + fhirRouter so every SMART v2 scope, compartment, and granular
	// ?category= rule executes identically across HTTP REST, Streamable HTTP MCP, and stdio MCP.
	b.srv.authMiddleware(b.srv.fhirRouter()).ServeHTTP(rec, req)
	return rec, nil
}

func (b *serverMCPBackend) buildToolResultFromRecorder(
	ctx context.Context,
	tenant string,
	action string,
	toolName string,
	method string,
	targetPath string,
	entityType string,
	entityID string,
	patientID string,
	secCtx *auth.SecurityContext,
	rec *httptest.ResponseRecorder,
) (*mcp.ToolCallResult, error) {
	rawBytes := bytes.TrimSpace(rec.Body.Bytes())
	b.recordMCPAudit(ctx, tenant, action, "mcp:"+toolName, method, targetPath, rec.Code, entityType, entityID, patientID, secCtx)

	switch rec.Code {
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %s", mcp.ErrUnauthorized, compactOutcomeSummary(rawBytes))
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", mcp.ErrForbidden, compactOutcomeSummary(rawBytes))
	}

	var parsed any
	_ = json.Unmarshal(rawBytes, &parsed)
	textStr := string(rawBytes)
	if textStr == "" {
		textStr = fmt.Sprintf(`{"status":%d}`, rec.Code)
	}
	return &mcp.ToolCallResult{
		Content:           []mcp.ContentBlock{{Type: "text", Text: textStr}},
		StructuredContent: parsed,
		IsError:           rec.Code >= 400,
		HTTPStatus:        rec.Code,
	}, nil
}

func compactOutcomeSummary(raw []byte) string {
	var oo struct {
		Issue []struct {
			Diagnostics string `json:"diagnostics"`
		} `json:"issue"`
	}
	if json.Unmarshal(raw, &oo) == nil && len(oo.Issue) > 0 && oo.Issue[0].Diagnostics != "" {
		return oo.Issue[0].Diagnostics
	}
	if len(raw) > 200 {
		return string(raw[:200])
	}
	if len(raw) == 0 {
		return "access denied"
	}
	return string(raw)
}

func strArg(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	switch tv := v.(type) {
	case string:
		return strings.TrimSpace(tv)
	case float64:
		return strconv.FormatInt(int64(tv), 10)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", tv))
	}
}

func (b *serverMCPBackend) CallTool(ctx context.Context, tenant string, toolName string, args map[string]any) (*mcp.ToolCallResult, error) {
	if strings.TrimSpace(tenant) == "" {
		tenant = "default"
	}
	secCtx, hasSec, err := b.checkTenantAndAuth(ctx, tenant)
	if err != nil {
		b.recordMCPAudit(ctx, tenant, audit.ActionExecute, "mcp:"+toolName, http.MethodPost, "/fhir/r4/"+tenant+"/$mcp", http.StatusForbidden, "OperationDefinition", toolName, "", secCtx)
		return nil, err
	}

	switch toolName {
	case "fhir_search":
		resType := strArg(args, "resourceType")
		if resType == "" {
			return nil, fmt.Errorf("%w: resourceType is required", mcp.ErrInvalidParam)
		}
		q := url.Values{}
		if qMap, ok := args["query"].(map[string]any); ok {
			for k, v := range qMap {
				ks := strings.TrimSpace(k)
				if ks == "" || v == nil {
					continue
				}
				switch vv := v.(type) {
				case []any:
					for _, item := range vv {
						q.Add(ks, fmt.Sprintf("%v", item))
					}
				default:
					q.Set(ks, fmt.Sprintf("%v", vv))
				}
			}
		}
		if cnt := strArg(args, "count"); cnt != "" && q.Get("_count") == "" {
			q.Set("_count", cnt)
		}
		path := "/fhir/r4/" + tenant + "/" + url.PathEscape(resType)
		if enc := q.Encode(); enc != "" {
			path += "?" + enc
		}
		rec, dErr := b.dispatchInternal(ctx, http.MethodGet, path, "", nil, nil)
		if dErr != nil {
			return nil, dErr
		}
		return b.buildToolResultFromRecorder(ctx, tenant, audit.ActionRead, toolName, http.MethodGet, path, resType, "", cleanID(q.Get("patient")), secCtx, rec)

	case "fhir_read":
		resType := strArg(args, "resourceType")
		id := cleanID(strArg(args, "id"))
		if resType == "" || id == "" {
			return nil, fmt.Errorf("%w: resourceType and id are required", mcp.ErrInvalidParam)
		}
		path := "/fhir/r4/" + tenant + "/" + url.PathEscape(resType) + "/" + url.PathEscape(id)
		rec, dErr := b.dispatchInternal(ctx, http.MethodGet, path, "", nil, nil)
		if dErr != nil {
			return nil, dErr
		}
		patID := ""
		if strings.EqualFold(resType, "Patient") {
			patID = id
		}
		return b.buildToolResultFromRecorder(ctx, tenant, audit.ActionRead, toolName, http.MethodGet, path, resType, id, patID, secCtx, rec)

	case "patient_summary_ips":
		patID := cleanID(strArg(args, "patientId"))
		if patID == "" {
			return nil, fmt.Errorf("%w: patientId is required", mcp.ErrInvalidParam)
		}
		path := "/fhir/r4/" + tenant + "/Patient/" + url.PathEscape(patID) + "/$summary"
		rec, dErr := b.dispatchInternal(ctx, http.MethodGet, path, "", nil, nil)
		if dErr != nil {
			return nil, dErr
		}
		return b.buildToolResultFromRecorder(ctx, tenant, audit.ActionRead, toolName, http.MethodGet, path, "Patient", patID, patID, secCtx, rec)

	case "patient_everything":
		patID := cleanID(strArg(args, "patientId"))
		if patID == "" {
			return nil, fmt.Errorf("%w: patientId is required", mcp.ErrInvalidParam)
		}
		q := url.Values{}
		if types := strArg(args, "types"); types != "" {
			q.Set("_type", types)
		}
		if since := strArg(args, "since"); since != "" {
			q.Set("_since", since)
		}
		path := "/fhir/r4/" + tenant + "/Patient/" + url.PathEscape(patID) + "/$everything"
		if enc := q.Encode(); enc != "" {
			path += "?" + enc
		}
		rec, dErr := b.dispatchInternal(ctx, http.MethodGet, path, "", nil, nil)
		if dErr != nil {
			return nil, dErr
		}
		return b.buildToolResultFromRecorder(ctx, tenant, audit.ActionRead, toolName, http.MethodGet, path, "Patient", patID, patID, secCtx, rec)

	case "run_view_definition":
		vdID := cleanID(strArg(args, "viewDefinitionId"))
		inlineVD, hasInline := args["viewDefinition"].(map[string]any)
		if vdID == "" && !hasInline {
			return nil, fmt.Errorf("%w: viewDefinitionId or viewDefinition is required", mcp.ErrInvalidParam)
		}
		patFilter := cleanID(strArg(args, "patientId"))
		if hasSec && auth.IsPatientRestricted(secCtx) {
			allowedPat := cleanID(secCtx.GetPatientId())
			if patFilter != "" && patFilter != allowedPat {
				b.recordMCPAudit(ctx, tenant, audit.ActionExecute, "mcp:"+toolName, http.MethodPost, "/fhir/r4/"+tenant+"/ViewDefinition/$run", http.StatusForbidden, "ViewDefinition", vdID, patFilter, secCtx)
				return nil, fmt.Errorf("%w: patient-scoped token (%s) cannot run ViewDefinition for Patient/%s", mcp.ErrForbidden, allowedPat, patFilter)
			}
			patFilter = allowedPat
		}
		if hasSec && hasInline {
			targetRes := strArg(inlineVD, "resource")
			if targetRes != "" && !auth.Allows(secCtx, targetRes, auth.ActionSearch) && !auth.Allows(secCtx, targetRes, auth.ActionRead) {
				b.recordMCPAudit(ctx, tenant, audit.ActionExecute, "mcp:"+toolName, http.MethodPost, "/fhir/r4/"+tenant+"/ViewDefinition/$run", http.StatusForbidden, targetRes, "", patFilter, secCtx)
				return nil, fmt.Errorf("%w: insufficient SMART scope to project %s via ViewDefinition", mcp.ErrForbidden, targetRes)
			}
		}

		q := url.Values{}
		if patFilter != "" {
			q.Set("patient", "Patient/"+patFilter)
		}
		var method, path string
		var bodyBytes []byte
		if hasInline {
			method = http.MethodPost
			paramsBody := map[string]any{
				"resourceType": "Parameters",
				"parameter": []map[string]any{
					{"name": "viewResource", "resource": inlineVD},
				},
			}
			bodyBytes, _ = json.Marshal(paramsBody)
			path = "/fhir/r4/" + tenant + "/ViewDefinition/$run"
		} else {
			method = http.MethodGet
			path = "/fhir/r4/" + tenant + "/ViewDefinition/" + url.PathEscape(vdID) + "/$run"
		}
		if enc := q.Encode(); enc != "" {
			path += "?" + enc
		}
		rec, dErr := b.dispatchInternal(ctx, method, path, "application/fhir+json", bodyBytes, nil)
		if dErr != nil {
			return nil, dErr
		}
		return b.buildToolResultFromRecorder(ctx, tenant, audit.ActionExecute, toolName, method, path, "ViewDefinition", vdID, patFilter, secCtx, rec)

	case "evaluate_measure":
		measureID := cleanID(strArg(args, "measureId"))
		if measureID == "" {
			return nil, fmt.Errorf("%w: measureId is required", mcp.ErrInvalidParam)
		}
		subject := strArg(args, "subject")
		if subject != "" && !strings.Contains(subject, "/") {
			subject = "Patient/" + subject
		}
		if hasSec && auth.IsPatientRestricted(secCtx) {
			allowedPat := cleanID(secCtx.GetPatientId())
			if subject != "" && cleanID(subject) != allowedPat {
				b.recordMCPAudit(ctx, tenant, audit.ActionExecute, "mcp:"+toolName, http.MethodGet, "/fhir/r4/"+tenant+"/Measure/"+measureID+"/$evaluate-measure", http.StatusForbidden, "Measure", measureID, cleanID(subject), secCtx)
				return nil, fmt.Errorf("%w: patient-scoped token (%s) cannot evaluate measure for %s", mcp.ErrForbidden, allowedPat, subject)
			}
			subject = "Patient/" + allowedPat
		}
		q := url.Values{}
		if subject != "" {
			q.Set("subject", subject)
		}
		ps := strArg(args, "periodStart")
		if ps == "" {
			ps = "2020-01-01"
		}
		pe := strArg(args, "periodEnd")
		if pe == "" {
			pe = "2030-12-31"
		}
		q.Set("periodStart", ps)
		q.Set("periodEnd", pe)
		if rt := strArg(args, "reportType"); rt != "" {
			q.Set("reportType", rt)
		}
		path := "/fhir/r4/" + tenant + "/Measure/" + url.PathEscape(measureID) + "/$evaluate-measure?" + q.Encode()
		rec, dErr := b.dispatchInternal(ctx, http.MethodGet, path, "", nil, nil)
		if dErr != nil {
			return nil, dErr
		}
		return b.buildToolResultFromRecorder(ctx, tenant, audit.ActionExecute, toolName, http.MethodGet, path, "Measure", measureID, cleanID(subject), secCtx, rec)

	case "cds_hooks_invoke":
		serviceID := strArg(args, "serviceId")
		patID := cleanID(strArg(args, "patientId"))
		if serviceID == "" || patID == "" {
			return nil, fmt.Errorf("%w: serviceId and patientId are required", mcp.ErrInvalidParam)
		}
		if hasSec {
			if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) {
				b.recordMCPAudit(ctx, tenant, audit.ActionExecute, "mcp:"+toolName, http.MethodPost, "/cds-services/"+serviceID, http.StatusForbidden, "Patient", patID, patID, secCtx)
				return nil, fmt.Errorf("%w: insufficient SMART scope for CDS Hooks patient context", mcp.ErrForbidden)
			}
			if auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) != patID {
				b.recordMCPAudit(ctx, tenant, audit.ActionExecute, "mcp:"+toolName, http.MethodPost, "/cds-services/"+serviceID, http.StatusForbidden, "Patient", patID, patID, secCtx)
				return nil, fmt.Errorf("%w: patient-scoped token (%s) cannot invoke CDS Hooks for Patient/%s", mcp.ErrForbidden, secCtx.GetPatientId(), patID)
			}
		}
		hookName := strArg(args, "hook")
		if hookName == "" {
			hookName = "patient-view"
		}
		cdsCtx := map[string]any{"patientId": patID}
		if encID := cleanID(strArg(args, "encounterId")); encID != "" {
			cdsCtx["encounterId"] = encID
		}
		payload, _ := json.Marshal(map[string]any{
			"hookInstance": uuid.NewString(),
			"hook":         hookName,
			"fhirServer":   "http://flint.internal/fhir/r4/" + tenant,
			"context":      cdsCtx,
		})
		path := "/fhir/r4/" + url.PathEscape(tenant) + "/cds-services/" + url.PathEscape(serviceID)
		rec, dErr := b.dispatchInternal(ctx, http.MethodPost, path, "application/json", payload, nil)
		if dErr != nil {
			return nil, dErr
		}
		return b.buildToolResultFromRecorder(ctx, tenant, audit.ActionExecute, toolName, http.MethodPost, path, "Patient", patID, patID, secCtx, rec)

	case "validate_resource":
		resMap, ok := args["resource"].(map[string]any)
		if !ok || len(resMap) == 0 {
			return nil, fmt.Errorf("%w: resource object is required", mcp.ErrInvalidParam)
		}
		resType := strArg(resMap, "resourceType")
		if resType == "" {
			return nil, fmt.Errorf("%w: resource.resourceType is required", mcp.ErrInvalidParam)
		}
		if hasSec && !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionCreate) && !auth.Allows(secCtx, resType, auth.ActionUpdate) {
			b.recordMCPAudit(ctx, tenant, audit.ActionExecute, "mcp:"+toolName, http.MethodPost, "/fhir/r4/"+tenant+"/"+resType+"/$validate", http.StatusForbidden, resType, "", "", secCtx)
			return nil, fmt.Errorf("%w: insufficient SMART scope to validate %s", mcp.ErrForbidden, resType)
		}
		rawBody, _ := json.Marshal(resMap)
		path := "/fhir/r4/" + tenant + "/" + url.PathEscape(resType) + "/$validate"
		if prof := strArg(args, "profile"); prof != "" {
			path += "?profile=" + url.QueryEscape(prof)
		}
		rec, dErr := b.dispatchInternal(ctx, http.MethodPost, path, "application/fhir+json", rawBody, nil)
		if dErr != nil {
			return nil, dErr
		}
		return b.buildToolResultFromRecorder(ctx, tenant, audit.ActionExecute, toolName, http.MethodPost, path, resType, strArg(resMap, "id"), "", secCtx, rec)

	case "create_or_update_resource":
		resMap, ok := args["resource"].(map[string]any)
		if !ok || len(resMap) == 0 {
			return nil, fmt.Errorf("%w: resource object is required", mcp.ErrInvalidParam)
		}
		resType := strArg(resMap, "resourceType")
		if resType == "" {
			return nil, fmt.Errorf("%w: resource.resourceType is required", mcp.ErrInvalidParam)
		}
		resID := cleanID(strArg(resMap, "id"))
		rawBody, _ := json.Marshal(resMap)
		headers := map[string]string{}
		if ifMatch := strArg(args, "ifMatch"); ifMatch != "" {
			headers["If-Match"] = ifMatch
		}

		var method, path, auditAct string
		if resID != "" {
			method = http.MethodPut
			path = "/fhir/r4/" + tenant + "/" + url.PathEscape(resType) + "/" + url.PathEscape(resID)
			auditAct = audit.ActionUpdate
		} else {
			method = http.MethodPost
			path = "/fhir/r4/" + tenant + "/" + url.PathEscape(resType)
			auditAct = audit.ActionCreate
		}
		rec, dErr := b.dispatchInternal(ctx, method, path, "application/fhir+json", rawBody, headers)
		if dErr != nil {
			return nil, dErr
		}
		return b.buildToolResultFromRecorder(ctx, tenant, auditAct, toolName, method, path, resType, resID, "", secCtx, rec)

	default:
		return nil, fmt.Errorf("%w: unsupported tool %q", mcp.ErrInvalidParam, toolName)
	}
}

func (b *serverMCPBackend) ReadResource(ctx context.Context, tenant string, uri string) ([]mcp.ResourceContent, error) {
	if !strings.HasPrefix(uri, "fhir://") {
		return nil, fmt.Errorf("%w: unsupported URI scheme in %q (expected fhir://)", mcp.ErrInvalidParam, uri)
	}
	trimmed := strings.TrimPrefix(uri, "fhir://")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("%w: invalid fhir:// URI %q", mcp.ErrInvalidParam, uri)
	}
	uriTenant := parts[0]
	if strings.TrimSpace(tenant) != "" && uriTenant != strings.TrimSpace(tenant) {
		return nil, fmt.Errorf("%w: resource URI tenant %q does not match active MCP tenant %q", mcp.ErrForbidden, uriTenant, tenant)
	}
	secCtx, _, err := b.checkTenantAndAuth(ctx, uriTenant)
	if err != nil {
		b.recordMCPAudit(ctx, uriTenant, audit.ActionRead, "mcp:resources/read", http.MethodGet, uri, http.StatusForbidden, "Resource", uri, "", secCtx)
		return nil, err
	}

	var targetPath, mime, entityType, entityID string
	switch {
	case len(parts) == 2 && parts[1] == "metadata":
		targetPath = "/fhir/r4/" + url.PathEscape(uriTenant) + "/metadata"
		mime = "application/fhir+json"
		entityType = "CapabilityStatement"
	case len(parts) == 2 && parts[1] == "smart-configuration":
		targetPath = "/fhir/r4/" + url.PathEscape(uriTenant) + "/.well-known/smart-configuration"
		mime = "application/json"
		entityType = "SMARTConfiguration"
	case len(parts) == 3:
		entityType = parts[1]
		entityID = parts[2]
		targetPath = "/fhir/r4/" + url.PathEscape(uriTenant) + "/" + url.PathEscape(entityType) + "/" + url.PathEscape(entityID)
		mime = "application/fhir+json"
	default:
		return nil, fmt.Errorf("%w: unrecognized fhir:// resource path %q", mcp.ErrInvalidParam, uri)
	}

	rec, dErr := b.dispatchInternal(ctx, http.MethodGet, targetPath, "", nil, nil)
	if dErr != nil {
		return nil, dErr
	}
	b.recordMCPAudit(ctx, uriTenant, audit.ActionRead, "mcp:resources/read", http.MethodGet, targetPath, rec.Code, entityType, entityID, "", secCtx)

	switch rec.Code {
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %s", mcp.ErrUnauthorized, compactOutcomeSummary(rec.Body.Bytes()))
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", mcp.ErrForbidden, compactOutcomeSummary(rec.Body.Bytes()))
	case http.StatusOK:
		return []mcp.ResourceContent{{
			URI:      uri,
			MimeType: mime,
			Text:     rec.Body.String(),
		}}, nil
	default:
		return nil, fmt.Errorf("%w: HTTP %d reading %s: %s", mcp.ErrInvalidParam, rec.Code, uri, compactOutcomeSummary(rec.Body.Bytes()))
	}
}

func (b *serverMCPBackend) GetPrompt(ctx context.Context, tenant string, name string, args map[string]string) (*mcp.GetPromptResult, error) {
	if strings.TrimSpace(tenant) == "" {
		tenant = "default"
	}
	secCtx, _, err := b.checkTenantAndAuth(ctx, tenant)
	if err != nil {
		b.recordMCPAudit(ctx, tenant, audit.ActionRead, "mcp:prompts/get", http.MethodPost, "/fhir/r4/"+tenant+"/$mcp", http.StatusForbidden, "Prompt", name, "", secCtx)
		return nil, err
	}
	patID := cleanID(args["patientId"])
	if patID == "" {
		return nil, fmt.Errorf("%w: patientId prompt argument is required", mcp.ErrInvalidParam)
	}

	switch name {
	case "clinical_chart_review":
		ipsRes, cErr := b.CallTool(ctx, tenant, "patient_summary_ips", map[string]any{"patientId": patID})
		if cErr != nil {
			return nil, cErr
		}
		focus := strings.TrimSpace(args["focus"])
		focusLine := ""
		if focus != "" {
			focusLine = " Clinical focus: " + focus + "."
		}
		summaryText := ""
		if len(ipsRes.Content) > 0 {
			summaryText = ipsRes.Content[0].Text
		}
		return &mcp.GetPromptResult{
			Description: fmt.Sprintf("Longitudinal IPS chart review for Patient/%s", patID),
			Messages: []mcp.PromptMessage{
				{
					Role: "user",
					Content: mcp.ContentBlock{
						Type: "text",
						Text: fmt.Sprintf("Synthesize a concise clinical chart review (active problems, medications, allergies, vital signs, and recent results) for Patient/%s.%s\n\nAuthorized IPS Bundle:\n%s", patID, focusLine, summaryText),
					},
				},
			},
		}, nil

	case "care_gap_closure_brief":
		patRes, cErr := b.CallTool(ctx, tenant, "fhir_read", map[string]any{"resourceType": "Patient", "id": patID})
		if cErr != nil {
			return nil, cErr
		}
		patJSON := ""
		if len(patRes.Content) > 0 {
			patJSON = patRes.Content[0].Text
		}
		return &mcp.GetPromptResult{
			Description: fmt.Sprintf("Quality care-gap closure brief for Patient/%s", patID),
			Messages: []mcp.PromptMessage{
				{
					Role: "user",
					Content: mcp.ContentBlock{
						Type: "text",
						Text: fmt.Sprintf("Review open preventive and chronic disease quality care gaps for Patient/%s using evaluate_measure and fhir_search, and recommend evidence-based next steps.\n\nAuthorized Patient:\n%s", patID, patJSON),
					},
				},
			},
		}, nil

	default:
		return nil, fmt.Errorf("%w: unknown prompt %q", mcp.ErrInvalidParam, name)
	}
}
