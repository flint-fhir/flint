package fhirutil_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"

	"github.com/flint-fhir/flint/pkg/fhirutil"
)

func TestProtoDescriptorToOpenAPISchema_Big5Resources(t *testing.T) {
	t.Run("Patient proto descriptor compiles choice[x], EnumDescriptor, and repeated HumanName", func(t *testing.T) {
		md := (&patpb.Patient{}).ProtoReflect().Descriptor()
		schema := fhirutil.ProtoDescriptorToOpenAPISchema("Patient", md, nil)

		assert.Equal(t, "object", schema["type"])
		assert.Equal(t, "google.fhir.r4.core.Patient", schema["x-protobuf-full-name"])

		props, ok := schema["properties"].(map[string]any)
		require.True(t, ok)

		// EnumDescriptor extraction for Patient.gender
		genderProp, ok := props["gender"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "string", genderProp["type"])
		assert.Equal(t, []string{"female", "male", "other", "unknown"}, genderProp["enum"])
		assert.Contains(t, genderProp["x-protobuf-enum"], "AdministrativeGenderCode")

		// Temporal format for birthDate
		birthProp, ok := props["birthDate"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "string", birthProp["type"])
		assert.Equal(t, "date", birthProp["format"])

		// choice[x] oneof expansion for deceased[x] and multipleBirth[x]
		decBool, ok := props["deceasedBoolean"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "boolean", decBool["type"])
		assert.Equal(t, "deceased.boolean", decBool["x-protobuf-field"])

		decDT, ok := props["deceasedDateTime"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "date-time", decDT["format"])

		// Repeated composite HumanName
		nameProp, ok := props["name"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "array", nameProp["type"])
		nameItems, ok := nameProp["items"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "google.fhir.r4.core.HumanName", nameItems["x-protobuf-message"])
	})

	t.Run("Observation proto descriptor compiles value[x], effective[x], and status EnumDescriptor", func(t *testing.T) {
		md := (&obspb.Observation{}).ProtoReflect().Descriptor()
		schema := fhirutil.ProtoDescriptorToOpenAPISchema("Observation", md, []string{"status", "code"})

		assert.Equal(t, []string{"resourceType", "status", "code"}, schema["required"])
		props := schema["properties"].(map[string]any)

		valQty, ok := props["valueQuantity"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "google.fhir.r4.core.Quantity", valQty["x-protobuf-message"])
		assert.Equal(t, "value.quantity", valQty["x-protobuf-field"])

		effDT, ok := props["effectiveDateTime"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "date-time", effDT["format"])
		assert.Equal(t, "effective.date_time", effDT["x-protobuf-field"])
	})

	t.Run("Condition, Encounter, and Practitioner proto descriptors compile cleanly", func(t *testing.T) {
		condSchema := fhirutil.ProtoDescriptorToOpenAPISchema("Condition", (&condpb.Condition{}).ProtoReflect().Descriptor(), []string{"subject"})
		condProps := condSchema["properties"].(map[string]any)
		assert.Contains(t, condProps, "onsetDateTime")
		assert.Contains(t, condProps, "abatementDateTime")

		encSchema := fhirutil.ProtoDescriptorToOpenAPISchema("Encounter", (&encpb.Encounter{}).ProtoReflect().Descriptor(), []string{"status", "class"})
		encProps := encSchema["properties"].(map[string]any)
		assert.Contains(t, encProps, "class")
		assert.NotContains(t, encProps, "classValue")

		pracSchema := fhirutil.ProtoDescriptorToOpenAPISchema("Practitioner", (&pracpb.Practitioner{}).ProtoReflect().Descriptor(), nil)
		pracProps := pracSchema["properties"].(map[string]any)
		assert.Contains(t, pracProps, "qualification")
	})
}
