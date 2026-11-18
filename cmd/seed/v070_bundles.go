package main

import (
	"encoding/base64"
	"encoding/json"
)

// CPDSTransactionBundleJSON builds the Cancer Pathology Data Sharing (CPDS v1.0.1) & US Core v5.0.1
// longitudinal cohort in an isolated tenant without mutating the baseline demo cohort.
func CPDSTransactionBundleJSON() ([]byte, error) {
	baseBytes, err := CohortTransactionBundleJSON()
	if err != nil {
		return nil, err
	}
	var bundle map[string]any
	if err := json.Unmarshal(baseBytes, &bundle); err != nil {
		return nil, err
	}
	rawEntries, _ := bundle["entry"].([]any)

	for _, raw := range rawEntries {
		em, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		res, ok := em["resource"].(map[string]any)
		if !ok {
			continue
		}
		switch em["fullUrl"] {
		case "Patient/pat-alice-001":
			res["name"] = []map[string]any{
				{"use": "official", "family": "Wonderland", "given": []string{"Alice", "Marie"}, "suffix": []string{"Jr."}, "period": map[string]any{"start": "2010-01-01"}},
				{"use": "old", "family": "Liddell", "given": []string{"Alice"}, "period": map[string]any{"start": "1985-04-12", "end": "2009-12-31"}},
			}
			res["address"] = []map[string]any{
				{"use": "home", "line": []string{"742 Evergreen Terrace"}, "city": "San Francisco", "state": "CA", "postalCode": "94105", "country": "US", "period": map[string]any{"start": "2015-01-01"}},
				{"use": "old", "line": []string{"10 Downing Way"}, "city": "Oakland", "state": "CA", "postalCode": "94607", "country": "US", "period": map[string]any{"start": "2005-01-01", "end": "2014-12-31"}},
			}
			if exts, ok := res["extension"].([]any); ok {
				exts = append(exts, map[string]any{
					"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-genderIdentity",
					"valueCodeableConcept": map[string]any{
						"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "446141000124107", "display": "Identifies as female gender"}},
					},
				})
				res["extension"] = exts
			}
		case "CarePlan/careplan-alice-htn":
			res["text"] = map[string]any{
				"status": "generated",
				"div":    `<div xmlns="http://www.w3.org/1999/xhtml"><p>Hypertension Longitudinal Management Plan</p></div>`,
			}
		}
	}

	noteData := base64.StdEncoding.EncodeToString([]byte("Clinical pathology & oncology note summary."))
	mkDocRef := func(id, loincCode, loincDisplay string) map[string]any {
		return putEntry("DocumentReference/"+id, map[string]any{
			"resourceType": "DocumentReference",
			"id":           id,
			"identifier":   []map[string]any{{"system": "http://hospital.example.org/documents", "value": "DOC-" + id}},
			"status":       "current",
			"type":         map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": loincCode, "display": loincDisplay}}},
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-documentreference-category", "code": "clinical-note", "display": "Clinical Note"}}},
			},
			"subject":   map[string]any{"reference": "Patient/pat-alice-001"},
			"date":      "2026-01-15T10:15:00Z",
			"author":    []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"custodian": map[string]any{"reference": "Organization/org-flint-health"},
			"content": []map[string]any{
				{
					"attachment": map[string]any{"contentType": "text/plain", "data": noteData},
					"format":     map[string]any{"system": "http://ihe.net/fhir/ihe.formatcode.fhir/CodeSystem/formatcode", "code": "urn:ihe:iti:xds:2017:mimeTypeSufficient"},
				},
			},
			"context": map[string]any{
				"encounter": []map[string]any{{"reference": "Encounter/enc-alice-001"}},
				"period":    map[string]any{"start": "2026-01-15T09:00:00Z", "end": "2026-01-15T10:00:00Z"},
			},
		})
	}

	extra := []any{
		mkDocRef("docref-cpds-consult", "11488-4", "Consult note"),
		mkDocRef("docref-cpds-discharge", "18842-5", "Discharge summary"),
		mkDocRef("docref-cpds-proc", "28570-0", "Procedure note"),
		mkDocRef("docref-cpds-progress", "11506-3", "Progress note"),
		putEntry("DiagnosticReport/dr-cpds-cardiology-note", map[string]any{
			"resourceType":       "DiagnosticReport",
			"id":                 "dr-cpds-cardiology-note",
			"status":             "final",
			"category":           []map[string]any{{"coding": []map[string]any{{"system": "http://loinc.org", "code": "LP29708-2", "display": "Cardiology"}}}},
			"code":               map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "18745-0", "display": "Cardiac catheterization study"}}},
			"subject":            map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":          map[string]any{"reference": "Encounter/enc-alice-001"},
			"effectiveDateTime":  "2026-01-15T09:42:00Z",
			"issued":             "2026-01-15T09:56:00Z",
			"performer":          []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"resultsInterpreter": []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"presentedForm":      []map[string]any{{"contentType": "text/plain", "data": noteData}},
		}),
		putEntry("DiagnosticReport/dr-cpds-pathology-note", map[string]any{
			"resourceType":       "DiagnosticReport",
			"id":                 "dr-cpds-pathology-note",
			"status":             "final",
			"category":           []map[string]any{{"coding": []map[string]any{{"system": "http://loinc.org", "code": "LP7839-6", "display": "Pathology"}}}},
			"code":               map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "60568-3", "display": "Pathology Synoptic report"}}},
			"subject":            map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":          map[string]any{"reference": "Encounter/enc-alice-001"},
			"effectiveDateTime":  "2026-01-15T09:44:00Z",
			"issued":             "2026-01-15T09:58:00Z",
			"performer":          []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"resultsInterpreter": []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"presentedForm":      []map[string]any{{"contentType": "text/plain", "data": noteData}},
		}),
		putEntry("Observation/obs-cpds-sex-orient", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-cpds-sex-orient",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history", "display": "Social History"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "76690-7", "display": "Sexual orientation"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:18:00Z",
			"valueCodeableConcept": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "38628009", "display": "Lesbian, gay, or homosexual"}},
			},
		}),
		putEntry("Observation/obs-cpds-clinical-test", map[string]any{
			"resourceType": "Observation",
			"id":           "obs-cpds-clinical-test",
			"status":       "final",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-observation-category", "code": "clinical-test", "display": "Clinical Test"}}},
				{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-category", "code": "clinical-test", "display": "Clinical Test"}}},
			},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8601-7", "display": "EKG impression"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:21:00Z",
			"valueString":       "Normal sinus rhythm",
		}),
	}

	rawEntries = append(rawEntries, extra...)
	bundle["entry"] = rawEntries
	return json.MarshalIndent(bundle, "", "  ")
}

// USQCTransactionBundleJSON builds the US Quality Core (v0.5.0 & v1.0.0-ballot) server cohort
// including positive clinical profiles and all QICore/USQC negation profiles.
func USQCTransactionBundleJSON() ([]byte, error) {
	baseBytes, err := CPDSTransactionBundleJSON()
	if err != nil {
		return nil, err
	}
	var bundle map[string]any
	if err := json.Unmarshal(baseBytes, &bundle); err != nil {
		return nil, err
	}
	rawEntries, _ := bundle["entry"].([]any)

	for _, raw := range rawEntries {
		em, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		res, ok := em["resource"].(map[string]any)
		if !ok {
			continue
		}
		switch em["fullUrl"] {
		case "MedicationRequest/medreq-alice-lisinopril", "MedicationRequest/medreq-alice-atorvastatin-ref":
			res["doNotPerform"] = false
			res["reasonCode"] = []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "38341003", "display": "Essential hypertension"}}}}
			res["reasonReference"] = []map[string]any{{"reference": "Condition/cond-alice-htn"}}
		case "ServiceRequest/sr-alice-lipid":
			res["doNotPerform"] = false
			res["reasonCode"] = []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "38341003", "display": "Essential hypertension"}}}}
			res["reasonReference"] = []map[string]any{{"reference": "Condition/cond-alice-htn"}}
			res["occurrencePeriod"] = map[string]any{"start": "2026-01-15T09:35:00Z", "end": "2026-01-15T10:00:00Z"}
		}
	}

	extra := []map[string]any{
		putEntry("AdverseEvent/ae-alice-001", map[string]any{
			"resourceType": "AdverseEvent",
			"id":           "ae-alice-001",
			"actuality":    "actual",
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/adverse-event-category", "code": "medication-mishap", "display": "Medication Mishap"}}},
			},
			"event":        map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "402387002", "display": "Allergic angioedema"}}},
			"subject":      map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":    map[string]any{"reference": "Encounter/enc-alice-001"},
			"date":         "2026-01-15T09:35:00Z",
			"recordedDate": "2026-01-15T09:40:00Z",
			"seriousness":  map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/adverse-event-seriousness", "code": "Non-serious"}}},
			"recorder":     map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"suspectEntity": []map[string]any{
				{
					"instance": map[string]any{"reference": "Medication/med-atorvastatin-001"},
					"causality": []map[string]any{
						{"assessment": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/adverse-event-causality-assess", "code": "Possible"}}}},
					},
				},
			},
		}),
		putEntry("FamilyMemberHistory/fmh-alice-001", map[string]any{
			"resourceType":    "FamilyMemberHistory",
			"id":              "fmh-alice-001",
			"status":          "completed",
			"patient":         map[string]any{"reference": "Patient/pat-alice-001"},
			"date":            "2026-01-15T09:10:00Z",
			"relationship":    map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-RoleCode", "code": "FTH", "display": "father"}}},
			"sex":             map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/administrative-gender", "code": "male", "display": "Male"}}},
			"deceasedBoolean": false,
			"condition": []map[string]any{
				{
					"code":     map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "56265001", "display": "Heart disease"}}},
					"onsetAge": map[string]any{"value": 58.0, "unit": "yr", "system": "http://unitsofmeasure.org", "code": "a"},
				},
			},
		}),
		putEntry("ImagingStudy/img-alice-001", map[string]any{
			"resourceType":      "ImagingStudy",
			"id":                "img-alice-001",
			"identifier":        []map[string]any{{"system": "urn:dicom:uid", "value": "urn:oid:2.16.840.1.113883.3.1985.1"}},
			"status":            "available",
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-alice-001"},
			"started":           "2026-01-15T09:38:00Z",
			"numberOfSeries":    1,
			"numberOfInstances": 1,
			"procedureCode":     []map[string]any{{"coding": []map[string]any{{"system": "http://loinc.org", "code": "30746-2", "display": "XR Chest 2 Views"}}}},
			"reasonCode":        []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "38341003", "display": "Essential hypertension"}}}},
			"series": []map[string]any{
				{
					"uid":      "2.16.840.1.113883.3.1985.1.1",
					"modality": map[string]any{"system": "http://dicom.nema.org/resources/ontology/DCM", "code": "DX", "display": "Digital Radiography"},
					"bodySite": map[string]any{"system": "http://snomed.info/sct", "code": "51185008", "display": "Thoracic structure"},
					"instance": []map[string]any{
						{
							"uid":      "2.16.840.1.113883.3.1985.1.1.1",
							"sopClass": map[string]any{"system": "urn:ietf:rfc:3986", "code": "urn:oid:1.2.840.10008.5.1.4.1.1.1.1"},
						},
					},
				},
			},
		}),
		putEntry("NutritionOrder/nutr-alice-001", map[string]any{
			"resourceType": "NutritionOrder",
			"id":           "nutr-alice-001",
			"status":       "active",
			"intent":       "order",
			"patient":      map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":    map[string]any{"reference": "Encounter/enc-alice-001"},
			"dateTime":     "2026-01-15T09:36:00Z",
			"orderer":      map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"oralDiet": map[string]any{
				"type":     []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "386619000", "display": "Low sodium diet"}}}},
				"schedule": []map[string]any{{"repeat": map[string]any{"frequency": 3, "period": 1.0, "periodUnit": "d"}}},
			},
		}),
		putEntry("Communication/comm-alice-done", map[string]any{
			"resourceType": "Communication",
			"id":           "comm-alice-done",
			"status":       "completed",
			"category":     []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/communication-category", "code": "instruction", "display": "Instruction"}}}},
			"medium":       []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-ParticipationMode", "code": "WRITTEN", "display": "written"}}}},
			"subject":      map[string]any{"reference": "Patient/pat-alice-001"},
			"topic":        map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "409073007", "display": "Education"}}},
			"encounter":    map[string]any{"reference": "Encounter/enc-alice-001"},
			"sent":         "2026-01-15T09:50:00Z",
			"received":     "2026-01-15T09:52:00Z",
			"recipient":    []map[string]any{{"reference": "Patient/pat-alice-001"}},
			"sender":       map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"reasonCode":   []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "38341003", "display": "Essential hypertension"}}}},
			"payload":      []map[string]any{{"contentString": "Home blood pressure monitoring education provided."}},
		}),
		putEntry("Communication/comm-alice-notdone", map[string]any{
			"resourceType": "Communication",
			"id":           "comm-alice-notdone",
			"status":       "not-done",
			"statusReason": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "183944003", "display": "Procedure refused"}}},
			"category":     []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/communication-category", "code": "instruction", "display": "Instruction"}}}},
			"subject":      map[string]any{"reference": "Patient/pat-alice-001"},
			"topic":        map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "409073007", "display": "Education"}}},
			"encounter":    map[string]any{"reference": "Encounter/enc-alice-001"},
			"sent":         "2026-01-15T09:53:00Z",
			"recipient":    []map[string]any{{"reference": "Patient/pat-alice-001"}},
			"sender":       map[string]any{"reference": "Practitioner/prac-cardiology-1"},
		}),
		putEntry("DeviceRequest/devreq-alice-done", map[string]any{
			"resourceType": "DeviceRequest",
			"id":           "devreq-alice-done",
			"modifierExtension": []map[string]any{
				{"url": "http://hl7.org/fhir/5.0/StructureDefinition/extension-DeviceRequest.doNotPerform", "valueBoolean": false},
			},
			"status":              "active",
			"intent":              "order",
			"codeCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "43770009", "display": "Doppler blood pressure monitor"}}},
			"subject":             map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":           map[string]any{"reference": "Encounter/enc-alice-001"},
			"occurrenceDateTime":  "2026-01-15T09:45:00Z",
			"authoredOn":          "2026-01-15T09:40:00Z",
			"requester":           map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"reasonCode":          []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "38341003", "display": "Essential hypertension"}}}},
		}),
		putEntry("DeviceRequest/devreq-alice-prohibited", map[string]any{
			"resourceType": "DeviceRequest",
			"id":           "devreq-alice-prohibited",
			"modifierExtension": []map[string]any{
				{"url": "http://hl7.org/fhir/5.0/StructureDefinition/extension-DeviceRequest.doNotPerform", "valueBoolean": true},
			},
			"status":              "completed",
			"intent":              "order",
			"codeCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "20568009", "display": "Urinary catheter"}}},
			"subject":             map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":           map[string]any{"reference": "Encounter/enc-alice-001"},
			"authoredOn":          "2026-01-15T09:41:00Z",
			"requester":           map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"reasonCode":          []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "183945002", "display": "Procedure contraindicated"}}}},
		}),
		putEntry("Immunization/imm-alice-notdone", map[string]any{
			"resourceType":       "Immunization",
			"id":                 "imm-alice-notdone",
			"status":             "not-done",
			"statusReason":       map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "183944003", "display": "Procedure refused"}}},
			"vaccineCode":        map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/cvx", "code": "208", "display": "COVID-19, mRNA, LNP-S, PF, 30 mcg/0.3 mL dose"}}},
			"patient":            map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":          map[string]any{"reference": "Encounter/enc-alice-001"},
			"occurrenceDateTime": "2026-01-15T09:51:00Z",
			"recorded":           "2026-01-15T09:51:00Z",
			"primarySource":      false,
		}),
		putEntry("MedicationAdministration/medadmin-alice-done", map[string]any{
			"resourceType": "MedicationAdministration",
			"id":           "medadmin-alice-done",
			"status":       "completed",
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "314076", "display": "Lisinopril 10 MG Oral Tablet"}},
			},
			"subject":         map[string]any{"reference": "Patient/pat-alice-001"},
			"context":         map[string]any{"reference": "Encounter/enc-alice-001"},
			"effectivePeriod": map[string]any{"start": "2026-01-15T09:30:00Z", "end": "2026-01-15T09:31:00Z"},
			"performer":       []map[string]any{{"actor": map[string]any{"reference": "Practitioner/prac-cardiology-1"}}},
			"reasonCode":      []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "38341003", "display": "Essential hypertension"}}}},
			"request":         map[string]any{"reference": "MedicationRequest/medreq-alice-lisinopril"},
			"dosage": map[string]any{
				"text":  "10 mg orally",
				"route": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "26643006", "display": "Oral route"}}},
				"dose":  map[string]any{"value": 10.0, "unit": "mg", "system": "http://unitsofmeasure.org", "code": "mg"},
			},
		}),
		putEntry("MedicationAdministration/medadmin-alice-notdone", map[string]any{
			"resourceType": "MedicationAdministration",
			"id":           "medadmin-alice-notdone",
			"status":       "not-done",
			"statusReason": []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "183944003", "display": "Procedure refused"}}}},
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "1191", "display": "Aspirin"}},
			},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"context":           map[string]any{"reference": "Encounter/enc-alice-001"},
			"effectiveDateTime": "2026-01-15T09:32:00Z",
		}),
		putEntry("MedicationDispense/meddisp-alice-declined", map[string]any{
			"resourceType": "MedicationDispense",
			"id":           "meddisp-alice-declined",
			"status":       "declined",
			"statusReasonCodeableConcept": map[string]any{
				"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "183944003", "display": "Procedure refused"}},
			},
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "314076", "display": "Lisinopril 10 MG Oral Tablet"}},
			},
			"subject":                 map[string]any{"reference": "Patient/pat-alice-001"},
			"context":                 map[string]any{"reference": "Encounter/enc-alice-001"},
			"authorizingPrescription": []map[string]any{{"reference": "MedicationRequest/medreq-alice-lisinopril"}},
			"whenPrepared":            "2026-01-15T10:15:00Z",
		}),
		putEntry("MedicationRequest/medreq-alice-prohibited", map[string]any{
			"resourceType":    "MedicationRequest",
			"id":              "medreq-alice-prohibited",
			"status":          "completed",
			"intent":          "order",
			"doNotPerform":    true,
			"reportedBoolean": false,
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/medicationrequest-category", "code": "outpatient", "display": "Outpatient"}}},
			},
			"medicationCodeableConcept": map[string]any{
				"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "1191", "display": "Aspirin"}},
			},
			"subject":    map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":  map[string]any{"reference": "Encounter/enc-alice-001"},
			"authoredOn": "2026-01-15T09:33:00Z",
			"requester":  map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"reasonCode": []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "183945002", "display": "Procedure contraindicated"}}}},
		}),
		putEntry("Observation/obs-alice-cancelled", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-alice-cancelled",
			"status":            "cancelled",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory", "display": "Laboratory"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "4548-4", "display": "Hemoglobin A1c/Hemoglobin.total in Blood"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"effectiveDateTime": "2026-01-15T09:28:00Z",
			"issued":            "2026-01-15T09:29:00Z",
			"dataAbsentReason":  map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason", "code": "not-performed", "display": "Not Performed"}}},
		}),
		putEntry("Observation/obs-alice-simple-survey", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-alice-simple-survey",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "survey", "display": "Survey"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "44261-6", "display": "Patient Health Questionnaire 9 item (PHQ-9) total score [Reported]"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-alice-001"},
			"performer":         []map[string]any{{"reference": "Practitioner/prac-cardiology-1"}},
			"effectiveDateTime": "2026-01-15T09:19:00Z",
			"valueQuantity":     map[string]any{"value": 2.0, "unit": "{score}", "system": "http://unitsofmeasure.org", "code": "{score}"},
			"derivedFrom":       []map[string]any{{"reference": "QuestionnaireResponse/qr-alice-phq9"}},
		}),
		putEntry("Procedure/proc-alice-notdone", map[string]any{
			"resourceType":      "Procedure",
			"id":                "proc-alice-notdone",
			"status":            "not-done",
			"statusReason":      map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "183944003", "display": "Procedure refused"}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "80146002", "display": "Appendectomy"}}},
			"subject":           map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-alice-001"},
			"performedDateTime": "2026-01-15T09:22:00Z",
		}),
		putEntry("ServiceRequest/sr-alice-prohibited", map[string]any{
			"resourceType": "ServiceRequest",
			"id":           "sr-alice-prohibited",
			"status":       "completed",
			"intent":       "order",
			"doNotPerform": true,
			"category":     []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "387713003", "display": "Surgical procedure"}}}},
			"code":         map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "71388002", "display": "Procedure"}}},
			"subject":      map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":    map[string]any{"reference": "Encounter/enc-alice-001"},
			"authoredOn":   "2026-01-15T09:37:00Z",
			"requester":    map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"reasonCode":   []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "183945002", "display": "Procedure contraindicated"}}}},
		}),
		putEntry("Task/task-alice-done", map[string]any{
			"resourceType":    "Task",
			"id":              "task-alice-done",
			"status":          "completed",
			"intent":          "order",
			"code":            map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/CodeSystem/task-code", "code": "fulfill", "display": "Fulfill the focal request"}}},
			"focus":           map[string]any{"reference": "ServiceRequest/sr-alice-lipid"},
			"for":             map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":       map[string]any{"reference": "Encounter/enc-alice-001"},
			"executionPeriod": map[string]any{"start": "2026-01-15T09:35:00Z", "end": "2026-01-15T09:55:00Z"},
			"authoredOn":      "2026-01-15T09:35:00Z",
			"lastModified":    "2026-01-15T09:55:00Z",
			"requester":       map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"owner":           map[string]any{"reference": "Practitioner/prac-cardiology-1"},
		}),
		putEntry("Task/task-alice-rejected", map[string]any{
			"resourceType": "Task",
			"id":           "task-alice-rejected",
			"status":       "rejected",
			"statusReason": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "183944003", "display": "Procedure refused"}}},
			"intent":       "order",
			"code":         map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/CodeSystem/task-code", "code": "fulfill", "display": "Fulfill the focal request"}}},
			"focus":        map[string]any{"reference": "ServiceRequest/sr-alice-lipid"},
			"for":          map[string]any{"reference": "Patient/pat-alice-001"},
			"encounter":    map[string]any{"reference": "Encounter/enc-alice-001"},
			"authoredOn":   "2026-01-15T09:36:00Z",
			"lastModified": "2026-01-15T09:56:00Z",
			"requester":    map[string]any{"reference": "Practitioner/prac-cardiology-1"},
			"owner":        map[string]any{"reference": "Practitioner/prac-cardiology-1"},
		}),
	}

	var allAliceRefs []map[string]any
	for _, raw := range rawEntries {
		if em, ok := raw.(map[string]any); ok {
			if fu, _ := em["fullUrl"].(string); fu != "" && fu != "Provenance/prov-alice-001" {
				allAliceRefs = append(allAliceRefs, map[string]any{"reference": fu})
			}
		}
	}
	for _, item := range extra {
		if fu, _ := item["fullUrl"].(string); fu != "" {
			allAliceRefs = append(allAliceRefs, map[string]any{"reference": fu})
		}
		rawEntries = append(rawEntries, item)
	}
	for _, raw := range rawEntries {
		if em, ok := raw.(map[string]any); ok && em["fullUrl"] == "Provenance/prov-alice-001" {
			if res, ok := em["resource"].(map[string]any); ok {
				res["target"] = allAliceRefs
			}
		}
	}

	bundle["entry"] = rawEntries
	return json.MarshalIndent(bundle, "", "  ")
}

// CCRRTransactionBundleJSON builds the HL7 Central Cancer Registry Reporting (CCRR v1.0.0) EHR cohort
// covering US Core v3.1.1 + all 9 mCODE v3.0.0 profiles for pat-ccrr-001.
func CCRRTransactionBundleJSON() ([]byte, error) {
	noteData := base64.StdEncoding.EncodeToString([]byte("Oncology clinical note."))
	entries := []map[string]any{
		putEntry("Organization/org-ccrr-1", map[string]any{
			"resourceType": "Organization",
			"id":           "org-ccrr-1",
			"identifier":   []map[string]any{{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1497758544"}},
			"active":       true,
			"name":         "Flint Comprehensive Cancer Center",
			"telecom":      []map[string]any{{"system": "phone", "value": "617-555-0300"}},
			"address":      []map[string]any{{"line": []string{"88 Cancer Center Way"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"}},
		}),
		putEntry("Location/loc-ccrr-1", map[string]any{
			"resourceType":         "Location",
			"id":                   "loc-ccrr-1",
			"status":               "active",
			"name":                 "Flint Oncology Clinic",
			"telecom":              []map[string]any{{"system": "phone", "value": "617-555-0302"}},
			"address":              map[string]any{"line": []string{"88 Cancer Center Way"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"},
			"managingOrganization": map[string]any{"reference": "Organization/org-ccrr-1"},
		}),
		putEntry("Practitioner/prac-ccrr-1", map[string]any{
			"resourceType": "Practitioner",
			"id":           "prac-ccrr-1",
			"identifier":   []map[string]any{{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1881630614"}},
			"active":       true,
			"name":         []map[string]any{{"use": "official", "family": "Patel", "given": []string{"Raj"}, "prefix": []string{"Dr."}}},
			"telecom":      []map[string]any{{"system": "phone", "value": "617-555-0310"}, {"system": "email", "value": "rpatel@flint-cancer.example.org"}},
			"address":      []map[string]any{{"line": []string{"88 Cancer Center Way"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"}},
		}),
		putEntry("PractitionerRole/pr-ccrr-1", map[string]any{
			"resourceType": "PractitionerRole",
			"id":           "pr-ccrr-1",
			"active":       true,
			"practitioner": map[string]any{"reference": "Practitioner/prac-ccrr-1"},
			"organization": map[string]any{"reference": "Organization/org-ccrr-1"},
			"code":         []map[string]any{{"coding": []map[string]any{{"system": "http://nucc.org/provider-taxonomy", "code": "207RX0202X", "display": "Medical Oncology Physician"}}}},
			"specialty":    []map[string]any{{"coding": []map[string]any{{"system": "http://nucc.org/provider-taxonomy", "code": "207RX0202X", "display": "Medical Oncology Physician"}}}},
			"location":     []map[string]any{{"reference": "Location/loc-ccrr-1"}},
			"telecom":      []map[string]any{{"system": "phone", "value": "617-555-0310"}},
		}),
		putEntry("Patient/pat-ccrr-001", map[string]any{
			"resourceType": "Patient",
			"id":           "pat-ccrr-001",
			"extension": []map[string]any{
				{
					"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
					"extension": []map[string]any{
						{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2106-3", "display": "White"}},
						{"url": "detailed", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2108-9", "display": "European"}},
						{"url": "text", "valueString": "White"},
					},
				},
				{
					"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
					"extension": []map[string]any{
						{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2186-5", "display": "Not Hispanic or Latino"}},
						{"url": "detailed", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2184-0", "display": "Dominican"}},
						{"url": "text", "valueString": "Not Hispanic or Latino"},
					},
				},
				{"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex", "valueCode": "F"},
			},
			"identifier": []map[string]any{
				{"use": "usual", "type": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MR"}}}, "system": "http://hospital.smarthealthit.org", "value": "MRN-CCRR-001"},
			},
			"active": true,
			"name": []map[string]any{
				{"use": "official", "family": "Oncology", "given": []string{"Clara", "Anne"}, "suffix": []string{"III"}, "period": map[string]any{"start": "2010-01-01"}},
				{"use": "old", "family": "Miller", "given": []string{"Clara"}, "period": map[string]any{"start": "1972-06-15", "end": "2009-12-31"}},
			},
			"telecom":   []map[string]any{{"system": "phone", "value": "617-555-0301", "use": "home"}, {"system": "email", "value": "clara.oncology@example.org", "use": "home"}},
			"gender":    "female",
			"birthDate": "1972-06-15",
			"address": []map[string]any{
				{"use": "home", "line": []string{"88 Cancer Center Way"}, "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US", "period": map[string]any{"start": "2015-01-01"}},
				{"use": "old", "line": []string{"12 Harbor Rd"}, "city": "Quincy", "state": "MA", "postalCode": "02169", "country": "US", "period": map[string]any{"start": "2000-01-01", "end": "2014-12-31"}},
			},
			"communication": []map[string]any{
				{"language": map[string]any{"coding": []map[string]any{{"system": "urn:ietf:bcp:47", "code": "en-US", "display": "English"}}}, "preferred": true},
			},
		}),
		putEntry("Encounter/enc-ccrr-001", map[string]any{
			"resourceType": "Encounter",
			"id":           "enc-ccrr-001",
			"identifier":   []map[string]any{{"system": "http://hospital.smarthealthit.org/encounters", "value": "ENC-CCRR-001"}},
			"status":       "finished",
			"class":        map[string]any{"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB", "display": "ambulatory"},
			"type":         []map[string]any{{"coding": []map[string]any{{"system": "http://www.ama-assn.org/go/cpt", "code": "99214", "display": "Office outpatient visit"}}}},
			"subject":      map[string]any{"reference": "Patient/pat-ccrr-001"},
			"participant": []map[string]any{
				{"type": []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v3-ParticipationType", "code": "PPRF"}}}}, "period": map[string]any{"start": "2024-09-10T09:00:00Z", "end": "2024-09-10T09:45:00Z"}, "individual": map[string]any{"reference": "Practitioner/prac-ccrr-1"}},
			},
			"period":          map[string]any{"start": "2024-09-10T09:00:00Z", "end": "2024-09-10T09:45:00Z"},
			"reasonCode":      []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
			"hospitalization": map[string]any{"dischargeDisposition": map[string]any{"coding": []map[string]any{{"system": "http://www.nubc.org/patient-discharge", "code": "01", "display": "Discharged to Home"}}}},
			"location":        []map[string]any{{"location": map[string]any{"reference": "Location/loc-ccrr-1"}}},
			"serviceProvider": map[string]any{"reference": "Organization/org-ccrr-1"},
		}),
		putEntry("Observation/obs-ccrr-tnm-t", map[string]any{
			"resourceType":         "Observation",
			"id":                   "obs-ccrr-tnm-t",
			"meta":                 map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-tnm-primary-tumor-category"}},
			"status":               "final",
			"category":             []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "imaging"}}}},
			"code":                 map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "21905-5", "display": "Primary tumor.clinical [Class] Cancer"}}},
			"subject":              map[string]any{"reference": "Patient/pat-ccrr-001"},
			"focus":                []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
			"effectiveDateTime":    "2024-09-10T09:30:00Z",
			"performer":            []map[string]any{{"reference": "Practitioner/prac-ccrr-1"}},
			"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://cancerstaging.org", "code": "cT2", "display": "T2"}}},
			"method":               map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "897275008", "display": "AJCC 8th edition"}}},
		}),
		putEntry("Observation/obs-ccrr-tnm-n", map[string]any{
			"resourceType":         "Observation",
			"id":                   "obs-ccrr-tnm-n",
			"meta":                 map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-tnm-regional-nodes-category"}},
			"status":               "final",
			"category":             []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "imaging"}}}},
			"code":                 map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "21906-3", "display": "Regional lymph nodes.clinical [Class] Cancer"}}},
			"subject":              map[string]any{"reference": "Patient/pat-ccrr-001"},
			"focus":                []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
			"effectiveDateTime":    "2024-09-10T09:30:00Z",
			"performer":            []map[string]any{{"reference": "Practitioner/prac-ccrr-1"}},
			"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://cancerstaging.org", "code": "cN0", "display": "N0"}}},
			"method":               map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "897275008", "display": "AJCC 8th edition"}}},
		}),
		putEntry("Observation/obs-ccrr-tnm-m", map[string]any{
			"resourceType":         "Observation",
			"id":                   "obs-ccrr-tnm-m",
			"meta":                 map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-tnm-distant-metastases-category"}},
			"status":               "final",
			"category":             []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "imaging"}}}},
			"code":                 map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "21907-1", "display": "Distant metastases.clinical [Class] Cancer"}}},
			"subject":              map[string]any{"reference": "Patient/pat-ccrr-001"},
			"focus":                []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
			"effectiveDateTime":    "2024-09-10T09:30:00Z",
			"performer":            []map[string]any{{"reference": "Practitioner/prac-ccrr-1"}},
			"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://cancerstaging.org", "code": "cM0", "display": "M0"}}},
			"method":               map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "897275008", "display": "AJCC 8th edition"}}},
		}),
		putEntry("Observation/obs-ccrr-tnm-stage", map[string]any{
			"resourceType":         "Observation",
			"id":                   "obs-ccrr-tnm-stage",
			"meta":                 map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-tnm-stage-group"}},
			"status":               "final",
			"category":             []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "imaging"}}}},
			"code":                 map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "21908-9", "display": "Stage group.clinical Cancer"}}},
			"subject":              map[string]any{"reference": "Patient/pat-ccrr-001"},
			"focus":                []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
			"effectiveDateTime":    "2024-09-10T09:30:00Z",
			"performer":            []map[string]any{{"reference": "Practitioner/prac-ccrr-1"}},
			"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://cancerstaging.org", "code": "2A", "display": "IIA"}}},
			"method":               map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "897275008", "display": "AJCC 8th edition"}}},
			"hasMember": []map[string]any{
				{"reference": "Observation/obs-ccrr-tnm-t"},
				{"reference": "Observation/obs-ccrr-tnm-n"},
				{"reference": "Observation/obs-ccrr-tnm-m"},
			},
		}),
		putEntry("Condition/cond-ccrr-primary", map[string]any{
			"resourceType": "Condition",
			"id":           "cond-ccrr-primary",
			"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-primary-cancer-condition"}},
			"extension": []map[string]any{
				{"url": "http://hl7.org/fhir/StructureDefinition/condition-assertedDate", "valueDateTime": "2024-09-10"},
				{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-histology-morphology-behavior", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "82711006", "display": "Infiltrating duct carcinoma"}}}},
			},
			"clinicalStatus":     map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}}},
			"verificationStatus": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}}},
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item"}}},
				{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-tags", "code": "sdoh"}}},
				{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "363346000", "display": "Malignant neoplastic disease"}}},
				{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/mcode/CodeSystem/snomed-requested-cs", "code": "primary-cancer-condition"}}},
			},
			"code": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}},
			"bodySite": []map[string]any{
				{
					"extension": []map[string]any{
						{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-body-location-qualifier", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "261183002", "display": "Upper"}}}},
						{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-laterality-qualifier", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "7771000", "display": "Left"}}}},
					},
					"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "76752008", "display": "Breast structure"}},
				},
			},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-ccrr-001"},
			"onsetDateTime":     "2024-09-01",
			"abatementDateTime": "2024-11-15",
			"recordedDate":      "2024-09-10",
			"stage": []map[string]any{
				{"summary": map[string]any{"coding": []map[string]any{{"system": "http://cancerstaging.org", "code": "2A", "display": "IIA"}}}, "assessment": []map[string]any{{"reference": "Observation/obs-ccrr-tnm-stage"}}},
			},
		}),
		putEntry("Condition/cond-ccrr-secondary", map[string]any{
			"resourceType": "Condition",
			"id":           "cond-ccrr-secondary",
			"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-secondary-cancer-condition"}},
			"extension": []map[string]any{
				{"url": "http://hl7.org/fhir/StructureDefinition/condition-assertedDate", "valueDateTime": "2024-09-12"},
				{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-histology-morphology-behavior", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "82711006", "display": "Infiltrating duct carcinoma"}}}},
				{"url": "http://hl7.org/fhir/StructureDefinition/condition-related", "valueReference": map[string]any{"reference": "Condition/cond-ccrr-primary"}},
			},
			"clinicalStatus":     map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}}},
			"verificationStatus": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}}},
			"category": []map[string]any{
				{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item"}}},
				{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-tags", "code": "sdoh"}}},
				{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "128462008", "display": "Secondary malignant neoplastic disease"}}},
				{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/mcode/CodeSystem/snomed-requested-cs", "code": "secondary-cancer-condition"}}},
			},
			"code": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "94391008", "display": "Secondary malignant neoplasm of axillary lymph nodes"}}},
			"bodySite": []map[string]any{
				{
					"extension": []map[string]any{
						{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-body-location-qualifier", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "261183002", "display": "Upper"}}}},
						{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-laterality-qualifier", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "7771000", "display": "Left"}}}},
					},
					"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "68171009", "display": "Axillary lymph node structure"}},
				},
			},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-ccrr-001"},
			"onsetDateTime":     "2024-09-12",
			"abatementDateTime": "2024-11-15",
			"_recordedDate": map[string]any{
				"extension": []map[string]any{{"url": "http://hl7.org/fhir/StructureDefinition/data-absent-reason", "valueCode": "unknown"}},
			},
		}),
		putEntry("BodyStructure/bs-ccrr-breast-1", map[string]any{
			"resourceType":      "BodyStructure",
			"id":                "bs-ccrr-breast-1",
			"meta":              map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-volume"}},
			"identifier":        []map[string]any{{"system": "http://hospital.smarthealthit.org/volumes", "value": "VOL-BREAST-L"}},
			"morphology":        map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "228793007", "display": "Planning target volume"}}},
			"location":          map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "76752008", "display": "Breast structure"}}},
			"locationQualifier": []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "7771000", "display": "Left"}}}},
			"patient":           map[string]any{"reference": "Patient/pat-ccrr-001"},
		}),
		putEntry("Procedure/proc-ccrr-radiotherapy", map[string]any{
			"resourceType": "Procedure",
			"id":           "proc-ccrr-radiotherapy",
			"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-course-summary"}},
			"extension": []map[string]any{
				{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-procedure-intent", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "373808002", "display": "Curative - procedure intent"}}}},
				{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-treatment-termination-reason", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "182992009", "display": "Treatment completed"}}}},
				{
					"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-modality-and-technique",
					"extension": []map[string]any{
						{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-modality", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "1156506007", "display": "External beam radiation therapy using photons"}}}},
						{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-technique", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "441799006", "display": "Intensity modulated radiation therapy"}}}},
					},
				},
				{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-sessions", "valueUnsignedInt": 15},
				{
					"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-radiotherapy-dose-delivered-to-volume",
					"extension": []map[string]any{
						{"url": "volume", "valueReference": map[string]any{"reference": "BodyStructure/bs-ccrr-breast-1"}},
						{"url": "totalDoseDelivered", "valueQuantity": map[string]any{"value": 4005.0, "unit": "cGy", "system": "http://unitsofmeasure.org", "code": "cGy"}},
						{"url": "fractionsDelivered", "valueUnsignedInt": 15},
					},
				},
			},
			"status":          "completed",
			"category":        map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "108290001", "display": "Radiation oncology AND/OR radiotherapy"}}},
			"code":            map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "1217123003", "display": "Radiotherapy course of treatment"}}},
			"subject":         map[string]any{"reference": "Patient/pat-ccrr-001"},
			"performedPeriod": map[string]any{"start": "2024-10-01", "end": "2024-10-21"},
			"reasonCode":      []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
			"reasonReference": []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
			"bodySite":        []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "76752008", "display": "Breast structure"}}}},
		}),
		putEntry("Medication/med-ccrr-paclitaxel", map[string]any{
			"resourceType": "Medication",
			"id":           "med-ccrr-paclitaxel",
			"code":         map[string]any{"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "56946", "display": "Paclitaxel"}}},
		}),
		putEntry("MedicationAdministration/ma-ccrr-1", map[string]any{
			"resourceType": "MedicationAdministration",
			"id":           "ma-ccrr-1",
			"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-cancer-related-medication-administration"}},
			"extension": []map[string]any{
				{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-procedure-intent", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "373808002", "display": "Curative - procedure intent"}}}},
				{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-treatment-termination-reason", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "182992009", "display": "Treatment completed"}}}},
			},
			"status":                    "completed",
			"statusReason":              []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "182856006", "display": "Drug stock out"}}}},
			"medicationCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "56946", "display": "Paclitaxel"}}},
			"subject":                   map[string]any{"reference": "Patient/pat-ccrr-001"},
			"context":                   map[string]any{"reference": "Encounter/enc-ccrr-001"},
			"effectiveDateTime":         "2024-09-20T10:00:00Z",
			"performer":                 []map[string]any{{"actor": map[string]any{"reference": "Practitioner/prac-ccrr-1"}}},
			"reasonCode":                []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
			"reasonReference":           []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
			"note":                      []map[string]any{{"text": "Infusion tolerated well"}},
			"dosage": map[string]any{
				"text":  "175 mg/m2 IV over 3 hours",
				"route": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "47625008", "display": "Intravenous route"}}},
				"dose":  map[string]any{"value": 300.0, "unit": "mg", "system": "http://unitsofmeasure.org", "code": "mg"},
			},
		}),
		putEntry("MedicationRequest/mr-ccrr-1", map[string]any{
			"resourceType": "MedicationRequest",
			"id":           "mr-ccrr-1",
			"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-cancer-related-medication-request"}},
			"extension": []map[string]any{
				{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-procedure-intent", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "373808002", "display": "Curative - procedure intent"}}}},
				{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-treatment-termination-reason", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "182992009", "display": "Treatment completed"}}}},
			},
			"status":                    "active",
			"intent":                    "order",
			"category":                  []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/medicationrequest-category", "code": "outpatient"}}}},
			"reportedBoolean":           false,
			"medicationCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "10324", "display": "Tamoxifen"}}},
			"subject":                   map[string]any{"reference": "Patient/pat-ccrr-001"},
			"encounter":                 map[string]any{"reference": "Encounter/enc-ccrr-001"},
			"authoredOn":                "2024-10-25",
			"requester":                 map[string]any{"reference": "Practitioner/prac-ccrr-1"},
			"reasonCode":                []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
			"reasonReference":           []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
			"dosageInstruction": []map[string]any{
				{
					"text":   "Take 1 tablet (20 mg) by mouth daily",
					"timing": map[string]any{"repeat": map[string]any{"frequency": 1, "period": 1.0, "periodUnit": "d"}},
					"route":  map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "26643006", "display": "Oral route"}}},
					"doseAndRate": []map[string]any{
						{"doseQuantity": map[string]any{"value": 20.0, "unit": "mg", "system": "http://unitsofmeasure.org", "code": "mg"}},
					},
				},
			},
			"dispenseRequest": map[string]any{
				"numberOfRepeatsAllowed": 3,
				"quantity":               map[string]any{"value": 30.0, "unit": "TAB", "system": "http://terminology.hl7.org/CodeSystem/v3-orderableDrugForm", "code": "TAB"},
			},
		}),
		putEntry("MedicationRequest/mr-ccrr-2", map[string]any{
			"resourceType": "MedicationRequest",
			"id":           "mr-ccrr-2",
			"meta":         map[string]any{"profile": []string{"http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-cancer-related-medication-request"}},
			"extension": []map[string]any{
				{"url": "http://hl7.org/fhir/us/mcode/StructureDefinition/mcode-procedure-intent", "valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "373808002", "display": "Curative - procedure intent"}}}},
			},
			"status":              "active",
			"intent":              "order",
			"category":            []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/medicationrequest-category", "code": "outpatient"}}}},
			"reportedReference":   map[string]any{"reference": "Practitioner/prac-ccrr-1"},
			"medicationReference": map[string]any{"reference": "Medication/med-ccrr-paclitaxel"},
			"subject":             map[string]any{"reference": "Patient/pat-ccrr-001"},
			"encounter":           map[string]any{"reference": "Encounter/enc-ccrr-001"},
			"authoredOn":          "2024-09-19",
			"requester":           map[string]any{"reference": "Practitioner/prac-ccrr-1"},
			"reasonCode":          []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "254837009", "display": "Malignant neoplasm of breast"}}}},
			"reasonReference":     []map[string]any{{"reference": "Condition/cond-ccrr-primary"}},
			"dosageInstruction":   []map[string]any{{"text": "175 mg/m2 IV every 21 days"}},
		}),
		putEntry("AllergyIntolerance/allergy-ccrr-1", map[string]any{
			"resourceType":       "AllergyIntolerance",
			"id":                 "allergy-ccrr-1",
			"clinicalStatus":     map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}}},
			"verificationStatus": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification", "code": "confirmed"}}},
			"code":               map[string]any{"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980", "display": "Penicillin G"}}},
			"patient":            map[string]any{"reference": "Patient/pat-ccrr-001"},
			"reaction":           []map[string]any{{"manifestation": []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "247472004", "display": "Hives"}}}}}},
		}),
		putEntry("CarePlan/cp-ccrr-1", map[string]any{
			"resourceType": "CarePlan",
			"id":           "cp-ccrr-1",
			"text":         map[string]any{"status": "generated", "div": `<div xmlns="http://www.w3.org/1999/xhtml"><p>Oncology Care Plan</p></div>`},
			"status":       "active",
			"intent":       "plan",
			"category":     []map[string]any{{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/careplan-category", "code": "assess-plan"}}}},
			"subject":      map[string]any{"reference": "Patient/pat-ccrr-001"},
		}),
		putEntry("CareTeam/ct-ccrr-1", map[string]any{
			"resourceType": "CareTeam",
			"id":           "ct-ccrr-1",
			"status":       "active",
			"subject":      map[string]any{"reference": "Patient/pat-ccrr-001"},
			"participant": []map[string]any{
				{"role": []map[string]any{{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "17561000", "display": "Oncologist"}}}}, "member": map[string]any{"reference": "Practitioner/prac-ccrr-1"}},
			},
		}),
		putEntry("Goal/goal-ccrr-1", map[string]any{
			"resourceType":    "Goal",
			"id":              "goal-ccrr-1",
			"lifecycleStatus": "active",
			"description":     map[string]any{"text": "Complete adjuvant radiotherapy and maintain disease-free survival"},
			"subject":         map[string]any{"reference": "Patient/pat-ccrr-001"},
			"target":          []map[string]any{{"dueDate": "2025-12-31"}},
		}),
		putEntry("Immunization/imm-ccrr-1", map[string]any{
			"resourceType":       "Immunization",
			"id":                 "imm-ccrr-1",
			"status":             "completed",
			"vaccineCode":        map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/cvx", "code": "208", "display": "COVID-19, mRNA, LNP-S, PF, 30 mcg/0.3 mL dose"}}},
			"patient":            map[string]any{"reference": "Patient/pat-ccrr-001"},
			"occurrenceDateTime": "2024-09-10T10:00:00Z",
			"primarySource":      true,
		}),
		putEntry("Device/dev-ccrr-port", map[string]any{
			"resourceType":       "Device",
			"id":                 "dev-ccrr-port",
			"udiCarrier":         []map[string]any{{"deviceIdentifier": "00844588003288", "carrierHRF": "(01)00844588003288(11)240101(17)290101(10)LOT1(21)SER1", "carrierAIDC": "KDAxKTAwODQ0NTg4MDAzMjg4"}},
			"distinctIdentifier": "00844588003288",
			"manufactureDate":    "2024-01-01T00:00:00Z",
			"expirationDate":     "2029-01-01T00:00:00Z",
			"lotNumber":          "LOT1",
			"serialNumber":       "SER1",
			"type":               map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "706004007", "display": "Implantable venous access port"}}},
			"patient":            map[string]any{"reference": "Patient/pat-ccrr-001"},
		}),
		putEntry("Observation/obs-ccrr-bp", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-bp",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure panel"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"component": []map[string]any{
				{"code": map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8480-6"}}}, "valueQuantity": map[string]any{"value": 124.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}},
				{"code": map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8462-4"}}}, "valueQuantity": map[string]any{"value": 78.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}},
				{"code": map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8478-0"}}}, "dataAbsentReason": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason", "code": "unknown"}}}},
			},
		}),
		putEntry("Observation/obs-ccrr-hr", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-hr",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8867-4", "display": "Heart rate"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 72.0, "unit": "beats/minute", "system": "http://unitsofmeasure.org", "code": "/min"},
		}),
		putEntry("Observation/obs-ccrr-resp", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-resp",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "9279-1", "display": "Respiratory rate"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 16.0, "unit": "breaths/minute", "system": "http://unitsofmeasure.org", "code": "/min"},
		}),
		putEntry("Observation/obs-ccrr-temp", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-temp",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8310-5", "display": "Body temperature"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 36.7, "unit": "C", "system": "http://unitsofmeasure.org", "code": "Cel"},
		}),
		putEntry("Observation/obs-ccrr-height", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-height",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8302-2", "display": "Body height"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 166.0, "unit": "cm", "system": "http://unitsofmeasure.org", "code": "cm"},
		}),
		putEntry("Observation/obs-ccrr-weight", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-weight",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "29463-7", "display": "Body weight"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 64.5, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"},
		}),
		putEntry("Observation/obs-ccrr-bmi", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-bmi",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "39156-5", "display": "Body mass index"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 23.4, "unit": "kg/m2", "system": "http://unitsofmeasure.org", "code": "kg/m2"},
		}),
		putEntry("Observation/obs-ccrr-pulse-ox", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-pulse-ox",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "59408-5"}, {"system": "http://loinc.org", "code": "2708-6"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 98.0, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"},
			"component": []map[string]any{
				{"code": map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "3151-8"}}}, "valueQuantity": map[string]any{"value": 2.0, "unit": "L/min", "system": "http://unitsofmeasure.org", "code": "L/min"}},
				{"code": map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "3150-0"}}}, "valueQuantity": map[string]any{"value": 28.0, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"}},
			},
		}),
		putEntry("Observation/obs-ccrr-ped-bmi", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-ped-bmi",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "59576-9"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 60.0, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"},
		}),
		putEntry("Observation/obs-ccrr-ped-hc", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-ped-hc",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8289-1"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 52.0, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"},
		}),
		putEntry("Observation/obs-ccrr-ped-wfh", map[string]any{
			"resourceType":      "Observation",
			"id":                "obs-ccrr-ped-wfh",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "77606-2"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime": "2024-09-10T09:05:00Z",
			"valueQuantity":     map[string]any{"value": 55.0, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"},
		}),
		putEntry("Observation/obs-ccrr-smoking", map[string]any{
			"resourceType":         "Observation",
			"id":                   "obs-ccrr-smoking",
			"status":               "final",
			"category":             []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "social-history"}}}},
			"code":                 map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "72166-2", "display": "Tobacco smoking status"}}},
			"subject":              map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime":    "2024-09-10T09:05:00Z",
			"issued":               "2024-09-10T09:06:00Z",
			"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "266919005", "display": "Never smoked tobacco"}}},
		}),
		putEntry("Observation/obs-ccrr-lab-er", map[string]any{
			"resourceType":         "Observation",
			"id":                   "obs-ccrr-lab-er",
			"status":               "final",
			"category":             []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}}}},
			"code":                 map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "85337-4", "display": "Estrogen receptor Ag [Presence] in Breast cancer specimen"}}},
			"subject":              map[string]any{"reference": "Patient/pat-ccrr-001"},
			"effectiveDateTime":    "2024-09-10T11:00:00Z",
			"performer":            []map[string]any{{"reference": "Organization/org-ccrr-1"}},
			"valueCodeableConcept": map[string]any{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "10828004", "display": "Positive"}}},
		}),
		putEntry("DiagnosticReport/dr-ccrr-lab", map[string]any{
			"resourceType":      "DiagnosticReport",
			"id":                "dr-ccrr-lab",
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "LAB"}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "24323-8", "display": "Comprehensive metabolic 2000 panel"}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-ccrr-001"},
			"effectiveDateTime": "2024-09-10T11:00:00Z",
			"issued":            "2024-09-10T12:00:00Z",
			"performer":         []map[string]any{{"reference": "Organization/org-ccrr-1"}},
			"result":            []map[string]any{{"reference": "Observation/obs-ccrr-lab-er"}},
		}),
	}

	for _, drSpec := range []struct {
		id, catCode, catDisp, loincCode string
	}{
		{"dr-ccrr-note-rad", "LP29684-5", "Radiology", "30746-2"},
		{"dr-ccrr-note-card", "LP29708-2", "Cardiology", "18745-0"},
		{"dr-ccrr-note-path", "LP7839-6", "Pathology", "60568-3"},
	} {
		entries = append(entries, putEntry("DiagnosticReport/"+drSpec.id, map[string]any{
			"resourceType":      "DiagnosticReport",
			"id":                drSpec.id,
			"status":            "final",
			"category":          []map[string]any{{"coding": []map[string]any{{"system": "http://loinc.org", "code": drSpec.catCode, "display": drSpec.catDisp}}}},
			"code":              map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": drSpec.loincCode}}},
			"subject":           map[string]any{"reference": "Patient/pat-ccrr-001"},
			"encounter":         map[string]any{"reference": "Encounter/enc-ccrr-001"},
			"effectiveDateTime": "2024-09-10T11:30:00Z",
			"issued":            "2024-09-10T12:30:00Z",
			"performer":         []map[string]any{{"reference": "Practitioner/prac-ccrr-1"}},
			"presentedForm":     []map[string]any{{"contentType": "text/plain", "data": noteData}},
		}))
	}

	for _, docSpec := range []struct {
		id, code, disp string
	}{
		{"docref-ccrr-consult", "11488-4", "Consult note"},
		{"docref-ccrr-discharge", "18842-5", "Discharge summary"},
		{"docref-ccrr-hp", "34117-2", "History and physical note"},
		{"docref-ccrr-proc", "28570-0", "Procedure note"},
		{"docref-ccrr-progress", "11506-3", "Progress note"},
	} {
		entries = append(entries, putEntry("DocumentReference/"+docSpec.id, map[string]any{
			"resourceType": "DocumentReference",
			"id":           docSpec.id,
			"identifier":   []map[string]any{{"system": "http://hospital.smarthealthit.org/docs", "value": "DOC-" + docSpec.id}},
			"status":       "current",
			"type":         map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": docSpec.code, "display": docSpec.disp}}},
			"category":     []map[string]any{{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-documentreference-category", "code": "clinical-note"}}}},
			"subject":      map[string]any{"reference": "Patient/pat-ccrr-001"},
			"date":         "2024-09-10T13:00:00Z",
			"author":       []map[string]any{{"reference": "Practitioner/prac-ccrr-1"}},
			"custodian":    map[string]any{"reference": "Organization/org-ccrr-1"},
			"content": []map[string]any{
				{
					"attachment": map[string]any{"contentType": "text/plain", "data": noteData},
					"format":     map[string]any{"system": "http://ihe.net/fhir/ihe.formatcode.fhir/CodeSystem/formatcode", "code": "urn:ihe:iti:xds:2017:mimeTypeSufficient"},
				},
			},
			"context": map[string]any{"encounter": []map[string]any{{"reference": "Encounter/enc-ccrr-001"}}, "period": map[string]any{"start": "2024-09-10T09:00:00Z", "end": "2024-09-10T09:45:00Z"}},
		}))
	}

	var targets []map[string]any
	for _, e := range entries {
		if fu, _ := e["fullUrl"].(string); fu != "" {
			targets = append(targets, map[string]any{"reference": fu})
		}
	}
	entries = append(entries, putEntry("Provenance/prov-ccrr-001", map[string]any{
		"resourceType": "Provenance",
		"id":           "prov-ccrr-001",
		"target":       targets,
		"recorded":     "2024-09-10T14:00:00Z",
		"agent": []map[string]any{
			{
				"type":       map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/provenance-participant-type", "code": "author"}}},
				"who":        map[string]any{"reference": "Practitioner/prac-ccrr-1"},
				"onBehalfOf": map[string]any{"reference": "Organization/org-ccrr-1"},
			},
			{
				"type":       map[string]any{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-provenance-participant-type", "code": "transmitter"}}},
				"who":        map[string]any{"reference": "Practitioner/prac-ccrr-1"},
				"onBehalfOf": map[string]any{"reference": "Organization/org-ccrr-1"},
			},
		},
	}))

	return json.MarshalIndent(map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry":        entries,
	}, "", "  ")
}
