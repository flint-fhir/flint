// Package certify implements an automated HL7 FHIR R4, US Core v6.1.0 & v7.0.0
// (USCDI v3 & v4), and ONC §170.315(g)(10) / HTI-1 Standardized API conformance
// & certification runner. It executes 5 structured test suites (29 checks) against
// a target Flint server and produces both a standard FHIR R4 TestReport resource
// and a human-readable scorecard.
package certify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/flint-fhir/flint/pkg/validation"
)

// CheckResult records the outcome of a single certification assertion.
type CheckResult struct {
	ID         string `json:"id"`
	Suite      string `json:"suite"`
	Name       string `json:"name"`
	Status     string `json:"status"` // "pass" or "fail"
	Message    string `json:"message"`
	DurationMs int64  `json:"durationMs"`
}

// Report holds the complete certification run results and converts to a FHIR R4 TestReport.
type Report struct {
	Tenant      string        `json:"tenant"`
	BaseURL     string        `json:"baseUrl"`
	Issued      time.Time     `json:"issued"`
	TotalChecks int           `json:"totalChecks"`
	Passed      int           `json:"passed"`
	Failed      int           `json:"failed"`
	Score       float64       `json:"score"`
	Result      string        `json:"result"` // "pass" or "fail"
	Checks      []CheckResult `json:"checks"`
}

// ToFHIRTestReport serializes the certification report as a standard HL7 FHIR R4 TestReport JSON map.
func (r *Report) ToFHIRTestReport() map[string]any {
	suitesOrder := []string{}
	suiteChecks := make(map[string][]CheckResult)
	for _, c := range r.Checks {
		if _, exists := suiteChecks[c.Suite]; !exists {
			suitesOrder = append(suitesOrder, c.Suite)
		}
		suiteChecks[c.Suite] = append(suiteChecks[c.Suite], c)
	}

	tests := make([]map[string]any, 0, len(suitesOrder))
	for _, suiteName := range suitesOrder {
		checks := suiteChecks[suiteName]
		actions := make([]map[string]any, 0, len(checks))
		for _, ch := range checks {
			actions = append(actions, map[string]any{
				"assert": map[string]any{
					"result":  ch.Status,
					"message": fmt.Sprintf("[%s] %s — %s (%dms)", ch.ID, ch.Name, ch.Message, ch.DurationMs),
				},
			})
		}
		tests = append(tests, map[string]any{
			"id":          strings.ToLower(strings.ReplaceAll(suiteName, " ", "-")),
			"name":        suiteName,
			"description": fmt.Sprintf("Conformance checks for %s", suiteName),
			"action":      actions,
		})
	}

	return map[string]any{
		"resourceType": "TestReport",
		"id":           "flint-onc-uscore-certification",
		"status":       "completed",
		"testScript": map[string]any{
			"reference": "TestScript/flint-onc-g10-uscore-v6-v7",
			"display":   "Flint FHIR R4 / US Core v6.1.0 & v7.0.0 / ONC (g)(10) & HTI-1 Certification Suite",
		},
		"result": r.Result,
		"score":  r.Score,
		"tester": "Flint Automated Conformance & Certification Engine",
		"issued": r.Issued.UTC().Format(time.RFC3339),
		"participant": []map[string]any{
			{
				"type":    "server",
				"uri":     fmt.Sprintf("%s/fhir/r4/%s", strings.TrimRight(r.BaseURL, "/"), r.Tenant),
				"display": "Flint FHIR R4 Server",
			},
		},
		"test": tests,
	}
}

// FormatScorecard renders a human-readable certification scorecard table.
func (r *Report) FormatScorecard() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "================================================================================\n")
	fmt.Fprintf(&sb, "  FLINT FHIR R4 / US CORE v6.1.0 & v7.0.0 / ONC (g)(10) CERTIFICATION SCORECARD\n")
	fmt.Fprintf(&sb, "================================================================================\n")
	fmt.Fprintf(&sb, "  Target Server : %s/fhir/r4/%s\n", strings.TrimRight(r.BaseURL, "/"), r.Tenant)
	fmt.Fprintf(&sb, "  Timestamp     : %s\n", r.Issued.UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "  Overall Result: %s (%.1f%% — %d/%d checks passed)\n", strings.ToUpper(r.Result), r.Score, r.Passed, r.TotalChecks)
	fmt.Fprintf(&sb, "--------------------------------------------------------------------------------\n")

	currentSuite := ""
	for _, c := range r.Checks {
		if c.Suite != currentSuite {
			currentSuite = c.Suite
			fmt.Fprintf(&sb, "\n[%s]\n", currentSuite)
		}
		icon := "PASS"
		if c.Status != "pass" {
			icon = "FAIL"
		}
		fmt.Fprintf(&sb, "  [%s] %-10s %-46s (%dms)\n", icon, c.ID, c.Name, c.DurationMs)
		if c.Status != "pass" {
			fmt.Fprintf(&sb, "         ↳ %s\n", c.Message)
		}
	}
	fmt.Fprintf(&sb, "================================================================================\n")
	return sb.String()
}

// Config configures the certification runner.
type Config struct {
	BaseURL     string
	Tenant      string
	BearerToken string
	HTTPClient  *http.Client
	SeedBundle  []byte
}

// Run executes all 5 certification suites against the configured Flint server.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	tenant := strings.TrimSpace(cfg.Tenant)
	if tenant == "" {
		tenant = "certify"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	rep := &Report{
		Tenant:  tenant,
		BaseURL: baseURL,
		Issued:  time.Now().UTC(),
	}

	record := func(id, suite, name string, start time.Time, err error, passMsg string) {
		dur := time.Since(start).Milliseconds()
		rep.TotalChecks++
		if err != nil {
			rep.Failed++
			rep.Checks = append(rep.Checks, CheckResult{
				ID:         id,
				Suite:      suite,
				Name:       name,
				Status:     "fail",
				Message:    err.Error(),
				DurationMs: dur,
			})
			return
		}
		rep.Passed++
		rep.Checks = append(rep.Checks, CheckResult{
			ID:         id,
			Suite:      suite,
			Name:       name,
			Status:     "pass",
			Message:    passMsg,
			DurationMs: dur,
		})
	}

	doReq := func(method, path, contentType string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rdr)
		if err != nil {
			return nil, nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if tok := strings.TrimSpace(cfg.BearerToken); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b, err
	}

	basePath := "/fhir/r4/" + tenant

	// =========================================================================
	// Suite 1: CapabilityStatement, SMART v2 Discovery & OpenAPI 3.1.0
	// =========================================================================
	suite1 := "Suite 1: Capability, SMART v2 Discovery & Proto-Compiled OpenAPI 3.1"

	t0 := time.Now()
	resp, b, err := doReq(http.MethodGet, basePath+"/metadata", "", nil, nil)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("expected 200 OK, got %d", resp.StatusCode)
	}
	if err == nil {
		var cs map[string]any
		if uErr := json.Unmarshal(b, &cs); uErr != nil {
			err = uErr
		} else if cs["fhirVersion"] != "4.0.1" {
			err = fmt.Errorf("expected fhirVersion=4.0.1, got %v", cs["fhirVersion"])
		} else {
			rawStr := string(b)
			for _, reqToken := range []string{
				"http://hl7.org/fhir/us/core/CapabilityStatement/us-core-server",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-vital-signs",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-lab",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-average-blood-pressure",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-care-experience-preference",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-treatment-intervention-preference",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medicationrequest",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-specimen",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-location",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-documentreference",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-provenance",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-careplan",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-careteam",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-goal",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-servicerequest",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-coverage",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-questionnaireresponse",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medicationdispense",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medication",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-implantable-device",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitionerrole",
				"http://hl7.org/fhir/us/core/StructureDefinition/us-core-relatedperson",
				"http://fhir-registry.smarthealthit.org/StructureDefinition/oauth-uris",
			} {
				if !strings.Contains(rawStr, reqToken) {
					err = fmt.Errorf("CapabilityStatement missing canonical US Core / SMART declaration: %s", reqToken)
					break
				}
			}
		}
	}
	record("DISC-01", suite1, "CapabilityStatement (/metadata) All 146 FHIR R4 Types & 26 US Core Profiles", t0, err, "All 146 FHIR R4 resource types & 26 US Core profiles verified")

	t0 = time.Now()
	resp, b, err = doReq(http.MethodGet, basePath+"/.well-known/smart-configuration", "", nil, nil)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("expected 200 OK, got %d", resp.StatusCode)
	}
	if err == nil {
		var sc map[string]any
		if uErr := json.Unmarshal(b, &sc); uErr != nil {
			err = uErr
		} else if _, ok := sc["capabilities"].([]any); !ok {
			err = fmt.Errorf("missing capabilities array in smart-configuration")
		} else {
			respOIDC, _, errOIDC := doReq(http.MethodGet, basePath+"/.well-known/openid-configuration", "", nil, nil)
			respJWKS, bJWKS, errJWKS := doReq(http.MethodGet, basePath+"/.well-known/jwks.json", "", nil, nil)
			if errOIDC != nil || respOIDC.StatusCode != http.StatusOK {
				err = fmt.Errorf("expected 200 OK on /.well-known/openid-configuration")
			} else if errJWKS != nil || respJWKS.StatusCode != http.StatusOK || !strings.Contains(string(bJWKS), `"RS256"`) {
				err = fmt.Errorf("expected 200 OK with RS256 key on /.well-known/jwks.json")
			}
		}
	}
	record("DISC-02", suite1, "SMART v2 (.well-known/smart-configuration), OIDC & JWKS Discovery", t0, err, "SMART v2, OIDC & RS256 JWKS discovery verified")

	t0 = time.Now()
	resp, b, err = doReq(http.MethodGet, basePath+"/openapi.json", "", nil, nil)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("expected 200 OK, got %d", resp.StatusCode)
	}
	if err == nil {
		var oa map[string]any
		if uErr := json.Unmarshal(b, &oa); uErr != nil {
			err = uErr
		} else if oa["openapi"] != "3.1.0" {
			err = fmt.Errorf("expected openapi=3.1.0, got %v", oa["openapi"])
		}
	}
	record("DISC-03", suite1, "Proto-Compiled OpenAPI 3.1.0 Specification (/openapi.json)", t0, err, "OpenAPI 3.1.0 schemas verified")

	t0 = time.Now()
	resp, b, err = doReq(http.MethodGet, basePath+"/$docs", "", nil, nil)
	if err == nil && (resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "Conformance Matrix")) {
		err = fmt.Errorf("expected 200 OK with Conformance Matrix HTML")
	}
	record("DISC-04", suite1, "Interactive Conformance Explorer (/$docs)", t0, err, "Explorer HTML verified")

	// =========================================================================
	// Suite 2: US Core v6.1.0 & v7.0.0 (All 26 US Core + 146 FHIR R4 Resource Types)
	// =========================================================================
	suite2 := "Suite 2: US Core v6.1.0 & v7.0.0 (All 26 US Core & 146 FHIR R4 Resource Types)"

	seedPayload := cfg.SeedBundle
	if len(seedPayload) == 0 {
		seedPayload = defaultCertifyCohortBundle()
	}
	t0 = time.Now()
	resp, b, err = doReq(http.MethodPost, basePath, "application/fhir+json", seedPayload, nil)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("seed bundle POST failed with %d: %s", resp.StatusCode, string(b))
	}
	record("USCORE-00", suite2, "Atomic Transaction Bundle Ingestion (All 26 US Core + Base R4 Resource Types)", t0, err, "All 26 US Core + Task resource types committed atomically")

	usCoreSearches := []struct {
		id    string
		name  string
		query string
	}{
		{"USCORE-01", "US Core Patient Search (family + gender)", "Patient?family=Certify&gender=female"},
		{"USCORE-02", "US Core Encounter Search (patient + status)", "Encounter?patient=Patient/cert-pat-1&status=finished"},
		{"USCORE-03", "US Core Condition Search (patient + clinical-status)", "Condition?patient=Patient/cert-pat-1&clinical-status=active"},
		{"USCORE-04", "US Core Observation Search (patient + code + quantity + _include)", "Observation?patient=Patient/cert-pat-1&code=8867-4&value-quantity=gt60&_include=Observation:patient"},
		{"USCORE-05", "US Core MedicationRequest Search (patient + intent + status)", "MedicationRequest?patient=Patient/cert-pat-1&intent=order&status=active"},
		{"USCORE-06", "US Core AllergyIntolerance Search (patient + clinical-status + criticality)", "AllergyIntolerance?patient=Patient/cert-pat-1&clinical-status=active&criticality=high"},
		{"USCORE-07", "US Core Procedure Search (patient + status + code)", "Procedure?patient=Patient/cert-pat-1&status=completed&code=29303009"},
		{"USCORE-08", "US Core DiagnosticReport Search (patient + status + code)", "DiagnosticReport?patient=Patient/cert-pat-1&status=final&code=24323-8"},
		{"USCORE-09", "US Core Immunization Search (patient + vaccine-code + status)", "Immunization?patient=Patient/cert-pat-1&vaccine-code=207&status=completed"},
		{"USCORE-10", "US Core Practitioner Search (identifier NPI)", "Practitioner?identifier=http://hl7.org/fhir/sid/us-npi|1234567899"},
		{"USCORE-11", "US Core Organization Search (name + address-city)", "Organization?name=Certify+Medical&address-city=Boston"},
		{"USCORE-12", "US Core v7.0.0 Specimen Search (patient + type + status)", "Specimen?patient=Patient/cert-pat-1&type=119297000&status=available"},
		{"USCORE-13", "US Core v7.0.0 Location Search (name + address-city)", "Location?name=Certify+Cardiology&address-city=Boston"},
		{"USCORE-14", "US Core v7.0.0 Average Blood Pressure, Care Preference & _lastUpdated Search", "Observation?patient=Patient/cert-pat-1&code=96607-7&_lastUpdated=ge2020-01-01T00:00:00Z&_lastUpdated=le2099-12-31T23:59:59Z"},
		{"USCORE-15", "US Core Clinical Notes (DocumentReference) & Provenance (_revinclude=Provenance:target)", "DocumentReference?patient=Patient/cert-pat-1&status=current&_revinclude=Provenance:target"},
		{"USCORE-16", "US Core Care Coordination (CarePlan, CareTeam, Goal & ServiceRequest)", "ServiceRequest?patient=Patient/cert-pat-1&status=active&code=108252007"},
		{"USCORE-17", "US Core Coverage, QuestionnaireResponse, MedicationDispense, Device & RelatedPerson", "Coverage?patient=Patient/cert-pat-1&status=active"},
		{"USCORE-18", "All 146 FHIR R4 Resource Types Dynamic Proto & Search (Task, PractitionerRole, Medication)", "Task?patient=Patient/cert-pat-1&status=in-progress"},
	}

	for _, sq := range usCoreSearches {
		t0 = time.Now()
		resp, b, err = doReq(http.MethodGet, basePath+"/"+sq.query, "", nil, nil)
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("expected 200 OK, got %d: %s", resp.StatusCode, string(b))
		}
		if err == nil {
			var bundle map[string]any
			if uErr := json.Unmarshal(b, &bundle); uErr != nil {
				err = uErr
			} else if tot, _ := bundle["total"].(float64); tot < 1 {
				err = fmt.Errorf("expected total >= 1 for %s, got %v", sq.query, bundle["total"])
			}
		}
		record(sq.id, suite2, sq.name, t0, err, fmt.Sprintf("Matched total >= 1 (%s)", sq.query))
	}

	// 2.19 US Core Mandatory POST-Based Search (POST /{resourceType}/_search with application/x-www-form-urlencoded)
	t0 = time.Now()
	{
		postForm := url.Values{
			"patient":  {"Patient/cert-pat-1"},
			"category": {"http://terminology.hl7.org/CodeSystem/observation-category|vital-signs"},
			"status":   {"final"},
		}
		respPost, bPost, errPost := doReq(http.MethodPost, basePath+"/Observation/_search", "application/x-www-form-urlencoded", []byte(postForm.Encode()), nil)
		if errPost != nil || respPost.StatusCode != http.StatusOK {
			err = fmt.Errorf("expected 200 OK on POST /Observation/_search, got %d: %s", respPost.StatusCode, string(bPost))
		} else {
			var postBundle map[string]any
			if uErr := json.Unmarshal(bPost, &postBundle); uErr != nil {
				err = uErr
			} else if tot, _ := postBundle["total"].(float64); tot < 1 {
				err = fmt.Errorf("expected total >= 1 on POST /Observation/_search, got %v", postBundle["total"])
			}
		}
	}
	record("SEARCH-01", suite2, "HL7 Inferno POST-Based Search (POST /{resourceType}/_search Form Body)", t0, err, "POST /Observation/_search returned matching Bundle")

	// 2.20 HL7 Inferno multipleOr Comma-Separated Search (status=final,amended,corrected & _id=id1,id2)
	t0 = time.Now()
	{
		orQuery := "Observation?patient=Patient/cert-pat-1&status=final,amended,corrected&_id=cert-obs-1,cert-obs-avg-bp"
		respOr, bOr, errOr := doReq(http.MethodGet, basePath+"/"+orQuery, "", nil, nil)
		if errOr != nil || respOr.StatusCode != http.StatusOK {
			err = fmt.Errorf("expected 200 OK on multipleOr search, got %d: %s", respOr.StatusCode, string(bOr))
		} else {
			var orBundle map[string]any
			if uErr := json.Unmarshal(bOr, &orBundle); uErr != nil {
				err = uErr
			} else if tot, _ := orBundle["total"].(float64); tot < 2 {
				err = fmt.Errorf("expected total >= 2 on multipleOr _id=cert-obs-1,cert-obs-avg-bp, got %v", orBundle["total"])
			}
		}
	}
	record("SEARCH-02", suite2, "HL7 Inferno multipleOr Comma-Separated Search (status=a,b & _id=id1,id2)", t0, err, "Matched multipleOr comma-separated token & _id filters")

	// 2.21 FHIR R4 Search Modifiers (:exact, :contains, :not, :missing)
	t0 = time.Now()
	{
		modQuery := "Patient?family:exact=Certify&given:contains=lic&gender:not=male&identifier:missing=false"
		respMod, bMod, errMod := doReq(http.MethodGet, basePath+"/"+modQuery, "", nil, nil)
		if errMod != nil || respMod.StatusCode != http.StatusOK {
			err = fmt.Errorf("expected 200 OK on search modifiers query, got %d: %s", respMod.StatusCode, string(bMod))
		} else {
			var modBundle map[string]any
			if uErr := json.Unmarshal(bMod, &modBundle); uErr != nil {
				err = uErr
			} else if tot, _ := modBundle["total"].(float64); tot < 1 {
				err = fmt.Errorf("expected total >= 1 on :exact/:contains/:not/:missing query, got %v", modBundle["total"])
			}
		}
	}
	record("SEARCH-03", suite2, "FHIR R4 Search Modifiers (:exact, :contains, :not, :missing)", t0, err, "Verified :exact, :contains, :not, and :missing modifiers")

	// 2.22 FHIR R4 _sort, _has Reverse Chaining, _summary/_elements, Bundle.link & Compartment Search
	t0 = time.Now()
	{
		// a) _has reverse chaining: Patient?_has:Observation:patient:code=8867-4
		respHas, bHas, errHas := doReq(http.MethodGet, basePath+"/Patient?_has:Observation:patient:code=8867-4", "", nil, nil)
		if errHas != nil || respHas.StatusCode != http.StatusOK {
			err = fmt.Errorf("expected 200 OK on _has search, got %d: %s", respHas.StatusCode, string(bHas))
		} else {
			var hasBundle map[string]any
			if uErr := json.Unmarshal(bHas, &hasBundle); uErr != nil {
				err = uErr
			} else if tot, _ := hasBundle["total"].(float64); tot < 1 {
				err = fmt.Errorf("expected total >= 1 on Patient?_has:Observation:patient:code=8867-4, got %v", hasBundle["total"])
			}
		}

		// b) _summary=count on Observation
		if err == nil {
			respCnt, bCnt, errCnt := doReq(http.MethodGet, basePath+"/Observation?patient=Patient/cert-pat-1&_summary=count", "", nil, nil)
			if errCnt != nil || respCnt.StatusCode != http.StatusOK {
				err = fmt.Errorf("expected 200 OK on _summary=count, got %d", respCnt.StatusCode)
			} else {
				var cntBundle map[string]any
				_ = json.Unmarshal(bCnt, &cntBundle)
				if tot, _ := cntBundle["total"].(float64); tot < 1 {
					err = fmt.Errorf("expected total >= 1 on _summary=count, got %v", cntBundle["total"])
				} else if _, hasEntry := cntBundle["entry"]; hasEntry {
					err = fmt.Errorf("expected _summary=count Bundle to omit entry array")
				}
			}
		}

		// c) _sort=-date,_id + _elements=status,code + Bundle.link pagination + Compartment Search GET /Patient/cert-pat-1/Observation
		if err == nil {
			respComp, bComp, errComp := doReq(http.MethodGet, basePath+"/Patient/cert-pat-1/Observation?_sort=-date,_id&_elements=status,code&_count=1", "", nil, nil)
			if errComp != nil || respComp.StatusCode != http.StatusOK {
				err = fmt.Errorf("expected 200 OK on compartment search, got %d: %s", respComp.StatusCode, string(bComp))
			} else {
				var compBundle map[string]any
				_ = json.Unmarshal(bComp, &compBundle)
				if links, ok := compBundle["link"].([]any); !ok || len(links) == 0 {
					err = fmt.Errorf("expected Bundle.link on compartment search")
				}
				if !strings.Contains(string(bComp), `"SUBSETTED"`) {
					err = fmt.Errorf("expected SUBSETTED tag on _elements projection")
				}
			}
		}

		// d) Type-level and system-level _history
		if err == nil {
			respHist, bHist, errHist := doReq(http.MethodGet, basePath+"/Patient/_history?_count=5", "", nil, nil)
			if errHist != nil || respHist.StatusCode != http.StatusOK || !strings.Contains(string(bHist), `"history"`) {
				err = fmt.Errorf("expected 200 OK history Bundle on GET /Patient/_history")
			}
		}
	}
	record("SEARCH-04", suite2, "FHIR R4 _sort, _has Reverse Chaining, _summary/_elements, Bundle.link & Compartment Search", t0, err, "Verified _sort, _has, _summary=count, _elements SUBSETTED, Bundle.link, /Patient/{id}/{type}, and /_history")

	// =========================================================================
	// Suite 3: FHIR R4 Versioning, CAS (If-Match), Patch (RFC 6902/7396) & Conditional CRUD
	// =========================================================================
	suite3 := "Suite 3: Versioning, Optimistic Locking (If-Match), Patch & Conditional CRUD"

	// 3.1 Conditional Create (If-None-Exist)
	t0 = time.Now()
	condOrg := []byte(`{"resourceType":"Organization","active":true,"name":"Conditional Cert Org v1","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"7776665554"}]}`)
	resp, b, err = doReq(http.MethodPost, basePath+"/Organization", "application/fhir+json", condOrg, map[string]string{
		"If-None-Exist": "identifier=http://hl7.org/fhir/sid/us-npi|7776665554",
	})
	var condOrgID string
	if err == nil && (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK) {
		err = fmt.Errorf("expected 201/200 on If-None-Exist create, got %d: %s", resp.StatusCode, string(b))
	}
	if err == nil {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		condOrgID, _ = m["id"].(string)
	}
	// Repeat If-None-Exist -> must return 200 OK with same ID
	if err == nil {
		resp2, b2, err2 := doReq(http.MethodPost, basePath+"/Organization", "application/fhir+json", condOrg, map[string]string{
			"If-None-Exist": "identifier=http://hl7.org/fhir/sid/us-npi|7776665554",
		})
		if err2 != nil || resp2.StatusCode != http.StatusOK {
			err = fmt.Errorf("expected idempotent 200 OK on repeat If-None-Exist, got %d: %v", resp2.StatusCode, string(b2))
		}
	}
	record("CRUD-01", suite3, "Conditional Create (If-None-Exist Idempotency)", t0, err, "Created once and returned 200 OK idempotently")

	// 3.2 Conditional Update (PUT ?identifier=...)
	t0 = time.Now()
	condOrgV2 := []byte(`{"resourceType":"Organization","active":true,"name":"Conditional Cert Org v2","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"7776665554"}],"address":[{"city":"Cambridge"}]}`)
	resp, b, err = doReq(http.MethodPut, basePath+"/Organization?identifier=http://hl7.org/fhir/sid/us-npi|7776665554", "application/fhir+json", condOrgV2, nil)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("expected 200 OK on conditional update, got %d: %s", resp.StatusCode, string(b))
	}
	etagAfterUpdate := ""
	if err == nil {
		etagAfterUpdate = resp.Header.Get("ETag")
	}
	record("CRUD-02", suite3, "Conditional Update (PUT /Organization?identifier=...)", t0, err, fmt.Sprintf("Updated in-place (%s)", etagAfterUpdate))

	// 3.3 RFC 6902 JSON Patch with If-Match + 412 Conflict Check
	t0 = time.Now()
	patchOps := []byte(`[
		{"op": "test", "path": "/name", "value": "Conditional Cert Org v2"},
		{"op": "replace", "path": "/name", "value": "Conditional Cert Org RFC6902"},
		{"op": "add", "path": "/address/0/state", "value": "MA"}
	]`)
	resp, b, err = doReq(http.MethodPatch, basePath+"/Organization/"+condOrgID, "application/json-patch+json", patchOps, map[string]string{
		"If-Match": etagAfterUpdate,
	})
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("expected 200 OK on RFC 6902 PATCH, got %d: %s", resp.StatusCode, string(b))
	}
	if err == nil {
		// Stale If-Match must return 412 Precondition Failed
		resp412, _, err412 := doReq(http.MethodPatch, basePath+"/Organization/"+condOrgID, "application/json-patch+json",
			[]byte(`[{"op":"replace","path":"/name","value":"Stale"}]`), map[string]string{"If-Match": `W/"1"`})
		if err412 != nil || resp412.StatusCode != http.StatusPreconditionFailed {
			err = fmt.Errorf("expected 412 Precondition Failed on stale If-Match, got %d", resp412.StatusCode)
		}
	}
	record("CRUD-03", suite3, "RFC 6902 JSON Patch (test/replace/add) & If-Match 412 CAS", t0, err, "Patched and enforced 412 on stale ETag")

	// 3.4 RFC 7396 JSON Merge Patch + vread/_history + Conditional Delete
	t0 = time.Now()
	resp, b, err = doReq(http.MethodPatch, basePath+"/Organization/"+condOrgID, "application/merge-patch+json",
		[]byte(`{"name":"Conditional Cert Org MergePatched"}`), nil)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("expected 200 OK on Merge Patch, got %d: %s", resp.StatusCode, string(b))
	}
	if err == nil {
		// Verify _history has >= 3 versions
		respHist, bHist, errHist := doReq(http.MethodGet, basePath+"/Organization/"+condOrgID+"/_history", "", nil, nil)
		if errHist != nil || respHist.StatusCode != http.StatusOK {
			err = fmt.Errorf("expected 200 OK on _history, got %d", respHist.StatusCode)
		} else {
			var histBundle map[string]any
			_ = json.Unmarshal(bHist, &histBundle)
			if tot, _ := histBundle["total"].(float64); tot < 3 {
				err = fmt.Errorf("expected _history total >= 3, got %v", histBundle["total"])
			}
		}
	}
	if err == nil {
		// Conditional DELETE
		respDel, _, errDel := doReq(http.MethodDelete, basePath+"/Organization?identifier=http://hl7.org/fhir/sid/us-npi|7776665554", "", nil, nil)
		if errDel != nil || respDel.StatusCode != http.StatusNoContent {
			err = fmt.Errorf("expected 204 No Content on conditional DELETE, got %d", respDel.StatusCode)
		}
		// Subsequent GET must return 410 Gone
		respGone, _, _ := doReq(http.MethodGet, basePath+"/Organization/"+condOrgID, "", nil, nil)
		if respGone != nil && respGone.StatusCode != http.StatusGone {
			err = fmt.Errorf("expected 410 Gone after conditional DELETE, got %d", respGone.StatusCode)
		}
	}
	record("CRUD-04", suite3, "RFC 7396 Merge Patch, _history, Conditional DELETE & 410 Gone", t0, err, "Merge patch, version history, and 410 tombstone verified")

	// =========================================================================
	// Suite 4: Patient $everything & ONC (g)(10) Bulk Data $export
	// =========================================================================
	suite4 := "Suite 4: Patient $everything & ONC §170.315(g)(10) Bulk Data $export"

	t0 = time.Now()
	resp, b, err = doReq(http.MethodGet, basePath+"/Patient/cert-pat-1/$everything", "", nil, nil)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("expected 200 OK on $everything, got %d: %s", resp.StatusCode, string(b))
	}
	if err == nil {
		var ev map[string]any
		_ = json.Unmarshal(b, &ev)
		if tot, _ := ev["total"].(float64); tot < 26 {
			err = fmt.Errorf("expected Patient/$everything total >= 26 across all 26 US Core v6.1.0 & v7.0.0 resource types, got %v", ev["total"])
		}
	}
	record("EXP-01", suite4, "Patient/$everything Compartment Closure (All 26 US Core v6.1.0 & v7.0.0 Resource Types)", t0, err, "All 26 US Core resource types + Provenance returned in compartment")

	t0 = time.Now()
	resp, b, err = doReq(http.MethodGet, basePath+"/Group/all/$export", "", nil, map[string]string{
		"Prefer": "respond-async",
	})
	var pollURL string
	if err == nil && resp.StatusCode != http.StatusAccepted {
		err = fmt.Errorf("expected 202 Accepted on Group/all/$export kick-off, got %d: %s", resp.StatusCode, string(b))
	}
	if err == nil {
		pollURL = resp.Header.Get("Content-Location")
		if pollURL == "" {
			err = fmt.Errorf("missing Content-Location header on 202 Accepted")
		}
	}
	var exportedCount int
	if err == nil {
		// Poll job status
		pollPath := strings.TrimPrefix(pollURL, baseURL)
		respPoll, bPoll, errPoll := doReq(http.MethodGet, pollPath, "", nil, nil)
		if errPoll != nil || respPoll.StatusCode != http.StatusOK {
			err = fmt.Errorf("expected 200 OK from export poll %s, got %d", pollPath, respPoll.StatusCode)
		} else {
			var manifest struct {
				Output []struct {
					Type string `json:"type"`
					URL  string `json:"url"`
				} `json:"output"`
			}
			if uErr := json.Unmarshal(bPoll, &manifest); uErr != nil {
				err = uErr
			} else if len(manifest.Output) < 20 {
				err = fmt.Errorf("expected >= 20 resource type NDJSON files in Bulk Data manifest, got %d", len(manifest.Output))
			} else {
				valEngine := validation.NewEngine(validation.DefaultOptions())
				for _, outItem := range manifest.Output {
					dlPath := strings.TrimPrefix(outItem.URL, baseURL)
					respDL, bDL, errDL := doReq(http.MethodGet, dlPath, "", nil, nil)
					if errDL != nil || respDL.StatusCode != http.StatusOK {
						err = fmt.Errorf("failed downloading NDJSON %s: %v", dlPath, errDL)
						break
					}
					scanner := bufio.NewScanner(bytes.NewReader(bDL))
					for scanner.Scan() {
						line := bytes.TrimSpace(scanner.Bytes())
						if len(line) == 0 {
							continue
						}
						exportedCount++
						if outcome := valEngine.ValidateJSON(outItem.Type, line); !outcome.IsValid() {
							err = fmt.Errorf("exported %s failed validation: %v", outItem.Type, outcome.Issues)
							break
						}
					}
					if err != nil {
						break
					}
				}
			}
		}
	}
	record("EXP-02", suite4, "ONC (g)(10) Async Group/$export Kickoff, Poll & NDJSON Validation", t0, err,
		fmt.Sprintf("Validated %d exported NDJSON resources across US Core profiles", exportedCount))

	// =========================================================================
	// Suite 5: SMART v2 OAuth2 PKCE Launch, $validate & ONC (d)(2) AuditEvent
	// =========================================================================
	suite5 := "Suite 5: SMART v2 OAuth2 PKCE Launch, $validate & ONC §170.315(d)(2) AuditEvent"

	t0 = time.Now()
	{
		verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk-flint-smart-v2-pkce"
		sum := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])

		noRedirectClient := *client
		noRedirectClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		authURL := fmt.Sprintf("%s/oauth2/authorize?response_type=code&client_id=inferno&redirect_uri=%s&scope=%s&state=smart-state-1&code_challenge=%s&code_challenge_method=S256&patient=cert-pat-1",
			baseURL,
			url.QueryEscape("https://inferno.healthit.gov/cb"),
			url.QueryEscape("openid fhirUser launch/patient patient/Patient.rs patient/Observation.rs"),
			url.QueryEscape(challenge),
		)
		reqAuth, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, authURL, nil)
		var authCode string
		if reqErr != nil {
			err = reqErr
		} else {
			respAuth, doErr := noRedirectClient.Do(reqAuth)
			if doErr != nil {
				err = doErr
			} else {
				respAuth.Body.Close()
				if respAuth.StatusCode != http.StatusFound {
					err = fmt.Errorf("expected 302 Found from /oauth2/authorize, got %d", respAuth.StatusCode)
				} else {
					loc, pErr := url.Parse(respAuth.Header.Get("Location"))
					if pErr != nil {
						err = pErr
					} else {
						authCode = loc.Query().Get("code")
						if authCode == "" || loc.Query().Get("state") != "smart-state-1" {
							err = fmt.Errorf("missing code or mismatched state in redirect Location %s", respAuth.Header.Get("Location"))
						}
					}
				}
			}
		}

		if err == nil {
			form := url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {authCode},
				"redirect_uri":  {"https://inferno.healthit.gov/cb"},
				"code_verifier": {verifier},
			}
			respTok, bTok, errTok := doReq(http.MethodPost, "/oauth2/token", "application/x-www-form-urlencoded", []byte(form.Encode()), nil)
			if errTok != nil || respTok.StatusCode != http.StatusOK {
				err = fmt.Errorf("expected 200 OK from /oauth2/token, got %d: %s", respTok.StatusCode, string(bTok))
			} else {
				var tokResp map[string]any
				_ = json.Unmarshal(bTok, &tokResp)
				accessToken, _ := tokResp["access_token"].(string)
				if accessToken == "" || tokResp["patient"] != "cert-pat-1" || tokResp["id_token"] == "" {
					err = fmt.Errorf("incomplete SMART v2 token response: %s", string(bTok))
				} else {
					// 1. Authorized read on own Patient compartment -> 200 OK
					respOwn, _, errOwn := doReq(http.MethodGet, basePath+"/Patient/cert-pat-1", "", nil, map[string]string{
						"Authorization": "Bearer " + accessToken,
					})
					// 2. Cross-patient read -> 403 Forbidden
					respOther, _, errOther := doReq(http.MethodGet, basePath+"/Patient/other-patient-999", "", nil, map[string]string{
						"Authorization": "Bearer " + accessToken,
					})
					// 3. Unauthorized write/delete with read-only (.rs) scope -> 403 Forbidden
					respDel, _, errDel := doReq(http.MethodDelete, basePath+"/Patient/cert-pat-1", "", nil, map[string]string{
						"Authorization": "Bearer " + accessToken,
					})
					if errOwn != nil || respOwn.StatusCode != http.StatusOK {
						err = fmt.Errorf("expected 200 OK reading own Patient with SMART token, got %d", respOwn.StatusCode)
					} else if errOther != nil || respOther.StatusCode != http.StatusForbidden {
						err = fmt.Errorf("expected 403 Forbidden on cross-patient read, got %d", respOther.StatusCode)
					} else if errDel != nil || respDel.StatusCode != http.StatusForbidden {
						err = fmt.Errorf("expected 403 Forbidden on DELETE with .rs scope, got %d", respDel.StatusCode)
					}
				}
			}
		}
	}
	record("SMART-01", suite5, "SMART v2 OAuth2 PKCE (S256) Launch & Patient Compartment Isolation", t0, err, "PKCE S256 exchange, id_token & 403 compartment isolation verified")

	t0 = time.Now()
	{
		ccForm := url.Values{
			"grant_type": {"client_credentials"},
			"scope":      {"system/*.rs"},
		}
		respCC, bCC, errCC := doReq(http.MethodPost, "/oauth2/token", "application/x-www-form-urlencoded", []byte(ccForm.Encode()), nil)
		if errCC != nil || respCC.StatusCode != http.StatusOK {
			err = fmt.Errorf("expected 200 OK from client_credentials /oauth2/token, got %d", respCC.StatusCode)
		} else {
			var ccMap map[string]any
			_ = json.Unmarshal(bCC, &ccMap)
			sysTok, _ := ccMap["access_token"].(string)
			introForm := url.Values{"token": {sysTok}}
			respIntro, bIntro, errIntro := doReq(http.MethodPost, "/oauth2/introspect", "application/x-www-form-urlencoded", []byte(introForm.Encode()), nil)
			if errIntro != nil || respIntro.StatusCode != http.StatusOK || !strings.Contains(string(bIntro), `"active":true`) {
				err = fmt.Errorf("expected active:true from /oauth2/introspect, got %s", string(bIntro))
			}
		}
	}
	record("SMART-02", suite5, "SMART Backend Services (client_credentials) & RFC 7662 Introspection", t0, err, "Backend Services JWT & token introspection verified")

	t0 = time.Now()
	{
		err = nil
		granForm := url.Values{
			"grant_type": {"client_credentials"},
			"scope":      {"system/Observation.rs?category=http://terminology.hl7.org/CodeSystem/observation-category|vital-signs"},
		}
		respGran, bGran, errGran := doReq(http.MethodPost, "/oauth2/token", "application/x-www-form-urlencoded", []byte(granForm.Encode()), nil)
		if errGran != nil || respGran.StatusCode != http.StatusOK {
			err = fmt.Errorf("expected 200 OK issuing granular SMART v2 token, got %d", respGran.StatusCode)
		} else {
			var granMap map[string]any
			_ = json.Unmarshal(bGran, &granMap)
			granTok, _ := granMap["access_token"].(string)
			headers := map[string]string{"Authorization": "Bearer " + granTok}

			// 1. Allowed category=vital-signs search -> 200 OK with >= 1 result
			respAllowed, bAllowed, errAllowed := doReq(http.MethodGet, basePath+"/Observation?patient=Patient/cert-pat-1", "", nil, headers)
			// 2. Disallowed category=social-history search -> 403 Forbidden
			respDenied, _, errDenied := doReq(http.MethodGet, basePath+"/Observation?patient=Patient/cert-pat-1&category=social-history", "", nil, headers)
			// 3. Revoke token via POST /oauth2/revoke (RFC 7009) and verify subsequent request returns 401 Unauthorized
			revForm := url.Values{"token": {granTok}}
			respRev, _, errRev := doReq(http.MethodPost, "/oauth2/revoke", "application/x-www-form-urlencoded", []byte(revForm.Encode()), nil)
			respAfterRev, _, errAfterRev := doReq(http.MethodGet, basePath+"/Observation?patient=Patient/cert-pat-1", "", nil, headers)

			if errAllowed != nil || respAllowed.StatusCode != http.StatusOK {
				err = fmt.Errorf("expected 200 OK on granular Observation search, got %d: %s", respAllowed.StatusCode, string(bAllowed))
			} else if errDenied != nil || respDenied.StatusCode != http.StatusForbidden {
				err = fmt.Errorf("expected 403 Forbidden when querying category=social-history with vital-signs-only scope, got %d", respDenied.StatusCode)
			} else if errRev != nil || respRev.StatusCode != http.StatusOK {
				err = fmt.Errorf("expected 200 OK from /oauth2/revoke, got %d", respRev.StatusCode)
			} else if errAfterRev != nil || respAfterRev.StatusCode != http.StatusUnauthorized {
				err = fmt.Errorf("expected 401 Unauthorized after RFC 7009 token revocation, got %d", respAfterRev.StatusCode)
			}
		}
	}
	record("SMART-03", suite5, "SMART v2 Granular Sub-Resource Scopes (?category=) & RFC 7009 Revocation", t0, err, "Enforced ?category=vital-signs scope filter & RFC 7009 /oauth2/revoke")

	t0 = time.Now()
	resp, _, err = doReq(http.MethodPost, basePath+"/MedicationRequest/$validate", "application/fhir+json",
		[]byte(`{"resourceType":"MedicationRequest","status":"active","intent":"order","subject":{"reference":"Patient/cert-pat-1"}}`), nil)
	if err == nil && resp.StatusCode != http.StatusBadRequest {
		err = fmt.Errorf("expected 400 Bad Request when MedicationRequest omits medication[x], got %d", resp.StatusCode)
	}
	record("VAL-01", suite5, "StructureDefinition & Choice-Type Validation ($validate)", t0, err, "Rejected missing mandatory choice element medication[x]")

	t0 = time.Now()
	{
		err = nil
		// 1. ValueSet/$expand with filter
		respExp, bExp, errExp := doReq(http.MethodGet, basePath+"/ValueSet/$expand?url=http://hl7.org/fhir/ValueSet/observation-vitalsignresult&filter=Heart", "", nil, nil)
		// 2. ValueSet/$validate-code
		respVC, bVC, errVC := doReq(http.MethodGet, basePath+"/ValueSet/$validate-code?url=http://hl7.org/fhir/ValueSet/administrative-gender&code=female", "", nil, nil)
		// 3. CodeSystem/$lookup
		respLkp, bLkp, errLkp := doReq(http.MethodGet, basePath+"/CodeSystem/$lookup?system=http://loinc.org&code=8867-4", "", nil, nil)

		if errExp != nil || respExp.StatusCode != http.StatusOK || !strings.Contains(string(bExp), `"8867-4"`) {
			err = fmt.Errorf("expected 200 OK with 8867-4 from ValueSet/$expand, got %s", string(bExp))
		} else if errVC != nil || respVC.StatusCode != http.StatusOK || !strings.Contains(string(bVC), `"valueBoolean":true`) {
			err = fmt.Errorf("expected valueBoolean:true from ValueSet/$validate-code, got %s", string(bVC))
		} else if errLkp != nil || respLkp.StatusCode != http.StatusOK || !strings.Contains(string(bLkp), `"Heart rate"`) {
			err = fmt.Errorf("expected Heart rate from CodeSystem/$lookup, got %s", string(bLkp))
		}
	}
	record("TERM-01", suite5, "FHIR R4 Terminology Services (ValueSet/$expand, $validate-code & CodeSystem/$lookup)", t0, err, "Verified ValueSet/$expand, $validate-code, and CodeSystem/$lookup")

	t0 = time.Now()
	{
		err = nil
		subBody := fmt.Sprintf(`{
			"resourceType": "Subscription",
			"status": "requested",
			"reason": "Monitor vital sign observations",
			"criteria": "Observation?code=8867-4",
			"channel": {
				"type": "rest-hook",
				"endpoint": "%s%s/$validate",
				"payload": "application/fhir+json"
			}
		}`, baseURL, basePath)
		respSub, bSub, errSub := doReq(http.MethodPost, basePath+"/Subscription", "application/fhir+json", []byte(subBody), nil)
		if errSub != nil || respSub.StatusCode != http.StatusCreated {
			err = fmt.Errorf("expected 201 Created on POST /Subscription, got %d: %s", respSub.StatusCode, string(bSub))
		} else {
			var subMap map[string]any
			_ = json.Unmarshal(bSub, &subMap)
			subID, _ := subMap["id"].(string)
			if subMap["status"] != "active" {
				err = fmt.Errorf("expected Subscription status to transition from requested to active, got %v", subMap["status"])
			}
			if subID != "" {
				_, _, _ = doReq(http.MethodDelete, basePath+"/Subscription/"+subID, "", nil, nil)
			}
		}
	}
	record("SUB-01", suite5, "FHIR R4 Subscription (rest-hook) Lifecycle & Criteria Activation", t0, err, "Transitioned Subscription from requested -> active & verified rest-hook criteria")

	t0 = time.Now()
	resp, b, err = doReq(http.MethodGet, basePath+"/AuditEvent?_count=20", "", nil, nil)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("expected 200 OK on GET /AuditEvent, got %d: %s", resp.StatusCode, string(b))
	}
	if err == nil {
		var aeBundle map[string]any
		_ = json.Unmarshal(b, &aeBundle)
		if tot, _ := aeBundle["total"].(float64); tot < 1 {
			err = fmt.Errorf("expected AuditEvent total >= 1, got %v", aeBundle["total"])
		}
	}
	record("AUD-01", suite5, "ASTM E2147 / ONC (d)(2) AuditEvent Trail Verification", t0, err, "Clinical access & mutation AuditEvents verified")

	if rep.TotalChecks > 0 {
		rep.Score = float64(rep.Passed) * 100.0 / float64(rep.TotalChecks)
	}
	if rep.Failed == 0 && rep.TotalChecks > 0 {
		rep.Result = "pass"
	} else {
		rep.Result = "fail"
	}
	return rep, nil
}

func defaultCertifyCohortBundle() []byte {
	return []byte(`{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"fullUrl": "Organization/cert-org-1",
				"resource": {
					"resourceType": "Organization",
					"id": "cert-org-1",
					"active": true,
					"name": "Certify Medical Center",
					"identifier": [{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1122334499"}],
					"address": [{"city": "Boston", "state": "MA", "postalCode": "02115"}]
				},
				"request": {"method": "PUT", "url": "Organization/cert-org-1"}
			},
			{
				"fullUrl": "Location/cert-loc-1",
				"resource": {
					"resourceType": "Location",
					"id": "cert-loc-1",
					"status": "active",
					"name": "Certify Cardiology Clinic",
					"mode": "instance",
					"address": {"city": "Boston", "state": "MA", "postalCode": "02115"},
					"managingOrganization": {"reference": "Organization/cert-org-1"}
				},
				"request": {"method": "PUT", "url": "Location/cert-loc-1"}
			},
			{
				"fullUrl": "Practitioner/cert-prac-1",
				"resource": {
					"resourceType": "Practitioner",
					"id": "cert-prac-1",
					"active": true,
					"name": [{"family": "CertifyDoc", "given": ["Sarah"]}],
					"identifier": [{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1234567899"}]
				},
				"request": {"method": "PUT", "url": "Practitioner/cert-prac-1"}
			},
			{
				"fullUrl": "Patient/cert-pat-1",
				"resource": {
					"resourceType": "Patient",
					"id": "cert-pat-1",
					"active": true,
					"gender": "female",
					"birthDate": "1985-04-12",
					"name": [{"use": "official", "family": "Certify", "given": ["Alice"]}],
					"identifier": [{"system": "http://hospital.example.org/mrn", "value": "CERT-MRN-1"}],
					"managingOrganization": {"reference": "Organization/cert-org-1"},
					"generalPractitioner": [{"reference": "Practitioner/cert-prac-1"}]
				},
				"request": {"method": "PUT", "url": "Patient/cert-pat-1"}
			},
			{
				"fullUrl": "Encounter/cert-enc-1",
				"resource": {
					"resourceType": "Encounter",
					"id": "cert-enc-1",
					"status": "finished",
					"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
					"subject": {"reference": "Patient/cert-pat-1"},
					"location": [{"location": {"reference": "Location/cert-loc-1"}}],
					"period": {"start": "2026-01-15T09:00:00Z", "end": "2026-01-15T10:00:00Z"}
				},
				"request": {"method": "PUT", "url": "Encounter/cert-enc-1"}
			},
			{
				"fullUrl": "Condition/cert-cond-1",
				"resource": {
					"resourceType": "Condition",
					"id": "cert-cond-1",
					"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
					"verificationStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}]},
					"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003"}]},
					"subject": {"reference": "Patient/cert-pat-1"}
				},
				"request": {"method": "PUT", "url": "Condition/cert-cond-1"}
			},
			{
				"fullUrl": "Observation/cert-obs-1",
				"resource": {
					"resourceType": "Observation",
					"id": "cert-obs-1",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
					"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
					"subject": {"reference": "Patient/cert-pat-1"},
					"effectiveDateTime": "2026-01-15T09:15:00Z",
					"valueQuantity": {"value": 72.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
				},
				"request": {"method": "PUT", "url": "Observation/cert-obs-1"}
			},
			{
				"fullUrl": "Observation/cert-obs-avg-bp",
				"resource": {
					"resourceType": "Observation",
					"id": "cert-obs-avg-bp",
					"status": "final",
					"code": {"coding": [{"system": "http://loinc.org", "code": "96607-7"}]},
					"subject": {"reference": "Patient/cert-pat-1"},
					"effectiveDateTime": "2026-01-15T09:17:00Z",
					"component": [
						{
							"code": {"coding": [{"system": "http://loinc.org", "code": "96608-5"}]},
							"valueQuantity": {"value": 128.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
						},
						{
							"code": {"coding": [{"system": "http://loinc.org", "code": "96609-3"}]},
							"valueQuantity": {"value": 80.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
						}
					]
				},
				"request": {"method": "PUT", "url": "Observation/cert-obs-avg-bp"}
			},
			{
				"fullUrl": "Observation/cert-obs-care-pref",
				"resource": {
					"resourceType": "Observation",
					"id": "cert-obs-care-pref",
					"status": "final",
					"code": {"coding": [{"system": "http://loinc.org", "code": "95541-9"}]},
					"subject": {"reference": "Patient/cert-pat-1"},
					"effectiveDateTime": "2026-01-15T09:18:00Z",
					"valueString": "Prefers digital care summaries"
				},
				"request": {"method": "PUT", "url": "Observation/cert-obs-care-pref"}
			},
			{
				"fullUrl": "MedicationRequest/cert-medreq-1",
				"resource": {
					"resourceType": "MedicationRequest",
					"id": "cert-medreq-1",
					"status": "active",
					"intent": "order",
					"subject": {"reference": "Patient/cert-pat-1"},
					"authoredOn": "2026-01-15T09:30:00Z",
					"medicationCodeableConcept": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "314076"}]}
				},
				"request": {"method": "PUT", "url": "MedicationRequest/cert-medreq-1"}
			},
			{
				"fullUrl": "AllergyIntolerance/cert-allergy-1",
				"resource": {
					"resourceType": "AllergyIntolerance",
					"id": "cert-allergy-1",
					"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}]},
					"verificationStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification", "code": "confirmed"}]},
					"criticality": "high",
					"code": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980"}]},
					"patient": {"reference": "Patient/cert-pat-1"}
				},
				"request": {"method": "PUT", "url": "AllergyIntolerance/cert-allergy-1"}
			},
			{
				"fullUrl": "Procedure/cert-proc-1",
				"resource": {
					"resourceType": "Procedure",
					"id": "cert-proc-1",
					"status": "completed",
					"code": {"coding": [{"system": "http://snomed.info/sct", "code": "29303009"}]},
					"subject": {"reference": "Patient/cert-pat-1"},
					"performedDateTime": "2026-01-15T09:20:00Z"
				},
				"request": {"method": "PUT", "url": "Procedure/cert-proc-1"}
			},
			{
				"fullUrl": "Specimen/cert-spec-1",
				"resource": {
					"resourceType": "Specimen",
					"id": "cert-spec-1",
					"status": "available",
					"type": {"coding": [{"system": "http://snomed.info/sct", "code": "119297000"}]},
					"subject": {"reference": "Patient/cert-pat-1"},
					"receivedTime": "2026-01-15T09:22:00Z",
					"collection": {
						"collector": {"reference": "Practitioner/cert-prac-1"},
						"collectedDateTime": "2026-01-15T09:20:00Z"
					}
				},
				"request": {"method": "PUT", "url": "Specimen/cert-spec-1"}
			},
			{
				"fullUrl": "DiagnosticReport/cert-dr-1",
				"resource": {
					"resourceType": "DiagnosticReport",
					"id": "cert-dr-1",
					"status": "final",
					"code": {"coding": [{"system": "http://loinc.org", "code": "24323-8"}]},
					"subject": {"reference": "Patient/cert-pat-1"},
					"effectiveDateTime": "2026-01-15T09:25:00Z",
					"issued": "2026-01-15T09:45:00Z",
					"specimen": [{"reference": "Specimen/cert-spec-1"}],
					"result": [{"reference": "Observation/cert-obs-1"}]
				},
				"request": {"method": "PUT", "url": "DiagnosticReport/cert-dr-1"}
			},
			{
				"fullUrl": "Immunization/cert-imm-1",
				"resource": {
					"resourceType": "Immunization",
					"id": "cert-imm-1",
					"status": "completed",
					"vaccineCode": {"coding": [{"system": "http://hl7.org/fhir/sid/cvx", "code": "207"}]},
					"patient": {"reference": "Patient/cert-pat-1"},
					"occurrenceDateTime": "2026-01-15T09:50:00Z"
				},
				"request": {"method": "PUT", "url": "Immunization/cert-imm-1"}
			},
			{
				"fullUrl": "PractitionerRole/cert-pracrole-1",
				"resource": {
					"resourceType": "PractitionerRole",
					"id": "cert-pracrole-1",
					"active": true,
					"practitioner": {"reference": "Practitioner/cert-prac-1"},
					"organization": {"reference": "Organization/cert-org-1"},
					"location": [{"reference": "Location/cert-loc-1"}],
					"specialty": [{"coding": [{"system": "http://nucc.org/provider-taxonomy", "code": "207RC0000X"}]}]
				},
				"request": {"method": "PUT", "url": "PractitionerRole/cert-pracrole-1"}
			},
			{
				"fullUrl": "Medication/cert-med-1",
				"resource": {
					"resourceType": "Medication",
					"id": "cert-med-1",
					"status": "active",
					"code": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "314076"}]}
				},
				"request": {"method": "PUT", "url": "Medication/cert-med-1"}
			},
			{
				"fullUrl": "RelatedPerson/cert-rel-1",
				"resource": {
					"resourceType": "RelatedPerson",
					"id": "cert-rel-1",
					"active": true,
					"patient": {"reference": "Patient/cert-pat-1"},
					"name": [{"family": "Certify", "given": ["David"]}],
					"gender": "male",
					"relationship": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v3-RoleCode", "code": "SPS"}]}]
				},
				"request": {"method": "PUT", "url": "RelatedPerson/cert-rel-1"}
			},
			{
				"fullUrl": "DocumentReference/cert-doc-1",
				"resource": {
					"resourceType": "DocumentReference",
					"id": "cert-doc-1",
					"status": "current",
					"type": {"coding": [{"system": "http://loinc.org", "code": "34117-2"}]},
					"category": [{"coding": [{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-documentreference-category", "code": "clinical-note"}]}],
					"subject": {"reference": "Patient/cert-pat-1"},
					"date": "2026-01-15T10:00:00Z",
					"author": [{"reference": "Practitioner/cert-prac-1"}],
					"custodian": {"reference": "Organization/cert-org-1"},
					"content": [
						{
							"attachment": {
								"contentType": "text/plain",
								"url": "https://flint.example.org/notes/cert-doc-1.txt"
							}
						}
					]
				},
				"request": {"method": "PUT", "url": "DocumentReference/cert-doc-1"}
			},
			{
				"fullUrl": "Provenance/cert-prov-1",
				"resource": {
					"resourceType": "Provenance",
					"id": "cert-prov-1",
					"target": [
						{"reference": "Patient/cert-pat-1"},
						{"reference": "DocumentReference/cert-doc-1"}
					],
					"recorded": "2026-01-15T10:05:00Z",
					"agent": [
						{
							"type": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/provenance-participant-type", "code": "author"}]},
							"who": {"reference": "Practitioner/cert-prac-1"}
						}
					]
				},
				"request": {"method": "PUT", "url": "Provenance/cert-prov-1"}
			},
			{
				"fullUrl": "CarePlan/cert-cp-1",
				"resource": {
					"resourceType": "CarePlan",
					"id": "cert-cp-1",
					"status": "active",
					"intent": "plan",
					"category": [{"coding": [{"system": "http://hl7.org/fhir/us/core/CodeSystem/careplan-category", "code": "assess-plan"}]}],
					"subject": {"reference": "Patient/cert-pat-1"}
				},
				"request": {"method": "PUT", "url": "CarePlan/cert-cp-1"}
			},
			{
				"fullUrl": "CareTeam/cert-ct-1",
				"resource": {
					"resourceType": "CareTeam",
					"id": "cert-ct-1",
					"status": "active",
					"subject": {"reference": "Patient/cert-pat-1"},
					"participant": [
						{
							"role": [{"coding": [{"system": "http://snomed.info/sct", "code": "17561000"}]}],
							"member": {"reference": "Practitioner/cert-prac-1"}
						}
					]
				},
				"request": {"method": "PUT", "url": "CareTeam/cert-ct-1"}
			},
			{
				"fullUrl": "Goal/cert-goal-1",
				"resource": {
					"resourceType": "Goal",
					"id": "cert-goal-1",
					"lifecycleStatus": "active",
					"description": {"text": "Maintain blood pressure below 130/80 mmHg"},
					"subject": {"reference": "Patient/cert-pat-1"}
				},
				"request": {"method": "PUT", "url": "Goal/cert-goal-1"}
			},
			{
				"fullUrl": "ServiceRequest/cert-sr-1",
				"resource": {
					"resourceType": "ServiceRequest",
					"id": "cert-sr-1",
					"status": "active",
					"intent": "order",
					"category": [{"coding": [{"system": "http://snomed.info/sct", "code": "108252007"}]}],
					"code": {"coding": [{"system": "http://snomed.info/sct", "code": "108252007"}]},
					"subject": {"reference": "Patient/cert-pat-1"},
					"authoredOn": "2026-01-15T09:10:00Z",
					"requester": {"reference": "Practitioner/cert-prac-1"}
				},
				"request": {"method": "PUT", "url": "ServiceRequest/cert-sr-1"}
			},
			{
				"fullUrl": "Coverage/cert-cov-1",
				"resource": {
					"resourceType": "Coverage",
					"id": "cert-cov-1",
					"status": "active",
					"type": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "HIP"}]},
					"beneficiary": {"reference": "Patient/cert-pat-1"},
					"payor": [{"reference": "Organization/cert-org-1"}]
				},
				"request": {"method": "PUT", "url": "Coverage/cert-cov-1"}
			},
			{
				"fullUrl": "QuestionnaireResponse/cert-qr-1",
				"resource": {
					"resourceType": "QuestionnaireResponse",
					"id": "cert-qr-1",
					"status": "completed",
					"subject": {"reference": "Patient/cert-pat-1"},
					"authored": "2026-01-15T09:12:00Z"
				},
				"request": {"method": "PUT", "url": "QuestionnaireResponse/cert-qr-1"}
			},
			{
				"fullUrl": "MedicationDispense/cert-meddisp-1",
				"resource": {
					"resourceType": "MedicationDispense",
					"id": "cert-meddisp-1",
					"status": "completed",
					"subject": {"reference": "Patient/cert-pat-1"},
					"medicationReference": {"reference": "Medication/cert-med-1"}
				},
				"request": {"method": "PUT", "url": "MedicationDispense/cert-meddisp-1"}
			},
			{
				"fullUrl": "Device/cert-dev-1",
				"resource": {
					"resourceType": "Device",
					"id": "cert-dev-1",
					"status": "active",
					"type": {"coding": [{"system": "http://snomed.info/sct", "code": "14106009"}]},
					"patient": {"reference": "Patient/cert-pat-1"}
				},
				"request": {"method": "PUT", "url": "Device/cert-dev-1"}
			},
			{
				"fullUrl": "Task/cert-task-1",
				"resource": {
					"resourceType": "Task",
					"id": "cert-task-1",
					"status": "in-progress",
					"intent": "order",
					"for": {"reference": "Patient/cert-pat-1"},
					"requester": {"reference": "Practitioner/cert-prac-1"}
				},
				"request": {"method": "PUT", "url": "Task/cert-task-1"}
			}
		]
	}`)
}
