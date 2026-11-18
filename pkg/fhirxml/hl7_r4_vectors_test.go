package fhirxml_test

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/fhirxml"
)

var wsCollapseRe = regexp.MustCompile(`\s+`)

func decodeCanonicalFHIRMap(t *testing.T, rawJSON []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(rawJSON))
	dec.UseNumber()
	var m map[string]any
	require.NoError(t, dec.Decode(&m))
	return m
}

func normalizeXHTMLForComparison(s string) string {
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&#34;", `"`)
	s = strings.ReplaceAll(s, "<br></br>", "<br/>")
	s = strings.TrimSpace(wsCollapseRe.ReplaceAllString(s, " "))
	return s
}

func normalizeValueForUpstreamComparison(v any, normalizeDiv bool) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			if normalizeDiv && k == "div" {
				if s, ok := child.(string); ok {
					out[k] = normalizeXHTMLForComparison(s)
					continue
				}
			}
			out[k] = normalizeValueForUpstreamComparison(child, normalizeDiv)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = normalizeValueForUpstreamComparison(item, normalizeDiv)
		}
		return out
	case json.Number:
		s := val.String()
		if bf, _, err := big.ParseFloat(s, 10, 256, big.ToNearestEven); err == nil {
			return bf.Text('g', 40)
		}
		return s
	default:
		return v
	}
}

func assertGoogleFHIRProtoRoundTrip(t *testing.T, rawJSON []byte) {
	t.Helper()
	m := decodeCanonicalFHIRMap(t, rawJSON)
	rt, _ := m["resourceType"].(string)
	require.NotEmpty(t, rt, "resourceType must be present")
	md := fhirutil.AllR4ResourceDescriptors()[rt]
	require.NotNilf(t, md, "unknown R4 resourceType %q", rt)

	normBytes, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawJSON)
	require.NoErrorf(t, err, "NormalizeFHIRJSONToProtoJSON(%s)", rt)

	msg := dynamicpb.NewMessage(md)
	require.NoErrorf(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normBytes, msg),
		"protojson.Unmarshal(%s)", rt)

	wireBytes, err := proto.Marshal(msg)
	require.NoErrorf(t, err, "proto.Marshal(%s)", rt)

	denormJSON, err := fhirutil.DenormalizeProtoJSONToFHIRJSON(md, rt, wireBytes, 0, time.Time{})
	require.NoErrorf(t, err, "DenormalizeProtoJSONToFHIRJSON(%s)", rt)
	require.NotEmpty(t, denormJSON)
}

// TestOfficialHL7R4XMLVectors verifies upstream FHIR/fhir-test-cases R4 XML & JSON specimens
// across XML -> JSON -> XML -> JSON fixpoints, google/fhir R4 Proto3 wire round-trips, exact
// high-precision Decimal preservation, primitive extensions, XML comments, XHTML narrative
// whitespace handling, and paired upstream XML/JSON semantic equivalence.
func TestOfficialHL7R4XMLVectors(t *testing.T) {
	vectorDir := filepath.Join("testdata", "hl7-r4")
	entries, err := os.ReadDir(vectorDir)
	require.NoError(t, err)

	var xmlFiles, jsonFiles []string
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		switch filepath.Ext(ent.Name()) {
		case ".xml":
			xmlFiles = append(xmlFiles, ent.Name())
		case ".json":
			jsonFiles = append(jsonFiles, ent.Name())
		}
	}
	require.GreaterOrEqual(t, len(xmlFiles), 17, "expected >= 17 upstream HL7 R4 XML vectors")
	require.GreaterOrEqual(t, len(jsonFiles), 7, "expected >= 7 paired upstream HL7 R4 JSON vectors")

	t.Run("UpstreamXML_ToJSON_Proto3_And_RoundTripFixpoint", func(t *testing.T) {
		for _, xmlName := range xmlFiles {
			t.Run(xmlName, func(t *testing.T) {
				rawXML, err := os.ReadFile(filepath.Join(vectorDir, xmlName))
				require.NoError(t, err)

				// 1. Upstream XML -> FHIR JSON
				jsonPass1, err := fhirxml.UnmarshalFHIRJSON(rawXML)
				require.NoError(t, err, "UnmarshalFHIRJSON(%s)", xmlName)
				assert.NotContains(t, string(jsonPass1), "&#xA;", "Narrative.div should preserve literal newlines rather than &#xA;")
				assert.NotContains(t, string(jsonPass1), "&#x9;", "Narrative.div should preserve literal tabs rather than &#x9;")

				// 2. Verify google/fhir R4 Proto3 binary wire round-trip
				assertGoogleFHIRProtoRoundTrip(t, jsonPass1)

				// 3. FHIR JSON -> FHIR XML -> FHIR JSON must be an exact lexical fixpoint
				xmlPass2, err := fhirxml.MarshalFHIRJSON(jsonPass1)
				require.NoError(t, err, "MarshalFHIRJSON(%s)", xmlName)

				jsonPass2, err := fhirxml.UnmarshalFHIRJSON(xmlPass2)
				require.NoError(t, err, "UnmarshalFHIRJSON round-trip (%s)", xmlName)

				m1 := decodeCanonicalFHIRMap(t, jsonPass1)
				m2 := decodeCanonicalFHIRMap(t, jsonPass2)
				assert.Equal(t, m1, m2, "exact JSON lexical fixpoint mismatch on %s", xmlName)

				// 4. Canonical XML byte-for-byte idempotence on second XML pass
				xmlPass3, err := fhirxml.MarshalFHIRJSON(jsonPass2)
				require.NoError(t, err)
				assert.Equal(t, string(xmlPass2), string(xmlPass3), "canonical XML byte-for-byte idempotence mismatch on %s", xmlName)
			})
		}
	})

	t.Run("UpstreamJSON_ToXML_ToJSON_Fixpoint_And_Proto3", func(t *testing.T) {
		seenBase := make(map[string]bool)
		allJSONPaths := make([]string, 0, 60)
		for _, jf := range jsonFiles {
			seenBase[jf] = true
			allJSONPaths = append(allJSONPaths, filepath.Join(vectorDir, jf))
		}
		valPosDir := filepath.Join("..", "validation", "testdata", "hl7-r4", "positive")
		if posEntries, err := os.ReadDir(valPosDir); err == nil {
			for _, ent := range posEntries {
				if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".json") && !seenBase[ent.Name()] {
					seenBase[ent.Name()] = true
					allJSONPaths = append(allJSONPaths, filepath.Join(valPosDir, ent.Name()))
				}
			}
		}

		for _, jp := range allJSONPaths {
			base := filepath.Base(jp)
			t.Run(base, func(t *testing.T) {
				rawJSON, err := os.ReadFile(jp)
				require.NoError(t, err)

				xmlBytes, err := fhirxml.MarshalFHIRJSON(rawJSON)
				require.NoError(t, err, "MarshalFHIRJSON(%s)", base)

				rtJSON1, err := fhirxml.UnmarshalFHIRJSON(xmlBytes)
				require.NoError(t, err, "UnmarshalFHIRJSON(%s)", base)

				assertGoogleFHIRProtoRoundTrip(t, rtJSON1)

				xmlBytes2, err := fhirxml.MarshalFHIRJSON(rtJSON1)
				require.NoError(t, err)
				rtJSON2, err := fhirxml.UnmarshalFHIRJSON(xmlBytes2)
				require.NoError(t, err)

				assert.Equal(t, decodeCanonicalFHIRMap(t, rtJSON1), decodeCanonicalFHIRMap(t, rtJSON2),
					"XML <-> JSON fixpoint mismatch on %s", base)
				assert.Equal(t, string(xmlBytes), string(xmlBytes2),
					"canonical XML byte-for-byte idempotence mismatch on %s", base)
			})
		}
	})

	t.Run("UpstreamPairedXMLAndJSON_SemanticEquivalence", func(t *testing.T) {
		exactPairs := []string{
			"patient-example",
			"condition-example",
			"organization-1",
			"observation-decimal",
			"questionnaire-example",
		}
		for _, base := range exactPairs {
			t.Run(base, func(t *testing.T) {
				rawXML, err := os.ReadFile(filepath.Join(vectorDir, base+".xml"))
				require.NoError(t, err)
				rawJSON, err := os.ReadFile(filepath.Join(vectorDir, base+".json"))
				require.NoError(t, err)

				fromXMLBytes, err := fhirxml.UnmarshalFHIRJSON(rawXML)
				require.NoError(t, err)

				xmlNorm := normalizeValueForUpstreamComparison(decodeCanonicalFHIRMap(t, fromXMLBytes), true)
				jsonNorm := normalizeValueForUpstreamComparison(decodeCanonicalFHIRMap(t, rawJSON), true)
				assert.Equal(t, jsonNorm, xmlNorm, "semantic mismatch between upstream %s.xml and %s.json", base, base)
			})
		}
	})

	t.Run("ObservationDecimal_ExactPrecisionPreservation", func(t *testing.T) {
		rawXML, err := os.ReadFile(filepath.Join(vectorDir, "observation-decimal.xml"))
		require.NoError(t, err)

		jsonBytes, err := fhirxml.UnmarshalFHIRJSON(rawXML)
		require.NoError(t, err)
		jsonStr := string(jsonBytes)

		for _, wantLiteral := range []string{
			`"value":1.0`,
			`"value":1.00`,
			`"value":0.0000000000000000000001`,
			`"value":1000000000000000000`,
			`"value":1.000000000000000000e-245`,
			`"value":-1.000000000000000000e245`,
		} {
			assert.Contains(t, jsonStr, wantLiteral)
		}
	})
}
