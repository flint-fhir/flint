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
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"return","valueMeta":{"tag":[{"code":"vip-cohort"}]}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$meta-delete") {
			metaDeleted = true
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"return","valueMeta":{}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$meta") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"return","valueMeta":{"tag":[{"code":"vip-cohort"}]}}]}`))
			return
		}

		// Suite 4: $everything & $export
		if strings.HasSuffix(path, "/$everything") {
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
		if strings.HasSuffix(path, "/Group/all/$export") {
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
		if strings.HasSuffix(path, "/$purge") {
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome"}`))
			return
		}

		// Suite 5: Validation, Terminology, Subscriptions, CQM, CDS, IPS, Da Vinci, SDC, Public Health, AuditEvent
		if strings.HasSuffix(path, "/$validate") {
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
			_, _ = w.Write([]byte(`{"resourceType":"Patient","id":"conv-pat-1"}`))
			return
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
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"match","part":[{"name":"code","valueCode":"59621000"}]}]}`))
			return
		}
		if strings.HasSuffix(path, "/ConceptMap/$translate") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"match","part":[{"name":"concept","valueCoding":{"code":"I10"}},{"name":"source","valueString":"38341003"}]}]}`))
			return
		}
		if strings.Contains(path, "/Subscription") {
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
			if strings.HasSuffix(path, "/$status") {
				_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"status","valueCode":"active"}],"type":"query-status"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(path, "/$evaluate-measure") {
			if q.Get("reportType") == "subject-list" {
				_, _ = w.Write([]byte(`{"resourceType":"MeasureReport","status":"complete","type":"subject-list","contained":[{"id":"numerator-list"}],"group":[{"population":[{"subjectResults":{"reference":"#numerator-list"}}]}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"MeasureReport","status":"complete","type":"individual","group":[{"measureScore":{"value":1.0}}]}`))
			return
		}
		if strings.HasSuffix(path, "/Measure/$care-gaps") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"document","entry":[{"resource":{"resourceType":"Composition","type":{"coding":[{"code":"96315-7"}]}}},{"resource":{"resourceType":"DetectedIssue","code":{"coding":[{"code":"closed-gap"}]}}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$collect-data") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"measureReport","resource":{"resourceType":"MeasureReport","id":"cert-pat-1","type":"data-collection","subject":{"reference":"Patient/cert-pat-1"}}}]}`))
			return
		}
		if strings.HasSuffix(path, "/cds-services") {
			_, _ = w.Write([]byte(`{"services":[{"id":"s1"},{"id":"s2"},{"id":"s3"}]}`))
			return
		}
		if strings.Contains(path, "/cds-services/") {
			_, _ = w.Write([]byte(`{"cards":[{"indicator":"critical","summary":"Allergy conflict"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$summary") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"document","entry":[{"resource":{"resourceType":"Composition","type":{"coding":[{"code":"60591-5"}]}}},{},{},{},{}]}`))
			return
		}
		if strings.HasSuffix(path, "/$member-match") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"MemberIdentifier","valueString":"Patient/cert-pat-1"}]}`))
			return
		}
		if strings.HasSuffix(path, "/Claim/$submit") {
			_, _ = w.Write([]byte(`{"resourceType":"ClaimResponse","outcome":"complete"}`))
			return
		}
		if strings.HasSuffix(path, "/$populate") {
			_, _ = w.Write([]byte(`{"resourceType":"QuestionnaireResponse","subject":{"display":"Alice Certify"}}`))
			return
		}
		if strings.HasSuffix(path, "/$extract") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"transaction","entry":[{"resource":{"resourceType":"Observation"}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$document") {
			w.Header().Set("Location", "/fhir/r4/certify/Bundle/doc-1")
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"document","entry":[{"resource":{"id":"cert-cond-1"}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$docref") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","entry":[{"resource":{"id":"cert-doc-1"}},{"resource":{"id":"docref-ips-cert-pat-1"}}]}`))
			return
		}
		if r.Method == http.MethodPut && strings.HasSuffix(path, "/Observation/cert-obs-prov-1") {
			w.Header().Set("X-Provenance-Location", "/fhir/r4/certify/Provenance/prov-1")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"resourceType":"Observation","id":"cert-obs-prov-1"}`))
			return
		}
		if strings.HasSuffix(path, "/$provenance") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Provenance","target":[{"reference":"Observation/cert-obs-prov-1"}]}}]}`))
			return
		}
		if strings.HasSuffix(path, "/$apply") {
			_, _ = w.Write([]byte(`{"resourceType":"CarePlan","id":"cp-1"}`))
			return
		}
		if strings.HasSuffix(path, "/$reindex") {
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"diagnostics":"Reindexed 1 resource(s)"}]}`))
			return
		}
		if strings.HasSuffix(path, "/$data-requirements") {
			_, _ = w.Write([]byte(`{"resourceType":"Library","type":{"coding":[{"code":"module-definition"}]}}`))
			return
		}
		if strings.HasSuffix(path, "/$fhirpath") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"result","valueString":"Certify"}]}`))
			return
		}
		if strings.HasSuffix(path, "/Observation/$lastn") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","entry":[{"resource":{"id":"cert-obs-prov-1"}}]}`))
			return
		}
		if strings.HasSuffix(path, "/Observation/$stats") {
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
		if strings.HasSuffix(path, "/StructureDefinition/$snapshot") {
			_, _ = w.Write([]byte(`{"resourceType":"StructureDefinition","snapshot":{"element":[{"id":"Patient.identifier"}]}}`))
			return
		}
		if strings.HasSuffix(path, "/NamingSystem/$preferred-id") {
			_, _ = w.Write([]byte(`{"resourceType":"Parameters","parameter":[{"name":"result","valueString":"http://loinc.org"}]}`))
			return
		}
		if strings.HasSuffix(path, "/CoverageEligibilityRequest/$submit") {
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

		// Search & Graph & MPI endpoints
		if strings.HasSuffix(path, "/$graph") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"id":"cert-pat-1"}},{"resource":{"id":"cert-obs-1"}}]}`))
			return
		}
		if strings.HasSuffix(path, "/Patient/$match") {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","total":1,"entry":[{"resource":{"id":"cert-pat-1"},"search":{"extension":[{"valueCode":"certain"}]}}]}`))
			return
		}
		if strings.HasSuffix(path, "/Patient/_history") {
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
	if rep.Result != "pass" || rep.Failed != 0 || rep.Passed != 75 {
		t.Fatalf("expected all 75 checks to pass, got result=%s passed=%d failed=%d\n%s",
			rep.Result, rep.Passed, rep.Failed, rep.FormatScorecard())
	}
	tr := rep.ToFHIRTestReport()
	trTests, ok := tr["test"].([]map[string]any)
	if !ok || len(trTests) != 7 {
		t.Fatalf("expected 7 suites in TestReport.test, got %d", len(trTests))
	}
}
