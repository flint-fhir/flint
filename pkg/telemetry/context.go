// Package telemetry implements pure-Go W3C Trace Context Level 1 propagation,
// OpenTelemetry Protocol (OTLP/HTTP v1 Protobuf & JSON) batch span export,
// and trace-correlated slog structured logging with zero external SDK bloat.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// FlagSampled is the W3C trace-flags bit indicating the trace is sampled (0x01).
const FlagSampled byte = 0x01

// SpanContext holds immutable W3C Trace Context identifiers for a span.
type SpanContext struct {
	TraceID    [16]byte
	SpanID     [8]byte
	TraceFlags byte
	TraceState string
	Remote     bool
}

var (
	zeroTraceID [16]byte
	zeroSpanID  [8]byte
)

// IsValid reports whether both TraceID and SpanID are non-zero per W3C Trace Context.
func (sc SpanContext) IsValid() bool {
	return sc.TraceID != zeroTraceID && sc.SpanID != zeroSpanID
}

// IsSampled reports whether the W3C sampled flag (0x01) is set.
func (sc SpanContext) IsSampled() bool {
	return (sc.TraceFlags & FlagSampled) != 0
}

// TraceIDHex returns the 32-character lowercase hex representation of TraceID, or "" if invalid.
func (sc SpanContext) TraceIDHex() string {
	if sc.TraceID == zeroTraceID {
		return ""
	}
	return hex.EncodeToString(sc.TraceID[:])
}

// SpanIDHex returns the 16-character lowercase hex representation of SpanID, or "" if invalid.
func (sc SpanContext) SpanIDHex() string {
	if sc.SpanID == zeroSpanID {
		return ""
	}
	return hex.EncodeToString(sc.SpanID[:])
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return len(s) > 0
}

// ParseTraceParent parses a W3C Trace Context "traceparent" header value
// ("00-<32-lower-hex-trace-id>-<16-lower-hex-parent-id>-<2-lower-hex-flags>").
func ParseTraceParent(raw string) (SpanContext, bool) {
	trimmed := strings.TrimSpace(raw)
	parts := strings.Split(trimmed, "-")
	if len(parts) < 4 {
		return SpanContext{}, false
	}
	verHex, traceHex, spanHex, flagsHex := parts[0], parts[1], parts[2], parts[3]
	if len(verHex) != 2 || !isLowerHex(verHex) || verHex == "ff" {
		return SpanContext{}, false
	}
	if verHex == "00" && len(parts) != 4 {
		return SpanContext{}, false
	}
	if len(traceHex) != 32 || !isLowerHex(traceHex) {
		return SpanContext{}, false
	}
	if len(spanHex) != 16 || !isLowerHex(spanHex) {
		return SpanContext{}, false
	}
	if len(flagsHex) != 2 || !isLowerHex(flagsHex) {
		return SpanContext{}, false
	}

	var sc SpanContext
	if _, err := hex.Decode(sc.TraceID[:], []byte(traceHex)); err != nil {
		return SpanContext{}, false
	}
	if _, err := hex.Decode(sc.SpanID[:], []byte(spanHex)); err != nil {
		return SpanContext{}, false
	}
	var flagBuf [1]byte
	if _, err := hex.Decode(flagBuf[:], []byte(flagsHex)); err != nil {
		return SpanContext{}, false
	}
	sc.TraceFlags = flagBuf[0]
	sc.Remote = true
	if !sc.IsValid() {
		return SpanContext{}, false
	}
	return sc, true
}

// FormatTraceParent formats a valid SpanContext into a W3C "traceparent" header string.
func FormatTraceParent(sc SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%02x", sc.TraceIDHex(), sc.SpanIDHex(), sc.TraceFlags)
}

type spanContextKeyType struct{}

var spanContextKey = spanContextKeyType{}

// ContextWithSpanContext returns a derived context carrying sc.
func ContextWithSpanContext(ctx context.Context, sc SpanContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, spanContextKey, sc)
}

// SpanContextFromContext retrieves the active SpanContext from ctx, if present and valid.
func SpanContextFromContext(ctx context.Context) (SpanContext, bool) {
	if ctx == nil {
		return SpanContext{}, false
	}
	sc, ok := ctx.Value(spanContextKey).(SpanContext)
	if !ok || !sc.IsValid() {
		return SpanContext{}, false
	}
	return sc, true
}

// FromContext is a shorthand alias for SpanContextFromContext.
func FromContext(ctx context.Context) (SpanContext, bool) {
	return SpanContextFromContext(ctx)
}

// ExtractHTTP extracts incoming W3C "traceparent" and "tracestate" headers into ctx.
func ExtractHTTP(ctx context.Context, h http.Header) context.Context {
	if h == nil {
		return ctx
	}
	tp := h.Get("Traceparent")
	if tp == "" {
		return ctx
	}
	sc, ok := ParseTraceParent(tp)
	if !ok {
		return ctx
	}
	sc.TraceState = strings.TrimSpace(h.Get("Tracestate"))
	return ContextWithSpanContext(ctx, sc)
}

// InjectHTTP writes W3C "traceparent" and optional "tracestate" headers from ctx into h.
func InjectHTTP(ctx context.Context, h http.Header) {
	if h == nil {
		return
	}
	sc, ok := SpanContextFromContext(ctx)
	if !ok {
		return
	}
	h.Set("Traceparent", FormatTraceParent(sc))
	if sc.TraceState != "" {
		h.Set("Tracestate", sc.TraceState)
	}
}

func newRandomTraceID() [16]byte {
	var id [16]byte
	_, _ = rand.Read(id[:])
	if id == zeroTraceID {
		id[15] = 1
	}
	return id
}

func newRandomSpanID() [8]byte {
	var id [8]byte
	_, _ = rand.Read(id[:])
	if id == zeroSpanID {
		id[7] = 1
	}
	return id
}
