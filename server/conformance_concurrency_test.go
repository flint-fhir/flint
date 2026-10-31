package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

// TestHL7FHIRR4ConcurrentContentionAndRaceConformanceSuite verifies high-concurrency
// optimistic locking (If-Match / 412 Precondition Failed), CAS retry loops,
// TOCTOU-free conditional create (If-None-Exist), concurrent unconditional PUT/PATCH
// version monotonicity, and multi-tenant transaction rollback isolation under -race.
func TestHL7FHIRR4ConcurrentContentionAndRaceConformanceSuite(t *testing.T) {
	db := openTestPostgres(t)
	store := postgres.New(db)
	ctx := context.Background()

	tenantA := "conformance-conc-alpha"
	tenantB := "conformance-conc-beta"
	_ = postgres.PurgeTenant(ctx, db, tenantA)
	_ = postgres.PurgeTenant(ctx, db, tenantB)
	t.Cleanup(func() {
		_ = postgres.PurgeTenant(ctx, db, tenantA)
		_ = postgres.PurgeTenant(ctx, db, tenantB)
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(store, logger)
	server.RegisterDefaultResourceTypes(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	doReq := func(method, path string, headers map[string]string, body string) (int, http.Header, map[string]any, error) {
		var bodyReader io.Reader
		if body != "" {
			bodyReader = bytes.NewBufferString(body)
		}
		req, err := http.NewRequest(method, ts.URL+path, bodyReader)
		if err != nil {
			return 0, nil, nil, err
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/fhir+json")
		}
		req.Header.Set("Accept", "application/fhir+json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &out)
		}
		return resp.StatusCode, resp.Header, out, nil
	}

	// =========================================================================
	// 1. Concurrent If-Match Optimistic Locking Contention & CAS Retry Loop
	// =========================================================================
	t.Run("Concurrent_IfMatch_OptimisticLocking_Contention_And_CAS_Retry_Loop", func(t *testing.T) {
		patID := "conc-cas-pat-1"
		initBody := fmt.Sprintf(`{"resourceType":"Patient","id":%q,"active":true,"name":[{"family":"Initial","given":["CAS"]}],"gender":"female","birthDate":"1990-01-01"}`, patID)
		status, hdr, _, err := doReq(http.MethodPut, "/fhir/r4/"+tenantA+"/Patient/"+patID, nil, initBody)
		if err != nil || (status != http.StatusCreated && status != http.StatusOK) {
			t.Fatalf("seed Patient status=%d err=%v", status, err)
		}
		if got := hdr.Get("ETag"); got != `W/"1"` {
			t.Fatalf("expected initial ETag W/\"1\", got %q", got)
		}

		// Phase A: 16 goroutines simultaneously attempt PUT with If-Match: W/"1".
		// Exactly 1 must win with 200 OK (ETag W/"2") and 15 must fail with 412 Precondition Failed.
		const contenders = 16
		var (
			wg        sync.WaitGroup
			startGate = make(chan struct{})
			wins      atomic.Int32
			conflicts atomic.Int32
			otherErr  atomic.Int32
		)
		for g := 0; g < contenders; g++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				<-startGate
				payload := fmt.Sprintf(`{"resourceType":"Patient","id":%q,"active":true,"name":[{"family":"Contender-%d","given":["CAS"]}],"gender":"female","birthDate":"1990-01-01"}`, patID, worker)
				st, h, bodyMap, reqErr := doReq(http.MethodPut, "/fhir/r4/"+tenantA+"/Patient/"+patID, map[string]string{
					"If-Match": `W/"1"`,
				}, payload)
				if reqErr != nil {
					otherErr.Add(1)
					return
				}
				switch st {
				case http.StatusOK:
					if h.Get("ETag") == `W/"2"` {
						wins.Add(1)
					} else {
						otherErr.Add(1)
					}
				case http.StatusPreconditionFailed:
					if bodyMap["resourceType"] == "OperationOutcome" {
						conflicts.Add(1)
					} else {
						otherErr.Add(1)
					}
				default:
					otherErr.Add(1)
				}
			}(g)
		}
		close(startGate)
		wg.Wait()

		if wins.Load() != 1 || conflicts.Load() != contenders-1 || otherErr.Load() != 0 {
			t.Fatalf("expected 1 win and %d conflicts (0 other), got wins=%d conflicts=%d other=%d",
				contenders-1, wins.Load(), conflicts.Load(), otherErr.Load())
		}

		// Phase B: 12 goroutines execute a CAS retry loop (GET current ETag -> PUT with If-Match)
		// until all 12 succeed. Final version must be 2 + 12 = 14.
		const casWorkers = 12
		var (
			casWG   sync.WaitGroup
			casGate = make(chan struct{})
			casWins atomic.Int32
		)
		for g := 0; g < casWorkers; g++ {
			casWG.Add(1)
			go func(worker int) {
				defer casWG.Done()
				<-casGate
				for attempt := 0; attempt < 64; attempt++ {
					st, getHdr, _, getErr := doReq(http.MethodGet, "/fhir/r4/"+tenantA+"/Patient/"+patID, nil, "")
					if getErr != nil || st != http.StatusOK {
						continue
					}
					etag := getHdr.Get("ETag")
					payload := fmt.Sprintf(`{"resourceType":"Patient","id":%q,"active":true,"name":[{"family":"CAS-Loop-%d","given":["Attempt-%d"]}],"gender":"female","birthDate":"1990-01-01"}`, patID, worker, attempt)
					putSt, _, _, putErr := doReq(http.MethodPut, "/fhir/r4/"+tenantA+"/Patient/"+patID, map[string]string{
						"If-Match": etag,
					}, payload)
					if putErr == nil && putSt == http.StatusOK {
						casWins.Add(1)
						return
					}
				}
			}(g)
		}
		close(casGate)
		casWG.Wait()

		if casWins.Load() != casWorkers {
			t.Fatalf("expected all %d CAS retry workers to succeed, got %d", casWorkers, casWins.Load())
		}

		// Verify final versionId == "14" and _history contains all 14 versions.
		st, finalHdr, finalPat, err := doReq(http.MethodGet, "/fhir/r4/"+tenantA+"/Patient/"+patID, nil, "")
		if err != nil || st != http.StatusOK {
			t.Fatalf("GET final Patient status=%d err=%v", st, err)
		}
		if got := finalHdr.Get("ETag"); got != `W/"14"` {
			t.Fatalf("expected final ETag W/\"14\", got %q", got)
		}
		meta, _ := finalPat["meta"].(map[string]any)
		if meta["versionId"] != "14" {
			t.Fatalf("expected meta.versionId=14, got %v", meta["versionId"])
		}

		hSt, _, histBundle, err := doReq(http.MethodGet, "/fhir/r4/"+tenantA+"/Patient/"+patID+"/_history?_count=50", nil, "")
		if err != nil || hSt != http.StatusOK {
			t.Fatalf("GET _history status=%d err=%v", hSt, err)
		}
		entries, _ := histBundle["entry"].([]any)
		if len(entries) != 14 {
			t.Fatalf("expected 14 _history entries, got %d", len(entries))
		}
	})

	// =========================================================================
	// 2. Concurrent Conditional Create (If-None-Exist) Idempotency
	// =========================================================================
	t.Run("Concurrent_Conditional_Create_IfNoneExist_Idempotency_Under_Contention", func(t *testing.T) {
		const creators = 16
		var (
			wg        sync.WaitGroup
			startGate = make(chan struct{})
			createdN  atomic.Int32
			reusedN   atomic.Int32
			otherN    atomic.Int32
			idsMu     sync.Mutex
			seenIDs   = make(map[string]bool)
		)

		for g := 0; g < creators; g++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				<-startGate
				body := fmt.Sprintf(`{
					"resourceType": "Patient",
					"active": true,
					"identifier": [{"system": "http://hospital.example.org/mrn", "value": "CONC-IFNONE-777"}],
					"name": [{"family": "IdempotentCreator", "given": ["Worker-%d"]}],
					"gender": "male",
					"birthDate": "1982-06-15"
				}`, worker)
				st, _, resMap, reqErr := doReq(http.MethodPost, "/fhir/r4/"+tenantA+"/Patient", map[string]string{
					"If-None-Exist": "identifier=http://hospital.example.org/mrn|CONC-IFNONE-777",
				}, body)
				if reqErr != nil {
					otherN.Add(1)
					return
				}
				switch st {
				case http.StatusCreated:
					createdN.Add(1)
				case http.StatusOK:
					reusedN.Add(1)
				default:
					otherN.Add(1)
					return
				}
				if id, ok := resMap["id"].(string); ok && id != "" {
					idsMu.Lock()
					seenIDs[id] = true
					idsMu.Unlock()
				}
			}(g)
		}
		close(startGate)
		wg.Wait()

		if createdN.Load() != 1 || reusedN.Load() != creators-1 || otherN.Load() != 0 {
			t.Fatalf("expected 1 Created (201) and %d OK (200), got created=%d reused=%d other=%d",
				creators-1, createdN.Load(), reusedN.Load(), otherN.Load())
		}
		if len(seenIDs) != 1 {
			t.Fatalf("expected all %d concurrent conditional creates to converge on 1 Patient ID, got %v", creators, seenIDs)
		}

		// Search by identifier must return total == 1
		sSt, _, searchBundle, err := doReq(http.MethodGet, "/fhir/r4/"+tenantA+"/Patient?identifier=http://hospital.example.org/mrn|CONC-IFNONE-777", nil, "")
		if err != nil || sSt != http.StatusOK {
			t.Fatalf("search conditional patient status=%d err=%v", sSt, err)
		}
		if total, _ := searchBundle["total"].(float64); int(total) != 1 {
			t.Fatalf("expected search total=1 for conditional create identifier, got %v", searchBundle["total"])
		}
	})

	// =========================================================================
	// 3. Concurrent Unconditional PUT, PATCH & Search Readers Monotonic History
	// =========================================================================
	t.Run("Concurrent_Unconditional_Put_Patch_And_Search_Readers_Monotonic_History", func(t *testing.T) {
		patID := "conc-rw-pat-1"
		initBody := fmt.Sprintf(`{"resourceType":"Patient","id":%q,"active":true,"name":[{"family":"ConcurrentRW","given":["Base"]}],"gender":"other","birthDate":"1975-09-20"}`, patID)
		st, _, _, err := doReq(http.MethodPut, "/fhir/r4/"+tenantA+"/Patient/"+patID, nil, initBody)
		if err != nil || (st != http.StatusCreated && st != http.StatusOK) {
			t.Fatalf("seed conc-rw-pat-1 status=%d err=%v", st, err)
		}

		const (
			putWriters   = 8
			patchWriters = 8
			readers      = 8
		)
		var (
			wg        sync.WaitGroup
			startGate = make(chan struct{})
			writeOK   atomic.Int32
			readOK    atomic.Int32
			failures  atomic.Int32
		)

		for i := 0; i < putWriters; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-startGate
				payload := fmt.Sprintf(`{"resourceType":"Patient","id":%q,"active":true,"name":[{"family":"ConcurrentRW","given":["Put-%d"]}],"gender":"other","birthDate":"1975-09-20"}`, patID, idx)
				code, _, _, reqErr := doReq(http.MethodPut, "/fhir/r4/"+tenantA+"/Patient/"+patID, nil, payload)
				if reqErr != nil || code != http.StatusOK {
					failures.Add(1)
					return
				}
				writeOK.Add(1)
			}(i)
		}

		for i := 0; i < patchWriters; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-startGate
				patchDoc := fmt.Sprintf(`[{"op":"replace","path":"/name/0/given/0","value":"Patch-%d"}]`, idx)
				code, _, _, reqErr := doReq(http.MethodPatch, "/fhir/r4/"+tenantA+"/Patient/"+patID, map[string]string{
					"Content-Type": "application/json-patch+json",
				}, patchDoc)
				if reqErr != nil || code != http.StatusOK {
					failures.Add(1)
					return
				}
				writeOK.Add(1)
			}(i)
		}

		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-startGate
				for r := 0; r < 4; r++ {
					c1, _, res1, e1 := doReq(http.MethodGet, "/fhir/r4/"+tenantA+"/Patient/"+patID, nil, "")
					c2, _, res2, e2 := doReq(http.MethodGet, "/fhir/r4/"+tenantA+"/Patient?family=ConcurrentRW", nil, "")
					if e1 != nil || e2 != nil || c1 != http.StatusOK || c2 != http.StatusOK {
						failures.Add(1)
						return
					}
					if res1["resourceType"] != "Patient" || res2["resourceType"] != "Bundle" {
						failures.Add(1)
						return
					}
				}
				readOK.Add(1)
			}()
		}

		close(startGate)
		wg.Wait()

		expectedWrites := int32(putWriters + patchWriters)
		if failures.Load() != 0 || writeOK.Load() != expectedWrites || readOK.Load() != readers {
			t.Fatalf("expected %d writes, %d readers, 0 failures; got writes=%d readers=%d failures=%d",
				expectedWrites, readers, writeOK.Load(), readOK.Load(), failures.Load())
		}

		// Final version must be 1 (seed) + 8 (PUT) + 8 (PATCH) = 17
		fSt, fHdr, _, err := doReq(http.MethodGet, "/fhir/r4/"+tenantA+"/Patient/"+patID, nil, "")
		if err != nil || fSt != http.StatusOK {
			t.Fatalf("final GET status=%d err=%v", fSt, err)
		}
		if got := fHdr.Get("ETag"); got != `W/"17"` {
			t.Fatalf("expected ETag W/\"17\" after 1+8+8 writes, got %q", got)
		}
	})

	// =========================================================================
	// 4. Concurrent Transaction Rollback Isolation & Multi-Tenant Separation
	// =========================================================================
	t.Run("Concurrent_Transaction_Rollback_Isolation_And_MultiTenant_Separation", func(t *testing.T) {
		const workers = 6
		var (
			wg        sync.WaitGroup
			startGate = make(chan struct{})
			errs      atomic.Int32
		)

		for w := 0; w < workers; w++ {
			wg.Add(3)

			// Goroutine 1: Valid transaction in tenantA
			go func(idx int) {
				defer wg.Done()
				<-startGate
				bundle := fmt.Sprintf(`{
					"resourceType": "Bundle",
					"type": "transaction",
					"entry": [
						{
							"fullUrl": "urn:uuid:11111111-2222-3333-4444-5555555500%02d",
							"resource": {
								"resourceType": "Patient",
								"active": true,
								"name": [{"family": "AlphaTxValid", "given": ["W%d"]}],
								"gender": "female",
								"birthDate": "1992-03-10"
							},
							"request": {"method": "POST", "url": "Patient"}
						}
					]
				}`, idx, idx)
				st, _, _, err := doReq(http.MethodPost, "/fhir/r4/"+tenantA, nil, bundle)
				if err != nil || st != http.StatusOK {
					errs.Add(1)
				}
			}(w)

			// Goroutine 2: Failing transaction in tenantA (entry 0 creates a Patient with family=GhostRollback, entry 1 is invalid)
			go func(idx int) {
				defer wg.Done()
				<-startGate
				bundle := fmt.Sprintf(`{
					"resourceType": "Bundle",
					"type": "transaction",
					"entry": [
						{
							"resource": {
								"resourceType": "Patient",
								"id": "ghost-tx-%d",
								"active": true,
								"name": [{"family": "GhostRollback", "given": ["NeverCommitted"]}],
								"gender": "male",
								"birthDate": "1990-01-01"
							},
							"request": {"method": "PUT", "url": "Patient/ghost-tx-%d"}
						},
						{
							"resource": {
								"resourceType": "Patient",
								"gender": "invalid-code-triggers-rollback"
							},
							"request": {"method": "POST", "url": "Patient"}
						}
					]
				}`, idx, idx)
				st, _, _, err := doReq(http.MethodPost, "/fhir/r4/"+tenantA, nil, bundle)
				if err != nil || st != http.StatusBadRequest {
					errs.Add(1)
				}
			}(w)

			// Goroutine 3: Valid transaction in tenantB
			go func(idx int) {
				defer wg.Done()
				<-startGate
				bundle := fmt.Sprintf(`{
					"resourceType": "Bundle",
					"type": "transaction",
					"entry": [
						{
							"resource": {
								"resourceType": "Patient",
								"id": "beta-only-%d",
								"active": true,
								"name": [{"family": "BetaTenantOnly", "given": ["B%d"]}],
								"gender": "male",
								"birthDate": "1988-11-11"
							},
							"request": {"method": "PUT", "url": "Patient/beta-only-%d"}
						}
					]
				}`, idx, idx, idx)
				st, _, _, err := doReq(http.MethodPost, "/fhir/r4/"+tenantB, nil, bundle)
				if err != nil || st != http.StatusOK {
					errs.Add(1)
				}
			}(w)
		}

		close(startGate)
		wg.Wait()

		if errs.Load() != 0 {
			t.Fatalf("expected 0 errors across concurrent multi-tenant transactions, got %d", errs.Load())
		}

		// Verify 0 GhostRollback resources exist in tenantA
		gSt, _, ghostBundle, err := doReq(http.MethodGet, "/fhir/r4/"+tenantA+"/Patient?family=GhostRollback", nil, "")
		if err != nil || gSt != http.StatusOK {
			t.Fatalf("search GhostRollback status=%d err=%v", gSt, err)
		}
		if total, _ := ghostBundle["total"].(float64); int(total) != 0 {
			t.Fatalf("expected 0 GhostRollback patients in tenantA, got %v", ghostBundle["total"])
		}

		// Verify tenantA has 0 BetaTenantOnly patients and tenantB has 0 AlphaTxValid patients
		bInASt, _, bInABundle, _ := doReq(http.MethodGet, "/fhir/r4/"+tenantA+"/Patient?family=BetaTenantOnly", nil, "")
		aInBSt, _, aInBBundle, _ := doReq(http.MethodGet, "/fhir/r4/"+tenantB+"/Patient?family=AlphaTxValid", nil, "")
		if bInASt != http.StatusOK || aInBSt != http.StatusOK {
			t.Fatalf("cross-tenant search failed: %d / %d", bInASt, aInBSt)
		}
		if t1, _ := bInABundle["total"].(float64); int(t1) != 0 {
			t.Fatalf("expected 0 BetaTenantOnly resources in tenantA, got %v", t1)
		}
		if t2, _ := aInBBundle["total"].(float64); int(t2) != 0 {
			t.Fatalf("expected 0 AlphaTxValid resources in tenantB, got %v", t2)
		}
	})
}
