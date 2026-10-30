package certify

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReport_ToFHIRTestReportAndFormatScorecard(t *testing.T) {
	issued := time.Date(2026, 10, 29, 18, 0, 0, 0, time.UTC)
	rep := &Report{
		Tenant:      "test-tenant",
		BaseURL:     "http://localhost:8080/",
		Issued:      issued,
		TotalChecks: 3,
		Passed:      2,
		Failed:      1,
		Score:       66.7,
		Result:      "fail",
		Checks: []CheckResult{
			{
				ID:         "FHIR-01",
				Suite:      "Suite 1: Core",
				Name:       "CapabilityStatement Check",
				Status:     "pass",
				Message:    "OK",
				DurationMs: 12,
			},
			{
				ID:         "FHIR-02",
				Suite:      "Suite 1: Core",
				Name:       "Patient CRUD Check",
				Status:     "fail",
				Message:    "expected 200, got 500",
				DurationMs: 8,
			},
			{
				ID:         "SEC-01",
				Suite:      "Suite 2: Security",
				Name:       "SMART Discovery Check",
				Status:     "pass",
				Message:    "Verified",
				DurationMs: 5,
			},
		},
	}

	tr := rep.ToFHIRTestReport()
	if tr["resourceType"] != "TestReport" {
		t.Fatalf("expected resourceType TestReport, got %v", tr["resourceType"])
	}
	if tr["result"] != "fail" {
		t.Fatalf("expected result fail, got %v", tr["result"])
	}
	tests, ok := tr["test"].([]map[string]any)
	if !ok || len(tests) != 2 {
		t.Fatalf("expected 2 suites in TestReport.test, got %#v", tr["test"])
	}

	scorecard := rep.FormatScorecard()
	if !strings.Contains(scorecard, "FAIL (66.7% — 2/3 checks passed)") {
		t.Fatalf("expected overall fail summary in scorecard, got:\n%s", scorecard)
	}
	if !strings.Contains(scorecard, "[FAIL] FHIR-02") || !strings.Contains(scorecard, "expected 200, got 500") {
		t.Fatalf("expected failure diagnostic line in scorecard, got:\n%s", scorecard)
	}
	if !strings.Contains(scorecard, "[PASS] SEC-01") {
		t.Fatalf("expected pass line in scorecard, got:\n%s", scorecard)
	}
}

func TestDefaultCertifyCohortBundleAndES384Assertion(t *testing.T) {
	b := defaultCertifyCohortBundle()
	var bundle map[string]any
	if err := json.Unmarshal(b, &bundle); err != nil {
		t.Fatalf("defaultCertifyCohortBundle invalid JSON: %v", err)
	}
	if bundle["resourceType"] != "Bundle" || bundle["type"] != "transaction" {
		t.Fatalf("unexpected bundle header: %v", bundle)
	}
	entries, _ := bundle["entry"].([]any)
	if len(entries) < 10 {
		t.Fatalf("expected at least 10 cohort entries, got %d", len(entries))
	}

	jwtStr, err := signES384ClientAssertion("bulk-client", "http://localhost:8080/oauth2/token", "jti-123")
	if err != nil {
		t.Fatalf("signES384ClientAssertion failed: %v", err)
	}
	parts := strings.Split(jwtStr, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}

	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if hdr["alg"] != "ES384" {
		t.Fatalf("expected alg ES384, got %v", hdr["alg"])
	}
	jwk, _ := hdr["jwk"].(map[string]any)
	xBytes, _ := base64.RawURLEncoding.DecodeString(jwk["x"].(string))
	yBytes, _ := base64.RawURLEncoding.DecodeString(jwk["y"].(string))
	uncompressed := make([]byte, 1+48+48)
	uncompressed[0] = 0x04
	copy(uncompressed[1+48-len(xBytes):1+48], xBytes)
	copy(uncompressed[1+96-len(yBytes):1+96], yBytes)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P384(), uncompressed)
	if err != nil {
		t.Fatalf("ParseUncompressedPublicKey failed: %v", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sigBytes) != 96 {
		t.Fatalf("expected 96-byte ES384 signature, got %d (%v)", len(sigBytes), err)
	}
	rInt := new(big.Int).SetBytes(sigBytes[:48])
	sInt := new(big.Int).SetBytes(sigBytes[48:])
	digest := sha512.Sum384([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], rInt, sInt) {
		t.Fatalf("ES384 signature verification failed")
	}
}

func TestRun_HandlesServerErrorGracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome"}`))
	}))
	defer srv.Close()

	rep, err := Run(t.Context(), Config{
		BaseURL:     srv.URL,
		Tenant:      "fail-tenant",
		BearerToken: "test-token",
	})
	if err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	if rep.Result != "fail" || rep.Failed == 0 {
		t.Fatalf("expected failing Report against 500 server, got result=%s failed=%d", rep.Result, rep.Failed)
	}
}
