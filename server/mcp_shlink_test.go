package server_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/shlink"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/sqlite"
)

func newMCPAndSHLinkTestServer(t testing.TB) (*server.Server, *httptest.Server) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "mcp-shlink.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, sqlite.RunMigrations(context.Background(), sqlDB, sqlite.JournalWAL))
	st := sqlite.New(sqlDB, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(dbPath))

	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	srv.SetAuditRecorder(audit.NewStoreRecorder(st))
	srv.EnableDevTokenEnforcement()

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func seedMCPAndSHLinkFixture(t testing.TB, srv *server.Server, ts *httptest.Server, tenant string) string {
	t.Helper()
	sysTok, err := srv.IssueDevToken("seed-admin", "system/*.cruds", tenant, "", time.Hour)
	require.NoError(t, err)

	putResource := func(path string, payload map[string]any) {
		raw, mErr := json.Marshal(payload)
		require.NoError(t, mErr)
		req, rErr := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/"+tenant+path, bytes.NewReader(raw))
		require.NoError(t, rErr)
		req.Header.Set("Authorization", "Bearer "+sysTok)
		req.Header.Set("Content-Type", "application/fhir+json")
		resp, dErr := ts.Client().Do(req)
		require.NoError(t, dErr)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, resp.StatusCode, "PUT %s failed: %s", path, string(body))
	}

	putResource("/Patient/pat-alice-001", map[string]any{
		"resourceType": "Patient",
		"id":           "pat-alice-001",
		"active":       true,
		"identifier":   []map[string]any{{"system": "http://hospital.smarthealthit.org", "value": "MRN-ALICE-001"}},
		"name":         []map[string]any{{"use": "official", "family": "Smith", "given": []string{"Alice"}}},
		"gender":       "female",
		"birthDate":    "1985-04-12",
	})
	putResource("/Patient/pat-bob-002", map[string]any{
		"resourceType": "Patient",
		"id":           "pat-bob-002",
		"active":       true,
		"identifier":   []map[string]any{{"system": "http://hospital.smarthealthit.org", "value": "MRN-BOB-002"}},
		"name":         []map[string]any{{"use": "official", "family": "Jones", "given": []string{"Robert"}}},
		"gender":       "male",
		"birthDate":    "1978-09-21",
	})
	putResource("/Observation/obs-alice-hr", map[string]any{
		"resourceType":      "Observation",
		"id":                "obs-alice-hr",
		"status":            "final",
		"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}}},
		"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
		"effectiveDateTime": "2026-02-01T10:00:00Z",
		"valueQuantity":     map[string]any{"value": 72, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"},
	})
	putResource("/Observation/obs-alice-soc", map[string]any{
		"resourceType":      "Observation",
		"id":                "obs-alice-soc",
		"status":            "final",
		"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history"}}}},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "72166-2", "display": "Tobacco smoking status"}}},
		"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
		"effectiveDateTime": "2026-02-01T10:05:00Z",
		"valueCodeableConcept": map[string]any{
			"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "266919005", "display": "Never smoked tobacco"}},
		},
	})
	putResource("/Observation/obs-bob-hr", map[string]any{
		"resourceType":      "Observation",
		"id":                "obs-bob-hr",
		"status":            "final",
		"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
		"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}}},
		"subject":           map[string]any{"reference": "Patient/pat-bob-002"},
		"effectiveDateTime": "2026-02-01T11:00:00Z",
		"valueQuantity":     map[string]any{"value": 81, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"},
	})
	putResource("/Immunization/imm-alice-covid", map[string]any{
		"resourceType":       "Immunization",
		"id":                 "imm-alice-covid",
		"status":             "completed",
		"vaccineCode":        map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/cvx", "code": "208", "display": "COVID-19, mRNA"}}},
		"patient":            map[string]any{"reference": "Patient/pat-alice-001"},
		"occurrenceDateTime": "2025-11-10",
	})
	putResource("/Condition/cond-alice-htn", map[string]any{
		"resourceType":   "Condition",
		"id":             "cond-alice-htn",
		"clinicalStatus": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}}},
		"verificationStatus": map[string]any{
			"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}},
		},
		"category": []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item"}}}},
		"code":     map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertensive disorder"}}},
		"subject":  map[string]any{"reference": "Patient/pat-alice-001"},
	})
	putResource("/ViewDefinition/vd-pat-flat", map[string]any{
		"resourceType": "ViewDefinition",
		"id":           "vd-pat-flat",
		"name":         "patient_demographics",
		"status":       "active",
		"resource":     "Patient",
		"select": []map[string]any{
			{
				"column": []map[string]any{
					{"name": "id", "path": "getResourceKey()"},
					{"name": "gender", "path": "gender"},
				},
			},
		},
	})
	return sysTok
}

type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func callMCPHTTP(t testing.TB, ts *httptest.Server, tenant, token, sessionID string, payload any) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/$mcp", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	return resp, body
}

func TestServer_MCPStreamableHTTP_ToolsResourcesPromptsAndSMARTv2Isolation(t *testing.T) {
	srv, ts := newMCPAndSHLinkTestServer(t)
	tenant := "demo"
	sysTok := seedMCPAndSHLinkFixture(t, srv, ts, tenant)

	// 1. ValidateBearerToken & IssueDevToken edge cases + revocation enforcement
	_, err := srv.ValidateBearerToken(t.Context(), "   ")
	require.Error(t, err)
	shortTok, err := srv.IssueDevToken("revoked-agent", "system/*.rs", tenant, "", 0)
	require.NoError(t, err)
	secCtx, err := srv.ValidateBearerToken(t.Context(), "Bearer "+shortTok)
	require.NoError(t, err)
	assert.Equal(t, "revoked-agent", secCtx.GetSubject())

	revForm := url.Values{"token": {shortTok}}
	revResp, err := ts.Client().Post(ts.URL+"/oauth2/revoke", "application/x-www-form-urlencoded", strings.NewReader(revForm.Encode()))
	require.NoError(t, err)
	_ = revResp.Body.Close()
	assert.Equal(t, http.StatusOK, revResp.StatusCode)
	_, err = srv.ValidateBearerToken(t.Context(), shortTok)
	require.ErrorContains(t, err, "revoked")

	// Unauthenticated & revoked requests to /$mcp must return 401 Unauthorized
	unauthResp, _ := callMCPHTTP(t, ts, tenant, "", "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	assert.Equal(t, http.StatusUnauthorized, unauthResp.StatusCode)
	revMCPResp, _ := callMCPHTTP(t, ts, tenant, shortTok, "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	assert.Equal(t, http.StatusUnauthorized, revMCPResp.StatusCode)

	// Cross-tenant token rejected with 403 Forbidden
	otherTenantTok, err := srv.IssueDevToken("wrong-tenant-agent", "system/*.rs", "other-tenant", "", time.Hour)
	require.NoError(t, err)
	wrongTenantResp, _ := callMCPHTTP(t, ts, tenant, otherTenantTok, "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	assert.Equal(t, http.StatusForbidden, wrongTenantResp.StatusCode)

	// 2. GET discovery JSON + GET SSE stream + Initialize + Notifications + SSE POST + Batch + DELETE session
	discReq, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/$mcp", nil)
	require.NoError(t, err)
	discReq.Header.Set("Authorization", "Bearer "+sysTok)
	discResp, err := ts.Client().Do(discReq)
	require.NoError(t, err)
	discBytes, _ := io.ReadAll(discResp.Body)
	_ = discResp.Body.Close()
	assert.Equal(t, http.StatusOK, discResp.StatusCode)
	assert.Contains(t, string(discBytes), "flint-fhir-mcp")

	initResp, initBytes := callMCPHTTP(t, ts, tenant, sysTok, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26"},
	})
	assert.Equal(t, http.StatusOK, initResp.StatusCode)
	sessionID := initResp.Header.Get("Mcp-Session-Id")
	require.NotEmpty(t, sessionID)
	assert.Contains(t, string(initBytes), `"protocolVersion":"2025-03-26"`)

	// Notification returns 202 Accepted
	notifResp, _ := callMCPHTTP(t, ts, tenant, sysTok, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	assert.Equal(t, http.StatusAccepted, notifResp.StatusCode)

	// GET SSE stream with short context cancellation
	sseCtx, cancelSSE := context.WithTimeout(t.Context(), 150*time.Millisecond)
	sseGetReq, err := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/$mcp", nil)
	require.NoError(t, err)
	sseGetReq.Header.Set("Authorization", "Bearer "+sysTok)
	sseGetReq.Header.Set("Accept", "text/event-stream")
	sseGetReq.Header.Set("Mcp-Session-Id", sessionID)
	sseGetResp, err := ts.Client().Do(sseGetReq)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, sseGetResp.StatusCode)
	assert.Contains(t, sseGetResp.Header.Get("Content-Type"), "text/event-stream")
	sseLine, _ := bufio.NewReader(sseGetResp.Body).ReadString('\n')
	assert.Contains(t, sseLine, "event: endpoint")
	cancelSSE()
	_ = sseGetResp.Body.Close()

	// POST with Accept: text/event-stream returns event: message framing
	ssePostReq, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/$mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	require.NoError(t, err)
	ssePostReq.Header.Set("Authorization", "Bearer "+sysTok)
	ssePostReq.Header.Set("Content-Type", "application/json")
	ssePostReq.Header.Set("Accept", "text/event-stream")
	ssePostReq.Header.Set("Mcp-Session-Id", sessionID)
	ssePostResp, err := ts.Client().Do(ssePostReq)
	require.NoError(t, err)
	ssePostBytes, _ := io.ReadAll(ssePostResp.Body)
	_ = ssePostResp.Body.Close()
	assert.Equal(t, http.StatusOK, ssePostResp.StatusCode)
	assert.Contains(t, string(ssePostBytes), "event: message")
	assert.Contains(t, string(ssePostBytes), "patient_summary_ips")

	// Discover a built-in CDS service ID for cds_hooks_invoke
	cdsDiscResp, err := ts.Client().Get(ts.URL + "/cds-services")
	require.NoError(t, err)
	var cdsDisc struct {
		Services []struct {
			ID   string `json:"id"`
			Hook string `json:"hook"`
		} `json:"services"`
	}
	require.NoError(t, json.NewDecoder(cdsDiscResp.Body).Decode(&cdsDisc))
	_ = cdsDiscResp.Body.Close()
	require.NotEmpty(t, cdsDisc.Services)
	cdsSvc := cdsDisc.Services[0]

	// 3. Exercise all 9 built-in clinical tools, resources/read, and prompts/get with authorized token
	toolChecks := []struct {
		name     string
		args     map[string]any
		contains string
	}{
		{
			name:     "fhir_search",
			args:     map[string]any{"resourceType": "Observation", "query": map[string]any{"patient": "Patient/pat-alice-001", "category": []any{"vital-signs"}}, "count": 10},
			contains: "obs-alice-hr",
		},
		{
			name:     "fhir_read",
			args:     map[string]any{"resourceType": "Patient", "id": "pat-alice-001"},
			contains: "MRN-ALICE-001",
		},
		{
			name:     "patient_summary_ips",
			args:     map[string]any{"patientId": "pat-alice-001"},
			contains: "60591-5",
		},
		{
			name:     "patient_everything",
			args:     map[string]any{"patientId": "pat-alice-001", "types": "Patient,Observation"},
			contains: "obs-alice-hr",
		},
		{
			name:     "run_view_definition",
			args:     map[string]any{"viewDefinitionId": "vd-pat-flat", "patientId": "pat-alice-001"},
			contains: "pat-alice-001",
		},
		{
			name: "run_view_definition",
			args: map[string]any{
				"patientId": "pat-alice-001",
				"viewDefinition": map[string]any{
					"resourceType": "ViewDefinition",
					"resource":     "Observation",
					"select": []map[string]any{
						{"column": []map[string]any{{"name": "id", "path": "getResourceKey()"}}},
					},
				},
			},
			contains: "obs-alice",
		},
		{
			name:     "evaluate_measure",
			args:     map[string]any{"measureId": "cms165-bp-control", "subject": "pat-alice-001", "periodStart": "2025-01-01", "periodEnd": "2027-01-01", "reportType": "individual"},
			contains: "MeasureReport",
		},
		{
			name:     "cds_hooks_invoke",
			args:     map[string]any{"serviceId": cdsSvc.ID, "patientId": "pat-alice-001", "hook": cdsSvc.Hook},
			contains: "cards",
		},
		{
			name: "validate_resource",
			args: map[string]any{
				"profile": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient",
				"resource": map[string]any{
					"resourceType": "Patient",
					"id":           "val-pat-ok",
					"identifier":   []map[string]any{{"system": "http://hospital.smarthealthit.org", "value": "MRN-VAL-1"}},
					"name":         []map[string]any{{"family": " Hopper", "given": []string{"Grace"}}},
					"gender":       "female",
				},
			},
			contains: "OperationOutcome",
		},
		{
			name: "create_or_update_resource",
			args: map[string]any{
				"resource": map[string]any{
					"resourceType":      "Observation",
					"id":                "obs-alice-mcp-write",
					"status":            "final",
					"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
					"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8310-5"}}},
					"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
					"effectiveDateTime": "2026-02-02T09:00:00Z",
					"valueQuantity":     map[string]any{"value": 36.8, "unit": "Cel", "system": "http://unitsofmeasure.org", "code": "Cel"},
				},
			},
			contains: "obs-alice-mcp-write",
		},
	}

	for idx, tc := range toolChecks {
		resp, b := callMCPHTTP(t, ts, tenant, sysTok, sessionID, map[string]any{
			"jsonrpc": "2.0",
			"id":      100 + idx,
			"method":  "tools/call",
			"params":  map[string]any{"name": tc.name, "arguments": tc.args},
		})
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		var r rpcReply
		require.NoError(t, json.Unmarshal(b, &r))
		require.Nil(t, r.Error, "tool %s returned unexpected JSON-RPC error: %+v", tc.name, r.Error)
		assert.Contains(t, string(r.Result), tc.contains, "tool %s missing expected content", tc.name)
	}

	// Also exercise POST create (no id) via create_or_update_resource
	postCreateResp, postCreateBytes := callMCPHTTP(t, ts, tenant, sysTok, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      150,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "create_or_update_resource",
			"arguments": map[string]any{
				"resource": map[string]any{
					"resourceType":      "Observation",
					"status":            "final",
					"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
					"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "9279-1"}}},
					"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
					"effectiveDateTime": "2026-02-02T09:05:00Z",
					"valueQuantity":     map[string]any{"value": 16, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"},
				},
			},
		},
	})
	assert.Equal(t, http.StatusOK, postCreateResp.StatusCode)
	assert.Contains(t, string(postCreateBytes), "9279-1")

	// Exercise resources/list, resources/templates/list, resources/read, prompts/list, and prompts/get
	batchResp, batchBytes := callMCPHTTP(t, ts, tenant, sysTok, sessionID, []map[string]any{
		{"jsonrpc": "2.0", "id": 201, "method": "resources/list"},
		{"jsonrpc": "2.0", "id": 202, "method": "resources/templates/list"},
		{"jsonrpc": "2.0", "id": 203, "method": "resources/read", "params": map[string]any{"uri": "fhir://demo/metadata"}},
		{"jsonrpc": "2.0", "id": 204, "method": "resources/read", "params": map[string]any{"uri": "fhir://demo/smart-configuration"}},
		{"jsonrpc": "2.0", "id": 205, "method": "resources/read", "params": map[string]any{"uri": "fhir://demo/Patient/pat-alice-001"}},
		{"jsonrpc": "2.0", "id": 206, "method": "prompts/list"},
		{"jsonrpc": "2.0", "id": 207, "method": "prompts/get", "params": map[string]any{"name": "clinical_chart_review", "arguments": map[string]string{"patientId": "pat-alice-001", "focus": "hypertension"}}},
		{"jsonrpc": "2.0", "id": 208, "method": "prompts/get", "params": map[string]any{"name": "care_gap_closure_brief", "arguments": map[string]string{"patientId": "pat-alice-001"}}},
	})
	assert.Equal(t, http.StatusOK, batchResp.StatusCode)
	var batchReplies []rpcReply
	require.NoError(t, json.Unmarshal(batchBytes, &batchReplies))
	require.Len(t, batchReplies, 8)
	for _, br := range batchReplies {
		assert.Nil(t, br.Error, "batch id=%v failed: %+v", br.ID, br.Error)
	}

	// 4. Enforce SMART v2 patient compartment isolation & granular ?category=vital-signs scope
	aliceGranularTok, err := srv.IssueDevToken(
		"patient-alice-app",
		"patient/Patient.rs patient/Observation.rs?category=http://terminology.hl7.org/CodeSystem/observation-category|vital-signs",
		tenant,
		"pat-alice-001",
		time.Hour,
	)
	require.NoError(t, err)

	// Allowed granular search returns Alice's vital-signs Observation and omits social-history + Bob's Observation
	_, granSearchBytes := callMCPHTTP(t, ts, tenant, aliceGranularTok, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      301,
		"method":  "tools/call",
		"params":  map[string]any{"name": "fhir_search", "arguments": map[string]any{"resourceType": "Observation"}},
	})
	assert.Contains(t, string(granSearchBytes), "obs-alice-hr")
	assert.NotContains(t, string(granSearchBytes), "obs-alice-soc")
	assert.NotContains(t, string(granSearchBytes), "obs-bob-hr")

	// Cross-patient, out-of-category, and write attempts must all fail with -32003 Forbidden
	forbiddenCalls := []map[string]any{
		{"jsonrpc": "2.0", "id": 310, "method": "tools/call", "params": map[string]any{"name": "fhir_read", "arguments": map[string]any{"resourceType": "Patient", "id": "pat-bob-002"}}},
		{"jsonrpc": "2.0", "id": 311, "method": "tools/call", "params": map[string]any{"name": "fhir_read", "arguments": map[string]any{"resourceType": "Observation", "id": "obs-alice-soc"}}},
		{"jsonrpc": "2.0", "id": 312, "method": "tools/call", "params": map[string]any{"name": "patient_summary_ips", "arguments": map[string]any{"patientId": "pat-bob-002"}}},
		{"jsonrpc": "2.0", "id": 313, "method": "tools/call", "params": map[string]any{"name": "patient_everything", "arguments": map[string]any{"patientId": "pat-bob-002"}}},
		{"jsonrpc": "2.0", "id": 314, "method": "tools/call", "params": map[string]any{"name": "run_view_definition", "arguments": map[string]any{"viewDefinitionId": "vd-pat-flat", "patientId": "pat-bob-002"}}},
		{"jsonrpc": "2.0", "id": 315, "method": "tools/call", "params": map[string]any{"name": "evaluate_measure", "arguments": map[string]any{"measureId": "cms165-bp-control", "subject": "Patient/pat-bob-002"}}},
		{"jsonrpc": "2.0", "id": 316, "method": "tools/call", "params": map[string]any{"name": "cds_hooks_invoke", "arguments": map[string]any{"serviceId": cdsSvc.ID, "patientId": "pat-bob-002"}}},
		{"jsonrpc": "2.0", "id": 317, "method": "tools/call", "params": map[string]any{"name": "validate_resource", "arguments": map[string]any{"resource": map[string]any{"resourceType": "MedicationRequest"}}}},
		{"jsonrpc": "2.0", "id": 318, "method": "tools/call", "params": map[string]any{"name": "create_or_update_resource", "arguments": map[string]any{"resource": map[string]any{"resourceType": "Patient", "id": "pat-alice-001", "gender": "female"}}}},
		{"jsonrpc": "2.0", "id": 319, "method": "resources/read", "params": map[string]any{"uri": "fhir://demo/Patient/pat-bob-002"}},
		{"jsonrpc": "2.0", "id": 320, "method": "prompts/get", "params": map[string]any{"name": "clinical_chart_review", "arguments": map[string]string{"patientId": "pat-bob-002"}}},
	}
	for _, fc := range forbiddenCalls {
		_, b := callMCPHTTP(t, ts, tenant, aliceGranularTok, "", fc)
		var r rpcReply
		require.NoError(t, json.Unmarshal(b, &r))
		require.NotNil(t, r.Error, "expected -32003 Forbidden for %v, got %s", fc["id"], string(b))
		assert.Equal(t, -32003, r.Error.Code, "unexpected error code for %v: %+v", fc["id"], r.Error)
	}

	// Terminate MCP session via DELETE
	delReq, err := http.NewRequest(http.MethodDelete, ts.URL+"/fhir/r4/"+tenant+"/$mcp", nil)
	require.NoError(t, err)
	delReq.Header.Set("Authorization", "Bearer "+sysTok)
	delReq.Header.Set("Mcp-Session-Id", sessionID)
	delResp, err := ts.Client().Do(delReq)
	require.NoError(t, err)
	_ = delResp.Body.Close()
	assert.Equal(t, http.StatusNoContent, delResp.StatusCode)

	// Reusing deleted session returns 404 Not Found
	staleResp, _ := callMCPHTTP(t, ts, tenant, sysTok, sessionID, map[string]any{"jsonrpc": "2.0", "id": 999, "method": "ping"})
	assert.Equal(t, http.StatusNotFound, staleResp.StatusCode)

	// 5. Verify ASTM E2147 AuditEvents captured MCP tool & resource invocations
	audReq, err := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/AuditEvent?_count=200", nil)
	require.NoError(t, err)
	audReq.Header.Set("Authorization", "Bearer "+sysTok)
	audResp, err := ts.Client().Do(audReq)
	require.NoError(t, err)
	audBytes, _ := io.ReadAll(audResp.Body)
	_ = audResp.Body.Close()
	assert.Equal(t, http.StatusOK, audResp.StatusCode)
	assert.Contains(t, string(audBytes), "mcp:fhir_search")
	assert.Contains(t, string(audBytes), "mcp:patient_summary_ips")
	assert.Contains(t, string(audBytes), "mcp:resources/read")
}

func TestServer_SHLinkEndToEnd_CryptoPasscodeLockoutExpiryRevokeAndWireParity(t *testing.T) {
	srv, ts := newMCPAndSHLinkTestServer(t)
	tenant := "demo"
	sysTok := seedMCPAndSHLinkFixture(t, srv, ts, tenant)

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	srv.SHLinkManager().SetClock(func() time.Time { return now })

	aliceTok, err := srv.IssueDevToken("alice-portal", "patient/Patient.rs patient/Observation.rs patient/Immunization.rs patient/Condition.rs", tenant, "pat-alice-001", time.Hour)
	require.NoError(t, err)
	noPatScopeTok, err := srv.IssueDevToken("obs-only", "system/Observation.rs", tenant, "", time.Hour)
	require.NoError(t, err)

	// 1. Method, missing patient, scope, and cross-patient guards on $shlink
	getMethodReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-alice-001/$shlink", nil)
	getMethodReq.Header.Set("Authorization", "Bearer "+aliceTok)
	getMethodResp, err := ts.Client().Do(getMethodReq)
	require.NoError(t, err)
	_ = getMethodResp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, getMethodResp.StatusCode)

	missingPatReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/$shlink", strings.NewReader(`{}`))
	missingPatReq.Header.Set("Authorization", "Bearer "+sysTok)
	missingPatReq.Header.Set("Content-Type", "application/fhir+json")
	missingPatResp, err := ts.Client().Do(missingPatReq)
	require.NoError(t, err)
	_ = missingPatResp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, missingPatResp.StatusCode)

	noScopeReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-alice-001/$shlink", nil)
	noScopeReq.Header.Set("Authorization", "Bearer "+noPatScopeTok)
	noScopeResp, err := ts.Client().Do(noScopeReq)
	require.NoError(t, err)
	_ = noScopeResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, noScopeResp.StatusCode)

	crossPatReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-bob-002/$shlink", nil)
	crossPatReq.Header.Set("Authorization", "Bearer "+aliceTok)
	crossPatResp, err := ts.Client().Do(crossPatReq)
	require.NoError(t, err)
	_ = crossPatResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, crossPatResp.StatusCode)

	// 2. Issue passcode-protected + maxViews=2 SMART Health Link via FHIR Parameters
	issueParams := map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "label", "valueString": "Alice Travel Summary"},
			{"name": "passcode", "valueString": "482910"},
			{"name": "maxViews", "valueInteger": 2},
			{"name": "exp", "valueInteger": now.Add(2 * time.Hour).Unix()},
			{"name": "longTerm", "valueBoolean": true},
			{"name": "viewerUrl", "valueUrl": "https://viewer.example.org/shl"},
		},
	}
	rawIssue, _ := json.Marshal(issueParams)
	issueReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-alice-001/$shlink", bytes.NewReader(rawIssue))
	issueReq.Header.Set("Authorization", "Bearer "+aliceTok)
	issueReq.Header.Set("Content-Type", "application/fhir+json")
	issueResp, err := ts.Client().Do(issueReq)
	require.NoError(t, err)
	issueBody, _ := io.ReadAll(issueResp.Body)
	_ = issueResp.Body.Close()
	require.Equal(t, http.StatusOK, issueResp.StatusCode, string(issueBody))

	var outParams struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name         string `json:"name"`
			ValueURI     string `json:"valueUri"`
			ValueURL     string `json:"valueUrl"`
			ValueString  string `json:"valueString"`
			ValueInteger int64  `json:"valueInteger"`
		} `json:"parameter"`
	}
	require.NoError(t, json.Unmarshal(issueBody, &outParams))
	assert.Equal(t, "Parameters", outParams.ResourceType)

	var shlinkURI, linkID, manifestURL, qrSVG, flagVal string
	for _, p := range outParams.Parameter {
		switch p.Name {
		case "shlink":
			shlinkURI = p.ValueURI
		case "linkId":
			linkID = p.ValueString
		case "manifestUrl":
			manifestURL = p.ValueURL
		case "qrCodeSvg":
			qrSVG = p.ValueString
		case "flag":
			flagVal = p.ValueString
		}
	}
	require.True(t, strings.HasPrefix(shlinkURI, "https://viewer.example.org/shl#shlink:/"))
	require.NotEmpty(t, linkID)
	require.Contains(t, manifestURL, "/shlink/"+linkID+"/manifest")
	assert.Equal(t, "LP", flagVal)
	assert.Contains(t, qrSVG, "<svg")

	decodedPayload, err := shlink.DecodeSHLinkURI(shlinkURI)
	require.NoError(t, err)
	assert.Equal(t, manifestURL, decodedPayload.URL)
	assert.Equal(t, "LP", decodedPayload.Flag)
	assert.Equal(t, "Alice Travel Summary", decodedPayload.Label)

	aesKey, err := shlink.DecodeAES256Key(decodedPayload.Key)
	require.NoError(t, err)
	require.Len(t, aesKey, 32)

	// 3. Verify XML and binary Proto3 wire parity on type-level POST /Patient/$shlink
	for _, format := range []string{"application/fhir+xml", "application/x-protobuf"} {
		wireBody := `{"patient":"Patient/pat-alice-001","label":"Wire Check","includeSHC":true,"includeIPS":false}`
		wReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/$shlink?_format="+url.QueryEscape(format), strings.NewReader(wireBody))
		wReq.Header.Set("Authorization", "Bearer "+aliceTok)
		wReq.Header.Set("Content-Type", "application/json")
		wResp, dErr := ts.Client().Do(wReq)
		require.NoError(t, dErr)
		wBytes, _ := io.ReadAll(wResp.Body)
		_ = wResp.Body.Close()
		assert.Equal(t, http.StatusOK, wResp.StatusCode)
		assert.NotEmpty(t, wBytes)
	}

	// 4. Public /shlink/{id}/manifest CORS, validation, passcode check, JWE decryption, and maxViews
	optReq, _ := http.NewRequest(http.MethodOptions, manifestURL, nil)
	optResp, err := ts.Client().Do(optReq)
	require.NoError(t, err)
	_ = optResp.Body.Close()
	assert.Equal(t, http.StatusNoContent, optResp.StatusCode)
	assert.Equal(t, "*", optResp.Header.Get("Access-Control-Allow-Origin"))

	// Malformed JSON -> 400
	badJSONResp, err := ts.Client().Post(manifestURL, "application/json", strings.NewReader(`{bad-json`))
	require.NoError(t, err)
	_ = badJSONResp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, badJSONResp.StatusCode)

	// Missing recipient -> 400
	noRecipResp, err := ts.Client().Post(manifestURL, "application/json", strings.NewReader(`{"passcode":"482910"}`))
	require.NoError(t, err)
	_ = noRecipResp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, noRecipResp.StatusCode)

	// Wrong passcode -> 401 with remainingAttempts=4
	wrongPinResp, err := ts.Client().Post(manifestURL, "application/json", strings.NewReader(`{"recipient":"ER Triage Nurse","passcode":"000000"}`))
	require.NoError(t, err)
	var pinErrDoc map[string]any
	require.NoError(t, json.NewDecoder(wrongPinResp.Body).Decode(&pinErrDoc))
	_ = wrongPinResp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, wrongPinResp.StatusCode)
	assert.Equal(t, float64(4), pinErrDoc["remainingAttempts"])

	// View #1 with correct passcode + embeddedLengthMax inline JWE + location download JWE
	view1Resp, err := ts.Client().Post(manifestURL, "application/json", strings.NewReader(`{"recipient":"ER Triage Nurse","passcode":"482910","embeddedLengthMax":100000}`))
	require.NoError(t, err)
	var manifest1 shlink.ManifestResponse
	require.NoError(t, json.NewDecoder(view1Resp.Body).Decode(&manifest1))
	_ = view1Resp.Body.Close()
	require.Equal(t, http.StatusOK, view1Resp.StatusCode)
	require.Len(t, manifest1.Files, 2)

	for _, f := range manifest1.Files {
		require.NotEmpty(t, f.Embedded, "expected embedded JWE when embeddedLengthMax is large")
		// Also fetch via location URL and verify byte-for-byte JWE + decryption
		cResp, cErr := ts.Client().Get(f.Location)
		require.NoError(t, cErr)
		jweBytes, _ := io.ReadAll(cResp.Body)
		_ = cResp.Body.Close()
		assert.Equal(t, http.StatusOK, cResp.StatusCode)
		assert.Equal(t, "application/jose", cResp.Header.Get("Content-Type"))

		plaintext, cty, decErr := shlink.DecryptCompactJWE(aesKey, string(jweBytes))
		require.NoError(t, decErr)
		assert.Equal(t, f.ContentType, cty)
		switch f.ContentType {
		case "application/smart-health-card":
			assert.Contains(t, string(plaintext), "verifiableCredential")
		case "application/fhir+json":
			assert.Contains(t, string(plaintext), "60591-5")
			assert.Contains(t, string(plaintext), "MRN-ALICE-001")
		default:
			t.Fatalf("unexpected manifest contentType %q", f.ContentType)
		}
	}

	// View #2 succeeds (reaches maxViews=2), View #3 returns 410 Gone
	view2Resp, err := ts.Client().Post(manifestURL, "application/json", strings.NewReader(`{"recipient":"ER Physician","passcode":"482910"}`))
	require.NoError(t, err)
	_ = view2Resp.Body.Close()
	assert.Equal(t, http.StatusOK, view2Resp.StatusCode)

	view3Resp, err := ts.Client().Post(manifestURL, "application/json", strings.NewReader(`{"recipient":"ER Physician","passcode":"482910"}`))
	require.NoError(t, err)
	_ = view3Resp.Body.Close()
	assert.Equal(t, http.StatusGone, view3Resp.StatusCode)

	// 5. Verify 5-strike brute-force passcode lockout (429 Too Many Requests)
	lockIssueReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-alice-001/$shlink?passcode=999999", nil)
	lockIssueReq.Header.Set("Authorization", "Bearer "+aliceTok)
	lockIssueResp, err := ts.Client().Do(lockIssueReq)
	require.NoError(t, err)
	var lockParams struct {
		Parameter []struct {
			Name     string `json:"name"`
			ValueURL string `json:"valueUrl"`
		} `json:"parameter"`
	}
	require.NoError(t, json.NewDecoder(lockIssueResp.Body).Decode(&lockParams))
	_ = lockIssueResp.Body.Close()
	var lockManifestURL string
	for _, p := range lockParams.Parameter {
		if p.Name == "manifestUrl" {
			lockManifestURL = p.ValueURL
		}
	}
	for attempt := 1; attempt <= shlink.DefaultMaxPasscodeFailures; attempt++ {
		r, pErr := ts.Client().Post(lockManifestURL, "application/json", strings.NewReader(`{"recipient":"Attacker","passcode":"111111"}`))
		require.NoError(t, pErr)
		_ = r.Body.Close()
		if attempt < shlink.DefaultMaxPasscodeFailures {
			assert.Equal(t, http.StatusUnauthorized, r.StatusCode)
		} else {
			assert.Equal(t, http.StatusTooManyRequests, r.StatusCode)
		}
	}
	// Even the valid passcode is rejected with 429 once locked out
	afterLockResp, err := ts.Client().Post(lockManifestURL, "application/json", strings.NewReader(`{"recipient":"Legit Viewer","passcode":"999999"}`))
	require.NoError(t, err)
	_ = afterLockResp.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, afterLockResp.StatusCode)

	// 6. Verify explicit revocation via Patient/{id}/$shlink-revoke & clock expiry (410 Gone)
	revLinkReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-alice-001/$shlink", strings.NewReader(`{"label":"Revocable Link"}`))
	revLinkReq.Header.Set("Authorization", "Bearer "+aliceTok)
	revLinkReq.Header.Set("Content-Type", "application/json")
	revLinkResp, err := ts.Client().Do(revLinkReq)
	require.NoError(t, err)
	var revIssueDoc struct {
		Parameter []struct {
			Name        string `json:"name"`
			ValueString string `json:"valueString"`
			ValueURL    string `json:"valueUrl"`
		} `json:"parameter"`
	}
	require.NoError(t, json.NewDecoder(revLinkResp.Body).Decode(&revIssueDoc))
	_ = revLinkResp.Body.Close()
	var revID, revManifest string
	for _, p := range revIssueDoc.Parameter {
		switch p.Name {
		case "linkId":
			revID = p.ValueString
		case "manifestUrl":
			revManifest = p.ValueURL
		}
	}

	// Cross-patient revoke attempt -> 403 Forbidden
	bobTok, err := srv.IssueDevToken("bob-portal", "patient/Patient.rs", tenant, "pat-bob-002", time.Hour)
	require.NoError(t, err)
	crossRevReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-bob-002/$shlink-revoke?linkId="+url.QueryEscape(revID), nil)
	crossRevReq.Header.Set("Authorization", "Bearer "+bobTok)
	crossRevResp, err := ts.Client().Do(crossRevReq)
	require.NoError(t, err)
	_ = crossRevResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, crossRevResp.StatusCode)

	// Authorized revoke -> 200 OK, followed by 410 Gone on /manifest and /content/0
	okRevReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-alice-001/$shlink-revoke", strings.NewReader(`{"resourceType":"Parameters","parameter":[{"name":"linkId","valueString":"`+revID+`"}]}`))
	okRevReq.Header.Set("Authorization", "Bearer "+aliceTok)
	okRevReq.Header.Set("Content-Type", "application/fhir+json")
	okRevResp, err := ts.Client().Do(okRevReq)
	require.NoError(t, err)
	_ = okRevResp.Body.Close()
	assert.Equal(t, http.StatusOK, okRevResp.StatusCode)

	goneManResp, err := ts.Client().Post(revManifest, "application/json", strings.NewReader(`{"recipient":"Clinic"}`))
	require.NoError(t, err)
	_ = goneManResp.Body.Close()
	assert.Equal(t, http.StatusGone, goneManResp.StatusCode)

	goneContentResp, err := ts.Client().Get(strings.TrimSuffix(revManifest, "/manifest") + "/content/0")
	require.NoError(t, err)
	_ = goneContentResp.Body.Close()
	assert.Equal(t, http.StatusGone, goneContentResp.StatusCode)

	// Clock advance past expiration -> 410 Gone
	expLinkReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-alice-001/$shlink", strings.NewReader(`{"expiresInSeconds":60}`))
	expLinkReq.Header.Set("Authorization", "Bearer "+aliceTok)
	expLinkReq.Header.Set("Content-Type", "application/json")
	expLinkResp, err := ts.Client().Do(expLinkReq)
	require.NoError(t, err)
	var expDoc struct {
		Parameter []struct {
			Name     string `json:"name"`
			ValueURL string `json:"valueUrl"`
		} `json:"parameter"`
	}
	require.NoError(t, json.NewDecoder(expLinkResp.Body).Decode(&expDoc))
	_ = expLinkResp.Body.Close()
	var expManifestURL string
	for _, p := range expDoc.Parameter {
		if p.Name == "manifestUrl" {
			expManifestURL = p.ValueURL
		}
	}
	now = now.Add(2 * time.Minute)
	expManResp, err := ts.Client().Post(expManifestURL, "application/json", strings.NewReader(`{"recipient":"Clinic"}`))
	require.NoError(t, err)
	_ = expManResp.Body.Close()
	assert.Equal(t, http.StatusGone, expManResp.StatusCode)
}

func TestProperty_MCPSMARTCompartmentAndSHLinkEndToEnd(t *testing.T) {
	srv, ts := newMCPAndSHLinkTestServer(t)
	tenant := "pbt"
	_ = seedMCPAndSHLinkFixture(t, srv, ts, tenant)

	patients := []string{"pat-alice-001", "pat-bob-002"}
	tokens := map[string]string{}
	for _, pid := range patients {
		tok, err := srv.IssueDevToken(
			"pbt-"+pid,
			"patient/Patient.rs patient/Observation.rs?category=http://terminology.hl7.org/CodeSystem/observation-category|vital-signs patient/Condition.rs patient/Immunization.rs",
			tenant,
			pid,
			time.Hour,
		)
		require.NoError(t, err)
		tokens[pid] = tok
	}

	hegel.Test(t, func(ht *hegel.T) {
		actorIdx := hegel.Draw(ht, hegel.Integers(0, 1))
		targetIdx := hegel.Draw(ht, hegel.Integers(0, 1))
		toolChoice := hegel.Draw(ht, hegel.Integers(0, 3))
		usePin := hegel.Draw(ht, hegel.Booleans())

		actorPat := patients[actorIdx]
		targetPat := patients[targetIdx]
		tok := tokens[actorPat]

		var toolName string
		var args map[string]any
		switch toolChoice {
		case 0:
			toolName = "fhir_read"
			args = map[string]any{"resourceType": "Patient", "id": targetPat}
		case 1:
			toolName = "patient_summary_ips"
			args = map[string]any{"patientId": targetPat}
		case 2:
			toolName = "patient_everything"
			args = map[string]any{"patientId": targetPat}
		default:
			toolName = "fhir_search"
			args = map[string]any{"resourceType": "Observation", "query": map[string]any{"patient": "Patient/" + targetPat}}
		}

		_, mcpBytes := callMCPHTTP(t, ts, tenant, tok, "", map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params":  map[string]any{"name": toolName, "arguments": args},
		})
		var reply rpcReply
		if err := json.Unmarshal(mcpBytes, &reply); err != nil {
			ht.Fatalf("unmarshal MCP reply: %v", err)
		}

		if actorPat != targetPat {
			if reply.Error == nil || reply.Error.Code != -32003 {
				ht.Fatalf("cross-patient MCP call (%s -> %s via %s) must fail with -32003, got %s", actorPat, targetPat, toolName, string(mcpBytes))
			}
			// Cross-patient $shlink issuance must also fail with 403 Forbidden
			shReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/fhir/r4/%s/Patient/%s/$shlink", ts.URL, tenant, targetPat), nil)
			shReq.Header.Set("Authorization", "Bearer "+tok)
			shResp, err := ts.Client().Do(shReq)
			if err != nil {
				ht.Fatalf("POST cross-patient $shlink: %v", err)
			}
			_ = shResp.Body.Close()
			if shResp.StatusCode != http.StatusForbidden {
				ht.Fatalf("cross-patient $shlink expected 403, got %d", shResp.StatusCode)
			}
			return
		}

		// Authorized own-compartment MCP call must succeed without leaking the other patient's data
		if reply.Error != nil {
			ht.Fatalf("own-patient MCP call (%s via %s) failed: %+v", actorPat, toolName, reply.Error)
		}
		otherPat := patients[1-actorIdx]
		if strings.Contains(string(reply.Result), otherPat) || strings.Contains(string(reply.Result), "obs-alice-soc") {
			ht.Fatalf("compartment or granular scope leak detected in %s: %s", toolName, string(reply.Result))
		}

		// Authorized own-compartment $shlink issuance + A256GCM JWE round-trip
		pin := ""
		if usePin {
			pin = "246810"
		}
		reqBody, _ := json.Marshal(map[string]any{
			"includeSHC": false,
			"includeIPS": true,
			"passcode":   pin,
			"maxViews":   1,
		})
		shReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/fhir/r4/%s/Patient/%s/$shlink", ts.URL, tenant, actorPat), bytes.NewReader(reqBody))
		shReq.Header.Set("Authorization", "Bearer "+tok)
		shReq.Header.Set("Content-Type", "application/json")
		shResp, err := ts.Client().Do(shReq)
		if err != nil {
			ht.Fatalf("POST own $shlink: %v", err)
		}
		shBytes, _ := io.ReadAll(shResp.Body)
		_ = shResp.Body.Close()
		if shResp.StatusCode != http.StatusOK {
			ht.Fatalf("own $shlink status=%d: %s", shResp.StatusCode, string(shBytes))
		}
		var pDoc struct {
			Parameter []struct {
				Name     string `json:"name"`
				ValueURI string `json:"valueUri"`
			} `json:"parameter"`
		}
		_ = json.Unmarshal(shBytes, &pDoc)
		var uri string
		for _, p := range pDoc.Parameter {
			if p.Name == "shlink" {
				uri = p.ValueURI
			}
		}
		payload, err := shlink.DecodeSHLinkURI(uri)
		if err != nil {
			ht.Fatalf("DecodeSHLinkURI: %v", err)
		}
		key, err := shlink.DecodeAES256Key(payload.Key)
		if err != nil {
			ht.Fatalf("DecodeAES256Key: %v", err)
		}
		manReq, _ := json.Marshal(map[string]any{"recipient": "PBT Verifier", "passcode": pin, "embeddedLengthMax": 1 << 20})
		manResp, err := ts.Client().Post(payload.URL, "application/json", bytes.NewReader(manReq))
		if err != nil {
			ht.Fatalf("POST manifest: %v", err)
		}
		var man shlink.ManifestResponse
		_ = json.NewDecoder(manResp.Body).Decode(&man)
		_ = manResp.Body.Close()
		if manResp.StatusCode != http.StatusOK || len(man.Files) != 1 {
			ht.Fatalf("unexpected manifest status=%d files=%d", manResp.StatusCode, len(man.Files))
		}
		pt, _, err := shlink.DecryptCompactJWE(key, man.Files[0].Embedded)
		if err != nil {
			ht.Fatalf("DecryptCompactJWE: %v", err)
		}
		if !strings.Contains(string(pt), actorPat) || strings.Contains(string(pt), otherPat) {
			ht.Fatalf("decrypted SHLink IPS bundle patient mismatch for %s", actorPat)
		}
	}, hegel.WithTestCases(25))
}
