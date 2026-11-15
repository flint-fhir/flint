package txclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flint-fhir/flint/pkg/txclient"
)

func TestNewValidationAndDefaults(t *testing.T) {
	if _, err := txclient.New(txclient.Config{}); err == nil {
		t.Fatal("expected error for empty BaseURL")
	}
	if _, err := txclient.New(txclient.Config{BaseURL: "ftp://tx.example.org/r4"}); err == nil {
		t.Fatal("expected error for unsupported URL scheme")
	}
	if _, err := txclient.New(txclient.Config{BaseURL: "https://tx.example.org/r4", AuthHeader: "Bearer bad\r\nheader"}); err == nil {
		t.Fatal("expected error for CRLF in AuthHeader")
	}

	var nilClient *txclient.Client
	if nilClient.BaseURL() != "" || !nilClient.FailOpen() || nilClient.Timeout() != txclient.DefaultTimeout {
		t.Fatal("unexpected nil client receiver behavior")
	}
	nilClient.PurgeCache()
	if entries, hits, misses := nilClient.CacheStats(); entries != 0 || hits != 0 || misses != 0 {
		t.Fatal("expected zero cache stats on nil client")
	}

	cli, err := txclient.New(txclient.Config{
		BaseURL:    "https://tx.example.org/r4/",
		AuthHeader: "Bearer tok-123",
		FailOpen:   txclient.BoolPtr(false),
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if cli.BaseURL() != "https://tx.example.org/r4" {
		t.Fatalf("expected trimmed BaseURL, got %q", cli.BaseURL())
	}
	if cli.FailOpen() {
		t.Fatal("expected FailOpen=false")
	}
	if cli.Timeout() != txclient.DefaultTimeout {
		t.Fatalf("expected DefaultTimeout, got %v", cli.Timeout())
	}
}

func TestClientOperationsCachingAndSingleflight(t *testing.T) {
	var hitCount atomic.Int32
	var seenAPIKey atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount.Add(1)
		seenAPIKey.Store(r.Header.Get("X-Tx-Key"))
		w.Header().Set("Content-Type", "application/fhir+json")

		switch r.URL.Path {
		case "/r4/ValueSet/$expand":
			if r.Method == http.MethodPost {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"resourceType": "ValueSet",
					"status":       "active",
					"expansion": map[string]any{
						"total": 1,
						"contains": []map[string]any{
							{"system": "http://snomed.info/sct", "code": "195967001", "display": "Asthma"},
						},
					},
				})
				return
			}
			vsURL := r.URL.Query().Get("url")
			ver := r.URL.Query().Get("valueSetVersion")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "ValueSet",
				"url":          vsURL,
				"version":      ver,
				"status":       "active",
				"expansion": map[string]any{
					"total": 2,
					"contains": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "195967001", "display": "Asthma"},
						{"system": "http://snomed.info/sct", "code": "233604007", "display": "Pneumonia"},
					},
				},
			})

		case "/r4/ValueSet/$validate-code":
			code := r.URL.Query().Get("code")
			if r.Method == http.MethodPost {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				for _, p := range body["parameter"].([]any) {
					pm := p.(map[string]any)
					if pm["name"] == "code" {
						code, _ = pm["valueCode"].(string)
					}
				}
			}
			valid := code == "195967001"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "Parameters",
				"parameter": []map[string]any{
					{"name": "result", "valueBoolean": valid},
					{"name": "system", "valueUri": "http://snomed.info/sct"},
					{"name": "display", "valueString": "Asthma"},
					{"name": "message", "valueString": "validated upstream"},
				},
			})

		case "/r4/CodeSystem/$lookup":
			if r.URL.Query().Get("code") == "missing" {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"resourceType": "OperationOutcome",
					"issue": []map[string]any{
						{"severity": "error", "code": "not-found", "diagnostics": "Concept missing not found"},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "Parameters",
				"parameter": []map[string]any{
					{"name": "name", "valueString": "SNOMED_CT"},
					{"name": "display", "valueString": "Asthma"},
				},
			})

		case "/r4/CodeSystem/$validate-code":
			sysURL := r.URL.Query().Get("url")
			code := r.URL.Query().Get("code")
			if r.Method == http.MethodPost {
				sysURL = "http://snomed.info/sct"
				code = "195967001"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "Parameters",
				"parameter": []map[string]any{
					{"name": "result", "valueBoolean": sysURL == "http://snomed.info/sct" && code == "195967001"},
					{"name": "display", "valueString": "Asthma"},
				},
			})

		case "/r4/CodeSystem/$subsumes":
			outcome := "not-subsumed"
			if r.Method == http.MethodPost || (r.URL.Query().Get("codeA") == "19829001" && r.URL.Query().Get("codeB") == "195967001") {
				outcome = "subsumes"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "Parameters",
				"parameter": []map[string]any{
					{"name": "outcome", "valueCode": outcome},
				},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clockFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}

	cli, err := txclient.New(txclient.Config{
		BaseURL:         srv.URL + "/r4",
		Timeout:         2 * time.Second,
		CacheTTL:        5 * time.Minute,
		CacheMaxEntries: 4,
		AuthHeader:      "X-Tx-Key: super-secret",
		NowFunc:         clockFn,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 1. Concurrent singleflight + caching on ExpandValueSet (with canonical|version splitting)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vs, err := cli.ExpandValueSet(context.Background(), map[string]string{
				"url":    "http://example.org/ValueSet/respiratory|2.1.0",
				"count":  "50",
				"offset": "0",
			}, nil)
			if err != nil {
				t.Errorf("ExpandValueSet error: %v", err)
				return
			}
			if vs["version"] != "2.1.0" {
				t.Errorf("expected version 2.1.0, got %v", vs["version"])
			}
			// Mutate returned map to ensure deep-copy isolation in cache
			vs["mutated"] = true
		}()
	}
	wg.Wait()

	if seenAPIKey.Load() != "super-secret" {
		t.Fatalf("expected X-Tx-Key header forwarded, got %v", seenAPIKey.Load())
	}
	vsCached, err := cli.ExpandValueSet(context.Background(), map[string]string{
		"url":    "http://example.org/ValueSet/respiratory|2.1.0",
		"count":  "50",
		"offset": "0",
	}, nil)
	if err != nil {
		t.Fatalf("cached ExpandValueSet: %v", err)
	}
	if _, mutated := vsCached["mutated"]; mutated {
		t.Fatal("cache leaked caller map mutation")
	}
	if gotHits := hitCount.Load(); gotHits != 1 {
		t.Fatalf("expected exactly 1 upstream HTTP call after 17 identical ExpandValueSet calls, got %d", gotHits)
	}

	// 2. Inline POST ExpandValueSet
	inlineVS := map[string]any{
		"resourceType": "ValueSet",
		"status":       "active",
		"compose": map[string]any{
			"include": []any{map[string]any{"system": "http://snomed.info/sct"}},
		},
	}
	postVS, err := cli.ExpandValueSet(context.Background(), map[string]string{
		"activeOnly": "true",
		"count":      "10",
	}, inlineVS)
	if err != nil || postVS["resourceType"] != "ValueSet" {
		t.Fatalf("POST ExpandValueSet failed: %v", err)
	}

	// 3. ValidateValueSetCode (GET & POST) + ParseValidateCodeResult
	valMap, err := cli.ValidateValueSetCode(context.Background(), map[string]string{
		"url":    "http://example.org/ValueSet/respiratory",
		"system": "http://snomed.info/sct",
		"code":   "195967001",
	}, nil)
	if err != nil {
		t.Fatalf("ValidateValueSetCode GET: %v", err)
	}
	valid, disp, sys, msg, ok := txclient.ParseValidateCodeResult(valMap)
	if !ok || !valid || disp != "Asthma" || sys != "http://snomed.info/sct" || msg != "validated upstream" {
		t.Fatalf("unexpected ParseValidateCodeResult: valid=%v disp=%q sys=%q msg=%q ok=%v", valid, disp, sys, msg, ok)
	}
	if _, err := cli.ValidateValueSetCode(context.Background(), map[string]string{
		"code":     "195967001",
		"abstract": "false",
	}, inlineVS); err != nil {
		t.Fatalf("ValidateValueSetCode POST: %v", err)
	}

	// 4. LookupCode (success + 404 UpstreamError)
	lookMap, err := cli.LookupCode(context.Background(), map[string]string{
		"system": "http://snomed.info/sct",
		"code":   "195967001",
	})
	if err != nil || lookMap["resourceType"] != "Parameters" {
		t.Fatalf("LookupCode failed: %v", err)
	}
	_, err = cli.LookupCode(context.Background(), map[string]string{
		"system": "http://snomed.info/sct",
		"code":   "missing",
	})
	ue, isUE := txclient.AsUpstreamError(err)
	if !isUE || !ue.IsNotFound() || !ue.IsClientError() || ue.IsTransient() {
		t.Fatalf("expected 404 UpstreamError, got %v (ue=%+v)", err, ue)
	}

	// 5. ValidateCodeSystemCode (GET & POST) & Subsumes (GET & POST)
	csVal, err := cli.ValidateCodeSystemCode(context.Background(), map[string]string{
		"system":        "http://snomed.info/sct",
		"systemVersion": "20260301",
		"code":          "195967001",
	}, nil)
	if err != nil {
		t.Fatalf("ValidateCodeSystemCode GET: %v", err)
	}
	if v, _, _, _, ok := txclient.ParseValidateCodeResult(csVal); !ok || !v {
		t.Fatalf("expected valid=true from ValidateCodeSystemCode")
	}
	if _, err := cli.ValidateCodeSystemCode(context.Background(), map[string]string{"code": "195967001"}, map[string]any{"resourceType": "CodeSystem"}); err != nil {
		t.Fatalf("ValidateCodeSystemCode POST: %v", err)
	}

	subMap, err := cli.Subsumes(context.Background(), map[string]string{
		"url":   "http://snomed.info/sct",
		"codeA": "19829001",
		"codeB": "195967001",
	}, nil)
	if err != nil {
		t.Fatalf("Subsumes GET: %v", err)
	}
	if out, ok := txclient.ParseSubsumesOutcome(subMap); !ok || out != "subsumes" {
		t.Fatalf("expected outcome=subsumes, got %q (ok=%v)", out, ok)
	}
	if _, err := cli.Subsumes(context.Background(), map[string]string{"codeA": "19829001", "codeB": "195967001"}, map[string]any{"resourceType": "CodeSystem"}); err != nil {
		t.Fatalf("Subsumes POST: %v", err)
	}

	// Verify LRU capacity cap (maxEntries=4) and TTL expiration
	entries, hits, misses := cli.CacheStats()
	if entries > 4 || hits == 0 || misses == 0 {
		t.Fatalf("unexpected CacheStats: entries=%d hits=%d misses=%d", entries, hits, misses)
	}

	clockMu.Lock()
	now = now.Add(10 * time.Minute)
	clockMu.Unlock()

	beforeExpHits := hitCount.Load()
	if _, err := cli.Subsumes(context.Background(), map[string]string{
		"url":   "http://snomed.info/sct",
		"codeA": "19829001",
		"codeB": "195967001",
	}, nil); err != nil {
		t.Fatalf("Subsumes after TTL expiry: %v", err)
	}
	if hitCount.Load() <= beforeExpHits {
		t.Fatal("expected upstream refetch after TTL expiry")
	}

	cli.PurgeCache()
	if entries, _, _ := cli.CacheStats(); entries != 0 {
		t.Fatalf("expected 0 entries after PurgeCache, got %d", entries)
	}
}

func TestClientTransientAndMalformedUpstreamErrors(t *testing.T) {
	mode := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 0:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("service unavailable"))
		case 1:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{not-json"))
		case 2:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "OperationOutcome",
				"issue": []map[string]any{
					{"severity": "error", "code": "invalid", "details": map[string]any{"text": "bad filter"}},
				},
			})
		case 3:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"resourceType": "Patient"})
		case 4:
			time.Sleep(150 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	cli, err := txclient.New(txclient.Config{
		BaseURL:    srv.URL,
		Timeout:    40 * time.Millisecond,
		CacheTTL:   -1,
		AuthHeader: "Bearer tok-xyz",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Case 0: 503 transient
	_, err = cli.LookupCode(context.Background(), map[string]string{"system": "http://loinc.org", "code": "1234-5"})
	ue, ok := txclient.AsUpstreamError(err)
	if !ok || !ue.IsTransient() || ue.IsClientError() || ue.Error() == "" {
		t.Fatalf("expected transient 503 UpstreamError, got %v", err)
	}

	// Case 1: invalid JSON
	mode.Store(1)
	if _, err := cli.LookupCode(context.Background(), map[string]string{"system": "http://loinc.org", "code": "1234-5"}); err == nil {
		t.Fatal("expected error on invalid JSON")
	}

	// Case 2: 200 OperationOutcome
	mode.Store(2)
	_, err = cli.LookupCode(context.Background(), map[string]string{"system": "http://loinc.org", "code": "1234-5"})
	if ue, ok := txclient.AsUpstreamError(err); !ok || ue.Outcome == nil || ue.Message != "bad filter" {
		t.Fatalf("expected parsed OperationOutcome error, got %v", err)
	}

	// Case 3: unexpected resourceType
	mode.Store(3)
	if _, err := cli.LookupCode(context.Background(), map[string]string{"system": "http://loinc.org", "code": "1234-5"}); err == nil {
		t.Fatal("expected error on mismatched resourceType")
	}

	// Case 4: timeout -> transient
	mode.Store(4)
	_, err = cli.LookupCode(context.Background(), map[string]string{"system": "http://loinc.org", "code": "1234-5"})
	if ue, ok := txclient.AsUpstreamError(err); !ok || !ue.IsTransient() || ue.Unwrap() == nil {
		t.Fatalf("expected timeout transient UpstreamError, got %v", err)
	}
}
