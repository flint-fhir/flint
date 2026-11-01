package store_test

import (
	"testing"
	"time"

	"github.com/flint-fhir/flint/store"
)

func TestEvaluateAdvancedSearchParams_AllFeatures(t *testing.T) {
	t0 := time.Date(2025, 3, 15, 12, 0, 0, 0, time.UTC)
	birthLow := time.Date(1980, 5, 10, 0, 0, 0, 0, time.UTC)
	birthHigh := time.Date(1980, 5, 11, 0, 0, 0, 0, time.UTC)

	orgView := store.IndexedResourceView{
		TenantID:    "t1",
		ResType:     "Organization",
		ResID:       "org-1",
		ResVersion:  1,
		LastUpdated: t0.Add(-2 * time.Hour),
		Indexes: &store.SearchIndexes{
			Strings: []store.SpidxString{
				{SpName: "name", SpValue: "General Hospital"},
			},
			Tokens: []store.SpidxToken{
				{SpName: "identifier", SpSystem: "http://hl7.org/fhir/sid/us-npi", SpValue: "999888"},
			},
		},
	}

	patView := store.IndexedResourceView{
		TenantID:    "t1",
		ResType:     "Patient",
		ResID:       "pat-1",
		ResVersion:  1,
		LastUpdated: t0.Add(-time.Hour),
		PatientID:   "pat-1",
		Indexes: &store.SearchIndexes{
			Strings: []store.SpidxString{
				{SpName: "family", SpValue: "Smith"},
				{SpName: "given", SpValue: "Alice"},
				{SpName: "name", SpValue: "Alice Smith"},
				{SpName: "_text", SpValue: "Narrative summary for Alice Smith"},
			},
			Tokens: []store.SpidxToken{
				{SpName: "gender", SpValue: "female"},
				{SpName: "identifier", SpSystem: "http://hospital.example.org/mrn", SpValue: "MRN-1"},
				{SpName: "identifier:of-type", SpSystem: "http://terminology.hl7.org/CodeSystem/v2-0203|MR", SpValue: "MRN-1"},
				{SpName: "characteristic-value$0", SpSystem: "http://loinc.org", SpValue: "gender"},
				{SpName: "characteristic-value$0", SpSystem: "", SpValue: "female"},
			},
			Dates: []store.SpidxDate{
				{SpName: "birthdate", SpLow: birthLow, SpHigh: birthHigh},
			},
			References: []store.SpidxReference{
				{SpName: "organization", TargetType: "Organization", TargetID: "org-1"},
			},
			URIs: []store.SpidxURI{
				{SpName: "_profile", SpValue: "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
			},
		},
	}

	obsView := store.IndexedResourceView{
		TenantID:    "t1",
		ResType:     "Observation",
		ResID:       "obs-1",
		ResVersion:  1,
		LastUpdated: t0,
		PatientID:   "pat-1",
		EncounterID: "enc-1",
		Indexes: &store.SearchIndexes{
			Strings: []store.SpidxString{
				{SpName: "code:text", SpValue: "Systolic Blood Pressure"},
				{SpName: "value-string", SpValue: "normal"},
			},
			Tokens: []store.SpidxToken{
				{SpName: "code", SpSystem: "http://loinc.org", SpValue: "8480-6"},
				{SpName: "status", SpValue: "final"},
				{SpName: "value-concept", SpSystem: "http://snomed.info/sct", SpValue: "17621005"},
				{SpName: "comp-cvc:http://loinc.org|8480-6", SpSystem: "http://snomed.info/sct", SpValue: "17621005"},
			},
			Dates: []store.SpidxDate{
				{SpName: "date", SpLow: t0, SpHigh: t0.Add(time.Hour)},
				{SpName: "value-date", SpLow: birthLow, SpHigh: birthHigh},
			},
			References: []store.SpidxReference{
				{SpName: "patient", TargetType: "Patient", TargetID: "pat-1"},
				{SpName: "subject", TargetType: "Patient", TargetID: "pat-1"},
				{SpName: "encounter", TargetType: "Encounter", TargetID: "enc-1"},
			},
			Quantities: []store.SpidxQuantity{
				{SpName: "value-quantity", SpSystem: "http://unitsofmeasure.org", SpCode: "kg", SpValue: 70.0},
				{SpName: "comp-cvq:http://loinc.org|8480-6", SpSystem: "http://unitsofmeasure.org", SpCode: "mm[Hg]", SpValue: 120.0},
			},
		},
	}

	tempObsView := store.IndexedResourceView{
		TenantID:    "t1",
		ResType:     "Observation",
		ResID:       "obs-temp",
		ResVersion:  1,
		LastUpdated: t0.Add(time.Minute),
		PatientID:   "pat-1",
		Indexes: &store.SearchIndexes{
			Tokens: []store.SpidxToken{
				{SpName: "code", SpSystem: "http://loinc.org", SpValue: "8310-5"},
			},
			Quantities: []store.SpidxQuantity{
				{SpName: "value-quantity", SpSystem: "http://unitsofmeasure.org", SpCode: "Cel", SpValue: 37.0},
			},
		},
	}

	listView := store.IndexedResourceView{
		TenantID:    "t1",
		ResType:     "List",
		ResID:       "list-1",
		ResVersion:  1,
		LastUpdated: t0,
		Indexes: &store.SearchIndexes{
			References: []store.SpidxReference{
				{SpName: "item", TargetType: "Patient", TargetID: "pat-1"},
			},
		},
	}

	provView := store.IndexedResourceView{
		TenantID:    "t1",
		ResType:     "Provenance",
		ResID:       "prov-1",
		ResVersion:  1,
		LastUpdated: t0,
		Indexes: &store.SearchIndexes{
			References: []store.SpidxReference{
				{SpName: "target", TargetType: "Observation", TargetID: "obs-1"},
			},
		},
	}

	all := []store.IndexedResourceView{orgView, patView, obsView, tempObsView, listView, provView}
	lookup := func(tenantID, resType, resID string) (store.IndexedResourceView, bool) {
		for _, v := range all {
			if v.TenantID == tenantID && v.ResID == resID && (resType == "" || v.ResType == resType) {
				return v, true
			}
		}
		return store.IndexedResourceView{}, false
	}
	listTenant := func(tenantID string) []store.IndexedResourceView {
		if tenantID != "t1" {
			return nil
		}
		return all
	}

	// 1. Date prefixes
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"eq1980-05-10", true},
		{"ne1990-01-01", true},
		{"ne1980-05-10", false},
		{"ge1980-01-01", true},
		{"gt1979-12-31", true},
		{"le1980-12-31", true},
		{"lt1981-01-01", true},
		{"sa1979-01-01", true},
		{"eb1981-01-01", true},
		{"ap1980-05-10", true},
	} {
		dOp, err := store.ParseDateOp(tc.raw)
		if err != nil {
			t.Fatalf("ParseDateOp(%q): %v", tc.raw, err)
		}
		got := store.EvaluateAdvancedSearchParams(patView, store.SearchParams{
			TenantID:    "t1",
			Dates:       map[string]store.DateOp{"birthdate": dOp},
			DateFilters: map[string][]store.DateOp{"birthdate": {dOp}},
		}, lookup, listTenant)
		if got != tc.want {
			t.Errorf("Date %q: got %v, want %v", tc.raw, got, tc.want)
		}
	}

	mustQty := func(raw string) store.QuantityOp {
		q, err := store.ParseQuantityOp(raw)
		if err != nil {
			t.Fatalf("ParseQuantityOp(%q): %v", raw, err)
		}
		return q
	}

	// 2. Quantity + UCUM conversion (70 kg == 70000 g, ~154.32 lb; 37 Cel == 98.6 [degF], 310.15 K)
	if !store.EvaluateAdvancedSearchParams(obsView, store.SearchParams{
		TenantID: "t1",
		Quantities: map[string]store.QuantityOp{
			"value-quantity": mustQty("eq70000|http://unitsofmeasure.org|g"),
		},
		QuantityFilters: map[string][]store.QuantityOp{
			"value-quantity": {
				mustQty("gt60000|http://unitsofmeasure.org|g"),
				mustQty("lt80000|http://unitsofmeasure.org|g"),
				mustQty("ge70|http://unitsofmeasure.org|kg"),
				mustQty("le70|http://unitsofmeasure.org|kg"),
				mustQty("ap71|http://unitsofmeasure.org|kg"),
				mustQty("ne50|http://unitsofmeasure.org|kg"),
			},
		},
	}, lookup, listTenant) {
		t.Fatalf("expected UCUM mass conversion (kg <-> g) and quantity prefixes to match")
	}
	if !store.EvaluateAdvancedSearchParams(tempObsView, store.SearchParams{
		TenantID: "t1",
		QuantityFilters: map[string][]store.QuantityOp{
			"value-quantity": {
				mustQty("eq98.6|http://unitsofmeasure.org|[degF]"),
				mustQty("eq310.15|http://unitsofmeasure.org|K"),
			},
		},
	}, lookup, listTenant) {
		t.Fatalf("expected UCUM temperature conversion (Cel <-> [degF] <-> K) to match")
	}

	// 3. :missing modifier
	if !store.EvaluateAdvancedSearchParams(patView, store.SearchParams{
		TenantID: "t1",
		Missing: map[string]bool{
			"family":   false,
			"deceased": true,
		},
	}, lookup, listTenant) {
		t.Fatalf("expected :missing modifier to match")
	}

	// 4. Single-hop and multi-hop Chained search
	ch1, ok := store.ParseChainedParam("subject:Patient.family", "Smith")
	if !ok || ch1 == nil {
		t.Fatalf("ParseChainedParam single-hop failed")
	}
	ch2, ok := store.ParseChainedParam("subject:Patient.organization:Organization.name", "General")
	if !ok || ch2 == nil {
		t.Fatalf("ParseChainedParam multi-hop failed")
	}
	if !store.EvaluateAdvancedSearchParams(obsView, store.SearchParams{
		TenantID: "t1",
		Chained:  []store.ChainedParam{*ch1, *ch2},
	}, lookup, listTenant) {
		t.Fatalf("expected single-hop and multi-hop Chained search to match")
	}

	// 5. Reverse chaining (_has) and nested _has
	hasSimple := store.HasParam{
		SourceType:  "Observation",
		RefParam:    "patient",
		SearchParam: "code",
		ParamType:   "token",
		Value:       "http://loinc.org|8480-6",
	}
	hasNested := store.HasParam{
		SourceType: "Observation",
		RefParam:   "patient",
		Child: &store.HasParam{
			SourceType:  "Provenance",
			RefParam:    "target",
			SearchParam: "_id",
			ParamType:   "id",
			Value:       "prov-1",
		},
	}
	if !store.EvaluateAdvancedSearchParams(patView, store.SearchParams{
		TenantID: "t1",
		Has:      []store.HasParam{hasSimple, hasNested},
	}, lookup, listTenant) {
		t.Fatalf("expected _has and nested _has to match")
	}

	// 6. _filter boolean AST (and, or, not, eq, ne, co, sw, ew, pr)
	filterAST := store.FilterExpr{
		Kind: "and",
		Children: []store.FilterExpr{
			{
				Kind: "or",
				Children: []store.FilterExpr{
					{Kind: "param", Param: "family", Op: "eq", Value: "Smith"},
					{Kind: "param", Param: "family", Op: "eq", Value: "Jones"},
				},
			},
			{
				Kind: "not",
				Children: []store.FilterExpr{
					{Kind: "param", Param: "gender", Op: "eq", Value: "male"},
				},
			},
			{Kind: "param", Param: "name", Op: "co", Value: "alice"},
			{Kind: "param", Param: "given", Op: "sw", Value: "ali"},
			{Kind: "param", Param: "family", Op: "ew", Value: "ith"},
			{Kind: "param", Param: "birthdate", Op: "pr", Value: "true"},
			{Kind: "param", Param: "deceased", Op: "pr", Value: "false"},
			{Kind: "param", Param: "_profile", Op: "sb", Value: "http://hl7.org/fhir/us/core"},
		},
	}
	if !store.EvaluateAdvancedSearchParams(patView, store.SearchParams{
		TenantID: "t1",
		Filters:  []store.FilterExpr{filterAST},
	}, lookup, listTenant) {
		t.Fatalf("expected _filter boolean AST to match")
	}

	// 7. Composite parameters, _content, _text, _list, and Compartment
	if !store.EvaluateAdvancedSearchParams(obsView, store.SearchParams{
		TenantID: "t1",
		CompositeTokenQuantities: [][]store.CompositeTokenQuantityParam{
			{
				{
					ParamName:   "component-code-value-quantity",
					TokenSystem: "http://loinc.org",
					TokenCode:   "8480-6",
					Quantity:    mustQty("eq120|http://unitsofmeasure.org|mm[Hg]"),
				},
			},
			{
				{
					ParamName:   "code-value-quantity",
					TokenSystem: "http://loinc.org",
					TokenCode:   "8480-6",
					Quantity:    mustQty("eq70000|http://unitsofmeasure.org|g"),
				},
			},
		},
		CompositeTokenTokens: [][]store.CompositeTokenTokenParam{
			{
				{
					ParamName:   "combo-code-value-concept",
					TokenSystem: "http://loinc.org",
					TokenCode:   "8480-6",
					ValueSystem: "http://snomed.info/sct",
					ValueCode:   "17621005",
				},
			},
		},
		CompositeTokenDates: [][]store.CompositeTokenDateParam{
			{
				{
					ParamName:   "code-value-date",
					TokenSystem: "http://loinc.org",
					TokenCode:   "8480-6",
					Date:        store.DateOp{Prefix: "eq", Value: birthLow, High: birthHigh},
				},
			},
		},
		CompositeTokenStrings: [][]store.CompositeTokenStringParam{
			{
				{
					ParamName:   "code-value-string",
					TokenSystem: "http://loinc.org",
					TokenCode:   "8480-6",
					Value:       "norm",
				},
			},
		},
		CompartmentType: "Patient",
		CompartmentID:   "pat-1",
	}, lookup, listTenant) {
		t.Fatalf("expected Composite parameters and Patient compartment to match")
	}

	if !store.EvaluateAdvancedSearchParams(patView, store.SearchParams{
		TenantID: "t1",
		Content:  []string{"Smith"},
		Text:     []string{"Narrative"},
		List:     []string{"list-1"},
	}, lookup, listTenant) {
		t.Fatalf("expected _content, _text, and _list to match")
	}

	// 8. Provenance compartment membership via transitive target
	if !store.MatchCompartment("t1", provView, "Patient", "pat-1", listTenant) {
		t.Fatalf("expected Provenance targeting Patient's Observation to match Patient compartment")
	}

	// 9. SortSearchResults & ResolveSearchIncludes (:iterate)
	matches := []store.SearchResult{
		{ResType: "Observation", ResID: "obs-temp", LastUpdated: tempObsView.LastUpdated},
		{ResType: "Observation", ResID: "obs-1", LastUpdated: obsView.LastUpdated},
	}
	store.SortSearchResults("t1", matches, []store.SortSpec{{Param: "value-quantity", Desc: false}}, lookup)
	if matches[0].ResID != "obs-temp" {
		t.Fatalf("expected SortSearchResults by value-quantity ASC to put obs-temp (37) before obs-1 (70), got %s", matches[0].ResID)
	}

	incs := store.ResolveSearchIncludes(
		"t1",
		[]store.SearchResult{{ResType: "Observation", ResID: "obs-1", LastUpdated: t0}},
		[]store.IncludeParam{
			{SourceType: "Observation", ParamName: "patient", TargetType: "Patient"},
			{SourceType: "Patient", ParamName: "organization", TargetType: "Organization", Iterate: true},
		},
		[]store.IncludeParam{
			{SourceType: "Provenance", ParamName: "target"},
		},
		lookup,
		listTenant,
	)
	if len(incs) != 3 {
		t.Fatalf("expected 3 included resources (Patient + iterative Organization + revincluded Provenance), got %d", len(incs))
	}

	// 10. Additional UCUM dimensions (length, volume, duration, mass_conc, subst_conc) and incompatible units
	for _, tc := range []struct {
		val    float64
		from   string
		to     string
		wantOK bool
	}{
		{1.0, "m", "cm", true},
		{1.0, "L", "mL", true},
		{1.0, "h", "min", true},
		{1.0, "g/dL", "mg/dL", true},
		{1.0, "mol/L", "mmol/L", true},
		{100.0, "K", "Cel", true},
		{98.6, "[degF]", "Cel", true},
		{1.0, "kg", "m", false},
	} {
		_, ok := store.ConvertUCUMValue(tc.val, tc.from, tc.to)
		if ok != tc.wantOK {
			t.Errorf("ConvertUCUMValue(%v, %q, %q) ok=%v, want %v", tc.val, tc.from, tc.to, ok, tc.wantOK)
		}
	}

	// 11. Token :of-type, :not, :text, URI :above/:below, Reference :identifier, Chained date/quantity/uri/ref/id
	if !store.MatchIndexToken(patView.Indexes, "identifier:of-type", "http://terminology.hl7.org/CodeSystem/v2-0203|MR|MRN-1") {
		t.Fatalf("expected :of-type token match")
	}
	if !store.MatchIndexToken(patView.Indexes, "gender:not", "male") {
		t.Fatalf("expected :not token match")
	}
	if !store.MatchIndexToken(obsView.Indexes, "code:text", "Systolic") {
		t.Fatalf("expected :text token match")
	}
	if !store.MatchIndexURI(patView.Indexes, "_profile:above", "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient/extra") {
		t.Fatalf("expected :above URI match")
	}
	if !store.MatchIndexReference("t1", obsView.Indexes, "patient:identifier", "http://hospital.example.org/mrn|MRN-1", lookup, listTenant) {
		t.Fatalf("expected :identifier reference match")
	}

	for _, kv := range [][2]string{
		{"subject:Patient._id", "pat-1"},
		{"subject:Patient.birthdate", "ge1980-01-01"},
		{"subject:Patient._lastUpdated", "ge2020-01-01"},
		{"subject:Patient._profile", "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
		{"subject:Patient.organization", "Organization/org-1"},
		{"subject:Patient.gender", "female"},
		{"patient:Patient.value-quantity", "70"},
	} {
		ch, ok := store.ParseChainedParam(kv[0], kv[1])
		if !ok || ch == nil {
			t.Fatalf("ParseChainedParam(%s, %s) failed", kv[0], kv[1])
		}
		if kv[0] == "patient:Patient.value-quantity" {
			// Should not match because patView has no value-quantity
			if store.MatchChainedParam("t1", obsView.Indexes, *ch, lookup, listTenant) {
				t.Fatalf("expected chained %s=%s NOT to match", kv[0], kv[1])
			}
			continue
		}
		if !store.MatchChainedParam("t1", obsView.Indexes, *ch, lookup, listTenant) {
			t.Fatalf("expected chained %s=%s to match", kv[0], kv[1])
		}
	}

	// Untyped polymorphic chain (without :Patient) using listTenant fallback
	if chPoly, ok := store.ParseChainedParam("subject.family", "Smith"); !ok || chPoly == nil ||
		!store.MatchChainedParam("t1", obsView.Indexes, *chPoly, nil, listTenant) {
		t.Fatalf("expected polymorphic chained subject.family=Smith to match via listTenant")
	}

	// 12. _filter operators on date, quantity, token, reference, uri, id, lastUpdated, and chained
	obsFilter := store.FilterExpr{
		Kind: "and",
		Children: []store.FilterExpr{
			{Kind: "param", Param: "_id", Op: "eq", Value: "obs-1"},
			{Kind: "param", Param: "_id", Op: "ne", Value: "obs-999"},
			{Kind: "param", Param: "_lastUpdated", Op: "ge", Value: "2020-01-01"},
			{Kind: "param", Param: "date", Op: "ge", Value: "2025-01-01"},
			{Kind: "param", Param: "value-quantity", Op: "eq", Value: "70000|http://unitsofmeasure.org|g"},
			{Kind: "param", Param: "code", Op: "eq", Value: "8480-6"},
			{Kind: "param", Param: "code", Op: "ne", Value: "9999-9"},
			{Kind: "param", Param: "code", Op: "co", Value: "Systolic"},
			{Kind: "param", Param: "patient", Op: "eq", Value: "Patient/pat-1"},
			{Kind: "param", Param: "patient", Op: "ne", Value: "Patient/pat-999"},
			{Kind: "param", Param: "subject:Patient.family", Op: "eq", Value: "Smith"},
			{Kind: "param", Param: "subject:Patient.family", Op: "ne", Value: "Zimmerman"},
			{Kind: "param", Param: "unknown-param", Op: "ne", Value: "nope"},
		},
	}
	if !store.MatchFilterExpr(obsView, obsFilter, lookup, listTenant) {
		t.Fatalf("expected comprehensive obsFilter AST to match")
	}

	// Negative branches in EvaluateAdvancedSearchParams
	for _, negParams := range []store.SearchParams{
		{TenantID: "t1", CompartmentType: "Patient", CompartmentID: "pat-other"},
		{TenantID: "t1", LastUpdated: []store.DateOp{{Prefix: "gt", Value: t0.Add(24 * time.Hour), High: t0.Add(25 * time.Hour)}}},
		{TenantID: "t1", Dates: map[string]store.DateOp{"birthdate": {Prefix: "gt", Value: t0, High: t0}}},
		{TenantID: "t1", Quantities: map[string]store.QuantityOp{"value-quantity": mustQty("gt9999|http://unitsofmeasure.org|kg")}},
		{TenantID: "t1", Missing: map[string]bool{"family": true}},
		{TenantID: "t1", Content: []string{"nonexistent-term"}},
		{TenantID: "t1", Text: []string{"nonexistent-narrative"}},
		{TenantID: "t1", List: []string{"nonexistent-list"}},
	} {
		if store.EvaluateAdvancedSearchParams(patView, negParams, lookup, listTenant) {
			t.Fatalf("expected negative SearchParams %+v to return false", negParams)
		}
	}

	// 13. SortSearchResults by _id, _lastUpdated, date, and lexical string
	patMatches := []store.SearchResult{
		{ResType: "Patient", ResID: "pat-1", LastUpdated: t0},
		{ResType: "Patient", ResID: "pat-2", LastUpdated: t0.Add(-time.Hour)},
	}
	for _, spec := range []store.SortSpec{
		{Param: "_id", Desc: true},
		{Param: "_lastUpdated", Desc: false},
		{Param: "birthdate", Desc: false},
		{Param: "family", Desc: true},
	} {
		store.SortSearchResults("t1", patMatches, []store.SortSpec{spec}, lookup)
	}
	store.SortSearchResults("t1", patMatches, nil, lookup)
}
