package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"hegel.dev/go/hegel"

	mrpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/measure_report_go_proto"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/fhirxml"
	"github.com/flint-fhir/flint/pkg/validation"
)

func seedECQMPatientRecord(
	t testing.TB,
	client *http.Client,
	baseURL, tenant, patID, gender, birthDate, ombRaceCode, ombRaceDisp, ombEthCode, ombEthDisp, payerCode, payerDisp string,
	condCodes []string,
	hasEncounter bool,
	bpSys, bpDia float64,
	hasBP bool,
) {
	t.Helper()
	patJSON := fmt.Sprintf(`{
		"resourceType": "Patient",
		"id": %q,
		"gender": %q,
		"birthDate": %q,
		"name": [{"family": "Cohort", "given": [%q]}],
		"extension": [
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
				"extension": [
					{"url": "ombCategory", "valueCoding": {"system": "urn:oid:2.16.840.1.113883.6.238", "code": %q, "display": %q}},
					{"url": "text", "valueString": %q}
				]
			},
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
				"extension": [
					{"url": "ombCategory", "valueCoding": {"system": "urn:oid:2.16.840.1.113883.6.238", "code": %q, "display": %q}},
					{"url": "text", "valueString": %q}
				]
			}
		]
	}`, patID, gender, birthDate, patID, ombRaceCode, ombRaceDisp, ombRaceDisp, ombEthCode, ombEthDisp, ombEthDisp)
	putTestResource(t, client, baseURL, tenant, "Patient", patID, patJSON)

	covJSON := fmt.Sprintf(`{
		"resourceType": "Coverage",
		"id": "cov-%s",
		"status": "active",
		"type": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": %q, "display": %q}], "text": %q},
		"beneficiary": {"reference": "Patient/%s"},
		"payor": [{"display": %q}]
	}`, patID, payerCode, payerDisp, payerDisp, patID, payerDisp)
	putTestResource(t, client, baseURL, tenant, "Coverage", "cov-"+patID, covJSON)

	if hasEncounter {
		encJSON := fmt.Sprintf(`{
			"resourceType": "Encounter",
			"id": "enc-%s",
			"status": "finished",
			"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
			"subject": {"reference": "Patient/%s"},
			"period": {"start": "2026-03-10T09:00:00Z", "end": "2026-03-10T09:30:00Z"}
		}`, patID, patID)
		putTestResource(t, client, baseURL, tenant, "Encounter", "enc-"+patID, encJSON)
	}

	for idx, cCode := range condCodes {
		cid := fmt.Sprintf("cond-%s-%d", patID, idx+1)
		condJSON := fmt.Sprintf(`{
			"resourceType": "Condition",
			"id": %q,
			"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
			"code": {"coding": [{"system": "http://snomed.info/sct", "code": %q}]},
			"subject": {"reference": "Patient/%s"}
		}`, cid, cCode, patID)
		putTestResource(t, client, baseURL, tenant, "Condition", cid, condJSON)
	}

	if hasBP {
		obsJSON := fmt.Sprintf(`{
			"resourceType": "Observation",
			"id": "obs-bp-%s",
			"status": "final",
			"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
			"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure panel"}]},
			"subject": {"reference": "Patient/%s"},
			"effectiveDateTime": "2026-06-01T10:00:00Z",
			"component": [
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic blood pressure"}]},
					"valueQuantity": {"value": %v, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				},
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8462-4", "display": "Diastolic blood pressure"}]},
					"valueQuantity": {"value": %v, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				}
			]
		}`, patID, patID, bpSys, bpDia)
		putTestResource(t, client, baseURL, tenant, "Observation", "obs-bp-"+patID, obsJSON)
	}
}

func customHEDISMeasureJSON(measureID string) string {
	return fmt.Sprintf(`{
		"resourceType": "Measure",
		"id": %q,
		"url": "http://example.org/fhir/Measure/%s",
		"name": "HEDISCBPExpanded",
		"title": "HEDIS Controlling High Blood Pressure with Stratifiers & SDE",
		"status": "active",
		"scoring": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-scoring", "code": "proportion"}]},
		"improvementNotation": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-improvement-notation", "code": "increase"}]},
		"group": [
			{
				"id": "group-1",
				"population": [
					{
						"id": "init-pop",
						"code": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-population", "code": "initial-population"}]},
						"criteria": {"language": "text/fhirpath", "expression": "Encounter.exists() and Condition.where(code.coding.code = '59621000').exists()"}
					},
					{
						"id": "denom",
						"code": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-population", "code": "denominator"}]},
						"criteria": {"language": "text/fhirpath", "expression": "Encounter.exists()"}
					},
					{
						"id": "denom-excl",
						"code": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-population", "code": "denominator-exclusion"}]},
						"criteria": {"language": "text/fhirpath", "expression": "Condition.where(code.coding.code = '431855005').exists()"}
					},
					{
						"id": "denom-excp",
						"code": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-population", "code": "denominator-exception"}]},
						"criteria": {"language": "text/fhirpath", "expression": "Condition.where(code.coding.code = '713458007').exists()"}
					},
					{
						"id": "numer",
						"code": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-population", "code": "numerator"}]},
						"criteria": {"language": "text/fhirpath", "expression": "Observation.where(component.where(code.coding.code = '8480-6' and valueQuantity.value < 140).exists() and component.where(code.coding.code = '8462-4' and valueQuantity.value < 90).exists()).exists()"}
					}
				],
				"stratifier": [
					{
						"id": "strat-gender",
						"code": {"coding": [{"system": "http://hl7.org/fhir/administrative-gender", "code": "gender"}], "text": "Administrative Gender"},
						"criteria": {"language": "text/fhirpath", "expression": "Patient.gender"}
					},
					{
						"id": "strat-gender-age",
						"code": {"text": "Gender and Senior Age Band"},
						"component": [
							{
								"id": "comp-gender",
								"code": {"coding": [{"system": "http://hl7.org/fhir/administrative-gender", "code": "gender"}], "text": "Gender"},
								"criteria": {"language": "text/fhirpath", "expression": "Patient.gender"}
							},
							{
								"id": "comp-ageband",
								"code": {"coding": [{"system": "http://example.org/CodeSystem/age-band", "code": "age-band"}], "text": "Age Band"},
								"criteria": {"language": "text/fhirpath", "expression": "iif(Patient.birthDate <= @1961-12-31, '65+', '18-64')"}
							}
						]
					}
				]
			}
		],
		"supplementalData": [
			{
				"id": "sde-sex",
				"code": {"coding": [{"system": "http://loinc.org", "code": "76689-9", "display": "Sex assigned at birth"}], "text": "sde-sex"},
				"usage": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-data-usage", "code": "supplemental-data"}]}],
				"criteria": {"language": "text/fhirpath", "expression": "Patient.gender"}
			},
			{
				"id": "sde-race",
				"code": {"text": "sde-race"},
				"usage": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-data-usage", "code": "supplemental-data"}]}],
				"criteria": {"language": "text/fhirpath", "expression": "Patient.extension.where(url = 'http://hl7.org/fhir/us/core/StructureDefinition/us-core-race').extension.where(url = 'ombCategory').valueCoding"}
			},
			{
				"id": "sde-ethnicity",
				"code": {"text": "sde-ethnicity"},
				"usage": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-data-usage", "code": "supplemental-data"}]}],
				"criteria": {"language": "text/fhirpath", "expression": "Patient.extension.where(url = 'http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity').extension.where(url = 'ombCategory').valueCoding"}
			},
			{
				"id": "sde-payer",
				"code": {"text": "sde-payer"},
				"usage": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/measure-data-usage", "code": "supplemental-data"}]}],
				"criteria": {"language": "text/fhirpath", "expression": "Coverage.type"}
			}
		]
	}`, measureID, measureID)
}

func extractPopCounts(popsRaw any) map[string]int {
	out := make(map[string]int)
	arr, ok := popsRaw.([]any)
	if !ok {
		return out
	}
	for _, rawP := range arr {
		pm, ok := rawP.(map[string]any)
		if !ok {
			continue
		}
		cnt, _ := pm["count"].(float64)
		codeObj, _ := pm["code"].(map[string]any)
		if codings, ok := codeObj["coding"].([]any); ok && len(codings) > 0 {
			if cm, ok := codings[0].(map[string]any); ok {
				code, _ := cm["code"].(string)
				out[code] = int(cnt)
			}
		}
	}
	return out
}

func extractPopListRef(popsRaw any, targetCode string) string {
	arr, ok := popsRaw.([]any)
	if !ok {
		return ""
	}
	for _, rawP := range arr {
		pm, ok := rawP.(map[string]any)
		if !ok {
			continue
		}
		codeObj, _ := pm["code"].(map[string]any)
		if codings, ok := codeObj["coding"].([]any); ok && len(codings) > 0 {
			if cm, ok := codings[0].(map[string]any); ok && cm["code"] == targetCode {
				if sr, ok := pm["subjectResults"].(map[string]any); ok {
					ref, _ := sr["reference"].(string)
					return strings.TrimPrefix(ref, "#")
				}
			}
		}
	}
	return ""
}

func containedListsByID(mr map[string]any) map[string][]string {
	out := make(map[string][]string)
	contained, ok := mr["contained"].([]any)
	if !ok {
		return out
	}
	for _, rawC := range contained {
		cm, ok := rawC.(map[string]any)
		if !ok || cm["resourceType"] != "List" {
			continue
		}
		id, _ := cm["id"].(string)
		var refs []string
		if entries, ok := cm["entry"].([]any); ok {
			for _, rawE := range entries {
				if em, ok := rawE.(map[string]any); ok {
					if item, ok := em["item"].(map[string]any); ok {
						if ref, _ := item["reference"].(string); ref != "" {
							refs = append(refs, ref)
						}
					}
				}
			}
		}
		sort.Strings(refs)
		out[id] = refs
	}
	return out
}

func TestECQMEvaluateMeasure_StratifiersSDEAndReportModes(t *testing.T) {
	srv, ts := newSQLiteTrack4Server(t)
	client := ts.Client()
	tenant := "ecqm-track5"
	valEngine := validation.NewEngine(validation.DefaultOptions())

	measureID := "hedis-cbp-expanded"
	putTestResource(t, client, ts.URL, tenant, "Measure", measureID, customHEDISMeasureJSON(measureID))

	// Seed 6 clinically distinct patients:
	// p1: female, 1955 (65+), White, Not Hispanic, Medicare, HTN, controlled BP (122/76) -> IP=1, Denom=1, Num=1
	seedECQMPatientRecord(t, client, ts.URL, tenant, "p1", "female", "1955-04-10", "2106-3", "White", "2186-5", "Not Hispanic or Latino", "MCPOL", "Medicare", []string{"59621000"}, true, 122, 76, true)
	// p2: female, 1988 (18-64), Black, Not Hispanic, Commercial, HTN, uncontrolled BP (152/94) -> IP=1, Denom=1, Num=0
	seedECQMPatientRecord(t, client, ts.URL, tenant, "p2", "female", "1988-08-15", "2054-5", "Black or African American", "2186-5", "Not Hispanic or Latino", "HIP", "Commercial", []string{"59621000"}, true, 152, 94, true)
	// p3: male, 1952 (65+), Asian, Hispanic, Medicare, HTN + ESRD exclusion (431855005), uncontrolled -> IP=1, Denom=1, Excl=1, Num=0
	seedECQMPatientRecord(t, client, ts.URL, tenant, "p3", "male", "1952-01-20", "2028-9", "Asian", "2135-2", "Hispanic or Latino", "MCPOL", "Medicare", []string{"59621000", "431855005"}, true, 160, 98, true)
	// p4: male, 1958 (65+), White, Not Hispanic, Medicaid, HTN + frailty exception (713458007), uncontrolled -> IP=1, Denom=1, Excp=1, Num=0
	seedECQMPatientRecord(t, client, ts.URL, tenant, "p4", "male", "1958-05-03", "2106-3", "White", "2186-5", "Not Hispanic or Latino", "SUBSIDIZ", "Medicaid", []string{"59621000", "713458007"}, true, 148, 92, true)
	// p5: male, 1985 (18-64), White, Hispanic, Commercial, HTN + frailty exception (713458007) BUT controlled BP (118/74) -> IP=1, Denom=1, Num=1, Excp=0 (exception does NOT fire when Numerator is met!)
	seedECQMPatientRecord(t, client, ts.URL, tenant, "p5", "male", "1985-11-12", "2106-3", "White", "2135-2", "Hispanic or Latino", "HIP", "Commercial", []string{"59621000", "713458007"}, true, 118, 74, true)
	// p6: female, 1992 (18-64), Asian, Not Hispanic, Commercial, NO HTN -> IP=0, Denom=0, Num=0
	seedECQMPatientRecord(t, client, ts.URL, tenant, "p6", "female", "1992-02-28", "2028-9", "Asian", "2186-5", "Not Hispanic or Latino", "HIP", "Commercial", []string{"195967001"}, true, 115, 72, true)

	// Expected cohort math:
	// Initial Population: p1, p2, p3, p4, p5 -> 5
	// Denominator: 5
	// Denominator Exclusion: p3 -> 1
	// Denominator Exception: p4 -> 1 (p5 met numerator so not an exception!)
	// Numerator: p1, p5 -> 2
	// Effective Denominator = 5 - 1 - 1 = 3; MeasureScore = 2 / 3 = 0.6667

	// 1. Evaluate summary report
	evalURL := ts.URL + "/fhir/r4/" + tenant + "/Measure/" + measureID + "/$evaluate-measure?periodStart=2026-01-01&periodEnd=2026-12-31&reportType=summary"
	respSum, err := client.Get(evalURL)
	require.NoError(t, err)
	sumBytes, _ := io.ReadAll(respSum.Body)
	respSum.Body.Close()
	require.Equal(t, http.StatusOK, respSum.StatusCode, string(sumBytes))
	require.True(t, valEngine.ValidateJSON("MeasureReport", sumBytes).IsValid(), "MeasureReport failed FHIR R4 validation: %s", string(sumBytes))

	var sumMR map[string]any
	require.NoError(t, fhirUnmarshalJSON(sumBytes, &sumMR))
	assert.Equal(t, "summary", sumMR["type"])

	groups, _ := sumMR["group"].([]any)
	require.Len(t, groups, 1)
	g0 := groups[0].(map[string]any)
	gCounts := extractPopCounts(g0["population"])
	assert.Equal(t, 5, gCounts["initial-population"])
	assert.Equal(t, 5, gCounts["denominator"])
	assert.Equal(t, 1, gCounts["denominator-exclusion"])
	assert.Equal(t, 1, gCounts["denominator-exception"])
	assert.Equal(t, 2, gCounts["numerator"])
	assert.InDelta(t, 0.6667, g0["measureScore"].(map[string]any)["value"].(float64), 0.0001)

	// Verify Stratifier 1 (single-component gender) and Stratifier 2 (multi-component gender x age-band)
	strats, _ := g0["stratifier"].([]any)
	require.Len(t, strats, 2)

	stratGender := strats[0].(map[string]any)
	genderStrata, _ := stratGender["stratum"].([]any)
	require.Len(t, genderStrata, 2) // female (p1, p2, p6 -> IP=2, Denom=2, Num=1 -> score 0.5) & male (p3, p4, p5 -> IP=3, Denom=3, Excl=1, Excp=1, Num=1 -> score 1.0)
	for _, rawSt := range genderStrata {
		stm := rawSt.(map[string]any)
		assert.NotNil(t, stm["value"], "single-component stratum must populate value")
		assert.Nil(t, stm["component"], "single-component stratum must omit component")
		valTxt, _ := stm["value"].(map[string]any)["text"].(string)
		sCounts := extractPopCounts(stm["population"])
		sScore, _ := stm["measureScore"].(map[string]any)["value"].(float64)
		switch valTxt {
		case "female":
			assert.Equal(t, 2, sCounts["initial-population"])
			assert.Equal(t, 1, sCounts["numerator"])
			assert.InDelta(t, 0.5, sScore, 0.0001)
		case "male":
			assert.Equal(t, 3, sCounts["initial-population"])
			assert.Equal(t, 1, sCounts["denominator-exclusion"])
			assert.Equal(t, 1, sCounts["denominator-exception"])
			assert.Equal(t, 1, sCounts["numerator"])
			assert.InDelta(t, 1.0, sScore, 0.0001)
		default:
			t.Fatalf("unexpected gender stratum %q", valTxt)
		}
	}

	stratMulti := strats[1].(map[string]any)
	multiStrata, _ := stratMulti["stratum"].([]any)
	require.Len(t, multiStrata, 4) // (female, 18-64), (female, 65+), (male, 18-64), (male, 65+)
	for _, rawSt := range multiStrata {
		stm := rawSt.(map[string]any)
		assert.Nil(t, stm["value"], "multi-component stratum must omit top-level value")
		comps, _ := stm["component"].([]any)
		require.Len(t, comps, 2, "multi-component stratum must carry 2 components")
	}

	// Verify SDE Observations in MeasureReport.contained and referenced in MeasureReport.evaluatedResource
	contained, _ := sumMR["contained"].([]any)
	require.NotEmpty(t, contained)
	sdeTotals := make(map[string]int)
	for _, rawC := range contained {
		cm, ok := rawC.(map[string]any)
		if !ok || cm["resourceType"] != "Observation" {
			continue
		}
		var popID string
		if exts, ok := cm["extension"].([]any); ok {
			for _, rawE := range exts {
				em, _ := rawE.(map[string]any)
				if em["url"] == "http://hl7.org/fhir/StructureDefinition/cqf-measureInfo" {
					for _, subRaw := range em["extension"].([]any) {
						sub, _ := subRaw.(map[string]any)
						if sub["url"] == "populationId" {
							popID, _ = sub["valueString"].(string)
						}
					}
				}
			}
		}
		if comps, ok := cm["component"].([]any); ok && len(comps) > 0 {
			cnt, _ := comps[0].(map[string]any)["valueInteger"].(float64)
			sdeTotals[popID] += int(cnt)
		}
	}
	// All 5 Initial Population patients (p1..p5) have 1 sex, 1 race, 1 ethnicity, and 1 payer -> each SDE totals 5!
	assert.Equal(t, 5, sdeTotals["sde-sex"])
	assert.Equal(t, 5, sdeTotals["sde-race"])
	assert.Equal(t, 5, sdeTotals["sde-ethnicity"])
	assert.Equal(t, 5, sdeTotals["sde-payer"])

	evalResRefs, _ := sumMR["evaluatedResource"].([]any)
	require.GreaterOrEqual(t, len(evalResRefs), len(contained))

	// 2. Evaluate subject-list report across JSON, XML, and Proto3 wire formats!
	slURL := ts.URL + "/fhir/r4/" + tenant + "/Measure/" + measureID + "/$evaluate-measure?periodStart=2026-01-01&periodEnd=2026-12-31&reportType=subject-list"
	respSL, err := client.Get(slURL)
	require.NoError(t, err)
	slBytes, _ := io.ReadAll(respSL.Body)
	respSL.Body.Close()
	require.Equal(t, http.StatusOK, respSL.StatusCode)
	require.True(t, valEngine.ValidateJSON("MeasureReport", slBytes).IsValid(), "subject-list MeasureReport failed FHIR R4 validation")

	var slMR map[string]any
	require.NoError(t, fhirUnmarshalJSON(slBytes, &slMR))
	assert.Equal(t, "subject-list", slMR["type"])
	listsMap := containedListsByID(slMR)
	assert.Equal(t, []string{"Patient/p1", "Patient/p2", "Patient/p3", "Patient/p4", "Patient/p5"}, listsMap["initial-population-list"])
	assert.Equal(t, []string{"Patient/p3"}, listsMap["denominator-exclusion-list"])
	assert.Equal(t, []string{"Patient/p4"}, listsMap["denominator-exception-list"])
	assert.Equal(t, []string{"Patient/p1", "Patient/p5"}, listsMap["numerator-list"])

	// Verify stratum-level subjectResults Lists also resolve in contained!
	slG0 := slMR["group"].([]any)[0].(map[string]any)
	slGenderStrata := slG0["stratifier"].([]any)[0].(map[string]any)["stratum"].([]any)
	for _, rawSt := range slGenderStrata {
		stm := rawSt.(map[string]any)
		numListID := extractPopListRef(stm["population"], "numerator")
		require.NotEmpty(t, numListID)
		_, ok := listsMap[numListID]
		assert.True(t, ok, "expected stratum numerator subjectResults #%s in contained Lists", numListID)
	}

	// XML wire format on subject-list MeasureReport
	reqXML, _ := http.NewRequest(http.MethodGet, slURL, nil)
	reqXML.Header.Set("Accept", "application/fhir+xml")
	respXML, err := client.Do(reqXML)
	require.NoError(t, err)
	xmlBytes, _ := io.ReadAll(respXML.Body)
	respXML.Body.Close()
	require.Equal(t, http.StatusOK, respXML.StatusCode)
	assert.Contains(t, respXML.Header.Get("Content-Type"), "application/fhir+xml")
	xmlMap, err := fhirxml.UnmarshalFHIRMap(xmlBytes)
	require.NoError(t, err)
	assert.Equal(t, "MeasureReport", xmlMap["resourceType"])
	assert.Equal(t, "subject-list", xmlMap["type"])

	// Binary Proto3 wire format on subject-list MeasureReport
	reqProto, _ := http.NewRequest(http.MethodGet, slURL, nil)
	reqProto.Header.Set("Accept", "application/x-protobuf")
	respProto, err := client.Do(reqProto)
	require.NoError(t, err)
	protoBytes, _ := io.ReadAll(respProto.Body)
	respProto.Body.Close()
	require.Equal(t, http.StatusOK, respProto.StatusCode)
	var mrProto mrpb.MeasureReport
	require.NoError(t, proto.Unmarshal(protoBytes, &mrProto))
	require.Len(t, mrProto.GetGroup(), 1)
	require.Len(t, mrProto.GetGroup()[0].GetStratifier(), 2)
	assert.NotEmpty(t, mrProto.GetContained())
	assert.NotEmpty(t, mrProto.GetEvaluatedResource())

	// 3. Individual report mode (POST Parameters) + Group cohort filtering + Negative validation checks
	indivParams := fmt.Sprintf(`{
		"resourceType": "Parameters",
		"parameter": [
			{"name": "measure", "valueCanonical": "http://example.org/fhir/Measure/%s"},
			{"name": "subject", "valueString": "Patient/p1"},
			{"name": "reportType", "valueCode": "individual"},
			{"name": "periodStart", "valueDate": "2026-01-01"},
			{"name": "periodEnd", "valueDate": "2026-12-31"}
		]
	}`, measureID)
	respIndiv, err := client.Post(ts.URL+"/fhir/r4/"+tenant+"/Measure/$evaluate-measure", "application/fhir+json", strings.NewReader(indivParams))
	require.NoError(t, err)
	indivBytes, _ := io.ReadAll(respIndiv.Body)
	respIndiv.Body.Close()
	require.Equal(t, http.StatusOK, respIndiv.StatusCode)
	var indivMR map[string]any
	require.NoError(t, fhirUnmarshalJSON(indivBytes, &indivMR))
	assert.Equal(t, "individual", indivMR["type"])
	assert.Equal(t, "Patient/p1", indivMR["subject"].(map[string]any)["reference"])
	assert.Contains(t, string(indivBytes), `"Observation/obs-bp-p1"`)
	assert.Contains(t, string(indivBytes), `"obs-sde-sex-1"`)

	// Group/{id} subject subset evaluation (only p1 and p2 -> IP=2, Num=1 -> score 0.5)
	putTestResource(t, client, ts.URL, tenant, "Group", "grp-females", `{
		"resourceType": "Group",
		"id": "grp-females",
		"type": "person",
		"actual": true,
		"member": [
			{"entity": {"reference": "Patient/p1"}},
			{"entity": {"reference": "Patient/p2"}}
		]
	}`)
	respGrp, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/Measure/" + measureID + "/$evaluate-measure?subject=Group/grp-females&reportType=summary")
	require.NoError(t, err)
	grpBytes, _ := io.ReadAll(respGrp.Body)
	respGrp.Body.Close()
	require.Equal(t, http.StatusOK, respGrp.StatusCode)
	assert.Contains(t, string(grpBytes), `"Group/grp-females"`)
	var grpMR map[string]any
	require.NoError(t, fhirUnmarshalJSON(grpBytes, &grpMR))
	assert.InDelta(t, 0.5, grpMR["group"].([]any)[0].(map[string]any)["measureScore"].(map[string]any)["value"].(float64), 0.0001)

	// Negative: reportType=individual without subject -> 400 Bad Request
	respMissingSubj, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/Measure/" + measureID + "/$evaluate-measure?reportType=individual")
	require.NoError(t, err)
	respMissingSubj.Body.Close()
	assert.Equal(t, http.StatusBadRequest, respMissingSubj.StatusCode)

	// Negative: invalid reportType -> 400 Bad Request
	respBadType, err := client.Get(ts.URL + "/fhir/r4/" + tenant + "/Measure/" + measureID + "/$evaluate-measure?reportType=unknown-mode")
	require.NoError(t, err)
	respBadType.Body.Close()
	assert.Equal(t, http.StatusBadRequest, respBadType.StatusCode)

	// Patient-scoped SMART token blocked from Group evaluation -> 403 Forbidden
	mockSMART, err := auth.NewMockTokenValidator("", "")
	require.NoError(t, err)
	srv.SetTokenValidator(mockSMART)
	t.Cleanup(func() { srv.SetTokenValidator(nil) })
	patTok, err := mockSMART.IssueToken("pat-u", "patient/Measure.rs patient/Patient.rs", "p1", 600_000_000_000, nil)
	require.NoError(t, err)
	reqGrpPatTok, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/Measure/"+measureID+"/$evaluate-measure?subject=Group/grp-females", nil)
	reqGrpPatTok.Header.Set("Authorization", "Bearer "+patTok)
	respGrpPatTok, err := client.Do(reqGrpPatTok)
	require.NoError(t, err)
	respGrpPatTok.Body.Close()
	assert.Equal(t, http.StatusForbidden, respGrpPatTok.StatusCode)
}

func fhirUnmarshalJSON(b []byte, out any) error {
	return json.Unmarshal(b, out)
}

func TestProperty_ECQMStratifierPartitionAndSubjectListBijection(t *testing.T) {
	_, ts := newSQLiteTrack4Server(t)
	client := ts.Client()

	var seq atomic.Int64

	hegel.Test(t, func(ht *hegel.T) {
		n := seq.Add(1)
		tenant := fmt.Sprintf("ecqm-pbt-%d", n)
		measureID := "pbt-hedis-cbp"
		putTestResource(ht, client, ts.URL, tenant, "Measure", measureID, customHEDISMeasureJSON(measureID))

		cohortSize := hegel.Draw(ht, hegel.Integers(2, 7))
		for i := 0; i < cohortSize; i++ {
			pid := fmt.Sprintf("pt-%d", i+1)
			gender := hegel.Draw(ht, hegel.SampledFrom([]string{"female", "male", "other"}))
			dob := hegel.Draw(ht, hegel.SampledFrom([]string{"1950-06-15", "1960-11-01", "1978-03-22", "1995-09-10"}))
			race := hegel.Draw(ht, hegel.SampledFrom([][2]string{
				{"2106-3", "White"},
				{"2054-5", "Black or African American"},
				{"2028-9", "Asian"},
			}))
			payer := hegel.Draw(ht, hegel.SampledFrom([][2]string{
				{"MCPOL", "Medicare"},
				{"HIP", "Commercial"},
				{"SUBSIDIZ", "Medicaid"},
			}))
			hasEnc := hegel.Draw(ht, hegel.Booleans())
			hasHTN := hegel.Draw(ht, hegel.Booleans())
			hasExcl := hegel.Draw(ht, hegel.Booleans())
			hasExcp := hegel.Draw(ht, hegel.Booleans())
			bpControlled := hegel.Draw(ht, hegel.Booleans())

			var conds []string
			if hasHTN {
				conds = append(conds, "59621000")
			}
			if hasExcl {
				conds = append(conds, "431855005")
			}
			if hasExcp {
				conds = append(conds, "713458007")
			}
			sys, dia := 152.0, 95.0
			if bpControlled {
				sys, dia = 124.0, 78.0
			}
			seedECQMPatientRecord(ht, client, ts.URL, tenant, pid, gender, dob, race[0], race[1], "2186-5", "Not Hispanic or Latino", payer[0], payer[1], conds, hasEnc, sys, dia, true)
		}

		url := ts.URL + "/fhir/r4/" + tenant + "/Measure/" + measureID + "/$evaluate-measure?periodStart=2026-01-01&periodEnd=2026-12-31&reportType=subject-list"
		resp, err := client.Get(url)
		if err != nil {
			ht.Fatalf("GET $evaluate-measure: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			ht.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
		}

		var mr map[string]any
		if err := json.Unmarshal(body, &mr); err != nil {
			ht.Fatalf("unmarshal MeasureReport: %v", err)
		}

		lists := containedListsByID(mr)
		g0 := mr["group"].([]any)[0].(map[string]any)
		gCounts := extractPopCounts(g0["population"])
		gScore, _ := g0["measureScore"].(map[string]any)["value"].(float64)

		// Law 1: Proportion subset & bounded score invariants
		if gCounts["numerator"]+gCounts["denominator-exclusion"]+gCounts["denominator-exception"] > gCounts["denominator"] {
			ht.Fatalf("group subset violation: num(%d)+excl(%d)+excp(%d) > denom(%d)",
				gCounts["numerator"], gCounts["denominator-exclusion"], gCounts["denominator-exception"], gCounts["denominator"])
		}
		if gCounts["denominator"] > gCounts["initial-population"] {
			ht.Fatalf("denom(%d) > initial-population(%d)", gCounts["denominator"], gCounts["initial-population"])
		}
		if gScore < 0.0 || gScore > 1.0 {
			ht.Fatalf("score out of [0,1]: %v", gScore)
		}

		// Law 2 & Law 3: Exact stratifier count conservation & subject-list set bijection across BOTH stratifiers!
		popCodes := []string{"initial-population", "denominator", "denominator-exclusion", "denominator-exception", "numerator"}
		for _, popCode := range popCodes {
			groupListID := extractPopListRef(g0["population"], popCode)
			groupRefs := lists[groupListID]
			if len(groupRefs) != gCounts[popCode] {
				ht.Fatalf("group %s list len (%d) != count (%d)", popCode, len(groupRefs), gCounts[popCode])
			}

			for sIdx, rawStrat := range g0["stratifier"].([]any) {
				stratMap := rawStrat.(map[string]any)
				sumStratumCount := 0
				var unionRefs []string
				seenRef := make(map[string]bool)

				for _, rawSt := range stratMap["stratum"].([]any) {
					stm := rawSt.(map[string]any)
					sCounts := extractPopCounts(stm["population"])
					sumStratumCount += sCounts[popCode]

					stListID := extractPopListRef(stm["population"], popCode)
					stRefs := lists[stListID]
					if len(stRefs) != sCounts[popCode] {
						ht.Fatalf("stratifier[%d] %s stratum list len (%d) != count (%d)", sIdx, popCode, len(stRefs), sCounts[popCode])
					}
					for _, r := range stRefs {
						if seenRef[r] {
							ht.Fatalf("stratifier[%d] %s duplicate patient %s across strata", sIdx, popCode, r)
						}
						seenRef[r] = true
						unionRefs = append(unionRefs, r)
					}
				}
				sort.Strings(unionRefs)
				if sumStratumCount != gCounts[popCode] {
					ht.Fatalf("stratifier[%d] %s count sum (%d) != group count (%d)", sIdx, popCode, sumStratumCount, gCounts[popCode])
				}
				if strings.Join(unionRefs, ",") != strings.Join(groupRefs, ",") {
					ht.Fatalf("stratifier[%d] %s subject-list union %v != group list %v", sIdx, popCode, unionRefs, groupRefs)
				}
			}
		}

		// Law 4: SDE count conservation (each Initial Population patient contributes 1 sex & 1 race)
		sdeSexSum := 0
		if contained, ok := mr["contained"].([]any); ok {
			for _, rawC := range contained {
				cm, ok := rawC.(map[string]any)
				if !ok || cm["resourceType"] != "Observation" {
					continue
				}
				id, _ := cm["id"].(string)
				if strings.HasPrefix(id, "obs-sde-sex-") {
					if comps, ok := cm["component"].([]any); ok && len(comps) > 0 {
						cnt, _ := comps[0].(map[string]any)["valueInteger"].(float64)
						sdeSexSum += int(cnt)
					}
				}
			}
		}
		if sdeSexSum != gCounts["initial-population"] {
			ht.Fatalf("sde-sex count sum (%d) != initial-population (%d)", sdeSexSum, gCounts["initial-population"])
		}
	}, hegel.WithTestCases(35))
}
