package telemetry

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config configures an OpenTelemetry OTLP/HTTP v1 Tracer.
type Config struct {
	// Endpoint is the OTLP/HTTP collector base URL or /v1/traces endpoint
	// (e.g. "http://otel-collector:4318" or "http://otel-collector:4318/v1/traces").
	// When empty and Exporter is nil, NewTracer returns a zero-overhead no-op Tracer.
	Endpoint       string
	Protocol       string // "http/protobuf" (default) or "http/json"
	ServiceName    string
	ServiceVersion string
	Headers        map[string]string
	// SampleRatio controls parent-based trace-ID ratio sampling in [0.0, 1.0].
	// Nil defaults to 1.0 (sample 100% of root traces).
	SampleRatio   *float64
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	HTTPClient    *http.Client
	// Exporter is an optional in-memory hook (e.g. for deterministic testing).
	Exporter func(ctx context.Context, spans []SpanData) error
}

// Float64Ptr returns a pointer to v (convenience helper for Config.SampleRatio).
func Float64Ptr(v float64) *float64 { return &v }

// ParseHeaders parses comma-separated "Key=Value,Key2=Value2" OTLP header strings.
func ParseHeaders(raw string) map[string]string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	out := make(map[string]string)
	for part := range strings.SplitSeq(trimmed, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(k) != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// ParseSampleRatio parses a [0.0, 1.0] float string for --otlp-sample-ratio / FLINT_OTLP_SAMPLE_RATIO.
func ParseSampleRatio(raw string) (*float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(v) || v < 0.0 || v > 1.0 {
		return nil, fmt.Errorf("invalid OTLP sample ratio %q (expected float in [0.0, 1.0])", raw)
	}
	return &v, nil
}

func resolveTracesURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid OTLP HTTP endpoint %q (expected http:// or https:// URL)", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/traces"
	}
	return u.String(), nil
}

// Tracer creates W3C spans and exports sampled spans asynchronously over OTLP/HTTP v1.
type Tracer struct {
	enabled         bool
	tracesURL       string
	useJSON         bool
	serviceName     string
	serviceVersion  string
	headers         map[string]string
	sampleThreshold uint64
	sampleAll       bool
	sampleNone      bool
	batchSize       int
	flushInterval   time.Duration
	httpClient      *http.Client
	customExporter  func(ctx context.Context, spans []SpanData) error

	queue         chan SpanData
	stopCh        chan struct{}
	wg            sync.WaitGroup
	closeOnce     sync.Once
	closed        atomic.Bool
	exportedSpans atomic.Uint64
	exportErrors  atomic.Uint64
	droppedSpans  atomic.Uint64
}

// NoopTracer returns a disabled zero-overhead Tracer.
func NoopTracer() *Tracer {
	return &Tracer{enabled: false}
}

// NewTracer initializes an asynchronous OTLP/HTTP v1 Tracer from cfg.
func NewTracer(cfg Config) (*Tracer, error) {
	tracesURL, err := resolveTracesURL(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if tracesURL == "" && cfg.Exporter == nil {
		return NoopTracer(), nil
	}

	ratio := 1.0
	if cfg.SampleRatio != nil {
		ratio = *cfg.SampleRatio
		if ratio < 0 {
			ratio = 0
		} else if ratio > 1 {
			ratio = 1
		}
	}

	qSize := cfg.QueueSize
	if qSize <= 0 {
		qSize = 2048
	}
	bSize := cfg.BatchSize
	if bSize <= 0 {
		bSize = 256
	}
	flushInt := cfg.FlushInterval
	if flushInt <= 0 {
		flushInt = 2 * time.Second
	}
	cli := cfg.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 5 * time.Second}
	}
	svcName := strings.TrimSpace(cfg.ServiceName)
	if svcName == "" {
		svcName = "flintd"
	}
	protoLower := strings.ToLower(strings.TrimSpace(cfg.Protocol))
	useJSON := protoLower == "http/json" || protoLower == "json"

	t := &Tracer{
		enabled:         true,
		tracesURL:       tracesURL,
		useJSON:         useJSON,
		serviceName:     svcName,
		serviceVersion:  strings.TrimSpace(cfg.ServiceVersion),
		headers:         cfg.Headers,
		sampleAll:       ratio >= 1.0,
		sampleNone:      ratio <= 0.0,
		sampleThreshold: uint64(ratio * float64(math.MaxUint64)),
		batchSize:       bSize,
		flushInterval:   flushInt,
		httpClient:      cli,
		customExporter:  cfg.Exporter,
		queue:           make(chan SpanData, qSize),
		stopCh:          make(chan struct{}),
	}

	t.wg.Add(1)
	go t.runBatchWorker()
	return t, nil
}

// Enabled reports whether this Tracer is actively recording and exporting spans.
func (t *Tracer) Enabled() bool {
	return t != nil && t.enabled && !t.closed.Load()
}

// Endpoint returns the resolved OTLP/HTTP traces URL (or "" if disabled/in-memory).
func (t *Tracer) Endpoint() string {
	if t == nil {
		return ""
	}
	return t.tracesURL
}

// Protocol returns the active OTLP/HTTP wire protocol ("http/protobuf" or "http/json").
func (t *Tracer) Protocol() string {
	if t == nil {
		return ""
	}
	if t.useJSON {
		return "http/json"
	}
	return "http/protobuf"
}

// ExportedSpans returns the total number of spans successfully exported via OTLP/HTTP.
func (t *Tracer) ExportedSpans() uint64 {
	if t == nil {
		return 0
	}
	return t.exportedSpans.Load()
}

// ExportErrors returns the total number of failed OTLP/HTTP export batches.
func (t *Tracer) ExportErrors() uint64 {
	if t == nil {
		return 0
	}
	return t.exportErrors.Load()
}

// DroppedSpans returns the total number of spans dropped due to a full async queue.
func (t *Tracer) DroppedSpans() uint64 {
	if t == nil {
		return 0
	}
	return t.droppedSpans.Load()
}

func (t *Tracer) shouldSample(traceID [16]byte, parent SpanContext, hasParent bool) bool {
	if hasParent {
		return parent.IsSampled()
	}
	if t.sampleAll {
		return true
	}
	if t.sampleNone {
		return false
	}
	upper := binary.BigEndian.Uint64(traceID[:8])
	return upper <= t.sampleThreshold
}

// Start begins a new Span as a child of any active SpanContext in ctx.
// Even when t is nil or disabled, if ctx already carries an incoming W3C traceparent,
// ctx is returned intact so downstream propagation continues seamlessly.
func (t *Tracer) Start(ctx context.Context, name string, kind SpanKind, attrs ...Attribute) (context.Context, *Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t == nil || !t.enabled || t.closed.Load() {
		return ctx, nil
	}

	parent, hasParent := SpanContextFromContext(ctx)
	var traceID [16]byte
	var parentSpanID [8]byte
	var traceState string
	if hasParent {
		traceID = parent.TraceID
		parentSpanID = parent.SpanID
		traceState = parent.TraceState
	} else {
		traceID = newRandomTraceID()
	}
	spanID := newRandomSpanID()
	sampled := t.shouldSample(traceID, parent, hasParent)
	var flags byte
	if sampled {
		flags = FlagSampled
	}

	sc := SpanContext{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: flags,
		TraceState: traceState,
	}
	childCtx := ContextWithSpanContext(ctx, sc)
	if !sampled {
		return childCtx, nil
	}

	sp := &Span{
		tracer:       t,
		sc:           sc,
		parentSpanID: parentSpanID,
		name:         name,
		kind:         kind,
		startTime:    time.Now().UTC(),
		attrs:        append([]Attribute(nil), attrs...),
	}
	return childCtx, sp
}

// Shutdown flushes all queued spans and stops the background OTLP batch worker.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t == nil || !t.enabled {
		return nil
	}
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		close(t.stopCh)
	})
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *Tracer) enqueue(sd SpanData) {
	if t == nil || !t.enabled {
		return
	}
	select {
	case t.queue <- sd:
	default:
		t.droppedSpans.Add(1)
	}
}

func (t *Tracer) runBatchWorker() {
	defer t.wg.Done()
	ticker := time.NewTicker(t.flushInterval)
	defer ticker.Stop()

	batch := make([]SpanData, 0, t.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		toSend := make([]SpanData, len(batch))
		copy(toSend, batch)
		batch = batch[:0]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := t.exportBatch(ctx, toSend)
		cancel()
		if err != nil {
			t.exportErrors.Add(1)
		} else {
			t.exportedSpans.Add(uint64(len(toSend)))
		}
	}

	for {
		select {
		case sd := <-t.queue:
			batch = append(batch, sd)
			if len(batch) >= t.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-t.stopCh:
			for {
				select {
				case sd := <-t.queue:
					batch = append(batch, sd)
					if len(batch) >= t.batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (t *Tracer) exportBatch(ctx context.Context, spans []SpanData) error {
	if t.customExporter != nil {
		if err := t.customExporter(ctx, spans); err != nil {
			return err
		}
		if t.tracesURL == "" {
			return nil
		}
	}
	if t.tracesURL == "" {
		return nil
	}

	var body []byte
	var contentType string
	if t.useJSON {
		var err error
		body, err = EncodeOTLPJSON(t.serviceName, t.serviceVersion, spans)
		if err != nil {
			return err
		}
		contentType = "application/json"
	} else {
		body = EncodeOTLPProto(t.serviceName, t.serviceVersion, spans)
		contentType = "application/x-protobuf"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tracesURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("otlp collector returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// Span represents an in-flight OpenTelemetry span. All methods are nil-safe.
type Span struct {
	mu           sync.Mutex
	tracer       *Tracer
	sc           SpanContext
	parentSpanID [8]byte
	name         string
	kind         SpanKind
	startTime    time.Time
	attrs        []Attribute
	status       StatusCode
	statusMsg    string
	ended        bool
}

// SpanContext returns the span's W3C SpanContext.
func (s *Span) SpanContext() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	return s.sc
}

// SetAttributes appends attributes to the span.
func (s *Span) SetAttributes(attrs ...Attribute) {
	if s == nil || len(attrs) == 0 {
		return
	}
	s.mu.Lock()
	if !s.ended {
		s.attrs = append(s.attrs, attrs...)
	}
	s.mu.Unlock()
}

// SetStatus sets the span's status code and optional description.
func (s *Span) SetStatus(code StatusCode, msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.ended {
		s.status = code
		s.statusMsg = msg
	}
	s.mu.Unlock()
}

// RecordError marks the span with StatusError and records error.type / exception.message attributes.
func (s *Span) RecordError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	if !s.ended {
		s.status = StatusError
		s.statusMsg = err.Error()
		s.attrs = append(s.attrs, String("exception.message", err.Error()))
	}
	s.mu.Unlock()
}

// End completes the span and enqueues it for OTLP batch export.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	sd := SpanData{
		TraceID:       s.sc.TraceID,
		SpanID:        s.sc.SpanID,
		ParentSpanID:  s.parentSpanID,
		TraceState:    s.sc.TraceState,
		Name:          s.name,
		Kind:          s.kind,
		StartTime:     s.startTime,
		EndTime:       time.Now().UTC(),
		Attributes:    append([]Attribute(nil), s.attrs...),
		Status:        s.status,
		StatusMessage: s.statusMsg,
	}
	s.mu.Unlock()
	s.tracer.enqueue(sd)
}

type tracingRoundTripper struct {
	base     http.RoundTripper
	tracer   *Tracer
	spanName string
}

// NewHTTPTransport wraps an http.RoundTripper to create client spans and inject W3C traceparent headers.
func NewHTTPTransport(base http.RoundTripper, tracer *Tracer, spanName string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if spanName == "" {
		spanName = "http.client"
	}
	return &tracingRoundTripper{
		base:     base,
		tracer:   tracer,
		spanName: spanName,
	}
}

func (rt *tracingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := rt.tracer.Start(req.Context(), rt.spanName, SpanKindClient,
		String("http.request.method", req.Method),
		String("url.full", req.URL.String()),
	)
	cloned := req.Clone(ctx)
	InjectHTTP(ctx, cloned.Header)
	resp, err := rt.base.RoundTrip(cloned)
	if err != nil {
		span.RecordError(err)
		span.End()
		return nil, err
	}
	span.SetAttributes(Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 500 {
		span.SetStatus(StatusError, resp.Status)
	} else {
		span.SetStatus(StatusOK, "")
	}
	span.End()
	return resp, nil
}
