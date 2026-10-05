// Package fhirutil provides nil-safe accessors for google/fhir R4 proto types.
//
// google/fhir protos wrap ALL primitives in messages. For example:
//
//	patient.GetBirthDate()         → *Date (may be nil)
//	patient.GetBirthDate().GetValue() → string (safe even if nil)
//
// This package provides helpers to reduce boilerplate for common operations.
package fhirutil
