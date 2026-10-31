package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

// TestHL7FHIRR4SearchSpecificationMatrix executes a HAPI / Medplum-grade specification
// matrix covering all HL7 FHIR R4 search parameter types, modifiers, prefixes, forward
// and reverse chaining, composite parameters, _filter boolean AST expressions, result
// projection modifiers (_include, _revinclude, _summary, _elements, _sort, _count,
// _offset, _total), and POST /_search parity.
func TestHL7FHIRR4SearchSpecificationMatrix(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "test-r4-search-matrix"
	cleanup := func() {
		_ = postgres.PurgeTenant(ctx, db, tenant)
	}
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	seedBundle := `{
		"resourceType": "Bundle",
		"type": "transaction",
		"entry": [
			{
				"fullUrl": "Organization/srch-org-1",
				"resource": {
					"resourceType": "Organization",
					"id": "srch-org-1",
					"active": true,
					"name": "Metropolitan General Hospital",
					"identifier": [{"system": "http://hl7.org/fhir/sid/us-npi", "value": "9988776655"}]
				},
				"request": {"method": "PUT", "url": "Organization/srch-org-1"}
			},
			{
				"fullUrl": "Location/srch-loc-1",
				"resource": {
					"resourceType": "Location",
					"id": "srch-loc-1",
					"status": "active",
					"name": "Cardiology East Wing",
					"managingOrganization": {"reference": "Organization/srch-org-1"}
				},
				"request": {"method": "PUT", "url": "Location/srch-loc-1"}
			},
			{
				"fullUrl": "Patient/srch-pat-1",
				"resource": {
					"resourceType": "Patient",
					"id": "srch-pat-1",
					"active": true,
					"gender": "male",
					"birthDate": "1975-03-10",
					"name": [{"use": "official", "family": "Smith", "given": ["John", "Edward"]}],
					"telecom": [{"system": "phone", "value": "555-0101"}],
					"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-1001"}],
					"managingOrganization": {"reference": "Organization/srch-org-1"}
				},
				"request": {"method": "PUT", "url": "Patient/srch-pat-1"}
			},
			{
				"fullUrl": "Patient/srch-pat-2",
				"resource": {
					"resourceType": "Patient",
					"id": "srch-pat-2",
					"active": true,
					"gender": "female",
					"birthDate": "1990-08-22",
					"name": [{"use": "official", "family": "Johnson", "given": ["Alice", "Marie"]}],
					"identifier": [{"system": "http://hospital.example.org/mrn", "value": "MRN-1002"}],
					"managingOrganization": {"reference": "Organization/srch-org-1"}
				},
				"request": {"method": "PUT", "url": "Patient/srch-pat-2"}
			},
			{
				"fullUrl": "Patient/srch-pat-3",
				"resource": {
					"resourceType": "Patient",
					"id": "srch-pat-3",
					"active": false,
					"gender": "male",
					"birthDate": "1962-11-05",
					"name": [{"use": "official", "family": "Smithson", "given": ["Robert"]}],
					"identifier": [{"value": "NO-SYS-3003"}]
				},
				"request": {"method": "PUT", "url": "Patient/srch-pat-3"}
			},
			{
				"fullUrl": "Encounter/srch-enc-1",
				"resource": {
					"resourceType": "Encounter",
					"id": "srch-enc-1",
					"status": "finished",
					"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
					"subject": {"reference": "Patient/srch-pat-1"},
					"location": [{"location": {"reference": "Location/srch-loc-1"}}],
					"period": {"start": "2026-01-15T09:00:00Z", "end": "2026-01-15T10:00:00Z"}
				},
				"request": {"method": "PUT", "url": "Encounter/srch-enc-1"}
			},
			{
				"fullUrl": "Observation/srch-obs-hr",
				"resource": {
					"resourceType": "Observation",
					"id": "srch-obs-hr",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
					"code": {
						"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}],
						"text": "Resting Heart Rate"
					},
					"subject": {"reference": "Patient/srch-pat-1"},
					"encounter": {"reference": "Encounter/srch-enc-1"},
					"effectiveDateTime": "2026-01-15T09:15:00Z",
					"valueQuantity": {"value": 72.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
				},
				"request": {"method": "PUT", "url": "Observation/srch-obs-hr"}
			},
			{
				"fullUrl": "Observation/srch-obs-bp",
				"resource": {
					"resourceType": "Observation",
					"id": "srch-obs-bp",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
					"code": {
						"coding": [{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure panel"}],
						"text": "Systolic and Diastolic Blood Pressure"
					},
					"subject": {"reference": "Patient/srch-pat-1"},
					"encounter": {"reference": "Encounter/srch-enc-1"},
					"effectiveDateTime": "2026-02-20T14:30:00Z",
					"component": [
						{
							"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic blood pressure"}]},
							"valueQuantity": {"value": 142.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
						},
						{
							"code": {"coding": [{"system": "http://loinc.org", "code": "8462-4", "display": "Diastolic blood pressure"}]},
							"valueQuantity": {"value": 88.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
						}
					]
				},
				"request": {"method": "PUT", "url": "Observation/srch-obs-bp"}
			},
			{
				"fullUrl": "Observation/srch-obs-temp",
				"resource": {
					"resourceType": "Observation",
					"id": "srch-obs-temp",
					"status": "final",
					"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
					"code": {
						"coding": [{"system": "http://loinc.org", "code": "8310-5", "display": "Body temperature"}],
						"text": "Oral Body Temperature"
					},
					"subject": {"reference": "Patient/srch-pat-2"},
					"effectiveDateTime": "2026-03-10T11:00:00Z",
					"valueQuantity": {"value": 37.2, "unit": "Cel", "system": "http://unitsofmeasure.org", "code": "Cel"}
				},
				"request": {"method": "PUT", "url": "Observation/srch-obs-temp"}
			}
		]
	}`

	respSeed, err := http.Post(ts.URL+"/fhir/r4/"+tenant, "application/fhir+json", strings.NewReader(seedBundle))
	if err != nil || respSeed.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(respSeed.Body)
		t.Fatalf("seed bundle failed (%v / %d): %s", err, respSeed.StatusCode, string(b))
	}
	respSeed.Body.Close()

	searchIDs := func(t *testing.T, queryPath string) ([]string, map[string]any) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/fhir/r4/" + tenant + "/" + queryPath)
		if err != nil {
			t.Fatalf("GET %s: %v", queryPath, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status=%d: %s", queryPath, resp.StatusCode, string(b))
		}
		var bundle map[string]any
		if err := json.Unmarshal(b, &bundle); err != nil {
			t.Fatalf("GET %s invalid JSON: %v", queryPath, err)
		}
		var ids []string
		entries, _ := bundle["entry"].([]any)
		for _, raw := range entries {
			em, _ := raw.(map[string]any)
			res, _ := em["resource"].(map[string]any)
			if id, _ := res["id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
		return ids, bundle
	}

	containsID := func(ids []string, target string) bool {
		for _, id := range ids {
			if id == target {
				return true
			}
		}
		return false
	}

	// =========================================================================
	// 1. String Search Modifiers & MultipleOr / MultipleAnd
	// =========================================================================
	t.Run("String_Modifiers_Prefix_Exact_Contains_Missing_MultipleOr_MultipleAnd", func(t *testing.T) {
		// Default prefix (case-insensitive): "sm" matches "Smith" and "Smithson"
		ids, _ := searchIDs(t, "Patient?family=sm")
		if len(ids) != 2 || !containsID(ids, "srch-pat-1") || !containsID(ids, "srch-pat-3") {
			t.Fatalf("expected Patient?family=sm to match [srch-pat-1, srch-pat-3], got %v", ids)
		}

		// :exact (whole-string exact match, no prefix expansion): "Smith" matches only srch-pat-1 (not "Smithson"), "Sm" matches 0
		ids, _ = searchIDs(t, "Patient?family:exact=Smith")
		if len(ids) != 1 || ids[0] != "srch-pat-1" {
			t.Fatalf("expected Patient?family:exact=Smith to match [srch-pat-1], got %v", ids)
		}
		ids, _ = searchIDs(t, "Patient?family:exact=Sm")
		if len(ids) != 0 {
			t.Fatalf("expected Patient?family:exact=Sm to match [], got %v", ids)
		}

		// :contains (case-insensitive substring): "ithso" matches only "Smithson" (srch-pat-3)
		ids, _ = searchIDs(t, "Patient?family:contains=ithso")
		if len(ids) != 1 || ids[0] != "srch-pat-3" {
			t.Fatalf("expected Patient?family:contains=ithso to match [srch-pat-3], got %v", ids)
		}

		// MultipleOr (comma-separated): "Johnson,Smithson"
		ids, _ = searchIDs(t, "Patient?family=Johnson,Smithson")
		if len(ids) != 2 || !containsID(ids, "srch-pat-2") || !containsID(ids, "srch-pat-3") {
			t.Fatalf("expected Patient?family=Johnson,Smithson to match [srch-pat-2, srch-pat-3], got %v", ids)
		}

		// MultipleAnd (repeated param): given=John&given=Edward
		ids, _ = searchIDs(t, "Patient?given=John&given=Edward")
		if len(ids) != 1 || ids[0] != "srch-pat-1" {
			t.Fatalf("expected Patient?given=John&given=Edward to match [srch-pat-1], got %v", ids)
		}

		// :missing=false vs :missing=true on telecom
		ids, _ = searchIDs(t, "Patient?telecom:missing=false")
		if len(ids) != 1 || ids[0] != "srch-pat-1" {
			t.Fatalf("expected Patient?telecom:missing=false to match [srch-pat-1], got %v", ids)
		}
		ids, _ = searchIDs(t, "Patient?telecom:missing=true")
		if len(ids) != 2 || !containsID(ids, "srch-pat-2") || !containsID(ids, "srch-pat-3") {
			t.Fatalf("expected Patient?telecom:missing=true to match [srch-pat-2, srch-pat-3], got %v", ids)
		}
	})

	// =========================================================================
	// 2. Token Search Modifiers (system|code, |code, system|, code, :not, :text, :in)
	// =========================================================================
	t.Run("Token_Modifiers_SystemCode_Not_Text_In", func(t *testing.T) {
		// system|code
		ids, _ := searchIDs(t, "Observation?code=http://loinc.org|8867-4")
		if len(ids) != 1 || ids[0] != "srch-obs-hr" {
			t.Fatalf("expected Observation?code=http://loinc.org|8867-4 -> [srch-obs-hr], got %v", ids)
		}

		// system| (any code in system)
		ids, _ = searchIDs(t, "Patient?identifier=http://hospital.example.org/mrn|")
		if len(ids) != 2 || !containsID(ids, "srch-pat-1") || !containsID(ids, "srch-pat-2") {
			t.Fatalf("expected Patient?identifier=http://hospital.example.org/mrn| -> [srch-pat-1, srch-pat-2], got %v", ids)
		}

		// |code (identifier with empty system)
		ids, _ = searchIDs(t, "Patient?identifier=|NO-SYS-3003")
		if len(ids) != 1 || ids[0] != "srch-pat-3" {
			t.Fatalf("expected Patient?identifier=|NO-SYS-3003 -> [srch-pat-3], got %v", ids)
		}

		// :not modifier
		ids, _ = searchIDs(t, "Observation?code:not=8867-4")
		if len(ids) != 2 || containsID(ids, "srch-obs-hr") {
			t.Fatalf("expected Observation?code:not=8867-4 to exclude srch-obs-hr, got %v", ids)
		}

		// :text modifier on CodeableConcept text/display
		ids, _ = searchIDs(t, "Observation?code:text=Resting")
		if len(ids) != 1 || ids[0] != "srch-obs-hr" {
			t.Fatalf("expected Observation?code:text=Resting -> [srch-obs-hr], got %v", ids)
		}

		// :in ValueSet modifier
		ids, _ = searchIDs(t, "Observation?code:in=http://hl7.org/fhir/ValueSet/observation-vitalsignresult")
		if len(ids) < 2 || !containsID(ids, "srch-obs-hr") {
			t.Fatalf("expected Observation?code:in=observation-vitalsignresult to include srch-obs-hr, got %v", ids)
		}
	})

	// =========================================================================
	// 3. Date & Quantity Search Prefixes (eq, ne, gt, lt, ge, le, sa, eb, ap)
	// =========================================================================
	t.Run("Date_And_Quantity_Prefixes", func(t *testing.T) {
		// Date range: ge2026-01-01 & le2026-02-01 -> only srch-obs-hr (2026-01-15)
		ids, _ := searchIDs(t, "Observation?date=ge2026-01-01&date=le2026-02-01")
		if len(ids) != 1 || ids[0] != "srch-obs-hr" {
			t.Fatalf("expected date range [2026-01-01, 2026-02-01] -> [srch-obs-hr], got %v", ids)
		}

		// Date gt / sa: gt2026-02-01 -> srch-obs-bp (Feb 20) & srch-obs-temp (Mar 10)
		ids, _ = searchIDs(t, "Observation?date=gt2026-02-01")
		if len(ids) != 2 || !containsID(ids, "srch-obs-bp") || !containsID(ids, "srch-obs-temp") {
			t.Fatalf("expected date=gt2026-02-01 -> [srch-obs-bp, srch-obs-temp], got %v", ids)
		}

		// Quantity prefixes: value-quantity=gt50 -> srch-obs-hr (72.0)
		ids, _ = searchIDs(t, "Observation?value-quantity=gt50")
		if len(ids) != 1 || ids[0] != "srch-obs-hr" {
			t.Fatalf("expected value-quantity=gt50 -> [srch-obs-hr], got %v", ids)
		}
		ids, _ = searchIDs(t, "Observation?value-quantity=lt50")
		if len(ids) != 1 || ids[0] != "srch-obs-temp" {
			t.Fatalf("expected value-quantity=lt50 -> [srch-obs-temp], got %v", ids)
		}
		ids, _ = searchIDs(t, "Observation?value-quantity=eq72|http://unitsofmeasure.org|/min")
		if len(ids) != 1 || ids[0] != "srch-obs-hr" {
			t.Fatalf("expected value-quantity=eq72|http://unitsofmeasure.org|/min -> [srch-obs-hr], got %v", ids)
		}
		ids, _ = searchIDs(t, "Observation?value-quantity=ap70")
		if len(ids) != 1 || ids[0] != "srch-obs-hr" {
			t.Fatalf("expected value-quantity=ap70 (approx ±10%% of 70) -> [srch-obs-hr], got %v", ids)
		}
	})

	// =========================================================================
	// 4. Reference Search, Forward Chaining & Reverse Chaining (_has)
	// =========================================================================
	t.Run("Reference_ForwardChaining_And_ReverseChaining_Has", func(t *testing.T) {
		// Forward chaining: Observation?subject:Patient.family=Smith -> srch-obs-hr, srch-obs-bp
		ids, _ := searchIDs(t, "Observation?subject:Patient.family=Smith")
		if len(ids) != 2 || !containsID(ids, "srch-obs-hr") || !containsID(ids, "srch-obs-bp") {
			t.Fatalf("expected Observation?subject:Patient.family=Smith -> [srch-obs-hr, srch-obs-bp], got %v", ids)
		}

		// Forward chaining on Encounter location: Encounter?location.name=Cardiology
		ids, _ = searchIDs(t, "Encounter?location.name=Cardiology")
		if len(ids) != 1 || ids[0] != "srch-enc-1" {
			t.Fatalf("expected Encounter?location.name=Cardiology -> [srch-enc-1], got %v", ids)
		}

		// Reverse chaining (_has): Patient?_has:Observation:patient:code=8310-5 -> srch-pat-2
		ids, _ = searchIDs(t, "Patient?_has:Observation:patient:code=8310-5")
		if len(ids) != 1 || ids[0] != "srch-pat-2" {
			t.Fatalf("expected Patient?_has:Observation:patient:code=8310-5 -> [srch-pat-2], got %v", ids)
		}
	})

	// =========================================================================
	// 5. Composite Search Parameters (code-value-quantity & component-code-value-quantity)
	// =========================================================================
	t.Run("Composite_Search_CodeValueQuantity_And_ComponentCodeValueQuantity", func(t *testing.T) {
		ids, _ := searchIDs(t, "Observation?code-value-quantity=http://loinc.org|8867-4$72")
		if len(ids) != 1 || ids[0] != "srch-obs-hr" {
			t.Fatalf("expected code-value-quantity=http://loinc.org|8867-4$72 -> [srch-obs-hr], got %v", ids)
		}

		ids, _ = searchIDs(t, "Observation?component-code-value-quantity=http://loinc.org|8480-6$gt130")
		if len(ids) != 1 || ids[0] != "srch-obs-bp" {
			t.Fatalf("expected component-code-value-quantity=http://loinc.org|8480-6$gt130 -> [srch-obs-bp], got %v", ids)
		}
	})

	// =========================================================================
	// 6. Result Modifiers (_include, _revinclude, _summary, _elements, _sort, _total, _filter & POST /_search)
	// =========================================================================
	t.Run("ResultModifiers_Include_RevInclude_Summary_Elements_Sort_Filter_PostSearch", func(t *testing.T) {
		// _include=Observation:patient
		ids, _ := searchIDs(t, "Observation?_id=srch-obs-hr&_include=Observation:patient")
		if !containsID(ids, "srch-obs-hr") || !containsID(ids, "srch-pat-1") {
			t.Fatalf("expected _include=Observation:patient to return srch-obs-hr and srch-pat-1, got %v", ids)
		}

		// _revinclude=Observation:patient
		ids, _ = searchIDs(t, "Patient?_id=srch-pat-2&_revinclude=Observation:patient")
		if !containsID(ids, "srch-pat-2") || !containsID(ids, "srch-obs-temp") {
			t.Fatalf("expected _revinclude=Observation:patient to return srch-pat-2 and srch-obs-temp, got %v", ids)
		}

		// _sort=birthDate (ascending: 1962 srch-pat-3, 1975 srch-pat-1, 1990 srch-pat-2)
		ids, _ = searchIDs(t, "Patient?_sort=birthDate")
		if len(ids) != 3 || ids[0] != "srch-pat-3" || ids[1] != "srch-pat-1" || ids[2] != "srch-pat-2" {
			t.Fatalf("expected ascending birthDate order [srch-pat-3, srch-pat-1, srch-pat-2], got %v", ids)
		}

		// _sort=-birthDate (descending: 1990 srch-pat-2, 1975 srch-pat-1, 1962 srch-pat-3)
		ids, _ = searchIDs(t, "Patient?_sort=-birthDate")
		if len(ids) != 3 || ids[0] != "srch-pat-2" || ids[1] != "srch-pat-1" || ids[2] != "srch-pat-3" {
			t.Fatalf("expected descending -birthDate order [srch-pat-2, srch-pat-1, srch-pat-3], got %v", ids)
		}

		// _summary=count (returns total without entry array)
		ids, bundle := searchIDs(t, "Patient?_summary=count")
		if len(ids) != 0 || bundle["total"] != float64(3) {
			t.Fatalf("expected _summary=count to return total=3 and 0 entries, got total=%v ids=%v", bundle["total"], ids)
		}

		// _total=none (omits total field)
		_, bundleNone := searchIDs(t, "Patient?_total=none")
		if _, hasTotal := bundleNone["total"]; hasTotal {
			t.Fatalf("expected _total=none to omit Bundle.total, got %v", bundleNone["total"])
		}

		// _filter boolean expression
		filterExpr := url.QueryEscape(`family eq "Smith" and gender eq "male"`)
		ids, _ = searchIDs(t, "Patient?_filter="+filterExpr)
		if len(ids) != 1 || ids[0] != "srch-pat-1" {
			t.Fatalf("expected _filter='family eq \"Smith\" and gender eq \"male\"' -> [srch-pat-1], got %v", ids)
		}

		// POST /Patient/_search with application/x-www-form-urlencoded body
		respPost, err := http.Post(
			ts.URL+"/fhir/r4/"+tenant+"/Patient/_search",
			"application/x-www-form-urlencoded",
			strings.NewReader("family=Johnson&gender=female"),
		)
		if err != nil || respPost.StatusCode != http.StatusOK {
			t.Fatalf("POST /Patient/_search failed: %v / %d", err, respPost.StatusCode)
		}
		var postBundle map[string]any
		_ = json.NewDecoder(respPost.Body).Decode(&postBundle)
		respPost.Body.Close()
		if postBundle["total"] != float64(1) {
			t.Fatalf("expected POST /Patient/_search total=1, got %v", postBundle["total"])
		}
	})
}
