package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/validation"
)

func TestTrack7_C4BBPDexAndPASConformance(t *testing.T) {
	_, ts := newSQLiteTrack4Server(t)
	client := ts.Client()
	valEngine := validation.NewEngine(validation.DefaultOptions())

	// 1. CapabilityStatement & SMART .well-known/smart-configuration checks
	respMeta, err := client.Get(ts.URL + "/fhir/r4/demo/metadata")
	require.NoError(t, err)
	metaBytes, _ := io.ReadAll(respMeta.Body)
	respMeta.Body.Close()
	require.Equal(t, http.StatusOK, respMeta.StatusCode)
	metaStr := string(metaBytes)
	for _, want := range []string{
		"http://hl7.org/fhir/us/carin-bb/CapabilityStatement/c4bb",
		"http://hl7.org/fhir/us/davinci-pdex/CapabilityStatement/pdex-server",
		"http://hl7.org/fhir/us/davinci-pas/CapabilityStatement/pas-server",
		"http://hl7.org/fhir/us/core/StructureDefinition/us-core-condition",
		"http://hl7.org/fhir/StructureDefinition/bp",
		"http://hl7.org/fhir/StructureDefinition/bodyweight",
		"http://hl7.org/fhir/StructureDefinition/heartrate",
		"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-ExplanationOfBenefit-Inpatient-Institutional",
		"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-ExplanationOfBenefit-Pharmacy",
		"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-priorauthorization",
		"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-medicationdispense",
		"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle",
		"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse",
		"/oauth2/register",
		"/oauth2/revoke",
	} {
		assert.Contains(t, metaStr, want)
	}

	respSmart, err := client.Get(ts.URL + "/fhir/r4/demo/.well-known/smart-configuration")
	require.NoError(t, err)
	smartBytes, _ := io.ReadAll(respSmart.Body)
	respSmart.Body.Close()
	require.Equal(t, http.StatusOK, respSmart.StatusCode)
	smartStr := string(smartBytes)
	for _, wantScope := range []string{
		"patient/ExplanationOfBenefit.read",
		"patient/Coverage.read",
		"patient/RelatedPerson.read",
		"user/ExplanationOfBenefit.read",
		"user/Coverage.read",
		"user/RelatedPerson.read",
	} {
		assert.Contains(t, smartStr, wantScope)
	}

	// 2. CARIN IG for Blue Button v2.0.0 search & _include verification
	c4bbTenant := "c4bb-track7"
	putTestResource(t, client, ts.URL, c4bbTenant, "Patient", "pat-alice-001", `{"resourceType":"Patient","id":"pat-alice-001","identifier":[{"system":"http://hospital.example.org/mrn","value":"MRN-1001"}],"name":[{"family":"Wonderland","given":["Alice"]}],"gender":"female","birthDate":"1985-04-12"}`)
	putTestResource(t, client, ts.URL, c4bbTenant, "Organization", "org-payer-1", `{"resourceType":"Organization","id":"org-payer-1","active":true,"name":"C4BB Health Plan"}`)
	putTestResource(t, client, ts.URL, c4bbTenant, "Organization", "org-prov-1", `{"resourceType":"Organization","id":"org-prov-1","active":true,"name":"C4BB General Hospital"}`)
	putTestResource(t, client, ts.URL, c4bbTenant, "Practitioner", "prac-1", `{"resourceType":"Practitioner","id":"prac-1","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}],"name":[{"family":"House","given":["Gregory"]}]}`)
	putTestResource(t, client, ts.URL, c4bbTenant, "Coverage", "cov-1", `{"resourceType":"Coverage","id":"cov-1","status":"active","subscriberId":"SUB-1001","beneficiary":{"reference":"Patient/pat-alice-001"},"relationship":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/subscriber-relationship","code":"self"}]},"payor":[{"reference":"Organization/org-payer-1"}]}`)
	putTestResource(t, client, ts.URL, c4bbTenant, "ExplanationOfBenefit", "eob-inp-1", `{
		"resourceType":"ExplanationOfBenefit",
		"id":"eob-inp-1",
		"identifier":[{"system":"http://c4bb.example.org/claim","value":"CLM-INP-001"}],
		"status":"active",
		"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"institutional"}]},
		"subType":{"coding":[{"system":"http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBInstitutionalClaimSubType","code":"inpatient"}]},
		"use":"claim",
		"patient":{"reference":"Patient/pat-alice-001"},
		"billablePeriod":{"start":"2026-01-10","end":"2026-01-14"},
		"created":"2026-01-15T12:00:00Z",
		"insurer":{"reference":"Organization/org-payer-1"},
		"provider":{"reference":"Organization/org-prov-1"},
		"payee":{"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/payeetype","code":"provider"}]},"party":{"reference":"Organization/org-prov-1"}},
		"outcome":"complete",
		"careTeam":[{"sequence":1,"provider":{"reference":"Practitioner/prac-1"},"role":{"coding":[{"system":"http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBClaimCareTeamRole","code":"attending"}]}}],
		"diagnosis":[{"sequence":1,"diagnosisCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/sid/icd-10-cm","code":"I10"}]}}],
		"insurance":[{"focal":true,"coverage":{"reference":"Coverage/cov-1"}}],
		"item":[{"sequence":1,"productOrService":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"99223"}]},"servicedDate":"2026-01-11"}]
	}`)

	for _, q := range []string{
		"ExplanationOfBenefit?patient=pat-alice-001",
		"ExplanationOfBenefit?patient=pat-alice-001&type=institutional",
		"ExplanationOfBenefit?patient=pat-alice-001&created=ge2026-01-01",
		"ExplanationOfBenefit?patient=pat-alice-001&service-date=ge2026-01-10",
		"ExplanationOfBenefit?patient=pat-alice-001&service-start-date=ge2026-01-10",
		"ExplanationOfBenefit?patient=pat-alice-001&billable-period-start=ge2026-01-01",
		"Coverage?beneficiary=Patient/pat-alice-001&_include=Coverage:payor",
	} {
		resp, qErr := client.Get(ts.URL + "/fhir/r4/" + c4bbTenant + "/" + q)
		require.NoError(t, qErr, q)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "query %s failed: %s", q, string(b))
		var bMap map[string]any
		require.NoError(t, json.Unmarshal(b, &bMap))
		entries, _ := bMap["entry"].([]any)
		assert.NotEmpty(t, entries, "expected matches for %s", q)
	}

	// Verify _include=ExplanationOfBenefit:payee, :care-team, and :*
	incQuery := "ExplanationOfBenefit?_id=eob-inp-1&_include=ExplanationOfBenefit:payee&_include=ExplanationOfBenefit:care-team&_include=ExplanationOfBenefit:*"
	respInc, err := client.Get(ts.URL + "/fhir/r4/" + c4bbTenant + "/" + incQuery)
	require.NoError(t, err)
	incBytes, _ := io.ReadAll(respInc.Body)
	respInc.Body.Close()
	require.Equal(t, http.StatusOK, respInc.StatusCode)
	var incBundle map[string]any
	require.NoError(t, json.Unmarshal(incBytes, &incBundle))
	incTypes := map[string]bool{}
	for _, rawE := range incBundle["entry"].([]any) {
		rm := rawE.(map[string]any)["resource"].(map[string]any)
		incTypes[rm["resourceType"].(string)+"/"+rm["id"].(string)] = true
	}
	assert.True(t, incTypes["ExplanationOfBenefit/eob-inp-1"])
	assert.True(t, incTypes["Organization/org-prov-1"], "expected payee/provider Organization in _include")
	assert.True(t, incTypes["Organization/org-payer-1"], "expected insurer Organization in _include=*")
	assert.True(t, incTypes["Practitioner/prac-1"], "expected care-team Practitioner in _include")
	assert.True(t, incTypes["Patient/pat-alice-001"], "expected patient in _include=*")

	// 3. Da Vinci PDex v2.0.0 / HRex v1.0.0 strict $member-match (1-match, 0-match, multi-match)
	pdexTenant := "pdex-track7"
	putTestResource(t, client, ts.URL, pdexTenant, "Patient", "pat-alice-001", `{"resourceType":"Patient","id":"pat-alice-001","identifier":[{"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v2-0203","code":"MB"}]},"system":"http://hospital.example.org/mrn","value":"MRN-1001"}],"name":[{"use":"official","family":"Wonderland","given":["Alice","Marie"]}],"gender":"female","birthDate":"1985-04-12"}`)
	putTestResource(t, client, ts.URL, pdexTenant, "Coverage", "cov-alice-1", `{"resourceType":"Coverage","id":"cov-alice-1","identifier":[{"system":"http://flint-health.example.org/memberid","value":"MEM-ALICE-001"}],"status":"active","subscriberId":"SUB-ALICE-001","beneficiary":{"reference":"Patient/pat-alice-001"},"payor":[{"display":"Flint Health Plan"}]}`)
	putTestResource(t, client, ts.URL, pdexTenant, "Patient", "pat-dup-a", `{"resourceType":"Patient","id":"pat-dup-a","identifier":[{"system":"http://flint.health/pdex/dup","value":"SUB-PDEX-DUP-999"}],"name":[{"family":"Dupont","given":["Jean"]}],"gender":"male","birthDate":"1991-03-11"}`)
	putTestResource(t, client, ts.URL, pdexTenant, "Patient", "pat-dup-b", `{"resourceType":"Patient","id":"pat-dup-b","identifier":[{"system":"http://flint.health/pdex/dup","value":"SUB-PDEX-DUP-999"}],"name":[{"family":"Dupont","given":["Jean"]}],"gender":"male","birthDate":"1991-03-11"}`)

	oneMatchReq := `{
		"resourceType":"Parameters",
		"meta":{"profile":["http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-parameters-member-match-in"]},
		"parameter":[
			{"name":"MemberPatient","resource":{"resourceType":"Patient","id":"1","identifier":[{"system":"http://hospital.example.org/mrn","value":"MRN-1001"}],"name":[{"family":"Wonderland","given":["Alice","Marie"]}],"gender":"female","birthDate":"1985-04-12"}},
			{"name":"CoverageToMatch","resource":{"resourceType":"Coverage","id":"cov-1","status":"draft","subscriberId":"SUB-ALICE-001","beneficiary":{"reference":"Patient/1"},"payor":[{"display":"Flint Health Plan"}]}},
			{"name":"Consent","resource":{"resourceType":"Consent","id":"con-1","status":"active","scope":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/consentscope","code":"patient-privacy"}]},"category":[{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v3-ActCode","code":"IDSCL"}]}],"patient":{"reference":"Patient/1"},"provision":{"type":"permit","period":{"start":"2020-01-01","end":"2099-12-31"}}}}
		]
	}`
	respMM, err := client.Post(ts.URL+"/fhir/r4/"+pdexTenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(oneMatchReq))
	require.NoError(t, err)
	mmBytes, _ := io.ReadAll(respMM.Body)
	respMM.Body.Close()
	require.Equal(t, http.StatusOK, respMM.StatusCode, string(mmBytes))

	mmOut := valEngine.ValidateJSONWithProfiles("Parameters", mmBytes, []string{"http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-parameters-member-match-out"}, "")
	assert.True(t, mmOut.IsValid(), "expected strict 1-parameter hrex-parameters-member-match-out, got: %+v (%s)", mmOut.Issues, string(mmBytes))

	var mmParams map[string]any
	require.NoError(t, json.Unmarshal(mmBytes, &mmParams))
	pSlice := mmParams["parameter"].([]any)
	require.Len(t, pSlice, 1)
	memIdentVal := pSlice[0].(map[string]any)["valueIdentifier"].(map[string]any)["value"].(string)
	require.NotEmpty(t, memIdentVal)

	respPatLookup, err := client.Get(ts.URL + "/fhir/r4/" + pdexTenant + "/Patient?identifier=" + memIdentVal)
	require.NoError(t, err)
	patLookupBytes, _ := io.ReadAll(respPatLookup.Body)
	respPatLookup.Body.Close()
	require.Equal(t, http.StatusOK, respPatLookup.StatusCode)
	assert.Contains(t, string(patLookupBytes), `"pat-alice-001"`)

	noMatchReq := `{"resourceType":"Parameters","meta":{"profile":["http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-parameters-member-match-in"]},"parameter":[{"name":"MemberPatient","resource":{"resourceType":"Patient","id":"1","name":[{"family":"Nobody","given":["Zero"]}],"gender":"other","birthDate":"1905-01-01"}},{"name":"CoverageToMatch","resource":{"resourceType":"Coverage","id":"c0","status":"draft","subscriberId":"NOMATCH-000","beneficiary":{"reference":"Patient/1"},"payor":[{"display":"Old Plan"}]}}]}`
	respNoMM, err := client.Post(ts.URL+"/fhir/r4/"+pdexTenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(noMatchReq))
	require.NoError(t, err)
	respNoMM.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, respNoMM.StatusCode)

	multiMatchReq := `{"resourceType":"Parameters","meta":{"profile":["http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-parameters-member-match-in"]},"parameter":[{"name":"MemberPatient","resource":{"resourceType":"Patient","id":"1","identifier":[{"system":"http://flint.health/pdex/dup","value":"SUB-PDEX-DUP-999"}],"name":[{"family":"Dupont","given":["Jean"]}],"gender":"male","birthDate":"1991-03-11"}},{"name":"CoverageToMatch","resource":{"resourceType":"Coverage","id":"cm","status":"draft","subscriberId":"SUB-PDEX-DUP-999","beneficiary":{"reference":"Patient/1"},"payor":[{"display":"Plan"}]}}]}`
	respMultiMM, err := client.Post(ts.URL+"/fhir/r4/"+pdexTenant+"/Patient/$member-match", "application/fhir+json", strings.NewReader(multiMatchReq))
	require.NoError(t, err)
	respMultiMM.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, respMultiMM.StatusCode)

	// 4. Da Vinci PAS v2.2.1 Subscription + $submit (Approval, Denial, Pended+Webhook, Claim Updates, Error) + $inquire (Parameters wrapper, spec-47, DAR, Error)
	pasTenant := "pas-track7"
	var webhookMu sync.Mutex
	var receivedWebhooks []map[string]any
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-pas-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			webhookMu.Lock()
			receivedWebhooks = append(receivedWebhooks, m)
			webhookMu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	subPayload := `{
		"resourceType":"Subscription",
		"meta":{"profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-subscription","http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription"]},
		"status":"requested",
		"reason":"PAS Pended PA Finalization",
		"criteria":"http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic",
		"_criteria":{"extension":[{"url":"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria","valueString":"org-identifier=8189991234"}]},
		"channel":{
			"type":"rest-hook",
			"endpoint":"` + webhookSrv.URL + `/notify",
			"payload":"application/fhir+json",
			"_payload":{"extension":[{"url":"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content","valueCode":"full-resource"}]},
			"header":["Authorization: Bearer test-pas-token"]
		}
	}`
	respSub, err := client.Post(ts.URL+"/fhir/r4/"+pasTenant+"/Subscription", "application/fhir+json", strings.NewReader(subPayload))
	require.NoError(t, err)
	subRespBytes, _ := io.ReadAll(respSub.Body)
	respSub.Body.Close()
	require.Equal(t, http.StatusCreated, respSub.StatusCode, string(subRespBytes))

	require.Eventually(t, func() bool {
		webhookMu.Lock()
		defer webhookMu.Unlock()
		return len(receivedWebhooks) >= 1
	}, 4*time.Second, 50*time.Millisecond, "expected PAS Subscription handshake notification")

	makePASBundle := func(traceVal string, isInquire bool) string {
		claimProf := "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim"
		bundleProf := "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"
		if isInquire {
			claimProf = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-inquiry"
			bundleProf = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-inquiry-request-bundle"
		}
		doc := map[string]any{
			"resourceType": "Bundle",
			"id":           "11111111-1111-4111-8111-000000000099",
			"meta":         map[string]any{"profile": []string{bundleProf}},
			"identifier":   map[string]any{"system": "http://flint.health/pas/bundle", "value": traceVal},
			"type":         "collection",
			"timestamp":    "2026-02-01T10:00:00Z",
			"entry": []any{
				map[string]any{
					"fullUrl": "urn:uuid:11111111-1111-4111-8111-000000000011",
					"resource": map[string]any{
						"resourceType": "Claim",
						"id":           "11111111-1111-4111-8111-000000000011",
						"meta":         map[string]any{"profile": []string{claimProf}},
						"identifier":   []any{map[string]any{"system": "http://flint.health/pas/trace", "value": traceVal}},
						"status":       "active",
						"type":         map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "professional"}}},
						"use":          "preauthorization",
						"patient":      map[string]any{"reference": "urn:uuid:11111111-1111-4111-8111-000000000003"},
						"created":      "2026-02-01T10:00:00Z",
						"insurer":      map[string]any{"reference": "urn:uuid:11111111-1111-4111-8111-000000000001"},
						"provider":     map[string]any{"reference": "urn:uuid:11111111-1111-4111-8111-000000000002"},
						"priority":     map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/processpriority", "code": "normal"}}},
						"insurance":    []any{map[string]any{"sequence": 1, "focal": true, "coverage": map[string]any{"reference": "urn:uuid:11111111-1111-4111-8111-000000000004"}}},
						"item": []any{
							map[string]any{
								"extension": []any{
									map[string]any{"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-authorizationNumber", "valueString": "AUTH-OLD-001"},
								},
								"sequence":         1,
								"category":         map[string]any{"coding": []any{map[string]any{"system": "https://codesystem.x12.org/005010/1365", "code": "3"}}},
								"productOrService": map[string]any{"coding": []any{map[string]any{"system": "https://codesystem.x12.org/005010/1365", "code": "3"}}},
								"servicedDate":     "2026-02-10",
								"quantity":         map[string]any{"value": 1, "unit": "unit", "system": "https://codesystem.x12.org/005010/673", "code": "UN"},
							},
						},
					},
				},
				map[string]any{
					"fullUrl": "urn:uuid:11111111-1111-4111-8111-000000000003",
					"resource": map[string]any{
						"resourceType": "Patient",
						"id":           "11111111-1111-4111-8111-000000000003",
						"identifier":   []any{map[string]any{"system": "http://flint.health/pas/member", "value": "PAS-MEM-001"}},
						"name":         []any{map[string]any{"family": "Smith", "given": []string{"Joe"}}},
					},
				},
				map[string]any{
					"fullUrl": "urn:uuid:11111111-1111-4111-8111-000000000001",
					"resource": map[string]any{
						"resourceType": "Organization",
						"id":           "11111111-1111-4111-8111-000000000001",
						"identifier":   []any{map[string]any{"system": "http://hl7.org/fhir/sid/us-npi", "value": "8383066098"}},
						"active":       true,
						"type":         []any{map[string]any{"coding": []any{map[string]any{"system": "https://codesystem.x12.org/005010/98", "code": "PR"}}}},
						"name":         "PAS Payer",
					},
				},
				map[string]any{
					"fullUrl": "urn:uuid:11111111-1111-4111-8111-000000000002",
					"resource": map[string]any{
						"resourceType": "Organization",
						"id":           "11111111-1111-4111-8111-000000000002",
						"identifier":   []any{map[string]any{"system": "http://hl7.org/fhir/sid/us-npi", "value": "8189991234"}},
						"active":       true,
						"type":         []any{map[string]any{"coding": []any{map[string]any{"system": "https://codesystem.x12.org/005010/98", "code": "X3"}}}},
						"name":         "PAS Requestor",
					},
				},
				map[string]any{
					"fullUrl": "urn:uuid:11111111-1111-4111-8111-000000000004",
					"resource": map[string]any{
						"resourceType": "Coverage",
						"id":           "11111111-1111-4111-8111-000000000004",
						"status":       "active",
						"beneficiary":  map[string]any{"reference": "urn:uuid:11111111-1111-4111-8111-000000000003"},
						"payor":        []any{map[string]any{"reference": "urn:uuid:11111111-1111-4111-8111-000000000001"}},
					},
				},
			},
		}
		raw, _ := json.Marshal(doc)
		return string(raw)
	}

	for _, tc := range []struct {
		trace    string
		wantCode string
	}{
		{"PA-APPROVAL-001", `"code":"A1"`},
		{"PA-DENIAL-001", `"code":"A3"`},
		{"PA-PENDED-001", `"code":"A4"`},
		{"PA-MUST-SUPPORT-SUBMIT-001", `"CommunicationRequest"`},
	} {
		respSubOp, sErr := client.Post(ts.URL+"/fhir/r4/"+pasTenant+"/Claim/$submit", "application/fhir+json", strings.NewReader(makePASBundle(tc.trace, false)))
		require.NoError(t, sErr, tc.trace)
		b, _ := io.ReadAll(respSubOp.Body)
		respSubOp.Body.Close()
		require.Equal(t, http.StatusOK, respSubOp.StatusCode, string(b))
		assert.Contains(t, string(b), tc.wantCode, tc.trace)

		out := valEngine.ValidateJSONWithProfiles("Bundle", b, []string{"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-response-bundle"}, "")
		assert.True(t, out.IsValid(), "PAS response bundle validation failed for %s: %+v", tc.trace, out.Issues)
	}

	// Verify async pended finalization event-notification Bundle arrived with PAS Response Bundle focus (A1)
	require.Eventually(t, func() bool {
		webhookMu.Lock()
		defer webhookMu.Unlock()
		return len(receivedWebhooks) >= 2
	}, 5*time.Second, 50*time.Millisecond, "expected async PAS pended finalization event-notification")
	webhookMu.Lock()
	eventNotif := receivedWebhooks[len(receivedWebhooks)-1]
	webhookMu.Unlock()
	notifEntries, _ := eventNotif["entry"].([]any)
	require.GreaterOrEqual(t, len(notifEntries), 2)
	focusRes, _ := notifEntries[1].(map[string]any)["resource"].(map[string]any)
	assert.Equal(t, "Bundle", focusRes["resourceType"])
	assert.Equal(t, "collection", focusRes["type"])
	focusRaw, _ := json.Marshal(focusRes)
	assert.Contains(t, string(focusRaw), `"code":"A1"`)

	// Verify Claim/$inquire returns Parameters{parameter: [{name: "return", resource: Bundle}]} for both reference & DAR modes
	for _, inqTrace := range []string{"PA-MS-INQUIRE-001", "PA-MS-INQUIRE-DAR-002"} {
		respInq, iErr := client.Post(ts.URL+"/fhir/r4/"+pasTenant+"/Claim/$inquire", "application/fhir+json", strings.NewReader(makePASBundle(inqTrace, true)))
		require.NoError(t, iErr, inqTrace)
		inqBytes, _ := io.ReadAll(respInq.Body)
		respInq.Body.Close()
		require.Equal(t, http.StatusOK, respInq.StatusCode, string(inqBytes))

		var inqParams map[string]any
		require.NoError(t, json.Unmarshal(inqBytes, &inqParams))
		assert.Equal(t, "Parameters", inqParams["resourceType"])
		retParam := inqParams["parameter"].([]any)[0].(map[string]any)
		assert.Equal(t, "return", retParam["name"])
		retBundle := retParam["resource"].(map[string]any)
		assert.Equal(t, "Bundle", retBundle["resourceType"])
		assert.Equal(t, "collection", retBundle["type"])
		assert.Contains(t, string(inqBytes), `"AUTH-REPLACED-AUTH-OLD-001"`, "expected spec-47 replaced PA authorization number")
		if strings.Contains(inqTrace, "DAR") {
			assert.Contains(t, string(inqBytes), `http://hl7.org/fhir/StructureDefinition/data-absent-reason`)
		}
	}

	// Verify HTTP 400 OperationOutcome on empty nonconformant Bundle for both $submit and $inquire
	emptyBundle := []byte(`{"resourceType":"Bundle","type":"collection"}`)
	for _, ep := range []string{"/Claim/$submit", "/Claim/$inquire"} {
		respErr, eErr := client.Post(ts.URL+"/fhir/r4/"+pasTenant+ep, "application/fhir+json", bytes.NewReader(emptyBundle))
		require.NoError(t, eErr)
		errBytes, _ := io.ReadAll(respErr.Body)
		respErr.Body.Close()
		assert.Equal(t, http.StatusBadRequest, respErr.StatusCode, ep)
		assert.Contains(t, string(errBytes), `"OperationOutcome"`)
	}

	// 5. Verify Inferno FHIRPath evaluation mode (`POST /$fhirpath?mode=inferno&expression=...`) for PAS & DTR
	fpClaim := `{
		"resourceType":"Claim",
		"id":"fp-claim-1",
		"extension":[{"url":"http://hl7.org/fhir/5.0/StructureDefinition/extension-Claim.encounter","valueReference":{"reference":"urn:uuid:11111111-1111-4111-8111-000000000009"}}],
		"patient":{"reference":"urn:uuid:11111111-1111-4111-8111-000000000003"},
		"item":[{"sequence":1,"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-requestedService","valueReference":{"reference":"urn:uuid:11111111-1111-4111-8111-000000000008"}}]}]
	}`
	for _, fpTC := range []struct {
		expr    string
		wantRef string
	}{
		{"Claim.patient", "urn:uuid:11111111-1111-4111-8111-000000000003"},
		{"Claim.item.extension.value", "urn:uuid:11111111-1111-4111-8111-000000000008"},
		{"Claim.extension.where(url = 'http://hl7.org/fhir/5.0/StructureDefinition/extension-Claim.encounter').value", "urn:uuid:11111111-1111-4111-8111-000000000009"},
	} {
		fpURL := ts.URL + "/fhir/r4/" + pasTenant + "/$fhirpath?mode=inferno&expression=" + url.QueryEscape(fpTC.expr)
		respFP, fpErr := client.Post(fpURL, "application/fhir+json", strings.NewReader(fpClaim))
		require.NoError(t, fpErr, fpTC.expr)
		fpBytes, _ := io.ReadAll(respFP.Body)
		respFP.Body.Close()
		require.Equal(t, http.StatusOK, respFP.StatusCode, string(fpBytes))
		var fpArr []map[string]any
		require.NoError(t, json.Unmarshal(fpBytes, &fpArr))
		require.Len(t, fpArr, 1, fpTC.expr)
		assert.Equal(t, "Reference", fpArr[0]["type"], fpTC.expr)
		elemMap, _ := fpArr[0]["element"].(map[string]any)
		assert.Equal(t, fpTC.wantRef, elemMap["reference"], fpTC.expr)
	}
}
