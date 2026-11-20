package main

import "encoding/json"

// AUCoreTransactionBundleJSON builds an idempotent FHIR R4 transaction Bundle
// covering all HL7 AU Core v1.0.0 & v2.0.0 profiles, Australian national
// identifiers (IHI, Medicare, DVA, HPI-I, HPI-O, ABN, Medicare Provider Number),
// AU extensions (indigenous-status, individual-genderIdentity, individual-pronouns),
// and required MustSupport elements/slices (including dual BloodPressure DAR/value observations).
func AUCoreTransactionBundleJSON() ([]byte, error) {
	entries := []map[string]any{
		putEntry("Organization/org-au-001", map[string]any{
			"resourceType": "Organization",
			"id":           "org-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-organization"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org.au/CodeSystem/v2-0203", "code": "NOI"}},
						"text":   "HPI-O",
					},
					"system": "http://ns.electronichealth.net.au/id/hi/hpio/1.0",
					"value":  "8003621566684455",
				},
				{
					"type": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "XX"}},
						"text":   "ABN",
					},
					"system": "http://hl7.org.au/id/abn",
					"value":  "83153304928",
				},
			},
			"active": true,
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/organization-type", "code": "prov", "display": "Healthcare Provider"},
					},
				},
			},
			"name": "Sydney Harbour Medical Centre",
			"telecom": []map[string]any{
				{"system": "phone", "value": "(02) 5550 1234", "use": "work"},
				{"system": "email", "value": "reception@harbourmed.example.au", "use": "work"},
			},
			"address": []map[string]any{
				{
					"use":        "work",
					"line":       []string{"42 George Street"},
					"city":       "Sydney",
					"state":      "NSW",
					"postalCode": "2000",
					"country":    "AU",
				},
			},
		}),
		putEntry("Location/loc-au-001", map[string]any{
			"resourceType": "Location",
			"id":           "loc-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-location"},
			},
			"status": "active",
			"name":   "Harbour Clinic Wing A",
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/v3-RoleCode", "code": "HOSP", "display": "Hospital"},
					},
				},
			},
			"physicalType": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/location-physical-type", "code": "si", "display": "Site"},
				},
			},
			"address": map[string]any{
				"use":        "work",
				"line":       []string{"42 George Street"},
				"city":       "Sydney",
				"state":      "NSW",
				"postalCode": "2000",
				"country":    "AU",
			},
			"managingOrganization": map[string]any{"reference": "Organization/org-au-001"},
		}),
		putEntry("HealthcareService/hs-au-001", map[string]any{
			"resourceType": "HealthcareService",
			"id":           "hs-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-healthcareservice"},
			},
			"identifier": []map[string]any{
				{"system": "http://harbourmed.example.au/services", "value": "HS-GP-001"},
			},
			"active":     true,
			"providedBy": map[string]any{"reference": "Organization/org-au-001"},
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "788007007", "display": "General practice service"},
					},
				},
			},
			"location": []map[string]any{{"reference": "Location/loc-au-001"}},
			"name":     "Harbour General Practice",
			"telecom": []map[string]any{
				{"system": "phone", "value": "(02) 5550 1200", "use": "work"},
			},
		}),
		putEntry("Practitioner/prac-au-001", map[string]any{
			"resourceType": "Practitioner",
			"id":           "prac-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-practitioner"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "NPI"}},
						"text":   "HPI-I",
					},
					"system": "http://ns.electronichealth.net.au/id/hi/hpii/1.0",
					"value":  "8003619900015717",
				},
			},
			"active": true,
			"name": []map[string]any{
				{"use": "official", "family": "OConnor", "given": []string{"Liam"}, "prefix": []string{"Dr"}},
			},
		}),
		putEntry("Practitioner/prac-au-002", map[string]any{
			"resourceType": "Practitioner",
			"id":           "prac-au-002",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-practitioner"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "NPI"}},
						"text":   "HPI-I",
					},
					"system": "http://ns.electronichealth.net.au/id/hi/hpii/1.0",
					"value":  "8003619900015725",
				},
			},
			"active": true,
			"name": []map[string]any{
				{"use": "official", "family": "Patel", "given": []string{"Priya"}, "prefix": []string{"Dr"}},
			},
		}),
		putEntry("PractitionerRole/pr-au-001", map[string]any{
			"resourceType": "PractitionerRole",
			"id":           "pr-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-practitionerrole"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org.au/CodeSystem/v2-0203", "code": "UPIN"}},
						"text":   "Medicare Provider Number",
					},
					"system": "http://ns.electronichealth.net.au/id/medicare-provider-number",
					"value":  "2426621B",
				},
			},
			"active":       true,
			"practitioner": map[string]any{"reference": "Practitioner/prac-au-001"},
			"organization": map[string]any{"reference": "Organization/org-au-001"},
			"code": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "62247001", "display": "General practitioner"},
					},
				},
			},
			"specialty": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "408443003", "display": "General medical practice"},
					},
				},
			},
			"telecom": []map[string]any{
				{"system": "phone", "value": "(02) 5550 1235", "use": "work"},
			},
		}),
		putEntry("PractitionerRole/pr-au-002", map[string]any{
			"resourceType": "PractitionerRole",
			"id":           "pr-au-002",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-practitionerrole"},
			},
			"identifier": []map[string]any{
				{
					"system": "http://ns.electronichealth.net.au/id/medicare-provider-number",
					"value":  "2426622K",
				},
			},
			"active":       true,
			"practitioner": map[string]any{"reference": "Practitioner/prac-au-002"},
			"organization": map[string]any{"reference": "Organization/org-au-001"},
			"code": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "17561000", "display": "Cardiologist"},
					},
				},
			},
			"specialty": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "394579002", "display": "Cardiology"},
					},
				},
			},
			"telecom": []map[string]any{
				{"system": "phone", "value": "(02) 5550 1236", "use": "work"},
			},
		}),
		putEntry("Patient/pat-au-001", map[string]any{
			"resourceType": "Patient",
			"id":           "pat-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-patient"},
			},
			"extension": []map[string]any{
				{
					"url": "http://hl7.org.au/fhir/StructureDefinition/indigenous-status",
					"valueCoding": map[string]any{
						"system":  "https://healthterminologies.gov.au/fhir/CodeSystem/australian-indigenous-status-1",
						"code":    "4",
						"display": "Neither Aboriginal nor Torres Strait Islander origin",
					},
				},
				{
					"url": "http://hl7.org/fhir/StructureDefinition/individual-genderIdentity",
					"extension": []map[string]any{
						{
							"url": "value",
							"valueCodeableConcept": map[string]any{
								"coding": []map[string]any{
									{
										"system":  "http://snomed.info/sct",
										"code":    "446141000124107",
										"display": "Identifies as female gender",
									},
								},
							},
						},
					},
				},
				{
					"url": "http://hl7.org/fhir/StructureDefinition/individual-pronouns",
					"extension": []map[string]any{
						{
							"url": "value",
							"valueCodeableConcept": map[string]any{
								"coding": []map[string]any{
									{
										"system":  "http://loinc.org",
										"code":    "LA29519-8",
										"display": "she/her/her/hers/herself",
									},
								},
							},
						},
					},
				},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "NI"}},
						"text":   "IHI",
					},
					"system": "http://ns.electronichealth.net.au/id/hi/ihi/1.0",
					"value":  "8003608833357361",
				},
				{
					"type": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MC"}},
						"text":   "Medicare Number",
					},
					"system": "http://ns.electronichealth.net.au/id/medicare-number",
					"value":  "32788511952",
				},
				{
					"type": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org.au/CodeSystem/v2-0203", "code": "DVG"}},
						"text":   "DVA Number",
					},
					"system": "http://ns.electronichealth.net.au/id/dva",
					"value":  "NBUR9080",
				},
			},
			"active": true,
			"name": []map[string]any{
				{
					"use":    "official",
					"text":   "Matilda Rose Banks",
					"family": "Banks",
					"given":  []string{"Matilda", "Rose"},
				},
			},
			"telecom": []map[string]any{
				{"system": "phone", "value": "0491570156", "use": "mobile"},
				{"system": "email", "value": "matilda.banks@example.au", "use": "home"},
			},
			"gender":    "female",
			"birthDate": "1988-06-15",
			"address": []map[string]any{
				{
					"use":        "home",
					"line":       []string{"18 Campbell Parade"},
					"city":       "Sydney",
					"state":      "NSW",
					"postalCode": "2000",
					"country":    "AU",
				},
			},
			"communication": []map[string]any{
				{
					"language": map[string]any{
						"coding": []map[string]any{
							{"system": "urn:ietf:bcp:47", "code": "en-AU", "display": "English (Australia)"},
						},
					},
					"preferred": true,
				},
			},
		}),
		putEntry("Patient/pat-au-002", map[string]any{
			"resourceType": "Patient",
			"id":           "pat-au-002",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-patient"},
			},
			"extension": []map[string]any{
				{
					"url": "http://hl7.org.au/fhir/StructureDefinition/indigenous-status",
					"valueCoding": map[string]any{
						"system":  "https://healthterminologies.gov.au/fhir/CodeSystem/australian-indigenous-status-1",
						"code":    "1",
						"display": "Aboriginal but not Torres Strait Islander origin",
					},
				},
				{
					"url": "http://hl7.org/fhir/StructureDefinition/individual-genderIdentity",
					"extension": []map[string]any{
						{
							"url": "value",
							"valueCodeableConcept": map[string]any{
								"coding": []map[string]any{
									{
										"system":  "http://snomed.info/sct",
										"code":    "446151000124109",
										"display": "Identifies as male gender",
									},
								},
							},
						},
					},
				},
				{
					"url": "http://hl7.org/fhir/StructureDefinition/individual-pronouns",
					"extension": []map[string]any{
						{
							"url": "value",
							"valueCodeableConcept": map[string]any{
								"coding": []map[string]any{
									{
										"system":  "http://loinc.org",
										"code":    "LA29518-0",
										"display": "he/him/him/his/himself",
									},
								},
							},
						},
					},
				},
			},
			"identifier": []map[string]any{
				{
					"system": "http://ns.electronichealth.net.au/id/hi/ihi/1.0",
					"value":  "8003608833357379",
				},
				{
					"system": "http://ns.electronichealth.net.au/id/medicare-number",
					"value":  "29533070322",
				},
				{
					"system": "http://ns.electronichealth.net.au/id/dva",
					"value":  "VX900123",
				},
			},
			"active": true,
			"name": []map[string]any{
				{
					"use":    "official",
					"text":   "Jack William Mercer",
					"family": "Mercer",
					"given":  []string{"Jack", "William"},
				},
			},
			"telecom": []map[string]any{
				{"system": "phone", "value": "0491570157", "use": "mobile"},
			},
			"gender":    "male",
			"birthDate": "1975-03-22",
			"address": []map[string]any{
				{
					"use":        "home",
					"line":       []string{"95 Collins Street"},
					"city":       "Melbourne",
					"state":      "VIC",
					"postalCode": "3000",
					"country":    "AU",
				},
			},
			"communication": []map[string]any{
				{
					"language": map[string]any{
						"coding": []map[string]any{
							{"system": "urn:ietf:bcp:47", "code": "en-AU", "display": "English (Australia)"},
						},
					},
					"preferred": true,
				},
			},
		}),
		putEntry("RelatedPerson/rel-au-001", map[string]any{
			"resourceType": "RelatedPerson",
			"id":           "rel-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-relatedperson"},
			},
			"patient": map[string]any{"reference": "Patient/pat-au-001"},
			"relationship": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/v3-RoleCode", "code": "SPS", "display": "spouse"},
					},
				},
			},
			"name": []map[string]any{
				{
					"use":    "official",
					"text":   "Oliver Thomas Banks",
					"family": "Banks",
					"given":  []string{"Oliver", "Thomas"},
				},
			},
			"telecom": []map[string]any{
				{"system": "phone", "value": "0491570199", "use": "mobile"},
			},
			"address": []map[string]any{
				{
					"use":        "home",
					"line":       []string{"18 Campbell Parade"},
					"city":       "Sydney",
					"state":      "NSW",
					"postalCode": "2000",
					"country":    "AU",
				},
			},
		}),
		putEntry("Medication/med-au-001", map[string]any{
			"resourceType": "Medication",
			"id":           "med-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-medication"},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "376988009", "display": "Perindopril arginine 5 mg"},
				},
				"text": "Coversyl 5 mg tablet",
			},
		}),
		putEntry("Specimen/spec-au-001", map[string]any{
			"resourceType": "Specimen",
			"id":           "spec-au-001",
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "119364003", "display": "Serum specimen"},
				},
			},
			"subject": map[string]any{"reference": "Patient/pat-au-001"},
		}),
		putEntry("DiagnosticReport/dr-au-001", map[string]any{
			"resourceType": "DiagnosticReport",
			"id":           "dr-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/StructureDefinition/au-diagnosticreport"},
			},
			"status": "final",
			"code": map[string]any{
				"coding": []map[string]any{
					{"system": "http://loinc.org", "code": "24323-8", "display": "Comprehensive metabolic 2000 panel - Serum or Plasma"},
				},
			},
			"subject": map[string]any{"reference": "Patient/pat-au-001"},
		}),
		putEntry("AllergyIntolerance/ai-au-001", map[string]any{
			"resourceType": "AllergyIntolerance",
			"id":           "ai-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-allergyintolerance"},
			},
			"clinicalStatus": map[string]any{
				"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}},
			},
			"verificationStatus": map[string]any{
				"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification", "code": "confirmed"}},
			},
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "764146007", "display": "Penicillin"}},
			},
			"patient":       map[string]any{"reference": "Patient/pat-au-001"},
			"onsetDateTime": "2018-04-01",
			"note": []map[string]any{
				{"text": "Generalized urticaria reported after oral penicillin."},
			},
			"reaction": []map[string]any{
				{
					"manifestation": []map[string]any{
						{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "126485001", "display": "Urticaria"}}},
					},
					"severity": "moderate",
				},
			},
		}),
		putEntry("Condition/cond-au-001", map[string]any{
			"resourceType": "Condition",
			"id":           "cond-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-condition"},
			},
			"clinicalStatus": map[string]any{
				"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}},
			},
			"verificationStatus": map[string]any{
				"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}},
			},
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item", "display": "Problem List Item"}}},
			},
			"severity": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "24484000", "display": "Severe"}},
			},
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "59621000", "display": "Essential hypertension"}},
			},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"onsetDateTime":     "2024-05-01",
			"abatementDateTime": "2026-01-01",
			"note": []map[string]any{
				{"text": "Monitored with regular clinic and home blood pressure checks."},
			},
		}),
		putEntry("Encounter/enc-au-001", map[string]any{
			"resourceType": "Encounter",
			"id":           "enc-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-encounter"},
			},
			"status": "finished",
			"class": map[string]any{
				"system":  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
				"code":    "AMB",
				"display": "ambulatory",
			},
			"serviceType": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "788007007", "display": "General practice service"}},
			},
			"subject": map[string]any{"reference": "Patient/pat-au-001"},
			"participant": []map[string]any{
				{
					"type": []map[string]any{
						{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-ParticipationType", "code": "PPRF"}}},
					},
					"individual": map[string]any{"reference": "PractitionerRole/pr-au-001"},
				},
			},
			"period": map[string]any{
				"start": "2026-01-15T09:00:00Z",
				"end":   "2026-01-15T09:30:00Z",
			},
			"reasonCode": []map[string]any{
				{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "59621000", "display": "Essential hypertension"}}},
			},
			"reasonReference": []map[string]any{
				{"reference": "Condition/cond-au-001"},
			},
			"location": []map[string]any{
				{"location": map[string]any{"reference": "Location/loc-au-001"}},
			},
			"serviceProvider": map[string]any{"reference": "Organization/org-au-001"},
		}),
		putEntry("Immunization/imm-au-001", map[string]any{
			"resourceType": "Immunization",
			"id":           "imm-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-immunization"},
			},
			"status": "completed",
			"vaccineCode": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "836374004", "display": "Influenza vaccine"}},
			},
			"patient":            map[string]any{"reference": "Patient/pat-au-001"},
			"occurrenceDateTime": "2026-01-15T09:15:00Z",
			"primarySource":      true,
			"lotNumber":          "AU-FLU-2026",
			"note": []map[string]any{
				{"text": "Annual seasonal influenza immunisation."},
			},
		}),
		putEntry("Procedure/proc-au-001", map[string]any{
			"resourceType": "Procedure",
			"id":           "proc-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-procedure"},
			},
			"status": "completed",
			"code": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "46973005", "display": "Blood pressure taking"}},
			},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-au-001"},
			"performedDateTime": "2026-01-15T09:10:00Z",
			"reasonCode": []map[string]any{
				{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "59621000", "display": "Essential hypertension"}}},
			},
			"reasonReference": []map[string]any{
				{"reference": "Condition/cond-au-001"},
			},
		}),
		putEntry("MedicationRequest/mr-au-001", map[string]any{
			"resourceType": "MedicationRequest",
			"id":           "mr-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-medicationrequest"},
			},
			"status":              "active",
			"intent":              "order",
			"medicationReference": map[string]any{"reference": "Medication/med-au-001"},
			"subject":             map[string]any{"reference": "Patient/pat-au-001"},
			"encounter":           map[string]any{"reference": "Encounter/enc-au-001"},
			"authoredOn":          "2026-01-15T09:20:00Z",
			"requester":           map[string]any{"reference": "PractitionerRole/pr-au-001"},
			"reasonCode": []map[string]any{
				{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "59621000", "display": "Essential hypertension"}}},
			},
			"reasonReference": []map[string]any{
				{"reference": "Condition/cond-au-001"},
			},
			"dosageInstruction": []map[string]any{
				{"text": "Take 1 tablet by mouth every morning"},
			},
		}),
		putEntry("MedicationRequest/mr-au-002", map[string]any{
			"resourceType": "MedicationRequest",
			"id":           "mr-au-002",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-medicationrequest"},
			},
			"status": "active",
			"intent": "order",
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "376988009", "display": "Perindopril arginine 5 mg"},
				},
			},
			"subject":    map[string]any{"reference": "Patient/pat-au-001"},
			"encounter":  map[string]any{"reference": "Encounter/enc-au-001"},
			"authoredOn": "2026-01-15T09:20:00Z",
			"requester":  map[string]any{"reference": "PractitionerRole/pr-au-001"},
			"reasonCode": []map[string]any{
				{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "59621000", "display": "Essential hypertension"}}},
			},
			"reasonReference": []map[string]any{
				{"reference": "Condition/cond-au-001"},
			},
			"dosageInstruction": []map[string]any{
				{"text": "Take 1 tablet by mouth every morning"},
			},
		}),
		putEntry("MedicationStatement/ms-au-001", map[string]any{
			"resourceType": "MedicationStatement",
			"id":           "ms-au-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-medicationstatement"},
			},
			"status":              "active",
			"medicationReference": map[string]any{"reference": "Medication/med-au-001"},
			"subject":             map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime":   "2026-01-15T09:20:00Z",
			"dateAsserted":        "2026-01-15T09:20:00Z",
			"reasonCode": []map[string]any{
				{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "59621000", "display": "Essential hypertension"}}},
			},
			"reasonReference": []map[string]any{
				{"reference": "Condition/cond-au-001"},
			},
			"dosage": []map[string]any{
				{"text": "Take 1 tablet by mouth every morning"},
			},
		}),
		putEntry("MedicationStatement/ms-au-002", map[string]any{
			"resourceType": "MedicationStatement",
			"id":           "ms-au-002",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-medicationstatement"},
			},
			"status": "active",
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "376988009", "display": "Perindopril arginine 5 mg"},
				},
			},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:20:00Z",
			"dateAsserted":      "2026-01-15T09:20:00Z",
			"reasonCode": []map[string]any{
				{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "59621000", "display": "Essential hypertension"}}},
			},
			"reasonReference": []map[string]any{
				{"reference": "Condition/cond-au-001"},
			},
			"dosage": []map[string]any{
				{"text": "Take 1 tablet by mouth every morning"},
			},
		}),
		putEntry("Observation/obs-au-weight", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-weight",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-bodyweight"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "29463-7", "display": "Body weight"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 70.5, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"},
		}),
		putEntry("Observation/obs-au-height", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-height",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-bodyheight"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8302-2", "display": "Body height"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 172.0, "unit": "cm", "system": "http://unitsofmeasure.org", "code": "cm"},
		}),
		putEntry("Observation/obs-au-temp", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-temp",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-bodytemp"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8310-5", "display": "Body temperature"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 36.8, "unit": "Cel", "system": "http://unitsofmeasure.org", "code": "Cel"},
		}),
		putEntry("Observation/obs-au-hr", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-hr",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-heartrate"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 72.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"},
		}),
		putEntry("Observation/obs-au-rr", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-rr",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-resprate"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "9279-1", "display": "Respiratory rate"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 16.0, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"},
		}),
		putEntry("Observation/obs-au-waist", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-waist",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-waistcircum"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8280-0", "display": "Waist Circumference at umbilicus by Tape measure"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 82.0, "unit": "cm", "system": "http://unitsofmeasure.org", "code": "cm"},
		}),
		putEntry("Observation/obs-au-bp-1", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-bp-1",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-bloodpressure"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure panel"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:05:00Z",
			"component": []map[string]any{
				{
					"code":          map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic blood pressure"}}},
					"valueQuantity": map[string]any{"value": 120.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"},
				},
				{
					"code":          map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8462-4", "display": "Diastolic blood pressure"}}},
					"valueQuantity": map[string]any{"value": 80.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"},
				},
			},
		}),
		putEntry("Observation/obs-au-bp-2", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-bp-2",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-bloodpressure"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure panel"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:06:00Z",
			"component": []map[string]any{
				{
					"code": map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic blood pressure"}}},
					"dataAbsentReason": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason", "code": "unknown"}},
					},
				},
				{
					"code": map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8462-4", "display": "Diastolic blood pressure"}}},
					"dataAbsentReason": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason", "code": "unknown"}},
					},
				},
			},
		}),
		putEntry("Observation/obs-au-smoking", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-smoking",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-smokingstatus"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history", "display": "Social History"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "1747861000168109", "display": "Tobacco smoking consumption status"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:05:00Z",
			"valueCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "266919005", "display": "Never smoked tobacco"},
				},
			},
		}),
		putEntry("Observation/obs-au-path-member", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-path-member",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-diagnosticresult-path"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}}},
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "CH"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "2823-3", "display": "Potassium [Moles/volume] in Serum or Plasma"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:15:00Z",
			"performer":         []map[string]any{{"reference": "PractitionerRole/pr-au-001"}},
			"specimen":          map[string]any{"reference": "Specimen/spec-au-001"},
			"valueQuantity":     map[string]any{"value": 4.2, "unit": "mmol/L", "system": "http://unitsofmeasure.org", "code": "mmol/L"},
			"interpretation": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-ObservationInterpretation", "code": "N", "display": "Normal"}}},
			},
			"referenceRange": []map[string]any{
				{
					"low":  map[string]any{"value": 3.5, "unit": "mmol/L", "system": "http://unitsofmeasure.org", "code": "mmol/L"},
					"high": map[string]any{"value": 5.2, "unit": "mmol/L", "system": "http://unitsofmeasure.org", "code": "mmol/L"},
					"type": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/referencerange-meaning", "code": "normal"}}},
					"text": "3.5 - 5.2 mmol/L",
				},
			},
			"component": []map[string]any{
				{
					"code":          map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "2075-0", "display": "Chloride [Moles/volume] in Serum or Plasma"}}},
					"valueQuantity": map[string]any{"value": 102.0, "unit": "mmol/L", "system": "http://unitsofmeasure.org", "code": "mmol/L"},
				},
			},
		}),
		putEntry("Observation/obs-au-path", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-path",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-diagnosticresult-path"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}}},
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "CH"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "2951-2", "display": "Sodium [Moles/volume] in Serum or Plasma"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:15:00Z",
			"performer":         []map[string]any{{"reference": "PractitionerRole/pr-au-001"}},
			"specimen":          map[string]any{"reference": "Specimen/spec-au-001"},
			"valueQuantity":     map[string]any{"value": 140.0, "unit": "mmol/L", "system": "http://unitsofmeasure.org", "code": "mmol/L"},
			"interpretation": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-ObservationInterpretation", "code": "N", "display": "Normal"}}},
			},
			"referenceRange": []map[string]any{
				{
					"low":  map[string]any{"value": 135.0, "unit": "mmol/L", "system": "http://unitsofmeasure.org", "code": "mmol/L"},
					"high": map[string]any{"value": 145.0, "unit": "mmol/L", "system": "http://unitsofmeasure.org", "code": "mmol/L"},
					"type": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/referencerange-meaning", "code": "normal"}}},
					"text": "135 - 145 mmol/L",
				},
			},
			"hasMember": []map[string]any{{"reference": "Observation/obs-au-path-member"}},
			"component": []map[string]any{
				{
					"code":          map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "1963-8", "display": "Bicarbonate [Moles/volume] in Serum or Plasma"}}},
					"valueQuantity": map[string]any{"value": 24.0, "unit": "mmol/L", "system": "http://unitsofmeasure.org", "code": "mmol/L"},
				},
			},
		}),
		putEntry("Observation/obs-au-diag-member", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-diag-member",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-diagnosticresult"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "imaging"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "24701-5", "display": "Chest X-ray finding"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:20:00Z",
			"performer":         []map[string]any{{"reference": "PractitionerRole/pr-au-001"}},
			"bodySite":          map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "51185008", "display": "Thoracic structure"}}},
			"valueString":       "Clear lung fields, normal cardiomediastinal contour",
			"component": []map[string]any{
				{
					"code":        map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "19005-8", "display": "Radiology Imaging study [Impression] (narrative)"}}},
					"valueString": "No acute cardiopulmonary abnormality",
				},
			},
		}),
		putEntry("Observation/obs-au-diag", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-au-diag",
			"meta":         map[string]any{"profile": []string{"http://hl7.org.au/fhir/core/StructureDefinition/au-core-diagnosticresult"}},
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "imaging"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "24701-5", "display": "Chest X-ray finding"}}},
			"subject":           map[string]any{"reference": "Patient/pat-au-001"},
			"effectiveDateTime": "2026-01-15T09:20:00Z",
			"performer":         []map[string]any{{"reference": "PractitionerRole/pr-au-001"}},
			"bodySite":          map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "51185008", "display": "Thoracic structure"}}},
			"valueString":       "Normal chest study",
			"hasMember":         []map[string]any{{"reference": "Observation/obs-au-diag-member"}},
			"component": []map[string]any{
				{
					"code":        map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "19005-8", "display": "Radiology Imaging study [Impression] (narrative)"}}},
					"valueString": "No acute findings",
				},
			},
		}),
	}

	return json.MarshalIndent(map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry":        entries,
	}, "", "  ")
}
