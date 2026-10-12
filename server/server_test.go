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

	"github.com/flint-fhir/flint/automq"
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

	t.Run("AuditEvent is automatically recorded across CRUD interactions and queryable via FHIR REST", func(t *testing.T) {
		// Query AuditEvent for crud-pat-1 from the previous subtest
		auditSearchURL := ts.URL + "/fhir/r4/test-api/AuditEvent?entity=Patient/crud-pat-1"
		resp, err := http.Get(auditSearchURL)
		if err != nil {
			t.Fatalf("GET AuditEvent search: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK on AuditEvent search, got %d", resp.StatusCode)
		}
		var bundle map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
			t.Fatalf("decode AuditEvent bundle: %v", err)
		}
		if bundle["resourceType"] != "Bundle" || bundle["type"] != "searchset" {
			t.Fatalf("expected searchset Bundle, got %v / %v", bundle["resourceType"], bundle["type"])
		}
		total := int(bundle["total"].(float64))
		if total < 5 {
			t.Fatalf("expected at least 5 AuditEvents for crud-pat-1 lifecycle, got %d", total)
		}

		entries := bundle["entry"].([]any)
		firstEntry := entries[0].(map[string]any)
		firstRes := firstEntry["resource"].(map[string]any)
		if firstRes["resourceType"] != "AuditEvent" {
			t.Fatalf("expected AuditEvent resourceType, got %v", firstRes["resourceType"])
		}
		auditID, _ := firstRes["id"].(string)
		if auditID == "" {
			t.Fatal("expected non-empty AuditEvent.id")
		}

		// Read individual AuditEvent by ID
		readResp, err := http.Get(ts.URL + "/fhir/r4/test-api/AuditEvent/" + auditID)
		if err != nil {
			t.Fatalf("GET AuditEvent/%s: %v", auditID, err)
		}
		defer readResp.Body.Close()
		if readResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK on GET AuditEvent/%s, got %d", auditID, readResp.StatusCode)
		}
		var singleAudit map[string]any
		json.NewDecoder(readResp.Body).Decode(&singleAudit)
		if singleAudit["id"] != auditID {
			t.Errorf("expected AuditEvent id %s, got %v", auditID, singleAudit["id"])
		}

		// Verify reading/searching AuditEvent did NOT inflate the AuditEvent count
		resp2, err := http.Get(auditSearchURL)
		if err != nil {
			t.Fatalf("second GET AuditEvent search: %v", err)
		}
		defer resp2.Body.Close()
		var bundle2 map[string]any
		json.NewDecoder(resp2.Body).Decode(&bundle2)
		if int(bundle2["total"].(float64)) != total {
			t.Errorf("expected AuditEvent count to remain %d after AuditEvent reads, got %v", total, bundle2["total"])
		}
	})

	t.Run("Conditional Create (If-None-Exist) prevents duplicate creates and returns 200 OK or 412", func(t *testing.T) {
		patBody := `{"resourceType":"Patient","id":{"value":"cond-create-pat-1"},"name":[{"family":{"value":"UniqueFamilyName"}}]}`

		// 1. First POST with If-None-Exist (0 existing matches) -> 201 Created
		req1, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/test-api/Patient", strings.NewReader(patBody))
		req1.Header.Set("Content-Type", "application/fhir+json")
		req1.Header.Set("If-None-Exist", "family=UniqueFamilyName")
		resp1, err := http.DefaultClient.Do(req1)
		if err != nil {
			t.Fatalf("first conditional POST: %v", err)
		}
		resp1.Body.Close()
		if resp1.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 Created on 0 matches, got %d", resp1.StatusCode)
		}

		// 2. Second POST with same If-None-Exist (1 existing match) -> 200 OK with existing resource
		dupBody := `{"resourceType":"Patient","id":{"value":"cond-create-pat-dup"},"name":[{"family":{"value":"UniqueFamilyName"}}]}`
		req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/test-api/Patient", strings.NewReader(dupBody))
		req2.Header.Set("Content-Type", "application/fhir+json")
		req2.Header.Set("If-None-Exist", "Patient?family=UniqueFamilyName")
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatalf("second conditional POST: %v", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK on 1 match, got %d", resp2.StatusCode)
		}
		if loc := resp2.Header.Get("Location"); loc != "/fhir/r4/test-api/Patient/cond-create-pat-1" {
			t.Errorf("expected Location /fhir/r4/test-api/Patient/cond-create-pat-1, got %q", loc)
		}
		if etag := resp2.Header.Get("ETag"); etag != `W/"1"` {
			t.Errorf("expected ETag W/\"1\", got %q", etag)
		}

		// 3. Seed a second patient with family=UniqueFamilyName directly (without If-None-Exist),
		// then verify If-None-Exist returns 412 Precondition Failed when >1 matches exist.
		secondPat := `{"resourceType":"Patient","id":{"value":"cond-create-pat-2"},"name":[{"family":{"value":"UniqueFamilyName"}}]}`
		respSeed, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient", "application/fhir+json", strings.NewReader(secondPat))
		if err != nil {
			t.Fatalf("seed second patient: %v", err)
		}
		respSeed.Body.Close()

		req3, _ := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/test-api/Patient", strings.NewReader(dupBody))
		req3.Header.Set("Content-Type", "application/fhir+json")
		req3.Header.Set("If-None-Exist", "family=UniqueFamilyName")
		resp3, err := http.DefaultClient.Do(req3)
		if err != nil {
			t.Fatalf("third conditional POST: %v", err)
		}
		resp3.Body.Close()
		if resp3.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("expected 412 Precondition Failed on >1 matches, got %d", resp3.StatusCode)
		}

		// 4. Transaction Bundle with request.ifNoneExist matching 1 existing Practitioner -> 200 OK in bundle response
		practSeed := `{"resourceType":"Practitioner","id":{"value":"cond-pract-1"},"name":[{"family":{"value":"BundleIfNoneExistDoc"}}]}`
		respPract, err := http.Post(ts.URL+"/fhir/r4/test-api/Practitioner", "application/fhir+json", strings.NewReader(practSeed))
		if err != nil {
			t.Fatalf("seed practitioner: %v", err)
		}
		respPract.Body.Close()

		txBundle := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {
						"resourceType": "Practitioner",
						"id": {"value": "cond-pract-dup"},
						"name": [{"family": {"value": "BundleIfNoneExistDoc"}}]
					},
					"request": {
						"method": "POST",
						"url": "Practitioner",
						"ifNoneExist": "family=BundleIfNoneExistDoc"
					}
				}
			]
		}`
		respTx, err := http.Post(ts.URL+"/fhir/r4/test-api", "application/fhir+json", strings.NewReader(txBundle))
		if err != nil {
			t.Fatalf("POST transaction bundle with ifNoneExist: %v", err)
		}
		defer respTx.Body.Close()
		if respTx.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for transaction bundle with 1 ifNoneExist match, got %d", respTx.StatusCode)
		}
		var txRespMap map[string]any
		json.NewDecoder(respTx.Body).Decode(&txRespMap)
		txEntries := txRespMap["entry"].([]any)
		txEntryResp := txEntries[0].(map[string]any)["response"].(map[string]any)
		if txEntryResp["status"] != "200 OK" {
			t.Errorf("expected bundle entry status 200 OK, got %v", txEntryResp["status"])
		}
		if txEntryResp["location"] != "/fhir/r4/test-api/Practitioner/cond-pract-1" {
			t.Errorf("expected bundle entry location /fhir/r4/test-api/Practitioner/cond-pract-1, got %v", txEntryResp["location"])
		}

		// 5. Transaction Bundle with request.ifNoneExist matching >1 Patients -> 412 Precondition Failed
		txMulti := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {
						"resourceType": "Patient",
						"id": {"value": "cond-pat-tx-fail"},
						"name": [{"family": {"value": "UniqueFamilyName"}}]
					},
					"request": {
						"method": "POST",
						"url": "Patient",
						"ifNoneExist": "family=UniqueFamilyName"
					}
				}
			]
		}`
		respTxMulti, err := http.Post(ts.URL+"/fhir/r4/test-api", "application/fhir+json", strings.NewReader(txMulti))
		if err != nil {
			t.Fatalf("POST transaction bundle with >1 ifNoneExist matches: %v", err)
		}
		respTxMulti.Body.Close()
		if respTxMulti.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("expected 412 Precondition Failed for transaction bundle with >1 matches, got %d", respTxMulti.StatusCode)
		}
	})

	t.Run("GET /Patient/{id}/$everything returns full patient chart Bundle", func(t *testing.T) {
		// api-pat-1 was seeded earlier with linked Condition (post-cond-1)
		resp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/api-pat-1/$everything")
		if err != nil {
			t.Fatalf("GET Patient/api-pat-1/$everything: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200 OK on $everything, got %d: %s", resp.StatusCode, body)
		}

		var bundle map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
			t.Fatalf("decode $everything bundle: %v", err)
		}
		if bundle["resourceType"] != "Bundle" || bundle["type"] != "searchset" {
			t.Fatalf("expected searchset Bundle, got %v / %v", bundle["resourceType"], bundle["type"])
		}
		total := int(bundle["total"].(float64))
		if total < 2 {
			t.Fatalf("expected at least 2 resources (Patient + Condition) in $everything, got %d", total)
		}

		// Filter by _type=Condition
		respCond, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/api-pat-1/$everything?_type=Condition")
		if err != nil {
			t.Fatalf("GET $everything?_type=Condition: %v", err)
		}
		defer respCond.Body.Close()
		var condBundle map[string]any
		json.NewDecoder(respCond.Body).Decode(&condBundle)
		if int(condBundle["total"].(float64)) != 1 {
			t.Errorf("expected 1 Condition in _type=Condition $everything, got %v", condBundle["total"])
		}

		// Filter by valid _since date
		respSince, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/api-pat-1/$everything?_since=2020-01-01")
		if err != nil {
			t.Fatalf("GET $everything?_since=2020-01-01: %v", err)
		}
		respSince.Body.Close()
		if respSince.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK for valid _since date, got %d", respSince.StatusCode)
		}

		// Invalid _since returns 400 Bad Request
		respBadSince, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/api-pat-1/$everything?_since=not-a-date")
		if err != nil {
			t.Fatalf("GET $everything?_since=not-a-date: %v", err)
		}
		respBadSince.Body.Close()
		if respBadSince.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 Bad Request for invalid _since, got %d", respBadSince.StatusCode)
		}

		// $everything on non-Patient resource returns 404 Not Found
		respNonPat, err := http.Get(ts.URL + "/fhir/r4/test-api/Condition/post-cond-1/$everything")
		if err != nil {
			t.Fatalf("GET Condition/$everything: %v", err)
		}
		respNonPat.Body.Close()
		if respNonPat.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 Not Found for non-Patient $everything, got %d", respNonPat.StatusCode)
		}

		// Deleted patient (crud-pat-1) returns 410 Gone
		respGone, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/crud-pat-1/$everything")
		if err != nil {
			t.Fatalf("GET deleted Patient/$everything: %v", err)
		}
		respGone.Body.Close()
		if respGone.StatusCode != http.StatusGone {
			t.Errorf("expected 410 Gone for deleted Patient/$everything, got %d", respGone.StatusCode)
		}

		// Non-existent patient returns 404 Not Found
		resp404, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/non-existent-pat/$everything")
		if err != nil {
			t.Fatalf("GET missing Patient/$everything: %v", err)
		}
		resp404.Body.Close()
		if resp404.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 Not Found for missing Patient/$everything, got %d", resp404.StatusCode)
		}
	})

	t.Run("Standard flat HL7 FHIR R4 JSON wire compatibility, generated id in 201 Created, and _id search", func(t *testing.T) {
		// 1. POST Patient using standard flat FHIR R4 JSON without client-supplied id
		flatPatient := `{
			"resourceType": "Patient",
			"active": true,
			"gender": "female",
			"birthDate": "1988-11-23",
			"name": [{"family": "WireStandard", "given": ["Alice"]}]
		}`
		resp, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient", "application/fhir+json", strings.NewReader(flatPatient))
		if err != nil {
			t.Fatalf("POST flat Patient: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			bodyBytes, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201 Created for flat Patient JSON, got %d: %s", resp.StatusCode, bodyBytes)
		}

		var createdMap map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&createdMap); err != nil {
			t.Fatalf("decode 201 Created body: %v", err)
		}
		if createdMap["resourceType"] != "Patient" {
			t.Errorf("expected resourceType=Patient in 201 response body, got %v", createdMap["resourceType"])
		}
		// Verify server-generated ID is present in the 201 Created response body
		var generatedID string
		switch idVal := createdMap["id"].(type) {
		case string:
			generatedID = idVal
		case map[string]any:
			generatedID, _ = idVal["value"].(string)
		}
		if generatedID == "" {
			t.Fatalf("expected server-generated id in 201 Created response body, got %+v", createdMap["id"])
		}

		// 2. Search by _id=<generatedID> and verify resourceType is injected on GET
		searchResp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient?_id=" + generatedID)
		if err != nil {
			t.Fatalf("GET Patient?_id=%s: %v", generatedID, err)
		}
		defer searchResp.Body.Close()
		var searchBundle map[string]any
		json.NewDecoder(searchResp.Body).Decode(&searchBundle)
		if int(searchBundle["total"].(float64)) != 1 {
			t.Fatalf("expected _id search to match 1 Patient, got %v", searchBundle["total"])
		}

		// 3. POST Observation using standard flat FHIR R4 JSON with subject.reference = "Patient/<generatedID>"
		flatObs := `{
			"resourceType": "Observation",
			"status": "final",
			"code": {
				"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]
			},
			"subject": {"reference": "Patient/` + generatedID + `"},
			"valueQuantity": {"value": 74.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
		}`
		obsResp, err := http.Post(ts.URL+"/fhir/r4/test-api/Observation", "application/fhir+json", strings.NewReader(flatObs))
		if err != nil {
			t.Fatalf("POST flat Observation: %v", err)
		}
		defer obsResp.Body.Close()
		if obsResp.StatusCode != http.StatusCreated {
			bodyBytes, _ := io.ReadAll(obsResp.Body)
			t.Fatalf("expected 201 Created for flat Observation JSON, got %d: %s", obsResp.StatusCode, bodyBytes)
		}

		// Verify the flat Observation's subject reference was indexed and is searchable by patient=
		obsSearch, err := http.Get(ts.URL + "/fhir/r4/test-api/Observation?patient=Patient/" + generatedID)
		if err != nil {
			t.Fatalf("GET Observation?patient: %v", err)
		}
		defer obsSearch.Body.Close()
		var obsBundle map[string]any
		json.NewDecoder(obsSearch.Body).Decode(&obsBundle)
		if int(obsBundle["total"].(float64)) != 1 {
			t.Fatalf("expected 1 Observation linked to flat Patient/%s, got %v", generatedID, obsBundle["total"])
		}
	})

	t.Run("POST with duplicate existing ID returns 409 Conflict instead of overwriting", func(t *testing.T) {
		patJSON := `{"resourceType":"Patient","id":"dup-post-id-1","name":[{"family":"Original"}]}`
		resp1, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient", "application/fhir+json", strings.NewReader(patJSON))
		if err != nil {
			t.Fatalf("first POST: %v", err)
		}
		resp1.Body.Close()
		if resp1.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 Created on first POST, got %d", resp1.StatusCode)
		}

		// Second POST with same ID must return 409 Conflict and NOT overwrite version 1
		overwriteJSON := `{"resourceType":"Patient","id":"dup-post-id-1","name":[{"family":"Overwritten"}]}`
		resp2, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient", "application/fhir+json", strings.NewReader(overwriteJSON))
		if err != nil {
			t.Fatalf("second POST: %v", err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 Conflict on duplicate POST ID, got %d", resp2.StatusCode)
		}

		// Verify version is still W/"1" and family is still Original
		getResp, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/dup-post-id-1")
		if err != nil {
			t.Fatalf("GET dup-post-id-1: %v", err)
		}
		defer getResp.Body.Close()
		if getResp.Header.Get("ETag") != `W/"1"` {
			t.Errorf("expected ETag W/\"1\" (unmodified), got %q", getResp.Header.Get("ETag"))
		}
		bodyBytes, _ := io.ReadAll(getResp.Body)
		if !strings.Contains(string(bodyBytes), "Original") {
			t.Errorf("expected resource to retain Original family name, got %s", string(bodyBytes))
		}
	})

	t.Run("Transaction Bundle ACID rollback of DELETE and intra-bundle ifNoneExist visibility", func(t *testing.T) {
		// 1. Seed a patient that will be targeted by DELETE in entry[0] of a failing transaction bundle
		seedPat := `{"resourceType":"Patient","id":"tx-acid-pat-1","name":[{"family":"Survivor"}]}`
		respSeed, err := http.Post(ts.URL+"/fhir/r4/test-api/Patient", "application/fhir+json", strings.NewReader(seedPat))
		if err != nil {
			t.Fatalf("seed tx-acid-pat-1: %v", err)
		}
		respSeed.Body.Close()

		// Transaction bundle: entry[0] DELETEs tx-acid-pat-1, entry[1] PUTs with stale If-Match W/"999" -> 412
		failingTx := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"request": {
						"method": "DELETE",
						"url": "Patient/tx-acid-pat-1"
					}
				},
				{
					"resource": {
						"resourceType": "Patient",
						"id": "tx-acid-pat-1",
						"name": [{"family": "StaleUpdate"}]
					},
					"request": {
						"method": "PUT",
						"url": "Patient/tx-acid-pat-1",
						"ifMatch": "W/\"999\""
					}
				}
			]
		}`
		respFail, err := http.Post(ts.URL+"/fhir/r4/test-api", "application/fhir+json", strings.NewReader(failingTx))
		if err != nil {
			t.Fatalf("POST failing transaction bundle: %v", err)
		}
		respFail.Body.Close()
		if respFail.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("expected 412 Precondition Failed on failing transaction bundle, got %d", respFail.StatusCode)
		}

		// Verify tx-acid-pat-1 is STILL alive (200 OK, not 410 Gone!) because DELETE rolled back
		respCheck, err := http.Get(ts.URL + "/fhir/r4/test-api/Patient/tx-acid-pat-1")
		if err != nil {
			t.Fatalf("GET tx-acid-pat-1 after rollback: %v", err)
		}
		respCheck.Body.Close()
		if respCheck.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for tx-acid-pat-1 after transaction rollback, got %d", respCheck.StatusCode)
		}

		// 2. Intra-bundle ifNoneExist: entry[0] creates a Patient, entry[1] uses ifNoneExist for the same family name
		// and must see entry[0] inside the same transaction!
		intraTx := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {
						"resourceType": "Patient",
						"id": "intra-cond-1",
						"name": [{"family": "IntraBundleDedup"}]
					},
					"request": {
						"method": "POST",
						"url": "Patient",
						"ifNoneExist": "family=IntraBundleDedup"
					}
				},
				{
					"resource": {
						"resourceType": "Patient",
						"id": "intra-cond-2",
						"name": [{"family": "IntraBundleDedup"}]
					},
					"request": {
						"method": "POST",
						"url": "Patient",
						"ifNoneExist": "family=IntraBundleDedup"
					}
				}
			]
		}`
		respIntra, err := http.Post(ts.URL+"/fhir/r4/test-api", "application/fhir+json", strings.NewReader(intraTx))
		if err != nil {
			t.Fatalf("POST intra-bundle ifNoneExist: %v", err)
		}
		defer respIntra.Body.Close()
		if respIntra.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(respIntra.Body)
			t.Fatalf("expected 200 OK on intra-bundle ifNoneExist, got %d: %s", respIntra.StatusCode, bodyBytes)
		}
		var intraBundle map[string]any
		json.NewDecoder(respIntra.Body).Decode(&intraBundle)
		entries := intraBundle["entry"].([]any)
		e0 := entries[0].(map[string]any)["response"].(map[string]any)
		e1 := entries[1].(map[string]any)["response"].(map[string]any)
		if e0["status"] != "201 Created" {
			t.Errorf("expected entry[0] status 201 Created, got %v", e0["status"])
		}
		if e1["status"] != "200 OK" {
			t.Errorf("expected entry[1] status 200 OK (matched entry[0] in same tx), got %v", e1["status"])
		}
		if e0["location"] != e1["location"] {
			t.Errorf("expected entry[0] and entry[1] to share location %v, got %v", e0["location"], e1["location"])
		}
	})

	t.Run("Standard Flat FHIR R4 JSON Round-Trip (POST -> GET -> $validate) and Quantity/Enum Search", func(t *testing.T) {
		obsJSON := `{
			"resourceType": "Observation",
			"id": "flat-rt-obs-1",
			"status": "final",
			"code": {
				"coding": [{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}]
			},
			"subject": {
				"reference": "Patient/api-pat-1"
			},
			"effectiveDateTime": "2025-05-01T14:00:00Z",
			"valueQuantity": {
				"value": 92.5,
				"unit": "beats/minute",
				"system": "http://unitsofmeasure.org",
				"code": "/min"
			},
			"component": [
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
					"valueQuantity": {"value": 128, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				}
			]
		}`
		respPost, err := http.Post(ts.URL+"/fhir/r4/test-api/Observation", "application/fhir+json", strings.NewReader(obsJSON))
		if err != nil {
			t.Fatalf("POST flat Observation: %v", err)
		}
		respPost.Body.Close()
		if respPost.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 Created, got %d", respPost.StatusCode)
		}

		// GET back the Observation and verify standard flat HL7 FHIR R4 JSON + meta.versionId + meta.lastUpdated
		respGet, err := http.Get(ts.URL + "/fhir/r4/test-api/Observation/flat-rt-obs-1")
		if err != nil {
			t.Fatalf("GET flat-rt-obs-1: %v", err)
		}
		getBytes, _ := io.ReadAll(respGet.Body)
		respGet.Body.Close()
		if respGet.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", respGet.StatusCode, getBytes)
		}

		var gotMap map[string]any
		if err := json.Unmarshal(getBytes, &gotMap); err != nil {
			t.Fatalf("unmarshal GET response: %v", err)
		}
		if gotMap["id"] != "flat-rt-obs-1" {
			t.Errorf("expected flat string id 'flat-rt-obs-1', got %v", gotMap["id"])
		}
		if gotMap["status"] != "final" {
			t.Errorf("expected lowercase status 'final', got %v", gotMap["status"])
		}
		if subRef := gotMap["subject"].(map[string]any)["reference"]; subRef != "Patient/api-pat-1" {
			t.Errorf("expected subject.reference 'Patient/api-pat-1', got %v", subRef)
		}
		if vqVal := gotMap["valueQuantity"].(map[string]any)["value"]; vqVal != 92.5 {
			t.Errorf("expected numeric valueQuantity.value 92.5, got %v", vqVal)
		}
		metaMap, ok := gotMap["meta"].(map[string]any)
		if !ok || metaMap["versionId"] != "1" || metaMap["lastUpdated"] == "" {
			t.Errorf("expected meta.versionId=1 and meta.lastUpdated populated, got %v", gotMap["meta"])
		}

		// Feed the exact GET response into POST /Observation/$validate and verify 0 errors!
		respVal, err := http.Post(ts.URL+"/fhir/r4/test-api/Observation/$validate", "application/fhir+json", strings.NewReader(string(getBytes)))
		if err != nil {
			t.Fatalf("POST $validate on GET response: %v", err)
		}
		valBytes, _ := io.ReadAll(respVal.Body)
		respVal.Body.Close()
		var valOutcome map[string]any
		json.Unmarshal(valBytes, &valOutcome)
		for _, iss := range valOutcome["issue"].([]any) {
			issMap := iss.(map[string]any)
			if issMap["severity"] == "error" || issMap["severity"] == "fatal" {
				t.Errorf("round-tripped Observation failed $validate: %s", valBytes)
			}
		}

		// Search Observation by extracted value-quantity, component-value-quantity, and lowercase status=final
		for _, query := range []string{
			"value-quantity=gt90&_id=flat-rt-obs-1",
			"component-value-quantity=gt120&_id=flat-rt-obs-1",
			"status=final&_id=flat-rt-obs-1",
		} {
			sResp, err := http.Get(ts.URL + "/fhir/r4/test-api/Observation?" + query)
			if err != nil {
				t.Fatalf("GET /Observation?%s: %v", query, err)
			}
			var sb map[string]any
			json.NewDecoder(sResp.Body).Decode(&sb)
			sResp.Body.Close()
			if int(sb["total"].(float64)) != 1 {
				t.Errorf("expected 1 match for /Observation?%s, got %v", query, sb["total"])
			}
		}

		// Also verify Encounter?status=in-progress matches post-enc-1
		encResp, err := http.Get(ts.URL + "/fhir/r4/test-api/Encounter?status=in-progress&_id=post-enc-1")
		if err != nil {
			t.Fatalf("GET /Encounter?status=in-progress: %v", err)
		}
		var encBundle map[string]any
		json.NewDecoder(encResp.Body).Decode(&encBundle)
		encResp.Body.Close()
		if int(encBundle["total"].(float64)) != 1 {
			t.Errorf("expected 1 match for /Encounter?status=in-progress, got %v", encBundle["total"])
		}
	})

	t.Run("Transaction Bundle Resolves Intra-Bundle urn:uuid References and Rejects Unsupported HTTP Methods", func(t *testing.T) {
		// 1. Forward and backward urn:uuid: reference resolution
		urnBundle := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"fullUrl": "urn:uuid:aaaa1111-2222-3333-4444-555555555555",
					"resource": {
						"resourceType": "Observation",
						"status": "final",
						"code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
						"subject": {"reference": "urn:uuid:bbbb1111-2222-3333-4444-555555555555"}
					},
					"request": {"method": "POST", "url": "Observation"}
				},
				{
					"fullUrl": "urn:uuid:bbbb1111-2222-3333-4444-555555555555",
					"resource": {
						"resourceType": "Patient",
						"name": [{"family": "UrnResolvedPatient"}]
					},
					"request": {"method": "POST", "url": "Patient"}
				}
			]
		}`
		respUrn, err := http.Post(ts.URL+"/fhir/r4/test-api", "application/fhir+json", strings.NewReader(urnBundle))
		if err != nil {
			t.Fatalf("POST urn:uuid bundle: %v", err)
		}
		urnBytes, _ := io.ReadAll(respUrn.Body)
		respUrn.Body.Close()
		if respUrn.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for urn:uuid bundle, got %d: %s", respUrn.StatusCode, urnBytes)
		}

		var urnResp map[string]any
		json.Unmarshal(urnBytes, &urnResp)
		entries := urnResp["entry"].([]any)
		obsFullURL := entries[0].(map[string]any)["fullUrl"].(string) // "Observation/<id>"
		patFullURL := entries[1].(map[string]any)["fullUrl"].(string) // "Patient/<id>"

		// Read back the created Observation and verify its subject.reference was rewritten to patFullURL
		respObs, err := http.Get(ts.URL + "/fhir/r4/test-api/" + obsFullURL)
		if err != nil {
			t.Fatalf("GET %s: %v", obsFullURL, err)
		}
		var obsMap map[string]any
		json.NewDecoder(respObs.Body).Decode(&obsMap)
		respObs.Body.Close()
		if gotRef := obsMap["subject"].(map[string]any)["reference"]; gotRef != patFullURL {
			t.Errorf("expected Observation subject.reference rewritten to %q, got %v", patFullURL, gotRef)
		}

		// Verify spidx_reference was indexed with the resolved Patient ID so GET /Observation?patient=Patient/<id> finds it
		respSearch, err := http.Get(ts.URL + "/fhir/r4/test-api/Observation?patient=" + patFullURL)
		if err != nil {
			t.Fatalf("GET /Observation?patient=%s: %v", patFullURL, err)
		}
		var searchBundle map[string]any
		json.NewDecoder(respSearch.Body).Decode(&searchBundle)
		respSearch.Body.Close()
		if int(searchBundle["total"].(float64)) != 1 {
			t.Errorf("expected 1 Observation linked to %s, got %v", patFullURL, searchBundle["total"])
		}

		// 2. Unsupported HTTP method (HEAD) in transaction Bundle returns 400 Bad Request
		badMethodBundle := `{
			"resourceType": "Bundle",
			"type": "transaction",
			"entry": [
				{
					"resource": {"resourceType": "Patient", "id": "bad-method-1", "name": [{"family": "Head"}]},
					"request": {"method": "HEAD", "url": "Patient/bad-method-1"}
				}
			]
		}`
		respBad, err := http.Post(ts.URL+"/fhir/r4/test-api", "application/fhir+json", strings.NewReader(badMethodBundle))
		if err != nil {
			t.Fatalf("POST bad method bundle: %v", err)
		}
		respBad.Body.Close()
		if respBad.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 Bad Request for unsupported HEAD method in Bundle, got %d", respBad.StatusCode)
		}
	})

	t.Run("Batch Bundle Partial Failure, Malformed Search Query 400, and CDC Publisher Emission", func(t *testing.T) {
		cdcMem := automq.NewMemoryPublisher()
		srv.SetCDCPublisher(cdcMem)
		defer srv.SetCDCPublisher(nil)

		// 1. Batch Bundle with mixed valid and invalid entries returns 200 batch-response with per-entry outcomes
		batchBundle := `{
			"resourceType": "Bundle",
			"type": "batch",
			"entry": [
				{
					"resource": {"resourceType": "Patient", "id": "batch-pat-1", "name": [{"family": "BatchSuccess"}]},
					"request": {"method": "PUT", "url": "Patient/batch-pat-1"}
				},
				{
					"resource": {"resourceType": "Patient", "id": "batch-pat-bad"},
					"request": {"method": "HEAD", "url": "Patient/batch-pat-bad"}
				},
				{
					"resource": {"resourceType": "Patient", "id": "batch-mismatch", "name": [{"family": "Mismatch"}]},
					"request": {"method": "PUT", "url": "Patient/other-id"}
				},
				{
					"request": {"method": "DELETE", "url": "Patient/batch-pat-1"}
				}
			]
		}`
		respBatch, err := http.Post(ts.URL+"/fhir/r4/test-api", "application/fhir+json", strings.NewReader(batchBundle))
		if err != nil {
			t.Fatalf("POST batch bundle: %v", err)
		}
		batchBytes, _ := io.ReadAll(respBatch.Body)
		respBatch.Body.Close()
		if respBatch.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for batch bundle, got %d: %s", respBatch.StatusCode, batchBytes)
		}
		var batchResp map[string]any
		json.Unmarshal(batchBytes, &batchResp)
		if batchResp["type"] != "batch-response" {
			t.Errorf("expected type=batch-response, got %v", batchResp["type"])
		}
		bEntries := batchResp["entry"].([]any)
		if len(bEntries) != 4 {
			t.Fatalf("expected 4 batch response entries, got %d", len(bEntries))
		}
		if status0 := bEntries[0].(map[string]any)["response"].(map[string]any)["status"].(string); !strings.HasPrefix(status0, "201") && !strings.HasPrefix(status0, "200") {
			t.Errorf("expected entry[0] 200/201, got %s", status0)
		}
		if status1 := bEntries[1].(map[string]any)["response"].(map[string]any)["status"].(string); !strings.HasPrefix(status1, "400") {
			t.Errorf("expected entry[1] 400 Bad Request, got %s", status1)
		}
		if status2 := bEntries[2].(map[string]any)["response"].(map[string]any)["status"].(string); !strings.HasPrefix(status2, "400") {
			t.Errorf("expected entry[2] 400 Bad Request, got %s", status2)
		}
		if status3 := bEntries[3].(map[string]any)["response"].(map[string]any)["status"].(string); !strings.HasPrefix(status3, "204") {
			t.Errorf("expected entry[3] 204 No Content, got %s", status3)
		}

		// Verify CDC events were emitted for the PUT and DELETE in the batch bundle
		msgs := cdcMem.Messages()
		if len(msgs) != 2 || msgs[0].Op != "U" || msgs[1].Op != "D" {
			t.Errorf("expected 2 CDC messages (U then D) from batch bundle, got %+v", msgs)
		}

		// 2. Malformed search query parameters return 400 Bad Request OperationOutcome
		for _, badSearch := range []string{
			"/fhir/r4/test-api/Observation?date=not-a-valid-date",
			"/fhir/r4/test-api/Observation?value-quantity=not-a-number",
			"/fhir/r4/test-api/Patient?_count=-1",
			"/fhir/r4/test-api/Patient?_include=MalformedInclude",
		} {
			respBadSearch, err := http.Get(ts.URL + badSearch)
			if err != nil {
				t.Fatalf("GET %s: %v", badSearch, err)
			}
			respBadSearch.Body.Close()
			if respBadSearch.StatusCode != http.StatusBadRequest {
				t.Errorf("expected 400 Bad Request for %s, got %d", badSearch, respBadSearch.StatusCode)
			}
		}
	})

	// Clean up
	for _, table := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri", "fhir_audit_event", "fhir_resource_history", "fhir_resource"} {
		db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = 'test-api'")
	}
}
