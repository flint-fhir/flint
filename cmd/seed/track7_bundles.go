package main

import "encoding/json"

func c4bbSupportingInfo(seq int, code string, extra map[string]any) map[string]any {
	m := map[string]any{
		"sequence": seq,
		"category": map[string]any{
			"coding": []map[string]any{
				{
					"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBSupportingInfoType",
					"code":   code,
				},
			},
		},
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func c4bbDiscrimAdj(code string, extra map[string]any) map[string]any {
	m := map[string]any{
		"category": map[string]any{
			"coding": []map[string]any{
				{
					"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBAdjudicationDiscriminator",
					"code":   code,
				},
			},
		},
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func c4bbAmountAdj(sys, code string, val float64) map[string]any {
	return map[string]any{
		"category": map[string]any{
			"coding": []map[string]any{
				{
					"system": sys,
					"code":   code,
				},
			},
		},
		"amount": map[string]any{
			"value":    val,
			"currency": "USD",
		},
	}
}

func c4bbOrgIdentifiers(npi, tin, naic, payerID string) []map[string]any {
	return []map[string]any{
		{
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType", "code": "npi"},
				},
			},
			"system": "http://hl7.org/fhir/sid/us-npi",
			"value":  npi,
		},
		{
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "TAX"},
				},
			},
			"system": "urn:oid:2.16.840.1.113883.4.4",
			"value":  tin,
		},
		{
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType", "code": "naiccode"},
				},
			},
			"system": "urn:oid:2.16.840.1.113883.6.300",
			"value":  naic,
		},
		{
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType", "code": "payerid"},
				},
			},
			"value": payerID,
		},
	}
}

func c4bbPracIdentifiers(npi, tin string) []map[string]any {
	return []map[string]any{
		{
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType", "code": "npi"},
				},
			},
			"system": "http://hl7.org/fhir/sid/us-npi",
			"value":  npi,
		},
		{
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "TAX"},
				},
			},
			"system": "urn:oid:2.16.840.1.113883.4.4",
			"value":  tin,
		},
	}
}

// C4BBTransactionBundleJSON builds an idempotent FHIR R4 transaction Bundle
// covering all 11 HL7 CARIN IG for Blue Button (C4BB) v2.0.0 profiles and all 5 EOB subtypes.
func C4BBTransactionBundleJSON() ([]byte, error) {
	pnoReason := map[string]any{
		"coding": []map[string]any{
			{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "innetwork"},
		},
	}
	paidReason := map[string]any{
		"coding": []map[string]any{
			{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "innetwork"},
		},
	}
	adjReasonCC := map[string]any{
		"coding": []map[string]any{
			{"system": "https://x12.org/codes/claim-adjustment-reason-codes", "code": "45", "display": "Charge exceeds fee schedule"},
		},
	}

	entries := []map[string]any{
		putEntry("Patient/pat-alice-001", map[string]any{
			"resourceType": "Patient",
			"id":           "pat-alice-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-Patient"},
			},
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
						{"url": "text", "valueString": "White"},
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
						{"url": "text", "valueString": "Not Hispanic or Latino"},
					},
				},
				{
					"url":       "http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex",
					"valueCode": "F",
				},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MB"},
						},
					},
					"system": "http://flint.health/c4bb/memberid",
					"value":  "MB-ALICE-C4BB-001",
				},
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType", "code": "um"},
						},
					},
					"system": "http://flint.health/c4bb/uniquememberid",
					"value":  "UM-ALICE-C4BB-001",
				},
			},
			"active":          true,
			"name":            []map[string]any{{"use": "official", "family": "Hernandez", "given": []string{"Alice", "Elena"}}},
			"telecom":         []map[string]any{{"system": "phone", "value": "555-010-2001", "use": "home"}},
			"gender":          "female",
			"birthDate":       "1985-04-12",
			"deceasedBoolean": false,
			"address": []map[string]any{
				{
					"line":       []string{"742 Evergreen Terrace"},
					"city":       "Boston",
					"district":   "Suffolk",
					"state":      "MA",
					"postalCode": "02115",
					"country":    "US",
					"period":     map[string]any{"start": "2020-01-01"},
				},
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
		}),
		putEntry("Organization/org-c4bb-insurer", map[string]any{
			"resourceType": "Organization",
			"id":           "org-c4bb-insurer",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-Organization"},
			},
			"identifier": c4bbOrgIdentifiers("1234567893", "12-3456789", "12345", "PAYER-C4BB-001"),
			"active":     true,
			"name":       "Flint Blue Button Health Plan",
			"telecom":    []map[string]any{{"system": "phone", "value": "800-555-0199"}},
			"address": []map[string]any{
				{
					"line":       []string{"100 Blue Button Plaza"},
					"city":       "Boston",
					"state":      "MA",
					"postalCode": "02110",
					"country":    "US",
				},
			},
		}),
		putEntry("Organization/org-c4bb-hospital", map[string]any{
			"resourceType": "Organization",
			"id":           "org-c4bb-hospital",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-Organization"},
			},
			"identifier": c4bbOrgIdentifiers("1982736455", "98-7654321", "54321", "PROV-ORG-C4BB-002"),
			"active":     true,
			"name":       "Flint General Hospital",
			"telecom":    []map[string]any{{"system": "phone", "value": "617-555-0140"}},
			"address": []map[string]any{
				{
					"line":       []string{"55 Fruit Street"},
					"city":       "Boston",
					"state":      "MA",
					"postalCode": "02114",
					"country":    "US",
				},
			},
		}),
		putEntry("Practitioner/prac-c4bb-001", map[string]any{
			"resourceType": "Practitioner",
			"id":           "prac-c4bb-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-Practitioner"},
			},
			"identifier": c4bbPracIdentifiers("1497758544", "11-2233445"),
			"active":     true,
			"name":       []map[string]any{{"family": "Chen", "given": []string{"Marcus"}}},
		}),
		putEntry("Practitioner/prac-c4bb-002", map[string]any{
			"resourceType": "Practitioner",
			"id":           "prac-c4bb-002",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-Practitioner"},
			},
			"identifier": c4bbPracIdentifiers("1588667638", "22-3344556"),
			"active":     true,
			"name":       []map[string]any{{"family": "Patel", "given": []string{"Priya"}}},
		}),
		putEntry("RelatedPerson/rel-c4bb-001", map[string]any{
			"resourceType": "RelatedPerson",
			"id":           "rel-c4bb-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-RelatedPerson"},
			},
			"active":  true,
			"patient": map[string]any{"reference": "Patient/pat-alice-001"},
			"relationship": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/v3-RoleCode", "code": "SPS", "display": "spouse"},
					},
				},
			},
			"name":    []map[string]any{{"family": "Hernandez", "given": []string{"Carlos"}}},
			"telecom": []map[string]any{{"system": "phone", "value": "555-010-2002"}},
			"address": []map[string]any{
				{
					"line":       []string{"742 Evergreen Terrace"},
					"city":       "Boston",
					"state":      "MA",
					"postalCode": "02115",
					"country":    "US",
				},
			},
		}),
		putEntry("Location/loc-c4bb-001", map[string]any{
			"resourceType": "Location",
			"id":           "loc-c4bb-001",
			"status":       "active",
			"name":         "Flint Dental & Ambulatory Care Center",
			"address": map[string]any{
				"line":       []string{"200 Boylston Street"},
				"city":       "Boston",
				"state":      "MA",
				"postalCode": "02116",
				"country":    "US",
			},
		}),
		putEntry("Coverage/cov-c4bb-001", map[string]any{
			"resourceType": "Coverage",
			"id":           "cov-c4bb-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-Coverage"},
			},
			"identifier": []map[string]any{
				{"system": "http://flint.health/c4bb/coverage", "value": "COV-ALICE-C4BB-001"},
			},
			"status":       "active",
			"subscriberId": "SUB-ALICE-C4BB-001",
			"beneficiary":  map[string]any{"reference": "Patient/pat-alice-001"},
			"subscriber":   map[string]any{"reference": "Patient/pat-alice-001"},
			"relationship": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/subscriber-relationship", "code": "self"},
				},
			},
			"period": map[string]any{"start": "2025-01-01", "end": "2026-12-31"},
			"payor":  []map[string]any{{"reference": "Organization/org-c4bb-insurer"}},
			"class": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/coverage-class", "code": "group"},
						},
					},
					"value": "GRP-C4BB-100",
					"name":  "Flint Employer Group Plan",
				},
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/coverage-class", "code": "plan"},
						},
					},
					"value": "PLN-PPO-GOLD",
					"name":  "Flint Gold PPO",
				},
			},
		}),
		// EOB #1: Inpatient Institutional (carries 5 distinct included refs + Organization payee for Seed Rule C!)
		putEntry("ExplanationOfBenefit/eob-c4bb-01-inpatient", map[string]any{
			"resourceType": "ExplanationOfBenefit",
			"id":           "eob-c4bb-01-inpatient",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-ExplanationOfBenefit-Inpatient-Institutional"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType", "code": "uc"},
						},
					},
					"system": "http://flint.health/c4bb/claim",
					"value":  "UC-INP-001",
				},
			},
			"status": "active",
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "institutional"},
				},
			},
			"subType": map[string]any{
				"coding": []map[string]any{
					{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBInstitutionalClaimSubType", "code": "inpatient"},
				},
			},
			"use":            "claim",
			"patient":        map[string]any{"reference": "Patient/pat-alice-001"},
			"billablePeriod": map[string]any{"start": "2026-01-10", "end": "2026-01-14"},
			"created":        "2026-01-20T10:00:00Z",
			"insurer":        map[string]any{"reference": "Organization/org-c4bb-insurer"},
			"provider":       map[string]any{"reference": "Practitioner/prac-c4bb-001"},
			"outcome":        "complete",
			"related": []map[string]any{
				{
					"relationship": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBRelatedClaimRelationshipCodes", "code": "prior"},
						},
					},
					"reference": map[string]any{"system": "http://flint.health/c4bb/claim", "value": "UC-PRIOR-000"},
				},
			},
			"payee": map[string]any{
				"type": map[string]any{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/payeetype", "code": "provider"},
					},
				},
				"party": map[string]any{"reference": "Organization/org-c4bb-insurer"},
			},
			"careTeam": []map[string]any{
				{
					"sequence": 1,
					"provider": map[string]any{"reference": "Practitioner/prac-c4bb-002"},
					"role": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBClaimCareTeamRole", "code": "attending"},
						},
					},
				},
			},
			"supportingInfo": []map[string]any{
				c4bbSupportingInfo(1, "admissionperiod", map[string]any{"timingPeriod": map[string]any{"start": "2026-01-10", "end": "2026-01-14"}}),
				c4bbSupportingInfo(2, "clmrecvddate", map[string]any{"timingDate": "2026-01-18"}),
				c4bbSupportingInfo(3, "typeofbill", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "https://www.nubc.org/CodeSystem/TypeOfBill", "code": "0111"}}}}),
				c4bbSupportingInfo(4, "pointoforigin", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "https://www.nubc.org/CodeSystem/PointOfOrigin", "code": "1"}}}}),
				c4bbSupportingInfo(5, "admtype", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "https://www.nubc.org/CodeSystem/PriorityTypeOfAdmitOrVisit", "code": "1"}}}}),
				c4bbSupportingInfo(6, "discharge-status", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "https://www.nubc.org/CodeSystem/PatDischargeStatus", "code": "01"}}}}),
				c4bbSupportingInfo(7, "drg", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "https://www.cms.gov/Medicare/Medicare-Fee-for-Service-Payment/AcuteInpatientPPS/MS-DRG-Classifications-and-Software", "code": "291"}}}}),
				c4bbSupportingInfo(8, "medicalrecordnumber", map[string]any{"valueString": "MRN-ALICE-001"}),
				c4bbSupportingInfo(9, "patientaccountnumber", map[string]any{"valueString": "ACCT-ALICE-001"}),
				c4bbSupportingInfo(10, "billingnetworkcontractingstatus", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "contracted"}}}}),
			},
			"diagnosis": []map[string]any{
				{
					"sequence": 1,
					"diagnosisCodeableConcept": map[string]any{
						"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/icd-10-cm", "code": "I50.9"}},
					},
					"type": []map[string]any{
						{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/ex-diagnosistype", "code": "principal"}}},
					},
					"onAdmission": map[string]any{
						"coding": []map[string]any{{"system": "https://www.cms.gov/Medicare/Medicare-Fee-for-Service-Payment/HospitalAcqCond/Coding", "code": "Y"}},
					},
				},
			},
			"procedure": []map[string]any{
				{
					"sequence": 1,
					"type": []map[string]any{
						{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBClaimProcedureType", "code": "principal"}}},
					},
					"date": "2026-01-11",
					"procedureCodeableConcept": map[string]any{
						"coding": []map[string]any{{"system": "http://www.cms.gov/Medicare/Coding/ICD10", "code": "02HV33Z"}},
					},
				},
			},
			"insurance": []map[string]any{
				{"focal": true, "coverage": map[string]any{"reference": "Coverage/cov-c4bb-001"}},
			},
			"item": []map[string]any{
				{
					"sequence": 1,
					"revenue": map[string]any{
						"coding": []map[string]any{{"system": "https://www.nubc.org/CodeSystem/RevenueCodes", "code": "0120"}},
					},
					"productOrService": map[string]any{
						"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "99223"}},
					},
					"modifier": []map[string]any{
						{"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "25"}}},
					},
					"servicedPeriod": map[string]any{"start": "2026-01-10", "end": "2026-01-14"},
					"quantity":       map[string]any{"value": 4},
					"noteNumber":     []int{1},
					"adjudication": []map[string]any{
						c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 4200.00),
						c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "benefit", 3800.00),
						c4bbDiscrimAdj("adjustmentreason", map[string]any{"reason": adjReasonCC}),
						c4bbDiscrimAdj("denialreason", map[string]any{"reason": adjReasonCC}),
						c4bbDiscrimAdj("allowedunits", map[string]any{"value": 4}),
					},
				},
			},
			"adjudication": []map[string]any{
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 4200.00),
				c4bbDiscrimAdj("adjustmentreason", map[string]any{"reason": adjReasonCC}),
				c4bbDiscrimAdj("denialreason", map[string]any{"reason": adjReasonCC}),
				c4bbDiscrimAdj("billingnetworkstatus", map[string]any{"reason": pnoReason}),
				c4bbDiscrimAdj("benefitpaymentstatus", map[string]any{"reason": paidReason}),
			},
			"total": []map[string]any{
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 4200.00),
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "benefit", 3800.00),
				c4bbAmountAdj("http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "innetwork", 3800.00),
			},
			"payment": map[string]any{
				"type": map[string]any{
					"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "paid"}},
				},
				"date":   "2026-01-22",
				"amount": map[string]any{"value": 3800.00, "currency": "USD"},
			},
			"processNote": []map[string]any{
				{"number": 1, "text": "Inpatient institutional claim processed in full."},
			},
		}),
		// EOB #2: Outpatient Institutional (references RelatedPerson/rel-c4bb-001 on payee.party for Seed Rule B!)
		putEntry("ExplanationOfBenefit/eob-c4bb-02-outpatient", map[string]any{
			"resourceType": "ExplanationOfBenefit",
			"id":           "eob-c4bb-02-outpatient",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-ExplanationOfBenefit-Outpatient-Institutional"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType", "code": "uc"},
						},
					},
					"system": "http://flint.health/c4bb/claim",
					"value":  "UC-OUT-002",
				},
			},
			"status": "active",
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "institutional"},
				},
			},
			"subType": map[string]any{
				"coding": []map[string]any{
					{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBInstitutionalClaimSubType", "code": "outpatient"},
				},
			},
			"use":            "claim",
			"patient":        map[string]any{"reference": "Patient/pat-alice-001"},
			"billablePeriod": map[string]any{"start": "2026-02-05", "end": "2026-02-05"},
			"created":        "2026-02-10T10:00:00Z",
			"insurer":        map[string]any{"reference": "Organization/org-c4bb-insurer"},
			"provider":       map[string]any{"reference": "Organization/org-c4bb-hospital"},
			"outcome":        "complete",
			"payee": map[string]any{
				"type": map[string]any{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/payeetype", "code": "other"},
					},
				},
				"party": map[string]any{"reference": "RelatedPerson/rel-c4bb-001"},
			},
			"careTeam": []map[string]any{
				{
					"sequence": 1,
					"provider": map[string]any{"reference": "Practitioner/prac-c4bb-001"},
					"role": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBClaimCareTeamRole", "code": "performing"},
						},
					},
				},
			},
			"supportingInfo": []map[string]any{
				c4bbSupportingInfo(1, "clmrecvddate", map[string]any{"timingDate": "2026-02-07"}),
				c4bbSupportingInfo(2, "typeofbill", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "https://www.nubc.org/CodeSystem/TypeOfBill", "code": "0131"}}}}),
				c4bbSupportingInfo(3, "pointoforigin", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "https://www.nubc.org/CodeSystem/PointOfOrigin", "code": "1"}}}}),
				c4bbSupportingInfo(4, "admtype", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "https://www.nubc.org/CodeSystem/PriorityTypeOfAdmitOrVisit", "code": "3"}}}}),
				c4bbSupportingInfo(5, "discharge-status", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "https://www.nubc.org/CodeSystem/PatDischargeStatus", "code": "01"}}}}),
				c4bbSupportingInfo(6, "medicalrecordnumber", map[string]any{"valueString": "MRN-ALICE-001"}),
				c4bbSupportingInfo(7, "patientaccountnumber", map[string]any{"valueString": "ACCT-ALICE-002"}),
				c4bbSupportingInfo(8, "billingnetworkcontractingstatus", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "contracted"}}}}),
			},
			"diagnosis": []map[string]any{
				{
					"sequence": 1,
					"diagnosisCodeableConcept": map[string]any{
						"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/icd-10-cm", "code": "I10"}},
					},
					"type": []map[string]any{
						{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/ex-diagnosistype", "code": "principal"}}},
					},
				},
			},
			"insurance": []map[string]any{
				{"focal": true, "coverage": map[string]any{"reference": "Coverage/cov-c4bb-001"}},
			},
			"item": []map[string]any{
				{
					"sequence": 1,
					"revenue": map[string]any{
						"coding": []map[string]any{{"system": "https://www.nubc.org/CodeSystem/RevenueCodes", "code": "0300"}},
					},
					"productOrService": map[string]any{
						"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "80053"}},
					},
					"modifier": []map[string]any{
						{"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "26"}}},
					},
					"servicedDate": "2026-02-05",
					"quantity":     map[string]any{"value": 1},
					"noteNumber":   []int{1},
					"adjudication": []map[string]any{
						c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 250.00),
						c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "benefit", 210.00),
						c4bbDiscrimAdj("adjustmentreason", map[string]any{"reason": adjReasonCC}),
						c4bbDiscrimAdj("denialreason", map[string]any{"reason": adjReasonCC}),
						c4bbDiscrimAdj("allowedunits", map[string]any{"value": 1}),
					},
				},
			},
			"adjudication": []map[string]any{
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 250.00),
				c4bbDiscrimAdj("adjustmentreason", map[string]any{"reason": adjReasonCC}),
				c4bbDiscrimAdj("denialreason", map[string]any{"reason": adjReasonCC}),
				c4bbDiscrimAdj("billingnetworkstatus", map[string]any{"reason": pnoReason}),
				c4bbDiscrimAdj("benefitpaymentstatus", map[string]any{"reason": paidReason}),
			},
			"total": []map[string]any{
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 250.00),
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "benefit", 210.00),
				c4bbAmountAdj("http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "innetwork", 210.00),
			},
			"payment": map[string]any{
				"type": map[string]any{
					"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "paid"}},
				},
				"date":   "2026-02-12",
				"amount": map[string]any{"value": 210.00, "currency": "USD"},
			},
			"processNote": []map[string]any{
				{"number": 1, "text": "Outpatient laboratory panel adjudicated."},
			},
		}),
		// EOB #3: Oral
		putEntry("ExplanationOfBenefit/eob-c4bb-03-oral", map[string]any{
			"resourceType": "ExplanationOfBenefit",
			"id":           "eob-c4bb-03-oral",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-ExplanationOfBenefit-Oral"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType", "code": "uc"},
						},
					},
					"system": "http://flint.health/c4bb/claim",
					"value":  "UC-ORAL-003",
				},
			},
			"status": "active",
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "oral"},
				},
			},
			"use":            "claim",
			"patient":        map[string]any{"reference": "Patient/pat-alice-001"},
			"billablePeriod": map[string]any{"start": "2026-03-01", "end": "2026-03-01"},
			"created":        "2026-03-05T10:00:00Z",
			"insurer":        map[string]any{"reference": "Organization/org-c4bb-insurer"},
			"provider":       map[string]any{"reference": "Practitioner/prac-c4bb-001"},
			"outcome":        "complete",
			"payee": map[string]any{
				"type": map[string]any{
					"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/payeetype", "code": "provider"}},
				},
				"party": map[string]any{"reference": "Practitioner/prac-c4bb-001"},
			},
			"careTeam": []map[string]any{
				{
					"sequence": 1,
					"provider": map[string]any{"reference": "Practitioner/prac-c4bb-001"},
					"role": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBClaimCareTeamRole", "code": "rendering"},
						},
					},
					"qualification": map[string]any{
						"coding": []map[string]any{
							{"system": "http://nucc.org/provider-taxonomy", "code": "122300000X"},
						},
					},
				},
			},
			"supportingInfo": []map[string]any{
				c4bbSupportingInfo(1, "clmrecvddate", map[string]any{"timingDate": "2026-03-03"}),
				c4bbSupportingInfo(2, "servicefacility", map[string]any{"valueReference": map[string]any{"reference": "Location/loc-c4bb-001"}}),
				c4bbSupportingInfo(3, "orthodontics", map[string]any{"valueBoolean": false}),
				c4bbSupportingInfo(4, "prosthesis", map[string]any{"valueBoolean": false}),
				c4bbSupportingInfo(5, "additionalbodysite", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "245652007"}}}}),
				c4bbSupportingInfo(6, "missingtoothnumber", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/ex-tooth", "code": "18"}}}}),
				c4bbSupportingInfo(7, "medicalrecordnumber", map[string]any{"valueString": "MRN-ALICE-001"}),
				c4bbSupportingInfo(8, "patientaccountnumber", map[string]any{"valueString": "ACCT-ALICE-003"}),
			},
			"diagnosis": []map[string]any{
				{
					"sequence": 1,
					"diagnosisCodeableConcept": map[string]any{
						"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/icd-10-cm", "code": "K02.9"}},
					},
					"type": []map[string]any{
						{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/ex-diagnosistype", "code": "principal"}}},
					},
				},
			},
			"insurance": []map[string]any{
				{"focal": true, "coverage": map[string]any{"reference": "Coverage/cov-c4bb-001"}},
			},
			"item": []map[string]any{
				{
					"sequence":            1,
					"informationSequence": []int{5},
					"productOrService": map[string]any{
						"coding": []map[string]any{{"system": "http://www.ada.org/cdt", "code": "D0120"}},
					},
					"modifier": []map[string]any{
						{"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "22"}}},
					},
					"servicedDate": "2026-03-01",
					"locationCodeableConcept": map[string]any{
						"coding": []map[string]any{{"system": "https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set", "code": "11"}},
					},
					"bodySite": map[string]any{
						"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/ex-tooth", "code": "14"}},
					},
					"subSite": []map[string]any{
						{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/FDI-surface", "code": "M"}}},
					},
					"quantity":   map[string]any{"value": 1},
					"noteNumber": []int{1},
					"adjudication": []map[string]any{
						c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 120.00),
						c4bbDiscrimAdj("adjustmentreason", map[string]any{"reason": adjReasonCC}),
						c4bbDiscrimAdj("benefitpaymentstatus", map[string]any{"reason": paidReason}),
						c4bbDiscrimAdj("allowedunits", map[string]any{"value": 1}),
					},
				},
			},
			"adjudication": []map[string]any{
				c4bbDiscrimAdj("billingnetworkstatus", map[string]any{"reason": pnoReason}),
				c4bbDiscrimAdj("renderingnetworkstatus", map[string]any{"reason": pnoReason}),
				c4bbDiscrimAdj("benefitpaymentstatus", map[string]any{"reason": paidReason}),
			},
			"total": []map[string]any{
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 120.00),
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "benefit", 100.00),
			},
			"payment": map[string]any{
				"type": map[string]any{
					"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "paid"}},
				},
				"date":   "2026-03-08",
				"amount": map[string]any{"value": 100.00, "currency": "USD"},
			},
			"processNote": []map[string]any{
				{"number": 1, "text": "Periodic oral evaluation adjudicated."},
			},
		}),
		// EOB #4: Pharmacy
		putEntry("ExplanationOfBenefit/eob-c4bb-04-pharmacy", map[string]any{
			"resourceType": "ExplanationOfBenefit",
			"id":           "eob-c4bb-04-pharmacy",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-ExplanationOfBenefit-Pharmacy"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType", "code": "uc"},
						},
					},
					"system": "http://flint.health/c4bb/claim",
					"value":  "UC-PHARM-004",
				},
			},
			"status": "active",
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "pharmacy"},
				},
			},
			"use":            "claim",
			"patient":        map[string]any{"reference": "Patient/pat-alice-001"},
			"billablePeriod": map[string]any{"start": "2026-03-15", "end": "2026-03-15"},
			"created":        "2026-03-16T10:00:00Z",
			"insurer":        map[string]any{"reference": "Organization/org-c4bb-insurer"},
			"provider":       map[string]any{"reference": "Organization/org-c4bb-hospital"},
			"outcome":        "complete",
			"payee": map[string]any{
				"type": map[string]any{
					"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/payeetype", "code": "provider"}},
				},
				"party": map[string]any{"reference": "Organization/org-c4bb-hospital"},
			},
			"careTeam": []map[string]any{
				{
					"sequence":    1,
					"provider":    map[string]any{"reference": "Practitioner/prac-c4bb-001"},
					"responsible": true,
					"role": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBClaimCareTeamRole", "code": "prescribing"},
						},
					},
				},
			},
			"supportingInfo": []map[string]any{
				c4bbSupportingInfo(1, "dayssupply", map[string]any{"valueQuantity": map[string]any{"value": 30}}),
				c4bbSupportingInfo(2, "dawcode", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/NCPDPDispensedAsWrittenOrProductSelectionCode", "code": "0"}}}}),
				c4bbSupportingInfo(3, "refillnum", map[string]any{"valueQuantity": map[string]any{"value": 1}}),
				c4bbSupportingInfo(4, "refillsauthorized", map[string]any{"valueQuantity": map[string]any{"value": 5}}),
				c4bbSupportingInfo(5, "brandgenericindicator", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/NCPDPBrandGenericIndicator", "code": "G"}}}}),
				c4bbSupportingInfo(6, "rxorigincode", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/NCPDPRxOriginCode", "code": "3"}}}}),
				c4bbSupportingInfo(7, "clmrecvddate", map[string]any{"timingDate": "2026-03-15"}),
				c4bbSupportingInfo(8, "compoundcode", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/NCPDPCompoundCode", "code": "1"}}}}),
				c4bbSupportingInfo(9, "billingnetworkcontractingstatus", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "contracted"}}}}),
			},
			"insurance": []map[string]any{
				{"focal": true, "coverage": map[string]any{"reference": "Coverage/cov-c4bb-001"}},
			},
			"item": []map[string]any{
				{
					"sequence": 1,
					"productOrService": map[string]any{
						"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/ndc", "code": "00071-0155-23"}},
					},
					"servicedDate": "2026-03-15",
					"quantity":     map[string]any{"value": 30},
					"noteNumber":   []int{1},
					"adjudication": []map[string]any{
						c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 45.00),
						c4bbAmountAdj("http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBAdjudication", "drugcost", 40.00),
						c4bbDiscrimAdj("denialreason", map[string]any{"reason": adjReasonCC}),
						c4bbDiscrimAdj("rejectreason", map[string]any{
							"reason": map[string]any{
								"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/NCPDPRejectCode", "code": "70"}},
							},
						}),
					},
					"detail": []map[string]any{
						{
							"sequence": 1,
							"productOrService": map[string]any{
								"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/ndc", "code": "00071-0155-23"}},
							},
							"quantity": map[string]any{"value": 30},
						},
					},
				},
			},
			"adjudication": []map[string]any{
				c4bbDiscrimAdj("billingnetworkstatus", map[string]any{"reason": pnoReason}),
				c4bbDiscrimAdj("benefitpaymentstatus", map[string]any{"reason": paidReason}),
			},
			"total": []map[string]any{
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 45.00),
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "benefit", 35.00),
				c4bbAmountAdj("http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "innetwork", 35.00),
			},
			"payment": map[string]any{
				"type": map[string]any{
					"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "paid"}},
				},
				"date":   "2026-03-17",
				"amount": map[string]any{"value": 35.00, "currency": "USD"},
			},
			"processNote": []map[string]any{
				{"number": 1, "text": "Pharmacy prescription claim adjudicated."},
			},
		}),
		// EOB #5: Professional NonClinician
		putEntry("ExplanationOfBenefit/eob-c4bb-05-professional", map[string]any{
			"resourceType": "ExplanationOfBenefit",
			"id":           "eob-c4bb-05-professional",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/carin-bb/StructureDefinition/C4BB-ExplanationOfBenefit-Professional-NonClinician"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType", "code": "uc"},
						},
					},
					"system": "http://flint.health/c4bb/claim",
					"value":  "UC-PROF-005",
				},
			},
			"status": "active",
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "professional"},
				},
			},
			"use":            "claim",
			"patient":        map[string]any{"reference": "Patient/pat-alice-001"},
			"billablePeriod": map[string]any{"start": "2026-04-02", "end": "2026-04-02"},
			"created":        "2026-04-05T10:00:00Z",
			"insurer":        map[string]any{"reference": "Organization/org-c4bb-insurer"},
			"provider":       map[string]any{"reference": "Practitioner/prac-c4bb-001"},
			"outcome":        "complete",
			"payee": map[string]any{
				"type": map[string]any{
					"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/payeetype", "code": "provider"}},
				},
				"party": map[string]any{"reference": "Practitioner/prac-c4bb-001"},
			},
			"careTeam": []map[string]any{
				{
					"sequence": 1,
					"provider": map[string]any{"reference": "Practitioner/prac-c4bb-001"},
					"role": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBClaimCareTeamRole", "code": "rendering"},
						},
					},
					"qualification": map[string]any{
						"coding": []map[string]any{
							{"system": "http://nucc.org/provider-taxonomy", "code": "207RC0000X"},
						},
					},
				},
			},
			"supportingInfo": []map[string]any{
				c4bbSupportingInfo(1, "clmrecvddate", map[string]any{"timingDate": "2026-04-03"}),
				c4bbSupportingInfo(2, "servicefacility", map[string]any{"valueReference": map[string]any{"reference": "Location/loc-c4bb-001"}}),
				c4bbSupportingInfo(3, "medicalrecordnumber", map[string]any{"valueString": "MRN-ALICE-001"}),
				c4bbSupportingInfo(4, "patientaccountnumber", map[string]any{"valueString": "ACCT-ALICE-005"}),
				c4bbSupportingInfo(5, "patientweight", map[string]any{"valueQuantity": map[string]any{"value": 68, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"}}),
				c4bbSupportingInfo(6, "ambulancetransportreason", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBAmbulanceTransportReasonCodes", "code": "A"}}}}),
				c4bbSupportingInfo(7, "transportationdistance", map[string]any{"valueQuantity": map[string]any{"value": 12, "unit": "mi", "system": "http://unitsofmeasure.org", "code": "[mi_i]"}}),
				c4bbSupportingInfo(8, "roudtrippurpose", map[string]any{"valueString": "Specialty cardiology evaluation"}),
				c4bbSupportingInfo(9, "stretcherpurpose", map[string]any{"valueString": "Cardiac monitoring required"}),
				c4bbSupportingInfo(10, "pickuplocation", map[string]any{"valueString": "742 Evergreen Terrace, Boston, MA"}),
				c4bbSupportingInfo(11, "dropofflocation", map[string]any{"valueString": "55 Fruit Street, Boston, MA"}),
				c4bbSupportingInfo(12, "billingnetworkcontractingstatus", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "contracted"}}}}),
				c4bbSupportingInfo(13, "performingnetworkcontractingstatus", map[string]any{"code": map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "contracted"}}}}),
			},
			"diagnosis": []map[string]any{
				{
					"sequence": 1,
					"diagnosisCodeableConcept": map[string]any{
						"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/icd-10-cm", "code": "I25.10"}},
					},
					"type": []map[string]any{
						{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/ex-diagnosistype", "code": "principal"}}},
					},
				},
			},
			"insurance": []map[string]any{
				{"focal": true, "coverage": map[string]any{"reference": "Coverage/cov-c4bb-001"}},
			},
			"item": []map[string]any{
				{
					"sequence": 1,
					"productOrService": map[string]any{
						"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "99214"}},
					},
					"modifier": []map[string]any{
						{"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "25"}}},
					},
					"servicedPeriod": map[string]any{"start": "2026-04-02", "end": "2026-04-02"},
					"locationCodeableConcept": map[string]any{
						"coding": []map[string]any{{"system": "https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set", "code": "11"}},
					},
					"quantity":   map[string]any{"value": 1},
					"noteNumber": []int{1},
					"adjudication": []map[string]any{
						c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 180.00),
						c4bbDiscrimAdj("adjustmentreason", map[string]any{"reason": adjReasonCC}),
						c4bbDiscrimAdj("denialreason", map[string]any{"reason": adjReasonCC}),
						c4bbDiscrimAdj("benefitpaymentstatus", map[string]any{"reason": paidReason}),
						c4bbDiscrimAdj("allowedunits", map[string]any{"value": 1}),
					},
				},
			},
			"adjudication": []map[string]any{
				c4bbDiscrimAdj("billingnetworkstatus", map[string]any{"reason": pnoReason}),
				c4bbDiscrimAdj("renderingnetworkstatus", map[string]any{"reason": pnoReason}),
				c4bbDiscrimAdj("benefitpaymentstatus", map[string]any{"reason": paidReason}),
			},
			"total": []map[string]any{
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "submitted", 180.00),
				c4bbAmountAdj("http://terminology.hl7.org/CodeSystem/adjudication", "benefit", 150.00),
			},
			"payment": map[string]any{
				"type": map[string]any{
					"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBPayerAdjudicationStatus", "code": "paid"}},
				},
				"date":   "2026-04-07",
				"amount": map[string]any{"value": 150.00, "currency": "USD"},
			},
			"processNote": []map[string]any{
				{"number": 1, "text": "Professional cardiology visit adjudicated."},
			},
		}),
	}

	return json.Marshal(map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry":        entries,
	})
}

// PDexTransactionBundleJSON builds an idempotent FHIR R4 transaction Bundle
// combining the longitudinal US Core clinical cohort with Da Vinci PDex v2.0.0
// Prior Authorization ExplanationOfBenefit, PDex Device, PDex MedicationDispense,
// PDex Provenance, and duplicate $member-match negative-test fixtures.
func PDexTransactionBundleJSON() ([]byte, error) {
	baseBytes, err := CohortTransactionBundleJSON()
	if err != nil {
		return nil, err
	}
	var baseBundle map[string]any
	if err := json.Unmarshal(baseBytes, &baseBundle); err != nil {
		return nil, err
	}
	rawEntries, _ := baseBundle["entry"].([]any)

	pdexReviewAction := map[string]any{
		"url": "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewAction",
		"extension": []map[string]any{
			{
				"url": "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewActionCode",
				"valueCodeableConcept": map[string]any{
					"coding": []map[string]any{
						{"system": "https://codesystem.x12.org/005010/306", "code": "A1", "display": "Certified in total"},
					},
				},
			},
		},
	}

	extra := []map[string]any{
		putEntry("ExplanationOfBenefit/eob-pdex-pa-001", map[string]any{
			"resourceType": "ExplanationOfBenefit",
			"id":           "eob-pdex-pa-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-priorauthorization"},
			},
			"extension": []map[string]any{
				{
					"url": "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-levelOfServiceCode",
					"valueCodeableConcept": map[string]any{
						"coding": []map[string]any{
							{"system": "https://codesystem.x12.org/005010/1338", "code": "U", "display": "Urgent"},
						},
					},
				},
			},
			"identifier": []map[string]any{
				{"system": "http://flint.health/pdex/priorauth", "value": "PA-PDEX-ALICE-001"},
			},
			"status": "active",
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "professional"},
				},
			},
			"use":            "preauthorization",
			"patient":        map[string]any{"reference": "Patient/pat-alice-001"},
			"billablePeriod": map[string]any{"start": "2026-02-01", "end": "2026-02-28"},
			"created":        "2026-02-01T12:00:00Z",
			"enterer":        map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"insurer":        map[string]any{"reference": "Organization/org-flint-health"},
			"provider":       map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"priority": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/processpriority", "code": "normal"},
				},
			},
			"facility":         map[string]any{"reference": "Location/loc-flint-cardiology"},
			"outcome":          "complete",
			"preAuthRef":       []string{"AUTH-PDEX-1001"},
			"preAuthRefPeriod": []map[string]any{{"start": "2026-02-01", "end": "2026-05-01"}},
			"careTeam": []map[string]any{
				{
					"sequence": 1,
					"provider": map[string]any{"reference": "Practitioner/prac-cardiology-1"},
				},
			},
			"insurance": []map[string]any{
				{"focal": true, "coverage": map[string]any{"reference": "Coverage/cov-alice-medicare"}},
			},
			"item": []map[string]any{
				{
					"extension": []map[string]any{
						{
							"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber",
							"valueIdentifier": map[string]any{
								"system": "http://flint.health/pdex/trace",
								"value":  "TRACE-PDEX-001",
							},
						},
						{
							"url":       "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemPreAuthIssueDate",
							"valueDate": "2026-02-01",
						},
						{
							"url":         "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemPreAuthPeriod",
							"valuePeriod": map[string]any{"start": "2026-02-01", "end": "2026-05-01"},
						},
						{
							"url":         "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-authorizationNumber",
							"valueString": "AUTH-PDEX-PREV-000",
						},
						{
							"url":         "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-administrationReferenceNumber",
							"valueString": "ADMIN-PDEX-001",
						},
						{
							"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemAuthorizedDetail",
							"extension": []map[string]any{
								{
									"url": "productOrServiceCode",
									"valueCodeableConcept": map[string]any{
										"coding": []map[string]any{
											{"system": "http://www.ama-assn.org/go/cpt", "code": "93306"},
										},
									},
								},
							},
						},
						{
							"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemAuthorizedProvider",
							"extension": []map[string]any{
								{
									"url":            "provider",
									"valueReference": map[string]any{"reference": "Practitioner/prac-cardiology-1"},
								},
							},
						},
					},
					"sequence": 1,
					"category": map[string]any{
						"coding": []map[string]any{
							{"system": "https://codesystem.x12.org/005010/1365", "code": "3", "display": "Consultation"},
						},
					},
					"productOrService": map[string]any{
						"coding": []map[string]any{
							{"system": "http://www.ama-assn.org/go/cpt", "code": "93306", "display": "Echocardiography"},
						},
					},
					"servicedDate": "2026-02-10",
					"adjudication": []map[string]any{
						{
							"extension": []map[string]any{pdexReviewAction},
							"category": map[string]any{
								"coding": []map[string]any{
									{"system": "http://terminology.hl7.org/CodeSystem/adjudication", "code": "submitted"},
								},
							},
							"amount": map[string]any{"value": 850.00, "currency": "USD"},
						},
						{
							"category": map[string]any{
								"coding": []map[string]any{
									{"system": "http://hl7.org/fhir/us/davinci-pdex/CodeSystem/PDexAdjudicationDiscriminator", "code": "allowedunits"},
								},
							},
							"value": 1,
						},
						{
							"category": map[string]any{
								"coding": []map[string]any{
									{"system": "http://hl7.org/fhir/us/davinci-pdex/CodeSystem/PDexAdjudicationDiscriminator", "code": "consumedunits"},
								},
							},
							"value": 1,
						},
						{
							"category": map[string]any{
								"coding": []map[string]any{
									{"system": "http://hl7.org/fhir/us/davinci-pdex/CodeSystem/PDexAdjudicationDiscriminator", "code": "denialreason"},
								},
							},
							"reason": map[string]any{
								"coding": []map[string]any{
									{"system": "https://codesystem.x12.org/005010/1328", "code": "01"},
								},
							},
						},
					},
				},
			},
			"adjudication": []map[string]any{
				{
					"extension": []map[string]any{pdexReviewAction},
					"category": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/adjudication", "code": "submitted"},
						},
					},
					"amount": map[string]any{"value": 850.00, "currency": "USD"},
				},
				{
					"category": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/davinci-pdex/CodeSystem/PDexPayerAdjudicationStatus", "code": "innetwork"},
						},
					},
					"reason": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/davinci-pdex/CodeSystem/PDexPayerAdjudicationStatus", "code": "innetwork"},
						},
					},
				},
			},
			"total": []map[string]any{
				{
					"extension": []map[string]any{
						{
							"url":          "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/PriorAuthorizationUtilization",
							"valueBoolean": true,
						},
					},
					"category": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/adjudication", "code": "submitted"},
						},
					},
					"amount": map[string]any{"value": 850.00, "currency": "USD"},
				},
			},
		}),
		putEntry("Device/dev-pdex-001", map[string]any{
			"resourceType": "Device",
			"id":           "dev-pdex-001",
			"meta": map[string]any{
				"profile": []string{
					"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-device",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-implantable-device",
				},
			},
			"udiCarrier": []map[string]any{
				{
					"deviceIdentifier": "00844588003288",
					"carrierHRF":       "(01)00844588003288(11)240115(17)290115(10)LOTPDEX01(21)SERPDEX01",
					"carrierAIDC":      "KDAxKTAwODQ0NTg4MDAzMjg4",
					"entryType":        "barcode",
				},
			},
			"status":             "active",
			"distinctIdentifier": "DIST-PDEX-001",
			"expirationDate":     "2029-01-15T00:00:00Z",
			"lotNumber":          "LOTPDEX01",
			"serialNumber":       "SERPDEX01",
			"deviceName": []map[string]any{
				{"name": "Dual-Chamber Implantable Pacemaker", "type": "udi-label-name"},
			},
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://snomed.info/sct", "code": "14106009", "display": "Cardiac pacemaker, device"},
				},
			},
			"patient": map[string]any{"reference": "Patient/pat-alice-001"},
			"url":     "https://flint.health/devices/dev-pdex-001",
		}),
		putEntry("MedicationDispense/meddisp-pdex-001", map[string]any{
			"resourceType": "MedicationDispense",
			"id":           "meddisp-pdex-001",
			"meta": map[string]any{
				"profile": []string{
					"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-medicationdispense",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-medicationdispense",
				},
			},
			"status": "completed",
			"category": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/fhir/CodeSystem/medicationdispense-category", "code": "outpatient"},
				},
			},
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{
					{"system": "http://hl7.org/fhir/sid/ndc", "code": "00071-0155-23", "display": "Atorvastatin 10 MG Oral Tablet"},
					{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "617312", "display": "atorvastatin 10 MG Oral Tablet"},
				},
			},
			"subject": map[string]any{"reference": "Patient/pat-alice-001"},
			"performer": []map[string]any{
				{"actor": map[string]any{"reference": "Practitioner/prac-cardiology-1"}},
			},
			"type": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "FF", "display": "First Fill"},
				},
			},
			"quantity":       map[string]any{"value": 30, "unit": "TAB"},
			"daysSupply":     map[string]any{"value": 30, "unit": "d"},
			"whenHandedOver": "2026-02-12T14:30:00Z",
			"substitution":   map[string]any{"wasSubstituted": false},
		}),
		putEntry("Provenance/prov-pdex-001", map[string]any{
			"resourceType": "Provenance",
			"id":           "prov-pdex-001",
			"meta": map[string]any{
				"profile": []string{
					"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-provenance",
					"http://hl7.org/fhir/us/core/StructureDefinition/us-core-provenance",
				},
			},
			"target": []map[string]any{
				{"reference": "Patient/pat-alice-001"},
				{"reference": "Device/dev-pdex-001"},
				{"reference": "Device/dev-alice-pacemaker"},
				{"reference": "MedicationDispense/meddisp-pdex-001"},
				{"reference": "MedicationDispense/meddisp-alice-lisinopril"},
				{"reference": "ExplanationOfBenefit/eob-pdex-pa-001"},
			},
			"recorded": "2026-02-15T12:00:00Z",
			"agent": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/provenance-participant-type", "code": "author"},
						},
					},
					"who":        map[string]any{"reference": "Practitioner/prac-cardiology-1"},
					"onBehalfOf": map[string]any{"reference": "Organization/org-flint-health"},
				},
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-provenance-participant-type", "code": "transmitter"},
						},
					},
					"who":        map[string]any{"reference": "Organization/org-flint-health"},
					"onBehalfOf": map[string]any{"reference": "Organization/org-flint-health"},
				},
			},
			"entity": []map[string]any{
				{
					"extension": []map[string]any{
						{
							"url": "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/ProvenanceSourceFrom",
							"valueCodeableConcept": map[string]any{
								"coding": []map[string]any{
									{"system": "http://hl7.org/fhir/us/davinci-pdex/CodeSystem/ProvenancePayerDataSource", "code": "hl7fhir", "display": "HL7 FHIR"},
								},
							},
						},
					},
					"role": "source",
					"what": map[string]any{"display": "Prior Payer HL7 FHIR R4 Exchange"},
				},
			},
		}),
		// Duplicate $member-match negative-test patients (not in clinical patient_ids)
		putEntry("Patient/pat-pdex-dup-1", map[string]any{
			"resourceType": "Patient",
			"id":           "pat-pdex-dup-1",
			"identifier":   []map[string]any{{"system": "http://flint.health/pdex/dup", "value": "DUP-MB-001"}},
			"name":         []map[string]any{{"family": "Dupont", "given": []string{"Jean"}}},
			"gender":       "male",
			"birthDate":    "1991-03-11",
		}),
		putEntry("Patient/pat-pdex-dup-2", map[string]any{
			"resourceType": "Patient",
			"id":           "pat-pdex-dup-2",
			"identifier":   []map[string]any{{"system": "http://flint.health/pdex/dup", "value": "DUP-MB-002"}},
			"name":         []map[string]any{{"family": "Dupont", "given": []string{"Jean"}}},
			"gender":       "male",
			"birthDate":    "1991-03-11",
		}),
		putEntry("Coverage/cov-pdex-dup-1", map[string]any{
			"resourceType": "Coverage",
			"id":           "cov-pdex-dup-1",
			"status":       "active",
			"subscriberId": "SUB-PDEX-DUP-999",
			"beneficiary":  map[string]any{"reference": "Patient/pat-pdex-dup-1"},
			"payor":        []map[string]any{{"reference": "Organization/org-flint-health"}},
		}),
		putEntry("Coverage/cov-pdex-dup-2", map[string]any{
			"resourceType": "Coverage",
			"id":           "cov-pdex-dup-2",
			"status":       "active",
			"subscriberId": "SUB-PDEX-DUP-999",
			"beneficiary":  map[string]any{"reference": "Patient/pat-pdex-dup-2"},
			"payor":        []map[string]any{{"reference": "Organization/org-flint-health"}},
		}),
	}

	combined := make([]any, 0, len(rawEntries)+len(extra))
	combined = append(combined, rawEntries...)
	for _, e := range extra {
		combined = append(combined, e)
	}

	return json.Marshal(map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry":        combined,
	})
}

// PASTransactionBundleJSON builds an idempotent FHIR R4 transaction Bundle
// seeding baseline HL7 Da Vinci Prior Authorization Support (PAS) v2.2.1 resources.
func PASTransactionBundleJSON() ([]byte, error) {
	entries := []map[string]any{
		putEntry("Organization/pas-org-insurer", map[string]any{
			"resourceType": "Organization",
			"id":           "pas-org-insurer",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-insurer"},
			},
			"identifier": []map[string]any{
				{"system": "http://hl7.org/fhir/sid/us-npi", "value": "8383066098"},
				{"system": "http://terminology.hl7.org/NamingSystem/USEIN", "value": "7239994834"},
			},
			"active": true,
			"type": []map[string]any{
				{"coding": []map[string]any{{"system": "https://codesystem.x12.org/005010/98", "code": "PR"}}},
			},
			"name":    "Flint PAS Payer Organization",
			"telecom": []map[string]any{{"system": "phone", "value": "800-555-0188"}},
			"address": []map[string]any{
				{"line": []string{"100 Payer Way"}, "city": "Boston", "state": "MA", "postalCode": "02110", "country": "US"},
			},
		}),
		putEntry("Organization/pas-org-requestor", map[string]any{
			"resourceType": "Organization",
			"id":           "pas-org-requestor",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-requestor"},
			},
			"identifier": []map[string]any{
				{"system": "http://hl7.org/fhir/sid/us-npi", "value": "8189991234"},
				{"system": "http://terminology.hl7.org/NamingSystem/USEIN", "value": "8189948234"},
			},
			"active": true,
			"type": []map[string]any{
				{"coding": []map[string]any{{"system": "https://codesystem.x12.org/005010/98", "code": "X3"}}},
			},
			"name":    "Flint PAS Requestor Medical Group",
			"telecom": []map[string]any{{"system": "phone", "value": "617-555-0177"}},
			"address": []map[string]any{
				{"line": []string{"200 Provider Ave"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"},
			},
		}),
		putEntry("Patient/pas-pat-subscriber", map[string]any{
			"resourceType": "Patient",
			"id":           "pas-pat-subscriber",
			"meta": map[string]any{
				"profile": []string{
					"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-beneficiary",
					"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-subscriber",
				},
			},
			"identifier": []map[string]any{
				{"system": "http://flint.health/pas/member", "value": "PAS-MEM-001"},
			},
			"name":      []map[string]any{{"family": "Smith", "given": []string{"Joe"}}},
			"gender":    "male",
			"birthDate": "1987-02-20",
		}),
		putEntry("Coverage/pas-cov-001", map[string]any{
			"resourceType": "Coverage",
			"id":           "pas-cov-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-coverage"},
			},
			"status":       "active",
			"subscriberId": "PAS-MEM-001",
			"beneficiary":  map[string]any{"reference": "Patient/pas-pat-subscriber"},
			"subscriber":   map[string]any{"reference": "Patient/pas-pat-subscriber"},
			"relationship": map[string]any{
				"coding": []map[string]any{
					{"system": "http://terminology.hl7.org/CodeSystem/subscriber-relationship", "code": "self"},
				},
			},
			"payor": []map[string]any{{"reference": "Organization/pas-org-insurer"}},
		}),
	}

	return json.Marshal(map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry":        entries,
	})
}
