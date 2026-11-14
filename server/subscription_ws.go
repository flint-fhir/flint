package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/store"
	"github.com/google/uuid"
)

const (
	wsHandshakeGUID            = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	defaultWSHeartbeatInterval = 30 * time.Second
	defaultWSBindTokenTTL      = 5 * time.Minute
	defaultWSInitialBindWait   = 10 * time.Second
	maxWSMessageBytes          = 64 * 1024

	backportHeartbeatExtURL      = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-heartbeat-period"
	backportPayloadContentExtURL = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content"

	wsOpContinuation byte = 0x0
	wsOpText         byte = 0x1
	wsOpBinary       byte = 0x2
	wsOpClose        byte = 0x8
	wsOpPing         byte = 0x9
	wsOpPong         byte = 0xA
)

type wsBindToken struct {
	token           string
	tenant          string
	subscriptionIDs []string
	secCtx          *auth.SecurityContext
	expiresAt       time.Time
}

type wsSubscriptionBinding struct {
	subID           string
	contentMode     string
	legacyPing      bool
	heartbeatPeriod time.Duration
	lastHeartbeat   time.Time
	secCtx          *auth.SecurityContext
}

type wsSubscriberConn struct {
	id      string
	tenant  string
	conn    net.Conn
	bufrw   *bufio.ReadWriter
	writeMu sync.Mutex
	closed  atomic.Bool
	done    chan struct{}

	mu       sync.RWMutex
	bindings map[string]*wsSubscriptionBinding
}

func (c *wsSubscriberConn) close() {
	if c.closed.CompareAndSwap(false, true) {
		close(c.done)
		_ = c.conn.Close()
	}
}

func (c *wsSubscriberConn) boundCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.bindings)
}

func (c *wsSubscriberConn) setBinding(b *wsSubscriptionBinding) {
	c.mu.Lock()
	if c.bindings == nil {
		c.bindings = make(map[string]*wsSubscriptionBinding)
	}
	c.bindings[b.subID] = b
	c.mu.Unlock()
}

func (c *wsSubscriberConn) removeBinding(subID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.bindings[subID]; ok {
		delete(c.bindings, subID)
		return true
	}
	return false
}

func (c *wsSubscriberConn) getBinding(subID string) *wsSubscriptionBinding {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, ok := c.bindings[subID]
	if !ok || b == nil {
		return nil
	}
	cp := *b
	return &cp
}

func (c *wsSubscriberConn) writeFrame(opcode byte, payload []byte) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()

	var hdr [10]byte
	hdr[0] = 0x80 | (opcode & 0x0F) // FIN=1, RSV=0
	hdrLen := 2
	n := len(payload)
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hdrLen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hdrLen = 10
	}

	if _, err := c.bufrw.Write(hdr[:hdrLen]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.bufrw.Write(payload); err != nil {
			return err
		}
	}
	return c.bufrw.Flush()
}

func (c *wsSubscriberConn) writeText(msg string) error {
	return c.writeFrame(wsOpText, []byte(msg))
}

func (c *wsSubscriberConn) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(wsOpText, b)
}

func (c *wsSubscriberConn) writeClose(code uint16, reason string) {
	var payload []byte
	if code > 0 {
		reasonBytes := []byte(reason)
		if len(reasonBytes) > 120 {
			reasonBytes = reasonBytes[:120]
		}
		payload = make([]byte, 2+len(reasonBytes))
		binary.BigEndian.PutUint16(payload[:2], code)
		copy(payload[2:], reasonBytes)
	}
	_ = c.writeFrame(wsOpClose, payload)
}

func (c *wsSubscriberConn) readMessage() (byte, []byte, error) {
	var (
		msgOpcode   byte
		accumulated []byte
		inFragment  bool
	)
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(c.bufrw, hdr[:]); err != nil {
			return 0, nil, err
		}
		fin := (hdr[0] & 0x80) != 0
		rsv := hdr[0] & 0x70
		if rsv != 0 {
			c.writeClose(1002, "unsupported RSV bits")
			return 0, nil, fmt.Errorf("websocket protocol error: non-zero RSV bits")
		}
		opcode := hdr[0] & 0x0F
		masked := (hdr[1] & 0x80) != 0
		payloadLen := uint64(hdr[1] & 0x7F)

		switch payloadLen {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.bufrw, ext[:]); err != nil {
				return 0, nil, err
			}
			payloadLen = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.bufrw, ext[:]); err != nil {
				return 0, nil, err
			}
			payloadLen = binary.BigEndian.Uint64(ext[:])
		}

		if !masked {
			c.writeClose(1002, "client frames must be masked")
			return 0, nil, fmt.Errorf("websocket protocol error: unmasked client frame")
		}
		if payloadLen > maxWSMessageBytes || uint64(len(accumulated))+payloadLen > maxWSMessageBytes {
			c.writeClose(1009, "message too big")
			return 0, nil, fmt.Errorf("websocket message exceeds %d bytes", maxWSMessageBytes)
		}

		var maskKey [4]byte
		if _, err := io.ReadFull(c.bufrw, maskKey[:]); err != nil {
			return 0, nil, err
		}
		framePayload := make([]byte, payloadLen)
		if payloadLen > 0 {
			if _, err := io.ReadFull(c.bufrw, framePayload); err != nil {
				return 0, nil, err
			}
			for i := range framePayload {
				framePayload[i] ^= maskKey[i%4]
			}
		}

		// Control frames (Close, Ping, Pong) may arrive mid-fragmentation per RFC 6455 §5.5.
		switch opcode {
		case wsOpClose:
			c.writeClose(1000, "")
			return wsOpClose, nil, io.EOF
		case wsOpPing:
			_ = c.writeFrame(wsOpPong, framePayload)
			continue
		case wsOpPong:
			continue
		}

		if opcode == wsOpContinuation {
			if !inFragment {
				c.writeClose(1002, "unexpected continuation frame")
				return 0, nil, fmt.Errorf("websocket protocol error: unexpected continuation frame")
			}
			accumulated = append(accumulated, framePayload...)
			if fin {
				return msgOpcode, accumulated, nil
			}
			continue
		}

		if opcode != wsOpText && opcode != wsOpBinary {
			c.writeClose(1003, "unsupported opcode")
			return 0, nil, fmt.Errorf("websocket unsupported opcode 0x%x", opcode)
		}
		if inFragment {
			c.writeClose(1002, "expected continuation frame")
			return 0, nil, fmt.Errorf("websocket protocol error: expected continuation frame")
		}
		if fin {
			return opcode, framePayload, nil
		}
		inFragment = true
		msgOpcode = opcode
		accumulated = append(accumulated[:0], framePayload...)
	}
}

type subscriptionWSHub struct {
	mu                 sync.RWMutex
	tokens             map[string]wsBindToken
	conns              map[string]*wsSubscriberConn
	byTenantSub        map[string]map[string]*wsSubscriberConn
	defaultHeartbeat   time.Duration
	bindTokenTTL       time.Duration
	initialBindTimeout time.Duration
}

func newSubscriptionWSHub() *subscriptionWSHub {
	return &subscriptionWSHub{
		tokens:             make(map[string]wsBindToken),
		conns:              make(map[string]*wsSubscriberConn),
		byTenantSub:        make(map[string]map[string]*wsSubscriberConn),
		defaultHeartbeat:   defaultWSHeartbeatInterval,
		bindTokenTTL:       defaultWSBindTokenTTL,
		initialBindTimeout: defaultWSInitialBindWait,
	}
}

func (h *subscriptionWSHub) mintBindToken(tenant string, subIDs []string, secCtx *auth.SecurityContext, now time.Time) wsBindToken {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, v := range h.tokens {
		if !v.expiresAt.After(now) {
			delete(h.tokens, k)
		}
	}
	ttl := h.bindTokenTTL
	if ttl <= 0 {
		ttl = defaultWSBindTokenTTL
	}
	tok := wsBindToken{
		token:           "wsbt-" + uuid.NewString(),
		tenant:          tenant,
		subscriptionIDs: append([]string(nil), subIDs...),
		secCtx:          secCtx,
		expiresAt:       now.Add(ttl),
	}
	h.tokens[tok.token] = tok
	return tok
}

func (h *subscriptionWSHub) hasBindToken(token string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.tokens[token]
	return ok
}

func (h *subscriptionWSHub) consumeBindToken(tenant, token string, now time.Time) (wsBindToken, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tok, ok := h.tokens[token]
	if !ok {
		return wsBindToken{}, fmt.Errorf("invalid or unknown WebSocket binding token")
	}
	if !tok.expiresAt.After(now) {
		delete(h.tokens, token)
		return wsBindToken{}, fmt.Errorf("WebSocket binding token has expired")
	}
	if tok.tenant != tenant {
		return wsBindToken{}, fmt.Errorf("WebSocket binding token tenant %q cannot bind to tenant %q", tok.tenant, tenant)
	}
	delete(h.tokens, token)
	return tok, nil
}

func (h *subscriptionWSHub) registerConn(c *wsSubscriberConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c.id] = c
}

func (h *subscriptionWSHub) unregisterConn(c *wsSubscriberConn) {
	c.close()
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c.id)
	for key, subs := range h.byTenantSub {
		delete(subs, c.id)
		if len(subs) == 0 {
			delete(h.byTenantSub, key)
		}
	}
}

func (h *subscriptionWSHub) bindConnSubscription(c *wsSubscriberConn, b *wsSubscriptionBinding) {
	c.setBinding(b)
	key := c.tenant + "/" + b.subID
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.conns[c.id]; !exists {
		return
	}
	subs, ok := h.byTenantSub[key]
	if !ok {
		subs = make(map[string]*wsSubscriberConn)
		h.byTenantSub[key] = subs
	}
	subs[c.id] = c
}

func (h *subscriptionWSHub) unbindConnSubscription(c *wsSubscriberConn, subID string) bool {
	removed := c.removeBinding(subID)
	key := c.tenant + "/" + subID
	h.mu.Lock()
	defer h.mu.Unlock()
	if subs, ok := h.byTenantSub[key]; ok {
		delete(subs, c.id)
		if len(subs) == 0 {
			delete(h.byTenantSub, key)
		}
	}
	return removed
}

func (h *subscriptionWSHub) subscribersFor(tenant, subID string) []*wsSubscriberConn {
	key := tenant + "/" + subID
	h.mu.RLock()
	defer h.mu.RUnlock()
	subs := h.byTenantSub[key]
	if len(subs) == 0 {
		return nil
	}
	out := make([]*wsSubscriberConn, 0, len(subs))
	for _, c := range subs {
		if !c.closed.Load() {
			out = append(out, c)
		}
	}
	return out
}

func (h *subscriptionWSHub) unbindAllForSubscription(tenant, subID string) {
	key := tenant + "/" + subID
	h.mu.Lock()
	subs := h.byTenantSub[key]
	delete(h.byTenantSub, key)
	conns := make([]*wsSubscriberConn, 0, len(subs))
	for _, c := range subs {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		c.removeBinding(subID)
	}
}

// SetSubscriptionHeartbeatInterval configures the default periodic heartbeat interval
// for active RFC 6455 WebSocket Subscription bindings that do not specify an explicit
// HL7 R5 Backport backport-heartbeat-period extension.
func (s *Server) SetSubscriptionHeartbeatInterval(d time.Duration) {
	if s.subWSHub == nil {
		s.subWSHub = newSubscriptionWSHub()
	}
	s.subWSHub.mu.Lock()
	s.subWSHub.defaultHeartbeat = d
	s.subWSHub.mu.Unlock()
}

// SubscriptionHeartbeatInterval returns the configured default WebSocket Subscription heartbeat interval.
func (s *Server) SubscriptionHeartbeatInterval() time.Duration {
	if s.subWSHub == nil {
		return defaultWSHeartbeatInterval
	}
	s.subWSHub.mu.RLock()
	defer s.subWSHub.mu.RUnlock()
	return s.subWSHub.defaultHeartbeat
}

// ActiveWebSocketSubscribers returns the number of active WebSocket connections currently
// bound to Subscription/{subID} in tenant.
func (s *Server) ActiveWebSocketSubscribers(tenant, subID string) int {
	if s.subWSHub == nil {
		return 0
	}
	return len(s.subWSHub.subscribersFor(tenant, subID))
}

func computeWebSocketAcceptKey(clientKey string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(clientKey) + wsHandshakeGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func headerContainsToken(hdrVal, wantToken string) bool {
	for part := range strings.SplitSeq(hdrVal, ",") {
		if strings.EqualFold(strings.TrimSpace(part), wantToken) {
			return true
		}
	}
	return false
}

func upgradeWebSocket(w http.ResponseWriter, r *http.Request, tenant string) (*wsSubscriberConn, error) {
	if r.Method != http.MethodGet {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "WebSocket subscription endpoint requires HTTP GET")
		return nil, fmt.Errorf("invalid method %s", r.Method)
	}
	if !headerContainsToken(strings.Join(r.Header.Values("Connection"), ","), "upgrade") ||
		!strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "missing RFC 6455 Upgrade: websocket headers")
		return nil, fmt.Errorf("missing Upgrade: websocket header")
	}
	if strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")) != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		writeOperationOutcome(w, http.StatusUpgradeRequired, "not-supported", "Sec-WebSocket-Version must be 13")
		return nil, fmt.Errorf("unsupported Sec-WebSocket-Version")
	}
	wsKey := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if wsKey == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "missing Sec-WebSocket-Key header")
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}

	var (
		conn  net.Conn
		bufrw *bufio.ReadWriter
		err   error
	)
	if hj, ok := w.(http.Hijacker); ok {
		conn, bufrw, err = hj.Hijack()
	} else {
		rc := http.NewResponseController(w)
		conn, bufrw, err = rc.Hijack()
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "HTTP connection does not support WebSocket hijacking")
		return nil, err
	}

	_ = conn.SetDeadline(time.Time{})
	acceptKey := computeWebSocketAcceptKey(wsKey)

	var subProtocol string
	for _, protoHdr := range r.Header.Values("Sec-WebSocket-Protocol") {
		for cand := range strings.SplitSeq(protoHdr, ",") {
			cand = strings.TrimSpace(cand)
			if strings.EqualFold(cand, "fhir-subscriptions-ws") {
				subProtocol = cand
				break
			}
		}
	}

	var resp strings.Builder
	resp.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	resp.WriteString("Upgrade: websocket\r\n")
	resp.WriteString("Connection: Upgrade\r\n")
	resp.WriteString("Sec-WebSocket-Accept: " + acceptKey + "\r\n")
	if subProtocol != "" {
		resp.WriteString("Sec-WebSocket-Protocol: " + subProtocol + "\r\n")
	}
	resp.WriteString("\r\n")

	if _, err := bufrw.WriteString(resp.String()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := bufrw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return &wsSubscriberConn{
		id:       uuid.NewString(),
		tenant:   tenant,
		conn:     conn,
		bufrw:    bufrw,
		done:     make(chan struct{}),
		bindings: make(map[string]*wsSubscriptionBinding),
	}, nil
}

func extractSubscriptionChannelContentMode(subMap map[string]any) (contentMode string, explicit bool) {
	ch, _ := subMap["channel"].(map[string]any)
	if ch == nil {
		return "empty", false
	}
	checkExtSlice := func(raw any) string {
		arr, _ := raw.([]any)
		for _, item := range arr {
			em, _ := item.(map[string]any)
			u, _ := em["url"].(string)
			if u == backportPayloadContentExtURL || strings.HasSuffix(u, "/backport-payload-content") {
				if vc, _ := em["valueCode"].(string); vc != "" {
					return strings.ToLower(strings.TrimSpace(vc))
				}
				if vs, _ := em["valueString"].(string); vs != "" {
					return strings.ToLower(strings.TrimSpace(vs))
				}
			}
		}
		return ""
	}
	if payloadMeta, ok := ch["_payload"].(map[string]any); ok {
		if cm := checkExtSlice(payloadMeta["extension"]); cm == "empty" || cm == "id-only" || cm == "full-resource" {
			return cm, true
		}
	}
	if cm := checkExtSlice(ch["extension"]); cm == "empty" || cm == "id-only" || cm == "full-resource" {
		return cm, true
	}

	rawPayload, _ := ch["payload"].(string)
	lowerPayload := strings.ToLower(strings.TrimSpace(rawPayload))
	if semi := strings.IndexByte(lowerPayload, ';'); semi >= 0 {
		lowerPayload = strings.TrimSpace(lowerPayload[:semi])
	}
	switch lowerPayload {
	case "":
		return "empty", false
	case "empty":
		return "empty", true
	case "id-only":
		return "id-only", true
	case "full-resource":
		return "full-resource", true
	default:
		if strings.Contains(lowerPayload, "json") || strings.Contains(lowerPayload, "xml") {
			return "full-resource", true
		}
		return "empty", false
	}
}

func extractSubscriptionHeartbeatPeriod(subMap map[string]any, fallback time.Duration) time.Duration {
	ch, _ := subMap["channel"].(map[string]any)
	if ch != nil {
		if arr, ok := ch["extension"].([]any); ok {
			for _, item := range arr {
				em, _ := item.(map[string]any)
				u, _ := em["url"].(string)
				if u == backportHeartbeatExtURL || strings.HasSuffix(u, "/backport-heartbeat-period") {
					switch v := em["valueUnsignedInt"].(type) {
					case float64:
						if v > 0 {
							return time.Duration(v * float64(time.Second))
						}
					}
					switch v := em["valueInteger"].(type) {
					case float64:
						if v > 0 {
							return time.Duration(v * float64(time.Second))
						}
					}
				}
			}
		}
	}
	return fallback
}

// handleSubscriptionBindWS handles GET/POST /fhir/r4/{tenant}/Subscription/$bind-ws,
// /fhir/r4/{tenant}/Subscription/{id}/$bind-ws, and their $get-ws-binding-token aliases
// per the HL7 FHIR Subscriptions R5 Backport IG.
func (s *Server) handleSubscriptionBindWS(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = "default"
	}
	if resType := r.PathValue("resourceType"); resType != "" && !strings.EqualFold(resType, "Subscription") {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("$bind-ws is only supported on Subscription resources (got %s)", resType))
		return
	}
	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx {
		if !auth.Allows(secCtx, "Subscription", auth.ActionRead) && !auth.Allows(secCtx, "Subscription", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to bind Subscription WebSocket")
			return
		}
	}
	if s.store == nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
		return
	}

	seenIDs := make(map[string]bool)
	var subIDs []string
	addSubID := func(raw string) {
		for part := range strings.SplitSeq(raw, ",") {
			cleaned := cleanID(strings.TrimSpace(part))
			if cleaned != "" && !seenIDs[cleaned] {
				seenIDs[cleaned] = true
				subIDs = append(subIDs, cleaned)
			}
		}
	}

	if pathID := strings.TrimSpace(r.PathValue("id")); pathID != "" {
		addSubID(pathID)
	}
	for _, qid := range r.URL.Query()["id"] {
		addSubID(qid)
	}
	for _, qids := range r.URL.Query()["ids"] {
		addSubID(qids)
	}

	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		if body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024)); err == nil && len(bytes.TrimSpace(body)) > 0 {
			if raw, err := s.decodeOperationBodyMap(r, body, "Parameters"); err == nil {
				if paramsArr, ok := raw["parameter"].([]any); ok {
					for _, p := range paramsArr {
						pm, _ := p.(map[string]any)
						name, _ := pm["name"].(string)
						if name == "id" || name == "ids" || name == "subscription" {
							if sVal, ok := pm["valueString"].(string); ok {
								addSubID(sVal)
							} else if iVal, ok := pm["valueId"].(string); ok {
								addSubID(iVal)
							} else if uVal, ok := pm["valueUri"].(string); ok {
								addSubID(uVal)
							} else if refMap, ok := pm["valueReference"].(map[string]any); ok {
								if refStr, ok := refMap["reference"].(string); ok {
									addSubID(refStr)
								}
							}
						}
					}
				}
			}
		}
	}

	if len(subIDs) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"Subscription/$bind-ws requires at least one Subscription ID via path {id}, ?id=, or Parameters(ids)")
		return
	}

	now := time.Now().UTC()
	for _, sid := range subIDs {
		subMap, statusCode, issueCode, errMsg := s.loadAndValidateWebSocketSubscription(r.Context(), tenant, sid, now)
		if errMsg != "" {
			writeOperationOutcome(w, statusCode, issueCode, errMsg)
			return
		}
		_ = subMap
	}

	if s.subWSHub == nil {
		s.subWSHub = newSubscriptionWSHub()
	}
	tok := s.subWSHub.mintBindToken(tenant, subIDs, secCtx, now)

	wsScheme := "ws"
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		wsScheme = "wss"
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = "localhost:8080"
	}
	wsURL := fmt.Sprintf("%s://%s/fhir/r4/%s/$subscriptions-ws", wsScheme, host, tenant)

	params := []map[string]any{
		{
			"name":        "token",
			"valueString": tok.token,
		},
		{
			"name":         "expiration",
			"valueInstant": tok.expiresAt.UTC().Format(time.RFC3339),
		},
		{
			"name":     "websocket-url",
			"valueUrl": wsURL,
		},
	}
	for _, sid := range subIDs {
		params = append(params, map[string]any{
			"name":        "subscription",
			"valueString": sid,
		})
	}

	s.writeOperationResponse(w, r, http.StatusOK, "Parameters", map[string]any{
		"resourceType": "Parameters",
		"parameter":    params,
	})
}

func (s *Server) loadAndValidateWebSocketSubscription(ctx context.Context, tenant, subID string, now time.Time) (map[string]any, int, string, string) {
	if s.store == nil {
		return nil, http.StatusInternalServerError, "exception", "operational store is not configured"
	}
	rec, err := s.store.ReadResourceWithMeta(ctx, tenant, "Subscription", subID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, http.StatusNotFound, "not-found", fmt.Sprintf("Subscription/%s not found", subID)
	}
	if errors.Is(err, store.ErrResourceDeleted) {
		return nil, http.StatusGone, "deleted", fmt.Sprintf("Subscription/%s has been deleted", subID)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, "exception", "failed to read Subscription"
	}
	subMap, err := s.protoToFHIRMapWithMeta("Subscription", rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		return nil, http.StatusInternalServerError, "exception", "failed to decode Subscription"
	}
	status, _ := subMap["status"].(string)
	if subscriptionIsExpired(subMap, now) {
		status = "off"
	}
	if !strings.EqualFold(status, "active") {
		return nil, http.StatusUnprocessableEntity, "business-rule",
			fmt.Sprintf("Subscription/%s is not active (status=%q)", subID, status)
	}
	ch, _ := subMap["channel"].(map[string]any)
	chType, _ := ch["type"].(string)
	if !strings.EqualFold(strings.TrimSpace(chType), "websocket") {
		return nil, http.StatusBadRequest, "invalid",
			fmt.Sprintf("Subscription/%s channel.type is %q (expected \"websocket\")", subID, chType)
	}
	return subMap, http.StatusOK, "", ""
}

// handleSubscriptionsWS handles GET /fhir/r4/{tenant}/$subscriptions-ws and GET /fhir/r4/$subscriptions-ws,
// upgrading the HTTP connection to an RFC 6455 WebSocket stream for FHIR R4 & R5 Backport Subscriptions.
func (s *Server) handleSubscriptionsWS(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = strings.TrimSpace(r.URL.Query().Get("tenant"))
	}
	if tenant == "" {
		tenant = "default"
	}

	upgradeSecCtx, hasUpgradeSecCtx := auth.FromContext(r.Context())
	if s.subWSHub == nil {
		s.subWSHub = newSubscriptionWSHub()
	}

	ws, err := upgradeWebSocket(w, r, tenant)
	if err != nil {
		return
	}
	s.subWSHub.registerConn(ws)
	defer s.subWSHub.unregisterConn(ws)

	go s.runWSHeartbeatLoop(ws)

	requiresTokenBinding := s.tokenValidator != nil && !hasUpgradeSecCtx
	if qSub := cleanID(strings.TrimSpace(r.URL.Query().Get("subscription"))); qSub != "" && !requiresTokenBinding {
		_ = s.bindWSLegacyOrHandshake(r, ws, qSub, upgradeSecCtx, false)
	}

	for {
		if requiresTokenBinding && ws.boundCount() == 0 {
			s.subWSHub.mu.RLock()
			waitDur := s.subWSHub.initialBindTimeout
			s.subWSHub.mu.RUnlock()
			if waitDur <= 0 {
				waitDur = defaultWSInitialBindWait
			}
			_ = ws.conn.SetReadDeadline(time.Now().Add(waitDur))
		} else {
			_ = ws.conn.SetReadDeadline(time.Time{})
		}

		opcode, payload, readErr := ws.readMessage()
		if readErr != nil {
			return
		}
		if opcode != wsOpText {
			continue
		}

		cmd := strings.TrimSpace(string(payload))
		if cmd == "" {
			continue
		}
		lowerCmd := strings.ToLower(cmd)

		switch {
		case lowerCmd == "ping":
			_ = ws.writeText("pong")

		case strings.HasPrefix(lowerCmd, "bind-with-token:"):
			rawTok := strings.TrimSpace(cmd[len("bind-with-token:"):])
			if err := s.bindWSWithToken(r, ws, rawTok); err != nil {
				_ = ws.writeJSON(map[string]any{
					"resourceType": "OperationOutcome",
					"issue": []map[string]any{
						{
							"severity":    "error",
							"code":        "security",
							"diagnostics": err.Error(),
						},
					},
				})
				if requiresTokenBinding && ws.boundCount() == 0 {
					ws.writeClose(1008, "invalid binding token")
					return
				}
			}

		case strings.HasPrefix(lowerCmd, "bind ") || strings.HasPrefix(lowerCmd, "bind:"):
			var arg string
			if strings.HasPrefix(lowerCmd, "bind:") {
				arg = strings.TrimSpace(cmd[len("bind:"):])
			} else {
				arg = strings.TrimSpace(cmd[len("bind "):])
			}
			if arg != "" && s.subWSHub.hasBindToken(arg) {
				if err := s.bindWSWithToken(r, ws, arg); err != nil {
					_ = ws.writeJSON(map[string]any{
						"resourceType": "OperationOutcome",
						"issue": []map[string]any{
							{
								"severity":    "error",
								"code":        "security",
								"diagnostics": err.Error(),
							},
						},
					})
					if requiresTokenBinding && ws.boundCount() == 0 {
						ws.writeClose(1008, "invalid binding token")
						return
					}
				}
				continue
			}

			if requiresTokenBinding {
				_ = ws.writeJSON(map[string]any{
					"resourceType": "OperationOutcome",
					"issue": []map[string]any{
						{
							"severity":    "error",
							"code":        "login",
							"diagnostics": "unauthenticated WebSocket connection requires bind-with-token from Subscription/$bind-ws",
						},
					},
				})
				ws.writeClose(1008, "authentication required")
				return
			}

			subID := cleanID(arg)
			if err := s.bindWSLegacyOrHandshake(r, ws, subID, upgradeSecCtx, true); err != nil {
				_ = ws.writeJSON(map[string]any{
					"resourceType": "OperationOutcome",
					"issue": []map[string]any{
						{
							"severity":    "error",
							"code":        "invalid",
							"diagnostics": err.Error(),
						},
					},
				})
			}

		case strings.HasPrefix(lowerCmd, "unbind ") || strings.HasPrefix(lowerCmd, "unbind:"):
			var subArg string
			if strings.HasPrefix(lowerCmd, "unbind:") {
				subArg = strings.TrimSpace(cmd[len("unbind:"):])
			} else {
				subArg = strings.TrimSpace(cmd[len("unbind "):])
			}
			subID := cleanID(subArg)
			if subID != "" && s.subWSHub.unbindConnSubscription(ws, subID) {
				_ = ws.writeText("unbound " + subID)
			}

		default:
			_ = ws.writeJSON(map[string]any{
				"resourceType": "OperationOutcome",
				"issue": []map[string]any{
					{
						"severity":    "error",
						"code":        "not-supported",
						"diagnostics": fmt.Sprintf("unsupported WebSocket command %q", cmd),
					},
				},
			})
		}
	}
}

func (s *Server) bindWSWithToken(r *http.Request, ws *wsSubscriberConn, rawTok string) error {
	now := time.Now().UTC()
	tok, err := s.subWSHub.consumeBindToken(ws.tenant, rawTok, now)
	if err != nil {
		return err
	}
	s.subWSHub.mu.RLock()
	defaultHB := s.subWSHub.defaultHeartbeat
	s.subWSHub.mu.RUnlock()

	for _, subID := range tok.subscriptionIDs {
		subMap, _, _, errMsg := s.loadAndValidateWebSocketSubscription(context.WithoutCancel(r.Context()), ws.tenant, subID, now)
		if errMsg != "" {
			return errors.New(errMsg)
		}
		contentMode, _ := extractSubscriptionChannelContentMode(subMap)
		hb := extractSubscriptionHeartbeatPeriod(subMap, defaultHB)
		binding := &wsSubscriptionBinding{
			subID:           subID,
			contentMode:     contentMode,
			legacyPing:      false,
			heartbeatPeriod: hb,
			lastHeartbeat:   now,
			secCtx:          tok.secCtx,
		}
		s.subWSHub.bindConnSubscription(ws, binding)
		s.recordWSBindAuditEvent(r, ws.tenant, subID, tok.secCtx)

		handshakeBundle := s.buildSubscriptionNotificationBundle(ws.tenant, subID, "active", "handshake", "empty", nil)
		if err := ws.writeJSON(handshakeBundle); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) bindWSLegacyOrHandshake(r *http.Request, ws *wsSubscriberConn, subID string, secCtx *auth.SecurityContext, sendBoundText bool) error {
	if subID == "" {
		return fmt.Errorf("missing Subscription ID in bind command")
	}
	now := time.Now().UTC()
	subMap, _, _, errMsg := s.loadAndValidateWebSocketSubscription(context.WithoutCancel(r.Context()), ws.tenant, subID, now)
	if errMsg != "" {
		return errors.New(errMsg)
	}
	contentMode, explicitPayload := extractSubscriptionChannelContentMode(subMap)
	s.subWSHub.mu.RLock()
	defaultHB := s.subWSHub.defaultHeartbeat
	s.subWSHub.mu.RUnlock()
	hb := extractSubscriptionHeartbeatPeriod(subMap, defaultHB)

	binding := &wsSubscriptionBinding{
		subID:           subID,
		contentMode:     contentMode,
		legacyPing:      sendBoundText && !explicitPayload,
		heartbeatPeriod: hb,
		lastHeartbeat:   now,
		secCtx:          secCtx,
	}
	s.subWSHub.bindConnSubscription(ws, binding)
	s.recordWSBindAuditEvent(r, ws.tenant, subID, secCtx)

	if sendBoundText {
		return ws.writeText("bound " + subID)
	}
	handshakeBundle := s.buildSubscriptionNotificationBundle(ws.tenant, subID, "active", "handshake", "empty", nil)
	return ws.writeJSON(handshakeBundle)
}

func (s *Server) recordWSBindAuditEvent(r *http.Request, tenant, subID string, secCtx *auth.SecurityContext) {
	if s.auditRecorder == nil {
		return
	}
	subject := "anonymous"
	patientID := ""
	if secCtx != nil {
		if secCtx.GetSubject() != "" {
			subject = secCtx.GetSubject()
		}
		patientID = cleanID(secCtx.GetPatientId())
	}
	rec := store.AuditRecord{
		TenantID:     tenant,
		AuditID:      uuid.NewString(),
		Recorded:     time.Now().UTC(),
		Action:       audit.ActionExecute,
		SubtypeCode:  "subscriptions-ws-bind",
		Outcome:      audit.OutcomeSuccess,
		OutcomeDesc:  "101 Switching Protocols",
		HTTPMethod:   http.MethodGet,
		HTTPStatus:   http.StatusSwitchingProtocols,
		RequestURI:   r.URL.RequestURI(),
		AgentSubject: subject,
		AgentPatient: patientID,
		ClientIP:     extractClientIP(r),
		EntityType:   "Subscription",
		EntityID:     subID,
	}
	_ = s.auditRecorder.Record(context.WithoutCancel(r.Context()), rec)
}

func (s *Server) runWSHeartbeatLoop(ws *wsSubscriberConn) {
	s.subWSHub.mu.RLock()
	baseInterval := s.subWSHub.defaultHeartbeat
	s.subWSHub.mu.RUnlock()

	tickDur := 250 * time.Millisecond
	if baseInterval > 0 && baseInterval/2 < tickDur {
		tickDur = max(baseInterval/2, 15*time.Millisecond)
	}
	ticker := time.NewTicker(tickDur)
	defer ticker.Stop()

	for {
		select {
		case <-ws.done:
			return
		case now := <-ticker.C:
			var due []*wsSubscriptionBinding
			ws.mu.Lock()
			for _, b := range ws.bindings {
				if b.legacyPing || b.heartbeatPeriod <= 0 {
					continue
				}
				if now.Sub(b.lastHeartbeat) >= b.heartbeatPeriod {
					b.lastHeartbeat = now
					cp := *b
					due = append(due, &cp)
				}
			}
			ws.mu.Unlock()

			for _, b := range due {
				hbBundle := s.buildSubscriptionNotificationBundle(ws.tenant, b.subID, "active", "heartbeat", "empty", nil)
				if err := ws.writeJSON(hbBundle); err != nil {
					s.subWSHub.unregisterConn(ws)
					return
				}
			}
		}
	}
}

func (s *Server) wsSubscriberAllowedForEvent(secCtx *auth.SecurityContext, tenant, resType, resID string, resourceProto []byte, idx *store.SearchIndexes) bool {
	if secCtx == nil {
		return true
	}
	if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionSearch) {
		return false
	}
	if constraints, constrained := auth.ScopeQueryConstraints(secCtx, resType, auth.ActionRead); constrained && len(resourceProto) > 0 {
		if !s.resourceSatisfiesScopeConstraints(tenant, resType, resID, resourceProto, constraints) {
			return false
		}
	}
	if auth.IsPatientRestricted(secCtx) {
		allowedPat := cleanID(secCtx.GetPatientId())
		if allowedPat == "" {
			return false
		}
		if strings.EqualFold(resType, "Patient") {
			return cleanID(resID) == allowedPat
		}
		if !store.IsPatientCompartmentResourceType(resType) {
			return false
		}
		return store.IndexesBelongToPatientCompartment(resType, resID, idx, allowedPat)
	}
	return true
}

func (s *Server) dispatchWebSocketSubscription(
	tenant, subID, status string,
	ev subscriptionEventRecord,
	resourceProto []byte,
	idx *store.SearchIndexes,
) {
	if s.subWSHub == nil {
		return
	}
	conns := s.subWSHub.subscribersFor(tenant, subID)
	if len(conns) == 0 {
		return
	}
	eventsSlice := []subscriptionEventRecord{ev}
	for _, ws := range conns {
		b := ws.getBinding(subID)
		if b == nil {
			continue
		}
		if !s.wsSubscriberAllowedForEvent(b.secCtx, tenant, ev.ResType, ev.ResID, resourceProto, idx) {
			continue
		}
		var sendErr error
		if b.legacyPing {
			sendErr = ws.writeText("ping " + subID)
		} else {
			bundle := s.buildSubscriptionNotificationBundle(tenant, subID, status, "event-notification", b.contentMode, eventsSlice)
			sendErr = ws.writeJSON(bundle)
		}
		if sendErr != nil {
			s.recordSubscriptionDelivery(tenant, subID, false, sendErr.Error())
			if s.metrics != nil {
				s.metrics.subscriptionDeliveryErrorsTotal.Add(1)
			}
			s.subWSHub.unregisterConn(ws)
			continue
		}
		s.recordSubscriptionDelivery(tenant, subID, true, "")
		if s.metrics != nil {
			s.metrics.subscriptionsDeliveredTotal.Add(1)
		}
	}
}

func (s *Server) syncSubscriptionWebSocketLifecycle(tenant, subID string, resourceProto []byte) {
	if s.subWSHub == nil || subID == "" {
		return
	}
	if len(resourceProto) == 0 {
		s.subWSHub.unbindAllForSubscription(tenant, subID)
		return
	}
	subMap, err := s.protoToFHIRMap("Subscription", resourceProto)
	if err != nil {
		return
	}
	status, _ := subMap["status"].(string)
	if subscriptionIsExpired(subMap, time.Now().UTC()) || !strings.EqualFold(status, "active") {
		s.subWSHub.unbindAllForSubscription(tenant, subID)
	}
}

func (s *Server) buildSubscriptionNotificationBundle(
	tenant, subID, status, notificationType, contentMode string,
	matched []subscriptionEventRecord,
) map[string]any {
	stats := s.getSubscriptionDeliveryStats(tenant, subID)
	totalEvents := stats.eventsDelivered
	if n := len(stats.events); n > 0 && stats.events[n-1].EventNumber > totalEvents {
		totalEvents = stats.events[n-1].EventNumber
	}

	statusParams := []map[string]any{
		{
			"name": "subscription",
			"valueReference": map[string]any{
				"reference": "Subscription/" + subID,
			},
		},
		{
			"name":      "status",
			"valueCode": status,
		},
		{
			"name":      "type",
			"valueCode": notificationType,
		},
		{
			"name":        "events-since-subscription-start",
			"valueString": strconv.FormatInt(totalEvents, 10),
		},
		{
			"name":         "events-in-notification",
			"valueInteger": len(matched),
		},
	}

	for _, ev := range matched {
		parts := []map[string]any{
			{
				"name":        "event-number",
				"valueString": strconv.FormatInt(ev.EventNumber, 10),
			},
			{
				"name":         "timestamp",
				"valueInstant": ev.Timestamp.UTC().Format(time.RFC3339),
			},
		}
		if contentMode != "empty" {
			parts = append(parts, map[string]any{
				"name": "focus",
				"valueReference": map[string]any{
					"reference": ev.ResType + "/" + ev.ResID,
				},
			})
		}
		statusParams = append(statusParams, map[string]any{
			"name": "notification-event",
			"part": parts,
		})
	}

	statusRes := map[string]any{
		"resourceType": "Parameters",
		"id":           subID + "-" + notificationType + "-status",
		"parameter":    statusParams,
	}
	if notificationType == "event-notification" {
		statusRes["id"] = subID + "-events-status"
	}

	entries := []map[string]any{
		{
			"fullUrl":  fmt.Sprintf("Subscription/%s/$status", subID),
			"resource": statusRes,
			"request": map[string]any{
				"method": "GET",
				"url":    fmt.Sprintf("Subscription/%s/$status", subID),
			},
			"response": map[string]any{
				"status": "200 OK",
			},
		},
	}

	if contentMode != "empty" {
		for _, ev := range matched {
			method := "PUT"
			respStatus := "200 OK"
			if ev.Action == "delete" {
				method = "DELETE"
				respStatus = "204 No Content"
			}
			entry := map[string]any{
				"fullUrl": ev.ResType + "/" + ev.ResID,
				"request": map[string]any{
					"method": method,
					"url":    ev.ResType + "/" + ev.ResID,
				},
				"response": map[string]any{
					"status": respStatus,
				},
			}
			if ev.Action != "delete" {
				if contentMode == "full-resource" && ev.Resource != nil {
					entry["resource"] = ev.Resource
				} else if contentMode == "id-only" {
					entry["resource"] = map[string]any{
						"resourceType": ev.ResType,
						"id":           ev.ResID,
						"meta": map[string]any{
							"versionId":   strconv.Itoa(ev.ResVersion),
							"lastUpdated": ev.Timestamp.UTC().Format(time.RFC3339),
						},
					}
				}
			} else if contentMode == "id-only" {
				entry["resource"] = map[string]any{
					"resourceType": ev.ResType,
					"id":           ev.ResID,
					"meta": map[string]any{
						"versionId":   strconv.Itoa(ev.ResVersion),
						"lastUpdated": ev.Timestamp.UTC().Format(time.RFC3339),
					},
				}
			}
			entries = append(entries, entry)
		}
	}

	return map[string]any{
		"resourceType": "Bundle",
		"type":         "history",
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"total":        len(matched),
		"entry":        entries,
	}
}
