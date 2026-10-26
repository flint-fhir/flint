package validation

import (
	"regexp"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/flint-fhir/flint/pkg/fhirutil"
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

	DocumentReferenceStatusValueSet = map[string]bool{
		"current": true, "superseded": true, "entered-in-error": true,
	}

	CarePlanStatusValueSet = map[string]bool{
		"draft": true, "active": true, "on-hold": true, "revoked": true,
		"completed": true, "entered-in-error": true, "unknown": true,
	}

	CarePlanIntentValueSet = map[string]bool{
		"proposal": true, "plan": true, "order": true, "option": true,
	}

	CareTeamStatusValueSet = map[string]bool{
		"proposed": true, "active": true, "suspended": true, "inactive": true, "entered-in-error": true,
	}

	GoalLifecycleStatusValueSet = map[string]bool{
		"proposed": true, "planned": true, "accepted": true, "active": true,
		"on-hold": true, "completed": true, "cancelled": true, "entered-in-error": true, "rejected": true,
	}

	ServiceRequestStatusValueSet = map[string]bool{
		"draft": true, "active": true, "on-hold": true, "revoked": true,
		"completed": true, "entered-in-error": true, "unknown": true,
	}

	ServiceRequestIntentValueSet = map[string]bool{
		"proposal": true, "plan": true, "directive": true, "order": true,
		"original-order": true, "reflex-order": true, "filler-order": true, "instance-order": true, "option": true,
	}

	CoverageStatusValueSet = map[string]bool{
		"active": true, "cancelled": true, "draft": true, "entered-in-error": true,
	}

	QuestionnaireResponseStatusValueSet = map[string]bool{
		"in-progress": true, "completed": true, "amended": true, "entered-in-error": true, "stopped": true,
	}

	MedicationDispenseStatusValueSet = map[string]bool{
		"preparation": true, "in-progress": true, "cancelled": true, "on-hold": true,
		"completed": true, "entered-in-error": true, "stopped": true, "declined": true, "unknown": true,
	}

	MedicationStatusValueSet = map[string]bool{
		"active": true, "inactive": true, "entered-in-error": true,
	}

	DeviceStatusValueSet = map[string]bool{
		"active": true, "inactive": true, "entered-in-error": true, "unknown": true,
	}
)

// ResourceSchema represents validation rules derived from a FHIR StructureDefinition.
type ResourceSchema struct {
	ResourceType     string
	Descriptor       protoreflect.MessageDescriptor
	AllowedFields    map[string]bool
	RequiredFields   []string
	CodeBindings     map[string]map[string]bool // field -> valid code set
	ValueSetURIs     map[string]string          // field -> ValueSet canonical URI
	DateFields       map[string]bool            // primitive Date fields derived from proto descriptor
	DateTimeFields   map[string]bool            // primitive DateTime fields derived from proto descriptor
	InstantFields    map[string]bool            // primitive Instant fields derived from proto descriptor
	ChildDescriptors map[string]protoreflect.MessageDescriptor
}

var commonResourceFields = []string{
	"resourceType", "id", "meta", "implicitRules", "language",
	"text", "contained", "extension", "modifierExtension",
}

var commonElementFields = []string{
	"id", "extension", "modifierExtension", "fhir_comments", "_value",
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

type protoSchemaDetails struct {
	allowedFields    map[string]bool
	codeBindings     map[string]map[string]bool
	valueSetURIs     map[string]string
	dateFields       map[string]bool
	dateTimeFields   map[string]bool
	instantFields    map[string]bool
	childDescriptors map[string]protoreflect.MessageDescriptor
}

func isPrimitiveWrapperDescriptor(md protoreflect.MessageDescriptor) bool {
	if md == nil {
		return false
	}
	if md.Fields().ByName("value_us") != nil {
		return true
	}
	if valFD := md.Fields().ByName("value"); valFD != nil && valFD.Kind() != protoreflect.MessageKind {
		return true
	}
	return false
}

func deriveProtoSchemaDetails(md protoreflect.MessageDescriptor) protoSchemaDetails {
	det := protoSchemaDetails{
		allowedFields:    buildFieldMap(commonResourceFields, commonElementFields),
		codeBindings:     make(map[string]map[string]bool),
		valueSetURIs:     make(map[string]string),
		dateFields:       make(map[string]bool),
		dateTimeFields:   make(map[string]bool),
		instantFields:    make(map[string]bool),
		childDescriptors: make(map[string]protoreflect.MessageDescriptor),
	}
	if md == nil {
		return det
	}
	resTypeLower := strings.ToLower(string(md.Name()))
	oneofs := md.Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		od := oneofs.Get(i)
		det.allowedFields[string(od.Name())] = true
	}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		jsonName := fd.JSONName()
		det.allowedFields[jsonName] = true
		if jsonName == "classValue" {
			det.allowedFields["class"] = true
		}
		if jsonName == "stringValue" {
			det.allowedFields["string"] = true
		}
		if fd.Kind() != protoreflect.MessageKind || fd.Message() == nil {
			continue
		}
		fmd := fd.Message()
		if fmd.Oneofs().ByName("choice") != nil {
			choiceFields := fmd.Fields()
			for j := 0; j < choiceFields.Len(); j++ {
				cfd := choiceFields.Get(j)
				optJSON := cfd.JSONName()
				if optJSON == "stringValue" {
					optJSON = "string"
				}
				if len(optJSON) == 0 {
					continue
				}
				choiceFieldName := jsonName + strings.ToUpper(optJSON[:1]) + optJSON[1:]
				det.allowedFields[choiceFieldName] = true
				if cfd.Kind() == protoreflect.MessageKind && cfd.Message() != nil {
					cfmd := cfd.Message()
					switch string(cfmd.FullName()) {
					case "google.fhir.r4.core.Date":
						det.dateFields[choiceFieldName] = true
					case "google.fhir.r4.core.DateTime":
						det.dateTimeFields[choiceFieldName] = true
					case "google.fhir.r4.core.Instant":
						det.instantFields[choiceFieldName] = true
					default:
						if !isPrimitiveWrapperDescriptor(cfmd) {
							det.childDescriptors[choiceFieldName] = cfmd
						}
					}
				}
			}
			continue
		}
		if !isPrimitiveWrapperDescriptor(fmd) {
			det.childDescriptors[jsonName] = fmd
			if jsonName == "classValue" {
				det.childDescriptors["class"] = fmd
			}
		}
		if !fd.IsList() {
			switch string(fmd.FullName()) {
			case "google.fhir.r4.core.Date":
				det.dateFields[jsonName] = true
			case "google.fhir.r4.core.DateTime":
				det.dateTimeFields[jsonName] = true
			case "google.fhir.r4.core.Instant":
				if jsonName == "date" {
					det.dateTimeFields[jsonName] = true
				} else {
					det.instantFields[jsonName] = true
				}
			}
			if valFD := fmd.Fields().ByName("value"); valFD != nil && valFD.Kind() == protoreflect.EnumKind && valFD.Enum() != nil {
				enumVals := valFD.Enum().Values()
				codes := make(map[string]bool, enumVals.Len())
				for k := 0; k < enumVals.Len(); k++ {
					ev := enumVals.Get(k)
					if ev.Number() == 0 || string(ev.Name()) == "INVALID_UNINITIALIZED" {
						continue
					}
					rawName := strings.ToLower(string(ev.Name()))
					kebab := strings.ReplaceAll(rawName, "_", "-")
					codes[kebab] = true
					codes[rawName] = true
				}
				if len(codes) > 0 {
					det.codeBindings[jsonName] = codes
					det.valueSetURIs[jsonName] = "http://hl7.org/fhir/valueset/" + resTypeLower + "-" + strings.ToLower(jsonName)
				}
			}
		}
	}
	return det
}

// DefaultSchemas returns StructureDefinition validation schemas for all ~146 official
// HL7 FHIR R4 resource types (derived from compiled protobuf descriptors) overlaid with
// US Core v6.1.0 & v7.0.0 required element and ValueSet bindings across all 26 US Core types.
func DefaultSchemas() map[string]ResourceSchema {
	usCoreOverrides := map[string]ResourceSchema{
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

		"DocumentReference": {
			ResourceType:   "DocumentReference",
			RequiredFields: []string{"status", "type", "subject", "content"},
			CodeBindings: map[string]map[string]bool{
				"status": DocumentReferenceStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/document-reference-status",
			},
		},

		"Provenance": {
			ResourceType:   "Provenance",
			RequiredFields: []string{"target", "recorded", "agent"},
			CodeBindings:   map[string]map[string]bool{},
			ValueSetURIs:   map[string]string{},
		},

		"CarePlan": {
			ResourceType:   "CarePlan",
			RequiredFields: []string{"status", "intent", "subject"},
			CodeBindings: map[string]map[string]bool{
				"status": CarePlanStatusValueSet,
				"intent": CarePlanIntentValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/request-status",
				"intent": "http://hl7.org/fhir/valueset/care-plan-intent",
			},
		},

		"CareTeam": {
			ResourceType:   "CareTeam",
			RequiredFields: []string{"subject"},
			CodeBindings: map[string]map[string]bool{
				"status": CareTeamStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/care-team-status",
			},
		},

		"Goal": {
			ResourceType:   "Goal",
			RequiredFields: []string{"lifecycleStatus", "description", "subject"},
			CodeBindings: map[string]map[string]bool{
				"lifecycleStatus": GoalLifecycleStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"lifecycleStatus": "http://hl7.org/fhir/valueset/goal-status",
			},
		},

		"ServiceRequest": {
			ResourceType:   "ServiceRequest",
			RequiredFields: []string{"status", "intent", "code", "subject"},
			CodeBindings: map[string]map[string]bool{
				"status":   ServiceRequestStatusValueSet,
				"intent":   ServiceRequestIntentValueSet,
				"priority": RequestPriorityValueSet,
			},
			ValueSetURIs: map[string]string{
				"status":   "http://hl7.org/fhir/valueset/request-status",
				"intent":   "http://hl7.org/fhir/valueset/request-intent",
				"priority": "http://hl7.org/fhir/valueset/request-priority",
			},
		},

		"Coverage": {
			ResourceType:   "Coverage",
			RequiredFields: []string{"status", "beneficiary", "payor"},
			CodeBindings: map[string]map[string]bool{
				"status": CoverageStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/fm-status",
			},
		},

		"QuestionnaireResponse": {
			ResourceType:   "QuestionnaireResponse",
			RequiredFields: []string{"status"},
			CodeBindings: map[string]map[string]bool{
				"status": QuestionnaireResponseStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/questionnaire-answers-status",
			},
		},

		"MedicationDispense": {
			ResourceType:   "MedicationDispense",
			RequiredFields: []string{"status", "medication", "subject"},
			CodeBindings: map[string]map[string]bool{
				"status": MedicationDispenseStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/medicationdispense-status",
			},
		},

		"Medication": {
			ResourceType:   "Medication",
			RequiredFields: []string{"code"},
			CodeBindings: map[string]map[string]bool{
				"status": MedicationStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/medication-status",
			},
		},

		"Device": {
			ResourceType:   "Device",
			RequiredFields: []string{"type"},
			CodeBindings: map[string]map[string]bool{
				"status": DeviceStatusValueSet,
			},
			ValueSetURIs: map[string]string{
				"status": "http://hl7.org/fhir/valueset/device-status",
			},
		},

		"PractitionerRole": {
			ResourceType:   "PractitionerRole",
			RequiredFields: []string{},
			CodeBindings:   map[string]map[string]bool{},
			ValueSetURIs:   map[string]string{},
		},

		"RelatedPerson": {
			ResourceType:   "RelatedPerson",
			RequiredFields: []string{"patient"},
			CodeBindings: map[string]map[string]bool{
				"gender": AdministrativeGenderValueSet,
			},
			ValueSetURIs: map[string]string{
				"gender": "http://hl7.org/fhir/valueset/administrative-gender",
			},
		},
	}

	allDescs := fhirutil.AllR4ResourceDescriptors()
	schemas := make(map[string]ResourceSchema, len(allDescs))
	for resType, md := range allDescs {
		det := deriveProtoSchemaDetails(md)
		if override, ok := usCoreOverrides[resType]; ok {
			for k := range override.AllowedFields {
				det.allowedFields[k] = true
			}
			for k, v := range override.CodeBindings {
				det.codeBindings[k] = v
			}
			for k, v := range override.ValueSetURIs {
				det.valueSetURIs[k] = v
			}
			override.Descriptor = md
			override.AllowedFields = det.allowedFields
			override.CodeBindings = det.codeBindings
			override.ValueSetURIs = det.valueSetURIs
			override.DateFields = det.dateFields
			override.DateTimeFields = det.dateTimeFields
			override.InstantFields = det.instantFields
			override.ChildDescriptors = det.childDescriptors
			schemas[resType] = override
		} else {
			schemas[resType] = ResourceSchema{
				ResourceType:     resType,
				Descriptor:       md,
				AllowedFields:    det.allowedFields,
				RequiredFields:   []string{},
				CodeBindings:     det.codeBindings,
				ValueSetURIs:     det.valueSetURIs,
				DateFields:       det.dateFields,
				DateTimeFields:   det.dateTimeFields,
				InstantFields:    det.instantFields,
				ChildDescriptors: det.childDescriptors,
			}
		}
	}
	for resType, override := range usCoreOverrides {
		if _, exists := schemas[resType]; !exists {
			schemas[resType] = override
		}
	}
	return schemas
}
