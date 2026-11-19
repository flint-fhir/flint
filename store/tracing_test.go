package store_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/telemetry"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/sqlite"
)

func TestWrapWithTracing_And_ClusterStateParity(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	var spanNames []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Name string `json:"name"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if json.Unmarshal(body, &payload) == nil {
			mu.Lock()
			for _, rs := range payload.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						spanNames = append(spanNames, sp.Name)
					}
				}
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	tracer, err := telemetry.NewTracer(telemetry.Config{
		Endpoint:      collector.URL,
		Protocol:      "http/json",
		ServiceName:   "flint-store-test",
		SampleRatio:   telemetry.Float64Ptr(1.0),
		BatchSize:     8,
		FlushInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)

	// Disabled / nil passthrough
	rawSQLite := sqlite.New(nil, sqlite.WithDSN(":memory:"))
	assert.Equal(t, rawSQLite, store.WrapWithTracing(rawSQLite, nil))

	hyb := store.NewHybridStore(rawSQLite, doris.New(nil))
	traced := store.WrapWithTracing(hyb, tracer)
	as, ok := traced.(store.AnalyticalStore)
	require.True(t, ok, "wrapped HybridStore must preserve AnalyticalStore")
	assert.Equal(t, "doris", as.BackendName())

	for _, backend := range []struct {
		name string
		st   store.Store
	}{
		{"traced-hybrid", traced},
		{"sqlite", sqlite.New(nil)},
		{"doris", doris.New(nil)},
	} {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.st
			require.NoError(t, s.Ping(ctx))

			patBytes := buildHybridPatientProto(t, "tr-p1", "TraceFamily")
			rec, err := s.WriteResourceWithMeta(ctx, store.ResourceInput{
				TenantID:      "t1",
				ResType:       "Patient",
				ResID:         "tr-p1",
				ResourceProto: patBytes,
			})
			require.NoError(t, err)
			assert.Equal(t, 1, rec.ResVersion)
			require.NoError(t, s.WriteResource(ctx, store.ResourceInput{
				TenantID:      "t1",
				ResType:       "Patient",
				ResID:         "tr-p1",
				ResourceProto: patBytes,
			}))
			require.NoError(t, s.WriteBatch(ctx, []store.ResourceInput{{
				TenantID:      "t1",
				ResType:       "Patient",
				ResID:         "tr-p2",
				ResourceProto: buildHybridPatientProto(t, "tr-p2", "BatchTwo"),
			}}))
			_, _, _ = s.ConditionalCreateResource(ctx, store.ResourceInput{
				TenantID:      "t1",
				ResType:       "Patient",
				ResID:         "tr-p3",
				ResourceProto: buildHybridPatientProto(t, "tr-p3", "CondThree"),
			}, store.SearchParams{TenantID: "t1", ResType: "Patient", ID: "tr-p3"})
			_, _ = s.ConditionalUpdateResource(ctx, "t1", "Patient", store.SearchParams{TenantID: "t1", ResType: "Patient", ID: "tr-p3"}, func(matchedID string, _ bool) (store.ResourceInput, error) {
				return store.ResourceInput{TenantID: "t1", ResType: "Patient", ResID: matchedID, ResourceProto: patBytes}, nil
			})
			_, _ = s.ConditionalDeleteResource(ctx, "t1", "Patient", store.SearchParams{TenantID: "t1", ResType: "Patient", ID: "tr-p3"}, nil)
			_, _ = s.ConditionalDeleteResources(ctx, "t1", "Patient", store.SearchParams{TenantID: "t1", ResType: "Patient", ID: "tr-p2"}, 10, nil)
			_, _ = s.ExecuteTransactionBundle(ctx, []store.BundleOperation{{
				OpType: store.BundleOpWrite,
				Method: "PUT",
				Input:  store.ResourceInput{TenantID: "t1", ResType: "Patient", ResID: "tr-p1", ResourceProto: patBytes},
			}})

			_, err = s.ReadResource(ctx, "t1", "Patient", "tr-p1")
			require.NoError(t, err)
			_, _ = s.ReadResource(ctx, "t1", "Patient", "missing-404")
			_, _ = s.ReadResourceWithMeta(ctx, "t1", "Patient", "tr-p1")
			_, _ = s.ReadResourceVersion(ctx, "t1", "Patient", "tr-p1", 1)
			_, _, _ = s.ListResourceHistory(ctx, "t1", "Patient", "tr-p1", 10, 0)
			_, _, _ = s.ListHistory(ctx, "t1", "Patient", "tr-p1", nil, 10, 0)
			_, _, _ = s.ListHistoryWithParams(ctx, store.HistoryParams{TenantID: "t1", ResType: "Patient"})
			_, _ = s.Search(ctx, store.SearchParams{TenantID: "t1", ResType: "Patient"})
			_, _ = s.PatientEverything(ctx, store.PatientEverythingParams{TenantID: "t1", PatientID: "tr-p1"})
			_, _ = s.EncounterEverything(ctx, store.EncounterEverythingParams{TenantID: "t1", EncounterID: "missing"})
			_, _ = s.GroupEverything(ctx, store.GroupEverythingParams{TenantID: "t1", GroupID: "missing", AllPatients: true})
			_, _ = s.BelongsToPatientCompartment(ctx, "t1", "Patient", "tr-p1", "tr-p1")
			_, _ = s.SearchPatientMatchCandidates(ctx, store.PatientMatchCandidateParams{TenantID: "t1"})
			_, _ = s.ResourceGraph(ctx, store.ResourceGraphParams{TenantID: "t1", ResourceType: "Patient", ResourceID: "tr-p1"})
			_, _ = s.AggregateMeta(ctx, "t1", "Patient")
			_, _ = s.ExportResources(ctx, store.ExportParams{TenantID: "t1"})
			_ = s.StreamExportResources(ctx, store.ExportParams{TenantID: "t1"}, func(store.SearchResult) error { return nil })
			_, _ = s.Reindex(ctx, store.ReindexParams{TenantID: "t1", ResType: "Patient", Extractors: store.DefaultIndexExtractors()})

			// Cluster secrets, state, CAS, consume, nonces, and refresh peek
			sec1, err := s.GetOrCreateClusterSecret(ctx, "jwt-key", func() ([]byte, error) {
				return []byte("secret-v1"), nil
			})
			require.NoError(t, err)
			sec2, err := s.GetOrCreateClusterSecret(ctx, "jwt-key", func() ([]byte, error) {
				return []byte("secret-v2-ignored"), nil
			})
			require.NoError(t, err)
			assert.Equal(t, sec1, sec2)

			ok1, err := s.ClaimNonce(ctx, "jti-abc", time.Minute)
			require.NoError(t, err)
			assert.True(t, ok1)
			ok2, err := s.ClaimNonce(ctx, "jti-abc", time.Minute)
			require.NoError(t, err)
			assert.False(t, ok2, "duplicate jti nonce must be rejected")

			exp := time.Now().UTC().Add(time.Hour)
			require.NoError(t, s.SaveAuthCode(ctx, "ac-1", store.OAuthAuthCodeRecord{ClientID: "c1", TenantID: "t1", FhirUser: "Practitioner/1", ExpiresAt: exp}))
			_, acOK, err := s.ConsumeAuthCode(ctx, "ac-1")
			require.NoError(t, err)
			assert.True(t, acOK)

			require.NoError(t, s.SaveRefreshToken(ctx, "rt-1", store.OAuthRefreshTokenRecord{Subject: "u1", GrantID: "g-1", TenantID: "t1", ExpiresAt: exp}))
			peeked, rtOK, err := s.PeekRefreshToken(ctx, "rt-1")
			require.NoError(t, err)
			assert.True(t, rtOK)
			assert.Equal(t, "g-1", peeked.GrantID)
			_, rtOK, err = s.ConsumeRefreshToken(ctx, "rt-1")
			require.NoError(t, err)
			assert.True(t, rtOK)
			require.NoError(t, s.RevokeToken(ctx, "grant:g-1", time.Minute))
			rev, err := s.IsTokenRevoked(ctx, "grant:g-1")
			require.NoError(t, err)
			assert.True(t, rev)

			cs1, created, err := s.CompareAndSwapClusterState(ctx, 0, store.ClusterStateRecord{
				Namespace:    "shlink",
				Key:          "link-1",
				TenantID:     "t1",
				SecondaryKey: "sec-1",
				Value:        []byte(`{"views":0}`),
				ExpiresAt:    &exp,
			})
			require.NoError(t, err)
			assert.True(t, created)
			assert.Equal(t, int64(1), cs1.Version)

			_, dupCreated, err := s.CompareAndSwapClusterState(ctx, 0, store.ClusterStateRecord{
				Namespace: "shlink",
				Key:       "link-1",
				Value:     []byte(`{"views":99}`),
			})
			require.NoError(t, err)
			assert.False(t, dupCreated)

			cs2, casOK, err := s.CompareAndSwapClusterState(ctx, 1, store.ClusterStateRecord{
				Namespace:    "shlink",
				Key:          "link-1",
				TenantID:     "t1",
				SecondaryKey: "sec-1",
				Value:        []byte(`{"views":1}`),
				ExpiresAt:    &exp,
			})
			require.NoError(t, err)
			assert.True(t, casOK)
			assert.Equal(t, int64(2), cs2.Version)

			gotByKey, found, err := s.GetClusterState(ctx, "shlink", "link-1")
			require.NoError(t, err)
			assert.True(t, found)
			assert.Equal(t, int64(2), gotByKey.Version)

			gotBySec, found, err := s.GetClusterStateBySecondary(ctx, "shlink", "sec-1")
			require.NoError(t, err)
			assert.True(t, found)
			assert.Equal(t, "link-1", gotBySec.Key)

			putUpd, err := s.PutClusterState(ctx, store.ClusterStateRecord{
				Namespace: "shlink",
				Key:       "link-1",
				TenantID:  "t1",
				Value:     []byte(`{"views":2}`),
			})
			require.NoError(t, err)
			assert.Equal(t, int64(3), putUpd.Version)

			listed, err := s.ListClusterStates(ctx, "shlink", "t1")
			require.NoError(t, err)
			assert.Len(t, listed, 1)

			consumed, found, err := s.ConsumeClusterState(ctx, "shlink", "link-1")
			require.NoError(t, err)
			assert.True(t, found)
			assert.Equal(t, "link-1", consumed.Key)

			_, foundAgain, err := s.ConsumeClusterState(ctx, "shlink", "link-1")
			require.NoError(t, err)
			assert.False(t, foundAgain)

			_, _ = s.PutClusterState(ctx, store.ClusterStateRecord{Namespace: "shlink", Key: "link-del", Value: []byte("x")})
			deleted, err := s.DeleteClusterState(ctx, "shlink", "link-del")
			require.NoError(t, err)
			assert.True(t, deleted)

			require.NoError(t, s.SaveExportJob(ctx, store.ExportJobRecord{ID: "ej-1", TenantID: "t1", Status: "in-progress"}))
			ej, err := s.GetExportJob(ctx, "t1", "ej-1")
			require.NoError(t, err)
			assert.Equal(t, "in-progress", ej.Status)
			_, _ = s.DeleteExportJob(ctx, "t1", "ej-1")

			require.NoError(t, s.WriteAuditEvent(ctx, store.AuditRecord{
				TenantID: "t1",
				AuditID:  "aud-tr-1",
				Action:   "R",
				Outcome:  "0",
				TraceID:  "4bf92f3577b34da6a3ce929d0e0e4736",
				SpanID:   "00f067aa0ba902b7",
			}))
			aud, err := s.ReadAuditEvent(ctx, "t1", "aud-tr-1")
			require.NoError(t, err)
			assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", aud.TraceID)
			_, _, _ = s.QueryAuditEvents(ctx, store.AuditQueryParams{TenantID: "t1"})

			_ = s.DeleteResource(ctx, "t1", "Patient", "tr-p1")
			_, _ = s.DeleteResourceWithMeta(ctx, "t1", "Patient", "tr-p1")
			_, _ = s.DeleteResourceWithExpectedVersion(ctx, "t1", "Patient", "tr-p1", 99)
			_, _ = s.PurgePatientCompartment(ctx, "t1", "tr-p1", true)
			_, _ = s.ExpungeResources(ctx, store.ExpungeParams{TenantID: "t1", ExpungeEverything: true})
		})
	}

	_, _ = as.EvaluateObservationStats(ctx, store.ObservationStatsParams{TenantID: "t1"})
	_ = as.StreamAnalyticalExport(ctx, store.ExportParams{TenantID: "t1"}, func(store.SearchResult) error { return nil })
	_, _ = as.EvaluateCohortCounts(ctx, "t1", []string{"Patient"})

	require.NoError(t, tracer.Shutdown(ctx))
	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, spanNames, "store.WriteResourceWithMeta")
	assert.Contains(t, spanNames, "store.Search")
	assert.Contains(t, spanNames, "store.CompareAndSwapClusterState")
	assert.Contains(t, spanNames, "store.EvaluateObservationStats")
}
