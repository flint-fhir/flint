package server

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxRateLimitBuckets = 10000

type tenantTokenBucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
}

type tenantRateLimiter struct {
	rps   float64
	burst float64

	mu      sync.Mutex
	buckets map[string]*tenantTokenBucket
}

func newTenantRateLimiter(rps float64, burst int) *tenantRateLimiter {
	if rps <= 0 {
		return nil
	}
	b := burst
	if b <= 0 {
		b = max(1, int(math.Ceil(rps)))
	}
	return &tenantRateLimiter{
		rps:     rps,
		burst:   float64(b),
		buckets: make(map[string]*tenantTokenBucket),
	}
}

func (l *tenantRateLimiter) allow(tenant string, now time.Time) (bool, time.Duration) {
	if l == nil || l.rps <= 0 {
		return true, 0
	}
	if tenant == "" {
		tenant = "default"
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, ok := l.buckets[tenant]
	if !ok {
		if len(l.buckets) >= maxRateLimitBuckets {
			l.evictStaleLocked(now)
		}
		bucket = &tenantTokenBucket{
			tokens:     l.burst,
			lastRefill: now,
			lastSeen:   now,
		}
		l.buckets[tenant] = bucket
	}

	elapsed := now.Sub(bucket.lastRefill).Seconds()
	if elapsed > 0 {
		bucket.tokens = min(l.burst, bucket.tokens+elapsed*l.rps)
		bucket.lastRefill = now
	}
	bucket.lastSeen = now

	if bucket.tokens >= 1.0 {
		bucket.tokens -= 1.0
		return true, 0
	}

	deficit := 1.0 - bucket.tokens
	waitSecs := deficit / l.rps
	wait := time.Duration(math.Ceil(waitSecs) * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

func (l *tenantRateLimiter) evictStaleLocked(now time.Time) {
	cutoff := now.Add(-10 * time.Minute)
	for k, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
	// If still at cap, clear oldest one so map stays bounded.
	if len(l.buckets) >= maxRateLimitBuckets {
		var oldestKey string
		var oldestTime time.Time
		for k, b := range l.buckets {
			if oldestKey == "" || b.lastSeen.Before(oldestTime) {
				oldestKey = k
				oldestTime = b.lastSeen
			}
		}
		if oldestKey != "" {
			delete(l.buckets, oldestKey)
		}
	}
}

// rateLimitMiddleware enforces per-tenant token-bucket rate limiting when enabled in SecurityPolicy.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, limiter := s.getCompiledPolicy()
		if limiter == nil {
			next.ServeHTTP(w, r)
			return
		}

		path := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(path, "/")
		if isPublicEndpoint(parts) || isOperationalHealthPath(path, parts) {
			next.ServeHTTP(w, r)
			return
		}

		tenant := "default"
		if len(parts) >= 3 && parts[0] == "fhir" && parts[1] == "r4" && !strings.HasPrefix(parts[2], "$") && parts[2] != "cds-services" {
			tenant = parts[2]
		}

		allowed, retryAfter := limiter.allow(tenant, time.Now())
		if !allowed {
			secs := max(1, int(math.Ceil(retryAfter.Seconds())))
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeOperationOutcome(w, http.StatusTooManyRequests, "throttled",
				fmt.Sprintf("rate limit exceeded for tenant %s; retry after %ds", tenant, secs))
			return
		}

		next.ServeHTTP(w, r)
	})
}

func isOperationalHealthPath(path string, parts []string) bool {
	if path == "healthz" || path == "readyz" || path == "metrics" {
		return true
	}
	if len(parts) == 3 && parts[0] == "fhir" && parts[1] == "r4" {
		return parts[2] == "healthz" || parts[2] == "readyz" || parts[2] == "metrics"
	}
	if len(parts) == 4 && parts[0] == "fhir" && parts[1] == "r4" {
		return parts[3] == "healthz" || parts[3] == "readyz" || parts[3] == "metrics"
	}
	return false
}
