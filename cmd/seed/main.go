// Command seed populates a running Flint FHIR R4 server with a realistic,
// deterministic longitudinal clinical cohort covering all 26 US Core v6.1.0 &
// v7.0.0 (USCDI v3 & v4) and HL7 IPA resource types using an atomic FHIR R4
// transaction Bundle.
//
// Usage:
//
//	go run ./cmd/seed -url http://localhost:8080 -tenant demo
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config configures the Flint clinical cohort seeder.
type Config struct {
	BaseURL     string
	Tenant      string
	BearerToken string
	HTTPClient  *http.Client
	Out         io.Writer
}

func parseSeedConfig(args []string, getenv func(string) string) (Config, error) {
	defaultURL := getenv("FLINT_URL")
	if defaultURL == "" {
		defaultURL = "http://localhost:8080"
	}
	defaultTenant := getenv("FLINT_TENANT")
	if defaultTenant == "" {
		defaultTenant = "demo"
	}
	defaultToken := getenv("FLINT_TOKEN")

	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	urlFlag := fs.String("url", defaultURL, "Flint server base URL")
	tenantFlag := fs.String("tenant", defaultTenant, "Target FHIR tenant ID")
	tokenFlag := fs.String("token", defaultToken, "Optional SMART on FHIR Bearer token")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	return Config{
		BaseURL:     *urlFlag,
		Tenant:      *tenantFlag,
		BearerToken: *tokenFlag,
		HTTPClient:  &http.Client{Timeout: 30 * time.Second},
		Out:         os.Stdout,
	}, nil
}

func main() {
	cfg, err := parseSeedConfig(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed flags: %v\n", err)
		os.Exit(2)
	}

	if err := Run(context.Background(), cfg); err != nil {
		fmt.Fprintf(os.Stderr, "seed failed: %v\n", err)
		os.Exit(1)
	}
}

// Run executes the clinical cohort transaction Bundle against the target Flint server
// and prints a summary with copy-pasteable curl exploration commands.
func Run(ctx context.Context, cfg Config) error {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	tenant := strings.TrimSpace(cfg.Tenant)
	if tenant == "" {
		tenant = "demo"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}

	bundleBytes, err := CohortTransactionBundleJSON()
	if err != nil {
		return fmt.Errorf("build cohort bundle: %w", err)
	}

	endpoint := fmt.Sprintf("%s/fhir/r4/%s", baseURL, tenant)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bundleBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/fhir+json")
	if tok := strings.TrimSpace(cfg.BearerToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s returned HTTP %d: %s", endpoint, resp.StatusCode, string(respBody))
	}

	var txResp struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Entry        []struct {
			FullURL  string `json:"fullUrl"`
			Response struct {
				Status   string `json:"status"`
				Location string `json:"location"`
				ETag     string `json:"etag"`
			} `json:"response"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(respBody, &txResp); err != nil {
		return fmt.Errorf("decode transaction-response: %w", err)
	}

	fmt.Fprintf(out, "🔥 Seeded %d FHIR R4 resources (all 26 US Core v6.1.0 & v7.0.0 + HL7 IPA profiles) into tenant %q (%s)\n\n", len(txResp.Entry), tenant, endpoint)
	for _, e := range txResp.Entry {
		fmt.Fprintf(out, "  • %-42s  %-12s  ETag: %s\n", e.FullURL, e.Response.Status, e.Response.ETag)
	}

	fmt.Fprintf(out, "\nTry these queries:\n")
	fmt.Fprintf(out, "  # 1. Full longitudinal patient chart ($everything)\n")
	fmt.Fprintf(out, "  curl -s %s/Patient/pat-alice-001/$everything | jq .\n\n", endpoint)
	fmt.Fprintf(out, "  # 2. Search Observations by LOINC code + quantity + _include patient\n")
	fmt.Fprintf(out, "  curl -s '%s/Observation?code=8867-4&value-quantity=gt70&_include=Observation:patient' | jq .\n\n", endpoint)
	fmt.Fprintf(out, "  # 3. Search active MedicationRequests for Alice\n")
	fmt.Fprintf(out, "  curl -s '%s/MedicationRequest?patient=Patient/pat-alice-001&intent=order' | jq .\n\n", endpoint)
	fmt.Fprintf(out, "  # 4. Run ONC (g)(10) & US Core v6.1.0 / v7.0.0 Conformance Certification ($certify)\n")
	fmt.Fprintf(out, "  curl -s -X POST %s/$certify | jq .\n", endpoint)

	return nil
}

// CohortTransactionBundleJSON returns a deterministic, idempotent FHIR R4 transaction Bundle
// covering all 26 US Core v6.1.0 & v7.0.0 (USCDI v3 & v4) and HL7 IPA resource types plus
// vital-signs, laboratory, smoking-status, SDOH, screening, preference, pregnancy, occupation,
// health-concern, implantable-device, clinical-note, and medicationReference sub-profiles.
func CohortTransactionBundleJSON() ([]byte, error) {
	entries := []map[string]any{
		// 1. Organizations (2) & Location (1)
		putEntry("Organization/org-flint-health", map[string]any{
			"resourceType": "Organization",
			"id":           "org-flint-health",
			"active":       true,
			"name":         "Flint Medical Center",
			"alias":        []string{"Flint Health System"},
			"identifier": []map[string]any{
				{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1122334455"},
			},
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/organization-type", "code": "prov", "display": "Healthcare Provider"},
					},
				},
			},
			"address": []map[string]any{
				{"line": []string{"500 Parnassus Ave"}, "city": "San Francisco", "state": "CA", "postalCode": "94143"},
			},
		}),
		putEntry("Location/loc-flint-cardiology", map[string]any{
			"resourceType": "Location",
			"id":           "loc-flint-cardiology",
			"status":       "active",
			"name":         "Flint Cardiology Ambulatory Clinic",
			"alias":        []string{"Flint Heart Center"},
			"mode":         "instance",
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/v3-RoleCode", "code": "CARD", "display": "Ambulatory Health Care Facilities; Clinic/Center; Rehabilitation: Cardiac Facilities"},
					},
				},
			},
			"address": map[string]any{
				"line":       []string{"500 Parnassus Ave, Suite 400"},
				"city":       "San Francisco",
				"state":      "CA",
				"postalCode": "94143",
			},
			"managingOrganization": map[string]any{
				"reference": "Organization/org-flint-health",
			},
		}),
		putEntry("Organization/org-metro-lab", map[string]any{
			"resourceType": "Organization",
			"id":           "org-metro-lab",
			"active":       true,
			"name":         "Metro Clinical Reference Laboratory",
			"identifier": []map[string]any{
				{"system": "http://hl7.org/fhir/sid/us-npi", "value": "5544332211"},
			},
			"address": []map[string]any{
				{"line": []string{"250 Park Ave"}, "city": "New York", "state": "NY", "postalCode": "10177"},
			},
		}),

		// 2. Practitioners (2)
		putEntry("Practitioner/prac-cardiology-1", map[string]any{
			"resourceType": "Practitioner",
			"id":           "prac-cardiology-1",
			"active":       true,
			"gender":       "female",
			"identifier": []map[string]any{
				{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1234567890"},
			},
			"name": []map[string]any{
				{"use": "official", "family": "Vance", "given": []string{"Eleanor"}, "prefix": []string{"Dr."}},
			},
			"telecom": []map[string]any{
				{"system": "email", "value": "eleanor.vance@cardiology.example.org", "use": "work"},
			},
			"address": []map[string]any{
				{
					"use":        "work",
					"line":       []string{"100 Health Plaza, Suite 300"},
					"city":       "Boston",
					"state":      "MA",
					"postalCode": "02115",
					"country":    "US",
				},
			},
		}),
		putEntry("Practitioner/prac-primary-2", map[string]any{
			"resourceType": "Practitioner",
			"id":           "prac-primary-2",
			"active":       true,
			"gender":       "male",
			"identifier": []map[string]any{
				{"system": "http://hl7.org/fhir/sid/us-npi", "value": "0987654321"},
			},
			"name": []map[string]any{
				{"use": "official", "family": "Chen", "given": []string{"Marcus"}, "prefix": []string{"Dr."}},
			},
			"telecom": []map[string]any{
				{"system": "email", "value": "marcus.chen@primarycare.example.org", "use": "work"},
			},
			"address": []map[string]any{
				{
					"use":        "work",
					"line":       []string{"100 Health Plaza, Suite 110"},
					"city":       "Boston",
					"state":      "MA",
					"postalCode": "02115",
					"country":    "US",
				},
			},
		}),

		// 3. Patient 1: Alice Wonderland (Hypertension, Cardiology ambulatory visit)
		putEntry("Patient/pat-alice-001", map[string]any{
			"resourceType": "Patient",
			"id":           "pat-alice-001",
			"active":       true,
			"gender":       "female",
			"birthDate":    "1985-04-12",
			"extension": []map[string]any{
				{
					"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
					"extension": []map[string]any{
						{
							"url": "ombCategory",
							"valueCoding": map[string]any{
								"system":  "urn:oid:2.16.840.1.113883.6.238",
								"code":    "2106-3",
								"display": "White",
							},
						},
						{
							"url":         "text",
							"valueString": "White",
						},
					},
				},
				{
					"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
					"extension": []map[string]any{
						{
							"url": "ombCategory",
							"valueCoding": map[string]any{
								"system":  "urn:oid:2.16.840.1.113883.6.238",
								"code":    "2186-5",
								"display": "Not Hispanic or Latino",
							},
						},
						{
							"url":         "text",
							"valueString": "Not Hispanic or Latino",
						},
					},
				},
				{
					"url":       "http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex",
					"valueCode": "F",
				},
				{
					"url":       "http://hl7.org/fhir/us/core/StructureDefinition/us-core-sex",
					"valueCode": "248152002",
				},
				{
					"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-tribal-affiliation",
					"extension": []map[string]any{
						{
							"url": "tribalAffiliation",
							"valueCodeableConcept": map[string]any{
								"coding": []map[string]any{
									{
										"system":  "http://terminology.hl7.org/CodeSystem/v3-TribalEntityUS",
										"code":    "91",
										"display": "Eastern Band of Cherokee Indians",
									},
								},
							},
						},
						{
							"url":          "isEnrolled",
							"valueBoolean": false,
						},
					},
				},
			},
			"identifier": []map[string]any{
				{"system": "http://hospital.example.org/mrn", "value": "MRN-1001"},
			},
			"name": []map[string]any{
				{"use": "official", "family": "Wonderland", "given": []string{"Alice", "Marie"}},
			},
			"telecom": []map[string]any{
				{"system": "phone", "value": "555-010-1001", "use": "mobile"},
				{"system": "email", "value": "alice.wonderland@example.org", "use": "home"},
			},
			"address": []map[string]any{
				{"line": []string{"742 Evergreen Terrace"}, "city": "San Francisco", "state": "CA", "postalCode": "94105", "country": "US"},
			},
			"communication": []map[string]any{
				{
					"language": map[string]any{
						"coding": []map[string]any{
							{"system": "urn:ietf:bcp:47", "code": "en-US", "display": "English (United States)"},
						},
					},
					"preferred": true,
				},
			},
			"generalPractitioner": []map[string]any{
				{"reference": "Practitioner/prac-cardiology-1"},
			},
			"managingOrganization": map[string]any{
				"reference": "Organization/org-flint-health",
			},
		}),
		putEntry("Encounter/enc-alice-001", map[string]any{
			"resourceType": "Encounter",
			"id":           "enc-alice-001",
			"status":       "finished",
			"class": map[string]any{
				"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode",
				"code":   "AMB",
			},
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "185349003", "display": "Encounter for check up"},
					},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"participant": []map[string]any{
				{
					"individual": map[string]any{"reference": "Practitioner/prac-cardiology-1"},
				},
			},
			"location": []map[string]any{
				{
					"location": map[string]any{"reference": "Location/loc-flint-cardiology"},
				},
			},
			"serviceProvider": map[string]any{
				"reference": "Organization/org-flint-health",
			},
			"period": map[string]any{
				"start": "2026-01-15T09:00:00Z",
				"end":   "2026-01-15T10:00:00Z",
			},
		}),
		putEntry("Condition/cond-alice-htn", map[string]any{
			"resourceType": "Condition",
			"id":           "cond-alice-htn",
			"clinicalStatus": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"},
				},
			},
			"verificationStatus": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"},
				},
			},
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item", "display": "Problem List Item"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "38341003", "display": "Essential hypertension"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"encounter": map[string]any{
				"reference": "Encounter/enc-alice-001",
			},
			"onsetDateTime": "2024-06-01T00:00:00Z",
		}),
		putEntry("Observation/obs-alice-hr", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-hr",
			"status":       "final",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"performer": []map[string]any{
				{"reference": "Practitioner/prac-cardiology-1"},
			},
			"effectiveDateTime": "2026-01-15T09:15:00Z",
			"valueQuantity": map[string]any{
				"value":  76.0,
				"unit":   "beats/minute",
				"system": "http://unitsofmeasure.org",
				"code":   "/min",
			},
		}),
		putEntry("Observation/obs-alice-bp", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-bp",
			"status":       "final",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure panel"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"performer": []map[string]any{
				{"reference": "Practitioner/prac-cardiology-1"},
			},
			"effectiveDateTime": "2026-01-15T09:16:00Z",
			"component": []map[string]any{
				{
					"code": map[string]any{
						"coding": []map[string]any{
							{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic blood pressure"},
						},
					},
					"valueQuantity": map[string]any{
						"value":  138.0,
						"unit":   "mmHg",
						"system": "http://unitsofmeasure.org",
						"code":   "mm[Hg]",
					},
				},
				{
					"code": map[string]any{
						"coding": []map[string]any{
							{"system": "http://loinc.org", "code": "8462-4", "display": "Diastolic blood pressure"},
						},
					},
					"valueQuantity": map[string]any{
						"value":  86.0,
						"unit":   "mmHg",
						"system": "http://unitsofmeasure.org",
						"code":   "mm[Hg]",
					},
				},
				{
					"code": map[string]any{
						"coding": []map[string]any{
							{"system": "http://loinc.org", "code": "8478-0", "display": "Mean blood pressure"},
						},
					},
					"dataAbsentReason": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason", "code": "unknown", "display": "Unknown"},
						},
					},
				},
			},
		}),
		putEntry("Observation/obs-alice-avg-bp", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-avg-bp",
			"status":       "final",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "96607-7", "display": "Blood pressure panel mean systolic and mean diastolic"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"performer": []map[string]any{
				{"reference": "Practitioner/prac-cardiology-1"},
			},
			"effectiveDateTime": "2026-01-15T09:17:00Z",
			"component": []map[string]any{
				{
					"code": map[string]any{
						"coding": []map[string]any{
							{"system": "http://loinc.org", "code": "96608-5", "display": "Systolic blood pressure--mean"},
						},
					},
					"valueQuantity": map[string]any{
						"value":  132.0,
						"unit":   "mmHg",
						"system": "http://unitsofmeasure.org",
						"code":   "mm[Hg]",
					},
				},
				{
					"code": map[string]any{
						"coding": []map[string]any{
							{"system": "http://loinc.org", "code": "96609-3", "display": "Diastolic blood pressure--mean"},
						},
					},
					"valueQuantity": map[string]any{
						"value":  82.0,
						"unit":   "mmHg",
						"system": "http://unitsofmeasure.org",
						"code":   "mm[Hg]",
					},
				},
			},
		}),
		putEntry("Observation/obs-alice-care-pref", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-care-pref",
			"status":       "final",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-category", "code": "care-experience-preference", "display": "Care Experience Preference"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "95541-9", "display": "Care experience preference"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"performer": []map[string]any{
				{"reference": "Practitioner/prac-cardiology-1"},
			},
			"effectiveDateTime": "2026-01-15T09:18:00Z",
			"valueString":       "Prefers morning appointments and digital portal messaging",
		}),
		putEntry("MedicationRequest/medreq-alice-lisinopril", map[string]any{
			"resourceType": "MedicationRequest",
			"id":           "medreq-alice-lisinopril",
			"status":       "active",
			"intent":       "order",
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"encounter": map[string]any{
				"reference": "Encounter/enc-alice-001",
			},
			"requester": map[string]any{
				"reference": "Practitioner/prac-cardiology-1",
			},
			"authoredOn": "2026-01-15T09:30:00Z",
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "314076", "display": "Lisinopril 10 MG Oral Tablet"},
				},
			},
		}),
		putEntry("AllergyIntolerance/allergy-alice-penicillin", map[string]any{
			"resourceType": "AllergyIntolerance",
			"id":           "allergy-alice-penicillin",
			"clinicalStatus": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"},
				},
			},
			"verificationStatus": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification", "code": "confirmed"},
				},
			},
			"criticality": "high",
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980", "display": "Penicillin G"},
				},
			},
			"patient": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
		}),
		putEntry("Procedure/proc-alice-ecg", map[string]any{
			"resourceType": "Procedure",
			"id":           "proc-alice-ecg",
			"status":       "completed",
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "29303009", "display": "Electrocardiographic procedure"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"encounter": map[string]any{
				"reference": "Encounter/enc-alice-001",
			},
			"performedDateTime": "2026-01-15T09:20:00Z",
		}),
		putEntry("Specimen/spec-alice-blood", map[string]any{
			"resourceType": "Specimen",
			"id":           "spec-alice-blood",
			"status":       "available",
			"identifier": []map[string]any{
				{"system": "http://lab.example.org/specimens", "value": "SPEC-ALICE-001"},
			},
			"accessionIdentifier": map[string]any{
				"system": "http://lab.example.org/accessions",
				"value":  "ACC-2026-0001",
			},
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "119297000", "display": "Blood specimen"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"receivedTime": "2026-01-15T09:22:00Z",
			"collection": map[string]any{
				"collector": map[string]any{
					"reference": "Practitioner/prac-cardiology-1",
				},
				"collectedDateTime": "2026-01-15T09:20:00Z",
			},
		}),
		putEntry("DiagnosticReport/dr-alice-cardiac", map[string]any{
			"resourceType": "DiagnosticReport",
			"id":           "dr-alice-cardiac",
			"status":       "final",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "LAB"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "24323-8", "display": "Comprehensive metabolic 2000 panel"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"encounter": map[string]any{
				"reference": "Encounter/enc-alice-001",
			},
			"effectiveDateTime": "2026-01-15T09:25:00Z",
			"issued":            "2026-01-15T09:45:00Z",
			"performer": []map[string]any{
				{"reference": "Organization/org-flint-health"},
			},
			"specimen": []map[string]any{
				{"reference": "Specimen/spec-alice-blood"},
			},
			"result": []map[string]any{
				{"reference": "Observation/obs-alice-hr"},
			},
		}),

		// 4. Patient 2: Robert Frost (Type 2 Diabetes, Inpatient admission)
		putEntry("Patient/pat-robert-002", map[string]any{
			"resourceType": "Patient",
			"id":           "pat-robert-002",
			"active":       true,
			"gender":       "male",
			"birthDate":    "1962-11-03",
			"identifier": []map[string]any{
				{"system": "http://hospital.example.org/mrn", "value": "MRN-1002"},
			},
			"name": []map[string]any{
				{"use": "official", "family": "Frost", "given": []string{"Robert", "James"}},
			},
			"address": []map[string]any{
				{"line": []string{"100 Broadway"}, "city": "New York", "state": "NY", "postalCode": "10001"},
			},
			"generalPractitioner": []map[string]any{
				{"reference": "Practitioner/prac-primary-2"},
			},
		}),
		putEntry("Encounter/enc-robert-001", map[string]any{
			"resourceType": "Encounter",
			"id":           "enc-robert-001",
			"status":       "finished",
			"class": map[string]any{
				"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode",
				"code":   "IMP",
			},
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "32485007", "display": "Hospital admission"},
					},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-robert-002",
			},
			"participant": []map[string]any{
				{
					"individual": map[string]any{"reference": "Practitioner/prac-primary-2"},
				},
			},
			"period": map[string]any{
				"start": "2026-02-10T08:00:00Z",
				"end":   "2026-02-12T14:00:00Z",
			},
		}),
		putEntry("Condition/cond-robert-t2d", map[string]any{
			"resourceType": "Condition",
			"id":           "cond-robert-t2d",
			"clinicalStatus": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"},
				},
			},
			"verificationStatus": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"},
				},
			},
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "encounter-diagnosis", "display": "Encounter Diagnosis"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "44054006", "display": "Type 2 diabetes mellitus"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-robert-002",
			},
			"encounter": map[string]any{
				"reference": "Encounter/enc-robert-001",
			},
			"onsetDateTime": "2020-03-15T00:00:00Z",
			"_recordedDate": map[string]any{
				"extension": []map[string]any{
					{"url": "http://hl7.org/fhir/StructureDefinition/data-absent-reason", "valueCode": "unknown"},
				},
			},
		}),
		putEntry("Observation/obs-robert-hba1c", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-robert-hba1c",
			"status":       "final",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory", "display": "Laboratory"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "4548-4", "display": "Hemoglobin A1c/Hemoglobin.total in Blood"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-robert-002",
			},
			"performer": []map[string]any{
				{"reference": "Practitioner/prac-primary-2"},
			},
			"effectiveDateTime": "2026-02-10T10:30:00Z",
			"valueQuantity": map[string]any{
				"value":  7.4,
				"unit":   "%",
				"system": "http://unitsofmeasure.org",
				"code":   "%",
			},
		}),
		putEntry("Observation/obs-robert-wt", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-robert-wt",
			"status":       "final",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "29463-7", "display": "Body weight"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-robert-002",
			},
			"performer": []map[string]any{
				{"reference": "Practitioner/prac-primary-2"},
			},
			"effectiveDateTime": "2026-02-10T08:30:00Z",
			"valueQuantity": map[string]any{
				"value":  88.5,
				"unit":   "kg",
				"system": "http://unitsofmeasure.org",
				"code":   "kg",
			},
		}),
		putEntry("MedicationRequest/medreq-robert-metformin", map[string]any{
			"resourceType": "MedicationRequest",
			"id":           "medreq-robert-metformin",
			"status":       "active",
			"intent":       "order",
			"subject": map[string]any{
				"reference": "Patient/pat-robert-002",
			},
			"encounter": map[string]any{
				"reference": "Encounter/enc-robert-001",
			},
			"requester": map[string]any{
				"reference": "Practitioner/prac-primary-2",
			},
			"authoredOn": "2026-02-10T11:00:00Z",
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "861004", "display": "Metformin hydrochloride 500 MG Oral Tablet"},
				},
			},
		}),
		putEntry("Procedure/proc-robert-foot-exam", map[string]any{
			"resourceType": "Procedure",
			"id":           "proc-robert-foot-exam",
			"status":       "completed",
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "401191002", "display": "Diabetic foot examination"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-robert-002",
			},
			"encounter": map[string]any{
				"reference": "Encounter/enc-robert-001",
			},
			"performedDateTime": "2026-02-10T09:00:00Z",
		}),
		putEntry("DiagnosticReport/dr-robert-diabetes", map[string]any{
			"resourceType": "DiagnosticReport",
			"id":           "dr-robert-diabetes",
			"status":       "final",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "LAB"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "4548-4", "display": "Hemoglobin A1c/Hemoglobin.total in Blood"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-robert-002",
			},
			"encounter": map[string]any{
				"reference": "Encounter/enc-robert-001",
			},
			"effectiveDateTime": "2026-02-10T10:30:00Z",
			"issued":            "2026-02-10T11:15:00Z",
			"performer": []map[string]any{
				{"reference": "Organization/org-metro-lab"},
			},
			"result": []map[string]any{
				{"reference": "Observation/obs-robert-hba1c"},
			},
		}),

		// 5. Patient 3: Elena Rostova (Wellness check, active Encounter, COVID-19 Immunization)
		putEntry("Patient/pat-elena-003", map[string]any{
			"resourceType": "Patient",
			"id":           "pat-elena-003",
			"active":       true,
			"gender":       "female",
			"birthDate":    "1994-08-22",
			"identifier": []map[string]any{
				{"system": "http://hospital.example.org/mrn", "value": "MRN-1003"},
			},
			"name": []map[string]any{
				{"use": "official", "family": "Rostova", "given": []string{"Elena", "Sofia"}},
			},
			"address": []map[string]any{
				{"line": []string{"200 N Michigan Ave"}, "city": "Chicago", "state": "IL", "postalCode": "60611"},
			},
			"generalPractitioner": []map[string]any{
				{"reference": "Practitioner/prac-primary-2"},
			},
		}),
		putEntry("Encounter/enc-elena-001", map[string]any{
			"resourceType": "Encounter",
			"id":           "enc-elena-001",
			"status":       "in-progress",
			"class": map[string]any{
				"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode",
				"code":   "AMB",
			},
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "185349003", "display": "Encounter for check up"},
					},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-elena-003",
			},
			"participant": []map[string]any{
				{
					"individual": map[string]any{"reference": "Practitioner/prac-primary-2"},
				},
			},
			"period": map[string]any{
				"start": "2026-03-01T11:00:00Z",
				"_end": map[string]any{
					"extension": []map[string]any{
						{"url": "http://hl7.org/fhir/StructureDefinition/data-absent-reason", "valueCode": "unknown"},
					},
				},
			},
			"reasonCode": []map[string]any{
				{
					"extension": []map[string]any{
						{"url": "http://hl7.org/fhir/StructureDefinition/data-absent-reason", "valueCode": "unknown"},
					},
				},
			},
		}),
		putEntry("Observation/obs-elena-hr", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-elena-hr",
			"status":       "final",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/pat-elena-003",
			},
			"performer": []map[string]any{
				{"reference": "Practitioner/prac-primary-2"},
			},
			"effectiveDateTime": "2026-03-01T11:10:00Z",
			"valueQuantity": map[string]any{
				"value":  64.0,
				"unit":   "beats/minute",
				"system": "http://unitsofmeasure.org",
				"code":   "/min",
			},
		}),
		putEntry("Immunization/imm-elena-covid", map[string]any{
			"resourceType": "Immunization",
			"id":           "imm-elena-covid",
			"status":       "completed",
			"vaccineCode": map[string]any{
				"coding": []map[string]any{
					{"system": "http://hl7.org/fhir/sid/cvx", "code": "207", "display": "COVID-19, mRNA, LNP-S, PF, 100 mcg/0.5mL dose"},
				},
			},
			"patient": map[string]any{
				"reference": "Patient/pat-elena-003",
			},
			"encounter": map[string]any{
				"reference": "Encounter/enc-elena-001",
			},
			"occurrenceDateTime": "2026-03-01T11:20:00Z",
			"lotNumber":          "FLINT-LOT-2026A",
		}),

		// 6. Complete US Core v6.1.0 & v7.0.0 + HL7 IPA Resource Types & Sub-Profiles for Alice (pat-alice-001)
		putEntry("PractitionerRole/pracrole-cardiology-1", map[string]any{
			"resourceType": "PractitionerRole",
			"id":           "pracrole-cardiology-1",
			"active":       true,
			"practitioner": map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"organization": map[string]any{"reference": "Organization/org-flint-health"},
			"location": []map[string]any{
				{"reference": "Location/loc-flint-cardiology"},
			},
			"code": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://nucc.org/provider-taxonomy", "code": "207RC0000X", "display": "Cardiovascular Disease Physician"},
					},
				},
			},
			"specialty": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://nucc.org/provider-taxonomy", "code": "207RC0000X", "display": "Cardiovascular Disease Physician"},
					},
				},
			},
			"telecom": []map[string]any{
				{"system": "phone", "value": "555-019-2000", "use": "work"},
			},
		}),
		putEntry("RelatedPerson/relperson-alice-spouse", map[string]any{
			"resourceType": "RelatedPerson",
			"id":           "relperson-alice-spouse",
			"active":       true,
			"patient":      map[string]any{"reference": "Patient/pat-alice-001"},
			"relationship": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/v3-RoleCode", "code": "SPS", "display": "spouse"},
					},
				},
			},
			"name": []map[string]any{
				{"use": "official", "family": "Wonderland", "given": []string{"Arthur"}},
			},
			"telecom": []map[string]any{
				{"system": "phone", "value": "555-010-1002", "use": "mobile"},
			},
			"address": []map[string]any{
				{"line": []string{"742 Evergreen Terrace"}, "city": "San Francisco", "state": "CA", "postalCode": "94105", "country": "US"},
			},
		}),
		putEntry("Medication/med-atorvastatin-001", map[string]any{
			"resourceType": "Medication",
			"id":           "med-atorvastatin-001",
			"status":       "active",
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "617312", "display": "Atorvastatin 20 MG Oral Tablet"},
				},
			},
		}),
		putEntry("MedicationRequest/medreq-alice-atorvastatin-ref", map[string]any{
			"resourceType":        "MedicationRequest",
			"id":                  "medreq-alice-atorvastatin-ref",
			"status":              "active",
			"intent":              "order",
			"subject":             map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":           map[string]any{"reference": "Encounter/enc-alice-001"},
			"requester":           map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"authoredOn":          "2026-01-15T09:32:00Z",
			"medicationReference": map[string]any{"reference": "Medication/med-atorvastatin-001"},
		}),
		putEntry("MedicationDispense/meddisp-alice-lisinopril", map[string]any{
			"resourceType": "MedicationDispense",
			"id":           "meddisp-alice-lisinopril",
			"status":       "completed",
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "314076", "display": "Lisinopril 10 MG Oral Tablet"},
				},
			},
			"subject": map[string]any{"reference": "Patient/pat-alice-001"},
			"performer": []map[string]any{
				{"actor": map[string]any{"reference": "Practitioner/prac-cardiology-1"}},
			},
			"authorizingPrescription": []map[string]any{
				{"reference": "MedicationRequest/medreq-alice-lisinopril"},
			},
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "FF", "display": "First Fill"},
				},
			},
			"quantity": map[string]any{
				"value":  30.0,
				"unit":   "TAB",
				"system": "http://terminology.hl7.org/CodeSystem/v3-orderableDrugForm",
				"code":   "TAB",
			},
			"whenHandedOver": "2026-01-15T10:30:00Z",
		}),
		putEntry("Coverage/cov-alice-medicare", map[string]any{
			"resourceType": "Coverage",
			"id":           "cov-alice-medicare",
			"status":       "active",
			"identifier": []map[string]any{
				{"system": "http://flint-health.example.org/memberid", "value": "MEM-ALICE-001"},
			},
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "https://nahdo.org/sopt", "code": "1", "display": "MEDICARE"},
				},
			},
			"subscriberId": "SUB-ALICE-001",
			"beneficiary":  map[string]any{"reference": "Patient/pat-alice-001"},
			"relationship": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/subscriber-relationship", "code": "self", "display": "Self"},
				},
			},
			"period": map[string]any{
				"start": "2026-01-01",
				"end":   "2026-12-31",
			},
			"payor": []map[string]any{
				{"reference": "Organization/org-flint-health"},
			},
			"class": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/coverage-class", "code": "group"},
						},
					},
					"value": "GRP-CARDIO-100",
					"name":  "Flint Preferred Group Plan",
				},
			},
		}),
		putEntry("Goal/goal-alice-bp", map[string]any{
			"resourceType":    "Goal",
			"id":              "goal-alice-bp",
			"lifecycleStatus": "active",
			"achievementStatus": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/goal-achievement", "code": "in-progress", "display": "In Progress"},
				},
			},
			"description": map[string]any{
				"text": "Maintain systolic blood pressure below 130 mmHg",
			},
			"subject": map[string]any{"reference": "Patient/pat-alice-001"},
			"target": []map[string]any{
				{"dueDate": "2026-12-31"},
			},
		}),
		putEntry("CareTeam/careteam-alice-001", map[string]any{
			"resourceType": "CareTeam",
			"id":           "careteam-alice-001",
			"status":       "active",
			"name":         "Alice Wonderland Multidisciplinary Care Team",
			"subject":      map[string]any{"reference": "Patient/pat-alice-001"},
			"participant": []map[string]any{
				{
					"role": []map[string]any{
						{
							"coding": []map[string]any{
								{"system": "http://snomed.info/sct", "code": "17561000", "display": "Cardiologist"},
							},
						},
					},
					"member": map[string]any{"reference": "Practitioner/prac-cardiology-1"},
				},
			},
		}),
		putEntry("CarePlan/careplan-alice-htn", map[string]any{
			"resourceType": "CarePlan",
			"id":           "careplan-alice-htn",
			"status":       "active",
			"intent":       "plan",
			"title":        "Hypertension Longitudinal Management Plan",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://hl7.org/fhir/us/core/CodeSystem/careplan-category", "code": "assess-plan"},
					},
				},
			},
			"subject":   map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter": map[string]any{"reference": "Encounter/enc-alice-001"},
			"careTeam": []map[string]any{
				{"reference": "CareTeam/careteam-alice-001"},
			},
			"addresses": []map[string]any{
				{"reference": "Condition/cond-alice-htn"},
			},
			"goal": []map[string]any{
				{"reference": "Goal/goal-alice-bp"},
			},
		}),
		putEntry("ServiceRequest/sr-alice-lipid", map[string]any{
			"resourceType": "ServiceRequest",
			"id":           "sr-alice-lipid",
			"status":       "active",
			"intent":       "order",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "108252007", "display": "Laboratory procedure"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "24331-1", "display": "Lipid 1996 panel - Serum or Plasma"},
				},
			},
			"subject":    map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":  map[string]any{"reference": "Encounter/enc-alice-001"},
			"authoredOn": "2026-01-15T09:35:00Z",
			"requester":  map[string]any{"reference": "Practitioner/prac-cardiology-1"},
		}),
		putEntry("DocumentReference/docref-alice-note", map[string]any{
			"resourceType": "DocumentReference",
			"id":           "docref-alice-note",
			"status":       "current",
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "34117-2", "display": "History and physical note"},
				},
			},
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-documentreference-category", "code": "clinical-note", "display": "Clinical Note"},
					},
				},
			},
			"subject": map[string]any{"reference": "Patient/pat-alice-001"},
			"date":    "2026-01-15T10:05:00Z",
			"author": []map[string]any{
				{"reference": "Practitioner/prac-cardiology-1"},
			},
			"custodian": map[string]any{"reference": "Organization/org-flint-health"},
			"content": []map[string]any{
				{
					"attachment": map[string]any{
						"contentType": "text/plain",
						"data":        "Q2FyZGlvbG9neSBhbWJ1bGF0b3J5IGZvbGxvdy11cCBub3RlLg==",
					},
					"format": map[string]any{
						"system":  "http://ihe.net/fhir/ihe.formatcode.fhir/CodeSystem/formatcode",
						"code":    "urn:ihe:iti:xds:2017:mimeTypeSufficient",
						"display": "mimeType Sufficient",
					},
				},
			},
			"context": map[string]any{
				"encounter": []map[string]any{
					{"reference": "Encounter/enc-alice-001"},
				},
				"period": map[string]any{
					"start": "2026-01-15T09:00:00Z",
					"end":   "2026-01-15T10:00:00Z",
				},
			},
		}),
		putEntry("QuestionnaireResponse/qr-alice-phq9", map[string]any{
			"resourceType":  "QuestionnaireResponse",
			"id":            "qr-alice-phq9",
			"questionnaire": "http://hl7.org/fhir/us/core/Questionnaire/phq-9-quick-depression-assessment-panel",
			"status":        "completed",
			"subject":       map[string]any{"reference": "Patient/pat-alice-001"},
			"authored":      "2026-01-15T09:19:00Z",
			"author":        map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"item": []map[string]any{
				{
					"linkId": "44250-9",
					"text":   "Little interest or pleasure in doing things",
					"answer": []map[string]any{
						{
							"valueCoding": map[string]any{
								"system":  "http://loinc.org",
								"code":    "LA6568-5",
								"display": "Not at all",
							},
						},
					},
				},
			},
		}),
		putEntry("Device/dev-alice-pacemaker", map[string]any{
			"resourceType": "Device",
			"id":           "dev-alice-pacemaker",
			"udiCarrier": []map[string]any{
				{
					"deviceIdentifier": "00844588003288",
					"carrierHRF":       "(01)00844588003288(11)240101(17)290101(10)LOT1(21)SER1",
				},
			},
			"status":       "active",
			"manufacturer": "Acme Cardiac Devices",
			"lotNumber":    "LOT1",
			"serialNumber": "SER1",
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "14106009", "display": "Cardiac pacemaker, device"},
				},
			},
			"patient": map[string]any{"reference": "Patient/pat-alice-001"},
		}),
		putEntry("Provenance/prov-alice-001", map[string]any{
			"resourceType": "Provenance",
			"id":           "prov-alice-001",
			"target": []map[string]any{
				{"reference": "Patient/pat-alice-001"},
				{"reference": "Condition/cond-alice-htn"},
				{"reference": "Observation/obs-alice-bp"},
			},
			"recorded": "2026-01-15T10:10:00Z",
			"agent": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/provenance-participant-type", "code": "author", "display": "Author"},
						},
					},
					"who":        map[string]any{"reference": "Practitioner/prac-cardiology-1"},
					"onBehalfOf": map[string]any{"reference": "Organization/org-flint-health"},
				},
			},
		}),
		putEntry("Condition/cond-alice-health-concern", map[string]any{
			"resourceType": "Condition",
			"id":           "cond-alice-health-concern",
			"clinicalStatus": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"},
				},
			},
			"verificationStatus": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"},
				},
			},
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://hl7.org/fhir/us/core/CodeSystem/condition-category", "code": "health-concern", "display": "Health Concern"},
					},
				},
				{
					"coding": []map[string]any{
						{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-category", "code": "sdoh", "display": "SDOH"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "733423003", "display": "Food insecurity"},
				},
			},
			"subject":      map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":    map[string]any{"reference": "Encounter/enc-alice-001"},
			"recordedDate": "2026-01-15T09:18:00Z",
		}),
		putEntry("DiagnosticReport/dr-alice-radiology", map[string]any{
			"resourceType": "DiagnosticReport",
			"id":           "dr-alice-radiology",
			"status":       "final",
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://loinc.org", "code": "LP29684-5", "display": "Radiology"},
					},
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "30746-2", "display": "XR Chest 2 Views"},
				},
			},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-alice-001"},
			"effectiveDateTime": "2026-01-15T09:40:00Z",
			"issued":            "2026-01-15T09:55:00Z",
			"performer": []map[string]any{
				{"reference": "Practitioner/prac-cardiology-1"},
			},
			"presentedForm": []map[string]any{
				{
					"contentType": "text/plain",
					"data":        "Q2hlc3QgWC1yYXk6IG5vcm1hbCBjYXJkaW9tZWRpYXN0aW5hbCBzaWxob3VldHRlLg==",
				},
			},
		}),
		putEntry("Immunization/imm-alice-flu", map[string]any{
			"resourceType": "Immunization",
			"id":           "imm-alice-flu",
			"status":       "completed",
			"vaccineCode": map[string]any{
				"coding": []map[string]any{
					{"system": "http://hl7.org/fhir/sid/cvx", "code": "141", "display": "Influenza, seasonal, injectable"},
				},
			},
			"patient":            map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":          map[string]any{"reference": "Encounter/enc-alice-001"},
			"occurrenceDateTime": "2026-01-15T09:50:00Z",
			"primarySource":      true,
		}),
		putEntry("Observation/obs-alice-resp-rate", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-resp-rate",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "9279-1", "display": "Respiratory rate"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:15:00Z",
			"valueQuantity":     map[string]any{"value": 16.0, "unit": "breaths/minute", "system": "http://unitsofmeasure.org", "code": "/min"},
		}),
		putEntry("Observation/obs-alice-body-temp", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-body-temp",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8310-5", "display": "Body temperature"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:15:00Z",
			"valueQuantity":     map[string]any{"value": 36.8, "unit": "C", "system": "http://unitsofmeasure.org", "code": "Cel"},
		}),
		putEntry("Observation/obs-alice-body-height", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-body-height",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8302-2", "display": "Body height"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:15:00Z",
			"valueQuantity":     map[string]any{"value": 165.0, "unit": "cm", "system": "http://unitsofmeasure.org", "code": "cm"},
		}),
		putEntry("Observation/obs-alice-body-weight", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-body-weight",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "29463-7", "display": "Body weight"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:15:00Z",
			"valueQuantity":     map[string]any{"value": 64.0, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"},
		}),
		putEntry("Observation/obs-alice-bmi", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-bmi",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "39156-5", "display": "Body mass index (BMI) [Ratio]"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:15:00Z",
			"valueQuantity":     map[string]any{"value": 23.5, "unit": "kg/m2", "system": "http://unitsofmeasure.org", "code": "kg/m2"},
		}),
		putEntry("Observation/obs-alice-pulse-ox", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-pulse-ox",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "59408-5", "display": "Oxygen saturation in Arterial blood by Pulse oximetry"},
					{"system": "http://loinc.org", "code": "2708-6", "display": "Oxygen saturation in Arterial blood"},
				},
			},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:15:00Z",
			"valueQuantity":     map[string]any{"value": 98.0, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"},
			"component": []map[string]any{
				{
					"code":          map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "3151-8", "display": "Inhaled oxygen flow rate"}}},
					"valueQuantity": map[string]any{"value": 2.0, "unit": "L/min", "system": "http://unitsofmeasure.org", "code": "L/min"},
				},
				{
					"code":          map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "3150-0", "display": "Inhaled oxygen concentration"}}},
					"valueQuantity": map[string]any{"value": 28.0, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"},
				},
			},
		}),
		putEntry("Observation/obs-alice-head-circ", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-head-circ",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "9843-4", "display": "Head Occipital-frontal circumference"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:15:00Z",
			"valueQuantity":     map[string]any{"value": 55.0, "unit": "cm", "system": "http://unitsofmeasure.org", "code": "cm"},
		}),
		putEntry("Observation/obs-alice-smoking", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-smoking",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history", "display": "Social History"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "72166-2", "display": "Tobacco smoking status"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:16:00Z",
			"valueCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "266919005", "display": "Never smoked tobacco"},
				},
			},
		}),
		putEntry("Observation/obs-alice-sdoh", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-sdoh",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "survey", "display": "Survey"}}},
				{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-category", "code": "sdoh", "display": "SDOH"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "93030-5", "display": "Has lack of transportation kept you from medical appointments, meetings, work, or from getting things needed for daily living"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:17:00Z",
			"valueCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "LA32-8", "display": "No"},
				},
			},
		}),
		putEntry("Observation/obs-alice-treatment-pref", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-treatment-pref",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-category", "code": "treatment-intervention-preference", "display": "Treatment Intervention Preference"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "75773-2", "display": "Goals, preferences, and priorities for medical treatment"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:18:00Z",
			"valueString":       "Full code; prefers conservative non-invasive management first",
		}),
		putEntry("Observation/obs-alice-occupation", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-occupation",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history", "display": "Social History"}}},
			},
			"code":            map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "11341-5", "display": "History of Occupation"}}},
			"subject":         map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":       []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectivePeriod": map[string]any{"start": "2018-01-01"},
			"valueCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "https://www.cdc.gov/niosh/topics/coding/default.html", "code": "15-1252.00", "display": "Software Developers"},
				},
			},
		}),
		putEntry("Observation/obs-alice-pregnancy-status", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-pregnancy-status",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history", "display": "Social History"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "82810-3", "display": "Pregnancy status"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:18:00Z",
			"valueCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "60001007", "display": "Not pregnant"},
				},
			},
		}),
		putEntry("Observation/obs-alice-pregnancy-intent", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-pregnancy-intent",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history", "display": "Social History"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "86645-9", "display": "Pregnancy intention in the next year"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:18:00Z",
			"valueCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "454411000124108", "display": "No intention of becoming pregnant in next year"},
				},
			},
		}),
		putEntry("Observation/obs-alice-lab-ldl", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-lab-ldl",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory", "display": "Laboratory"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "13457-7", "display": "Cholesterol in LDL [Mass/volume] in Serum or Plasma by calculation"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Organization/org-flint-health"}},
			"effectiveDateTime": "2026-01-15T09:25:00Z",
			"valueQuantity":     map[string]any{"value": 112.0, "unit": "mg/dL", "system": "http://unitsofmeasure.org", "code": "mg/dL"},
		}),
		putEntry("Observation/obs-alice-clinical-result", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-alice-clinical-result",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "imaging", "display": "Imaging"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "18748-4", "display": "Diagnostic imaging study"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:40:00Z",
			"valueString":       "Normal cardiac silhouette, clear bilateral lung fields",
		}),
		putEntry("MedicationStatement/medstmt-alice-001", map[string]any{
			"resourceType":        "MedicationStatement",
			"id":                  "medstmt-alice-001",
			"status":              "active",
			"medicationReference": map[string]any{"reference": "Medication/med-atorvastatin-001"},
			"subject":             map[string]any{"reference": "Patient/pat-alice-001"},
			"context":             map[string]any{"reference": "Encounter/enc-alice-001"},
			"effectiveDateTime":   "2026-01-15T08:00:00Z",
			"dateAsserted":        "2026-01-15T09:30:00Z",
			"informationSource":   map[string]any{"reference": "Patient/pat-alice-001"},
			"dosage": []map[string]any{
				{"text": "Take 1 tablet by mouth nightly"},
			},
		}),
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry":        entries,
	}
	return json.MarshalIndent(bundle, "", "  ")
}

func putEntry(url string, resource map[string]any) map[string]any {
	return map[string]any{
		"fullUrl":  url,
		"resource": resource,
		"request": map[string]any{
			"method": "PUT",
			"url":    url,
		},
	}
}
