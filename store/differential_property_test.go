package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"hegel.dev/go/hegel"
	_ "modernc.org/sqlite"

	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

// ucumDimensionsByName enumerates every UCUM dimension and unit code supported by Flint's
// cross-backend search evaluator (store/search_eval.go) and PostgreSQL SQL pushdown (store/postgres/search.go).
var ucumDimensionsByName = [][]string{
	{"Cel", "[degF]", "K"},
	{"kg", "g", "mg", "ug", "ng", "pg", "[lb_av]", "lbs", "[oz_av]"},
	{"m", "cm", "mm", "um", "nm", "km", "[in_i]", "[ft_i]", "[yd_i]", "[mi_i]"},
	{"L", "l", "dL", "dl", "cL", "cl", "mL", "ml", "uL", "ul", "cm3"},
	{"s", "ms", "min", "h", "d", "wk", "mo", "a", "yr"},
	{"Pa", "kPa", "mm[Hg]", "mmHg"},
	{"g/L", "g/l", "g/dL", "g/dl", "mg/dL", "mg/dl", "mg/L", "mg/l", "ug/mL", "ng/mL"},
	{"mol", "mmol", "umol", "nmol", "pmol"},
	{"mol/L", "mmol/L", "umol/L", "nmol/L", "pmol/L"},
}

// TestProperty_UCUMAlgebraicLaws verifies reflexivity, symmetry (invertibility), transitivity,
// and dimension disjointness across all linear and affine (Cel/[degF]/K) UCUM dimensions.
func TestProperty_UCUMAlgebraicLaws(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		dimIdx := hegel.Draw(ht, hegel.Integers(0, len(ucumDimensionsByName)-1))
		otherOffset := hegel.Draw(ht, hegel.Integers(1, len(ucumDimensionsByName)-1))
		otherDimIdx := (dimIdx + otherOffset) % len(ucumDimensionsByName)

		dim := ucumDimensionsByName[dimIdx]
		otherDim := ucumDimensionsByName[otherDimIdx]

		u1 := hegel.Draw(ht, hegel.SampledFrom(dim))
		u2 := hegel.Draw(ht, hegel.SampledFrom(dim))
		u3 := hegel.Draw(ht, hegel.SampledFrom(dim))
		uDisjoint := hegel.Draw(ht, hegel.SampledFrom(otherDim))

		val := hegel.Draw(ht, hegel.Floats[float64]().Min(-40.0).Max(5000.0).AllowNaN(false).AllowInfinity(false))

		// 1. Reflexivity: converting any unit to itself is exact identity.
		selfVal, okSelf := store.ConvertUCUMValue(val, u1, u1)
		if !okSelf || selfVal != val {
			ht.Fatalf("reflexivity violated for (%v, %q -> %q): got (%v, %v)", val, u1, u1, selfVal, okSelf)
		}

		// 2. Symmetry / Invertibility: round-tripping u1 -> u2 -> u1 recovers val within float64 precision.
		v12, ok12 := store.ConvertUCUMValue(val, u1, u2)
		if !ok12 {
			ht.Fatalf("intra-dimension conversion failed for (%v, %q -> %q)", val, u1, u2)
		}
		v21, ok21 := store.ConvertUCUMValue(v12, u2, u1)
		if !ok21 {
			ht.Fatalf("inverse conversion failed for (%v, %q -> %q)", v12, u2, u1)
		}
		tol := 1e-6 * math.Max(1.0, math.Abs(val))
		if math.Abs(v21-val) > tol {
			ht.Fatalf("invertibility drift for %v (%q -> %q -> %q): got %v (diff=%g > tol=%g)", val, u1, u2, u1, v21, math.Abs(v21-val), tol)
		}

		// 3. Transitivity: (u1 -> u2 -> u3) agrees with direct (u1 -> u3).
		v123, ok23 := store.ConvertUCUMValue(v12, u2, u3)
		v13, ok13 := store.ConvertUCUMValue(val, u1, u3)
		if !ok23 || !ok13 {
			ht.Fatalf("transitivity conversion failed across (%q, %q, %q)", u1, u2, u3)
		}
		tol13 := 1e-6 * math.Max(1.0, math.Abs(v13))
		if math.Abs(v123-v13) > tol13 {
			ht.Fatalf("transitivity drift across (%q -> %q -> %q): chained=%v direct=%v", u1, u2, u3, v123, v13)
		}

		// 4. Dimension disjointness: incompatible dimensions never convert.
		if _, okBad := store.ConvertUCUMValue(val, u1, uDisjoint); okBad {
			ht.Fatalf("expected disjoint UCUM conversion (%q -> %q) to return ok=false", u1, uDisjoint)
		}
	}, hegel.WithTestCases(200))
}

// TestProperty_FilterBooleanAlgebraLaws verifies that Flint's FHIR R4 _filter parser and
// evaluator obey classical Boolean algebra laws (double negation, De Morgan's laws,
// commutativity, associativity, distributivity, and idempotence) across arbitrary indexed resources.
func TestProperty_FilterBooleanAlgebraLaws(t *testing.T) {
	noopLookup := func(_, _, _ string) (store.IndexedResourceView, bool) {
		return store.IndexedResourceView{}, false
	}
	noopList := func(_ string) []store.IndexedResourceView { return nil }

	atomicFilters := []string{
		`gender eq "female"`,
		`gender ne "male"`,
		`family co "lov"`,
		`family sw "Ada"`,
		`birthdate ge 1990-01-01`,
		`birthdate le 2005-12-31`,
		`value-quantity gt 50|http://unitsofmeasure.org|kg`,
		`identifier eq "http://hospital.example.org/mrn|MRN-100"`,
	}

	hegel.Test(t, func(ht *hegel.T) {
		gender := hegel.Draw(ht, hegel.SampledFrom([]string{"female", "male", "other", "unknown"}))
		family := hegel.Draw(ht, hegel.SampledFrom([]string{"Lovelace", "Adalov", "Turing", "Hopper"}))
		birthYear := hegel.Draw(ht, hegel.Integers(1975, 2020))
		weightGrams := float64(hegel.Draw(ht, hegel.Integers(30_000, 95_000)))
		mrn := hegel.Draw(ht, hegel.SampledFrom([]string{"MRN-100", "MRN-200", "MRN-300"}))

		bd := time.Date(birthYear, 6, 15, 0, 0, 0, 0, time.UTC)
		view := store.IndexedResourceView{
			TenantID:   "pbt-filter",
			ResType:    "Patient",
			ResID:      "pat-1",
			ResVersion: 1,
			Indexes: &store.SearchIndexes{
				Strings: []store.SpidxString{
					{TenantID: "pbt-filter", ResType: "Patient", ResID: "pat-1", SpName: "family", SpValue: family},
				},
				Tokens: []store.SpidxToken{
					{TenantID: "pbt-filter", ResType: "Patient", ResID: "pat-1", SpName: "gender", SpValue: gender},
					{TenantID: "pbt-filter", ResType: "Patient", ResID: "pat-1", SpName: "identifier", SpSystem: "http://hospital.example.org/mrn", SpValue: mrn},
				},
				Dates: []store.SpidxDate{
					{TenantID: "pbt-filter", ResType: "Patient", ResID: "pat-1", SpName: "birthdate", SpLow: bd, SpHigh: bd},
				},
				Quantities: []store.SpidxQuantity{
					{TenantID: "pbt-filter", ResType: "Patient", ResID: "pat-1", SpName: "value-quantity", SpSystem: "http://unitsofmeasure.org", SpCode: "g", SpValue: weightGrams},
				},
			},
		}

		aStr := hegel.Draw(ht, hegel.SampledFrom(atomicFilters))
		bStr := hegel.Draw(ht, hegel.SampledFrom(atomicFilters))
		cStr := hegel.Draw(ht, hegel.SampledFrom(atomicFilters))

		evalRaw := func(raw string) bool {
			ast, err := store.ParseFilterExpr(raw)
			if err != nil {
				ht.Fatalf("ParseFilterExpr(%q) failed: %v", raw, err)
			}
			return store.MatchFilterExpr(view, ast, noopLookup, noopList)
		}

		va := evalRaw(aStr)
		vb := evalRaw(bStr)
		vc := evalRaw(cStr)

		// Double negation: not (not A) == A
		if evalRaw(fmt.Sprintf("not (not (%s))", aStr)) != va {
			ht.Fatalf("double negation failed for A=%q", aStr)
		}
		// Idempotence: (A and A) == A, (A or A) == A
		if evalRaw(fmt.Sprintf("(%s) and (%s)", aStr, aStr)) != va {
			ht.Fatalf("AND idempotence failed for A=%q", aStr)
		}
		if evalRaw(fmt.Sprintf("(%s) or (%s)", aStr, aStr)) != va {
			ht.Fatalf("OR idempotence failed for A=%q", aStr)
		}
		// Commutativity: (A and B) == (B and A), (A or B) == (B or A)
		if evalRaw(fmt.Sprintf("(%s) and (%s)", aStr, bStr)) != (va && vb) ||
			evalRaw(fmt.Sprintf("(%s) and (%s)", bStr, aStr)) != (va && vb) {
			ht.Fatalf("AND commutativity failed for A=%q B=%q", aStr, bStr)
		}
		if evalRaw(fmt.Sprintf("(%s) or (%s)", aStr, bStr)) != (va || vb) ||
			evalRaw(fmt.Sprintf("(%s) or (%s)", bStr, aStr)) != (va || vb) {
			ht.Fatalf("OR commutativity failed for A=%q B=%q", aStr, bStr)
		}
		// De Morgan's laws:
		// not ((A) and (B)) == (not (A)) or (not (B))
		// not ((A) or (B))  == (not (A)) and (not (B))
		if evalRaw(fmt.Sprintf("not ((%s) and (%s))", aStr, bStr)) != evalRaw(fmt.Sprintf("(not (%s)) or (not (%s))", aStr, bStr)) {
			ht.Fatalf("De Morgan AND failed for A=%q B=%q", aStr, bStr)
		}
		if evalRaw(fmt.Sprintf("not ((%s) or (%s))", aStr, bStr)) != evalRaw(fmt.Sprintf("(not (%s)) and (not (%s))", aStr, bStr)) {
			ht.Fatalf("De Morgan OR failed for A=%q B=%q", aStr, bStr)
		}
		// Distributivity: A and (B or C) == (A and B) or (A and C)
		lhsDist := evalRaw(fmt.Sprintf("(%s) and ((%s) or (%s))", aStr, bStr, cStr))
		rhsDist := evalRaw(fmt.Sprintf("((%s) and (%s)) or ((%s) and (%s))", aStr, bStr, aStr, cStr))
		if lhsDist != rhsDist || lhsDist != (va && (vb || vc)) {
			ht.Fatalf("distributivity failed for A=%q B=%q C=%q", aStr, bStr, cStr)
		}
	}, hegel.WithTestCases(150))
}

type modelResourceVersion struct {
	version   int
	deleted   bool
	protoData []byte
	family    string
	gender    string
	mrn       string
	birthYear int
	weightKg  float64
}

type namedStoreTarget struct {
	name  string
	store store.Store
}

// storeDifferentialMachine implements a Hegel model-based state machine (hegel.RunStateful)
// executing randomized CRUD, CAS, ConditionalCreate, Delete, TransactionBundle, VRead/History,
// and Search/Filter operations across SQLite WAL, Doris, HybridStore (SQLite OLTP + Doris OLAP),
// and live PostgreSQL (when FLINT_TEST_POSTGRES_DSN is configured).
type storeDifferentialMachine struct {
	ctx        context.Context
	tenant     string
	targets    []namedStoreTarget
	hybrid     *store.HybridStore
	sqliteDB   *sql.DB
	model      map[string][]modelResourceVersion // resID -> ordered versions 1..N
	touchedIDs map[string]bool
}

func classifyStoreErr(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, sql.ErrNoRows):
		return "not_found"
	case errors.Is(err, store.ErrResourceDeleted):
		return "deleted"
	case errors.Is(err, store.ErrVersionConflict):
		return "version_conflict"
	case errors.Is(err, store.ErrResourceAlreadyExists):
		return "already_exists"
	case errors.Is(err, store.ErrPreconditionFailed):
		return "precondition_failed"
	default:
		return "other:" + err.Error()
	}
}

func buildPatientStateInput(tenant, resID, family, gender, mrn string, birthYear int, weightKg float64, expectedVer int, createOnly bool) store.ResourceInput {
	bd := time.Date(birthYear, 5, 10, 0, 0, 0, 0, time.UTC)
	payload := []byte(fmt.Sprintf("proto:%s|fam=%s|gen=%s|mrn=%s|by=%d|wt=%.1f", resID, family, gender, mrn, birthYear, weightKg))
	return store.ResourceInput{
		TenantID:        tenant,
		ResType:         "Patient",
		ResID:           resID,
		ResourceProto:   payload,
		ExpectedVersion: expectedVer,
		CreateOnly:      createOnly,
		SearchIndexes: &store.SearchIndexes{
			Strings: []store.SpidxString{
				{TenantID: tenant, ResType: "Patient", ResID: resID, SpName: "family", SpValue: family},
			},
			Tokens: []store.SpidxToken{
				{TenantID: tenant, ResType: "Patient", ResID: resID, SpName: "gender", SpValue: gender},
				{TenantID: tenant, ResType: "Patient", ResID: resID, SpName: "identifier", SpSystem: "http://hospital.example.org/mrn", SpValue: mrn},
			},
			Dates: []store.SpidxDate{
				{TenantID: tenant, ResType: "Patient", ResID: resID, SpName: "birthdate", SpLow: bd, SpHigh: bd},
			},
			Quantities: []store.SpidxQuantity{
				{TenantID: tenant, ResType: "Patient", ResID: resID, SpName: "value-quantity", SpSystem: "http://unitsofmeasure.org", SpCode: "kg", SpValue: weightKg},
			},
		},
	}
}

func (m *storeDifferentialMachine) currentModelState(resID string) (curVer int, exists bool, deleted bool) {
	vers := m.model[resID]
	if len(vers) == 0 {
		return 0, false, false
	}
	last := vers[len(vers)-1]
	return last.version, true, last.deleted
}

func (m *storeDifferentialMachine) RuleWriteWithCASOrCreateOnly(tc hegel.TestCase) {
	resID := hegel.Draw(tc, hegel.SampledFrom([]string{"pat-1", "pat-2", "pat-3", "pat-4", "pat-5"}))
	family := hegel.Draw(tc, hegel.SampledFrom([]string{"Ada", "Babbage", "Curie", "Darwin"}))
	gender := hegel.Draw(tc, hegel.SampledFrom([]string{"female", "male", "other"}))
	mrn := hegel.Draw(tc, hegel.SampledFrom([]string{"MRN-1", "MRN-2", "MRN-3", "MRN-4"}))
	birthYear := hegel.Draw(tc, hegel.SampledFrom([]int{1980, 1990, 2000, 2010}))
	weightKg := float64(hegel.Draw(tc, hegel.SampledFrom([]int{50, 65, 80, 95})))

	curVer, _, _ := m.currentModelState(resID)
	mode := hegel.Draw(tc, hegel.SampledFrom([]string{"unconditional", "cas_exact", "cas_stale", "create_only"}))
	expectedVer := 0
	createOnly := false
	switch mode {
	case "cas_exact":
		if curVer > 0 {
			expectedVer = curVer
		} else {
			expectedVer = 1
		}
	case "cas_stale":
		expectedVer = curVer + 2
	case "create_only":
		createOnly = true
	}

	in := buildPatientStateInput(m.tenant, resID, family, gender, mrn, birthYear, weightKg, expectedVer, createOnly)
	m.touchedIDs[resID] = true

	var refClass string
	var refVer int
	var refCreated bool
	for i, tgt := range m.targets {
		rec, err := tgt.store.WriteResourceWithMeta(m.ctx, in)
		cls := classifyStoreErr(err)
		if i == 0 {
			refClass = cls
			if err == nil && rec != nil {
				refVer = rec.ResVersion
				refCreated = rec.Created
			}
			continue
		}
		if cls != refClass {
			tc.Errorf("WriteResourceWithMeta(%s, mode=%s) error mismatch: %s=%s vs %s=%s", resID, mode, m.targets[0].name, refClass, tgt.name, cls)
		}
		if err == nil && rec != nil {
			if rec.ResVersion != refVer || rec.Created != refCreated {
				tc.Errorf("WriteResourceWithMeta(%s) meta mismatch: %s=(v%d,created=%v) vs %s=(v%d,created=%v)",
					resID, m.targets[0].name, refVer, refCreated, tgt.name, rec.ResVersion, rec.Created)
			}
		}
	}

	if refClass == "ok" {
		m.model[resID] = append(m.model[resID], modelResourceVersion{
			version:   refVer,
			deleted:   false,
			protoData: append([]byte(nil), in.ResourceProto...),
			family:    family,
			gender:    gender,
			mrn:       mrn,
			birthYear: birthYear,
			weightKg:  weightKg,
		})
	}
}

func (m *storeDifferentialMachine) RuleConditionalCreate(tc hegel.TestCase) {
	resID := hegel.Draw(tc, hegel.SampledFrom([]string{"pat-cc-1", "pat-cc-2", "pat-cc-3"}))
	mrn := hegel.Draw(tc, hegel.SampledFrom([]string{"MRN-1", "MRN-2", "MRN-CC-A", "MRN-CC-B"}))
	family := hegel.Draw(tc, hegel.SampledFrom([]string{"Ada", "Curie"}))
	gender := hegel.Draw(tc, hegel.SampledFrom([]string{"female", "male"}))

	in := buildPatientStateInput(m.tenant, resID, family, gender, mrn, 1995, 70.0, 0, false)
	cond := store.SearchParams{
		TenantID: m.tenant,
		ResType:  "Patient",
		Tokens:   map[string]string{"identifier": "http://hospital.example.org/mrn|" + mrn},
	}

	var refClass, refResID string
	var refVer int
	var refCreated bool
	for i, tgt := range m.targets {
		rec, created, err := tgt.store.ConditionalCreateResource(m.ctx, in, cond)
		cls := classifyStoreErr(err)
		if i == 0 {
			refClass = cls
			refCreated = created
			if err == nil && rec != nil {
				refVer = rec.ResVersion
				refResID = rec.ResID
			}
			continue
		}
		if cls != refClass || created != refCreated {
			tc.Errorf("ConditionalCreateResource(%s, mrn=%s) mismatch: %s=(%s,created=%v) vs %s=(%s,created=%v)",
				resID, mrn, m.targets[0].name, refClass, refCreated, tgt.name, cls, created)
		}
		if err == nil && rec != nil && (rec.ResID != refResID || rec.ResVersion != refVer) {
			tc.Errorf("ConditionalCreateResource(%s) record mismatch: %s=(%s@v%d) vs %s=(%s@v%d)",
				resID, m.targets[0].name, refResID, refVer, tgt.name, rec.ResID, rec.ResVersion)
		}
	}

	if refClass == "ok" && refCreated {
		m.touchedIDs[refResID] = true
		m.model[refResID] = append(m.model[refResID], modelResourceVersion{
			version:   refVer,
			deleted:   false,
			protoData: append([]byte(nil), in.ResourceProto...),
			family:    family,
			gender:    gender,
			mrn:       mrn,
			birthYear: 1995,
			weightKg:  70.0,
		})
	}
}

func (m *storeDifferentialMachine) RuleDeleteResource(tc hegel.TestCase) {
	resID := hegel.Draw(tc, hegel.SampledFrom([]string{"pat-1", "pat-2", "pat-3", "pat-4", "pat-5"}))
	useCAS := hegel.Draw(tc, hegel.Booleans())
	curVer, _, _ := m.currentModelState(resID)
	expectedVer := 0
	if useCAS {
		if hegel.Draw(tc, hegel.Booleans()) && curVer > 0 {
			expectedVer = curVer
		} else {
			expectedVer = curVer + 3
		}
	}

	m.touchedIDs[resID] = true

	var refClass string
	var refNilRec bool
	var refVer int
	for i, tgt := range m.targets {
		var rec *store.ResourceRecord
		var err error
		if expectedVer > 0 {
			rec, err = tgt.store.DeleteResourceWithExpectedVersion(m.ctx, m.tenant, "Patient", resID, expectedVer)
		} else {
			rec, err = tgt.store.DeleteResourceWithMeta(m.ctx, m.tenant, "Patient", resID)
		}
		cls := classifyStoreErr(err)
		if i == 0 {
			refClass = cls
			refNilRec = (rec == nil)
			if rec != nil {
				refVer = rec.ResVersion
			}
			continue
		}
		if cls != refClass || (rec == nil) != refNilRec {
			tc.Errorf("Delete(%s, expVer=%d) mismatch: %s=(%s,nil=%v) vs %s=(%s,nil=%v)",
				resID, expectedVer, m.targets[0].name, refClass, refNilRec, tgt.name, cls, rec == nil)
		}
		if err == nil && rec != nil && rec.ResVersion != refVer {
			tc.Errorf("Delete(%s) version mismatch: %s=v%d vs %s=v%d", resID, m.targets[0].name, refVer, tgt.name, rec.ResVersion)
		}
	}

	if refClass == "ok" && !refNilRec {
		// FHIR soft-delete appends a tombstone version only when transitioning active -> deleted;
		// deleting an already-deleted resource returns the existing tombstone idempotently.
		if refVer > curVer {
			m.model[resID] = append(m.model[resID], modelResourceVersion{
				version: refVer,
				deleted: true,
			})
		}
	}
}

func (m *storeDifferentialMachine) RuleTransactionBundle(tc hegel.TestCase) {
	idA := hegel.Draw(tc, hegel.SampledFrom([]string{"pat-1", "pat-2", "pat-3"}))
	idB := hegel.Draw(tc, hegel.SampledFrom([]string{"pat-4", "pat-5"}))
	famA := hegel.Draw(tc, hegel.SampledFrom([]string{"Ada", "Darwin"}))
	famB := hegel.Draw(tc, hegel.SampledFrom([]string{"Babbage", "Curie"}))
	injectFailure := hegel.Draw(tc, hegel.Booleans())

	inA := buildPatientStateInput(m.tenant, idA, famA, "female", "MRN-TX-A", 1988, 60.0, 0, false)
	curVerB, _, _ := m.currentModelState(idB)
	expVerB := 0
	if injectFailure {
		expVerB = curVerB + 99 // guaranteed optimistic CAS conflict -> must rollback op[0] on idA!
	}
	inB := buildPatientStateInput(m.tenant, idB, famB, "male", "MRN-TX-B", 1992, 82.0, expVerB, false)

	ops := []store.BundleOperation{
		{OpType: store.BundleOpWrite, Method: "PUT", Input: inA},
		{OpType: store.BundleOpWrite, Method: "PUT", Input: inB},
	}

	m.touchedIDs[idA] = true
	m.touchedIDs[idB] = true

	var refClass string
	var refRes []store.BundleOpResult
	for i, tgt := range m.targets {
		res, err := tgt.store.ExecuteTransactionBundle(m.ctx, ops)
		cls := classifyStoreErr(err)
		if i == 0 {
			refClass = cls
			refRes = res
			continue
		}
		if cls != refClass {
			tc.Errorf("ExecuteTransactionBundle(injectFailure=%v) error mismatch: %s=%s vs %s=%s",
				injectFailure, m.targets[0].name, refClass, tgt.name, cls)
		}
		if err == nil && len(res) == len(refRes) {
			for k := range res {
				if res[k].ResID != refRes[k].ResID || res[k].ResVersion != refRes[k].ResVersion {
					tc.Errorf("ExecuteTransactionBundle op[%d] mismatch: %s=(%s@v%d) vs %s=(%s@v%d)",
						k, m.targets[0].name, refRes[k].ResID, refRes[k].ResVersion, tgt.name, res[k].ResID, res[k].ResVersion)
				}
			}
		}
	}

	if injectFailure && refClass != "version_conflict" {
		tc.Errorf("expected injected CAS failure in ExecuteTransactionBundle to yield version_conflict, got %s", refClass)
	}
	if refClass == "ok" && len(refRes) == 2 {
		m.model[idA] = append(m.model[idA], modelResourceVersion{
			version:   refRes[0].ResVersion,
			deleted:   false,
			protoData: append([]byte(nil), inA.ResourceProto...),
			family:    famA,
			gender:    "female",
			mrn:       "MRN-TX-A",
			birthYear: 1988,
			weightKg:  60.0,
		})
		m.model[idB] = append(m.model[idB], modelResourceVersion{
			version:   refRes[1].ResVersion,
			deleted:   false,
			protoData: append([]byte(nil), inB.ResourceProto...),
			family:    famB,
			gender:    "male",
			mrn:       "MRN-TX-B",
			birthYear: 1992,
			weightKg:  82.0,
		})
	}
}

func (m *storeDifferentialMachine) RuleReadVReadAndHistory(tc hegel.TestCase) {
	resID := hegel.Draw(tc, hegel.SampledFrom([]string{"pat-1", "pat-2", "pat-3", "pat-4", "pat-5", "pat-missing"}))
	vers := m.model[resID]

	for _, tgt := range m.targets {
		rec, err := tgt.store.ReadResourceWithMeta(m.ctx, m.tenant, "Patient", resID)
		cls := classifyStoreErr(err)
		switch {
		case len(vers) == 0:
			if cls != "not_found" {
				tc.Errorf("%s ReadResourceWithMeta(%s): expected not_found, got %s", tgt.name, resID, cls)
			}
		case vers[len(vers)-1].deleted:
			if cls != "deleted" {
				tc.Errorf("%s ReadResourceWithMeta(%s): expected deleted, got %s", tgt.name, resID, cls)
			}
		default:
			want := vers[len(vers)-1]
			if cls != "ok" || rec == nil || rec.ResVersion != want.version || !bytes.Equal(rec.ResourceProto, want.protoData) {
				tc.Errorf("%s ReadResourceWithMeta(%s): got (%s, rec=%+v), want v%d payload=%q",
					tgt.name, resID, cls, rec, want.version, string(want.protoData))
			}
		}

		// Verify VRead across every historical version 1..len(vers).
		for _, mv := range vers {
			vrec, verr := tgt.store.ReadResourceVersion(m.ctx, m.tenant, "Patient", resID, mv.version)
			vcls := classifyStoreErr(verr)
			if mv.deleted {
				if vcls != "deleted" {
					tc.Errorf("%s ReadResourceVersion(%s, v%d): expected deleted, got %s", tgt.name, resID, mv.version, vcls)
				}
			} else {
				if vcls != "ok" || vrec == nil || vrec.ResVersion != mv.version || !bytes.Equal(vrec.ResourceProto, mv.protoData) {
					tc.Errorf("%s ReadResourceVersion(%s, v%d): mismatch got (%s, %+v)", tgt.name, resID, mv.version, vcls, vrec)
				}
			}
		}

		// Verify ListResourceHistory descending version order and total count.
		hist, total, herr := tgt.store.ListResourceHistory(m.ctx, m.tenant, "Patient", resID, 100, 0)
		if herr != nil {
			tc.Errorf("%s ListResourceHistory(%s) failed: %v", tgt.name, resID, herr)
		}
		if total != len(vers) || len(hist) != len(vers) {
			tc.Errorf("%s ListResourceHistory(%s): got len=%d total=%d, want %d", tgt.name, resID, len(hist), total, len(vers))
		}
		for idx, h := range hist {
			wantVer := len(vers) - idx
			if h.ResVersion != wantVer {
				tc.Errorf("%s ListResourceHistory(%s)[%d]: got v%d, want v%d", tgt.name, resID, idx, h.ResVersion, wantVer)
			}
		}
	}
}

func (m *storeDifferentialMachine) RuleSearchAndUCUMFilterParity(tc hegel.TestCase) {
	queryKind := hegel.Draw(tc, hegel.SampledFrom([]string{
		"all",
		"family_prefix",
		"gender_token",
		"gender_not",
		"birthdate_ge",
		"ucum_grams_gt",
		"filter_compound",
	}))

	sp := store.SearchParams{
		TenantID: m.tenant,
		ResType:  "Patient",
		Count:    100,
	}
	switch queryKind {
	case "family_prefix":
		fam := hegel.Draw(tc, hegel.SampledFrom([]string{"Ad", "Ba", "Cu", "Da"}))
		sp.Strings = map[string]string{"family": fam}
	case "gender_token":
		gen := hegel.Draw(tc, hegel.SampledFrom([]string{"female", "male", "other"}))
		sp.Tokens = map[string]string{"gender": gen}
	case "gender_not":
		sp.Tokens = map[string]string{"gender:not": "male"}
	case "birthdate_ge":
		op, err := store.ParseDateOp("ge1990-01-01")
		if err != nil {
			tc.Errorf("ParseDateOp: %v", err)
		}
		sp.Dates = map[string]store.DateOp{"birthdate": op}
	case "ucum_grams_gt":
		// Stored in "kg" (50..95 kg); query in "g" (> 60000 g == 60 kg) to exercise cross-unit UCUM parity!
		qOp, err := store.ParseQuantityOp("gt60000|http://unitsofmeasure.org|g")
		if err != nil {
			tc.Errorf("ParseQuantityOp: %v", err)
		}
		sp.Quantities = map[string]store.QuantityOp{"value-quantity": qOp}
	case "filter_compound":
		fAst, err := store.ParseFilterExpr(`(gender eq "female" or family sw "Ba") and not (family eq "Darwin")`)
		if err != nil {
			tc.Errorf("ParseFilterExpr: %v", err)
		}
		sp.Filters = []store.FilterExpr{fAst}
	}

	formatHits := func(sr *store.SearchResults) []string {
		out := make([]string, 0, len(sr.Matches))
		for _, hit := range sr.Matches {
			out = append(out, fmt.Sprintf("%s/%s@v%d:%s", hit.ResType, hit.ResID, hit.ResVersion, string(hit.ResourceProto)))
		}
		sort.Strings(out)
		return out
	}

	var refHits []string
	var refTotal int
	for i, tgt := range m.targets {
		sr, err := tgt.store.Search(m.ctx, sp)
		if err != nil {
			tc.Errorf("%s Search(%s) failed: %v", tgt.name, queryKind, err)
		}
		hits := formatHits(sr)
		if i == 0 {
			refHits = hits
			refTotal = sr.Total
			continue
		}
		if sr.Total != refTotal || len(hits) != len(refHits) {
			tc.Errorf("Search(%s) count mismatch: %s=(total=%d,hits=%v) vs %s=(total=%d,hits=%v)",
				queryKind, m.targets[0].name, refTotal, refHits, tgt.name, sr.Total, hits)
		}
		for k := range hits {
			if hits[k] != refHits[k] {
				tc.Errorf("Search(%s) hit[%d] mismatch: %s=%q vs %s=%q", queryKind, k, m.targets[0].name, refHits[k], tgt.name, hits[k])
			}
		}
	}
}

func (m *storeDifferentialMachine) InvariantReplicaAndBackendConvergence(tc hegel.TestCase) {
	activeExpected := 0
	for resID := range m.touchedIDs {
		vers := m.model[resID]
		isActive := len(vers) > 0 && !vers[len(vers)-1].deleted
		if isActive {
			activeExpected++
		}
		for _, tgt := range m.targets {
			rec, err := tgt.store.ReadResourceWithMeta(m.ctx, m.tenant, "Patient", resID)
			cls := classifyStoreErr(err)
			switch {
			case len(vers) == 0:
				if cls != "not_found" {
					tc.Errorf("invariant %s/%s: expected not_found, got %s", tgt.name, resID, cls)
				}
			case vers[len(vers)-1].deleted:
				if cls != "deleted" {
					tc.Errorf("invariant %s/%s: expected deleted, got %s", tgt.name, resID, cls)
				}
			default:
				want := vers[len(vers)-1]
				if cls != "ok" || rec == nil || rec.ResVersion != want.version || !bytes.Equal(rec.ResourceProto, want.protoData) {
					tc.Errorf("invariant %s/%s: state drift got (%s, %+v), want v%d", tgt.name, resID, cls, rec, want.version)
				}
			}
		}
	}

	// Verify HybridStore's analytical replica (Doris) matches active cohort counts & stream export!
	var exported int
	if err := m.hybrid.StreamAnalyticalExport(m.ctx, store.ExportParams{
		TenantID:      m.tenant,
		ResourceTypes: []string{"Patient"},
	}, func(_ store.SearchResult) error {
		exported++
		return nil
	}); err != nil {
		tc.Errorf("invariant HybridStore.StreamAnalyticalExport failed: %v", err)
	}
	if exported != activeExpected {
		tc.Errorf("invariant HybridStore analytical replica drift: exported=%d activeExpected=%d", exported, activeExpected)
	}

	// Verify real modernc.org/sqlite SQL table row count matches activeExpected!
	if m.sqliteDB != nil {
		var sqlActive int
		if err := m.sqliteDB.QueryRowContext(
			m.ctx,
			"SELECT COUNT(*) FROM fhir_resource WHERE tenant_id = ? AND res_type = 'Patient' AND is_deleted = 0",
			m.tenant,
		).Scan(&sqlActive); err != nil {
			tc.Errorf("invariant sqlite SQL count query failed: %v", err)
		}
		if sqlActive != activeExpected {
			tc.Errorf("invariant sqlite SQL table drift: sqlActive=%d activeExpected=%d", sqlActive, activeExpected)
		}
	}
}

func TestProperty_StatefulStoreDifferentialMachine(t *testing.T) {
	ctx := context.Background()

	// Optional live PostgreSQL connection when FLINT_TEST_POSTGRES_DSN is configured (always enabled in CI).
	var pgDB *sql.DB
	if dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN"); dsn != "" {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := postgres.RunMigrations(ctx, db); err != nil {
			t.Fatalf("postgres migrations: %v", err)
		}
		pgDB = db
	}

	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)
	if err := sqlite.RunMigrations(ctx, sqlDB, sqlite.JournalMemory); err != nil {
		t.Fatalf("sqlite migrations: %v", err)
	}

	var runSeq atomic.Int64

	hegel.Test(t, func(ht *hegel.T) {
		seq := runSeq.Add(1)
		tenant := fmt.Sprintf("pbt-diff-%d", seq)

		sqliteStandalone := sqlite.New(sqlDB, sqlite.WithJournalMode(sqlite.JournalMemory), sqlite.WithDSN(":memory:"))
		dorisStandalone := doris.New(nil)

		hybridPrimary := sqlite.New(nil, sqlite.WithDSN(":memory:"))
		hybridReplica := doris.New(nil)
		hybridStore := store.NewHybridStore(hybridPrimary, hybridReplica)

		targets := []namedStoreTarget{
			{name: "sqlite-sql", store: sqliteStandalone},
			{name: "doris-mow", store: dorisStandalone},
			{name: "hybrid-sqlite-doris", store: hybridStore},
		}

		if pgDB != nil {
			_ = postgres.PurgeTenant(ctx, pgDB, tenant)
			defer func(tn string) { _ = postgres.PurgeTenant(context.Background(), pgDB, tn) }(tenant)
			targets = append(targets, namedStoreTarget{
				name:  "postgres-acid",
				store: postgres.New(pgDB),
			})
		}

		machine := &storeDifferentialMachine{
			ctx:        ctx,
			tenant:     tenant,
			targets:    targets,
			hybrid:     hybridStore,
			sqliteDB:   sqlDB,
			model:      make(map[string][]modelResourceVersion),
			touchedIDs: make(map[string]bool),
		}

		hegel.RunStateful(
			ht,
			machine,
			hegel.WithStatefulStepCount(25),
			hegel.WithAlwaysCheckInvariants("InvariantReplicaAndBackendConvergence"),
		)

		// Final crash-recovery check: hydrate a fresh sqlite.Store from sqlDB via LoadFromDB
		// and verify its active cohort matches the reference model!
		reloaded := sqlite.New(sqlDB, sqlite.WithJournalMode(sqlite.JournalMemory), sqlite.WithDSN(":memory:"))
		if err := reloaded.LoadFromDB(ctx); err != nil {
			ht.Fatalf("sqlite LoadFromDB failed: %v", err)
		}
		srReload, err := reloaded.Search(ctx, store.SearchParams{TenantID: tenant, ResType: "Patient", Count: 100})
		if err != nil {
			ht.Fatalf("reloaded sqlite Search failed: %v", err)
		}
		srOrig, err := sqliteStandalone.Search(ctx, store.SearchParams{TenantID: tenant, ResType: "Patient", Count: 100})
		if err != nil {
			ht.Fatalf("original sqlite Search failed: %v", err)
		}
		if srReload.Total != srOrig.Total {
			ht.Fatalf("sqlite LoadFromDB cohort mismatch: reloaded=%d orig=%d", srReload.Total, srOrig.Total)
		}

		// Verify ExpungeResources parity across backends and SQLite LoadFromDB persistence after expunge.
		var refExpunged int
		for i, tgt := range targets {
			n, err := tgt.store.ExpungeResources(ctx, store.ExpungeParams{
				TenantID:                tenant,
				ExpungeDeletedResources: true,
				ExpungePreviousVersions: true,
			})
			if err != nil {
				ht.Fatalf("%s ExpungeResources failed: %v", tgt.name, err)
			}
			if i == 0 {
				refExpunged = n
			} else if n != refExpunged {
				ht.Fatalf("ExpungeResources count mismatch: %s=%d vs %s=%d", targets[0].name, refExpunged, tgt.name, n)
			}
		}
		postExpungeReload := sqlite.New(sqlDB, sqlite.WithJournalMode(sqlite.JournalMemory), sqlite.WithDSN(":memory:"))
		if err := postExpungeReload.LoadFromDB(ctx); err != nil {
			ht.Fatalf("post-expunge sqlite LoadFromDB failed: %v", err)
		}
		_, histTotalReload, err := postExpungeReload.ListHistory(ctx, tenant, "Patient", "", nil, 100, 0)
		if err != nil {
			ht.Fatalf("post-expunge reloaded ListHistory failed: %v", err)
		}
		if histTotalReload != srOrig.Total {
			ht.Fatalf("post-expunge sqlite history drift after LoadFromDB: got %d versions, want %d active-only versions", histTotalReload, srOrig.Total)
		}
	}, hegel.WithTestCases(40))
}
