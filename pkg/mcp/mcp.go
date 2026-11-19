// Package mcp implements a pure-Go Model Context Protocol (MCP 2025-03-26 / 2024-11-05)
// JSON-RPC 2.0 server supporting both Streamable HTTP (POST/GET/DELETE with SSE)
// and newline-delimited stdio transports over a pluggable clinical FHIR backend.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const (
	// LatestProtocolVersion is the MCP Streamable HTTP specification revision.
	LatestProtocolVersion = "2025-03-26"
	// LegacyProtocolVersion is the MCP 2024-11-05 specification revision.
	LegacyProtocolVersion = "2024-11-05"

	JSONRPCVersion = "2.0"

	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeUnauthorized   = -32001
	CodeForbidden      = -32003

	MaxStdioLineBytes = 16 << 20
)

var (
	ErrUnauthorized = errors.New("mcp: unauthorized")
	ErrForbidden    = errors.New("mcp: forbidden by SMART scope or patient compartment")
	ErrInvalidParam = errors.New("mcp: invalid parameters")
)

// ContentBlock represents an MCP text or embedded resource content block.
type ContentBlock struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	Resource *ResourceContent `json:"resource,omitempty"`
}

// ToolAnnotations provides MCP 2025-03-26 behavioral hints for AI agents.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

// ToolDefinition describes an exposed clinical FHIR R4 MCP tool.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema map[string]any  `json:"inputSchema"`
	Annotations ToolAnnotations `json:"annotations"`
}

// ToolCallResult represents the result of a tools/call invocation.
type ToolCallResult struct {
	Content           []ContentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
	HTTPStatus        int            `json:"-"`
}

// ResourceDefinition describes a static MCP resource entry.
type ResourceDefinition struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceTemplate describes an RFC 6570 URI template exposed by resources/templates/list.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceContent represents a concrete resource payload returned by resources/read.
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// PromptArgument describes an argument accepted by an MCP prompt.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptDefinition describes an exposed clinical MCP prompt template.
type PromptDefinition struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptMessage represents a message in a resolved MCP prompt.
type PromptMessage struct {
	Role    string       `json:"role"`
	Content ContentBlock `json:"content"`
}

// GetPromptResult is returned by prompts/get.
type GetPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// ClinicalBackend abstracts SMART-authorized clinical FHIR execution from MCP wire framing.
type ClinicalBackend interface {
	CallTool(ctx context.Context, tenant string, toolName string, args map[string]any) (*ToolCallResult, error)
	ReadResource(ctx context.Context, tenant string, uri string) ([]ResourceContent, error)
	GetPrompt(ctx context.Context, tenant string, name string, args map[string]string) (*GetPromptResult, error)
}

// RPCRequest is a JSON-RPC 2.0 request or notification envelope.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// RPCResponse is a JSON-RPC 2.0 response envelope.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// SessionStore is an optional persistence hook allowing stateless multi-replica clusters
// to share MCP Streamable HTTP session IDs without importing database drivers.
type SessionStore interface {
	SaveSession(ctx context.Context, sessionID string) error
	HasSession(ctx context.Context, sessionID string) (bool, error)
	DeleteSession(ctx context.Context, sessionID string) (bool, error)
}

// Server implements a transport-agnostic MCP JSON-RPC 2.0 server.
type Server struct {
	name         string
	version      string
	backend      ClinicalBackend
	sessionStore SessionStore

	mu       sync.RWMutex
	sessions map[string]bool
}

// NewServer creates a new MCP Server backed by backend.
func NewServer(name, version string, backend ClinicalBackend) *Server {
	if strings.TrimSpace(name) == "" {
		name = "flint-fhir-mcp"
	}
	if strings.TrimSpace(version) == "" {
		version = "0.7.0"
	}
	return &Server{
		name:     name,
		version:  version,
		backend:  backend,
		sessions: make(map[string]bool),
	}
}

// SetSessionStore configures an optional cluster-shared SessionStore for Mcp-Session-Id state.
func (s *Server) SetSessionStore(st SessionStore) {
	s.mu.Lock()
	s.sessionStore = st
	s.mu.Unlock()
}

func (s *Server) getSessionStore() SessionStore {
	s.mu.RLock()
	st := s.sessionStore
	s.mu.RUnlock()
	return st
}

func (s *Server) newSessionID(ctx context.Context) string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	id := hex.EncodeToString(buf[:])
	if st := s.getSessionStore(); st != nil {
		_ = st.SaveSession(ctx, id)
		return id
	}
	s.mu.Lock()
	s.sessions[id] = true
	s.mu.Unlock()
	return id
}

func (s *Server) hasSession(ctx context.Context, id string) bool {
	if st := s.getSessionStore(); st != nil {
		ok, err := st.HasSession(ctx, id)
		return err == nil && ok
	}
	s.mu.RLock()
	ok := s.sessions[id]
	s.mu.RUnlock()
	return ok
}

func (s *Server) deleteSession(ctx context.Context, id string) bool {
	if st := s.getSessionStore(); st != nil {
		ok, err := st.DeleteSession(ctx, id)
		return err == nil && ok
	}
	s.mu.Lock()
	existed := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	return existed
}

// BuiltinTools returns all 9 SMART-authorized FHIR R4 clinical MCP tool definitions.
func BuiltinTools() []ToolDefinition {
	ro := ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	rw := ToolAnnotations{ReadOnlyHint: false, DestructiveHint: false, IdempotentHint: true}
	return []ToolDefinition{
		{
			Name:        "fhir_search",
			Description: "Search FHIR R4 resources within the authorized tenant and SMART compartment/granular scopes.",
			Annotations: ro,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"resourceType"},
				"properties": map[string]any{
					"resourceType": map[string]any{"type": "string", "description": "FHIR R4 resource type (e.g. Patient, Observation, Condition)"},
					"query":        map[string]any{"type": "object", "description": "FHIR search parameter key/value map (e.g. {\"patient\":\"pat-1\",\"category\":\"vital-signs\"})"},
					"count":        map[string]any{"type": "integer", "description": "Optional _count page size limit"},
				},
			},
		},
		{
			Name:        "fhir_read",
			Description: "Read a single FHIR R4 resource by resourceType and logical id.",
			Annotations: ro,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"resourceType", "id"},
				"properties": map[string]any{
					"resourceType": map[string]any{"type": "string"},
					"id":           map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "patient_summary_ips",
			Description: "Generate an HL7 International Patient Summary (Patient/{id}/$summary) document Bundle.",
			Annotations: ro,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"patientId"},
				"properties": map[string]any{
					"patientId": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "patient_everything",
			Description: "Retrieve the longitudinal patient compartment Bundle (Patient/{id}/$everything).",
			Annotations: ro,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"patientId"},
				"properties": map[string]any{
					"patientId": map[string]any{"type": "string"},
					"types":     map[string]any{"type": "string", "description": "Optional comma-separated _type filter"},
					"since":     map[string]any{"type": "string", "description": "Optional ISO-8601 _since timestamp"},
				},
			},
		},
		{
			Name:        "run_view_definition",
			Description: "Execute an HL7 SQL-on-FHIR v2 ViewDefinition ($run) projection into tabular rows.",
			Annotations: ro,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"viewDefinitionId": map[string]any{"type": "string", "description": "Saved ViewDefinition logical ID"},
					"viewDefinition":   map[string]any{"type": "object", "description": "Inline ViewDefinition FHIR resource"},
					"patientId":        map[string]any{"type": "string", "description": "Optional Patient ID filter"},
				},
			},
		},
		{
			Name:        "evaluate_measure",
			Description: "Evaluate a Clinical Quality Measure (Measure/{id}/$evaluate-measure).",
			Annotations: ro,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"measureId"},
				"properties": map[string]any{
					"measureId":   map[string]any{"type": "string"},
					"subject":     map[string]any{"type": "string", "description": "Optional Patient/{id} subject"},
					"periodStart": map[string]any{"type": "string", "description": "Measurement period start (YYYY-MM-DD)"},
					"periodEnd":   map[string]any{"type": "string", "description": "Measurement period end (YYYY-MM-DD)"},
					"reportType":  map[string]any{"type": "string", "description": "individual, subject-list, or summary"},
				},
			},
		},
		{
			Name:        "cds_hooks_invoke",
			Description: "Invoke a CDS Hooks 2.0 clinical decision support service for an authorized patient.",
			Annotations: ro,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"serviceId", "patientId"},
				"properties": map[string]any{
					"serviceId":   map[string]any{"type": "string"},
					"patientId":   map[string]any{"type": "string"},
					"encounterId": map[string]any{"type": "string"},
					"hook":        map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "validate_resource",
			Description: "Validate a FHIR R4 resource against base R4 and Implementation Guide StructureDefinitions ($validate).",
			Annotations: ro,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"resource"},
				"properties": map[string]any{
					"resource": map[string]any{"type": "object"},
					"profile":  map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "create_or_update_resource",
			Description: "Create (POST) or update (PUT) a FHIR R4 resource subject to SMART write scopes and validation.",
			Annotations: rw,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"resource"},
				"properties": map[string]any{
					"resource": map[string]any{"type": "object"},
					"ifMatch":  map[string]any{"type": "string", "description": "Optional ETag (e.g. W/\"1\") for optimistic concurrency"},
				},
			},
		},
	}
}

// BuiltinResources returns static MCP resource descriptors for the given tenant.
func BuiltinResources(tenant string) []ResourceDefinition {
	if strings.TrimSpace(tenant) == "" {
		tenant = "default"
	}
	return []ResourceDefinition{
		{
			URI:         fmt.Sprintf("fhir://%s/metadata", tenant),
			Name:        "FHIR R4 CapabilityStatement",
			Description: "Server conformance metadata, supported profiles, search parameters, and operations",
			MimeType:    "application/fhir+json",
		},
		{
			URI:         fmt.Sprintf("fhir://%s/smart-configuration", tenant),
			Name:        "SMART on FHIR v2 Configuration",
			Description: "OAuth2 endpoints, PKCE methods, scopes, and SMART capabilities",
			MimeType:    "application/json",
		},
	}
}

// BuiltinResourceTemplates returns MCP URI templates for point-reading FHIR resources.
func BuiltinResourceTemplates() []ResourceTemplate {
	return []ResourceTemplate{
		{
			URITemplate: "fhir://{tenant}/{resourceType}/{id}",
			Name:        "FHIR R4 Resource by ID",
			Description: "Read any authorized FHIR R4 resource in a tenant compartment",
			MimeType:    "application/fhir+json",
		},
	}
}

// BuiltinPrompts returns built-in clinical workflow prompts.
func BuiltinPrompts() []PromptDefinition {
	return []PromptDefinition{
		{
			Name:        "clinical_chart_review",
			Description: "Synthesize a structured longitudinal chart review from the patient's IPS summary",
			Arguments: []PromptArgument{
				{Name: "patientId", Description: "Target Patient logical ID", Required: true},
				{Name: "focus", Description: "Optional clinical specialty or chief complaint focus", Required: false},
			},
		},
		{
			Name:        "care_gap_closure_brief",
			Description: "Review open quality measure care gaps and active conditions for a patient",
			Arguments: []PromptArgument{
				{Name: "patientId", Description: "Target Patient logical ID", Required: true},
			},
		},
	}
}

func isNotificationID(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0
}

func nullID() json.RawMessage {
	return json.RawMessage("null")
}

// HandlePayload processes a single or batch JSON-RPC 2.0 payload.
func (s *Server) HandlePayload(ctx context.Context, tenant string, body []byte) (respJSON []byte, isNotification bool, newSessionID string, err error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		resp, _ := json.Marshal(RPCResponse{
			JSONRPC: JSONRPCVersion,
			ID:      nullID(),
			Error:   &RPCError{Code: CodeInvalidRequest, Message: "empty JSON-RPC request body"},
		})
		return resp, false, "", nil
	}

	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			resp, _ := json.Marshal(RPCResponse{
				JSONRPC: JSONRPCVersion,
				ID:      nullID(),
				Error:   &RPCError{Code: CodeParseError, Message: "invalid JSON batch: " + err.Error()},
			})
			return resp, false, "", nil
		}
		if len(batch) == 0 {
			resp, _ := json.Marshal(RPCResponse{
				JSONRPC: JSONRPCVersion,
				ID:      nullID(),
				Error:   &RPCError{Code: CodeInvalidRequest, Message: "empty JSON-RPC batch array"},
			})
			return resp, false, "", nil
		}
		responses := make([]RPCResponse, 0, len(batch))
		var issuedSession string
		for _, item := range batch {
			rObj, notif, sid := s.dispatchSingleRaw(ctx, tenant, item)
			if sid != "" && issuedSession == "" {
				issuedSession = sid
			}
			if !notif && rObj != nil {
				responses = append(responses, *rObj)
			}
		}
		if len(responses) == 0 {
			return nil, true, issuedSession, nil
		}
		out, mErr := json.Marshal(responses)
		return out, false, issuedSession, mErr
	}

	rObj, notif, sid := s.dispatchSingleRaw(ctx, tenant, trimmed)
	if notif || rObj == nil {
		return nil, true, sid, nil
	}
	out, mErr := json.Marshal(rObj)
	return out, false, sid, mErr
}

func (s *Server) dispatchSingleRaw(ctx context.Context, tenant string, raw []byte) (*RPCResponse, bool, string) {
	var req RPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return &RPCResponse{
			JSONRPC: JSONRPCVersion,
			ID:      nullID(),
			Error:   &RPCError{Code: CodeParseError, Message: "parse error: " + err.Error()},
		}, false, ""
	}
	if req.JSONRPC != JSONRPCVersion || strings.TrimSpace(req.Method) == "" {
		id := req.ID
		if len(bytes.TrimSpace(id)) == 0 {
			id = nullID()
		}
		return &RPCResponse{
			JSONRPC: JSONRPCVersion,
			ID:      id,
			Error:   &RPCError{Code: CodeInvalidRequest, Message: "invalid JSON-RPC 2.0 request"},
		}, false, ""
	}

	if isNotificationID(req.ID) || strings.HasPrefix(req.Method, "notifications/") {
		return nil, true, ""
	}

	res, rpcErr, sid := s.executeMethod(ctx, tenant, req.Method, req.Params)
	if rpcErr != nil {
		return &RPCResponse{
			JSONRPC: JSONRPCVersion,
			ID:      req.ID,
			Error:   rpcErr,
		}, false, sid
	}
	return &RPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      req.ID,
		Result:  res,
	}, false, sid
}

func mapBackendError(err error) *RPCError {
	switch {
	case errors.Is(err, ErrUnauthorized):
		return &RPCError{Code: CodeUnauthorized, Message: err.Error()}
	case errors.Is(err, ErrForbidden):
		return &RPCError{Code: CodeForbidden, Message: err.Error()}
	case errors.Is(err, ErrInvalidParam):
		return &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	default:
		return &RPCError{Code: CodeInternalError, Message: err.Error()}
	}
}

func (s *Server) executeMethod(ctx context.Context, tenant string, method string, rawParams json.RawMessage) (any, *RPCError, string) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(bytes.TrimSpace(rawParams)) > 0 {
			if err := json.Unmarshal(rawParams, &p); err != nil {
				return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid initialize params: " + err.Error()}, ""
			}
		}
		negVer := LatestProtocolVersion
		if p.ProtocolVersion == LegacyProtocolVersion {
			negVer = LegacyProtocolVersion
		}
		sid := s.newSessionID(ctx)
		return map[string]any{
			"protocolVersion": negVer,
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"subscribe": false, "listChanged": false},
				"prompts":   map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{
				"name":    s.name,
				"version": s.version,
			},
			"instructions": "SMART-on-FHIR v2 authorized clinical R4 Model Context Protocol server. All tools and resources enforce the caller's SMART scopes, patient compartment, and AuditEvent trail.",
		}, nil, sid

	case "ping":
		return map[string]any{}, nil, ""

	case "tools/list":
		return map[string]any{
			"tools": BuiltinTools(),
		}, nil, ""

	case "tools/call":
		if s.backend == nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "clinical backend not configured"}, ""
		}
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(rawParams, &p); err != nil || strings.TrimSpace(p.Name) == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "tools/call requires tool 'name'"}, ""
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		known := false
		for _, td := range BuiltinTools() {
			if td.Name == p.Name {
				known = true
				break
			}
		}
		if !known {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown MCP tool %q", p.Name)}, ""
		}
		callRes, err := s.backend.CallTool(ctx, tenant, p.Name, p.Arguments)
		if err != nil {
			return nil, mapBackendError(err), ""
		}
		return callRes, nil, ""

	case "resources/list":
		return map[string]any{
			"resources": BuiltinResources(tenant),
		}, nil, ""

	case "resources/templates/list":
		return map[string]any{
			"resourceTemplates": BuiltinResourceTemplates(),
		}, nil, ""

	case "resources/read":
		if s.backend == nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "clinical backend not configured"}, ""
		}
		var p struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(rawParams, &p); err != nil || strings.TrimSpace(p.URI) == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "resources/read requires 'uri'"}, ""
		}
		contents, err := s.backend.ReadResource(ctx, tenant, strings.TrimSpace(p.URI))
		if err != nil {
			return nil, mapBackendError(err), ""
		}
		return map[string]any{
			"contents": contents,
		}, nil, ""

	case "prompts/list":
		return map[string]any{
			"prompts": BuiltinPrompts(),
		}, nil, ""

	case "prompts/get":
		if s.backend == nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "clinical backend not configured"}, ""
		}
		var p struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		if err := json.Unmarshal(rawParams, &p); err != nil || strings.TrimSpace(p.Name) == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "prompts/get requires 'name'"}, ""
		}
		if p.Arguments == nil {
			p.Arguments = map[string]string{}
		}
		promptRes, err := s.backend.GetPrompt(ctx, tenant, p.Name, p.Arguments)
		if err != nil {
			return nil, mapBackendError(err), ""
		}
		return promptRes, nil, ""

	default:
		return nil, &RPCError{Code: CodeMethodNotFound, Message: fmt.Sprintf("method %q not found", method)}, ""
	}
}

// ServeHTTP handles MCP Streamable HTTP (POST, GET SSE/metadata, DELETE session).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request, tenant string) {
	if strings.TrimSpace(tenant) == "" {
		tenant = "default"
	}
	w.Header().Set("Cache-Control", "no-store")

	switch r.Method {
	case http.MethodDelete:
		sid := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
		if sid == "" {
			http.Error(w, `{"error":"missing Mcp-Session-Id header"}`, http.StatusBadRequest)
			return
		}
		if !s.deleteSession(r.Context(), sid) {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		accept := strings.ToLower(r.Header.Get("Accept"))
		if strings.Contains(accept, "text/event-stream") {
			sid := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
			if sid == "" {
				sid = s.newSessionID(r.Context())
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("Mcp-Session-Id", sid)
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "event: endpoint\ndata: /fhir/r4/%s/$mcp\n\n", tenant)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			// If query ?once=true is supplied (for non-blocking health/stream probes), return immediately;
			// otherwise wait until client disconnects.
			if r.URL.Query().Get("once") == "true" {
				return
			}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":            s.name,
			"version":         s.version,
			"protocolVersion": LatestProtocolVersion,
			"tenant":          tenant,
			"transport":       "streamable-http",
			"toolsCount":      len(BuiltinTools()),
		})

	case http.MethodPost:
		existingSID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
		if existingSID != "" {
			if !s.hasSession(r.Context(), existingSID) {
				http.Error(w, `{"error":"invalid or expired Mcp-Session-Id"}`, http.StatusNotFound)
				return
			}
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, MaxStdioLineBytes))
		if err != nil {
			http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
			return
		}
		respJSON, isNotif, newSID, err := s.HandlePayload(r.Context(), tenant, body)
		if err != nil {
			http.Error(w, `{"error":"internal serialization failure"}`, http.StatusInternalServerError)
			return
		}
		outSID := newSID
		if outSID == "" {
			outSID = existingSID
		}
		if outSID != "" {
			w.Header().Set("Mcp-Session-Id", outSID)
		}
		if isNotif {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		accept := strings.ToLower(r.Header.Get("Accept"))
		wantsSSE := r.URL.Query().Get("format") == "sse" ||
			(strings.Contains(accept, "text/event-stream") && !strings.Contains(accept, "application/json"))
		if wantsSSE {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", string(respJSON))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respJSON)

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// ServeStdio processes newline-delimited JSON-RPC 2.0 messages from in and writes
// newline-delimited JSON-RPC 2.0 responses to out until EOF or context cancellation.
func (s *Server) ServeStdio(ctx context.Context, tenant string, in io.Reader, out io.Writer) error {
	if strings.TrimSpace(tenant) == "" {
		tenant = "default"
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), MaxStdioLineBytes)

	var writeMu sync.Mutex
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		respJSON, isNotif, _, err := s.HandlePayload(ctx, tenant, line)
		if err != nil {
			return err
		}
		if isNotif || len(respJSON) == 0 {
			continue
		}
		writeMu.Lock()
		_, wErr := out.Write(append(respJSON, '\n'))
		writeMu.Unlock()
		if wErr != nil {
			return wErr
		}
	}
	return scanner.Err()
}
