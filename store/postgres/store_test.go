package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/flint-fhir/flint/store/postgres"
)

// TestStore_Integration runs against a real Postgres instance.
// Set FLINT_TEST_POSTGRES_DSN to enable. Example:
//
//	FLINT_TEST_POSTGRES_DSN="postgres://flint:flint@localhost:5432/flint?sslmode=disable" go test ./store/postgres/ -v
func TestStore_Integration(t *testing.T) {
	dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("FLINT_TEST_POSTGRES_DSN not set; skipping integration test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Run migrations
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	store := postgres.New(db)

	t.Run("WriteAndReadResource", func(t *testing.T) {
		input := postgres.ResourceInput{
			TenantID:       "test-tenant",
			ResType:        "Patient",
			ResID:          "pat-001",
			ResourceProto:  []byte{0x0a, 0x07, 0x70, 0x61, 0x74, 0x2d, 0x30, 0x30, 0x31}, // mock proto bytes
			IdempotencyKey: "bundle-123",
			SearchIndexes: &postgres.SearchIndexes{
				Strings: []postgres.SpidxString{
					{TenantID: "test-tenant", ResType: "Patient", ResID: "pat-001", SpName: "family", SpValue: "smith"},
					{TenantID: "test-tenant", ResType: "Patient", ResID: "pat-001", SpName: "given", SpValue: "john"},
				},
				Tokens: []postgres.SpidxToken{
					{TenantID: "test-tenant", ResType: "Patient", ResID: "pat-001", SpName: "identifier", SpSystem: "http://mrn", SpValue: "MRN-456"},
					{TenantID: "test-tenant", ResType: "Patient", ResID: "pat-001", SpName: "active", SpValue: "true"},
				},
				Dates: []postgres.SpidxDate{
					{TenantID: "test-tenant", ResType: "Patient", ResID: "pat-001", SpName: "birthdate",
						SpLow: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC), SpHigh: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)},
				},
				References: []postgres.SpidxReference{
					{TenantID: "test-tenant", ResType: "Patient", ResID: "pat-001", SpName: "organization", TargetType: "Organization", TargetID: "org-789"},
				},
			},
		}

		// Write
		if err := store.WriteResource(ctx, input); err != nil {
			t.Fatalf("WriteResource: %v", err)
		}

		// Read
		proto, err := store.ReadResource(ctx, "test-tenant", "Patient", "pat-001")
		if err != nil {
			t.Fatalf("ReadResource: %v", err)
		}
		if len(proto) == 0 {
			t.Fatal("empty proto")
		}
		t.Logf("Read %d bytes of proto", len(proto))

		// Verify search indexes
		var count int
		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM spidx_string WHERE tenant_id = $1 AND res_id = $2",
			"test-tenant", "pat-001").Scan(&count)
		if count != 2 {
			t.Errorf("expected 2 string indexes, got %d", count)
		}

		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM spidx_token WHERE tenant_id = $1 AND res_id = $2",
			"test-tenant", "pat-001").Scan(&count)
		if count != 2 {
			t.Errorf("expected 2 token indexes, got %d", count)
		}

		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM spidx_date WHERE tenant_id = $1 AND res_id = $2",
			"test-tenant", "pat-001").Scan(&count)
		if count != 1 {
			t.Errorf("expected 1 date index, got %d", count)
		}

		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM spidx_reference WHERE tenant_id = $1 AND res_id = $2",
			"test-tenant", "pat-001").Scan(&count)
		if count != 1 {
			t.Errorf("expected 1 reference index, got %d", count)
		}
	})

	t.Run("UpdateReplacesIndexes", func(t *testing.T) {
		// Update patient with new name
		input := postgres.ResourceInput{
			TenantID:      "test-tenant",
			ResType:       "Patient",
			ResID:         "pat-001",
			ResourceProto: []byte{0x0a, 0x07, 0x75, 0x70, 0x64, 0x61, 0x74, 0x65, 0x64},
			SearchIndexes: &postgres.SearchIndexes{
				Strings: []postgres.SpidxString{
					{TenantID: "test-tenant", ResType: "Patient", ResID: "pat-001", SpName: "family", SpValue: "smith-jones"},
				},
			},
		}
		if err := store.WriteResource(ctx, input); err != nil {
			t.Fatalf("WriteResource (update): %v", err)
		}

		// Old "smith" should be gone, only "smith-jones" now
		var count int
		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM spidx_string WHERE tenant_id = $1 AND res_id = $2",
			"test-tenant", "pat-001").Scan(&count)
		if count != 1 {
			t.Errorf("expected 1 string index after update, got %d", count)
		}

		// Old tokens should also be gone
		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM spidx_token WHERE tenant_id = $1 AND res_id = $2",
			"test-tenant", "pat-001").Scan(&count)
		if count != 0 {
			t.Errorf("expected 0 token indexes after update, got %d", count)
		}
	})

	t.Run("DeleteResource", func(t *testing.T) {
		if err := store.DeleteResource(ctx, "test-tenant", "Patient", "pat-001"); err != nil {
			t.Fatalf("DeleteResource: %v", err)
		}

		// Read should return not found
		_, err := store.ReadResource(ctx, "test-tenant", "Patient", "pat-001")
		if err != sql.ErrNoRows {
			t.Errorf("expected ErrNoRows, got %v", err)
		}

		// Search indexes should be gone
		var count int
		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM spidx_string WHERE tenant_id = $1 AND res_id = $2",
			"test-tenant", "pat-001").Scan(&count)
		if count != 0 {
			t.Errorf("expected 0 string indexes after delete, got %d", count)
		}
	})

	t.Run("WriteBatch", func(t *testing.T) {
		inputs := []postgres.ResourceInput{
			{
				TenantID:      "test-tenant",
				ResType:       "Patient",
				ResID:         "pat-100",
				ResourceProto: []byte{1, 2, 3},
				SearchIndexes: &postgres.SearchIndexes{
					Strings: []postgres.SpidxString{
						{TenantID: "test-tenant", ResType: "Patient", ResID: "pat-100", SpName: "family", SpValue: "doe"},
					},
				},
			},
			{
				TenantID:      "test-tenant",
				ResType:       "Patient",
				ResID:         "pat-101",
				ResourceProto: []byte{4, 5, 6},
				SearchIndexes: &postgres.SearchIndexes{
					Strings: []postgres.SpidxString{
						{TenantID: "test-tenant", ResType: "Patient", ResID: "pat-101", SpName: "family", SpValue: "roe"},
					},
				},
			},
		}

		if err := store.WriteBatch(ctx, inputs); err != nil {
			t.Fatalf("WriteBatch: %v", err)
		}

		// Verify both written
		for _, id := range []string{"pat-100", "pat-101"} {
			_, err := store.ReadResource(ctx, "test-tenant", "Patient", id)
			if err != nil {
				t.Errorf("ReadResource(%s): %v", id, err)
			}
		}
	})

	t.Run("SearchWithPagination", func(t *testing.T) {
		// Seed 5 patients with family=pagination
		for i := range 5 {
			id := fmt.Sprintf("pag-%d", i)
			err := store.WriteResource(ctx, postgres.ResourceInput{
				TenantID:      "test-tenant",
				ResType:       "Patient",
				ResID:         id,
				ResourceProto: []byte{0x0a, byte(i)},
				SearchIndexes: &postgres.SearchIndexes{
					Strings: []postgres.SpidxString{
						{TenantID: "test-tenant", ResType: "Patient", ResID: id, SpName: "family", SpValue: "pagination"},
					},
				},
			})
			if err != nil {
				t.Fatalf("seed pag-%d: %v", i, err)
			}
		}

		// Page 1: count=2, offset=0
		sr1, err := store.Search(ctx, postgres.SearchParams{
			TenantID: "test-tenant",
			ResType:  "Patient",
			Strings:  map[string]string{"family": "pagination"},
			Count:    2,
			Offset:   0,
		})
		if err != nil {
			t.Fatalf("search page 1: %v", err)
		}
		if sr1.Total != 5 {
			t.Errorf("expected total 5, got %d", sr1.Total)
		}
		if len(sr1.Matches) != 2 {
			t.Errorf("expected 2 results, got %d", len(sr1.Matches))
		}
		t.Logf("Page 1: total=%d, results=%d, first=%s", sr1.Total, len(sr1.Matches), sr1.Matches[0].ResID)

		// Page 2: count=2, offset=2
		sr2, err := store.Search(ctx, postgres.SearchParams{
			TenantID: "test-tenant",
			ResType:  "Patient",
			Strings:  map[string]string{"family": "pagination"},
			Count:    2,
			Offset:   2,
		})
		if err != nil {
			t.Fatalf("search page 2: %v", err)
		}
		if sr2.Total != 5 {
			t.Errorf("page 2 total should still be 5, got %d", sr2.Total)
		}
		if len(sr2.Matches) != 2 {
			t.Errorf("expected 2 results, got %d", len(sr2.Matches))
		}
		// Verify pages don't overlap
		if sr1.Matches[0].ResID == sr2.Matches[0].ResID {
			t.Errorf("pages overlap: both start with %s", sr1.Matches[0].ResID)
		}
		t.Logf("Page 2: total=%d, results=%d, first=%s", sr2.Total, len(sr2.Matches), sr2.Matches[0].ResID)

		// Page 3: count=2, offset=4 — should get 1 result
		sr3, err := store.Search(ctx, postgres.SearchParams{
			TenantID: "test-tenant",
			ResType:  "Patient",
			Strings:  map[string]string{"family": "pagination"},
			Count:    2,
			Offset:   4,
		})
		if err != nil {
			t.Fatalf("search page 3: %v", err)
		}
		if len(sr3.Matches) != 1 {
			t.Errorf("last page expected 1 result, got %d", len(sr3.Matches))
		}
		t.Logf("Page 3: results=%d (last page)", len(sr3.Matches))
	})

	t.Run("VersioningHistoryAndCAS", func(t *testing.T) {
		v1Bytes := []byte("v1-payload")
		v2Bytes := []byte("v2-payload")

		rec1, err := store.WriteResourceWithMeta(ctx, postgres.ResourceInput{
			TenantID:      "test-tenant",
			ResType:       "Patient",
			ResID:         "pat-cas-1",
			ResourceProto: v1Bytes,
		})
		if err != nil {
			t.Fatalf("WriteResourceWithMeta v1: %v", err)
		}
		if rec1.ResVersion != 1 || !rec1.Created {
			t.Fatalf("expected version 1 created=true, got version=%d created=%v", rec1.ResVersion, rec1.Created)
		}

		// Update with matching ExpectedVersion=1 -> version 2
		rec2, err := store.WriteResourceWithMeta(ctx, postgres.ResourceInput{
			TenantID:        "test-tenant",
			ResType:         "Patient",
			ResID:           "pat-cas-1",
			ResourceProto:   v2Bytes,
			ExpectedVersion: 1,
		})
		if err != nil {
			t.Fatalf("WriteResourceWithMeta v2: %v", err)
		}
		if rec2.ResVersion != 2 || rec2.Created {
			t.Fatalf("expected version 2 created=false, got version=%d created=%v", rec2.ResVersion, rec2.Created)
		}

		// Stale ExpectedVersion=1 -> ErrVersionConflict
		_, err = store.WriteResourceWithMeta(ctx, postgres.ResourceInput{
			TenantID:        "test-tenant",
			ResType:         "Patient",
			ResID:           "pat-cas-1",
			ResourceProto:   []byte("stale"),
			ExpectedVersion: 1,
		})
		if !errors.Is(err, postgres.ErrVersionConflict) {
			t.Fatalf("expected ErrVersionConflict, got %v", err)
		}

		// vread v1 and v2
		hist1, err := store.ReadResourceVersion(ctx, "test-tenant", "Patient", "pat-cas-1", 1)
		if err != nil || !bytes.Equal(hist1.ResourceProto, v1Bytes) {
			t.Fatalf("ReadResourceVersion(1): err=%v proto=%q", err, hist1.ResourceProto)
		}
		hist2, err := store.ReadResourceVersion(ctx, "test-tenant", "Patient", "pat-cas-1", 2)
		if err != nil || !bytes.Equal(hist2.ResourceProto, v2Bytes) {
			t.Fatalf("ReadResourceVersion(2): err=%v proto=%q", err, hist2.ResourceProto)
		}

		// Delete -> version 3 tombstone
		delRec, err := store.DeleteResourceWithMeta(ctx, "test-tenant", "Patient", "pat-cas-1")
		if err != nil {
			t.Fatalf("DeleteResourceWithMeta: %v", err)
		}
		if delRec.ResVersion != 3 || !delRec.IsDeleted {
			t.Fatalf("expected deleted version 3, got %+v", delRec)
		}

		// ReadResourceWithMeta returns ErrResourceDeleted
		_, err = store.ReadResourceWithMeta(ctx, "test-tenant", "Patient", "pat-cas-1")
		if !errors.Is(err, postgres.ErrResourceDeleted) {
			t.Fatalf("expected ErrResourceDeleted, got %v", err)
		}

		// vread v3 returns ErrResourceDeleted
		_, err = store.ReadResourceVersion(ctx, "test-tenant", "Patient", "pat-cas-1", 3)
		if !errors.Is(err, postgres.ErrResourceDeleted) {
			t.Fatalf("expected ErrResourceDeleted on vread(3), got %v", err)
		}

		// ListResourceHistory returns [3, 2, 1]
		history, total, err := store.ListResourceHistory(ctx, "test-tenant", "Patient", "pat-cas-1", 10, 0)
		if err != nil {
			t.Fatalf("ListResourceHistory: %v", err)
		}
		if total != 3 || len(history) != 3 {
			t.Fatalf("expected 3 history records, got total=%d len=%d", total, len(history))
		}
		if history[0].ResVersion != 3 || !history[0].IsDeleted || history[1].ResVersion != 2 || history[2].ResVersion != 1 {
			t.Fatalf("unexpected history order: %+v", history)
		}
	})

	t.Run("AuditEventWriteReadAndQuery", func(t *testing.T) {
		rec1 := postgres.AuditRecord{
			TenantID:      "test-tenant",
			AuditID:       "audit-001",
			Recorded:      time.Now().UTC().Add(-time.Minute),
			Action:        "C",
			SubtypeCode:   "create",
			Outcome:       "0",
			OutcomeDesc:   "201 Created",
			HTTPMethod:    "POST",
			HTTPStatus:    201,
			RequestURI:    "/fhir/r4/test-tenant/Patient",
			AgentSubject:  "Practitioner/dr-smith",
			AgentPatient:  "pat-audit-1",
			ClientIP:      "10.0.0.1",
			EntityType:    "Patient",
			EntityID:      "pat-audit-1",
			EntityVersion: "1",
		}
		rec2 := postgres.AuditRecord{
			TenantID:     "test-tenant",
			AuditID:      "audit-002",
			Recorded:     time.Now().UTC(),
			Action:       "R",
			SubtypeCode:  "read",
			Outcome:      "4",
			OutcomeDesc:  "403 Forbidden",
			HTTPMethod:   "GET",
			HTTPStatus:   403,
			RequestURI:   "/fhir/r4/test-tenant/Patient/pat-audit-1",
			AgentSubject: "Patient/intruder",
			ClientIP:     "10.0.0.2",
			EntityType:   "Patient",
			EntityID:     "pat-audit-1",
		}

		if err := store.WriteAuditEvent(ctx, rec1); err != nil {
			t.Fatalf("WriteAuditEvent rec1: %v", err)
		}
		if err := store.WriteAuditEvent(ctx, rec2); err != nil {
			t.Fatalf("WriteAuditEvent rec2: %v", err)
		}

		got1, err := store.ReadAuditEvent(ctx, "test-tenant", "audit-001")
		if err != nil {
			t.Fatalf("ReadAuditEvent: %v", err)
		}
		if got1.Action != "C" || got1.AgentSubject != "Practitioner/dr-smith" || got1.EntityID != "pat-audit-1" {
			t.Fatalf("unexpected ReadAuditEvent result: %+v", got1)
		}

		// Query by outcome=4 (security rejection)
		rejected, total, err := store.QueryAuditEvents(ctx, postgres.AuditQueryParams{
			TenantID: "test-tenant",
			Outcome:  "4",
		})
		if err != nil {
			t.Fatalf("QueryAuditEvents: %v", err)
		}
		if total != 1 || len(rejected) != 1 || rejected[0].AuditID != "audit-002" {
			t.Fatalf("expected 1 rejected audit event (audit-002), got total=%d records=%+v", total, rejected)
		}
	})

	// Clean up
	for _, table := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri", "fhir_audit_event", "fhir_resource_history", "fhir_resource"} {
		db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = 'test-tenant'")
	}
}
