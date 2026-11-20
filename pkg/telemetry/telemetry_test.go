package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"hegel.dev/go/hegel"

	"github.com/flint-fhir/flint/pkg/telemetry"
)

func TestW3CTraceParent_ParseFormatExtractInject(t *testing.T) {
	valid := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc, ok := telemetry.ParseTraceParent(valid)
	require.True(t, ok)
	assert.True(t, sc.IsValid())
	assert.True(t, sc.IsSampled())
	assert.True(t, sc.Remote)
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", sc.TraceIDHex())
	assert.Equal(t, "00f067aa0ba902b7", sc.SpanIDHex())
	assert.Equal(t, valid, telemetry.FormatTraceParent(sc))

	invalidCases := []string{
		"",
		"bad",
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",       // forbidden version ff
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", // extra segment in v00
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",       // uppercase hex rejected
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",       // all-zero trace-id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",       // all-zero parent-id
		"00-4bf92f3577b34da6-00f067aa0ba902b7-01",                       // short trace-id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa-01",               // short span-id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz",       // bad flags
	}
	for _, tc := range invalidCases {
		_, parsedOK := telemetry.ParseTraceParent(tc)
		assert.False(t, parsedOK, "expected ParseTraceParent(%q) to fail", tc)
	}

	assert.Empty(t, telemetry.FormatTraceParent(telemetry.SpanContext{}))
	assert.Empty(t, (telemetry.SpanContext{}).TraceIDHex())
	assert.Empty(t, (telemetry.SpanContext{}).SpanIDHex())

	inHdr := http.Header{}
	inHdr.Set("Traceparent", valid)
	inHdr.Set("Tracestate", "flint=node-a,vendor=ok")
	ctx := telemetry.ExtractHTTP(context.Background(), inHdr)

	outHdr := http.Header{}
	telemetry.InjectHTTP(ctx, outHdr)
	assert.Equal(t, valid, outHdr.Get("Traceparent"))
	assert.Equal(t, "flint=node-a,vendor=ok", outHdr.Get("Tracestate"))
}

func TestTracer_OTLPProtoAndJSONExport_HTTPTransportAndSlog(t *testing.T) {
	var (
		mu          sync.Mutex
		protoBodies [][]byte
		jsonBodies  [][]byte
		gotAuthHdr  string
	)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotAuthHdr = r.Header.Get("Authorization")
		if strings.Contains(r.Header.Get("Content-Type"), "protobuf") {
			protoBodies = append(protoBodies, raw)
		} else {
			jsonBodies = append(jsonBodies, raw)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	// 1. OTLP/HTTP binary Protobuf exporter + child spans + HTTP client transport + slog correlation
	var captured []telemetry.SpanData
	var capMu sync.Mutex
	trProto, err := telemetry.NewTracer(telemetry.Config{
		Endpoint:       collector.URL, // base URL auto-appends /v1/traces
		Protocol:       "http/protobuf",
		ServiceName:    "flint-test",
		ServiceVersion: "0.7.0",
		Headers:        telemetry.ParseHeaders("Authorization=Bearer otlp-secret, X-Env=ci"),
		SampleRatio:    telemetry.Float64Ptr(1.0),
		BatchSize:      4,
		FlushInterval:  50 * time.Millisecond,
		Exporter: func(_ context.Context, spans []telemetry.SpanData) error {
			capMu.Lock()
			captured = append(captured, spans...)
			capMu.Unlock()
			return nil
		},
	})
	require.NoError(t, err)
	require.True(t, trProto.Enabled())
	assert.Equal(t, collector.URL+"/v1/traces", trProto.Endpoint())
	assert.Equal(t, "http/protobuf", trProto.Protocol())

	var logBuf bytes.Buffer
	logger := slog.New(telemetry.WrapSlogHandler(slog.NewJSONHandler(&logBuf, nil))).With("component", "fhir").WithGroup("req")

	// Downstream HTTP server verifying incoming W3C traceparent injected by NewHTTPTransport
	var downstreamTraceParent string
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstreamTraceParent = r.Header.Get("Traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	defer downstream.Close()

	rootCtx, rootSpan := trProto.Start(
		context.Background(),
		"POST /fhir/r4/{tenant}/Patient",
		telemetry.SpanKindServer,
		telemetry.String("flint.tenant_id", "demo"),
		telemetry.Int("http.response.status_code", 201),
		telemetry.Int64("flint.bytes", 512),
		telemetry.Float64("flint.latency_ms", 3.5),
		telemetry.Bool("flint.cached", false),
	)
	require.NotNil(t, rootSpan)
	logger.InfoContext(rootCtx, "handling patient write", "id", "pat-1")

	httpCli := &http.Client{
		Transport: telemetry.NewHTTPTransport(http.DefaultTransport, trProto, "fhir.terminology.lookup"),
	}
	outReq, err := http.NewRequestWithContext(rootCtx, http.MethodGet, downstream.URL+"/CodeSystem/$lookup", nil)
	require.NoError(t, err)
	outResp, err := httpCli.Do(outReq)
	require.NoError(t, err)
	_ = outResp.Body.Close()

	_, errSpan := trProto.Start(rootCtx, "store.WriteResource", telemetry.SpanKindInternal)
	errSpan.RecordError(errors.New("simulated conflict"))
	errSpan.End()

	rootSpan.SetStatus(telemetry.StatusOK, "")
	rootSpan.End()
	rootSpan.End() // idempotent second End()

	require.NoError(t, trProto.Shutdown(context.Background()))
	assert.Equal(t, uint64(3), trProto.ExportedSpans())
	assert.Equal(t, uint64(0), trProto.ExportErrors())
	assert.Equal(t, uint64(0), trProto.DroppedSpans())

	// Verify slog JSON output contains exact root trace_id & span_id
	assert.Contains(t, logBuf.String(), rootSpan.SpanContext().TraceIDHex())
	assert.Contains(t, logBuf.String(), rootSpan.SpanContext().SpanIDHex())

	// Verify downstream received valid W3C traceparent sharing root TraceID
	parsedDown, ok := telemetry.ParseTraceParent(downstreamTraceParent)
	require.True(t, ok)
	assert.Equal(t, rootSpan.SpanContext().TraceID, parsedDown.TraceID)
	assert.NotEqual(t, rootSpan.SpanContext().SpanID, parsedDown.SpanID)

	// Verify binary OTLP protobuf wire framing (field 1 = ResourceSpans)
	mu.Lock()
	require.NotEmpty(t, protoBodies)
	assert.Equal(t, "Bearer otlp-secret", gotAuthHdr)
	num, typ, n := protowire.ConsumeTag(protoBodies[0])
	mu.Unlock()
	require.Greater(t, n, 0)
	assert.Equal(t, protowire.Number(1), num)
	assert.Equal(t, protowire.BytesType, typ)

	// 2. OTLP/HTTP JSON exporter + 0.0 sample ratio + error collector handling
	trJSON, err := telemetry.NewTracer(telemetry.Config{
		Endpoint:       collector.URL + "/v1/traces",
		Protocol:       "http/json",
		ServiceName:    "flint-json",
		ServiceVersion: "0.7.0",
	})
	require.NoError(t, err)
	_, jSpan := trJSON.Start(context.Background(), "GET /healthz", telemetry.SpanKindServer, telemetry.String("check", "liveness"))
	jSpan.End()
	require.NoError(t, trJSON.Shutdown(context.Background()))

	mu.Lock()
	require.Len(t, jsonBodies, 1)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(jsonBodies[0], &decoded))
	mu.Unlock()
	assert.Contains(t, string(jsonBodies[0]), "flint-json")

	// SampleRatio = 0.0 drops unsampled root traces while respecting incoming sampled parent
	zeroRatio, err := telemetry.ParseSampleRatio("0.0")
	require.NoError(t, err)
	trZero, err := telemetry.NewTracer(telemetry.Config{
		Endpoint:    collector.URL,
		SampleRatio: zeroRatio,
	})
	require.NoError(t, err)
	_, unsampled := trZero.Start(context.Background(), "unsampled", telemetry.SpanKindInternal)
	assert.Nil(t, unsampled)

	remoteCtx := telemetry.ExtractHTTP(context.Background(), http.Header{"Traceparent": []string{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}})
	_, parentSampled := trZero.Start(remoteCtx, "sampled-from-parent", telemetry.SpanKindServer)
	require.NotNil(t, parentSampled)
	parentSampled.End()
	require.NoError(t, trZero.Shutdown(context.Background()))

	// Validation & error branches
	_, err = telemetry.ParseSampleRatio("1.5")
	require.Error(t, err)
	_, err = telemetry.NewTracer(telemetry.Config{Endpoint: "ftp://bad"})
	require.Error(t, err)

	noop := telemetry.NoopTracer()
	assert.False(t, noop.Enabled())
	_, nSpan := noop.Start(context.Background(), "noop", telemetry.SpanKindInternal)
	assert.Nil(t, nSpan)
	require.NoError(t, noop.Shutdown(context.Background()))
}

func TestProperty_W3CTraceParentRoundTrip(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		var sc telemetry.SpanContext
		for i := range sc.TraceID {
			sc.TraceID[i] = byte(hegel.Draw(ht, hegel.Integers(0, 255)))
		}
		for i := range sc.SpanID {
			sc.SpanID[i] = byte(hegel.Draw(ht, hegel.Integers(0, 255)))
		}
		sc.TraceFlags = byte(hegel.Draw(ht, hegel.Integers(0, 255)))
		if !sc.IsValid() {
			return
		}
		formatted := telemetry.FormatTraceParent(sc)
		parsed, ok := telemetry.ParseTraceParent(formatted)
		if !ok {
			ht.Fatalf("ParseTraceParent failed on %q", formatted)
		}
		if parsed.TraceID != sc.TraceID || parsed.SpanID != sc.SpanID || parsed.TraceFlags != sc.TraceFlags {
			ht.Fatalf("round-trip mismatch: got %+v want %+v", parsed, sc)
		}
	}, hegel.WithTestCases(100))
}
