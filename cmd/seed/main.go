// Command seed populates a running Flint FHIR R4 server with a realistic,
// deterministic longitudinal clinical cohort (Practitioner, Patient, Encounter,
// Condition, Observation) using an atomic FHIR R4 transaction Bundle.
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

func main() {
	defaultURL := os.Getenv("FLINT_URL")
	if defaultURL == "" {
		defaultURL = "http://localhost:8080"
	}
	defaultTenant := os.Getenv("FLINT_TENANT")
	if defaultTenant == "" {
		defaultTenant = "demo"
	}
	defaultToken := os.Getenv("FLINT_TOKEN")

	urlFlag := flag.String("url", defaultURL, "Flint server base URL")
	tenantFlag := flag.String("tenant", defaultTenant, "Target FHIR tenant ID")
	tokenFlag := flag.String("token", defaultToken, "Optional SMART on FHIR Bearer token")
	flag.Parse()

	cfg := Config{
		BaseURL:     *urlFlag,
		Tenant:      *tenantFlag,
		BearerToken: *tokenFlag,
		HTTPClient:  &http.Client{Timeout: 30 * time.Second},
		Out:         os.Stdout,
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

	fmt.Fprintf(out, "🔥 Seeded %d FHIR R4 resources into tenant %q (%s)\n\n", len(txResp.Entry), tenant, endpoint)
	for _, e := range txResp.Entry {
		fmt.Fprintf(out, "  • %-30s  %-12s  ETag: %s\n", e.FullURL, e.Response.Status, e.Response.ETag)
	}

	fmt.Fprintf(out, "\nTry these queries:\n")
	fmt.Fprintf(out, "  # 1. Full longitudinal patient chart ($everything)\n")
	fmt.Fprintf(out, "  curl -s %s/Patient/pat-alice-001/$everything | jq .\n\n", endpoint)
	fmt.Fprintf(out, "  # 2. Search Observations by LOINC code + quantity + _include patient\n")
	fmt.Fprintf(out, "  curl -s '%s/Observation?code=8867-4&value-quantity=gt70&_include=Observation:patient' | jq .\n\n", endpoint)
	fmt.Fprintf(out, "  # 3. Chained search: Observations for patients with family name Wonderland\n")
	fmt.Fprintf(out, "  curl -s '%s/Observation?patient.family=Wonderland' | jq .\n\n", endpoint)
	fmt.Fprintf(out, "  # 4. HIPAA / ONC Security Audit Log (AuditEvent)\n")
	fmt.Fprintf(out, "  curl -s '%s/AuditEvent?_count=5' | jq .\n", endpoint)

	return nil
}

// CohortTransactionBundleJSON returns a deterministic, idempotent FHIR R4 transaction Bundle
// containing 2 Practitioners, 3 Patients, 3 Encounters, 2 Conditions, and 5 Observations.
func CohortTransactionBundleJSON() ([]byte, error) {
	entries := []map[string]any{
		// 1. Practitioners
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
		}),

		// 2. Patient 1: Alice Wonderland (Hypertension, Cardiology ambulatory visit)
		putEntry("Patient/pat-alice-001", map[string]any{
			"resourceType": "Patient",
			"id":           "pat-alice-001",
			"active":       true,
			"gender":       "female",
			"birthDate":    "1985-04-12",
			"identifier": []map[string]any{
				{"system": "http://hospital.example.org/mrn", "value": "MRN-1001"},
			},
			"name": []map[string]any{
				{"use": "official", "family": "Wonderland", "given": []string{"Alice", "Marie"}},
			},
			"address": []map[string]any{
				{"line": []string{"742 Evergreen Terrace"}, "city": "San Francisco", "state": "CA", "postalCode": "94105"},
			},
			"generalPractitioner": []map[string]any{
				{"reference": "Practitioner/prac-cardiology-1"},
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
			"subject": map[string]any{
				"reference": "Patient/pat-alice-001",
			},
			"participant": []map[string]any{
				{
					"individual": map[string]any{"reference": "Practitioner/prac-cardiology-1"},
				},
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
			},
		}),

		// 3. Patient 2: Robert Frost (Type 2 Diabetes, Inpatient admission)
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
		}),
		putEntry("Observation/obs-robert-hba1c", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-robert-hba1c",
			"status":       "final",
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

		// 4. Patient 3: Elena Rostova (Wellness check, active Encounter)
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
			},
		}),
		putEntry("Observation/obs-elena-hr", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-elena-hr",
			"status":       "final",
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
