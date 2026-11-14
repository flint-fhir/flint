package fhirxml_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/fhirxml"
)

func TestPatientFullRoundTripAndElementOrdering(t *testing.T) {
	rawJSON := []byte(`{
		"gender": "female",
		"birthDate": "1974-12-25",
		"_birthDate": {
			"id": "bd-1",
			"extension": [
				{
					"url": "http://hl7.org/fhir/StructureDefinition/patient-birthTime",
					"valueDateTime": "1974-12-25T14:35:45-05:00"
				}
			]
		},
		"_maritalStatus": {
			"id": "ms-unused"
		},
		"deceasedBoolean": false,
		"active": true,
		"name": [
			{
				"id": "name-1",
				"use": "official",
				"family": "Chalmers",
				"given": ["Peter", null, "James"],
				"_given": [
					null,
					{
						"extension": [
							{
								"url": "http://hl7.org/fhir/StructureDefinition/data-absent-reason",
								"valueCode": "masked"
							}
						]
					},
					{
						"id": "g-3"
					}
				]
			}
		],
		"extension": [
			{
				"id": "ext-race",
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
				"extension": [
					{
						"url": "ombCategory",
						"valueCoding": {
							"system": "urn:oid:2.16.840.1.113883.6.238",
							"code": "2106-3",
							"display": "White"
						}
					},
					{
						"url": "text",
						"valueString": "White"
					}
				]
			}
		],
		"modifierExtension": [
			{
				"url": "http://example.org/fhir/StructureDefinition/do-not-contact",
				"valueBoolean": true
			}
		],
		"contained": [
			{
				"resourceType": "Organization",
				"id": "org-inline-1",
				"active": true,
				"name": "Good Health Clinic"
			}
		],
		"managingOrganization": {
			"reference": "#org-inline-1",
			"type": "Organization",
			"identifier": {
				"system": "http://hl7.org/fhir/sid/us-npi",
				"value": "1234567890"
			},
			"display": "Good Health Clinic"
		},
		"text": {
			"status": "generated",
			"div": "<div xmlns=\"http://www.w3.org/1999/xhtml\"><p>Patient <b>Peter Chalmers</b> &amp; family</p></div>"
		},
		"meta": {
			"versionId": "3",
			"lastUpdated": "2026-01-15T10:20:30Z",
			"profile": ["http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"]
		},
		"id": "pat-xml-1",
		"resourceType": "Patient"
	}`)

	xmlBytes, err := fhirxml.MarshalFHIRJSON(rawJSON)
	require.NoError(t, err)
	xmlStr := string(xmlBytes)

	// Verify canonical HL7 FHIR R4 StructureDefinition element ordering:
	// id -> meta -> text -> contained -> extension -> modifierExtension -> active -> name -> gender -> birthDate -> deceasedBoolean -> managingOrganization
	idxID := strings.Index(xmlStr, `<id value="pat-xml-1"/>`)
	idxMeta := strings.Index(xmlStr, `<meta>`)
	idxText := strings.Index(xmlStr, `<text>`)
	idxContained := strings.Index(xmlStr, `<contained><Organization><id value="org-inline-1"/><active value="true"/><name value="Good Health Clinic"/></Organization></contained>`)
	idxExt := strings.Index(xmlStr, `<extension id="ext-race" url="http://hl7.org/fhir/us/core/StructureDefinition/us-core-race">`)
	idxModExt := strings.Index(xmlStr, `<modifierExtension url="http://example.org/fhir/StructureDefinition/do-not-contact">`)
	idxActive := strings.LastIndex(xmlStr, `<active value="true"/>`)
	idxName := strings.Index(xmlStr, `<name id="name-1">`)
	idxGender := strings.Index(xmlStr, `<gender value="female"/>`)
	idxBirth := strings.Index(xmlStr, `<birthDate id="bd-1" value="1974-12-25">`)
	idxDeceased := strings.Index(xmlStr, `<deceasedBoolean value="false"/>`)
	idxOrg := strings.Index(xmlStr, `<managingOrganization>`)

	require.Greater(t, idxID, 0)
	assert.Less(t, idxID, idxMeta)
	assert.Less(t, idxMeta, idxText)
	assert.Less(t, idxText, idxContained)
	assert.Less(t, idxContained, idxExt)
	assert.Less(t, idxExt, idxModExt)
	assert.Less(t, idxModExt, idxActive)
	assert.Less(t, idxActive, idxName)
	assert.Less(t, idxName, idxGender)
	assert.Less(t, idxGender, idxBirth)
	assert.Less(t, idxBirth, idxDeceased)
	assert.Less(t, idxDeceased, idxOrg)
	assert.Contains(t, xmlStr, `<div xmlns="http://www.w3.org/1999/xhtml"><p>Patient <b>Peter Chalmers</b> &amp; family</p></div>`)

	// Unmarshal back to FHIR JSON and FHIR Map
	roundTripJSON, err := fhirxml.UnmarshalFHIRJSON(xmlBytes)
	require.NoError(t, err)

	patMap, err := fhirxml.UnmarshalFHIRMap(xmlBytes)
	require.NoError(t, err)
	assert.Equal(t, "Patient", patMap["resourceType"])
	assert.Equal(t, "pat-xml-1", patMap["id"])
	assert.Equal(t, true, patMap["active"])
	assert.Equal(t, false, patMap["deceasedBoolean"])
	assert.Equal(t, "female", patMap["gender"])

	names, ok := patMap["name"].([]any)
	require.True(t, ok)
	require.Len(t, names, 1)
	name0 := names[0].(map[string]any)
	assert.Equal(t, "name-1", name0["id"])
	assert.Equal(t, []any{"Peter", nil, "James"}, name0["given"])
	givenExt := name0["_given"].([]any)
	require.Len(t, givenExt, 3)
	assert.Nil(t, givenExt[0])
	assert.NotNil(t, givenExt[1])
	assert.Equal(t, map[string]any{"id": "g-3"}, givenExt[2])

	// Verify roundTripJSON normalizes cleanly into google/fhir Patient proto!
	patDesc := fhirutil.AllR4ResourceDescriptors()["Patient"]
	_, err = fhirutil.NormalizeFHIRJSONToProtoJSON(patDesc, roundTripJSON)
	require.NoError(t, err)
}

func TestObservationDecimalPrecisionEncounterClassBundleAndParameters(t *testing.T) {
	t.Run("Observation preserves exact trailing decimal zeroes and components", func(t *testing.T) {
		obsJSON := []byte(`{
			"resourceType": "Observation",
			"id": "obs-temp-1",
			"status": "final",
			"code": {
				"coding": [{"system": "http://loinc.org", "code": "8310-5", "display": "Body temperature"}]
			},
			"subject": {"reference": "Patient/pat-1"},
			"effectiveDateTime": "2026-03-01T09:00:00Z",
			"valueQuantity": {
				"value": 98.60,
				"unit": "degF",
				"system": "http://unitsofmeasure.org",
				"code": "[degF]"
			},
			"component": [
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
					"valueQuantity": {"value": 120, "unit": "mmHg"}
				}
			]
		}`)

		xmlBytes, err := fhirxml.MarshalFHIRJSON(obsJSON)
		require.NoError(t, err)
		assert.Contains(t, string(xmlBytes), `<value value="98.60"/>`)

		rtJSON, err := fhirxml.UnmarshalFHIRJSON(xmlBytes)
		require.NoError(t, err)
		assert.Contains(t, string(rtJSON), `"value":98.60`)

		rtMap, err := fhirxml.UnmarshalFHIRMap(xmlBytes)
		require.NoError(t, err)
		vq := rtMap["valueQuantity"].(map[string]any)
		assert.InDelta(t, 98.6, vq["value"].(float64), 1e-9)
	})

	t.Run("Encounter serializes class as <class> rather than <classValue>", func(t *testing.T) {
		encJSON := []byte(`{
			"resourceType": "Encounter",
			"id": "enc-1",
			"status": "in-progress",
			"class": {
				"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode",
				"code": "IMP",
				"display": "inpatient encounter"
			},
			"subject": {"reference": "Patient/pat-1"}
		}`)

		xmlBytes, err := fhirxml.MarshalFHIRJSON(encJSON)
		require.NoError(t, err)
		assert.Contains(t, string(xmlBytes), `<class><system value="http://terminology.hl7.org/CodeSystem/v3-ActCode"/><code value="IMP"/><display value="inpatient encounter"/></class>`)
		assert.NotContains(t, string(xmlBytes), `classValue`)

		encMap, err := fhirxml.UnmarshalFHIRMap(xmlBytes)
		require.NoError(t, err)
		cls, ok := encMap["class"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "IMP", cls["code"])
	})

	t.Run("Bundle with inline entry.resource and entry.response.outcome plus Go typed slices", func(t *testing.T) {
		bundleMap := map[string]any{
			"resourceType": "Bundle",
			"id":           "bundle-1",
			"type":         "transaction-response",
			"total":        1,
			"link": []map[string]string{
				{"relation": "self", "url": "http://example.org/fhir/r4/default/Patient"},
			},
			"entry": []map[string]any{
				{
					"fullUrl": "Patient/pat-1",
					"resource": map[string]any{
						"resourceType": "Patient",
						"id":           "pat-1",
						"gender":       "male",
					},
					"response": map[string]any{
						"status": "201 Created",
						"etag":   `W/"1"`,
						"outcome": map[string]any{
							"resourceType": "OperationOutcome",
							"issue": []map[string]any{
								{
									"severity":    "information",
									"code":        "informational",
									"diagnostics": "Created Patient/pat-1",
								},
							},
						},
					},
				},
			},
		}

		xmlBytes, err := fhirxml.MarshalFHIRMap(bundleMap)
		require.NoError(t, err)
		xmlStr := string(xmlBytes)
		assert.Contains(t, xmlStr, `<resource><Patient><id value="pat-1"/><gender value="male"/></Patient></resource>`)
		assert.Contains(t, xmlStr, `<outcome><OperationOutcome><issue><severity value="information"/>`)

		decoded, err := fhirxml.UnmarshalFHIRMap(xmlBytes)
		require.NoError(t, err)
		assert.Equal(t, float64(1), decoded["total"])
		entries := decoded["entry"].([]any)
		require.Len(t, entries, 1)
		entry0 := entries[0].(map[string]any)
		res0 := entry0["resource"].(map[string]any)
		assert.Equal(t, "Patient", res0["resourceType"])
		assert.Equal(t, "pat-1", res0["id"])
		resp0 := entry0["response"].(map[string]any)
		out0 := resp0["outcome"].(map[string]any)
		assert.Equal(t, "OperationOutcome", out0["resourceType"])
	})

	t.Run("Parameters with typed primitives, positiveInt, unsignedInt, and inline resource", func(t *testing.T) {
		paramsXML := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<Parameters xmlns="http://hl7.org/fhir" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="http://hl7.org/fhir http://hl7.org/fhir/parameters.xsd">
  <!-- XML comment allowed -->
  <id value="params-1"/>
  <parameter>
    <name value="count"/>
    <valueInteger value="-5"/>
  </parameter>
  <parameter>
    <name value="pageSize"/>
    <valuePositiveInt value="+25"/>
  </parameter>
  <parameter>
    <name value="offset"/>
    <valueUnsignedInt value="0"/>
  </parameter>
  <parameter>
    <name value="active"/>
    <valueBoolean value="true"/>
  </parameter>
  <parameter>
    <name value="resource"/>
    <resource>
      <ValueSet>
        <id value="vs-inline"/>
        <url value="http://example.org/ValueSet/test"/>
        <status value="active"/>
      </ValueSet>
    </resource>
  </parameter>
</Parameters>`)

		pm, err := fhirxml.UnmarshalFHIRMap(paramsXML)
		require.NoError(t, err)
		assert.Equal(t, "Parameters", pm["resourceType"])
		pList := pm["parameter"].([]any)
		require.Len(t, pList, 5)
		assert.Equal(t, float64(-5), pList[0].(map[string]any)["valueInteger"])
		assert.Equal(t, float64(25), pList[1].(map[string]any)["valuePositiveInt"])
		assert.Equal(t, float64(0), pList[2].(map[string]any)["valueUnsignedInt"])
		assert.Equal(t, true, pList[3].(map[string]any)["valueBoolean"])
		vs := pList[4].(map[string]any)["resource"].(map[string]any)
		assert.Equal(t, "ValueSet", vs["resourceType"])
		assert.Equal(t, "vs-inline", vs["id"])

		jsonBytes, err := fhirxml.UnmarshalFHIRJSON(paramsXML)
		require.NoError(t, err)
		assert.True(t, json.Valid(jsonBytes))
	})
}

func TestAll146R4ResourceTypesRoundTrip(t *testing.T) {
	allTypes := fhirutil.AllR4ResourceTypes()
	require.GreaterOrEqual(t, len(allTypes), 140)

	for _, rt := range allTypes {
		m := map[string]any{
			"resourceType": rt,
			"id":           "id-" + strings.ToLower(rt),
			"meta": map[string]any{
				"versionId": "1",
				"tag":       []string{}, // empty slice omitted cleanly
			},
		}
		xmlBytes, err := fhirxml.MarshalFHIRMap(m)
		require.NoError(t, err, "MarshalFHIRMap failed for %s", rt)

		decodedJSON, err := fhirxml.UnmarshalFHIRJSON(xmlBytes)
		require.NoError(t, err, "UnmarshalFHIRJSON failed for %s", rt)

		var roundTrip map[string]any
		require.NoError(t, json.Unmarshal(decodedJSON, &roundTrip))
		assert.Equal(t, rt, roundTrip["resourceType"])
		assert.Equal(t, "id-"+strings.ToLower(rt), roundTrip["id"])
	}

	// Custom / untyped resource fallback (e.g. SQL-on-FHIR ViewDefinition)
	vdMap := map[string]any{
		"resourceType": "ViewDefinition",
		"id":           "vd-1",
		"name":         "patient_demographics",
		"resource":     "Patient",
		"status":       "active",
		"tagList":      []string{"analytics", "sql"},
		"counts":       []int{1, 2},
		"weights":      []float64{0.5, 1.5},
		"flags":        []bool{true, false},
		"labels":       map[string]string{"env": "prod"},
		"div":          "Plain narrative text fallback",
		"contained": []map[string]any{
			{"resourceType": "Patient", "id": "p-sub"},
		},
		"extension": []map[string]any{
			{"url": "http://example.org/ext", "valueString": "ok"},
		},
	}
	vdXML, err := fhirxml.MarshalFHIRMap(vdMap)
	require.NoError(t, err)
	vdDecoded, err := fhirxml.UnmarshalFHIRMap(vdXML)
	require.NoError(t, err)
	assert.Equal(t, "ViewDefinition", vdDecoded["resourceType"])
	assert.Equal(t, "patient_demographics", vdDecoded["name"])
}

func TestSecurityAndMalformedXMLRejection(t *testing.T) {
	badPayloads := []struct {
		name    string
		xml     string
		wantSub string
	}{
		{
			name:    "Empty payload",
			xml:     "   ",
			wantSub: "empty FHIR XML",
		},
		{
			name: "XXE external SYSTEM entity",
			xml: `<?xml version="1.0"?>
<!DOCTYPE Patient [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>
<Patient xmlns="http://hl7.org/fhir"><id value="&xxe;"/></Patient>`,
			wantSub: "forbidden",
		},
		{
			name: "Billion laughs internal ENTITY expansion",
			xml: `<?xml version="1.0"?>
<!DOCTYPE lolz [
  <!ENTITY lol "lol">
  <!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;">
]>
<Patient xmlns="http://hl7.org/fhir"><id value="1"/></Patient>`,
			wantSub: "forbidden",
		},
		{
			name:    "DOCTYPE inside Narrative div",
			xml:     `<Patient xmlns="http://hl7.org/fhir"><text><status value="generated"/><div xmlns="http://www.w3.org/1999/xhtml"><!DOCTYPE html><p>hi</p></div></text></Patient>`,
			wantSub: "forbidden",
		},
		{
			name:    "Wrong root XML namespace",
			xml:     `<Envelope xmlns="http://schemas.xmlsoap.org/soap/envelope/"><Body/></Envelope>`,
			wantSub: "namespace",
		},
		{
			name:    "Unexpected attribute on root resource",
			xml:     `<Patient xmlns="http://hl7.org/fhir" id="attr-not-allowed"><active value="true"/></Patient>`,
			wantSub: "unexpected XML attribute",
		},
		{
			name:    "Primitive text content instead of value attribute",
			xml:     `<Patient xmlns="http://hl7.org/fhir"><gender>female</gender></Patient>`,
			wantSub: "value=",
		},
		{
			name:    "Duplicate non-repeating scalar element",
			xml:     `<Patient xmlns="http://hl7.org/fhir"><gender value="female"/><gender value="male"/></Patient>`,
			wantSub: "duplicate",
		},
		{
			name:    "Conflicting choice[x] elements",
			xml:     `<Patient xmlns="http://hl7.org/fhir"><deceasedBoolean value="true"/><deceasedDateTime value="2026-01-01"/></Patient>`,
			wantSub: "choice[x]",
		},
		{
			name:    "Invalid boolean literal",
			xml:     `<Patient xmlns="http://hl7.org/fhir"><active value="yes"/></Patient>`,
			wantSub: "boolean",
		},
		{
			name:    "Invalid integer literal",
			xml:     `<Patient xmlns="http://hl7.org/fhir"><multipleBirthInteger value="abc"/></Patient>`,
			wantSub: "integer",
		},
		{
			name:    "Invalid decimal literal",
			xml:     `<Observation xmlns="http://hl7.org/fhir"><valueQuantity><value value="12..34"/></valueQuantity></Observation>`,
			wantSub: "decimal",
		},
		{
			name:    "Invalid positiveInt zero",
			xml:     `<Parameters xmlns="http://hl7.org/fhir"><parameter><name value="p"/><valuePositiveInt value="0"/></parameter></Parameters>`,
			wantSub: "positiveInt",
		},
		{
			name:    "Invalid unsignedInt negative",
			xml:     `<Bundle xmlns="http://hl7.org/fhir"><type value="searchset"/><total value="-1"/></Bundle>`,
			wantSub: "unsignedInt",
		},
		{
			name:    "Empty primitive element without value/id/extension",
			xml:     `<Patient xmlns="http://hl7.org/fhir"><gender/></Patient>`,
			wantSub: "empty FHIR XML primitive",
		},
		{
			name:    "Empty inline contained wrapper",
			xml:     `<Patient xmlns="http://hl7.org/fhir"><contained/></Patient>`,
			wantSub: "empty inline resource",
		},
		{
			name:    "Multiple root elements",
			xml:     `<Patient xmlns="http://hl7.org/fhir"><id value="1"/></Patient><Patient xmlns="http://hl7.org/fhir"><id value="2"/></Patient>`,
			wantSub: "second root",
		},
		{
			name:    "Deep nesting bomb (>64 levels)",
			xml:     `<Patient xmlns="http://hl7.org/fhir">` + strings.Repeat(`<extension url="http://example.org/e">`, 70) + strings.Repeat(`</extension>`, 70) + `</Patient>`,
			wantSub: "nesting depth",
		},
	}

	for _, tc := range badPayloads {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fhirxml.UnmarshalFHIRJSON([]byte(tc.xml))
			require.Error(t, err)
			assert.Contains(t, strings.ToLower(err.Error()), strings.ToLower(tc.wantSub))
		})
	}

	// Marshal validation errors
	_, err := fhirxml.MarshalFHIRJSON(nil)
	require.Error(t, err)
	_, err = fhirxml.MarshalFHIRJSON([]byte(`{"id":"missing-type"}`))
	require.Error(t, err)
	_, err = fhirxml.MarshalFHIRMap(map[string]any{"resourceType": "Bad<Tag>"})
	require.Error(t, err)
}

func FuzzXMLRoundTrip(f *testing.F) {
	seeds := [][]byte{
		[]byte(`<?xml version="1.0" encoding="UTF-8"?><Patient xmlns="http://hl7.org/fhir"><id value="f1"/><active value="true"/><name><family value="Doe"/><given value="Jane"/></name><gender value="female"/><birthDate value="1990-01-01"/></Patient>`),
		[]byte(`<Observation xmlns="http://hl7.org/fhir"><id value="o1"/><status value="final"/><code><coding><system value="http://loinc.org"/><code value="8867-4"/></coding></code><valueQuantity><value value="72.5"/><unit value="/min"/></valueQuantity></Observation>`),
		[]byte(`<Bundle xmlns="http://hl7.org/fhir"><type value="searchset"/><total value="1"/><entry><resource><Patient><id value="p1"/></Patient></resource></entry></Bundle>`),
		[]byte(`<OperationOutcome xmlns="http://hl7.org/fhir"><issue><severity value="error"/><code value="invalid"/><diagnostics value="Bad input"/></issue></OperationOutcome>`),
		[]byte(`<!DOCTYPE foo [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><Patient xmlns="http://hl7.org/fhir"><id value="&xxe;"/></Patient>`),
		[]byte(`<Bundle xmlns="http://hl7.org/fhir"><A/></Bundle>`),
		[]byte(`<Patient xmlns="http://hl7.org/fhir"><_foo value="bar"/></Patient>`),
		[]byte(`<Bundle xmlns="http://hl7.org/fhir"><foo value="1" id="x"/><foo value="2"/></Bundle>`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64*1024 {
			return
		}
		jsonBytes, err := fhirxml.UnmarshalFHIRJSON(data)
		if err != nil {
			return
		}
		if !bytes.HasPrefix(bytes.TrimSpace(jsonBytes), []byte("{")) {
			t.Fatalf("expected JSON object output, got %q", string(jsonBytes))
		}
		xmlOut, err := fhirxml.MarshalFHIRJSON(jsonBytes)
		if err != nil {
			return
		}
		jsonBytes2, err := fhirxml.UnmarshalFHIRJSON(xmlOut)
		if err != nil {
			t.Fatalf("round-trip UnmarshalFHIRJSON failed on marshaled XML %s: %v", string(xmlOut), err)
		}
		if !bytes.Equal(jsonBytes, jsonBytes2) {
			t.Fatalf("round-trip JSON mismatch:\nfirst:  %s\nsecond: %s", string(jsonBytes), string(jsonBytes2))
		}
	})
}
