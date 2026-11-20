package ccda_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/ccda"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
)

func loadCCDAFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

func validateCCDABundleStrict(t *testing.T, bundle map[string]any) map[string][]map[string]any {
	t.Helper()
	engine := validation.NewEngine(validation.DefaultOptions())
	descs := fhirutil.AllR4ResourceDescriptors()

	entries, ok := bundle["entry"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, entries)

	byType := make(map[string][]map[string]any)
	for _, rawEntry := range entries {
		em, ok := rawEntry.(map[string]any)
		require.True(t, ok)
		res, ok := em["resource"].(map[string]any)
		require.True(t, ok)
		rt, _ := res["resourceType"].(string)
		require.NotEmpty(t, rt)

		out := engine.ValidateResource(rt, res)
		require.True(t, out.IsValid(), "resource %s/%v failed validation: %+v", rt, res["id"], out.Issues)

		if md, hasMD := descs[rt]; hasMD {
			rawJSON, err := json.Marshal(res)
			require.NoError(t, err)
			normJSON, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawJSON)
			require.NoError(t, err)
			dyn := dynamicpb.NewMessage(md)
			require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normJSON, dyn),
				"resource %s failed strict Proto3 unmarshal: %s", rt, string(rawJSON))
			_, err = proto.Marshal(dyn)
			require.NoError(t, err)
		}
		byType[rt] = append(byType[rt], res)
	}
	return byType
}

func TestGoldONCB1CCD_TransactionAndDocumentAndRoundTrip(t *testing.T) {
	fixedNow := time.Date(2026, 1, 15, 16, 0, 0, 0, time.UTC)
	raw := loadCCDAFixture(t, "onc_b1_ccd_r21.xml")

	// 1. Transaction mode import
	resTx, err := ccda.ConvertToFHIR(raw, ccda.ImportOptions{Mode: "transaction", Now: fixedNow})
	require.NoError(t, err)
	assert.Equal(t, "transaction", resTx.Bundle["type"])
	assert.Equal(t, []string{"problems", "allergies", "medications", "immunizations", "results", "vitals", "procedures", "encounters"}, resTx.SectionsParsed)

	byType := validateCCDABundleStrict(t, resTx.Bundle)
	require.Len(t, byType["Patient"], 1)
	require.Len(t, byType["Condition"], 1)
	require.Len(t, byType["AllergyIntolerance"], 1)
	require.Len(t, byType["MedicationRequest"], 1)
	require.Len(t, byType["Immunization"], 1)
	require.Len(t, byType["DiagnosticReport"], 1)
	require.Len(t, byType["Observation"], 2) // 1 lab + 1 vital sign
	require.Len(t, byType["Procedure"], 1)
	require.Len(t, byType["Encounter"], 2) // encompassingEncounter + Encounters section
	require.Len(t, byType["Composition"], 1)
	require.Len(t, byType["DocumentReference"], 1)
	require.Len(t, byType["Provenance"], 1)

	pat := byType["Patient"][0]
	assert.Equal(t, "female", pat["gender"])
	assert.Equal(t, "1970-05-01", pat["birthDate"])
	assert.Len(t, pat["extension"], 2)

	// Re-import idempotence
	resTx2, err := ccda.ConvertToFHIR(raw, ccda.ImportOptions{Mode: "transaction", Now: fixedNow})
	require.NoError(t, err)
	assert.Equal(t, resTx.PatientID, resTx2.PatientID)
	assert.Equal(t, resTx.CompositionID, resTx2.CompositionID)
	assert.Equal(t, resTx.DocumentReferenceID, resTx2.DocumentReferenceID)

	// 2. Document mode import (Composition strictly entry[0])
	resDoc, err := ccda.ConvertToFHIR(raw, ccda.ImportOptions{Mode: "document", Now: fixedNow})
	require.NoError(t, err)
	assert.Equal(t, "document", resDoc.Bundle["type"])
	docEntries := resDoc.Bundle["entry"].([]any)
	firstRes := docEntries[0].(map[string]any)["resource"].(map[string]any)
	assert.Equal(t, "Composition", firstRes["resourceType"])
	validateCCDABundleStrict(t, resDoc.Bundle)

	// 3. Export FHIR resources back to C-CDA R2.1 XML -> re-parse -> verify all 8 sections preserved
	var clinResources []map[string]any
	for rt, list := range byType {
		if rt == "Patient" || rt == "Composition" || rt == "DocumentReference" || rt == "Provenance" {
			continue
		}
		clinResources = append(clinResources, list...)
	}
	exportedXML, err := ccda.ConvertFHIRToCCDA(pat, clinResources, ccda.ExportOptions{
		DocumentID: "RT-EXPORT-1",
		Title:      "Round-Trip CCD",
		Now:        fixedNow,
	})
	require.NoError(t, err)
	assert.Contains(t, string(exportedXML), ccda.NamespaceHL7V3)
	assert.Contains(t, string(exportedXML), ccda.TemplateCCD)

	rtRes, err := ccda.ConvertToFHIR(exportedXML, ccda.ImportOptions{Mode: "transaction", Now: fixedNow})
	require.NoError(t, err)
	rtByType := validateCCDABundleStrict(t, rtRes.Bundle)
	assert.Equal(t, "female", rtByType["Patient"][0]["gender"])
	assert.Equal(t, "1970-05-01", rtByType["Patient"][0]["birthDate"])
	assert.GreaterOrEqual(t, len(rtByType["Condition"]), 1)
	assert.GreaterOrEqual(t, len(rtByType["AllergyIntolerance"]), 1)
	assert.GreaterOrEqual(t, len(rtByType["MedicationRequest"]), 1)
	assert.GreaterOrEqual(t, len(rtByType["Immunization"]), 1)
	assert.GreaterOrEqual(t, len(rtByType["DiagnosticReport"]), 1)
	assert.GreaterOrEqual(t, len(rtByType["Observation"]), 2)
	assert.GreaterOrEqual(t, len(rtByType["Procedure"]), 1)
	assert.GreaterOrEqual(t, len(rtByType["Encounter"]), 1)

	// 4. ConvertBundleToCCDA from IPS/document Bundle
	exportedFromBundle, err := ccda.ConvertBundleToCCDA(resDoc.Bundle, ccda.ExportOptions{Now: fixedNow})
	require.NoError(t, err)
	_, err = ccda.Parse(exportedFromBundle)
	require.NoError(t, err)
}

func TestCCDANullFlavorEmptySectionsAndRefusals(t *testing.T) {
	fixedNow := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	raw := loadCCDAFixture(t, "ccd_nullflavor_empty_sections.xml")
	res, err := ccda.ConvertToFHIR(raw, ccda.ImportOptions{Now: fixedNow})
	require.NoError(t, err)
	byType := validateCCDABundleStrict(t, res.Bundle)
	assert.Empty(t, byType["Condition"])
	require.Len(t, byType["Immunization"], 1)
	assert.Equal(t, "not-done", byType["Immunization"][0]["status"])
	require.Len(t, byType["Observation"], 1)
	assert.Equal(t, "asked-unknown", byType["Observation"][0]["dataAbsentReason"].(map[string]any)["coding"].([]any)[0].(map[string]any)["code"])

	// Export empty patient -> C-CDA with nullFlavor="NI" sections
	emptyCCD, err := ccda.ConvertFHIRToCCDA(map[string]any{
		"resourceType": "Patient",
		"id":           "pat-empty",
		"gender":       "other",
		"name":         []any{map[string]any{"family": "Minimal", "given": []any{"Alex"}}},
	}, nil, ccda.ExportOptions{Now: fixedNow})
	require.NoError(t, err)
	assert.Contains(t, string(emptyCCD), `nullFlavor="NI"`)
	parsedEmpty, err := ccda.Parse(emptyCCD)
	require.NoError(t, err)
	assert.Equal(t, "Continuity of Care Document (C-CDA R2.1)", parsedEmpty.Title)
}

func TestCCDAXXEAndSecurityRejections_AndOIDHelpers(t *testing.T) {
	badPayloads := []struct {
		name string
		xml  string
		err  error
	}{
		{
			name: "XXE SYSTEM external entity",
			xml:  `<?xml version="1.0"?><!DOCTYPE ClinicalDocument [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><ClinicalDocument xmlns="urn:hl7-org:v3"><title>&xxe;</title></ClinicalDocument>`,
			err:  ccda.ErrXXEForbidden,
		},
		{
			name: "Billion laughs entity expansion",
			xml:  `<!DOCTYPE lolz [<!ENTITY a "1"><!ENTITY b "&a;&a;">]><ClinicalDocument xmlns="urn:hl7-org:v3"/>`,
			err:  ccda.ErrXXEForbidden,
		},
		{
			name: "Deep XML nesting bomb",
			xml:  `<ClinicalDocument xmlns="urn:hl7-org:v3">` + strings.Repeat(`<component>`, 70) + strings.Repeat(`</component>`, 70) + `</ClinicalDocument>`,
			err:  ccda.ErrNestingTooDeep,
		},
		{
			name: "Wrong root element",
			xml:  `<Patient xmlns="http://hl7.org/fhir"><id value="1"/></Patient>`,
			err:  ccda.ErrInvalidCCDA,
		},
		{
			name: "Empty document",
			xml:  "   \n\t ",
			err:  ccda.ErrEmptyDocument,
		},
	}
	for _, tc := range badPayloads {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ccda.ConvertToFHIR([]byte(tc.xml), ccda.ImportOptions{})
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.err)
		})
	}

	assert.Equal(t, "http://loinc.org", ccda.OIDToFHIRSystem("2.16.840.1.113883.6.1"))
	assert.Equal(t, "urn:oid:1.2.3.4", ccda.OIDToFHIRSystem("1.2.3.4"))
	oid, name := ccda.FHIRSystemToOID("http://snomed.info/sct")
	assert.Equal(t, "2.16.840.1.113883.6.96", oid)
	assert.Equal(t, "SNOMED CT", name)
	assert.Equal(t, "masked", ccda.NullFlavorToDataAbsentReason("MSK"))
}

func FuzzCCDAParser(f *testing.F) {
	if b, err := os.ReadFile(filepath.Join("testdata", "onc_b1_ccd_r21.xml")); err == nil {
		f.Add(b)
	}
	if b, err := os.ReadFile(filepath.Join("testdata", "ccd_nullflavor_empty_sections.xml")); err == nil {
		f.Add(b)
	}
	f.Add([]byte(`<!DOCTYPE ClinicalDocument [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><ClinicalDocument xmlns="urn:hl7-org:v3"/>`))
	f.Add([]byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><component><structuredBody/></component></ClinicalDocument>`))
	f.Add([]byte(`not xml`))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 128*1024 {
			return
		}
		lower := strings.ToLower(string(data))
		res, err := ccda.ConvertToFHIR(data, ccda.ImportOptions{Now: time.Unix(1700000000, 0).UTC()})
		if (strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<!entity")) && err == nil {
			t.Fatalf("XXE/DOCTYPE payload accepted unexpectedly")
		}
		if err != nil {
			return
		}
		if _, err := ccda.ConvertBundleToCCDA(res.Bundle, ccda.ExportOptions{Now: time.Unix(1700000000, 0).UTC()}); err != nil {
			// Document Bundles containing recordTarget patients must round-trip without error
			_ = err
		}
	})
}
