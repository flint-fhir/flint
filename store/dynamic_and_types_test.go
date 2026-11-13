package store

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/fhirutil"
)

func TestStoreDomainTypesAndHelpers(t *testing.T) {
	// ETag formatting & parsing
	assert.Equal(t, `W/"3"`, FormatETag(3))
	v, err := ParseETagVersion(`W/"3"`)
	require.NoError(t, err)
	assert.Equal(t, 3, v)

	v, err = ParseETagVersion(`"42"`)
	require.NoError(t, err)
	assert.Equal(t, 42, v)

	for _, bad := range []string{"", "   ", `W/""`, `W/"0"`, `W/"-2"`, `W/"abc"`, "unquoted"} {
		_, err := ParseETagVersion(bad)
		require.Error(t, err, "expected invalid ETag for %q", bad)
	}

	// Compartment types
	for _, ct := range []string{"patient", "Encounter", "PRACTITIONER", "RelatedPerson", "device"} {
		assert.NotEmpty(t, CanonicalCompartmentType(ct))
		assert.True(t, IsCompartmentType(ct))
	}
	assert.Equal(t, "", CanonicalCompartmentType("UnknownType"))
	assert.False(t, IsCompartmentType("UnknownType"))

	patTypes := PatientCompartmentResourceTypes()
	assert.NotEmpty(t, patTypes)
	assert.True(t, IsPatientCompartmentResourceType("Observation"))
	assert.False(t, IsPatientCompartmentResourceType("StructureDefinition"))

	// IndexesBelongToPatientCompartment
	assert.True(t, IndexesBelongToPatientCompartment("Patient", "p-1", nil, "p-1"))
	assert.False(t, IndexesBelongToPatientCompartment("Patient", "p-2", nil, "p-1"))
	assert.False(t, IndexesBelongToPatientCompartment("Observation", "obs-1", nil, "p-1"))
	assert.False(t, IndexesBelongToPatientCompartment("Observation", "obs-1", &SearchIndexes{}, ""))
	assert.True(t, IndexesBelongToPatientCompartment("Observation", "obs-1", &SearchIndexes{
		References: []SpidxReference{{SpName: "subject", TargetType: "Patient", TargetID: "p-1"}},
	}, "p-1"))
	assert.False(t, IndexesBelongToPatientCompartment("Observation", "obs-1", &SearchIndexes{
		References: []SpidxReference{{SpName: "subject", TargetType: "Patient", TargetID: "p-2"}},
	}, "p-1"))
	assert.False(t, IndexesBelongToPatientCompartment("Observation", "obs-1", &SearchIndexes{
		References: []SpidxReference{{SpName: "performer", TargetType: "Practitioner", TargetID: "prac-1"}},
	}, "p-1"))

	// DefaultGraphLinksForResource
	assert.NotEmpty(t, DefaultGraphLinksForResource("Patient"))
	assert.NotEmpty(t, DefaultGraphLinksForResource("Encounter"))
	assert.NotEmpty(t, DefaultGraphLinksForResource("Observation"))

	// CoreResourceTableName
	for _, rt := range USCoreResourceTypes {
		tbl, ok := CoreResourceTableName(rt)
		assert.True(t, ok, "expected core table for %s", rt)
		assert.NotEmpty(t, tbl)
	}
	_, ok := CoreResourceTableName("MeasureReport")
	assert.False(t, ok)
}

func TestDynamicResourceExtractorsAndCustomSearchParams(t *testing.T) {
	customDefs := []CustomSearchParamDef{
		{ID: "sp-str", Code: "custom-str", Base: []string{"Patient"}, Type: "string", Expression: "(Patient.name.family) | Patient.name.given"},
		{ID: "sp-tok", Code: "custom-tok", Base: []string{"Patient"}, Type: "token", Expression: "Patient.extension('http://example.org/ext-code')"},
		{ID: "sp-date", Code: "custom-date", Base: []string{"Patient"}, Type: "date", Expression: "Patient.birthDate"},
		{ID: "sp-qty", Code: "custom-qty", Base: []string{"Observation"}, Type: "quantity", Expression: "Observation.valueQuantity"},
		{ID: "sp-ref", Code: "custom-ref", Base: []string{"Observation"}, Type: "reference", Expression: "Observation.subject"},
		{ID: "sp-uri", Code: "custom-uri", Base: []string{"ConceptMap"}, Type: "uri", Expression: "ConceptMap.url"},
		{ID: "sp-obs-str", Code: "custom-obs-str", Base: []string{"Observation"}, Type: "string", Expression: "Observation.code.coding | Observation.code | Observation.identifier | Observation.status"},
		{ID: "sp-ext-where", Code: "custom-where", Base: []string{"Patient"}, Type: "string", Expression: "Patient.extension.where(url='http://example.org/ext-where').valueString"},
		{ID: "sp-choice", Code: "custom-choice", Base: []string{"MedicationDispense"}, Type: "token", Expression: "MedicationDispense.medicationCodeableConcept"},
	}
	for _, cd := range customDefs {
		RegisterCustomSearchParam(cd)
	}
	defer func() {
		for _, cd := range customDefs {
			UnregisterCustomSearchParam(cd.ID)
		}
	}()
	assert.NotEmpty(t, CustomSearchParamsForType("Patient"))
	assert.Equal(t, "string", SearchParamCategory("custom-str"))
	assert.Equal(t, "token", SearchParamCategory("custom-tok"))
	assert.Equal(t, "date", SearchParamCategory("custom-date"))
	assert.Equal(t, "quantity", SearchParamCategory("custom-qty"))
	assert.Equal(t, "reference", SearchParamCategory("custom-ref"))
	assert.Equal(t, "uri", SearchParamCategory("custom-uri"))

	descs := fhirutil.AllR4ResourceDescriptors()
	staticExtractors := DefaultIndexExtractors()

	testResources := []struct {
		resType string
		id      string
		rawJSON string
	}{
		{
			resType: "Patient",
			id:      "pat-dyn-1",
			rawJSON: `{
				"resourceType": "Patient",
				"id": "pat-dyn-1",
				"meta": {
					"source": "http://source.example.org",
					"profile": ["http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"],
					"security": [{"system": "http://terminology.hl7.org/CodeSystem/v3-Confidentiality", "code": "N", "display": "Normal"}],
					"tag": [{"system": "http://example.org/tag", "code": "vip", "display": "VIP Patient"}]
				},
				"text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\"><p>Alice Smith Narrative</p></div>"},
				"extension": [
					{
						"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
						"extension": [
							{"url": "ombCategory", "valueCoding": {"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2106-3", "display": "White"}},
							{"url": "text", "valueString": "White"}
						]
					},
					{
						"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
						"extension": [
							{"url": "ombCategory", "valueCoding": {"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2186-5", "display": "Not Hispanic or Latino"}}
						]
					},
					{
						"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex",
						"valueCode": "F"
					},
					{"url": "http://example.org/ext-code", "valueCodeableConcept": {"coding": [{"system": "http://example.org", "code": "C1"}], "text": "Concept 1"}},
					{"url": "http://example.org/ext-code", "valueIdentifier": {"system": "http://example.org/id", "value": "ID-99"}},
					{"url": "http://example.org/ext-code", "valueReference": {"reference": "Organization/org-1"}},
					{"url": "http://example.org/ext-code", "valueDate": "2020-01-01"},
					{"url": "http://example.org/ext-code", "valuePeriod": {"start": "2020-01-01", "end": "2021-01-01"}},
					{"url": "http://example.org/ext-code", "valueQuantity": {"value": 12.5, "unit": "mg", "system": "http://unitsofmeasure.org", "code": "mg"}},
					{"url": "http://example.org/ext-code", "valueDecimal": 3.14},
					{"url": "http://example.org/ext-code", "valueInteger": 42},
					{"url": "http://example.org/ext-code", "valueBoolean": true},
					{"url": "http://example.org/ext-code", "valueUri": "http://example.org/u1"},
					{"url": "http://example.org/ext-where", "valueString": "MatchedWhereValue"}
				],
				"identifier": [{
					"type": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MR", "display": "Medical Record"}], "text": "MRN"},
					"system": "http://hospital.example.org/mrn",
					"value": "MRN-777"
				}],
				"active": true,
				"name": [{"family": "Smith", "given": ["Alice", "Marie"], "text": "Alice Marie Smith"}],
				"telecom": [
					{"system": "phone", "use": "home", "value": "555-0101"},
					{"system": "email", "use": "work", "value": "alice@example.org"}
				],
				"gender": "female",
				"birthDate": "1985-04-12",
				"deceasedBoolean": false,
				"maritalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v3-MaritalStatus", "code": "M"}]},
				"communication": [{"language": {"coding": [{"system": "urn:ietf:bcp:47", "code": "en"}]}}],
				"address": [{"use": "home", "line": ["123 Main St"], "city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"}],
				"generalPractitioner": [{"reference": "Practitioner/prac-1"}],
				"managingOrganization": {"reference": "Organization/org-1"}
			}`,
		},
		{
			resType: "MedicationRequest",
			id:      "mr-dyn-1",
			rawJSON: `{
				"resourceType": "MedicationRequest",
				"id": "mr-dyn-1",
				"contained": [{
					"resourceType": "Medication",
					"id": "med-inline-1",
					"code": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "723", "display": "Amoxicillin"}], "text": "Amoxicillin"}
				}],
				"status": "active",
				"intent": "order",
				"priority": "routine",
				"category": [{"coding": [{"code": "outpatient"}]}],
				"medicationReference": {"reference": "#med-inline-1"},
				"subject": {"reference": "Patient/pat-dyn-1", "identifier": {"system": "http://hospital.example.org/mrn", "value": "MRN-777"}},
				"encounter": {"reference": "Encounter/enc-1"},
				"authoredOn": "2026-01-15T10:00:00Z",
				"requester": {"reference": "Practitioner/prac-1"}
			}`,
		},
		{
			resType: "Observation",
			id:      "obs-dyn-1",
			rawJSON: `{
				"resourceType": "Observation",
				"id": "obs-dyn-1",
				"identifier": [{"system": "http://example.org/obs", "value": "OBS-1"}],
				"status": "final",
				"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}]}],
				"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure"}], "text": "BP"},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"encounter": {"reference": "Encounter/enc-1"},
				"performer": [{"reference": "Practitioner/prac-1"}],
				"effectivePeriod": {"start": "2026-01-01T00:00:00Z", "end": "2026-01-02T00:00:00Z"},
				"valueQuantity": {"value": 120, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"},
				"component": [
					{
						"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic"}]},
						"valueQuantity": {"value": 120, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
					},
					{
						"code": {"coding": [{"system": "http://loinc.org", "code": "72166-2", "display": "Smoking"}]},
						"valueCodeableConcept": {"coding": [{"system": "http://snomed.info/sct", "code": "449868002"}]}
					}
				]
			}`,
		},
		{
			resType: "Observation",
			id:      "obs-dyn-str-date",
			rawJSON: `{
				"resourceType": "Observation",
				"id": "obs-dyn-str-date",
				"status": "final",
				"code": {"coding": [{"system": "http://loinc.org", "code": "11778-8"}]},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"effectiveDateTime": "2026-02-01",
				"valueString": "Positive result"
			}`,
		},
		{
			resType: "Observation",
			id:      "obs-dyn-concept",
			rawJSON: `{
				"resourceType": "Observation",
				"id": "obs-dyn-concept",
				"status": "final",
				"code": {"coding": [{"system": "http://loinc.org", "code": "72166-2"}]},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"effectiveDateTime": "2026-02-01T12:00:00Z",
				"valueCodeableConcept": {"coding": [{"system": "http://snomed.info/sct", "code": "449868002"}], "text": "Current every day smoker"}
			}`,
		},
		{
			resType: "Condition",
			id:      "cond-dyn-1",
			rawJSON: `{
				"resourceType": "Condition",
				"id": "cond-dyn-1",
				"clinicalStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}]},
				"verificationStatus": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}]},
				"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item"}]}],
				"severity": {"coding": [{"code": "24484000"}]},
				"code": {"coding": [{"system": "http://snomed.info/sct", "code": "38341003", "display": "Hypertension"}], "text": "Hypertension"},
				"bodySite": [{"coding": [{"code": "39607008"}]}],
				"subject": {"reference": "Patient/pat-dyn-1"},
				"encounter": {"reference": "Encounter/enc-1"},
				"onsetDateTime": "2024-05-01",
				"recordedDate": "2024-05-02"
			}`,
		},
		{
			resType: "Encounter",
			id:      "enc-dyn-1",
			rawJSON: `{
				"resourceType": "Encounter",
				"id": "enc-dyn-1",
				"status": "finished",
				"class": {"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB"},
				"type": [{"coding": [{"system": "http://www.ama-assn.org/go/cpt", "code": "99213"}]}],
				"subject": {"reference": "Patient/pat-dyn-1"},
				"participant": [{"individual": {"reference": "Practitioner/prac-1"}}],
				"period": {"start": "2026-01-10T09:00:00Z", "end": "2026-01-10T09:30:00Z"},
				"location": [{"location": {"reference": "Location/loc-1"}}],
				"serviceProvider": {"reference": "Organization/org-1"}
			}`,
		},
		{
			resType: "AllergyIntolerance",
			id:      "all-dyn-1",
			rawJSON: `{
				"resourceType": "AllergyIntolerance",
				"id": "all-dyn-1",
				"clinicalStatus": {"coding": [{"code": "active"}]},
				"verificationStatus": {"coding": [{"code": "confirmed"}]},
				"criticality": "high",
				"code": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980"}]},
				"patient": {"reference": "Patient/pat-dyn-1"},
				"recordedDate": "2025-03-01",
				"reaction": [{"manifestation": [{"coding": [{"code": "247472004"}]}], "severity": "severe"}]
			}`,
		},
		{
			resType: "DiagnosticReport",
			id:      "dr-dyn-1",
			rawJSON: `{
				"resourceType": "DiagnosticReport",
				"id": "dr-dyn-1",
				"status": "final",
				"category": [{"coding": [{"code": "LAB"}]}],
				"code": {"coding": [{"system": "http://loinc.org", "code": "24323-8"}]},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"encounter": {"reference": "Encounter/enc-1"},
				"effectiveDateTime": "2026-01-12T08:00:00Z",
				"issued": "2026-01-12T10:00:00Z",
				"performer": [{"reference": "Practitioner/prac-1"}],
				"result": [{"reference": "Observation/obs-dyn-1"}]
			}`,
		},
		{
			resType: "Procedure",
			id:      "proc-dyn-1",
			rawJSON: `{
				"resourceType": "Procedure",
				"id": "proc-dyn-1",
				"status": "completed",
				"code": {"coding": [{"system": "http://snomed.info/sct", "code": "80146002"}]},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"encounter": {"reference": "Encounter/enc-1"},
				"performedDateTime": "2025-11-20",
				"performer": [{"actor": {"reference": "Practitioner/prac-1"}}]
			}`,
		},
		{
			resType: "Immunization",
			id:      "imm-dyn-1",
			rawJSON: `{
				"resourceType": "Immunization",
				"id": "imm-dyn-1",
				"status": "completed",
				"vaccineCode": {"coding": [{"system": "http://hl7.org/fhir/sid/cvx", "code": "208"}]},
				"patient": {"reference": "Patient/pat-dyn-1"},
				"occurrenceDateTime": "2025-10-01",
				"lotNumber": "LOT-42"
			}`,
		},
		{
			resType: "CarePlan",
			id:      "cp-dyn-1",
			rawJSON: `{
				"resourceType": "CarePlan",
				"id": "cp-dyn-1",
				"status": "active",
				"intent": "plan",
				"category": [{"coding": [{"code": "assess-plan"}]}],
				"subject": {"reference": "Patient/pat-dyn-1"},
				"period": {"start": "2026-01-01"}
			}`,
		},
		{
			resType: "CareTeam",
			id:      "ct-dyn-1",
			rawJSON: `{
				"resourceType": "CareTeam",
				"id": "ct-dyn-1",
				"status": "active",
				"subject": {"reference": "Patient/pat-dyn-1"},
				"participant": [{"role": [{"coding": [{"code": "primary"}]}], "member": {"reference": "Practitioner/prac-1"}}]
			}`,
		},
		{
			resType: "ServiceRequest",
			id:      "sr-dyn-1",
			rawJSON: `{
				"resourceType": "ServiceRequest",
				"id": "sr-dyn-1",
				"status": "active",
				"intent": "order",
				"category": [{"coding": [{"code": "386053000"}]}],
				"code": {"coding": [{"code": "24627-2"}]},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"authoredOn": "2026-01-15",
				"requester": {"reference": "Practitioner/prac-1"}
			}`,
		},
		{
			resType: "DocumentReference",
			id:      "doc-dyn-1",
			rawJSON: `{
				"resourceType": "DocumentReference",
				"id": "doc-dyn-1",
				"status": "current",
				"docStatus": "final",
				"type": {"coding": [{"system": "http://loinc.org", "code": "34133-9"}]},
				"category": [{"coding": [{"code": "clinical-note"}]}],
				"subject": {"reference": "Patient/pat-dyn-1"},
				"date": "2026-01-15T10:00:00Z",
				"author": [{"reference": "Practitioner/prac-1"}],
				"custodian": {"reference": "Organization/org-1"},
				"context": {"encounter": [{"reference": "Encounter/enc-1"}], "period": {"start": "2026-01-15"}}
			}`,
		},
		{
			resType: "Provenance",
			id:      "prov-dyn-1",
			rawJSON: `{
				"resourceType": "Provenance",
				"id": "prov-dyn-1",
				"target": [{"reference": "Patient/pat-dyn-1"}],
				"recorded": "2026-01-15T10:00:00Z",
				"agent": [{"type": {"coding": [{"code": "author"}]}, "who": {"reference": "Practitioner/prac-1"}}]
			}`,
		},
		{
			resType: "PractitionerRole",
			id:      "pr-dyn-1",
			rawJSON: `{
				"resourceType": "PractitionerRole",
				"id": "pr-dyn-1",
				"active": true,
				"practitioner": {"reference": "Practitioner/prac-1"},
				"organization": {"reference": "Organization/org-1"},
				"code": [{"coding": [{"code": "doctor"}]}],
				"specialty": [{"coding": [{"code": "207R00000X"}]}],
				"location": [{"reference": "Location/loc-1"}],
				"endpoint": [{"reference": "Endpoint/ep-1"}]
			}`,
		},
		{
			resType: "Coverage",
			id:      "cov-dyn-1",
			rawJSON: `{
				"resourceType": "Coverage",
				"id": "cov-dyn-1",
				"status": "active",
				"type": {"coding": [{"code": "EHCPOL"}]},
				"subscriber": {"reference": "Patient/pat-dyn-1"},
				"subscriberId": "SUB-999",
				"beneficiary": {"reference": "Patient/pat-dyn-1", "identifier": {"system": "http://mrn", "value": "MRN-777"}},
				"payor": [{"reference": "Organization/org-1"}],
				"period": {"start": "2025-01-01", "end": "2026-12-31"}
			}`,
		},
		{
			resType: "ConceptMap",
			id:      "cm-dyn-1",
			rawJSON: `{
				"resourceType": "ConceptMap",
				"id": "cm-dyn-1",
				"url": "http://example.org/ConceptMap/cm-dyn-1",
				"status": "active",
				"sourceUri": "http://example.org/vs-src",
				"targetUri": "http://example.org/vs-dst"
			}`,
		},
		{
			resType: "QuestionnaireResponse",
			id:      "qr-dyn-1",
			rawJSON: `{
				"resourceType": "QuestionnaireResponse",
				"id": "qr-dyn-1",
				"questionnaire": "http://example.org/Questionnaire/q-1",
				"status": "completed",
				"subject": {"reference": "Patient/pat-dyn-1"},
				"author": {"reference": "Practitioner/prac-1"},
				"source": {"reference": "Patient/pat-dyn-1"},
				"authored": "2026-01-15T10:00:00Z"
			}`,
		},
		{
			resType: "Goal",
			id:      "goal-dyn-1",
			rawJSON: `{
				"resourceType": "Goal",
				"id": "goal-dyn-1",
				"lifecycleStatus": "active",
				"description": {"coding": [{"system": "http://snomed.info/sct", "code": "289169006", "display": "Weight loss"}], "text": "Lose weight"},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"startDate": "2026-01-01",
				"target": [{"dueDate": "2026-06-01"}]
			}`,
		},
		{
			resType: "Group",
			id:      "grp-dyn-1",
			rawJSON: `{
				"resourceType": "Group",
				"id": "grp-dyn-1",
				"type": "person",
				"actual": true,
				"member": [{"entity": {"reference": "Patient/pat-dyn-1"}}]
			}`,
		},
		{
			resType: "List",
			id:      "list-dyn-1",
			rawJSON: `{
				"resourceType": "List",
				"id": "list-dyn-1",
				"status": "current",
				"mode": "working",
				"title": "Active Cohort",
				"subject": {"reference": "Patient/pat-dyn-1"},
				"entry": [{"item": {"reference": "Condition/cond-dyn-1"}}]
			}`,
		},
		{
			resType: "Device",
			id:      "dev-dyn-1",
			rawJSON: `{
				"resourceType": "Device",
				"id": "dev-dyn-1",
				"status": "active",
				"udiCarrier": [{"deviceIdentifier": "UDI-12345", "carrierHRF": "(01)UDI-12345"}],
				"deviceName": [{"name": "Pacemaker", "type": "user-friendly-name"}],
				"type": {"coding": [{"code": "14106009"}]},
				"patient": {"reference": "Patient/pat-dyn-1"}
			}`,
		},
		{
			resType: "MedicationDispense",
			id:      "md-dyn-1",
			rawJSON: `{
				"resourceType": "MedicationDispense",
				"id": "md-dyn-1",
				"status": "completed",
				"medicationCodeableConcept": {"coding": [{"code": "723"}]},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"performer": [{"actor": {"reference": "Practitioner/prac-1"}}],
				"whenPrepared": "2026-01-01T09:00:00Z",
				"whenHandedOver": "2026-01-01T10:00:00Z"
			}`,
		},
		{
			resType: "AuditEvent",
			id:      "ae-dyn-1",
			rawJSON: `{
				"resourceType": "AuditEvent",
				"id": "ae-dyn-1",
				"type": {"system": "http://dicom.nema.org/resources/ontology/DCM", "code": "110100"},
				"subtype": [{"code": "read"}],
				"action": "R",
				"recorded": "2026-01-15T10:00:00Z",
				"outcome": "0",
				"agent": [{"who": {"reference": "Practitioner/prac-1"}, "requestor": true}],
				"source": {"observer": {"reference": "Device/dev-dyn-1"}},
				"entity": [{"what": {"reference": "Patient/pat-dyn-1"}}]
			}`,
		},
	}

	for _, tr := range testResources {
		md := descs[tr.resType]
		require.NotNil(t, md)
		norm, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, []byte(tr.rawJSON))
		require.NoError(t, err, "NormalizeFHIRJSONToProtoJSON(%s)", tr.resType)
		msg := dynamicpb.NewMessage(md)
		require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(norm, msg), "Unmarshal(%s)", tr.resType)
		pBytes, err := proto.Marshal(msg)
		require.NoError(t, err)

		dynExt := NewDynamicResourceExtractor(tr.resType, md)
		idx, err := dynExt("t1", "", pBytes)
		require.NoError(t, err)
		assert.NotNil(t, idx)

		if staticExt, ok := staticExtractors[tr.resType]; ok {
			sIdx, err := staticExt("t1", tr.id, pBytes)
			require.NoError(t, err)
			assert.NotNil(t, sIdx)
		}
	}

	// Query & _filter validation edge cases
	validFilters := []string{
		`name eq "smith" or (gender eq 'female' and not active eq "false")`,
		`name co "sm\"ith"`,
		`deceased pr true`,
		`deceased pr false`,
		`birthdate ge 1980-01-01`,
		`value-quantity gt 5.5`,
		`subject.name eq "smith"`,
	}
	for _, vf := range validFilters {
		_, err := ParseFilterExpr(vf)
		require.NoError(t, err, "filter: %s", vf)
	}

	invalidFilters := []string{
		``,
		`   `,
		`name eq "unterminated`,
		`(name eq "smith"`,
		`name`,
		`"quotedparam" eq "x"`,
		`unknown_param_xyz eq "x"`,
		`name invalid_op "x"`,
		`name eq ""`,
		`name eq (`,
		`deceased pr maybe`,
		`birthdate eq not-a-date`,
		`value-quantity gt not-a-qty`,
		`name eq "smith" extra_token`,
	}
	for _, inf := range invalidFilters {
		_, err := ParseFilterExpr(inf)
		require.Error(t, err, "expected error for filter: %s", inf)
	}

	validQ, _ := url.ParseQuery("_count=10&_offset=0&_list=list1&_include=Observation:patient&_include:iterate=Patient:organization&_revinclude=Provenance:target&_revinclude:iterate=Observation:subject&_sort=-_lastUpdated,name&_summary=true&_total=accurate&_lastUpdated=ge2020-01-01&_filter=name+eq+%22smith%22&_content=heart&_text=sinus&_has:Observation:patient:code=8867-4&deceased:missing=false&identifier:of-type=http://sys|MR|123&subject.birthdate=2000-01-01&subject.value-quantity=gt5&code-value-quantity=http://loinc.org|8867-4$gt60&code-value-concept=http://loinc.org|1$http://snomed.info/sct|2&code-value-date=http://loinc.org|1$2020-01-01&code-value-string=http://loinc.org|1$abc&url:below=http://example.org&birthdate=2020-01-01&value-quantity=70||kg&_type=Patient,Observation&_elements=id,name&_id=Patient/p1,p2")
	require.NoError(t, ValidateSearchQuery(validQ))
	built := BuildSearchParamsFromQuery("t1", "Observation", validQ)
	assert.True(t, HasSearchCriteria(built))
}
