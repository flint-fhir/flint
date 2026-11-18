package server_test

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/fhirxml"
	"github.com/flint-fhir/flint/server"
)

type rfc6902PatchVector struct {
	Comment  string          `json:"comment"`
	Doc      json.RawMessage `json:"doc"`
	Patch    json.RawMessage `json:"patch"`
	Expected json.RawMessage `json:"expected"`
	Error    string          `json:"error"`
}

type fhirPatchXMLSuite struct {
	XMLName xml.Name           `xml:"tests"`
	Cases   []fhirPatchXMLCase `xml:"case"`
}

type fhirPatchXMLCase struct {
	Name   string            `xml:"name,attr"`
	Mode   string            `xml:"mode,attr"`
	Input  fhirPatchInnerXML `xml:"input"`
	Diff   fhirPatchInnerXML `xml:"diff"`
	Output fhirPatchInnerXML `xml:"output"`
	Error  *fhirPatchXMLErr  `xml:"error"`
}

type fhirPatchInnerXML struct {
	Inner string `xml:",innerxml"`
}

type fhirPatchXMLErr struct {
	Msg string `xml:"msg,attr"`
}

var patchWSRe = regexp.MustCompile(`\s+`)

func normalizePatchedMap(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			if k == "div" {
				if s, ok := child.(string); ok {
					s = strings.ReplaceAll(s, "&quot;", `"`)
					s = strings.ReplaceAll(s, "&#34;", `"`)
					out[k] = strings.TrimSpace(patchWSRe.ReplaceAllString(s, " "))
					continue
				}
			}
			normChild := normalizePatchedMap(child)
			if cm, ok := normChild.(map[string]any); ok && len(cm) == 0 {
				continue
			}
			if ca, ok := normChild.([]any); ok && len(ca) == 0 {
				continue
			}
			out[k] = normChild
		}
		return out
	case []any:
		var out []any
		for _, item := range val {
			normItem := normalizePatchedMap(item)
			if cm, ok := normItem.(map[string]any); ok && len(cm) == 0 {
				continue
			}
			out = append(out, normItem)
		}
		return out
	default:
		return v
	}
}

func decodeJSONMapOrAny(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	require.NoError(t, dec.Decode(&out))
	return out
}

// TestOfficialHL7R4PatchVectors executes the vendored upstream FHIR/fhir-test-cases
// r4/patch/json-patch-tests.json (RFC 6902) and r4/patch/fhir-patch-tests.xml
// (HL7 R4 FHIRPath Patch) suites against Flint's patch engines.
func TestOfficialHL7R4PatchVectors(t *testing.T) {
	t.Run("UpstreamRFC6902JSONPatchSuite", func(t *testing.T) {
		rawSuite, err := os.ReadFile(filepath.Join("testdata", "patch-r4", "json-patch-tests.json"))
		require.NoError(t, err)

		var cases []rfc6902PatchVector
		require.NoError(t, json.Unmarshal(rawSuite, &cases))
		require.NotEmpty(t, cases)

		for _, tc := range cases {
			t.Run(tc.Comment, func(t *testing.T) {
				patched, status, patchErr := server.ApplyJSONPatch(tc.Doc, tc.Patch)
				if tc.Error != "" {
					require.Errorf(t, patchErr, "expected RFC 6902 error %q", tc.Error)
					assert.GreaterOrEqual(t, status, 400)
					return
				}
				require.NoError(t, patchErr)
				assert.Equal(t, 200, status)
				assert.Equal(t, decodeJSONMapOrAny(t, tc.Expected), decodeJSONMapOrAny(t, patched))
			})
		}
	})

	t.Run("UpstreamHL7FHIRPathPatchSuite", func(t *testing.T) {
		rawXML, err := os.ReadFile(filepath.Join("testdata", "patch-r4", "fhir-patch-tests.xml"))
		require.NoError(t, err)

		var suite fhirPatchXMLSuite
		require.NoError(t, xml.Unmarshal(rawXML, &suite))
		require.GreaterOrEqual(t, len(suite.Cases), 30)

		for _, tc := range suite.Cases {
			if strings.EqualFold(tc.Mode, "backwards") {
				continue
			}
			t.Run(tc.Name, func(t *testing.T) {
				inputJSON, err := fhirxml.UnmarshalFHIRJSON([]byte(tc.Input.Inner))
				require.NoError(t, err, "decode <input> XML")

				diffJSON, err := fhirxml.UnmarshalFHIRJSON([]byte(tc.Diff.Inner))
				require.NoError(t, err, "decode <diff> Parameters XML")

				patchedJSON, status, patchErr := server.ApplyFHIRPatch(inputJSON, diffJSON)
				if tc.Error != nil {
					require.Errorf(t, patchErr, "expected FHIRPatch error %q", tc.Error.Msg)
					assert.GreaterOrEqual(t, status, 400)
					return
				}
				require.NoErrorf(t, patchErr, "ApplyFHIRPatch(%s) failed; diff=%s", tc.Name, string(diffJSON))
				assert.Equal(t, 200, status)

				expectedJSON, err := fhirxml.UnmarshalFHIRJSON([]byte(tc.Output.Inner))
				require.NoError(t, err, "decode <output> XML")

				gotNorm := normalizePatchedMap(decodeJSONMapOrAny(t, patchedJSON))
				wantNorm := normalizePatchedMap(decodeJSONMapOrAny(t, expectedJSON))
				assert.Equal(t, wantNorm, gotNorm, "FHIRPatch output mismatch for %s", tc.Name)
			})
		}
	})
}
