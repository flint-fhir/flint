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

// DefaultSchemas returns built-in StructureDefinition rules for Core Big 5 resources.
func DefaultSchemas() map[string]ResourceSchema {
	return map[string]ResourceSchema{
		"Patient": {
			ResourceType: "Patient",
			AllowedFields: buildFieldMap(commonResourceFields, []string{
				"identifier", "active", "name", "telecom", "gender",
				"birthDate", "deceasedBoolean", "deceasedDateTime", "address",
				"maritalStatus", "multipleBirthBoolean", "multipleBirthInteger",
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
				"subject", "focus", "encounter", "effectiveDateTime", "effectivePeriod",
				"effectiveTiming", "effectiveInstant", "issued", "performer",
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
				"severity", "code", "bodySite", "subject", "encounter",
				"onsetDateTime", "onsetAge", "onsetPeriod", "onsetRange", "onsetString",
				"abatementDateTime", "abatementAge", "abatementPeriod", "abatementRange",
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
				"identifier", "status", "statusHistory", "class", "classHistory",
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
	}
}
