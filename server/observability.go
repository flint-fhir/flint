package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type serverMetrics struct {
	mu                              sync.Mutex
	requestsByMethodStatus          map[string]uint64
	requestCountTotal               atomic.Uint64
	durationMicrosTotal             atomic.Uint64
	cdcPublishedTotal               atomic.Uint64
	cdcPublishErrorsTotal           atomic.Uint64
	subscriptionsDeliveredTotal     atomic.Uint64
	subscriptionDeliveryErrorsTotal atomic.Uint64
}

func newServerMetrics() *serverMetrics {
	return &serverMetrics{
		requestsByMethodStatus: make(map[string]uint64),
	}
}

func (m *serverMetrics) recordRequest(method string, status int, dur time.Duration) {
	if m == nil {
		return
	}
	m.requestCountTotal.Add(1)
	if micros := dur.Microseconds(); micros > 0 {
		m.durationMicrosTotal.Add(uint64(micros))
	}
	key := method + ":" + strconv.Itoa(status)
	m.mu.Lock()
	m.requestsByMethodStatus[key]++
	m.mu.Unlock()
}

// metricsMiddleware instruments HTTP requests with Prometheus-compatible request counters and durations.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		mrw := &auditResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(mrw, r)
		if s.metrics != nil {
			s.metrics.recordRequest(r.Method, mrw.statusCode, time.Since(start))
		}
	})
}

// handleHealthz serves GET /healthz and GET /fhir/r4/{tenant}/healthz liveness probes.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"service":     "flintd",
		"fhirVersion": "4.0.1",
	})
}

// handleReadyz serves GET /readyz and GET /fhir/r4/{tenant}/readyz readiness probes.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if s.store != nil {
		if err := s.store.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "not_ready",
				"store":  "error",
				"error":  err.Error(),
			})
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":                  "ready",
		"store":                   "ok",
		"registeredResourceTypes": len(s.protoRegistry),
	})
}

// handleMetrics serves GET /metrics and GET /fhir/r4/{tenant}/metrics in Prometheus exposition format.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder

	b.WriteString("# HELP flint_up Whether the Flint FHIR R4 server is up (1 = up).\n")
	b.WriteString("# TYPE flint_up gauge\n")
	b.WriteString("flint_up 1\n")

	b.WriteString("# HELP flint_registered_resource_types Total number of registered FHIR R4 Protobuf resource types.\n")
	b.WriteString("# TYPE flint_registered_resource_types gauge\n")
	fmt.Fprintf(&b, "flint_registered_resource_types %d\n", len(s.protoRegistry))

	s.exportMu.RLock()
	inMemJobs := len(s.exportJobs)
	s.exportMu.RUnlock()
	b.WriteString("# HELP flint_bulk_export_jobs_in_memory Number of Bulk Data ($export) jobs cached in memory.\n")
	b.WriteString("# TYPE flint_bulk_export_jobs_in_memory gauge\n")
	fmt.Fprintf(&b, "flint_bulk_export_jobs_in_memory %d\n", inMemJobs)

	var (
		reqCount       uint64
		durMicros      uint64
		cdcPub         uint64
		cdcErr         uint64
		subDelivered   uint64
		subDeliveryErr uint64
		methodStatuses []string
		countsByKey    map[string]uint64
	)
	if s.metrics != nil {
		reqCount = s.metrics.requestCountTotal.Load()
		durMicros = s.metrics.durationMicrosTotal.Load()
		cdcPub = s.metrics.cdcPublishedTotal.Load()
		cdcErr = s.metrics.cdcPublishErrorsTotal.Load()
		subDelivered = s.metrics.subscriptionsDeliveredTotal.Load()
		subDeliveryErr = s.metrics.subscriptionDeliveryErrorsTotal.Load()

		s.metrics.mu.Lock()
		countsByKey = make(map[string]uint64, len(s.metrics.requestsByMethodStatus))
		for k, v := range s.metrics.requestsByMethodStatus {
			countsByKey[k] = v
			methodStatuses = append(methodStatuses, k)
		}
		s.metrics.mu.Unlock()
		sort.Strings(methodStatuses)
	}

	b.WriteString("# HELP flint_http_requests_total Total number of HTTP requests processed by method and status.\n")
	b.WriteString("# TYPE flint_http_requests_total counter\n")
	for _, k := range methodStatuses {
		parts := strings.SplitN(k, ":", 2)
		if len(parts) == 2 {
			fmt.Fprintf(&b, "flint_http_requests_total{method=%q,status=%q} %d\n", parts[0], parts[1], countsByKey[k])
		}
	}

	b.WriteString("# HELP flint_http_request_duration_seconds_sum Total time spent serving HTTP requests in seconds.\n")
	b.WriteString("# TYPE flint_http_request_duration_seconds_sum counter\n")
	fmt.Fprintf(&b, "flint_http_request_duration_seconds_sum %.6f\n", float64(durMicros)/1e6)

	b.WriteString("# HELP flint_http_request_duration_seconds_count Total number of timed HTTP requests.\n")
	b.WriteString("# TYPE flint_http_request_duration_seconds_count counter\n")
	fmt.Fprintf(&b, "flint_http_request_duration_seconds_count %d\n", reqCount)

	b.WriteString("# HELP flint_cdc_events_published_total Total number of FHIR CDC events published to AutoMQ.\n")
	b.WriteString("# TYPE flint_cdc_events_published_total counter\n")
	fmt.Fprintf(&b, "flint_cdc_events_published_total %d\n", cdcPub)

	b.WriteString("# HELP flint_cdc_publish_errors_total Total number of AutoMQ CDC publish errors.\n")
	b.WriteString("# TYPE flint_cdc_publish_errors_total counter\n")
	fmt.Fprintf(&b, "flint_cdc_publish_errors_total %d\n", cdcErr)

	b.WriteString("# HELP flint_subscriptions_delivered_total Total number of FHIR Subscription rest-hook notifications delivered.\n")
	b.WriteString("# TYPE flint_subscriptions_delivered_total counter\n")
	fmt.Fprintf(&b, "flint_subscriptions_delivered_total %d\n", subDelivered)

	b.WriteString("# HELP flint_subscription_delivery_errors_total Total number of FHIR Subscription rest-hook delivery errors.\n")
	b.WriteString("# TYPE flint_subscription_delivery_errors_total counter\n")
	fmt.Fprintf(&b, "flint_subscription_delivery_errors_total %d\n", subDeliveryErr)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
