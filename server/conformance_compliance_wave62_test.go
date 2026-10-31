package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestConformance_R4CoreInvariants(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-r4-invariants-w62"
	cleanup := func() {
		_ = postgres.PurgeTenant(ctx, db, tenant)
	}
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	baseURL := ts.URL + "/fhir/r4/" + tenant

	validateJSON := func(resType, payload string) string {
		t.Helper()
		resp, err := http.Post(baseURL+"/"+resType+"/$validate", "application/fhir+json", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("POST /%s/$validate: %v", resType, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// 1. obs-6 on root Observation (valueQuantity + dataAbsentReason)
	outObs6Root := validateJSON("Observation", `{
		"resourceType": "Observation",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
		"valueQuantity": {"value": 72, "unit": "/min"},
		"dataAbsentReason": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason", "code": "unknown"}]}
	}`)
	if !strings.Contains(outObs6Root, "obs-6") {
		t.Fatalf("expected obs-6 violation on root Observation, got: %s", outObs6Root)
	}

	// 2. obs-6 on Observation.component[0]
	outObs6Comp := validateJSON("Observation", `{
		"resourceType": "Observation",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
		"component": [{
			"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
			"valueQuantity": {"value": 120, "unit": "mmHg"},
			"dataAbsentReason": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason", "code": "unknown"}]}
		}]
	}`)
	if !strings.Contains(outObs6Comp, "obs-6") {
		t.Fatalf("expected obs-6 violation on Observation.component[0], got: %s", outObs6Comp)
	}

	// 3. obs-7 on Observation (Observation.code == Observation.component[0].code while root valueQuantity present)
	outObs7 := validateJSON("Observation", `{
		"resourceType": "Observation",
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
		"valueQuantity": {"value": 120, "unit": "mmHg"},
		"component": [{
			"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
			"valueQuantity": {"value": 120, "unit": "mmHg"}
		}]
	}`)
	if !strings.Contains(outObs7, "obs-7") {
		t.Fatalf("expected obs-7 violation on Observation, got: %s", outObs7)
	}

	// 4. pat-1 on Patient (contact without name, telecom, address, or organization)
	outPat1Bad := validateJSON("Patient", `{
		"resourceType": "Patient",
		"contact": [{"relationship": [{"text": "Emergency"}]}]
	}`)
	if !strings.Contains(outPat1Bad, "pat-1") {
		t.Fatalf("expected pat-1 violation on Patient.contact[0], got: %s", outPat1Bad)
	}

	outPat1Good := validateJSON("Patient", `{
		"resourceType": "Patient",
		"contact": [{
			"relationship": [{"text": "Emergency"}],
			"name": {"family": "Smith"}
		}]
	}`)
	if strings.Contains(outPat1Good, "pat-1") {
		t.Fatalf("unexpected pat-1 violation on valid Patient.contact[0]: %s", outPat1Good)
	}

	// 5. dom-3 on Patient (unreferenced contained resource)
	outDom3Bad := validateJSON("Patient", `{
		"resourceType": "Patient",
		"name": [{"family": "UnreferencedContained"}],
		"contained": [{
			"resourceType": "Organization",
			"id": "org-orphan",
			"name": "Orphan Org"
		}]
	}`)
	if !strings.Contains(outDom3Bad, "dom-3") {
		t.Fatalf("expected dom-3 violation on unreferenced contained resource, got: %s", outDom3Bad)
	}

	outDom3Good := validateJSON("Patient", `{
		"resourceType": "Patient",
		"name": [{"family": "ReferencedContained"}],
		"managingOrganization": {"reference": "#org-linked"},
		"contained": [{
			"resourceType": "Organization",
			"id": "org-linked",
			"name": "Linked Org"
		}]
	}`)
	if strings.Contains(outDom3Good, "dom-3") {
		t.Fatalf("unexpected dom-3 violation when contained resource is referenced: %s", outDom3Good)
	}

	// 6. bdl-3 on Bundle (collection bundle with entry.request)
	outBdl3 := validateJSON("Bundle", `{
		"resourceType": "Bundle",
		"type": "collection",
		"entry": [{
			"resource": {"resourceType": "Patient", "id": "p1"},
			"request": {"method": "POST", "url": "Patient"}
		}]
	}`)
	if !strings.Contains(outBdl3, "bdl-3") {
		t.Fatalf("expected bdl-3 violation on collection Bundle with entry.request, got: %s", outBdl3)
	}
}

func TestConformance_RuntimeCustomStructureDefinitionAndFHIRPathConstraints(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-custom-sd-w62"
	cleanup := func() {
		_ = postgres.PurgeTenant(ctx, db, tenant)
	}
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	baseURL := ts.URL + "/fhir/r4/" + tenant
	profileURL := "http://example.org/fhir/StructureDefinition/custom-obs-profile"

	sdJSON := `{
		"resourceType": "StructureDefinition",
		"id": "custom-obs-profile",
		"url": "http://example.org/fhir/StructureDefinition/custom-obs-profile",
		"version": "1.0.0",
		"name": "CustomObservationProfile",
		"status": "active",
		"kind": "resource",
		"abstract": false,
		"type": "Observation",
		"baseDefinition": "http://hl7.org/fhir/StructureDefinition/Observation",
		"derivation": "constraint",
		"differential": {
			"element": [
				{
					"id": "Observation",
					"path": "Observation",
					"constraint": [
						{
							"key": "co-1",
							"severity": "error",
							"human": "Observation valueQuantity must be positive and subject must be present",
							"expression": "Observation.value.value > 0 and Observation.subject.exists()"
						}
					]
				},
				{
					"id": "Observation.status",
					"path": "Observation.status",
					"fixedCode": "final"
				},
				{
					"id": "Observation.effective[x]",
					"path": "Observation.effective[x]",
					"min": 1
				},
				{
					"id": "Observation.note",
					"path": "Observation.note",
					"max": "0"
				},
				{
					"id": "Observation.extension:deviceModality",
					"path": "Observation.extension",
					"sliceName": "deviceModality",
					"min": 1,
					"fixedUri": "http://example.org/fhir/StructureDefinition/device-modality"
				}
			]
		}
	}`

	reqPut, _ := http.NewRequest(http.MethodPut, baseURL+"/StructureDefinition/custom-obs-profile", strings.NewReader(sdJSON))
	reqPut.Header.Set("Content-Type", "application/fhir+json")
	respPut, err := http.DefaultClient.Do(reqPut)
	if err != nil {
		t.Fatalf("PUT StructureDefinition: %v", err)
	}
	respPut.Body.Close()
	if respPut.StatusCode != http.StatusOK && respPut.StatusCode != http.StatusCreated {
		t.Fatalf("expected 200/201 on PUT StructureDefinition, got %d", respPut.StatusCode)
	}

	// 1. Non-conforming Observation (negative value, status=preliminary, missing effective[x], prohibited note, missing extension slice)
	badObs := `{
		"resourceType": "Observation",
		"status": "preliminary",
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
		"valueQuantity": {"value": -5, "unit": "/min"},
		"note": [{"text": "prohibited note"}]
	}`
	respBad, err := http.Post(baseURL+"/Observation/$validate?profile="+profileURL, "application/fhir+json", strings.NewReader(badObs))
	if err != nil {
		t.Fatalf("POST $validate badObs: %v", err)
	}
	bBad, _ := io.ReadAll(respBad.Body)
	respBad.Body.Close()
	badStr := string(bBad)
	for _, expectedSub := range []string{"co-1", "effective", "note", "final", "deviceModality"} {
		if !strings.Contains(badStr, expectedSub) {
			t.Errorf("expected custom profile validation output to contain %q, got:\n%s", expectedSub, badStr)
		}
	}

	// 2. Conforming Observation declaring meta.profile
	goodObs := `{
		"resourceType": "Observation",
		"meta": {
			"profile": ["http://example.org/fhir/StructureDefinition/custom-obs-profile|1.0.0"]
		},
		"extension": [
			{
				"url": "http://example.org/fhir/StructureDefinition/device-modality",
				"valueCode": "wearable-ppg"
			}
		],
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
		"subject": {"reference": "Patient/example"},
		"effectiveDateTime": "2026-10-31T12:00:00Z",
		"valueQuantity": {"value": 68, "unit": "/min"}
	}`
	respGood, err := http.Post(baseURL+"/Observation/$validate", "application/fhir+json", strings.NewReader(goodObs))
	if err != nil {
		t.Fatalf("POST $validate goodObs: %v", err)
	}
	bGood, _ := io.ReadAll(respGood.Body)
	respGood.Body.Close()
	goodStr := string(bGood)
	if strings.Contains(goodStr, `"severity":"error"`) || strings.Contains(goodStr, "not registered") {
		t.Fatalf("expected 0 errors and no unregistered profile warning on conforming Observation, got:\n%s", goodStr)
	}
}

func TestConformance_ONCHTI1_DSISourceAttributesAndFeedback(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-hti1-dsi-w62"
	cleanup := func() {
		_ = postgres.PurgeTenant(ctx, db, tenant)
	}
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Verify GET /cds-services includes ONC HTI-1 (b)(11) DSI source attributes
	respDisc, err := http.Get(ts.URL + "/cds-services")
	if err != nil {
		t.Fatalf("GET /cds-services: %v", err)
	}
	bDisc, _ := io.ReadAll(respDisc.Body)
	respDisc.Body.Close()
	if respDisc.StatusCode != http.StatusOK || !strings.Contains(string(bDisc), "170.315(b)(11)") || !strings.Contains(string(bDisc), "dsiSourceAttributes") {
		t.Fatalf("expected GET /cds-services to include ONC HTI-1 (b)(11) dsiSourceAttributes, got %d: %s", respDisc.StatusCode, string(bDisc))
	}

	// 2. Verify GET /cds-services/{serviceId}/$dsi-attributes and tenant-scoped equivalent
	for _, urlPath := range []string{
		ts.URL + "/cds-services/flint-hypertension-care-gap/$dsi-attributes",
		ts.URL + "/fhir/r4/" + tenant + "/cds-services/flint-diabetes-a1c-care-gap/$dsi-attributes",
	} {
		respAttr, err := http.Get(urlPath)
		if err != nil {
			t.Fatalf("GET %s: %v", urlPath, err)
		}
		var attrs map[string]any
		_ = json.NewDecoder(respAttr.Body).Decode(&attrs)
		respAttr.Body.Close()
		if respAttr.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK from %s, got %d", urlPath, respAttr.StatusCode)
		}
		for _, reqKey := range []string{
			"regulatoryCriterion",
			"bibliographicCitation",
			"developer",
			"fundingSource",
			"releaseVersion",
			"revisionDate",
			"patientDemographicsUsed",
			"socialDeterminantsUsed",
			"intendedUse",
			"cautionedOutOfScopeUse",
			"knownRisksAndLimitations",
			"validationSummary",
		} {
			if attrs[reqKey] == nil {
				t.Errorf("missing required ONC HTI-1 (b)(11) attribute %q in %v", reqKey, attrs)
			}
		}
	}

	// 3. Verify POST /cds-services/{serviceId}/feedback records accepted/overridden feedback and rejects invalid outcome
	fbPayload := `{
		"feedback": [
			{
				"card": "card-htn-gap-pat-1",
				"outcome": "overridden",
				"overrideReason": {
					"reason": {
						"system": "http://terminology.hl7.org/CodeSystem/cds-override-reason",
						"code": "contraindicated"
					},
					"userComment": "Patient on palliative care pathway"
				}
			},
			{
				"card": "card-htn-gap-pat-2",
				"outcome": "accepted",
				"acceptedSuggestions": [{"id": "sug-bp-order-pat-2"}]
			}
		]
	}`
	respFB, err := http.Post(ts.URL+"/fhir/r4/"+tenant+"/cds-services/flint-hypertension-care-gap/feedback", "application/json", strings.NewReader(fbPayload))
	if err != nil {
		t.Fatalf("POST feedback: %v", err)
	}
	bFB, _ := io.ReadAll(respFB.Body)
	respFB.Body.Close()
	if respFB.StatusCode != http.StatusOK || !strings.Contains(string(bFB), `"recordedCount":2`) {
		t.Fatalf("expected 200 OK with recordedCount=2 on CDS feedback, got %d: %s", respFB.StatusCode, string(bFB))
	}

	respBadFB, err := http.Post(ts.URL+"/cds-services/flint-hypertension-care-gap/feedback", "application/json", strings.NewReader(`{"feedback":[{"card":"c1","outcome":"ignored"}]}`))
	if err != nil {
		t.Fatalf("POST invalid feedback: %v", err)
	}
	respBadFB.Body.Close()
	if respBadFB.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid CDS feedback outcome, got %d", respBadFB.StatusCode)
	}
}

func TestConformance_TEFCAPurposeOfUseAndTamperEvidentAuditChain(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-tefca-audit-w62"
	cleanup := func() {
		_ = postgres.PurgeTenant(ctx, db, tenant)
	}
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	srv.SetAuditRecorder(audit.NewStoreRecorder(st))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	baseURL := ts.URL + "/fhir/r4/" + tenant

	// 1. Create a Patient with TEFCA X-Purpose-Of-Use: TREAT
	reqCreate, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient", bytes.NewBufferString(`{
		"resourceType": "Patient",
		"id": "tefca-pat-1",
		"active": true,
		"name": [{"family": "TefcaAudit", "given": ["Alice"]}]
	}`))
	reqCreate.Header.Set("Content-Type", "application/fhir+json")
	reqCreate.Header.Set("X-Purpose-Of-Use", "TREAT")
	respCreate, err := http.DefaultClient.Do(reqCreate)
	if err != nil {
		t.Fatalf("POST Patient with X-Purpose-Of-Use: %v", err)
	}
	respCreate.Body.Close()
	if respCreate.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", respCreate.StatusCode)
	}
	if gotHdr := respCreate.Header.Get("X-TEFCA-Exchange-Purpose"); gotHdr != "TREAT" {
		t.Fatalf("expected X-TEFCA-Exchange-Purpose=TREAT response header, got %q", gotHdr)
	}

	// 2. Read the Patient with X-TEFCA-Exchange-Purpose: IAS
	reqRead, _ := http.NewRequest(http.MethodGet, baseURL+"/Patient/tefca-pat-1", nil)
	reqRead.Header.Set("X-TEFCA-Exchange-Purpose", "IAS")
	respRead, err := http.DefaultClient.Do(reqRead)
	if err != nil {
		t.Fatalf("GET Patient with X-TEFCA-Exchange-Purpose: %v", err)
	}
	respRead.Body.Close()
	if respRead.StatusCode != http.StatusOK || respRead.Header.Get("X-TEFCA-Exchange-Purpose") != "IAS" {
		t.Fatalf("expected 200 OK with X-TEFCA-Exchange-Purpose=IAS, got %d", respRead.StatusCode)
	}

	// 3. Reject invalid PurposeOfUse header with 400 Bad Request
	reqBadPOU, _ := http.NewRequest(http.MethodGet, baseURL+"/Patient/tefca-pat-1", nil)
	reqBadPOU.Header.Set("X-Purpose-Of-Use", "MARKETING-UNAUTHORIZED")
	respBadPOU, err := http.DefaultClient.Do(reqBadPOU)
	if err != nil {
		t.Fatalf("GET Patient with invalid X-Purpose-Of-Use: %v", err)
	}
	respBadPOU.Body.Close()
	if respBadPOU.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid X-Purpose-Of-Use, got %d", respBadPOU.StatusCode)
	}

	// 4. Search AuditEvents and verify purposeOfEvent & audit-sha256-digest
	respSearch, err := http.Get(baseURL + "/AuditEvent")
	if err != nil {
		t.Fatalf("GET /AuditEvent: %v", err)
	}
	bSearch, _ := io.ReadAll(respSearch.Body)
	respSearch.Body.Close()
	searchStr := string(bSearch)
	for _, want := range []string{"purposeOfEvent", "tefca-purpose-of-use", "audit-sha256-digest", "TREAT", "IAS"} {
		if !strings.Contains(searchStr, want) {
			t.Errorf("expected AuditEvent Bundle to contain %q, got:\n%s", want, searchStr)
		}
	}

	// 5. Verify tamper-evident SHA-256 hash chain via GET /AuditEvent/$verify-integrity
	respVer, err := http.Get(baseURL + "/AuditEvent/$verify-integrity")
	if err != nil {
		t.Fatalf("GET /AuditEvent/$verify-integrity: %v", err)
	}
	bVer, _ := io.ReadAll(respVer.Body)
	respVer.Body.Close()
	if respVer.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /AuditEvent/$verify-integrity, got %d: %s", respVer.StatusCode, string(bVer))
	}
	verStr := string(bVer)
	for _, want := range []string{`"name":"verified","valueBoolean":true`, `"SHA-256-HASH-CHAIN"`, `"chainHeadDigest"`} {
		if !strings.Contains(verStr, want) {
			t.Errorf("expected /AuditEvent/$verify-integrity output to contain %q, got:\n%s", want, verStr)
		}
	}
}
