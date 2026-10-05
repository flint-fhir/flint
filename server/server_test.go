package server_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"log/slog"

	_ "github.com/lib/pq"
	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"

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
	migration, err := os.ReadFile("../store/postgres/migrations/001_core_schema.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), string(migration)); err != nil {
		t.Fatalf("run migration: %v", err)
	}

	store := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := server.New(store, logger)
	srv.RegisterResourceType("Patient", &patpb.Patient{})
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
				},
				Tokens: []postgres.SpidxToken{
					{TenantID: "test-api", ResType: "Patient", ResID: "api-pat-2", SpName: "identifier", SpSystem: "http://mrn", SpValue: "MRN-200"},
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

	// Clean up
	for _, table := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri", "fhir_resource"} {
		db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = 'test-api'")
	}
}
