package server_test

import (
	"bytes"
	"compress/flate"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/sqlite"
)

func newInfernoV070TestServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "inferno-v070.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, sqlite.RunMigrations(t.Context(), sqlDB, sqlite.JournalWAL))
	st := sqlite.New(sqlDB, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(dbPath))

	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	srv.EnableDevTokenEnforcement()

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func mintDevBearerToken(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := ts.Client().Post(
		ts.URL+"/oauth2/token",
		"application/x-www-form-urlencoded",
		strings.NewReader("grant_type=client_credentials&client_id=v070-test&scope=system/*.cruds"),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var doc struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	require.NotEmpty(t, doc.AccessToken)
	return doc.AccessToken
}

func TestInfernoV070_SmartHealthCardsES256AndSchedulingLinks(t *testing.T) {
	_, ts := newInfernoV070TestServer(t)
	tok := mintDevBearerToken(t, ts)

	// Seed Patient + COVID-19 CVX 208 Immunization
	putAuth := func(path string, payload map[string]any) {
		t.Helper()
		raw, mErr := json.Marshal(payload)
		require.NoError(t, mErr)
		req, rErr := http.NewRequestWithContext(t.Context(), http.MethodPut, ts.URL+path, bytes.NewReader(raw))
		require.NoError(t, rErr)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/fhir+json")
		resp, dErr := ts.Client().Do(req)
		require.NoError(t, dErr)
		defer resp.Body.Close()
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, resp.StatusCode)
	}
	putAuth("/fhir/r4/demo/Patient/pat-shc-1", map[string]any{
		"resourceType": "Patient",
		"id":           "pat-shc-1",
		"name":         []map[string]any{{"family": "Cardholder", "given": []string{"Alice"}}},
		"birthDate":    "1985-04-12",
		"gender":       "female",
	})
	putAuth("/fhir/r4/demo/Immunization/imm-shc-covid", map[string]any{
		"resourceType":       "Immunization",
		"id":                 "imm-shc-covid",
		"status":             "completed",
		"vaccineCode":        map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/cvx", "code": "208"}}},
		"patient":            map[string]any{"reference": "Patient/pat-shc-1"},
		"occurrenceDateTime": "2024-10-15T09:00:00Z",
		"lotNumber":          "EL9262",
	})

	// 1. Verify /.well-known/smart-configuration advertises health-cards capability
	smartResp, err := ts.Client().Get(ts.URL + "/fhir/r4/demo/.well-known/smart-configuration")
	require.NoError(t, err)
	defer smartResp.Body.Close()
	require.Equal(t, http.StatusOK, smartResp.StatusCode)
	var smartCfg map[string]any
	require.NoError(t, json.NewDecoder(smartResp.Body).Decode(&smartCfg))
	assert.Contains(t, smartCfg["capabilities"], "health-cards")

	// 2. Verify /.well-known/jwks.json exposes EC P-256 ES256 key and CORS headers
	jwksResp, err := ts.Client().Get(ts.URL + "/fhir/r4/demo/.well-known/jwks.json")
	require.NoError(t, err)
	defer jwksResp.Body.Close()
	require.Equal(t, http.StatusOK, jwksResp.StatusCode)
	assert.Equal(t, "*", jwksResp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "*", jwksResp.Header.Get("Control-Allow-Origin"))

	var jwksDoc struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.NewDecoder(jwksResp.Body).Decode(&jwksDoc))
	var ecKeyMap map[string]any
	for _, k := range jwksDoc.Keys {
		if k["kty"] == "EC" && k["crv"] == "P-256" && k["alg"] == "ES256" {
			ecKeyMap = k
			break
		}
	}
	require.NotNil(t, ecKeyMap, "expected ES256 P-256 JWK in /.well-known/jwks.json")
	xBytes, err := base64.RawURLEncoding.DecodeString(ecKeyMap["x"].(string))
	require.NoError(t, err)
	yBytes, err := base64.RawURLEncoding.DecodeString(ecKeyMap["y"].(string))
	require.NoError(t, err)
	uncompressed := make([]byte, 0, 1+len(xBytes)+len(yBytes))
	uncompressed = append(uncompressed, 0x04)
	uncompressed = append(uncompressed, xBytes...)
	uncompressed = append(uncompressed, yBytes...)
	pubKey, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), uncompressed)
	require.NoError(t, err)

	// 3. Issue SMART Health Card via POST $health-cards-issue (unauthenticated allowed & lab + immunization)
	issueParams := map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "credentialType", "valueUri": "https://smarthealth.cards#immunization"},
			{"name": "credentialType", "valueUri": "https://smarthealth.cards#laboratory"},
			{"name": "credentialType", "valueUri": "https://smarthealth.cards#covid19"},
		},
	}
	issueBytes, _ := json.Marshal(issueParams)
	issueResp, err := ts.Client().Post(
		ts.URL+"/fhir/r4/demo/Patient/pat-shc-1/$health-cards-issue",
		"application/fhir+json",
		bytes.NewReader(issueBytes),
	)
	require.NoError(t, err)
	defer issueResp.Body.Close()
	require.Equal(t, http.StatusOK, issueResp.StatusCode)

	var outParams struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name        string `json:"name"`
			ValueString string `json:"valueString"`
		} `json:"parameter"`
	}
	require.NoError(t, json.NewDecoder(issueResp.Body).Decode(&outParams))
	assert.Equal(t, "Parameters", outParams.ResourceType)

	var jws string
	for _, p := range outParams.Parameter {
		if p.Name == "verifiableCredential" && p.ValueString != "" {
			jws = p.ValueString
			break
		}
	}
	require.NotEmpty(t, jws)
	jwsParts := strings.Split(jws, ".")
	require.Len(t, jwsParts, 3)

	// Verify compact JWS header key order, ES256 signature, and raw DEFLATE decompression
	hdrJSON, err := base64.RawURLEncoding.DecodeString(jwsParts[0])
	require.NoError(t, err)
	assert.Contains(t, string(hdrJSON), `"alg":"ES256"`)
	assert.Contains(t, string(hdrJSON), `"zip":"DEF"`)

	sigRaw, err := base64.RawURLEncoding.DecodeString(jwsParts[2])
	require.NoError(t, err)
	require.Len(t, sigRaw, 64, "ES256 IEEE P1363 signature must be 64 bytes")
	digest := sha256.Sum256([]byte(jwsParts[0] + "." + jwsParts[1]))
	rInt := new(big.Int).SetBytes(sigRaw[:32])
	sInt := new(big.Int).SetBytes(sigRaw[32:])
	assert.True(t, ecdsa.Verify(pubKey, digest[:], rInt, sInt), "ES256 JWS signature verification failed")

	compressedPayload, err := base64.RawURLEncoding.DecodeString(jwsParts[1])
	require.NoError(t, err)
	inflated, err := io.ReadAll(flate.NewReader(bytes.NewReader(compressedPayload)))
	require.NoError(t, err)
	var vcClaims map[string]any
	require.NoError(t, json.Unmarshal(inflated, &vcClaims))
	assert.Equal(t, ts.URL+"/fhir/r4/demo", vcClaims["iss"])

	// 4. Download .smart-health-card file unauthenticated (plus 404 on missing patient)
	dlResp, err := ts.Client().Get(ts.URL + "/fhir/r4/demo/Patient/pat-shc-1/$health-cards.smart-health-card")
	require.NoError(t, err)
	defer dlResp.Body.Close()
	require.Equal(t, http.StatusOK, dlResp.StatusCode)
	assert.Contains(t, dlResp.Header.Get("Content-Type"), "application/smart-health-card")

	missingResp, err := ts.Client().Get(ts.URL + "/fhir/r4/demo/Patient/does-not-exist/$health-cards.smart-health-card")
	require.NoError(t, err)
	defer missingResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, missingResp.StatusCode)

	// 5. SMART Scheduling Links $bulk-publish manifest & NDJSON streams (unauthenticated)
	engine := validation.NewEngine(validation.DefaultOptions())
	pubResp, err := ts.Client().Get(ts.URL + "/fhir/r4/demo/$bulk-publish")
	require.NoError(t, err)
	defer pubResp.Body.Close()
	require.Equal(t, http.StatusOK, pubResp.StatusCode)

	var pubManifest struct {
		Output []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"output"`
	}
	require.NoError(t, json.NewDecoder(pubResp.Body).Decode(&pubManifest))
	require.Len(t, pubManifest.Output, 3)

	for _, outItem := range pubManifest.Output {
		fResp, fErr := ts.Client().Get(outItem.URL)
		require.NoError(t, fErr)
		rawNDJSON, rErr := io.ReadAll(fResp.Body)
		_ = fResp.Body.Close()
		require.NoError(t, rErr)
		require.Equal(t, http.StatusOK, fResp.StatusCode)
		lines := strings.Split(strings.TrimSpace(string(rawNDJSON)), "\n")
		assert.GreaterOrEqual(t, len(lines), 2, "expected >= 2 NDJSON lines for %s", outItem.Type)
		for _, line := range lines {
			vo := engine.ValidateJSON(outItem.Type, []byte(line))
			assert.True(t, vo.IsValid(), "SMART Scheduling Links %s validation failed: %+v", outItem.Type, vo.Issues)
		}
	}

	nfResp, err := ts.Client().Get(ts.URL + "/fhir/r4/demo/$bulk-publish-file/Unknown.ndjson")
	require.NoError(t, err)
	_ = nfResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, nfResp.StatusCode)
}

func TestInfernoV070_UDSPlusCPDSHomeLabAndCCRRBundles(t *testing.T) {
	_, ts := newInfernoV070TestServer(t)
	tok := mintDevBearerToken(t, ts)
	engine := validation.NewEngine(validation.DefaultOptions())

	// 1. HRSA UDS+ Manifest, all 16 NDJSON streams, and all 20 individual resource slugs (unauthenticated)
	udsResp, err := ts.Client().Get(ts.URL + "/fhir/r4/demo/$uds-plus-manifest")
	require.NoError(t, err)
	rawManifest, err := io.ReadAll(udsResp.Body)
	_ = udsResp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, udsResp.StatusCode)
	assert.True(t, engine.ValidateJSON("Parameters", rawManifest).IsValid())

	var manifestParams struct {
		Parameter []struct {
			Name string `json:"name"`
			Part []struct {
				Name     string `json:"name"`
				ValueURL string `json:"valueUrl"`
			} `json:"part"`
		} `json:"parameter"`
	}
	require.NoError(t, json.Unmarshal(rawManifest, &manifestParams))
	udsStreamCount := 0
	for _, p := range manifestParams.Parameter {
		if p.Name != "udsData" {
			continue
		}
		udsStreamCount++
		for _, part := range p.Part {
			if part.Name == "url" {
				sResp, sErr := ts.Client().Get(part.ValueURL)
				require.NoError(t, sErr)
				sBody, _ := io.ReadAll(sResp.Body)
				_ = sResp.Body.Close()
				require.Equal(t, http.StatusOK, sResp.StatusCode)
				for _, line := range strings.Split(strings.TrimSpace(string(sBody)), "\n") {
					vo := engine.ValidateJSON("", []byte(line))
					assert.True(t, vo.IsValid(), "UDS+ NDJSON stream %s failed validation: %+v", part.ValueURL, vo.Issues)
				}
			}
		}
	}
	assert.Equal(t, 16, udsStreamCount)

	for _, slug := range []string{
		"manifest", "patient", "diagnosis", "encounter", "adverse_event",
		"allergy_intolerance", "coverage", "immunization", "medication_request",
		"medication_statement", "clinical_result", "observation", "income", "lab",
		"procedure", "related_person", "reporting_parameters",
		"service_request", "data_urls", "location",
	} {
		rResp, rErr := ts.Client().Get(ts.URL + "/fhir/r4/demo/$uds-plus-resource/" + slug)
		require.NoError(t, rErr)
		rBody, _ := io.ReadAll(rResp.Body)
		_ = rResp.Body.Close()
		require.Equal(t, http.StatusOK, rResp.StatusCode, "slug %s", slug)
		vo := engine.ValidateJSON("", rBody)
		assert.True(t, vo.IsValid(), "UDS+ slug %s failed validation: %+v", slug, vo.Issues)
	}

	// 404 on unknown UDS+ stream and slug
	badStream, err := ts.Client().Get(ts.URL + "/fhir/r4/demo/$uds-plus-ndjson/unknown")
	require.NoError(t, err)
	_ = badStream.Body.Close()
	assert.Equal(t, http.StatusNotFound, badStream.StatusCode)

	badSlug, err := ts.Client().Get(ts.URL + "/fhir/r4/demo/$uds-plus-resource/unknown")
	require.NoError(t, err)
	_ = badSlug.Body.Close()
	assert.Equal(t, http.StatusNotFound, badSlug.StatusCode)

	getAuthBundle := func(path string) []byte {
		t.Helper()
		req, rErr := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+path, nil)
		require.NoError(t, rErr)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, dErr := ts.Client().Do(req)
		require.NoError(t, dErr)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", path)
		b, iErr := io.ReadAll(resp.Body)
		require.NoError(t, iErr)
		return b
	}

	validateBundleAndEntries := func(label string, raw []byte) {
		t.Helper()
		bo := engine.ValidateJSON("Bundle", raw)
		assert.True(t, bo.IsValid(), "%s Bundle validation failed: %+v", label, bo.Issues)
		var parsed struct {
			Entry []struct {
				FullURL  string          `json:"fullUrl"`
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		}
		require.NoError(t, json.Unmarshal(raw, &parsed))
		require.NotEmpty(t, parsed.Entry)
		for _, ent := range parsed.Entry {
			eo := engine.ValidateJSON("", ent.Resource)
			assert.True(t, eo.IsValid(), "%s entry %s validation failed: %+v", label, ent.FullURL, eo.Issues)
		}
	}

	// 2. CPDS $pathology-exchange-bundle (show DAR & hide DAR)
	cpdsShow := getAuthBundle("/fhir/r4/demo/Patient/pat-alice-001/$pathology-exchange-bundle")
	assert.Contains(t, string(cpdsShow), "http://hl7.org/fhir/StructureDefinition/data-absent-reason")
	assert.Contains(t, string(cpdsShow), "http://terminology.hl7.org/CodeSystem/data-absent-reason")
	validateBundleAndEntries("CPDS show DAR", cpdsShow)

	cpdsHide := getAuthBundle("/fhir/r4/demo/Patient/pat-alice-001/$pathology-exchange-bundle?missingData=hide")
	assert.NotContains(t, string(cpdsHide), "http://hl7.org/fhir/StructureDefinition/data-absent-reason")
	validateBundleAndEntries("CPDS hide DAR", cpdsHide)

	// 3. At-Home In-Vitro $home-lab-report
	homeLab := getAuthBundle("/fhir/r4/demo/Patient/pat-alice-001/$home-lab-report")
	validateBundleAndEntries("At-Home In-Vitro", homeLab)

	// 4. Central Cancer Registry Reporting $cancer-registry-report (document & message bundles)
	ccrrDoc := getAuthBundle("/fhir/r4/demo/Patient/pat-ccrr-001/$cancer-registry-report")
	validateBundleAndEntries("CCRR Content Document", ccrrDoc)

	ccrrMsg := getAuthBundle("/fhir/r4/demo/Patient/pat-ccrr-001/$cancer-registry-report?bundleType=message")
	validateBundleAndEntries("CCRR Reporting Message", ccrrMsg)
}
