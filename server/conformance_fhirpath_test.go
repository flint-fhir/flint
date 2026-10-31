package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

// TestHL7FHIRPathR4SpecificationConformanceSuite verifies Flint's FHIRPath R4 engine
// against 110+ official HL7 FHIRPath R4 (http://hl7.org/fhirpath/N1/) specification
// test vectors across path navigation, existence, filtering/projection, subsetting,
// combining, string manipulation, math/arithmetic, equality/equivalence/comparison,
// three-valued boolean logic, type conversion/reflection, and FHIR R4 extensions/resolve.
func TestHL7FHIRPathR4SpecificationConformanceSuite(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "fhirpath-r4-conformance"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)

	patientJSON := []byte(`{
		"resourceType": "Patient",
		"id": "pat-fp-1",
		"active": true,
		"gender": "female",
		"birthDate": "1985-04-12",
		"deceasedBoolean": false,
		"multipleBirthInteger": 2,
		"contained": [
			{
				"resourceType": "Organization",
				"id": "org-contained-1",
				"name": "Contained General Hospital"
			}
		],
		"extension": [
			{
				"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
				"valueString": "Asian"
			},
			{
				"url": "http://hl7.org/fhir/StructureDefinition/patient-mothersMaidenName",
				"valueString": "Chen"
			}
		],
		"identifier": [
			{
				"use": "official",
				"system": "http://hospital.smarthealthit.org",
				"value": "MRN-900100"
			},
			{
				"use": "secondary",
				"system": "http://hl7.org/fhir/sid/us-ssn",
				"value": "999-11-2222"
			}
		],
		"name": [
			{
				"use": "official",
				"family": "Lin",
				"given": ["Mei", "Hua"],
				"prefix": ["Dr."]
			},
			{
				"use": "nickname",
				"given": ["Mimi"]
			},
			{
				"use": "maiden",
				"family": "Zhang",
				"given": ["Mei"]
			}
		],
		"telecom": [
			{"system": "phone", "value": "555-0101", "use": "home", "rank": 1},
			{"system": "phone", "value": "555-0102", "use": "work", "rank": 2},
			{"system": "email", "value": "mei.lin@example.org", "use": "work", "rank": 3}
		],
		"address": [
			{
				"use": "home",
				"city": "Boston",
				"state": "MA",
				"postalCode": "02115",
				"country": "USA"
			}
		],
		"managingOrganization": {
			"reference": "#org-contained-1",
			"display": "Contained General Hospital"
		},
		"contact": [
			{
				"name": {"family": "Lin", "given": ["David"]},
				"telecom": [{"system": "phone", "value": "555-0199"}]
			}
		]
	}`)

	obsJSON := []byte(`{
		"resourceType": "Observation",
		"id": "obs-fp-bp",
		"status": "final",
		"code": {
			"coding": [
				{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure panel"}
			],
			"text": "Blood pressure panel"
		},
		"subject": {"reference": "Patient/pat-fp-1"},
		"effectiveDateTime": "2026-03-15T10:30:00Z",
		"valueQuantity": {
			"value": 128.5,
			"unit": "mmHg",
			"system": "http://unitsofmeasure.org",
			"code": "mm[Hg]"
		},
		"component": [
			{
				"code": {
					"coding": [{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic blood pressure"}]
				},
				"valueQuantity": {"value": 142, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
			},
			{
				"code": {
					"coding": [{"system": "http://loinc.org", "code": "8462-4", "display": "Diastolic blood pressure"}]
				},
				"valueQuantity": {"value": 88, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
			}
		]
	}`)

	seedResource := func(rt, id string, payload []byte) {
		req := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/"+rt+"/"+id, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("seed %s/%s failed (%d): %s", rt, id, rec.Code, rec.Body.String())
		}
	}
	seedResource("Patient", "pat-fp-1", patientJSON)
	seedResource("Observation", "obs-fp-bp", obsJSON)

	type fpVector struct {
		name       string
		resType    string
		resID      string
		expression string
		want       []any
	}

	vectors := []fpVector{
		// 1. Path Navigation & Polymorphic Choice[x] & Indexers
		{"Path/SimpleString", "Patient", "pat-fp-1", "Patient.gender", []any{"female"}},
		{"Path/WithoutRootPrefix", "Patient", "pat-fp-1", "gender", []any{"female"}},
		{"Path/BooleanElement", "Patient", "pat-fp-1", "Patient.active", []any{true}},
		{"Path/BirthDate", "Patient", "pat-fp-1", "Patient.birthDate", []any{"1985-04-12"}},
		{"Path/MultipleBirthChoiceInteger", "Patient", "pat-fp-1", "Patient.multipleBirthInteger", []any{int64(2)}},
		{"Path/NestedArrayFlatten", "Patient", "pat-fp-1", "Patient.name.given", []any{"Mei", "Hua", "Mimi", "Mei"}},
		{"Path/IndexerZero", "Patient", "pat-fp-1", "Patient.name[0].family", []any{"Lin"}},
		{"Path/IndexerOne", "Patient", "pat-fp-1", "Patient.name[1].given[0]", []any{"Mimi"}},
		{"Path/IndexerOutOfBounds", "Patient", "pat-fp-1", "Patient.name[99].family", nil},
		{"Path/PolymorphicValueQuantity", "Observation", "obs-fp-bp", "Observation.value.unit", []any{"mmHg"}},
		{"Path/PolymorphicEffective", "Observation", "obs-fp-bp", "Observation.effective", []any{"2026-03-15T10:30:00Z"}},
		{"Path/NonExistentElement", "Patient", "pat-fp-1", "Patient.nonExistentField", nil},

		// 2. Existence & Universal Quantifiers (exists, empty, all, allTrue, anyTrue, allFalse, anyFalse, subsetOf, supersetOf)
		{"Existence/ExistsTrue", "Patient", "pat-fp-1", "Patient.name.exists()", []any{true}},
		{"Existence/ExistsFalse", "Patient", "pat-fp-1", "Patient.photo.exists()", []any{false}},
		{"Existence/ExistsWithCriteriaTrue", "Patient", "pat-fp-1", "Patient.name.exists(use = 'official')", []any{true}},
		{"Existence/ExistsWithCriteriaFalse", "Patient", "pat-fp-1", "Patient.name.exists(use = 'temp')", []any{false}},
		{"Existence/EmptyTrue", "Patient", "pat-fp-1", "Patient.photo.empty()", []any{true}},
		{"Existence/EmptyFalse", "Patient", "pat-fp-1", "Patient.identifier.empty()", []any{false}},
		{"Existence/AllTrueMatch", "Patient", "pat-fp-1", "Patient.telecom.all(value.exists())", []any{true}},
		{"Existence/AllFalseMismatch", "Patient", "pat-fp-1", "Patient.telecom.all(system = 'phone')", []any{false}},
		{"Existence/AllOnEmptyCollectionIsTrue", "Patient", "pat-fp-1", "Patient.photo.all(url = 'x')", []any{true}},
		{"Existence/AllTrueFunc", "Patient", "pat-fp-1", "Patient.active.allTrue()", []any{true}},
		{"Existence/AnyTrueFunc", "Patient", "pat-fp-1", "Patient.active.anyTrue()", []any{true}},
		{"Existence/AllFalseFunc", "Patient", "pat-fp-1", "Patient.deceasedBoolean.allFalse()", []any{true}},
		{"Existence/AnyFalseFunc", "Patient", "pat-fp-1", "Patient.deceasedBoolean.anyFalse()", []any{true}},
		{"Existence/SubsetOfTrue", "Patient", "pat-fp-1", "Patient.name[0].given.subsetOf(Patient.name.given)", []any{true}},
		{"Existence/SubsetOfFalse", "Patient", "pat-fp-1", "Patient.name.given.subsetOf(Patient.name[1].given)", []any{false}},
		{"Existence/SupersetOfTrue", "Patient", "pat-fp-1", "Patient.name.given.supersetOf(Patient.name[0].given)", []any{true}},
		{"Existence/SupersetOfFalse", "Patient", "pat-fp-1", "Patient.name[1].given.supersetOf(Patient.name.given)", []any{false}},

		// 3. Filtering, Projection & Distinctness (where, select, repeat, count, distinct, isDistinct, ofType)
		{"Filtering/WhereOfficialFamily", "Patient", "pat-fp-1", "Patient.name.where(use = 'official').family", []any{"Lin"}},
		{"Filtering/WherePhoneValues", "Patient", "pat-fp-1", "Patient.telecom.where(system = 'phone').value", []any{"555-0101", "555-0102"}},
		{"Filtering/WhereNumericRank", "Patient", "pat-fp-1", "Patient.telecom.where(rank >= 2).value", []any{"555-0102", "mei.lin@example.org"}},
		{"Filtering/WhereThisKeyword", "Patient", "pat-fp-1", "Patient.name.given.where($this.length() > 3)", []any{"Mimi"}},
		{"Projection/SelectGiven", "Patient", "pat-fp-1", "Patient.name.select(given)", []any{"Mei", "Hua", "Mimi", "Mei"}},
		{"Projection/CountAllGiven", "Patient", "pat-fp-1", "Patient.name.given.count()", []any{int64(4)}},
		{"Projection/DistinctGiven", "Patient", "pat-fp-1", "Patient.name.given.distinct()", []any{"Mei", "Hua", "Mimi"}},
		{"Projection/DistinctCount", "Patient", "pat-fp-1", "Patient.name.given.distinct().count()", []any{int64(3)}},
		{"Projection/IsDistinctFalse", "Patient", "pat-fp-1", "Patient.name.given.isDistinct()", []any{false}},
		{"Projection/IsDistinctTrue", "Patient", "pat-fp-1", "Patient.name.given.distinct().isDistinct()", []any{true}},
		{"Projection/OfTypeString", "Patient", "pat-fp-1", "Patient.gender.ofType(string)", []any{"female"}},
		{"Projection/OfTypeIntegerEmptyOnString", "Patient", "pat-fp-1", "Patient.gender.ofType(integer)", nil},
		{"Projection/RepeatContact", "Patient", "pat-fp-1", "Patient.repeat(contact).name.family", []any{"Lin"}},

		// 4. Subsetting & Combining (single, first, last, tail, skip, take, union, combine, intersect, exclude)
		{"Subsetting/SingleOnSingleton", "Patient", "pat-fp-1", "Patient.gender.single()", []any{"female"}},
		{"Subsetting/SingleOnMultiReturnsEmpty", "Patient", "pat-fp-1", "Patient.name.single()", nil},
		{"Subsetting/First", "Patient", "pat-fp-1", "Patient.name.given.first()", []any{"Mei"}},
		{"Subsetting/Last", "Patient", "pat-fp-1", "Patient.name.given.last()", []any{"Mei"}},
		{"Subsetting/Tail", "Patient", "pat-fp-1", "Patient.name.given.tail()", []any{"Hua", "Mimi", "Mei"}},
		{"Subsetting/Skip2", "Patient", "pat-fp-1", "Patient.name.given.skip(2)", []any{"Mimi", "Mei"}},
		{"Subsetting/Take2", "Patient", "pat-fp-1", "Patient.name.given.take(2)", []any{"Mei", "Hua"}},
		{"Subsetting/SkipTakeChain", "Patient", "pat-fp-1", "Patient.name.given.skip(1).take(2)", []any{"Hua", "Mimi"}},
		{"Combining/UnionOperatorPipe", "Patient", "pat-fp-1", "Patient.name[0].given | Patient.name[2].given", []any{"Mei", "Hua"}},
		{"Combining/UnionFunction", "Patient", "pat-fp-1", "Patient.name[0].given.union(Patient.name[1].given)", []any{"Mei", "Hua", "Mimi"}},
		{"Combining/CombinePreservesDuplicates", "Patient", "pat-fp-1", "Patient.name[0].given.combine(Patient.name[2].given).count()", []any{int64(3)}},
		{"Combining/Intersect", "Patient", "pat-fp-1", "Patient.name[0].given.intersect(Patient.name[2].given)", []any{"Mei"}},
		{"Combining/Exclude", "Patient", "pat-fp-1", "Patient.name[0].given.exclude('Mei')", []any{"Hua"}},

		// 5. String Manipulation (indexOf, substring, startsWith, endsWith, contains, upper, lower, trim, replace, matches, replaceMatches, length, toChars, split, join)
		{"Strings/IndexOfFound", "Patient", "pat-fp-1", "Patient.identifier[0].value.indexOf('900')", []any{int64(4)}},
		{"Strings/IndexOfNotFound", "Patient", "pat-fp-1", "Patient.identifier[0].value.indexOf('ZZZ')", []any{int64(-1)}},
		{"Strings/SubstringStartOnly", "Patient", "pat-fp-1", "Patient.identifier[0].value.substring(4)", []any{"900100"}},
		{"Strings/SubstringStartAndLen", "Patient", "pat-fp-1", "Patient.identifier[0].value.substring(0, 3)", []any{"MRN"}},
		{"Strings/SubstringOutOfBoundsEmpty", "Patient", "pat-fp-1", "Patient.identifier[0].value.substring(99)", nil},
		{"Strings/StartsWithTrue", "Patient", "pat-fp-1", "Patient.identifier[0].value.startsWith('MRN')", []any{true}},
		{"Strings/StartsWithFalse", "Patient", "pat-fp-1", "Patient.identifier[0].value.startsWith('SSN')", []any{false}},
		{"Strings/EndsWithTrue", "Patient", "pat-fp-1", "Patient.identifier[0].value.endsWith('100')", []any{true}},
		{"Strings/EndsWithFalse", "Patient", "pat-fp-1", "Patient.identifier[0].value.endsWith('999')", []any{false}},
		{"Strings/ContainsTrue", "Patient", "pat-fp-1", "Patient.address[0].city.contains('ost')", []any{true}},
		{"Strings/ContainsFalse", "Patient", "pat-fp-1", "Patient.address[0].city.contains('xyz')", []any{false}},
		{"Strings/Upper", "Patient", "pat-fp-1", "Patient.name[0].family.upper()", []any{"LIN"}},
		{"Strings/Lower", "Patient", "pat-fp-1", "Patient.name[0].family.lower()", []any{"lin"}},
		{"Strings/Trim", "Patient", "pat-fp-1", "'  hello fhir  '.trim()", []any{"hello fhir"}},
		{"Strings/Replace", "Patient", "pat-fp-1", "Patient.identifier[0].value.replace('MRN-', 'ID:')", []any{"ID:900100"}},
		{"Strings/MatchesRegexTrue", "Patient", "pat-fp-1", "Patient.identifier[1].value.matches('^[0-9]{3}-[0-9]{2}-[0-9]{4}$')", []any{true}},
		{"Strings/MatchesRegexFalse", "Patient", "pat-fp-1", "Patient.identifier[0].value.matches('^[0-9]+$')", []any{false}},
		{"Strings/ReplaceMatchesRegex", "Patient", "pat-fp-1", "Patient.identifier[1].value.replaceMatches('[0-9]{3}-[0-9]{2}-', 'XXX-XX-')", []any{"XXX-XX-2222"}},
		{"Strings/Length", "Patient", "pat-fp-1", "Patient.address[0].city.length()", []any{int64(6)}},
		{"Strings/ToChars", "Patient", "pat-fp-1", "Patient.name[0].family.toChars()", []any{"L", "i", "n"}},
		{"Strings/Split", "Patient", "pat-fp-1", "Patient.birthDate.split('-')", []any{"1985", "04", "12"}},
		{"Strings/Join", "Patient", "pat-fp-1", "Patient.name[0].given.join(' ')", []any{"Mei Hua"}},
		{"Strings/ConcatPlus", "Patient", "pat-fp-1", "Patient.name[0].given[0] + ' ' + Patient.name[0].family", []any{"Mei Lin"}},
		{"Strings/ConcatAmpersandWithEmpty", "Patient", "pat-fp-1", "Patient.name[0].family & Patient.photo & '!'", []any{"Lin!"}},

		// 6. Math & Arithmetic (+, -, *, /, div, mod, abs, ceiling, floor, truncate, round, sqrt, power)
		{"Math/Addition", "Patient", "pat-fp-1", "10 + 25", []any{int64(35)}},
		{"Math/Subtraction", "Patient", "pat-fp-1", "50 - 18", []any{int64(32)}},
		{"Math/Multiplication", "Patient", "pat-fp-1", "Patient.multipleBirthInteger * 6", []any{int64(12)}},
		{"Math/FloatDivision", "Patient", "pat-fp-1", "15 / 2", []any{7.5}},
		{"Math/IntDiv", "Patient", "pat-fp-1", "17 div 5", []any{int64(3)}},
		{"Math/Modulo", "Patient", "pat-fp-1", "17 mod 5", []any{int64(2)}},
		{"Math/PrecedenceMulBeforeAdd", "Patient", "pat-fp-1", "2 + 3 * 4", []any{int64(14)}},
		{"Math/ParenthesesOverridePrecedence", "Patient", "pat-fp-1", "(2 + 3) * 4", []any{int64(20)}},
		{"Math/AbsNegative", "Patient", "pat-fp-1", "(-42).abs()", []any{int64(42)}},
		{"Math/Ceiling", "Observation", "obs-fp-bp", "Observation.valueQuantity.value.ceiling()", []any{int64(129)}},
		{"Math/Floor", "Observation", "obs-fp-bp", "Observation.valueQuantity.value.floor()", []any{int64(128)}},
		{"Math/Truncate", "Observation", "obs-fp-bp", "Observation.valueQuantity.value.truncate()", []any{int64(128)}},
		{"Math/Round", "Observation", "obs-fp-bp", "Observation.valueQuantity.value.round()", []any{int64(129)}},
		{"Math/Sqrt", "Patient", "pat-fp-1", "81.sqrt()", []any{int64(9)}},
		{"Math/Power", "Patient", "pat-fp-1", "2.power(5)", []any{int64(32)}},

		// 7. Comparison, Equality, Equivalence & Membership (=, !=, ~, !~, >, >=, <, <=, in, contains)
		{"Compare/EqualTrue", "Patient", "pat-fp-1", "Patient.gender = 'female'", []any{true}},
		{"Compare/NotEqualTrue", "Patient", "pat-fp-1", "Patient.gender != 'male'", []any{true}},
		{"Compare/EquivalentCaseInsensitive", "Patient", "pat-fp-1", "Patient.name[0].family ~ '  lIn  '", []any{true}},
		{"Compare/NotEquivalent", "Patient", "pat-fp-1", "Patient.name[0].family !~ 'Smith'", []any{true}},
		{"Compare/EmptyEqualsEmptyIsEmpty", "Patient", "pat-fp-1", "Patient.photo = {}", nil},
		{"Compare/EmptyEquivalentEmptyIsTrue", "Patient", "pat-fp-1", "Patient.photo ~ {}", []any{true}},
		{"Compare/GreaterThanComponentBP", "Observation", "obs-fp-bp", "Observation.component.where(code.coding.code = '8480-6').valueQuantity.value >= 140", []any{true}},
		{"Compare/LessThanDiastolicBP", "Observation", "obs-fp-bp", "Observation.component.where(code.coding.code = '8462-4').valueQuantity.value < 90", []any{true}},
		{"Membership/InOperatorTrue", "Patient", "pat-fp-1", "'Hua' in Patient.name.given", []any{true}},
		{"Membership/InOperatorFalse", "Patient", "pat-fp-1", "'Bob' in Patient.name.given", []any{false}},
		{"Membership/ContainsOperatorTrue", "Patient", "pat-fp-1", "Patient.name.given contains 'Mimi'", []any{true}},

		// 8. Boolean Logic & Control Flow (and, or, xor, implies, not, iif)
		{"Logic/AndTrue", "Patient", "pat-fp-1", "Patient.active and (Patient.gender = 'female')", []any{true}},
		{"Logic/AndFalse", "Patient", "pat-fp-1", "Patient.active and (Patient.gender = 'male')", []any{false}},
		{"Logic/OrTrue", "Patient", "pat-fp-1", "(Patient.gender = 'male') or (Patient.gender = 'female')", []any{true}},
		{"Logic/XorTrue", "Patient", "pat-fp-1", "true xor false", []any{true}},
		{"Logic/XorFalse", "Patient", "pat-fp-1", "true xor true", []any{false}},
		{"Logic/ImpliesFalseAntecedent", "Patient", "pat-fp-1", "(Patient.gender = 'male') implies (Patient.photo.exists())", []any{true}},
		{"Logic/ImpliesTrueAntecedent", "Patient", "pat-fp-1", "(Patient.gender = 'female') implies Patient.active", []any{true}},
		{"Logic/NotTrue", "Patient", "pat-fp-1", "Patient.deceasedBoolean.not()", []any{true}},
		{"Logic/IifTrueBranch", "Patient", "pat-fp-1", "iif(Patient.active, 'ACTIVE_PAT', 'INACTIVE_PAT')", []any{"ACTIVE_PAT"}},
		{"Logic/IifFalseBranch", "Patient", "pat-fp-1", "iif(Patient.deceasedBoolean, 'DECEASED', 'ALIVE')", []any{"ALIVE"}},

		// 9. Type Conversions & Reflection (toInteger, toDecimal, toString, toBoolean, convertsTo*, hasValue, is, as)
		{"Convert/StringToInt", "Patient", "pat-fp-1", "'42'.toInteger()", []any{int64(42)}},
		{"Convert/ConvertsToIntFalse", "Patient", "pat-fp-1", "'abc'.convertsToInteger()", []any{false}},
		{"Convert/IntToString", "Patient", "pat-fp-1", "Patient.multipleBirthInteger.toString()", []any{"2"}},
		{"Convert/StringToBoolean", "Patient", "pat-fp-1", "'true'.toBoolean()", []any{true}},
		{"Convert/HasValueTrue", "Patient", "pat-fp-1", "Patient.gender.hasValue()", []any{true}},
		{"Convert/HasValueFalseOnEmpty", "Patient", "pat-fp-1", "Patient.photo.hasValue()", []any{false}},
		{"Convert/IsStringTrue", "Patient", "pat-fp-1", "Patient.gender is string", []any{true}},
		{"Convert/AsString", "Patient", "pat-fp-1", "(Patient.gender as string)", []any{"female"}},

		// 10. FHIR R4 Extensions, Environment Variables & Contained Reference Resolution
		{"FHIR/ExtensionByURL", "Patient", "pat-fp-1", "Patient.extension('http://hl7.org/fhir/StructureDefinition/patient-mothersMaidenName').valueString", []any{"Chen"}},
		{"FHIR/ExtensionPolymorphicValue", "Patient", "pat-fp-1", "Patient.extension('http://hl7.org/fhir/us/core/StructureDefinition/us-core-race').value", []any{"Asian"}},
		{"FHIR/ResolveContainedOrganization", "Patient", "pat-fp-1", "Patient.managingOrganization.resolve().name", []any{"Contained General Hospital"}},
		{"FHIR/EnvVarLoinc", "Observation", "obs-fp-bp", "Observation.code.coding.where(system = %loinc).code", []any{"85354-9"}},
		{"FHIR/EnvVarUcum", "Observation", "obs-fp-bp", "Observation.valueQuantity.system = %ucum", []any{true}},
	}

	if len(vectors) < 110 {
		t.Fatalf("expected at least 110 FHIRPath R4 conformance vectors, got %d", len(vectors))
	}

	extractResults := func(t *testing.T, body []byte) []any {
		t.Helper()
		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			t.Fatalf("failed to unmarshal $fhirpath Parameters response: %v (%s)", err, string(body))
		}
		if rt, _ := params["resourceType"].(string); rt != "Parameters" {
			t.Fatalf("expected Parameters resourceType, got %q (%s)", rt, string(body))
		}
		pList, _ := params["parameter"].([]any)
		var out []any
		for _, item := range pList {
			pm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if name, _ := pm["name"].(string); name != "result" {
				continue
			}
			switch {
			case pm["valueBoolean"] != nil:
				out = append(out, pm["valueBoolean"].(bool))
			case pm["valueInteger"] != nil:
				out = append(out, int64(pm["valueInteger"].(float64)))
			case pm["valueDecimal"] != nil:
				out = append(out, pm["valueDecimal"].(float64))
			case pm["valueString"] != nil:
				out = append(out, pm["valueString"].(string))
			case pm["resource"] != nil:
				out = append(out, pm["resource"])
			}
		}
		return out
	}

	for _, tc := range vectors {
		t.Run(tc.name, func(t *testing.T) {
			u := "/fhir/r4/" + tenant + "/" + tc.resType + "/" + tc.resID + "/$fhirpath?expression=" + url.QueryEscape(tc.expression)
			req := httptest.NewRequest(http.MethodGet, u, nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s returned %d: %s", u, rec.Code, rec.Body.String())
			}
			got := extractResults(t, rec.Body.Bytes())
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("expression %q:\n  got:  %#v\n  want: %#v", tc.expression, got, tc.want)
			}
		})
	}

	// Verify POST /fhir/r4/{tenant}/$fhirpath with inline Parameters payload
	t.Run("POST_InlineParametersEvaluation", func(t *testing.T) {
		postBody := []byte(`{
			"resourceType": "Parameters",
			"parameter": [
				{"name": "expression", "valueString": "Patient.name.where(use = 'official').given.join(' ') + ' ' + Patient.name.where(use = 'official').family.upper()"},
				{"name": "resource", "resource": ` + string(patientJSON) + `}
			]
		}`)
		req := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/$fhirpath", bytes.NewReader(postBody))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /$fhirpath returned %d: %s", rec.Code, rec.Body.String())
		}
		got := extractResults(t, rec.Body.Bytes())
		want := []any{"Mei Hua LIN"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("POST /$fhirpath got %#v, want %#v", got, want)
		}
	})
}
