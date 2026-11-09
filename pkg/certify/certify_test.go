package certify

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestRun_FullMockPassAll50Checks(t *testing.T) {
	var (
		mu          sync.Mutex
		condCreates int
		orgDeleted  bool
		metaDeleted bool
		revokedTok  bool
		seenJTI     = make(map[string]bool)
		serverURL   string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		path := r.URL.Path
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/fhir+json")
		if pou := r.Header.Get("X-Purpose-Of-Use"); pou != "" {
			w.Header().Set("X-TEFCA-Exchange-Purpose", pou)
		}

		// OAuth2 endpoints
		switch path {
		case "/oauth2/authorize":
			redir := q.Get("redirect_uri")
			state := q.Get("state")
			w.Header().Set("Location", fmt.Sprintf("%s?code=auth-code-123&state=%s", redir, state))
			w.WriteHeader(http.StatusFound)
			return
		case "/oauth2/token":
			bodyBytes, _ := io.ReadAll(r.Body)
			bodyStr := string(bodyBytes)
			if strings.Contains(bodyStr, "client_assertion=") {
				if seenJTI["es384"] {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
					return
				}
				seenJTI["es384"] = true
			}
			tok := "tok-pkce"
			if strings.Contains(bodyStr, "vital-signs") {
				tok = "tok-gran"
			} else if strings.Contains(bodyStr, "client_credentials") {
				tok = "tok-cc"
			}
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"patient":"cert-pat-1","id_token":"eyJ.id.tok","token_type":"Bearer"}`, tok)
			return
		case "/oauth2/introspect":
			_, _ = w.Write([]byte(`{"active":true,"scope":"system/*.rs"}`))
			return
		case "/oauth2/revoke":
			revokedTok = true
			w.WriteHeader(http.StatusOK)
			return
		}

		// Enforce SMART-01 & SMART-03 auth checks
		authHdr := r.Header.Get("Authorization")
		if authHdr == "Bearer tok-pkce" {
			if strings.HasSuffix(path, "/Patient/other-patient-999") || r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		if authHdr == "Bearer tok-gran" {
			if revokedTok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if q.Get("category") == "social-history" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}

		// Suite 1: Discovery
		switch {
		case strings.HasSuffix(path, "/metadata"):
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x6d, 0x65, 0x74, 0x61})
				return
			}
			_, _ = w.Write([]byte(`{
				"resourceType":"CapabilityStatement",
				"fhirVersion":"4.0.1",
				"instantiates":["http://hl7.org/fhir/us/core/CapabilityStatement/us-core-server"],
				"extensions":[
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-vital-signs",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-lab",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-average-blood-pressure",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-care-experience-preference",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-treatment-intervention-preference",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medicationrequest",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-specimen",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-location",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-documentreference",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-provenance",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-careplan",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-careteam",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-goal",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-servicerequest",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-coverage",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-questionnaireresponse",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medicationdispense",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medication",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-implantable-device",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitionerrole",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-relatedperson",
					"http://fhir-registry.smarthealthit.org/StructureDefinition/oauth-uris",
					"http://hl7.org/fhir/CompartmentDefinition/patient",
					"http://hl7.org/fhir/CompartmentDefinition/encounter",
					"http://hl7.org/fhir/CompartmentDefinition/practitioner",
					"http://hl7.org/fhir/CompartmentDefinition/relatedPerson",
					"http://hl7.org/fhir/CompartmentDefinition/device"
				]
			}`))
			return
		case strings.HasSuffix(path, "/.well-known/smart-configuration"):
			_, _ = w.Write([]byte(`{"capabilities":["launch-ehr","client-public"]}`))
			return
		case strings.HasSuffix(path, "/.well-known/openid-configuration"):
			_, _ = w.Write([]byte(`{"issuer":"http://localhost"}`))
			return
		case strings.HasSuffix(path, "/.well-known/jwks.json"):
			_, _ = w.Write([]byte(`{"keys":[{"alg":"RS256","kty":"RSA"}]}`))
			return
		case strings.HasSuffix(path, "/.well-known/udap"):
			_, _ = w.Write([]byte(`{"udap_versions_supported":["1.0"],"udap_profiles_supported":["udap_dcr"]}`))
			return
		case strings.HasSuffix(path, "/openapi.json"):
			_, _ = w.Write([]byte(`{"openapi":"3.1.0"}`))
			return
		case strings.HasSuffix(path, "/$docs"):
			_, _ = w.Write([]byte(`<html><body>Conformance Matrix</body></html>`))
			return
		}

		// Suite 3: Organization Conditional CRUD & Patch
		if strings.Contains(path, "/Organization") && !strings.Contains(path, "$ndh-directory-summary") && q.Get("name") == "" {
			if r.Method == http.MethodPost && r.Header.Get("If-None-Exist") != "" {
				condCreates++
				w.Header().Set("ETag", `W/"1"`)
				w.Header().Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
				if condCreates == 1 {
					w.WriteHeader(http.StatusCreated)
				} else {
					w.WriteHeader(http.StatusOK)
				}
				_, _ = w.Write([]byte(`{"resourceType":"Organization","id":"cond-org-1"}`))
				return
			}
			if r.Method == http.MethodGet && strings.HasSuffix(path, "/Organization/cond-org-1") {
				if orgDeleted {
					w.WriteHeader(http.StatusGone)
					return
				}
				if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
			if r.Method == http.MethodPut {
				w.Header().Set("ETag", `W/"2"`)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"resourceType":"Organization","id":"cond-org-1"}`))
				return
			}
			if r.Method == http.MethodPatch {
				if r.Header.Get("If-Match") == `W/"1"` {
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"resourceType":"Organization","id":"cond-org-1","name":"Conditional Cert Org FHIRPatched"}`))
				return
			}
			if r.Method == http.MethodGet && strings.HasSuffix(path, "/_history") {
				_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"history","total":4}`))
				return
			}
			if r.Method == http.MethodGet && strings.Contains(path, "/_history/1") {
				_, _ = w.Write([]byte(`{"resourceType":"Organization","id":"cond-org-1","meta":{"tag":[{"code":"SUBSETTED"}]}}`))
				return
			}
			if r.Method == http.MethodDelete {
				if r.Header.Get("If-Match") == `W/"1"` {
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				orgDeleted = true
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		// Suite 3: $meta-add / $meta / $meta-delete
		if strings.HasSuffix(path, "/$meta-add") {
			metaDeleted = false
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x6d, 0x65, 0x74, 0x61})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"return","valueMeta":{"tag":[{"code":"vip-cohort"}]}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$meta-delete") {
			metaDeleted = true
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x6d, 0x65, 0x74, 0x61})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"return","valueMeta":{}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$meta") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x6d, 0x65, 0x74, 0x61})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"return","valueMeta":{"tag":[{"code":"vip-cohort"}]}}]}`))
			return
		}

		// Suite 4: $everything & $export
		if strings.HasSuffix(path, "/$everything") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x0c, 0x0a, 0x0a, 0x63, 0x65, 0x72, 0x74, 0x2d, 0x70, 0x61, 0x74, 0x2d, 0x31})
				return
			}
			if strings.Contains(path, "/Encounter") {
				_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","total":2,"entry":[{"fullUrl":"Encounter/cert-enc-1","resource":{"resourceType":"Encounter","id":"cert-enc-1"}},{"fullUrl":"Location/cert-loc-1"}]}`))
				return
			}
			if strings.Contains(path, "/Group/all") {
				_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","total":2,"entry":[{"fullUrl":"Patient/cert-pat-1","resource":{"resourceType":"Patient","id":"cert-pat-1","meta":{"tag":[{"code":"SUBSETTED"}]}}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","total":26,"link":[{"relation":"next","url":"http://next"}],"entry":[{"resource":{"resourceType":"Observation","id":"cert-obs-1"}}]}`))
			return
		}
		if !strings.Contains(path, "/ViewDefinition") && (strings.HasSuffix(path, "/Group/all/$export") || strings.HasSuffix(path, "/$export")) {
			w.Header().Set("Content-Location", serverURL+"/fhir/r4/certify/$export-poll/job-1")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.Contains(path, "/$export-poll/") {
			var outs []map[string]string
			for i := 0; i < 20; i++ {
				outs = append(outs, map[string]string{
					"type": "Patient",
					"url":  fmt.Sprintf("%s/fhir/r4/certify/$export-file/job-1/Patient-%d.ndjson", serverURL, i),
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"output": outs})
			return
		}
		if strings.Contains(path, "/$export-file/") {
			_, _ = w.Write([]byte(`{"resourceType":"Patient","id":"cert-pat-1","active":true,"gender":"female","name":[{"family":"Certify","given":["Alice"]}]}` + "\n"))
			return
		}
		if strings.HasSuffix(path, "/$import") {
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"diagnostics":"Successfully imported 1 resource"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$purge") || strings.HasSuffix(path, "/$expunge") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x70, 0x75, 0x72, 0x67})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome"}`))
			return
		}

		// Suite 5: Validation, Terminology, Subscriptions, CQM, CDS, IPS, Da Vinci, SDC, Public Health, AuditEvent
		if strings.HasSuffix(path, "/$validate") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				w.Header().Set("X-Flint-Storage-Model", "zero-json-proto3")
				_, _ = w.Write([]byte{0x4a, 0x04, 0x12, 0x02, 0x08, 0x04})
				return
			}
			if strings.HasSuffix(path, "/Observation/$validate") {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invariant","diagnostics":"Observation violated invariant obs-6"}]}`))
				return
			}
			if strings.HasSuffix(path, "/Patient/$validate") && r.Method == http.MethodPost {
				bodyBytes, _ := io.ReadAll(r.Body)
				bodyStr := string(bodyBytes)
				if q.Get("profile") == "http://flint.fhir.org/StructureDefinition/cert-custom-patient-profile" {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invariant","diagnostics":"Profile constraint cp-1 failed and birthDate min=1"}]}`))
					return
				}
				if strings.Contains(bodyStr, `"contact"`) {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invariant","diagnostics":"Patient.contact violated invariant pat-1"}]}`))
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome"}`))
				return
			}
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome"}`))
			return
		}
		if strings.HasSuffix(path, "/$convert") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x0c, 0x0a, 0x0a, 0x63, 0x65, 0x72, 0x74, 0x2d, 0x63, 0x6f, 0x6e, 0x76, 0x31})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Patient","id":"conv-pat-1","name":[{"family":"CertifyProto"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$expand") || strings.HasSuffix(path, "/$validate-code") || strings.HasSuffix(path, "/$lookup") || strings.HasSuffix(path, "/$subsumes") || strings.HasSuffix(path, "/$find-matches") || strings.HasSuffix(path, "/$translate") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				w.Header().Set("X-Flint-Storage-Model", "zero-json-proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x74, 0x65, 0x72, 0x6d})
				return
			}
		}
		if strings.HasSuffix(path, "/ValueSet/$expand") {
			_, _ = w.Write([]byte(`{"resourceType":"ValueSet","expansion":{"contains":[{"code":"8867-4","display":"Heart rate"}]}}`))
			return
		}
		if strings.HasSuffix(path, "/$validate-code") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"result","valueBoolean":true}]}`))
			return
		}
		if strings.HasSuffix(path, "/CodeSystem/$lookup") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"display","valueString":"Heart rate"}]}`))
			return
		}
		if strings.HasSuffix(path, "/CodeSystem/$subsumes") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"outcome","valueCode":"subsumes"}]}`))
			return
		}
		if strings.HasSuffix(path, "/CodeSystem/$find-matches") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x66, 0x6d, 0x74, 0x63})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"match","part":[{"name":"code","valueCode":"59621000"}]}]}`))
			return
		}
		if strings.HasSuffix(path, "/ConceptMap/$translate") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"match","part":[{"name":"concept","valueCoding":{"code":"I10"}},{"name":"source","valueString":"38341003"}]}]}`))
			return
		}
		if strings.Contains(path, "/Subscription") {
			if strings.HasSuffix(path, "/$status") || strings.HasSuffix(path, "/$events") {
				if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
					w.Header().Set("Content-Type", "application/x-protobuf")
					w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
					_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x73, 0x75, 0x62, 0x31})
					return
				}
				_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"status","valueCode":"active"}],"type":"query-status"}`))
				return
			}
			if r.Method == http.MethodPost {
				b, _ := io.ReadAll(r.Body)
				if strings.Contains(string(b), "169.254.169.254") {
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"resourceType":"Subscription","id":"sub-1","status":"active"}`))
				return
			}
			if r.Method == http.MethodPut {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"resourceType":"Subscription","id":"cert-sub-proto-1","status":"active"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(path, "/$evaluate-measure") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x65, 0x76, 0x61, 0x6c})
				return
			}
			if q.Get("reportType") == "subject-list" {
				_, _ = w.Write([]byte(`{"resourceType":"MeasureReport","status":"complete","type":"subject-list","contained":[{"id":"numerator-list"}],"group":[{"population":[{"subjectResults":{"reference":"#numerator-list"}}]}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"MeasureReport","status":"complete","type":"individual","group":[{"measureScore":{"value":1.0}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$care-gaps") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x63, 0x67, 0x61, 0x70})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"document","entry":[{"resource":{"resourceType":"Composition","type":{"coding":[{"code":"96315-7"}]}}},{"resource":{"resourceType":"DetectedIssue","code":{"coding":[{"code":"closed-gap"}]}}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$collect-data") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x63, 0x6f, 0x6c, 0x6c})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"measureReport","resource":{"resourceType":"MeasureReport","id":"cert-pat-1","type":"data-collection","subject":{"reference":"Patient/cert-pat-1"}}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$submit-data") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x73, 0x75, 0x62, 0x6d})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"transaction-response"}`))
			return
		}
		if strings.HasSuffix(path, "/$package") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x70, 0x6b, 0x67, 0x31})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"collection"}`))
			return
		}
		if strings.HasSuffix(path, "/cds-services") {
			_, _ = w.Write([]byte(`{"services":[{"id":"s1"},{"id":"s2"},{"id":"s3"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$dsi-attributes") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x64, 0x73, 0x69, 0x31})
				return
			}
			_, _ = w.Write([]byte(`{"serviceId":"flint-hypertension-care-gap","regulatoryCriterion":"45 CFR §170.315(b)(11)","validationSummary":{"sensitivity":0.988}}`))
			return
		}
		if strings.HasSuffix(path, "/feedback") {
			_, _ = w.Write([]byte(`{"status":"recorded","recordedCount":1}`))
			return
		}
		if strings.Contains(path, "/cds-services/") {
			_, _ = w.Write([]byte(`{"cards":[{"indicator":"critical","summary":"Allergy conflict"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$summary") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x69, 0x70, 0x73, 0x31})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"document","entry":[{"resource":{"resourceType":"Composition","type":{"coding":[{"code":"60591-5"}]}}},{},{},{},{}]}`))
			return
		}
		if strings.HasSuffix(path, "/$member-match") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x6d, 0x6d, 0x74, 0x63})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"MemberIdentifier","valueString":"Patient/cert-pat-1"}]}`))
			return
		}
		if strings.HasSuffix(path, "/Claim/$submit") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x63, 0x6c, 0x6d, 0x31})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"ClaimResponse","outcome":"complete"}`))
			return
		}
		if strings.HasSuffix(path, "/$populate") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x70, 0x6f, 0x70, 0x31})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"QuestionnaireResponse","subject":{"display":"Alice Certify"}}`))
			return
		}
		if strings.HasSuffix(path, "/$extract") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x65, 0x78, 0x74, 0x31})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"transaction","entry":[{"resource":{"resourceType":"Observation"}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$document") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x64, 0x6f, 0x63, 0x31})
				return
			}
			w.Header().Set("Location", "/fhir/r4/certify/Bundle/doc-1")
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"document","entry":[{"resource":{"id":"cert-cond-1"}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$docref") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x64, 0x72, 0x65, 0x66})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","entry":[{"resource":{"id":"cert-doc-1"}},{"resource":{"id":"docref-ips-cert-pat-1"}}]}`))
			return
		}
		if r.Method == http.MethodPut && strings.HasSuffix(path, "/Observation/cert-obs-prov-1") {
			w.Header().Set("X-Provenance-Location", "/fhir/r4/certify/Provenance/prov-1")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"resourceType":"Observation","id":"cert-obs-prov-1"}`))
			return
		}
		if strings.HasSuffix(path, "/$provenance") || strings.HasSuffix(path, "/CapabilityStatement/$subset") || strings.HasSuffix(path, "/CapabilityStatement/$implements") || strings.HasSuffix(path, "/metadata/$subset") || strings.HasSuffix(path, "/metadata/$implements") || strings.HasSuffix(path, "/Claim/$inquire") || strings.HasSuffix(path, "/$inquire") || strings.HasSuffix(path, "/Measure/$evaluate-risk-condition-category") || strings.HasSuffix(path, "/$evaluate-risk-condition-category") || strings.HasSuffix(path, "/$davinci-data-export") || strings.HasSuffix(path, "/List/$find") || strings.HasSuffix(path, "/StructureMap/$transform") || strings.HasSuffix(path, "/$transform") || strings.HasSuffix(path, "/Schedule/$find") || strings.HasSuffix(path, "/Schedule/cert-sched-w93-1/$find") || strings.HasSuffix(path, "/RiskAssessment/$evaluate") || strings.HasSuffix(path, "/RiskAssessment/cert-ra-proto-1/$evaluate") || strings.HasSuffix(path, "/Consent/$evaluate") || strings.HasSuffix(path, "/Consent/cert-con-w97-1/$evaluate") || strings.HasSuffix(path, "/CarePlan/$evaluate") || strings.HasSuffix(path, "/CarePlan/cert-cp-proto-1/$evaluate") || strings.HasSuffix(path, "/AdverseEvent/$evaluate") || strings.HasSuffix(path, "/AdverseEvent/cert-ae-proto-1/$evaluate") || strings.HasSuffix(path, "/$collect-specimen") || strings.HasSuffix(path, "/DiagnosticReport/$evaluate") || strings.HasSuffix(path, "/DiagnosticReport/cert-dr-1/$evaluate") || strings.HasSuffix(path, "/Encounter/$admit") || strings.HasSuffix(path, "/$transfer") || strings.HasSuffix(path, "/$discharge") || strings.HasSuffix(path, "/$dispense") || strings.HasSuffix(path, "/MedicationRequest/$check-interactions") || strings.HasSuffix(path, "/$check-interactions") || strings.HasSuffix(path, "/MedicationKnowledge/$lookup") || strings.HasSuffix(path, "/MedicationKnowledge/cert-mk-w97-1/$lookup") || strings.HasSuffix(path, "/PractitionerRole/$find-providers") || strings.HasSuffix(path, "/HealthcareService/$find-services") || strings.HasSuffix(path, "/MessageHeader/$process-notification") || strings.HasSuffix(path, "/$process-notification") || strings.HasSuffix(path, "/$ihe-pix") || strings.HasSuffix(path, "/$sdoh-referral") || strings.HasSuffix(path, "/$sdoh-summary") || strings.HasSuffix(path, "/$carin-bb") || strings.HasSuffix(path, "/$carin-bb-summary") || strings.HasSuffix(path, "/$ecr-report") || strings.HasSuffix(path, "/PlanDefinition/$ersd-lookup") || strings.HasSuffix(path, "/$ersd-lookup") || strings.HasSuffix(path, "/$stage-cancer") || strings.HasSuffix(path, "/$mcode-summary") || strings.HasSuffix(path, "/MedicationKnowledge/$formulary-lookup") || strings.HasSuffix(path, "/$formulary-lookup") || strings.HasSuffix(path, "/$formulary-coverage") || strings.HasSuffix(path, "/$decease") || strings.HasSuffix(path, "/$iti-65") || strings.HasSuffix(path, "/$iti-67") || strings.HasSuffix(path, "/$find-subject-variants") || strings.HasSuffix(path, "/$find-subject-tx-implications") || strings.HasSuffix(path, "/$pacio-summary") || strings.HasSuffix(path, "/$pacio-assess") || strings.HasSuffix(path, "/$nhsn-hai-report") || strings.HasSuffix(path, "/Measure/$nhsn-acute-care") || strings.HasSuffix(path, "/$nhsn-acute-care") || strings.HasSuffix(path, "/$rwd-cohort") || strings.HasSuffix(path, "/$enroll") || strings.HasSuffix(path, "/$birth-report") || strings.HasSuffix(path, "/$bfdr-summary") || strings.HasSuffix(path, "/Patient/$ihe-pdqm") || strings.HasSuffix(path, "/$ihe-pdqm") || strings.HasSuffix(path, "/$ihe-iti-68") || strings.HasSuffix(path, "/Endpoint/$fast-endpoint-query") || strings.HasSuffix(path, "/$fast-endpoint-query") || strings.HasSuffix(path, "/$fast-verify") || strings.HasSuffix(path, "/$physical-activity-summary") || strings.HasSuffix(path, "/$physical-activity-referral") || strings.HasSuffix(path, "/$epi-document") || strings.HasSuffix(path, "/$evaluate-authorization") || strings.HasSuffix(path, "/$elr-submit") || strings.HasSuffix(path, "/$reportable-labs") || strings.HasSuffix(path, "/$adi-record") || strings.HasSuffix(path, "/$adi-document") || strings.HasSuffix(path, "/$ra-coding-gaps") || strings.HasSuffix(path, "/$ra-remediate") || strings.HasSuffix(path, "/$eicr-document") || strings.HasSuffix(path, "/$rr-process") || strings.HasSuffix(path, "/$pdex-WriteData") || strings.HasSuffix(path, "/$formulary-alternatives") || strings.HasSuffix(path, "/$record-occupation") || strings.HasSuffix(path, "/$odh-summary") || strings.HasSuffix(path, "/$cancer-registry-report") || strings.HasSuffix(path, "/$medmorph-kar") || strings.HasSuffix(path, "/$vbpr-performance") || strings.HasSuffix(path, "/$vbpr-attest") || strings.HasSuffix(path, "/$pacio-cognitive-assess") || strings.HasSuffix(path, "/$pacio-cognitive-summary") || strings.HasSuffix(path, "/$rtpbc-submit") || strings.HasSuffix(path, "/$rtpbc-coverage") || strings.HasSuffix(path, "/$idi-match") || strings.HasSuffix(path, "/$verify-identity") || strings.HasSuffix(path, "/$pacio-timepoint") || strings.HasSuffix(path, "/$pacio-timepoints") || strings.HasSuffix(path, "/$vrdr-document") || strings.HasSuffix(path, "/$vrdr-submit") || strings.HasSuffix(path, "/$cdex-request") || strings.HasSuffix(path, "/$cdex-poll") || strings.HasSuffix(path, "/$pacio-pfe-record") || strings.HasSuffix(path, "/$pacio-pfe-summary") || strings.HasSuffix(path, "/$bfdr-document") || strings.HasSuffix(path, "/$bfdr-submit") || strings.HasSuffix(path, "/$cdex-submit-attachment") || strings.HasSuffix(path, "/$cdex-questionnaire-request") || strings.HasSuffix(path, "/$mcode-radiotherapy-record") || strings.HasSuffix(path, "/$mcode-radiotherapy-summary") || strings.HasSuffix(path, "/$iis-submit") || strings.HasSuffix(path, "/$iis-history") || strings.HasSuffix(path, "/$cdex-sign") || strings.HasSuffix(path, "/$cdex-verify-signature") || strings.HasSuffix(path, "/$mcode-genomics-report") || strings.HasSuffix(path, "/$mcode-genomics-summary") || strings.HasSuffix(path, "/$nbs-submit") || strings.HasSuffix(path, "/$nbs-summary") || strings.HasSuffix(path, "/$pct-aeob-summary") || strings.HasSuffix(path, "/$pct-gfe-coordination") || strings.HasSuffix(path, "/$vulcan-icsr-report") || strings.HasSuffix(path, "/$vulcan-soa-timeline") || strings.HasSuffix(path, "/$ecr-submit") || strings.HasSuffix(path, "/$ecr-summary") || strings.HasSuffix(path, "/$sdoh-assess") || strings.HasSuffix(path, "/$sdoh-fulfill") || strings.HasSuffix(path, "/$vulcan-esource-extract") || strings.HasSuffix(path, "/$vulcan-summary") || strings.HasSuffix(path, "/$elr-summary") || strings.HasSuffix(path, "/$cancer-registry-submit") || strings.HasSuffix(path, "/$carin-bb-claims") || strings.HasSuffix(path, "/$rtpbc-summary") || strings.HasSuffix(path, "/$pacio-toc-bundle") || strings.HasSuffix(path, "/$adi-verify") || strings.HasSuffix(path, "/$nhsn-aur-submit") || strings.HasSuffix(path, "/$nhsn-hai-summary") || strings.HasSuffix(path, "/$mcode-trial-match") || strings.HasSuffix(path, "/$mcode-disease-status") || strings.HasSuffix(path, "/$fast-network-query") || strings.HasSuffix(path, "/$fast-attest") || strings.HasSuffix(path, "/$ra-evaluate-risk") || strings.HasSuffix(path, "/$ra-coding-gap-report") || strings.HasSuffix(path, "/$nhsn-dqm-evaluate") || strings.HasSuffix(path, "/$nhsn-ach-summary") || strings.HasSuffix(path, "/$dtr-questionnaire-package") || strings.HasSuffix(path, "/$pas-inquire") || strings.HasSuffix(path, "/$burden-reduction-summary") || strings.HasSuffix(path, "/$mcode-trial-summary") || strings.HasSuffix(path, "/$ndh-verify") || strings.HasSuffix(path, "/$ndh-directory-summary") || strings.HasSuffix(path, "/$confirm-attribution-list") || strings.HasSuffix(path, "/$atr-summary") || strings.HasSuffix(path, "/$pacio-splasch-record") || strings.HasSuffix(path, "/$pacio-splasch-summary") || strings.HasSuffix(path, "/$vulcan-rwd-cohort") || strings.HasSuffix(path, "/$vulcan-rwd-summary") || strings.HasSuffix(path, "/$saner-report-capacity") || strings.HasSuffix(path, "/$saner-capacity-summary") || strings.HasSuffix(path, "/$codex-rttd-plan") || strings.HasSuffix(path, "/$codex-rttd-summary") || strings.HasSuffix(path, "/$sdoh-consent-record") || strings.HasSuffix(path, "/$sdoh-care-bundle") || strings.HasSuffix(path, "/$udap-register") || strings.HasSuffix(path, "/$udap-trust-summary") || strings.HasSuffix(path, "/$pdex-provider-member-match") || strings.HasSuffix(path, "/$pdex-provider-everything") || strings.HasSuffix(path, "/$pdex-payer-exchange") || strings.HasSuffix(path, "/$pdex-payer-summary") || strings.HasSuffix(path, "/$hold") || strings.HasSuffix(path, "/$book") || strings.HasSuffix(path, "/$submit-attachment") || strings.HasSuffix(path, "/Library/$evaluate") || strings.HasSuffix(path, "/Library/CMS165-Logic/$evaluate") || strings.HasSuffix(path, "/$health-cards-issue") || strings.HasSuffix(path, "/$pcde") || strings.HasSuffix(path, "/ResearchStudy/$evaluate-eligibility") || strings.HasSuffix(path, "/$evaluate-eligibility") || strings.HasSuffix(path, "/Claim/$predetermine") || strings.HasSuffix(path, "/$predetermine") || strings.HasSuffix(path, "/$crd") || strings.HasSuffix(path, "/$immds-forecast") || strings.HasSuffix(path, "/Claim/$gfe-submit") || strings.HasSuffix(path, "/$gfe") || strings.HasSuffix(path, "/$member-add") || strings.HasSuffix(path, "/$member-remove") || strings.HasSuffix(path, "/Patient/$bulk-member-match") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x70, 0x72, 0x6f, 0x76})
				return
			}
		}
		if (r.Method == http.MethodPatch || r.Method == http.MethodDelete) && strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x70, 0x74, 0x63, 0x68})
			return
		}
		if strings.HasSuffix(path, "/$provenance") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Provenance","target":[{"reference":"Observation/cert-obs-prov-1"}]}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$apply") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x61, 0x70, 0x6c, 0x79})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"CarePlan","id":"cp-1"}`))
			return
		}
		if strings.HasSuffix(path, "/$reindex") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x72, 0x69, 0x64, 0x78})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"diagnostics":"Reindexed 1 resource(s)"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$data-requirements") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x64, 0x72, 0x65, 0x71})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Library","type":{"coding":[{"code":"module-definition"}]}}`))
			return
		}
		if strings.HasSuffix(path, "/$fhirpath") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x66, 0x70, 0x74, 0x68})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"result","valueString":"Certify"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$add") || strings.HasSuffix(path, "/$remove") || strings.HasSuffix(path, "/$merge") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x67, 0x72, 0x70, 0x31})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Group","id":"cert-grp-1"}`))
			return
		}
		if strings.HasSuffix(path, "/Observation/$lastn") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x6c, 0x73, 0x74, 0x6e})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","entry":[{"resource":{"id":"cert-obs-prov-1"}}]}`))
			return
		}
		if strings.HasSuffix(path, "/Observation/$stats") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x73, 0x74, 0x61, 0x74})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"statistic","resource":{"code":{"coding":[{"code":"average"}]}}}]}`))
			return
		}
		if strings.HasSuffix(path, "/CapabilityStatement/$subset") {
			_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement","meta":{"tag":[{"code":"SUBSETTED"}]}}`))
			return
		}
		if strings.HasSuffix(path, "/CapabilityStatement/$implements") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"result","valueBoolean":true}]}`))
			return
		}
		if strings.HasSuffix(path, "/$snapshot") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x73, 0x6e, 0x61, 0x70})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"StructureDefinition","snapshot":{"element":[{"id":"Patient.identifier"}]}}`))
			return
		}
		if strings.HasSuffix(path, "/NamingSystem/$preferred-id") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x70, 0x72, 0x65, 0x66})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"result","valueString":"http://loinc.org"}]}`))
			return
		}
		if strings.HasSuffix(path, "/CoverageEligibilityRequest/$submit") || strings.HasSuffix(path, "/CoverageEligibilityRequest/cert-cer-proto-1/$submit") || strings.HasSuffix(path, "/CoverageEligibilityRequest/cert-cer-w109/$submit") || strings.HasSuffix(path, "/Schedule/cert-sched-w109/$find") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x63, 0x65, 0x72, 0x31})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"CoverageEligibilityResponse","insurance":[{"inforce":true}]}`))
			return
		}
		if strings.HasSuffix(path, "/Library/CMS165-Logic/$evaluate") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"InInitialPopulation","valueBoolean":true}]}`))
			return
		}
		if strings.HasSuffix(path, "/$submit-attachment") {
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"diagnostics":"DocumentReference/cert-cdex-doc-1"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$health-cards-issue") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"verifiableCredential","valueString":"vc"},{"name":"resource:0"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$pcde") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","entry":[{"resource":{"code":"11503-0","id":"cert-cp-1"}}]}`))
			return
		}
		if strings.HasSuffix(path, "/Claim/$predetermine") {
			_, _ = w.Write([]byte(`{"resourceType":"ClaimResponse","use":"predetermination","preAuthRef":"PREDET-CERT-CLAIM-PREDET-1"}`))
			return
		}
		if strings.HasSuffix(path, "/Measure/$evaluate-risk-condition-category") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"ra-measurereport","text":"CMS-HCC 85"}`))
			return
		}
		if strings.HasSuffix(path, "/$crd") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","ext":"ext-coverage-information"}`))
			return
		}
		if strings.HasSuffix(path, "/CarePlan/cert-cp-1/$evaluate") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x63, 0x70, 0x65, 0x76})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","achievementStatus":"achieved"}`))
			return
		}
		if strings.HasSuffix(path, "/$immds-forecast") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","res":"ImmunizationRecommendation"}`))
			return
		}
		if strings.HasSuffix(path, "/RiskAssessment/$evaluate") {
			_, _ = w.Write([]byte(`{"resourceType":"RiskAssessment","probabilityDecimal":0.12}`))
			return
		}
		if strings.HasSuffix(path, "/MedicationKnowledge/$lookup") || strings.HasSuffix(path, "/MedicationKnowledge/$formulary-lookup") {
			_, _ = w.Write([]byte(`{"resourceType":"MedicationKnowledge","profile":"usdf-FormularyDrug"}`))
			return
		}
		if strings.HasSuffix(path, "/MedicationRequest/$check-interactions") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","interactionCount":1}`))
			return
		}
		if strings.HasSuffix(path, "/$ihe-pix") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","targetId":"cert-pat-1"}`))
			return
		}
		if strings.HasSuffix(path, "/$collect-specimen") {
			_, _ = w.Write([]byte(`{"resourceType":"Specimen","accessionIdentifier":"ACC-CERT-SR-1"}`))
			return
		}
		if strings.HasSuffix(path, "/Patient/$bulk-member-match") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","name":"MatchedMembers"}`))
			return
		}
		if strings.HasSuffix(path, "/$dispense") {
			_, _ = w.Write([]byte(`{"resourceType":"MedicationDispense","id":"disp-1"}`))
			return
		}
		if strings.HasSuffix(path, "/PractitionerRole/$find-providers") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"plannet-PractitionerRole"}`))
			return
		}
		if strings.HasSuffix(path, "/Consent/$evaluate") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","valueCode":"permit"}`))
			return
		}
		if strings.HasSuffix(path, "/$carin-bb") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"C4BB-Bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/PlanDefinition/$ersd-lookup") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","id":"ersd-rctc-plan-1"}`))
			return
		}
		if strings.HasSuffix(path, "/$mcode-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"mcode-patient-bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$find-subject-variants") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","code":"69548-6"}`))
			return
		}
		if strings.HasSuffix(path, "/Measure/$nhsn-acute-care") {
			_, _ = w.Write([]byte(`{"resourceType":"MeasureReport","measure":"nhsn-hai-aur-measure"}`))
			return
		}
		if strings.HasSuffix(path, "/Patient/$ihe-pdqm") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"IHE.PDQm.QueryPatientResourceResponseMessage"}`))
			return
		}
		if strings.HasSuffix(path, "/$rwd-cohort") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","name":"cohortGroup"}`))
			return
		}
		if strings.HasSuffix(path, "/Endpoint/$fast-endpoint-query") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"ndh-Endpoint"}`))
			return
		}
		if strings.HasSuffix(path, "/MedicinalProduct/$epi-document") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"Bundle-uv-epi"}`))
			return
		}
		if strings.HasSuffix(path, "/Observation/$reportable-labs") || strings.HasSuffix(path, "/$elr-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","code":"94500-6"}`))
			return
		}
		if strings.HasSuffix(path, "/$ra-coding-gaps") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"ra-measurereport"}`))
			return
		}
		if strings.HasSuffix(path, "/Composition/$eicr-document") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"eicr-document-bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$odh-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","code":"11341-5"}`))
			return
		}
		if strings.HasSuffix(path, "/$cancer-registry-report") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"ccrr-content-bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/MeasureReport/$vbpr-performance") {
			_, _ = w.Write([]byte(`{"resourceType":"MeasureReport","profile":"vbp-measurereport"}`))
			return
		}
		if strings.HasSuffix(path, "/ClaimResponse/$rtpbc-coverage") {
			_, _ = w.Write([]byte(`{"resourceType":"ClaimResponse","profile":"rtpbc-response-claimresponse"}`))
			return
		}
		if strings.HasSuffix(path, "/$pacio-timepoints") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"pacio-rt-reassessment-timepoint-encounter"}`))
			return
		}
		if strings.HasSuffix(path, "/Composition/$vrdr-document") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"vrdr-death-certificate-document"}`))
			return
		}
		if strings.HasSuffix(path, "/$pacio-pfe-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"pacio-pfe-observation-single"}`))
			return
		}
		if strings.HasSuffix(path, "/Composition/$bfdr-document") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"Bundle-document-live-birth-report"}`))
			return
		}
		if strings.HasSuffix(path, "/$mcode-radiotherapy-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"mcode-radiotherapy-course-summary"}`))
			return
		}
		if strings.HasSuffix(path, "/$iis-history") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"immds-immunizationevaluation"}`))
			return
		}
		if strings.HasSuffix(path, "/$mcode-genomics-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"mcode-cancer-genomics-report"}`))
			return
		}
		if strings.HasSuffix(path, "/$nbs-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","code":"54089-8"}`))
			return
		}
		if strings.HasSuffix(path, "/ExplanationOfBenefit/$pct-aeob-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"davinci-pct-aeob"}`))
			return
		}
		if strings.HasSuffix(path, "/$ecr-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","code":"55751-2"}`))
			return
		}
		if strings.HasSuffix(path, "/$vulcan-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","study":"vulcan-study-1"}`))
			return
		}
		if strings.HasSuffix(path, "/ExplanationOfBenefit/$carin-bb-claims") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"C4BB-ExplanationOfBenefit-Oral"}`))
			return
		}
		if strings.HasSuffix(path, "/$nhsn-hai-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"nhsn-aur-measure-report"}`))
			return
		}
		if strings.HasSuffix(path, "/OrganizationAffiliation/$fast-network-query") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"ndh-OrganizationAffiliation"}`))
			return
		}
		if strings.HasSuffix(path, "/$ra-coding-gap-report") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"ra-condition-gap-bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$nhsn-ach-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"ach-daily-initial-population-bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$burden-reduction-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"burden-reduction-summary-bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$mcode-trial-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"codex-clinical-trial-match-bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$ndh-directory-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"ndh-ExchangeBundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$atr-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"atr-bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$pacio-splasch-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"splasch-Bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$vulcan-rwd-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"vulcan-rwd-bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$saner-capacity-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"PublicHealthBundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$codex-rttd-summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"codexrt-summary-bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/$sdoh-care-bundle") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","profile":"SDOHCC-Bundle"}`))
			return
		}
		if strings.HasSuffix(path, "/TestScript/$evaluate") || strings.HasSuffix(path, "/TestScript/cert-stored-ts-1/$evaluate") || strings.HasSuffix(path, "/TestScript/cert-proto-ts-1/$evaluate") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x74, 0x73, 0x65, 0x76})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"TestReport","status":"completed","result":"pass","score":100}`))
			return
		}
		if strings.HasSuffix(path, "/$diff") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x64, 0x69, 0x66, 0x66})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"operation"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$closure") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x63, 0x6c, 0x6f, 0x73})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"ConceptMap","status":"active"}`))
			return
		}
		if r.Method == http.MethodPut && strings.HasSuffix(path, "/TestScript/cert-stored-ts-1") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"resourceType":"TestScript","id":"cert-stored-ts-1"}`))
			return
		}
		if r.Method == http.MethodPut && strings.HasSuffix(path, "/StructureDefinition/cert-custom-patient-profile") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"resourceType":"StructureDefinition","id":"cert-custom-patient-profile"}`))
			return
		}
		if strings.HasSuffix(path, "/Observation/$validate") {
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invariant","diagnostics":"Observation violated invariant obs-6"}]}`))
			return
		}
		if strings.HasSuffix(path, "/Patient/$validate") && q.Get("profile") == "http://flint.fhir.org/StructureDefinition/cert-custom-patient-profile" {
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invariant","diagnostics":"Profile constraint cp-1 failed and birthDate min=1"}]}`))
			return
		}
		if strings.HasSuffix(path, "/cds-services/flint-hypertension-care-gap/$dsi-attributes") {
			_, _ = w.Write([]byte(`{"serviceId":"flint-hypertension-care-gap","regulatoryCriterion":"45 CFR §170.315(b)(11)","validationSummary":{"sensitivity":0.988}}`))
			return
		}
		if strings.HasSuffix(path, "/cds-services/flint-hypertension-care-gap/feedback") {
			_, _ = w.Write([]byte(`{"status":"recorded","recordedCount":1}`))
			return
		}
		if strings.HasSuffix(path, "/AuditEvent/$verify-integrity") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x61, 0x75, 0x64, 0x31})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"verified","valueBoolean":true},{"name":"algorithm","valueString":"SHA-256-HASH-CHAIN"}]}`))
			return
		}
		if strings.Contains(path, "/ViewDefinition") && r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if (strings.Contains(path, "/ViewDefinition") || strings.HasSuffix(path, "/$sql")) && (strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf")) {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
			if r.Method == http.MethodPost && strings.HasSuffix(path, "/ViewDefinition") {
				w.WriteHeader(http.StatusCreated)
			}
			_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x76, 0x64, 0x70, 0x33})
			return
		}
		if strings.HasSuffix(path, "/ViewDefinition/patient-demographics/$run") || strings.HasSuffix(path, "/ViewDefinition/$run") {
			_, _ = w.Write([]byte(`[{"id":"cert-pat-1","rk":"cert-pat-1","family":"Certify","given":"Alice","given_val":"Alice","given_upper":"ALICE","part_type":"given","part_val":"ALICE","gender":"female","pat_gender":"female","birth_date":"1985-04-12","active":true,"mrn_val":"CERT-MRN-1","gender_mrn":"female:CERT-MRN-1","active_mrn":"CERT-MRN-1","loinc_uri":"http://loinc.org","sct_uri":"http://snomed.info/sct","ucum_uri":"http://unitsofmeasure.org","gp_ref":"Practitioner/cert-prac-1","gp_type":"Practitioner","gp_id":"cert-prac-1","gp_prac_id":"cert-prac-1","gp_label":"PRAC:cert-prac-1","gp_coal":"cert-prac-1","is_prac":true,"sys_val_upper":"HTTP://HOSPITAL.EXAMPLE.ORG/MRN|CERT-MRN-1","sys_val_len":41,"root_plus_val_len":20,"val_len_abs":10,"val_len_scaled":15,"entry_kind":"mrn","entry_val":"CERT-MRN-1","fallback_note":null},{"rk":"cert-pat-1","part_type":"mrn","part_val":"CERT-MRN-1","entry_kind":"phone","entry_val":"NO_PHONE","fallback_note":null}]`))
			return
		}
		if strings.HasSuffix(path, "/ViewDefinition/observation-vitals/$run") {
			if strings.Contains(q.Get("_format"), "tab-separated-values") {
				w.Header().Set("Content-Type", "text/tab-separated-values")
				_, _ = w.Write([]byte("id\tpatient_id\tstatus\tcode\tsystem\tvalue\tunit\teffective\ncert-obs-1\tcert-pat-1\tfinal\t8867-4\thttp://loinc.org\t72\t/min\t2026-01-15T09:15:00Z\n"))
				return
			}
			w.Header().Set("Content-Type", "text/csv")
			_, _ = w.Write([]byte("id,patient_id,status,code,system,value,unit,effective\ncert-obs-1,cert-pat-1,final,8867-4,http://loinc.org,72,/min,2026-01-15T09:15:00Z\n"))
			return
		}
		if r.Header.Get("Prefer") == "handling=strict" && q.Get("unknown_proto_param") != "" {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
			w.Header().Set("Preference-Applied", "handling=strict")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x65, 0x72, 0x72, 0x31})
			return
		}
		if strings.Contains(q.Get("_format"), "protobuf") || strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") && r.Method == http.MethodPost && strings.HasSuffix(path, "/fhir/r4/certify") {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
			w.Header().Set("X-Flint-Storage-Model", "zero-json-proto3")
			_, _ = w.Write([]byte{0x0a, 0x0c, 0x0a, 0x0a, 0x63, 0x65, 0x72, 0x74, 0x2d, 0x70, 0x61, 0x74, 0x2d, 0x31})
			return
		}
		if strings.HasSuffix(path, "/ViewDefinition/observation-vitals/$sql") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x76, 0x73, 0x71, 0x6c})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"dialect","valueCode":"doris"},{"name":"storageModel","valueString":"zero-json-proto3-wire"},{"name":"targetTable","valueString":"fhir_observation"},{"name":"sql","valueString":"SELECT r.res_id AS ` + "`id`" + ` FROM fhir_observation r"}]}`))
			return
		}
		if strings.HasSuffix(path, "/ViewDefinition/$sql") {
			w.Header().Set("Content-Type", "application/sql; charset=utf-8")
			_, _ = w.Write([]byte("SELECT r.res_id AS id, r.value_quantity AS val, CONCAT(r.code_system, '|', r.code), (COALESCE(r.code_system, '') || '|' || COALESCE(r.code, '')), ((r.value_quantity * 2) + 10), CAST((r.value_quantity / 10) AS BIGINT), CAST(TRUNC((r.value_quantity) / (10)) AS BIGINT), MOD(r.value_quantity, 5), MOD(CAST(r.value_quantity AS NUMERIC), CAST(5 AS NUMERIC)), CAST(r.effective_start AS DATE), CURRENT_DATE(), CURRENT_DATE, NOW(), CURRENT_TIMESTAMP, CURTIME(), CURRENT_TIME, REGEXP(r.code, '^8867-4$'), r.code ~ '^8867-4$', ABS(r.value_quantity), CEIL(r.value_quantity), FLOOR(r.value_quantity), ROUND(r.value_quantity, 2), SQRT(r.value_quantity), EXP(r.value_quantity), LN(r.value_quantity), LOG(10, r.value_quantity), POW(r.value_quantity, 2), POWER(r.value_quantity, 2), TRUNCATE(r.value_quantity, 0), TRUNC(r.value_quantity), ARRAY_SORT(r.string_tokens), ARRAY_REVERSE_SORT(r.string_tokens), ARRAY_SORT(ARRAY_DISTINCT(r.string_tokens)), element_at(SPLIT_BY_STRING(r.res_id, '-'), 1), ARRAY_SIZE(SPLIT_BY_STRING(r.res_id, '-')), SELECT DISTINCT s.sp_value, SPLIT_PART(r.res_id, '-', 1), ARRAY_LENGTH(STRING_TO_ARRAY(r.res_id, '-'), 1), ARRAY_SLICE(ARRAY_SLICE(r.string_tokens, 2), 1, 2), ARRAY_SLICE(ARRAY_SORT(r.string_tokens), 1, 2), element_at(ARRAY_SLICE(ARRAY_SORT(r.string_tokens), 2), 1), ARRAY_SIZE(ARRAY_SLICE(ARRAY_SLICE(r.string_tokens, 2), 1, 2)), ARRAY_SLICE(ARRAY_SORT(ARRAY_UNION(r.string_tokens, r.string_tokens)), 1, 2), element_at(ARRAY_SLICE(ARRAY_SORT(ARRAY_UNION(r.string_tokens, r.string_tokens)), 2), 1), element_at(ARRAY_SORT(ARRAY_UNION(r.string_tokens, r.string_tokens)), -1), ARRAY_SIZE(ARRAY_SLICE(ARRAY_SORT(ARRAY_DISTINCT(ARRAY_CONCAT(r.string_tokens, r.string_tokens))), 1, 2)), ARRAY_SIZE(ARRAY_SLICE(r.string_tokens, 2)), ARRAY_SIZE(ARRAY_EXCEPT(r.string_tokens, r.string_tokens)), ARRAY_SIZE(ARRAY_DISTINCT(ARRAY_SLICE(ARRAY_CONCAT(r.string_tokens, r.string_tokens), 1, 2))), ARRAY_EXCEPT(ARRAY_SLICE(ARRAY_SORT(r.string_tokens), 1, 2), r.string_tokens), ARRAY_SUM(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_SLICE(r.code_tokens, 2))), ARRAY_AVG(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_SLICE(ARRAY_SORT(r.code_tokens), 1, 2))), element_at(ARRAY_SORT(ARRAY_DISTINCT(r.string_tokens)), 2), element_at(ARRAY_SLICE(ARRAY_SLICE(r.string_tokens, 2), 1, 1), 1), COALESCE(ARRAY_SUM(ARRAY_MAP(x -> CHAR_LENGTH(x), ARRAY_SLICE(ARRAY_DISTINCT(r.string_tokens), 1, 2))), 0), NOT ARRAY_EXISTS(x -> x = '', ARRAY_SLICE(r.string_tokens, 2)), NOT ARRAY_EXISTS(x -> CHAR_LENGTH(x) < 2, ARRAY_UNION(r.string_tokens, r.string_tokens)), ARRAY_EXISTS(x -> CHAR_LENGTH(x) >= 3, ARRAY_SLICE(ARRAY_SORT(r.string_tokens), 1, 2)), ARRAY_CONTAINS(ARRAY_CONCAT(r.code_tokens, r.code_tokens), 'true'), NOT ARRAY_CONTAINS(ARRAY_EXCEPT(r.code_tokens, r.code_tokens), 'true'), NOT EXISTS (SELECT 1 FROM (SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'given' OFFSET 1) sub WHERE sub.sp_value = ''), WHERE CHAR_LENGTH(sub.sp_value) < 2), WHERE CHAR_LENGTH(sub.sp_value) >= 3), WHERE sub.sp_value = 'true'), SELECT SUM(sub.sp_value) FROM (SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' OFFSET 1) sub, SELECT AVG(sub.sp_value) FROM (SELECT q.sp_value FROM spidx_quantity q WHERE q.tenant_id = r.tenant_id AND q.res_type = r.res_type AND q.res_id = r.res_id AND q.sp_name = 'value-quantity' ORDER BY q.sp_value ASC LIMIT 2) sub, ORDER BY sp_value ASC LIMIT 1 OFFSET 1) AS second_sorted_given, SELECT SUM(CHAR_LENGTH(sub.sp_value)) FROM (SELECT DISTINCT s.sp_value FROM spidx_string s, OFFSET 1), NOT (EXISTS (SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'given' EXCEPT SELECT s.sp_value FROM spidx_string s)), SELECT COUNT(DISTINCT sub.sp_value) FROM (SELECT s.sp_value FROM spidx_string s, NOT EXISTS (SELECT sub.sp_value FROM (SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'given' ORDER BY sp_value ASC LIMIT 2) sub EXCEPT SELECT s.sp_value FROM spidx_string s), UNION SELECT s.sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'family' ORDER BY sp_value ASC LIMIT 2, ORDER BY sp_value ASC LIMIT 1 OFFSET 1, ORDER BY sp_value DESC LIMIT 1, LIMIT 2 OFFSET 1, ORDER BY sp_value ASC LIMIT 2, ORDER BY s.sp_value ASC LIMIT 1 OFFSET 1, SELECT COUNT(*) FROM (SELECT s.sp_value, ORDER BY sp_value ASC, ORDER BY sp_value DESC, LIKE 'cert%', LIKE '%TIFY', ARRAY_DISTINCT(r.string_tokens), ARRAY_UNION(r.string_tokens, r.string_tokens), ARRAY_CONCAT(r.string_tokens, r.string_tokens), ARRAY_INTERSECT(r.string_tokens, r.string_tokens), ARRAY_EXCEPT(r.string_tokens, r.string_tokens), ARRAY_SIZE(ARRAY_EXCEPT(r.string_tokens, r.string_tokens)), SPLIT_BY_STRING(r.code, '-'), STRING_TO_ARRAY(r.code, '-'), SPLIT_BY_STRING(r.code, ''), REGEXP_SPLIT_TO_ARRAY(r.code, ''), IN ('final', 'amended'), IN ('female', 'other'), CAST(r.effective_start AS TIME), TO_BASE64(r.res_id), FROM_BASE64(TO_BASE64(r.res_id)), ENCODE(CAST(r.res_id AS BYTEA), 'base64'), CONVERT_FROM(DECODE(r.res_id, 'base64'), 'UTF8'), IS NOT NULL AND CAST(, ARRAY_SUM(ARRAY_MAP(x -> CHAR_LENGTH(x), LAST_DAY(CAST(, SUM(CHAR_LENGTH(sub.sp_value)), DATE_TRUNC('month', CHAR_LENGTH(CAST(, REPLACE(REPLACE(REPLACE(r.code, '&', '&amp;'), '<', '&lt;'), '>', '&gt;'), REPLACE(REPLACE(REPLACE(r.code, '&gt;', '>'), '&lt;', '<'), '&amp;', '&'), LOWER(TRIM(r.code)) = LOWER(TRIM('8867-4')), ((1 = 1) <> (1 = 0)), (NOT (1 = 1) OR (1 = 1)), ARRAY_AGG(DISTINCT s.sp_value), COUNT(DISTINCT s.sp_value), SELECT s.sp_value FROM spidx_string s UNION SELECT s.sp_value FROM spidx_string s UNION ALL SELECT s.sp_value FROM spidx_string s INTERSECT SELECT s.sp_value FROM spidx_string s EXCEPT SELECT s.sp_value FROM spidx_string s, NOT EXISTS (SELECT s.sp_value FROM spidx_string s EXCEPT SELECT s.sp_value FROM spidx_string s), NOT array_exists(x -> x LIKE 'family=%', r.string_tokens), array_exists(x -> x = 'given=Alice', r.string_tokens), NOT array_contains(r.code_tokens, 'active=|false'), ext:us-core-birthsex=|%, NOT EXISTS (SELECT 1 FROM spidx_string s), EXISTS (SELECT 1 FROM spidx_string s), NOT EXISTS (SELECT 1 FROM spidx_token t), t.sp_system = 'http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex', identifier=http://hospital.example.org/mrn|%, identifier=http://hospital.example.org/mrn|CERT-MRN-1, CAST(r.code AS BOOLEAN), (CAST(r.effective_start AS DATE) IS NOT NULL), t.sp_name = 'identifier' AND t.sp_system = 'http://hospital.example.org/mrn', t.sp_value = 'CERT-MRN-1', ARRAY_SUM(r.code_tokens), ARRAY_AVG(r.code_tokens), ARRAY_MIN(r.code_tokens), ARRAY_MAX(r.code_tokens), SELECT SUM(q.sp_value) FROM spidx_quantity, SELECT AVG(q.sp_value) FROM spidx_quantity, SELECT MIN(q.sp_value) FROM spidx_quantity, SELECT MAX(q.sp_value) FROM spidx_quantity, LIKE '88%', OFFSET 1) sub), REGEXP_REPLACE(r.code, '[^0-9]', ''), REGEXP(r.code, '^88.*'), r.code ~ '^88.*', CAST(r.value_quantity AS BIGINT), CAST(r.value_quantity AS DOUBLE), CAST(r.value_quantity AS DOUBLE PRECISION), CAST(r.value_quantity AS STRING), CAST(r.value_quantity AS TEXT), (LOCATE('88', r.code) - 1), (STRPOS(r.code, '88') - 1), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens), ARRAY_JOIN(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)), ' '), ARRAY_SIZE(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)), element_at(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)), -1), ARRAY_SLICE(ARRAY_MAP(x -> SPLIT_PART(x, '=', 2), ARRAY_FILTER(x -> x LIKE 'given=%', r.string_tokens)), 2), ORDER BY s.sp_value DESC LIMIT 1, LIMIT 1 OFFSET 1, LIMIT 2) sub, COALESCE(r.res_id, 'Unknown'), CASE WHEN 1=1 THEN 'Active' ELSE 'Inactive' END, REPLACE(r.res_id, 'Cert', 'Mod'), SUBSTR(r.res_id, 1, 4), TRIM(r.res_id), LOWER(r.res_id), UPPER(r.res_id), CHAR_LENGTH(r.res_id), LENGTH(r.res_id), (SELECT COUNT(*) FROM spidx_string s), ARRAY_TO_STRING(ARRAY_AGG(s.sp_value), ' '), ARRAY_AGG(s.sp_value), ARRAY_JOIN(ARRAY_SORT(ARRAY_MAP(x -> LOWER(x), ARRAY_DISTINCT(r.string_tokens))), ','), element_at(ARRAY_REVERSE_SORT(ARRAY_MAP(x -> UPPER(x), r.string_tokens)), 1), ARRAY_MAP(x -> REPLACE(x, 'a', 'A'), r.string_tokens), ARRAY_MAP(x -> SUBSTR(x, 1, 1), r.string_tokens), ARRAY_SUM(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_MAP(x -> CHAR_LENGTH(x), ARRAY_DISTINCT(r.string_tokens)))), SELECT DISTINCT LOWER(s.sp_value) AS sp_value FROM spidx_string s, SELECT UPPER(s.sp_value) AS sp_value FROM spidx_string s, SELECT DISTINCT REPLACE(s.sp_value, 'a', 'A') AS sp_value FROM spidx_string s, SELECT DISTINCT SUBSTR(s.sp_value, 1, 1) AS sp_value FROM spidx_string s, SELECT SUM(CAST(sub.sp_value AS DOUBLE PRECISION)) FROM (SELECT DISTINCT CHAR_LENGTH(s.sp_value) AS sp_value FROM spidx_string s, ARRAY_JOIN(ARRAY_SORT(ARRAY_MAP(x -> REGEXP_REPLACE(x, '[0-9]+', '#'), r.string_tokens)), ','), ARRAY_SUM(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_MAP(x -> (LOCATE('a', x) - 1), ARRAY_DISTINCT(r.string_tokens)))), ARRAY_MAX(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_MAP(x -> CAST(x AS BIGINT), r.code_tokens))), ARRAY_JOIN(ARRAY_DISTINCT(ARRAY_MAP(x -> CAST(x AS STRING), r.code_tokens)), '|'), NOT ARRAY_EXISTS(x -> x < 0, ARRAY_MAP(x -> CAST(x AS BIGINT), r.code_tokens)), SELECT REGEXP_REPLACE(s.sp_value, '[0-9]+', '#', 'g') AS sp_value FROM spidx_string s, SELECT DISTINCT (STRPOS(s.sp_value, 'a') - 1) AS sp_value FROM spidx_string s, SELECT CAST(t.sp_value AS BIGINT) AS sp_value FROM spidx_token t, ARRAY[]::bigint[], ARRAY_SUM(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_MAP(x -> ABS(x), ARRAY_MAP(x -> CAST(x AS BIGINT), r.code_tokens)))), ARRAY_AVG(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_MAP(x -> ROUND(x, 1), ARRAY_MAP(x -> CAST(x AS DOUBLE), r.code_tokens)))), ARRAY_MAX(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_MAP(x -> POW(x, 2), ARRAY_MAP(x -> CAST(x AS BIGINT), r.code_tokens)))), ARRAY_JOIN(ARRAY_SORT(ARRAY_MAP(x -> HEX(x), r.string_tokens)), ','), element_at(ARRAY_MAP(x -> TO_BASE64(x), r.string_tokens), 1), NOT ARRAY_EXISTS(x -> x < 0, ARRAY_MAP(x -> ABS(x), ARRAY_MAP(x -> CAST(x AS BIGINT), r.code_tokens))), SELECT ABS(CAST(t.sp_value AS BIGINT)) AS sp_value FROM spidx_token t, SELECT ROUND(CAST(t.sp_value AS DOUBLE PRECISION), 1) AS sp_value FROM spidx_token t, SELECT POWER(CAST(t.sp_value AS BIGINT), 2) AS sp_value FROM spidx_token t, SELECT ENCODE(CAST(s.sp_value AS BYTEA), 'hex') AS sp_value FROM spidx_string s, SELECT ENCODE(CAST(s.sp_value AS BYTEA), 'base64') AS sp_value FROM spidx_string s, ARRAY[]::double precision[], ARRAY_JOIN(ARRAY_SORT(ARRAY_FILTER(x -> x <> 'Alice', r.string_tokens)), ','), ARRAY_SIZE(ARRAY_FILTER(x -> CHAR_LENGTH(x) >= 4, r.string_tokens)), element_at(ARRAY_FILTER(x -> x LIKE 'A%', r.string_tokens), 1), element_at(ARRAY_FILTER(x -> x LIKE '%e', r.string_tokens), -1), ARRAY_JOIN(ARRAY_MAP(x -> UPPER(x), ARRAY_FILTER(x -> x LIKE '%li%', r.string_tokens)), '|'), ARRAY_FILTER(x -> REGEXP(x, '^A.*'), r.string_tokens), ARRAY_SUM(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_FILTER(x -> x >= 10, ARRAY_MAP(x -> CAST(x AS BIGINT), r.code_tokens)))), AND s.sp_value <> 'Alice' ORDER BY sp_value ASC, AND CHAR_LENGTH(s.sp_value) >= 4) sub), AND s.sp_value LIKE 'A%' LIMIT 1), AND s.sp_value LIKE '%e' ORDER BY sp_value DESC LIMIT 1), SELECT UPPER(s.sp_value) AS sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'given' AND s.sp_value LIKE '%li%', AND s.sp_value ~ '^A.*') sub), AND CAST(t.sp_value AS BIGINT) >= 10) sub), ARRAY_JOIN(ARRAY_MAP(x -> CONCAT('[', COALESCE(UPPER(x), ''), ']'), r.string_tokens), ','), element_at(ARRAY_FILTER(x -> (x LIKE 'A%' AND CHAR_LENGTH(x) >= 4), r.string_tokens), 1), ARRAY_SIZE(ARRAY_FILTER(x -> (x = 'Alice' OR x = 'Bob'), r.string_tokens)), ARRAY_JOIN(ARRAY_SORT(ARRAY_FILTER(x -> NOT (x = 'Alice'), r.string_tokens)), ','), ARRAY_FILTER(x -> x IN ('Alice', 'Marie'), r.string_tokens), ARRAY_SUM(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_MAP(x -> (CAST(x AS DOUBLE) * 2), ARRAY_MAP(x -> CAST(x AS BIGINT), r.code_tokens)))), ARRAY_MAX(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_MAP(x -> (CAST(CHAR_LENGTH(x) AS DOUBLE) + 10), r.string_tokens))), SELECT ('[' || COALESCE(UPPER(s.sp_value), '') || ']') AS sp_value FROM spidx_string s, AND (s.sp_value LIKE 'A%' AND CHAR_LENGTH(s.sp_value) >= 4) LIMIT 1), AND (s.sp_value = 'Alice' OR s.sp_value = 'Bob')) sub), AND NOT (s.sp_value = 'Alice') ORDER BY sp_value ASC, AND s.sp_value IN ('Alice', 'Marie')) sub), SELECT (CAST(CAST(t.sp_value AS BIGINT) AS DOUBLE PRECISION) * 2) AS sp_value FROM spidx_token t, SELECT (CAST(CHAR_LENGTH(s.sp_value) AS DOUBLE PRECISION) + 10) AS sp_value FROM spidx_string s, ARRAY_JOIN(ARRAY_MAP(x -> CASE WHEN x = 'Alice' THEN UPPER(x) ELSE LOWER(x) END, r.string_tokens), ','), element_at(ARRAY_MAP(x -> COALESCE(NULLIF(SUBSTR(x, 11, 2), ''), NULLIF(LOWER(x), '')), r.string_tokens), 1), element_at(ARRAY_FILTER(x -> (CASE WHEN x LIKE 'A%' THEN CHAR_LENGTH(x) >= 4 ELSE x = 'Bob' END), r.string_tokens), 1), ARRAY_SUM(ARRAY_MAP(x -> CAST(x AS DOUBLE), ARRAY_MAP(x -> CASE WHEN x > 50 THEN (CAST(x AS DOUBLE) * 2) ELSE (CAST(x AS DOUBLE) + 5) END, ARRAY_MAP(x -> CAST(x AS BIGINT), r.code_tokens)))), ARRAY_EXISTS(x -> (CASE WHEN x = 'Alice' THEN (1 = 1) ELSE (1 = 0) END), r.string_tokens), SELECT CASE WHEN s.sp_value = 'Alice' THEN UPPER(s.sp_value) ELSE LOWER(s.sp_value) END AS sp_value FROM spidx_string s, SELECT COALESCE(NULLIF(SUBSTR(s.sp_value, 11, 2), ''), NULLIF(LOWER(s.sp_value), '')) AS sp_value FROM spidx_string s, AND (CASE WHEN s.sp_value LIKE 'A%' THEN CHAR_LENGTH(s.sp_value) >= 4 ELSE s.sp_value = 'Bob' END) LIMIT 1), SELECT CASE WHEN CAST(t.sp_value AS BIGINT) > 50 THEN (CAST(CAST(t.sp_value AS BIGINT) AS DOUBLE PRECISION) * 2) ELSE (CAST(CAST(t.sp_value AS BIGINT) AS DOUBLE PRECISION) + 5) END AS sp_value FROM spidx_token t, element_at(ARRAY_FILTER(x -> LOWER(TRIM(x)) = LOWER(TRIM('alice')), r.string_tokens), 1), ARRAY_JOIN(ARRAY_SORT(ARRAY_FILTER(x -> LOWER(TRIM(x)) <> LOWER(TRIM('zed')), r.string_tokens)), ','), ARRAY_SIZE(ARRAY_FILTER(x -> x IN ('Alice', 'Bob'), r.string_tokens)), element_at(ARRAY_FILTER(x -> (NOT (x LIKE 'A%') OR (CHAR_LENGTH(x) >= 4)), r.string_tokens), 1), ARRAY_SIZE(ARRAY_FILTER(x -> ((x = 'Alice') <> (x = 'Bob')), r.string_tokens)), ARRAY_FILTER(x -> REGEXP(x, '^A[a-z]+$'), r.string_tokens), ARRAY_SIZE(ARRAY_FILTER(x -> (x IS NOT NULL AND x <> ''), r.string_tokens)), ARRAY_EXISTS(x -> LOWER(TRIM(x)) = LOWER(TRIM('alice')), r.string_tokens), AND LOWER(TRIM(s.sp_value)) = LOWER(TRIM('alice')) LIMIT 1), AND LOWER(TRIM(s.sp_value)) <> LOWER(TRIM('zed')) ORDER BY sp_value ASC, AND s.sp_value IN ('Alice', 'Bob')) sub), AND (NOT (s.sp_value LIKE 'A%') OR (CHAR_LENGTH(s.sp_value) >= 4)) LIMIT 1), AND ((s.sp_value = 'Alice') <> (s.sp_value = 'Bob'))) sub), AND s.sp_value ~ '^A[a-z]+$') sub), AND (s.sp_value IS NOT NULL AND s.sp_value <> '')) sub), LATERAL VIEW EXPLODE(ARRAY_SORT(ARRAY_DISTINCT(ARRAY_FILTER(x -> x LIKE 'A%', r.string_tokens)))) fe_tbl AS fe_tok, UPPER(TRIM(fe_tok)) AS given_upper, CHAR_LENGTH(fe_tok) AS given_len, CASE WHEN CHAR_LENGTH(fe_tok) >= 4 THEN UPPER(fe_tok) ELSE LOWER(fe_tok) END AS given_iif, CONCAT(COALESCE(UPPER(fe_tok), ''), '-TAG') AS given_tag, INNER JOIN LATERAL (SELECT DISTINCT s.sp_value AS sp_value FROM spidx_string s WHERE s.tenant_id = r.tenant_id AND s.res_type = r.res_type AND s.res_id = r.res_id AND s.sp_name = 'given' AND s.sp_value LIKE 'A%' ORDER BY sp_value ASC) fe ON TRUE, UPPER(TRIM(fe.sp_value)) AS given_upper, CHAR_LENGTH(fe.sp_value) AS given_len, CASE WHEN CHAR_LENGTH(fe.sp_value) >= 4 THEN UPPER(fe.sp_value) ELSE LOWER(fe.sp_value) END AS given_iif, (COALESCE(UPPER(fe.sp_value), '') || '-TAG') AS given_tag, LATERAL VIEW EXPLODE(r.string_tokens) fe_tbl AS fe_tok, LATERAL VIEW EXPLODE(r.code_tokens) fe2_tbl AS fe2_tok, SPLIT_PART(fe_tok, '=', 2) AS given_val, SPLIT_PART(fe2_tok, '|', 2) AS mrn_val, r.gender AS pat_gender, 'http://loinc.org' AS loinc_uri, CONCAT(COALESCE(r.gender, ''), ':', COALESCE(SPLIT_PART(fe2_tok, '|', 2), '')) AS gender_mrn, UPPER(SPLIT_PART(fe_tok, '=', 2)) AS given_upper, (SPLIT_PART(fe_tok, '=', 2) LIKE 'A%' AND CHAR_LENGTH(SPLIT_PART(fe_tok, '=', 2)) >= 3), INNER JOIN spidx_string fe ON fe.tenant_id = r.tenant_id AND fe.res_type = r.res_type AND fe.res_id = r.res_id AND s.sp_name = 'given', INNER JOIN spidx_string fe ON fe.tenant_id = r.tenant_id AND fe.res_type = r.res_type AND fe.res_id = r.res_id AND fe.sp_name = 'given', INNER JOIN spidx_token fe2 ON fe2.tenant_id = r.tenant_id AND fe2.res_type = r.res_type AND fe2.res_id = r.res_id AND fe2.sp_name = 'identifier', LEFT JOIN spidx_token fe2 ON fe2.tenant_id = r.tenant_id AND fe2.res_type = r.res_type AND fe2.res_id = r.res_id AND fe2.sp_name = 'telecom', LATERAL VIEW OUTER EXPLODE(r.string_tokens) fe2_tbl AS fe2_tok, NULL AS fallback_note, COALESCE(NULLIF(SPLIT_PART(fe2_tok, '=', 2), ''), 'NO_PHONE') AS entry_val, COALESCE(NULLIF(fe2.sp_value, ''), 'NO_PHONE') AS entry_val, THEN 'HAS_PHONE' ELSE NULL END AS fallback_note, fe.sp_value AS given_val, fe2.sp_value AS mrn_val, UPPER(fe.sp_value) AS given_upper, (fe.sp_value LIKE 'A%' AND CHAR_LENGTH(fe.sp_value) >= 3), SPLIT_PART(SPLIT_PART(fe_tok, '=', 2), '|', 1) = 'http://hospital.example.org/mrn', fe.sp_system = 'http://hospital.example.org/mrn', LATERAL VIEW EXPLODE(ARRAY_FILTER(x -> x LIKE 'general-practitioner=%', r.ref_tokens)) fe_tbl AS fe_tok, SPLIT_PART(fe_tok, '=', 2) AS gp_ref, SPLIT_PART(SPLIT_PART(fe_tok, '=', 2), '/', 1) AS gp_type, SPLIT_PART(fe_tok, '/', 2) AS gp_id, CASE WHEN SPLIT_PART(SPLIT_PART(fe_tok, '=', 2), '/', 1) = 'Practitioner' THEN SPLIT_PART(fe_tok, '/', 2) ELSE NULL END AS gp_prac_id, CONCAT('PRAC:', COALESCE(CASE WHEN SPLIT_PART(SPLIT_PART(fe_tok, '=', 2), '/', 1) = 'Practitioner', INNER JOIN spidx_reference fe ON fe.tenant_id = r.tenant_id AND fe.res_type = r.res_type AND fe.res_id = r.res_id AND fe.sp_name = 'general-practitioner', (fe.target_type || '/' || fe.target_id) AS gp_ref, fe.target_type AS gp_type, fe.target_id AS gp_id, CASE WHEN fe.target_type = 'Practitioner' THEN fe.target_id ELSE NULL END AS gp_prac_id, ('PRAC:' || COALESCE(CASE WHEN fe.target_type = 'Practitioner' THEN fe.target_id ELSE NULL END, '')), UPPER(CONCAT(COALESCE(SPLIT_PART(SPLIT_PART(fe_tok, '=', 2), '|', 1), ''), '|', COALESCE(SPLIT_PART(fe_tok, '|', 2), ''))) AS sys_val_upper, (CHAR_LENGTH(SPLIT_PART(SPLIT_PART(fe_tok, '=', 2), '|', 1)) + CHAR_LENGTH(SPLIT_PART(fe_tok, '|', 2))) AS sys_val_len, (CHAR_LENGTH(r.res_id) + CHAR_LENGTH(SPLIT_PART(fe_tok, '|', 2))) AS root_plus_val_len, ABS((CHAR_LENGTH(SPLIT_PART(fe_tok, '|', 2)) - 20)) AS val_len_abs, ROUND((CHAR_LENGTH(SPLIT_PART(fe_tok, '|', 2)) * 1.5), 1) AS val_len_scaled, UPPER((COALESCE(fe.sp_system, '') || '|' || COALESCE(fe.sp_value, ''))) AS sys_val_upper, (CHAR_LENGTH(fe.sp_system) + CHAR_LENGTH(fe.sp_value)) AS sys_val_len, (LENGTH(r.res_id) + CHAR_LENGTH(fe.sp_value)) AS root_plus_val_len, ABS((CHAR_LENGTH(fe.sp_value) - 20)) AS val_len_abs, ROUND((CHAR_LENGTH(fe.sp_value) * 1.5), 1) AS val_len_scaled, ((COALESCE(fe.sp_system, '') || '|' || COALESCE(fe.sp_value, '')) LIKE 'http://hospital.example.org/mrn|CERT-%' AND (CHAR_LENGTH(fe.sp_system) + CHAR_LENGTH(fe.sp_value)) > 10), (LENGTH(fe.sp_system) + LENGTH(fe.sp_value)) AS sys_val_len, (LENGTH(r.res_id) + LENGTH(fe.sp_value)) AS root_plus_val_len, WHERE sub.sp_value < 0) FROM fhir_observation r LATERAL VIEW EXPLODE(r.code_tokens) fe_tbl AS fe_tok INNER JOIN spidx_token fe ON fe.tenant_id = r.tenant_id WHERE r.value_quantity >= 120 AND NOT array_contains(r.code_tokens, 'status=|entered-in-error') AND r.effective_start IS NOT NULL AND array_exists(x -> x LIKE 'family=Cert%', r.string_tokens) AND array_exists(x -> x LIKE 'family=%tify', r.string_tokens) AND array_exists(x -> x LIKE 'given=%lic%', r.string_tokens) UNION ALL SELECT r.res_id AS id, r.value_quantity AS val FROM fhir_observation r\n"))
			return
		}
		if strings.HasSuffix(path, "/$sync-analytics") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x73, 0x79, 0x6e, 0x63})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"syncedResources","valueInteger":18},{"name":"replicationErrors","valueInteger":0}]}`))
			return
		}
		if strings.HasSuffix(path, "/$process-message") || strings.HasSuffix(path, "/Appointment/$find") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x70, 0x6d, 0x73, 0x67})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"message"}`))
			return
		}
		if strings.HasSuffix(path, "/Patient/cert-pat-1") && (strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf")) {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
			_, _ = w.Write([]byte{0x0a, 0x0c, 0x0a, 0x0a, 0x63, 0x65, 0x72, 0x74, 0x2d, 0x70, 0x61, 0x74, 0x2d, 0x31})
			return
		}
		if pou := r.Header.Get("X-Purpose-Of-Use"); pou != "" {
			w.Header().Set("X-TEFCA-Exchange-Purpose", pou)
		}

		// Search & Graph & MPI endpoints
		if strings.HasSuffix(path, "/$graph") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x67, 0x72, 0x70, 0x68})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"id":"cert-pat-1"}},{"resource":{"id":"cert-obs-1"}}]}`))
			return
		}
		if strings.HasSuffix(path, "/Patient/$match") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x6d, 0x74, 0x63, 0x68})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","total":1,"entry":[{"resource":{"id":"cert-pat-1"},"search":{"extension":[{"valueCode":"certain"}]}}]}`))
			return
		}
		if strings.HasSuffix(path, "/_history") {
			if strings.Contains(r.Header.Get("Accept"), "application/x-protobuf") || strings.Contains(q.Get("_format"), "protobuf") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("X-Flint-Wire-Format", "google.fhir.r4.proto3")
				_, _ = w.Write([]byte{0x0a, 0x06, 0x0a, 0x04, 0x68, 0x69, 0x73, 0x74})
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"history","total":1,"link":[{"relation":"self"}],"entry":[{"response":{"location":"Patient/cert-pat-1/_history/1"}}]}`))
			return
		}
		if q.Get("_summary") == "count" {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","total":3}`))
			return
		}
		if q.Get("_tag") != "" && metaDeleted {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","total":0}`))
			return
		}
		if r.Header.Get("Prefer") == "handling=lenient" {
			w.Header().Set("Preference-Applied", "handling=lenient")
		}
		_, _ = w.Write([]byte(`{
			"resourceType":"Bundle",
			"type":"searchset",
			"total":2,
			"link":[{"relation":"self","url":"http://self"}],
			"entry":[
				{"resource":{"resourceType":"OperationOutcome","id":"oo-1"}},
				{"resource":{"resourceType":"Patient","id":"cert-pat-1","meta":{"tag":[{"code":"SUBSETTED"}]}}},
				{"resource":{"resourceType":"Condition","id":"cert-cond-1"}},
				{"resource":{"resourceType":"Encounter","id":"cert-enc-1"}},
				{"resource":{"resourceType":"Observation","id":"cert-obs-1"}},
				{"resource":{"resourceType":"Observation","id":"cert-obs-avg-bp"}},
				{"resource":{"resourceType":"Observation","id":"cert-obs-care-pref"}},
				{"resource":{"resourceType":"Coverage","id":"cert-cov-1"}}
			]
		}`))
	}))
	defer srv.Close()
	serverURL = srv.URL

	rep, err := Run(t.Context(), Config{
		BaseURL: srv.URL,
		Tenant:  "certify",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if rep.Result != "pass" || rep.Failed != 0 || rep.Passed != 127 {
		t.Fatalf("expected all 127 checks to pass, got result=%s passed=%d failed=%d\n%s",
			rep.Result, rep.Passed, rep.Failed, rep.FormatScorecard())
	}
	tr := rep.ToFHIRTestReport()
	trTests, ok := tr["test"].([]map[string]any)
	if !ok || len(trTests) != 10 {
		t.Fatalf("expected 10 suites in TestReport.test, got %d", len(trTests))
	}
}
