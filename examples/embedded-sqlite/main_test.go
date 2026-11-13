package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flint-fhir/flint/server"
)

func TestEmbeddedServer_InMemoryEndToEnd(t *testing.T) {
	ctx := context.Background()
	srv, cleanup, err := NewEmbeddedServer(ctx, Config{})
	if err != nil {
		t.Fatalf("NewEmbeddedServer(:memory:): %v", err)
	}
	defer func() {
		if closeErr := cleanup(); closeErr != nil {
			t.Fatalf("cleanup: %v", closeErr)
		}
	}()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. CapabilityStatement (/metadata)
	metaResp, err := http.Get(ts.URL + "/fhir/r4/demo/metadata")
	if err != nil {
		t.Fatalf("GET /metadata: %v", err)
	}
	defer metaResp.Body.Close()
	if metaResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metadata status = %d, want 200", metaResp.StatusCode)
	}
	var capStmt map[string]any
	if err := json.NewDecoder(metaResp.Body).Decode(&capStmt); err != nil {
		t.Fatalf("decode CapabilityStatement: %v", err)
	}
	if capStmt["fhirVersion"] != "4.0.1" {
		t.Fatalf("fhirVersion = %v, want 4.0.1", capStmt["fhirVersion"])
	}

	// 2. Strict referential integrity blocks dangling Patient reference (422)
	danglingObs := `{"resourceType":"Observation","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/missing-patient"}}`
	badResp, err := http.Post(ts.URL+"/fhir/r4/demo/Observation", "application/fhir+json", strings.NewReader(danglingObs))
	if err != nil {
		t.Fatalf("POST dangling Observation: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("dangling Observation status = %d, want 422", badResp.StatusCode)
	}

	// 3. Create Patient + Vital-Signs Observation
	patientJSON := `{"resourceType":"Patient","id":"pat-embedded-1","active":true,"name":[{"use":"official","family":"Hopper","given":["Grace"]}],"gender":"female","birthDate":"1906-12-09"}`
	patReq, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/demo/Patient/pat-embedded-1", strings.NewReader(patientJSON))
	if err != nil {
		t.Fatalf("new PUT Patient req: %v", err)
	}
	patReq.Header.Set("Content-Type", "application/fhir+json")
	patResp, err := http.DefaultClient.Do(patReq)
	if err != nil {
		t.Fatalf("PUT Patient: %v", err)
	}
	_ = patResp.Body.Close()
	if patResp.StatusCode != http.StatusCreated && patResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT Patient status = %d, want 201/200", patResp.StatusCode)
	}

	obsJSON := `{
		"resourceType": "Observation",
		"id": "obs-hr-1",
		"status": "final",
		"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]},
		"subject": {"reference": "Patient/pat-embedded-1"},
		"effectiveDateTime": "2026-01-15T10:00:00Z",
		"valueQuantity": {"value": 72, "unit": "beats/minute", "system": "http://unitsofmeasure.org", "code": "/min"}
	}`
	obsReq, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/demo/Observation/obs-hr-1", strings.NewReader(obsJSON))
	if err != nil {
		t.Fatalf("new PUT Observation req: %v", err)
	}
	obsReq.Header.Set("Content-Type", "application/fhir+json")
	obsResp, err := http.DefaultClient.Do(obsReq)
	if err != nil {
		t.Fatalf("PUT Observation: %v", err)
	}
	_ = obsResp.Body.Close()
	if obsResp.StatusCode != http.StatusCreated && obsResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT Observation status = %d, want 201/200", obsResp.StatusCode)
	}

	// 4. Patient/$everything longitudinal compartment retrieval
	evResp, err := http.Get(ts.URL + "/fhir/r4/demo/Patient/pat-embedded-1/$everything")
	if err != nil {
		t.Fatalf("GET Patient/$everything: %v", err)
	}
	defer evResp.Body.Close()
	if evResp.StatusCode != http.StatusOK {
		t.Fatalf("Patient/$everything status = %d, want 200", evResp.StatusCode)
	}
	var bundle struct {
		Entry []map[string]any `json:"entry"`
	}
	if err := json.NewDecoder(evResp.Body).Decode(&bundle); err != nil {
		t.Fatalf("decode $everything bundle: %v", err)
	}
	if len(bundle.Entry) != 2 {
		t.Fatalf("$everything entries = %d, want 2", len(bundle.Entry))
	}

	// 5. HL7 SQL-on-FHIR v2 ViewDefinition/$run over embedded SQLite
	viewParams := `{
		"resourceType": "Parameters",
		"parameter": [{
			"name": "viewResource",
			"resource": {
				"resourceType": "ViewDefinition",
				"status": "active",
				"resource": "Patient",
				"select": [{
					"column": [
						{"name": "id", "path": "getResourceKey()"},
						{"name": "family", "path": "name.family.first()"}
					]
				}]
			}
		}]
	}`
	vdResp, err := http.Post(ts.URL+"/fhir/r4/demo/ViewDefinition/$run", "application/fhir+json", strings.NewReader(viewParams))
	if err != nil {
		t.Fatalf("POST ViewDefinition/$run: %v", err)
	}
	defer vdResp.Body.Close()
	if vdResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(vdResp.Body)
		t.Fatalf("ViewDefinition/$run status = %d (%s), want 200", vdResp.StatusCode, string(body))
	}
	rawRows, err := io.ReadAll(vdResp.Body)
	if err != nil {
		t.Fatalf("read ViewDefinition/$run body: %v", err)
	}
	if !strings.Contains(string(rawRows), "Hopper") {
		t.Fatalf("ViewDefinition/$run output %q missing projected family name Hopper", string(rawRows))
	}

	// 6. Strict delete conflict (409) & cascade delete (?_cascade=delete)
	delReq, err := http.NewRequest(http.MethodDelete, ts.URL+"/fhir/r4/demo/Patient/pat-embedded-1", nil)
	if err != nil {
		t.Fatalf("new DELETE req: %v", err)
	}
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE Patient without cascade: %v", err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusConflict {
		t.Fatalf("DELETE referenced Patient status = %d, want 409 Conflict", delResp.StatusCode)
	}

	cascadeReq, err := http.NewRequest(http.MethodDelete, ts.URL+"/fhir/r4/demo/Patient/pat-embedded-1?_cascade=delete", nil)
	if err != nil {
		t.Fatalf("new cascade DELETE req: %v", err)
	}
	cascadeResp, err := http.DefaultClient.Do(cascadeReq)
	if err != nil {
		t.Fatalf("DELETE Patient?_cascade=delete: %v", err)
	}
	_ = cascadeResp.Body.Close()
	if cascadeResp.StatusCode != http.StatusOK && cascadeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("cascade DELETE status = %d, want 200/204", cascadeResp.StatusCode)
	}
}

func TestEmbeddedServer_PersistentWALReload(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "flint-wal-test.db")

	// Boot 1: write Patient to SQLite WAL file and shut down cleanly.
	srv1, cleanup1, err := NewEmbeddedServer(ctx, Config{
		DSN:                  dbPath,
		ReferentialIntegrity: server.ReferentialIntegrityStrict,
	})
	if err != nil {
		t.Fatalf("boot 1 NewEmbeddedServer(%q): %v", dbPath, err)
	}
	req := httptest.NewRequest(http.MethodPut, "/fhir/r4/demo/Patient/pat-wal-1", strings.NewReader(`{"resourceType":"Patient","id":"pat-wal-1","active":true,"name":[{"family":"Lovelace"}]}`))
	req.Header.Set("Content-Type", "application/fhir+json")
	rec := httptest.NewRecorder()
	srv1.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("boot 1 PUT status = %d, want 201/200", rec.Code)
	}
	if err := cleanup1(); err != nil {
		t.Fatalf("boot 1 cleanup: %v", err)
	}

	// Boot 2: reopen the same SQLite WAL file and verify Proto3 state reloaded.
	srv2, cleanup2, err := NewEmbeddedServer(ctx, Config{
		DSN:                  dbPath,
		ReferentialIntegrity: server.ReferentialIntegrityStrict,
	})
	if err != nil {
		t.Fatalf("boot 2 NewEmbeddedServer(%q): %v", dbPath, err)
	}
	defer func() {
		_ = cleanup2()
	}()

	getReq := httptest.NewRequest(http.MethodGet, "/fhir/r4/demo/Patient/pat-wal-1", nil)
	getRec := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("boot 2 GET Patient/pat-wal-1 status = %d, want 200 (%s)", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), "Lovelace") {
		t.Fatalf("boot 2 GET response %q missing Lovelace", getRec.Body.String())
	}
}

func TestRunCLIAndServeHTTP(t *testing.T) {
	var out bytes.Buffer
	var capturedAddr string
	err := run(
		context.Background(),
		[]string{"-addr", "127.0.0.1:19090", "-dsn", ":memory:", "-referential-integrity", "lenient"},
		&out,
		func(_ context.Context, addr string, handler http.Handler) error {
			capturedAddr = addr
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("/readyz status = %d, want 200", rec.Code)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("run valid flags: %v", err)
	}
	if capturedAddr != "127.0.0.1:19090" {
		t.Fatalf("capturedAddr = %q, want 127.0.0.1:19090", capturedAddr)
	}
	if !strings.Contains(out.String(), "CapabilityStatement") {
		t.Fatalf("banner output missing CapabilityStatement link: %q", out.String())
	}

	// Error cases: invalid flag, invalid referential integrity mode, invalid SQLite path, and invalid TCP address
	if err := run(context.Background(), []string{"-unknown-flag"}, io.Discard, nil); err == nil {
		t.Fatal("expected error for unknown CLI flag")
	}
	if err := run(context.Background(), []string{"-referential-integrity", "invalid-mode"}, io.Discard, nil); err == nil {
		t.Fatal("expected error for invalid referential integrity mode")
	}
	badDBPath := filepath.Join(t.TempDir(), "nonexistent-dir", "sub", "bad.db")
	if err := run(context.Background(), []string{"-dsn", badDBPath}, io.Discard, nil); err == nil {
		t.Fatal("expected error for unwritable SQLite path")
	}
	if err := serveHTTP(context.Background(), "127.0.0.1:99999", http.NotFoundHandler(), nil); err == nil {
		t.Fatal("expected error for invalid TCP port")
	}

	// Verify serveHTTP starts a live TCP listener and shuts down cleanly on context cancel.
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), ready)
	}()

	boundAddr := <-ready
	resp, err := http.Get("http://" + boundAddr + "/health")
	if err != nil {
		cancel()
		t.Fatalf("GET live serveHTTP listener: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("live serveHTTP status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case serveErr := <-done:
		if serveErr != nil {
			t.Fatalf("serveHTTP graceful shutdown returned error: %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for serveHTTP shutdown")
	}
}
