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

	unifiedDDL := doris.GenerateResourceTableDDL("")
	assert.Contains(t, unifiedDDL, "PARTITION BY LIST (res_type)")
	assert.Contains(t, unifiedDDL, `PARTITION p_observation`)
	assert.Contains(t, unifiedDDL, `PARTITION p_patient`)

	ddl := doris.GenerateResourceTableDDL("Observation")
	assert.Contains(t, ddl, "CREATE TABLE IF NOT EXISTS fhir_observation")
	assert.Contains(t, ddl, `"enable_unique_key_merge_on_write" = "true"`)
	assert.Contains(t, ddl, `"function_column.sequence_col" = "res_version"`)
	assert.Contains(t, ddl, `"store_row_column" = "true"`)
	assert.Contains(t, ddl, "USING INVERTED")
	assert.Contains(t, ddl, "resource_proto_hex STRING")

	customDDL := doris.GenerateResourceTableDDL("Binary")
	assert.Contains(t, customDDL, "CREATE TABLE IF NOT EXISTS fhir_binary")

	coreDDLs := doris.CoreResourceTableDDLs()
	assert.Len(t, coreDDLs, 13)
	for _, cddl := range coreDDLs {
		assert.NotContains(t, cddl, "resource_proto_hex")
		assert.Contains(t, cddl, `"enable_unique_key_merge_on_write" = "true"`)
	}
	_, ok := doris.CoreResourceTableDDL("Binary")
	assert.False(t, ok)
	_, _, ok = doris.ProjectCoreResourceUpsertSQL(doris.DorisResourceRow{ResType: "Binary"})
	assert.False(t, ok)
	_, _, ok = doris.ProjectCoreResourceDeleteSQL("t1", "Binary", "b1", 1, time.Now())
	assert.False(t, ok)

	stmts := doris.SchemaDDL()
	assert.Len(t, stmts, 17)

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
	assert.Equal(t, 3, encEv.Total)

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
	assert.Equal(t, 3, graphRes.Total)

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

func TestDorisStore_ConditionalOps_SearchModifiers_And_EdgeCases(t *testing.T) {
	ds := doris.New(nil)
	ctx := context.Background()

	// Empty & nil receiver checks
	var nilStore *doris.Store
	assert.Equal(t, doris.GroupCommitSync, nilStore.GroupCommit())
	assert.Equal(t, "flint", nilStore.Database())
	require.NoError(t, nilStore.Ping(ctx))

	// Missing tenant error
	_, err := ds.WriteResourceWithMeta(ctx, store.ResourceInput{ResType: "Patient", ResID: "no-tenant"})
	require.ErrorIs(t, err, store.ErrNoTenant)

	// WriteBatch: empty, cross-tenant error, and valid batch
	require.NoError(t, ds.WriteBatch(ctx, nil))
	err = ds.WriteBatch(ctx, []store.ResourceInput{
		{TenantID: "t1", ResType: "Patient", ResID: "b1"},
		{TenantID: "t2", ResType: "Patient", ResID: "b2"},
	})
	require.ErrorIs(t, err, store.ErrCrossTenantBatch)

	require.NoError(t, ds.WriteBatch(ctx, []store.ResourceInput{
		{
			TenantID:      "t1",
			ResType:       "Patient",
			ResID:         "b1",
			ResourceProto: []byte{0x01},
			SearchIndexes: &store.SearchIndexes{
				Strings:    []store.SpidxString{{TenantID: "t1", ResType: "Patient", ResID: "b1", SpName: "family", SpValue: "Anderson"}},
				Tokens:     []store.SpidxToken{{TenantID: "t1", ResType: "Patient", ResID: "b1", SpName: "gender", SpValue: "female"}, {TenantID: "t1", ResType: "Patient", ResID: "b1", SpName: "_security", SpSystem: "sec", SpValue: "R"}},
				References: []store.SpidxReference{{TenantID: "t1", ResType: "Patient", ResID: "b1", SpName: "organization", TargetType: "Organization", TargetID: "org-1"}},
				URIs:       []store.SpidxURI{{TenantID: "t1", ResType: "Patient", ResID: "b1", SpName: "_profile", SpValue: "http://example.org/PatientProfile"}},
			},
		},
		{
			TenantID:      "t1",
			ResType:       "Patient",
			ResID:         "b2",
			ResourceProto: []byte{0x02},
			SearchIndexes: &store.SearchIndexes{
				Strings:    []store.SpidxString{{TenantID: "t1", ResType: "Patient", ResID: "b2", SpName: "family", SpValue: "Andrews"}},
				Tokens:     []store.SpidxToken{{TenantID: "t1", ResType: "Patient", ResID: "b2", SpName: "gender", SpValue: "female"}},
				References: []store.SpidxReference{{TenantID: "t1", ResType: "Patient", ResID: "b2", SpName: "organization", TargetType: "Organization", TargetID: "org-1"}},
			},
		},
	}))

	// BuildInvertedSearchSQL with multiple IDs, ResTypes, Strings, References, URIs
	sqlStr, sqlArgs := doris.BuildInvertedSearchSQL(store.SearchParams{
		TenantID:   "t1",
		ResTypes:   []string{"Patient", "Observation"},
		ID:         "b1, b2",
		Tokens:     map[string]string{"gender": "female"},
		References: map[string]string{"organization": "Organization/org-1"},
		Strings:    map[string]string{"family": "And"},
		URIs:       map[string]string{"_profile": "http://example.org/PatientProfile"},
		Count:      5000,
		Offset:     -5,
	})
	assert.Contains(t, sqlStr, "res_type IN (?, ?)")
	assert.Contains(t, sqlStr, "res_id IN (?, ?)")
	assert.NotEmpty(t, sqlArgs)

	// Search modifiers: ResTypes, comma-separated IDs, :exact, :contains, :not token, ReferenceFilters, StringFilters, TokenFilters, URIs
	sr, err := ds.Search(ctx, store.SearchParams{
		TenantID:         "t1",
		ResTypes:         []string{"Patient"},
		ID:               "Patient/b1,b2",
		Strings:          map[string]string{"family:exact": "anderson", "family:contains": "nder"},
		StringFilters:    map[string][]string{"family": {"and"}},
		Tokens:           map[string]string{"gender": "female", "gender:not": "male"},
		TokenFilters:     map[string][]string{"gender": {"female"}},
		References:       map[string]string{"organization": "Organization/org-1"},
		ReferenceFilters: map[string][]string{"organization": {"org-1"}},
		URIs:             map[string]string{"_profile": "http://example.org/PatientProfile"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, sr.Total)
	assert.Equal(t, "b1", sr.Matches[0].ResID)

	// ConditionalCreate when >1 match -> ErrPreconditionFailed
	_, _, err = ds.ConditionalCreateResource(ctx, store.ResourceInput{
		TenantID: "t1", ResType: "Patient", ResID: "b3",
	}, store.SearchParams{
		TenantID: "t1", ResType: "Patient", Tokens: map[string]string{"gender": "female"},
	})
	require.ErrorIs(t, err, store.ErrPreconditionFailed)

	// ConditionalUpdate: >1 match -> ErrPreconditionFailed; 1 match -> updates; 0 match -> creates
	_, err = ds.ConditionalUpdateResource(ctx, "t1", "Patient", store.SearchParams{
		TenantID: "t1", ResType: "Patient", Tokens: map[string]string{"gender": "female"},
	}, func(matchedID string, isExisting bool) (store.ResourceInput, error) {
		return store.ResourceInput{TenantID: "t1", ResType: "Patient", ResID: matchedID}, nil
	})
	require.ErrorIs(t, err, store.ErrPreconditionFailed)

	updRec, err := ds.ConditionalUpdateResource(ctx, "t1", "Patient", store.SearchParams{
		TenantID: "t1", ResType: "Patient", ID: "b1",
	}, func(matchedID string, isExisting bool) (store.ResourceInput, error) {
		assert.True(t, isExisting)
		assert.Equal(t, "b1", matchedID)
		return store.ResourceInput{
			TenantID:      "t1",
			ResType:       "Patient",
			ResID:         matchedID,
			ResourceProto: []byte{0x09},
			SearchIndexes: &store.SearchIndexes{
				Tokens: []store.SpidxToken{{TenantID: "t1", ResType: "Patient", ResID: "b1", SpName: "gender", SpValue: "female"}},
			},
		}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, updRec.ResVersion)

	newRec, err := ds.ConditionalUpdateResource(ctx, "t1", "Patient", store.SearchParams{
		TenantID: "t1", ResType: "Patient", ID: "b-new",
	}, func(_ string, isExisting bool) (store.ResourceInput, error) {
		assert.False(t, isExisting)
		return store.ResourceInput{TenantID: "t1", ResType: "Patient", ResID: "b-new", ResourceProto: []byte{0x0a}}, nil
	})
	require.NoError(t, err)
	assert.True(t, newRec.Created)

	// ConditionalDeleteResource & ConditionalDeleteResources
	_, err = ds.ConditionalDeleteResource(ctx, "t1", "Patient", store.SearchParams{
		TenantID: "t1", ResType: "Patient", Tokens: map[string]string{"gender": "female"},
	}, nil)
	require.ErrorIs(t, err, store.ErrPreconditionFailed)

	noMatchDel, err := ds.ConditionalDeleteResource(ctx, "t1", "Patient", store.SearchParams{
		TenantID: "t1", ResType: "Patient", ID: "nonexistent",
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, noMatchDel)

	_, err = ds.ConditionalDeleteResource(ctx, "t1", "Patient", store.SearchParams{
		TenantID: "t1", ResType: "Patient", ID: "nonexistent", ExpectedVersion: 1,
	}, nil)
	require.ErrorIs(t, err, store.ErrVersionConflict)

	delRec, err := ds.ConditionalDeleteResource(ctx, "t1", "Patient", store.SearchParams{
		TenantID: "t1", ResType: "Patient", ID: "b-new",
	}, func(matchedID string) error {
		assert.Equal(t, "b-new", matchedID)
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, delRec)
	assert.True(t, delRec.IsDeleted)

	// Reading deleted resource & deleted version returns ErrResourceDeleted
	_, err = ds.ReadResource(ctx, "t1", "Patient", "b-new")
	require.ErrorIs(t, err, store.ErrResourceDeleted)
	_, err = ds.ReadResourceVersion(ctx, "t1", "Patient", "b-new", delRec.ResVersion)
	require.ErrorIs(t, err, store.ErrResourceDeleted)

	// Deleting already-deleted resource or non-existent resource via DeleteResource
	require.NoError(t, ds.DeleteResource(ctx, "t1", "Patient", "b-new"))
	require.NoError(t, ds.DeleteResource(ctx, "t1", "Patient", "never-existed"))

	// ExecuteTransactionBundle: empty, cross-tenant, conditional POST, conditional PUT, conditional DELETE
	emptyRes, err := ds.ExecuteTransactionBundle(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, emptyRes)

	_, err = ds.ExecuteTransactionBundle(ctx, []store.BundleOperation{
		{Input: store.ResourceInput{TenantID: "t1"}},
		{Input: store.ResourceInput{TenantID: "t2"}},
	})
	require.ErrorIs(t, err, store.ErrCrossTenantBatch)

	txRes, err := ds.ExecuteTransactionBundle(ctx, []store.BundleOperation{
		{
			OpType:      store.BundleOpWrite,
			Method:      "POST",
			Input:       store.ResourceInput{TenantID: "t1", ResType: "Patient", ResID: "tx-post", ResourceProto: []byte{0x01}},
			IfNoneExist: &store.SearchParams{TenantID: "t1", ResType: "Patient", ID: "b1"},
		},
		{
			OpType:            store.BundleOpWrite,
			Method:            "PUT",
			Input:             store.ResourceInput{TenantID: "t1", ResType: "Patient"},
			ConditionalParams: &store.SearchParams{TenantID: "t1", ResType: "Patient", ID: "b2"},
			BuildWriteInput: func(matchedID string, _ bool) (store.ResourceInput, error) {
				return store.ResourceInput{TenantID: "t1", ResType: "Patient", ResID: matchedID, ResourceProto: []byte{0x03}}, nil
			},
		},
		{
			OpType:            store.BundleOpDelete,
			Method:            "DELETE",
			Input:             store.ResourceInput{TenantID: "t1", ResType: "Patient"},
			ConditionalParams: &store.SearchParams{TenantID: "t1", ResType: "Patient", ID: "b2"},
			VerifyDeleteMatch: func(_ string) error { return nil },
		},
	})
	require.NoError(t, err)
	require.Len(t, txRes, 3)
	assert.Equal(t, "200 OK", txRes[0].Status)
	assert.Equal(t, "200 OK", txRes[1].Status)
	assert.Equal(t, "204 No Content", txRes[2].Status)

	// ExpungePreviousVersions & hard PurgePatientCompartment
	expPrev, err := ds.ExpungeResources(ctx, store.ExpungeParams{
		TenantID:                "t1",
		ResType:                 "Patient",
		ResID:                   "b1",
		ExpungePreviousVersions: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, expPrev)

	hardPurged, err := ds.PurgePatientCompartment(ctx, "t1", "b1", true)
	require.NoError(t, err)
	assert.Len(t, hardPurged, 1)

	// Advanced Search: Includes, RevIncludes, :of-type, :identifier, :text, URIFilters (:below/:above)
	require.NoError(t, ds.WriteResource(ctx, store.ResourceInput{
		TenantID:      "t2",
		ResType:       "Patient",
		ResID:         "pat-adv",
		ResourceProto: []byte{0x01},
		SearchIndexes: &store.SearchIndexes{
			Tokens: []store.SpidxToken{
				{TenantID: "t2", ResType: "Patient", ResID: "pat-adv", SpName: "identifier", SpSystem: "http://hospital.example.org/mrn", SpValue: "MRN-99"},
				{TenantID: "t2", ResType: "Patient", ResID: "pat-adv", SpName: "identifier:of-type", SpSystem: "http://terminology.hl7.org/CodeSystem/v2-0203|MR", SpValue: "MRN-99"},
			},
			URIs: []store.SpidxURI{
				{TenantID: "t2", ResType: "Patient", ResID: "pat-adv", SpName: "_profile", SpValue: "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
			},
		},
	}))
	require.NoError(t, ds.WriteResource(ctx, store.ResourceInput{
		TenantID:      "t2",
		ResType:       "Observation",
		ResID:         "obs-adv",
		ResourceProto: []byte{0x02},
		SearchIndexes: &store.SearchIndexes{
			Strings: []store.SpidxString{
				{TenantID: "t2", ResType: "Observation", ResID: "obs-adv", SpName: "code:text", SpValue: "essential hypertension"},
			},
			References: []store.SpidxReference{
				{TenantID: "t2", ResType: "Observation", ResID: "obs-adv", SpName: "patient", TargetType: "Patient", TargetID: "pat-adv"},
			},
		},
	}))

	advPat, err := ds.Search(ctx, store.SearchParams{
		TenantID:    "t2",
		ResType:     "Patient",
		Tokens:      map[string]string{"identifier:of-type": "http://terminology.hl7.org/CodeSystem/v2-0203|MR|MRN-99"},
		URIFilters:  map[string][]string{"_profile:below": {"http://hl7.org/fhir/us/core/StructureDefinition/"}, "_profile:above": {"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient/v1"}},
		RevIncludes: []store.IncludeParam{{SourceType: "Observation", ParamName: "patient"}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, advPat.Total)
	assert.Len(t, advPat.Includes, 1)

	advObs, err := ds.Search(ctx, store.SearchParams{
		TenantID:   "t2",
		ResType:    "Observation",
		Strings:    map[string]string{"code:text": "hypertension"},
		References: map[string]string{"patient:identifier": "http://hospital.example.org/mrn|MRN-99"},
		Includes:   []store.IncludeParam{{SourceType: "Observation", ParamName: "patient"}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, advObs.Total)
	assert.Len(t, advObs.Includes, 1)
}
