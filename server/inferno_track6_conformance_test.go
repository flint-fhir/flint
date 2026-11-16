package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/validation"
)

func TestTrack6_IPSPlanNetAndUSDrugFormularyConformance(t *testing.T) {
	_, ts := newSQLiteTrack4Server(t)
	client := ts.Client()
	valEngine := validation.NewEngine(validation.DefaultOptions())

	// 1. CapabilityStatement advertisement checks for IPS, Plan-Net, and US Drug Formulary
	respMeta, err := client.Get(ts.URL + "/fhir/r4/demo/metadata")
	require.NoError(t, err)
	metaBytes, _ := io.ReadAll(respMeta.Body)
	respMeta.Body.Close()
	require.Equal(t, http.StatusOK, respMeta.StatusCode)
	metaStr := string(metaBytes)
	for _, want := range []string{
		"http://hl7.org/fhir/uv/ips/CapabilityStatement/ips-server",
		"http://hl7.org/fhir/us/davinci-pdex-plan-net/CapabilityStatement/plan-net",
		"http://hl7.org/fhir/us/davinci-drug-formulary/CapabilityStatement/usdf-server",
		"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Endpoint",
		"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-HealthcareService",
		"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-InsurancePlan",
		"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Location",
		"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Network",
		"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Organization",
		"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-OrganizationAffiliation",
		"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Practitioner",
		"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-PractitionerRole",
		"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-PayerInsurancePlan",
		"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-Formulary",
		"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-FormularyItem",
		"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-FormularyDrug",
		"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-InsurancePlanLocation",
	} {
		assert.Contains(t, metaStr, want)
	}

	// 2. HL7 IPS v1.1.0 Patient/{id}/$summary document Bundle conformance
	ipsTenant := "ips-track6"
	putTestResource(t, client, ts.URL, ipsTenant, "Organization", "org-ips-1", `{"resourceType":"Organization","id":"org-ips-1","active":true,"name":"IPS General Hospital"}`)
	putTestResource(t, client, ts.URL, ipsTenant, "Medication", "med-ips-1", `{"resourceType":"Medication","id":"med-ips-1","code":{"coding":[{"system":"http://www.nlm.nih.gov/research/umls/rxnorm","code":"314076","display":"lisinopril 10 MG Oral Tablet"}]}}`)
	putTestResource(t, client, ts.URL, ipsTenant, "Patient", "pat-ips-1", `{"resourceType":"Patient","id":"pat-ips-1","active":true,"name":[{"family":"Summary","given":["Alice"]}],"gender":"female","birthDate":"1980-04-12","managingOrganization":{"reference":"Organization/org-ips-1"}}`)
	putTestResource(t, client, ts.URL, ipsTenant, "MedicationStatement", "ms-ips-1", `{"resourceType":"MedicationStatement","id":"ms-ips-1","status":"active","medicationReference":{"reference":"Medication/med-ips-1"},"subject":{"reference":"Patient/pat-ips-1"},"effectiveDateTime":"2026-01-10T08:00:00Z"}`)
	putTestResource(t, client, ts.URL, ipsTenant, "AllergyIntolerance", "all-ips-1", `{"resourceType":"AllergyIntolerance","id":"all-ips-1","clinicalStatus":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical","code":"active"}]},"code":{"coding":[{"system":"http://www.nlm.nih.gov/research/umls/rxnorm","code":"7980","display":"Penicillin G"}]},"patient":{"reference":"Patient/pat-ips-1"}}`)
	// Seed one problem-list Condition AND one DAR encounter-diagnosis Condition -> IPS $summary must include ONLY the problem-list Condition!
	putTestResource(t, client, ts.URL, ipsTenant, "Condition", "cond-ips-prob", `{"resourceType":"Condition","id":"cond-ips-prob","clinicalStatus":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/condition-clinical","code":"active"}]},"category":[{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/condition-category","code":"problem-list-item"}]}],"code":{"coding":[{"system":"http://snomed.info/sct","code":"59621000","display":"Essential hypertension"}]},"subject":{"reference":"Patient/pat-ips-1"}}`)
	putTestResource(t, client, ts.URL, ipsTenant, "Condition", "cond-ips-enc-dar", `{"resourceType":"Condition","id":"cond-ips-enc-dar","_clinicalStatus":{"extension":[{"url":"http://hl7.org/fhir/StructureDefinition/data-absent-reason","valueCode":"unknown"}]},"category":[{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/condition-category","code":"encounter-diagnosis"}]}],"code":{"coding":[{"system":"http://snomed.info/sct","code":"44054006"}]},"subject":{"reference":"Patient/pat-ips-1"}}`)

	respIPS, err := client.Get(ts.URL + "/fhir/r4/" + ipsTenant + "/Patient/pat-ips-1/$summary")
	require.NoError(t, err)
	ipsBytes, _ := io.ReadAll(respIPS.Body)
	respIPS.Body.Close()
	require.Equal(t, http.StatusOK, respIPS.StatusCode, string(ipsBytes))

	ipsOut := valEngine.ValidateJSONWithProfiles("Bundle", ipsBytes, []string{"http://hl7.org/fhir/uv/ips/StructureDefinition/Bundle-uv-ips"}, "")
	assert.True(t, ipsOut.IsValid(), "IPS Bundle-uv-ips validation failed: %+v", ipsOut.Issues)

	var ipsBundle map[string]any
	require.NoError(t, json.Unmarshal(ipsBytes, &ipsBundle))
	identVal, _ := ipsBundle["identifier"].(map[string]any)["value"].(string)
	assert.Regexp(t, regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`), identVal)

	ipsEntries, _ := ipsBundle["entry"].([]any)
	require.NotEmpty(t, ipsEntries)
	entrySet := make(map[string]bool)
	for _, rawE := range ipsEntries {
		em := rawE.(map[string]any)
		fullURL, _ := em["fullUrl"].(string)
		assert.True(t, strings.HasPrefix(fullURL, ts.URL+"/fhir/r4/"+ipsTenant+"/"), "expected absolute fullUrl, got %q", fullURL)
		rm := em["resource"].(map[string]any)
		rt, _ := rm["resourceType"].(string)
		id, _ := rm["id"].(string)
		entrySet[rt+"/"+id] = true
	}
	assert.True(t, entrySet["Composition/ips-pat-ips-1"])
	assert.True(t, entrySet["Patient/pat-ips-1"])
	assert.True(t, entrySet["MedicationStatement/ms-ips-1"])
	assert.True(t, entrySet["Medication/med-ips-1"], "1-hop Medication target must be resolved into document Bundle")
	assert.True(t, entrySet["Organization/org-ips-1"], "1-hop Organization target must be resolved into document Bundle")
	assert.True(t, entrySet["Condition/cond-ips-prob"])
	assert.False(t, entrySet["Condition/cond-ips-enc-dar"], "DAR encounter-diagnosis Condition must be excluded from IPS Bundle")

	// 3. Da Vinci Plan-Net v1.1.0 search, forward chains, _has reverse chains, _include, and _revinclude
	pnTenant := "plannet-track6"
	putTestResource(t, client, ts.URL, pnTenant, "Endpoint", "ep-1", `{"resourceType":"Endpoint","id":"ep-1","status":"active","connectionType":{"system":"http://terminology.hl7.org/CodeSystem/endpoint-connection-type","code":"hl7-fhir-rest"},"name":"Dir EP","managingOrganization":{"reference":"Organization/org-child"},"payloadType":[{"coding":[{"code":"NA"}]}],"address":"https://fhir.example.org"}`)
	putTestResource(t, client, ts.URL, pnTenant, "Organization", "org-parent", `{"resourceType":"Organization","id":"org-parent","active":true,"type":[{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/OrgTypeCS","code":"payer"}]}],"name":"Acme Parent Payer","address":[{"city":"Boston","state":"MA","postalCode":"02110"}],"endpoint":[{"reference":"Endpoint/ep-1"}]}`)
	putTestResource(t, client, ts.URL, pnTenant, "Organization", "org-child", `{"resourceType":"Organization","id":"org-child","extension":[{"url":"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/location-reference","valueReference":{"reference":"Location/loc-child"}}],"active":true,"type":[{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/OrgTypeCS","code":"fac"}]}],"name":"Acme Child Clinic","partOf":{"reference":"Organization/org-parent"},"address":[{"line":["250 CambridgePark Dr"],"city":"Cambridge","state":"MA","postalCode":"02140"}],"endpoint":[{"reference":"Endpoint/ep-1"}]}`)
	putTestResource(t, client, ts.URL, pnTenant, "Organization", "org-net", `{"resourceType":"Organization","id":"org-net","extension":[{"url":"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/location-reference","valueReference":{"reference":"Location/loc-parent"}}],"active":true,"type":[{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/OrgTypeCS","code":"ntwk"}]}],"name":"Acme PPO Network","partOf":{"reference":"Organization/org-parent"},"endpoint":[{"reference":"Endpoint/ep-1"}]}`)
	putTestResource(t, client, ts.URL, pnTenant, "Location", "loc-parent", `{"resourceType":"Location","id":"loc-parent","status":"active","name":"Parent Campus","type":[{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v3-RoleCode","code":"HOSP"}]}],"address":{"line":["100 Main St"],"city":"Boston","state":"MA","postalCode":"02110"},"managingOrganization":{"reference":"Organization/org-child"},"endpoint":[{"reference":"Endpoint/ep-1"}]}`)
	putTestResource(t, client, ts.URL, pnTenant, "Location", "loc-child", `{"resourceType":"Location","id":"loc-child","status":"active","name":"Child Cardio Suite","type":[{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v3-RoleCode","code":"CARD"}]}],"address":{"line":["250 CambridgePark Dr"],"city":"Cambridge","state":"MA","postalCode":"02140"},"managingOrganization":{"reference":"Organization/org-child"},"partOf":{"reference":"Location/loc-parent"},"endpoint":[{"reference":"Endpoint/ep-1"}]}`)
	putTestResource(t, client, ts.URL, pnTenant, "HealthcareService", "svc-1", `{"resourceType":"HealthcareService","id":"svc-1","active":true,"providedBy":{"reference":"Organization/org-child"},"category":[{"coding":[{"code":"outpat"}]}],"type":[{"coding":[{"code":"394579002"}]}],"specialty":[{"coding":[{"system":"http://nucc.org/provider-taxonomy","code":"207RC0000X"}]}],"location":[{"reference":"Location/loc-child"}],"coverageArea":[{"reference":"Location/loc-parent"}],"name":"Cardiology Outpatient","endpoint":[{"reference":"Endpoint/ep-1"}]}`)
	putTestResource(t, client, ts.URL, pnTenant, "Practitioner", "prac-1", `{"resourceType":"Practitioner","id":"prac-1","active":true,"name":[{"family":"Chen","given":["Clara"],"prefix":["Dr."],"suffix":["MD"]}]}`)
	putTestResource(t, client, ts.URL, pnTenant, "PractitionerRole", "role-1", `{"resourceType":"PractitionerRole","id":"role-1","extension":[{"url":"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/network-reference","valueReference":{"reference":"Organization/org-net"}}],"active":true,"practitioner":{"reference":"Practitioner/prac-1"},"organization":{"reference":"Organization/org-child"},"code":[{"coding":[{"code":"ph"}]}],"specialty":[{"coding":[{"system":"http://nucc.org/provider-taxonomy","code":"207RC0000X"}]}],"location":[{"reference":"Location/loc-child"}],"healthcareService":[{"reference":"HealthcareService/svc-1"}],"endpoint":[{"reference":"Endpoint/ep-1"}]}`)
	putTestResource(t, client, ts.URL, pnTenant, "OrganizationAffiliation", "affil-1", `{"resourceType":"OrganizationAffiliation","id":"affil-1","active":true,"organization":{"reference":"Organization/org-child"},"participatingOrganization":{"reference":"Organization/org-child"},"network":[{"reference":"Organization/org-net"}],"code":[{"coding":[{"code":"provider"}]}],"specialty":[{"coding":[{"system":"http://nucc.org/provider-taxonomy","code":"207RC0000X"}]}],"location":[{"reference":"Location/loc-child"}],"healthcareService":[{"reference":"HealthcareService/svc-1"}],"endpoint":[{"reference":"Endpoint/ep-1"}]}`)
	putTestResource(t, client, ts.URL, pnTenant, "InsurancePlan", "plan-1", `{"resourceType":"InsurancePlan","id":"plan-1","status":"active","type":[{"coding":[{"code":"commppo"}]}],"name":"Acme Gold PPO","ownedBy":{"reference":"Organization/org-child"},"administeredBy":{"reference":"Organization/org-child"},"coverageArea":[{"reference":"Location/loc-child"}],"endpoint":[{"reference":"Endpoint/ep-1"}],"network":[{"reference":"Organization/org-net"}],"plan":[{"type":{"coding":[{"code":"gold"}]}}]}`)

	assertSearchAtLeastOne := func(tenant, query string) map[string]any {
		t.Helper()
		resp, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/" + query)
		require.NoError(t, err, query)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "query %s failed: %s", query, string(b))
		var bundle map[string]any
		require.NoError(t, json.Unmarshal(b, &bundle))
		entries, _ := bundle["entry"].([]any)
		require.NotEmpty(t, entries, "expected >=1 search match for %s, got 0: %s", query, string(b))
		return bundle
	}

	for _, q := range []string{
		"Location?partof.type=HOSP",
		"Location?partof.address=Boston",
		"Location?partof.organization=Organization/org-child",
		"Location?endpoint=Endpoint/ep-1&_include=Location:endpoint",
		"Location?_has:InsurancePlan:coverage-area:owned-by=Organization/org-child",
		"Organization?coverage-area=Location/loc-child&_include=Organization:coverage-area",
		"Organization?_has:OrganizationAffiliation:participating-organization:network=Organization/org-net",
		"Organization?_has:InsurancePlan:owned-by:coverage-area=Location/loc-child",
		"Practitioner?name=Dr.",
		"Practitioner?_has:PractitionerRole:practitioner:network=Organization/org-net&_revinclude=PractitionerRole:practitioner",
		"PractitionerRole?network=Organization/org-net&role=ph&service=HealthcareService/svc-1&_include=PractitionerRole:network",
		"PractitionerRole?service.service-category=outpat",
		"OrganizationAffiliation?primary-organization=Organization/org-child&role=provider&service=HealthcareService/svc-1",
		"OrganizationAffiliation?primary-organization.partof=Organization/org-parent",
		"HealthcareService?organization=Organization/org-child&service-category=outpat&service-type=394579002",
		"HealthcareService?specialty=207RC0000X&_has:OrganizationAffiliation:service:network=Organization/org-net",
		"InsurancePlan?plan-type=gold&owned-by.partof=Organization/org-parent&administered-by.name=Acme&_include=InsurancePlan:owned-by",
	} {
		assertSearchAtLeastOne(pnTenant, q)
	}

	// 4. Da Vinci US Drug Formulary v2.0.1 search & _include conformance
	usdfTenant := "formulary-track6"
	putTestResource(t, client, ts.URL, usdfTenant, "Location", "loc-usdf-001", `{"resourceType":"Location","id":"loc-usdf-001","status":"active","name":"MA Area","address":{"line":["100 State St"],"city":"Boston","state":"MA","postalCode":"02109"}}`)
	putTestResource(t, client, ts.URL, usdfTenant, "InsurancePlan", "form-1", `{"resourceType":"InsurancePlan","id":"form-1","identifier":[{"system":"http://example.org/form","value":"F-1"}],"status":"active","type":[{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v3-ActCode","code":"DRUGPOL"}]}],"name":"2026 Drug Formulary","period":{"start":"2026-01-01","end":"2026-12-31"},"coverageArea":[{"reference":"Location/loc-usdf-001"}]}`)
	putTestResource(t, client, ts.URL, usdfTenant, "InsurancePlan", "payer-1", `{"resourceType":"InsurancePlan","id":"payer-1","identifier":[{"system":"http://example.org/plan","value":"P-1"}],"status":"active","type":[{"coding":[{"code":"commppo"}]}],"name":"Commercial PPO","period":{"start":"2026-01-01","end":"2026-12-31"},"coverageArea":[{"reference":"Location/loc-usdf-001"}],"coverage":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-FormularyReference-extension","valueReference":{"reference":"InsurancePlan/form-1"}}],"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v3-ActCode","code":"DRUGPOL"}]},"benefit":[{"type":{"coding":[{"code":"drug"}]}}]}],"plan":[{"type":{"coding":[{"code":"drug"}]}}]}`)
	putTestResource(t, client, ts.URL, usdfTenant, "MedicationKnowledge", "mk-1", `{"resourceType":"MedicationKnowledge","id":"mk-1","status":"active","code":{"coding":[{"system":"http://www.nlm.nih.gov/research/umls/rxnorm","code":"314076","display":"lisinopril 10 MG Oral Tablet"}]},"doseForm":{"coding":[{"system":"http://www.nlm.nih.gov/research/umls/rxnorm","code":"317541","display":"Oral Tablet"}]},"synonym":["Prinivil 10mg"]}`)
	putTestResource(t, client, ts.URL, usdfTenant, "Basic", "item-1", `{"resourceType":"Basic","id":"item-1","extension":[{"url":"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-FormularyReference-extension","valueReference":{"reference":"InsurancePlan/form-1"}},{"url":"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-AvailabilityStatus-extension","valueCode":"active"},{"url":"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-AvailabilityPeriod-extension","valuePeriod":{"start":"2026-01-01","end":"2026-12-31"}},{"url":"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-PharmacyBenefitType-extension","valueCodeableConcept":{"coding":[{"code":"1-month-in-retail"}]}},{"url":"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-DrugTierID-extension","valueCodeableConcept":{"coding":[{"code":"generic"}]}}],"code":{"coding":[{"code":"formulary-item"}]},"subject":{"reference":"MedicationKnowledge/mk-1"}}`)

	for _, q := range []string{
		"InsurancePlan?coverage-type=DRUGPOL",
		"InsurancePlan?formulary-coverage=InsurancePlan/form-1&_include=InsurancePlan:formulary-coverage",
		"InsurancePlan?type=commppo",
		"MedicationKnowledge?drug-name=lisinopril",
		"MedicationKnowledge?drug-name=Prinivil",
		"MedicationKnowledge?doseform=317541",
		"Basic?code=formulary-item&status=active&period=gt2025-12-31T00:00:00Z&formulary=InsurancePlan/form-1&pharmacy-benefit-type=1-month-in-retail&drug-tier=generic&_include=Basic:subject&_include=Basic:formulary",
		"Basic?period=2026",
	} {
		b := assertSearchAtLeastOne(usdfTenant, q)
		if strings.Contains(q, "InsurancePlan?type=commppo") {
			// Verify nested benefit/cost types did NOT pollute top-level InsurancePlan?type=
			assert.Len(t, b["entry"].([]any), 1)
		}
	}

	// Verify POST /$validate against USDF & Plan-Net profiles
	valReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+usdfTenant+"/Basic/$validate?profile=http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-FormularyItem", bytes.NewReader([]byte(`{"resourceType":"Basic","code":{"coding":[{"code":"formulary-item"}]},"subject":{"reference":"MedicationKnowledge/mk-1"},"extension":[{"url":"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-FormularyReference-extension","valueReference":{"reference":"InsurancePlan/form-1"}}]}`)))
	valReq.Header.Set("Content-Type", "application/fhir+json")
	valResp, err := client.Do(valReq)
	require.NoError(t, err)
	valResp.Body.Close()
	assert.Equal(t, http.StatusOK, valResp.StatusCode)
}
