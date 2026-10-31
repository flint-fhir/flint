package doris_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
)

type stubDorisDriver struct {
	execErr error
}

type stubDorisConn struct {
	execErr error
}

func (d *stubDorisDriver) Open(_ string) (driver.Conn, error) {
	return &stubDorisConn{execErr: d.execErr}, nil
}

func (c *stubDorisConn) Prepare(_ string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *stubDorisConn) Close() error                          { return nil }
func (c *stubDorisConn) Begin() (driver.Tx, error)             { return nil, errors.New("unsupported") }
func (c *stubDorisConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if c.execErr != nil {
		return nil, c.execErr
	}
	return driver.RowsAffected(1), nil
}

var registerDorisDriversOnce sync.Once

func registerDorisDrivers() {
	registerDorisDriversOnce.Do(func() {
		sql.Register("doris_stub_ok", &stubDorisDriver{})
		sql.Register("doris_stub_err", &stubDorisDriver{execErr: errors.New("doris write error")})
	})
}

func TestDorisDDLAndGroupCommit(t *testing.T) {
	registerDorisDrivers()

	mode, err := doris.ParseGroupCommitMode("")
	require.NoError(t, err)
	assert.Equal(t, doris.GroupCommitSync, mode)

	mode, err = doris.ParseGroupCommitMode("async_mode")
	require.NoError(t, err)
	assert.Equal(t, doris.GroupCommitAsync, mode)

	mode, err = doris.ParseGroupCommitMode("off")
	require.NoError(t, err)
	assert.Equal(t, doris.GroupCommitOff, mode)

	_, err = doris.ParseGroupCommitMode("invalid")
	require.ErrorContains(t, err, "invalid doris group commit mode")

	ddl := doris.GenerateResourceTableDDL("Observation")
	assert.Contains(t, ddl, "CREATE TABLE IF NOT EXISTS fhir_observation")
	assert.Contains(t, ddl, `"enable_unique_key_merge_on_write" = "true"`)
	assert.Contains(t, ddl, `"function_column.sequence_col" = "res_version"`)
	assert.Contains(t, ddl, `"store_row_column" = "true"`)
	assert.Contains(t, ddl, "USING INVERTED")
	assert.Contains(t, ddl, "resource_proto_hex STRING")

	stmts := doris.SchemaDDL()
	assert.Len(t, stmts, 4)

	require.NoError(t, doris.RunMigrations(context.Background(), nil))

	dbOK, err := sql.Open("doris_stub_ok", "")
	require.NoError(t, err)
	defer dbOK.Close()
	require.NoError(t, doris.RunMigrations(context.Background(), dbOK))

	dbErr, err := sql.Open("doris_stub_err", "")
	require.NoError(t, err)
	defer dbErr.Close()
	require.ErrorContains(t, doris.RunMigrations(context.Background(), dbErr), "doris migration[0]")
}

func TestDorisStore_CRUD_OptimisticLocking_And_Transactions(t *testing.T) {
	registerDorisDrivers()
	dbOK, err := sql.Open("doris_stub_ok", "")
	require.NoError(t, err)
	defer dbOK.Close()

	ds := doris.New(dbOK, doris.WithGroupCommit(doris.GroupCommitAsync), doris.WithDatabase("flint_analytics"))
	assert.Equal(t, "doris", ds.BackendName())
	assert.Equal(t, doris.GroupCommitAsync, ds.GroupCommit())
	assert.Equal(t, "flint_analytics", ds.Database())
	require.NoError(t, ds.Ping(context.Background()))

	ctx := context.Background()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	patInput := store.ResourceInput{
		TenantID:      "t1",
		ResType:       "Patient",
		ResID:         "pat-1",
		ResourceProto: []byte{0x0a, 0x05, 'p', 'a', 't', '-', '1'},
		CreateOnly:    true,
		SearchIndexes: &store.SearchIndexes{
			Strings: []store.SpidxString{
				{TenantID: "t1", ResType: "Patient", ResID: "pat-1", SpName: "family", SpValue: "Smith"},
			},
			Tokens: []store.SpidxToken{
				{TenantID: "t1", ResType: "Patient", ResID: "pat-1", SpName: "identifier", SpSystem: "http://hospital.org/mrn", SpValue: "MRN-100"},
				{TenantID: "t1", ResType: "Patient", ResID: "pat-1", SpName: "_tag", SpSystem: "http://flint.health/tag", SpValue: "vip"},
				{TenantID: "t1", ResType: "Patient", ResID: "pat-1", SpName: "_security", SpSystem: "http://terminology.hl7.org/CodeSystem/v3-Confidentiality", SpValue: "R"},
			},
			URIs: []store.SpidxURI{
				{TenantID: "t1", ResType: "Patient", ResID: "pat-1", SpName: "_profile", SpValue: "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
			},
		},
	}

	rec, err := ds.WriteResourceWithMeta(ctx, patInput)
	require.NoError(t, err)
	assert.Equal(t, 1, rec.ResVersion)
	assert.True(t, rec.Created)

	// Duplicate CreateOnly fails with ErrResourceAlreadyExists
	_, err = ds.WriteResourceWithMeta(ctx, patInput)
	require.ErrorIs(t, err, store.ErrResourceAlreadyExists)

	// Optimistic concurrency (If-Match) mismatch fails with ErrVersionConflict
	updInput := patInput
	updInput.CreateOnly = false
	updInput.ExpectedVersion = 99
	_, err = ds.WriteResourceWithMeta(ctx, updInput)
	require.ErrorIs(t, err, store.ErrVersionConflict)

	// Matching ExpectedVersion = 1 succeeds and increments to version 2
	updInput.ExpectedVersion = 1
	rec2, err := ds.WriteResourceWithMeta(ctx, updInput)
	require.NoError(t, err)
	assert.Equal(t, 2, rec2.ResVersion)

	// Read current and historical versions
	protoBytes, err := ds.ReadResource(ctx, "t1", "Patient", "pat-1")
	require.NoError(t, err)
	assert.Equal(t, patInput.ResourceProto, protoBytes)

	v1, err := ds.ReadResourceVersion(ctx, "t1", "Patient", "pat-1", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, v1.ResVersion)

	hist, totalHist, err := ds.ListResourceHistory(ctx, "t1", "Patient", "pat-1", 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, totalHist)
	assert.Len(t, hist, 2)

	// Concurrent If-None-Exist conditional create (20 goroutines -> exactly 1 created)
	var wg sync.WaitGroup
	createdCount := 0
	var mu sync.Mutex
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			in := patInput
			in.ResID = fmt.Sprintf("pat-cond-%d", idx)
			in.SearchIndexes = &store.SearchIndexes{
				Tokens: []store.SpidxToken{
					{TenantID: "t1", ResType: "Patient", ResID: in.ResID, SpName: "identifier", SpSystem: "http://hospital.org/mrn", SpValue: "MRN-COND"},
				},
			}
			_, created, cErr := ds.ConditionalCreateResource(ctx, in, store.SearchParams{
				TenantID: "t1",
				ResType:  "Patient",
				Tokens:   map[string]string{"identifier": "http://hospital.org/mrn|MRN-COND"},
			})
			require.NoError(t, cErr)
			if created {
				mu.Lock()
				createdCount++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	assert.Equal(t, 1, createdCount)

	// Add Observation with quantity & date for columnar $stats and compartment tests
	obsInput := store.ResourceInput{
		TenantID:      "t1",
		ResType:       "Observation",
		ResID:         "obs-1",
		ResourceProto: []byte{0x0a, 0x05, 'o', 'b', 's', '-', '1'},
		SearchIndexes: &store.SearchIndexes{
			Tokens: []store.SpidxToken{
				{TenantID: "t1", ResType: "Observation", ResID: "obs-1", SpName: "code", SpSystem: "http://loinc.org", SpValue: "8480-6"},
			},
			References: []store.SpidxReference{
				{TenantID: "t1", ResType: "Observation", ResID: "obs-1", SpName: "patient", TargetType: "Patient", TargetID: "pat-1"},
			},
			Dates: []store.SpidxDate{
				{TenantID: "t1", ResType: "Observation", ResID: "obs-1", SpName: "date", SpLow: now, SpHigh: now},
			},
			Quantities: []store.SpidxQuantity{
				{TenantID: "t1", ResType: "Observation", ResID: "obs-1", SpName: "value-quantity", SpSystem: "http://unitsofmeasure.org", SpCode: "mm[Hg]", SpValue: 120.0},
			},
		},
	}
	require.NoError(t, ds.WriteResource(ctx, obsInput))

	obsInput2 := obsInput
	obsInput2.ResID = "obs-2"
	obsInput2.SearchIndexes = &store.SearchIndexes{
		Tokens: []store.SpidxToken{
			{TenantID: "t1", ResType: "Observation", ResID: "obs-2", SpName: "code", SpSystem: "http://loinc.org", SpValue: "8480-6"},
		},
		References: []store.SpidxReference{
			{TenantID: "t1", ResType: "Observation", ResID: "obs-2", SpName: "patient", TargetType: "Patient", TargetID: "pat-1"},
		},
		Quantities: []store.SpidxQuantity{
			{TenantID: "t1", ResType: "Observation", ResID: "obs-2", SpName: "value-quantity", SpSystem: "http://unitsofmeasure.org", SpCode: "mm[Hg]", SpValue: 140.0},
		},
	}
	require.NoError(t, ds.WriteResource(ctx, obsInput2))

	// Test AnalyticalStore: EvaluateObservationStats, StreamAnalyticalExport, EvaluateCohortCounts
	stats, err := ds.EvaluateObservationStats(ctx, store.ObservationStatsParams{
		TenantID:  "t1",
		PatientID: "pat-1",
		Code:      "8480-6",
	})
	require.NoError(t, err)
	require.Len(t, stats.Series, 1)
	assert.Equal(t, 2, stats.Series[0].Count)
	assert.InDelta(t, 130.0, stats.Series[0].Average, 0.001)
	assert.InDelta(t, 120.0, stats.Series[0].Min, 0.001)
	assert.InDelta(t, 140.0, stats.Series[0].Max, 0.001)
	assert.InDelta(t, 260.0, stats.Series[0].Sum, 0.001)

	counts, err := ds.EvaluateCohortCounts(ctx, "t1", nil)
	require.NoError(t, err)
	assert.Equal(t, 2, counts["Observation"])

	exported, err := ds.ExportResources(ctx, store.ExportParams{
		TenantID:      "t1",
		ResourceTypes: []string{"Observation"},
	})
	require.NoError(t, err)
	assert.Len(t, exported, 2)

	// Test Transaction Bundle compensating rollback on Doris when entry #2 fails
	_, err = ds.ExecuteTransactionBundle(ctx, []store.BundleOperation{
		{
			OpType: store.BundleOpWrite,
			Method: "POST",
			Input: store.ResourceInput{
				TenantID:      "t1",
				ResType:       "Patient",
				ResID:         "pat-rolled-back",
				ResourceProto: []byte{0x01},
				CreateOnly:    true,
			},
		},
		{
			OpType: store.BundleOpWrite,
			Method: "PUT",
			Input: store.ResourceInput{
				TenantID:        "t1",
				ResType:         "Patient",
				ResID:           "pat-1",
				ResourceProto:   []byte{0x02},
				ExpectedVersion: 999, // Fails with ErrVersionConflict!
			},
		},
	})
	require.ErrorIs(t, err, store.ErrVersionConflict)

	// Verify pat-rolled-back was cleanly rolled back!
	_, err = ds.ReadResourceWithMeta(ctx, "t1", "Patient", "pat-rolled-back")
	assert.ErrorIs(t, err, sql.ErrNoRows)

	// Verify Doris SQL log captured INSERT & SELECT statements
	assert.NotEmpty(t, ds.ExecutedStatements())
}

func TestDorisStore_Compartments_Audit_OAuth_And_Maintenance(t *testing.T) {
	ds := doris.New(nil)
	ctx := context.Background()

	// Seed Patient, Encounter, Observation
	_, err := ds.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t1",
		ResType:       "Patient",
		ResID:         "p1",
		ResourceProto: []byte{0x01},
		SearchIndexes: &store.SearchIndexes{
			Tokens: []store.SpidxToken{{TenantID: "t1", ResType: "Patient", ResID: "p1", SpName: "_tag", SpSystem: "sys", SpValue: "t1"}},
			URIs:   []store.SpidxURI{{TenantID: "t1", ResType: "Patient", ResID: "p1", SpName: "_profile", SpValue: "http://example.org/Profile"}},
		},
	})
	require.NoError(t, err)

	_, err = ds.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t1",
		ResType:       "Encounter",
		ResID:         "e1",
		ResourceProto: []byte{0x02},
		SearchIndexes: &store.SearchIndexes{
			References: []store.SpidxReference{{TenantID: "t1", ResType: "Encounter", ResID: "e1", SpName: "patient", TargetType: "Patient", TargetID: "p1"}},
		},
	})
	require.NoError(t, err)

	_, err = ds.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t1",
		ResType:       "Observation",
		ResID:         "o1",
		ResourceProto: []byte{0x03},
		SearchIndexes: &store.SearchIndexes{
			References: []store.SpidxReference{
				{TenantID: "t1", ResType: "Observation", ResID: "o1", SpName: "patient", TargetType: "Patient", TargetID: "p1"},
				{TenantID: "t1", ResType: "Observation", ResID: "o1", SpName: "encounter", TargetType: "Encounter", TargetID: "e1"},
			},
		},
	})
	require.NoError(t, err)

	// PatientEverything, EncounterEverything, GroupEverything, BelongsToPatientCompartment
	patEv, err := ds.PatientEverything(ctx, store.PatientEverythingParams{TenantID: "t1", PatientID: "p1"})
	require.NoError(t, err)
	assert.Equal(t, 3, patEv.Total)

	encEv, err := ds.EncounterEverything(ctx, store.EncounterEverythingParams{TenantID: "t1", EncounterID: "e1"})
	require.NoError(t, err)
	assert.Equal(t, 2, encEv.Total)

	grpEv, err := ds.GroupEverything(ctx, store.GroupEverythingParams{TenantID: "t1", GroupID: "all", AllPatients: true})
	require.NoError(t, err)
	assert.Equal(t, 3, grpEv.Total)

	belongs, err := ds.BelongsToPatientCompartment(ctx, "t1", "Observation", "o1", "p1")
	require.NoError(t, err)
	assert.True(t, belongs)

	candidates, err := ds.SearchPatientMatchCandidates(ctx, store.PatientMatchCandidateParams{TenantID: "t1"})
	require.NoError(t, err)
	assert.Len(t, candidates, 1)

	graphRes, err := ds.ResourceGraph(ctx, store.ResourceGraphParams{TenantID: "t1", ResourceType: "Patient", ResourceID: "p1"})
	require.NoError(t, err)
	assert.Equal(t, 1, graphRes.Total)

	metaRes, err := ds.AggregateMeta(ctx, "t1", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"http://example.org/Profile"}, metaRes.Profiles)
	assert.Len(t, metaRes.Tags, 1)

	// Reindex
	reidx, err := ds.Reindex(ctx, store.ReindexParams{
		TenantID: "t1",
		Extractors: map[string]store.IndexExtractorFunc{
			"Patient": func(tenantID, resID string, _ []byte) (*store.SearchIndexes, error) {
				return &store.SearchIndexes{
					Strings: []store.SpidxString{{TenantID: tenantID, ResType: "Patient", ResID: resID, SpName: "family", SpValue: "Reindexed"}},
				}, nil
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, reidx.ReindexedResources)

	// AuditEvent
	require.NoError(t, ds.WriteAuditEvent(ctx, store.AuditRecord{
		TenantID:    "t1",
		AuditID:     "aud-1",
		Action:      "R",
		SubtypeCode: "read",
		Outcome:     "0",
		EntityType:  "Patient",
		EntityID:    "p1",
	}))
	aud, err := ds.ReadAuditEvent(ctx, "t1", "aud-1")
	require.NoError(t, err)
	assert.Equal(t, "aud-1", aud.AuditID)

	audList, audTotal, err := ds.QueryAuditEvents(ctx, store.AuditQueryParams{TenantID: "t1", Action: "R"})
	require.NoError(t, err)
	assert.Equal(t, 1, audTotal)
	assert.Len(t, audList, 1)

	// OAuth2 & ExportJob
	require.NoError(t, ds.SaveAuthCode(ctx, "code-123", store.OAuthAuthCodeRecord{
		ClientID:  "c1",
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}))
	ac, ok, err := ds.ConsumeAuthCode(ctx, "code-123")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "c1", ac.ClientID)

	require.NoError(t, ds.SaveRefreshToken(ctx, "rt-123", store.OAuthRefreshTokenRecord{
		Subject:   "practitioner-1",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}))
	rt, ok, err := ds.ConsumeRefreshToken(ctx, "rt-123")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "practitioner-1", rt.Subject)

	require.NoError(t, ds.RevokeToken(ctx, "tok-rev", time.Hour))
	revoked, err := ds.IsTokenRevoked(ctx, "tok-rev")
	require.NoError(t, err)
	assert.True(t, revoked)

	require.NoError(t, ds.SaveExportJob(ctx, store.ExportJobRecord{ID: "j1", TenantID: "t1"}))
	job, err := ds.GetExportJob(ctx, "t1", "j1")
	require.NoError(t, err)
	assert.Equal(t, "j1", job.ID)
	deletedJob, err := ds.DeleteExportJob(ctx, "t1", "j1")
	require.NoError(t, err)
	assert.True(t, deletedJob)

	// Purge & Expunge
	purged, err := ds.PurgePatientCompartment(ctx, "t1", "p1", false)
	require.NoError(t, err)
	assert.Len(t, purged, 3)

	expunged, err := ds.ExpungeResources(ctx, store.ExpungeParams{TenantID: "t1", ExpungeDeletedResources: true})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, expunged, 3)
}
