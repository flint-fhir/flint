package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flint-fhir/flint/pkg/txclient"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/sqlite"
)

func TestTerminologyFederation_OperationsSearchAndRequiredValidation(t *testing.T) {
	var upstreamHits atomic.Int32
	var force503 atomic.Bool

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if force503.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("upstream down"))
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")

		switch r.URL.Path {
		case "/r4/ValueSet/$expand":
			if r.Method == http.MethodPost {
				// Inline ValueSet fallback (e.g. local stored intensional ValueSet unknown upstream by URL)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"resourceType": "ValueSet",
					"status":       "active",
					"expansion": map[string]any{
						"total": 2,
						"contains": []map[string]any{
							{"system": "http://snomed.info/sct", "code": "195967001", "display": "Asthma"},
							{"system": "http://snomed.info/sct", "code": "233604007", "display": "Pneumonia"},
						},
					},
				})
				return
			}
			vsURL := r.URL.Query().Get("url")
			if vsURL == "http://example.org/ValueSet/respiratory" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"resourceType": "ValueSet",
					"url":          vsURL,
					"name":         "RespiratoryConditions",
					"status":       "active",
					"expansion": map[string]any{
						"total": 2,
						"contains": []map[string]any{
							{"system": "http://snomed.info/sct", "code": "195967001", "display": "Asthma"},
							{"system": "http://snomed.info/sct", "code": "233604007", "display": "Pneumonia"},
						},
					},
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "OperationOutcome",
				"issue":        []map[string]any{{"severity": "error", "code": "not-found", "diagnostics": "ValueSet not found"}},
			})

		case "/r4/ValueSet/$validate-code":
			vsURL := r.URL.Query().Get("url")
			code := r.URL.Query().Get("code")
			if vsURL == "http://unknown.example.org/ValueSet/does-not-exist" {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"resourceType": "OperationOutcome",
					"issue":        []map[string]any{{"severity": "error", "code": "not-found", "diagnostics": "unknown vs"}},
				})
				return
			}
			valid := code == "195967001" || code == "E11.9"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "Parameters",
				"parameter": []map[string]any{
					{"name": "result", "valueBoolean": valid},
					{"name": "code", "valueCode": code},
					{"name": "display", "valueString": "Federated Match"},
				},
			})

		case "/r4/CodeSystem/$lookup":
			sys := r.URL.Query().Get("system")
			code := r.URL.Query().Get("code")
			if sys == "http://hl7.org/fhir/sid/icd-10-cm" && code == "E11.9" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"resourceType": "Parameters",
					"parameter": []map[string]any{
						{"name": "name", "valueString": "ICD10CM"},
						{"name": "code", "valueCode": "E11.9"},
						{"name": "display", "valueString": "Type 2 diabetes mellitus without complications"},
					},
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "OperationOutcome",
				"issue":        []map[string]any{{"severity": "error", "code": "not-found", "diagnostics": "code not found"}},
			})

		case "/r4/CodeSystem/$validate-code":
			sys := r.URL.Query().Get("url")
			code := r.URL.Query().Get("code")
			valid := sys == "http://hl7.org/fhir/sid/icd-10-cm" && code == "E11.9"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resourceType": "Parameters",
				"parameter": []map[string]any{
					{"name": "result", "valueBoolean": valid},
					{"name": "code", "valueCode": code},
				},
			})

		case "/r4/CodeSystem/$subsumes":
			outcome := "not-subsumed"
			if r.URL.Query().Get("codeA") == "19829001" && r.URL.Query().Get("codeB") == "195967001" {
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
	defer upstream.Close()

	st := sqlite.New(nil, sqlite.WithDSN(":memory:"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)

	txCli, err := txclient.New(txclient.Config{
		BaseURL:  upstream.URL + "/r4",
		Timeout:  2 * time.Second,
		CacheTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("txclient.New: %v", err)
	}
	srv.SetTerminologyClient(txCli)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Local-first fast path: AdministrativeGender and known LOINC 8867-4 should make 0 upstream HTTP calls.
	resp, err := http.Get(ts.URL + "/fhir/r4/demo/ValueSet/$expand?url=http://hl7.org/fhir/ValueSet/administrative-gender")
	if err != nil {
		t.Fatalf("local $expand: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local $expand status=%d", resp.StatusCode)
	}
	resp, err = http.Get(ts.URL + "/fhir/r4/demo/CodeSystem/$lookup?system=http://loinc.org&code=8867-4")
	if err != nil {
		t.Fatalf("local $lookup: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local $lookup status=%d", resp.StatusCode)
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("expected 0 upstream hits for local-first lookups, got %d", got)
	}

	// 2. Federated ValueSet/$expand + cache reuse
	resp, err = http.Get(ts.URL + "/fhir/r4/demo/ValueSet/$expand?url=http://example.org/ValueSet/respiratory")
	if err != nil {
		t.Fatalf("federated $expand: %v", err)
	}
	var expVS map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&expVS)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || expVS["name"] != "RespiratoryConditions" {
		t.Fatalf("unexpected federated $expand: status=%d body=%v", resp.StatusCode, expVS)
	}
	hitsAfterFirstExpand := upstreamHits.Load()

	// Second call hits LRU cache -> 0 new upstream calls
	resp, err = http.Get(ts.URL + "/fhir/r4/demo/ValueSet/$expand?url=http://example.org/ValueSet/respiratory")
	if err != nil {
		t.Fatalf("cached federated $expand: %v", err)
	}
	resp.Body.Close()
	if upstreamHits.Load() != hitsAfterFirstExpand {
		t.Fatalf("expected cached $expand without upstream hit")
	}

	// 3. Stored intensional ValueSet (is-a SNOMED filter) expanded via inline POST fallback when URL 404s upstream
	putVSBody := `{
		"resourceType": "ValueSet",
		"id": "local-asthma-isa",
		"url": "http://hospital.local/ValueSet/asthma-isa",
		"status": "active",
		"compose": {
			"include": [{
				"system": "http://snomed.info/sct",
				"filter": [{"property": "concept", "op": "is-a", "value": "195967001"}]
			}]
		}
	}`
	putReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/demo/ValueSet/local-asthma-isa", strings.NewReader(putVSBody))
	putReq.Header.Set("Content-Type", "application/fhir+json")
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil || (putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusCreated) {
		t.Fatalf("PUT local ValueSet failed: err=%v status=%v", err, putResp)
	}
	putResp.Body.Close()

	resp, err = http.Get(ts.URL + "/fhir/r4/demo/ValueSet/local-asthma-isa/$expand")
	if err != nil {
		t.Fatalf("instance intensional $expand: %v", err)
	}
	var instExp map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&instExp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || instExp["id"] != "local-asthma-isa" {
		t.Fatalf("instance intensional $expand failed: status=%d body=%v", resp.StatusCode, instExp)
	}

	// 4. Federated :in search modifier over Condition
	condBody := `{
		"resourceType": "Condition",
		"id": "cond-asthma-1",
		"subject": {"reference": "Patient/pat-1"},
		"code": {"coding": [{"system": "http://snomed.info/sct", "code": "195967001", "display": "Asthma"}]}
	}`
	cReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/demo/Condition/cond-asthma-1", strings.NewReader(condBody))
	cReq.Header.Set("Content-Type", "application/fhir+json")
	cResp, err := http.DefaultClient.Do(cReq)
	if err != nil {
		t.Fatalf("seed Condition: %v", err)
	}
	cResp.Body.Close()

	searchResp, err := http.Get(ts.URL + "/fhir/r4/demo/Condition?code:in=http://example.org/ValueSet/respiratory")
	if err != nil {
		t.Fatalf(":in search: %v", err)
	}
	var searchBundle map[string]any
	_ = json.NewDecoder(searchResp.Body).Decode(&searchBundle)
	searchResp.Body.Close()
	if searchResp.StatusCode != http.StatusOK || searchBundle["total"] != float64(1) {
		t.Fatalf("expected 1 Condition from federated :in search, got status=%d bundle=%v", searchResp.StatusCode, searchBundle)
	}

	// 5. Federated ValueSet/$validate-code, CodeSystem/$lookup, CodeSystem/$validate-code, CodeSystem/$subsumes
	for _, tc := range []struct {
		name       string
		path       string
		wantStatus int
	}{
		{"vs-validate", "/fhir/r4/demo/ValueSet/$validate-code?url=http://example.org/ValueSet/respiratory&system=http://snomed.info/sct&code=195967001", http.StatusOK},
		{"cs-lookup", "/fhir/r4/demo/CodeSystem/$lookup?system=http://hl7.org/fhir/sid/icd-10-cm&code=E11.9", http.StatusOK},
		{"cs-lookup-404", "/fhir/r4/demo/CodeSystem/$lookup?system=http://hl7.org/fhir/sid/icd-10-cm&code=Z99.999", http.StatusNotFound},
		{"cs-validate", "/fhir/r4/demo/CodeSystem/$validate-code?url=http://hl7.org/fhir/sid/icd-10-cm&code=E11.9", http.StatusOK},
		{"cs-subsumes", "/fhir/r4/demo/CodeSystem/$subsumes?system=http://snomed.info/sct&codeA=19829001&codeB=195967001", http.StatusOK},
	} {
		r, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		r.Body.Close()
		if r.StatusCode != tc.wantStatus {
			t.Fatalf("%s: status=%d want=%d", tc.name, r.StatusCode, tc.wantStatus)
		}
	}

	// 6. Federated StructureDefinition required binding validation + FailOpen vs FailClosed
	sdJSON := `{
		"resourceType": "StructureDefinition",
		"id": "fed-condition-profile",
		"url": "http://example.org/fhir/StructureDefinition/FedRespiratoryCondition",
		"name": "FedRespiratoryCondition",
		"status": "active",
		"kind": "resource",
		"abstract": false,
		"type": "Condition",
		"baseDefinition": "http://hl7.org/fhir/StructureDefinition/Condition",
		"derivation": "constraint",
		"differential": {
			"element": [{
				"id": "Condition.code",
				"path": "Condition.code",
				"min": 1,
				"binding": {
					"strength": "required",
					"valueSet": "http://example.org/ValueSet/respiratory"
				}
			}]
		}
	}`
	sdReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/default/StructureDefinition/fed-condition-profile", strings.NewReader(sdJSON))
	sdReq.Header.Set("Content-Type", "application/fhir+json")
	sdResp, err := http.DefaultClient.Do(sdReq)
	if err != nil || (sdResp.StatusCode != http.StatusOK && sdResp.StatusCode != http.StatusCreated) {
		t.Fatalf("register StructureDefinition failed: err=%v status=%v", err, sdResp)
	}
	sdResp.Body.Close()

	validateCond := func(code string) (int, string) {
		payload := map[string]any{
			"resourceType": "Condition",
			"meta": map[string]any{
				"profile": []string{"http://example.org/fhir/StructureDefinition/FedRespiratoryCondition"},
			},
			"subject": map[string]any{"reference": "Patient/pat-1"},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": code},
				},
			},
		}
		raw, _ := json.Marshal(payload)
		vr, err := http.Post(ts.URL+"/fhir/r4/default/Condition", "application/fhir+json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("POST Condition: %v", err)
		}
		defer vr.Body.Close()
		b, _ := io.ReadAll(vr.Body)
		return vr.StatusCode, string(b)
	}

	// Valid federated code -> 201 Created
	if codeStatus, body := validateCond("195967001"); codeStatus != http.StatusCreated {
		t.Fatalf("expected valid federated code 201, got %d: %s", codeStatus, body)
	}
	// Invalid federated code -> 400/422 rejected by required binding
	if codeStatus, body := validateCond("99999999"); codeStatus < 400 {
		t.Fatalf("expected invalid federated code rejected, got %d: %s", codeStatus, body)
	}

	// Simulate upstream outage: FailOpen=true (default) passes uncached code open-world
	txCli.PurgeCache()
	force503.Store(true)
	if codeStatus, body := validateCond("88888888"); codeStatus != http.StatusCreated {
		t.Fatalf("expected FailOpen=true to allow write during 503 outage, got %d: %s", codeStatus, body)
	}

	// Switch to FailOpen=false -> fails closed during 503 outage
	closedCli, err := txclient.New(txclient.Config{
		BaseURL:  upstream.URL + "/r4",
		Timeout:  2 * time.Second,
		FailOpen: txclient.BoolPtr(false),
	})
	if err != nil {
		t.Fatalf("closedCli: %v", err)
	}
	srv.SetTerminologyClient(closedCli)
	if codeStatus, body := validateCond("77777777"); codeStatus < 400 {
		t.Fatalf("expected FailOpen=false to reject write during 503 outage, got %d: %s", codeStatus, body)
	}
}
