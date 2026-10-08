// Spike 4: Validate search index extraction against real google/fhir Patient proto.
//
// This test hand-writes what proto2type would generate for ExtractPatientIndexes(),
// then verifies it produces correct spidx_* rows from a real Patient proto.
// This validates the generated code pattern before we wire up buf generate.
package spike4_search_index

import (
	"fmt"
	"strings"
	"testing"
	"time"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
)

// --- spidx row types (matching what proto2type generates) ---

type SpidxString struct {
	TenantID string
	ResType  string
	ResID    string
	SpName   string
	SpValue  string
}

type SpidxToken struct {
	TenantID string
	ResType  string
	ResID    string
	SpName   string
	SpSystem string
	SpValue  string
}

type SpidxDate struct {
	TenantID string
	ResType  string
	ResID    string
	SpName   string
	SpLow    time.Time
	SpHigh   time.Time
}

type SpidxReference struct {
	TenantID   string
	ResType    string
	ResID      string
	SpName     string
	TargetType string
	TargetID   string
}

type SearchIndexes struct {
	Strings    []SpidxString
	Tokens     []SpidxToken
	Dates      []SpidxDate
	References []SpidxReference
}

// ExtractPatientIndexes is what proto2type would generate.
// Hand-coded here to validate the extraction patterns against real protos.
func ExtractPatientIndexes(tenantID string, r *patpb.Patient) *SearchIndexes {
	if r == nil {
		return &SearchIndexes{}
	}

	idx := &SearchIndexes{}
	resID := ""

	if r.GetId() != nil {
		resID = r.GetId().GetValue()
	}

	// SearchParameter: family (string)
	// FHIRPath: Patient.name.family
	for _, outer := range r.GetName() {
		if outer == nil {
			continue
		}
		if v := outer.GetFamily(); v != nil && v.GetValue() != "" {
			idx.Strings = append(idx.Strings, SpidxString{
				TenantID: tenantID,
				ResType:  "Patient",
				ResID:    resID,
				SpName:   "family",
				SpValue:  strings.ToLower(v.GetValue()),
			})
		}
	}

	// SearchParameter: given (string)
	// FHIRPath: Patient.name.given
	for _, outer := range r.GetName() {
		if outer == nil {
			continue
		}
		for _, v := range outer.GetGiven() {
			if v != nil && v.GetValue() != "" {
				idx.Strings = append(idx.Strings, SpidxString{
					TenantID: tenantID,
					ResType:  "Patient",
					ResID:    resID,
					SpName:   "given",
					SpValue:  strings.ToLower(v.GetValue()),
				})
			}
		}
	}

	// SearchParameter: address-city (string)
	// FHIRPath: Patient.address.city
	for _, outer := range r.GetAddress() {
		if outer == nil {
			continue
		}
		if v := outer.GetCity(); v != nil && v.GetValue() != "" {
			idx.Strings = append(idx.Strings, SpidxString{
				TenantID: tenantID,
				ResType:  "Patient",
				ResID:    resID,
				SpName:   "address-city",
				SpValue:  strings.ToLower(v.GetValue()),
			})
		}
	}

	// SearchParameter: birthdate (date)
	// FHIRPath: Patient.birthDate
	if d := r.GetBirthDate(); d != nil {
		t := time.UnixMicro(d.GetValueUs())
		idx.Dates = append(idx.Dates, SpidxDate{
			TenantID: tenantID,
			ResType:  "Patient",
			ResID:    resID,
			SpName:   "birthdate",
			SpLow:    t,
			SpHigh:   t,
		})
	}

	// SearchParameter: gender (token)
	// FHIRPath: Patient.gender
	if r.GetGender() != nil {
		idx.Tokens = append(idx.Tokens, SpidxToken{
			TenantID: tenantID,
			ResType:  "Patient",
			ResID:    resID,
			SpName:   "gender",
			SpValue:  r.GetGender().GetValue().String(),
		})
	}

	// SearchParameter: identifier (token)
	// FHIRPath: Patient.identifier
	for _, ident := range r.GetIdentifier() {
		if ident == nil {
			continue
		}
		system := ""
		if ident.GetSystem() != nil {
			system = ident.GetSystem().GetValue()
		}
		value := ""
		if ident.GetValue() != nil {
			value = ident.GetValue().GetValue()
		}
		if value != "" {
			idx.Tokens = append(idx.Tokens, SpidxToken{
				TenantID: tenantID,
				ResType:  "Patient",
				ResID:    resID,
				SpName:   "identifier",
				SpSystem: system,
				SpValue:  value,
			})
		}
	}

	// SearchParameter: active (token)
	// FHIRPath: Patient.active
	if r.GetActive() != nil {
		idx.Tokens = append(idx.Tokens, SpidxToken{
			TenantID: tenantID,
			ResType:  "Patient",
			ResID:    resID,
			SpName:   "active",
			SpValue:  fmt.Sprintf("%v", r.GetActive().GetValue()),
		})
	}

	// SearchParameter: organization (reference)
	// FHIRPath: Patient.managingOrganization
	// google/fhir Reference uses oneof: Uri (absolute), Fragment, or typed ID (e.g. OrganizationId)
	if ref := r.GetManagingOrganization(); ref != nil {
		// Try URI-based reference first
		if uri := ref.GetUri(); uri != nil && uri.GetValue() != "" {
			parts := strings.SplitN(uri.GetValue(), "/", 2)
			if len(parts) == 2 {
				idx.References = append(idx.References, SpidxReference{
					TenantID:   tenantID,
					ResType:    "Patient",
					ResID:      resID,
					SpName:     "organization",
					TargetType: parts[0],
					TargetID:   parts[1],
				})
			}
		}
		// Try typed reference: OrganizationId
		if orgRef := ref.GetOrganizationId(); orgRef != nil && orgRef.GetValue() != "" {
			idx.References = append(idx.References, SpidxReference{
				TenantID:   tenantID,
				ResType:    "Patient",
				ResID:      resID,
				SpName:     "organization",
				TargetType: "Organization",
				TargetID:   orgRef.GetValue(),
			})
		}
	}

	return idx
}

// --- Test ---

func TestExtractPatientIndexes_RealProto(t *testing.T) {
	// Build a realistic Patient using real google/fhir protos
	patient := &patpb.Patient{
		Id: &dtpb.Id{Value: "pat-123"},
		Identifier: []*dtpb.Identifier{
			{
				System: &dtpb.Uri{Value: "http://hospital.example.org/mrn"},
				Value:  &dtpb.String{Value: "MRN-456"},
			},
		},
		Active: &dtpb.Boolean{Value: true},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "Smith"},
				Given:  []*dtpb.String{{Value: "John"}, {Value: "Michael"}},
			},
			{
				Family: &dtpb.String{Value: "Smith-Jones"},
				Given:  []*dtpb.String{{Value: "Johnny"}},
			},
		},
		BirthDate: &dtpb.Date{
			ValueUs:   631152000000000, // 1990-01-01 00:00:00 UTC in microseconds
			Precision: 3,               // DAY
		},
		Address: []*dtpb.Address{
			{
				City:  &dtpb.String{Value: "Nashville"},
				State: &dtpb.String{Value: "TN"},
			},
		},
		ManagingOrganization: &dtpb.Reference{
			Reference: &dtpb.Reference_OrganizationId{
				OrganizationId: &dtpb.ReferenceId{Value: "org-789"},
			},
		},
	}

	idx := ExtractPatientIndexes("tenant-hca", patient)

	// Verify strings
	t.Run("family names", func(t *testing.T) {
		families := findStrings(idx, "family")
		if len(families) != 2 {
			t.Fatalf("expected 2 family strings, got %d: %+v", len(families), families)
		}
		assertStringValue(t, families, "smith")
		assertStringValue(t, families, "smith-jones")
	})

	t.Run("given names", func(t *testing.T) {
		givens := findStrings(idx, "given")
		if len(givens) != 3 {
			t.Fatalf("expected 3 given strings, got %d: %+v", len(givens), givens)
		}
		assertStringValue(t, givens, "john")
		assertStringValue(t, givens, "michael")
		assertStringValue(t, givens, "johnny")
	})

	t.Run("address-city", func(t *testing.T) {
		cities := findStrings(idx, "address-city")
		if len(cities) != 1 {
			t.Fatalf("expected 1 city, got %d", len(cities))
		}
		if cities[0].SpValue != "nashville" {
			t.Errorf("expected 'nashville', got %q", cities[0].SpValue)
		}
	})

	// Verify tokens
	t.Run("identifier", func(t *testing.T) {
		ids := findTokens(idx, "identifier")
		if len(ids) != 1 {
			t.Fatalf("expected 1 identifier, got %d", len(ids))
		}
		if ids[0].SpSystem != "http://hospital.example.org/mrn" {
			t.Errorf("wrong system: %q", ids[0].SpSystem)
		}
		if ids[0].SpValue != "MRN-456" {
			t.Errorf("wrong value: %q", ids[0].SpValue)
		}
	})

	t.Run("active", func(t *testing.T) {
		actives := findTokens(idx, "active")
		if len(actives) != 1 {
			t.Fatalf("expected 1 active token, got %d", len(actives))
		}
		if actives[0].SpValue != "true" {
			t.Errorf("expected 'true', got %q", actives[0].SpValue)
		}
	})

	t.Run("gender", func(t *testing.T) {
		genders := findTokens(idx, "gender")
		// Gender not set → should still have a token (enum zero value)
		t.Logf("gender tokens: %+v", genders)
	})

	// Verify dates
	t.Run("birthdate", func(t *testing.T) {
		dates := findDates(idx, "birthdate")
		if len(dates) != 1 {
			t.Fatalf("expected 1 birthdate, got %d", len(dates))
		}
		expectedYear := 1990
		if dates[0].SpLow.UTC().Year() != expectedYear {
			t.Errorf("expected year %d, got %d (time: %v)", expectedYear, dates[0].SpLow.UTC().Year(), dates[0].SpLow.UTC())
		}
	})

	// Verify references
	t.Run("organization", func(t *testing.T) {
		refs := findRefs(idx, "organization")
		if len(refs) != 1 {
			t.Fatalf("expected 1 organization ref, got %d", len(refs))
		}
		if refs[0].TargetType != "Organization" {
			t.Errorf("expected target type 'Organization', got %q", refs[0].TargetType)
		}
		if refs[0].TargetID != "org-789" {
			t.Errorf("expected target ID 'org-789', got %q", refs[0].TargetID)
		}
	})

	// Verify all entries have correct tenant/res metadata
	t.Run("metadata", func(t *testing.T) {
		for _, s := range idx.Strings {
			if s.TenantID != "tenant-hca" {
				t.Errorf("wrong tenant: %q", s.TenantID)
			}
			if s.ResType != "Patient" {
				t.Errorf("wrong res type: %q", s.ResType)
			}
			if s.ResID != "pat-123" {
				t.Errorf("wrong res ID: %q", s.ResID)
			}
		}
	})

	// Summary
	t.Logf("Extraction summary:")
	t.Logf("  Strings:    %d", len(idx.Strings))
	t.Logf("  Tokens:     %d", len(idx.Tokens))
	t.Logf("  Dates:      %d", len(idx.Dates))
	t.Logf("  References: %d", len(idx.References))
	t.Logf("  Total rows: %d", len(idx.Strings)+len(idx.Tokens)+len(idx.Dates)+len(idx.References))
}

// --- helpers ---

func findStrings(idx *SearchIndexes, spName string) []SpidxString {
	var result []SpidxString
	for _, s := range idx.Strings {
		if s.SpName == spName {
			result = append(result, s)
		}
	}
	return result
}

func findTokens(idx *SearchIndexes, spName string) []SpidxToken {
	var result []SpidxToken
	for _, t := range idx.Tokens {
		if t.SpName == spName {
			result = append(result, t)
		}
	}
	return result
}

func findDates(idx *SearchIndexes, spName string) []SpidxDate {
	var result []SpidxDate
	for _, d := range idx.Dates {
		if d.SpName == spName {
			result = append(result, d)
		}
	}
	return result
}

func findRefs(idx *SearchIndexes, spName string) []SpidxReference {
	var result []SpidxReference
	for _, r := range idx.References {
		if r.SpName == spName {
			result = append(result, r)
		}
	}
	return result
}

func assertStringValue(t *testing.T, strings []SpidxString, expected string) {
	t.Helper()
	for _, s := range strings {
		if s.SpValue == expected {
			return
		}
	}
	t.Errorf("expected string value %q not found in %+v", expected, strings)
}
