package server_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"hegel.dev/go/hegel"

	oo_pb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/operation_outcome_go_proto"
	parampb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/parameters_go_proto"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/fhirxml"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/sqlite"
)

func newSQLiteTrack4Server(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, sqlite.RunMigrations(context.Background(), sqlDB, sqlite.JournalMemory))

	st := sqlite.New(sqlDB, sqlite.WithJournalMode(sqlite.JournalMemory), sqlite.WithDSN(":memory:"))
	return newHardeningServer(t, st)
}

func ecPublicKeyToJWK(t *testing.T, pub *ecdsa.PublicKey, kid, alg string) auth.JWK {
	t.Helper()
	raw, err := pub.Bytes()
	require.NoError(t, err)
	var crv string
	var coordLen int
	switch pub.Curve {
	case elliptic.P256():
		crv = "P-256"
		coordLen = 32
	case elliptic.P384():
		crv = "P-384"
		coordLen = 48
	default:
		t.Fatalf("unsupported curve")
	}
	require.Len(t, raw, 1+2*coordLen)
	return auth.JWK{
		Kty: "EC",
		Crv: crv,
		Kid: kid,
		Use: "sig",
		Alg: alg,
		X:   base64.RawURLEncoding.EncodeToString(raw[1 : 1+coordLen]),
		Y:   base64.RawURLEncoding.EncodeToString(raw[1+coordLen:]),
	}
}

func rsaPublicKeyToJWK(pub *rsa.PublicKey, kid string) auth.JWK {
	eBytes := []byte{byte(pub.E >> 16), byte(pub.E >> 8), byte(pub.E)}
	for len(eBytes) > 1 && eBytes[0] == 0 {
		eBytes = eBytes[1:]
	}
	return auth.JWK{
		Kty: "RSA",
		Kid: kid,
		Use: "sig",
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(eBytes),
	}
}

func signCDSJWT(t testing.TB, header map[string]any, claims map[string]any, priv any) string {
	t.Helper()
	hb, err := json.Marshal(header)
	require.NoError(t, err)
	pb, err := json.Marshal(claims)
	require.NoError(t, err)

	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	alg, _ := header["alg"].(string)

	var sig []byte
	switch alg {
	case "ES256":
		ecPriv := priv.(*ecdsa.PrivateKey)
		sum := sha256.Sum256([]byte(signingInput))
		rInt, sInt, err := ecdsa.Sign(rand.Reader, ecPriv, sum[:])
		require.NoError(t, err)
		sig = make([]byte, 64)
		rInt.FillBytes(sig[:32])
		sInt.FillBytes(sig[32:])
	case "ES384":
		ecPriv := priv.(*ecdsa.PrivateKey)
		sum := sha512.Sum384([]byte(signingInput))
		rInt, sInt, err := ecdsa.Sign(rand.Reader, ecPriv, sum[:])
		require.NoError(t, err)
		sig = make([]byte, 96)
		rInt.FillBytes(sig[:48])
		sInt.FillBytes(sig[48:])
	case "RS256":
		rsaPriv := priv.(*rsa.PrivateKey)
		sum := sha256.Sum256([]byte(signingInput))
		sig, err = rsa.SignPKCS1v15(rand.Reader, rsaPriv, crypto.SHA256, sum[:])
		require.NoError(t, err)
	case "HS256":
		secret := priv.([]byte)
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(signingInput))
		sig = mac.Sum(nil)
	case "none":
		sig = nil
	default:
		t.Fatalf("unsupported test alg %q", alg)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func putTestResource(t testing.TB, client *http.Client, baseURL, tenant, resType, id, bodyJSON string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, baseURL+"/fhir/r4/"+tenant+"/"+resType+"/"+id, strings.NewReader(bodyJSON))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/fhir+json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, resp.StatusCode, "PUT %s/%s failed: %s", resType, id, string(respBody))
}

func TestHRexMemberMatch_FullConformance(t *testing.T) {
	_, ts := newSQLiteTrack4Server(t)
	client := ts.Client()
	tenant := "hrex-mm"

	// 1. Verify CapabilityStatement & OpenAPI 3.1 advertise Da Vinci HRex $member-match & CDS Hooks 2.0
	respMeta, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/metadata")
	require.NoError(t, err)
	metaBytes, _ := io.ReadAll(respMeta.Body)
	respMeta.Body.Close()
	require.Equal(t, http.StatusOK, respMeta.StatusCode)
	assert.Contains(t, string(metaBytes), "http://hl7.org/fhir/us/davinci-hrex/CapabilityStatement/hrex-server")
	assert.Contains(t, string(metaBytes), "http://hl7.org/fhir/us/davinci-hrex/OperationDefinition/member-match")

	respOA, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/openapi.json")
	require.NoError(t, err)
	oaBytes, _ := io.ReadAll(respOA.Body)
	respOA.Body.Close()
	require.Equal(t, http.StatusOK, respOA.StatusCode)
	assert.Contains(t, string(oaBytes), `"/Patient/$member-match"`)
	assert.Contains(t, string(oaBytes), `"/cds-services/{serviceId}/feedback"`)

	// Seed Alice Member and prior Coverage
	putTestResource(t, client, ts.URL, tenant, "Patient", "pat-alice", `{
		"resourceType": "Patient",
		"id": "pat-alice",
		"identifier": [{"system": "http://oldplan.example.org/member", "value": "UMB-ALICE-001"}],
		"name": [{"use": "official", "family": "Rivera", "given": ["Alice", "Marie"]}],
		"gender": "female",
		"birthDate": "1984-07-19",
		"address": [{"postalCode": "02115"}]
	}`)
	putTestResource(t, client, ts.URL, tenant, "Coverage", "cov-alice-old", `{
		"resourceType": "Coverage",
		"id": "cov-alice-old",
		"status": "active",
		"subscriberId": "SUB-ALICE-77",
		"identifier": [{"system": "http://oldplan.example.org/coverage", "value": "COV-ALICE-77"}],
		"beneficiary": {"reference": "Patient/pat-alice"},
		"payor": [{"reference": "Organization/org-oldplan", "display": "Old Plan Health"}]
	}`)

	// 2. Full HRex $member-match with MemberPatient, CoverageToMatch, CoverageToLink, and active Consent -> 200 OK
	validHRexReq := `{
		"resourceType": "Parameters",
		"parameter": [
			{
				"name": "MemberPatient",
				"resource": {
					"resourceType": "Patient",
					"name": [{"family": "Rivera", "given": ["Alice"]}],
					"gender": "female",
					"birthDate": "1984-07-19"
				}
			},
			{
				"name": "CoverageToMatch",
				"resource": {
					"resourceType": "Coverage",
					"status": "active",
					"subscriberId": "SUB-ALICE-77",
					"payor": [{"display": "Old Plan Health"}]
				}
			},
			{
				"name": "CoverageToLink",
				"resource": {
					"resourceType": "Coverage",
					"id": "cov-alice-newplan",
					"status": "active",
					"subscriberId": "NEWPLAN-999",
					"payor": [{"display": "New Plan Health"}]
				}
			},
			{
				"name": "Consent",
				"resource": {
					"resourceType": "Consent",
					"id": "consent-alice-hrex",
					"status": "active",
					"provision": {
						"type": "permit",
						"period": {"start": "2020-01-01", "end": "2099-12-31"}
					}
				}
			}
		]
	}`
	respMM, err := client.Post(ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(validHRexReq))
	require.NoError(t, err)
	mmBody, _ := io.ReadAll(respMM.Body)
	respMM.Body.Close()
	require.Equal(t, http.StatusOK, respMM.StatusCode, string(mmBody))

	var mmParams map[string]any
	require.NoError(t, json.Unmarshal(mmBody, &mmParams))
	assert.Equal(t, "Parameters", mmParams["resourceType"])
	paramSlice, _ := mmParams["parameter"].([]any)
	require.Len(t, paramSlice, 4)
	assert.Contains(t, string(mmBody), `"SUB-ALICE-77"`)
	assert.Contains(t, string(mmBody), `"Patient/pat-alice"`)
	assert.Contains(t, string(mmBody), `"Coverage/cov-alice-newplan"`)
	assert.Contains(t, string(mmBody), `"Consent/consent-alice-hrex"`)
	assert.Contains(t, string(mmBody), `"Old Plan Health"`)

	// Verify CoverageToLink & Consent were persisted and linked to Patient/pat-alice
	respLinkedCov, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/Coverage/cov-alice-newplan")
	require.NoError(t, err)
	linkedCovBytes, _ := io.ReadAll(respLinkedCov.Body)
	respLinkedCov.Body.Close()
	require.Equal(t, http.StatusOK, respLinkedCov.StatusCode)
	assert.Contains(t, string(linkedCovBytes), `"Patient/pat-alice"`)

	respLinkedConsent, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/Consent/consent-alice-hrex")
	require.NoError(t, err)
	linkedConsentBytes, _ := io.ReadAll(respLinkedConsent.Body)
	respLinkedConsent.Body.Close()
	require.Equal(t, http.StatusOK, respLinkedConsent.StatusCode)
	assert.Contains(t, string(linkedConsentBytes), `"Patient/pat-alice"`)

	// Verify ASTM E2147 AuditEvent recorded for Patient/$member-match linked to pat-alice
	respAudit, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/AuditEvent?subtype=member-match")
	require.NoError(t, err)
	auditBytes, _ := io.ReadAll(respAudit.Body)
	respAudit.Body.Close()
	require.Equal(t, http.StatusOK, respAudit.StatusCode)
	assert.Contains(t, string(auditBytes), `"member-match"`)
	assert.Contains(t, string(auditBytes), `"Patient/pat-alice"`)

	// 3. Reject inactive, denied, expired, and future HRex Consent with 422 Unprocessable Entity
	consentRejectCases := []struct {
		name    string
		consent string
	}{
		{
			name:    "inactive status",
			consent: `{"resourceType":"Consent","status":"inactive"}`,
		},
		{
			name:    "deny provision",
			consent: `{"resourceType":"Consent","status":"active","provision":{"type":"deny"}}`,
		},
		{
			name:    "expired provision period",
			consent: `{"resourceType":"Consent","status":"active","provision":{"type":"permit","period":{"start":"2019-01-01","end":"2020-01-01"}}}`,
		},
		{
			name:    "future provision period",
			consent: `{"resourceType":"Consent","status":"active","provision":{"type":"permit","period":{"start":"2099-01-01","end":"2099-12-31"}}}`,
		},
	}
	for _, tc := range consentRejectCases {
		t.Run("ConsentRejected_"+tc.name, func(t *testing.T) {
			reqBody := fmt.Sprintf(`{
				"resourceType": "Parameters",
				"parameter": [
					{"name": "MemberPatient", "resource": {"resourceType": "Patient", "name": [{"family": "Rivera", "given": ["Alice"]}], "birthDate": "1984-07-19"}},
					{"name": "CoverageToMatch", "resource": {"resourceType": "Coverage", "status": "active", "subscriberId": "SUB-ALICE-77"}},
					{"name": "Consent", "resource": %s}
				]
			}`, tc.consent)
			resp, err := client.Post(ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(reqBody))
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
		})
	}

	// 4. Reject deterministic birthDate conflict or conflicting Coverage subscriberId with 422
	dobMismatchReq := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "MemberPatient", "resource": {"resourceType": "Patient", "identifier": [{"value": "UMB-ALICE-001"}], "name": [{"family": "Rivera", "given": ["Alice"]}], "birthDate": "1990-01-01"}},
			{"name": "CoverageToMatch", "resource": {"resourceType": "Coverage", "status": "active", "subscriberId": "SUB-ALICE-77"}}
		]
	}`
	respDOB, err := client.Post(ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(dobMismatchReq))
	require.NoError(t, err)
	respDOB.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, respDOB.StatusCode)

	wrongSubReq := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "MemberPatient", "resource": {"resourceType": "Patient", "name": [{"family": "Rivera", "given": ["Alice"]}], "birthDate": "1984-07-19"}},
			{"name": "CoverageToMatch", "resource": {"resourceType": "Coverage", "status": "active", "subscriberId": "WRONG-SUB-000"}}
		]
	}`
	respWrongSub, err := client.Post(ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(wrongSubReq))
	require.NoError(t, err)
	respWrongSub.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, respWrongSub.StatusCode)

	// 5. Ambiguous multi-match -> 422 multiple-matches, then disambiguate via Coverage subscriberId
	putTestResource(t, client, ts.URL, tenant, "Patient", "pat-twin-1", `{
		"resourceType": "Patient",
		"id": "pat-twin-1",
		"name": [{"family": "Dupont", "given": ["Jean"]}],
		"gender": "male",
		"birthDate": "1991-03-11"
	}`)
	putTestResource(t, client, ts.URL, tenant, "Patient", "pat-twin-2", `{
		"resourceType": "Patient",
		"id": "pat-twin-2",
		"name": [{"family": "Dupont", "given": ["Jean"]}],
		"gender": "male",
		"birthDate": "1991-03-11"
	}`)
	ambigReq := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "MemberPatient", "resource": {"resourceType": "Patient", "name": [{"family": "Dupont", "given": ["Jean"]}], "birthDate": "1991-03-11"}},
			{"name": "CoverageToMatch", "resource": {"resourceType": "Coverage", "status": "active"}}
		]
	}`
	respAmbig, err := client.Post(ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(ambigReq))
	require.NoError(t, err)
	ambigBytes, _ := io.ReadAll(respAmbig.Body)
	respAmbig.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, respAmbig.StatusCode)
	assert.Contains(t, string(ambigBytes), `"multiple-matches"`)

	// Add distinct Coverage to both twins -> exact subscriberId disambiguates to pat-twin-2!
	putTestResource(t, client, ts.URL, tenant, "Coverage", "cov-twin-1", `{
		"resourceType": "Coverage",
		"id": "cov-twin-1",
		"status": "active",
		"subscriberId": "SUB-TWIN-1",
		"beneficiary": {"reference": "Patient/pat-twin-1"},
		"payor": [{"display": "Twin Plan"}]
	}`)
	putTestResource(t, client, ts.URL, tenant, "Coverage", "cov-twin-2", `{
		"resourceType": "Coverage",
		"id": "cov-twin-2",
		"status": "active",
		"subscriberId": "SUB-TWIN-2",
		"beneficiary": {"reference": "Patient/pat-twin-2"},
		"payor": [{"display": "Twin Plan"}]
	}`)
	disambigReq := `{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "MemberPatient", "resource": {"resourceType": "Patient", "name": [{"family": "Dupont", "given": ["Jean"]}], "birthDate": "1991-03-11"}},
			{"name": "CoverageToMatch", "resource": {"resourceType": "Coverage", "status": "active", "subscriberId": "SUB-TWIN-2"}}
		]
	}`
	respDisambig, err := client.Post(ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(disambigReq))
	require.NoError(t, err)
	disambigBytes, _ := io.ReadAll(respDisambig.Body)
	respDisambig.Body.Close()
	require.Equal(t, http.StatusOK, respDisambig.StatusCode)
	assert.Contains(t, string(disambigBytes), `"Patient/pat-twin-2"`)

	// 6. XML (application/fhir+xml) & Proto3 (application/x-protobuf) wire negotiation for 200 OK & 422 OperationOutcome
	xmlBytes, err := fhirxml.MarshalFHIRJSON([]byte(disambigReq))
	require.NoError(t, err)
	reqXML, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", bytes.NewReader(xmlBytes))
	require.NoError(t, err)
	reqXML.Header.Set("Content-Type", "application/fhir+xml")
	reqXML.Header.Set("Accept", "application/fhir+xml")
	respXML, err := client.Do(reqXML)
	require.NoError(t, err)
	outXML, _ := io.ReadAll(respXML.Body)
	respXML.Body.Close()
	require.Equal(t, http.StatusOK, respXML.StatusCode)
	assert.Contains(t, respXML.Header.Get("Content-Type"), "application/fhir+xml")
	assert.Contains(t, string(outXML), "<Parameters")
	assert.Contains(t, string(outXML), "Patient/pat-twin-2")

	reqProto422, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", strings.NewReader(wrongSubReq))
	require.NoError(t, err)
	reqProto422.Header.Set("Content-Type", "application/fhir+json")
	reqProto422.Header.Set("Accept", "application/x-protobuf")
	respProto422, err := client.Do(reqProto422)
	require.NoError(t, err)
	proto422Bytes, _ := io.ReadAll(respProto422.Body)
	respProto422.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, respProto422.StatusCode)
	var ooProto oo_pb.OperationOutcome
	require.NoError(t, proto.Unmarshal(proto422Bytes, &ooProto))
	require.NotEmpty(t, ooProto.GetIssue())

	// Also verify Proto3 200 OK Parameters unmarshals cleanly
	reqProto200, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", strings.NewReader(disambigReq))
	require.NoError(t, err)
	reqProto200.Header.Set("Content-Type", "application/fhir+json")
	reqProto200.Header.Set("Accept", "application/x-protobuf")
	respProto200, err := client.Do(reqProto200)
	require.NoError(t, err)
	proto200Bytes, _ := io.ReadAll(respProto200.Body)
	respProto200.Body.Close()
	require.Equal(t, http.StatusOK, respProto200.StatusCode)
	var outParamProto parampb.Parameters
	require.NoError(t, proto.Unmarshal(proto200Bytes, &outParamProto))
	require.Len(t, outParamProto.GetParameter(), 2)
}

func TestCDSHooks20_CryptographicJWTVerification(t *testing.T) {
	srv, ts := newSQLiteTrack4Server(t)
	client := ts.Client()
	tenant := "cds-jwt-sec"

	// Seed clinical data for patient-view CDS hook
	putTestResource(t, client, ts.URL, tenant, "Patient", "pat-cds-1", `{
		"resourceType": "Patient",
		"id": "pat-cds-1",
		"name": [{"family": "HookTest", "given": ["Elena"]}],
		"gender": "female",
		"birthDate": "1972-11-04"
	}`)
	putTestResource(t, client, ts.URL, tenant, "Condition", "cond-cds-1", `{
		"resourceType": "Condition",
		"id": "cond-cds-1",
		"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
		"code": {"coding": [{"system": "http://snomed.info/sct", "code": "59621000", "display": "Essential hypertension"}]},
		"subject": {"reference": "Patient/pat-cds-1"}
	}`)

	// Generate ES256, ES384, and RS256 (2048-bit) CDS Client keypairs
	ec256Priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ec384Priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	staticIssuer := "https://ehr-static.example.org"
	srv.RegisterCDSClientJWKS(staticIssuer, auth.JWKSet{
		Keys: []auth.JWK{
			ecPublicKeyToJWK(t, &ec256Priv.PublicKey, "es256-k1", "ES256"),
			ecPublicKeyToJWK(t, &ec384Priv.PublicKey, "es384-k1", "ES384"),
			rsaPublicKeyToJWK(&rsaPriv.PublicKey, "rs256-k1"),
		},
	})

	// Spin up an allowlisted loopback JKU JWKS server to verify live JKU resolution + caching
	var jkuHits atomic.Int64
	jkuECPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	jkuJWKS := auth.JWKSet{
		Keys: []auth.JWK{ecPublicKeyToJWK(t, &jkuECPriv.PublicKey, "jku-es256-1", "ES256")},
	}
	jkuSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jkuHits.Add(1)
		w.Header().Set("Content-Type", "application/jwk-set+json")
		_ = json.NewEncoder(w).Encode(jkuJWKS)
	}))
	t.Cleanup(jkuSrv.Close)

	srv.SetCDSHooksAllowedJKUPrefixes([]string{jkuSrv.URL + "/.well-known"})
	srv.SetCDSHooksRequireJWT(true)

	invokeBody := fmt.Sprintf(`{
		"hook": "patient-view",
		"hookInstance": %q,
		"fhirServer": %q,
		"context": {"userId": "Practitioner/prac-1", "patientId": "pat-cds-1"}
	}`, uuid.NewString(), ts.URL+"/fhir/r4/"+tenant)

	feedbackBody := `{"feedback": [{"card": "card-htn-gap-pat-cds-1", "outcome": "accepted"}]}`

	makeValidToken := func(alg, kid string, priv any, iss, aud, jku string) string {
		now := time.Now().UTC()
		hdr := map[string]any{
			"alg": alg,
			"kid": kid,
			"typ": "JWT",
		}
		if jku != "" {
			hdr["jku"] = jku
		}
		claims := map[string]any{
			"iss": iss,
			"aud": aud,
			"iat": now.Unix(),
			"exp": now.Add(5 * time.Minute).Unix(),
			"jti": uuid.NewString(),
		}
		return signCDSJWT(t, hdr, claims, priv)
	}

	// 1. Verify ES256, ES384, and RS256 across all CDS Hooks discovery, invoke, $dsi-attributes, and feedback routes
	endpoints := []struct {
		method string
		path   string
		body   string
		alg    string
		kid    string
		priv   any
	}{
		{http.MethodGet, "/cds-services", "", "ES256", "es256-k1", ec256Priv},
		{http.MethodGet, "/fhir/r4/cds-services", "", "ES384", "es384-k1", ec384Priv},
		{http.MethodGet, "/fhir/r4/" + tenant + "/cds-services", "", "RS256", "rs256-k1", rsaPriv},
		{http.MethodPost, "/cds-services/flint-hypertension-care-gap", invokeBody, "ES256", "es256-k1", ec256Priv},
		{http.MethodPost, "/fhir/r4/cds-services/flint-hypertension-care-gap", invokeBody, "ES384", "es384-k1", ec384Priv},
		{http.MethodPost, "/fhir/r4/" + tenant + "/cds-services/flint-hypertension-care-gap", invokeBody, "RS256", "rs256-k1", rsaPriv},
		{http.MethodGet, "/cds-services/flint-hypertension-care-gap/$dsi-attributes", "", "ES256", "es256-k1", ec256Priv},
		{http.MethodGet, "/fhir/r4/cds-services/flint-hypertension-care-gap/$dsi-attributes", "", "ES384", "es384-k1", ec384Priv},
		{http.MethodGet, "/fhir/r4/" + tenant + "/cds-services/flint-hypertension-care-gap/$dsi-attributes", "", "RS256", "rs256-k1", rsaPriv},
		{http.MethodPost, "/cds-services/flint-hypertension-care-gap/feedback", feedbackBody, "ES256", "es256-k1", ec256Priv},
		{http.MethodPost, "/fhir/r4/cds-services/flint-hypertension-care-gap/feedback", feedbackBody, "ES384", "es384-k1", ec384Priv},
		{http.MethodPost, "/fhir/r4/" + tenant + "/cds-services/flint-hypertension-care-gap/feedback", feedbackBody, "RS256", "rs256-k1", rsaPriv},
	}

	for _, ep := range endpoints {
		t.Run(ep.alg+"_"+ep.method+"_"+ep.path, func(t *testing.T) {
			fullURL := ts.URL + ep.path
			tok := makeValidToken(ep.alg, ep.kid, ep.priv, staticIssuer, fullURL, "")
			var bodyReader io.Reader
			if ep.body != "" {
				bodyReader = strings.NewReader(ep.body)
			}
			req, err := http.NewRequest(ep.method, fullURL, bodyReader)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+tok)
			if ep.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := client.Do(req)
			require.NoError(t, err)
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
		})
	}

	// 2. Verify live JKU resolution + in-memory caching (2 requests with distinct jti -> only 1 HTTP fetch!)
	jkuURL := jkuSrv.URL + "/.well-known/jwks.json"
	for i := 0; i < 2; i++ {
		tok := makeValidToken("ES256", "jku-es256-1", jkuECPriv, "https://ehr-dynamic.example.org", ts.URL+"/cds-services", jkuURL)
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/cds-services", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	assert.Equal(t, int64(1), jkuHits.Load(), "expected JKU JWKS to be cached after first fetch")

	// 3. Adversarial security attacks -> all MUST fail with 401 Unauthorized + ASTM E2147 AuditEvent
	targetURL := ts.URL + "/fhir/r4/" + tenant + "/cds-services/flint-hypertension-care-gap"
	now := time.Now().UTC()
	rogueEC, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	adversarialCases := []struct {
		name  string
		token string
	}{
		{
			name:  "missing_authorization_header",
			token: "",
		},
		{
			name: "alg_none_attack",
			token: signCDSJWT(t,
				map[string]any{"alg": "none", "typ": "JWT"},
				map[string]any{"iss": staticIssuer, "aud": targetURL, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "jti": uuid.NewString()},
				nil,
			),
		},
		{
			name: "alg_hs256_symmetric_confusion",
			token: signCDSJWT(t,
				map[string]any{"alg": "HS256", "kid": "es256-k1", "typ": "JWT"},
				map[string]any{"iss": staticIssuer, "aud": targetURL, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "jti": uuid.NewString()},
				[]byte("shared-secret"),
			),
		},
		{
			name: "wrong_signing_key",
			token: signCDSJWT(t,
				map[string]any{"alg": "ES256", "kid": "es256-k1", "typ": "JWT"},
				map[string]any{"iss": staticIssuer, "aud": targetURL, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "jti": uuid.NewString()},
				rogueEC,
			),
		},
		{
			name: "inline_jwk_injection",
			token: signCDSJWT(t,
				map[string]any{"alg": "ES256", "typ": "JWT", "jwk": ecPublicKeyToJWK(t, &rogueEC.PublicKey, "evil", "ES256")},
				map[string]any{"iss": staticIssuer, "aud": targetURL, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "jti": uuid.NewString()},
				rogueEC,
			),
		},
		{
			name:  "untrusted_issuer",
			token: makeValidToken("ES256", "es256-k1", ec256Priv, "https://untrusted-ehr.example.org", targetURL, ""),
		},
		{
			name:  "unlisted_jku_ssrf_block",
			token: makeValidToken("ES256", "jku-es256-1", jkuECPriv, "https://ehr-dynamic.example.org", targetURL, jkuSrv.URL+"/unapproved/jwks.json"),
		},
		{
			name:  "host_suffix_jku_bypass_block",
			token: makeValidToken("ES256", "jku-es256-1", jkuECPriv, "https://ehr-dynamic.example.org", targetURL, "https://127.0.0.1.evil.example/.well-known/jwks.json"),
		},
		{
			name:  "mismatched_audience",
			token: makeValidToken("ES256", "es256-k1", ec256Priv, staticIssuer, ts.URL+"/cds-services/flint-diabetes-a1c-care-gap", ""),
		},
		{
			name: "expired_exp",
			token: signCDSJWT(t,
				map[string]any{"alg": "ES256", "kid": "es256-k1", "typ": "JWT"},
				map[string]any{"iss": staticIssuer, "aud": targetURL, "iat": now.Add(-10 * time.Minute).Unix(), "exp": now.Add(-1 * time.Minute).Unix(), "jti": uuid.NewString()},
				ec256Priv,
			),
		},
		{
			name: "future_iat",
			token: signCDSJWT(t,
				map[string]any{"alg": "ES256", "kid": "es256-k1", "typ": "JWT"},
				map[string]any{"iss": staticIssuer, "aud": targetURL, "iat": now.Add(10 * time.Minute).Unix(), "exp": now.Add(15 * time.Minute).Unix(), "jti": uuid.NewString()},
				ec256Priv,
			),
		},
		{
			name: "missing_jti",
			token: signCDSJWT(t,
				map[string]any{"alg": "ES256", "kid": "es256-k1", "typ": "JWT"},
				map[string]any{"iss": staticIssuer, "aud": targetURL, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()},
				ec256Priv,
			),
		},
	}

	for _, tc := range adversarialCases {
		t.Run("Reject_"+tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, targetURL, strings.NewReader(invokeBody))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			resp, err := client.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assert.NotEmpty(t, resp.Header.Get("WWW-Authenticate"))
		})
	}

	// Verify ASTM E2147 AuditEvents were persisted for the failed authentication attempts!
	respAuditFail, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/AuditEvent?subtype=cds-hooks-auth&outcome=4")
	require.NoError(t, err)
	auditFailBytes, _ := io.ReadAll(respAuditFail.Body)
	respAuditFail.Body.Close()
	require.Equal(t, http.StatusOK, respAuditFail.StatusCode)
	var auditBundle map[string]any
	require.NoError(t, json.Unmarshal(auditFailBytes, &auditBundle))
	totalAudits, _ := auditBundle["total"].(float64)
	assert.GreaterOrEqual(t, int(totalAudits), len(adversarialCases))

	// Also verify root /cds-services auth failure records an AuditEvent in the default tenant
	reqRootFail, err := http.NewRequest(http.MethodGet, ts.URL+"/cds-services", nil)
	require.NoError(t, err)
	respRootFail, err := client.Do(reqRootFail)
	require.NoError(t, err)
	respRootFail.Body.Close()
	require.Equal(t, http.StatusUnauthorized, respRootFail.StatusCode)

	respDefaultAudit, err := client.Get(ts.URL + "/fhir/r4/default/AuditEvent?subtype=cds-hooks-auth&outcome=4")
	require.NoError(t, err)
	defAuditBytes, _ := io.ReadAll(respDefaultAudit.Body)
	respDefaultAudit.Body.Close()
	require.Equal(t, http.StatusOK, respDefaultAudit.StatusCode)
	assert.Contains(t, string(defAuditBytes), `"cds-hooks-auth"`)

	// 4. Concurrent -race JTI Replay Attack: 24 goroutines submit the SAME valid JWT -> exactly 1 succeeds (200) and 23 get 401!
	replayToken := makeValidToken("ES256", "es256-k1", ec256Priv, staticIssuer, targetURL, "")
	const workers = 24
	var wg sync.WaitGroup
	startGate := make(chan struct{})
	var okCount, unauthCount atomic.Int32
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startGate
			req, reqErr := http.NewRequest(http.MethodPost, targetURL, strings.NewReader(invokeBody))
			if reqErr != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+replayToken)
			resp, doErr := client.Do(req)
			if doErr != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				okCount.Add(1)
			case http.StatusUnauthorized:
				unauthCount.Add(1)
			}
		}()
	}
	close(startGate)
	wg.Wait()
	assert.Equal(t, int32(1), okCount.Load(), "expected exactly 1 request with shared jti to succeed")
	assert.Equal(t, int32(workers-1), unauthCount.Load(), "expected all concurrent replays of jti to be rejected with 401")

	// 5. Optional mode (SetCDSHooksRequireJWT(false)): unauthenticated works AND patient-scoped SMART tokens coexist cleanly
	srv.SetCDSHooksRequireJWT(false)
	respUnauthDisc, err := client.Get(ts.URL + "/cds-services")
	require.NoError(t, err)
	respUnauthDisc.Body.Close()
	assert.Equal(t, http.StatusOK, respUnauthDisc.StatusCode)

	mockSMART, err := auth.NewMockTokenValidator("", "")
	require.NoError(t, err)
	srv.SetTokenValidator(mockSMART)
	t.Cleanup(func() { srv.SetTokenValidator(nil) })
	smartTokOwn, err := mockSMART.IssueToken("pat-user-1", "patient/Patient.rs", "pat-cds-1", 10*time.Minute, nil)
	require.NoError(t, err)
	smartTokOther, err := mockSMART.IssueToken("pat-user-2", "patient/Patient.rs", "other-pat-999", 10*time.Minute, nil)
	require.NoError(t, err)

	reqSMARTOwn, _ := http.NewRequest(http.MethodPost, targetURL, strings.NewReader(invokeBody))
	reqSMARTOwn.Header.Set("Content-Type", "application/json")
	reqSMARTOwn.Header.Set("Authorization", "Bearer "+smartTokOwn)
	respSMARTOwn, err := client.Do(reqSMARTOwn)
	require.NoError(t, err)
	respSMARTOwn.Body.Close()
	assert.Equal(t, http.StatusOK, respSMARTOwn.StatusCode)

	reqSMARTOther, _ := http.NewRequest(http.MethodPost, targetURL, strings.NewReader(invokeBody))
	reqSMARTOther.Header.Set("Content-Type", "application/json")
	reqSMARTOther.Header.Set("Authorization", "Bearer "+smartTokOther)
	respSMARTOther, err := client.Do(reqSMARTOther)
	require.NoError(t, err)
	respSMARTOther.Body.Close()
	assert.Equal(t, http.StatusForbidden, respSMARTOther.StatusCode)
}

func TestProperty_HRexAndCDSHooksInvariants(t *testing.T) {
	srv, ts := newSQLiteTrack4Server(t)
	client := ts.Client()

	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	wrongPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	issuer := "https://pbt-ehr.example.org"
	srv.RegisterCDSClientJWKS(issuer, auth.JWKSet{
		Keys: []auth.JWK{ecPublicKeyToJWK(t, &ecPriv.PublicKey, "pbt-k1", "ES256")},
	})
	srv.SetCDSHooksRequireJWT(true)

	var seq atomic.Int64

	t.Run("CDSHooksJWT_TamperAndReplayInvariants", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			n := seq.Add(1)
			endpoint := hegel.Draw(ht, hegel.SampledFrom([]string{
				ts.URL + "/cds-services",
				ts.URL + "/cds-services/flint-hypertension-care-gap/$dsi-attributes",
			}))
			now := time.Now().UTC()
			jti := fmt.Sprintf("pbt-jti-%d-%s", n, hegel.Draw(ht, hegel.SampledFrom([]string{"a", "b", "c", "d"})))

			validTok := signCDSJWT(ht,
				map[string]any{"alg": "ES256", "kid": "pbt-k1", "typ": "JWT"},
				map[string]any{
					"iss": issuer,
					"aud": endpoint,
					"iat": now.Unix(),
					"exp": now.Add(3 * time.Minute).Unix(),
					"jti": jti,
				},
				ecPriv,
			)

			// First presentation of valid JWT MUST succeed (200 OK)
			req1, _ := http.NewRequest(http.MethodGet, endpoint, nil)
			req1.Header.Set("Authorization", "Bearer "+validTok)
			resp1, err := client.Do(req1)
			if err != nil {
				ht.Fatalf("GET %s: %v", endpoint, err)
			}
			_, _ = io.Copy(io.Discard, resp1.Body)
			resp1.Body.Close()
			if resp1.StatusCode != http.StatusOK {
				ht.Fatalf("expected 200 OK on fresh valid CDS JWT, got %d", resp1.StatusCode)
			}

			// Replay of identical JWT MUST fail (401 Unauthorized)
			reqReplay, _ := http.NewRequest(http.MethodGet, endpoint, nil)
			reqReplay.Header.Set("Authorization", "Bearer "+validTok)
			respReplay, err := client.Do(reqReplay)
			if err != nil {
				ht.Fatalf("replay GET %s: %v", endpoint, err)
			}
			_, _ = io.Copy(io.Discard, respReplay.Body)
			respReplay.Body.Close()
			if respReplay.StatusCode != http.StatusUnauthorized {
				ht.Fatalf("expected 401 Unauthorized on replayed jti, got %d", respReplay.StatusCode)
			}

			// Random mutation / attack vector MUST fail (401 Unauthorized)
			mutation := hegel.Draw(ht, hegel.SampledFrom([]string{
				"flip_sig_byte",
				"wrong_key",
				"wrong_aud",
				"expired",
				"empty_jti",
				"alg_none",
			}))
			var badTok string
			freshJTI := fmt.Sprintf("pbt-mut-%d", seq.Add(1))
			switch mutation {
			case "flip_sig_byte":
				fresh := signCDSJWT(ht,
					map[string]any{"alg": "ES256", "kid": "pbt-k1", "typ": "JWT"},
					map[string]any{"iss": issuer, "aud": endpoint, "iat": now.Unix(), "exp": now.Add(3 * time.Minute).Unix(), "jti": freshJTI},
					ecPriv,
				)
				parts := strings.Split(fresh, ".")
				sigBytes, _ := base64.RawURLEncoding.DecodeString(parts[2])
				idx := hegel.Draw(ht, hegel.Integers(0, len(sigBytes)-1))
				sigBytes[idx] ^= 0xFF
				badTok = parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(sigBytes)
			case "wrong_key":
				badTok = signCDSJWT(ht,
					map[string]any{"alg": "ES256", "kid": "pbt-k1", "typ": "JWT"},
					map[string]any{"iss": issuer, "aud": endpoint, "iat": now.Unix(), "exp": now.Add(3 * time.Minute).Unix(), "jti": freshJTI},
					wrongPriv,
				)
			case "wrong_aud":
				badTok = signCDSJWT(ht,
					map[string]any{"alg": "ES256", "kid": "pbt-k1", "typ": "JWT"},
					map[string]any{"iss": issuer, "aud": ts.URL + "/wrong-endpoint", "iat": now.Unix(), "exp": now.Add(3 * time.Minute).Unix(), "jti": freshJTI},
					ecPriv,
				)
			case "expired":
				badTok = signCDSJWT(ht,
					map[string]any{"alg": "ES256", "kid": "pbt-k1", "typ": "JWT"},
					map[string]any{"iss": issuer, "aud": endpoint, "iat": now.Add(-5 * time.Minute).Unix(), "exp": now.Add(-1 * time.Second).Unix(), "jti": freshJTI},
					ecPriv,
				)
			case "empty_jti":
				badTok = signCDSJWT(ht,
					map[string]any{"alg": "ES256", "kid": "pbt-k1", "typ": "JWT"},
					map[string]any{"iss": issuer, "aud": endpoint, "iat": now.Unix(), "exp": now.Add(3 * time.Minute).Unix()},
					ecPriv,
				)
			case "alg_none":
				badTok = signCDSJWT(ht,
					map[string]any{"alg": "none", "typ": "JWT"},
					map[string]any{"iss": issuer, "aud": endpoint, "iat": now.Unix(), "exp": now.Add(3 * time.Minute).Unix(), "jti": freshJTI},
					nil,
				)
			}

			reqBad, _ := http.NewRequest(http.MethodGet, endpoint, nil)
			reqBad.Header.Set("Authorization", "Bearer "+badTok)
			respBad, err := client.Do(reqBad)
			if err != nil {
				ht.Fatalf("mutated GET %s: %v", endpoint, err)
			}
			_, _ = io.Copy(io.Discard, respBad.Body)
			respBad.Body.Close()
			if respBad.StatusCode != http.StatusUnauthorized {
				ht.Fatalf("mutation %s unexpectedly returned %d instead of 401", mutation, respBad.StatusCode)
			}
		})
	})

	t.Run("HRexMemberMatch_DeterministicUniquenessOracle", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			tenant := fmt.Sprintf("hrex-pbt-%d", seq.Add(1))
			numIdentical := hegel.Draw(ht, hegel.Integers(0, 3))
			family := hegel.Draw(ht, hegel.SampledFrom([]string{"Vega", "Kowalski", "Okafor", "Sato"}))
			given := hegel.Draw(ht, hegel.SampledFrom([]string{"Maya", "Liam", "Nia", "Kenji"}))
			dob := hegel.Draw(ht, hegel.SampledFrom([]string{"1978-02-14", "1989-09-23", "2001-12-05"}))
			subID := fmt.Sprintf("SUB-%d", nOrOne(numIdentical))

			for i := 0; i < numIdentical; i++ {
				pid := fmt.Sprintf("m-%d", i+1)
				putTestResource(ht, client, ts.URL, tenant, "Patient", pid, fmt.Sprintf(`{
					"resourceType": "Patient",
					"id": %q,
					"name": [{"family": %q, "given": [%q]}],
					"birthDate": %q,
					"gender": "female"
				}`, pid, family, given, dob))
				putTestResource(ht, client, ts.URL, tenant, "Coverage", "cov-"+pid, fmt.Sprintf(`{
					"resourceType": "Coverage",
					"id": %q,
					"status": "active",
					"subscriberId": %q,
					"beneficiary": {"reference": "Patient/%s"},
					"payor": [{"display": "PBT Plan"}]
				}`, "cov-"+pid, subID, pid))
			}

			reqBody := fmt.Sprintf(`{
				"resourceType": "Parameters",
				"parameter": [
					{"name": "MemberPatient", "resource": {"resourceType": "Patient", "name": [{"family": %q, "given": [%q]}], "birthDate": %q}},
					{"name": "CoverageToMatch", "resource": {"resourceType": "Coverage", "status": "active", "subscriberId": %q}}
				]
			}`, family, given, dob, subID)

			resp, err := client.Post(ts.URL+"/fhir/r4/"+tenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(reqBody))
			if err != nil {
				ht.Fatalf("POST $member-match: %v", err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if numIdentical == 1 {
				if resp.StatusCode != http.StatusOK {
					ht.Fatalf("expected 200 OK for unique member match, got %d: %s", resp.StatusCode, string(b))
				}
				if !strings.Contains(string(b), `"Patient/m-1"`) {
					ht.Fatalf("expected Patient/m-1 in unique match output, got %s", string(b))
				}
			} else {
				if resp.StatusCode != http.StatusUnprocessableEntity {
					ht.Fatalf("expected 422 Unprocessable Entity for %d matches, got %d: %s", numIdentical, resp.StatusCode, string(b))
				}
			}
		})
	})
}

func nOrOne(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}
