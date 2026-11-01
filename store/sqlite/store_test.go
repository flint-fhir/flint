package sqlite_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/sqlite"
)

type stubSQLiteDriver struct {
	execErr error
}

type stubSQLiteConn struct {
	execErr error
}

func (d *stubSQLiteDriver) Open(_ string) (driver.Conn, error) {
	return &stubSQLiteConn{execErr: d.execErr}, nil
}

func (c *stubSQLiteConn) Prepare(_ string) (driver.Stmt, error) { return nil, nil }
func (c *stubSQLiteConn) Close() error                          { return nil }
func (c *stubSQLiteConn) Begin() (driver.Tx, error)             { return nil, nil }
func (c *stubSQLiteConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if c.execErr != nil {
		return nil, c.execErr
	}
	return driver.RowsAffected(1), nil
}

var registerSQLiteDriversOnce sync.Once

func registerSQLiteDrivers() {
	registerSQLiteDriversOnce.Do(func() {
		sql.Register("sqlite_stub_ok", &stubSQLiteDriver{})
		sql.Register("sqlite_stub_err", &stubSQLiteDriver{execErr: errors.New("sqlite write error")})
	})
}

func TestSQLiteDDL_Pragmas_And_DuckDBAttach(t *testing.T) {
	registerSQLiteDrivers()

	mode, err := sqlite.ParseJournalMode("")
	require.NoError(t, err)
	assert.Equal(t, sqlite.JournalWAL, mode)

	mode, err = sqlite.ParseJournalMode("memory")
	require.NoError(t, err)
	assert.Equal(t, sqlite.JournalMemory, mode)

	mode, err = sqlite.ParseJournalMode("delete")
	require.NoError(t, err)
	assert.Equal(t, sqlite.JournalDelete, mode)

	_, err = sqlite.ParseJournalMode("invalid")
	require.ErrorContains(t, err, "invalid sqlite journal mode")

	pragmas := sqlite.PragmaStatements(sqlite.JournalWAL)
	assert.Len(t, pragmas, 5)
	assert.Contains(t, pragmas[0], "PRAGMA journal_mode = WAL;")

	ddls := sqlite.SchemaDDL()
	assert.NotEmpty(t, ddls)
	assert.Contains(t, ddls[0], "WITHOUT ROWID")

	duckStmts := sqlite.DuckDBAttachSQL("data/flint.db")
	assert.Len(t, duckStmts, 5)
	assert.Contains(t, duckStmts[2], "ATTACH 'data/flint.db' AS flint_sqlite (TYPE SQLITE, READ_ONLY TRUE);")
	assert.Contains(t, duckStmts[4], "MEDIAN(q.sp_value)")

	defaultDuckStmts := sqlite.DuckDBAttachSQL("")
	assert.Contains(t, defaultDuckStmts[2], "ATTACH 'flint.db'")

	require.NoError(t, sqlite.RunMigrations(context.Background(), nil, ""))

	dbOK, err := sql.Open("sqlite_stub_ok", "")
	require.NoError(t, err)
	defer dbOK.Close()
	require.NoError(t, sqlite.RunMigrations(context.Background(), dbOK, sqlite.JournalWAL))

	dbErr, err := sql.Open("sqlite_stub_err", "")
	require.NoError(t, err)
	defer dbErr.Close()
	require.ErrorContains(t, sqlite.RunMigrations(context.Background(), dbErr, sqlite.JournalWAL), "sqlite pragma[0]")
}

func TestSQLiteStore_CRUD_OptimisticLocking_And_Transactions(t *testing.T) {
	registerSQLiteDrivers()
	dbOK, err := sql.Open("sqlite_stub_ok", "")
	require.NoError(t, err)
	defer dbOK.Close()

	ss := sqlite.New(dbOK, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(":memory:"))
	assert.Equal(t, "sqlite", ss.BackendName())
	assert.Equal(t, sqlite.JournalWAL, ss.JournalMode())
	assert.Equal(t, ":memory:", ss.DSN())
	require.NoError(t, ss.Ping(context.Background()))

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

	rec, err := ss.WriteResourceWithMeta(ctx, patInput)
	require.NoError(t, err)
	assert.Equal(t, 1, rec.ResVersion)
	assert.True(t, rec.Created)

	// Duplicate CreateOnly fails with ErrResourceAlreadyExists
	_, err = ss.WriteResourceWithMeta(ctx, patInput)
	require.ErrorIs(t, err, store.ErrResourceAlreadyExists)

	// Optimistic concurrency (If-Match) mismatch fails with ErrVersionConflict
	updInput := patInput
	updInput.CreateOnly = false
	updInput.ExpectedVersion = 99
	_, err = ss.WriteResourceWithMeta(ctx, updInput)
	require.ErrorIs(t, err, store.ErrVersionConflict)

	// Matching ExpectedVersion = 1 succeeds and increments to version 2
	updInput.ExpectedVersion = 1
	rec2, err := ss.WriteResourceWithMeta(ctx, updInput)
	require.NoError(t, err)
	assert.Equal(t, 2, rec2.ResVersion)

	protoBytes, err := ss.ReadResource(ctx, "t1", "Patient", "pat-1")
	require.NoError(t, err)
	assert.Equal(t, patInput.ResourceProto, protoBytes)

	v1, err := ss.ReadResourceVersion(ctx, "t1", "Patient", "pat-1", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, v1.ResVersion)

	hist, totalHist, err := ss.ListResourceHistory(ctx, "t1", "Patient", "pat-1", 10, 0)
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
			_, created, cErr := ss.ConditionalCreateResource(ctx, in, store.SearchParams{
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

	// Add Observations for AnalyticalStore ($stats, $export, cohort counts)
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
	require.NoError(t, ss.WriteResource(ctx, obsInput))

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
	require.NoError(t, ss.WriteResource(ctx, obsInput2))

	stats, err := ss.EvaluateObservationStats(ctx, store.ObservationStatsParams{
		TenantID:  "t1",
		PatientID: "pat-1",
		Code:      "8480-6",
	})
	require.NoError(t, err)
	require.Len(t, stats.Series, 1)
	assert.Equal(t, 2, stats.Series[0].Count)
	assert.InDelta(t, 130.0, stats.Series[0].Average, 0.001)

	counts, err := ss.EvaluateCohortCounts(ctx, "t1", nil)
	require.NoError(t, err)
	assert.Equal(t, 2, counts["Observation"])

	exported, err := ss.ExportResources(ctx, store.ExportParams{
		TenantID:      "t1",
		ResourceTypes: []string{"Observation"},
	})
	require.NoError(t, err)
	assert.Len(t, exported, 2)

	// Transaction Bundle rollback on failure
	_, err = ss.ExecuteTransactionBundle(ctx, []store.BundleOperation{
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
				ExpectedVersion: 999,
			},
		},
	})
	require.ErrorIs(t, err, store.ErrVersionConflict)
	_, err = ss.ReadResourceWithMeta(ctx, "t1", "Patient", "pat-rolled-back")
	assert.ErrorIs(t, err, sql.ErrNoRows)

	assert.NotEmpty(t, ss.ExecutedStatements())
}

func TestSQLiteStore_Compartments_Audit_OAuth_Search_And_Maintenance(t *testing.T) {
	ss := sqlite.New(nil)
	ctx := context.Background()

	var nilStore *sqlite.Store
	assert.Equal(t, sqlite.JournalWAL, nilStore.JournalMode())
	assert.Equal(t, "flint.db", nilStore.DSN())
	require.NoError(t, nilStore.Ping(ctx))

	_, err := ss.WriteResourceWithMeta(ctx, store.ResourceInput{ResType: "Patient", ResID: "no-tenant"})
	require.ErrorIs(t, err, store.ErrNoTenant)

	require.NoError(t, ss.WriteBatch(ctx, nil))
	err = ss.WriteBatch(ctx, []store.ResourceInput{
		{TenantID: "t1", ResType: "Patient", ResID: "b1"},
		{TenantID: "t2", ResType: "Patient", ResID: "b2"},
	})
	require.ErrorIs(t, err, store.ErrCrossTenantBatch)

	require.NoError(t, ss.WriteBatch(ctx, []store.ResourceInput{
		{
			TenantID:      "t1",
			ResType:       "Patient",
			ResID:         "p1",
			ResourceProto: []byte{0x01},
			SearchIndexes: &store.SearchIndexes{
				Strings:    []store.SpidxString{{TenantID: "t1", ResType: "Patient", ResID: "p1", SpName: "family", SpValue: "Anderson"}},
				Tokens:     []store.SpidxToken{{TenantID: "t1", ResType: "Patient", ResID: "p1", SpName: "gender", SpValue: "female"}, {TenantID: "t1", ResType: "Patient", ResID: "p1", SpName: "_tag", SpSystem: "sys", SpValue: "t1"}, {TenantID: "t1", ResType: "Patient", ResID: "p1", SpName: "_security", SpSystem: "sec", SpValue: "R"}},
				References: []store.SpidxReference{{TenantID: "t1", ResType: "Patient", ResID: "p1", SpName: "organization", TargetType: "Organization", TargetID: "org-1"}},
				URIs:       []store.SpidxURI{{TenantID: "t1", ResType: "Patient", ResID: "p1", SpName: "_profile", SpValue: "http://example.org/Profile"}},
			},
		},
		{
			TenantID:      "t1",
			ResType:       "Encounter",
			ResID:         "e1",
			ResourceProto: []byte{0x02},
			SearchIndexes: &store.SearchIndexes{
				References: []store.SpidxReference{{TenantID: "t1", ResType: "Encounter", ResID: "e1", SpName: "patient", TargetType: "Patient", TargetID: "p1"}},
			},
		},
		{
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
		},
	}))

	// Search modifiers
	sr, err := ss.Search(ctx, store.SearchParams{
		TenantID:         "t1",
		ResTypes:         []string{"Patient"},
		ID:               "Patient/p1,p2",
		Strings:          map[string]string{"family:exact": "anderson", "family:contains": "nder"},
		StringFilters:    map[string][]string{"family": {"and"}},
		Tokens:           map[string]string{"gender": "female", "gender:not": "male"},
		TokenFilters:     map[string][]string{"gender": {"female"}},
		References:       map[string]string{"organization": "Organization/org-1"},
		ReferenceFilters: map[string][]string{"organization": {"org-1"}},
		URIs:             map[string]string{"_profile": "http://example.org/Profile"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, sr.Total)

	// Compartments & Meta
	patEv, err := ss.PatientEverything(ctx, store.PatientEverythingParams{TenantID: "t1", PatientID: "p1"})
	require.NoError(t, err)
	assert.Equal(t, 3, patEv.Total)

	encEv, err := ss.EncounterEverything(ctx, store.EncounterEverythingParams{TenantID: "t1", EncounterID: "e1"})
	require.NoError(t, err)
	assert.Equal(t, 3, encEv.Total)

	grpEv, err := ss.GroupEverything(ctx, store.GroupEverythingParams{TenantID: "t1", GroupID: "all", AllPatients: true})
	require.NoError(t, err)
	assert.Equal(t, 3, grpEv.Total)

	belongs, err := ss.BelongsToPatientCompartment(ctx, "t1", "Observation", "o1", "p1")
	require.NoError(t, err)
	assert.True(t, belongs)

	candidates, err := ss.SearchPatientMatchCandidates(ctx, store.PatientMatchCandidateParams{TenantID: "t1"})
	require.NoError(t, err)
	assert.Len(t, candidates, 1)

	graphRes, err := ss.ResourceGraph(ctx, store.ResourceGraphParams{TenantID: "t1", ResourceType: "Patient", ResourceID: "p1"})
	require.NoError(t, err)
	assert.Equal(t, 3, graphRes.Total)

	metaRes, err := ss.AggregateMeta(ctx, "t1", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"http://example.org/Profile"}, metaRes.Profiles)

	// ConditionalUpdate & ConditionalDelete & Transaction Bundle conditional entries
	updRec, err := ss.ConditionalUpdateResource(ctx, "t1", "Patient", store.SearchParams{
		TenantID: "t1", ResType: "Patient", ID: "p1",
	}, func(matchedID string, isExisting bool) (store.ResourceInput, error) {
		assert.True(t, isExisting)
		return store.ResourceInput{TenantID: "t1", ResType: "Patient", ResID: matchedID, ResourceProto: []byte{0x09}}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, updRec.ResVersion)

	newRec, err := ss.ConditionalUpdateResource(ctx, "t1", "Patient", store.SearchParams{
		TenantID: "t1", ResType: "Patient", ID: "p-temp",
	}, func(_ string, isExisting bool) (store.ResourceInput, error) {
		assert.False(t, isExisting)
		return store.ResourceInput{TenantID: "t1", ResType: "Patient", ResID: "p-temp", ResourceProto: []byte{0x0a}}, nil
	})
	require.NoError(t, err)
	assert.True(t, newRec.Created)

	delRec, err := ss.ConditionalDeleteResource(ctx, "t1", "Patient", store.SearchParams{
		TenantID: "t1", ResType: "Patient", ID: "p-temp",
	}, func(matchedID string) error {
		assert.Equal(t, "p-temp", matchedID)
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, delRec)
	assert.True(t, delRec.IsDeleted)

	txRes, err := ss.ExecuteTransactionBundle(ctx, []store.BundleOperation{
		{
			OpType:      store.BundleOpWrite,
			Method:      "POST",
			Input:       store.ResourceInput{TenantID: "t1", ResType: "Patient", ResID: "tx-post", ResourceProto: []byte{0x01}},
			IfNoneExist: &store.SearchParams{TenantID: "t1", ResType: "Patient", ID: "p1"},
		},
		{
			OpType:            store.BundleOpWrite,
			Method:            "PUT",
			Input:             store.ResourceInput{TenantID: "t1", ResType: "Observation"},
			ConditionalParams: &store.SearchParams{TenantID: "t1", ResType: "Observation", ID: "o1"},
			BuildWriteInput: func(matchedID string, _ bool) (store.ResourceInput, error) {
				return store.ResourceInput{TenantID: "t1", ResType: "Observation", ResID: matchedID, ResourceProto: []byte{0x03}}, nil
			},
		},
		{
			OpType:            store.BundleOpDelete,
			Method:            "DELETE",
			Input:             store.ResourceInput{TenantID: "t1", ResType: "Observation"},
			ConditionalParams: &store.SearchParams{TenantID: "t1", ResType: "Observation", ID: "o1"},
			VerifyDeleteMatch: func(_ string) error { return nil },
		},
	})
	require.NoError(t, err)
	require.Len(t, txRes, 3)

	// Reindex
	reidx, err := ss.Reindex(ctx, store.ReindexParams{
		TenantID: "t1",
		Extractors: map[string]store.IndexExtractorFunc{
			"Patient": func(tenantID, resID string, _ []byte) (*store.SearchIndexes, error) {
				return &store.SearchIndexes{
					Strings:    []store.SpidxString{{TenantID: tenantID, ResType: "Patient", ResID: resID, SpName: "family", SpValue: "Reindexed"}},
					References: []store.SpidxReference{{TenantID: tenantID, ResType: "Patient", ResID: resID, SpName: "patient", TargetType: "Patient", TargetID: resID}},
				}, nil
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, reidx.ReindexedResources)

	// Audit, OAuth2, ExportJob
	require.NoError(t, ss.WriteAuditEvent(ctx, store.AuditRecord{
		TenantID: "t1", AuditID: "aud-1", Action: "R", SubtypeCode: "read", Outcome: "0", EntityType: "Patient", EntityID: "p1",
	}))
	aud, err := ss.ReadAuditEvent(ctx, "t1", "aud-1")
	require.NoError(t, err)
	assert.Equal(t, "aud-1", aud.AuditID)

	audList, audTotal, err := ss.QueryAuditEvents(ctx, store.AuditQueryParams{TenantID: "t1", Action: "R"})
	require.NoError(t, err)
	assert.Equal(t, 1, audTotal)
	assert.Len(t, audList, 1)

	require.NoError(t, ss.SaveAuthCode(ctx, "code-123", store.OAuthAuthCodeRecord{ClientID: "c1", ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}))
	ac, ok, err := ss.ConsumeAuthCode(ctx, "code-123")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "c1", ac.ClientID)

	require.NoError(t, ss.SaveRefreshToken(ctx, "rt-123", store.OAuthRefreshTokenRecord{Subject: "practitioner-1", ExpiresAt: time.Now().UTC().Add(time.Hour)}))
	rt, ok, err := ss.ConsumeRefreshToken(ctx, "rt-123")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "practitioner-1", rt.Subject)

	require.NoError(t, ss.RevokeToken(ctx, "tok-rev", time.Hour))
	revoked, err := ss.IsTokenRevoked(ctx, "tok-rev")
	require.NoError(t, err)
	assert.True(t, revoked)

	require.NoError(t, ss.SaveExportJob(ctx, store.ExportJobRecord{ID: "j1", TenantID: "t1"}))
	job, err := ss.GetExportJob(ctx, "t1", "j1")
	require.NoError(t, err)
	assert.Equal(t, "j1", job.ID)
	deletedJob, err := ss.DeleteExportJob(ctx, "t1", "j1")
	require.NoError(t, err)
	assert.True(t, deletedJob)

	// ExpungePreviousVersions, Purge, ExpungeDeletedResources
	expPrev, err := ss.ExpungeResources(ctx, store.ExpungeParams{TenantID: "t1", ResType: "Patient", ResID: "p1", ExpungePreviousVersions: true})
	require.NoError(t, err)
	assert.Equal(t, 1, expPrev)

	purged, err := ss.PurgePatientCompartment(ctx, "t1", "p1", false)
	require.NoError(t, err)
	assert.NotEmpty(t, purged)

	expunged, err := ss.ExpungeResources(ctx, store.ExpungeParams{TenantID: "t1", ExpungeDeletedResources: true})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, expunged, 1)

	// Advanced Search: Includes, RevIncludes, :of-type, :identifier, :text, URIFilters (:below/:above)
	require.NoError(t, ss.WriteResource(ctx, store.ResourceInput{
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
	require.NoError(t, ss.WriteResource(ctx, store.ResourceInput{
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

	advPat, err := ss.Search(ctx, store.SearchParams{
		TenantID:    "t2",
		ResType:     "Patient",
		Tokens:      map[string]string{"identifier:of-type": "http://terminology.hl7.org/CodeSystem/v2-0203|MR|MRN-99"},
		URIFilters:  map[string][]string{"_profile:below": {"http://hl7.org/fhir/us/core/StructureDefinition/"}, "_profile:above": {"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient/v1"}},
		RevIncludes: []store.IncludeParam{{SourceType: "Observation", ParamName: "patient"}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, advPat.Total)
	assert.Len(t, advPat.Includes, 1)

	advObs, err := ss.Search(ctx, store.SearchParams{
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

type stubHydrationRows struct {
	cols []string
	data [][]driver.Value
	idx  int
}

func (r *stubHydrationRows) Columns() []string { return r.cols }
func (r *stubHydrationRows) Close() error      { return nil }
func (r *stubHydrationRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

type stubHydrationDriver struct{}
type stubHydrationConn struct{}

func (d *stubHydrationDriver) Open(_ string) (driver.Conn, error) {
	return &stubHydrationConn{}, nil
}

func (c *stubHydrationConn) Prepare(_ string) (driver.Stmt, error) { return nil, nil }
func (c *stubHydrationConn) Close() error                          { return nil }
func (c *stubHydrationConn) Begin() (driver.Tx, error)             { return nil, nil }
func (c *stubHydrationConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (c *stubHydrationConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	nowStr := "2025-05-01T12:00:00Z"
	switch {
	case strings.Contains(q, "FROM fhir_resource_history"):
		return &stubHydrationRows{
			cols: []string{"tenant_id", "res_type", "res_id", "res_version", "is_deleted", "last_updated", "resource_proto"},
			data: [][]driver.Value{
				{"t1", "Patient", "pat-1", int64(1), int64(0), nowStr, []byte{0x0a, 0x05, 'p', 'a', 't', '-', '1'}},
				{"t1", "Patient", "pat-1", int64(2), int64(0), nowStr, []byte{0x0a, 0x05, 'p', 'a', 't', '-', '1'}},
			},
		}, nil
	case strings.Contains(q, "FROM fhir_resource"):
		return &stubHydrationRows{
			cols: []string{"tenant_id", "res_type", "res_id", "res_version", "is_deleted", "last_updated", "idempotency_key", "resource_proto"},
			data: [][]driver.Value{
				{"t1", "Patient", "pat-1", int64(2), int64(0), nowStr, "idem-1", []byte{0x0a, 0x05, 'p', 'a', 't', '-', '1'}},
				{"t1", "Observation", "obs-1", int64(1), int64(0), nowStr, "", []byte{0x0a, 0x05, 'o', 'b', 's', '-', '1'}},
			},
		}, nil
	case strings.Contains(q, "FROM spidx_string"):
		return &stubHydrationRows{
			cols: []string{"tenant_id", "res_type", "res_id", "sp_name", "sp_value"},
			data: [][]driver.Value{
				{"t1", "Patient", "pat-1", "family", "HydratedSmith"},
			},
		}, nil
	case strings.Contains(q, "FROM spidx_token"):
		return &stubHydrationRows{
			cols: []string{"tenant_id", "res_type", "res_id", "sp_name", "sp_system", "sp_value"},
			data: [][]driver.Value{
				{"t1", "Patient", "pat-1", "gender", "", "female"},
				{"t1", "Observation", "obs-1", "code", "http://loinc.org", "8867-4"},
			},
		}, nil
	case strings.Contains(q, "FROM spidx_date"):
		return &stubHydrationRows{
			cols: []string{"tenant_id", "res_type", "res_id", "sp_name", "sp_low", "sp_high"},
			data: [][]driver.Value{
				{"t1", "Patient", "pat-1", "birthdate", "1985-04-12T00:00:00Z", "1985-04-13T00:00:00Z"},
			},
		}, nil
	case strings.Contains(q, "FROM spidx_reference"):
		return &stubHydrationRows{
			cols: []string{"tenant_id", "res_type", "res_id", "sp_name", "target_type", "target_id"},
			data: [][]driver.Value{
				{"t1", "Observation", "obs-1", "patient", "Patient", "pat-1"},
			},
		}, nil
	case strings.Contains(q, "FROM spidx_quantity"):
		return &stubHydrationRows{
			cols: []string{"tenant_id", "res_type", "res_id", "sp_name", "sp_system", "sp_code", "sp_value"},
			data: [][]driver.Value{
				{"t1", "Observation", "obs-1", "value-quantity", "http://unitsofmeasure.org", "/min", float64(72.0)},
			},
		}, nil
	case strings.Contains(q, "FROM spidx_uri"):
		return &stubHydrationRows{
			cols: []string{"tenant_id", "res_type", "res_id", "sp_name", "sp_value"},
			data: [][]driver.Value{
				{"t1", "Patient", "pat-1", "_profile", "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
			},
		}, nil
	case strings.Contains(q, "FROM fhir_audit_event"):
		return &stubHydrationRows{
			cols: []string{
				"tenant_id", "audit_id", "recorded", "action", "subtype_code", "outcome",
				"outcome_desc", "http_method", "http_status", "request_uri",
				"agent_subject", "agent_patient", "client_ip", "entity_type", "entity_id", "entity_version",
			},
			data: [][]driver.Value{
				{"t1", "aud-hydrated", nowStr, "R", "read", "0", "", "GET", int64(200), "/fhir/r4/t1/Patient/pat-1", "doc-1", "pat-1", "127.0.0.1", "Patient", "pat-1", "2"},
			},
		}, nil
	case strings.Contains(q, "FROM fhir_export_job"):
		return &stubHydrationRows{
			cols: []string{"tenant_id", "job_id", "patient_id", "request_url", "created_at"},
			data: [][]driver.Value{
				{"t1", "job-hydrated", "pat-1", "http://localhost/fhir/r4/t1/$export", nowStr},
			},
		}, nil
	default:
		return &stubHydrationRows{}, nil
	}
}

var registerHydrationDriverOnce sync.Once

func TestSQLiteStore_LoadFromDB_RoundTripHydration(t *testing.T) {
	registerHydrationDriverOnce.Do(func() {
		sql.Register("sqlite_stub_hydrate", &stubHydrationDriver{})
	})
	db, err := sql.Open("sqlite_stub_hydrate", "")
	require.NoError(t, err)
	defer db.Close()

	ss := sqlite.New(db)
	ctx := context.Background()
	require.NoError(t, ss.LoadFromDB(ctx))

	// Verify hydrated active resource & vread history
	rec, err := ss.ReadResourceWithMeta(ctx, "t1", "Patient", "pat-1")
	require.NoError(t, err)
	assert.Equal(t, 2, rec.ResVersion)

	v1, err := ss.ReadResourceVersion(ctx, "t1", "Patient", "pat-1", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, v1.ResVersion)

	// Verify hydrated spidx_* indexes & compartment linkage
	belongs, err := ss.BelongsToPatientCompartment(ctx, "t1", "Observation", "obs-1", "pat-1")
	require.NoError(t, err)
	assert.True(t, belongs)

	// Verify hydrated AuditRecord & ExportJobRecord
	aud, err := ss.ReadAuditEvent(ctx, "t1", "aud-hydrated")
	require.NoError(t, err)
	assert.Equal(t, "doc-1", aud.AgentSubject)

	job, err := ss.GetExportJob(ctx, "t1", "job-hydrated")
	require.NoError(t, err)
	assert.Equal(t, "pat-1", job.PatientID)
}
