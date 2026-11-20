package ccda_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"hegel.dev/go/hegel"

	"github.com/flint-fhir/flint/pkg/ccda"
	"github.com/flint-fhir/flint/pkg/validation"
)

func TestProperty_CCDAFHIRBidirectionalRoundTrip(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())
	fixedNow := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

	hegel.Test(t, func(ht *hegel.T) {
		patID := hegel.Draw(ht, hegel.FromRegex(`pat-[a-z0-9]{4,8}`, true))
		family := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{2,10}`, true))
		given := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{2,10}`, true))
		gender := hegel.Draw(ht, hegel.SampledFrom([]string{"male", "female", "other"}))
		year := hegel.Draw(ht, hegel.Integers(1940, 2020))
		month := hegel.Draw(ht, hegel.Integers(1, 12))
		day := hegel.Draw(ht, hegel.Integers(1, 28))
		dob := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
		hr := hegel.Draw(ht, hegel.Integers(45, 160))
		glucose := hegel.Draw(ht, hegel.Integers(60, 280))

		pat := map[string]any{
			"resourceType": "Patient",
			"id":           patID,
			"gender":       gender,
			"birthDate":    dob,
			"identifier":   []any{map[string]any{"system": "urn:oid:2.16.840.1.113883.19.5", "value": patID}},
			"name":         []any{map[string]any{"use": "official", "family": family, "given": []any{given}}},
		}
		resources := []map[string]any{
			{
				"resourceType":       "Condition",
				"id":                 "cond-" + patID,
				"clinicalStatus":     map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}}},
				"verificationStatus": map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}}},
				"code":               map[string]any{"coding": []any{map[string]any{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertension"}}},
				"subject":            map[string]any{"reference": "Patient/" + patID},
				"onsetDateTime":      "2023-05-01",
			},
			{
				"resourceType": "AllergyIntolerance",
				"id":           "alg-" + patID,
				"code":         map[string]any{"coding": []any{map[string]any{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980", "display": "Penicillin G"}}},
				"patient":      map[string]any{"reference": "Patient/" + patID},
			},
			{
				"resourceType":              "MedicationRequest",
				"id":                        "med-" + patID,
				"status":                    "active",
				"intent":                    "plan",
				"medicationCodeableConcept": map[string]any{"coding": []any{map[string]any{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "314076", "display": "Lisinopril"}}},
				"subject":                   map[string]any{"reference": "Patient/" + patID},
			},
			{
				"resourceType":       "Immunization",
				"id":                 "imm-" + patID,
				"status":             "completed",
				"vaccineCode":        map[string]any{"coding": []any{map[string]any{"system": "http://hl7.org/fhir/sid/cvx", "code": "141", "display": "Influenza"}}},
				"patient":            map[string]any{"reference": "Patient/" + patID},
				"occurrenceDateTime": "2025-10-01",
			},
			{
				"resourceType":  "Observation",
				"id":            "vit-" + patID,
				"status":        "final",
				"category":      []any{map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
				"code":          map[string]any{"coding": []any{map[string]any{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}}},
				"subject":       map[string]any{"reference": "Patient/" + patID},
				"valueQuantity": map[string]any{"value": float64(hr), "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"},
			},
			{
				"resourceType":  "Observation",
				"id":            "lab-" + patID,
				"status":        "final",
				"category":      []any{map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}}}},
				"code":          map[string]any{"coding": []any{map[string]any{"system": "http://loinc.org", "code": "2345-7", "display": "Glucose"}}},
				"subject":       map[string]any{"reference": "Patient/" + patID},
				"valueQuantity": map[string]any{"value": float64(glucose), "unit": "mg/dL", "system": "http://unitsofmeasure.org", "code": "mg/dL"},
			},
			{
				"resourceType":      "Procedure",
				"id":                "proc-" + patID,
				"status":            "completed",
				"code":              map[string]any{"coding": []any{map[string]any{"system": "http://snomed.info/sct", "code": "29303009", "display": "ECG"}}},
				"subject":           map[string]any{"reference": "Patient/" + patID},
				"performedDateTime": "2026-01-15",
			},
			{
				"resourceType": "Encounter",
				"id":           "enc-" + patID,
				"status":       "finished",
				"class":        map[string]any{"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
				"subject":      map[string]any{"reference": "Patient/" + patID},
				"period":       map[string]any{"start": "2026-01-15T09:00:00Z"},
			},
		}

		xmlBytes, err := ccda.ConvertFHIRToCCDA(pat, resources, ccda.ExportOptions{Now: fixedNow})
		if err != nil {
			ht.Fatalf("ConvertFHIRToCCDA failed: %v", err)
		}
		imp, err := ccda.ConvertToFHIR(xmlBytes, ccda.ImportOptions{Mode: "transaction", Now: fixedNow})
		if err != nil {
			ht.Fatalf("ConvertToFHIR failed: %v", err)
		}
		if imp.ResourceCounts["Patient"] != 1 ||
			imp.ResourceCounts["Condition"] != 1 ||
			imp.ResourceCounts["AllergyIntolerance"] != 1 ||
			imp.ResourceCounts["MedicationRequest"] != 1 ||
			imp.ResourceCounts["Immunization"] != 1 ||
			imp.ResourceCounts["Procedure"] != 1 ||
			imp.ResourceCounts["Encounter"] != 1 ||
			imp.ResourceCounts["Observation"] < 2 {
			ht.Fatalf("lost section resources on C-CDA round-trip: counts=%+v", imp.ResourceCounts)
		}
		entries := imp.Bundle["entry"].([]any)
		for _, e := range entries {
			rm := e.(map[string]any)["resource"].(map[string]any)
			rt := rm["resourceType"].(string)
			out := engine.ValidateResource(rt, rm)
			if !out.IsValid() {
				ht.Fatalf("invalid round-tripped %s: %+v", rt, out.Issues)
			}
			if rt == "Patient" {
				if rm["birthDate"] != dob {
					ht.Fatalf("birthDate mismatch: got %v want %s", rm["birthDate"], dob)
				}
			}
		}
	}, hegel.WithTestCases(40))
}

func TestProperty_CCDAXXERejection(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		ent := hegel.Draw(ht, hegel.FromRegex(`[a-z]{2,8}`, true))
		target := hegel.Draw(ht, hegel.SampledFrom([]string{
			"file:///etc/passwd",
			"http://169.254.169.254/latest/meta-data/",
		}))
		payload := fmt.Sprintf(`<?xml version="1.0"?><!DOCTYPE ClinicalDocument [<!ENTITY %s SYSTEM %q>]><ClinicalDocument xmlns="urn:hl7-org:v3"><title>&%s;</title></ClinicalDocument>`, ent, target, ent)
		if _, err := ccda.Parse([]byte(strings.TrimSpace(payload))); err == nil {
			ht.Fatalf("expected Parse to reject DOCTYPE/ENTITY payload")
		}
	}, hegel.WithTestCases(40))
}
