package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"github.com/flint-fhir/flint/pkg/mcp"
)

type stubClinicalBackend struct {
	callFn   func(ctx context.Context, tenant, toolName string, args map[string]any) (*mcp.ToolCallResult, error)
	readFn   func(ctx context.Context, tenant, uri string) ([]mcp.ResourceContent, error)
	promptFn func(ctx context.Context, tenant, name string, args map[string]string) (*mcp.GetPromptResult, error)
}

func (b *stubClinicalBackend) CallTool(ctx context.Context, tenant, toolName string, args map[string]any) (*mcp.ToolCallResult, error) {
	if b.callFn != nil {
		return b.callFn(ctx, tenant, toolName, args)
	}
	return &mcp.ToolCallResult{
		Content:           []mcp.ContentBlock{{Type: "text", Text: `{"resourceType":"Patient","id":"pat-1"}`}},
		StructuredContent: map[string]any{"resourceType": "Patient", "id": "pat-1", "tenant": tenant, "tool": toolName},
	}, nil
}

func (b *stubClinicalBackend) ReadResource(ctx context.Context, tenant, uri string) ([]mcp.ResourceContent, error) {
	if b.readFn != nil {
		return b.readFn(ctx, tenant, uri)
	}
	return []mcp.ResourceContent{{
		URI:      uri,
		MimeType: "application/fhir+json",
		Text:     `{"resourceType":"CapabilityStatement","status":"active"}`,
	}}, nil
}

func (b *stubClinicalBackend) GetPrompt(ctx context.Context, tenant, name string, args map[string]string) (*mcp.GetPromptResult, error) {
	if b.promptFn != nil {
		return b.promptFn(ctx, tenant, name, args)
	}
	return &mcp.GetPromptResult{
		Description: "Clinical chart review for " + args["patientId"],
		Messages: []mcp.PromptMessage{
			{Role: "user", Content: mcp.ContentBlock{Type: "text", Text: "Review Patient/" + args["patientId"]}},
		},
	}, nil
}

func TestMCP_StreamableHTTPAndSessionLifecycle(t *testing.T) {
	backend := &stubClinicalBackend{}
	srv := mcp.NewServer("", "", backend)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r, "demo")
	}))
	defer ts.Close()

	// 1. GET metadata discovery (JSON) & GET SSE stream (?once=true)
	metaResp, err := ts.Client().Get(ts.URL)
	require.NoError(t, err)
	defer metaResp.Body.Close()
	assert.Equal(t, http.StatusOK, metaResp.StatusCode)

	sseGetReq, _ := http.NewRequest(http.MethodGet, ts.URL+"?once=true", nil)
	sseGetReq.Header.Set("Accept", "text/event-stream")
	sseGetResp, err := ts.Client().Do(sseGetReq)
	require.NoError(t, err)
	defer sseGetResp.Body.Close()
	assert.Equal(t, http.StatusOK, sseGetResp.StatusCode)
	assert.Contains(t, sseGetResp.Header.Get("Content-Type"), "text/event-stream")
	assert.NotEmpty(t, sseGetResp.Header.Get("Mcp-Session-Id"))

	// 2. Initialize (negotiating both 2025-03-26 and 2024-11-05)
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"test-agent","version":"1.0"}}}`
	initResp, err := ts.Client().Post(ts.URL, "application/json", strings.NewReader(initBody))
	require.NoError(t, err)
	defer initResp.Body.Close()
	require.Equal(t, http.StatusOK, initResp.StatusCode)
	sid := initResp.Header.Get("Mcp-Session-Id")
	require.NotEmpty(t, sid)

	var initOut mcp.RPCResponse
	require.NoError(t, json.NewDecoder(initResp.Body).Decode(&initOut))
	require.Nil(t, initOut.Error)
	resMap := initOut.Result.(map[string]any)
	assert.Equal(t, "2024-11-05", resMap["protocolVersion"])

	// 3. Notification returns 202 Accepted
	notifReq, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	notifReq.Header.Set("Mcp-Session-Id", sid)
	notifResp, err := ts.Client().Do(notifReq)
	require.NoError(t, err)
	notifResp.Body.Close()
	assert.Equal(t, http.StatusAccepted, notifResp.StatusCode)

	// 4. POST with SSE Accept returns text/event-stream event: message
	ssePostReq, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	ssePostReq.Header.Set("Accept", "text/event-stream")
	ssePostReq.Header.Set("Mcp-Session-Id", sid)
	ssePostResp, err := ts.Client().Do(ssePostReq)
	require.NoError(t, err)
	var sseBuf bytes.Buffer
	_, _ = sseBuf.ReadFrom(ssePostResp.Body)
	ssePostResp.Body.Close()
	assert.Equal(t, http.StatusOK, ssePostResp.StatusCode)
	assert.Contains(t, sseBuf.String(), "event: message\ndata: ")
	assert.Contains(t, sseBuf.String(), "fhir_search")
	assert.Contains(t, sseBuf.String(), "patient_summary_ips")

	// 5. Batch JSON-RPC request (ping + resources/list + resources/templates/list + prompts/list + prompts/get + resources/read + tools/call)
	batchBody := `[
		{"jsonrpc":"2.0","id":"p1","method":"ping"},
		{"jsonrpc":"2.0","method":"notifications/cancelled"},
		{"jsonrpc":"2.0","id":"r1","method":"resources/list"},
		{"jsonrpc":"2.0","id":"rt1","method":"resources/templates/list"},
		{"jsonrpc":"2.0","id":"rr1","method":"resources/read","params":{"uri":"fhir://demo/metadata"}},
		{"jsonrpc":"2.0","id":"pr1","method":"prompts/list"},
		{"jsonrpc":"2.0","id":"pg1","method":"prompts/get","params":{"name":"clinical_chart_review","arguments":{"patientId":"pat-1"}}},
		{"jsonrpc":"2.0","id":"tc1","method":"tools/call","params":{"name":"fhir_read","arguments":{"resourceType":"Patient","id":"pat-1"}}}
	]`
	batchReq, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(batchBody))
	batchReq.Header.Set("Accept", "application/json")
	batchReq.Header.Set("Mcp-Session-Id", sid)
	batchResp, err := ts.Client().Do(batchReq)
	require.NoError(t, err)
	var batchOut []mcp.RPCResponse
	require.NoError(t, json.NewDecoder(batchResp.Body).Decode(&batchOut))
	batchResp.Body.Close()
	require.Len(t, batchOut, 7, "notification inside batch must not emit response")
	for _, r := range batchOut {
		assert.Nil(t, r.Error, "batch item %s failed: %+v", string(r.ID), r.Error)
	}

	// 6. DELETE session -> 204, subsequent POST with deleted session -> 404
	delNoHdr, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	delNoHdrResp, _ := ts.Client().Do(delNoHdr)
	delNoHdrResp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, delNoHdrResp.StatusCode)

	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	delReq.Header.Set("Mcp-Session-Id", sid)
	delResp, err := ts.Client().Do(delReq)
	require.NoError(t, err)
	delResp.Body.Close()
	assert.Equal(t, http.StatusNoContent, delResp.StatusCode)

	delAgain, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	delAgain.Header.Set("Mcp-Session-Id", sid)
	delAgainResp, _ := ts.Client().Do(delAgain)
	delAgainResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, delAgainResp.StatusCode)

	stalePost, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"ping"}`))
	stalePost.Header.Set("Mcp-Session-Id", sid)
	staleResp, _ := ts.Client().Do(stalePost)
	staleResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, staleResp.StatusCode)

	// Method not allowed (PUT)
	putReq, _ := http.NewRequest(http.MethodPut, ts.URL, nil)
	putResp, _ := ts.Client().Do(putReq)
	putResp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, putResp.StatusCode)
}

func TestMCP_StdioAndErrorMapping(t *testing.T) {
	backend := &stubClinicalBackend{
		callFn: func(_ context.Context, _, toolName string, args map[string]any) (*mcp.ToolCallResult, error) {
			switch args["mode"] {
			case "unauth":
				return nil, mcp.ErrUnauthorized
			case "forbid":
				return nil, mcp.ErrForbidden
			case "badparam":
				return nil, mcp.ErrInvalidParam
			case "boom":
				return nil, errors.New("storage failure")
			}
			return &mcp.ToolCallResult{
				Content: []mcp.ContentBlock{{Type: "text", Text: "ok:" + toolName}},
			}, nil
		},
	}
	srv := mcp.NewServer("flint-stdio", "0.7.0", backend)

	inputLines := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		``,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"unknown/method"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nonexistent_tool"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fhir_read","arguments":{"mode":"unauth"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"fhir_read","arguments":{"mode":"forbid"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"fhir_read","arguments":{"mode":"badparam"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"fhir_read","arguments":{"mode":"boom"}}}`,
		`{malformed-json`,
		`[]`,
		`{"jsonrpc":"1.0","id":8,"method":""}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	require.NoError(t, srv.ServeStdio(t.Context(), "demo", strings.NewReader(inputLines), &out))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 10, "10 requests (excluding blank line and notification) must produce 10 JSON-RPC lines")

	var r4, r5 mcp.RPCResponse
	require.NoError(t, json.Unmarshal([]byte(lines[3]), &r4))
	assert.Equal(t, mcp.CodeUnauthorized, r4.Error.Code)
	require.NoError(t, json.Unmarshal([]byte(lines[4]), &r5))
	assert.Equal(t, mcp.CodeForbidden, r5.Error.Code)

	// Nil backend defensive checks
	nilSrv := mcp.NewServer("nil", "0.7.0", nil)
	respBytes, _, _, err := nilSrv.HandlePayload(t.Context(), "demo", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fhir_search"}}`))
	require.NoError(t, err)
	assert.Contains(t, string(respBytes), "-32603")
}

func TestProperty_MCPJSONRPCFramingInvariants(t *testing.T) {
	srv := mcp.NewServer("flint-pbt", "0.7.0", &stubClinicalBackend{})
	toolNames := make([]string, 0, len(mcp.BuiltinTools()))
	for _, td := range mcp.BuiltinTools() {
		toolNames = append(toolNames, td.Name)
	}

	hegel.Test(t, func(ht *hegel.T) {
		idNum := hegel.Draw(ht, hegel.Integers(1, 99999))
		toolName := hegel.Draw(ht, hegel.SampledFrom(toolNames))
		patID := hegel.Draw(ht, hegel.FromRegex(`pat-[a-z0-9]{3,8}`, true))

		reqBytes, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      idNum,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      toolName,
				"arguments": map[string]any{"resourceType": "Patient", "id": patID, "patientId": patID, "measureId": "m1", "serviceId": "s1", "resource": map[string]any{"resourceType": "Patient"}},
			},
		})
		respBytes, isNotif, _, err := srv.HandlePayload(t.Context(), "demo", reqBytes)
		if err != nil || isNotif {
			ht.Fatalf("unexpected HandlePayload error=%v isNotif=%v", err, isNotif)
		}
		var rpcResp mcp.RPCResponse
		if err := json.Unmarshal(respBytes, &rpcResp); err != nil {
			ht.Fatalf("invalid JSON-RPC response: %v", err)
		}
		if rpcResp.Error != nil {
			ht.Fatalf("unexpected RPC error: %+v", rpcResp.Error)
		}
		if string(rpcResp.ID) != json.Number(strings.TrimSpace(string(rpcResp.ID))).String() {
			ht.Fatalf("mismatched JSON-RPC ID: %s", string(rpcResp.ID))
		}
	}, hegel.WithTestCases(100))
}
