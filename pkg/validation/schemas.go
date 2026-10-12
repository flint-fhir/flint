package validation

import (
	"regexp"
)

// Standard FHIR R4 Primitive Format Regexes
var (
	// FHIR id regex: [A-Za-z0-9\-\.]{1,64}
	IDRegex = regexp.MustCompile(`^[A-Za-z0-9\-\.]{1,64}$`)

	// FHIR date regex: YYYY, YYYY-MM, or YYYY-MM-DD
	DateRegex = regexp.MustCompile(`^[0-9]{4}(-(0[1-9]|1[0-2])(-(0[1-9]|[12][0-9]|3[01]))?)?$`)

	// FHIR dateTime regex: YYYY, YYYY-MM, YYYY-MM-DD or full timestamp with timezone
	DateTimeRegex = regexp.MustCompile(`^[0-9]{4}(-(0[1-9]|1[0-2])(-(0[1-9]|[12][0-9]|3[01])(T([01][0-9]|2[0-3]):[0-5][0-9]:([0-5][0-9]|60)(\.[0-9]+)?(Z|(\+|\-)([01][0-9]|2[0-3]):[0-5][0-9]))?)?)?$`)

	// FHIR instant regex: full date + time + timezone required
	InstantRegex = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:([0-5][0-9]|60)(\.[0-9]+)?(Z|(\+|\-)([01][0-9]|2[0-3]):[0-5][0-9])$`)
)

// Standard Required ValueSets in FHIR R4
var (
	AdministrativeGenderValueSet = map[string]bool{
		"male": true, "female": true, "other": true, "unknown": true,
	}

	ObservationStatusValueSet = map[string]bool{
		"registered": true, "preliminary": true, "final": true,
		"amended": true, "corrected": true, "cancelled": true,
		"entered-in-error": true, "unknown": true,
	}

	ConditionClinicalStatusValueSet = map[string]bool{
		"active": true, "recurrence": true, "relapse": true,
		"inactive": true, "remission": true, "resolved": true,
	}

	ConditionVerificationStatusValueSet = map[string]bool{
		"unconfirmed": true, "provisional": true, "differential": true,
		"confirmed": true, "refuted": true, "entered-in-error": true,
	}

	EncounterStatusValueSet = map[string]bool{
		"planned": true, "arrived": true, "triaged": true,
		"in-progress": true, "onleave": true, "finished": true,
		"cancelled": true, "entered-in-error": true, "unknown": true,
	}

	MedicationRequestStatusValueSet = map[string]bool{
		"active": true, "on-hold": true, "cancelled": true, "completed": true,
		"entered-in-error": true, "stopped": true, "draft": true, "unknown": true,
	}

	MedicationRequestIntentValueSet = map[string]bool{
		"proposal": true, "plan": true, "order": true, "original-order": true,
		"reflex-order": true, "filler-order": true, "instance-order": true, "option": true,
	}

	RequestPriorityValueSet = map[string]bool{
		"routine": true, "urgent": true, "asap": true, "stat": true,
	}

	AllergyIntoleranceClinicalStatusValueSet = map[string]bool{
		"active": true, "inactive": true, "resolved": true,
	}

	AllergyIntoleranceVerificationStatusValueSet = map[string]bool{
		"unconfirmed": true, "confirmed": true, "refuted": true, "entered-in-error": true,
	}

	AllergyIntoleranceTypeValueSet = map[string]bool{
		"allergy": true, "intolerance": true,
	}

	AllergyIntoleranceCriticalityValueSet = map[string]bool{
		"low": true, "high": true, "unable-to-assess": true,
	}

	ProcedureStatusValueSet = map[string]bool{
		"preparation": true, "in-progress": true, "not-done": true, "on-hold": true,
		"stopped": true, "completed": true, "entered-in-error": true, "unknown": true,
	}

	DiagnosticReportStatusValueSet = map[string]bool{
		"registered": true, "partial": true, "preliminary": true, "final": true,
		"amended": true, "corrected": true, "appended": true, "cancelled": true,
		"entered-in-error": true, "unknown": true,
	}

	ImmunizationStatusValueSet = map[string]bool{
		"completed": true, "entered-in-error": true, "not-done": true,
	}

	SpecimenStatusValueSet = map[string]bool{
		"available": true, "unavailable": true, "unsatisfactory": true, "entered-in-error": true,
	}

	LocationStatusValueSet = map[string]bool{
		"active": true, "suspended": true, "inactive": true,
	}

	LocationModeValueSet = map[string]bool{
		"instance": true, "kind": true,
	}
)

// ResourceSchema represents validation rules derived from a FHIR StructureDefinition.
type ResourceSchema struct {
	ResourceType   string
	AllowedFields  map[string]bool
	RequiredFields []string
	CodeBindings   map[string]map[string]bool // field -> valid code set
	ValueSetURIs   map[string]string          // field -> ValueSet canonical URI
}

var commonResourceFields = []string{
	"resourceType", "id", "meta", "implicitRules", "language",
	"text", "contained", "extension", "modifierExtension",
}

func buildFieldMap(fields ...[]string) map[string]bool {
	m := make(map[string]bool)
	for _, fList := range fields {
		for _, f := range fList {
			m[f] = true
		}
	}
	return m
}

// DefaultSchemas returns built-in StructureDefinition rules for US Core / USCDI v3 FHIR R4 resources.
func DefaultSchemas() map[string]ResourceSchema {
	return map[string]ResourceSchema{
		"Patient": {
			ResourceType: "Patient",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "active", "name", "telecom", "gender",
				"birthDate", "deceased", "deceasedBoolean", "deceasedDateTime", "address",
				"maritalStatus", "multipleBirth", "multipleBirthBoolean", "multipleBirthInteger",
				"photo", "contact", "communication", "generalPractitioner",
				"managingOrganization", "link",
			}),
			RequiredFields: []string{}, // Patient has no mandatory 1..1 fields in base FHIR R4
			CodeBindings: map[string]map[string]bool{
				"gender": AdministrativeGenderValueSet,
			},
			ValueSetURIs: map[string]string{
				"gender": "http://hl7.org/fhir/valueset/administrative-gender",
			},
		},

		"Observation": {
			ResourceType: "Observation",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "basedOn", "partOf", "status", "category", "code",
				"subject", "focus", "encounter", "effective", "effectiveDateTime", "effectivePeriod",
				"effectiveTiming", "effectiveInstant", "issued", "performer", "value",
				"valueQuantity", "valueCodeableConcept", "valueString", "valueBoolean",
				"valueInteger", "valueRange", "valueRatio", "valueSampledData",
				"valueTime", "valueDateTime", "valuePeriod", "dataAbsentReason",
				"interpretation", "note", "bodySite", "method", "specimen",
				"device", "referenceRange", "hasMember", "derivedFrom", "component",
			}),
			RequiredFields: []string{"status", "code"},
			CodeBindings: map[string]map[string]bool{
				"status": ObservationStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/observation-status",
			},
		},

		"Condition": {
			ResourceType: "Condition",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "clinicalStatus", "verificationStatus", "category",
				"severity", "code", "bodySite", "subject", "encounter", "onset",
				"onsetDateTime", "onsetAge", "onsetPeriod", "onsetRange", "onsetString",
				"abatement", "abatementDateTime", "abatementAge", "abatementPeriod", "abatementRange",
				"abatementString", "recordedDate", "recorder", "asserter", "stage",
				"evidence", "note",
			}),
			RequiredFields: []string{"subject"},
			CodeBindings: map[string]map[string]bool{
				"clinicalStatus":     ConditionClinicalStatusValueSet,
				"verificationStatus": ConditionVerificationStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"clinicalStatus":     "http://hl7.org/fhir/valueset/condition-clinical",
				"verificationStatus": "http://hl7.org/fhir/valueset/condition-ver-status",
			},
		},

		"Encounter": {
			ResourceType: "Encounter",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "status", "statusHistory", "class", "classValue", "classHistory",
				"type", "serviceType", "priority", "subject", "episodeOfCare",
				"basedOn", "participant", "appointment", "period", "length",
				"reasonCode", "reasonReference", "diagnosis", "account",
				"hospitalization", "location", "serviceProvider", "partOf",
			}),
			RequiredFields: []string{"status", "class"},
			CodeBindings: map[string]map[string]bool{
				"status": EncounterStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/encounter-status",
			},
		},

		"Practitioner": {
			ResourceType: "Practitioner",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "active", "name", "telecom", "address", "gender",
				"birthDate", "photo", "qualification", "communication",
			}),
			RequiredFields: []string{},
			CodeBindings: map[string]map[string]bool{
				"gender": AdministrativeGenderValueSet,
			},
			ValueSetURIs: map[string]string{
				"gender": "http://hl7.org/fhir/valueset/administrative-gender",
			},
		},

		"MedicationRequest": {
			ResourceType: "MedicationRequest",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "status", "statusReason", "intent", "category", "priority",
				"doNotPerform", "reported", "reportedBoolean", "reportedReference",
				"medication", "medicationCodeableConcept", "medicationReference",
				"subject", "encounter", "supportingInformation", "authoredOn", "requester",
				"performer", "performerType", "recorder", "reasonCode", "reasonReference",
				"instantiatesCanonical", "instantiatesUri", "basedOn", "groupIdentifier",
				"courseOfTherapyType", "insurance", "note", "dosageInstruction",
				"dispenseRequest", "substitution", "priorPrescription", "detectedIssue",
				"eventHistory",
			}),
			RequiredFields: []string{"status", "intent", "medication", "subject"},
			CodeBindings: map[string]map[string]bool{
				"status":   MedicationRequestStatusValueSet,
				"intent":   MedicationRequestIntentValueSet,
				"priority": RequestPriorityValueSet,
			},
			ValueSetURIs: map[string]string{
				"status":   "http://hl7.org/fhir/valueset/medicationrequest-status",
				"intent":   "http://hl7.org/fhir/valueset/medicationrequest-intent",
				"priority": "http://hl7.org/fhir/valueset/request-priority",
			},
		},

		"AllergyIntolerance": {
			ResourceType: "AllergyIntolerance",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "clinicalStatus", "verificationStatus", "type", "category",
				"criticality", "code", "patient", "encounter", "onset",
				"onsetDateTime", "onsetAge", "onsetPeriod", "onsetRange", "onsetString",
				"recordedDate", "recorder", "asserter", "lastOccurrence", "note", "reaction",
			}),
			RequiredFields: []string{"patient"},
			CodeBindings: map[string]map[string]bool{
				"clinicalStatus":     AllergyIntoleranceClinicalStatusValueSet,
				"verificationStatus": AllergyIntoleranceVerificationStatusValueSet,
				"type":               AllergyIntoleranceTypeValueSet,
				"criticality":        AllergyIntoleranceCriticalityValueSet,
			},
			ValueSetURIs: map[string]string{
				"clinicalStatus":     "http://hl7.org/fhir/valueset/allergyintolerance-clinical",
				"verificationStatus": "http://hl7.org/fhir/valueset/allergyintolerance-verification",
				"type":               "http://hl7.org/fhir/valueset/allergy-intolerance-type",
				"criticality":        "http://hl7.org/fhir/valueset/allergy-intolerance-criticality",
			},
		},

		"Procedure": {
			ResourceType: "Procedure",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "instantiatesCanonical", "instantiatesUri", "basedOn", "partOf",
				"status", "statusReason", "category", "code", "subject", "encounter",
				"performed", "performedDateTime", "performedPeriod", "performedString",
				"performedAge", "performedRange", "recorder", "asserter", "performer",
				"location", "reasonCode", "reasonReference", "bodySite", "outcome",
				"report", "complication", "complicationDetail", "followUp", "note",
				"focalDevice", "usedReference", "usedCode",
			}),
			RequiredFields: []string{"status", "subject"},
			CodeBindings: map[string]map[string]bool{
				"status": ProcedureStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/event-status",
			},
		},

		"DiagnosticReport": {
			ResourceType: "DiagnosticReport",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "basedOn", "status", "category", "code", "subject",
				"encounter", "effective", "effectiveDateTime", "effectivePeriod",
				"issued", "performer", "resultsInterpreter", "specimen", "result",
				"imagingStudy", "media", "conclusion", "conclusionCode", "presentedForm",
			}),
			RequiredFields: []string{"status", "code"},
			CodeBindings: map[string]map[string]bool{
				"status": DiagnosticReportStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/diagnostic-report-status",
			},
		},

		"Immunization": {
			ResourceType: "Immunization",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "status", "statusReason", "vaccineCode", "patient",
				"encounter", "occurrence", "occurrenceDateTime", "occurrenceString",
				"recorded", "primarySource", "reportOrigin", "location", "manufacturer",
				"lotNumber", "expirationDate", "site", "route", "doseQuantity",
				"performer", "note", "reasonCode", "reasonReference", "isSubpotent",
				"subpotentReason", "education", "programEligibility", "fundingSource",
				"reaction", "protocolApplied",
			}),
			RequiredFields: []string{"status", "vaccineCode", "patient", "occurrence"},
			CodeBindings: map[string]map[string]bool{
				"status": ImmunizationStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/immunization-status",
			},
		},

		"Organization": {
			ResourceType: "Organization",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "active", "type", "name", "alias", "telecom",
				"address", "partOf", "contact", "endpoint",
			}),
			RequiredFields: []string{},
			CodeBindings:   map[string]map[string]bool{},
			ValueSetURIs:   map[string]string{},
		},

		"Specimen": {
			ResourceType: "Specimen",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "accessionIdentifier", "status", "type", "subject",
				"receivedTime", "parent", "request", "collection", "processing",
				"container", "condition", "note",
			}),
			RequiredFields: []string{"type", "subject"},
			CodeBindings: map[string]map[string]bool{
				"status": SpecimenStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/specimen-status",
			},
		},

		"Location": {
			ResourceType: "Location",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "status", "operationalStatus", "name", "alias",
				"description", "mode", "type", "telecom", "address", "physicalType",
				"position", "managingOrganization", "partOf", "hoursOfOperation",
				"availabilityExceptions", "endpoint",
			}),
			RequiredFields: []string{"name"},
			CodeBindings: map[string]map[string]bool{
				"status": LocationStatusValueSet,
				"mode":   LocationModeValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/location-status",
				"mode":   "http://hl7.org/fhir/valueset/location-mode",
			},
		},
	}
}
