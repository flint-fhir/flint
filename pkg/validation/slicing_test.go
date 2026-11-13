package validation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/validation"
)

func TestStructureDefinitionPatternAndFixedEvaluation(t *testing.T) {
	eng := validation.NewEngine(validation.DefaultOptions())

	sdJSON := []byte(`{
		"resourceType": "StructureDefinition",
		"url": "http://example.org/fhir/StructureDefinition/strict-bp",
		"type": "Observation",
		"differential": {
			"element": [
				{
					"id": "Observation.status",
					"path": "Observation.status",
					"fixedCode": "final"
				},
				{
					"id": "Observation.code",
					"path": "Observation.code",
					"patternCodeableConcept": {
						"coding": [
							{
								"system": "http://loinc.org",
								"code": "85354-9"
							}
						]
					}
				},
				{
					"id": "Observation.component",
					"path": "Observation.component",
					"slicing": {
						"discriminator": [
							{"type": "pattern", "path": "code"}
						],
						"rules": "open",
						"ordered": false
					},
					"min": 2
				},
				{
					"id": "Observation.component:systolic",
					"path": "Observation.component",
					"sliceName": "systolic",
					"min": 1,
					"max": "1"
				},
				{
					"id": "Observation.component:systolic.code",
					"path": "Observation.component.code",
					"patternCodeableConcept": {
						"coding": [{"system": "http://loinc.org", "code": "8480-6"}]
					}
				},
				{
					"id": "Observation.component:systolic.value[x]",
					"path": "Observation.component.value[x]",
					"min": 1
				},
				{
					"id": "Observation.component:systolic.valueQuantity.code",
					"path": "Observation.component.valueQuantity.code",
					"fixedCode": "mm[Hg]"
				},
				{
					"id": "Observation.component:diastolic",
					"path": "Observation.component",
					"sliceName": "diastolic",
					"min": 1,
					"max": "1"
				},
				{
					"id": "Observation.component:diastolic.code",
					"path": "Observation.component.code",
					"patternCodeableConcept": {
						"coding": [{"system": "http://loinc.org", "code": "8462-4"}]
					}
				},
				{
					"id": "Observation.component:diastolic.valueQuantity.code",
					"path": "Observation.component.valueQuantity.code",
					"fixedCode": "mm[Hg]"
				}
			]
		}
	}`)
	require.NoError(t, eng.RegisterStructureDefinitionJSON(sdJSON))

	t.Run("valid resource with extra codings and extra component passes", func(t *testing.T) {
		validObs := []byte(`{
			"resourceType": "Observation",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/strict-bp"]},
			"status": "final",
			"code": {
				"coding": [
					{"system": "http://snomed.info/sct", "code": "75367002", "display": "Blood pressure"},
					{"system": "http://loinc.org", "code": "85354-9", "display": "BP panel"}
				],
				"text": "Blood Pressure"
			},
			"component": [
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic"}]},
					"valueQuantity": {"value": 120, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				},
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8462-4", "display": "Diastolic"}]},
					"valueQuantity": {"value": 80, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				},
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8478-0", "display": "Mean BP"}]},
					"valueQuantity": {"value": 93, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				}
			]
		}`)
		out := eng.ValidateJSON("Observation", validObs)
		assert.True(t, out.IsValid(), "expected 0 errors, got: %+v", out.Issues)
	})

	t.Run("missing diastolic slice and wrong unit inside systolic slice fail", func(t *testing.T) {
		badObs := []byte(`{
			"resourceType": "Observation",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/strict-bp"]},
			"status": "preliminary",
			"code": {
				"coding": [{"system": "http://loinc.org", "code": "99999-9"}]
			},
			"component": [
				{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
					"valueQuantity": {"value": 120, "unit": "cmH2O", "system": "http://unitsofmeasure.org", "code": "cm[H2O]"}
				}
			]
		}`)
		out := eng.ValidateJSON("Observation", badObs)
		assert.False(t, out.IsValid())
		assert.GreaterOrEqual(t, len(out.Issues), 4)
	})
}

func TestStructureDefinitionSlicingRulesAndDiscriminatorTypes(t *testing.T) {
	eng := validation.NewEngine(validation.DefaultOptions())

	// 1. Closed + ordered slicing with value discriminator
	closedOrderedSD := []byte(`{
		"resourceType": "StructureDefinition",
		"url": "http://example.org/fhir/StructureDefinition/closed-ordered-identifiers",
		"type": "Patient",
		"differential": {
			"element": [
				{
					"path": "Patient.identifier",
					"slicing": {
						"discriminator": [{"type": "value", "path": "system"}],
						"rules": "closed",
						"ordered": true
					}
				},
				{
					"path": "Patient.identifier",
					"sliceName": "mrn",
					"min": 1,
					"max": "1"
				},
				{
					"path": "Patient.identifier.system",
					"fixedUri": "http://hospital.example.org/mrn"
				},
				{
					"path": "Patient.identifier",
					"sliceName": "ssn",
					"min": 0,
					"max": "1"
				},
				{
					"path": "Patient.identifier.system",
					"fixedUri": "http://hl7.org/fhir/sid/us-ssn"
				}
			]
		}
	}`)
	require.NoError(t, eng.RegisterStructureDefinitionJSON(closedOrderedSD))

	t.Run("closed ordered valid", func(t *testing.T) {
		out := eng.ValidateJSON("Patient", []byte(`{
			"resourceType": "Patient",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/closed-ordered-identifiers"]},
			"identifier": [
				{"system": "http://hospital.example.org/mrn", "value": "123"},
				{"system": "http://hl7.org/fhir/sid/us-ssn", "value": "999-00-1111"}
			]
		}`))
		assert.True(t, out.IsValid(), "unexpected errors: %+v", out.Issues)
	})

	t.Run("closed rejects unmatched identifier and wrong slice order", func(t *testing.T) {
		outUnmatched := eng.ValidateJSON("Patient", []byte(`{
			"resourceType": "Patient",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/closed-ordered-identifiers"]},
			"identifier": [
				{"system": "http://hospital.example.org/mrn", "value": "123"},
				{"system": "http://other.example.org/id", "value": "456"}
			]
		}`))
		assert.False(t, outUnmatched.IsValid())

		outOutOfOrder := eng.ValidateJSON("Patient", []byte(`{
			"resourceType": "Patient",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/closed-ordered-identifiers"]},
			"identifier": [
				{"system": "http://hl7.org/fhir/sid/us-ssn", "value": "999-00-1111"},
				{"system": "http://hospital.example.org/mrn", "value": "123"}
			]
		}`))
		assert.False(t, outOutOfOrder.IsValid())
	})

	// 2. openAtEnd slicing rule
	openAtEndSD := []byte(`{
		"resourceType": "StructureDefinition",
		"url": "http://example.org/fhir/StructureDefinition/open-at-end-cat",
		"type": "Observation",
		"differential": {
			"element": [
				{
					"path": "Observation.category",
					"slicing": {
						"discriminator": [{"type": "value", "path": "coding.code"}],
						"rules": "openAtEnd"
					}
				},
				{
					"path": "Observation.category",
					"sliceName": "VSCat",
					"min": 1,
					"max": "1",
					"patternCodeableConcept": {
						"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]
					}
				}
			]
		}
	}`)
	require.NoError(t, eng.RegisterStructureDefinitionJSON(openAtEndSD))

	t.Run("openAtEnd allows unmatched after matched, rejects unmatched before matched", func(t *testing.T) {
		okOut := eng.ValidateJSON("Observation", []byte(`{
			"resourceType": "Observation",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/open-at-end-cat"]},
			"status": "final",
			"code": {"text": "HR"},
			"category": [
				{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]},
				{"coding": [{"system": "http://example.org/cat", "code": "extra"}]}
			]
		}`))
		assert.True(t, okOut.IsValid(), "unexpected errors: %+v", okOut.Issues)

		badOut := eng.ValidateJSON("Observation", []byte(`{
			"resourceType": "Observation",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/open-at-end-cat"]},
			"status": "final",
			"code": {"text": "HR"},
			"category": [
				{"coding": [{"system": "http://example.org/cat", "code": "extra"}]},
				{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}
			]
		}`))
		assert.False(t, badOut.IsValid())
	})

	// 3. Type, Profile, and Exists discriminators + fixed map equality
	typeProfileExistsSD := []byte(`{
		"resourceType": "StructureDefinition",
		"url": "http://example.org/fhir/StructureDefinition/type-profile-exists-obs",
		"type": "Observation",
		"differential": {
			"element": [
				{
					"path": "Observation.value[x]",
					"slicing": {
						"discriminator": [{"type": "type", "path": "$this"}],
						"rules": "closed"
					}
				},
				{
					"path": "Observation.value[x]",
					"sliceName": "valueQuantity",
					"min": 1,
					"type": [{"code": "Quantity"}]
				},
				{
					"path": "Observation.extension",
					"slicing": {
						"discriminator": [{"type": "profile", "path": "$this"}],
						"rules": "open"
					}
				},
				{
					"path": "Observation.extension",
					"sliceName": "methodExt",
					"min": 1,
					"type": [{"code": "Extension", "profile": ["http://example.org/fhir/StructureDefinition/ext-method"]}]
				},
				{
					"path": "Observation.identifier",
					"slicing": {
						"discriminator": [{"type": "exists", "path": "period"}],
						"rules": "open"
					}
				},
				{
					"path": "Observation.identifier",
					"sliceName": "datedId",
					"min": 1
				},
				{
					"path": "Observation.identifier.period",
					"min": 1
				},
				{
					"path": "Observation.bodySite",
					"fixedCodeableConcept": {
						"text": "Left arm"
					}
				}
			]
		}
	}`)
	require.NoError(t, eng.RegisterStructureDefinitionJSON(typeProfileExistsSD))

	t.Run("type, profile, exists discriminators and fixed complex map", func(t *testing.T) {
		okRes := eng.ValidateJSON("Observation", []byte(`{
			"resourceType": "Observation",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/type-profile-exists-obs"]},
			"status": "final",
			"code": {"text": "Test"},
			"valueQuantity": {"value": 42, "unit": "mg"},
			"extension": [{"url": "http://example.org/fhir/StructureDefinition/ext-method", "valueString": "automated"}],
			"identifier": [{"system": "urn:oid:1.2.3", "value": "A1", "period": {"start": "2025-01-01"}}],
			"bodySite": {"text": "Left arm"}
		}`))
		assert.True(t, okRes.IsValid(), "unexpected errors: %+v", okRes.Issues)

		// Wrong value[x] choice type (valueString instead of valueQuantity) + extra field in fixedCodeableConcept + missing period
		badRes := eng.ValidateJSON("Observation", []byte(`{
			"resourceType": "Observation",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/type-profile-exists-obs"]},
			"status": "final",
			"code": {"text": "Test"},
			"valueString": "not-a-quantity",
			"identifier": [{"system": "urn:oid:1.2.3", "value": "A1"}],
			"bodySite": {"text": "Left arm", "coding": [{"code": "extra-not-allowed-in-fixed"}]}
		}`))
		assert.False(t, badRes.IsValid())
		assert.GreaterOrEqual(t, len(badRes.Issues), 3)
	})
}

func TestStructureDefinitionImplicitDiscriminatorsAndEdgeCases(t *testing.T) {
	eng := validation.NewEngine(validation.DefaultOptions())
	require.Error(t, eng.RegisterStructureDefinitionJSON([]byte(`{invalid-json`)))

	// Register a target sub-profile for Bundle.entry.resource profile discriminator testing
	require.NoError(t, eng.RegisterStructureDefinitionJSON([]byte(`{
		"resourceType": "StructureDefinition",
		"url": "http://example.org/fhir/StructureDefinition/active-patient",
		"type": "Patient",
		"differential": {
			"element": [
				{"path": "Patient.active", "fixedBoolean": true},
				{"path": "Patient.multipleBirthBoolean", "patternBoolean": false},
				{"path": "Patient.deceased[x]", "max": "0"},
				{"path": "Patient.name", "max": "1"},
				{"path": "Patient.telecom", "sliceName": "phone", "min": 1, "max": "1"},
				{"path": "Patient.telecom.system", "fixedCode": "phone"},
				{"path": "Patient.extension", "sliceName": "raceExt", "min": 1, "type": [{"code": "Extension", "profile": ["http://example.org/ext/race"]}]}
			]
		}
	}`)))

	t.Run("implicit discriminators, boolean fixed/pattern, max=0 and max=1", func(t *testing.T) {
		okPat := eng.ValidateJSON("Patient", []byte(`{
			"resourceType": "Patient",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/active-patient"]},
			"active": true,
			"multipleBirthBoolean": false,
			"name": [{"family": "Doe", "given": ["Jane"]}],
			"telecom": [{"system": "phone", "value": "555-0100"}],
			"extension": [{"url": "http://example.org/ext/race", "valueCode": "2106-3"}]
		}`))
		assert.True(t, okPat.IsValid(), "unexpected errors: %+v", okPat.Issues)

		badPat := eng.ValidateJSON("Patient", []byte(`{
			"resourceType": "Patient",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/active-patient"]},
			"active": false,
			"multipleBirthBoolean": true,
			"deceasedBoolean": true,
			"name": [{"family": "A"}, {"family": "B"}],
			"telecom": [
				{"system": "phone", "value": "555-0100"},
				{"system": "phone", "value": "555-0101"}
			]
		}`))
		assert.False(t, badPat.IsValid())
		assert.GreaterOrEqual(t, len(badPat.Issues), 5)
	})

	// Test ofType(), resolve(), exists max=0, $this value/pattern, choice types, and Bundle resource profile discriminator
	require.NoError(t, eng.RegisterStructureDefinitionJSON([]byte(`{
		"resourceType": "StructureDefinition",
		"url": "http://example.org/fhir/StructureDefinition/advanced-discriminators",
		"type": "Observation",
		"snapshot": {
			"element": [
				{
					"path": "Observation.value[x]",
					"slicing": {
						"discriminator": [{"type": "type", "path": "$this.ofType(Range)"}]
					}
				},
				{
					"path": "Observation.value[x]",
					"sliceName": "rangeVal",
					"min": 1,
					"type": [{"code": "Range"}]
				},
				{
					"path": "Observation.identifier",
					"slicing": {
						"discriminator": [{"type": "exists", "path": "assigner.resolve()"}]
					}
				},
				{
					"path": "Observation.identifier",
					"sliceName": "noAssigner",
					"min": 1
				},
				{
					"path": "Observation.identifier.assigner",
					"max": "0"
				},
				{
					"path": "Observation.note",
					"sliceName": "fixedNote",
					"min": 1,
					"fixedAnnotation": {"text": "verified"}
				},
				{
					"path": "Observation.performer",
					"sliceName": "orgPerformer",
					"min": 1,
					"patternReference": {"display": "General Hospital"}
				}
			]
		}
	}`)))

	t.Run("snapshot slicing with ofType, resolve, exists max=0, and implicit root fixed/pattern", func(t *testing.T) {
		okObs := eng.ValidateJSON("Observation", []byte(`{
			"resourceType": "Observation",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/advanced-discriminators"]},
			"status": "final",
			"code": {"coding": [{"system": "http://loinc.org", "code": "1234-5"}]},
			"valueRange": {"low": {"value": 10}, "high": {"value": 20}},
			"identifier": [{"system": "urn:oid:1.2", "value": "X"}],
			"note": [{"text": "verified"}],
			"performer": [{"reference": "Organization/org-1", "display": "General Hospital"}]
		}`))
		assert.True(t, okObs.IsValid(), "unexpected errors: %+v", okObs.Issues)
	})

	// Test Bundle entry.resource profile discriminator & choice suffix normalization
	require.NoError(t, eng.RegisterStructureDefinitionJSON([]byte(`{
		"resourceType": "StructureDefinition",
		"url": "http://example.org/fhir/StructureDefinition/patient-bundle",
		"type": "Bundle",
		"differential": {
			"element": [
				{
					"path": "Bundle.entry",
					"slicing": {
						"discriminator": [{"type": "profile", "path": "resource"}],
						"rules": "open"
					}
				},
				{
					"path": "Bundle.entry",
					"sliceName": "activePatEntry",
					"min": 1
				},
				{
					"path": "Bundle.entry.resource",
					"type": [{"code": "Patient", "profile": ["http://example.org/fhir/StructureDefinition/active-patient"]}]
				}
			]
		}
	}`)))

	t.Run("bundle entry resource profile discriminator via custom SD", func(t *testing.T) {
		okBundle := eng.ValidateJSON("Bundle", []byte(`{
			"resourceType": "Bundle",
			"meta": {"profile": ["http://example.org/fhir/StructureDefinition/patient-bundle"]},
			"type": "collection",
			"entry": [
				{
					"resource": {
						"resourceType": "Patient",
						"active": true,
						"multipleBirthBoolean": false,
						"name": [{"family": "Doe"}],
						"telecom": [{"system": "phone", "value": "555-0100"}],
						"extension": [{"url": "http://example.org/ext/race", "valueCode": "2106-3"}]
					}
				}
			]
		}`))
		assert.True(t, okBundle.IsValid(), "unexpected errors: %+v", okBundle.Issues)
	})

	// Test newly added US Core v6.1.0 & v7.0.0 sub-profiles in ValidateJSONWithProfiles
	t.Run("US Core v6.1.0 and v7.0.0 sub-profiles", func(t *testing.T) {
		sdohObs := []byte(`{
			"resourceType": "Observation",
			"status": "final",
			"category": [{"coding": [{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-category", "code": "sdoh"}]}],
			"code": {"coding": [{"system": "http://loinc.org", "code": "93025-5"}]},
			"subject": {"reference": "Patient/pat-1"},
			"effectiveDateTime": "2025-01-15T10:00:00Z",
			"valueCodeableConcept": {"coding": [{"system": "http://loinc.org", "code": "LA33-6"}]}
		}`)
		out := eng.ValidateJSONWithProfiles("Observation", sdohObs, []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-observation-sdoh-assessment"}, "")
		assert.True(t, out.IsValid(), "unexpected errors: %+v", out.Issues)

		headCirc := []byte(`{
			"resourceType": "Observation",
			"status": "final",
			"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs"}]}],
			"code": {"coding": [{"system": "http://loinc.org", "code": "9843-4"}]},
			"subject": {"reference": "Patient/pat-1"},
			"effectiveDateTime": "2025-01-15",
			"valueQuantity": {"value": 35.2, "unit": "cm", "system": "http://unitsofmeasure.org", "code": "cm"}
		}`)
		outHC := eng.ValidateJSONWithProfiles("Observation", headCirc, []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-head-circumference"}, "")
		assert.True(t, outHC.IsValid(), "unexpected errors: %+v", outHC.Issues)

		// HL7 IPA v1.0.0 & base FHIR vital-signs profiles
		outVital := eng.ValidateJSONWithProfiles("Observation", headCirc, []string{"http://hl7.org/fhir/StructureDefinition/vitalsigns|4.0.1"}, "")
		assert.True(t, outVital.IsValid() && len(outVital.Issues) == 0, "unexpected vitalsigns issues: %+v", outVital.Issues)

		ipaMedStmt := []byte(`{
			"resourceType": "MedicationStatement",
			"status": "active",
			"medicationReference": {"reference": "Medication/med-1"},
			"subject": {"reference": "Patient/pat-1"}
		}`)
		outIPA := eng.ValidateJSONWithProfiles("MedicationStatement", ipaMedStmt, []string{"http://hl7.org/fhir/uv/ipa/StructureDefinition/ipa-medicationstatement|1.0.0"}, "")
		assert.True(t, outIPA.IsValid() && len(outIPA.Issues) == 0, "unexpected IPA issues: %+v", outIPA.Issues)
	})
}
