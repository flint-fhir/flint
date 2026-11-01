package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"

	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/sqlite"
)

func buildHybridPatientProto(t *testing.T, id, family string) []byte {
	t.Helper()
	p := &patpb.Patient{
		Id: &dtpb.Id{Value: id},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: family},
				Given:  []*dtpb.String{{Value: "HybridGiven"}},
			},
		},
	}
	b, err := proto.Marshal(p)
	require.NoError(t, err)
	return b
}

func TestHybridStore_ReplicationAndAnalyticalPushdown(t *testing.T) {
	ctx := context.Background()
	primary := sqlite.New(nil, sqlite.WithDSN(":memory:"))
	replica := doris.New(nil)

	hyb := store.NewHybridStore(primary, replica)
	assert.Equal(t, primary, hyb.Primary())
	assert.Equal(t, replica, hyb.Replica())
	assert.Equal(t, "doris", hyb.BackendName())
	require.NoError(t, hyb.Ping(ctx))

	// 1. WriteResource replicates automatically to Doris with auto-extracted SearchIndexes
	patBytes := buildHybridPatientProto(t, "hyb-p1", "HybridSmith")
	require.NoError(t, hyb.WriteResource(ctx, store.ResourceInput{
		TenantID:      "t-hyb",
		ResType:       "Patient",
		ResID:         "hyb-p1",
		ResourceProto: patBytes,
	}))

	// Verify primary AND replica both have the resource and search index
	gotPrimary, err := hyb.ReadResource(ctx, "t-hyb", "Patient", "hyb-p1")
	require.NoError(t, err)
	assert.Equal(t, patBytes, gotPrimary)

	gotReplica, err := replica.ReadResourceWithMeta(ctx, "t-hyb", "Patient", "hyb-p1")
	require.NoError(t, err)
	assert.Equal(t, 1, gotReplica.ResVersion)

	srReplica, err := replica.Search(ctx, store.SearchParams{
		TenantID: "t-hyb",
		ResType:  "Patient",
		Strings:  map[string]string{"family": "HybridSmith"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, srReplica.Total)

	// 2. WriteResourceWithMeta update replicates version 2 to Doris
	recV2, err := hyb.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:        "t-hyb",
		ResType:         "Patient",
		ResID:           "hyb-p1",
		ResourceProto:   patBytes,
		ExpectedVersion: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, recV2.ResVersion)

	gotReplicaV2, err := replica.ReadResourceWithMeta(ctx, "t-hyb", "Patient", "hyb-p1")
	require.NoError(t, err)
	assert.Equal(t, 2, gotReplicaV2.ResVersion)

	// ReadResourceVersion & History from primary
	ver1, err := hyb.ReadResourceVersion(ctx, "t-hyb", "Patient", "hyb-p1", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, ver1.ResVersion)
	hist, totalHist, err := hyb.ListResourceHistory(ctx, "t-hyb", "Patient", "hyb-p1", 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, totalHist)
	assert.Len(t, hist, 2)
	_, _, err = hyb.ListHistory(ctx, "t-hyb", "Patient", "hyb-p1", nil, 10, 0)
	require.NoError(t, err)
	_, _, err = hyb.ListHistoryWithParams(ctx, store.HistoryParams{TenantID: "t-hyb", ResType: "Patient", ResID: "hyb-p1"})
	require.NoError(t, err)

	// 3. WriteBatch replicates batch
	pat2 := buildHybridPatientProto(t, "hyb-p2", "BatchFamily")
	require.NoError(t, hyb.WriteBatch(ctx, []store.ResourceInput{
		{
			TenantID:      "t-hyb",
			ResType:       "Patient",
			ResID:         "hyb-p2",
			ResourceProto: pat2,
		},
	}))
	_, err = replica.ReadResourceWithMeta(ctx, "t-hyb", "Patient", "hyb-p2")
	require.NoError(t, err)

	// 4. ConditionalCreateResource, ConditionalUpdateResource, ConditionalDeleteResource, ConditionalDeleteResources
	pat3 := buildHybridPatientProto(t, "hyb-p3", "CondCreate")
	condRec, created, err := hyb.ConditionalCreateResource(ctx, store.ResourceInput{
		TenantID:      "t-hyb",
		ResType:       "Patient",
		ResID:         "hyb-p3",
		ResourceProto: pat3,
	}, store.SearchParams{
		TenantID: "t-hyb",
		ResType:  "Patient",
		Strings:  map[string]string{"family": "CondCreate"},
	})
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "hyb-p3", condRec.ResID)
	_, err = replica.ReadResourceWithMeta(ctx, "t-hyb", "Patient", "hyb-p3")
	require.NoError(t, err)

	updRec, err := hyb.ConditionalUpdateResource(ctx, "t-hyb", "Patient", store.SearchParams{
		TenantID: "t-hyb",
		ResType:  "Patient",
		ID:       "hyb-p3",
	}, func(matchedID string, isExisting bool) (store.ResourceInput, error) {
		return store.ResourceInput{
			TenantID:      "t-hyb",
			ResType:       "Patient",
			ResID:         matchedID,
			ResourceProto: pat3,
		}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, updRec.ResVersion)

	delCondRec, err := hyb.ConditionalDeleteResource(ctx, "t-hyb", "Patient", store.SearchParams{
		TenantID: "t-hyb",
		ResType:  "Patient",
		ID:       "hyb-p3",
	}, nil)
	require.NoError(t, err)
	assert.True(t, delCondRec.IsDeleted)
	_, err = replica.ReadResourceWithMeta(ctx, "t-hyb", "Patient", "hyb-p3")
	assert.ErrorIs(t, err, store.ErrResourceDeleted)

	// Re-create hyb-p3 and test ConditionalDeleteResources
	_, err = hyb.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      "t-hyb",
		ResType:       "Patient",
		ResID:         "hyb-p3",
		ResourceProto: pat3,
	})
	require.NoError(t, err)
	delRecs, err := hyb.ConditionalDeleteResources(ctx, "t-hyb", "Patient", store.SearchParams{
		TenantID: "t-hyb",
		ResType:  "Patient",
		ID:       "hyb-p3",
	}, 10, nil)
	require.NoError(t, err)
	assert.Len(t, delRecs, 1)

	// 5. ExecuteTransactionBundle commit & rollback
	pat4 := buildHybridPatientProto(t, "hyb-p4", "TxCommit")
	txRes, err := hyb.ExecuteTransactionBundle(ctx, []store.BundleOperation{
		{
			OpType: store.BundleOpWrite,
			Method: "POST",
			Input: store.ResourceInput{
				TenantID:      "t-hyb",
				ResType:       "Patient",
				ResID:         "hyb-p4",
				ResourceProto: pat4,
			},
		},
		{
			OpType: store.BundleOpDelete,
			Method: "DELETE",
			Input: store.ResourceInput{
				TenantID: "t-hyb",
				ResType:  "Patient",
				ResID:    "hyb-p2",
			},
		},
	})
	require.NoError(t, err)
	assert.Len(t, txRes, 2)
	_, err = replica.ReadResourceWithMeta(ctx, "t-hyb", "Patient", "hyb-p4")
	require.NoError(t, err)
	_, err = replica.ReadResourceWithMeta(ctx, "t-hyb", "Patient", "hyb-p2")
	assert.ErrorIs(t, err, store.ErrResourceDeleted)

	// 6. DeleteResource, DeleteResourceWithMeta, DeleteResourceWithExpectedVersion, Purge, Expunge, Reindex
	_, err = hyb.Reindex(ctx, store.ReindexParams{TenantID: "t-hyb", ResType: "Patient"})
	require.NoError(t, err)

	_, err = hyb.DeleteResourceWithExpectedVersion(ctx, "t-hyb", "Patient", "hyb-p4", 1)
	require.NoError(t, err)
	_, err = replica.ReadResourceWithMeta(ctx, "t-hyb", "Patient", "hyb-p4")
	assert.ErrorIs(t, err, store.ErrResourceDeleted)

	// Re-write hyb-p4 and test DeleteResource & DeleteResourceWithMeta
	require.NoError(t, hyb.WriteResource(ctx, store.ResourceInput{
		TenantID:      "t-hyb",
		ResType:       "Patient",
		ResID:         "hyb-p4",
		ResourceProto: pat4,
	}))
	require.NoError(t, hyb.DeleteResource(ctx, "t-hyb", "Patient", "hyb-p4"))
	require.NoError(t, hyb.WriteResource(ctx, store.ResourceInput{
		TenantID:      "t-hyb",
		ResType:       "Patient",
		ResID:         "hyb-p4",
		ResourceProto: pat4,
	}))
	_, err = hyb.DeleteResourceWithMeta(ctx, "t-hyb", "Patient", "hyb-p4")
	require.NoError(t, err)

	// Expunge deleted versions
	expCount, err := hyb.ExpungeResources(ctx, store.ExpungeParams{
		TenantID:                "t-hyb",
		ResType:                 "Patient",
		ResID:                   "hyb-p4",
		ExpungeDeletedResources: true,
		ExpungePreviousVersions: true,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, expCount, 1)

	// PurgePatientCompartment
	purged, err := hyb.PurgePatientCompartment(ctx, "t-hyb", "hyb-p1", false)
	require.NoError(t, err)
	assert.NotEmpty(t, purged)

	// 7. Read/Search/Analytical delegation methods
	_, err = hyb.Search(ctx, store.SearchParams{TenantID: "t-hyb", ResType: "Patient"})
	require.NoError(t, err)
	_, _ = hyb.PatientEverything(ctx, store.PatientEverythingParams{TenantID: "t-hyb", PatientID: "hyb-p1"})
	_, _ = hyb.EncounterEverything(ctx, store.EncounterEverythingParams{TenantID: "t-hyb", EncounterID: "e1"})
	_, _ = hyb.GroupEverything(ctx, store.GroupEverythingParams{TenantID: "t-hyb", GroupID: "g1", EmptyCohort: true})
	_, _ = hyb.BelongsToPatientCompartment(ctx, "t-hyb", "Patient", "hyb-p1", "hyb-p1")
	_, _ = hyb.SearchPatientMatchCandidates(ctx, store.PatientMatchCandidateParams{TenantID: "t-hyb"})
	_, _ = hyb.ResourceGraph(ctx, store.ResourceGraphParams{TenantID: "t-hyb", ResourceType: "Patient", ResourceID: "hyb-p1"})
	_, _ = hyb.AggregateMeta(ctx, "t-hyb", "Patient")

	// Export, Stats, CohortCounts
	_, err = hyb.ExportResources(ctx, store.ExportParams{TenantID: "t-hyb"})
	require.NoError(t, err)
	require.NoError(t, hyb.StreamExportResources(ctx, store.ExportParams{TenantID: "t-hyb"}, func(sr store.SearchResult) error { return nil }))
	require.NoError(t, hyb.StreamAnalyticalExport(ctx, store.ExportParams{TenantID: "t-hyb"}, func(sr store.SearchResult) error { return nil }))
	_, err = hyb.EvaluateObservationStats(ctx, store.ObservationStatsParams{TenantID: "t-hyb", Code: "8867-4"})
	require.NoError(t, err)
	_, err = hyb.EvaluateCohortCounts(ctx, "t-hyb", []string{"Patient"})
	require.NoError(t, err)

	// ExportJob, AuditEvent, OAuth2
	require.NoError(t, hyb.SaveExportJob(ctx, store.ExportJobRecord{ID: "j1", TenantID: "t-hyb"}))
	_, err = hyb.GetExportJob(ctx, "t-hyb", "j1")
	require.NoError(t, err)
	_, err = hyb.DeleteExportJob(ctx, "t-hyb", "j1")
	require.NoError(t, err)

	require.NoError(t, hyb.WriteAuditEvent(ctx, store.AuditRecord{AuditID: "a1", TenantID: "t-hyb", Action: "R", SubtypeCode: "read"}))
	_, err = hyb.ReadAuditEvent(ctx, "t-hyb", "a1")
	require.NoError(t, err)
	_, _, err = hyb.QueryAuditEvents(ctx, store.AuditQueryParams{TenantID: "t-hyb"})
	require.NoError(t, err)

	require.NoError(t, hyb.SaveAuthCode(ctx, "c1", store.OAuthAuthCodeRecord{ClientID: "cli", ExpiresAt: time.Now().Add(time.Minute)}))
	_, _, err = hyb.ConsumeAuthCode(ctx, "c1")
	require.NoError(t, err)
	require.NoError(t, hyb.SaveRefreshToken(ctx, "r1", store.OAuthRefreshTokenRecord{Subject: "practitioner-1", ExpiresAt: time.Now().Add(time.Minute)}))
	_, _, err = hyb.ConsumeRefreshToken(ctx, "r1")
	require.NoError(t, err)
	require.NoError(t, hyb.RevokeToken(ctx, "tok1", time.Minute))
	_, err = hyb.IsTokenRevoked(ctx, "tok1")
	require.NoError(t, err)

	assert.GreaterOrEqual(t, hyb.ReplicatedWrites(), uint64(5))
	assert.GreaterOrEqual(t, hyb.ReplicatedDeletes(), uint64(5))
	assert.Equal(t, uint64(0), hyb.ReplicationErrors())
}
