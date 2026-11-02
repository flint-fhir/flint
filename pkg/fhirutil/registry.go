package fhirutil

import (
	"sort"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"

	bcrpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/bundle_and_contained_resource_go_proto"
)

var (
	r4RegistryOnce sync.Once
	r4Descriptors  map[string]protoreflect.MessageDescriptor
	r4CrFields     map[string]protoreflect.FieldDescriptor
	r4CrDesc       protoreflect.MessageDescriptor
	r4BundleDesc   protoreflect.MessageDescriptor
	r4SortedNames  []string
)

// AllR4ResourceDescriptors returns the protobuf MessageDescriptor for every official
// HL7 FHIR R4 resource type compiled into google/fhir R4 ContainedResource (~146 resource types).
func AllR4ResourceDescriptors() map[string]protoreflect.MessageDescriptor {
	r4RegistryOnce.Do(func() {
		r4CrDesc = (&bcrpb.ContainedResource{}).ProtoReflect().Descriptor()
		r4BundleDesc = (&bcrpb.Bundle{}).ProtoReflect().Descriptor()
		fields := r4CrDesc.Fields()
		r4Descriptors = make(map[string]protoreflect.MessageDescriptor, fields.Len())
		r4CrFields = make(map[string]protoreflect.FieldDescriptor, fields.Len())
		r4SortedNames = make([]string, 0, fields.Len())
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			md := fd.Message()
			if md == nil {
				continue
			}
			name := string(md.Name())
			r4Descriptors[name] = md
			r4CrFields[name] = fd
			r4SortedNames = append(r4SortedNames, name)
		}
		sort.Strings(r4SortedNames)
	})
	return r4Descriptors
}

// ContainedResourceDescriptor returns the google.fhir.r4.core.ContainedResource descriptor.
func ContainedResourceDescriptor() protoreflect.MessageDescriptor {
	_ = AllR4ResourceDescriptors()
	return r4CrDesc
}

// BundleDescriptor returns the google.fhir.r4.core.Bundle descriptor.
func BundleDescriptor() protoreflect.MessageDescriptor {
	_ = AllR4ResourceDescriptors()
	return r4BundleDesc
}

// ContainedResourceFieldByType returns the oneof FieldDescriptor inside google.fhir.r4.core.ContainedResource
// for the given FHIR R4 resourceType (e.g. "Patient", "Observation", "OperationOutcome").
func ContainedResourceFieldByType(resType string) protoreflect.FieldDescriptor {
	_ = AllR4ResourceDescriptors()
	return r4CrFields[resType]
}

// AllR4ResourceTypes returns the alphabetically sorted list of all official HL7 FHIR R4 resource type names.
func AllR4ResourceTypes() []string {
	_ = AllR4ResourceDescriptors()
	out := make([]string, len(r4SortedNames))
	copy(out, r4SortedNames)
	return out
}

// USCoreResourceTypes returns the 26 official HL7 US Core v6.1.0 & v7.0.0 resource types.
func USCoreResourceTypes() []string {
	return []string{
		"Patient",
		"Encounter",
		"Condition",
		"Observation",
		"MedicationRequest",
		"AllergyIntolerance",
		"Procedure",
		"DiagnosticReport",
		"Immunization",
		"Practitioner",
		"Organization",
		"Specimen",
		"Location",
		"DocumentReference",
		"Provenance",
		"CarePlan",
		"CareTeam",
		"Goal",
		"ServiceRequest",
		"Coverage",
		"QuestionnaireResponse",
		"MedicationDispense",
		"Medication",
		"Device",
		"PractitionerRole",
		"RelatedPerson",
	}
}
