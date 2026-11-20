package txclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"hegel.dev/go/hegel"
)

type refCacheEntry struct {
	key       string
	val       map[string]any
	expiresAt time.Time
}

type lruTTLMachine struct {
	cache       *lruCache
	maxEntries  int
	ttl         time.Duration
	now         time.Time
	ref         []refCacheEntry // index 0 = MRU, len-1 = LRU
	expectedHit uint64
	expectedMis uint64
}

func (m *lruTTLMachine) RulePut(tc hegel.TestCase) {
	key := hegel.Draw(tc, hegel.SampledFrom([]string{
		"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7",
	}))
	code := hegel.Draw(tc, hegel.FromRegex(`[A-Z0-9]{2,8}`, true))
	display := hegel.Draw(tc, hegel.FromRegex(`[A-Za-z ]{3,12}`, true))

	callerMap := map[string]any{
		"resourceType": "Parameters",
		"code":         code,
		"nested": map[string]any{
			"display": display,
			"tags":    []any{"a", "b"},
		},
	}
	snapshotExpected := cloneMap(callerMap)

	m.cache.put(key, callerMap)

	// Mutate callerMap immediately after put to verify defensive deep-cloning on write.
	callerMap["code"] = "CORRUPTED_CALLER_MUTATION"
	callerMap["nested"].(map[string]any)["display"] = "CORRUPTED"
	callerMap["nested"].(map[string]any)["tags"].([]any)[0] = "CORRUPTED"

	exp := m.now.Add(m.ttl)
	for i, item := range m.ref {
		if item.key == key {
			m.ref = append(m.ref[:i], m.ref[i+1:]...)
			break
		}
	}
	m.ref = append([]refCacheEntry{{
		key:       key,
		val:       snapshotExpected,
		expiresAt: exp,
	}}, m.ref...)
	if len(m.ref) > m.maxEntries {
		m.ref = m.ref[:m.maxEntries]
	}
}

func (m *lruTTLMachine) RuleGet(tc hegel.TestCase) {
	key := hegel.Draw(tc, hegel.SampledFrom([]string{
		"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k-missing",
	}))

	got, ok := m.cache.get(key)

	refIdx := -1
	for i, item := range m.ref {
		if item.key == key {
			refIdx = i
			break
		}
	}

	if refIdx == -1 {
		m.expectedMis++
		if ok || got != nil {
			tc.Errorf("get(%q) on absent key: got ok=%v val=%v, want miss", key, ok, got)
		}
		return
	}

	item := m.ref[refIdx]
	if !m.now.Before(item.expiresAt) {
		// Lazy TTL expiration removes entry on access and records a miss.
		m.ref = append(m.ref[:refIdx], m.ref[refIdx+1:]...)
		m.expectedMis++
		if ok || got != nil {
			tc.Errorf("get(%q) on expired entry (now=%v, exp=%v): got hit %v, want TTL miss", key, m.now, item.expiresAt, got)
		}
		return
	}

	// Hit promotes entry to MRU front without resetting its TTL.
	m.expectedHit++
	if !ok {
		tc.Errorf("get(%q) on live entry: got miss, want hit %v", key, item.val)
	}
	if !reflect.DeepEqual(got, item.val) {
		tc.Errorf("get(%q) payload mismatch:\ngot:  %#v\nwant: %#v", key, got, item.val)
	}

	// Mutate returned map to verify defensive deep-cloning on read.
	got["code"] = "MUTATED_BY_READER"
	if nested, isMap := got["nested"].(map[string]any); isMap {
		nested["display"] = "MUTATED_BY_READER"
	}

	m.ref = append(m.ref[:refIdx], m.ref[refIdx+1:]...)
	m.ref = append([]refCacheEntry{item}, m.ref...)
}

func (m *lruTTLMachine) RuleAdvanceClock(tc hegel.TestCase) {
	deltaSec := hegel.Draw(tc, hegel.Integers(0, 8))
	m.now = m.now.Add(time.Duration(deltaSec) * time.Second)
}

func (m *lruTTLMachine) RulePurge(tc hegel.TestCase) {
	shouldPurge := hegel.Draw(tc, hegel.WeightedBooleans(0.1))
	if !shouldPurge {
		return
	}
	m.cache.purge()
	m.ref = nil
}

func (m *lruTTLMachine) InvariantBoundsAndConsistency(tc hegel.TestCase) {
	n := m.cache.len()
	if n > m.maxEntries {
		tc.Errorf("cache len %d exceeds maxEntries %d", n, m.maxEntries)
	}
	if n != len(m.ref) {
		tc.Errorf("cache len %d != reference model len %d", n, len(m.ref))
	}

	m.cache.mu.Lock()
	mapLen := len(m.cache.items)
	listLen := m.cache.ll.Len()
	idx := 0
	for e := m.cache.ll.Front(); e != nil; e = e.Next() {
		ce := e.Value.(*cacheEntry)
		if m.cache.items[ce.key] != e {
			m.cache.mu.Unlock()
			tc.Errorf("linked list element %q out of sync with items index", ce.key)
		}
		if idx < len(m.ref) && ce.key != m.ref[idx].key {
			m.cache.mu.Unlock()
			tc.Errorf("MRU->LRU order mismatch at pos %d: cache=%q want=%q", idx, ce.key, m.ref[idx].key)
		}
		idx++
	}
	m.cache.mu.Unlock()

	if mapLen != listLen {
		tc.Errorf("items map len (%d) != linked list len (%d)", mapLen, listLen)
	}
	if hits := m.cache.hits.Load(); hits != m.expectedHit {
		tc.Errorf("cache hits=%d != expected=%d", hits, m.expectedHit)
	}
	if misses := m.cache.misses.Load(); misses != m.expectedMis {
		tc.Errorf("cache misses=%d != expected=%d", misses, m.expectedMis)
	}
}

// TestProperty_TxClientLRUTTLStateMachine verifies bounded LRU capacity, MRU promotion on get/put,
// lazy TTL expiry under a deterministic clock, defensive deep-cloning, and internal map/list invariants.
func TestProperty_TxClientLRUTTLStateMachine(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		maxEntries := hegel.Draw(ht, hegel.Integers(2, 6))
		ttlSec := hegel.Draw(ht, hegel.Integers(3, 12))
		ttl := time.Duration(ttlSec) * time.Second

		m := &lruTTLMachine{
			maxEntries: maxEntries,
			ttl:        ttl,
			now:        time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		}
		m.cache = newLRUCache(maxEntries, ttl, func() time.Time {
			return m.now
		})

		hegel.RunStateful(
			ht,
			m,
			hegel.WithStatefulStepCount(35),
			hegel.WithAlwaysCheckInvariants("InvariantBoundsAndConsistency"),
		)
	}, hegel.WithTestCases(100))
}

// TestProperty_TxClientCacheKeyDeterminismAndCollisionFreedom verifies that GET query encoding
// and POST Parameters SHA-256 cache keys are deterministic, canonical across `url|version` splitting,
// and collision-free across distinct terminology coordinates.
func TestProperty_TxClientCacheKeyDeterminismAndCollisionFreedom(t *testing.T) {
	ops := []string{"ValueSet/$expand", "ValueSet/$validate-code", "CodeSystem/$lookup", "CodeSystem/$validate-code"}

	computeKeys := func(op string, params map[string]string) (string, string) {
		var allowed []string
		var boolKeys, intKeys map[string]bool
		resParamName := "valueSet"
		switch op {
		case "ValueSet/$expand":
			allowed = expandAllowedParams
			boolKeys = expandBooleanParams
			intKeys = expandIntegerParams
		case "ValueSet/$validate-code":
			allowed = validateVSAllowedParams
			boolKeys = validateBooleanParams
		case "CodeSystem/$lookup":
			allowed = lookupAllowedParams
			resParamName = "codeSystem"
		case "CodeSystem/$validate-code":
			allowed = validateCSAllowedParams
			boolKeys = validateBooleanParams
			resParamName = "codeSystem"
		}
		q := pickQueryParams(params, allowed)
		getKey := "GET:https://tx.example.org/r4/" + op
		if enc := q.Encode(); enc != "" {
			getKey += "?" + enc
		}
		body := buildParametersResource(q, resParamName, map[string]any{"resourceType": "ValueSet", "status": "active"}, boolKeys, intKeys)
		raw, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(raw)
		postKey := "POST:https://tx.example.org/r4/" + op + ":" + hex.EncodeToString(sum[:])
		return getKey, postKey
	}

	hegel.Test(t, func(ht *hegel.T) {
		opA := hegel.Draw(ht, hegel.SampledFrom(ops))
		opB := hegel.Draw(ht, hegel.SampledFrom(ops))

		urlSlugA := hegel.Draw(ht, hegel.FromRegex(`[a-z]{3,10}`, true))
		urlSlugB := hegel.Draw(ht, hegel.FromRegex(`[a-z]{3,10}`, true))
		verA := hegel.Draw(ht, hegel.FromRegex(`[1-9]\.[0-9]\.[0-9]`, true))
		verB := hegel.Draw(ht, hegel.FromRegex(`[1-9]\.[0-9]\.[0-9]`, true))
		codeA := hegel.Draw(ht, hegel.FromRegex(`[0-9]{4,7}`, true))
		codeB := hegel.Draw(ht, hegel.FromRegex(`[0-9]{4,7}`, true))

		baseURLA := "http://hl7.org/fhir/ValueSet/" + urlSlugA
		baseURLB := "http://hl7.org/fhir/ValueSet/" + urlSlugB

		// 1. Canonical url|version splitting equivalence for ValueSet/$expand:
		pipeMap := map[string]string{
			"url":        "  " + baseURLA + "|" + verA + "  ",
			"activeOnly": "true",
			"count":      "25",
			"ignoredKey": "noise-1",
		}
		splitMap := map[string]string{
			"count":           "25",
			"valueSetVersion": verA,
			"activeOnly":      "true",
			"url":             baseURLA,
			"ignoredKey":      "noise-2",
		}
		getPipe, postPipe := computeKeys("ValueSet/$expand", pipeMap)
		getSplit, postSplit := computeKeys("ValueSet/$expand", splitMap)
		if getPipe != getSplit {
			ht.Fatalf("GET canonical url|version mismatch:\npipe:  %s\nsplit: %s", getPipe, getSplit)
		}
		if postPipe != postSplit {
			ht.Fatalf("POST canonical url|version mismatch:\npipe:  %s\nsplit: %s", postPipe, postSplit)
		}

		// 2. Collision freedom across distinct (op, url, code) for ValueSet/$validate-code vs CodeSystem/$validate-code:
		if (opA == "ValueSet/$validate-code" || opA == "CodeSystem/$validate-code") &&
			(opB == "ValueSet/$validate-code" || opB == "CodeSystem/$validate-code") {
			pA := map[string]string{"url": baseURLA, "code": codeA}
			pB := map[string]string{"url": baseURLB, "code": codeB}
			gA, kA := computeKeys(opA, pA)
			gB, kB := computeKeys(opB, pB)
			sameTuple := opA == opB && urlSlugA == urlSlugB && codeA == codeB
			if sameTuple {
				if gA != gB || kA != kB {
					ht.Fatalf("identical tuple produced different keys: (%s, %s) vs (%s, %s)", gA, kA, gB, kB)
				}
			} else {
				if gA == gB {
					ht.Fatalf("GET key collision for distinct tuples (%s,%s,%s) vs (%s,%s,%s): %s",
						opA, urlSlugA, codeA, opB, urlSlugB, codeB, gA)
				}
				if kA == kB {
					ht.Fatalf("POST key collision for distinct tuples (%s,%s,%s) vs (%s,%s,%s): %s",
						opA, urlSlugA, codeA, opB, urlSlugB, codeB, kA)
				}
			}
		}

		// 3. Verify verA vs verB collision freedom on ValueSet/$expand:
		if verA != verB {
			g1, p1 := computeKeys("ValueSet/$expand", map[string]string{"url": baseURLA + "|" + verA})
			g2, p2 := computeKeys("ValueSet/$expand", map[string]string{"url": baseURLA + "|" + verB})
			if g1 == g2 || p1 == p2 {
				ht.Fatalf("version collision for %s vs %s", verA, verB)
			}
		}
	}, hegel.WithTestCases(200))
}

// TestProperty_UpstreamErrorClassificationPartition verifies that UpstreamError status predicates
// partition HTTP status codes strictly (IsClientError and IsTransient are mutually exclusive,
// 429 and >=500 and 0 are transient, and 404 is both IsNotFound and IsClientError).
func TestProperty_UpstreamErrorClassificationPartition(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		code := hegel.Draw(ht, hegel.Integers(0, 599))
		msg := hegel.Draw(ht, hegel.FromRegex(`[A-Za-z0-9 _:-]{0,30}`, true))

		ue := &UpstreamError{
			StatusCode: code,
			Message:    msg,
		}

		if ue.IsClientError() && ue.IsTransient() {
			ht.Fatalf("status %d classified as both IsClientError and IsTransient", code)
		}

		wantNotFound := code == 404
		wantTransient := code == 0 || code == 429 || code >= 500
		wantClient := code >= 400 && code < 500 && code != 429

		if ue.IsNotFound() != wantNotFound {
			ht.Fatalf("status %d IsNotFound=%v, want %v", code, ue.IsNotFound(), wantNotFound)
		}
		if ue.IsTransient() != wantTransient {
			ht.Fatalf("status %d IsTransient=%v, want %v", code, ue.IsTransient(), wantTransient)
		}
		if ue.IsClientError() != wantClient {
			ht.Fatalf("status %d IsClientError=%v, want %v", code, ue.IsClientError(), wantClient)
		}

		errStr := ue.Error()
		if code > 0 && !strings.Contains(errStr, fmt.Sprintf("%d", code)) {
			ht.Fatalf("UpstreamError.Error() %q missing status code %d", errStr, code)
		}
	}, hegel.WithTestCases(300))
}
