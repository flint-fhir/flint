package postgres

import (
	"net/url"
	"testing"
)

// FuzzParseFilterExpr fuzzes the FHIR R4 _filter boolean AST parser (ParseFilterExpr)
// and search parameter builder to guarantee zero panics on arbitrary or malformed filter expressions.
func FuzzParseFilterExpr(f *testing.F) {
	seeds := []string{
		`name co "Smith"`,
		`given eq "Alice" and family ne "Jones"`,
		`not (gender eq "unknown" or active eq "false")`,
		`(birthdate ge 1980-01-01 and birthdate le 2020-12-31) or (name sw "Al" and name ew "ce")`,
		`code in "http://hl7.org/fhir/ValueSet/observation-codes" and value-quantity gt 120.5|http://unitsofmeasure.org|mm[Hg]`,
		`code ni "http://example.org/vs" or status pr true`,
		`identifier re "^[A-Z0-9-]+$" and _lastUpdated sa 2025-01-01T00:00:00Z`,
		`date eb 2026-01-01 and date ap 2025-06-15`,
		`date po 2025-01-01 and code ss "1234-5" and code sb "9999-9"`,
		`(((((name eq "deep")))))`,
		`name co "escaped \"quote\" inside" and family eq "O'Connor"`,
		`((((unbalanced parens`,
		`unclosed quote "hello`,
		`and or not`,
		`eq "missing-lhs"`,
		`name unknownop "val"`,
		`12345`,
		`'; DROP TABLE fhir_resource; --`,
		"",
		"   \t\n  ",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		expr, err := ParseFilterExpr(raw)
		if err == nil && expr.Kind == "" {
			t.Fatalf("ParseFilterExpr(%q) returned empty expr.Kind with nil error", raw)
		}
		q := url.Values{"_filter": []string{raw}}
		_ = ValidateSearchQuery(q)
		params := BuildSearchParamsFromQuery("fuzz-tenant", "Patient", q)
		_ = HasSearchCriteria(params)
	})
}

// FuzzSearchQueryAndIfNoneExist fuzzes FHIR R4 search query parsing, chained parameters,
// _has reverse chaining, composite search parameters, date/quantity prefixes, and If-None-Exist headers.
func FuzzSearchQueryAndIfNoneExist(f *testing.F) {
	seeds := []string{
		"name=Smith&gender=female&_count=10&_offset=0&_sort=-_lastUpdated,name",
		"identifier:of-type=http://terminology.hl7.org/CodeSystem/v2-0203|MR|12345",
		"birthdate=ge1990-01-01&birthdate=le2025-12-31&_include=Observation:patient&_revinclude:iterate=Provenance:target",
		"_has:Observation:patient:code=8867-4&_has:Encounter:patient:_has:Observation:encounter:code=8480-6",
		"subject:Patient.organization:Organization.name:exact=Acme",
		"code-value-quantity=http://loinc.org|8480-6$ge140|http://unitsofmeasure.org|mm[Hg]",
		"code-value-concept=http://loinc.org|72166-2$http://snomed.info/sct|449868002",
		"code-value-date=http://loinc.org|11778-8$ap2026-01-15",
		"code-value-string=http://loinc.org|95541-9$positive",
		"value-quantity=ap98.6|http://unitsofmeasure.org|[degF]",
		"code:in=http://hl7.org/fhir/ValueSet/observation-vitalsignresult",
		"code:below=http://snomed.info/sct|73211009&url:above=http://example.org/fhir/StructureDefinition/test/v1",
		"deceased:missing=true&_elements=id,name,birthDate&_summary=true",
		"Patient?identifier=http://hospital.example.org/mrn|999",
		"?identifier=123,456&_id=pat-1,pat-2",
		"_filter=name%20co%20%22Smith%22%20and%20active%20eq%20true",
		"invalid===&&&_has:broken:onlytwo=1&code-value-quantity=no-dollar",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, rawQuery string) {
		_, _ = ParseIfNoneExist("fuzz-tenant", "Observation", rawQuery)
		_, _ = ParseDateOp(rawQuery)
		_, _ = ParseQuantityOp(rawQuery)
		_, _ = ParseInclude(rawQuery)
		_, _ = ParseSortSpecs(rawQuery)
		_, _ = ParseCompositeTokenQuantity("code-value-quantity", rawQuery)
		_, _ = ParseCompositeTokenToken("code-value-concept", rawQuery)
		_, _ = ParseCompositeTokenDate("code-value-date", rawQuery)
		_, _ = ParseCompositeTokenString("code-value-string", rawQuery)

		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return
		}
		_ = ValidateSearchQuery(q)
		for k, vals := range q {
			for _, v := range vals {
				_, _ = ParseChainedParam(k, v)
				_, _, _ = ParseHasParam(k, v)
			}
		}
		params := BuildSearchParamsFromQuery("fuzz-tenant", "Observation", q)
		_ = HasSearchCriteria(params)
	})
}
