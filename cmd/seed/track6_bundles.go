package main

import "encoding/json"

func plannetGeoExt(lat, lon float64) map[string]any {
	return map[string]any{
		"url": "http://hl7.org/fhir/StructureDefinition/geolocation",
		"extension": []map[string]any{
			{"url": "latitude", "valueDecimal": lat},
			{"url": "longitude", "valueDecimal": lon},
		},
	}
}

func plannetAvailableTimeExt() map[string]any {
	return map[string]any{
		"url": "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/contactpoint-availabletime",
		"extension": []map[string]any{
			{"url": "daysOfWeek", "valueCode": "mon"},
			{"url": "allDay", "valueBoolean": false},
			{"url": "availableStartTime", "valueTime": "08:00:00"},
			{"url": "availableEndTime", "valueTime": "17:00:00"},
		},
	}
}

func plannetViaIntermediaryExt(orgRef string) map[string]any {
	return map[string]any{
		"url":            "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/via-intermediary",
		"valueReference": map[string]any{"reference": orgRef},
	}
}

func plannetNewPatientsExt(networkRef string) map[string]any {
	return map[string]any{
		"url": "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/newpatients",
		"extension": []map[string]any{
			{
				"url": "acceptingPatients",
				"valueCodeableConcept": map[string]any{
					"coding": []map[string]any{
						{
							"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/AcceptingPatientsCS",
							"code":    "newpt",
							"display": "Accepting existing patients and new patients",
						},
					},
				},
			},
			{
				"url":            "fromNetwork",
				"valueReference": map[string]any{"reference": networkRef},
			},
		},
	}
}

func plannetQualificationExt(issuerRef string) map[string]any {
	return map[string]any{
		"url": "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/qualification",
		"extension": []map[string]any{
			{
				"url": "identifier",
				"valueIdentifier": map[string]any{
					"system": "http://acme-health.example.org/credentials",
					"value":  "CRED-MA-9001",
				},
			},
			{
				"url": "code",
				"valueCodeableConcept": map[string]any{
					"coding": []map[string]any{
						{
							"system":  "http://terminology.hl7.org/CodeSystem/v2-0360",
							"code":    "MD",
							"display": "Doctor of Medicine",
						},
					},
				},
			},
			{
				"url":            "issuer",
				"valueReference": map[string]any{"reference": issuerRef},
			},
			{
				"url":       "status",
				"valueCode": "active",
			},
			{
				"url": "period",
				"valuePeriod": map[string]any{
					"start": "2020-01-01",
					"end":   "2030-12-31",
				},
			},
			{
				"url": "whereValid",
				"valueCodeableConcept": map[string]any{
					"coding": []map[string]any{
						{
							"system":  "https://www.usps.com/",
							"code":    "MA",
							"display": "Massachusetts",
						},
					},
				},
			},
		},
	}
}

// PlanNetTransactionBundleJSON returns a deterministic FHIR R4 transaction Bundle
// covering all 9 HL7 Da Vinci PDex Plan-Net v1.1.0 profiles, Must-Support elements,
// extensions, forward chains, reverse chains (_has), _include, and _revinclude links.
func PlanNetTransactionBundleJSON() ([]byte, error) {
	entries := []map[string]any{
		putEntry("Endpoint/plannet-ep-001", map[string]any{
			"resourceType": "Endpoint",
			"id":           "plannet-ep-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Endpoint"},
			},
			"extension": []map[string]any{
				{
					"url": "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/endpoint-usecase",
					"extension": []map[string]any{
						{
							"url": "type",
							"valueCodeableConcept": map[string]any{
								"coding": []map[string]any{
									{
										"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/EndpointUsecaseCS",
										"code":    "treatment",
										"display": "Treatment",
									},
								},
							},
						},
						{
							"url":      "standard",
							"valueUri": "http://hl7.org/fhir/us/core",
						},
					},
				},
			},
			"status": "active",
			"connectionType": map[string]any{
				"system":  "http://terminology.hl7.org/CodeSystem/endpoint-connection-type",
				"code":    "hl7-fhir-rest",
				"display": "HL7 FHIR",
			},
			"name":                 "Acme FHIR R4 Provider Directory Endpoint",
			"managingOrganization": map[string]any{"reference": "Organization/plannet-org-clinic"},
			"contact": []map[string]any{
				{"system": "email", "value": "fhir-directory@acme-health.example.org"},
			},
			"payloadType": []map[string]any{
				{
					"coding": []map[string]any{
						{
							"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/EndpointPayloadTypeCS",
							"code":    "NA",
							"display": "Not Applicable",
						},
					},
				},
			},
			"payloadMimeType": []string{"application/fhir+json"},
			"address":         "https://fhir.acme-health.example.org/r4",
		}),

		putEntry("Organization/plannet-org-parent", map[string]any{
			"resourceType": "Organization",
			"id":           "plannet-org-parent",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Organization"},
			},
			"extension": []map[string]any{
				{
					"url":         "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/org-description",
					"valueString": "Acme Regional Health System and Integrated Payer",
				},
				plannetQualificationExt("Organization/plannet-org-parent"),
				{
					"url":            "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/location-reference",
					"valueReference": map[string]any{"reference": "Location/plannet-loc-parent"},
				},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "NPI", "display": "National provider identifier"},
						},
					},
					"system": "http://hl7.org/fhir/sid/us-npi",
					"value":  "1114999998",
				},
			},
			"active": true,
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{
							"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/OrgTypeCS",
							"code":    "payer",
							"display": "Payer",
						},
					},
				},
			},
			"name":  "Acme Regional Health System",
			"alias": []string{"Acme Health"},
			"telecom": []map[string]any{
				{
					"system": "phone",
					"value":  "800-555-1000",
					"rank":   1,
					"extension": []map[string]any{
						plannetAvailableTimeExt(),
						plannetViaIntermediaryExt("Organization/plannet-org-parent"),
					},
				},
			},
			"address": []map[string]any{
				{
					"type":       "both",
					"text":       "100 Main St, Boston, MA 02110, US",
					"line":       []string{"100 Main St"},
					"city":       "Boston",
					"state":      "MA",
					"postalCode": "02110",
					"country":    "US",
					"extension":  []map[string]any{plannetGeoExt(42.3601, -71.0589)},
				},
			},
			"contact": []map[string]any{
				{
					"name": map[string]any{"text": "Provider Relations"},
					"telecom": []map[string]any{
						{"system": "phone", "value": "800-555-1001", "use": "work"},
					},
				},
			},
			"endpoint": []map[string]any{
				{"reference": "Endpoint/plannet-ep-001"},
			},
		}),

		putEntry("Organization/plannet-org-clinic", map[string]any{
			"resourceType": "Organization",
			"id":           "plannet-org-clinic",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Organization"},
			},
			"extension": []map[string]any{
				{
					"url":         "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/org-description",
					"valueString": "Acme Outpatient Cardiology Specialty Clinic",
				},
				plannetQualificationExt("Organization/plannet-org-parent"),
				{
					"url":            "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/location-reference",
					"valueReference": map[string]any{"reference": "Location/plannet-loc-clinic"},
				},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "NPI", "display": "National provider identifier"},
						},
					},
					"system": "http://hl7.org/fhir/sid/us-npi",
					"value":  "1225888887",
				},
			},
			"active": true,
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{
							"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/OrgTypeCS",
							"code":    "fac",
							"display": "Facility",
						},
					},
				},
			},
			"name":  "Acme Cardiology Specialty Clinic",
			"alias": []string{"Acme Heart Clinic"},
			"telecom": []map[string]any{
				{
					"system": "phone",
					"value":  "617-555-2200",
					"rank":   1,
					"extension": []map[string]any{
						plannetAvailableTimeExt(),
						plannetViaIntermediaryExt("Organization/plannet-org-parent"),
					},
				},
			},
			"address": []map[string]any{
				{
					"type":       "both",
					"text":       "250 CambridgePark Dr, Cambridge, MA 02140, US",
					"line":       []string{"250 CambridgePark Dr"},
					"city":       "Cambridge",
					"state":      "MA",
					"postalCode": "02140",
					"country":    "US",
					"extension":  []map[string]any{plannetGeoExt(42.3956, -71.1426)},
				},
			},
			"partOf": map[string]any{"reference": "Organization/plannet-org-parent"},
			"contact": []map[string]any{
				{
					"name": map[string]any{"text": "Clinic Scheduling"},
					"telecom": []map[string]any{
						{"system": "phone", "value": "617-555-2201", "use": "work"},
					},
				},
			},
			"endpoint": []map[string]any{
				{"reference": "Endpoint/plannet-ep-001"},
			},
		}),

		putEntry("Organization/plannet-network-001", map[string]any{
			"resourceType": "Organization",
			"id":           "plannet-network-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Network"},
			},
			"extension": []map[string]any{
				{
					"url":            "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/location-reference",
					"valueReference": map[string]any{"reference": "Location/plannet-loc-parent"},
				},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "XX", "display": "Organization identifier"},
						},
					},
					"system": "http://acme-health.example.org/network-id",
					"value":  "NET-PPO-MA-001",
				},
			},
			"active": true,
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{
							"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/OrgTypeCS",
							"code":    "ntwk",
							"display": "Network",
						},
					},
				},
			},
			"name": "Acme Premier PPO Network",
			"telecom": []map[string]any{
				{"system": "phone", "value": "800-555-3100"},
			},
			"address": []map[string]any{
				{
					"line":       []string{"100 Main St"},
					"city":       "Boston",
					"state":      "MA",
					"postalCode": "02110",
					"country":    "US",
				},
			},
			"partOf": map[string]any{"reference": "Organization/plannet-org-parent"},
			"contact": []map[string]any{
				{
					"name": map[string]any{"text": "Network Helpdesk"},
					"telecom": []map[string]any{
						{"system": "phone", "value": "800-555-3101"},
					},
				},
			},
			"endpoint": []map[string]any{
				{"reference": "Endpoint/plannet-ep-001"},
			},
		}),

		putEntry("Location/plannet-loc-parent", map[string]any{
			"resourceType": "Location",
			"id":           "plannet-loc-parent",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Location"},
			},
			"extension": []map[string]any{
				plannetNewPatientsExt("Organization/plannet-network-001"),
				{
					"url": "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/accessibility",
					"valueCodeableConcept": map[string]any{
						"coding": []map[string]any{
							{
								"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/AccessibilityCS",
								"code":    "handiaccess",
								"display": "handicap accessible",
							},
						},
					},
				},
				{
					"url": "http://hl7.org/fhir/StructureDefinition/location-boundary-geojson",
					"valueAttachment": map[string]any{
						"contentType": "application/geo+json",
						"url":         "https://acme-health.example.org/geo/ma.geojson",
					},
				},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "FI", "display": "Facility ID"},
						},
					},
					"system": "http://acme-health.example.org/location-id",
					"value":  "LOC-BOSTON-CAMPUS",
				},
			},
			"status":      "active",
			"name":        "Acme Boston Medical Campus",
			"alias":       []string{"Boston Campus"},
			"description": "Main medical campus serving Greater Boston",
			"mode":        "instance",
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/v3-RoleCode", "code": "HOSP", "display": "Hospital"},
					},
				},
			},
			"telecom": []map[string]any{
				{
					"system": "phone",
					"value":  "617-555-4000",
					"extension": []map[string]any{
						plannetAvailableTimeExt(),
						plannetViaIntermediaryExt("Organization/plannet-org-parent"),
					},
				},
			},
			"address": map[string]any{
				"line":       []string{"100 Main St"},
				"city":       "Boston",
				"state":      "MA",
				"postalCode": "02110",
				"country":    "US",
			},
			"position": map[string]any{
				"longitude": -71.0589,
				"latitude":  42.3601,
			},
			"managingOrganization": map[string]any{"reference": "Organization/plannet-org-clinic"},
			"hoursOfOperation": []map[string]any{
				{
					"daysOfWeek":  []string{"mon", "tue", "wed", "thu", "fri"},
					"allDay":      false,
					"openingTime": "08:00:00",
					"closingTime": "18:00:00",
				},
			},
			"availabilityExceptions": "Closed on federal holidays",
			"endpoint": []map[string]any{
				{"reference": "Endpoint/plannet-ep-001"},
			},
		}),

		putEntry("Location/plannet-loc-clinic", map[string]any{
			"resourceType": "Location",
			"id":           "plannet-loc-clinic",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Location"},
			},
			"extension": []map[string]any{
				plannetNewPatientsExt("Organization/plannet-network-001"),
				{
					"url": "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/accessibility",
					"valueCodeableConcept": map[string]any{
						"coding": []map[string]any{
							{
								"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/AccessibilityCS",
								"code":    "pubtrans",
								"display": "public transit options",
							},
						},
					},
				},
				{
					"url": "http://hl7.org/fhir/StructureDefinition/location-boundary-geojson",
					"valueAttachment": map[string]any{
						"contentType": "application/geo+json",
						"url":         "https://acme-health.example.org/geo/cambridge.geojson",
					},
				},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "FI", "display": "Facility ID"},
						},
					},
					"system": "http://acme-health.example.org/location-id",
					"value":  "LOC-CAMBRIDGE-CARDIO",
				},
			},
			"status":      "active",
			"name":        "Acme Cambridge Cardiology Suite 300",
			"alias":       []string{"Cambridge Heart Suite"},
			"description": "Outpatient cardiology clinic suite",
			"mode":        "instance",
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://terminology.hl7.org/CodeSystem/v3-RoleCode", "code": "CARD", "display": "Ambulatory Health Care Facilities; Clinic/Center; Cardiology"},
					},
				},
			},
			"telecom": []map[string]any{
				{
					"system": "phone",
					"value":  "617-555-4100",
					"extension": []map[string]any{
						plannetAvailableTimeExt(),
						plannetViaIntermediaryExt("Organization/plannet-org-clinic"),
					},
				},
			},
			"address": map[string]any{
				"line":       []string{"250 CambridgePark Dr, Suite 300"},
				"city":       "Cambridge",
				"state":      "MA",
				"postalCode": "02140",
				"country":    "US",
			},
			"position": map[string]any{
				"longitude": -71.1426,
				"latitude":  42.3956,
			},
			"managingOrganization": map[string]any{"reference": "Organization/plannet-org-clinic"},
			"partOf":               map[string]any{"reference": "Location/plannet-loc-parent"},
			"hoursOfOperation": []map[string]any{
				{
					"daysOfWeek":  []string{"mon", "tue", "wed", "thu", "fri"},
					"allDay":      false,
					"openingTime": "08:30:00",
					"closingTime": "17:00:00",
				},
			},
			"availabilityExceptions": "Closed weekends and federal holidays",
			"endpoint": []map[string]any{
				{"reference": "Endpoint/plannet-ep-001"},
			},
		}),

		putEntry("HealthcareService/plannet-svc-001", map[string]any{
			"resourceType": "HealthcareService",
			"id":           "plannet-svc-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-HealthcareService"},
			},
			"extension": []map[string]any{
				plannetNewPatientsExt("Organization/plannet-network-001"),
				{
					"url": "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/delivery-method",
					"extension": []map[string]any{
						{
							"url": "type",
							"valueCodeableConcept": map[string]any{
								"coding": []map[string]any{
									{
										"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/DeliveryMethodCS",
										"code":    "physical",
										"display": "Physical",
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
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "PRN", "display": "Provider number"},
						},
					},
					"system": "http://acme-health.example.org/service-id",
					"value":  "SVC-CARDIO-001",
				},
			},
			"active":     true,
			"providedBy": map[string]any{"reference": "Organization/plannet-org-clinic"},
			"category": []map[string]any{
				{
					"coding": []map[string]any{
						{
							"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/HealthcareServiceCategoryCS",
							"code":    "outpat",
							"display": "Outpatient",
						},
					},
				},
			},
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "394579002", "display": "Cardiology"},
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
			"location": []map[string]any{
				{"reference": "Location/plannet-loc-clinic"},
			},
			"name":    "Outpatient Consultative Cardiology",
			"comment": "Comprehensive diagnostic and preventive cardiology care",
			"telecom": []map[string]any{
				{
					"system": "phone",
					"value":  "617-555-4200",
					"extension": []map[string]any{
						plannetAvailableTimeExt(),
						plannetViaIntermediaryExt("Organization/plannet-org-clinic"),
					},
				},
			},
			"coverageArea": []map[string]any{
				{"reference": "Location/plannet-loc-parent"},
			},
			"appointmentRequired": true,
			"availableTime": []map[string]any{
				{
					"daysOfWeek":         []string{"mon", "tue", "wed", "thu", "fri"},
					"allDay":             false,
					"availableStartTime": "08:30:00",
					"availableEndTime":   "17:00:00",
				},
			},
			"notAvailable": []map[string]any{
				{
					"description": "Closed Thanksgiving Day",
					"during": map[string]any{
						"start": "2026-11-26",
						"end":   "2026-11-27",
					},
				},
			},
			"availabilityExceptions": "Holiday hours may vary",
			"endpoint": []map[string]any{
				{"reference": "Endpoint/plannet-ep-001"},
			},
		}),

		putEntry("Practitioner/plannet-prac-001", map[string]any{
			"resourceType": "Practitioner",
			"id":           "plannet-prac-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-Practitioner"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "NPI", "display": "National provider identifier"},
						},
					},
					"system": "http://hl7.org/fhir/sid/us-npi",
					"value":  "1992999123",
				},
			},
			"active": true,
			"name": []map[string]any{
				{
					"text":   "Dr. Clara Chen, MD",
					"family": "Chen",
					"given":  []string{"Clara"},
					"prefix": []string{"Dr."},
					"suffix": []string{"MD"},
				},
			},
			"telecom": []map[string]any{
				{"system": "phone", "value": "617-555-4300", "use": "work"},
			},
			"address": []map[string]any{
				{
					"line":       []string{"250 CambridgePark Dr"},
					"city":       "Cambridge",
					"state":      "MA",
					"postalCode": "02140",
					"country":    "US",
					"extension":  []map[string]any{plannetGeoExt(42.3956, -71.1426)},
				},
			},
			"qualification": []map[string]any{
				{
					"extension": []map[string]any{
						{
							"url": "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/practitioner-qualification",
							"extension": []map[string]any{
								{"url": "status", "valueCode": "active"},
								{
									"url": "whereValid",
									"valueCodeableConcept": map[string]any{
										"coding": []map[string]any{
											{"system": "https://www.usps.com/", "code": "MA", "display": "Massachusetts"},
										},
									},
								},
							},
						},
					},
					"identifier": []map[string]any{
						{"system": "http://mass.gov/borim", "value": "MA-MD-778899"},
					},
					"code": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0360", "code": "MD", "display": "Doctor of Medicine"},
						},
					},
					"period": map[string]any{
						"start": "2018-06-01",
						"end":   "2028-06-01",
					},
					"issuer": map[string]any{"reference": "Organization/plannet-org-parent"},
				},
			},
			"communication": []map[string]any{
				{
					"extension": []map[string]any{
						{
							"url": "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/communication-proficiency",
							"valueCodeableConcept": map[string]any{
								"coding": []map[string]any{
									{
										"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/LanguageProficiencyCS",
										"code":    "50",
										"display": "Native or bilingual proficiency",
									},
								},
							},
						},
					},
					"coding": []map[string]any{
						{"system": "urn:ietf:bcp:47", "code": "en", "display": "English"},
					},
				},
			},
		}),

		putEntry("PractitionerRole/plannet-role-001", map[string]any{
			"resourceType": "PractitionerRole",
			"id":           "plannet-role-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-PractitionerRole"},
			},
			"extension": []map[string]any{
				plannetNewPatientsExt("Organization/plannet-network-001"),
				{
					"url":            "http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/network-reference",
					"valueReference": map[string]any{"reference": "Organization/plannet-network-001"},
				},
				plannetQualificationExt("Organization/plannet-org-parent"),
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "PRN", "display": "Provider number"},
						},
					},
					"system": "http://acme-health.example.org/role-id",
					"value":  "ROLE-CHEN-001",
				},
			},
			"active": true,
			"period": map[string]any{
				"start": "2022-01-01",
				"end":   "2028-12-31",
			},
			"practitioner": map[string]any{"reference": "Practitioner/plannet-prac-001"},
			"organization": map[string]any{"reference": "Organization/plannet-org-clinic"},
			"code": []map[string]any{
				{
					"coding": []map[string]any{
						{
							"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/ProviderRoleCS",
							"code":    "ph",
							"display": "Physician",
						},
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
			"location": []map[string]any{
				{"reference": "Location/plannet-loc-clinic"},
			},
			"healthcareService": []map[string]any{
				{"reference": "HealthcareService/plannet-svc-001"},
			},
			"telecom": []map[string]any{
				{
					"system": "phone",
					"value":  "617-555-4301",
					"rank":   1,
					"extension": []map[string]any{
						plannetAvailableTimeExt(),
						plannetViaIntermediaryExt("Organization/plannet-org-clinic"),
					},
				},
			},
			"availableTime": []map[string]any{
				{
					"daysOfWeek":         []string{"mon", "tue", "wed", "thu", "fri"},
					"allDay":             false,
					"availableStartTime": "09:00:00",
					"availableEndTime":   "16:30:00",
				},
			},
			"notAvailable": []map[string]any{
				{
					"description": "Annual cardiology conference",
					"during": map[string]any{
						"start": "2026-11-10",
						"end":   "2026-11-12",
					},
				},
			},
			"endpoint": []map[string]any{
				{"reference": "Endpoint/plannet-ep-001"},
			},
		}),

		putEntry("OrganizationAffiliation/plannet-orgaffil-001", map[string]any{
			"resourceType": "OrganizationAffiliation",
			"id":           "plannet-orgaffil-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-OrganizationAffiliation"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "PRN", "display": "Provider number"},
						},
					},
					"system": "http://acme-health.example.org/orgaffil-id",
					"value":  "AFFIL-CARDIO-001",
				},
			},
			"active": true,
			"period": map[string]any{
				"start": "2022-01-01",
				"end":   "2028-12-31",
			},
			"organization":              map[string]any{"reference": "Organization/plannet-org-clinic"},
			"participatingOrganization": map[string]any{"reference": "Organization/plannet-org-clinic"},
			"network": []map[string]any{
				{"reference": "Organization/plannet-network-001"},
			},
			"code": []map[string]any{
				{
					"coding": []map[string]any{
						{
							"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/OrganizationAffiliationRoleCS",
							"code":    "provider",
							"display": "Provider",
						},
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
			"location": []map[string]any{
				{"reference": "Location/plannet-loc-clinic"},
			},
			"healthcareService": []map[string]any{
				{"reference": "HealthcareService/plannet-svc-001"},
			},
			"telecom": []map[string]any{
				{"system": "phone", "value": "617-555-4400", "rank": 1},
			},
			"endpoint": []map[string]any{
				{"reference": "Endpoint/plannet-ep-001"},
			},
		}),

		putEntry("InsurancePlan/plannet-insplan-001", map[string]any{
			"resourceType": "InsurancePlan",
			"id":           "plannet-insplan-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-pdex-plan-net/StructureDefinition/plannet-InsurancePlan"},
			},
			"identifier": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "NIIP", "display": "National Insurance Payor Identifier (Payor)"},
						},
					},
					"system":   "http://acme-health.example.org/plan-id",
					"value":    "PLAN-PPO-GOLD-2026",
					"assigner": map[string]any{"reference": "Organization/plannet-org-clinic"},
				},
			},
			"status": "active",
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{
							"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/InsuranceProductTypeCS",
							"code":    "commppo",
							"display": "Commercial PPO",
						},
					},
				},
			},
			"name":  "Acme Premier Gold PPO",
			"alias": []string{"Gold PPO 2026"},
			"period": map[string]any{
				"start": "2026-01-01",
				"end":   "2026-12-31",
			},
			"ownedBy":        map[string]any{"reference": "Organization/plannet-org-clinic"},
			"administeredBy": map[string]any{"reference": "Organization/plannet-org-clinic"},
			"coverageArea": []map[string]any{
				{"reference": "Location/plannet-loc-clinic"},
			},
			"contact": []map[string]any{
				{
					"purpose": map[string]any{
						"coding": []map[string]any{
							{"system": "http://terminology.hl7.org/CodeSystem/contactentity-type", "code": "PATINF", "display": "Patient"},
						},
					},
					"name": map[string]any{"text": "Member Plan Services"},
					"telecom": []map[string]any{
						{"system": "phone", "value": "800-555-5000"},
					},
				},
			},
			"endpoint": []map[string]any{
				{"reference": "Endpoint/plannet-ep-001"},
			},
			"network": []map[string]any{
				{"reference": "Organization/plannet-network-001"},
			},
			"plan": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{
								"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/InsurancePlanTypeCS",
								"code":    "gold",
								"display": "Gold",
							},
						},
					},
					"coverageArea": []map[string]any{
						{"reference": "Location/plannet-loc-clinic"},
					},
					"network": []map[string]any{
						{"reference": "Organization/plannet-network-001"},
					},
				},
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

// DrugFormularyTransactionBundleJSON returns a deterministic FHIR R4 transaction Bundle
// covering all 5 HL7 Da Vinci PDex US Drug Formulary v2.0.1 profiles, Must-Support
// slices, extensions, and _include references.
func DrugFormularyTransactionBundleJSON() ([]byte, error) {
	entries := []map[string]any{
		putEntry("Location/loc-usdf-001", map[string]any{
			"resourceType": "Location",
			"id":           "loc-usdf-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-InsurancePlanLocation"},
			},
			"extension": []map[string]any{
				{
					"url": "http://hl7.org/fhir/StructureDefinition/location-boundary-geojson",
					"valueAttachment": map[string]any{
						"contentType": "application/geo+json",
						"url":         "https://acme-health.example.org/geo/ma-service-area.geojson",
					},
				},
			},
			"status": "active",
			"name":   "Massachusetts Formulary Service Area",
			"address": map[string]any{
				"line":       []string{"100 State St"},
				"city":       "Boston",
				"state":      "MA",
				"postalCode": "02109",
				"country":    "US",
			},
		}),

		putEntry("InsurancePlan/insplan-usdf-formulary-001", map[string]any{
			"resourceType": "InsurancePlan",
			"id":           "insplan-usdf-formulary-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-Formulary"},
			},
			"identifier": []map[string]any{
				{
					"system": "http://acme-health.example.org/formulary-id",
					"value":  "FORM-2026-001",
				},
			},
			"status": "active",
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{
							"system":  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
							"code":    "DRUGPOL",
							"display": "drug policy",
						},
					},
				},
			},
			"name": "Acme 2026 Commercial Drug Formulary",
			"period": map[string]any{
				"start": "2026-01-01",
				"end":   "2026-12-31",
			},
			"coverageArea": []map[string]any{
				{"reference": "Location/loc-usdf-001"},
			},
		}),

		putEntry("InsurancePlan/insplan-usdf-payer-001", map[string]any{
			"resourceType": "InsurancePlan",
			"id":           "insplan-usdf-payer-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-PayerInsurancePlan"},
			},
			"identifier": []map[string]any{
				{
					"system": "http://acme-health.example.org/payer-plan-id",
					"value":  "PAYER-PLAN-2026-001",
				},
			},
			"status": "active",
			"type": []map[string]any{
				{
					"coding": []map[string]any{
						{
							"system":  "http://hl7.org/fhir/us/davinci-pdex-plan-net/CodeSystem/InsuranceProductTypeCS",
							"code":    "commppo",
							"display": "Commercial PPO",
						},
					},
				},
			},
			"name": "Acme Commercial PPO Health Plan",
			"period": map[string]any{
				"start": "2026-01-01",
				"end":   "2026-12-31",
			},
			"coverageArea": []map[string]any{
				{"reference": "Location/loc-usdf-001"},
			},
			"contact": []map[string]any{
				{
					"name": map[string]any{"text": "Pharmacy Member Services"},
					"telecom": []map[string]any{
						{"system": "phone", "value": "800-555-6000"},
						{"system": "url", "value": "https://acme-health.example.org/pharmacy"},
					},
				},
			},
			"coverage": []map[string]any{
				{
					"extension": []map[string]any{
						{
							"url":            "http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-FormularyReference-extension",
							"valueReference": map[string]any{"reference": "InsurancePlan/insplan-usdf-formulary-001"},
						},
					},
					"type": map[string]any{
						"coding": []map[string]any{
							{
								"system":  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
								"code":    "DRUGPOL",
								"display": "drug policy",
							},
						},
					},
					"benefit": []map[string]any{
						{
							"type": map[string]any{
								"coding": []map[string]any{
									{
										"system":  "http://terminology.hl7.org/CodeSystem/insurance-plan-type",
										"code":    "drug",
										"display": "Drug",
									},
								},
							},
						},
					},
				},
			},
			"plan": []map[string]any{
				{
					"type": map[string]any{
						"coding": []map[string]any{
							{
								"system":  "http://terminology.hl7.org/CodeSystem/insurance-plan-type",
								"code":    "drug",
								"display": "Drug",
							},
						},
					},
					"coverageArea": []map[string]any{
						{"reference": "Location/loc-usdf-001"},
					},
					"specificCost": []map[string]any{
						{
							"category": map[string]any{
								"coding": []map[string]any{
									{
										"system":  "http://hl7.org/fhir/us/davinci-drug-formulary/CodeSystem/usdf-PharmacyBenefitTypeCS-TEMPORARY-TRIAL-USE",
										"code":    "1-month-in-retail",
										"display": "1 month in network retail",
									},
								},
							},
							"benefit": []map[string]any{
								{
									"type": map[string]any{
										"coding": []map[string]any{
											{
												"system":  "http://hl7.org/fhir/us/davinci-drug-formulary/CodeSystem/usdf-DrugTierCS-TEMPORARY-TRIAL-USE",
												"code":    "generic",
												"display": "Generic",
											},
										},
									},
									"cost": []map[string]any{
										{
											"type": map[string]any{
												"coding": []map[string]any{
													{
														"system":  "http://hl7.org/fhir/us/davinci-drug-formulary/CodeSystem/usdf-BenefitCostTypeCS-TEMPORARY-TRIAL-USE",
														"code":    "copay",
														"display": "Copay",
													},
												},
											},
											"qualifiers": []map[string]any{
												{
													"coding": []map[string]any{
														{
															"system":  "http://hl7.org/fhir/us/davinci-drug-formulary/CodeSystem/usdf-CostShareOptionCS-TEMPORARY-TRIAL-USE",
															"code":    "after-deductible",
															"display": "After Deductible",
														},
													},
												},
											},
											"value": map[string]any{
												"value":  10.0,
												"unit":   "USD",
												"system": "urn:iso:std:iso:4217",
												"code":   "USD",
											},
										},
										{
											"type": map[string]any{
												"coding": []map[string]any{
													{
														"system":  "http://hl7.org/fhir/us/davinci-drug-formulary/CodeSystem/usdf-BenefitCostTypeCS-TEMPORARY-TRIAL-USE",
														"code":    "coinsurance",
														"display": "Coinsurance",
													},
												},
											},
											"qualifiers": []map[string]any{
												{
													"coding": []map[string]any{
														{
															"system":  "http://hl7.org/fhir/us/davinci-drug-formulary/CodeSystem/usdf-CostShareOptionCS-TEMPORARY-TRIAL-USE",
															"code":    "coinsurance-not-applicable",
															"display": "No Coinsurance",
														},
													},
												},
											},
											"value": map[string]any{
												"value":  0.0,
												"unit":   "%",
												"system": "http://unitsofmeasure.org",
												"code":   "%",
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}),

		putEntry("MedicationKnowledge/medknow-usdf-001", map[string]any{
			"resourceType": "MedicationKnowledge",
			"id":           "medknow-usdf-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-FormularyDrug"},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{
						"system":  "http://www.nlm.nih.gov/research/umls/rxnorm",
						"code":    "314076",
						"display": "lisinopril 10 MG Oral Tablet",
					},
					{
						"system":  "http://www.nlm.nih.gov/research/umls/rxnorm",
						"code":    "1156189",
						"display": "lisinopril Oral Product",
					},
				},
			},
			"status": "active",
			"doseForm": map[string]any{
				"coding": []map[string]any{
					{
						"system":  "http://www.nlm.nih.gov/research/umls/rxnorm",
						"code":    "317541",
						"display": "Oral Tablet",
					},
				},
			},
			"synonym": []string{"Lisinopril 10mg Tablet"},
		}),

		putEntry("Basic/basic-usdf-item-001", map[string]any{
			"resourceType": "Basic",
			"id":           "basic-usdf-item-001",
			"meta": map[string]any{
				"profile": []string{"http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-FormularyItem"},
			},
			"extension": []map[string]any{
				{
					"url":            "http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-FormularyReference-extension",
					"valueReference": map[string]any{"reference": "InsurancePlan/insplan-usdf-formulary-001"},
				},
				{
					"url":       "http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-AvailabilityStatus-extension",
					"valueCode": "active",
				},
				{
					"url": "http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-AvailabilityPeriod-extension",
					"valuePeriod": map[string]any{
						"start": "2026-01-01",
						"end":   "2026-12-31",
					},
				},
				{
					"url": "http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-PharmacyBenefitType-extension",
					"valueCodeableConcept": map[string]any{
						"coding": []map[string]any{
							{
								"system":  "http://hl7.org/fhir/us/davinci-drug-formulary/CodeSystem/usdf-PharmacyBenefitTypeCS-TEMPORARY-TRIAL-USE",
								"code":    "1-month-in-retail",
								"display": "1 month in network retail",
							},
						},
					},
				},
				{
					"url": "http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-DrugTierID-extension",
					"valueCodeableConcept": map[string]any{
						"coding": []map[string]any{
							{
								"system":  "http://hl7.org/fhir/us/davinci-drug-formulary/CodeSystem/usdf-DrugTierCS-TEMPORARY-TRIAL-USE",
								"code":    "generic",
								"display": "Generic",
							},
						},
					},
				},
				{
					"url":          "http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-PriorAuthorization-extension",
					"valueBoolean": false,
				},
				{
					"url":          "http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-StepTherapyLimit-extension",
					"valueBoolean": false,
				},
				{
					"url":          "http://hl7.org/fhir/us/davinci-drug-formulary/StructureDefinition/usdf-QuantityLimit-extension",
					"valueBoolean": true,
				},
			},
			"code": map[string]any{
				"coding": []map[string]any{
					{
						"system":  "http://hl7.org/fhir/us/davinci-drug-formulary/CodeSystem/usdf-InsuranceItemTypeCS-TEMPORARY-TRIAL-USE",
						"code":    "formulary-item",
						"display": "Formulary Item",
					},
				},
			},
			"subject": map[string]any{"reference": "MedicationKnowledge/medknow-usdf-001"},
		}),
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry":        entries,
	}
	return json.MarshalIndent(bundle, "", "  ")
}
