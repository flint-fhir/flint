package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/flint-fhir/flint/store"
)

const idempotencyNamespacePrefix = "idempotency:"

type idempotencyStoredRecord struct {
	RequestHash  string    `json:"request_hash"`
	StatusCode   int       `json:"status_code"`
	ContentType  string    `json:"content_type,omitempty"`
	Location     string    `json:"location,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Body         []byte    `json:"body,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func parseIdempotencyTarget(r *http.Request) (tenant string, isIdempotentOp bool) {
	if r == nil || r.Method != http.MethodPost {
		return "", false
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "fhir" || parts[1] != "r4" {
		return "", false
	}
	t := parts[2]
	if t == "" || strings.HasPrefix(t, "$") || strings.HasPrefix(t, "_") || t == "cds-services" {
		return "", false
	}
	// POST /fhir/r4/{tenant} (Bundle) OR POST /fhir/r4/{tenant}/{resourceType} (Create)
	if len(parts) == 3 {
		return t, true
	}
	if len(parts) == 4 && !strings.HasPrefix(parts[3], "$") && !strings.HasPrefix(parts[3], "_") && parts[3] != "cds-services" {
		return t, true
	}
	return "", false
}

// idempotencyMiddleware enforces tenant-scoped Idempotency-Key / X-Idempotency-Key
// persistence across replicas via store.ClusterStateRecord.
func (s *Server) idempotencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, eligible := parseIdempotencyTarget(r)
		if !eligible {
			next.ServeHTTP(w, r)
			return
		}

		k1 := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		k2 := strings.TrimSpace(r.Header.Get("X-Idempotency-Key"))
		if k1 != "" && k2 != "" && k1 != k2 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				"conflicting Idempotency-Key and X-Idempotency-Key header values")
			return
		}
		idemKey := k1
		if idemKey == "" {
			idemKey = k2
		}
		if idemKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(idemKey) > 256 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "Idempotency-Key exceeds maximum length of 256 bytes")
			return
		}

		maxBytes := s.effectiveMaxBundleBytes()
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read request body")
			return
		}
		if int64(len(body)) > maxBytes {
			writeOperationOutcome(w, http.StatusRequestEntityTooLarge, "too-costly",
				fmt.Sprintf("request body exceeds configured limit of %d bytes", maxBytes))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		reqHash := computeIdempotencyRequestHash(r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("If-None-Exist"), body)
		keySum := sha256.Sum256([]byte(idemKey))
		keyHash := hex.EncodeToString(keySum[:])

		shard := &s.idemLocks[keySum[0]]
		shard.Lock()
		defer shard.Unlock()

		now := time.Now().UTC()
		if existing, found, err := s.loadIdempotencyRecord(r.Context(), tenant, keyHash, now); err == nil && found {
			if existing.RequestHash != reqHash {
				writeOperationOutcome(w, http.StatusConflict, "conflict",
					"Idempotency-Key was already used with a different request payload")
				return
			}
			writeReplayedIdempotencyResponse(w, existing)
			return
		}

		cw := &idempotencyCaptureWriter{
			header:     make(http.Header),
			statusCode: http.StatusOK,
		}
		next.ServeHTTP(cw, r)

		// Persist successful 2xx responses across cluster replicas
		if cw.statusCode >= 200 && cw.statusCode < 300 {
			exp := now.Add(s.effectiveIdempotencyTTL())
			rec := idempotencyStoredRecord{
				RequestHash:  reqHash,
				StatusCode:   cw.statusCode,
				ContentType:  cw.header.Get("Content-Type"),
				Location:     cw.header.Get("Location"),
				ETag:         cw.header.Get("ETag"),
				LastModified: cw.header.Get("Last-Modified"),
				Body:         cw.buf.Bytes(),
				CreatedAt:    now,
				ExpiresAt:    exp,
			}
			if saveErr := s.saveIdempotencyRecord(r.Context(), tenant, keyHash, rec); saveErr != nil {
				s.logger.Warn("failed to persist idempotency record", "error", saveErr, "tenant", tenant)
			}
		}

		copyHeaderMap(w.Header(), cw.header)
		w.WriteHeader(cw.statusCode)
		_, _ = w.Write(cw.buf.Bytes())
	})
}

func computeIdempotencyRequestHash(method, canonicalPath, rawQuery, ifNoneExist string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{'\n'})
	h.Write([]byte(canonicalPath))
	h.Write([]byte{'\n'})
	h.Write([]byte(rawQuery))
	h.Write([]byte{'\n'})
	h.Write([]byte(strings.TrimSpace(ifNoneExist)))
	h.Write([]byte{'\n'})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) loadIdempotencyRecord(ctx context.Context, tenant, keyHash string, now time.Time) (idempotencyStoredRecord, bool, error) {
	ns := idempotencyNamespacePrefix + tenant
	var raw []byte
	if s.store != nil {
		cs, ok, err := s.store.GetClusterState(ctx, ns, keyHash)
		if err != nil {
			return idempotencyStoredRecord{}, false, err
		}
		if !ok {
			return idempotencyStoredRecord{}, false, nil
		}
		if cs.ExpiresAt != nil && now.After(*cs.ExpiresAt) {
			return idempotencyStoredRecord{}, false, nil
		}
		raw = cs.Value
	} else {
		s.idemMem.mu.RLock()
		val, ok := s.idemMem.entries[ns+"|"+keyHash]
		s.idemMem.mu.RUnlock()
		if !ok {
			return idempotencyStoredRecord{}, false, nil
		}
		raw = val
	}

	var rec idempotencyStoredRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return idempotencyStoredRecord{}, false, err
	}
	if !rec.ExpiresAt.IsZero() && now.After(rec.ExpiresAt) {
		return idempotencyStoredRecord{}, false, nil
	}
	return rec, true, nil
}

func (s *Server) saveIdempotencyRecord(ctx context.Context, tenant, keyHash string, rec idempotencyStoredRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	ns := idempotencyNamespacePrefix + tenant
	if s.store != nil {
		exp := rec.ExpiresAt
		_, err := s.store.PutClusterState(ctx, store.ClusterStateRecord{
			Namespace: ns,
			Key:       keyHash,
			TenantID:  tenant,
			Value:     raw,
			ExpiresAt: &exp,
		})
		return err
	}
	s.idemMem.mu.Lock()
	if s.idemMem.entries == nil {
		s.idemMem.entries = make(map[string][]byte)
	}
	s.idemMem.entries[ns+"|"+keyHash] = raw
	s.idemMem.mu.Unlock()
	return nil
}

func writeReplayedIdempotencyResponse(w http.ResponseWriter, rec idempotencyStoredRecord) {
	if rec.ContentType != "" {
		w.Header().Set("Content-Type", rec.ContentType)
	}
	if rec.Location != "" {
		w.Header().Set("Location", rec.Location)
	}
	if rec.ETag != "" {
		w.Header().Set("ETag", rec.ETag)
	}
	if rec.LastModified != "" {
		w.Header().Set("Last-Modified", rec.LastModified)
	}
	w.Header().Set("X-Idempotency-Replayed", "true")
	w.WriteHeader(rec.StatusCode)
	_, _ = w.Write(rec.Body)
}

func copyHeaderMap(dst, src http.Header) {
	for k, vals := range src {
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

type idempotencyCaptureWriter struct {
	header      http.Header
	buf         bytes.Buffer
	statusCode  int
	wroteHeader bool
}

func (w *idempotencyCaptureWriter) Header() http.Header {
	return w.header
}

func (w *idempotencyCaptureWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
	}
}

func (w *idempotencyCaptureWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.statusCode = http.StatusOK
		w.wroteHeader = true
	}
	return w.buf.Write(b)
}
