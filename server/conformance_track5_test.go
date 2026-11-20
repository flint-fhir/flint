package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

func forEachTrack5Backend(t *testing.T, fn func(t *testing.T, bkName string, ts *httptest.Server, srv *server.Server)) {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	runWithStore := func(t *testing.T, bkName string, st store.Store, closeFn func() error) {
		t.Helper()
		defer func() {
			if closeFn != nil {
				_ = closeFn()
			}
		}()
		srv := server.New(st, logger)
		server.RegisterDefaultResourceTypes(srv)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		fn(t, bkName, ts, srv)
	}

	t.Run("postgres", func(t *testing.T) {
		pgDB := openMemPostgres(t)
		pgStore := postgres.New(pgDB, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
		runWithStore(t, "postgres", pgStore, pgDB.Close)
	})

	t.Run("sqlite", func(t *testing.T) {
		sqPath := filepath.Join(t.TempDir(), "track5.db")
		sqDB, err := sql.Open("sqlite", sqPath)
		require.NoError(t, err)
		sqDB.SetMaxOpenConns(1)
		require.NoError(t, sqlite.RunMigrations(ctx, sqDB, sqlite.JournalWAL))
		sqStore := sqlite.New(sqDB, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(sqPath))
		runWithStore(t, "sqlite", sqStore, sqDB.Close)
	})
}

func doTrack5HTTP(t *testing.T, client *http.Client, method, reqURL, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, reqURL, reader)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	return resp, raw
}

func outcomeErrorDiagnostics(t *testing.T, raw []byte) []string {
	t.Helper()
	var oo struct {
		ResourceType string `json:"resourceType"`
		Issue        []struct {
			Severity    string `json:"severity"`
			Code        string `json:"code"`
			Diagnostics string `json:"diagnostics"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(raw, &oo), string(raw))
	require.Equal(t, "OperationOutcome", oo.ResourceType)
	var errs []string
	for _, iss := range oo.Issue {
		if iss.Severity == "error" || iss.Severity == "fatal" {
			errs = append(errs, iss.Code+": "+iss.Diagnostics)
		}
	}
	return errs
}

func TestTrack5_ServiceBaseURLsAndSMARTBrands(t *testing.T) {
	forEachTrack5Backend(t, func(t *testing.T, _ string, ts *httptest.Server, _ *server.Server) {
		client := ts.Client()

		// 1. Service Base URLs at root and tenant discovery endpoints
		for _, ep := range []string{
			ts.URL + "/.well-known/service-base-urls",
			ts.URL + "/fhir/r4/demo/.well-known/service-base-urls",
		} {
			resp, raw := doTrack5HTTP(t, client, http.MethodGet, ep, "", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
			assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
			assert.NotEmpty(t, resp.Header.Get("ETag"))
			assert.NotEmpty(t, resp.Header.Get("Cache-Control"))

			var b struct {
				ResourceType string `json:"resourceType"`
				Type         string `json:"type"`
				Entry        []struct {
					FullURL  string         `json:"fullUrl"`
					Resource map[string]any `json:"resource"`
				} `json:"entry"`
			}
			require.NoError(t, json.Unmarshal(raw, &b), string(raw))
			assert.Equal(t, "Bundle", b.ResourceType)
			assert.Equal(t, "collection", b.Type)

			var orgCount, epCount int
			var hasPartOf, hasFHIRVersion bool
			for _, e := range b.Entry {
				rt, _ := e.Resource["resourceType"].(string)
				switch rt {
				case "Organization":
					orgCount++
					if partOf, ok := e.Resource["partOf"].(map[string]any); ok {
						if ref, _ := partOf["reference"].(string); ref == "Organization/org-flint-health-system" {
							hasPartOf = true
						}
					}
				case "Endpoint":
					epCount++
					assert.Equal(t, "active", e.Resource["status"])
					if exts, ok := e.Resource["extension"].([]any); ok {
						for _, rawExt := range exts {
							if extMap, ok := rawExt.(map[string]any); ok {
								if extMap["url"] == "http://hl7.org/fhir/StructureDefinition/endpoint-fhir-version" && extMap["valueCode"] == "4.0.1" {
									hasFHIRVersion = true
								}
							}
						}
					}
				}
			}
			assert.GreaterOrEqual(t, orgCount, 2)
			assert.GreaterOrEqual(t, epCount, 1)
			assert.True(t, hasPartOf, "expected Organization partOf hierarchy in %s", ep)
			assert.True(t, hasFHIRVersion, "expected endpoint-fhir-version=4.0.1 in %s", ep)
		}

		// 2. SMART User-Access Brands at root and tenant discovery endpoints + $validate
		for _, ep := range []string{
			ts.URL + "/.well-known/smart-brands.json",
			ts.URL + "/fhir/r4/demo/.well-known/smart-brands.json",
		} {
			resp, raw := doTrack5HTTP(t, client, http.MethodGet, ep, "", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
			assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
			assert.NotEmpty(t, resp.Header.Get("ETag"))

			var bundleMap map[string]any
			require.NoError(t, json.Unmarshal(raw, &bundleMap), string(raw))
			assert.Equal(t, "Bundle", bundleMap["resourceType"])
			metaMap, _ := bundleMap["meta"].(map[string]any)
			profiles, _ := metaMap["profile"].([]any)
			assert.Contains(t, profiles, "http://hl7.org/fhir/smart-app-launch/StructureDefinition/user-access-brands-bundle")

			var orgEndpointRef, portalEndpointRef string
			entries, _ := bundleMap["entry"].([]any)
			for _, rawEntry := range entries {
				em, _ := rawEntry.(map[string]any)
				res, _ := em["resource"].(map[string]any)
				if res["resourceType"] != "Organization" {
					continue
				}
				if eps, ok := res["endpoint"].([]any); ok && len(eps) > 0 {
					if firstEP, ok := eps[0].(map[string]any); ok {
						orgEndpointRef, _ = firstEP["reference"].(string)
					}
				}
				if exts, ok := res["extension"].([]any); ok {
					for _, rawExt := range exts {
						extMap, _ := rawExt.(map[string]any)
						if extMap["url"] != "http://hl7.org/fhir/StructureDefinition/organization-portal" {
							continue
						}
						subExts, _ := extMap["extension"].([]any)
						for _, rawSub := range subExts {
							subMap, _ := rawSub.(map[string]any)
							if subMap["url"] == "portalEndpoint" {
								vr, _ := subMap["valueReference"].(map[string]any)
								portalEndpointRef, _ = vr["reference"].(string)
							}
						}
					}
				}
			}
			require.NotEmpty(t, orgEndpointRef)
			assert.Equal(t, orgEndpointRef, portalEndpointRef)

			valResp, valRaw := doTrack5HTTP(
				t, client, http.MethodPost,
				ts.URL+"/fhir/r4/demo/Bundle/$validate",
				string(raw),
				map[string]string{"Content-Type": "application/fhir+json"},
			)
			require.Equal(t, http.StatusOK, valResp.StatusCode, string(valRaw))
			assert.Empty(t, outcomeErrorDiagnostics(t, valRaw), "smart-brands bundle validation errors")
		}
	})
}

func TestTrack5_SMARTSTU22_CORSAndPreflight(t *testing.T) {
	const testOrigin = "http://127.0.0.1:14569"

	forEachTrack5Backend(t, func(t *testing.T, _ string, ts *httptest.Server, srv *server.Server) {
		client := ts.Client()
		putTestResource(t, client, ts.URL, "demo", "Patient", "pat-alice-001", `{
			"resourceType": "Patient",
			"id": "pat-alice-001",
			"active": true,
			"gender": "female",
			"name": [{"family": "Wonderland", "given": ["Alice"]}]
		}`)
		srv.EnableDevTokenEnforcement()

		originHeaders := map[string]string{"Origin": testOrigin}

		// Discovery & metadata with Origin header
		respCfg, rawCfg := doTrack5HTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/demo/.well-known/smart-configuration", "", originHeaders)
		require.Equal(t, http.StatusOK, respCfg.StatusCode, string(rawCfg))
		assert.Equal(t, "*", respCfg.Header.Get("Access-Control-Allow-Origin"))
		var smartCfg map[string]any
		require.NoError(t, json.Unmarshal(rawCfg, &smartCfg))
		assert.Contains(t, smartCfg["code_challenge_methods_supported"], "S256")
		assert.Contains(t, smartCfg["response_types_supported"], "code")

		respMeta, _ := doTrack5HTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/demo/metadata", "", originHeaders)
		require.Equal(t, http.StatusOK, respMeta.StatusCode)
		assert.Equal(t, "*", respMeta.Header.Get("Access-Control-Allow-Origin"))

		// Preflight OPTIONS requests
		for _, optURL := range []string{ts.URL + "/oauth2/token", ts.URL + "/fhir/r4/demo/Patient"} {
			optResp, _ := doTrack5HTTP(t, client, http.MethodOptions, optURL, "", map[string]string{
				"Origin":                         testOrigin,
				"Access-Control-Request-Method":  "POST",
				"Access-Control-Request-Headers": "Authorization, Content-Type",
			})
			require.Equal(t, http.StatusNoContent, optResp.StatusCode)
			assert.Equal(t, "*", optResp.Header.Get("Access-Control-Allow-Origin"))
			assert.Contains(t, optResp.Header.Get("Access-Control-Allow-Headers"), "Authorization")
		}

		// Public PKCE flow + CORS on /oauth2/token and authenticated Patient search
		verifier, challenge := pkceS256Pair()
		redirectURI := testOrigin + "/callback"
		authParams := url.Values{
			"response_type":         {"code"},
			"client_id":             {"track5-stu22-public"},
			"redirect_uri":          {redirectURI},
			"scope":                 {"launch/patient openid fhirUser patient/Patient.rs"},
			"state":                 {"stu22-state"},
			"aud":                   {ts.URL + "/fhir/r4/demo"},
			"patient":               {"pat-alice-001"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
		}
		noRedir := noRedirectClient()
		authResp, err := noRedir.Get(ts.URL + "/oauth2/authorize?" + authParams.Encode())
		require.NoError(t, err)
		authResp.Body.Close()
		require.Equal(t, http.StatusFound, authResp.StatusCode)

		locURL, err := url.Parse(authResp.Header.Get("Location"))
		require.NoError(t, err)
		authCode := locURL.Query().Get("code")
		require.NotEmpty(t, authCode)

		formBody := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {authCode},
			"redirect_uri":  {redirectURI},
			"client_id":     {"track5-stu22-public"},
			"code_verifier": {verifier},
		}.Encode()
		tokResp, tokRaw := doTrack5HTTP(t, client, http.MethodPost, ts.URL+"/oauth2/token", formBody, map[string]string{
			"Origin":       testOrigin,
			"Content-Type": "application/x-www-form-urlencoded",
		})
		require.Equal(t, http.StatusOK, tokResp.StatusCode, string(tokRaw))
		assert.Equal(t, "*", tokResp.Header.Get("Access-Control-Allow-Origin"))

		var tokPayload struct {
			AccessToken string `json:"access_token"`
		}
		require.NoError(t, json.Unmarshal(tokRaw, &tokPayload))
		require.NotEmpty(t, tokPayload.AccessToken)

		patResp, patRaw := doTrack5HTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/demo/Patient", "", map[string]string{
			"Origin":        testOrigin,
			"Authorization": "Bearer " + tokPayload.AccessToken,
		})
		require.Equal(t, http.StatusOK, patResp.StatusCode, string(patRaw))
		assert.Equal(t, "*", patResp.Header.Get("Access-Control-Allow-Origin"))
		assert.Contains(t, string(patRaw), "pat-alice-001")
	})
}

func TestTrack5_AUCoreExtensionsSearchAndValidation(t *testing.T) {
	auBundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry": []map[string]any{
			{
				"resource": map[string]any{
					"resourceType": "Organization",
					"id":           "org-au-001",
					"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-organization"}},
					"identifier":   []map[string]any{{"system": "http://ns.electronichealth.net.au/id/hi/hpio/1.0", "value": "8003621566684455"}},
					"active":       true,
					"name":         "Sydney Harbour Medical Centre",
				},
				"request": map[string]any{"method": "PUT", "url": "Organization/org-au-001"},
			},
			{
				"resource": map[string]any{
					"resourceType":         "Location",
					"id":                   "loc-au-001",
					"meta":                 map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-location"}},
					"status":               "active",
					"name":                 "Harbour Clinic Wing A",
					"managingOrganization": map[string]any{"reference": "Organization/org-au-001"},
				},
				"request": map[string]any{"method": "PUT", "url": "Location/loc-au-001"},
			},
			{
				"resource": map[string]any{
					"resourceType": "Practitioner",
					"id":           "prac-au-001",
					"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-practitioner"}},
					"identifier":   []map[string]any{{"system": "http://ns.electronichealth.net.au/id/hi/hpii/1.0", "value": "8003619900015717"}},
					"active":       true,
					"name":         []map[string]any{{"use": "official", "family": "OConnor", "given": []string{"Liam"}}},
				},
				"request": map[string]any{"method": "PUT", "url": "Practitioner/prac-au-001"},
			},
			{
				"resource": map[string]any{
					"resourceType": "PractitionerRole",
					"id":           "pr-au-001",
					"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-practitionerrole"}},
					"identifier":   []map[string]any{{"system": "http://ns.electronichealth.net.au/id/medicare-provider-number", "value": "2426621B"}},
					"active":       true,
					"practitioner": map[string]any{"reference": "Practitioner/prac-au-001"},
					"organization": map[string]any{"reference": "Organization/org-au-001"},
				},
				"request": map[string]any{"method": "PUT", "url": "PractitionerRole/pr-au-001"},
			},
			{
				"resource": map[string]any{
					"resourceType": "Patient",
					"id":           "pat-au-001",
					"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-patient"}},
					"extension": []map[string]any{
						{
							"url":         "http://hl7.org.au/fhir/StructureDefinition/indigenous-status",
							"valueCoding": map[string]any{"system": "https://healthterminologies.gov.au/fhir/CodeSystem/australian-indigenous-status-1", "code": "4"},
						},
						{
							"url": "http://hl7.org/fhir/StructureDefinition/individual-genderIdentity",
							"extension": []map[string]any{
								{"url": "value", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "446141000124107"}}}},
							},
						},
					},
					"identifier": []map[string]any{
						{"system": "http://ns.electronichealth.net.au/id/hi/ihi/1.0", "value": "8003608833357361"},
						{"system": "http://ns.electronichealth.net.au/id/medicare-number", "value": "32788511952"},
					},
					"active":    true,
					"name":      []map[string]any{{"use": "official", "family": "Banks", "given": []string{"Matilda"}}},
					"gender":    "female",
					"birthDate": "1988-06-15",
				},
				"request": map[string]any{"method": "PUT", "url": "Patient/pat-au-001"},
			},
			{
				"resource": map[string]any{
					"resourceType": "Patient",
					"id":           "pat-au-002",
					"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-patient"}},
					"extension": []map[string]any{
						{
							"url":         "http://hl7.org.au/fhir/StructureDefinition/indigenous-status",
							"valueCoding": map[string]any{"system": "https://healthterminologies.gov.au/fhir/CodeSystem/australian-indigenous-status-1", "code": "1"},
						},
						{
							"url": "http://hl7.org/fhir/StructureDefinition/individual-genderIdentity",
							"extension": []map[string]any{
								{"url": "value", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "446151000124109"}}}},
							},
						},
					},
					"identifier": []map[string]any{
						{"system": "http://ns.electronichealth.net.au/id/hi/ihi/1.0", "value": "8003608833357379"},
						{"system": "http://ns.electronichealth.net.au/id/dva", "value": "VX900123"},
					},
					"active":    true,
					"name":      []map[string]any{{"use": "official", "family": "Mercer", "given": []string{"Jack"}}},
					"gender":    "male",
					"birthDate": "1975-03-22",
				},
				"request": map[string]any{"method": "PUT", "url": "Patient/pat-au-002"},
			},
			{
				"resource": map[string]any{
					"resourceType":      "Condition",
					"id":                "cond-au-001",
					"meta":              map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-condition"}},
					"clinicalStatus":    map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}}},
					"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item"}}}},
					"code":              map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "59621000"}}},
					"subject":           map[string]any{"reference": "Patient/pat-au-001"},
					"onsetDateTime":     "2024-05-01",
					"abatementDateTime": "2026-01-01",
					"recordedDate":      "2026-01-15T09:10:00Z",
				},
				"request": map[string]any{"method": "PUT", "url": "Condition/cond-au-001"},
			},
			{
				"resource": map[string]any{
					"resourceType": "Medication",
					"id":           "med-au-001",
					"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-medication"}},
					"code":         map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "376988009"}}},
				},
				"request": map[string]any{"method": "PUT", "url": "Medication/med-au-001"},
			},
			{
				"resource": map[string]any{
					"resourceType":        "MedicationStatement",
					"id":                  "ms-au-001",
					"meta":                map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-medicationstatement"}},
					"status":              "active",
					"medicationReference": map[string]any{"reference": "Medication/med-au-001"},
					"subject":             map[string]any{"reference": "Patient/pat-au-001"},
					"effectiveDateTime":   "2026-01-15T09:20:00Z",
				},
				"request": map[string]any{"method": "PUT", "url": "MedicationStatement/ms-au-001"},
			},
			{
				"resource": map[string]any{
					"resourceType":      "Observation",
					"id":                "obs-au-bp-1",
					"meta":              map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-bloodpressure"}},
					"status":            "final",
					"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
					"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "85354-9"}}},
					"subject":           map[string]any{"reference": "Patient/pat-au-001"},
					"effectiveDateTime": "2026-01-15T09:05:00Z",
					"component": []map[string]any{
						{"code": map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8480-6"}}}, "valueQuantity": map[string]any{"value": 120.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}},
						{"code": map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8462-4"}}}, "valueQuantity": map[string]any{"value": 80.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}},
					},
				},
				"request": map[string]any{"method": "PUT", "url": "Observation/obs-au-bp-1"},
			},
		},
	}
	bundleBytes, err := json.Marshal(auBundle)
	require.NoError(t, err)

	forEachTrack5Backend(t, func(t *testing.T, _ string, ts *httptest.Server, _ *server.Server) {
		client := ts.Client()
		txResp, txRaw := doTrack5HTTP(t, client, http.MethodPost, ts.URL+"/fhir/r4/aucore", string(bundleBytes), map[string]string{
			"Content-Type": "application/fhir+json",
		})
		require.Equal(t, http.StatusOK, txResp.StatusCode, string(txRaw))

		// 1. AU Core extension searches
		respIndig, rawIndig := doTrack5HTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/aucore/Patient?indigenous-status=1", "", nil)
		require.Equal(t, http.StatusOK, respIndig.StatusCode, string(rawIndig))
		assert.Contains(t, string(rawIndig), "pat-au-002")
		assert.NotContains(t, string(rawIndig), "pat-au-001")

		respGI, rawGI := doTrack5HTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/aucore/Patient?gender-identity=446141000124107", "", nil)
		require.Equal(t, http.StatusOK, respGI.StatusCode, string(rawGI))
		assert.Contains(t, string(rawGI), "pat-au-001")
		assert.NotContains(t, string(rawGI), "pat-au-002")

		// 2. MedicationStatement date + patient search
		respMS, rawMS := doTrack5HTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/aucore/MedicationStatement?patient=Patient/pat-au-001&effective=ge2026-01-01", "", nil)
		require.Equal(t, http.StatusOK, respMS.StatusCode, string(rawMS))
		assert.Contains(t, string(rawMS), "ms-au-001")

		// 3. PractitionerRole chained identifier + _include
		prQuery := ts.URL + "/fhir/r4/aucore/PractitionerRole?practitioner.identifier=" +
			url.QueryEscape("http://ns.electronichealth.net.au/id/hi/hpii/1.0|8003619900015717") +
			"&_include=PractitionerRole:practitioner"
		respPR, rawPR := doTrack5HTTP(t, client, http.MethodGet, prQuery, "", nil)
		require.Equal(t, http.StatusOK, respPR.StatusCode, string(rawPR))
		var prBundle struct {
			Entry []struct {
				Search struct {
					Mode string `json:"mode"`
				} `json:"search"`
				Resource map[string]any `json:"resource"`
			} `json:"entry"`
		}
		require.NoError(t, json.Unmarshal(rawPR, &prBundle))
		modesByID := make(map[string]string)
		for _, e := range prBundle.Entry {
			id, _ := e.Resource["id"].(string)
			modesByID[id] = e.Search.Mode
		}
		assert.Equal(t, "match", modesByID["pr-au-001"])
		assert.Equal(t, "include", modesByID["prac-au-001"])

		// 4. CapabilityStatement advertisements
		respMeta, rawMeta := doTrack5HTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/aucore/metadata", "", nil)
		require.Equal(t, http.StatusOK, respMeta.StatusCode, string(rawMeta))
		metaStr := string(rawMeta)
		assert.Contains(t, metaStr, "http://hl7.org.au/fhir/core/StructureDefinition/au-core-patient")
		assert.Contains(t, metaStr, "http://hl7.org/fhir/smart-app-launch/StructureDefinition/user-access-brands-bundle")

		var capStmt struct {
			Rest []struct {
				Resource []struct {
					Type        string `json:"type"`
					SearchParam []struct {
						Name string `json:"name"`
					} `json:"searchParam"`
				} `json:"resource"`
			} `json:"rest"`
		}
		require.NoError(t, json.Unmarshal(rawMeta, &capStmt))
		spByType := make(map[string]map[string]bool)
		for _, r := range capStmt.Rest {
			for _, res := range r.Resource {
				m := make(map[string]bool)
				for _, sp := range res.SearchParam {
					m[sp.Name] = true
				}
				spByType[res.Type] = m
			}
		}
		assert.True(t, spByType["MedicationStatement"]["effective"])
		assert.True(t, spByType["Condition"]["recorded-date"])
		assert.True(t, spByType["Condition"]["onset-date"])
		assert.True(t, spByType["Condition"]["abatement-date"])

		// 5. $validate against au-core-patient (positive & negative)
		validPatBytes, err := json.Marshal(auBundle["entry"].([]map[string]any)[4]["resource"])
		require.NoError(t, err)
		valURL := ts.URL + "/fhir/r4/aucore/Patient/$validate?profile=" +
			url.QueryEscape("http://hl7.org.au/fhir/core/StructureDefinition/au-core-patient")

		valOKResp, valOKRaw := doTrack5HTTP(t, client, http.MethodPost, valURL, string(validPatBytes), map[string]string{
			"Content-Type": "application/fhir+json",
		})
		require.Equal(t, http.StatusOK, valOKResp.StatusCode, string(valOKRaw))
		assert.Empty(t, outcomeErrorDiagnostics(t, valOKRaw))

		valBadResp, valBadRaw := doTrack5HTTP(t, client, http.MethodPost, valURL, `{"resourceType":"Patient","id":"pat-au-invalid"}`, map[string]string{
			"Content-Type": "application/fhir+json",
		})
		require.Equal(t, http.StatusBadRequest, valBadResp.StatusCode, string(valBadRaw))
		errs := outcomeErrorDiagnostics(t, valBadRaw)
		require.NotEmpty(t, errs, "expected validation error for Patient missing required name")
		assert.True(t, bytes.Contains(bytes.ToLower(valBadRaw), []byte("name")))
	})
}
