package server_test

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"log/slog"

	_ "github.com/lib/pq"
	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestServer_Integration(t *testing.T) {
	dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("FLINT_TEST_POSTGRES_DSN not set")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Run migrations
	if err := postgres.RunMigrations(t.Context(), db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	store := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := server.New(store, logger)
	srv.RegisterResourceType("Patient", &patpb.Patient{})
	srv.RegisterResourceType("Condition", &condpb.Condition{})
	srv.RegisterResourceType("Encounter", &encpb.Encounter{})
	srv.RegisterResourceType("Observation", &obspb.Observation{})
	srv.RegisterResourceType("Practitioner", &pracpb.Practitioner{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Build real Patient protos
	ctx := t.Context()
	pat1 := &patpb.Patient{
		Id:     &dtpb.Id{Value: "api-pat-1"},
		Active: &dtpb.Boolean{Value: true},
		Name: []*dtpb.HumanName{
			{Family: &dtpb.String{Value: "Smith"}, Given: []*dtpb.String{{Value: "John"}}},
		},
		BirthDate: &dtpb.Date{ValueUs: 631152000000000}, // 1990-01-01
		Identifier: []*dtpb.Identifier{
			{System: &dtpb.Uri{Value: "http://mrn"}, Value: &dtpb.String{Value: "MRN-100"}},
		},
	}
	pat1Bytes, _ := proto.Marshal(pat1)

	pat2 := &patpb.Patient{
		Id: &dtpb.Id{Value: "api-pat-2"},
		Name: []*dtpb.HumanName{
			{Family: &dtpb.String{Value: "Jones"}},
		},
		Identifier: []*dtpb.Identifier{
			{System: &dtpb.Uri{Value: "http://mrn"}, Value: &dtpb.String{Value: "MRN-200"}},
		},
	}
	pat2Bytes, _ := proto.Marshal(pat2)

	obs1 := &obspb.Observation{
		Id: &dtpb.Id{Value: "api-obs-1"},
		Subject: &dtpb.Reference{
			Reference: &dtpb.Reference_PatientId{
				PatientId: &dtpb.ReferenceId{Value: "api-pat-1"},
			},
		},
		Code: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{System: &dtpb.Uri{Value: "http://loinc.org"}, Code: &dtpb.Code{Value: "29463-7"}},
			},
		},
	}
	obs1Bytes, _ := proto.Marshal(obs1)

	inputs := []postgres.ResourceInput{
		{
			TenantID:      "test-api",
			ResType:       "Patient",
			ResID:         "api-pat-1",
			ResourceProto: pat1Bytes,
			SearchIndexes: &postgres.SearchIndexes{
				Strings: []postgres.SpidxString{
					{TenantID: "test-api", ResType: "Patient", ResID: "api-pat-1", SpName: "family", SpValue: "smith"},
					{TenantID: "test-api", ResType: "Patient", ResID: "api-pat-1", SpName: "given", SpValue: "john"},
					{TenantID: "test-api", ResType: "Patient", ResID: "api-pat-1", SpName: "name", SpValue: "smith"},
				},
				Tokens: []postgres.SpidxToken{
					{TenantID: "test-api", ResType: "Patient", ResID: "api-pat-1", SpName: "identifier", SpSystem: "http://mrn", SpValue: "MRN-100"},
					{TenantID: "test-api", ResType: "Patient", ResID: "api-pat-1", SpName: "active", SpValue: "true"},
				},
				Dates: []postgres.SpidxDate{
					{TenantID: "test-api", ResType: "Patient", ResID: "api-pat-1", SpName: "birthdate",
						SpLow: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC), SpHigh: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)},
				},
			},
		},
		{
			TenantID:      "test-api",
			ResType:       "Patient",
			ResID:         "api-pat-2",
			ResourceProto: pat2Bytes,
			SearchIndexes: &postgres.SearchIndexes{
				Strings: []postgres.SpidxString{
					{TenantID: "test-api", ResType: "Patient", ResID: "api-pat-2", SpName: "family", SpValue: "jones"},
					{TenantID: "test-api", ResType: "Patient", ResID: "api-pat-2", SpName: "name", SpValue: "jones"},
				},
				Tokens: []postgres.SpidxToken{
					{TenantID: "test-api", ResType: "Patient", ResID: "api-pat-2", SpName: "identifier", SpSystem: "http://mrn", SpValue: "MRN-200"},
				},
			},
		},
		{
			TenantID:      "test-api",
			ResType:       "Observation",
			ResID:         "api-obs-1",
			ResourceProto: obs1Bytes,
			SearchIndexes: &postgres.SearchIndexes{
				Tokens: []postgres.SpidxToken{
					{TenantID: "test-api", ResType: "Observation", ResID: "api-obs-1", SpName: "code", SpSystem: "http://loinc.org", SpValue: "29463-7"},
				},
				References: []postgres.SpidxReference{
					{TenantID: "test-api", ResType: "Observation", ResID: "api-obs-1", SpName: "patient", TargetType: "Patient", TargetID: "api-pat-1"},
					{TenantID: "test-api", ResType: "Observation", ResID: "api-obs-1", SpName: "subject", TargetType: "Patient", TargetID: "api-pat-1"},
				},
				Quantities: []postgres.SpidxQuantity{
					{TenantID: "test-api", ResType: "Observation", ResID: "api-obs-1", SpName: "value-quantity", SpValue: 72.5, SpSystem: "http://unitsofmeasure.org", SpCode: "kg"},
				},
			},
		},
	}
	if err := store.WriteBatch(ctx, inputs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Test cases
	t.Run("GET /metadata", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/metadata")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status: %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/fhir+json; charset=utf-8" {
			t.Errorf("content-type: %s", ct)
		}
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		if body["resourceType"] != "CapabilityStatement" {
			t.Errorf("expected CapabilityStatement, got %v", body["resourceType"])
		}
		t.Logf("CapabilityStatement: %v", body["software"])
	})

	t.Run("GET Patient by ID (not found)", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/nonexistent")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("GET Patient by ID (found)", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/api-pat-1")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		t.Logf("Content-Type: %s", resp.Header.Get("Content-Type"))
	})

	t.Run("Search Patient by family=smith", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient?family=smith")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		entries := bundle["entry"].([]any)
		t.Logf("Search family=smith: total=%d, entries=%d", total, len(entries))
		if total != 1 {
			t.Errorf("expected total 1, got %d", total)
		}
		if len(entries) != 1 {
			t.Errorf("expected 1 entry, got %d", len(entries))
		}
	})

	t.Run("Search Patient by identifier=MRN-200", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient?identifier=MRN-200")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		t.Logf("Search identifier=MRN-200: total=%d", total)
		if total != 1 {
			t.Errorf("expected total 1, got %d", total)
		}
	})

	t.Run("Search Patient by identifier with system|value", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient?identifier=http://mrn|MRN-100")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		t.Logf("Search identifier=http://mrn|MRN-100: total=%d", total)
		if total != 1 {
			t.Errorf("expected total 1, got %d", total)
		}
	})

	t.Run("Search Patient no results", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient?family=zzzzzz")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		if total != 0 {
			t.Errorf("expected total 0, got %d", total)
		}
	})

	t.Run("Search Patient by date birthdate=ge1980-01-01", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient?birthdate=ge1980-01-01")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		if total != 1 {
			t.Errorf("expected total 1, got %d", total)
		}
		entries := bundle["entry"].([]any)
		entry0 := entries[0].(map[string]any)
		searchMeta := entry0["search"].(map[string]any)
		if searchMeta["mode"] != "match" {
			t.Errorf("expected search mode match, got %v", searchMeta["mode"])
		}
	})

	t.Run("Search Patient by date birthdate=lt1985-01-01 returns empty", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient?birthdate=lt1985-01-01")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		if total != 0 {
			t.Errorf("expected total 0, got %d", total)
		}
	})

	t.Run("Search Observation with _include=Observation:patient", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Observation?code=29463-7&_include=Observation:patient")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		if total != 1 {
			t.Errorf("expected total matches 1, got %d", total)
		}
		entries := bundle["entry"].([]any)
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries (1 match + 1 include), got %d", len(entries))
		}
		entry0 := entries[0].(map[string]any)
		if entry0["search"].(map[string]any)["mode"] != "match" {
			t.Errorf("expected entry 0 mode match, got %v", entry0["search"])
		}
		entry1 := entries[1].(map[string]any)
		if entry1["search"].(map[string]any)["mode"] != "include" {
			t.Errorf("expected entry 1 mode include, got %v", entry1["search"])
		}
		if entry1["fullUrl"] != "Patient/api-pat-1" {
			t.Errorf("expected include fullUrl Patient/api-pat-1, got %v", entry1["fullUrl"])
		}
	})

	t.Run("Search Patient with _revinclude=Observation:patient", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient?family=smith&_revinclude=Observation:patient")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		if total != 1 {
			t.Errorf("expected total matches 1, got %d", total)
		}
		entries := bundle["entry"].([]any)
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries (1 match + 1 revinclude), got %d", len(entries))
		}
		entry0 := entries[0].(map[string]any)
		if entry0["search"].(map[string]any)["mode"] != "match" {
			t.Errorf("expected entry 0 mode match, got %v", entry0["search"])
		}
		entry1 := entries[1].(map[string]any)
		if entry1["search"].(map[string]any)["mode"] != "include" {
			t.Errorf("expected entry 1 mode include, got %v", entry1["search"])
		}
		if entry1["fullUrl"] != "Observation/api-obs-1" {
			t.Errorf("expected revinclude fullUrl Observation/api-obs-1, got %v", entry1["fullUrl"])
		}
	})

	t.Run("Search Observation with chained param patient.name=smith", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Observation?patient.name=smith")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		if total != 1 {
			t.Errorf("expected total 1, got %d", total)
		}
	})

	t.Run("Search Observation with chained param patient.name=jones returns empty", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Observation?patient.name=jones")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		if total != 0 {
			t.Errorf("expected total 0, got %d", total)
		}
	})

	t.Run("Search Observation by quantity value-quantity=gt70", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Observation?value-quantity=gt70")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		if total != 1 {
			t.Errorf("expected total 1, got %d", total)
		}
	})

	t.Run("Search Observation by quantity value-quantity=lt70 returns empty", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Observation?value-quantity=lt70")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var bundle map[string]any
		json.NewDecoder(resp.Body).Decode(&bundle)
		total := int(bundle["total"].(float64))
		if total != 0 {
			t.Errorf("expected total 0, got %d", total)
		}
	})

	t.Run("POST Patient creates resource", func(t *testing.T) {
		patJSON := `{"resourceType":"Patient","id":{"value":"post-pat-1"},"name":[{"family":{"value":"Created"}}]}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient", "application/fhir+json", strings.NewReader(patJSON))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 201 {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			t.Error("expected Location header")
		}
		t.Logf("Created: Location=%s", loc)
	})

	t.Run("POST then GET round-trip", func(t *testing.T) {
		// Read back the posted resource
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/post-pat-1")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		t.Log("POST→GET round-trip successful")
	})

	t.Run("POST invalid JSON returns 400", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient", "application/fhir+json", strings.NewReader("{not valid json"))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("POST transaction Bundle creates multiple resources", func(t *testing.T) {
		bundleJSON := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {"resourceType":"Patient","id":{"value":"bnd-pat-1"},"name":[{"family":{"value":"BundleSmith"}}]},
					"request": {"method":"PUT","url":"Patient/bnd-pat-1"}
				},
				{
					"resource": {"resourceType":"Patient","id":{"value":"bnd-pat-2"},"name":[{"family":{"value":"BundleJones"}}]},
					"request": {"method":"PUT","url":"Patient/bnd-pat-2"}
				}
			]
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api", "application/fhir+json", strings.NewReader(bundleJSON))
		if err != nil {
			t.Fatalf("POST Bundle: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var respBundle map[string]any
		json.NewDecoder(resp.Body).Decode(&respBundle)
		if respBundle["type"] != "transaction-response" {
			t.Errorf("expected transaction-response, got %v", respBundle["type"])
		}
		entries := respBundle["entry"].([]any)
		if len(entries) != 2 {
			t.Errorf("expected 2 entries, got %d", len(entries))
		}
		t.Logf("Bundle response: %d entries", len(entries))
	})

	t.Run("Bundle resources are readable", func(t *testing.T) {
		for _, id := range []string{"bnd-pat-1", "bnd-pat-2"} {
			resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/" + id)
			if err != nil {
				t.Fatalf("GET %s: %v", id, err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("GET %s: expected 200, got %d", id, resp.StatusCode)
			}
		}
		t.Log("Both Bundle resources readable via GET")
	})

	t.Run("POST invalid Bundle returns 400", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api", "application/fhir+json",
			strings.NewReader(`{"resourceType":"Patient"}`))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("POST Condition extracts index and is searchable by code", func(t *testing.T) {
		condJSON := `{
			"resourceType": "Condition",
			"id": {"value": "post-cond-1"},
			"subject": {
				"patientId": {"value": "api-pat-1"}
			},
			"code": {
				"coding": [{
					"system": {"value": "http://snomed.info/sct"},
					"code": {"value": "44054006"}
				}]
			}
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Condition", "application/fhir+json", strings.NewReader(condJSON))
		if err != nil {
			t.Fatalf("POST Condition: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("expected 201, got %d", resp.StatusCode)
		}

		// Search Condition by code
		searchResp, err := http.Get(ts.URL + "/fhir/r4/test-api/Condition?code=44054006")
		if err != nil {
			t.Fatalf("search Condition: %v", err)
		}
		defer searchResp.Body.Close()
		if searchResp.StatusCode != 200 {
			t.Fatalf("search status: %d", searchResp.StatusCode)
		}
		var bundle map[string]any
		json.NewDecoder(searchResp.Body).Decode(&bundle)
		if bundle["total"].(float64) != 1 {
			t.Errorf("expected total 1, got %v", bundle["total"])
		}
	})

	t.Run("POST Encounter extracts index and is searchable by class", func(t *testing.T) {
		encJSON := `{
			"resourceType": "Encounter",
			"id": {"value": "post-enc-1"},
			"status": {"value": "in-progress"},
			"class": {
				"system": {"value": "http://terminology.hl7.org/CodeSystem/v3-ActCode"},
				"code": {"value": "AMB"}
			}
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Encounter", "application/fhir+json", strings.NewReader(encJSON))
		if err != nil {
			t.Fatalf("POST Encounter: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("expected 201, got %d", resp.StatusCode)
		}

		// Search Encounter by class
		searchResp, err := http.Get(ts.URL + "/fhir/r4/test-api/Encounter?class=AMB")
		if err != nil {
			t.Fatalf("search Encounter: %v", err)
		}
		defer searchResp.Body.Close()
		if searchResp.StatusCode != 200 {
			t.Fatalf("search status: %d", searchResp.StatusCode)
		}
		var bundle map[string]any
		json.NewDecoder(searchResp.Body).Decode(&bundle)
		if bundle["total"].(float64) != 1 {
			t.Errorf("expected total 1, got %v", bundle["total"])
		}
	})

	t.Run("POST Observation extracts index and is searchable by code", func(t *testing.T) {
		obsJSON := `{
			"resourceType": "Observation",
			"id": {"value": "post-obs-1"},
			"status": {"value": "final"},
			"code": {
				"coding": [{
					"system": {"value": "http://loinc.org"},
					"code": {"value": "8867-4"}
				}]
			}
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Observation", "application/fhir+json", strings.NewReader(obsJSON))
		if err != nil {
			t.Fatalf("POST Observation: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("expected 201, got %d", resp.StatusCode)
		}

		// Search Observation by code
		searchResp, err := http.Get(ts.URL + "/fhir/r4/test-api/Observation?code=8867-4")
		if err != nil {
			t.Fatalf("search Observation: %v", err)
		}
		defer searchResp.Body.Close()
		if searchResp.StatusCode != 200 {
			t.Fatalf("search status: %d", searchResp.StatusCode)
		}
		var bundle map[string]any
		json.NewDecoder(searchResp.Body).Decode(&bundle)
		if bundle["total"].(float64) != 1 {
			t.Errorf("expected total 1, got %v", bundle["total"])
		}
	})

	t.Run("POST Practitioner extracts index and is searchable by family", func(t *testing.T) {
		pracJSON := `{
			"resourceType": "Practitioner",
			"id": {"value": "post-prac-1"},
			"name": [{"family": {"value": "House"}}]
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Practitioner", "application/fhir+json", strings.NewReader(pracJSON))
		if err != nil {
			t.Fatalf("POST Practitioner: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("expected 201, got %d", resp.StatusCode)
		}

		// Search Practitioner by family
		searchResp, err := http.Get(ts.URL + "/fhir/r4/test-api/Practitioner?family=house")
		if err != nil {
			t.Fatalf("search Practitioner: %v", err)
		}
		defer searchResp.Body.Close()
		if searchResp.StatusCode != 200 {
			t.Fatalf("search status: %d", searchResp.StatusCode)
		}
		var bundle map[string]any
		json.NewDecoder(searchResp.Body).Decode(&bundle)
		if bundle["total"].(float64) != 1 {
			t.Errorf("expected total 1, got %v", bundle["total"])
		}
	})

	t.Run("Validation rejects undeclared elements on POST /Patient", func(t *testing.T) {
		invalidJSON := `{
			"resourceType": "Patient",
			"name": [{"family": "Smith"}],
			"nonExistentField": "invalid_value"
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient", "application/fhir+json", strings.NewReader(invalidJSON))
		if err != nil {
			t.Fatalf("POST Patient: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d", resp.StatusCode)
		}
		var outcome map[string]any
		json.NewDecoder(resp.Body).Decode(&outcome)
		if outcome["resourceType"] != "OperationOutcome" {
			t.Fatalf("expected OperationOutcome, got %v", outcome["resourceType"])
		}
		issues := outcome["issue"].([]any)
		if len(issues) == 0 {
			t.Fatalf("expected at least 1 issue in outcome")
		}
		iss0 := issues[0].(map[string]any)
		if iss0["code"] != "structure" {
			t.Errorf("expected issue code structure, got %v", iss0["code"])
		}
	})

	t.Run("Validation rejects invalid ValueSet code on POST /Patient", func(t *testing.T) {
		invalidJSON := `{
			"resourceType": "Patient",
			"gender": "non-standard-gender"
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient", "application/fhir+json", strings.NewReader(invalidJSON))
		if err != nil {
			t.Fatalf("POST Patient: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d", resp.StatusCode)
		}
		var outcome map[string]any
		json.NewDecoder(resp.Body).Decode(&outcome)
		issues := outcome["issue"].([]any)
		iss0 := issues[0].(map[string]any)
		if iss0["code"] != "code-invalid" {
			t.Errorf("expected issue code code-invalid, got %v", iss0["code"])
		}
	})

	t.Run("Validation rejects missing required fields in transaction Bundle", func(t *testing.T) {
		bundleJSON := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {"resourceType":"Observation","id":"obs-invalid-1"},
					"request": {"method":"POST","url":"Observation"}
				}
			]
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api", "application/fhir+json", strings.NewReader(bundleJSON))
		if err != nil {
			t.Fatalf("POST Bundle: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d", resp.StatusCode)
		}
		var outcome map[string]any
		json.NewDecoder(resp.Body).Decode(&outcome)
		if outcome["resourceType"] != "OperationOutcome" {
			t.Errorf("expected OperationOutcome, got %v", outcome["resourceType"])
		}
	})

	t.Run("POST /Patient/$validate with valid resource returns 200 OperationOutcome", func(t *testing.T) {
		validJSON := `{
			"resourceType": "Patient",
			"active": true,
			"gender": "female",
			"birthDate": "1995-06-15",
			"name": [{"family": "Doe", "given": ["Jane"]}]
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient/$validate", "application/fhir+json", strings.NewReader(validJSON))
		if err != nil {
			t.Fatalf("POST $validate: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}
		var outcome map[string]any
		json.NewDecoder(resp.Body).Decode(&outcome)
		if outcome["resourceType"] != "OperationOutcome" {
			t.Fatalf("expected OperationOutcome, got %v", outcome["resourceType"])
		}
		issues := outcome["issue"].([]any)
		if len(issues) == 0 {
			t.Fatalf("expected at least 1 issue")
		}
		iss0 := issues[0].(map[string]any)
		if iss0["severity"] != "information" {
			t.Errorf("expected severity information, got %v", iss0["severity"])
		}
	})

	t.Run("POST /Patient/$validate with invalid resource returns 400 OperationOutcome", func(t *testing.T) {
		invalidJSON := `{
			"resourceType": "Patient",
			"gender": "unknown-invalid-gender"
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient/$validate", "application/fhir+json", strings.NewReader(invalidJSON))
		if err != nil {
			t.Fatalf("POST $validate: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d", resp.StatusCode)
		}
		var outcome map[string]any
		json.NewDecoder(resp.Body).Decode(&outcome)
		issues := outcome["issue"].([]any)
		iss0 := issues[0].(map[string]any)
		if iss0["severity"] != "error" || iss0["code"] != "code-invalid" {
			t.Errorf("expected error severity and code-invalid, got %v / %v", iss0["severity"], iss0["code"])
		}
	})

	t.Run("POST /$validate with Parameters resource wrapper returns 200 OperationOutcome", func(t *testing.T) {
		paramsJSON := `{
			"resourceType": "Parameters",
			"parameter": [
				{
					"name": "resource",
					"resource": {
						"resourceType": "Observation",
						"status": "final",
						"code": {
							"coding": [{
								"system": "http://loinc.org",
								"code": "8867-4"
							}]
						}
					}
				}
			]
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/$validate", "application/fhir+json", strings.NewReader(paramsJSON))
		if err != nil {
			t.Fatalf("POST system $validate: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}
		var outcome map[string]any
		json.NewDecoder(resp.Body).Decode(&outcome)
		if outcome["resourceType"] != "OperationOutcome" {
			t.Fatalf("expected OperationOutcome, got %v", outcome["resourceType"])
		}
		issues := outcome["issue"].([]any)
		iss0 := issues[0].(map[string]any)
		if iss0["severity"] != "information" {
			t.Errorf("expected severity information, got %v", iss0["severity"])
		}
	})

	t.Run("CapabilityStatement declares $validate operation", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/metadata")
		if err != nil {
			t.Fatalf("GET metadata: %v", err)
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		rest := body["rest"].([]any)
		rest0 := rest[0].(map[string]any)
		ops, ok := rest0["operation"].([]any)
		if !ok || len(ops) == 0 {
			t.Fatalf("expected operation array in CapabilityStatement rest")
		}
		op0 := ops[0].(map[string]any)
		if op0["name"] != "validate" {
			t.Errorf("expected operation name validate, got %v", op0["name"])
		}
	})

	t.Run("Full CRUD lifecycle with ETag, If-Match, vread, _history, and DELETE (410 Gone)", func(t *testing.T) {
		resURL := ts.URL + "/fhir/r4/test-api/Patient/crud-pat-1"

		// 1. PUT create-on-update -> 201 Created with ETag W/"1"
		v1Body := `{"resourceType":"Patient","id":{"value":"crud-pat-1"},"name":[{"family":{"value":"Alpha"}}]}`
		reqPut1, _ := http.NewRequest(http.MethodPut, resURL, strings.NewReader(v1Body))
		reqPut1.Header.Set("Content-Type", "application/fhir+json")
		resp1, err := http.DefaultClient.Do(reqPut1)
		if err != nil {
			t.Fatalf("PUT v1: %v", err)
		}
		resp1.Body.Close()
		if resp1.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 Created on initial PUT, got %d", resp1.StatusCode)
		}
		if etag := resp1.Header.Get("ETag"); etag != `W/"1"` {
			t.Fatalf("expected ETag W/\"1\", got %q", etag)
		}
		if resp1.Header.Get("Last-Modified") == "" {
			t.Fatal("expected Last-Modified header on PUT")
		}

		// 2. PUT update with matching If-Match: W/"1" -> 200 OK with ETag W/"2"
		v2Body := `{"resourceType":"Patient","id":{"value":"crud-pat-1"},"name":[{"family":{"value":"Beta"}}]}`
		reqPut2, _ := http.NewRequest(http.MethodPut, resURL, strings.NewReader(v2Body))
		reqPut2.Header.Set("Content-Type", "application/fhir+json")
		reqPut2.Header.Set("If-Match", `W/"1"`)
		resp2, err := http.DefaultClient.Do(reqPut2)
		if err != nil {
			t.Fatalf("PUT v2: %v", err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK on PUT update, got %d", resp2.StatusCode)
		}
		if etag := resp2.Header.Get("ETag"); etag != `W/"2"` {
			t.Fatalf("expected ETag W/\"2\", got %q", etag)
		}

		// 3. PUT update with stale If-Match: W/"1" -> 412 Precondition Failed
		reqPutStale, _ := http.NewRequest(http.MethodPut, resURL, strings.NewReader(v2Body))
		reqPutStale.Header.Set("Content-Type", "application/fhir+json")
		reqPutStale.Header.Set("If-Match", `W/"1"`)
		respStale, err := http.DefaultClient.Do(reqPutStale)
		if err != nil {
			t.Fatalf("PUT stale: %v", err)
		}
		respStale.Body.Close()
		if respStale.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("expected 412 Precondition Failed on stale If-Match, got %d", respStale.StatusCode)
		}

		// 4. PUT with mismatched body ID -> 400 Bad Request
		mismatchBody := `{"resourceType":"Patient","id":{"value":"wrong-id"},"name":[{"family":{"value":"Beta"}}]}`
		reqMismatch, _ := http.NewRequest(http.MethodPut, resURL, strings.NewReader(mismatchBody))
		reqMismatch.Header.Set("Content-Type", "application/fhir+json")
		respMismatch, err := http.DefaultClient.Do(reqMismatch)
		if err != nil {
			t.Fatalf("PUT mismatch: %v", err)
		}
		respMismatch.Body.Close()
		if respMismatch.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on mismatched ID, got %d", respMismatch.StatusCode)
		}

		// 5. vread v1 and v2
		vread1, err := http.Get(resURL + "/_history/1")
		if err != nil {
			t.Fatalf("GET vread 1: %v", err)
		}
		defer vread1.Body.Close()
		if vread1.StatusCode != http.StatusOK || vread1.Header.Get("ETag") != `W/"1"` {
			t.Fatalf("expected 200 OK with ETag W/\"1\" on vread 1, got %d (%q)", vread1.StatusCode, vread1.Header.Get("ETag"))
		}
		vread1Bytes, _ := io.ReadAll(vread1.Body)
		if !strings.Contains(string(vread1Bytes), "Alpha") {
			t.Errorf("expected v1 body to contain Alpha, got %s", string(vread1Bytes))
		}

		vread2, err := http.Get(resURL + "/_history/2")
		if err != nil {
			t.Fatalf("GET vread 2: %v", err)
		}
		defer vread2.Body.Close()
		if vread2.StatusCode != http.StatusOK || vread2.Header.Get("ETag") != `W/"2"` {
			t.Fatalf("expected 200 OK with ETag W/\"2\" on vread 2, got %d (%q)", vread2.StatusCode, vread2.Header.Get("ETag"))
		}
		vread2Bytes, _ := io.ReadAll(vread2.Body)
		if !strings.Contains(string(vread2Bytes), "Beta") {
			t.Errorf("expected v2 body to contain Beta, got %s", string(vread2Bytes))
		}

		// 6. DELETE -> 204 No Content with ETag W/"3"
		reqDel, _ := http.NewRequest(http.MethodDelete, resURL, nil)
		respDel, err := http.DefaultClient.Do(reqDel)
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		respDel.Body.Close()
		if respDel.StatusCode != http.StatusNoContent {
			t.Fatalf("expected 204 No Content on DELETE, got %d", respDel.StatusCode)
		}
		if etag := respDel.Header.Get("ETag"); etag != `W/"3"` {
			t.Fatalf("expected ETag W/\"3\" on DELETE, got %q", etag)
		}

		// 7. GET after DELETE -> 410 Gone
		respGone, err := http.Get(resURL)
		if err != nil {
			t.Fatalf("GET after DELETE: %v", err)
		}
		respGone.Body.Close()
		if respGone.StatusCode != http.StatusGone {
			t.Fatalf("expected 410 Gone after DELETE, got %d", respGone.StatusCode)
		}

		// 8. vread v3 (tombstone) -> 410 Gone
		vread3, err := http.Get(resURL + "/_history/3")
		if err != nil {
			t.Fatalf("GET vread 3: %v", err)
		}
		vread3.Body.Close()
		if vread3.StatusCode != http.StatusGone {
			t.Fatalf("expected 410 Gone on vread 3, got %d", vread3.StatusCode)
		}

		// 9. GET /_history -> Bundle of type "history" with 3 entries
		histResp, err := http.Get(resURL + "/_history")
		if err != nil {
			t.Fatalf("GET _history: %v", err)
		}
		defer histResp.Body.Close()
		if histResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK on _history, got %d", histResp.StatusCode)
		}
		var histBundle map[string]any
		json.NewDecoder(histResp.Body).Decode(&histBundle)
		if histBundle["type"] != "history" {
			t.Fatalf("expected bundle type history, got %v", histBundle["type"])
		}
		if int(histBundle["total"].(float64)) != 3 {
			t.Fatalf("expected 3 history entries, got %v", histBundle["total"])
		}
	})

	// Clean up
	for _, table := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri", "fhir_resource_history", "fhir_resource"} {
		db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = 'test-api'")
	}
}
