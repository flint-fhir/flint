package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
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
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/sqlite"
)

func TestSubscriptionsR5Backport_FullWorkflowAndRejections(t *testing.T) {
	sqStore := sqlite.New(nil)

	srv := server.New(sqStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	t.Cleanup(func() { _ = srv.Close() })

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	fhirBase := ts.URL + "/fhir/r4/demo"

	// 1. Verify CapabilityStatement declares backport-subscription profile & canonical SubscriptionTopics
	metaResp, err := http.Get(fhirBase + "/metadata")
	require.NoError(t, err)
	metaBytes, err := io.ReadAll(metaResp.Body)
	metaResp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, metaResp.StatusCode)

	var capMap map[string]any
	require.NoError(t, json.Unmarshal(metaBytes, &capMap))
	restArr := capMap["rest"].([]any)
	resArr := restArr[0].(map[string]any)["resource"].([]any)
	var subEntry map[string]any
	for _, r := range resArr {
		rm := r.(map[string]any)
		if rm["type"] == "Subscription" {
			subEntry = rm
			break
		}
	}
	require.NotNil(t, subEntry, "CapabilityStatement must include Subscription resource entry")
	assert.Contains(t, subEntry["supportedProfile"], "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription")
	exts := subEntry["extension"].([]any)
	require.NotEmpty(t, exts, "Subscription CapabilityStatement entry must advertise capabilitystatement-subscriptiontopic-canonical extensions")

	// 2. Verify all 7 R5 Backport Subscription rejection scenarios return 422 Unprocessable Entity
	validBase := func() map[string]any {
		return map[string]any{
			"resourceType": "Subscription",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription"},
			},
			"status":   "requested",
			"end":      "2099-12-31T12:00:00Z",
			"reason":   "Backport rejection test",
			"criteria": "https://inferno.healthit.gov/suites/custom/subscriptions_r5_backport_r4_client/topics/patient-admission",
			"_criteria": map[string]any{
				"extension": []map[string]any{
					{
						"url":         "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria",
						"valueString": "Encounter.patient=Patient/pat-alice-001",
					},
				},
			},
			"channel": map[string]any{
				"type":     "rest-hook",
				"endpoint": "https://example.org/hook",
				"payload":  "application/fhir+json",
				"_payload": map[string]any{
					"extension": []map[string]any{
						{
							"url":       "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content",
							"valueCode": "full-resource",
						},
					},
				},
			},
		}
	}

	rejectionCases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{
			name: "cross-version R5 extension",
			mutate: func(m map[string]any) {
				m["_criteria"] = map[string]any{
					"extension": []map[string]any{
						{
							"url":         "http://hl7.org/fhir/5.0/subscriptions-backport/StructureDefinition/backport-filter-criteria",
							"valueString": "Encounter.patient=Patient/pat-alice-001",
						},
					},
				}
			},
		},
		{
			name: "unsupported SubscriptionTopic canonical URL",
			mutate: func(m map[string]any) {
				m["criteria"] = "http://example.org/FHIR/SubscriptionTopic/unsupported-topic"
			},
		},
		{
			name: "unsupported backport-filter-criteria parameter",
			mutate: func(m map[string]any) {
				m["_criteria"] = map[string]any{
					"extension": []map[string]any{
						{
							"url":         "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria",
							"valueString": "Encounter?unsupported_search_param=123",
						},
					},
				}
			},
		},
		{
			name: "unsupported channel.type",
			mutate: func(m map[string]any) {
				m["channel"].(map[string]any)["type"] = "email"
			},
		},
		{
			name: "unsupported channel.endpoint scheme",
			mutate: func(m map[string]any) {
				m["channel"].(map[string]any)["endpoint"] = "ftp://unsupported.example.org/hook"
			},
		},
		{
			name: "unsupported channel.payload MIME type",
			mutate: func(m map[string]any) {
				m["channel"].(map[string]any)["payload"] = "application/pdf"
			},
		},
		{
			name: "unsupported websocket + cloudevents combination",
			mutate: func(m map[string]any) {
				ch := m["channel"].(map[string]any)
				ch["type"] = "websocket"
				ch["payload"] = "application/cloudevents+json"
			},
		},
	}

	for _, tc := range rejectionCases {
		t.Run("Reject/"+tc.name, func(t *testing.T) {
			sub := validBase()
			tc.mutate(sub)
			raw, err := json.Marshal(sub)
			require.NoError(t, err)
			resp, err := http.Post(fhirBase+"/Subscription", "application/fhir+json", bytes.NewReader(raw))
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, "expected 422 rejection for %s", tc.name)
		})
	}

	// 3. Verify live rest-hook handshake, heartbeat, event-notification (empty, id-only, full-resource), $status, PUT, PATCH, DELETE
	type capturedWebhook struct {
		AuthHeader  string
		ContentType string
		Bundle      map[string]any
		StatusParam map[string]any
		NotifType   string
	}
	var (
		mu       sync.Mutex
		received []capturedWebhook
	)
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(body, &b)
		var stMap map[string]any
		var nType string
		if entries, ok := b["entry"].([]any); ok && len(entries) > 0 {
			if e0, ok := entries[0].(map[string]any); ok {
				stMap, _ = e0["resource"].(map[string]any)
				if params, ok := stMap["parameter"].([]any); ok {
					for _, p := range params {
						pm, _ := p.(map[string]any)
						if pm["name"] == "type" {
							nType, _ = pm["valueCode"].(string)
						}
					}
				}
			}
		}
		mu.Lock()
		received = append(received, capturedWebhook{
			AuthHeader:  r.Header.Get("Authorization"),
			ContentType: r.Header.Get("Content-Type"),
			Bundle:      b,
			StatusParam: stMap,
			NotifType:   nType,
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(webhookSrv.Close)

	valEngine := validation.NewEngine(validation.DefaultOptions())

	for _, mode := range []string{"full-resource", "id-only", "empty"} {
		t.Run("Mode/"+mode, func(t *testing.T) {
			mu.Lock()
			received = nil
			mu.Unlock()

			sub := validBase()
			ch := sub["channel"].(map[string]any)
			ch["endpoint"] = webhookSrv.URL
			ch["header"] = []string{"Authorization: Bearer notify-tok-" + mode}
			if mode == "full-resource" {
				ch["extension"] = []map[string]any{
					{
						"url":              "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-heartbeat-period",
						"valueUnsignedInt": 1,
					},
				}
			}
			ch["_payload"] = map[string]any{
				"extension": []map[string]any{
					{
						"url":       "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content",
						"valueCode": mode,
					},
				},
			}

			rawSub, err := json.Marshal(sub)
			require.NoError(t, err)
			createResp, err := http.Post(fhirBase+"/Subscription", "application/fhir+json", bytes.NewReader(rawSub))
			require.NoError(t, err)
			createdBytes, err := io.ReadAll(createResp.Body)
			createResp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, createResp.StatusCode)

			var createdMap map[string]any
			require.NoError(t, json.Unmarshal(createdBytes, &createdMap))
			assert.Equal(t, "requested", createdMap["status"], "Backport rest-hook creation response must preserve status='requested'")
			subID := createdMap["id"].(string)

			// Wait for async handshake (and heartbeat when mode == full-resource)
			require.Eventually(t, func() bool {
				mu.Lock()
				defer mu.Unlock()
				hasHS := false
				hasHB := mode != "full-resource"
				for _, w := range received {
					if w.NotifType == "handshake" {
						hasHS = true
					}
					if w.NotifType == "heartbeat" {
						hasHB = true
					}
				}
				return hasHS && hasHB
			}, 4*time.Second, 50*time.Millisecond, "expected handshake (and heartbeat) delivery")

			// Verify $status returns a searchset Bundle with status='active'
			stResp, err := http.Get(fhirBase + "/Subscription/" + subID + "/$status")
			require.NoError(t, err)
			stBytes, err := io.ReadAll(stResp.Body)
			stResp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, stResp.StatusCode)

			var stBundle map[string]any
			require.NoError(t, json.Unmarshal(stBytes, &stBundle))
			assert.Equal(t, "Bundle", stBundle["resourceType"])
			assert.Equal(t, "searchset", stBundle["type"])
			stEntries := stBundle["entry"].([]any)
			require.Len(t, stEntries, 1)
			stParams := stEntries[0].(map[string]any)["resource"].(map[string]any)
			stParamsJSON, _ := json.Marshal(stParams)
			stVal := valEngine.ValidateJSONWithProfiles("Parameters", stParamsJSON, []string{"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription-status-r4"}, "")
			assert.True(t, stVal.IsValid(), "expected backport-subscription-status-r4 Parameters to pass validation: %+v", stVal.Issues)

			// Trigger matching Encounter creation
			encJSON := `{"resourceType":"Encounter","status":"in-progress","class":{"system":"http://terminology.hl7.org/CodeSystem/v3-ActCode","code":"IMP"},"type":[{"coding":[{"system":"http://snomed.info/sct","code":"32485007"}]}],"subject":{"reference":"Patient/pat-alice-001"}}`
			encResp, err := http.Post(fhirBase+"/Encounter", "application/fhir+json", strings.NewReader(encJSON))
			require.NoError(t, err)
			encResp.Body.Close()
			require.Equal(t, http.StatusCreated, encResp.StatusCode)

			var evCapture capturedWebhook
			require.Eventually(t, func() bool {
				mu.Lock()
				defer mu.Unlock()
				for _, w := range received {
					if w.NotifType == "event-notification" {
						evCapture = w
						return true
					}
				}
				return false
			}, 4*time.Second, 50*time.Millisecond, "expected event-notification delivery")

			assert.Equal(t, "Bearer notify-tok-"+mode, evCapture.AuthHeader)
			assert.True(t, strings.HasPrefix(evCapture.ContentType, "application/fhir+json"))
			assert.Equal(t, "history", evCapture.Bundle["type"])

			bEntries := evCapture.Bundle["entry"].([]any)
			e0 := bEntries[0].(map[string]any)
			assert.True(t, strings.HasSuffix(e0["request"].(map[string]any)["url"].(string), "Subscription/"+subID+"/$status"))
			assert.Equal(t, "200", e0["response"].(map[string]any)["status"])

			switch mode {
			case "empty":
				assert.Len(t, bEntries, 1, "empty notification must only include SubscriptionStatus entry")
			case "id-only":
				require.Len(t, bEntries, 2)
				e1 := bEntries[1].(map[string]any)
				assert.Nil(t, e1["resource"], "id-only notification entry[1] SHALL NOT contain resource field")
				assert.Contains(t, e1["fullUrl"].(string), "/Encounter/")
			case "full-resource":
				require.Len(t, bEntries, 2)
				e1 := bEntries[1].(map[string]any)
				require.NotNil(t, e1["resource"], "full-resource notification entry[1] must contain Encounter resource")
			}

			// Verify JSON Patch & Delete on Backport Subscription
			patchBody := `[{"op":"replace","path":"/reason","value":"Updated via JSON Patch"}]`
			patchReq, err := http.NewRequest(http.MethodPatch, fhirBase+"/Subscription/"+subID, strings.NewReader(patchBody))
			require.NoError(t, err)
			patchReq.Header.Set("Content-Type", "application/json-patch+json")
			patchResp, err := http.DefaultClient.Do(patchReq)
			require.NoError(t, err)
			patchResp.Body.Close()
			assert.Equal(t, http.StatusOK, patchResp.StatusCode)

			delReq, err := http.NewRequest(http.MethodDelete, fhirBase+"/Subscription/"+subID, nil)
			require.NoError(t, err)
			delResp, err := http.DefaultClient.Do(delReq)
			require.NoError(t, err)
			delResp.Body.Close()
			assert.Equal(t, http.StatusNoContent, delResp.StatusCode)
		})
	}
}

func TestONCG10SMART_ContextStyleScopeNarrowingAndCascadeRevocation(t *testing.T) {
	sqStore := sqlite.New(nil)

	srv := server.New(sqStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	srv.EnableDevTokenEnforcement()
	srv.SetRequireRegisteredClientJWK(true)
	srv.RegisterOAuthClientSecret("g10-sym-client", "g10-sym-secret")
	t.Cleanup(func() { _ = srv.Close() })

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	fhirBase := ts.URL + "/fhir/r4/demo"

	// 1. SMART Discovery advertises context-banner & context-style
	discResp, err := http.Get(fhirBase + "/.well-known/smart-configuration")
	require.NoError(t, err)
	var discDoc map[string]any
	require.NoError(t, json.NewDecoder(discResp.Body).Decode(&discDoc))
	discResp.Body.Close()
	assert.Contains(t, discDoc["capabilities"], "context-banner")
	assert.Contains(t, discDoc["capabilities"], "context-style")

	// 2. Consent scope narrowing via granted_scope + live smart_style_url + cascade token revocation
	verifier, challenge := pkceS256Pair()
	redirectURI := "http://127.0.0.1:14569/custom/smart/redirect"
	noRedir := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	authResp, err := noRedir.Get(ts.URL + "/oauth2/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {"g10-sym-client"},
		"redirect_uri":          {redirectURI},
		"scope":                 {"launch/patient openid fhirUser offline_access patient/Patient.rs patient/Condition.rs patient/Observation.rs"},
		"granted_scope":         {"launch/patient openid fhirUser offline_access patient/Patient.rs patient/Condition.rs?category=http://terminology.hl7.org/CodeSystem/condition-category|problem-list-item"},
		"state":                 {"s1"},
		"aud":                   {fhirBase},
		"patient":               {"pat-alice-001"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode())
	require.NoError(t, err)
	authResp.Body.Close()
	require.Equal(t, http.StatusFound, authResp.StatusCode)
	cbURI, err := url.Parse(authResp.Header.Get("Location"))
	require.NoError(t, err)
	code := cbURI.Query().Get("code")
	require.NotEmpty(t, code)

	tokReq, err := http.NewRequest(http.MethodPost, ts.URL+"/oauth2/token", strings.NewReader(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}.Encode()))
	require.NoError(t, err)
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokReq.SetBasicAuth("g10-sym-client", "g10-sym-secret")
	tokResp, err := http.DefaultClient.Do(tokReq)
	require.NoError(t, err)
	var tokDoc map[string]any
	require.NoError(t, json.NewDecoder(tokResp.Body).Decode(&tokDoc))
	tokResp.Body.Close()
	require.Equal(t, http.StatusOK, tokResp.StatusCode)

	assert.Contains(t, tokDoc["scope"].(string), "patient/Condition.rs?category=")
	assert.NotContains(t, tokDoc["scope"].(string), "patient/Observation.rs")

	// smart_style_url must resolve 200 OK JSON on Flint itself
	styleURL := tokDoc["smart_style_url"].(string)
	styleResp, err := http.Get(styleURL)
	require.NoError(t, err)
	styleBytes, _ := io.ReadAll(styleResp.Body)
	styleResp.Body.Close()
	assert.Equal(t, http.StatusOK, styleResp.StatusCode)
	assert.True(t, json.Valid(styleBytes))

	// Revoking access_token must immediately invalidate BOTH access_token AND refresh_token
	revReq, err := http.NewRequest(http.MethodPost, ts.URL+"/oauth2/revoke", strings.NewReader(url.Values{
		"token": {tokDoc["access_token"].(string)},
	}.Encode()))
	require.NoError(t, err)
	revReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	revReq.SetBasicAuth("g10-sym-client", "g10-sym-secret")
	revResp, err := http.DefaultClient.Do(revReq)
	require.NoError(t, err)
	revResp.Body.Close()
	require.Equal(t, http.StatusOK, revResp.StatusCode)

	patReq, err := http.NewRequest(http.MethodGet, fhirBase+"/Patient/pat-alice-001", nil)
	require.NoError(t, err)
	patReq.Header.Set("Authorization", "Bearer "+tokDoc["access_token"].(string))
	patResp, err := http.DefaultClient.Do(patReq)
	require.NoError(t, err)
	patResp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, patResp.StatusCode)

	rtReq, err := http.NewRequest(http.MethodPost, ts.URL+"/oauth2/token", strings.NewReader(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tokDoc["refresh_token"].(string)},
	}.Encode()))
	require.NoError(t, err)
	rtReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rtReq.SetBasicAuth("g10-sym-client", "g10-sym-secret")
	rtResp, err := http.DefaultClient.Do(rtReq)
	require.NoError(t, err)
	rtResp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, rtResp.StatusCode, "refresh_token must fail after sibling access_token revocation")
}
