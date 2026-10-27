package server

import (
	"testing"
	"time"

	"github.com/flint-fhir/flint/store/postgres"
)

func TestSubscriptionMatchesResourceWithMeta_AllCriteriaFamilies(t *testing.T) {
	obsDate := time.Date(2026, 2, 10, 14, 0, 0, 0, time.UTC)
	lastUpdated := time.Date(2026, 2, 10, 15, 0, 0, 0, time.UTC)

	idx := &postgres.SearchIndexes{
		Tokens: []postgres.SpidxToken{
			{SpName: "code", SpSystem: "http://loinc.org", SpValue: "8867-4"},
			{SpName: "category", SpSystem: "http://terminology.hl7.org/CodeSystem/observation-category", SpValue: "vital-signs"},
			{SpName: "status", SpValue: "final"},
			{SpName: "_profile", SpValue: "http://hl7.org/fhir/us/core/StructureDefinition/us-core-heart-rate"},
		},
		Strings: []postgres.SpidxString{
			{SpName: "device-name", SpValue: "Bedside Cardiac Monitor"},
		},
		Dates: []postgres.SpidxDate{
			{SpName: "date", SpLow: obsDate, SpHigh: obsDate},
		},
		Quantities: []postgres.SpidxQuantity{
			{SpName: "value-quantity", SpSystem: "http://unitsofmeasure.org", SpCode: "/min", SpValue: 112},
			{SpName: "comp-cvq:http://loinc.org|8480-6", SpSystem: "http://unitsofmeasure.org", SpCode: "mm[Hg]", SpValue: 145},
		},
		References: []postgres.SpidxReference{
			{SpName: "patient", TargetType: "Patient", TargetID: "pat-42"},
			{SpName: "subject", TargetType: "Patient", TargetID: "pat-42"},
		},
		URIs: []postgres.SpidxURI{
			{SpName: "_profile", SpValue: "http://hl7.org/fhir/us/core/StructureDefinition/us-core-heart-rate"},
			{SpName: "_source", SpValue: "https://ehr.hospital.org/Epic/ICU"},
		},
	}

	tests := []struct {
		name     string
		criteria string
		want     bool
	}{
		{
			name:     "Quantity gt + token system|code + reference :TargetType",
			criteria: "Observation?code=http://loinc.org|8867-4&value-quantity=gt100|http://unitsofmeasure.org|/min&subject:Patient=pat-42",
			want:     true,
		},
		{
			name:     "Quantity out of range fails",
			criteria: "Observation?value-quantity=lt90",
			want:     false,
		},
		{
			name:     "Date range ge and le matches",
			criteria: "Observation?date=ge2026-02-01&date=le2026-02-28&_lastUpdated=ge2026-02-10T00:00:00Z",
			want:     true,
		},
		{
			name:     "Date out of range fails",
			criteria: "Observation?date=lt2026-01-01",
			want:     false,
		},
		{
			name:     "Token :not modifier excludes matching status",
			criteria: "Observation?status:not=final",
			want:     false,
		},
		{
			name:     "Token :not modifier passes when non-matching",
			criteria: "Observation?status:not=cancelled,entered-in-error",
			want:     true,
		},
		{
			name:     "String :contains and :exact modifiers",
			criteria: "Observation?device-name:contains=cardiac&device-name:exact=Bedside+Cardiac+Monitor",
			want:     true,
		},
		{
			name:     "URI :below and :above modifiers on _profile and _source",
			criteria: "Observation?_profile:below=http://hl7.org/fhir/us/core/StructureDefinition/&_source:above=https://ehr.hospital.org/Epic/ICU/bed-3",
			want:     true,
		},
		{
			name:     "Composite code-value-quantity and component-code-value-quantity",
			criteria: "Observation?code-value-quantity=http://loinc.org|8867-4$gt100&component-code-value-quantity=http://loinc.org|8480-6$ge140",
			want:     true,
		},
		{
			name:     ":missing=false on present param and :missing=true on absent param",
			criteria: "Observation?code:missing=false&performer:missing=true",
			want:     true,
		},
		{
			name:     ":missing=true on present param fails",
			criteria: "Observation?code:missing=true",
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := subscriptionMatchesResourceWithMeta(tc.criteria, "Observation", "obs-1", lastUpdated, idx)
			if got != tc.want {
				t.Errorf("subscriptionMatchesResourceWithMeta(%q) = %v, want %v", tc.criteria, got, tc.want)
			}
		})
	}
}
