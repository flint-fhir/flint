package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

type backendStoreFixture struct {
	name string
	open func(t *testing.T, tenant string) store.Store
}

func concurrencyBackendFixtures() []backendStoreFixture {
	return []backendStoreFixture{
		{
			name: "sqlite",
			open: func(t *testing.T, _ string) store.Store {
				t.Helper()
				return sqlite.New(nil, sqlite.WithDSN(":memory:"))
			},
		},
		{
			name: "doris",
			open: func(t *testing.T, _ string) store.Store {
				t.Helper()
				return doris.New(nil)
			},
		},
		{
			name: "postgres",
			open: func(t *testing.T, tenant string) store.Store {
				t.Helper()
				db := openTestPostgres(t)
				t.Cleanup(func() { _ = db.Close() })
				ctx := t.Context()
				if err := postgres.RunMigrations(ctx, db); err != nil {
					t.Fatalf("run migrations: %v", err)
				}
				_ = postgres.PurgeTenant(ctx, db, tenant)
				t.Cleanup(func() { _ = postgres.PurgeTenant(context.Background(), db, tenant) })
				return postgres.New(db)
			},
		},
	}
}

func newHardeningServer(t *testing.T, st store.Store) (*server.Server, *httptest.Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		_ = srv.Close()
		ts.Close()
	})
	return srv, ts
}

func marshalHardeningPatientProto(t *testing.T, id, family string) []byte {
	t.Helper()
	p := &patpb.Patient{
		Id: &dtpb.Id{Value: id},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: family},
				Given:  []*dtpb.String{{Value: "Hardening"}},
			},
		},
	}
	b, err := proto.Marshal(p)
	if err != nil {
		t.Fatalf("marshal patient proto: %v", err)
	}
	return b
}

// TestConcurrency_IfMatchOptimisticCASContention verifies 16-way parallel HTTP PUTs with
// If-Match: W/"1" across all storage backends produce exactly 1 winner (200 OK, v2) and
// 15 version conflicts (412 Precondition Failed) with zero lost updates.
func TestConcurrency_IfMatchOptimisticCASContention(t *testing.T) {
	const workers = 16

	for _, fx := range concurrencyBackendFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			tenant := "cas-" + fx.name
			st := fx.open(t, tenant)
			_, ts := newHardeningServer(t, st)

			initStatus, _ := doJSONReq(t, http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-cas-1", "", map[string]any{
				"resourceType": "Patient",
				"id":           "pat-cas-1",
				"name":         []map[string]any{{"family": "Initial"}},
			})
			if initStatus != http.StatusCreated && initStatus != http.StatusOK {
				t.Fatalf("seed Patient/pat-cas-1 status=%d", initStatus)
			}

			start := make(chan struct{})
			var wg sync.WaitGroup
			var okCount, conflictCount, otherCount atomic.Int32
			var winningFamily atomic.Value

			for i := range workers {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()
					fam := fmt.Sprintf("Winner-%02d", workerID)
					bodyBytes, _ := json.Marshal(map[string]any{
						"resourceType": "Patient",
						"id":           "pat-cas-1",
						"name":         []map[string]any{{"family": fam}},
					})
					<-start
					req, err := http.NewRequest(http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-cas-1", bytes.NewReader(bodyBytes))
					if err != nil {
						otherCount.Add(1)
						return
					}
					req.Header.Set("Content-Type", "application/fhir+json")
					req.Header.Set("If-Match", `W/"1"`)
					resp, err := ts.Client().Do(req)
					if err != nil {
						otherCount.Add(1)
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()

					switch resp.StatusCode {
					case http.StatusOK:
						okCount.Add(1)
						winningFamily.Store(fam)
					case http.StatusPreconditionFailed:
						conflictCount.Add(1)
					default:
						otherCount.Add(1)
					}
				}(i)
			}

			close(start)
			wg.Wait()

			if okCount.Load() != 1 || conflictCount.Load() != workers-1 || otherCount.Load() != 0 {
				t.Fatalf("CAS contention expected 1x200 + %dx412, got 200=%d 412=%d other=%d",
					workers-1, okCount.Load(), conflictCount.Load(), otherCount.Load())
			}

			getStatus, got := doJSONReq(t, http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-cas-1", "", nil)
			if getStatus != http.StatusOK {
				t.Fatalf("GET Patient/pat-cas-1 status=%d", getStatus)
			}
			meta, _ := got["meta"].(map[string]any)
			if meta["versionId"] != "2" {
				t.Fatalf("expected final versionId=2, got %v", meta["versionId"])
			}
			names, _ := got["name"].([]any)
			if len(names) == 0 {
				t.Fatalf("expected Patient.name")
			}
			firstName, _ := names[0].(map[string]any)
			if firstName["family"] != winningFamily.Load() {
				t.Fatalf("persisted family=%v does not match CAS winner %v", firstName["family"], winningFamily.Load())
			}
		})
	}
}

// TestConcurrency_CreateOnlyStoreContention verifies 16-way parallel CreateOnly writes on the
// exact same (tenant, resType, resID) yield 1 created version-1 record and 15 ErrResourceAlreadyExists
// across SQLite, Doris, and PostgreSQL (closing the READ COMMITTED missing-row race).
func TestConcurrency_CreateOnlyStoreContention(t *testing.T) {
	const workers = 16

	for _, fx := range concurrencyBackendFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			tenant := "createonly-" + fx.name
			st := fx.open(t, tenant)
			ctx := t.Context()

			start := make(chan struct{})
			var wg sync.WaitGroup
			var createdCount, existsCount, otherErrCount atomic.Int32

			for i := range workers {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()
					payload := marshalHardeningPatientProto(t, "pat-co-1", fmt.Sprintf("Create-%02d", workerID))
					<-start
					rec, err := st.WriteResourceWithMeta(ctx, store.ResourceInput{
						TenantID:      tenant,
						ResType:       "Patient",
						ResID:         "pat-co-1",
						ResourceProto: payload,
						CreateOnly:    true,
					})
					switch {
					case err == nil && rec != nil && rec.Created && rec.ResVersion == 1:
						createdCount.Add(1)
					case errors.Is(err, store.ErrResourceAlreadyExists):
						existsCount.Add(1)
					default:
						otherErrCount.Add(1)
					}
				}(i)
			}

			close(start)
			wg.Wait()

			if createdCount.Load() != 1 || existsCount.Load() != workers-1 || otherErrCount.Load() != 0 {
				t.Fatalf("CreateOnly contention expected 1 created + %d ErrResourceAlreadyExists, got created=%d exists=%d other=%d",
					workers-1, createdCount.Load(), existsCount.Load(), otherErrCount.Load())
			}

			rec, err := st.ReadResourceWithMeta(ctx, tenant, "Patient", "pat-co-1")
			if err != nil {
				t.Fatalf("ReadResourceWithMeta after CreateOnly contention: %v", err)
			}
			if rec.ResVersion != 1 {
				t.Fatalf("expected ResVersion=1 after CreateOnly contention, got %d", rec.ResVersion)
			}
		})
	}
}

// TestConcurrency_IfNoneExistConditionalCreateContention verifies 16-way parallel POST /Patient
// requests carrying an identical If-None-Exist header produce exactly 1 created resource (201)
// and 15 idempotent matches (200) with total=1 in search.
func TestConcurrency_IfNoneExistConditionalCreateContention(t *testing.T) {
	const workers = 16

	for _, fx := range concurrencyBackendFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			tenant := "ifnone-" + fx.name
			st := fx.open(t, tenant)
			_, ts := newHardeningServer(t, st)

			bodyBytes, _ := json.Marshal(map[string]any{
				"resourceType": "Patient",
				"identifier": []map[string]any{
					{"system": "http://hospital.example.org/mrn", "value": "MRN-COND-999"},
				},
				"name": []map[string]any{{"family": "Idempotent"}},
			})

			start := make(chan struct{})
			var wg sync.WaitGroup
			var c201, c200, cOther atomic.Int32

			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					req, err := http.NewRequest(http.MethodPost, ts.URL+"/fhir/r4/"+tenant+"/Patient", bytes.NewReader(bodyBytes))
					if err != nil {
						cOther.Add(1)
						return
					}
					req.Header.Set("Content-Type", "application/fhir+json")
					req.Header.Set("If-None-Exist", "identifier=http://hospital.example.org/mrn|MRN-COND-999")
					resp, err := ts.Client().Do(req)
					if err != nil {
						cOther.Add(1)
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()

					switch resp.StatusCode {
					case http.StatusCreated:
						c201.Add(1)
					case http.StatusOK:
						c200.Add(1)
					default:
						cOther.Add(1)
					}
				}()
			}

			close(start)
			wg.Wait()

			if c201.Load() != 1 || c200.Load() != workers-1 || cOther.Load() != 0 {
				t.Fatalf("If-None-Exist contention expected 1x201 + %dx200, got 201=%d 200=%d other=%d",
					workers-1, c201.Load(), c200.Load(), cOther.Load())
			}

			sStatus, sr := doJSONReq(t, http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/Patient?identifier=http://hospital.example.org/mrn|MRN-COND-999", "", nil)
			if sStatus != http.StatusOK {
				t.Fatalf("search status=%d", sStatus)
			}
			if total, _ := sr["total"].(float64); int(total) != 1 {
				t.Fatalf("expected exactly 1 Patient for MRN-COND-999, got total=%v", sr["total"])
			}
		})
	}
}

// TestConcurrency_TransactionDeadlockFreeCrossOrder executes opposing multi-resource transaction
// bundles (A->B vs B->A) in parallel across SQLite, Doris, and PostgreSQL, verifying sorted
// lock acquisition prevents deadlocks (40P01) and leaves consistent final versions.
func TestConcurrency_TransactionDeadlockFreeCrossOrder(t *testing.T) {
	const workers = 16

	for _, fx := range concurrencyBackendFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			tenant := "txlock-" + fx.name
			st := fx.open(t, tenant)
			_, ts := newHardeningServer(t, st)

			for _, id := range []string{"pat-lock-a", "pat-lock-b"} {
				stCode, _ := doJSONReq(t, http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Patient/"+id, "", map[string]any{
					"resourceType": "Patient",
					"id":           id,
					"name":         []map[string]any{{"family": "Seed"}},
				})
				if stCode != http.StatusCreated && stCode != http.StatusOK {
					t.Fatalf("seed %s status=%d", id, stCode)
				}
			}

			start := make(chan struct{})
			var wg sync.WaitGroup
			var okCount, errCount atomic.Int32

			for i := range workers {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()
					firstID, secondID := "pat-lock-a", "pat-lock-b"
					if workerID%2 == 1 {
						firstID, secondID = "pat-lock-b", "pat-lock-a"
					}
					bundle := map[string]any{
						"resourceType": "Bundle",
						"type":         "transaction",
						"entry": []map[string]any{
							{
								"resource": map[string]any{
									"resourceType": "Patient",
									"id":           firstID,
									"name":         []map[string]any{{"family": fmt.Sprintf("Tx-%02d-1", workerID)}},
								},
								"request": map[string]any{"method": "PUT", "url": "Patient/" + firstID},
							},
							{
								"resource": map[string]any{
									"resourceType": "Patient",
									"id":           secondID,
									"name":         []map[string]any{{"family": fmt.Sprintf("Tx-%02d-2", workerID)}},
								},
								"request": map[string]any{"method": "PUT", "url": "Patient/" + secondID},
							},
						},
					}
					raw, _ := json.Marshal(bundle)
					<-start
					resp, err := ts.Client().Post(ts.URL+"/fhir/r4/"+tenant, "application/fhir+json", bytes.NewReader(raw))
					if err != nil {
						errCount.Add(1)
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						okCount.Add(1)
					} else {
						errCount.Add(1)
					}
				}(i)
			}

			close(start)
			wg.Wait()

			if okCount.Load() != workers || errCount.Load() != 0 {
				t.Fatalf("cross-order transaction bundles expected %d/0 ok/err, got ok=%d err=%d",
					workers, okCount.Load(), errCount.Load())
			}

			for _, id := range []string{"pat-lock-a", "pat-lock-b"} {
				stCode, got := doJSONReq(t, http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/Patient/"+id, "", nil)
				if stCode != http.StatusOK {
					t.Fatalf("GET %s status=%d", id, stCode)
				}
				meta, _ := got["meta"].(map[string]any)
				wantVer := fmt.Sprintf("%d", 1+workers)
				if meta["versionId"] != wantVer {
					t.Fatalf("Patient/%s expected versionId=%s, got %v", id, wantVer, meta["versionId"])
				}
			}
		})
	}
}

type txHookStore struct {
	store.Store
	beforeTx func(ctx context.Context)
}

func (h *txHookStore) ExecuteTransactionBundle(ctx context.Context, ops []store.BundleOperation) ([]store.BundleOpResult, error) {
	if h.beforeTx != nil {
		h.beforeTx(ctx)
	}
	return h.Store.ExecuteTransactionBundle(ctx, ops)
}

// TestConcurrency_BundlePatchDetectsConcurrentModification deterministically injects a concurrent
// resource modification between Bundle PATCH pre-read and ExecuteTransactionBundle commit, proving
// the transaction rejects the stale patch with 412 Precondition Failed instead of losing the update.
func TestConcurrency_BundlePatchDetectsConcurrentModification(t *testing.T) {
	base := sqlite.New(nil, sqlite.WithDSN(":memory:"))
	tenant := "bundle-patch-cas"
	var injectOnce sync.Once

	hooked := &txHookStore{
		Store: base,
		beforeTx: func(ctx context.Context) {
			injectOnce.Do(func() {
				_, err := base.WriteResourceWithMeta(ctx, store.ResourceInput{
					TenantID:      tenant,
					ResType:       "Patient",
					ResID:         "pat-patch-1",
					ResourceProto: marshalHardeningPatientProto(t, "pat-patch-1", "ConcurrentWinner"),
				})
				if err != nil {
					t.Errorf("concurrent mid-flight write failed: %v", err)
				}
			})
		},
	}

	_, ts := newHardeningServer(t, hooked)

	stCode, _ := doJSONReq(t, http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-patch-1", "", map[string]any{
		"resourceType": "Patient",
		"id":           "pat-patch-1",
		"active":       false,
		"name":         []map[string]any{{"family": "Original"}},
	})
	if stCode != http.StatusCreated && stCode != http.StatusOK {
		t.Fatalf("seed status=%d", stCode)
	}

	// Submit a transaction Bundle containing a PATCH WITHOUT an explicit ifMatch header.
	patchBundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry": []map[string]any{
			{
				"resource": map[string]any{
					"resourceType": "Parameters",
					"parameter": []map[string]any{
						{
							"name": "operation",
							"part": []map[string]any{
								{"name": "type", "valueCode": "replace"},
								{"name": "path", "valueString": "Patient.active"},
								{"name": "value", "valueBoolean": true},
							},
						},
					},
				},
				"request": map[string]any{
					"method": "PATCH",
					"url":    "Patient/pat-patch-1",
				},
			},
		},
	}

	txStatus, _ := doJSONReq(t, http.MethodPost, ts.URL+"/fhir/r4/"+tenant, "", patchBundle)
	if txStatus != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 Precondition Failed when concurrent modification occurs mid-Bundle PATCH, got %d", txStatus)
	}

	// Verify ConcurrentWinner (v2) was preserved intact.
	getStatus, got := doJSONReq(t, http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-patch-1", "", nil)
	if getStatus != http.StatusOK {
		t.Fatalf("GET status=%d", getStatus)
	}
	names, _ := got["name"].([]any)
	first, _ := names[0].(map[string]any)
	if first["family"] != "ConcurrentWinner" {
		t.Fatalf("expected ConcurrentWinner to survive failed Bundle PATCH, got %v", first["family"])
	}
}

// TestConcurrency_DorisRollbackIsolationAndWriteBatch hammers doris.Store concurrently with
// failing transaction bundles alongside standalone WriteResourceWithMeta, WriteBatch, and
// ConditionalCreateResource calls, ensuring rollbacks never revert concurrent standalone commits.
func TestConcurrency_DorisRollbackIsolationAndWriteBatch(t *testing.T) {
	ds := doris.New(nil)
	ctx := t.Context()
	tenant := "doris-isolation"

	// Seed shared target whose CAS failure triggers transaction rollback on the second op.
	_, err := ds.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      tenant,
		ResType:       "Patient",
		ResID:         "shared-target",
		ResourceProto: marshalHardeningPatientProto(t, "shared-target", "SeedV1"),
	})
	if err != nil {
		t.Fatalf("seed shared-target: %v", err)
	}

	const N = 24
	start := make(chan struct{})
	var wg sync.WaitGroup

	// Half the goroutines run failing 2-op transactions (op 1 mutates shared-target, op 2 fails CAS on ExpectedVersion=99).
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, txErr := ds.ExecuteTransactionBundle(ctx, []store.BundleOperation{
				{
					OpType: store.BundleOpWrite,
					Method: "PUT",
					Input: store.ResourceInput{
						TenantID:      tenant,
						ResType:       "Patient",
						ResID:         "shared-target",
						ResourceProto: marshalHardeningPatientProto(t, "shared-target", fmt.Sprintf("RolledBack-%d", idx)),
					},
				},
				{
					OpType: store.BundleOpWrite,
					Method: "PUT",
					Input: store.ResourceInput{
						TenantID:        tenant,
						ResType:         "Patient",
						ResID:           "shared-target",
						ResourceProto:   marshalHardeningPatientProto(t, "shared-target", "NeverCommits"),
						ExpectedVersion: 99,
					},
				},
			})
			if !errors.Is(txErr, store.ErrVersionConflict) {
				t.Errorf("expected ErrVersionConflict from failing tx %d, got %v", idx, txErr)
			}
		}(i)

		// Concurrent standalone writers, WriteBatch calls, and ConditionalCreateResource calls.
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			indID := fmt.Sprintf("standalone-%02d", idx)
			if _, wErr := ds.WriteResourceWithMeta(ctx, store.ResourceInput{
				TenantID:      tenant,
				ResType:       "Patient",
				ResID:         indID,
				ResourceProto: marshalHardeningPatientProto(t, indID, "Committed"),
			}); wErr != nil {
				t.Errorf("standalone write %s failed: %v", indID, wErr)
			}

			batchA := fmt.Sprintf("batch-%02d-a", idx)
			batchB := fmt.Sprintf("batch-%02d-b", idx)
			if bErr := ds.WriteBatch(ctx, []store.ResourceInput{
				{TenantID: tenant, ResType: "Patient", ResID: batchA, ResourceProto: marshalHardeningPatientProto(t, batchA, "Batch")},
				{TenantID: tenant, ResType: "Patient", ResID: batchB, ResourceProto: marshalHardeningPatientProto(t, batchB, "Batch")},
			}); bErr != nil {
				t.Errorf("WriteBatch %d failed: %v", idx, bErr)
			}

			_, _, ccErr := ds.ConditionalCreateResource(ctx, store.ResourceInput{
				TenantID:      tenant,
				ResType:       "Patient",
				ResID:         fmt.Sprintf("cond-cand-%02d", idx),
				ResourceProto: marshalHardeningPatientProto(t, fmt.Sprintf("cond-cand-%02d", idx), "CondShared"),
				SearchIndexes: &store.SearchIndexes{
					Tokens: []store.SpidxToken{
						{TenantID: tenant, ResType: "Patient", ResID: fmt.Sprintf("cond-cand-%02d", idx), SpName: "identifier", SpSystem: "http://example.org", SpValue: "DORIS-SHARED"},
					},
				},
			}, store.SearchParams{
				TenantID: tenant,
				ResType:  "Patient",
				Tokens:   map[string]string{"identifier": "http://example.org|DORIS-SHARED"},
			})
			if ccErr != nil {
				t.Errorf("ConditionalCreateResource %d failed: %v", idx, ccErr)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	// shared-target must still be at version 1 ("SeedV1").
	sharedRec, err := ds.ReadResourceWithMeta(ctx, tenant, "Patient", "shared-target")
	if err != nil {
		t.Fatalf("read shared-target: %v", err)
	}
	if sharedRec.ResVersion != 1 {
		t.Fatalf("expected shared-target ResVersion=1 after rolled-back txs, got %d", sharedRec.ResVersion)
	}

	// Every standalone and batch resource must still be persisted.
	for i := range N {
		for _, id := range []string{
			fmt.Sprintf("standalone-%02d", i),
			fmt.Sprintf("batch-%02d-a", i),
			fmt.Sprintf("batch-%02d-b", i),
		} {
			if _, rErr := ds.ReadResourceWithMeta(ctx, tenant, "Patient", id); rErr != nil {
				t.Fatalf("expected %s to remain committed, got %v", id, rErr)
			}
		}
	}

	// ConditionalCreateResource across N goroutines must have persisted exactly 1 resource.
	sr, err := ds.Search(ctx, store.SearchParams{
		TenantID: tenant,
		ResType:  "Patient",
		Tokens:   map[string]string{"identifier": "http://example.org|DORIS-SHARED"},
	})
	if err != nil {
		t.Fatalf("search DORIS-SHARED: %v", err)
	}
	if sr.Total != 1 {
		t.Fatalf("expected 1 DORIS-SHARED resource, got %d", sr.Total)
	}
}

type reorderPrimaryStore struct {
	store.Store
	v2Entered chan struct{}
	releaseV2 chan struct{}
}

func (r *reorderPrimaryStore) WriteResourceWithMeta(ctx context.Context, input store.ResourceInput) (*store.ResourceRecord, error) {
	rec, err := r.Store.WriteResourceWithMeta(ctx, input)
	if err != nil {
		return nil, err
	}
	if rec.ResID == "pat-ooo" && rec.ResVersion == 2 {
		close(r.v2Entered)
		<-r.releaseV2
	}
	return rec, nil
}

// TestConcurrency_HybridStoreMonotonicReplication deterministically forces version 2 to reach
// HybridStore replication AFTER version 3 has already replicated, verifying the monotonic
// version guard drops stale v2 on the replica and resets cleanly on ExpungeResources.
func TestConcurrency_HybridStoreMonotonicReplication(t *testing.T) {
	ctx := t.Context()
	rawPrimary := sqlite.New(nil, sqlite.WithDSN(":memory:"))
	reordered := &reorderPrimaryStore{
		Store:     rawPrimary,
		v2Entered: make(chan struct{}),
		releaseV2: make(chan struct{}),
	}
	replica := doris.New(nil)
	hyb := store.NewHybridStore(reordered, replica)

	tenant := "hyb-monotonic"
	// v1 seed
	if _, err := hyb.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      tenant,
		ResType:       "Patient",
		ResID:         "pat-ooo",
		ResourceProto: marshalHardeningPatientProto(t, "pat-ooo", "VersionOne"),
	}); err != nil {
		t.Fatalf("write v1: %v", err)
	}

	// Launch v2 writer — commits v2 on primary, then pauses before replicating to Doris.
	v2Done := make(chan error, 1)
	go func() {
		_, err := hyb.WriteResourceWithMeta(ctx, store.ResourceInput{
			TenantID:      tenant,
			ResType:       "Patient",
			ResID:         "pat-ooo",
			ResourceProto: marshalHardeningPatientProto(t, "pat-ooo", "StaleVersionTwo"),
		})
		v2Done <- err
	}()

	<-reordered.v2Entered

	// Now commit AND replicate v3 while v2 is still paused before replicateRecord!
	recV3, err := hyb.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      tenant,
		ResType:       "Patient",
		ResID:         "pat-ooo",
		ResourceProto: marshalHardeningPatientProto(t, "pat-ooo", "FreshVersionThree"),
	})
	if err != nil || recV3.ResVersion != 3 {
		t.Fatalf("write v3 err=%v rec=%+v", err, recV3)
	}

	// Release v2 to attempt stale replication into Doris.
	close(reordered.releaseV2)
	if err := <-v2Done; err != nil {
		t.Fatalf("v2 write returned error: %v", err)
	}

	// Replica MUST retain FreshVersionThree (and NOT be overwritten by StaleVersionTwo).
	repRec, err := replica.ReadResourceWithMeta(ctx, tenant, "Patient", "pat-ooo")
	if err != nil {
		t.Fatalf("read replica: %v", err)
	}
	var gotPat patpb.Patient
	if err := proto.Unmarshal(repRec.ResourceProto, &gotPat); err != nil {
		t.Fatalf("unmarshal replica patient: %v", err)
	}
	if len(gotPat.GetName()) == 0 || gotPat.GetName()[0].GetFamily().GetValue() != "FreshVersionThree" {
		t.Fatalf("expected replica to retain FreshVersionThree, got %v", gotPat.GetName())
	}

	// Hard expunge resets tracked version state so recreating pat-ooo at v1 replicates cleanly.
	if _, err := hyb.ExpungeResources(ctx, store.ExpungeParams{
		TenantID:          tenant,
		ResType:           "Patient",
		ResID:             "pat-ooo",
		ExpungeEverything: true,
	}); err != nil {
		t.Fatalf("ExpungeResources: %v", err)
	}

	recReborn, err := hyb.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      tenant,
		ResType:       "Patient",
		ResID:         "pat-ooo",
		ResourceProto: marshalHardeningPatientProto(t, "pat-ooo", "RebornVersionOne"),
	})
	if err != nil || recReborn.ResVersion != 1 {
		t.Fatalf("recreate v1 after expunge err=%v rec=%+v", err, recReborn)
	}
	repReborn, err := replica.ReadResourceWithMeta(ctx, tenant, "Patient", "pat-ooo")
	if err != nil {
		t.Fatalf("read reborn replica: %v", err)
	}
	var rebornPat patpb.Patient
	_ = proto.Unmarshal(repReborn.ResourceProto, &rebornPat)
	if len(rebornPat.GetName()) == 0 || rebornPat.GetName()[0].GetFamily().GetValue() != "RebornVersionOne" {
		t.Fatalf("expected RebornVersionOne on replica after expunge reset, got %v", rebornPat.GetName())
	}
}

// TestConcurrency_CrossBackendUCUMSearchParity verifies identical quantity search behavior across
// SQLite, Doris, and PostgreSQL for all synchronized UCUM conversion factors (pressure, small mass,
// length, substance amount, mass/molar concentration, and Julian calendar durations).
func TestConcurrency_CrossBackendUCUMSearchParity(t *testing.T) {
	cases := []struct {
		id          string
		code        string
		storedVal   float64
		storedUnit  string
		queryString string
	}{
		// 120 mm[Hg] == 15.9986848 kPa -> search gt15|http://unitsofmeasure.org|kPa & lt17|...|kPa
		{id: "obs-bp", code: "8480-6", storedVal: 120, storedUnit: "mm[Hg]", queryString: "code=8480-6&value-quantity=gt15|http://unitsofmeasure.org|kPa&value-quantity=lt17|http://unitsofmeasure.org|kPa"},
		// 500 pg == 0.5 ng
		{id: "obs-pg", code: "1000-1", storedVal: 500, storedUnit: "pg", queryString: "code=1000-1&value-quantity=0.5|http://unitsofmeasure.org|ng"},
		// 550 nm == 0.55 um
		{id: "obs-nm", code: "1000-2", storedVal: 550, storedUnit: "nm", queryString: "code=1000-2&value-quantity=0.55|http://unitsofmeasure.org|um"},
		// 5 mmol == 0.005 mol
		{id: "obs-mol", code: "1000-3", storedVal: 5, storedUnit: "mmol", queryString: "code=1000-3&value-quantity=0.005|http://unitsofmeasure.org|mol"},
		// 250 ug/mL == 250 mg/L == 0.25 g/L
		{id: "obs-conc", code: "1000-4", storedVal: 250, storedUnit: "ug/mL", queryString: "code=1000-4&value-quantity=250|http://unitsofmeasure.org|mg/L"},
		// 12 mo == 1 a (Julian year: 12 * 2,629,800s == 31,557,600s)
		{id: "obs-age", code: "1000-5", storedVal: 12, storedUnit: "mo", queryString: "code=1000-5&value-quantity=1|http://unitsofmeasure.org|a"},
	}

	for _, fx := range concurrencyBackendFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			tenant := "ucum-" + fx.name
			st := fx.open(t, tenant)
			_, ts := newHardeningServer(t, st)

			for _, tc := range cases {
				obs := map[string]any{
					"resourceType": "Observation",
					"id":           tc.id,
					"status":       "final",
					"code": map[string]any{
						"coding": []map[string]any{{"system": "http://loinc.org", "code": tc.code}},
					},
					"valueQuantity": map[string]any{
						"value":  tc.storedVal,
						"unit":   tc.storedUnit,
						"system": "http://unitsofmeasure.org",
						"code":   tc.storedUnit,
					},
				}
				stCode, _ := doJSONReq(t, http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Observation/"+tc.id, "", obs)
				if stCode != http.StatusCreated && stCode != http.StatusOK {
					t.Fatalf("[%s] PUT Observation/%s status=%d", fx.name, tc.id, stCode)
				}

				qStatus, bundle := doJSONReq(t, http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/Observation?"+tc.queryString, "", nil)
				if qStatus != http.StatusOK {
					t.Fatalf("[%s] GET Observation?%s status=%d", fx.name, tc.queryString, qStatus)
				}
				if total, _ := bundle["total"].(float64); int(total) != 1 {
					t.Fatalf("[%s] UCUM parity failed for %s (%v %s -> %s): expected total=1, got %v",
						fx.name, tc.id, tc.storedVal, tc.storedUnit, tc.queryString, bundle["total"])
				}
			}
		})
	}
}

type blockingExportStore struct {
	store.Store
	exportStarted chan struct{}
	startedOnce   sync.Once
}

func (b *blockingExportStore) StreamExportResources(ctx context.Context, params store.ExportParams, fn func(store.SearchResult) error) error {
	b.startedOnce.Do(func() { close(b.exportStarted) })
	<-ctx.Done()
	return ctx.Err()
}

// TestServer_GracefulShutdownAndCloseZeroGoroutineLeaks verifies Server.Close() / Shutdown()
// cleanly terminates hijacked RFC 6455 WebSocket streams, slow rest-hook webhook retries, and
// in-flight async Bulk $export jobs without goroutine leaks or data races.
func TestServer_GracefulShutdownAndCloseZeroGoroutineLeaks(t *testing.T) {
	// Settle runtime goroutines before capturing baseline.
	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()

	webhookEntered := make(chan struct{})
	var webhookOnce sync.Once
	webhookUnblock := make(chan struct{})
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookOnce.Do(func() { close(webhookEntered) })
		select {
		case <-r.Context().Done():
		case <-webhookUnblock:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer func() {
		close(webhookUnblock)
		webhookSrv.Close()
	}()

	exportStarted := make(chan struct{})
	st := &blockingExportStore{
		Store:         sqlite.New(nil, sqlite.WithDSN(":memory:")),
		exportStarted: exportStarted,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	tenant := "shutdown-leak-test"

	// 1. Create a WebSocket Subscription and bind an upgraded RFC 6455 client.
	wsSubStatus, _ := doJSONReq(t, http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Subscription/sub-ws-1", "", map[string]any{
		"resourceType": "Subscription",
		"id":           "sub-ws-1",
		"status":       "requested",
		"reason":       "Leak test WS",
		"criteria":     "Observation?code=8867-4",
		"channel": map[string]any{
			"type":    "websocket",
			"payload": "application/fhir+json",
		},
	})
	if wsSubStatus != http.StatusCreated && wsSubStatus != http.StatusOK {
		t.Fatalf("create ws subscription status=%d", wsSubStatus)
	}

	wsClient, hsCode, hsBody := dialTestWS(t, ts.URL, "/fhir/r4/"+tenant+"/$subscriptions-ws", nil)
	if hsCode != http.StatusSwitchingProtocols {
		t.Fatalf("ws handshake status=%d body=%s", hsCode, hsBody)
	}
	wsClient.sendText("bind sub-ws-1")
	opAck, ackPayload, ackErr := wsClient.readFrame(3 * time.Second)
	if ackErr != nil || opAck != 0x1 || string(ackPayload) != "bound sub-ws-1" {
		t.Fatalf("ws bind ack err=%v opcode=0x%x payload=%q", ackErr, opAck, string(ackPayload))
	}

	// 2. Create a rest-hook Subscription pointing at slow/503 webhookSrv and trigger a matching write.
	rhStatus, _ := doJSONReq(t, http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Subscription/sub-rh-1", "", map[string]any{
		"resourceType": "Subscription",
		"id":           "sub-rh-1",
		"status":       "requested",
		"reason":       "Leak test rest-hook",
		"criteria":     "Patient?active=true",
		"channel": map[string]any{
			"type":     "rest-hook",
			"endpoint": webhookSrv.URL + "/notify",
			"payload":  "application/fhir+json",
		},
	})
	if rhStatus != http.StatusCreated && rhStatus != http.StatusOK {
		t.Fatalf("create rest-hook subscription status=%d", rhStatus)
	}
	patStatus, _ := doJSONReq(t, http.MethodPut, ts.URL+"/fhir/r4/"+tenant+"/Patient/pat-sub-1", "", map[string]any{
		"resourceType": "Patient",
		"id":           "pat-sub-1",
		"active":       true,
		"name":         []map[string]any{{"family": "Trigger"}},
	})
	if patStatus != http.StatusCreated && patStatus != http.StatusOK {
		t.Fatalf("trigger patient write status=%d", patStatus)
	}
	select {
	case <-webhookEntered:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for rest-hook delivery worker to start")
	}

	// 3. Kick off an async Bulk $export job that blocks inside StreamExportResources until cancelled.
	expReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/fhir/r4/"+tenant+"/$export?_type=Patient", nil)
	expReq.Header.Set("Prefer", "respond-async")
	expReq.Header.Set("Accept", "application/fhir+json")
	expResp, err := ts.Client().Do(expReq)
	if err != nil {
		t.Fatalf("kickoff async $export: %v", err)
	}
	_, _ = io.Copy(io.Discard, expResp.Body)
	expResp.Body.Close()
	if expResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted for async $export, got %d", expResp.StatusCode)
	}
	select {
	case <-exportStarted:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for async $export worker to start")
	}

	// 4. Gracefully close the server and verify all background workers & hijacked WS connections drain!
	if err := srv.Close(); err != nil {
		t.Fatalf("srv.Close() returned error: %v", err)
	}
	// Verify Shutdown is idempotent when called again.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("second srv.Shutdown(context.Background()) returned error: %v", err)
	}
	_ = wsClient.conn.Close()
	ts.CloseClientConnections()
	ts.Close()
	webhookSrv.CloseClientConnections()

	// Allow runtime scheduler to reap closed HTTP/TCP goroutines.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baselineGoroutines+2 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	finalGoroutines := runtime.NumGoroutine()
	if strings.Contains(fmt.Sprintf("%d", finalGoroutines), "") && finalGoroutines > baselineGoroutines+2 {
		t.Fatalf("goroutine leak detected after Server.Close(): baseline=%d final=%d", baselineGoroutines, finalGoroutines)
	}
}
