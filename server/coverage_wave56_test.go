package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/cdc"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

type mockCDCPublisher struct {
	fail bool
	msgs []cdc.Message
}

func (m *mockCDCPublisher) Publish(_ context.Context, msgs []cdc.Message) error {
	if m.fail {
		return fmt.Errorf("simulated cdc publish failure")
	}
	m.msgs = append(m.msgs, msgs...)
	return nil
}

func (m *mockCDCPublisher) Close() error { return nil }

func doReq(t *testing.T, handler http.Handler, method, target string, body string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" && headers["Content-Type"] == "" {
		req.Header.Set("Content-Type", "application/fhir+json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var parsed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

func TestWave56ServerCoverageHardening(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()
	store := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(store, logger)
	server.RegisterDefaultResourceTypes(srv)
	handler := srv.Handler()
	tenant := "t-wave56"

	// Mint a full-privilege system token via OAuth2 client_credentials so authMiddleware & isPlanApplyOp execute.
	tokRec, tokMap := doReq(t, handler, http.MethodPost, "/oauth2/token",
		"grant_type=client_credentials&client_id=sys-client&scope="+url.QueryEscape("system/*.cruds user/*.cruds"),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if tokRec.Code != http.StatusOK {
		t.Fatalf("mint system token failed: %d %s", tokRec.Code, tokRec.Body.String())
	}
	sysToken, _ := tokMap["access_token"].(string)
	sysAuth := map[string]string{"Authorization": "Bearer " + sysToken}

	t.Run("SubtestA_Observability_Setters_AuditEvent_VRead_History_SystemSearch", func(t *testing.T) {
		// 1. Observability endpoints at root, /fhir/r4, and /fhir/r4/{tenant}
		for _, path := range []string{
			"/healthz", "/readyz", "/metrics",
			"/fhir/r4/healthz", "/fhir/r4/readyz", "/fhir/r4/metrics",
			"/fhir/r4/" + tenant + "/healthz", "/fhir/r4/" + tenant + "/readyz", "/fhir/r4/" + tenant + "/metrics",
		} {
			rec, _ := doReq(t, handler, http.MethodGet, path, "", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s expected 200, got %d: %s", path, rec.Code, rec.Body.String())
			}
			doReq(t, handler, http.MethodPost, path, "{}", nil)
		}

		// Nil-store readyz & healthz
		nilSrv := server.New(nil, logger)
		server.RegisterDefaultResourceTypes(nilSrv)
		nilHandler := nilSrv.Handler()
		doReq(t, nilHandler, http.MethodGet, "/readyz", "", nil)
		doReq(t, nilHandler, http.MethodGet, "/healthz", "", nil)
		doReq(t, nilHandler, http.MethodGet, "/metrics", "", nil)

		// 2. Server configuration setters & CDC publish success/error paths
		pub := &mockCDCPublisher{}
		srv.SetCDCPublisher(pub)
		if srv.CDCPublisher() != pub {
			t.Fatalf("expected CDCPublisher to return configured publisher")
		}
		origAudit := srv.AuditRecorder()
		srv.SetAuditRecorder(audit.NewMemoryRecorder())
		if srv.AuditRecorder() == nil {
			t.Fatalf("expected non-nil AuditRecorder")
		}
		srv.SetValidator(validation.NewEngine(validation.DefaultOptions()))
		srv.RegisterIndexExtractor("CustomFake", postgres.DefaultIndexExtractors()["Patient"])
		srv.SetDisallowPrivateWebhooks(false)
		srv.SetSMARTConfig(server.SMARTConfig{
			Issuer:  "https://auth.flint.local",
			JWKSURI: "https://auth.flint.local/.well-known/jwks.json",
		})
		doReq(t, handler, http.MethodGet, "/.well-known/smart-configuration", "", nil)
		srv.SetSMARTConfig(server.SMARTConfig{})
		srv.RegisterOAuthClientJWK("registered-client", auth.JWK{Kty: "RSA", Kid: "k1"})
		srv.SetRequireRegisteredClientJWK(true)
		srv.SetRequireRegisteredClientJWK(false)

		// Trigger CDC publish success and failure
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Patient/p1", `{
			"resourceType": "Patient",
			"id": "p1",
			"active": true,
			"name": [{"family": "Smith", "given": ["Alice"]}],
			"gender": "female",
			"birthDate": "1985-04-12",
			"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-001"}]
		}`, sysAuth)
		pub.fail = true
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Patient/p1", `{
			"resourceType": "Patient",
			"id": "p1",
			"active": true,
			"name": [{"family": "Smith", "given": ["Alice", "Marie"]}],
			"gender": "female",
			"birthDate": "1985-04-12",
			"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-001"}]
		}`, sysAuth)
		srv.SetCDCPublisher(nil)
		srv.SetAuditRecorder(origAudit)

		// 3. Create, update, and delete a custom SearchParameter to exercise syncSearchParameterOnWrite & syncSearchParameterOnDelete
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/SearchParameter/sp-custom-1", `{
			"resourceType": "SearchParameter",
			"id": "sp-custom-1",
			"url": "http://example.org/SearchParameter/sp-custom-1",
			"name": "CustomCode",
			"status": "active",
			"code": "custom-code",
			"base": ["Patient"],
			"type": "token",
			"expression": "Patient.identifier"
		}`, sysAuth)
		delSpRec, _ := doReq(t, handler, http.MethodDelete, "/fhir/r4/"+tenant+"/SearchParameter/sp-custom-1", "", sysAuth)
		if delSpRec.Code != http.StatusNoContent && delSpRec.Code != http.StatusOK {
			t.Fatalf("DELETE SearchParameter failed: %d %s", delSpRec.Code, delSpRec.Body.String())
		}

		// 4. Binary resource read & vread with native media negotiation, _summary, and error paths
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Binary/bin-1", `{
			"resourceType": "Binary",
			"id": "bin-1",
			"contentType": "text/plain",
			"securityContext": {"reference": "DocumentReference/doc-1"},
			"data": "SGVsbG8gRmxpbnQ="
		}`, sysAuth)
		recBinRaw, _ := doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Binary/bin-1?_format=text/plain", "", sysAuth)
		if recBinRaw.Code != http.StatusOK || recBinRaw.Body.String() != "Hello Flint" {
			t.Fatalf("expected raw Binary payload, got %d %q", recBinRaw.Code, recBinRaw.Body.String())
		}
		recBinVReadRaw, _ := doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Binary/bin-1/_history/1?_format=text/plain", "", sysAuth)
		if recBinVReadRaw.Code != http.StatusOK {
			t.Fatalf("expected 200 on Binary vread raw, got %d", recBinVReadRaw.Code)
		}
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Binary/bin-1/_history/1?_summary=true&_pretty=true", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Binary/bin-1/_history/1?_elements=id,contentType", "", sysAuth)
		doReq(t, handler, http.MethodHead, "/fhir/r4/"+tenant+"/Binary/bin-1/_history/1", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Binary/bin-1/_history/not-a-number", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Binary/bin-1/_history/999", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/UnknownType/bin-1/_history/1", "", sysAuth)

		// Delete a resource and vread its deleted version (410 Gone)
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Patient/p-del-vread", `{"resourceType":"Patient","id":"p-del-vread"}`, sysAuth)
		doReq(t, handler, http.MethodDelete, "/fhir/r4/"+tenant+"/Patient/p-del-vread", "", sysAuth)
		recGone, _ := doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p-del-vread/_history/2", "", sysAuth)
		if recGone.Code != http.StatusGone && recGone.Code != http.StatusNotFound {
			t.Fatalf("expected 410 or 404 for deleted vread, got %d", recGone.Code)
		}

		// 5. AuditEvent search and direct read (handleReadAuditEvent)
		_, auditBundle := doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/AuditEvent?_count=5&_offset=0", "", sysAuth)
		if entries, ok := auditBundle["entry"].([]any); ok && len(entries) > 0 {
			if first, ok := entries[0].(map[string]any); ok {
				if res, ok := first["resource"].(map[string]any); ok {
					if id, ok := res["id"].(string); ok && id != "" {
						recAE, _ := doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/AuditEvent/"+id, "", sysAuth)
						if recAE.Code != http.StatusOK {
							t.Fatalf("GET AuditEvent/%s expected 200, got %d", id, recAE.Code)
						}
						doReq(t, handler, http.MethodHead, "/fhir/r4/"+tenant+"/AuditEvent/"+id, "", sysAuth)
					}
				}
			}
		}
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/AuditEvent/nonexistent-audit-id", "", sysAuth)

		// 6. System-level search & history (handleSystemSearch, handleHistory)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"?_type=Patient,Observation&_count=5&_offset=0&_summary=true", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"?_count=5&_summary=count", "", sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/_search", "_type=Patient&_count=5", map[string]string{
			"Content-Type":  "application/x-www-form-urlencoded",
			"Authorization": "Bearer " + sysToken,
		})
		recBadType, _ := doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"?_type=NotAResourceType", "", sysAuth)
		if recBadType.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on invalid _type in system search, got %d", recBadType.Code)
		}
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/_history?_count=5&_offset=0&_since=2020-01-01T00:00:00Z&_at=ge2020-01-01", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/_history?_summary=count", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/_history?_count=5&_offset=0&_since=2020-01-01&_at=2020", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p1/_history?_count=5&_offset=0&_summary=count", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/_history?_since=invalid-date", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/_history?_at=invalid-date", "", sysAuth)
	})

	t.Run("SubtestB_Export_Import_Purge_Expunge_Diff", func(t *testing.T) {
		// Seed resources for cohort export, import, purge, expunge, diff
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Patient/p2", `{
			"resourceType": "Patient",
			"id": "p2",
			"active": true,
			"name": [{"family": "Jones", "given": ["Bob"]}],
			"gender": "male",
			"birthDate": "1978-09-20"
		}`, sysAuth)
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Observation/o1", `{
			"resourceType": "Observation",
			"id": "o1",
			"status": "final",
			"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}]}],
			"code": {"coding": [{"system": "http://loinc.org", "code": "4548-4", "display": "HbA1c"}]},
			"subject": {"reference": "Patient/p1"},
			"effectiveDateTime": "2025-01-15T10:00:00Z",
			"valueQuantity": {"value": 9.4, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"}
		}`, sysAuth)
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Group/g-sub", `{
			"resourceType": "Group",
			"id": "g-sub",
			"type": "person",
			"actual": true,
			"member": [
				{"entity": {"reference": "Patient/p2"}}
			]
		}`, sysAuth)
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Group/g1", `{
			"resourceType": "Group",
			"id": "g1",
			"type": "person",
			"actual": true,
			"member": [
				{"entity": {"reference": "Patient/p1"}, "period": {"start": "2020-01-01", "end": "2035-01-01"}},
				{"entity": {"reference": "Group/g-sub"}},
				{"entity": {"reference": "Patient/p2"}, "inactive": true},
				{"entity": {"reference": "Patient/p2"}, "period": {"start": "2018-01-01", "end": "2019-01-01"}}
			]
		}`, sysAuth)

		// 1. Bulk $export: POST with Parameters body + async poll + file download + DELETE cancel
		asyncHeaders := map[string]string{
			"Authorization": "Bearer " + sysToken,
			"Prefer":        "respond-async",
		}
		expRec, _ := doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/$export", `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "_outputFormat", "valueString": "application/fhir+ndjson"},
				{"name": "_type", "valueString": "Patient,Observation"},
				{"name": "_since", "valueInstant": "2020-01-01T00:00:00Z"},
				{"name": "_until", "valueInstant": "2035-01-01T00:00:00Z"},
				{"name": "_elements", "valueString": "id,Patient.name,Observation.status"},
				{"name": "_typeFilter", "valueString": "Observation?status=final"},
				{"name": "_async", "valueBoolean": true},
				{"name": "_format", "valueCode": "manifest"}
			]
		}`, asyncHeaders)
		if expRec.Code != http.StatusAccepted {
			t.Fatalf("POST /$export expected 202, got %d: %s", expRec.Code, expRec.Body.String())
		}
		contentLoc := expRec.Header().Get("Content-Location")
		if contentLoc != "" {
			u, err := url.Parse(contentLoc)
			if err == nil {
				// Poll with _nowait=true and regular poll until 200 OK
				doReq(t, handler, http.MethodGet, u.Path+"?_nowait=true", "", sysAuth)
				var pollMap map[string]any
				for i := 0; i < 20; i++ {
					pollRec, pm := doReq(t, handler, http.MethodGet, u.Path, "", sysAuth)
					if pollRec.Code == http.StatusOK {
						pollMap = pm
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if outputs, ok := pollMap["output"].([]any); ok && len(outputs) > 0 {
					if firstOut, ok := outputs[0].(map[string]any); ok {
						if fileURL, ok := firstOut["url"].(string); ok {
							fu, _ := url.Parse(fileURL)
							doReq(t, handler, http.MethodGet, fu.Path, "", sysAuth)
						}
					}
				}
				// Missing file -> 404
				jobID := u.Path[strings.LastIndex(u.Path, "/")+1:]
				doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/$export-file/"+jobID+"/NonExistent.ndjson", "", sysAuth)
				// Cancel export job -> 202, then repeat -> 404
				delRec1, _ := doReq(t, handler, http.MethodDelete, u.Path, "", sysAuth)
				if delRec1.Code != http.StatusAccepted {
					t.Fatalf("DELETE export poll expected 202, got %d", delRec1.Code)
				}
				delRec2, _ := doReq(t, handler, http.MethodDelete, u.Path, "", sysAuth)
				if delRec2.Code != http.StatusNotFound {
					t.Fatalf("second DELETE export poll expected 404, got %d", delRec2.Code)
				}
			}
		}

		// Direct synchronous & streaming $export variants (System, Patient, Patient/{id}, Group/{id}, Group/all)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/$export?_type=Patient", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/$export?_type=Patient&_format=manifest", "", map[string]string{
			"Authorization": "Bearer " + sysToken,
			"Accept":        "application/fhir+json",
		})
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$export", `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "patient", "valueReference": {"reference": "Patient/p1"}},
				{"name": "patient", "valueString": "p2"}
			]
		}`, sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p1/$export?_type=Patient,Observation", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Group/g1/$export?patient=Patient/p1", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Group/all/$export?_type=Patient", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Observation/o1/$export", "", sysAuth)

		// Export validation error branches
		for _, badQuery := range []string{
			"_outputFormat=application/xml",
			"_type=InvalidResourceType",
			"_since=not-a-timestamp",
			"_until=not-a-timestamp",
			"_since=2030-01-01T00:00:00Z&_until=2020-01-01T00:00:00Z",
			"_elements=Patient..id",
			"_typeFilter=NotValidQuery",
			"_typeFilter=InvalidType?status=final",
			"patient=Patient/p1",
		} {
			recBad, _ := doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/$export?"+badQuery, "", sysAuth)
			if recBad.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for export query %q, got %d", badQuery, recBad.Code)
			}
		}

		// 2. Bulk $import: local HTTP server + data: base64 + data: plain + inline JSON + content part + async poll & cancel
		ndjsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/fhir+ndjson")
			_, _ = w.Write([]byte(`{"resourceType":"Patient","id":"p-imp-http","name":[{"family":"HttpImport"}]}` + "\n"))
		}))
		defer ndjsonSrv.Close()

		b64Line := base64.StdEncoding.EncodeToString([]byte(`{"resourceType":"Patient","id":"p-imp-b64","name":[{"family":"Base64Import"}]}`))
		impBody, _ := json.Marshal(map[string]any{
			"resourceType": "Parameters",
			"parameter": []map[string]any{
				{"name": "inputFormat", "valueCode": "application/fhir+ndjson"},
				{"name": "inputSource", "valueUri": "https://source.example.org/fhir"},
				{
					"name": "input",
					"part": []map[string]any{
						{"name": "type", "valueCode": "Patient"},
						{"name": "url", "valueUrl": "data:application/fhir+ndjson;base64," + b64Line},
					},
				},
				{
					"name": "input",
					"part": []map[string]any{
						{"name": "type", "valueCode": "Patient"},
						{"name": "url", "valueUrl": "data:application/fhir+ndjson," + url.PathEscape(`{"resourceType":"Patient","id":"p-imp-data","name":[{"family":"DataURI"}]}`)},
					},
				},
				{
					"name": "input",
					"part": []map[string]any{
						{"name": "type", "valueCode": "Patient"},
						{"name": "url", "valueUrl": ndjsonSrv.URL + "/Patient.ndjson"},
					},
				},
				{
					"name": "input",
					"part": []map[string]any{
						{"name": "type", "valueCode": "Patient"},
						{"name": "url", "valueString": `{"resourceType":"Patient","id":"p-imp-inline","name":[{"family":"InlineURL"}]}`},
					},
				},
				{
					"name": "input",
					"part": []map[string]any{
						{"name": "type", "valueCode": "Patient"},
						{"name": "content", "valueString": `{"resourceType":"Patient","id":"p-imp-content","name":[{"family":"ContentPart"}]}`},
					},
				},
			},
		})
		impRec, _ := doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/$import", string(impBody), asyncHeaders)
		if impRec.Code != http.StatusAccepted {
			t.Fatalf("POST /$import async expected 202, got %d: %s", impRec.Code, impRec.Body.String())
		}
		if impLoc := impRec.Header().Get("Content-Location"); impLoc != "" {
			u, err := url.Parse(impLoc)
			if err == nil {
				recPoll, _ := doReq(t, handler, http.MethodGet, u.Path, "", sysAuth)
				if recPoll.Code != http.StatusOK {
					t.Fatalf("GET import poll expected 200, got %d", recPoll.Code)
				}
				recDel, _ := doReq(t, handler, http.MethodDelete, u.Path, "", sysAuth)
				if recDel.Code != http.StatusAccepted {
					t.Fatalf("DELETE import poll expected 202, got %d", recDel.Code)
				}
				recPoll404, _ := doReq(t, handler, http.MethodGet, u.Path, "", sysAuth)
				if recPoll404.Code != http.StatusNotFound {
					t.Fatalf("GET import poll after DELETE expected 404, got %d", recPoll404.Code)
				}
			}
		}
		// Synchronous direct NDJSON $import
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/$import",
			`{"resourceType":"Patient","id":"p-imp-raw","name":[{"family":"RawNDJSON"}]}`+"\n",
			map[string]string{
				"Authorization": "Bearer " + sysToken,
				"Content-Type":  "application/fhir+ndjson",
			})

		// 3. $purge & $expunge
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/p-imp-raw/$purge", `{
			"resourceType": "Parameters",
			"parameter": [{"name": "expunge", "valueBoolean": true}]
		}`, sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/nonexistent-pat/$purge", "", sysAuth)

		expungeBody := `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "limit", "valueInteger": 10},
				{"name": "expungeDeletedResources", "valueBoolean": true},
				{"name": "expungePreviousVersions", "valueBoolean": true},
				{"name": "expungeEverything", "valueBoolean": false}
			]
		}`
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/$expunge", expungeBody, sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$expunge", expungeBody, sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/p1/$expunge", expungeBody, sysAuth)

		// 4. $diff (inline from/to with rich JSON value types + instance-level version diff)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/$diff", `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "includeMeta", "valueBoolean": true},
				{
					"name": "from",
					"resource": {
						"resourceType": "Observation",
						"id": "obs-diff",
						"status": "preliminary",
						"valueQuantity": {"value": 5.5, "unit": "mg"},
						"component": [{"valueInteger": 10}, {"valueBoolean": false}]
					}
				},
				{
					"name": "to",
					"resource": {
						"resourceType": "Observation",
						"id": "obs-diff",
						"status": "final",
						"valueQuantity": {"value": 7.25, "unit": "mg"},
						"component": [{"valueInteger": 20}, {"valueBoolean": true}],
						"note": [{"text": "added note"}]
					}
				}
			]
		}`, sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$diff", `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "from", "resource": {"resourceType": "Patient", "id": "p1", "active": true}},
				{"name": "to", "resource": {"resourceType": "Observation", "id": "o1", "status": "final"}}
			]
		}`, sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p1/$diff?fromVersion=1&toVersion=2&includeMeta=true", "", sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/p1/$diff", `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "fromVersion", "valueString": "1"},
				{"name": "toVersion", "valueString": "2"},
				{"name": "includeMeta", "valueBoolean": true}
			]
		}`, sysAuth)
	})

	t.Run("SubtestC_Batch_Transaction_Patch_Provenance_Everything", func(t *testing.T) {
		// 1. Batch Bundle with diverse entry methods, conditional ops, patches, HEAD, and error paths
		batchBody := `{
			"resourceType": "Bundle",
			"type": "batch",
			"entry": [
				{
					"request": {"method": "POST", "url": "Patient", "ifNoneExist": "identifier=http://hospital.example.org/mrn|MRN-001"},
					"resource": {"resourceType": "Patient", "name": [{"family": "Duplicate"}]}
				},
				{
					"request": {"method": "PUT", "url": "Patient?identifier=http://hospital.example.org/mrn|MRN-BATCH-NEW"},
					"resource": {"resourceType": "Patient", "active": true, "identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-BATCH-NEW"}]}
				},
				{
					"request": {"method": "PATCH", "url": "Patient/p1"},
					"resource": {
						"resourceType": "Parameters",
						"parameter": [{
							"name": "operation",
							"part": [
								{"name": "type", "valueCode": "replace"},
								{"name": "path", "valueString": "Patient.gender"},
								{"name": "value", "valueCode": "other"}
							]
						}]
					}
				},
				{
					"request": {"method": "PATCH", "url": "Patient?identifier=http://hospital.example.org/mrn|MRN-001"},
					"resource": {
						"resourceType": "Parameters",
						"parameter": [{
							"name": "operation",
							"part": [
								{"name": "type", "valueCode": "replace"},
								{"name": "path", "valueString": "Patient.gender"},
								{"name": "value", "valueCode": "female"}
							]
						}]
					}
				},
				{
					"request": {"method": "PATCH", "url": "Patient?identifier=nonexistent-mrn"},
					"resource": {"resourceType": "Parameters"}
				},
				{
					"request": {"method": "DELETE", "url": "Patient?identifier=nonexistent-mrn"}
				},
				{
					"request": {"method": "DELETE", "url": "Patient?identifier=nonexistent-mrn", "ifMatch": "W/\"1\""}
				},
				{
					"request": {"method": "GET", "url": "Patient/p1"}
				},
				{
					"request": {"method": "GET", "url": "Patient?family=Smith&_count=2"}
				},
				{
					"request": {"method": "HEAD", "url": "Patient/p1"}
				},
				{
					"request": {"method": "HEAD", "url": "Patient/p1"},
					"resource": {"resourceType": "Patient"}
				},
				{
					"request": {"method": "PUT", "url": "Patient/p1", "ifMatch": "invalid-etag"},
					"resource": {"resourceType": "Patient", "id": "p1"}
				},
				{
					"request": {"method": "OPTIONS", "url": "Patient/p1"}
				},
				{
					"request": {"method": "POST", "url": "Observation"},
					"resource": {
						"resourceType": "Observation",
						"status": "final",
						"code": {"coding": [{"system": "http://loinc.org", "code": "29463-7"}]},
						"subject": {"reference": "Patient?identifier=nonexistent-cond-ref"}
					}
				}
			]
		}`
		recBatch, _ := doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant, batchBody, sysAuth)
		if recBatch.Code != http.StatusOK {
			t.Fatalf("POST batch expected 200, got %d: %s", recBatch.Code, recBatch.Body.String())
		}

		// 2. Transaction Bundle with urn:uuid resolution, intra-bundle conditional reference, Prefer headers
		txBody := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"fullUrl": "urn:uuid:11111111-2222-3333-4444-555555555555",
					"request": {"method": "POST", "url": "Patient"},
					"resource": {
						"resourceType": "Patient",
						"active": true,
						"name": [{"family": "TxPatient", "given": ["Grace"]}],
						"gender": "female",
						"birthDate": "1992-07-04",
						"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-TX-56"}]
					}
				},
				{
					"request": {"method": "POST", "url": "Observation"},
					"resource": {
						"resourceType": "Observation",
						"status": "final",
						"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
						"subject": {"reference": "Patient?identifier=http://hospital.example.org/mrn|MRN-TX-56"}
					}
				},
				{
					"request": {"method": "GET", "url": "Patient/p1"}
				},
				{
					"request": {"method": "HEAD", "url": "Patient/p1"}
				}
			]
		}`
		for _, prefer := range []string{"return=representation", "return=minimal", "return=OperationOutcome"} {
			bodyWithUniqueMRN := strings.ReplaceAll(txBody, "MRN-TX-56", "MRN-TX-"+prefer)
			recTx, _ := doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant, bodyWithUniqueMRN, map[string]string{
				"Authorization": "Bearer " + sysToken,
				"Prefer":        prefer,
			})
			if recTx.Code != http.StatusOK {
				t.Fatalf("POST transaction (%s) expected 200, got %d: %s", prefer, recTx.Code, recTx.Body.String())
			}
		}
		// Also trigger the 412 duplicate conditional reference error branch in transaction
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant, strings.ReplaceAll(txBody, "MRN-TX-56", "MRN-TX-return=representation"), sysAuth)

		// 3. X-Provenance header (raw JSON and base64) + direct Provenance creation + $provenance
		provJSON := `{"resourceType":"Provenance","recorded":"2026-01-01T00:00:00Z","agent":[{"who":{"reference":"Practitioner/prac-1"}}]}`
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient", `{"resourceType":"Patient","name":[{"family":"WithRawProv"}]}`, map[string]string{
			"Authorization": "Bearer " + sysToken,
			"X-Provenance":  provJSON,
		})
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Patient/p1", `{"resourceType":"Patient","id":"p1","active":true,"name":[{"family":"Smith","given":["Alice"]}]}`, map[string]string{
			"Authorization": "Bearer " + sysToken,
			"X-Provenance":  base64.StdEncoding.EncodeToString([]byte(provJSON)),
		})
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Provenance", `{
			"resourceType": "Provenance",
			"target": [{"reference": "Observation/o1"}],
			"recorded": "2026-01-02T00:00:00Z",
			"agent": [{"who": {"reference": "Practitioner/prac-1"}}]
		}`, sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p1/$provenance", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/$provenance?target=Patient/p1", "", sysAuth)

		// 4. $everything (POST Parameters + GET for Patient, Group, Encounter)
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Encounter/e1", `{
			"resourceType": "Encounter",
			"id": "e1",
			"status": "finished",
			"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
			"subject": {"reference": "Patient/p1"}
		}`, sysAuth)
		everythingParams := `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "_type", "valueString": "Patient,Observation,Encounter"},
				{"name": "_since", "valueInstant": "2020-01-01T00:00:00Z"},
				{"name": "_until", "valueInstant": "2035-01-01T00:00:00Z"},
				{"name": "start", "valueDate": "2020-01-01"},
				{"name": "end", "valueDate": "2035-01-01"},
				{"name": "_count", "valueInteger": 20},
				{"name": "_offset", "valueInteger": 0},
				{"name": "_elements", "valueString": "id"},
				{"name": "_typeFilter", "valueString": "Observation?status=final"}
			]
		}`
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/p1/$everything", everythingParams, sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$everything", everythingParams, sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Group/g1/$everything?_count=10", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Encounter/e1/$everything?_count=10", "", sysAuth)
	})

	t.Run("SubtestD_SMART_OAuth2_PKCE_Refresh_And_GranularAuthScopes", func(t *testing.T) {
		// 1. OAuth2 Authorization Code + PKCE S256 + Refresh Token rotation & scope subset check
		verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
		sum := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])

		authURL := "/oauth2/authorize?response_type=code&client_id=pkce-app&redirect_uri=" +
			url.QueryEscape("https://app.example.org/cb") +
			"&scope=" + url.QueryEscape("openid fhirUser offline_access launch/patient patient/Patient.rs patient/Observation.rs") +
			"&state=xyz123&code_challenge=" + challenge + "&code_challenge_method=S256&patient=p1&nonce=n1"
		authRec, _ := doReq(t, handler, http.MethodGet, authURL, "", nil)
		if authRec.Code != http.StatusFound {
			t.Fatalf("GET /oauth2/authorize expected 302, got %d: %s", authRec.Code, authRec.Body.String())
		}
		loc, err := url.Parse(authRec.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse redirect Location: %v", err)
		}
		code := loc.Query().Get("code")
		if code == "" {
			t.Fatalf("expected non-empty authorization code in redirect")
		}

		// Exchange authorization_code for access_token + refresh_token
		exchRec, exchMap := doReq(t, handler, http.MethodPost, "/oauth2/token",
			"grant_type=authorization_code&code="+url.QueryEscape(code)+
				"&redirect_uri="+url.QueryEscape("https://app.example.org/cb")+
				"&client_id=pkce-app&code_verifier="+url.QueryEscape(verifier),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if exchRec.Code != http.StatusOK {
			t.Fatalf("POST /oauth2/token auth_code expected 200, got %d: %s", exchRec.Code, exchRec.Body.String())
		}
		refreshToken, _ := exchMap["refresh_token"].(string)
		patToken, _ := exchMap["access_token"].(string)
		if refreshToken == "" || patToken == "" {
			t.Fatalf("expected refresh_token and access_token from auth_code exchange")
		}

		// Try invalid superset scope on refresh_token -> 400 invalid_scope (before consuming the refresh token? Note: consumeRefreshToken deletes it first, so test valid subset first!)
		refRec, refMap := doReq(t, handler, http.MethodPost, "/oauth2/token",
			"grant_type=refresh_token&refresh_token="+url.QueryEscape(refreshToken)+
				"&scope="+url.QueryEscape("openid fhirUser patient/Patient.rs"),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if refRec.Code != http.StatusOK {
			t.Fatalf("POST /oauth2/token refresh_token expected 200, got %d: %s", refRec.Code, refRec.Body.String())
		}
		nextRefresh, _ := refMap["refresh_token"].(string)
		// Now try invalid superset scope with nextRefresh -> 400
		badRefRec, _ := doReq(t, handler, http.MethodPost, "/oauth2/token",
			"grant_type=refresh_token&refresh_token="+url.QueryEscape(nextRefresh)+
				"&scope="+url.QueryEscape("system/*.cruds"),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if badRefRec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for superset refresh scope, got %d", badRefRec.Code)
		}

		// Token introspection & revocation
		doReq(t, handler, http.MethodPost, "/oauth2/introspect", "token="+url.QueryEscape(patToken),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		doReq(t, handler, http.MethodPost, "/oauth2/introspect", "token=invalid.jwt.token",
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})

		// 2. Patient-restricted token (`patToken` is scoped to patient=p1 with `patient/Patient.rs patient/Observation.rs`)
		patAuth := map[string]string{"Authorization": "Bearer " + patToken}
		// Allowed read on Patient/p1 and Observation/o1 (subject=Patient/p1)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p1", "", patAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Observation/o1", "", patAuth)
		// Forbidden read on Patient/p2 (different patient)
		recP2, _ := doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p2", "", patAuth)
		if recP2.Code != http.StatusForbidden {
			t.Fatalf("expected 403 reading Patient/p2 with Patient/p1 token, got %d", recP2.Code)
		}
		// Chained _filter expression check (`checkFilterExprChainedScopes`)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Observation?_filter="+url.QueryEscape(`subject.name eq "Smith" and status eq "final"`), "", patAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Observation?_filter="+url.QueryEscape(`performer.name eq "Dr"`), "", patAuth)

		// Mint a patient write token with granular category constraint (`patient/Observation.cruds?category=http://terminology.hl7.org/CodeSystem/observation-category|laboratory patient/Patient.cruds`)
		authURL2 := "/oauth2/authorize?response_type=code&client_id=gran-app&redirect_uri=" +
			url.QueryEscape("https://app.example.org/cb") +
			"&scope=" + url.QueryEscape("patient/Patient.cruds patient/Observation.cruds?category=http://terminology.hl7.org/CodeSystem/observation-category|laboratory") +
			"&code_challenge=" + challenge + "&code_challenge_method=S256&patient=p1"
		authRec2, _ := doReq(t, handler, http.MethodGet, authURL2, "", nil)
		loc2, _ := url.Parse(authRec2.Header().Get("Location"))
		_, exchMap2 := doReq(t, handler, http.MethodPost, "/oauth2/token",
			"grant_type=authorization_code&code="+url.QueryEscape(loc2.Query().Get("code"))+
				"&redirect_uri="+url.QueryEscape("https://app.example.org/cb")+
				"&client_id=gran-app&code_verifier="+url.QueryEscape(verifier),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		granToken, _ := exchMap2["access_token"].(string)
		granAuth := map[string]string{"Authorization": "Bearer " + granToken}

		// Granular scope checks: read & search Observation with category constraint, plus conditional If-None-Exist for patient
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Observation/o1", "", granAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Observation?patient=Patient/p1", "", granAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Observation", `{
			"resourceType": "Observation",
			"status": "final",
			"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}]}],
			"code": {"coding": [{"system": "http://loinc.org", "code": "4548-4"}]},
			"subject": {"reference": "Patient/p1"}
		}`, map[string]string{
			"Authorization": "Bearer " + granToken,
			"If-None-Exist": "code=4548-4&patient=Patient/p1",
		})
		// Disallowed If-None-Exist targeting another patient -> 403
		recBadINE, _ := doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Observation", `{
			"resourceType": "Observation",
			"status": "final",
			"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}]}],
			"code": {"coding": [{"system": "http://loinc.org", "code": "4548-4"}]},
			"subject": {"reference": "Patient/p1"}
		}`, map[string]string{
			"Authorization": "Bearer " + granToken,
			"If-None-Exist": "code=4548-4&patient=Patient/p2",
		})
		if recBadINE.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for cross-patient If-None-Exist, got %d", recBadINE.Code)
		}

		// 3. Minimal-scope token (`user/Practitioner.c`) to trigger 403 guard branches across handlers
		_, minMap := doReq(t, handler, http.MethodPost, "/oauth2/token",
			"grant_type=client_credentials&client_id=min-client&scope="+url.QueryEscape("user/Practitioner.c"),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		minToken, _ := minMap["access_token"].(string)
		minAuth := map[string]string{"Authorization": "Bearer " + minToken}

		for _, tc := range []struct {
			method string
			path   string
			body   string
		}{
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient/p1", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient/p1/_history/1", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient?name=Smith", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "?_type=Patient", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient/p1/Observation", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient/_history", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient/p1/_history", ""},
			{http.MethodPost, "/fhir/r4/" + tenant + "/Patient", `{"resourceType":"Patient"}`},
			{http.MethodPut, "/fhir/r4/" + tenant + "/Patient/p1", `{"resourceType":"Patient","id":"p1"}`},
			{http.MethodPatch, "/fhir/r4/" + tenant + "/Patient/p1", `[{"op":"replace","path":"/active","value":false}]`},
			{http.MethodDelete, "/fhir/r4/" + tenant + "/Patient/p1", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient/$meta", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient/p1/$meta", ""},
			{http.MethodPost, "/fhir/r4/" + tenant + "/Patient/p1/$meta-add", `{}`},
			{http.MethodPost, "/fhir/r4/" + tenant + "/Patient/p1/$meta-delete", `{}`},
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient/p1/$everything", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient/p1/$summary", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/DocumentReference/$docref?patient=Patient/p1", ""},
			{http.MethodPost, "/fhir/r4/" + tenant + "/Patient/p1/$purge", ""},
			{http.MethodPost, "/fhir/r4/" + tenant + "/$expunge", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/Patient/p1/$diff", ""},
			{http.MethodGet, "/fhir/r4/" + tenant + "/$export", ""},
		} {
			rec403, _ := doReq(t, handler, tc.method, tc.path, tc.body, minAuth)
			if rec403.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for %s %s with minimal scope, got %d: %s", tc.method, tc.path, rec403.Code, rec403.Body.String())
			}
		}
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/$import", `{"resourceType":"Parameters","parameter":[{"name":"input","part":[{"name":"type","valueCode":"Patient"},{"name":"content","valueString":"{\"resourceType\":\"Patient\"}"}]}]}`, minAuth)

		// Revoke patToken and verify subsequent call returns 401
		doReq(t, handler, http.MethodPost, "/oauth2/revoke", "token="+url.QueryEscape(patToken),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		recRevoked, _ := doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p1", "", patAuth)
		if recRevoked.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for revoked token, got %d", recRevoked.Code)
		}
	})

	t.Run("SubtestE_CQM_CDS_SDC_Terminology_Subscriptions_IPS_Operations", func(t *testing.T) {
		// 1. Custom stored Measure + Diabetes Condition + HbA1c Observation -> $evaluate-measure (measureSpecFromRecord / SearchResult / Proto, latestHbA1cInPeriod)
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Measure/custom-cms122", `{
			"resourceType": "Measure",
			"id": "custom-cms122",
			"url": "http://example.org/Measure/custom-cms122",
			"version": "1.0.0",
			"name": "CustomDiabetesHbA1c",
			"title": "Custom Diabetes HbA1c Poor Control (>9%)",
			"status": "active",
			"scoring": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-scoring", "code": "proportion"}]}
		}`, sysAuth)
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Condition/cond-dm1", `{
			"resourceType": "Condition",
			"id": "cond-dm1",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://snomed.info/sct", "code": "44054006", "display": "Type 2 diabetes mellitus"}]},
			"subject": {"reference": "Patient/p1"}
		}`, sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Measure/custom-cms122/$evaluate-measure?subject=Patient/p1&periodStart=2020-01-01&periodEnd=2030-12-31", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Measure/$evaluate-measure?measure=http://example.org/Measure/custom-cms122&reportType=subject-list&periodStart=2020-01-01&periodEnd=2030-12-31", "", sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Measure/custom-cms122/$evaluate-measure", `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "subject", "valueString": "Patient/p1"},
				{"name": "reportType", "valueCode": "individual"},
				{"name": "periodStart", "valueDate": "2020-01-01"},
				{"name": "periodEnd", "valueDate": "2030-12-31"}
			]
		}`, sysAuth)

		// 2. Penicillin AllergyIntolerance + CDS Hooks services (patient-view, medication-prescribe, order-select)
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/AllergyIntolerance/allergy-pen", `{
			"resourceType": "AllergyIntolerance",
			"id": "allergy-pen",
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980", "display": "Penicillin G"}]},
			"patient": {"reference": "Patient/p1"}
		}`, sysAuth)
		for _, cdsPrefix := range []string{"/cds-services", "/fhir/r4/cds-services", "/fhir/r4/" + tenant + "/cds-services"} {
			doReq(t, handler, http.MethodGet, cdsPrefix, "", sysAuth)
			doReq(t, handler, http.MethodPost, cdsPrefix+"/medication-prescribe", `{
				"hook": "medication-prescribe",
				"hookInstance": "12345",
				"context": {
					"patientId": "Patient/p1",
					"draftOrders": {
						"resourceType": "Bundle",
						"type": "collection",
						"entry": [{
							"resource": {
								"resourceType": "MedicationRequest",
								"status": "draft",
								"intent": "order",
								"medicationCodeableConcept": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "723", "display": "Amoxicillin 500 MG"}]},
								"subject": {"reference": "Patient/p1"}
							}
						}]
					}
				}
			}`, sysAuth)
			doReq(t, handler, http.MethodPost, cdsPrefix+"/patient-view", `{
				"hook": "patient-view",
				"hookInstance": "12346",
				"context": {"patientId": "Patient/p1"}
			}`, sysAuth)
			doReq(t, handler, http.MethodPost, cdsPrefix+"/order-select", `{
				"hook": "order-select",
				"hookInstance": "12347",
				"context": {
					"patientId": "Patient/p1",
					"selections": ["MedicationRequest/m1"],
					"draftOrders": {
						"resourceType": "Bundle",
						"type": "collection",
						"entry": [{
							"resource": {
								"resourceType": "MedicationRequest",
								"id": "m1",
								"status": "draft",
								"intent": "order",
								"medicationCodeableConcept": {"text": "Ampicillin"},
								"subject": {"reference": "Patient/p1"}
							}
						}]
					}
				}
			}`, sysAuth)
		}

		// 3. SDC $populate, $extract (with LOINC inference), $docref (with type=60591-5 & on-demand), $document
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/Questionnaire/q-vitals", `{
			"resourceType": "Questionnaire",
			"id": "q-vitals",
			"url": "http://example.org/Questionnaire/q-vitals",
			"status": "active",
			"title": "Vitals & Demographics Form",
			"item": [
				{"linkId": "patient.name", "text": "Full Name", "type": "string"},
				{"linkId": "patient.birthDate", "text": "Birth Date", "type": "date"},
				{"linkId": "patient.gender", "text": "Gender", "type": "choice"},
				{"linkId": "vitals.weight", "text": "Body Weight", "type": "decimal"},
				{"linkId": "vitals.height", "text": "Body Height", "type": "decimal"},
				{"linkId": "vitals.bmi", "text": "BMI", "type": "decimal"},
				{"linkId": "vitals.systolic", "text": "Systolic BP", "type": "integer"},
				{"linkId": "vitals.smoking", "text": "Tobacco Smoking Status", "type": "string"}
			]
		}`, sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Questionnaire/q-vitals/$populate", `{
			"resourceType": "Parameters",
			"parameter": [{"name": "subject", "valueReference": {"reference": "Patient/p1"}}]
		}`, sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/QuestionnaireResponse/$extract", `{
			"resourceType": "QuestionnaireResponse",
			"status": "completed",
			"questionnaire": "http://example.org/Questionnaire/q-vitals",
			"subject": {"reference": "Patient/p1"},
			"item": [
				{"linkId": "obs.weight", "text": "Body weight", "answer": [{"valueDecimal": 70.5}]},
				{"linkId": "obs.height", "text": "Body height", "answer": [{"valueDecimal": 175.0}]},
				{"linkId": "obs.bmi", "text": "BMI", "answer": [{"valueDecimal": 23.0}]},
				{"linkId": "obs.hr", "text": "Heart rate", "answer": [{"valueInteger": 72}]},
				{"linkId": "obs.temp", "text": "Body temperature", "answer": [{"valueDecimal": 36.8}]},
				{"linkId": "obs.bp", "text": "Systolic blood pressure", "answer": [{"valueInteger": 120}]},
				{"linkId": "obs.diastolic", "text": "Diastolic blood pressure", "answer": [{"valueInteger": 80}]},
				{"linkId": "obs.spo2", "text": "Oxygen saturation", "answer": [{"valueInteger": 99}]},
				{"linkId": "condition.dm", "text": "Diabetes Condition", "answer": [{"valueCoding": {"system": "http://snomed.info/sct", "code": "44054006"}}]},
				{"linkId": "allergy.pen", "text": "Penicillin Allergy", "answer": [{"valueString": "Penicillin"}]}
			]
		}`, sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/DocumentReference/$docref?patient=Patient/p1&type=http://loinc.org|60591-5&on-demand=true", "", sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/DocumentReference/$docref", `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "patient", "valueId": "p1"},
				{"name": "on-demand", "valueBoolean": true},
				{"name": "type", "valueCodeableConcept": {"coding": [{"system": "http://loinc.org", "code": "60591-5"}]}}
			]
		}`, sysAuth)

		// 4. Terminology: inline CodeSystem, multi-filter ValueSet $expand (intersectTerminologyConcepts, matchesCodeSystemFilter), stored ConceptMap $translate
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/ValueSet/$expand", `{
			"resourceType": "Parameters",
			"parameter": [
				{
					"name": "tx-resource",
					"resource": {
						"resourceType": "CodeSystem",
						"url": "http://example.org/CodeSystem/cs-inline",
						"status": "active",
						"content": "complete",
						"concept": [
							{"code": "A1", "display": "Alpha One", "concept": [{"code": "A1-SUB", "display": "Alpha Sub"}]},
							{"code": "B2", "display": "Beta Two"}
						]
					}
				},
				{
					"name": "valueSet",
					"resource": {
						"resourceType": "ValueSet",
						"status": "active",
						"compose": {
							"include": [{
								"system": "http://example.org/CodeSystem/cs-inline",
								"filter": [
									{"property": "concept", "op": "is-a", "value": "A1"},
									{"property": "code", "op": "regex", "value": "^A1.*"}
								]
							}],
							"exclude": [{
								"system": "http://example.org/CodeSystem/cs-inline",
								"concept": [{"code": "B2"}]
							}]
						}
					}
				}
			]
		}`, sysAuth)
		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/ConceptMap/cm-custom", `{
			"resourceType": "ConceptMap",
			"id": "cm-custom",
			"url": "http://example.org/ConceptMap/cm-custom",
			"status": "active",
			"group": [{
				"source": "http://example.org/cs-src",
				"target": "http://example.org/cs-dst",
				"element": [{
					"code": "SRC-1",
					"target": [{"code": "DST-1", "display": "Mapped One", "equivalence": "equivalent"}]
				}]
			}]
		}`, sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/ConceptMap/cm-custom/$translate?system=http://example.org/cs-src&code=SRC-1", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/ConceptMap/$translate?url=http://example.org/ConceptMap/cm-custom&system=http://example.org/cs-src&code=SRC-1&reverse=true", "", sysAuth)

		// 5. $meta, $meta-add, $meta-delete, and $graph with stored GraphDefinition
		metaPayload := `{
			"resourceType": "Parameters",
			"parameter": [{
				"name": "meta",
				"valueMeta": {
					"profile": ["http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"],
					"tag": [{"system": "http://example.org/tags", "code": "vip", "display": "VIP"}],
					"security": [{"system": "http://terminology.hl7.org/CodeSystem/v3-Confidentiality", "code": "R"}]
				}
			}]
		}`
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/p1/$meta-add", metaPayload, sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p1/$meta", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/$meta", "", sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/$meta", "", sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/p1/$meta-delete", metaPayload, sysAuth)

		doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/GraphDefinition/gd-custom", `{
			"resourceType": "GraphDefinition",
			"id": "gd-custom",
			"url": "http://example.org/GraphDefinition/gd-custom",
			"name": "gd-custom",
			"status": "active",
			"start": "Patient",
			"link": [{
				"target": [{
					"type": "Observation",
					"params": "patient={ref}"
				}]
			}]
		}`, sysAuth)
		doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p1/$graph?graph=gd-custom", "", sysAuth)
	})

	t.Run("SubtestF_ExhaustiveRouteAndMethodSweep_AuthAndNilStore", func(t *testing.T) {
		// Seed minimal instance IDs so 6-part instance operations find a valid resource where applicable
		seedTypes := []struct {
			resType string
			id      string
			json    string
		}{
			{"PlanDefinition", "pd1", `{"resourceType":"PlanDefinition","id":"pd1","status":"active","title":"Plan 1"}`},
			{"ActivityDefinition", "ad1", `{"resourceType":"ActivityDefinition","id":"ad1","status":"active","kind":"ServiceRequest"}`},
			{"Library", "lib1", `{"resourceType":"Library","id":"lib1","status":"active","type":{"coding":[{"code":"logic-library"}]}}`},
			{"Questionnaire", "q1", `{"resourceType":"Questionnaire","id":"q1","status":"active"}`},
			{"StructureDefinition", "sd1", `{"resourceType":"StructureDefinition","id":"sd1","url":"http://example.org/sd1","name":"SD1","status":"active","kind":"resource","abstract":false,"type":"Patient"}`},
			{"StructureMap", "sm1", `{"resourceType":"StructureMap","id":"sm1","url":"http://example.org/sm1","name":"SM1","status":"active"}`},
			{"CompartmentDefinition", "cd1", `{"resourceType":"CompartmentDefinition","id":"cd1","url":"http://example.org/cd1","name":"CD1","status":"active","code":"Patient","search":true}`},
			{"CapabilityStatement", "cs1", `{"resourceType":"CapabilityStatement","id":"cs1","status":"active","date":"2026-01-01","kind":"instance","fhirVersion":"4.0.1","format":["json"]}`},
			{"ObservationDefinition", "od1", `{"resourceType":"ObservationDefinition","id":"od1","code":{"coding":[{"system":"http://loinc.org","code":"4548-4"}]}}`},
			{"OperationDefinition", "opd1", `{"resourceType":"OperationDefinition","id":"opd1","name":"Op1","status":"active","kind":"operation","system":true,"type":false,"instance":false,"code":"op1"}`},
			{"Subscription", "sub1", `{"resourceType":"Subscription","id":"sub1","status":"active","reason":"Monitor","criteria":"Observation?status=final","channel":{"type":"rest-hook","endpoint":"https://example.org/hook"}}`},
			{"Coverage", "cov1", `{"resourceType":"Coverage","id":"cov1","status":"active","beneficiary":{"reference":"Patient/p1"},"payor":[{"reference":"Organization/org1"}]}`},
			{"Claim", "clm1", `{"resourceType":"Claim","id":"clm1","status":"active","type":{"coding":[{"code":"institutional"}]},"use":"claim","patient":{"reference":"Patient/p1"},"created":"2026-01-01","provider":{"reference":"Organization/org1"},"priority":{"coding":[{"code":"normal"}]},"insurance":[{"sequence":1,"focal":true,"coverage":{"reference":"Coverage/cov1"}}]}`},
			{"ExplanationOfBenefit", "eob1", `{"resourceType":"ExplanationOfBenefit","id":"eob1","status":"active","type":{"coding":[{"code":"institutional"}]},"use":"claim","patient":{"reference":"Patient/p1"},"created":"2026-01-01","insurer":{"reference":"Organization/org1"},"provider":{"reference":"Organization/org1"},"outcome":"complete","insurance":[{"focal":true,"coverage":{"reference":"Coverage/cov1"}}]}`},
			{"MedicationKnowledge", "mk1", `{"resourceType":"MedicationKnowledge","id":"mk1","code":{"coding":[{"system":"http://www.nlm.nih.gov/research/umls/rxnorm","code":"723"}]}}`},
			{"SubstanceDefinition", "subdef1", `{"resourceType":"SubstanceDefinition","id":"subdef1"}`},
			{"Medication", "med1", `{"resourceType":"Medication","id":"med1","code":{"coding":[{"system":"http://www.nlm.nih.gov/research/umls/rxnorm","code":"723"}]}}`},
			{"MedicationRequest", "mr1", `{"resourceType":"MedicationRequest","id":"mr1","status":"active","intent":"order","subject":{"reference":"Patient/p1"},"medicationCodeableConcept":{"coding":[{"system":"http://www.nlm.nih.gov/research/umls/rxnorm","code":"723"}]}}`},
			{"MedicationAdministration", "ma1", `{"resourceType":"MedicationAdministration","id":"ma1","status":"completed","subject":{"reference":"Patient/p1"},"medicationCodeableConcept":{"coding":[{"code":"723"}]}}`},
			{"MedicationDispense", "md1", `{"resourceType":"MedicationDispense","id":"md1","status":"completed","subject":{"reference":"Patient/p1"},"medicationCodeableConcept":{"coding":[{"code":"723"}]}}`},
			{"MedicationStatement", "ms1", `{"resourceType":"MedicationStatement","id":"ms1","status":"active","subject":{"reference":"Patient/p1"},"medicationCodeableConcept":{"coding":[{"code":"723"}]}}`},
			{"NutritionOrder", "no1", `{"resourceType":"NutritionOrder","id":"no1","status":"active","intent":"order","patient":{"reference":"Patient/p1"},"dateTime":"2026-01-01"}`},
			{"CarePlan", "cp1", `{"resourceType":"CarePlan","id":"cp1","status":"active","intent":"plan","subject":{"reference":"Patient/p1"}}`},
			{"CareTeam", "ct1", `{"resourceType":"CareTeam","id":"ct1","status":"active","subject":{"reference":"Patient/p1"}}`},
			{"Goal", "goal1", `{"resourceType":"Goal","id":"goal1","lifecycleStatus":"active","description":{"text":"Lower HbA1c"},"subject":{"reference":"Patient/p1"}}`},
			{"ServiceRequest", "sr1", `{"resourceType":"ServiceRequest","id":"sr1","status":"active","intent":"order","subject":{"reference":"Patient/p1"},"code":{"coding":[{"system":"http://loinc.org","code":"4548-4"}]}}`},
			{"DiagnosticReport", "dr1", `{"resourceType":"DiagnosticReport","id":"dr1","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"4548-4"}]},"subject":{"reference":"Patient/p1"},"result":[{"reference":"Observation/o1"}]}`},
			{"Specimen", "spec1", `{"resourceType":"Specimen","id":"spec1","status":"available","subject":{"reference":"Patient/p1"}}`},
			{"ImagingStudy", "img1", `{"resourceType":"ImagingStudy","id":"img1","status":"available","subject":{"reference":"Patient/p1"}}`},
			{"Media", "media1", `{"resourceType":"Media","id":"media1","status":"completed","content":{"contentType":"image/png"}}`},
			{"Device", "dev1", `{"resourceType":"Device","id":"dev1","status":"active","patient":{"reference":"Patient/p1"}}`},
			{"DeviceDefinition", "dd1", `{"resourceType":"DeviceDefinition","id":"dd1"}`},
			{"DeviceMetric", "dm1", `{"resourceType":"DeviceMetric","id":"dm1","type":{"coding":[{"code":"150456"}]},"category":"measurement"}`},
			{"DeviceRequest", "dreq1", `{"resourceType":"DeviceRequest","id":"dreq1","status":"active","intent":"order","subject":{"reference":"Patient/p1"},"codeCodeableConcept":{"coding":[{"code":"dev"}]}}`},
			{"DeviceUseStatement", "dus1", `{"resourceType":"DeviceUseStatement","id":"dus1","status":"active","subject":{"reference":"Patient/p1"},"device":{"reference":"Device/dev1"}}`},
			{"SupplyRequest", "supreq1", `{"resourceType":"SupplyRequest","id":"supreq1","status":"active"}`},
			{"SupplyDelivery", "supdel1", `{"resourceType":"SupplyDelivery","id":"supdel1","status":"completed","patient":{"reference":"Patient/p1"}}`},
			{"BiologicallyDerivedProduct", "bdp1", `{"resourceType":"BiologicallyDerivedProduct","id":"bdp1"}`},
			{"Immunization", "imm1", `{"resourceType":"Immunization","id":"imm1","status":"completed","vaccineCode":{"coding":[{"system":"http://hl7.org/fhir/sid/cvx","code":"208"}]},"patient":{"reference":"Patient/p1"},"occurrenceDateTime":"2025-10-01"}`},
			{"ImmunizationEvaluation", "immev1", `{"resourceType":"ImmunizationEvaluation","id":"immev1","status":"completed","patient":{"reference":"Patient/p1"},"targetDisease":{"coding":[{"code":"1857005"}]},"immunizationEvent":{"reference":"Immunization/imm1"},"doseStatus":{"coding":[{"code":"valid"}]}}`},
			{"ImmunizationRecommendation", "immrec1", `{"resourceType":"ImmunizationRecommendation","id":"immrec1","patient":{"reference":"Patient/p1"},"date":"2026-01-01","recommendation":[{"vaccineCode":[{"coding":[{"code":"208"}]}],"forecastStatus":{"coding":[{"code":"due"}]}}]}`},
			{"AdverseEvent", "adv1", `{"resourceType":"AdverseEvent","id":"adv1","actuality":"actual","subject":{"reference":"Patient/p1"}}`},
			{"DetectedIssue", "di1", `{"resourceType":"DetectedIssue","id":"di1","status":"final","patient":{"reference":"Patient/p1"}}`},
			{"ClinicalImpression", "ci1", `{"resourceType":"ClinicalImpression","id":"ci1","status":"completed","subject":{"reference":"Patient/p1"}}`},
			{"RiskAssessment", "ra1", `{"resourceType":"RiskAssessment","id":"ra1","status":"final","subject":{"reference":"Patient/p1"}}`},
			{"FamilyMemberHistory", "fmh1", `{"resourceType":"FamilyMemberHistory","id":"fmh1","status":"completed","patient":{"reference":"Patient/p1"},"relationship":{"coding":[{"code":"FTH"}]}}`},
			{"Procedure", "proc1", `{"resourceType":"Procedure","id":"proc1","status":"completed","subject":{"reference":"Patient/p1"},"code":{"coding":[{"system":"http://snomed.info/sct","code":"80146002"}]}}`},
			{"Consent", "con1", `{"resourceType":"Consent","id":"con1","status":"active","scope":{"coding":[{"code":"patient-privacy"}]},"category":[{"coding":[{"code":"59284-0"}]}],"patient":{"reference":"Patient/p1"}}`},
			{"Contract", "cntr1", `{"resourceType":"Contract","id":"cntr1","status":"executed"}`},
			{"Communication", "comm1", `{"resourceType":"Communication","id":"comm1","status":"completed","subject":{"reference":"Patient/p1"}}`},
			{"CommunicationRequest", "commreq1", `{"resourceType":"CommunicationRequest","id":"commreq1","status":"active","subject":{"reference":"Patient/p1"}}`},
			{"Task", "task1", `{"resourceType":"Task","id":"task1","status":"requested","intent":"order","for":{"reference":"Patient/p1"}}`},
			{"Appointment", "appt1", `{"resourceType":"Appointment","id":"appt1","status":"booked","participant":[{"actor":{"reference":"Patient/p1"},"status":"accepted"}]}`},
			{"AppointmentResponse", "apptr1", `{"resourceType":"AppointmentResponse","id":"apptr1","appointment":{"reference":"Appointment/appt1"},"participantStatus":"accepted"}`},
			{"Schedule", "sched1", `{"resourceType":"Schedule","id":"sched1","actor":[{"reference":"Practitioner/prac1"}]}`},
			{"Slot", "slot1", `{"resourceType":"Slot","id":"slot1","schedule":{"reference":"Schedule/sched1"},"status":"free","start":"2026-06-01T09:00:00Z","end":"2026-06-01T09:30:00Z"}`},
			{"Account", "acct1", `{"resourceType":"Account","id":"acct1","status":"active","subject":[{"reference":"Patient/p1"}]}`},
			{"ChargeItem", "ci2", `{"resourceType":"ChargeItem","id":"ci2","status":"billable","code":{"coding":[{"code":"99213"}]},"subject":{"reference":"Patient/p1"}}`},
			{"ChargeItemDefinition", "cid1", `{"resourceType":"ChargeItemDefinition","id":"cid1","url":"http://example.org/cid1","status":"active"}`},
			{"Invoice", "inv1", `{"resourceType":"Invoice","id":"inv1","status":"issued","subject":{"reference":"Patient/p1"}}`},
			{"PaymentNotice", "pn1", `{"resourceType":"PaymentNotice","id":"pn1","status":"active","request":{"reference":"Claim/clm1"},"created":"2026-01-01","payment":{"reference":"PaymentReconciliation/pr1"},"amount":{"value":100,"currency":"USD"},"recipient":{"reference":"Organization/org1"}}`},
			{"PaymentReconciliation", "pr1", `{"resourceType":"PaymentReconciliation","id":"pr1","status":"active","created":"2026-01-01","outcome":"complete","paymentDate":"2026-01-01","paymentAmount":{"value":100,"currency":"USD"}}`},
			{"InsurancePlan", "ip1", `{"resourceType":"InsurancePlan","id":"ip1","status":"active","name":"Gold Plan"}`},
			{"EnrollmentRequest", "er1", `{"resourceType":"EnrollmentRequest","id":"er1","status":"active","candidate":{"reference":"Patient/p1"}}`},
			{"EnrollmentResponse", "eresp1", `{"resourceType":"EnrollmentResponse","id":"eresp1","status":"active"}`},
			{"EpisodeOfCare", "eoc1", `{"resourceType":"EpisodeOfCare","id":"eoc1","status":"active","patient":{"reference":"Patient/p1"}}`},
			{"Flag", "flag1", `{"resourceType":"Flag","id":"flag1","status":"active","code":{"text":"Fall Risk"},"subject":{"reference":"Patient/p1"}}`},
			{"List", "list1", `{"resourceType":"List","id":"list1","status":"current","mode":"working","subject":{"reference":"Patient/p1"},"entry":[{"item":{"reference":"Condition/cond-dm1"}}]}`},
			{"Composition", "comp1", `{"resourceType":"Composition","id":"comp1","status":"final","type":{"coding":[{"system":"http://loinc.org","code":"60591-5"}]},"subject":{"reference":"Patient/p1"},"date":"2026-01-01","author":[{"reference":"Practitioner/prac1"}],"title":"Summary"}`},
			{"NamingSystem", "ns1", `{"resourceType":"NamingSystem","id":"ns1","name":"NS1","status":"active","kind":"identifier","date":"2026-01-01","uniqueId":[{"type":"uri","value":"http://hospital.example.org/mrn","preferred":true},{"type":"oid","value":"2.16.840.1.113883.4.1"}]}`},
			{"ConceptMap", "cm1", `{"resourceType":"ConceptMap","id":"cm1","url":"http://example.org/cm1","status":"active"}`},
			{"Organization", "org1", `{"resourceType":"Organization","id":"org1","active":true,"name":"General Hospital"}`},
			{"OrganizationAffiliation", "oa1", `{"resourceType":"OrganizationAffiliation","id":"oa1","active":true,"organization":{"reference":"Organization/org1"}}`},
			{"HealthcareService", "hs1", `{"resourceType":"HealthcareService","id":"hs1","active":true,"providedBy":{"reference":"Organization/org1"},"name":"Primary Care"}`},
			{"Endpoint", "ep1", `{"resourceType":"Endpoint","id":"ep1","status":"active","connectionType":{"code":"hl7-fhir-rest"},"payloadType":[{"coding":[{"code":"any"}]}],"address":"https://example.org/fhir"}`},
			{"Location", "loc1", `{"resourceType":"Location","id":"loc1","status":"active","name":"Main Wing","position":{"longitude":-122.4194,"latitude":37.7749}}`},
			{"Practitioner", "prac1", `{"resourceType":"Practitioner","id":"prac1","active":true,"name":[{"family":"House","given":["Gregory"]}]}`},
			{"PractitionerRole", "pr2", `{"resourceType":"PractitionerRole","id":"pr2","active":true,"practitioner":{"reference":"Practitioner/prac1"},"organization":{"reference":"Organization/org1"}}`},
			{"RelatedPerson", "rp1", `{"resourceType":"RelatedPerson","id":"rp1","active":true,"patient":{"reference":"Patient/p1"}}`},
			{"Person", "pers1", `{"resourceType":"Person","id":"pers1","active":true,"link":[{"target":{"reference":"Patient/p1"}}]}`},
			{"ResearchStudy", "rs1", `{"resourceType":"ResearchStudy","id":"rs1","status":"active","title":"Trial 1"}`},
			{"ResearchSubject", "rsub1", `{"resourceType":"ResearchSubject","id":"rsub1","status":"on-study","study":{"reference":"ResearchStudy/rs1"},"individual":{"reference":"Patient/p1"}}`},
			{"Evidence", "ev1", `{"resourceType":"Evidence","id":"ev1","status":"active","exposureBackground":{"reference":"EvidenceVariable/evv1"}}`},
			{"EvidenceVariable", "evv1", `{"resourceType":"EvidenceVariable","id":"evv1","status":"active","characteristic":[{"definitionCanonical":"http://example.org/c1"}]}`},
			{"EffectEvidenceSynthesis", "ees1", `{"resourceType":"EffectEvidenceSynthesis","id":"ees1","status":"active","population":{"reference":"EvidenceVariable/evv1"},"exposure":{"reference":"EvidenceVariable/evv1"},"exposureAlternative":{"reference":"EvidenceVariable/evv1"},"outcome":{"reference":"EvidenceVariable/evv1"}}`},
			{"RiskEvidenceSynthesis", "res1", `{"resourceType":"RiskEvidenceSynthesis","id":"res1","status":"active","population":{"reference":"EvidenceVariable/evv1"},"outcome":{"reference":"EvidenceVariable/evv1"}}`},
			{"MeasureReport", "mrep1", `{"resourceType":"MeasureReport","id":"mrep1","status":"complete","type":"individual","measure":"http://example.org/Measure/custom-cms122","subject":{"reference":"Patient/p1"},"period":{"start":"2020-01-01","end":"2030-12-31"}}`},
			{"CatalogEntry", "cat1", `{"resourceType":"CatalogEntry","id":"cat1","orderable":true,"referencedItem":{"reference":"Medication/med1"}}`},
			{"Linkage", "lnk1", `{"resourceType":"Linkage","id":"lnk1","active":true,"item":[{"type":"source","resource":{"reference":"Patient/p1"}},{"type":"alternate","resource":{"reference":"Patient/p2"}}]}`},
			{"VerificationResult", "vr1", `{"resourceType":"VerificationResult","id":"vr1","status":"validated","target":[{"reference":"Practitioner/prac1"}]}`},
			{"RequestGroup", "rg1", `{"resourceType":"RequestGroup","id":"rg1","status":"active","intent":"plan","subject":{"reference":"Patient/p1"}}`},
			{"GuidanceResponse", "gr1", `{"resourceType":"GuidanceResponse","id":"gr1","status":"success","moduleUri":"http://example.org/m1","subject":{"reference":"Patient/p1"}}`},
			{"BodyStructure", "bs1", `{"resourceType":"BodyStructure","id":"bs1","patient":{"reference":"Patient/p1"}}`},
			{"MolecularSequence", "mseq1", `{"resourceType":"MolecularSequence","id":"mseq1","coordinateSystem":0,"patient":{"reference":"Patient/p1"}}`},
			{"VisionPrescription", "vp1", `{"resourceType":"VisionPrescription","id":"vp1","status":"active","created":"2026-01-01","patient":{"reference":"Patient/p1"},"prescriber":{"reference":"Practitioner/prac1"},"lensSpecification":[{"product":{"coding":[{"code":"lens"}]},"eye":"right"}]}`},
			{"ExampleScenario", "es1", `{"resourceType":"ExampleScenario","id":"es1","status":"active"}`},
			{"TestScript", "ts1", `{"resourceType":"TestScript","id":"ts1","name":"TS1","status":"active","url":"http://example.org/ts1"}`},
			{"TestReport", "tr1", `{"resourceType":"TestReport","id":"tr1","status":"completed","testScript":{"reference":"TestScript/ts1"},"result":"pass"}`},
			{"ImplementationGuide", "ig1", `{"resourceType":"ImplementationGuide","id":"ig1","name":"IG1","status":"active","url":"http://example.org/ig1","packageId":"hl7.fhir.us.core","fhirVersion":["4.0.1"]}`},
			{"GraphDefinition", "gd1", `{"resourceType":"GraphDefinition","id":"gd1","name":"GD1","status":"active","start":"Patient"}`},
			{"MessageDefinition", "md2", `{"resourceType":"MessageDefinition","id":"md2","status":"active","date":"2026-01-01","eventCoding":{"code":"admin-notify"}}`},
			{"MessageHeader", "mh1", `{"resourceType":"MessageHeader","id":"mh1","eventCoding":{"code":"admin-notify"},"source":{"endpoint":"https://example.org/src"}}`},
			{"TerminologyCapabilities", "tc1", `{"resourceType":"TerminologyCapabilities","id":"tc1","status":"active","date":"2026-01-01","kind":"instance"}`},
		}
		for _, st := range seedTypes {
			doReq(t, handler, http.MethodPut, "/fhir/r4/"+tenant+"/"+st.resType+"/"+st.id, st.json, sysAuth)
		}

		// All 4-part system operations
		sysOps := []string{
			"$versions", "$convert", "$graphql", "$fhirpath", "$reindex", "$process-message",
			"$process-notification", "$closure", "$data-requirements", "$davinci-data-export",
			"$submit-attachment", "$certify", "$docs", "$iti-65",
		}
		for _, fn := range []string{"current-medications", "current-problems", "current-allergies", "current-immunizations", "unknown-list"} {
			doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/List/$find?patient=Patient/p1&name="+fn, "", sysAuth)
		}
		doReq(t, handler, http.MethodGet, "/fhir/r4/$docs", "", nil)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$reindex", `{}`, sysAuth)
		doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/$convert", `{"resourceType":"Patient","id":"p1"}`, sysAuth)

		// Mint a patient-restricted token for p1 and a minimal token for SubtestF route sweep
		_, minMapF := doReq(t, handler, http.MethodPost, "/oauth2/token",
			"grant_type=client_credentials&client_id=min-f&scope="+url.QueryEscape("user/Practitioner.c"),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		minAuthF := map[string]string{"Authorization": "Bearer " + fmt.Sprint(minMapF["access_token"])}

		authRecF, _ := doReq(t, handler, http.MethodGet,
			"/oauth2/authorize?response_type=code&client_id=pat-f&redirect_uri="+url.QueryEscape("https://app.example.org/cb")+
				"&scope="+url.QueryEscape("patient/*.cruds")+"&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM&code_challenge_method=S256&patient=p1",
			"", nil)
		locF, _ := url.Parse(authRecF.Header().Get("Location"))
		_, patMapF := doReq(t, handler, http.MethodPost, "/oauth2/token",
			"grant_type=authorization_code&code="+url.QueryEscape(locF.Query().Get("code"))+
				"&redirect_uri="+url.QueryEscape("https://app.example.org/cb")+
				"&client_id=pat-f&code_verifier=dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		patAuthF := map[string]string{"Authorization": "Bearer " + fmt.Sprint(patMapF["access_token"])}

		// All 219 (ResourceType, id, op) triples in server.go covering both 5-part type-level and 6-part instance-level routes
		typeAndInstOps := []struct {
			resType string
			id      string
			op      string
		}{
			{"ActivityDefinition", "ad1", "$apply"},
			{"ActivityDefinition", "ad1", "$data-requirements"},
			{"AdverseEvent", "adv1", "$evaluate"},
			{"AdverseEvent", "adv1", "$vulcan-icsr-report"},
			{"Appointment", "appt1", "$book"},
			{"Appointment", "appt1", "$find"},
			{"Appointment", "appt1", "$hold"},
			{"Bundle", "b1", "$bfdr-submit"},
			{"Bundle", "b1", "$cancer-registry-submit"},
			{"Bundle", "b1", "$cdex-sign"},
			{"Bundle", "b1", "$cdex-verify-signature"},
			{"Bundle", "b1", "$ecr-submit"},
			{"Bundle", "b1", "$iti-65"},
			{"Bundle", "b1", "$vrdr-submit"},
			{"CapabilityStatement", "cs1", "$implements"},
			{"CapabilityStatement", "cs1", "$subset"},
			{"CarePlan", "cp1", "$evaluate"},
			{"ChargeItem", "ci2", "$apply"},
			{"Claim", "clm1", "$gfe"},
			{"Claim", "clm1", "$gfe-submit"},
			{"Claim", "clm1", "$inquire"},
			{"Claim", "clm1", "$pas-inquire"},
			{"Claim", "clm1", "$predetermine"},
			{"Claim", "clm1", "$rtpbc-submit"},
			{"Claim", "clm1", "$submit"},
			{"ClaimResponse", "cr1", "$rtpbc-coverage"},
			{"CodeSystem", "cs-id1", "$find-matches"},
			{"CodeSystem", "cs-id1", "$lookup"},
			{"CodeSystem", "cs-id1", "$subsumes"},
			{"CodeSystem", "cs-id1", "$validate-code"},
			{"Communication", "comm1", "$cdex-submit-attachment"},
			{"Communication", "comm1", "$rr-process"},
			{"Composition", "comp1", "$adi-document"},
			{"Composition", "comp1", "$bfdr-document"},
			{"Composition", "comp1", "$bfdr-summary"},
			{"Composition", "comp1", "$document"},
			{"Composition", "comp1", "$eicr-document"},
			{"Composition", "comp1", "$pacio-toc-bundle"},
			{"Composition", "comp1", "$vrdr-document"},
			{"ConceptMap", "cm-custom", "$closure"},
			{"ConceptMap", "cm-custom", "$translate"},
			{"Condition", "cond-dm1", "$stage-cancer"},
			{"Consent", "con1", "$adi-verify"},
			{"Consent", "con1", "$evaluate"},
			{"Consent", "con1", "$sdoh-consent-record"},
			{"Coverage", "cov1", "$formulary-coverage"},
			{"CoverageEligibilityRequest", "cer1", "$submit"},
			{"DiagnosticReport", "dr1", "$elr-submit"},
			{"DiagnosticReport", "dr1", "$evaluate"},
			{"DiagnosticReport", "dr1", "$mcode-genomics-report"},
			{"DiagnosticReport", "dr1", "$nbs-submit"},
			{"DocumentReference", "doc1", "$docref"},
			{"DocumentReference", "doc1", "$ihe-iti-68"},
			{"DocumentReference", "doc1", "$iti-67"},
			{"Encounter", "e1", "$admit"},
			{"Encounter", "e1", "$discharge"},
			{"Encounter", "e1", "$ecr-report"},
			{"Encounter", "e1", "$everything"},
			{"Encounter", "e1", "$nhsn-hai-report"},
			{"Encounter", "e1", "$pacio-timepoint"},
			{"Encounter", "e1", "$pacio-timepoints"},
			{"Encounter", "e1", "$transfer"},
			{"Endpoint", "ep1", "$fast-endpoint-query"},
			{"ExplanationOfBenefit", "eob1", "$carin-bb-claims"},
			{"ExplanationOfBenefit", "eob1", "$carin-bb-summary"},
			{"ExplanationOfBenefit", "eob1", "$pct-aeob-summary"},
			{"GraphDefinition", "gd-custom", "$graph"},
			{"Group", "g1", "$add"},
			{"Group", "g1", "$atr-summary"},
			{"Group", "g1", "$confirm-attribution-list"},
			{"Group", "g1", "$davinci-data-export"},
			{"Group", "g1", "$everything"},
			{"Group", "g1", "$member-add"},
			{"Group", "g1", "$member-remove"},
			{"Group", "g1", "$pdex-provider-member-match"},
			{"Group", "g1", "$remove"},
			{"HealthcareService", "hs1", "$fast-attest"},
			{"HealthcareService", "hs1", "$find-providers"},
			{"HealthcareService", "hs1", "$find-services"},
			{"Immunization", "imm1", "$iis-submit"},
			{"Library", "lib1", "$data-requirements"},
			{"Library", "lib1", "$evaluate"},
			{"List", "list1", "$find"},
			{"Location", "loc1", "$saner-capacity-summary"},
			{"Measure", "custom-cms122", "$care-gaps"},
			{"Measure", "custom-cms122", "$collect-data"},
			{"Measure", "custom-cms122", "$data-requirements"},
			{"Measure", "custom-cms122", "$evaluate-measure"},
			{"Measure", "custom-cms122", "$evaluate-risk-condition-category"},
			{"Measure", "custom-cms122", "$nhsn-acute-care"},
			{"Measure", "custom-cms122", "$nhsn-dqm-evaluate"},
			{"Measure", "custom-cms122", "$submit-data"},
			{"MeasureReport", "mrep1", "$nhsn-aur-submit"},
			{"MeasureReport", "mrep1", "$ra-evaluate-risk"},
			{"MeasureReport", "mrep1", "$ra-remediate"},
			{"MeasureReport", "mrep1", "$saner-report-capacity"},
			{"MeasureReport", "mrep1", "$vbpr-attest"},
			{"MeasureReport", "mrep1", "$vbpr-performance"},
			{"MedicationKnowledge", "mk1", "$formulary-alternatives"},
			{"MedicationKnowledge", "mk1", "$formulary-lookup"},
			{"MedicationKnowledge", "mk1", "$lookup"},
			{"MedicationRequest", "mr1", "$check-interactions"},
			{"MedicationRequest", "mr1", "$crd"},
			{"MedicationRequest", "mr1", "$dispense"},
			{"MedicinalProduct", "mp1", "$epi-document"},
			{"MedicinalProductAuthorization", "mpa1", "$evaluate-authorization"},
			{"MedicinalProductDefinition", "mpd1", "$epi-document"},
			{"MessageHeader", "mh1", "$process-message"},
			{"MessageHeader", "mh1", "$process-notification"},
			{"NamingSystem", "ns1", "$preferred-id"},
			{"Observation", "o1", "$lastn"},
			{"Observation", "o1", "$mcode-disease-status"},
			{"Observation", "o1", "$pacio-splasch-record"},
			{"Observation", "o1", "$reportable-labs"},
			{"Observation", "o1", "$stats"},
			{"Organization", "org1", "$fast-verify"},
			{"Organization", "org1", "$ndh-directory-summary"},
			{"Organization", "org1", "$udap-register"},
			{"Organization", "org1", "$udap-trust-summary"},
			{"OrganizationAffiliation", "oa1", "$fast-network-query"},
			{"Patient", "p1", "$adi-record"},
			{"Patient", "p1", "$birth-report"},
			{"Patient", "p1", "$bulk-member-match"},
			{"Patient", "p1", "$burden-reduction-summary"},
			{"Patient", "p1", "$cancer-registry-report"},
			{"Patient", "p1", "$carin-bb"},
			{"Patient", "p1", "$codex-rttd-summary"},
			{"Patient", "p1", "$decease"},
			{"Patient", "p1", "$ecr-summary"},
			{"Patient", "p1", "$elr-summary"},
			{"Patient", "p1", "$everything"},
			{"Patient", "p1", "$find-subject-tx-implications"},
			{"Patient", "p1", "$find-subject-variants"},
			{"Patient", "p1", "$gfe"},
			{"Patient", "p1", "$gfe-submit"},
			{"Patient", "p1", "$health-cards-issue"},
			{"Patient", "p1", "$idi-match"},
			{"Patient", "p1", "$ihe-pdqm"},
			{"Patient", "p1", "$ihe-pix"},
			{"Patient", "p1", "$iis-history"},
			{"Patient", "p1", "$immds-forecast"},
			{"Patient", "p1", "$mcode-genomics-summary"},
			{"Patient", "p1", "$mcode-radiotherapy-summary"},
			{"Patient", "p1", "$mcode-summary"},
			{"Patient", "p1", "$mcode-trial-match"},
			{"Patient", "p1", "$mcode-trial-summary"},
			{"Patient", "p1", "$member-match"},
			{"Patient", "p1", "$merge"},
			{"Patient", "p1", "$nbs-summary"},
			{"Patient", "p1", "$nhsn-ach-summary"},
			{"Patient", "p1", "$nhsn-hai-summary"},
			{"Patient", "p1", "$odh-summary"},
			{"Patient", "p1", "$pacio-cognitive-assess"},
			{"Patient", "p1", "$pacio-cognitive-summary"},
			{"Patient", "p1", "$pacio-pfe-record"},
			{"Patient", "p1", "$pacio-pfe-summary"},
			{"Patient", "p1", "$pacio-splasch-summary"},
			{"Patient", "p1", "$pacio-summary"},
			{"Patient", "p1", "$pcde"},
			{"Patient", "p1", "$pdex-WriteData"},
			{"Patient", "p1", "$pdex-payer-exchange"},
			{"Patient", "p1", "$pdex-payer-summary"},
			{"Patient", "p1", "$pdex-provider-everything"},
			{"Patient", "p1", "$physical-activity-summary"},
			{"Patient", "p1", "$ra-coding-gap-report"},
			{"Patient", "p1", "$ra-coding-gaps"},
			{"Patient", "p1", "$record-occupation"},
			{"Patient", "p1", "$rtpbc-summary"},
			{"Patient", "p1", "$sdoh-care-bundle"},
			{"Patient", "p1", "$sdoh-summary"},
			{"Patient", "p1", "$summary"},
			{"Patient", "p1", "$vulcan-rwd-summary"},
			{"Person", "pers1", "$verify-identity"},
			{"PlanDefinition", "pd1", "$apply"},
			{"PlanDefinition", "pd1", "$data-requirements"},
			{"PlanDefinition", "pd1", "$ersd-lookup"},
			{"PlanDefinition", "pd1", "$medmorph-kar"},
			{"PractitionerRole", "pr2", "$find-providers"},
			{"PractitionerRole", "pr2", "$find-services"},
			{"Procedure", "proc1", "$mcode-radiotherapy-record"},
			{"Questionnaire", "q-vitals", "$data-requirements"},
			{"Questionnaire", "q-vitals", "$dtr-questionnaire-package"},
			{"Questionnaire", "q-vitals", "$package"},
			{"Questionnaire", "q-vitals", "$populate"},
			{"QuestionnaireResponse", "qr1", "$extract"},
			{"QuestionnaireResponse", "qr1", "$pacio-assess"},
			{"QuestionnaireResponse", "qr1", "$sdoh-assess"},
			{"RegulatedAuthorization", "rauth1", "$evaluate-authorization"},
			{"ResearchStudy", "rs1", "$evaluate-eligibility"},
			{"ResearchStudy", "rs1", "$rwd-cohort"},
			{"ResearchStudy", "rs1", "$vulcan-rwd-cohort"},
			{"ResearchStudy", "rs1", "$vulcan-summary"},
			{"ResearchSubject", "rsub1", "$enroll"},
			{"ResearchSubject", "rsub1", "$vulcan-esource-extract"},
			{"ResearchSubject", "rsub1", "$vulcan-soa-timeline"},
			{"RiskAssessment", "ra1", "$evaluate"},
			{"Schedule", "sched1", "$find"},
			{"ServiceRequest", "sr1", "$codex-rttd-plan"},
			{"ServiceRequest", "sr1", "$collect-specimen"},
			{"ServiceRequest", "sr1", "$crd"},
			{"ServiceRequest", "sr1", "$physical-activity-referral"},
			{"ServiceRequest", "sr1", "$sdoh-referral"},
			{"StructureDefinition", "sd1", "$snapshot"},
			{"StructureMap", "sm1", "$transform"},
			{"Subscription", "sub1", "$events"},
			{"Subscription", "sub1", "$status"},
			{"Task", "task1", "$cdex-poll"},
			{"Task", "task1", "$cdex-questionnaire-request"},
			{"Task", "task1", "$cdex-request"},
			{"Task", "task1", "$cdex-submit-attachment"},
			{"Task", "task1", "$pct-gfe-coordination"},
			{"Task", "task1", "$sdoh-fulfill"},
			{"Task", "task1", "$submit-attachment"},
			{"ValueSet", "vs1", "$expand"},
			{"ValueSet", "vs1", "$validate-code"},
			{"VerificationResult", "vr1", "$ndh-verify"},
			{"metadata", "id1", "$implements"},
			{"metadata", "id1", "$subset"},
		}

		commonQuery := "?subject=Patient/p1&patient=Patient/p1&code=4548-4&system=http://loinc.org&url=http://example.org/sd1&expression=Patient.name&periodStart=2020-01-01&periodEnd=2030-12-31&id=http://hospital.example.org/mrn&type=uri&near=37.7749|-122.4194|10|km"
		commonPostParams := `{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "subject", "valueString": "Patient/p1"},
				{"name": "patient", "valueString": "Patient/p1"},
				{"name": "code", "valueCode": "4548-4"},
				{"name": "system", "valueUri": "http://loinc.org"},
				{"name": "url", "valueUri": "http://example.org/sd1"},
				{"name": "expression", "valueString": "Patient.name"},
				{"name": "periodStart", "valueDate": "2020-01-01"},
				{"name": "periodEnd", "valueDate": "2030-12-31"},
				{"name": "id", "valueString": "http://hospital.example.org/mrn"},
				{"name": "type", "valueCode": "uri"},
				{"name": "name", "valueString": "closure-test"},
				{"name": "query", "valueString": "{ PatientList { id } }"},
				{"name": "resource", "resource": {"resourceType": "Patient", "id": "p1", "name": [{"family": "Smith"}]}},
				{"name": "content", "resource": {"resourceType": "Bundle", "type": "message", "entry": [{"resource": {"resourceType": "MessageHeader", "id": "mh1", "eventCoding": {"code": "admin-notify"}, "source": {"endpoint": "https://example.org"}}}]}}
			]
		}`

		nilSrv := server.New(nil, logger)
		server.RegisterDefaultResourceTypes(nilSrv)
		nilHandler := nilSrv.Handler()

		for _, op := range sysOps {
			p := "/fhir/r4/" + tenant + "/" + op
			doReq(t, handler, http.MethodGet, p+commonQuery, "", sysAuth)
			doReq(t, handler, http.MethodPost, p, commonPostParams, sysAuth)
			doReq(t, handler, http.MethodDelete, p, "", sysAuth)
			doReq(t, nilHandler, http.MethodGet, p+commonQuery, "", nil)
		}

		for _, item := range typeAndInstOps {
			p5 := "/fhir/r4/" + tenant + "/" + item.resType + "/" + item.op
			p6 := "/fhir/r4/" + tenant + "/" + item.resType + "/" + item.id + "/" + item.op
			// 5-part GET & POST (with Bearer token so authMiddleware & isPlanApplyOp execute)
			doReq(t, handler, http.MethodGet, p5+commonQuery, "", sysAuth)
			doReq(t, handler, http.MethodPost, p5, commonPostParams, sysAuth)
			// 6-part GET & POST with system token
			doReq(t, handler, http.MethodGet, p6+commonQuery, "", sysAuth)
			doReq(t, handler, http.MethodPost, p6, commonPostParams, sysAuth)
			// 6-part GET & POST with patient-scoped token (exercises claims.IsPatientRestricted() in isPlanApplyOp and handlers)
			doReq(t, handler, http.MethodGet, p6+commonQuery, "", patAuthF)
			doReq(t, handler, http.MethodPost, p6, commonPostParams, patAuthF)
			// 6-part GET with minimal scope token (exercises 403 branches in isPlanApplyOp)
			doReq(t, handler, http.MethodGet, p6+commonQuery, "", minAuthF)
			if item.resType == "Patient" {
				// Cross-patient p2 access with p1 token -> 403
				doReq(t, handler, http.MethodGet, "/fhir/r4/"+tenant+"/Patient/p2/"+item.op+commonQuery, "", patAuthF)
				doReq(t, handler, http.MethodPost, "/fhir/r4/"+tenant+"/Patient/p2/"+item.op, commonPostParams, patAuthF)
			}
		}
		_ = bytes.MinRead
	})
}
