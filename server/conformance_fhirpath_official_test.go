package server_test

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

type fhirPathXMLTests struct {
	XMLName xml.Name           `xml:"tests"`
	Groups  []fhirPathXMLGroup `xml:"group"`
}

type fhirPathXMLGroup struct {
	Name        string            `xml:"name,attr"`
	Description string            `xml:"description,attr"`
	Mode        string            `xml:"mode,attr"`
	Tests       []fhirPathXMLTest `xml:"test"`
}

type fhirPathXMLTest struct {
	Name        string           `xml:"name,attr"`
	Description string           `xml:"description,attr"`
	InputFile   string           `xml:"inputfile,attr"`
	Context     string           `xml:"context,attr"`
	ContextType string           `xml:"contextType,attr"`
	Predicate   string           `xml:"predicate,attr"`
	Ordered     string           `xml:"ordered,attr"`
	Mode        string           `xml:"mode,attr"`
	Expression  fhirPathXMLExpr  `xml:"expression"`
	Outputs     []fhirPathXMLOut `xml:"output"`
}

type fhirPathXMLExpr struct {
	Invalid string `xml:"invalid,attr"`
	Body    string `xml:",chardata"`
}

type fhirPathXMLOut struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

// TestOfficialFHIRPathR4ComplianceSuite executes the official HL7 FHIRPath R4 test corpus
// (server/testdata/fhirpath-r4/tests-fhir-r4.xml from FHIR/fhir-test-cases) against both
// Flint's FHIRPath R4 engine and the live FHIR R4 REST /$fhirpath endpoint.
func TestOfficialFHIRPathR4ComplianceSuite(t *testing.T) {
	corpusPath := filepath.Join("testdata", "fhirpath-r4", "tests-fhir-r4.xml")
	rawXML, err := os.ReadFile(corpusPath)
	require.NoError(t, err, "read official HL7 FHIRPath R4 XML corpus")

	var suite fhirPathXMLTests
	require.NoError(t, xml.Unmarshal(rawXML, &suite), "parse tests-fhir-r4.xml")
	require.NotEmpty(t, suite.Groups)

	fixtureCache := make(map[string]map[string]any)
	loadFixture := func(t *testing.T, inputFile string) map[string]any {
		t.Helper()
		name := strings.TrimSpace(inputFile)
		if name == "" {
			return map[string]any{"resourceType": "Patient", "id": "empty-context"}
		}
		if strings.HasSuffix(strings.ToLower(name), ".xml") {
			name = name[:len(name)-4] + ".json"
		}
		if cached, ok := fixtureCache[name]; ok {
			return cached
		}
		fullPath := filepath.Join("testdata", "fhirpath-r4", name)
		rawBytes, readErr := os.ReadFile(fullPath)
		require.NoError(t, readErr, "load FHIRPath R4 JSON fixture %s", fullPath)
		m, decErr := server.UnmarshalFHIRJSONPreservingNumbers(rawBytes)
		require.NoError(t, decErr, "decode FHIRPath R4 JSON fixture %s", fullPath)
		fixtureCache[name] = m
		return m
	}

	// Also boot a live Flint HTTP server to verify HTTP /$fhirpath status codes & Parameters wire format
	db := openMemPostgres(t)
	defer db.Close()
	pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
	srv := server.New(pgStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var totalExecuted, skippedCustom, skippedSemantic, verifiedHTTPEndpoint int

	for _, grp := range suite.Groups {
		grp := grp
		if grp.Mode == "custom" {
			skippedCustom += len(grp.Tests)
			continue
		}
		t.Run(grp.Name, func(t *testing.T) {
			for idx, tc := range grp.Tests {
				tc := tc
				tcName := tc.Name
				if tcName == "" {
					tcName = fmt.Sprintf("case_%02d", idx+1)
				}
				t.Run(tcName, func(t *testing.T) {
					inv := strings.TrimSpace(tc.Expression.Invalid)
					if inv == "semantic" {
						skippedSemantic++
						t.Skip("static semantic type-check test (not runtime evaluation)")
					}

					root := loadFixture(t, tc.InputFile)
					expr := tc.Expression.Body

					if inv == "syntax" || inv == "true" || inv == "execution" {
						_, evalErr := server.EvalFHIRPathTypedWithContextExpr(root, tc.Context, expr)
						require.Error(t, evalErr, "expected %s error for expression: %s", inv, expr)
						totalExecuted++

						// Spot-check syntax errors over live HTTP /$fhirpath endpoint (must return HTTP 400)
						if (inv == "syntax" || inv == "true") && verifiedHTTPEndpoint < 15 {
							verifiedHTTPEndpoint++
							httpStatus := postLiveFHIRPathEndpoint(t, ts.URL, root, expr)
							assert.Equal(t, http.StatusBadRequest, httpStatus, "HTTP /$fhirpath syntax error status for %s", expr)
						}
						return
					}

					isPred := strings.EqualFold(strings.TrimSpace(tc.Predicate), "true")
					if isPred {
						var focus []any
						if tc.Context != "" {
							ctxVals, ctxErr := server.EvalFHIRPathTyped(root, tc.Context)
							require.NoError(t, ctxErr)
							for _, cv := range ctxVals {
								focus = append(focus, cv.Value)
							}
						}
						gotBool, predErr := server.EvalFHIRPathPredicate(root, focus, expr)
						require.NoError(t, predErr, "predicate failed for expression: %s", expr)
						require.Len(t, tc.Outputs, 1, "predicate test must have 1 boolean output")
						expBool, parseErr := strconv.ParseBool(strings.TrimSpace(tc.Outputs[0].Body))
						require.NoError(t, parseErr)
						assert.Equal(t, expBool, gotBool, "predicate mismatch for expression: %s", expr)
						totalExecuted++
						return
					}

					actual, evalErr := server.EvalFHIRPathTypedWithContextExpr(root, tc.Context, expr)
					require.NoError(t, evalErr, "unexpected error evaluating expression: %s", expr)
					totalExecuted++

					unordered := strings.EqualFold(strings.TrimSpace(tc.Ordered), "false")
					assertFHIRPathOutputsMatch(t, expr, tc.Outputs, actual, unordered)
				})
			}
		})
	}

	t.Logf("Official HL7 FHIRPath R4 Conformance: %d runtime tests passed (skipped %d static-semantic, %d custom Java-RI extensions; %d HTTP /$fhirpath negative checks)",
		totalExecuted, skippedSemantic, skippedCustom, verifiedHTTPEndpoint)
	require.GreaterOrEqual(t, totalExecuted, 800)
}

func postLiveFHIRPathEndpoint(t *testing.T, baseURL string, resource map[string]any, expr string) int {
	t.Helper()
	payload := map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "resource", "resource": resource},
			{"name": "expression", "valueString": expr},
		},
	}
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	resp, err := http.Post(baseURL+"/fhir/r4/default/$fhirpath", "application/fhir+json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

func assertFHIRPathOutputsMatch(t *testing.T, expr string, expected []fhirPathXMLOut, actual []server.FHIRPathValue, unordered bool) {
	t.Helper()
	if !assert.Equal(t, len(expected), len(actual), "cardinality mismatch for expression %q\nexpected=%+v\nactual=%+v", expr, expected, actual) {
		return
	}
	if !unordered {
		for i := range expected {
			if !singleFHIRPathOutputMatches(expected[i], actual[i]) {
				t.Errorf("item[%d] mismatch for expression %q:\n  expected: (%s) %q\n  actual:   (%s) value=%v decText=%q",
					i, expr, expected[i].Type, expected[i].Body, actual[i].FHIRType, actual[i].Value, actual[i].DecText)
			}
		}
		return
	}
	used := make([]bool, len(actual))
	for i, exp := range expected {
		matched := false
		for j, act := range actual {
			if used[j] {
				continue
			}
			if singleFHIRPathOutputMatches(exp, act) {
				used[j] = true
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("unordered item[%d] (%s %q) not found in actual %+v for expression %q",
				i, exp.Type, exp.Body, actual, expr)
		}
	}
}

func singleFHIRPathOutputMatches(exp fhirPathXMLOut, act server.FHIRPathValue) bool {
	expText := strings.TrimSpace(exp.Body)
	expType := strings.TrimSpace(exp.Type)

	// Type check when XML specifies an explicit output type
	if expType != "" && act.FHIRType != "" {
		if !fhirOutputTypesCompatible(expType, act.FHIRType) {
			return false
		}
	}

	// Temporal outputs in tests-fhir-r4.xml begin with '@' (e.g. @2014-01, @T10:30:00.000)
	if strings.HasPrefix(expText, "@") {
		trimmedExp := strings.TrimPrefix(expText, "@")
		actStr := fmt.Sprintf("%v", act.Value)
		if act.DecText != "" {
			if strings.TrimPrefix(act.DecText, "@") == trimmedExp {
				return true
			}
		}
		if strings.TrimPrefix(actStr, "@") == trimmedExp || "T"+actStr == trimmedExp {
			return true
		}
		return false
	}

	switch expType {
	case "boolean":
		eb, err := strconv.ParseBool(expText)
		if err != nil {
			return false
		}
		ab, ok := act.Value.(bool)
		return ok && eb == ab

	case "integer", "positiveInt", "unsignedInt":
		ei, err := strconv.ParseInt(expText, 10, 64)
		if err != nil {
			return false
		}
		switch av := act.Value.(type) {
		case int:
			return int64(av) == ei
		case int64:
			return av == ei
		case float64:
			return av == float64(ei)
		}
		return false

	case "decimal":
		if act.DecText != "" && act.DecText == expText {
			return true
		}
		ef, err := strconv.ParseFloat(expText, 64)
		if err != nil {
			return false
		}
		var af float64
		switch av := act.Value.(type) {
		case float64:
			af = av
		case int:
			af = float64(av)
		case int64:
			af = float64(av)
		default:
			return false
		}
		return math.Abs(ef-af) <= 1e-9

	case "Quantity":
		if act.DecText == expText {
			return true
		}
		return normalizeQuantityOutput(expText) == normalizeQuantityOutput(act.DecText)

	case "date", "dateTime", "instant", "time":
		trimmedExp := strings.TrimPrefix(expText, "@")
		actStr := strings.TrimPrefix(fmt.Sprintf("%v", act.Value), "@")
		if act.DecText != "" && strings.TrimPrefix(act.DecText, "@") == trimmedExp {
			return true
		}
		return actStr == trimmedExp || "T"+actStr == trimmedExp

	case "string", "code", "id", "uri", "url", "canonical", "oid", "uuid", "markdown":
		return fmt.Sprintf("%v", act.Value) == expText
	}

	// Untyped <output> (e.g. LowBoundary, HighBoundary, Comparable, Precision)
	if act.DecText != "" && act.DecText == expText {
		return true
	}
	if fmt.Sprintf("%v", act.Value) == expText {
		return true
	}
	if ef, err := strconv.ParseFloat(expText, 64); err == nil {
		if af, ok := act.Value.(float64); ok {
			if act.DecText != "" {
				return act.DecText == expText
			}
			return math.Abs(ef-af) <= 1e-9
		}
	}
	return false
}

func fhirOutputTypesCompatible(expType, actType string) bool {
	if strings.EqualFold(expType, actType) {
		return true
	}
	strFamily := map[string]bool{
		"string": true, "code": true, "id": true, "uri": true, "url": true,
		"canonical": true, "oid": true, "uuid": true, "markdown": true,
	}
	if strFamily[expType] && strFamily[actType] {
		return true
	}
	intFamily := map[string]bool{"integer": true, "positiveInt": true, "unsignedInt": true}
	if intFamily[expType] && intFamily[actType] {
		return true
	}
	if (expType == "dateTime" || expType == "instant" || expType == "date") &&
		(actType == "dateTime" || actType == "instant" || actType == "date") {
		return true
	}
	if strings.EqualFold(expType, "Quantity") && (actType == "Age" || actType == "Distance" || actType == "Duration" || actType == "Count" || actType == "SimpleQuantity") {
		return true
	}
	return false
}

func normalizeQuantityOutput(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}
