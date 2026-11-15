package server_test

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	authv1 "github.com/flint-fhir/flint/gen/go/flint/auth/v1"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/sqlite"
)

type testWSClient struct {
	t     *testing.T
	conn  net.Conn
	bufrw *bufio.ReadWriter
	proto string
}

func dialTestWS(t *testing.T, tsURL, path string, headers map[string]string) (*testWSClient, int, string) {
	t.Helper()
	addr := strings.TrimPrefix(tsURL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial tcp %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var rawKey [16]byte
	_, _ = rand.Read(rawKey[:])
	wsKey := base64.StdEncoding.EncodeToString(rawKey[:])

	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", addr)
	if _, hasUpgrade := headers["Upgrade"]; !hasUpgrade {
		req.WriteString("Upgrade: websocket\r\n")
	}
	if _, hasConn := headers["Connection"]; !hasConn {
		req.WriteString("Connection: Upgrade\r\n")
	}
	if _, hasVer := headers["Sec-WebSocket-Version"]; !hasVer {
		req.WriteString("Sec-WebSocket-Version: 13\r\n")
	}
	if _, hasKey := headers["Sec-WebSocket-Key"]; !hasKey {
		fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", wsKey)
	}
	for k, v := range headers {
		if v != "" {
			fmt.Fprintf(&req, "%s: %s\r\n", k, v)
		}
	}
	req.WriteString("\r\n")

	if _, err := conn.Write([]byte(req.String())); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	bufrw := bufio.NewReaderSize(conn, 64*1024)
	resp, err := http.ReadResponse(bufrw, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, resp.StatusCode, string(body)
	}

	sum := sha1.Sum([]byte(wsKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	wantAccept := base64.StdEncoding.EncodeToString(sum[:])
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != wantAccept {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, wantAccept)
	}

	return &testWSClient{
		t:     t,
		conn:  conn,
		bufrw: bufio.NewReadWriter(bufrw, bufio.NewWriter(conn)),
		proto: resp.Header.Get("Sec-WebSocket-Protocol"),
	}, resp.StatusCode, ""
}

func (c *testWSClient) writeMaskedFrame(fin bool, opcode byte, payload []byte) {
	c.t.Helper()
	var hdr []byte
	b0 := opcode & 0x0F
	if fin {
		b0 |= 0x80
	}
	hdr = append(hdr, b0)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 65535:
		hdr = append(hdr, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, 0x80|127)
		hdr = append(hdr, ext[:]...)
	}
	mask := [4]byte{0x37, 0xFA, 0x21, 0x3D}
	hdr = append(hdr, mask[:]...)

	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}

	_ = c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.bufrw.Write(hdr); err != nil {
		c.t.Fatalf("write ws header: %v", err)
	}
	if len(masked) > 0 {
		if _, err := c.bufrw.Write(masked); err != nil {
			c.t.Fatalf("write ws payload: %v", err)
		}
	}
	if err := c.bufrw.Flush(); err != nil {
		c.t.Fatalf("flush ws frame: %v", err)
	}
}

func (c *testWSClient) sendText(msg string) {
	c.writeMaskedFrame(true, 0x1, []byte(msg))
}

func (c *testWSClient) readFrame(timeout time.Duration) (byte, []byte, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	var b [2]byte
	if _, err := io.ReadFull(c.bufrw, b[:]); err != nil {
		return 0, nil, err
	}
	opcode := b[0] & 0x0F
	masked := (b[1] & 0x80) != 0
	if masked {
		return 0, nil, fmt.Errorf("RFC 6455 violation: server sent masked frame")
	}
	length := uint64(b[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.bufrw, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.bufrw, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(c.bufrw, payload); err != nil {
			return 0, nil, err
		}
	}
	return opcode, payload, nil
}

func (c *testWSClient) readJSONBundle(timeout time.Duration) map[string]any {
	c.t.Helper()
	opcode, payload, err := c.readFrame(timeout)
	if err != nil {
		c.t.Fatalf("readJSONBundle: %v", err)
	}
	if opcode != 0x1 {
		c.t.Fatalf("expected text opcode 0x1, got 0x%x (%q)", opcode, string(payload))
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		c.t.Fatalf("unmarshal bundle JSON (%q): %v", string(payload), err)
	}
	return m
}

func newWSTestServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	st := sqlite.New(nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func doJSONReq(t *testing.T, method, url, bearer string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/fhir+json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(bytes.TrimSpace(respBytes)) > 0 {
		_ = json.Unmarshal(respBytes, &out)
	}
	return resp.StatusCode, out
}

func extractStatusParamCode(bundle map[string]any, paramName string) string {
	entries, _ := bundle["entry"].([]any)
	if len(entries) == 0 {
		return ""
	}
	first, _ := entries[0].(map[string]any)
	res, _ := first["resource"].(map[string]any)
	params, _ := res["parameter"].([]any)
	for _, p := range params {
		pm, _ := p.(map[string]any)
		if pm["name"] == paramName {
			if s, ok := pm["valueCode"].(string); ok {
				return s
			}
			if s, ok := pm["valueString"].(string); ok {
				return s
			}
		}
	}
	return ""
}

func extractBindTokenFromParams(t *testing.T, paramsMap map[string]any) string {
	t.Helper()
	params, _ := paramsMap["parameter"].([]any)
	for _, p := range params {
		pm, _ := p.(map[string]any)
		if pm["name"] == "token" {
			if tok, _ := pm["valueString"].(string); tok != "" {
				return tok
			}
		}
	}
	t.Fatalf("missing token parameter in $bind-ws response: %#v", paramsMap)
	return ""
}

func TestWebSocketSubscriptions_R5BackportHandshakeHeartbeatAndCRUDModes(t *testing.T) {
	srv, ts := newWSTestServer(t)
	srv.SetSubscriptionHeartbeatInterval(45 * time.Millisecond)
	if got := srv.SubscriptionHeartbeatInterval(); got != 45*time.Millisecond {
		t.Fatalf("SubscriptionHeartbeatInterval = %v, want 45ms", got)
	}

	// 1. Create three WebSocket Subscriptions covering full-resource, id-only, and empty modes.
	status, subFull := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Subscription", "", map[string]any{
		"resourceType": "Subscription",
		"status":       "requested",
		"reason":       "HR critical monitor (full-resource)",
		"criteria":     "Observation?code=http://loinc.org|8867-4",
		"channel": map[string]any{
			"type":    "websocket",
			"payload": "application/fhir+json",
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create subFull status = %d, body = %#v", status, subFull)
	}
	subFullID, _ := subFull["id"].(string)

	status, subIDOnly := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Subscription", "", map[string]any{
		"resourceType": "Subscription",
		"status":       "requested",
		"reason":       "HR critical monitor (id-only)",
		"criteria":     "Observation?code=http://loinc.org|8867-4",
		"channel": map[string]any{
			"type":    "websocket",
			"payload": "application/fhir+json",
			"_payload": map[string]any{
				"extension": []map[string]any{
					{
						"url":       "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content",
						"valueCode": "id-only",
					},
				},
			},
			"extension": []map[string]any{
				{
					"url":              "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-heartbeat-period",
					"valueUnsignedInt": 60,
				},
			},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create subIDOnly status = %d", status)
	}
	subIDOnlyID, _ := subIDOnly["id"].(string)

	status, subEmpty := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Subscription", "", map[string]any{
		"resourceType": "Subscription",
		"status":       "requested",
		"reason":       "HR critical monitor (empty)",
		"criteria":     "Observation?code=http://loinc.org|8867-4",
		"channel": map[string]any{
			"type": "websocket",
			"extension": []map[string]any{
				{
					"url":       "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content",
					"valueCode": "empty",
				},
				{
					"url":              "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-heartbeat-period",
					"valueUnsignedInt": 60,
				},
			},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create subEmpty status = %d", status)
	}
	subEmptyID, _ := subEmpty["id"].(string)

	// 2. Mint binding tokens via POST /Subscription/$bind-ws, GET /Subscription/{id}/$bind-ws, and POST $get-ws-binding-token.
	bStatus, bindFullParams := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Subscription/$bind-ws", "", map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "ids", "valueString": subFullID},
		},
	})
	if bStatus != http.StatusOK {
		t.Fatalf("POST $bind-ws status = %d, body = %#v", bStatus, bindFullParams)
	}
	tokFull := extractBindTokenFromParams(t, bindFullParams)

	bStatus, bindIDOnlyParams := doJSONReq(t, http.MethodGet, ts.URL+"/fhir/r4/t1/Subscription/"+subIDOnlyID+"/$bind-ws", "", nil)
	if bStatus != http.StatusOK {
		t.Fatalf("GET instance $bind-ws status = %d", bStatus)
	}
	tokIDOnly := extractBindTokenFromParams(t, bindIDOnlyParams)

	bStatus, bindEmptyParams := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Subscription/"+subEmptyID+"/$get-ws-binding-token", "", nil)
	if bStatus != http.StatusOK {
		t.Fatalf("POST $get-ws-binding-token status = %d", bStatus)
	}
	tokEmpty := extractBindTokenFromParams(t, bindEmptyParams)

	// 3. Upgrade three WebSocket clients with subprotocol negotiation.
	wsFull, code, _ := dialTestWS(t, ts.URL, "/fhir/r4/t1/$subscriptions-ws", map[string]string{
		"Sec-WebSocket-Protocol": "fhir-subscriptions-ws",
	})
	if code != http.StatusSwitchingProtocols || wsFull.proto != "fhir-subscriptions-ws" {
		t.Fatalf("wsFull upgrade status=%d proto=%q", code, wsFull.proto)
	}
	wsIDOnly, _, _ := dialTestWS(t, ts.URL, "/fhir/r4/t1/$subscriptions-ws", nil)
	wsEmpty, _, _ := dialTestWS(t, ts.URL, "/fhir/r4/t1/$subscriptions-ws", nil)

	wsFull.sendText("bind-with-token: " + tokFull)
	hsFull := wsFull.readJSONBundle(2 * time.Second)
	if got := extractStatusParamCode(hsFull, "type"); got != "handshake" {
		t.Fatalf("wsFull handshake type = %q, want handshake", got)
	}

	wsIDOnly.sendText("bind-with-token: " + tokIDOnly)
	if got := extractStatusParamCode(wsIDOnly.readJSONBundle(2*time.Second), "type"); got != "handshake" {
		t.Fatalf("wsIDOnly handshake type = %q, want handshake", got)
	}

	wsEmpty.sendText("bind-with-token: " + tokEmpty)
	if got := extractStatusParamCode(wsEmpty.readJSONBundle(2*time.Second), "type"); got != "handshake" {
		t.Fatalf("wsEmpty handshake type = %q, want handshake", got)
	}

	if got := srv.ActiveWebSocketSubscribers("t1", subFullID); got != 1 {
		t.Fatalf("ActiveWebSocketSubscribers = %d, want 1", got)
	}

	// 4. Verify periodic heartbeat arrives on wsFull (configured at 45ms).
	hbBundle := wsFull.readJSONBundle(1 * time.Second)
	if got := extractStatusParamCode(hbBundle, "type"); got != "heartbeat" {
		t.Fatalf("expected periodic heartbeat bundle, got type=%q", got)
	}
	// Pause rapid heartbeats on wsFull so subsequent event assertions are deterministic.
	srv.SetSubscriptionHeartbeatInterval(30 * time.Second)

	readNextEventNotification := func(ws *testWSClient) map[string]any {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			b := ws.readJSONBundle(2 * time.Second)
			if extractStatusParamCode(b, "type") == "event-notification" {
				return b
			}
		}
		t.Fatalf("timed out waiting for event-notification bundle")
		return nil
	}

	// 5. Non-matching Observation (different LOINC code) must NOT emit event-notification.
	cStatus, _ := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Observation", "", map[string]any{
		"resourceType": "Observation",
		"status":       "final",
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://loinc.org", "code": "29463-7"}},
		},
	})
	if cStatus != http.StatusCreated {
		t.Fatalf("create non-matching Observation status = %d", cStatus)
	}

	// 6. Matching CREATE, UPDATE, and DELETE Observation!
	cStatus, createdObs := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Observation", "", map[string]any{
		"resourceType": "Observation",
		"status":       "preliminary",
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://loinc.org", "code": "8867-4"}},
		},
		"valueQuantity": map[string]any{
			"value":  118,
			"unit":   "/min",
			"system": "http://unitsofmeasure.org",
			"code":   "/min",
		},
	})
	if cStatus != http.StatusCreated {
		t.Fatalf("create matching Observation status = %d", cStatus)
	}
	obsID, _ := createdObs["id"].(string)

	evFullCreate := readNextEventNotification(wsFull)
	fullEntries, _ := evFullCreate["entry"].([]any)
	if len(fullEntries) != 2 {
		t.Fatalf("full-resource create entries = %d, want 2", len(fullEntries))
	}
	fullResEntry, _ := fullEntries[1].(map[string]any)
	fullRes, _ := fullResEntry["resource"].(map[string]any)
	if fullRes["id"] != obsID || fullRes["status"] != "preliminary" {
		t.Fatalf("unexpected full-resource payload: %#v", fullRes)
	}

	evIDOnlyCreate := readNextEventNotification(wsIDOnly)
	idEntries, _ := evIDOnlyCreate["entry"].([]any)
	if len(idEntries) != 2 {
		t.Fatalf("id-only create entries = %d, want 2", len(idEntries))
	}
	idResEntry, _ := idEntries[1].(map[string]any)
	idRes, _ := idResEntry["resource"].(map[string]any)
	if idRes["id"] != obsID || idRes["status"] != nil {
		t.Fatalf("id-only payload leaked clinical fields: %#v", idRes)
	}

	evEmptyCreate := readNextEventNotification(wsEmpty)
	emptyEntries, _ := evEmptyCreate["entry"].([]any)
	if len(emptyEntries) != 1 {
		t.Fatalf("empty mode entries = %d, want 1 (SubscriptionStatus only)", len(emptyEntries))
	}

	// Matching UPDATE:
	uStatus, _ := doJSONReq(t, http.MethodPut, ts.URL+"/fhir/r4/t1/Observation/"+obsID, "", map[string]any{
		"resourceType": "Observation",
		"id":           obsID,
		"status":       "final",
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://loinc.org", "code": "8867-4"}},
		},
	})
	if uStatus != http.StatusOK {
		t.Fatalf("update Observation status = %d", uStatus)
	}
	evFullUpdate := readNextEventNotification(wsFull)
	updEntries, _ := evFullUpdate["entry"].([]any)
	updRes, _ := updEntries[1].(map[string]any)["resource"].(map[string]any)
	if updRes["status"] != "final" {
		t.Fatalf("expected updated status=final, got %#v", updRes)
	}
	_ = readNextEventNotification(wsIDOnly)
	_ = readNextEventNotification(wsEmpty)

	// Matching DELETE:
	dStatus, _ := doJSONReq(t, http.MethodDelete, ts.URL+"/fhir/r4/t1/Observation/"+obsID, "", nil)
	if dStatus != http.StatusNoContent {
		t.Fatalf("delete Observation status = %d", dStatus)
	}
	evFullDelete := readNextEventNotification(wsFull)
	delEntries, _ := evFullDelete["entry"].([]any)
	if len(delEntries) != 2 {
		t.Fatalf("delete event entries = %d, want 2", len(delEntries))
	}
	delReq, _ := delEntries[1].(map[string]any)["request"].(map[string]any)
	delResp, _ := delEntries[1].(map[string]any)["response"].(map[string]any)
	if delReq["method"] != "DELETE" || delResp["status"] != "204 No Content" {
		t.Fatalf("unexpected delete notification entry: req=%#v resp=%#v", delReq, delResp)
	}

	// 7. Deactivating or deleting Subscription immediately unbinds active WebSockets.
	delSubStatus, _ := doJSONReq(t, http.MethodDelete, ts.URL+"/fhir/r4/t1/Subscription/"+subFullID, "", nil)
	if delSubStatus != http.StatusNoContent {
		t.Fatalf("delete Subscription status = %d", delSubStatus)
	}
	if got := srv.ActiveWebSocketSubscribers("t1", subFullID); got != 0 {
		t.Fatalf("ActiveWebSocketSubscribers after Subscription delete = %d, want 0", got)
	}
}

func TestWebSocketSubscriptions_LegacyR4FlowAndRFC6455Framing(t *testing.T) {
	_, ts := newWSTestServer(t)

	// Create legacy R4 websocket Subscription (no explicit payload -> legacy "bound <id>" / "ping <id>")
	// in default tenant to also verify system-level GET /fhir/r4/$subscriptions-ws!
	status, sub := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/default/Subscription", "", map[string]any{
		"resourceType": "Subscription",
		"status":       "requested",
		"reason":       "Legacy R4 WebSocket ping",
		"criteria":     "Patient?active=true",
		"channel": map[string]any{
			"type": "websocket",
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create legacy Subscription status = %d", status)
	}
	subID, _ := sub["id"].(string)

	// Verify invalid WebSocket upgrade requests return proper HTTP 400 / 426 errors.
	_, badVerCode, _ := dialTestWS(t, ts.URL, "/fhir/r4/$subscriptions-ws", map[string]string{
		"Sec-WebSocket-Version": "8",
	})
	if badVerCode != http.StatusUpgradeRequired {
		t.Fatalf("Sec-WebSocket-Version=8 status = %d, want 426", badVerCode)
	}
	noUpgradeCode, _ := doJSONReq(t, http.MethodGet, ts.URL+"/fhir/r4/$subscriptions-ws", "", nil)
	if noUpgradeCode != http.StatusBadRequest {
		t.Fatalf("plain HTTP GET without Upgrade header status = %d, want 400", noUpgradeCode)
	}

	// Connect to system-level /fhir/r4/$subscriptions-ws and exercise fragmented frames, Ping/Pong, bind/ping/unbind, Close.
	ws, code, _ := dialTestWS(t, ts.URL, "/fhir/r4/$subscriptions-ws", nil)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade /fhir/r4/$subscriptions-ws status = %d", code)
	}

	// Send RFC 6455 control Ping (0x9) -> expect Pong (0xA) with identical payload!
	ws.writeMaskedFrame(true, 0x9, []byte("flint-ping"))
	// Also send text "ping" -> expect text "pong".
	ws.sendText("ping")

	gotControlPong := false
	gotTextPong := false
	for range 2 {
		op, pl, err := ws.readFrame(2 * time.Second)
		if err != nil {
			t.Fatalf("read pong frame: %v", err)
		}
		if op == 0xA && string(pl) == "flint-ping" {
			gotControlPong = true
		} else if op == 0x1 && string(pl) == "pong" {
			gotTextPong = true
		}
	}
	if !gotControlPong || !gotTextPong {
		t.Fatalf("missing pong responses: control=%v text=%v", gotControlPong, gotTextPong)
	}

	// Send fragmented client text message ("bi" + "nd " + subID) across 3 frames!
	ws.writeMaskedFrame(false, 0x1, []byte("bi"))
	ws.writeMaskedFrame(false, 0x0, []byte("nd "))
	ws.writeMaskedFrame(true, 0x0, []byte(subID))

	op, pl, err := ws.readFrame(2 * time.Second)
	if err != nil || op != 0x1 || string(pl) != "bound "+subID {
		t.Fatalf("expected 'bound %s', got op=0x%x payload=%q err=%v", subID, op, string(pl), err)
	}

	// Create matching Patient -> expect legacy "ping <subID>" text frame!
	pStatus, _ := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/default/Patient", "", map[string]any{
		"resourceType": "Patient",
		"active":       true,
		"name":         []map[string]any{{"family": "Rivera"}},
	})
	if pStatus != http.StatusCreated {
		t.Fatalf("create Patient status = %d", pStatus)
	}
	op, pl, err = ws.readFrame(2 * time.Second)
	if err != nil || op != 0x1 || string(pl) != "ping "+subID {
		t.Fatalf("expected 'ping %s', got op=0x%x payload=%q err=%v", subID, op, string(pl), err)
	}

	// Test extended >125-byte masked frame (unknown command -> OperationOutcome) and unbind + Close handshake.
	longCmd := "unknown-" + strings.Repeat("x", 180)
	ws.sendText(longCmd)
	ooMap := ws.readJSONBundle(2 * time.Second)
	if ooMap["resourceType"] != "OperationOutcome" {
		t.Fatalf("expected OperationOutcome for unknown command, got %#v", ooMap)
	}

	ws.sendText("unbind " + subID)
	op, pl, err = ws.readFrame(2 * time.Second)
	if err != nil || op != 0x1 || string(pl) != "unbound "+subID {
		t.Fatalf("expected 'unbound %s', got op=0x%x payload=%q err=%v", subID, op, string(pl), err)
	}

	ws.writeMaskedFrame(true, 0x8, []byte{0x03, 0xE8}) // 1000 Normal Closure
	op, _, err = ws.readFrame(2 * time.Second)
	if err != nil || op != 0x8 {
		t.Fatalf("expected close frame 0x8, got op=0x%x err=%v", op, err)
	}
}

func TestWebSocketSubscriptions_TenantIsolationAndSMARTPatientCompartment(t *testing.T) {
	srv, ts := newWSTestServer(t)

	mockAuth, err := auth.NewMockTokenValidator("https://auth.example.org", "https://fhir.example.org")
	if err != nil {
		t.Fatalf("NewMockTokenValidator: %v", err)
	}
	mockAuth.RegisterStatic("sys-admin-t1", &authv1.SecurityContext{
		Subject:  "system-client",
		TenantId: "t1",
		Scopes:   auth.ParseScopes("system/*.cruds"),
	})
	mockAuth.RegisterStatic("pat1-token", &authv1.SecurityContext{
		Subject:   "patient-user-1",
		TenantId:  "t1",
		PatientId: "pat-1",
		Scopes:    auth.ParseScopes("patient/Observation.rs patient/Subscription.rs"),
	})
	mockAuth.RegisterStatic("no-sub-scope", &authv1.SecurityContext{
		Subject:  "limited-user",
		TenantId: "t1",
		Scopes:   auth.ParseScopes("user/Patient.rs"),
	})
	srv.SetTokenValidator(mockAuth)

	// Create Patients and active WebSocket Subscription as system client in tenant t1.
	for _, pid := range []string{"pat-1", "pat-2"} {
		st, _ := doJSONReq(t, http.MethodPut, ts.URL+"/fhir/r4/t1/Patient/"+pid, "sys-admin-t1", map[string]any{
			"resourceType": "Patient",
			"id":           pid,
			"active":       true,
		})
		if st != http.StatusCreated && st != http.StatusOK {
			t.Fatalf("upsert Patient/%s status = %d", pid, st)
		}
	}
	st, sub := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Subscription", "sys-admin-t1", map[string]any{
		"resourceType": "Subscription",
		"status":       "requested",
		"reason":       "Observation feed",
		"criteria":     "Observation?status=final",
		"channel": map[string]any{
			"type":    "websocket",
			"payload": "application/fhir+json",
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create Subscription status = %d", st)
	}
	subID, _ := sub["id"].(string)

	// Token lacking Subscription scope gets 403 on $bind-ws and $subscriptions-ws!
	forbidStatus, _ := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Subscription/"+subID+"/$bind-ws", "no-sub-scope", nil)
	if forbidStatus != http.StatusForbidden {
		t.Fatalf("expected 403 on $bind-ws without Subscription scope, got %d", forbidStatus)
	}
	_, wsForbidStatus, _ := dialTestWS(t, ts.URL, "/fhir/r4/t1/$subscriptions-ws", map[string]string{
		"Authorization": "Bearer no-sub-scope",
	})
	if wsForbidStatus != http.StatusForbidden {
		t.Fatalf("expected 403 on $subscriptions-ws without Subscription scope, got %d", wsForbidStatus)
	}

	// Unauthenticated browser WebSocket upgrade succeeds (101), but raw "bind <id>" is rejected with 1008!
	unauthWS, uCode, _ := dialTestWS(t, ts.URL, "/fhir/r4/t1/$subscriptions-ws", nil)
	if uCode != http.StatusSwitchingProtocols {
		t.Fatalf("browser unauthenticated upgrade status = %d, want 101", uCode)
	}
	unauthWS.sendText("bind " + subID)
	errOO := unauthWS.readJSONBundle(2 * time.Second)
	if errOO["resourceType"] != "OperationOutcome" {
		t.Fatalf("expected OperationOutcome on unauthenticated raw bind, got %#v", errOO)
	}
	closeOp, _, err := unauthWS.readFrame(2 * time.Second)
	if err != nil || closeOp != 0x8 {
		t.Fatalf("expected 1008 close frame after unauthenticated raw bind, got op=0x%x err=%v", closeOp, err)
	}

	// Cross-tenant token binding attempt must fail!
	bStatus, sysBindParams := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Subscription/"+subID+"/$bind-ws", "sys-admin-t1", nil)
	if bStatus != http.StatusOK {
		t.Fatalf("sys $bind-ws status = %d", bStatus)
	}
	crossTok := extractBindTokenFromParams(t, sysBindParams)
	wrongTenantWS, _, _ := dialTestWS(t, ts.URL, "/fhir/r4/t2/$subscriptions-ws", nil)
	wrongTenantWS.sendText("bind-with-token: " + crossTok)
	crossOO := wrongTenantWS.readJSONBundle(2 * time.Second)
	if crossOO["resourceType"] != "OperationOutcome" {
		t.Fatalf("expected cross-tenant OperationOutcome rejection, got %#v", crossOO)
	}

	// Patient-scoped SMART token ("pat-1") mints binding token and connects via browser WebSocket!
	pBindStatus, patBindParams := doJSONReq(t, http.MethodGet, ts.URL+"/fhir/r4/t1/Subscription/"+subID+"/$bind-ws", "pat1-token", nil)
	if pBindStatus != http.StatusOK {
		t.Fatalf("patient $bind-ws status = %d, body = %#v", pBindStatus, patBindParams)
	}
	patBindTok := extractBindTokenFromParams(t, patBindParams)

	patWS, _, _ := dialTestWS(t, ts.URL, "/fhir/r4/t1/$subscriptions-ws", nil)
	patWS.sendText("bind-with-token: " + patBindTok)
	hs := patWS.readJSONBundle(2 * time.Second)
	if got := extractStatusParamCode(hs, "type"); got != "handshake" {
		t.Fatalf("patient handshake type = %q, want handshake", got)
	}

	// Replaying single-use patBindTok on a second socket must fail!
	replayWS, _, _ := dialTestWS(t, ts.URL, "/fhir/r4/t1/$subscriptions-ws", nil)
	replayWS.sendText("bind-with-token: " + patBindTok)
	replayOO := replayWS.readJSONBundle(2 * time.Second)
	if replayOO["resourceType"] != "OperationOutcome" {
		t.Fatalf("expected single-use token replay rejection, got %#v", replayOO)
	}

	// Emit Observation for pat-2 FIRST (must be blocked by compartment filter), then Observation for pat-1!
	st2, _ := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Observation", "sys-admin-t1", map[string]any{
		"resourceType": "Observation",
		"status":       "final",
		"subject":      map[string]any{"reference": "Patient/pat-2"},
		"code":         map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8867-4"}}},
	})
	if st2 != http.StatusCreated {
		t.Fatalf("create pat-2 Observation status = %d", st2)
	}

	st1, pat1Obs := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/t1/Observation", "sys-admin-t1", map[string]any{
		"resourceType": "Observation",
		"status":       "final",
		"subject":      map[string]any{"reference": "Patient/pat-1"},
		"code":         map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8867-4"}}},
	})
	if st1 != http.StatusCreated {
		t.Fatalf("create pat-1 Observation status = %d", st1)
	}
	pat1ObsID, _ := pat1Obs["id"].(string)

	ev := patWS.readJSONBundle(2 * time.Second)
	if got := extractStatusParamCode(ev, "type"); got != "event-notification" {
		t.Fatalf("expected event-notification, got %q", got)
	}
	entries, _ := ev["entry"].([]any)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	gotRes, _ := entries[1].(map[string]any)["resource"].(map[string]any)
	if gotRes["id"] != pat1ObsID {
		t.Fatalf("compartment leak! expected Observation/%s for pat-1, got %#v", pat1ObsID, gotRes)
	}
}

func TestCloudEventsSubscriptions_RestHookSingleAndBatchAndMetadata(t *testing.T) {
	_, ts := newWSTestServer(t)

	var mu sync.Mutex
	type capturedReq struct {
		contentType string
		body        []byte
	}
	var received []capturedReq
	notifyCh := make(chan struct{}, 8)

	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, capturedReq{
			contentType: r.Header.Get("Content-Type"),
			body:        b,
		})
		mu.Unlock()
		select {
		case notifyCh <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	// 1. Create single CloudEvents v1.0 Subscription and CloudEvents batch Subscription.
	st, ceSingle := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/ce-tenant/Subscription", "", map[string]any{
		"resourceType": "Subscription",
		"status":       "requested",
		"reason":       "CloudEvents v1.0 Observation stream",
		"criteria":     "Observation?code=http://loinc.org|85354-9",
		"channel": map[string]any{
			"type":     "rest-hook",
			"endpoint": webhookSrv.URL + "/single",
			"payload":  "application/cloudevents+json",
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create CloudEvents Subscription status = %d", st)
	}
	ceSingleID, _ := ceSingle["id"].(string)

	st, _ = doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/ce-tenant/Subscription", "", map[string]any{
		"resourceType": "Subscription",
		"status":       "requested",
		"reason":       "CloudEvents batch Observation stream",
		"criteria":     "Observation?code=http://loinc.org|85354-9",
		"channel": map[string]any{
			"type":     "rest-hook",
			"endpoint": webhookSrv.URL + "/batch",
			"payload":  "application/cloudevents-batch+json",
			"_payload": map[string]any{
				"extension": []map[string]any{
					{
						"url":       "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content",
						"valueCode": "id-only",
					},
				},
			},
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create CloudEvents batch Subscription status = %d", st)
	}

	// 2. Trigger matching create AND conditional delete!
	cStatus, obs := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/ce-tenant/Observation", "", map[string]any{
		"resourceType": "Observation",
		"status":       "final",
		"code": map[string]any{
			"coding": []map[string]any{{"system": "http://loinc.org", "code": "85354-9"}},
		},
	})
	if cStatus != http.StatusCreated {
		t.Fatalf("create Observation status = %d", cStatus)
	}
	obsID, _ := obs["id"].(string)

	for range 2 {
		select {
		case <-notifyCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for CloudEvents create webhooks")
		}
	}

	// Conditional delete triggers delete CloudEvents!
	delStatus, _ := doJSONReq(t, http.MethodDelete, ts.URL+"/fhir/r4/ce-tenant/Observation?code=http://loinc.org|85354-9", "", nil)
	if delStatus != http.StatusNoContent {
		t.Fatalf("conditional delete status = %d", delStatus)
	}
	for range 2 {
		select {
		case <-notifyCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for CloudEvents delete webhooks")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) < 4 {
		t.Fatalf("expected 4 CloudEvents deliveries (2 create + 2 delete), got %d", len(received))
	}

	var sawSingleCreate, sawSingleDelete, sawBatch bool
	for _, item := range received {
		if strings.HasPrefix(item.contentType, "application/cloudevents+json") {
			var evt map[string]any
			if err := json.Unmarshal(item.body, &evt); err != nil {
				t.Fatalf("unmarshal CloudEvent: %v", err)
			}
			if evt["specversion"] != "1.0" ||
				evt["type"] != "org.hl7.fhir.r4.subscription-notification" ||
				evt["source"] != "/fhir/r4/ce-tenant/Subscription/"+ceSingleID ||
				evt["subject"] != "Observation/"+obsID ||
				evt["datacontenttype"] != "application/fhir+json" {
				t.Fatalf("unexpected CloudEvent envelope: %#v", evt)
			}
			switch evt["fhiraction"] {
			case "create":
				sawSingleCreate = true
				data, _ := evt["data"].(map[string]any)
				if data["resourceType"] != "Observation" || data["id"] != obsID {
					t.Fatalf("unexpected CloudEvent create data: %#v", data)
				}
			case "delete":
				sawSingleDelete = true
			}
		} else if strings.HasPrefix(item.contentType, "application/cloudevents-batch+json") {
			var batch []map[string]any
			if err := json.Unmarshal(item.body, &batch); err != nil || len(batch) != 1 {
				t.Fatalf("unmarshal CloudEvents batch: err=%v len=%d", err, len(batch))
			}
			if batch[0]["specversion"] != "1.0" || batch[0]["type"] != "org.hl7.fhir.r4.subscription-notification" {
				t.Fatalf("unexpected CloudEvents batch item: %#v", batch[0])
			}
			sawBatch = true
		}
	}
	if !sawSingleCreate || !sawSingleDelete || !sawBatch {
		t.Fatalf("missing expected CloudEvent variants: singleCreate=%v singleDelete=%v batch=%v",
			sawSingleCreate, sawSingleDelete, sawBatch)
	}

	// 3. Verify CapabilityStatement (/metadata), OpenAPI (/openapi.json), and AuditEvent logging.
	mStatus, capStmt := doJSONReq(t, http.MethodGet, ts.URL+"/fhir/r4/ce-tenant/metadata", "", nil)
	if mStatus != http.StatusOK {
		t.Fatalf("GET /metadata status = %d", mStatus)
	}
	capBytes, _ := json.Marshal(capStmt)
	for _, wantOp := range []string{"bind-ws", "get-ws-binding-token", "subscriptions-ws"} {
		if !bytes.Contains(capBytes, []byte(`"name":"`+wantOp+`"`)) {
			t.Fatalf("CapabilityStatement missing operation %q", wantOp)
		}
	}

	oStatus, openAPI := doJSONReq(t, http.MethodGet, ts.URL+"/fhir/r4/ce-tenant/openapi.json", "", nil)
	if oStatus != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d", oStatus)
	}
	paths, _ := openAPI["paths"].(map[string]any)
	if paths["/$subscriptions-ws"] == nil || paths["/Subscription/$bind-ws"] == nil {
		t.Fatalf("openapi.json missing WebSocket paths: keys present = %d", len(paths))
	}
}
