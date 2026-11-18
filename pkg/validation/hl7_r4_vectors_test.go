package validation_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/fhirxml"
	"github.com/flint-fhir/flint/pkg/validation"
)

type negativeHL7Vector struct {
	Name             string          `json:"name"`
	File             string          `json:"file,omitempty"`
	ResourceType     string          `json:"resourceType"`
	ExpectCode       string          `json:"expectCode"`
	ExpectExprPrefix string          `json:"expectExprPrefix"`
	Resource         json.RawMessage `json:"resource,omitempty"`
}

// TestOfficialHL7R4ValidationVectors verifies Flint's StructureDefinition, primitive,
// choice[x], ValueSet, and R4 invariant validator against vendored upstream
// FHIR/fhir-test-cases R4 positive examples (both JSON and XML->JSON) and negative vectors.
func TestOfficialHL7R4ValidationVectors(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())

	t.Run("PositiveUpstreamR4Examples_JSON", func(t *testing.T) {
		posDir := filepath.Join("testdata", "hl7-r4", "positive")
		entries, err := os.ReadDir(posDir)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(entries), 45, "expected >= 45 upstream HL7 R4 positive examples")

		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
				continue
			}
			t.Run(ent.Name(), func(t *testing.T) {
				rawJSON, err := os.ReadFile(filepath.Join(posDir, ent.Name()))
				require.NoError(t, err)

				var header struct {
					ResourceType string `json:"resourceType"`
				}
				require.NoError(t, json.Unmarshal(rawJSON, &header))
				require.NotEmpty(t, header.ResourceType)

				outcome := engine.ValidateJSON(header.ResourceType, rawJSON)
				assert.Truef(t, outcome.IsValid(), "expected upstream %s (%s) to pass R4 validation, got issues: %+v",
					ent.Name(), header.ResourceType, outcome.Issues)
			})
		}
	})

	t.Run("PositiveUpstreamR4Examples_TranscodedFromXML", func(t *testing.T) {
		xmlDir := filepath.Join("..", "fhirxml", "testdata", "hl7-r4")
		entries, err := os.ReadDir(xmlDir)
		require.NoError(t, err)

		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".xml") {
				continue
			}
			t.Run(ent.Name(), func(t *testing.T) {
				rawXML, err := os.ReadFile(filepath.Join(xmlDir, ent.Name()))
				require.NoError(t, err)

				rawJSON, err := fhirxml.UnmarshalFHIRJSON(rawXML)
				require.NoError(t, err)

				var header struct {
					ResourceType string `json:"resourceType"`
				}
				require.NoError(t, json.Unmarshal(rawJSON, &header))

				outcome := engine.ValidateJSON(header.ResourceType, rawJSON)
				assert.Truef(t, outcome.IsValid(), "expected upstream XML %s (%s) to pass R4 validation, got issues: %+v",
					ent.Name(), header.ResourceType, outcome.Issues)
			})
		}
	})

	t.Run("NegativeHL7R4ValidationVectors", func(t *testing.T) {
		negDir := filepath.Join("testdata", "hl7-r4", "negative")
		rawManifest, err := os.ReadFile(filepath.Join(negDir, "manifest.json"))
		require.NoError(t, err)

		var cases []negativeHL7Vector
		require.NoError(t, json.Unmarshal(rawManifest, &cases))
		require.GreaterOrEqual(t, len(cases), 15)

		for _, tc := range cases {
			t.Run(tc.Name, func(t *testing.T) {
				payload := tc.Resource
				if tc.File != "" {
					var readErr error
					payload, readErr = os.ReadFile(filepath.Join(negDir, tc.File))
					require.NoError(t, readErr)
				}

				outcome := engine.ValidateJSON(tc.ResourceType, payload)
				require.Falsef(t, outcome.IsValid(), "expected negative vector %s to fail validation", tc.Name)

				matched := false
				for _, iss := range outcome.Issues {
					if iss.Severity != validation.SeverityError && iss.Severity != validation.SeverityFatal {
						continue
					}
					codeMatch := tc.ExpectCode == "" || string(iss.Code) == tc.ExpectCode
					exprMatch := tc.ExpectExprPrefix == ""
					if !exprMatch {
						for _, expr := range iss.Expression {
							if strings.HasPrefix(expr, tc.ExpectExprPrefix) {
								exprMatch = true
								break
							}
						}
					}
					if codeMatch && exprMatch {
						matched = true
						break
					}
				}
				assert.Truef(t, matched, "expected error with code=%q exprPrefix=%q on %s, got issues: %+v",
					tc.ExpectCode, tc.ExpectExprPrefix, tc.Name, outcome.Issues)
			})
		}
	})
}
