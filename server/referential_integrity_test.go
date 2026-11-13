package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

func TestParseReferentialIntegrityModeAndCapabilityPolicy(t *testing.T) {
	mode, err := server.ParseReferentialIntegrityMode("")
	require.NoError(t, err)
	assert.Equal(t, server.ReferentialIntegrityLenient, mode)

	for _, raw := range []string{"lenient", "off", "false", "disabled"} {
		m, pErr := server.ParseReferentialIntegrityMode(raw)
		require.NoError(t, pErr)
		assert.Equal(t, server.ReferentialIntegrityLenient, m)
	}
	for _, raw := range []string{"strict", "enforced", "true", "on", " STRICT "} {
		m, pErr := server.ParseReferentialIntegrityMode(raw)
		require.NoError(t, pErr)
		assert.Equal(t, server.ReferentialIntegrityStrict, m)
	}
	_, err = server.ParseReferentialIntegrityMode("unknown-policy")
	require.Error(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ss := sqlite.New(nil, sqlite.WithDSN(":memory:"))
	srv := server.New(ss, logger)
	server.RegisterDefaultResourceTypes(srv)
	assert.Equal(t, server.ReferentialIntegrityLenient, srv.ReferentialIntegrity())

	// Lenient CapabilityStatement omits "enforced"
	req := httptest.NewRequest(http.MethodGet, "/fhir/r4/demo/metadata", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var capLenient map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &capLenient))
	restArr := capLenient["rest"].([]any)
	resArr := restArr[0].(map[string]any)["resource"].([]any)
	patEntry := resArr[0].(map[string]any)
	assert.Equal(t, []any{"literal", "logical", "local"}, patEntry["referencePolicy"])

	// Strict CapabilityStatement advertises "enforced"
	srv.SetReferentialIntegrity(server.ReferentialIntegrityStrict)
	assert.Equal(t, server.ReferentialIntegrityStrict, srv.ReferentialIntegrity())

	reqStrict := httptest.NewRequest(http.MethodGet, "/fhir/r4/demo/metadata", nil)
	recStrict := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recStrict, reqStrict)
	require.Equal(t, http.StatusOK, recStrict.Code)

	var capStrict map[string]any
	require.NoError(t, json.Unmarshal(recStrict.Body.Bytes(), &capStrict))
	patStrict := capStrict["rest"].([]any)[0].(map[string]any)["resource"].([]any)[0].(map[string]any)
	assert.Equal(t, []any{"literal", "logical", "local", "enforced"}, patStrict["referencePolicy"])
}

func TestReferentialIntegrityAndCascadeDeleteAcrossBackends(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backends := []struct {
		name  string
		setup func(t *testing.T) (*server.Server, func())
	}{
		{
			name: "postgres",
			setup: func(t *testing.T) (*server.Server, func()) {
				db := openMemPostgres(t)
				pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
				srv := server.New(pgStore, logger)
				server.RegisterDefaultResourceTypes(srv)
				return srv, func() { _ = db.Close() }
			},
		},
		{
			name: "sqlite",
			setup: func(t *testing.T) (*server.Server, func()) {
				ss := sqlite.New(nil, sqlite.WithDSN(":memory:"))
				srv := server.New(ss, logger)
				server.RegisterDefaultResourceTypes(srv)
				return srv, func() {}
			},
		},
		{
			name: "doris",
			setup: func(t *testing.T) (*server.Server, func()) {
				ds := doris.New(nil)
				srv := server.New(ds, logger)
				server.RegisterDefaultResourceTypes(srv)
				return srv, func() {}
			},
		},
	}

	doJSON := func(t *testing.T, h http.Handler, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = bytes.NewBufferString(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		if body != "" {
			if ct, ok := headers["Content-Type"]; ok {
				req.Header.Set("Content-Type", ct)
			} else {
				req.Header.Set("Content-Type", "application/fhir+json")
			}
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			srv, cleanup := b.setup(t)
			defer cleanup()
			srv.SetReferentialIntegrity(server.ReferentialIntegrityStrict)
			h := srv.Handler()

			// 1. POST, PUT, and Conditional PUT with dangling target fail with 422 Unprocessable Entity
			rec := doJSON(t, h, http.MethodPost, "/fhir/r4/demo/Observation", `{
				"resourceType": "Observation",
				"status": "final",
				"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
				"subject": {"reference": "Patient/missing-001"}
			}`, nil)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "Patient/missing-001")

			// Per-request header override to lenient allows dangling reference when requested
			recLenient := doJSON(t, h, http.MethodPut, "/fhir/r4/demo/Observation/obs-lenient-1", `{
				"resourceType": "Observation",
				"id": "obs-lenient-1",
				"status": "final",
				"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
				"subject": {"reference": "Patient/missing-lenient"}
			}`, map[string]string{"X-Flint-Referential-Integrity": "lenient"})
			require.Equal(t, http.StatusCreated, recLenient.Code, recLenient.Body.String())

			recPut := doJSON(t, h, http.MethodPut, "/fhir/r4/demo/Observation/obs-dangling", `{
				"resourceType": "Observation",
				"id": "obs-dangling",
				"status": "final",
				"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
				"subject": {"reference": "Patient/missing-002"}
			}`, nil)
			assert.Equal(t, http.StatusUnprocessableEntity, recPut.Code, recPut.Body.String())

			recCondPut := doJSON(t, h, http.MethodPut, "/fhir/r4/demo/Observation?code=8867-4", `{
				"resourceType": "Observation",
				"status": "final",
				"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
				"subject": {"reference": "Patient/missing-003"}
			}`, nil)
			assert.Equal(t, http.StatusUnprocessableEntity, recCondPut.Code, recCondPut.Body.String())

			// 2. Self-reference, external URL, logical identifier-only ref, and #contained fragment succeed in strict mode
			recPat := doJSON(t, h, http.MethodPut, "/fhir/r4/demo/Patient/pat-strict-1", `{
				"resourceType": "Patient",
				"id": "pat-strict-1",
				"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-STRICT-1"}],
				"active": true,
				"name": [{"family": "Strict", "given": ["Alice"]}],
				"managingOrganization": {"reference": "https://external.example.org/Organization/ext-org"},
				"generalPractitioner": [{"identifier": {"system": "http://hl7.org/fhir/sid/us-npi", "value": "1234567893"}}],
				"link": [{"other": {"reference": "Patient/pat-strict-1"}, "type": "seealso"}]
			}`, nil)
			require.Equal(t, http.StatusCreated, recPat.Code, recPat.Body.String())

			recObsOK := doJSON(t, h, http.MethodPut, "/fhir/r4/demo/Observation/obs-strict-1", `{
				"resourceType": "Observation",
				"id": "obs-strict-1",
				"contained": [{"resourceType": "Practitioner", "id": "inline-prac", "name": [{"family": "Inline"}]}],
				"status": "final",
				"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
				"subject": {"reference": "Patient/pat-strict-1/_history/1"},
				"performer": [{"reference": "#inline-prac"}]
			}`, nil)
			require.Equal(t, http.StatusCreated, recObsOK.Code, recObsOK.Body.String())

			// Multi-hop DiagnosticReport -> Observation -> Patient
			recDR := doJSON(t, h, http.MethodPut, "/fhir/r4/demo/DiagnosticReport/dr-strict-1", `{
				"resourceType": "DiagnosticReport",
				"id": "dr-strict-1",
				"status": "final",
				"code": {"coding": [{"system": "http://loinc.org", "code": "11502-2"}]},
				"subject": {"reference": "Patient/pat-strict-1"},
				"result": [{"reference": "Observation/obs-strict-1"}]
			}`, nil)
			require.Equal(t, http.StatusCreated, recDR.Code, recDR.Body.String())

			// 3. PATCH changing subject to non-existent Patient fails with 422
			recPatch := doJSON(t, h, http.MethodPatch, "/fhir/r4/demo/Observation/obs-strict-1", `[
				{"op": "replace", "path": "/subject/reference", "value": "Patient/missing-patched"}
			]`, map[string]string{"Content-Type": "application/json-patch+json"})
			assert.Equal(t, http.StatusUnprocessableEntity, recPatch.Code, recPatch.Body.String())

			// 4. DELETE and Conditional DELETE on referenced Patient fail with 409 Conflict
			recDelBlocked := doJSON(t, h, http.MethodDelete, "/fhir/r4/demo/Patient/pat-strict-1", "", nil)
			assert.Equal(t, http.StatusConflict, recDelBlocked.Code, recDelBlocked.Body.String())
			assert.Contains(t, recDelBlocked.Body.String(), "Observation/obs-strict-1")

			recCondDelBlocked := doJSON(t, h, http.MethodDelete, "/fhir/r4/demo/Patient?identifier=MRN-STRICT-1", "", nil)
			assert.Equal(t, http.StatusConflict, recCondDelBlocked.Code, recCondDelBlocked.Body.String())

			// Invalid _cascade parameter returns 400 Bad Request
			recBadCascade := doJSON(t, h, http.MethodDelete, "/fhir/r4/demo/Patient/pat-strict-1?_cascade=bogus", "", nil)
			assert.Equal(t, http.StatusBadRequest, recBadCascade.Code, recBadCascade.Body.String())

			// 5. DELETE ?_cascade=delete cascades through DiagnosticReport -> Observation -> Patient
			recCascade := doJSON(t, h, http.MethodDelete, "/fhir/r4/demo/Patient/pat-strict-1?_cascade=delete", "",
				map[string]string{"Prefer": "return=OperationOutcome"})
			require.Equal(t, http.StatusOK, recCascade.Code, recCascade.Body.String())
			assert.Contains(t, recCascade.Body.String(), "cascade-deleted")

			for _, goneURL := range []string{
				"/fhir/r4/demo/Patient/pat-strict-1",
				"/fhir/r4/demo/Observation/obs-strict-1",
				"/fhir/r4/demo/DiagnosticReport/dr-strict-1",
			} {
				recGone := doJSON(t, h, http.MethodGet, goneURL, "", nil)
				assert.Equal(t, http.StatusGone, recGone.Code, "expected 410 Gone for %s", goneURL)
			}

			// 6. Transaction Bundle: forward reference succeeds; dangling reference rolls back with 422; co-delete succeeds
			recTxOK := doJSON(t, h, http.MethodPost, "/fhir/r4/demo", `{
				"resourceType": "Bundle",
				"type": "transaction",
				"entry": [
					{
						"request": {"method": "PUT", "url": "Observation/obs-tx-fwd"},
						"resource": {
							"resourceType": "Observation",
							"id": "obs-tx-fwd",
							"status": "final",
							"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
							"subject": {"reference": "Patient/pat-tx-fwd"}
						}
					},
					{
						"request": {"method": "PUT", "url": "Patient/pat-tx-fwd"},
						"resource": {
							"resourceType": "Patient",
							"id": "pat-tx-fwd",
							"name": [{"family": "Forward"}]
						}
					}
				]
			}`, nil)
			require.Equal(t, http.StatusOK, recTxOK.Code, recTxOK.Body.String())

			// Transaction co-deleting both Observation and Patient in strict mode succeeds
			recTxCoDel := doJSON(t, h, http.MethodPost, "/fhir/r4/demo", `{
				"resourceType": "Bundle",
				"type": "transaction",
				"entry": [
					{"request": {"method": "DELETE", "url": "Patient/pat-tx-fwd"}},
					{"request": {"method": "DELETE", "url": "Observation/obs-tx-fwd"}}
				]
			}`, nil)
			require.Equal(t, http.StatusOK, recTxCoDel.Code, recTxCoDel.Body.String())

			// Transaction with dangling reference returns 422
			recTxBad := doJSON(t, h, http.MethodPost, "/fhir/r4/demo", `{
				"resourceType": "Bundle",
				"type": "transaction",
				"entry": [
					{
						"request": {"method": "POST", "url": "Observation"},
						"resource": {
							"resourceType": "Observation",
							"status": "final",
							"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
							"subject": {"reference": "Patient/never-existed"}
						}
					}
				]
			}`, nil)
			assert.Equal(t, http.StatusUnprocessableEntity, recTxBad.Code, recTxBad.Body.String())

			// Batch Bundle returns per-entry 422 on dangling ref without failing valid entries
			recBatch := doJSON(t, h, http.MethodPost, "/fhir/r4/demo", `{
				"resourceType": "Bundle",
				"type": "batch",
				"entry": [
					{
						"request": {"method": "PUT", "url": "Patient/pat-batch-1"},
						"resource": {"resourceType": "Patient", "id": "pat-batch-1", "name": [{"family": "Batch"}]}
					},
					{
						"request": {"method": "POST", "url": "Observation"},
						"resource": {
							"resourceType": "Observation",
							"status": "final",
							"code": {"text": "HR"},
							"subject": {"reference": "Patient/missing-batch"}
						}
					}
				]
			}`, nil)
			require.Equal(t, http.StatusOK, recBatch.Code, recBatch.Body.String())
			assert.Contains(t, recBatch.Body.String(), "201 Created")
			assert.Contains(t, recBatch.Body.String(), "422 Unprocessable Entity")
		})
	}
}
