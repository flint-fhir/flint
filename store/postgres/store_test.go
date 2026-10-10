package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
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

	t.Run("PatientEverything", func(t *testing.T) {
		// Seed a Patient, a Practitioner, an Encounter referencing both, and an Observation referencing the Patient
		if err := store.WriteResource(ctx, postgres.ResourceInput{
			TenantID:      "test-tenant",
			ResType:       "Practitioner",
			ResID:         "prac-ev-1",
			ResourceProto: []byte("prac-proto"),
		}); err != nil {
			t.Fatalf("write practitioner: %v", err)
		}
		if err := store.WriteResource(ctx, postgres.ResourceInput{
			TenantID:      "test-tenant",
			ResType:       "Patient",
			ResID:         "pat-ev-1",
			ResourceProto: []byte("pat-proto"),
		}); err != nil {
			t.Fatalf("write patient: %v", err)
		}
		if err := store.WriteResource(ctx, postgres.ResourceInput{
			TenantID:      "test-tenant",
			ResType:       "Encounter",
			ResID:         "enc-ev-1",
			ResourceProto: []byte("enc-proto"),
			SearchIndexes: &postgres.SearchIndexes{
				References: []postgres.SpidxReference{
					{TenantID: "test-tenant", ResType: "Encounter", ResID: "enc-ev-1", SpName: "subject", TargetType: "Patient", TargetID: "pat-ev-1"},
					{TenantID: "test-tenant", ResType: "Encounter", ResID: "enc-ev-1", SpName: "participant", TargetType: "Practitioner", TargetID: "prac-ev-1"},
				},
			},
		}); err != nil {
			t.Fatalf("write encounter: %v", err)
		}
		if err := store.WriteResource(ctx, postgres.ResourceInput{
			TenantID:      "test-tenant",
			ResType:       "Observation",
			ResID:         "obs-ev-1",
			ResourceProto: []byte("obs-proto"),
			SearchIndexes: &postgres.SearchIndexes{
				References: []postgres.SpidxReference{
					{TenantID: "test-tenant", ResType: "Observation", ResID: "obs-ev-1", SpName: "subject", TargetType: "Patient", TargetID: "pat-ev-1"},
				},
			},
		}); err != nil {
			t.Fatalf("write observation: %v", err)
		}

		sr, err := store.PatientEverything(ctx, postgres.PatientEverythingParams{
			TenantID:  "test-tenant",
			PatientID: "pat-ev-1",
		})
		if err != nil {
			t.Fatalf("PatientEverything: %v", err)
		}
		// Expect 1 match (Patient) + 3 includes (Encounter, Observation, and Encounter's participant Practitioner)
		if len(sr.Matches) != 1 || sr.Matches[0].ResID != "pat-ev-1" {
			t.Fatalf("expected 1 Patient match, got %+v", sr.Matches)
		}
		if len(sr.Includes) != 3 || sr.Total != 4 {
			t.Fatalf("expected 3 compartment includes (total 4), got len=%d total=%d includes=%+v", len(sr.Includes), sr.Total, sr.Includes)
		}

		// Filter by _type=Observation
		srObs, err := store.PatientEverything(ctx, postgres.PatientEverythingParams{
			TenantID:      "test-tenant",
			PatientID:     "pat-ev-1",
			ResourceTypes: []string{"Observation"},
		})
		if err != nil {
			t.Fatalf("PatientEverything(_type=Observation): %v", err)
		}
		if len(srObs.Matches) != 0 || len(srObs.Includes) != 1 || srObs.Includes[0].ResType != "Observation" {
			t.Fatalf("expected only Observation in filtered $everything, got matches=%+v includes=%+v", srObs.Matches, srObs.Includes)
		}

		// Filter by _since in the past -> 1 Patient match + 3 compartment includes
		past := time.Now().UTC().Add(-time.Hour)
		srPast, err := store.PatientEverything(ctx, postgres.PatientEverythingParams{
			TenantID:  "test-tenant",
			PatientID: "pat-ev-1",
			Since:     &past,
		})
		if err != nil {
			t.Fatalf("PatientEverything(_since=past): %v", err)
		}
		if len(srPast.Matches) != 1 || len(srPast.Includes) != 3 {
			t.Fatalf("expected 1 Patient match and 3 includes for past _since, got matches=%d includes=%d", len(srPast.Matches), len(srPast.Includes))
		}

		// Filter by _since in the future -> 0 matches and 0 compartment includes
		future := time.Now().UTC().Add(time.Hour)
		srFuture, err := store.PatientEverything(ctx, postgres.PatientEverythingParams{
			TenantID:  "test-tenant",
			PatientID: "pat-ev-1",
			Since:     &future,
		})
		if err != nil {
			t.Fatalf("PatientEverything(_since=future): %v", err)
		}
		if len(srFuture.Matches) != 0 || len(srFuture.Includes) != 0 {
			t.Fatalf("expected 0 matches and 0 includes for future _since, got matches=%d includes=%d", len(srFuture.Matches), len(srFuture.Includes))
		}

		// Pagination (_count=1, _offset=1) on compartment includes
		srPage, err := store.PatientEverything(ctx, postgres.PatientEverythingParams{
			TenantID:  "test-tenant",
			PatientID: "pat-ev-1",
			Count:     1,
			Offset:    1,
		})
		if err != nil {
			t.Fatalf("PatientEverything(count=1,offset=1): %v", err)
		}
		if srPage.Total != 4 || len(srPage.Includes) != 1 {
			t.Fatalf("expected Total=4 and 1 paginated include, got Total=%d len(Includes)=%d", srPage.Total, len(srPage.Includes))
		}

		// Soft-deleting Observation removes it from PatientEverything
		if _, err := store.DeleteResourceWithMeta(ctx, "test-tenant", "Observation", "obs-ev-1"); err != nil {
			t.Fatalf("delete obs-ev-1: %v", err)
		}
		srAfterDel, err := store.PatientEverything(ctx, postgres.PatientEverythingParams{
			TenantID:  "test-tenant",
			PatientID: "pat-ev-1",
		})
		if err != nil {
			t.Fatalf("PatientEverything after delete: %v", err)
		}
		if srAfterDel.Total != 3 || len(srAfterDel.Includes) != 2 {
			t.Fatalf("expected Total=3 (Patient + Encounter + Practitioner) after deleting Observation, got Total=%d len=%d", srAfterDel.Total, len(srAfterDel.Includes))
		}
	})

	t.Run("CreateOnly rejects duplicate active ID with ErrResourceAlreadyExists and allows re-creation after soft delete", func(t *testing.T) {
		input := postgres.ResourceInput{
			TenantID:      "test-tenant",
			ResType:       "Patient",
			ResID:         "pat-create-only-1",
			ResourceProto: []byte("v1"),
			CreateOnly:    true,
		}
		rec1, err := store.WriteResourceWithMeta(ctx, input)
		if err != nil || !rec1.Created || rec1.ResVersion != 1 {
			t.Fatalf("expected initial CreateOnly write to succeed at v1, got rec=%+v err=%v", rec1, err)
		}

		// Second CreateOnly with same ID must fail with ErrResourceAlreadyExists
		input.ResourceProto = []byte("v2-illegal-overwrite")
		_, err = store.WriteResourceWithMeta(ctx, input)
		if !errors.Is(err, postgres.ErrResourceAlreadyExists) {
			t.Fatalf("expected ErrResourceAlreadyExists on duplicate CreateOnly, got %v", err)
		}

		// Soft-delete and then CreateOnly again -> succeeds at v3
		if _, err := store.DeleteResourceWithMeta(ctx, "test-tenant", "Patient", "pat-create-only-1"); err != nil {
			t.Fatalf("delete pat-create-only-1: %v", err)
		}
		input.ResourceProto = []byte("v3-recreated")
		rec3, err := store.WriteResourceWithMeta(ctx, input)
		if err != nil || !rec3.Created || rec3.ResVersion != 3 {
			t.Fatalf("expected CreateOnly after delete to succeed at v3, got rec=%+v err=%v", rec3, err)
		}
	})

	t.Run("Concurrent ConditionalCreateResource serializes via advisory lock without TOCTOU duplicates", func(t *testing.T) {
		const workers = 10
		condParams, err := postgres.ParseIfNoneExist("test-tenant", "Patient", "identifier=http://hospital.org/mrn|CONC-MRN-999")
		if err != nil {
			t.Fatalf("ParseIfNoneExist: %v", err)
		}

		var wg sync.WaitGroup
		createdCount := 0
		matchedCount := 0
		var mu sync.Mutex
		var winningID string
		var errs []error

		for i := range workers {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				resID := fmt.Sprintf("pat-conc-%d", workerID)
				input := postgres.ResourceInput{
					TenantID:      "test-tenant",
					ResType:       "Patient",
					ResID:         resID,
					ResourceProto: []byte(fmt.Sprintf("proto-%d", workerID)),
					CreateOnly:    true,
					SearchIndexes: &postgres.SearchIndexes{
						Tokens: []postgres.SpidxToken{
							{
								TenantID: "test-tenant",
								ResType:  "Patient",
								ResID:    resID,
								SpName:   "identifier",
								SpSystem: "http://hospital.org/mrn",
								SpValue:  "CONC-MRN-999",
							},
						},
					},
				}
				rec, created, cErr := store.ConditionalCreateResource(ctx, input, condParams)
				mu.Lock()
				defer mu.Unlock()
				if cErr != nil {
					errs = append(errs, cErr)
					return
				}
				if created {
					createdCount++
				} else {
					matchedCount++
				}
				if winningID == "" {
					winningID = rec.ResID
				} else if winningID != rec.ResID {
					errs = append(errs, fmt.Errorf("expected all workers to converge on ID %s, got %s", winningID, rec.ResID))
				}
			}(i)
		}
		wg.Wait()

		if len(errs) > 0 {
			t.Fatalf("concurrent ConditionalCreateResource errors: %v", errs)
		}
		if createdCount != 1 || matchedCount != workers-1 {
			t.Fatalf("expected exactly 1 created and %d matched, got created=%d matched=%d", workers-1, createdCount, matchedCount)
		}
	})

	t.Run("ExecuteTransactionBundle rolls back earlier DELETE and POST when subsequent entry fails", func(t *testing.T) {
		// Seed a resource to be deleted in op[0] and another resource for op[2] If-Match conflict
		if _, err := store.WriteResourceWithMeta(ctx, postgres.ResourceInput{
			TenantID:      "test-tenant",
			ResType:       "Patient",
			ResID:         "tx-survivor-1",
			ResourceProto: []byte("survivor-v1"),
		}); err != nil {
			t.Fatalf("seed tx-survivor-1: %v", err)
		}

		ops := []postgres.BundleOperation{
			{
				Method: "DELETE",
				Input: postgres.ResourceInput{
					TenantID: "test-tenant",
					ResType:  "Patient",
					ResID:    "tx-survivor-1",
				},
			},
			{
				Method: "POST",
				Input: postgres.ResourceInput{
					TenantID:      "test-tenant",
					ResType:       "Patient",
					ResID:         "tx-ephemeral-1",
					ResourceProto: []byte("ephemeral"),
					CreateOnly:    true,
				},
			},
			{
				Method: "PUT",
				Input: postgres.ResourceInput{
					TenantID:        "test-tenant",
					ResType:         "Patient",
					ResID:           "tx-survivor-1",
					ResourceProto:   []byte("conflict"),
					ExpectedVersion: 99, // Stale version -> triggers ErrVersionConflict
				},
			},
		}

		_, err := store.ExecuteTransactionBundle(ctx, ops)
		if !errors.Is(err, postgres.ErrVersionConflict) {
			t.Fatalf("expected ErrVersionConflict from ExecuteTransactionBundle, got %v", err)
		}

		// Verify tx-survivor-1 was NOT deleted (DELETE in op[0] rolled back!)
		survivor, err := store.ReadResourceWithMeta(ctx, "test-tenant", "Patient", "tx-survivor-1")
		if err != nil || survivor.IsDeleted || survivor.ResVersion != 1 {
			t.Fatalf("expected tx-survivor-1 to remain intact at v1 after rollback, got rec=%+v err=%v", survivor, err)
		}

		// Verify tx-ephemeral-1 was NOT created (POST in op[1] rolled back!)
		_, err = store.ReadResourceWithMeta(ctx, "test-tenant", "Patient", "tx-ephemeral-1")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected tx-ephemeral-1 to not exist after rollback, got %v", err)
		}
	})

	t.Run("BelongsToPatientCompartment and large bulkInsert chunking (>2000 index rows)", func(t *testing.T) {
		manyStrings := make([]postgres.SpidxString, 2500)
		for i := range manyStrings {
			manyStrings[i] = postgres.SpidxString{
				TenantID: "test-tenant",
				ResType:  "Observation",
				ResID:    "obs-large-idx-1",
				SpName:   "note",
				SpValue:  fmt.Sprintf("chunk-val-%d", i),
			}
		}
		idx := &postgres.SearchIndexes{
			Strings: manyStrings,
			References: []postgres.SpidxReference{
				{
					TenantID:   "test-tenant",
					ResType:    "Observation",
					ResID:      "obs-large-idx-1",
					SpName:     "patient",
					TargetType: "Patient",
					TargetID:   "pat-ev-1",
				},
			},
		}

		if !postgres.IndexesBelongToPatientCompartment("Observation", "obs-large-idx-1", idx, "pat-ev-1") {
			t.Fatal("expected IndexesBelongToPatientCompartment to return true for pat-ev-1")
		}
		if postgres.IndexesBelongToPatientCompartment("Observation", "obs-large-idx-1", idx, "pat-other") {
			t.Fatal("expected IndexesBelongToPatientCompartment to return false for pat-other")
		}

		if err := store.WriteResource(ctx, postgres.ResourceInput{
			TenantID:      "test-tenant",
			ResType:       "Observation",
			ResID:         "obs-large-idx-1",
			ResourceProto: []byte("large-obs"),
			SearchIndexes: idx,
		}); err != nil {
			t.Fatalf("WriteResource with 2500 string indexes failed: %v", err)
		}

		belongs, err := store.BelongsToPatientCompartment(ctx, "test-tenant", "Observation", "obs-large-idx-1", "pat-ev-1")
		if err != nil || !belongs {
			t.Fatalf("expected BelongsToPatientCompartment=true for pat-ev-1, got belongs=%v err=%v", belongs, err)
		}
		belongsOther, err := store.BelongsToPatientCompartment(ctx, "test-tenant", "Observation", "obs-large-idx-1", "pat-other")
		if err != nil || belongsOther {
			t.Fatalf("expected BelongsToPatientCompartment=false for pat-other, got belongs=%v err=%v", belongsOther, err)
		}
	})

	// Clean up
	for _, table := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri", "fhir_audit_event", "fhir_resource_history", "fhir_resource"} {
		db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = 'test-tenant'")
	}
}
